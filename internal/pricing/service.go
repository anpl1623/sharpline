// The service: a kafka.Handler that turns one odds.normalized record into one
// price.computed record, plus the warm start, the readiness contract and the
// shutdown ordering around it.
//
// Read doc.go first. It carries the argument for the function-typed engine seam,
// for change detection that never decodes its own payload, for why the warm
// start happens at startup rather than inside a handler, and for the shutdown
// order. This file is the code those arguments describe.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Wire and topology constants
// -----------------------------------------------------------------------------

// MessageType — the kafka.Message.Type stamped on every record this service
// writes — is declared in payload.go beside the document it names, because it
// versions the PAYLOAD SHAPE and the payload is the engine's. This file only
// stamps it.
//
// price.computed is COMPACTED, which gives a bump a consequence a
// retention-based topic would not have: records written under v1 stay in the log
// until their key is next written, so a consumer rebuilding the snapshot from
// scratch will meet both versions. That is why the type name travels on every
// record instead of being inferred from the topic.

// GroupPricer is this service's consumer group on odds.normalized.
//
// It is the unit of offset ownership, so it is frozen: changing it on a running
// deployment starts from no committed offsets and replays the entire compacted
// topic — which is not harmful (that replay is exactly what makes the priced
// snapshot complete on a first deploy) but is a bus event nobody would want by
// accident.
//
// Every replica of `pricer` shares it. That is what makes CLAUDE.md §9's HPA a
// capacity decision rather than a correctness one: Kafka splits the partitions
// between the members, each market is owned by exactly one replica at a time,
// and per-key ordering is preserved because a key always hashes to one partition.
const GroupPricer = "pricer"

// Defaults. Each is overridable through Options; zero means the default.
const (
	// DefaultFlushTimeout bounds the final producer flush during shutdown.
	//
	// It is below Kubernetes' default terminationGracePeriodSeconds (30s) and
	// matches internal/ingest's choice for the same reason: the whole process
	// has to drain inside the orchestrator's grace period, and a flush that
	// outlives it becomes a SIGKILL with the records still buffered, which is
	// precisely the loss the flush exists to prevent.
	DefaultFlushTimeout = 10 * time.Second

	// DefaultWarmStartAttempts bounds how many times a failing warm start is
	// retried before the service prices cold. See Service.Warm.
	DefaultWarmStartAttempts = 3

	// MaxEngineRevisionLen bounds ServiceOptions.EngineRevision.
	//
	// The revision is concatenated onto a 64-character hex fingerprint to form
	// kafka.Message.ID, which the bus rejects past domain.MaxIDLen (128). 32
	// leaves generous headroom and is far more than a digest needs.
	MaxEngineRevisionLen = 32

	// DefaultEngineTimeout bounds one engine call.
	//
	// CLAUDE.md §12 puts a timeout on every external call, and this one is not
	// external — but it is also a REBALANCE-SAFETY parameter, which is the real
	// reason it exists. internal/platform/kafka's Consumer blocks the group's
	// rebalance for a whole poll and fences a member that exceeds
	// RebalanceTimeout (60s), so an engine that could spin without bound would
	// convert a pathological market into LOST partitions and duplicated work
	// across the entire group. Two seconds is eight times the p99 the
	// PricingLatencyHigh alert already considers a problem.
	DefaultEngineTimeout = 2 * time.Second
)

// engineRevisionSep joins the source fingerprint and the engine revision into
// the record's message id. It is a character domain.NewMarketID's charset does
// not admit, so the two halves can never be confused for one identifier.
const engineRevisionSep = "@"

// tombstoneReason is written to every tombstone this service publishes. It is
// the operator-facing half of kafka.Tombstone's ceremony: six months later,
// kafka-ui shows this string beside a deletion and it says who decided and why.
const tombstoneReason = "source market was tombstoned on odds.normalized; " +
	"a priced record for a market that no longer exists would stay in the compacted snapshot for ever"

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

// ErrInvalidOptions, ErrUnsupportedMessage and ErrStaleRecord live in errors.go
// with the rest of this package's sentinels. The two below are about the
// SERVICE's lifecycle rather than about why a market could not be priced, which
// is what that block describes, so they are declared where they are used.
var (
	// ErrNotRunning is what the readiness checker reports before Run has started
	// the consumer and after it has stopped.
	ErrNotRunning = errors.New("pricing: service is not running")

	// ErrNotRunnable is returned by Run when it was given no consumer.
	ErrNotRunnable = errors.New("pricing: no consumer")
)

// -----------------------------------------------------------------------------
// Consumer-declared seams (CLAUDE.md §12: "Interfaces are declared by the
// consumer, not the producer. Keep them small.")
// -----------------------------------------------------------------------------

// PriceFunc computes the price.computed payload for one normalized market.
//
// It is the seam between this service and the odds mathematics, and doc.go
// argues at length why it is a function type rather than an interface: Go has no
// covariance, so an interface returning `any` would be satisfied by no engine
// that returned a concrete payload, and every engine would need a hand-written
// adapter regardless. As a function type the adapter is one line at the
// composition root.
//
// # The contract an implementation must honour
//
//   - IT MUST BE A PURE FUNCTION OF rec. Same record in, same value out. The
//     suppression below rests on it: an engine that read the wall clock would
//     make this service skip a republication whose answer had in fact changed,
//     and the compacted snapshot would hold a stale price that nothing would
//     ever correct.
//   - IT MUST NOT RETAIN rec, and it must not mutate it. The slices on rec alias
//     the decode of a fetch buffer.
//   - IT MUST RETURN PROMPTLY. The group's rebalance is blocked for the whole
//     poll; DefaultEngineTimeout is the ceiling this service imposes.
//   - A returned error is treated as PERMANENT — a market this engine cannot
//     price. It is counted, logged and skipped, because redelivering a payload
//     the arithmetic already refused cannot produce a different answer. A
//     transient failure has no place here; nothing in the odds mathematics does
//     I/O.
//
// The returned value is marshalled as the envelope's data, so it must be
// JSON-encodable and must not be nil (kafka.Message rejects a nil or "null"
// payload, which is the guard that makes an accidental tombstone impossible).
type PriceFunc func(ctx context.Context, rec normalizer.NormalizedMarket) (any, error)

// Publisher is the slice of *kafka.OddsProducer this package uses.
//
// Three methods, because three is all this service needs: publish a priced
// market, delete one whose source has gone, and make sure the buffer is empty
// before the process exits. Closing is deliberately absent — this package does
// not own the producer's lifetime and must not be able to end it while something
// else still holds it.
//
// PublishPrice is the SYNCHRONOUS form on purpose; doc.go's durability argument
// depends on it. PublishPriceAsync exists and is not used here.
type Publisher interface {
	PublishPrice(ctx context.Context, id domain.MarketID, msg kafka.Message) error
	TombstonePrice(ctx context.Context, id domain.MarketID, ts kafka.Tombstone) error
	Flush(ctx context.Context) error
}

// Consumer is the part of *kafka.Consumer that Run drives.
//
// The Consumer owns the poll loop, the commit boundary and the group lifecycle,
// and this package reimplements none of them: it hands over a handler and waits
// for Run to return, which it does only after committing what it has handled.
//
// The Consumer handed in MUST be built with DisableLagExport left false: the
// dashboard's bus-lag panels are fed by its background refresher and this
// package deliberately emits no competing series.
type Consumer interface {
	Run(ctx context.Context, h kafka.Handler) error
}

// Snapshot is the half of *kafka.Snapshotter this package uses.
//
// Read streams a compacted topic from the beginning to the end offsets listed
// when the read began, which is what makes "caught up" a definite condition
// rather than a timeout — the property the whole warm-start argument rests on.
type Snapshot interface {
	Read(ctx context.Context, fn func(context.Context, *kafka.Delivery) error) (kafka.SnapshotStats, error)
}

// Compile-time proof that the shipped types satisfy the declarations above. They
// are here rather than at the call site because a mismatch should break THIS
// package's build, where the interfaces are declared.
var (
	_ Publisher     = (*kafka.OddsProducer)(nil)
	_ Consumer      = (*kafka.Consumer)(nil)
	_ Snapshot      = (*kafka.Snapshotter)(nil)
	_ kafka.Handler = (*Service)(nil)
	_ httpx.Checker = (*Service)(nil)
)

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// ServiceOptions are New's dependencies. Everything is constructor-injected;
// nothing is read from a global (CLAUDE.md §12).
//
// The name is qualified because [Options] in this package configures the ENGINE
// — the devig method, the reference-book preference, the staleness policy. The
// two are different kinds of thing: one is a set of live dependencies, the other
// is a set of trading judgements, and a single struct carrying both would let a
// caller inject a producer and a Kelly multiplier in the same literal.
type ServiceOptions struct {
	// Price is the odds mathematics. Required — there is deliberately no default
	// engine, because a pricer that silently priced nothing would be
	// indistinguishable from a pricer whose bus was empty.
	Price PriceFunc

	// Producer writes price.computed. Required.
	Producer Publisher

	// Consumer subscribes to odds.normalized and drives this service. Required
	// by Run; New does not need it, so a caller may build the handler alone for
	// a snapshot-only tool.
	Consumer Consumer

	// Snapshotter rebuilds priced state from the compacted price.computed topic.
	//
	// REQUIRED, and deliberately not optional: without it every deploy reprices
	// and republishes the entire slate, which is the failure the whole
	// change-detection path exists to prevent.
	Snapshotter Snapshot

	// Logger is required. A pricer that logs nowhere makes a skipped market
	// invisible.
	Logger *slog.Logger

	// Registry receives this package's collectors. Nil builds them unregistered,
	// which is right for a unit test. Ignored when Metrics is set.
	Registry prometheus.Registerer

	// Metrics is an already-registered collector set, for a process running more
	// than one Service. Takes precedence over Registry.
	Metrics *Metrics

	// EngineRevision identifies the engine's CONFIGURATION, so that changing it
	// reprices the board instead of silently leaving the old answers in place.
	//
	// # The hole it closes
	//
	// Suppression compares the SOURCE record's fingerprint, which identifies the
	// input and says nothing about the function applied to it. Change the devig
	// method or the reference-book preference and every market that has not moved
	// since keeps the price computed under the old configuration — for a futures
	// market, potentially for ever, because nothing will ever republish it. The
	// board would then be a mixture of two pricing models with no way to tell
	// which record came from which.
	//
	// Supplying a value that changes whenever the engine's output for a fixed
	// input would change turns that into a one-off reprice of the slate:
	// yesterday's ids no longer match, so every market is repriced exactly once
	// and then settles. Compaction absorbs it and the topic is last-write-wins by
	// key, so the cost is bounded and self-healing — the same trade the cold-start
	// path already makes.
	//
	// Empty is legal and means "suppress on the source fingerprint alone", which
	// is correct for a deployment whose engine configuration never changes and is
	// what a unit test wants. Longer than MaxEngineRevisionLen is rejected.
	EngineRevision string

	// FlushTimeout bounds the shutdown flush. Zero means DefaultFlushTimeout.
	FlushTimeout time.Duration

	// EngineTimeout bounds one engine call. Zero means DefaultEngineTimeout.
	EngineTimeout time.Duration

	// WarmStartAttempts bounds warm-start retries. Zero means
	// DefaultWarmStartAttempts.
	WarmStartAttempts int

	// Clock is the source of "now" for the staleness observations and the
	// pricing timer. Nil means time.Now.
	//
	// Injected because a test that asserts an exact staleness rather than a
	// bound needs it, and because CLAUDE.md §12 forbids reaching for a global.
	// NOTHING THE ENGINE SEES READS IT — every instant on a published record
	// comes from the source record, which came from the provider.
	Clock func() time.Time
}

func (o ServiceOptions) validate() error {
	switch {
	case o.Price == nil:
		return fmt.Errorf("%w: Price is nil; there is no default engine and a pricer that "+
			"published nothing would look exactly like a pricer whose bus was empty", ErrInvalidOptions)
	case o.Producer == nil:
		return fmt.Errorf("%w: Producer is nil", ErrInvalidOptions)
	case o.Snapshotter == nil:
		return fmt.Errorf("%w: Snapshotter is nil; priced state must survive a restart or every "+
			"deploy reprices and republishes the whole slate", ErrInvalidOptions)
	case o.Logger == nil:
		return fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	case o.FlushTimeout < 0:
		return fmt.Errorf("%w: FlushTimeout is negative", ErrInvalidOptions)
	case o.EngineTimeout < 0:
		return fmt.Errorf("%w: EngineTimeout is negative", ErrInvalidOptions)
	case o.WarmStartAttempts < 0:
		return fmt.Errorf("%w: WarmStartAttempts is negative", ErrInvalidOptions)
	case len(o.EngineRevision) > MaxEngineRevisionLen:
		return fmt.Errorf("%w: EngineRevision is %d bytes, limit is %d; it is concatenated onto a "+
			"64-character fingerprint to form the record's message id, which the bus caps at %d",
			ErrInvalidOptions, len(o.EngineRevision), MaxEngineRevisionLen, domain.MaxIDLen)
	}
	if strings.Contains(o.EngineRevision, engineRevisionSep) {
		return fmt.Errorf("%w: EngineRevision contains %q, which separates it from the fingerprint",
			ErrInvalidOptions, engineRevisionSep)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Priced state
// -----------------------------------------------------------------------------

// entry is what this service remembers about a market it has already priced.
//
// Both fields come off the ENVELOPE of the record it published — never out of
// the payload — which is what keeps this package independent of whatever the
// engine computes. See doc.go.
type entry struct {
	// source is the normalizer fingerprint of the odds.normalized record this
	// price was computed from, carried as kafka.Message.ID. Equality means the
	// INPUT has not changed.
	source string

	// observedAt is the provider observation instant of that record, propagated
	// unchanged. It is the monotonicity guard's comparand.
	observedAt time.Time
}

// -----------------------------------------------------------------------------
// Service
// -----------------------------------------------------------------------------

// Service consumes odds.normalized, prices every market that moved, and
// publishes to price.computed.
//
// It implements kafka.Handler and honours that interface's contract: idempotent
// (a redelivery re-derives an identical fingerprint and is suppressed by it),
// tombstone-aware, prompt, and it retains nothing from Delivery.Value past its
// return.
type Service struct {
	price     PriceFunc
	producer  Publisher
	consumer  Consumer
	snapshots Snapshot

	log     *slog.Logger
	metrics *Metrics
	clock   func() time.Time

	revision      string
	flushTimeout  time.Duration
	engineTimeout time.Duration

	// mu guards tracked. Records are delivered sequentially on the consumer's
	// goroutine, so the contention that matters is between Warm and the loop —
	// which the composition root orders, but which a caller could get wrong.
	mu      sync.Mutex
	tracked map[domain.MarketID]entry

	warmMu       sync.Mutex
	warmed       bool
	warmAttempts int
	maxWarm      int

	// running gates the readiness checker. It is false before Run has started
	// the consumer and false again once it has stopped, so /readyz reports the
	// truth during both a cold start and a drain rather than only in between.
	running atomic.Bool
}

// New validates the options and builds the service. It performs no I/O and
// starts nothing; call Warm and then Run.
func New(opts ServiceOptions) (*Service, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	m := opts.Metrics
	if m == nil {
		var err error
		if m, err = NewMetrics(opts.Registry); err != nil {
			return nil, fmt.Errorf("pricing: register metrics: %w", err)
		}
	}

	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	return &Service{
		price:         opts.Price,
		producer:      opts.Producer,
		consumer:      opts.Consumer,
		snapshots:     opts.Snapshotter,
		log:           opts.Logger.With(slog.String("component", "pricing.service")),
		metrics:       m,
		clock:         clock,
		revision:      opts.EngineRevision,
		flushTimeout:  positiveOr(opts.FlushTimeout, DefaultFlushTimeout),
		engineTimeout: positiveOr(opts.EngineTimeout, DefaultEngineTimeout),
		tracked:       make(map[domain.MarketID]entry),
		maxWarm:       positiveIntOr(opts.WarmStartAttempts, DefaultWarmStartAttempts),
	}, nil
}

// Metrics returns the collector set, so a process running several Services — or
// an engine in this package that wants to add families of its own — can share
// one registration.
func (s *Service) Metrics() *Metrics { return s.metrics }

// Name implements httpx.Checker. It is the key this dependency appears under in
// the /readyz payload.
func (s *Service) Name() string { return "pricer" }

// Check implements httpx.Checker: the service is ready when its consumer loop is
// running.
//
// It answers a question the bus checker does not. That one reports that the
// broker is reachable; this reports that THIS PROCESS is actually consuming. A
// replica whose consumer has exited but whose listener is still up would
// otherwise look healthy while pricing nothing, which is the shape of the
// phase-2 defect the contract ledger records: a probe that returns 200 for a
// dependency the binary never opened is worse than no probe at all.
func (s *Service) Check(context.Context) error {
	if !s.running.Load() {
		return ErrNotRunning
	}
	return nil
}

// -----------------------------------------------------------------------------
// Warm start
// -----------------------------------------------------------------------------

// Warm rebuilds priced state from the compacted price.computed topic.
//
// # What it reads, and what it deliberately does not
//
// The ENVELOPE only: kafka.Message.ID, which carries the normalizer fingerprint
// of the odds.normalized record each price was computed from, and
// kafka.Message.ObservedAt, the provider instant propagated unchanged. The
// payload is never decoded. doc.go argues why at length; the short form is that
// it makes this package independent of the engine's payload shape, so bumping
// the payload's schema cannot break the warm start on exactly the deploy where
// that would hurt most.
//
// # Caught up is an offset, not a timeout
//
// kafka.Snapshotter defines "done" as: for every partition, the next offset to
// read has reached the high watermark LISTED WHEN THE READ BEGAN. An empty
// partition, and one whose whole log has been deleted by retention, are complete
// immediately. Nothing here waits on a clock.
//
// # A failed warm start does not stop the pipeline
//
// After WarmStartAttempts failures the service prices COLD and says so at ERROR.
// That is the lesser of two evils: proceeding reprices the slate once — bounded,
// self-healing, and harmless downstream because price.computed is compacted and
// last-write-wins by key — whereas refusing means the priced board freezes for as
// long as the broker is unhappy, which is the failure nobody would choose
// deliberately.
//
// Calling it explicitly satisfies the once-only guard, so a composition root
// that warms eagerly does not pay for a second read on the first record. It is
// safe to call concurrently and it is idempotent.
func (s *Service) Warm(ctx context.Context) error {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	if s.warmed {
		return nil
	}
	s.warmAttempts++
	if err := s.warm(ctx); err != nil {
		return err
	}
	s.warmed = true
	return nil
}

// warm performs the read. The caller holds warmMu.
func (s *Service) warm(ctx context.Context) error {
	start := s.clock()

	var undecodable int
	stats, err := s.snapshots.Read(ctx, func(_ context.Context, d *kafka.Delivery) error {
		if !s.absorb(d) {
			undecodable++
		}
		// Always nil: one unreadable record on the compacted topic must not
		// abort the rebuild of every other market's state. The market it belongs
		// to simply reprices once.
		return nil
	})
	took := s.clock().Sub(start)
	if err != nil {
		s.metrics.observeWarmStart(warmStartFailed, took)
		return fmt.Errorf("pricing: warm start from %s: %w", kafka.TopicPriceComputed, err)
	}

	held := s.trackedLen()
	s.metrics.observeWarmStart(warmStartOK, took)
	s.metrics.observeTracked(held)

	s.log.Info("warm start complete",
		slog.Any("snapshot", stats),
		slog.Int("markets_tracked", held),
		slog.Int("unusable_records", undecodable),
	)
	return nil
}

// absorb folds one snapshot record into the tracker. It reports whether the
// record was usable.
func (s *Service) absorb(d *kafka.Delivery) bool {
	id, err := d.MarketID()
	if err != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if d.Tombstone {
		// A deleted market must NOT stay tracked: nothing else will ever
		// republish its price, so keeping the entry would let a later record for
		// the same key be suppressed against state the log no longer holds.
		delete(s.tracked, id)
		return true
	}

	// A record with no message id is one this build did not write — or wrote
	// before the id carried the fingerprint. It is kept out of the tracker
	// rather than stored with an empty source, because an empty source would
	// compare equal to nothing and the market would reprice anyway; leaving it
	// out says the same thing without pretending to know something.
	if d.Envelope.ID == "" {
		return false
	}

	s.tracked[id] = entry{source: d.Envelope.ID, observedAt: d.Envelope.ObservedAt}
	return true
}

// ensureWarm runs the warm start at most once, retrying a failure up to the
// attempt budget and then giving up loudly.
//
// It is the lazy path, kept correct so that a caller which forgets to warm
// eagerly still gets a warm start. The composition root calls Warm at startup
// precisely so this never fires — see doc.go on why paying for a snapshot read
// inside a handler blocks the whole group's rebalance.
func (s *Service) ensureWarm(ctx context.Context) {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	if s.warmed {
		return
	}
	s.warmAttempts++
	if err := s.warm(ctx); err != nil {
		if s.warmAttempts >= s.maxWarm {
			s.warmed = true
			s.log.Error("warm start failed and the attempt budget is exhausted; pricing COLD. "+
				"Every market on the slate will reprice and republish once, which is bounded and "+
				"self-healing, but the suppression ratio will read as zero until it settles",
				slog.Int("attempts", s.warmAttempts),
				slog.String("error", err.Error()),
			)
			return
		}
		s.log.Error("warm start failed; retrying on the next record",
			slog.Int("attempt", s.warmAttempts),
			slog.Int("budget", s.maxWarm),
			slog.String("error", err.Error()),
		)
		return
	}
	s.warmed = true
}

// -----------------------------------------------------------------------------
// Run
// -----------------------------------------------------------------------------

// Run consumes odds.normalized until ctx is cancelled or the consumer stops, and
// then flushes the producer.
//
// The shutdown order is doc.go's, and every step is load-bearing:
//
//  1. ctx cancellation stops the consumer polling for new records;
//  2. Consumer.Run drains the record already in the handler and commits the
//     offsets of everything it handled — including that one;
//  3. only once Run has returned is the producer flushed, on a context DETACHED
//     from the cancelled one. A flush racing a live handler flushes a buffer
//     still being filled; a flush on a cancelled context returns instantly with
//     the buffer intact, which is exactly the accepted-but-unwritten loss the
//     flush exists to prevent.
//
// The producer is flushed, never closed: this package does not own its lifetime.
// The composition root closes it, and kgo.Client.Close FAILS every still-buffered
// record, which is why this explicit flush comes first rather than being left to
// a deferred close.
func (s *Service) Run(ctx context.Context) error {
	if s.consumer == nil {
		return ErrNotRunnable
	}

	s.log.Info("pricer running",
		slog.String("consumes", kafka.TopicOddsNormalized),
		slog.String("publishes", kafka.TopicPriceComputed),
		slog.String("group", GroupPricer),
		slog.String("message_type", MessageType),
		slog.String("engine_timeout", s.engineTimeout.String()),
	)

	s.running.Store(true)
	err := s.consumer.Run(ctx, s)
	s.running.Store(false)
	s.log.Info("pricer consumer stopped")

	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.flushTimeout)
	defer cancel()
	if ferr := s.producer.Flush(flushCtx); ferr != nil {
		s.log.Error("final producer flush failed; prices accepted but not written are lost",
			slog.String("error", ferr.Error()))
		return errors.Join(err, fmt.Errorf("pricing: final flush: %w", ferr))
	}
	s.log.Info("producer flushed; every accepted price is written")
	return err
}

// -----------------------------------------------------------------------------
// Handling one record
// -----------------------------------------------------------------------------

// HandleMessage prices one odds.normalized record and returns only once the
// result is acknowledged by the broker.
//
// It implements kafka.Handler. The Consumer commits the last successfully
// handled record per partition, so a nil return here is a claim of durability:
// every path that returns nil has either had its publish acknowledged or has
// deliberately published nothing.
//
// # Errors and what the Consumer does with them
//
// A returned error leaves the offset uncommitted, so the record is redelivered.
// Only TRANSIENT failures are returned — a publish or a tombstone the broker
// refused — because those are the ones redelivery can fix. A PERMANENT failure
// (a payload that cannot be decoded, an engine that refuses the market) returns
// nil after counting and logging it: retrying cannot change the bytes on disk,
// and the alternative is one poison market halting every other market on the
// partition. That is the same call internal/platform/kafka's ErrorPolicySkip
// note blesses for the odds path specifically, "because the next provider poll
// republishes the same market".
func (s *Service) HandleMessage(ctx context.Context, d *kafka.Delivery) error {
	if d == nil {
		return fmt.Errorf("pricing: nil delivery")
	}
	s.ensureWarm(ctx)

	// Typed rather than parsed from d.Key: Delivery.MarketID checks the topic's
	// declared key kind, so asking odds.normalized for an EventID fails instead
	// of returning a plausible identifier of the wrong sort.
	id, err := d.MarketID()
	if err != nil {
		s.metrics.observeMarket(resultInvalid)
		s.log.Error("record rejected: unusable market key; redelivery cannot change the outcome",
			slog.Any("record", d),
			slog.Bool("permanent", true),
			slog.String("error", err.Error()),
		)
		return nil
	}

	if d.Tombstone {
		return s.handleTombstone(ctx, d, id)
	}

	if d.Envelope.Type != normalizer.MessageType {
		// Counted rather than ignored, because it means something this build
		// does not read is writing the topic. Skipped rather than failed:
		// decoding it would misparse rather than error.
		s.metrics.observeMarket(resultInvalid)
		s.log.Warn("skipping a record whose envelope this build does not read",
			slog.Any("record", d),
			slog.String("want", normalizer.MessageType),
			slog.String("error", ErrUnsupportedMessage.Error()),
		)
		return nil
	}

	var rec normalizer.NormalizedMarket
	if err := d.Unmarshal(&rec); err != nil {
		s.metrics.observeMarket(resultInvalid)
		s.log.Error("record rejected: payload could not be decoded; redelivery cannot change the outcome",
			slog.Any("record", d),
			slog.Bool("permanent", true),
			slog.String("error", err.Error()),
		)
		return nil
	}

	if skip, result := s.shouldSkip(id, rec); skip {
		s.metrics.observeMarket(result)
		return nil
	}

	priced, err := s.compute(ctx, rec)
	if err != nil {
		// An engine failure is PERMANENT by contract: nothing in the odds
		// mathematics does I/O, so the same record produces the same refusal for
		// ever. Halting the partition over one unpriceable market would freeze
		// every other market on it to preserve that one.
		s.metrics.observeMarket(resultInvalid)
		s.log.Error("market could not be priced; redelivery cannot change the outcome",
			slog.Any("record", d),
			slog.String("market", id.String()),
			slog.Bool("permanent", true),
			slog.String("error", err.Error()),
		)
		return nil
	}

	// SYNCHRONOUS. Returning before the acknowledgement would let this record's
	// offset commit ahead of a price that never reached the broker; see doc.go.
	//
	// ObservedAt is the PROVIDER's instant, propagated unchanged — no hop
	// re-stamps it, which is what makes the staleness SLO measurable at fanout.
	// ID is the SOURCE record's fingerprint, which is the whole of this service's
	// change-detection state and is why the warm start never decodes a payload.
	if err := s.producer.PublishPrice(ctx, id, kafka.Message{
		Type:       MessageType,
		ID:         s.sourceID(rec.Fingerprint),
		ObservedAt: rec.ObservedAt,
		Payload:    priced,
	}); err != nil {
		s.metrics.observeMarket(resultFailed)
		s.log.Error("publishing a priced market failed; the offset is uncommitted and the record "+
			"will be redelivered",
			slog.Any("record", d),
			slog.String("market", id.String()),
			slog.Bool("permanent", false),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("pricing: publish market %s: %w", id, err)
	}

	s.remember(id, entry{source: s.sourceID(rec.Fingerprint), observedAt: rec.ObservedAt})
	s.metrics.observeMarket(resultPublished)
	s.metrics.observePriced(rec, s.clock())
	s.metrics.observeTracked(s.trackedLen())
	return nil
}

// handleTombstone propagates a deletion from odds.normalized to price.computed.
//
// # Why it is unconditional
//
// The tracker cannot prove absence. It is a fold of price.computed as of THIS
// replica's warm start plus what this replica has published since, so a market
// priced by another replica after that instant and then reassigned here by a
// rebalance is genuinely absent from it while a live value sits in the log.
// Suppressing on absence would leave that value in the compacted snapshot for
// ever, because no further record for the key is coming and compaction never
// collapses a key it only ever saw once. A duplicate tombstone, by contrast,
// costs one valueless record that the broker collects after
// delete.retention.ms. The asymmetry decides it.
func (s *Service) handleTombstone(ctx context.Context, d *kafka.Delivery, id domain.MarketID) error {
	ts := kafka.Tombstone{
		Reason:      tombstoneReason,
		Acknowledge: kafka.AcknowledgeDeletesKeyFromSnapshot,
	}
	// The instant the deletion was decided upstream, where the source carried
	// one. Propagated rather than re-stamped, for the same reason every other
	// instant on this path is.
	if at, ok := d.ObservedAt(); ok {
		ts.ObservedAt = at
	}

	if err := s.producer.TombstonePrice(ctx, id, ts); err != nil {
		s.metrics.observeMarket(resultFailed)
		s.log.Error("tombstoning a priced market failed; the offset is uncommitted and the record "+
			"will be redelivered",
			slog.Any("record", d),
			slog.String("market", id.String()),
			slog.Bool("permanent", false),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("pricing: tombstone market %s: %w", id, err)
	}

	s.forget(id)
	s.metrics.observeMarket(resultTombstoned)
	s.metrics.observeTracked(s.trackedLen())
	return nil
}

// shouldSkip applies the two guards that keep a compacted topic honest, and
// reports which one fired.
//
// Order matters. The monotonicity guard runs FIRST: an older observation with a
// different fingerprint would otherwise be "published" and would make the newest
// record for the key the older state. Fingerprint equality is checked second and
// is the ordinary steady-state outcome.
func (s *Service) shouldSkip(id domain.MarketID, rec normalizer.NormalizedMarket) (bool, string) {
	s.mu.Lock()
	held, ok := s.tracked[id]
	s.mu.Unlock()

	if !ok {
		return false, ""
	}

	if !held.observedAt.IsZero() && rec.ObservedAt.Before(held.observedAt) {
		s.log.Debug("skipping an observation older than the priced state",
			slog.String("market", id.String()),
			slog.Time("observed_at", rec.ObservedAt),
			slog.Time("priced_observed_at", held.observedAt),
			slog.String("reason", ErrStaleRecord.Error()),
		)
		return true, resultStale
	}

	// Identical input AND identical engine configuration. Pricing is a pure
	// function of the record (see PriceFunc), so the output would be identical too
	// and the publish would be a no-op on a compacted topic — the same argument
	// CLAUDE.md §5 makes one hop upstream.
	//
	// An empty fingerprint is never equal to anything: a record that carries none
	// is repriced rather than assumed unchanged.
	if id := s.sourceID(rec.Fingerprint); id != "" && held.source == id {
		return true, resultSuppressed
	}
	return false, ""
}

// sourceID composes the record's dedup identity: the source fingerprint, plus
// the engine revision when one is configured.
//
// It is what kafka.Message.ID carries and what the warm start reads back, so the
// two sides of the comparison are built by the same function and cannot drift.
// An empty fingerprint composes to the empty string rather than to a bare
// revision, because a record with no fingerprint has no input identity and must
// be repriced rather than matched against every other record that also lacked
// one.
//
// The source fingerprint is ALSO on the payload as ComputedMarket's
// SourceFingerprint, so a downstream consumer that wants the normalizer's hash
// reads it there and is unaffected by what this service puts in the envelope.
func (s *Service) sourceID(fingerprint string) string {
	if fingerprint == "" || s.revision == "" {
		return fingerprint
	}
	return fingerprint + engineRevisionSep + s.revision
}

// compute calls the engine under its own deadline and times it.
//
// The deadline is imposed here rather than trusted to the engine because an
// engine that spun would not merely be slow: it would block the group's
// rebalance until this member was fenced, and its partitions would be
// redelivered to someone else. See DefaultEngineTimeout.
//
// The timer covers the engine call alone — not the decode before it and not the
// publish after it — because sharpline_pricing_duration_seconds is what
// PricingLatencyHigh pages on, and a broker hiccup must not read as slow
// arithmetic.
func (s *Service) compute(ctx context.Context, rec normalizer.NormalizedMarket) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, s.engineTimeout)
	defer cancel()

	start := s.clock()
	priced, err := s.price(ctx, rec)
	s.metrics.observeDuration(s.clock().Sub(start))
	if err != nil {
		return nil, err
	}
	if priced == nil {
		// kafka.Message would reject this as ErrEmptyPayload one layer down, but
		// the message there is about an empty payload rather than about an engine
		// that returned nothing, and the distinction is the difference between
		// five minutes and an hour.
		return nil, fmt.Errorf("pricing: engine returned no payload for market %s", rec.Market.ID)
	}
	return priced, nil
}

// -----------------------------------------------------------------------------
// Tracker access
// -----------------------------------------------------------------------------

func (s *Service) remember(id domain.MarketID, e entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tracked[id] = e
}

func (s *Service) forget(id domain.MarketID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tracked, id)
}

func (s *Service) trackedLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tracked)
}

// positiveOr returns d when it is positive and fallback otherwise.
func positiveOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// positiveIntOr returns n when it is positive and fallback otherwise.
func positiveIntOr(n, fallback int) int {
	if n > 0 {
		return n
	}
	return fallback
}
