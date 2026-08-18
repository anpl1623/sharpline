// The orchestrator: odds.raw.{provider} in, odds.normalized out, with change
// detection in between.
//
// CLAUDE.md §3's flow, and §5's obligation:
//
//	provider → ingest → [odds.raw.{provider}] → normalizer → [odds.normalized]
//
//	"Hash each normalized market to suppress no-op updates — most polls return
//	 identical data and must not generate bus traffic."
//
// # What is hashed, stated once and exactly
//
// THE NORMALIZED MARKET, never the raw payload, and everything on the published
// record EXCEPT the fingerprint field itself and the three observation and
// ingestion instants. fingerprint.go owns the encoding and enumerates the
// exclusions; fingerprint_test.go walks NormalizedMarket by reflection and fails
// the build if a field is neither excluded by name nor able to move the hash.
//
// Hashing the raw bytes instead would suppress nothing, because a provider
// response differs on a response-level timestamp, on bookmakers this build does
// not map, and on JSON key ordering after a re-serialisation. That failure looks
// like it is working: the pipeline runs, the board is correct, and the only
// symptom is that the suppression ratio is zero.
//
// # The four outcomes of one market
//
//	published   the fingerprint differs — a real move
//	suppressed  the fingerprint is identical and the refresh ceiling has not elapsed
//	refreshed   identical, but past the ceiling, so republished anyway
//	stale       observed strictly before the state already published; skipped
//
// `stale` is the monotonicity guard. odds.normalized is compacted, so publishing
// an older observation makes the LATEST record for that key the OLDER state, and
// every consumer that builds its snapshot from the log then serves it. Kafka
// orders per key and odds.raw.{provider} is keyed by event, so this only fires
// on a redelivery or on two pollers racing — both of which happen.
//
// # Publishing is SYNCHRONOUS, and that is load-bearing
//
// internal/platform/kafka's Consumer commits the last successfully handled
// record per partition. A handler that returned before its produce was
// acknowledged would let the raw offset commit ahead of a record that never
// reached the broker, and the market would simply be missing after the next
// restart, with nothing anywhere reporting a loss.
// kafka.OddsProducer.PublishNormalized waits; PublishNormalizedAsync does not.
package normalizer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// RawMessageType is the kafka.Message.Type this package reads on
// odds.raw.{provider}.
//
// It restates internal/ingest's constant of the same name rather than importing
// it: internal/ingest is this package's PARENT and its composition root, and a
// child importing its own composition root is one refactor away from an import
// cycle. The coupling is a wire contract either way, so it is written down and
// tested (normalizer_test.go asserts the two strings agree) instead of being
// enforced by the type system in a way that constrains the dependency graph.
//
// internal/ingest is explicit about what a bump means: "the FRAME changed (the
// data stopped being the provider's verbatim bytes). A provider changing its own
// payload shape does not touch this." So a record carrying any other type is not
// something a Decoder should be pointed at — decoding it would misparse rather
// than fail.
const RawMessageType = "odds.raw.v1"

// Defaults. Each is overridable through Options; zero means the default.
const (
	// DefaultRefreshAfter is the suppression ceiling: an unchanged market is
	// republished once this long has passed since its last publication.
	//
	// # Why there is a ceiling at all
	//
	// Two things, both bounded costs bought with a trickle of bus traffic.
	// First, any defect in the fingerprint self-heals within one interval
	// instead of persisting until the market next moves — and "until the market
	// next moves" is unbounded for a futures market. Second, the record's
	// observation instant cannot drift arbitrarily far behind the provider's.
	//
	// # Why it is minutes and not seconds
	//
	// Suppression only saves anything when the ceiling is comfortably ABOVE the
	// poll cadence. ADR 0003 buys a 90-second live cadence, so a 90-second
	// ceiling would republish on every single poll and the mechanism would
	// cost bus traffic while saving none. Five minutes is a little over three
	// live polls: high enough that the steady state is genuinely suppressed,
	// low enough that a stuck market is visible within one dashboard refresh.
	//
	// # The interaction with the freshness SLO, stated plainly
	//
	// Suppression trades observation freshness for bus traffic BY DESIGN. The
	// Odds API advances last_update on every refresh whether or not a price
	// moved, so a market that has not moved in five minutes carries a
	// five-minute-old observed_at on the compacted topic even though nothing
	// about it is wrong. That does not break SLO 1, because the SLO is measured
	// at FANOUT — at the instant a price is written to a client socket — and a
	// static market produces no delta to fan out. It does show up in the
	// snapshot a newly-connected client receives, which is the honest reading:
	// that is genuinely the last time the price was observed to be what it is.
	//
	// A deployment that retunes the poll cadence must retune this with it.
	DefaultRefreshAfter = 5 * time.Minute

	// DefaultWarmStartAttempts bounds how many times a failing warm start is
	// retried before the process proceeds cold. See Normalizer.Warm.
	DefaultWarmStartAttempts = 3
)

// Publisher is the half of kafka.OddsProducer this package uses.
//
// Declared here, by the consumer, per CLAUDE.md §12, and kept to one method:
// this package writes exactly one topic and has no business being handed an API
// that can write the others. *kafka.OddsProducer satisfies it.
//
// PublishNormalized is the SYNCHRONOUS form deliberately; see the file comment.
type Publisher interface {
	PublishNormalized(ctx context.Context, id domain.MarketID, msg kafka.Message) error
}

// Snapshot is the half of kafka.Snapshotter this package uses.
//
// Read streams a compacted topic from the beginning to the end offsets listed
// when the read began, which is what makes "caught up" a definite condition
// rather than a timeout — the property the whole warm-start argument rests on.
// *kafka.Snapshotter satisfies it.
type Snapshot interface {
	Read(ctx context.Context, fn func(context.Context, *kafka.Delivery) error) (kafka.SnapshotStats, error)
}

// Options configures a Normalizer.
type Options struct {
	// Provider is the adapter slug whose raw topic this instance consumes. It
	// selects the topic name, seeds every derived identifier, and is stamped on
	// every published record so a consumer can tell a synthetic quote from a
	// real one.
	Provider kafka.Provider

	// Decoder is the per-provider syntax layer. Its Provider() must equal
	// Provider — that check is the one thing standing between a deployment and
	// running one provider's payloads through the other's parser, which does not
	// fail, it just produces wrong numbers.
	Decoder Decoder

	// Producer writes odds.normalized. Required.
	Producer Publisher

	// Snapshotter rebuilds change-detection state from odds.normalized at
	// startup. REQUIRED, and deliberately not optional: without it the first
	// poll after every deploy republishes the entire board, which is the failure
	// this whole file exists to prevent.
	Snapshotter Snapshot

	// Store holds the fingerprints. Nil means an in-process MemoryStore.
	Store FingerprintStore

	// Logger is required.
	Logger *slog.Logger

	// Registry receives this package's collectors. Nil builds them
	// unregistered, which is right for a unit test.
	Registry prometheus.Registerer

	// RefreshAfter is the suppression ceiling. Zero means DefaultRefreshAfter;
	// negative is rejected. A deployment that wants no ceiling has to say so by
	// setting a very large value rather than by leaving a field blank.
	RefreshAfter time.Duration

	// SlugNamespace prefixes derived slugs. See MapperOptions.SlugNamespace.
	SlugNamespace string

	// WarmStartAttempts bounds warm-start retries. Zero means
	// DefaultWarmStartAttempts.
	WarmStartAttempts int

	// Clock is the source of "now" for the refresh ceiling and for the staleness
	// observations. Nil means time.Now.
	//
	// It is injected because the refresh ceiling is otherwise untestable without
	// a sleep, and a test that sleeps for five minutes is a test nobody runs.
	// NOTHING IN THE MAPPING READS IT — mapper.go is a pure function of the
	// payload, and the only instants that reach a record come from the provider.
	Clock func() time.Time
}

func (o Options) validate() error {
	switch {
	case o.Decoder == nil:
		return fmt.Errorf("%w: Decoder is nil", ErrInvalidOptions)
	case o.Producer == nil:
		return fmt.Errorf("%w: Producer is nil", ErrInvalidOptions)
	case o.Snapshotter == nil:
		return fmt.Errorf("%w: Snapshotter is nil; change-detection state must survive a restart or "+
			"the first poll after every deploy republishes the whole board", ErrInvalidOptions)
	case o.Logger == nil:
		return fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	case o.RefreshAfter < 0:
		return fmt.Errorf("%w: RefreshAfter is negative", ErrInvalidOptions)
	case o.WarmStartAttempts < 0:
		return fmt.Errorf("%w: WarmStartAttempts is negative", ErrInvalidOptions)
	}
	p, err := kafka.NewProvider(string(o.Provider))
	if err != nil {
		return fmt.Errorf("%w: provider: %w", ErrInvalidOptions, err)
	}
	if got := o.Decoder.Provider(); got != p {
		return fmt.Errorf("%w: %w: decoder handles %q, this normalizer consumes %q",
			ErrInvalidOptions, ErrUnsupportedProvider, got, p)
	}
	return nil
}

// Normalizer maps raw provider payloads onto the domain and decides which of
// them reach the bus.
//
// It implements kafka.Handler and honours that interface's contract:
// idempotent (a redelivery re-derives identical identifiers and is suppressed by
// its own fingerprint), tombstone-aware, prompt, and it retains nothing from
// Delivery.Value past its return.
type Normalizer struct {
	prov        kafka.Provider
	rawTopic    string
	decoder     Decoder
	mapper      *Mapper
	producer    Publisher
	snapshotter Snapshot
	store       FingerprintStore

	log     *slog.Logger
	m       *Metrics
	clock   func() time.Time
	refresh time.Duration

	warmMu       sync.Mutex
	warmed       bool
	warmAttempts int
	maxWarm      int
}

// New builds a normalizer.
//
// It does NO I/O: the warm start needs a context and this constructor has none,
// so it happens on the first delivered record (or on an explicit Warm call).
// See Warm for the argument.
func New(o Options) (*Normalizer, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	prov, err := kafka.NewProvider(string(o.Provider))
	if err != nil {
		return nil, fmt.Errorf("%w: provider: %w", ErrInvalidOptions, err)
	}
	topic, err := kafka.OddsRaw(prov)
	if err != nil {
		return nil, fmt.Errorf("%w: raw topic: %w", ErrInvalidOptions, err)
	}
	mapper, err := NewMapper(MapperOptions{Provider: prov, SlugNamespace: o.SlugNamespace})
	if err != nil {
		return nil, err
	}
	metrics, err := NewMetrics(o.Registry)
	if err != nil {
		return nil, err
	}

	store := o.Store
	if store == nil {
		store = NewMemoryStore()
	}
	clock := o.Clock
	if clock == nil {
		clock = time.Now
	}

	return &Normalizer{
		prov:        prov,
		rawTopic:    topic.Name(),
		decoder:     o.Decoder,
		mapper:      mapper,
		producer:    o.Producer,
		snapshotter: o.Snapshotter,
		store:       store,
		log: o.Logger.With(
			slog.String("component", "normalizer"),
			slog.String("provider", prov.String()),
		),
		m:       metrics,
		clock:   clock,
		refresh: positiveOr(o.RefreshAfter, DefaultRefreshAfter),
		maxWarm: positiveIntOr(o.WarmStartAttempts, DefaultWarmStartAttempts),
	}, nil
}

// RawTopic returns the topic this normalizer consumes.
func (n *Normalizer) RawTopic() string { return n.rawTopic }

// Warm rebuilds change-detection state from odds.normalized.
//
// # Why it is not in the constructor
//
// It does I/O, and CLAUDE.md §12 puts a context.Context first on anything that
// does. A constructor with no context that opened a network read on
// context.Background would be exactly the shape that rule exists to forbid, so
// the read is deferred to the first record — where a context is in hand, and
// where it is bounded by the snapshotter's own timeout (60s by default) rather
// than by nothing.
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
// After WarmStartAttempts failures the process normalizes COLD and says so at
// ERROR. That is deliberate and it is the lesser of two evils: proceeding
// republishes the slate once — bounded, self-healing, and harmless downstream
// because the compacted topic is last-write-wins by key and the prices
// hypertable absorbs duplicates on its natural key — whereas refusing means the
// board freezes for as long as the broker is unhappy, which is the failure
// nobody would choose deliberately.
// Calling it explicitly satisfies the once-only guard, so a caller that warms
// eagerly does not pay for a second read on the first record.
func (n *Normalizer) Warm(ctx context.Context) error {
	n.warmMu.Lock()
	defer n.warmMu.Unlock()
	n.warmAttempts++
	if err := n.warm(ctx); err != nil {
		return err
	}
	n.warmed = true
	return nil
}

// warm performs the read. The caller holds warmMu.
func (n *Normalizer) warm(ctx context.Context) error {
	start := n.clock()

	var (
		mismatches int
		decodes    int
	)
	stats, err := n.snapshotter.Read(ctx, func(ctx context.Context, d *kafka.Delivery) error {
		bad, mismatch := n.absorb(ctx, d)
		decodes += bad
		mismatches += mismatch
		// Always nil: a single unreadable record on the compacted topic must not
		// abort the rebuild of every other market's state. It is counted as a
		// reject, and the market it belongs to simply republishes once.
		return nil
	})
	took := n.clock().Sub(start)
	if err != nil {
		n.m.observeWarmStart(warmStartFailed, took, mismatches)
		return fmt.Errorf("normalizer: warm start from %s: %w", kafka.TopicOddsNormalized, err)
	}

	held, _ := n.store.Len(ctx)
	n.m.observeWarmStart(warmStartOK, took, mismatches)
	n.m.observeHeld(held)

	attrs := []any{
		slog.Any("snapshot", stats),
		slog.Int("fingerprints", held),
		slog.Int("undecodable", decodes),
		slog.Int("mismatched", mismatches),
	}
	if mismatches > 0 {
		// Loud, because it means this build's hash disagrees with the one that
		// wrote the topic. Every affected market republishes once and then
		// settles, so it is self-correcting — but a silent self-correction that
		// republishes the slate is indistinguishable from a broken fingerprint.
		n.log.Warn("warm start recomputed a different fingerprint than the producer stored; "+
			"the hash or the payload shape changed without SchemaVersion being bumped", attrs...)
	} else {
		n.log.Info("warm start complete", attrs...)
	}
	return nil
}

// absorb folds one snapshot record into the store. It returns (undecodable,
// mismatched) as counts so the caller can total them.
func (n *Normalizer) absorb(ctx context.Context, d *kafka.Delivery) (int, int) {
	id, err := d.MarketID()
	if err != nil {
		n.m.observeReject(reject(ScopeMarket, ReasonInvalidIdentifier, d.Key, err))
		return 1, 0
	}

	if d.Tombstone {
		// A deleted market must NOT stay suppressed: nothing else will ever
		// republish it, so ignoring the tombstone would make the deletion
		// permanent in this process's view while the market carried on trading.
		if err := n.store.Delete(ctx, id); err != nil {
			n.log.Warn("forgetting a tombstoned market failed",
				slog.String("market", id.String()), slog.String("error", err.Error()))
		}
		return 0, 0
	}

	var rec NormalizedMarket
	if err := d.Unmarshal(&rec); err != nil {
		n.m.observeReject(reject(ScopeMarket, ReasonDecode, d.Key, err))
		return 1, 0
	}

	// The RECOMPUTED hash is what gets stored, not the one the record carries.
	// The comparison this store feeds is against a hash this build computes, so
	// storing the producer's value would compare two different functions and
	// suppress or republish on the difference between them rather than on a
	// change to the market. Carrying the stored value at all is what makes the
	// disagreement VISIBLE, which is the only reason the field is on the wire.
	fp := rec.Hash()
	mismatch := 0
	if rec.Fingerprint != "" && Fingerprint(rec.Fingerprint) != fp {
		mismatch = 1
	}

	if err := n.store.Store(ctx, id, Entry{
		Fingerprint: fp,
		ObservedAt:  rec.ObservedAt,
		// The ORIGINAL publication instant, not the moment this process read it.
		// Using the read instant would restart the refresh ceiling on every
		// deploy, and the ceiling exists precisely to bound how long a defect can
		// persist across them.
		PublishedAt: d.Envelope.ProducedAt,
	}); err != nil {
		n.log.Warn("recording a warm-start fingerprint failed",
			slog.String("market", id.String()), slog.String("error", err.Error()))
	}
	return 0, mismatch
}

// ensureWarm runs the warm start at most once, retrying a failure up to the
// attempt budget and then giving up loudly.
func (n *Normalizer) ensureWarm(ctx context.Context) {
	n.warmMu.Lock()
	defer n.warmMu.Unlock()
	if n.warmed {
		return
	}
	n.warmAttempts++
	if err := n.warm(ctx); err != nil {
		if n.warmAttempts >= n.maxWarm {
			n.warmed = true
			n.log.Error("warm start failed and the attempt budget is exhausted; normalizing COLD. "+
				"Every market on the slate will republish once, which is bounded and self-healing, "+
				"but the suppression ratio will read as zero until it settles",
				slog.Int("attempts", n.warmAttempts),
				slog.String("error", err.Error()),
			)
			return
		}
		n.log.Error("warm start failed; retrying on the next record",
			slog.Int("attempt", n.warmAttempts),
			slog.Int("budget", n.maxWarm),
			slog.String("error", err.Error()),
		)
		return
	}
	n.warmed = true
}

// HandleMessage implements kafka.Handler for one odds.raw.{provider} record.
func (n *Normalizer) HandleMessage(ctx context.Context, d *kafka.Delivery) error {
	if d == nil {
		return fmt.Errorf("normalizer: nil delivery")
	}
	if d.Topic != n.rawTopic {
		// A wiring error, not a data error: this normalizer holds one provider's
		// decoder and cannot give another provider's bytes meaning.
		return fmt.Errorf("normalizer: %w: record from %s, this normalizer consumes %s",
			ErrUnsupportedProvider, d.Topic, n.rawTopic)
	}

	n.ensureWarm(ctx)

	if d.Tombstone {
		// odds.raw.{provider} is retention-based and this pipeline never writes
		// one. Counted rather than ignored, because it means something else did.
		n.m.observeRecord(recordTombstone)
		return nil
	}
	if d.Envelope.Type != RawMessageType {
		n.m.observeRecord(recordUnsupported)
		n.m.observeReject(reject(ScopeEvent, ReasonUnsupportedMessage, d.Envelope.Type, nil))
		n.log.Warn("skipping a raw record whose envelope this build does not read",
			slog.Any("delivery", d), slog.String("want", RawMessageType))
		return nil
	}

	started := n.clock()
	raw, err := n.decoder.Decode(d.Envelope.Data)
	if err != nil {
		n.m.observeRecord(recordRejected)
		n.m.observeReject(reject(ScopeEvent, ReasonDecode, d.Key, err))
		n.log.Warn("decoding a raw payload failed",
			slog.Any("delivery", d), slog.String("error", err.Error()))
		return nil
	}

	res, err := n.mapper.Map(raw)
	n.m.observeMapping(n.clock().Sub(started))
	for _, r := range res.Rejects {
		n.m.observeReject(r)
	}
	if err != nil {
		n.m.observeRecord(recordRejected)
		var r Reject
		if errors.As(err, &r) {
			n.m.observeReject(r)
		}
		n.log.Warn("an event payload could not be normalized",
			slog.Any("delivery", d), slog.String("error", err.Error()))
		return nil
	}
	if len(res.Views) == 0 {
		n.m.observeRecord(recordRejected)
		n.log.Debug("an event payload yielded no publishable market",
			slog.Any("delivery", d),
			slog.Int("rejects", len(res.Rejects)),
			slog.String("reason", ErrNothingNormalized.Error()),
		)
		return nil
	}
	n.m.observeRecord(recordMapped)

	// The raw envelope's ProducedAt is when `ingest` received this payload.
	// payload.go: "IngestedAt is when ingest received the payload this record was
	// derived from — the raw envelope's ProducedAt, propagated unchanged."
	ingestedAt := d.Envelope.ProducedAt

	for _, view := range res.Views {
		if err := n.emit(ctx, view, ingestedAt); err != nil {
			return err
		}
	}
	if held, err := n.store.Len(ctx); err == nil {
		n.m.observeHeld(held)
	}
	return nil
}

// emit applies change detection to one market and publishes it when it moved.
func (n *Normalizer) emit(ctx context.Context, view MarketView, ingestedAt time.Time) error {
	rec := newRecord(n.prov.String(), view, ingestedAt)
	fp := rec.Hash()
	rec.Fingerprint = fp.String()

	id, err := rec.MarketID()
	if err != nil {
		// Unreachable through the mapper, which builds the identifier with
		// domain.NewMarketID before the market exists. Counted rather than
		// asserted, because "unreachable" is a claim about today's code.
		n.m.observeReject(reject(ScopeMarket, ReasonInvalidIdentifier, rec.Market.ID, err))
		return nil
	}

	entry, held, err := n.store.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("normalizer: load fingerprint for %s: %w", id, err)
	}

	result := resultPublished
	if held {
		if !entry.ObservedAt.IsZero() && rec.ObservedAt.Before(entry.ObservedAt) {
			// Publishing would make the newest record for this key the OLDER
			// state, and every consumer that builds a snapshot from the log would
			// then serve it.
			n.m.observeMarket(resultStale)
			n.log.Debug("skipping an observation older than the published state",
				slog.String("market", id.String()),
				slog.Time("observed_at", rec.ObservedAt),
				slog.Time("published_observed_at", entry.ObservedAt),
				slog.String("reason", ErrStaleObservation.Error()),
			)
			return nil
		}
		if entry.Fingerprint == fp {
			if !n.pastRefresh(entry.PublishedAt) {
				// THE outcome CLAUDE.md §5 exists to produce.
				n.m.observeMarket(resultSuppressed)
				return nil
			}
			result = resultRefreshed
		}
	}

	if err := n.producer.PublishNormalized(ctx, id, kafka.Message{
		Type: MessageType,
		// The fingerprint doubles as the message id, so a consumer that needs to
		// deduplicate across a redelivery has one without rehashing the payload.
		ID:         rec.Fingerprint,
		ObservedAt: rec.ObservedAt,
		Payload:    rec,
	}); err != nil {
		n.m.observeMarket(resultFailed)
		// Returned, not swallowed: the raw offset must not commit ahead of a
		// record that never reached the broker. Markets already published from
		// this payload are suppressed on the redelivery by their own
		// fingerprints, so the retry costs one produce per market that failed.
		return fmt.Errorf("normalizer: publish market %s: %w", id, err)
	}

	now := n.clock()
	if err := n.store.Store(ctx, id, Entry{
		Fingerprint: fp,
		ObservedAt:  rec.ObservedAt,
		PublishedAt: now,
	}); err != nil {
		return fmt.Errorf("normalizer: record fingerprint for %s: %w", id, err)
	}
	n.m.observeMarket(result)
	n.m.observePublished(rec, now)
	return nil
}

// pastRefresh reports whether an unchanged market is due a republication.
//
// A zero publication instant means the entry came from a record with no
// ProducedAt, which no build of this pipeline writes; treating it as due is the
// safe reading, because the alternative suppresses the market for ever.
func (n *Normalizer) pastRefresh(publishedAt time.Time) bool {
	if publishedAt.IsZero() {
		return true
	}
	return n.clock().Sub(publishedAt) >= n.refresh
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

var _ kafka.Handler = (*Normalizer)(nil)
