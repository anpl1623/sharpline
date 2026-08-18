// Tests for the SERVICE half of this package: the consume → price → publish
// loop, the two guards that keep a compacted topic honest, the tombstone
// propagation, the warm start, the readiness contract and the shutdown flush.
//
// engine_test.go covers the arithmetic. Nothing here asserts a probability.
//
// # No canned records
//
// Every market these tests feed the service is built by harness_test.go's
// marketFixture, which turns a chosen probability vector into the record shape
// odds.normalized carries by running the synthetic generator's own margin
// relation. The contract ledger's NO MOCK DATA rule forbids a hand-written
// market snapshot, and none appears below.
//
// The bus, by contrast, IS faked, and deliberately: the three seams
// service.go declares (Publisher, Consumer, Snapshot) exist so that the
// commit-boundary and shutdown behaviour can be asserted exactly — a real broker
// makes "did the flush happen before the close" a timing question rather than an
// observation. The integration tier drives the same code against a real Kafka.
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Fakes for the three consumer-declared seams
// -----------------------------------------------------------------------------

// publishedPrice is one accepted publication.
type publishedPrice struct {
	market     domain.MarketID
	msgType    string
	id         string
	observedAt time.Time
	payload    any
}

// fakePublisher records what reached the bus.
type fakePublisher struct {
	published  []publishedPrice
	tombstoned []domain.MarketID
	reasons    []string
	flushes    int

	publishErr   error
	tombstoneErr error
	flushErr     error
}

func (p *fakePublisher) PublishPrice(_ context.Context, id domain.MarketID, msg kafka.Message) error {
	if p.publishErr != nil {
		return p.publishErr
	}
	p.published = append(p.published, publishedPrice{
		market: id, msgType: msg.Type, id: msg.ID, observedAt: msg.ObservedAt, payload: msg.Payload,
	})
	return nil
}

func (p *fakePublisher) TombstonePrice(_ context.Context, id domain.MarketID, ts kafka.Tombstone) error {
	if p.tombstoneErr != nil {
		return p.tombstoneErr
	}
	if ts.Acknowledge != kafka.AcknowledgeDeletesKeyFromSnapshot {
		return fmt.Errorf("tombstone was not acknowledged")
	}
	p.tombstoned = append(p.tombstoned, id)
	p.reasons = append(p.reasons, ts.Reason)
	return nil
}

func (p *fakePublisher) Flush(context.Context) error {
	p.flushes++
	return p.flushErr
}

// fakeSnapshot replays a fixed set of deliveries as the compacted price.computed
// log.
type fakeSnapshot struct {
	records []*kafka.Delivery
	err     error
	reads   int
}

func (s *fakeSnapshot) Read(
	ctx context.Context, fn func(context.Context, *kafka.Delivery) error,
) (kafka.SnapshotStats, error) {
	s.reads++
	if s.err != nil {
		return kafka.SnapshotStats{}, s.err
	}
	var stats kafka.SnapshotStats
	stats.Partitions = 1
	for _, d := range s.records {
		if err := fn(ctx, d); err != nil {
			return stats, err
		}
		if d.Tombstone {
			stats.Tombstones++
			continue
		}
		stats.Values++
	}
	return stats, nil
}

// fakeConsumer hands a fixed batch to the handler and then returns, which is
// what a real Consumer does when its context is cancelled.
type fakeConsumer struct {
	records []*kafka.Delivery
	err     error

	handled  int
	returned error
}

func (c *fakeConsumer) Run(ctx context.Context, h kafka.Handler) error {
	for _, d := range c.records {
		if err := h.HandleMessage(ctx, d); err != nil {
			c.returned = err
			c.handled++
			return err
		}
		c.handled++
	}
	return c.err
}

// -----------------------------------------------------------------------------
// Delivery construction
// -----------------------------------------------------------------------------

// normalizedDelivery frames a record the way internal/ingest/normalizer would
// have put it on odds.normalized.
func normalizedDelivery(t *testing.T, rec normalizer.NormalizedMarket) *kafka.Delivery {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal normalized market: %v", err)
	}
	return &kafka.Delivery{
		Topic: kafka.TopicOddsNormalized,
		Key:   rec.Market.ID,
		Envelope: kafka.Envelope{
			Version:    kafka.EnvelopeVersion,
			Type:       normalizer.MessageType,
			ID:         rec.Fingerprint,
			Producer:   "ingest",
			ProducedAt: rec.IngestedAt,
			ObservedAt: rec.ObservedAt,
			Data:       data,
		},
	}
}

// pricedDelivery frames a record the way THIS service would have put it on
// price.computed. Only the envelope is populated, which is the point: the warm
// start must not need the payload.
func pricedDelivery(market, id string, observedAt time.Time) *kafka.Delivery {
	return &kafka.Delivery{
		Topic: kafka.TopicPriceComputed,
		Key:   market,
		Envelope: kafka.Envelope{
			Version:    kafka.EnvelopeVersion,
			Type:       MessageType,
			ID:         id,
			Producer:   "pricer",
			ObservedAt: observedAt,
			Data:       json.RawMessage(`{}`),
		},
	}
}

// tombstoneDelivery frames a deletion.
func tombstoneDelivery(topic, market string) *kafka.Delivery {
	return &kafka.Delivery{
		Topic:           topic,
		Key:             market,
		Tombstone:       true,
		TombstoneReason: "test",
		Headers:         map[string]string{kafka.HeaderTombstone: "1"},
	}
}

// -----------------------------------------------------------------------------
// Service construction
// -----------------------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// countingPrice is a PriceFunc that records how many markets it was asked to
// price. The value it returns is deliberately trivial: what the engine computes
// is engine_test.go's subject, and the service is contractually indifferent to it.
type countingPrice struct {
	calls int
	err   error
}

func (c *countingPrice) fn(_ context.Context, rec normalizer.NormalizedMarket) (any, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return map[string]string{"market": rec.Market.ID}, nil
}

// serviceHarness is a Service and everything that was injected into it.
type serviceHarness struct {
	svc      *Service
	pub      *fakePublisher
	snap     *fakeSnapshot
	price    *countingPrice
	metrics  *Metrics
	registry *prometheus.Registry
}

func newHarness(t *testing.T, opts ServiceOptions) serviceHarness {
	t.Helper()

	h := serviceHarness{
		pub:      &fakePublisher{},
		snap:     &fakeSnapshot{},
		price:    &countingPrice{},
		registry: prometheus.NewRegistry(),
	}
	m, err := NewMetrics(h.registry)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	h.metrics = m

	if opts.Price == nil {
		opts.Price = h.price.fn
	}
	if opts.Producer == nil {
		opts.Producer = h.pub
	} else {
		h.pub, _ = opts.Producer.(*fakePublisher)
	}
	if opts.Snapshotter == nil {
		opts.Snapshotter = h.snap
	} else {
		h.snap, _ = opts.Snapshotter.(*fakeSnapshot)
	}
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	opts.Metrics = m

	svc, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	return h
}

// twoWayFixture builds a complete two-way market quoted by the sharp book. The
// probabilities are chosen here, not recorded, and the margin relation is the
// synthetic generator's own.
func twoWayFixture(t *testing.T, id string, observedAt time.Time) normalizer.NormalizedMarket {
	t.Helper()
	fair := []float64{0.58, 0.42}
	quoted := multiplicativeMargin(t, fair, 0.04)
	f := marketFixture{
		id:         id,
		selections: []domain.SelectionRole{domain.SelectionRoleHome, domain.SelectionRoleAway},
		observedAt: observedAt,
		books: []bookFixture{
			{slug: string(domain.SyntheticBookSlug), prices: decimalsOf(quoted)},
		},
	}
	return f.build(t)
}

func counter(t *testing.T, m *Metrics, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(m.markets.WithLabelValues(result))
}

// -----------------------------------------------------------------------------
// The two guards
// -----------------------------------------------------------------------------

func TestIdenticalInputIsSuppressedAndCostsNoPublish(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	rec := twoWayFixture(t, "mkt-suppress", fixtureEpoch)
	d := normalizedDelivery(t, rec)
	ctx := context.Background()

	if err := h.svc.HandleMessage(ctx, d); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := h.svc.HandleMessage(ctx, d); err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	if got := len(h.pub.published); got != 1 {
		t.Fatalf("published %d records for one unchanged market, want 1", got)
	}
	if got := h.price.calls; got != 1 {
		t.Fatalf("engine called %d times, want 1: a suppressed market must not be repriced", got)
	}
	if got := counter(t, h.metrics, resultSuppressed); got != 1 {
		t.Fatalf("markets_total{result=suppressed} = %v, want 1", got)
	}
	if got := counter(t, h.metrics, resultPublished); got != 1 {
		t.Fatalf("markets_total{result=published} = %v, want 1", got)
	}
}

func TestObservationOlderThanThePricedStateIsRefused(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	ctx := context.Background()

	newer := twoWayFixture(t, "mkt-stale", fixtureEpoch)
	if err := h.svc.HandleMessage(ctx, normalizedDelivery(t, newer)); err != nil {
		t.Fatalf("newer delivery: %v", err)
	}

	// A DIFFERENT fingerprint, so suppression cannot be what refuses it, and an
	// earlier observation instant, which is the only thing that should.
	older := twoWayFixture(t, "mkt-stale", fixtureEpoch.Add(-time.Minute))
	older.Fingerprint = "fixture-mkt-stale-older"
	if err := h.svc.HandleMessage(ctx, normalizedDelivery(t, older)); err != nil {
		t.Fatalf("older delivery: %v", err)
	}

	if got := len(h.pub.published); got != 1 {
		t.Fatalf("published %d records, want 1: an older observation must not regress the snapshot", got)
	}
	if got := counter(t, h.metrics, resultStale); got != 1 {
		t.Fatalf("markets_total{result=stale} = %v, want 1", got)
	}
}

func TestChangedInputIsRepublished(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	ctx := context.Background()

	first := twoWayFixture(t, "mkt-move", fixtureEpoch)
	if err := h.svc.HandleMessage(ctx, normalizedDelivery(t, first)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	moved := twoWayFixture(t, "mkt-move", fixtureEpoch.Add(time.Minute))
	moved.Fingerprint = "fixture-mkt-move-2"
	if err := h.svc.HandleMessage(ctx, normalizedDelivery(t, moved)); err != nil {
		t.Fatalf("moved delivery: %v", err)
	}

	if got := len(h.pub.published); got != 2 {
		t.Fatalf("published %d records, want 2: a moved market must reprice", got)
	}
}

// -----------------------------------------------------------------------------
// The engine revision
// -----------------------------------------------------------------------------

func TestEngineRevisionIsPartOfTheSuppressionIdentity(t *testing.T) {
	rec := twoWayFixture(t, "mkt-revision", fixtureEpoch)
	d := normalizedDelivery(t, rec)
	ctx := context.Background()

	// A replica running revision A prices the market and records what it wrote.
	a := newHarness(t, ServiceOptions{EngineRevision: "revA"})
	if err := a.svc.HandleMessage(ctx, d); err != nil {
		t.Fatalf("revision A: %v", err)
	}
	if len(a.pub.published) != 1 {
		t.Fatalf("revision A published %d records, want 1", len(a.pub.published))
	}
	wroteID := a.pub.published[0].id
	if !strings.HasPrefix(wroteID, rec.Fingerprint+engineRevisionSep) {
		t.Fatalf("message id %q does not compose the fingerprint with the revision", wroteID)
	}

	// A replica running revision B warms from that record and must NOT suppress:
	// the input is unchanged but the function applied to it is not.
	b := newHarness(t, ServiceOptions{EngineRevision: "revB"})
	b.snap.records = []*kafka.Delivery{pricedDelivery(rec.Market.ID, wroteID, rec.ObservedAt)}
	if err := b.svc.Warm(ctx); err != nil {
		t.Fatalf("revision B warm: %v", err)
	}
	if err := b.svc.HandleMessage(ctx, d); err != nil {
		t.Fatalf("revision B: %v", err)
	}
	if got := len(b.pub.published); got != 1 {
		t.Fatalf("a changed engine revision published %d records, want 1: the slate must reprice", got)
	}

	// A replica running revision A again warms from the same record and DOES
	// suppress, which is what shows the reprice above was the revision and not
	// merely a cold tracker.
	c := newHarness(t, ServiceOptions{EngineRevision: "revA"})
	c.snap.records = []*kafka.Delivery{pricedDelivery(rec.Market.ID, wroteID, rec.ObservedAt)}
	if err := c.svc.Warm(ctx); err != nil {
		t.Fatalf("revision A warm: %v", err)
	}
	if err := c.svc.HandleMessage(ctx, d); err != nil {
		t.Fatalf("revision A second run: %v", err)
	}
	if got := len(c.pub.published); got != 0 {
		t.Fatalf("an unchanged revision published %d records, want 0", got)
	}
}

func TestEngineRevisionIsBoundedAndCannotForgeTheSeparator(t *testing.T) {
	for name, revision := range map[string]string{
		"too long":         strings.Repeat("x", MaxEngineRevisionLen+1),
		"forged separator": "a" + engineRevisionSep + "b",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(ServiceOptions{
				Price:          (&countingPrice{}).fn,
				Producer:       &fakePublisher{},
				Snapshotter:    &fakeSnapshot{},
				Logger:         discardLogger(),
				EngineRevision: revision,
			})
			if !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("err = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestNoRevisionLeavesTheFingerprintAsTheMessageID(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	rec := twoWayFixture(t, "mkt-bare-id", fixtureEpoch)
	if err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if got := h.pub.published[0].id; got != rec.Fingerprint {
		t.Fatalf("message id = %q, want the bare fingerprint %q", got, rec.Fingerprint)
	}
}

// -----------------------------------------------------------------------------
// Tombstones
// -----------------------------------------------------------------------------

func TestTombstoneInPropagatesATombstoneOutAndForgetsTheMarket(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	ctx := context.Background()

	rec := twoWayFixture(t, "mkt-tombstone", fixtureEpoch)
	if err := h.svc.HandleMessage(ctx, normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	if got := h.svc.trackedLen(); got != 1 {
		t.Fatalf("tracked %d markets after a publish, want 1", got)
	}

	if err := h.svc.HandleMessage(ctx, tombstoneDelivery(kafka.TopicOddsNormalized, rec.Market.ID)); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	if got := len(h.pub.tombstoned); got != 1 {
		t.Fatalf("published %d tombstones, want 1: a deleted market must not stay in the priced snapshot", got)
	}
	if got, want := string(h.pub.tombstoned[0]), rec.Market.ID; got != want {
		t.Fatalf("tombstoned %q, want %q", got, want)
	}
	if h.pub.reasons[0] == "" {
		t.Fatal("tombstone carried no reason; kafka.Tombstone requires one and an operator reads it")
	}
	if got := h.svc.trackedLen(); got != 0 {
		t.Fatalf("tracked %d markets after a tombstone, want 0", got)
	}
	if got := counter(t, h.metrics, resultTombstoned); got != 1 {
		t.Fatalf("markets_total{result=tombstoned} = %v, want 1", got)
	}
}

func TestTombstoneFailureIsReturnedSoTheOffsetIsNotCommitted(t *testing.T) {
	pub := &fakePublisher{tombstoneErr: errors.New("broker refused")}
	h := newHarness(t, ServiceOptions{Producer: pub})

	err := h.svc.HandleMessage(context.Background(),
		tombstoneDelivery(kafka.TopicOddsNormalized, "mkt-tombstone-fail"))
	if err == nil {
		t.Fatal("a failed tombstone returned nil, which would commit the offset for a deletion that never happened")
	}
	if got := counter(t, h.metrics, resultFailed); got != 1 {
		t.Fatalf("markets_total{result=failed} = %v, want 1", got)
	}
}

// -----------------------------------------------------------------------------
// Warm start
// -----------------------------------------------------------------------------

func TestWarmStartFoldsTheEnvelopeAndNeverThePayload(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	rec := twoWayFixture(t, "mkt-warm", fixtureEpoch)

	// The payload is deliberately `{}` — see pricedDelivery. If the warm start
	// decoded it, this would fail rather than suppress.
	h.snap.records = []*kafka.Delivery{
		pricedDelivery(rec.Market.ID, rec.Fingerprint, rec.ObservedAt),
		pricedDelivery("mkt-warm-other", "fp-other", fixtureEpoch),
		tombstoneDelivery(kafka.TopicPriceComputed, "mkt-warm-other"),
	}

	ctx := context.Background()
	if err := h.svc.Warm(ctx); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if got := h.svc.trackedLen(); got != 1 {
		t.Fatalf("tracked %d markets, want 1: the tombstone must delete its key from the fold", got)
	}
	if err := h.svc.HandleMessage(ctx, normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if got := len(h.pub.published); got != 0 {
		t.Fatalf("published %d records after a warm start that already held this market, want 0", got)
	}
	if got := testutil.ToFloat64(h.metrics.warmStart.WithLabelValues(warmStartOK)); got != 1 {
		t.Fatalf("warm_start_total{outcome=ok} = %v, want 1", got)
	}
}

func TestWarmStartIsIdempotentAndReadsOnce(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := h.svc.Warm(ctx); err != nil {
			t.Fatalf("Warm %d: %v", i, err)
		}
	}
	if got := h.snap.reads; got != 1 {
		t.Fatalf("snapshot read %d times, want 1", got)
	}
}

func TestFailedWarmStartPricesColdRatherThanRefusing(t *testing.T) {
	snap := &fakeSnapshot{err: errors.New("broker unhappy")}
	h := newHarness(t, ServiceOptions{Snapshotter: snap, WarmStartAttempts: 1})
	ctx := context.Background()

	if err := h.svc.Warm(ctx); err == nil {
		t.Fatal("Warm returned nil for a failing snapshot read")
	}
	if got := testutil.ToFloat64(h.metrics.warmStart.WithLabelValues(warmStartFailed)); got != 1 {
		t.Fatalf("warm_start_total{outcome=failed} = %v, want 1", got)
	}

	// The pipeline still runs. That is the whole argument: a frozen board is
	// worse than one republication of the slate.
	rec := twoWayFixture(t, "mkt-cold", fixtureEpoch)
	if err := h.svc.HandleMessage(ctx, normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("HandleMessage after a failed warm start: %v", err)
	}
	if got := len(h.pub.published); got != 1 {
		t.Fatalf("published %d records while cold, want 1", got)
	}
}

// -----------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------

func TestPublishFailureIsReturnedSoTheRecordIsRedelivered(t *testing.T) {
	pub := &fakePublisher{publishErr: errors.New("broker refused")}
	h := newHarness(t, ServiceOptions{Producer: pub})
	rec := twoWayFixture(t, "mkt-publish-fail", fixtureEpoch)

	err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec))
	if err == nil {
		t.Fatal("a failed publish returned nil, which would commit an offset ahead of a record that never reached the broker")
	}
	if got := counter(t, h.metrics, resultFailed); got != 1 {
		t.Fatalf("markets_total{result=failed} = %v, want 1", got)
	}
	if got := h.svc.trackedLen(); got != 0 {
		t.Fatalf("tracked %d markets after a failed publish, want 0: a market that was not written must reprice", got)
	}
}

func TestPermanentFailuresAreSkippedRatherThanHaltingThePartition(t *testing.T) {
	rec := twoWayFixture(t, "mkt-permanent", fixtureEpoch)

	t.Run("unsupported message type", func(t *testing.T) {
		h := newHarness(t, ServiceOptions{})
		d := normalizedDelivery(t, rec)
		d.Envelope.Type = "odds.normalized.v99"
		if err := h.svc.HandleMessage(context.Background(), d); err != nil {
			t.Fatalf("HandleMessage returned %v; an unreadable envelope must be skipped, not retried for ever", err)
		}
		if got := counter(t, h.metrics, resultInvalid); got != 1 {
			t.Fatalf("markets_total{result=invalid} = %v, want 1", got)
		}
	})

	t.Run("undecodable payload", func(t *testing.T) {
		h := newHarness(t, ServiceOptions{})
		d := normalizedDelivery(t, rec)
		d.Envelope.Data = json.RawMessage(`{"schema_version":`)
		if err := h.svc.HandleMessage(context.Background(), d); err != nil {
			t.Fatalf("HandleMessage returned %v; redelivery cannot change the bytes on disk", err)
		}
		if got := counter(t, h.metrics, resultInvalid); got != 1 {
			t.Fatalf("markets_total{result=invalid} = %v, want 1", got)
		}
	})

	t.Run("unusable market key", func(t *testing.T) {
		h := newHarness(t, ServiceOptions{})
		d := normalizedDelivery(t, rec)
		d.Key = ""
		if err := h.svc.HandleMessage(context.Background(), d); err != nil {
			t.Fatalf("HandleMessage returned %v; an unusable key is permanent", err)
		}
		if got := counter(t, h.metrics, resultInvalid); got != 1 {
			t.Fatalf("markets_total{result=invalid} = %v, want 1", got)
		}
	})

	t.Run("engine refusal", func(t *testing.T) {
		price := &countingPrice{err: ErrNoReferenceBook}
		h := newHarness(t, ServiceOptions{Price: price.fn})
		if err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec)); err != nil {
			t.Fatalf("HandleMessage returned %v; nothing in the odds mathematics does I/O, "+
				"so a refusal is permanent and must not halt every other market on the partition", err)
		}
		if got := counter(t, h.metrics, resultInvalid); got != 1 {
			t.Fatalf("markets_total{result=invalid} = %v, want 1", got)
		}
		if got := len(h.pub.published); got != 0 {
			t.Fatalf("published %d records for a market the engine refused, want 0", got)
		}
	})

	t.Run("engine returned no payload", func(t *testing.T) {
		h := newHarness(t, ServiceOptions{
			Price: func(context.Context, normalizer.NormalizedMarket) (any, error) { return nil, nil },
		})
		if err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec)); err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
		if got := len(h.pub.published); got != 0 {
			t.Fatalf("published %d records for a nil payload, want 0", got)
		}
	})
}

func TestNilDeliveryIsRefused(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	if err := h.svc.HandleMessage(context.Background(), nil); err == nil {
		t.Fatal("a nil delivery returned nil")
	}
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

func TestServiceOptionsFailAtConstruction(t *testing.T) {
	base := func() ServiceOptions {
		return ServiceOptions{
			Price:       (&countingPrice{}).fn,
			Producer:    &fakePublisher{},
			Snapshotter: &fakeSnapshot{},
			Logger:      discardLogger(),
		}
	}
	cases := map[string]func(*ServiceOptions){
		"no engine":       func(o *ServiceOptions) { o.Price = nil },
		"no producer":     func(o *ServiceOptions) { o.Producer = nil },
		"no snapshotter":  func(o *ServiceOptions) { o.Snapshotter = nil },
		"no logger":       func(o *ServiceOptions) { o.Logger = nil },
		"negative flush":  func(o *ServiceOptions) { o.FlushTimeout = -time.Second },
		"negative engine": func(o *ServiceOptions) { o.EngineTimeout = -time.Second },
		"negative warm":   func(o *ServiceOptions) { o.WarmStartAttempts = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := base()
			mutate(&o)
			if _, err := New(o); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("err = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Run, readiness and shutdown
// -----------------------------------------------------------------------------

func TestCheckIsFalseUntilRunStartsAndFalseAgainAfterItStops(t *testing.T) {
	rec := twoWayFixture(t, "mkt-ready", fixtureEpoch)
	consumer := &fakeConsumer{records: []*kafka.Delivery{normalizedDelivery(t, rec)}}
	h := newHarness(t, ServiceOptions{Consumer: consumer})

	if err := h.svc.Check(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Check before Run = %v, want ErrNotRunning", err)
	}
	if err := h.svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := h.svc.Check(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Check after Run returned = %v, want ErrNotRunning", err)
	}
	if h.svc.Name() != "pricer" {
		t.Fatalf("Name = %q, want pricer", h.svc.Name())
	}
}

func TestRunFlushesTheProducerAfterTheConsumerStops(t *testing.T) {
	consumer := &fakeConsumer{}
	h := newHarness(t, ServiceOptions{Consumer: consumer})

	if err := h.svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.pub.flushes; got != 1 {
		t.Fatalf("flushed %d times, want 1: closing without flushing discards accepted records", got)
	}
}

func TestRunFlushesEvenOnACancelledContext(t *testing.T) {
	// The flush must run on a context DETACHED from the cancelled one, or it
	// returns instantly with the buffer intact.
	consumer := &fakeConsumer{}
	h := newHarness(t, ServiceOptions{Consumer: consumer})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.pub.flushes; got != 1 {
		t.Fatalf("flushed %d times on a cancelled context, want 1", got)
	}
}

func TestRunReportsAFailedFlushAlongsideTheConsumerError(t *testing.T) {
	consumerErr := errors.New("consumer stopped badly")
	pub := &fakePublisher{flushErr: errors.New("flush timed out")}
	h := newHarness(t, ServiceOptions{
		Consumer: &fakeConsumer{err: consumerErr},
		Producer: pub,
	})

	err := h.svc.Run(context.Background())
	if !errors.Is(err, consumerErr) {
		t.Fatalf("Run error %v does not wrap the consumer's", err)
	}
	if !strings.Contains(err.Error(), "final flush") {
		t.Fatalf("Run error %v does not mention the failed flush", err)
	}
}

func TestRunWithoutAConsumerIsRefused(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	if err := h.svc.Run(context.Background()); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("Run without a consumer = %v, want ErrNotRunnable", err)
	}
}

// -----------------------------------------------------------------------------
// The metric contract
// -----------------------------------------------------------------------------

func TestPricingDurationCarriesTheContractedName(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	rec := twoWayFixture(t, "mkt-metric", fixtureEpoch)
	if err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	families, err := h.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}

	// Each of these is read by deploy/observability. Renaming one silently
	// empties a dashboard panel or an alert rule.
	for _, want := range []string{
		"sharpline_pricing_duration_seconds",
		"sharpline_pricing_markets_total",
		"sharpline_pricing_markets_tracked",
		"sharpline_pricing_warm_start_total",
		"sharpline_odds_staleness_seconds",
		"sharpline_pipeline_latency_seconds",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("metric family %q is absent; emitted: %v", want, names)
		}
	}
}

func TestPricingDurationHistogramCarriesTheAlertThresholdBoundary(t *testing.T) {
	// deploy/observability/rules/sharpline-alerts.yml: PricingLatencyHigh
	// compares the p99 against 0.25, and the alert file states that a rule
	// selecting a bucket that does not exist evaluates to empty rather than to
	// false. So the boundary has to be there exactly.
	if !slices.Contains(PricingBuckets(), 0.25) {
		t.Fatalf("PricingBuckets() = %v, which has no 0.25 boundary", PricingBuckets())
	}

	h := newHarness(t, ServiceOptions{})
	rec := twoWayFixture(t, "mkt-buckets", fixtureEpoch)
	if err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	families, err := h.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var bounds []float64
	for _, f := range families {
		if f.GetName() != "sharpline_pricing_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, b := range m.GetHistogram().GetBucket() {
				bounds = append(bounds, b.GetUpperBound())
			}
		}
	}
	if !slices.Contains(bounds, 0.25) {
		t.Fatalf("emitted bucket bounds %v carry no le=0.25", bounds)
	}
}

func TestStalenessIsObservedAtThePricedStageOncePerPrice(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	rec := twoWayFixture(t, "mkt-staleness", fixtureEpoch)
	if err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// One observation per PRICE — the dashboard's definition — and one pipeline
	// observation per RECORD, because ingested_at is a property of the payload.
	// The counts are read off the gathered families rather than through
	// testutil.ToFloat64, which takes a Collector and a histogram child is only
	// an Observer.
	families, err := h.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	counts := map[string]uint64{}
	for _, f := range families {
		name := f.GetName()
		if name != "sharpline_odds_staleness_seconds" && name != "sharpline_pipeline_latency_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			stage := ""
			for _, l := range m.GetLabel() {
				if l.GetName() == "stage" {
					stage = l.GetValue()
				}
			}
			if stage != "priced" {
				t.Fatalf("%s carried stage=%q, want priced", name, stage)
			}
			counts[name] += m.GetHistogram().GetSampleCount()
		}
	}
	if got, want := counts["sharpline_odds_staleness_seconds"], uint64(len(rec.Prices)); got != want {
		t.Fatalf("staleness observations = %d, want one per price (%d)", got, want)
	}
	if got := counts["sharpline_pipeline_latency_seconds"]; got != 1 {
		t.Fatalf("pipeline observations = %d, want one per record", got)
	}
}

func TestClockSkewIsCountedRatherThanRecordedAsExcellentFreshness(t *testing.T) {
	// A provider instant AHEAD of the pricing clock makes the raw age negative,
	// which a histogram would file in the lowest bucket and read as excellent.
	// The contract is: clamp at zero, and count the clamp.
	frozen := fixtureEpoch
	h := newHarness(t, ServiceOptions{Clock: func() time.Time { return frozen }})
	rec := twoWayFixture(t, "mkt-skew", frozen.Add(time.Minute))

	if err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	got := testutil.ToFloat64(h.metrics.clockSkew.WithLabelValues(rec.Provider, "priced"))
	if want := float64(len(rec.Prices)); got != want {
		t.Fatalf("odds_clock_skew_total = %v, want %v", got, want)
	}
}

func TestMetricsRegisterOnceAndFailLoudlyOnAConflict(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := NewMetrics(reg); err != nil {
		t.Fatalf("first NewMetrics: %v", err)
	}
	if _, err := NewMetrics(reg); err == nil {
		t.Fatal("a second registration on the same registry succeeded; a duplicate collector must fail startup")
	}
	if _, err := NewMetrics(nil); err != nil {
		t.Fatalf("NewMetrics(nil): %v", err)
	}
}

func TestPublishedRecordCarriesTheContractedEnvelopeFields(t *testing.T) {
	h := newHarness(t, ServiceOptions{})
	rec := twoWayFixture(t, "mkt-envelope", fixtureEpoch)
	if err := h.svc.HandleMessage(context.Background(), normalizedDelivery(t, rec)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	got := h.pub.published[0]
	if got.msgType != MessageType {
		t.Fatalf("message type = %q, want %q", got.msgType, MessageType)
	}
	if !got.observedAt.Equal(rec.ObservedAt) {
		t.Fatalf("observed_at = %s, want the source instant %s unchanged; no hop may re-stamp it",
			got.observedAt, rec.ObservedAt)
	}
	if string(got.market) != rec.Market.ID {
		t.Fatalf("keyed by %q, want the market id %q", got.market, rec.Market.ID)
	}
}
