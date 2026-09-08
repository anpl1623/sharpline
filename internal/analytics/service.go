// The service: the stage that consumes price.computed, drives the three
// detectors, persists what they find and publishes it to the signals topics.
//
// Read doc.go first. It carries the argument for why this is a SECOND CONSUMER
// inside `pricer` rather than a hook inside the pricing pass, for why signals are
// events rather than state, and for why nothing here may key a finding on a
// clock reading. This file is the code those arguments describe.
package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/analytics/steam"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// GroupSignals is this stage's consumer group on price.computed.
//
// It is DISTINCT from [pricing.GroupPricer], which subscribes to
// odds.normalized, and the distinction is what lets both loops run in one process
// without either seeing the other's offsets. Two groups on two topics is the
// whole of the coupling between the pricing stage and this one.
//
// Like every group id it is the unit of offset ownership and is therefore frozen:
// changing it on a running deployment starts from no committed offsets and
// replays the entire compacted price.computed topic. That replay is harmless —
// every write below is an upsert on a replay key — but it is a bus event nobody
// would want by accident.
//
// Every replica of `pricer` shares it, so Kafka splits the partitions between
// them and each market is owned by exactly one replica at a time. That matters
// more here than it does for the pricing stage: the steam detector holds
// per-market windowed state, and per-key ownership is what makes that state
// correct rather than a race between replicas that each saw half a market's
// observations.
const GroupSignals = "pricer-signals"

// Defaults. Each is overridable through [ServiceOptions]; zero means the default.
const (
	// DefaultDetectorTimeout bounds one record's whole detector pass.
	//
	// CLAUDE.md §12 puts a timeout on every external call and this one is not
	// external — but it is a REBALANCE-SAFETY parameter, which is the real reason
	// it exists. internal/platform/kafka's Consumer blocks the group's rebalance
	// for a whole poll and fences a member that exceeds its RebalanceTimeout
	// (60s), so a detector that could spin without bound would convert a
	// pathological market into LOST partitions and duplicated work across the
	// entire group. Two seconds matches internal/pricing's engine budget and is
	// three orders of magnitude above the work actually done.
	DefaultDetectorTimeout = 2 * time.Second

	// signalIDLen is how many hex characters of the replay-key digest travel as
	// kafka.Message.ID. 32 is 128 bits — far more than a deduplication identifier
	// needs, and comfortably inside domain.MaxIDLen (128 bytes), which the bus
	// enforces.
	signalIDLen = 32

	// storeAttempts bounds how many times one finding's store write is re-run
	// after [ErrContended]. Three, for the reason its doc gives: the contention it
	// survives is another transaction holding the catalogue rows this write's
	// foreign keys need, which clears the moment that transaction commits. A
	// deadlock that survives three attempts is sustained contention an operator has
	// to see, not something a longer loop should absorb.
	storeAttempts = 3

	// storeBackoff is the base pause between those attempts, multiplied by the
	// attempt number. Deliberately short: the transaction that won has already
	// committed by the time the victim's error arrives, so the pause exists to
	// avoid colliding with the NEXT one rather than to wait out the last.
	storeBackoff = 10 * time.Millisecond
)

// signalIDSep separates the parts of a composed message id before they are
// digested. 0x1f, the ASCII unit separator: a byte no identifier or formatted
// instant can contain, so ("ab","c") and ("a","bc") cannot digest alike.
const signalIDSep = "\x1f"

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// ServiceOptions are [New]'s dependencies. Everything is constructor-injected;
// nothing is read from a global (CLAUDE.md §12).
type ServiceOptions struct {
	// EV, Arb and Steam configure the three detectors. A ZERO VALUE means that
	// detector's documented defaults rather than an invalid configuration,
	// because a service configured from the environment must be constructible
	// without an operator naming a window length or a magnitude threshold.
	//
	// There is no switch to disable a detector. All three run over a record the
	// decode has already produced, they read no clock and do no I/O, and a stage
	// that had silently stopped looking for arbitrage would look exactly like one
	// that found none — which is the failure mode metrics.go exists to defend
	// against and which a configuration flag would reintroduce.
	EV    EVConfig
	Arb   ArbConfig
	Steam steam.Config

	// Store persists findings.
	//
	// OPTIONAL, and the consequence is stated loudly rather than hidden: a nil
	// Store means NOTHING IS PERSISTED and the query surface `api` serves stays
	// empty.
	//
	// It stays optional even though `pricer` always wires one — config.Pricer
	// declares RequirePostgres, so that binary cannot start without a pool — because
	// refusing to construct without a store would take the whole pricing stage down
	// for the sake of the analytics half, and because a one-shot tool or a test may
	// legitimately want the detectors without a database. [writeNoSink] counts every
	// finding a nil store drops, so the gap is a number on the dashboard rather than
	// a startup line nobody re-reads.
	Store Store

	// Publisher writes the signals topics.
	//
	// OPTIONAL for the same reason and with the same treatment. A nil Publisher
	// means no finding reaches signals.ev, signals.arb or signals.steam, counted
	// under [writeNoSink]. *kafka.OddsProducer satisfies this port; see
	// [Publisher] for why all three methods are keyed by market.
	Publisher Publisher

	// Consumer subscribes to price.computed and drives this service. Required by
	// Run; New does not need it, so a caller may build the handler alone for a
	// one-shot tool or a test.
	Consumer Consumer

	// Logger is required. A signals stage that logs nowhere makes a refused
	// record invisible.
	Logger *slog.Logger

	// Registry receives this package's collectors. Nil builds them unregistered,
	// which is right for a unit test. Ignored when Metrics is set.
	Registry prometheus.Registerer

	// Metrics is an already-registered collector set, for a process running more
	// than one Service. Takes precedence over Registry.
	Metrics *Metrics

	// Clock is the source of DetectedAt and of the detector timings. Nil means
	// time.Now.
	//
	// NOTHING A DETECTOR SEES READS IT. Every instant that participates in a
	// finding's identity comes from the source record, which came from the
	// provider; see [Clock] and doc.go on why that is a requirement rather than a
	// habit.
	Clock Clock

	// DetectorTimeout bounds one record's detector pass. Zero means
	// [DefaultDetectorTimeout].
	DetectorTimeout time.Duration
}

func (o ServiceOptions) validate() error {
	switch {
	case o.Logger == nil:
		return fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	case o.DetectorTimeout < 0:
		return fmt.Errorf("%w: DetectorTimeout is negative", ErrInvalidOptions)
	}
	if err := o.EV.Validate(); err != nil {
		return err
	}
	if err := o.Arb.Validate(); err != nil {
		return err
	}
	return o.Steam.Validate()
}

// -----------------------------------------------------------------------------
// Service
// -----------------------------------------------------------------------------

// Service consumes price.computed, detects, persists and publishes.
//
// It implements [kafka.Handler] and honours that interface's contract:
// idempotent (every write is an upsert on a replay key derived from the input),
// tombstone-aware, prompt, and it retains nothing from Delivery.Value past its
// return.
//
// IT IS NOT SAFE FOR CONCURRENT HandleMessage CALLS, because the steam detector
// holds per-market state and is explicitly single-goroutine. That is not a
// restriction in practice: internal/platform/kafka's Consumer delivers records
// sequentially on its own goroutine, which is the only caller.
type Service struct {
	ev    *EVFinder
	arb   *ArbSurface
	steam *steam.Detector

	store     Store
	publisher Publisher
	consumer  Consumer

	log     *slog.Logger
	metrics *Metrics
	clock   Clock

	timeout time.Duration

	// running gates the readiness checker. It is false before Run has started the
	// consumer and false again once it has stopped, so /readyz reports the truth
	// during both a cold start and a drain rather than only in between.
	running atomic.Bool
}

// Compile-time proof that this type satisfies the two interfaces the composition
// root hands it to. Here rather than at the call site because a mismatch should
// break this package's build.
var (
	_ kafka.Handler = (*Service)(nil)
	_ httpx.Checker = (*Service)(nil)
)

// New validates the options and builds the service. It performs no I/O and
// starts nothing; call Run.
//
// There is deliberately no Warm equivalent to internal/pricing's. That service
// warms because price.computed is COMPACTED and it must not republish what an
// earlier run already priced; this one has nothing to warm from, because a signal
// is an event rather than a state and there is no snapshot in which a previously
// emitted finding would appear. A fresh group replays price.computed from the
// beginning, which on a compacted topic is the current slate, and re-derives
// today's +EV and arbitrage findings into the same upserted rows. That replay
// produces NO steam findings, and correctly so: a compacted snapshot holds one
// record per market, and one observation is not a window.
func New(opts ServiceOptions) (*Service, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	m := opts.Metrics
	if m == nil {
		var err error
		if m, err = NewMetrics(opts.Registry); err != nil {
			return nil, fmt.Errorf("analytics: register metrics: %w", err)
		}
	}

	ev, err := NewEVFinder(opts.EV)
	if err != nil {
		return nil, err
	}
	arb, err := NewArbSurface(opts.Arb)
	if err != nil {
		return nil, err
	}
	det, err := steam.New(opts.Steam)
	if err != nil {
		return nil, err
	}

	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	timeout := opts.DetectorTimeout
	if timeout <= 0 {
		timeout = DefaultDetectorTimeout
	}

	return &Service{
		ev:        ev,
		arb:       arb,
		steam:     det,
		store:     opts.Store,
		publisher: opts.Publisher,
		consumer:  opts.Consumer,
		log:       opts.Logger.With(slog.String("component", "analytics.service")),
		metrics:   m,
		clock:     clock,
		timeout:   timeout,
	}, nil
}

// Metrics returns the collector set, so a process running several Services can
// share one registration.
func (s *Service) Metrics() *Metrics { return s.metrics }

// Name implements [httpx.Checker]. It is the key this dependency appears under
// in the /readyz payload, and it is distinct from internal/pricing's "pricer"
// because the two answer different questions about the same process.
func (s *Service) Name() string { return "analytics" }

// Check implements [httpx.Checker]: the stage is ready when its consumer loop is
// running.
//
// It answers a question neither the bus checker nor the pricing stage's checker
// does. The bus checker reports that the broker is reachable; the pricing
// checker reports that prices are being computed. A replica whose SIGNALS
// consumer has exited while its pricing consumer is healthy would otherwise look
// entirely fine while the analytics surface silently stopped updating — which is
// the shape of the phase-2 defect the contract ledger records, a probe returning
// 200 for something the process is not actually doing.
func (s *Service) Check(context.Context) error {
	if !s.running.Load() {
		return ErrNotRunning
	}
	return nil
}

// Run consumes price.computed until ctx is cancelled or the consumer stops.
//
// # There is no producer flush here, deliberately
//
// internal/pricing's Run flushes because it OWNS the publish path's durability
// for a compacted topic. This stage shares the same *kafka.OddsProducer instance
// with that service, which already performs an explicit pre-close flush on
// shutdown, and the composition root's deferred Close flushes again. A third
// flush from here would race the second: a flush issued while another goroutine
// is still filling the buffer flushes a buffer still being filled, which is the
// exact failure internal/pricing's shutdown ordering exists to avoid. So this
// package publishes synchronously — every accepted finding is acknowledged before
// its record's offset can commit — and leaves the buffer's lifetime to its owner.
func (s *Service) Run(ctx context.Context) error {
	if s.consumer == nil {
		return ErrNotRunnable
	}

	s.log.Info("signals stage running",
		slog.String("consumes", kafka.TopicPriceComputed),
		slog.String("group", GroupSignals),
		slog.Float64("ev_min_percent", s.ev.Config().MinEVPercent),
		slog.String("ev_max_quote_age", s.ev.Config().MaxQuoteAge.String()),
		slog.String("arb_max_leg_age", s.arb.Config().MaxLegAge.String()),
		slog.Float64("arb_min_return", s.arb.Config().MinReturn),
		slog.String("steam_window", s.steam.Config().Window.String()),
		slog.String("steam_hop", s.steam.Config().Hop.String()),
		slog.Float64("steam_min_magnitude", s.steam.Config().MinMagnitude),
		slog.Bool("store_wired", s.store != nil),
		slog.Bool("publisher_wired", s.publisher != nil),
	)

	s.running.Store(true)
	err := s.consumer.Run(ctx, s)
	s.running.Store(false)
	s.log.Info("signals stage consumer stopped")
	return err
}

// -----------------------------------------------------------------------------
// Handling one record
// -----------------------------------------------------------------------------

// HandleMessage runs the three detectors over one priced market and returns only
// once every finding is durably written.
//
// It implements [kafka.Handler]. The Consumer commits the last successfully
// handled record per partition, so a nil return here is a claim of durability:
// every path that returns nil has either had its writes acknowledged or has
// deliberately written nothing.
//
// # Errors and what the Consumer does with them
//
// A returned error is reported to the Consumer, and what happens next is the
// Consumer's ErrorPolicy, not this method's choice: under ErrorPolicyStop the
// offset stays uncommitted and the record is redelivered, under ErrorPolicySkip —
// which is what `pricer` wires, so that one market cannot halt every other market
// on the partition — the record is counted, logged and advanced over. This method
// must therefore be correct under BOTH, and it is: the write path is an idempotent
// upsert on an input-derived replay key, so a redelivery re-derives identical
// findings and re-upserts identical rows, and a skip loses nothing that the
// market's next price change does not re-derive, because price.computed is
// compacted and republished on every change.
//
// Only TRANSIENT failures are returned — a store or a broker that refused. A
// PERMANENT failure (an undecodable payload, a record whose envelope this build
// does not read) returns nil after counting and logging it: neither redelivery nor
// a reprice can change the bytes on disk.
//
// One transient condition deliberately returns nil as well: a record whose
// findings were refused ONLY because their catalogue rows have not committed yet
// (see [ErrCatalogueLag]). It is counted under its own outcome and logged at WARN,
// because it is not an outage, it is not this stage's defect, and no policy's
// response to it is better than advancing.
func (s *Service) HandleMessage(ctx context.Context, d *kafka.Delivery) error {
	if d == nil {
		return fmt.Errorf("analytics: nil delivery")
	}

	id, err := d.MarketID()
	if err != nil {
		s.metrics.observeRecord(resultInvalid)
		s.log.Error("record rejected: unusable market key; redelivery cannot change the outcome",
			slog.Any("record", d),
			slog.Bool("permanent", true),
			slog.String("error", err.Error()),
		)
		return nil
	}

	if d.Tombstone {
		// A deleted market's findings are NOT retracted. A signal is a statement
		// that something happened at an instant, and the market ceasing to exist
		// afterwards does not un-happen it — the same reason wager.events is
		// retention-based rather than compacted. What IS released is the steam
		// detector's windowed state for the market, which would otherwise
		// accumulate for the life of the process on a slate that rolls over daily.
		s.steam.Forget(id)
		s.metrics.observeSteamState(s.steam.Markets())
		s.metrics.observeRecord(resultTombstoned)
		return nil
	}

	if d.Envelope.Type != pricing.MessageType {
		// Counted rather than ignored, because it means something this build does
		// not read is writing the topic. Skipped rather than failed: decoding it
		// would misparse rather than error.
		s.metrics.observeRecord(resultInvalid)
		s.log.Warn("skipping a record whose envelope this build does not read",
			slog.Any("record", d),
			slog.String("want", pricing.MessageType),
			slog.String("error", ErrUnsupportedMessage.Error()),
		)
		return nil
	}

	var rec pricing.ComputedMarket
	if err := d.Unmarshal(&rec); err != nil {
		s.metrics.observeRecord(resultInvalid)
		s.log.Error("record rejected: payload could not be decoded; redelivery cannot change the outcome",
			slog.Any("record", d),
			slog.Bool("permanent", true),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if err := rec.Validate(); err != nil {
		s.metrics.observeRecord(resultInvalid)
		s.log.Error("record rejected: the priced market does not validate",
			slog.Any("record", d),
			slog.Bool("permanent", true),
			slog.String("error", err.Error()),
		)
		return nil
	}

	if err := s.process(ctx, id, rec); err != nil {
		// A record whose findings were refused ONLY because their catalogue rows
		// have not committed yet is not a sink failure and must not be reported as
		// one. It is `ingest`'s writer running behind this stage, it clears on its
		// own, and returning an error for it would be wrong under either
		// ErrorPolicy: Stop would halt the whole signals consumer over a transient
		// referential gap — which on a cold start is most of the first replay — and
		// Skip would advance past the record anyway while the log claimed a
		// redelivery that never comes. Counted, named, and advanced over.
		if onlyCatalogueLag(err) {
			s.metrics.observeRecord(resultDeferred)
			s.log.Warn("every finding on this record references a catalogue row that has not "+
				"committed yet; nothing was written, and the findings are re-derived when this "+
				"market next reprices",
				slog.Any("record", d),
				slog.String("market", id.String()),
				slog.String("error", err.Error()),
			)
			return nil
		}
		s.metrics.observeRecord(resultFailed)
		s.log.Error("a sink refused a finding; the record is reported to the consumer as failed "+
			"and its findings are re-derived when this market next reprices",
			slog.Any("record", d),
			slog.String("market", id.String()),
			slog.Bool("permanent", false),
			slog.String("error", err.Error()),
		)
		return err
	}

	s.metrics.observeRecord(resultProcessed)
	return nil
}

// process runs the detectors under their own deadline and writes what they find.
//
// # Why the deadline is imposed here rather than trusted to the detectors
//
// A detector that spun would not merely be slow: it would block the consumer
// group's rebalance until this member was fenced, and its partitions would be
// redelivered to somebody else. See [DefaultDetectorTimeout].
//
// # Why the detectors all run before anything is written
//
// So that a store failure on the first +EV finding cannot leave the steam
// detector's window state un-advanced. The detectors mutate state (the steam
// windows); the writes do not. Running every detector first means a redelivery
// re-runs them over a detector whose state has already absorbed this record —
// which is exactly why the steam detector drops observations older than its
// watermark and why every write is an upsert. The alternative ordering would make
// a transient store outage silently skip windows.
func (s *Service) process(ctx context.Context, id domain.MarketID, rec pricing.ComputedMarket) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	detectedAt := s.clock()

	start := s.clock()
	evs, evStats := s.ev.Scan(rec, detectedAt)
	s.metrics.observeDetector(kindEV, s.clock().Sub(start), evStats.Signals, evReasonLabels(evStats))

	start = s.clock()
	arbs, arbStats := s.arb.Scan(rec, detectedAt)
	s.metrics.observeDetector(kindArb, s.clock().Sub(start), arbStats.Signals, arbReasonLabels(arbStats))

	start = s.clock()
	findings, steamStats := s.steam.Observe(steamUpdate(id, rec))
	s.metrics.observeDetector(kindSteam, s.clock().Sub(start), steamStats.Findings, steamReasonLabels(steamStats))
	s.metrics.observeSteamWindows(steamStats.Windows, steamStats.Candidates)
	s.metrics.observeSteamState(s.steam.Markets())

	var errs []error
	for _, sig := range evs {
		errs = append(errs, s.emitEV(ctx, id, sig))
	}
	for _, sig := range arbs {
		errs = append(errs, s.emitArbitrage(ctx, id, sig))
	}
	for _, f := range findings {
		sig, ok := s.steamSignal(rec, f, detectedAt)
		if !ok {
			continue
		}
		s.metrics.observeWindowLag(detectedAt.Sub(f.WindowEnd))
		errs = append(errs, s.emitSteam(ctx, id, sig))
	}
	return errors.Join(errs...)
}

// emitEV persists then publishes one +EV finding.
//
// # The order is PERSIST THEN PUBLISH, and it is deliberate
//
// The database is the query surface `api` serves and the bus is a notification.
// A finding that is stored and not published is visible to anyone who looks; a
// finding that is published and not stored is a notification pointing at a row
// that does not exist. Since a failure at either step redelivers the whole
// record, and the store write is an upsert, the worst case of this ordering is a
// duplicate bus record — which a consumer can deduplicate on kafka.Message.ID —
// and the worst case of the other ordering is a dangling alert.
func (s *Service) emitEV(ctx context.Context, id domain.MarketID, sig EVSignal) error {
	if err := s.persist(ctx, kindEV, func() error { return s.store.RecordEVSignal(ctx, sig) }); err != nil {
		return err
	}
	return s.publish(ctx, kindEV, func(p Publisher) error {
		return p.PublishEVSignal(ctx, id, kafka.Message{
			Type: MessageTypeEV,
			// The replay key, digested: (selection, book, quote instant), which is
			// exactly ev_signals' natural key. A consumer that deduplicates on it
			// therefore agrees with the database about what "the same finding"
			// means.
			ID: signalID(string(sig.SelectionID), string(sig.BookID), stamp(sig.QuoteObservedAt)),
			// The PROVIDER's instant, propagated unchanged. It is what makes a
			// signal's staleness measurable on the same footing as a price's; a
			// detection timestamp here would report perfect freshness for a finding
			// about a quote from ten minutes ago.
			ObservedAt: sig.QuoteObservedAt,
			Payload:    sig,
		})
	})
}

// emitArbitrage persists then publishes one arbitrage finding. See [emitEV] on
// the ordering.
func (s *Service) emitArbitrage(ctx context.Context, id domain.MarketID, sig ArbitrageSignal) error {
	if err := s.persist(ctx, kindArb, func() error { return s.store.RecordArbitrageSignal(ctx, sig) }); err != nil {
		return err
	}
	return s.publish(ctx, kindArb, func(p Publisher) error {
		return p.PublishArbitrageSignal(ctx, id, kafka.Message{
			Type: MessageTypeArbitrage,
			// arbitrage_signals' natural key. The fingerprint is already a digest;
			// it is re-digested with the other two parts rather than used alone so
			// that every message id in this package is built the same way.
			ID: signalID(string(sig.MarketID), stamp(sig.ObservedAt), sig.LegsFingerprint),
			// The OLDEST leg's instant. An opportunity is exactly as fresh as its
			// stalest leg, so that is the instant its staleness must be measured
			// from.
			ObservedAt: sig.ObservedAt,
			Payload:    sig,
		})
	})
}

// emitSteam persists then publishes one steam finding. See [emitEV] on the
// ordering.
func (s *Service) emitSteam(ctx context.Context, id domain.MarketID, sig SteamSignal) error {
	if err := s.persist(ctx, kindSteam, func() error { return s.store.RecordSteamSignal(ctx, sig) }); err != nil {
		return err
	}
	return s.publish(ctx, kindSteam, func(p Publisher) error {
		return p.PublishSteamSignal(ctx, id, kafka.Message{
			Type: MessageTypeSteam,
			// steam_signals' natural key: (market, selection, window). The window
			// bounds are part of it because the same selection steaming twice in one
			// session is two findings.
			ID: signalID(string(sig.MarketID), string(sig.SelectionID),
				stamp(sig.WindowStart), stamp(sig.WindowEnd)),
			// The window's END. It is the newest event-time instant the finding is
			// about, and it is the instant migrations/00009 partitions on, so the
			// bus and the table measure a steam finding's age the same way.
			ObservedAt: sig.WindowEnd,
			Payload:    sig,
		})
	})
}

// persist runs one store write, counting the outcome — including the outcome
// where there is no store at all.
//
// A missing store is NOT an error: it does not fail the record and does not
// trigger a redelivery, because redelivery cannot conjure a database. It is
// counted under [writeNoSink] so the gap is a number on the dashboard rather than
// a startup line nobody re-reads.
// Two store failures are retried in place rather than left to redelivery, and
// both for the same reason: every Store method is an idempotent upsert on an
// input-derived replay key, so re-running one writes exactly what the first
// attempt would have. Redelivery would technically also fix them, but at the cost
// of re-running every detector on the record and re-emitting its OTHER findings —
// and under the ErrorPolicySkip the pricer wires, redelivery does not happen at
// all. The local fix is the only one that is both cheap and real.
//
//   - [ErrContended]: the write lost a lock-ordering race and was rolled back
//     having written nothing. Its doc carries the argument for why re-running is
//     safe and why the platform layer refuses to do it on a caller's behalf.
//   - [ErrCatalogueLag]: a catalogue parent this finding references has not
//     committed yet, because `ingest` writes it and nothing orders that write
//     against this one. A retry wins whenever the parent lands inside the loop's
//     budget; when it does not, the finding is re-derived on the market's next
//     price change.
//
// The loop is bounded by [storeAttempts]. What it must not become is a loop that
// hides sustained contention: the last failure is returned with its SQLSTATE
// intact, every retried attempt is counted under [writeContended], and the
// record's offset stays uncommitted.
func (s *Service) persist(ctx context.Context, kind string, write func() error) error {
	if s.store == nil {
		s.metrics.observeWrite(kind, sinkStore, writeNoSink)
		return nil
	}

	var err error
	for attempt := 1; ; attempt++ {
		if err = write(); err == nil {
			s.metrics.observeWrite(kind, sinkStore, writeOK)
			return nil
		}
		if attempt >= storeAttempts || !retryableWrite(err) {
			break
		}
		s.metrics.observeWrite(kind, sinkStore, retryLabel(err))
		if waitErr := sleepCtx(ctx, storeBackoff*time.Duration(attempt)); waitErr != nil {
			return fmt.Errorf("analytics: persist %s signal: %w", kind, waitErr)
		}
	}

	s.metrics.observeWrite(kind, sinkStore, writeFailed)
	return fmt.Errorf("analytics: persist %s signal: %w", kind, err)
}

// onlyCatalogueLag reports whether EVERY leaf of err is [ErrCatalogueLag].
//
// It walks the joined error rather than calling errors.Is, because errors.Is on a
// multi-error is satisfied by ANY leaf matching — and a record that produced one
// catalogue-lag finding and one genuine store outage must be reported as the
// outage. The walk is over Unwrap() []error, the shape errors.Join builds, and
// falls back to errors.Is for a single wrapped error.
func onlyCatalogueLag(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		leaves := joined.Unwrap()
		if len(leaves) == 0 {
			return false
		}
		for _, leaf := range leaves {
			if !onlyCatalogueLag(leaf) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, ErrCatalogueLag)
}

// retryableWrite reports whether a store failure is one this package re-runs in
// place. Both conditions GUARANTEE that nothing was written — a rolled-back
// transaction and a refused referential check respectively — so a re-run cannot
// double anything even before the replay key's idempotence is considered.
func retryableWrite(err error) bool {
	return errors.Is(err, ErrContended) || errors.Is(err, ErrCatalogueLag)
}

// retryLabel names the condition a retried attempt is being counted under, so
// that "this stage is only succeeding on its second try" and "this stage is
// racing the catalogue writer" are two different lines on the dashboard rather
// than one. They have different fixes: the first is contention on the catalogue
// rows, the second is a parent that does not exist yet.
func retryLabel(err error) string {
	if errors.Is(err, ErrCatalogueLag) {
		return writeCatalogueLag
	}
	return writeContended
}

// sleepCtx pauses for d, or returns the context's error if it ends first.
//
// Never a bare time.Sleep: the record's context carries the consumer's rebalance
// budget, and a pause that ignored it would convert a moment of database
// contention into a fenced group member.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// publish runs one bus write, counting the outcome — including the outcome where
// there is no publisher at all. See [persist] on why a missing sink is not an
// error, and [Publisher] on which three methods internal/platform/kafka still
// owes this package.
func (s *Service) publish(_ context.Context, kind string, write func(Publisher) error) error {
	if s.publisher == nil {
		s.metrics.observeWrite(kind, sinkBus, writeNoSink)
		return nil
	}
	if err := write(s.publisher); err != nil {
		s.metrics.observeWrite(kind, sinkBus, writeFailed)
		return fmt.Errorf("analytics: publish %s signal: %w", kind, err)
	}
	s.metrics.observeWrite(kind, sinkBus, writeOK)
	return nil
}

// -----------------------------------------------------------------------------
// Adapting the priced record to the steam detector, and back
// -----------------------------------------------------------------------------

// steamUpdate lifts one priced market into the detector's input.
//
// # Every quote is offered, not only the scored ones
//
// [pricing.QuoteStatusStale] and [pricing.QuoteStatusLineMismatch] quotes are
// included deliberately. Those statuses are judgements about whether a quote is a
// comparable BETTING opportunity — is it fresh enough to score, is it at the
// reference book's line — and neither has anything to do with whether the book's
// price MOVED. A book that has drifted off the reference line is still a book
// whose implied probability changed, and excluding it would remove exactly the
// lagging books whose late agreement is the steam signature.
//
// The implied probability is the raw one, margin included; steam's doc.go argues
// why it is not devigged.
//
// The anchor is the record's own observation instant, so a market that produced a
// record with no usable quote still advances event time and lets a pending window
// close. Without it a market that went quiet would hold its last window open for
// ever.
func steamUpdate(id domain.MarketID, rec pricing.ComputedMarket) steam.Update {
	u := steam.Update{Market: id, Anchor: rec.ObservedAt}
	for _, b := range rec.Books {
		for _, q := range b.Quotes {
			u.Quotes = append(u.Quotes, steam.Quote{
				Selection:  q.SelectionID,
				Book:       b.BookID,
				Implied:    q.Implied,
				ObservedAt: q.ObservedAt,
			})
		}
	}
	return u
}

// steamSignal shapes one detector finding into the wire and storage document,
// adding the catalogue facts the detector has no business knowing.
//
// It returns false for a finding that would violate a CHECK in
// migrations/00009 — see validate.go on why the rule is applied here rather than
// left to the database. The refusal is logged at WARN rather than counted
// silently: a detector producing a finding the schema refuses is a disagreement
// between two halves of this phase, and it should be noisy.
func (s *Service) steamSignal(
	rec pricing.ComputedMarket, f steam.Finding, detectedAt time.Time,
) (SteamSignal, bool) {
	followers := make([]SteamFollower, 0, len(f.Followers))
	for _, fol := range f.Followers {
		followers = append(followers, SteamFollower{
			BookID:           fol.Book,
			MovedAt:          fol.MovedAt.UTC(),
			LagSeconds:       fol.Lag.Seconds(),
			DeltaProbability: fol.Delta,
		})
	}

	sig := SteamSignal{
		SchemaVersion: SchemaVersion,
		MarketID:      f.Market,
		MarketType:    rec.Market.Type,
		LeagueID:      domain.LeagueID(rec.League.ID),
		SelectionID:   f.Selection,
		WindowStart:   f.WindowStart,
		WindowEnd:     f.WindowEnd,
		WindowSeconds: f.Window.Seconds(),
		HopSeconds:    f.Hop.Seconds(),
		Direction:     string(f.Direction),

		DeltaProbability:             f.Delta,
		MagnitudeProbabilityPoints:   f.Magnitude,
		VelocityProbabilityPerMinute: f.Velocity,

		// "none": the detector works on raw implied probabilities. The column
		// admits this fifth value where the other tables' devig_method columns do
		// not, precisely so the choice is recorded rather than implied.
		DevigMethod: steamDevigMethod,

		LeadBookID:  f.LeadBook,
		LeadMovedAt: f.LeadMovedAt,

		Followers:          followers,
		FollowerCount:      len(followers),
		ParticipatingBooks: f.ParticipatingBooks,

		CrossBookCorrelation: f.Correlation,

		ThresholdVelocity:     f.ThresholdVelocity,
		ThresholdMagnitude:    f.ThresholdMagnitude,
		ThresholdCorrelation:  f.ThresholdCorrelation,
		MinFollowers:          f.MinFollowers,
		MaxFollowerLagSeconds: f.MaxFollowerLag.Seconds(),

		DetectedAt: detectedAt,
	}

	if err := sig.validate(); err != nil {
		s.log.Warn("a steam finding was refused before it reached a sink; the detector and "+
			"migrations/00009 disagree about what a finding may look like",
			slog.String("market", string(sig.MarketID)),
			slog.String("selection", string(sig.SelectionID)),
			slog.String("finding", f.String()),
			slog.String("error", err.Error()),
		)
		return SteamSignal{}, false
	}
	return sig, true
}

// steamDevigMethod is the value written into steam_signals.devig_method. See
// steam's package doc: the detector works on raw implied probabilities, and the
// column admits "none" for exactly this case.
const steamDevigMethod = "none"

// validate mirrors the CHECK constraints migrations/00009 puts on steam_signals.
// See validate.go.
func (s SteamSignal) validate() error {
	switch {
	case !finite(s.WindowSeconds, s.HopSeconds, s.DeltaProbability, s.MagnitudeProbabilityPoints,
		s.VelocityProbabilityPerMinute, s.CrossBookCorrelation, s.ThresholdVelocity,
		s.ThresholdMagnitude, s.ThresholdCorrelation, s.MaxFollowerLagSeconds):
		return fmt.Errorf("%w: a value on the finding is not finite", ErrInvalidConfig)
	case !s.WindowEnd.After(s.WindowStart):
		return fmt.Errorf("%w: window [%s, %s) is empty or inverted",
			ErrInvalidConfig, s.WindowStart, s.WindowEnd)
	case s.WindowSeconds <= 0 || s.HopSeconds <= 0 || s.HopSeconds > s.WindowSeconds:
		return fmt.Errorf("%w: window %vs / hop %vs", ErrInvalidConfig, s.WindowSeconds, s.HopSeconds)
	case s.Direction != string(steam.DirectionShorten) && s.Direction != string(steam.DirectionDrift):
		return fmt.Errorf("%w: direction %q is neither shorten nor drift", ErrInvalidConfig, s.Direction)
	case (s.Direction == string(steam.DirectionShorten)) != (s.DeltaProbability > 0):
		return fmt.Errorf("%w: direction %q disagrees with delta %v",
			ErrInvalidConfig, s.Direction, s.DeltaProbability)
	case s.DeltaProbability <= -1 || s.DeltaProbability >= 1 || s.DeltaProbability == 0:
		return fmt.Errorf("%w: delta %v outside (-1, 0) ∪ (0, 1)", ErrInvalidConfig, s.DeltaProbability)
	case s.MagnitudeProbabilityPoints != math.Abs(s.DeltaProbability):
		return fmt.Errorf("%w: magnitude %v is not |delta %v|",
			ErrInvalidConfig, s.MagnitudeProbabilityPoints, s.DeltaProbability)
	case (s.VelocityProbabilityPerMinute > 0) != (s.DeltaProbability > 0):
		return fmt.Errorf("%w: velocity %v disagrees in sign with delta %v",
			ErrInvalidConfig, s.VelocityProbabilityPerMinute, s.DeltaProbability)
	case s.LeadMovedAt.Before(s.WindowStart) || !s.LeadMovedAt.Before(s.WindowEnd):
		return fmt.Errorf("%w: lead moved at %s, outside the half-open window [%s, %s)",
			ErrInvalidConfig, s.LeadMovedAt, s.WindowStart, s.WindowEnd)
	case s.FollowerCount != len(s.Followers):
		return fmt.Errorf("%w: follower count %d against %d followers",
			ErrInvalidConfig, s.FollowerCount, len(s.Followers))
	case s.FollowerCount < 1 || s.FollowerCount > maxSteamFollowers:
		return fmt.Errorf("%w: %d followers, the table admits [1, %d]",
			ErrInvalidConfig, s.FollowerCount, maxSteamFollowers)
	case s.ParticipatingBooks != s.FollowerCount+1:
		return fmt.Errorf("%w: %d participating books against %d followers",
			ErrInvalidConfig, s.ParticipatingBooks, s.FollowerCount)
	case s.CrossBookCorrelation < -1 || s.CrossBookCorrelation > 1:
		return fmt.Errorf("%w: correlation %v outside [-1, 1]", ErrInvalidConfig, s.CrossBookCorrelation)
	case s.ThresholdMagnitude <= 0 || s.ThresholdMagnitude >= 1:
		return fmt.Errorf("%w: magnitude threshold %v outside (0, 1)", ErrInvalidConfig, s.ThresholdMagnitude)
	case s.ThresholdVelocity <= 0:
		return fmt.Errorf("%w: velocity threshold %v is not positive", ErrInvalidConfig, s.ThresholdVelocity)
	case s.ThresholdCorrelation < -1 || s.ThresholdCorrelation > 1:
		return fmt.Errorf("%w: correlation threshold %v outside [-1, 1]",
			ErrInvalidConfig, s.ThresholdCorrelation)
	case s.MinFollowers < 1:
		return fmt.Errorf("%w: min followers %d is below 1", ErrInvalidConfig, s.MinFollowers)
	case s.MaxFollowerLagSeconds <= 0:
		return fmt.Errorf("%w: max follower lag %v is not positive", ErrInvalidConfig, s.MaxFollowerLagSeconds)

	// The four "meets its own thresholds" clauses, which migrations/00009
	// enforces as one CHECK. They are what makes a stored row unable to claim a
	// bound it fails.
	case math.Abs(s.VelocityProbabilityPerMinute) < s.ThresholdVelocity:
		return fmt.Errorf("%w: velocity %v is below the threshold %v it claims to meet",
			ErrInvalidConfig, s.VelocityProbabilityPerMinute, s.ThresholdVelocity)
	case s.MagnitudeProbabilityPoints < s.ThresholdMagnitude:
		return fmt.Errorf("%w: magnitude %v is below the threshold %v it claims to meet",
			ErrInvalidConfig, s.MagnitudeProbabilityPoints, s.ThresholdMagnitude)
	case s.CrossBookCorrelation < s.ThresholdCorrelation:
		return fmt.Errorf("%w: correlation %v is below the threshold %v it claims to meet",
			ErrInvalidConfig, s.CrossBookCorrelation, s.ThresholdCorrelation)
	case s.FollowerCount < s.MinFollowers:
		return fmt.Errorf("%w: %d followers is below the minimum %d it claims to meet",
			ErrInvalidConfig, s.FollowerCount, s.MinFollowers)
	}

	for i, f := range s.Followers {
		switch {
		case f.BookID.IsZero():
			return fmt.Errorf("%w: follower %d has no book", ErrInvalidConfig, i)
		case !finite(f.LagSeconds, f.DeltaProbability):
			return fmt.Errorf("%w: follower %d carries a non-finite value", ErrInvalidConfig, i)
		case f.LagSeconds < 0:
			return fmt.Errorf("%w: follower %d has a negative lag; a book that moved before the "+
				"lead would be the lead", ErrInvalidConfig, i)
		case f.LagSeconds > s.MaxFollowerLagSeconds:
			return fmt.Errorf("%w: follower %d lagged %vs, past the bound %vs",
				ErrInvalidConfig, i, f.LagSeconds, s.MaxFollowerLagSeconds)
		case i > 0 && s.Followers[i-1].LagSeconds > f.LagSeconds:
			return fmt.Errorf("%w: followers are not ordered by lag ascending", ErrInvalidConfig)
		}
	}

	// The market TYPE is checked and the line is not, because steam_signals
	// carries no line column at all: a steam finding is a statement about one
	// selection's probability over time, and the market's handicap is a property
	// of the market rather than of the move. [lineRule] would refuse every spread
	// and every total here for want of a line the table does not have.
	return marketTypeKnown(s.MarketType)
}

// maxSteamFollowers is the bound migrations/00009 puts on follower_count. 256 is
// far past any plausible book set and small enough that a runaway would be
// refused rather than stored.
const maxSteamFollowers = 256

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// signalID composes a deterministic kafka.Message.ID from a finding's replay key.
//
// It is a DIGEST rather than the concatenated key, and both properties matter.
// Deterministic, so a redelivery produces the same id and a consumer's
// deduplication agrees with the database's ON CONFLICT about what "the same
// finding" means. Digested, because the raw key can be three 128-byte
// identifiers and an instant, and the bus caps a message id at domain.MaxIDLen.
//
// Nothing derived from a clock of ours goes in. The parts are always the columns
// of the table's natural key and nothing else.
func signalID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, signalIDSep)))
	return hex.EncodeToString(sum[:])[:signalIDLen]
}

// stamp renders an instant for a message id: RFC 3339 with nanoseconds, in UTC.
//
// UTC is not cosmetic. time.Time carries a location, and two instants that are
// equal but carry different locations format differently — so a record decoded
// in one zone and re-derived in another would produce two ids for one finding.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// evReasonLabels flattens the +EV scan's reason counts into metric labels.
//
// EVReasonSignal is deliberately dropped: it is the numerator, counted on
// sharpline_analytics_signals_total, and repeating it under "suppressed" would
// make a panel that summed the reasons overcount every finding.
func evReasonLabels(s EVScanStats) map[string]int {
	out := make(map[string]int, len(s.Reasons))
	for r, n := range s.Reasons {
		if r == EVReasonSignal {
			continue
		}
		out[string(r)] = n
	}
	return out
}

// arbReasonLabels flattens the arbitrage scan's reason counts. See
// [evReasonLabels] on why the success reason is dropped.
func arbReasonLabels(s ArbScanStats) map[string]int {
	out := make(map[string]int, len(s.Reasons))
	for r, n := range s.Reasons {
		if r == ArbReasonSignal {
			continue
		}
		out[string(r)] = n
	}
	return out
}

// steamReasonLabels flattens the steam detector's stats into the same shape.
//
// The detector reports counters rather than a reason map, because its refusals
// are not per-candidate branches but properties of a window, so the translation
// is explicit. `late` and `skipped_windows` are not suppressions of a finding at
// all — they are observations the detector could not use and windows it never
// evaluated — and they are carried under the same series anyway, because the
// question they answer is the same one: WHY is this detector reporting nothing.
func steamReasonLabels(s steam.Stats) map[string]int {
	return map[string]int{
		"below_threshold": s.SuppressedByThreshold,
		"cooldown":        s.SuppressedByCooldown,
		"late":            s.Late,
		"skipped_windows": int(s.SkippedWindows),
	}
}
