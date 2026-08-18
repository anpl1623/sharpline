package normalizer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Fakes
//
// They are fakes of THIS package's own consumer-declared seams (Publisher,
// Snapshot), not of Kafka. CLAUDE.md §10 forbids a mocked broker for the
// interesting behaviour — "the interesting bugs live in consumer-group
// rebalancing and offset handling" — and that behaviour belongs to
// internal/platform/kafka, which tests it against a real broker under
// testcontainers. What is under test here is change detection: given a delivery,
// which records reach the producer. A real broker would make that slower to run
// and no more true.
// -----------------------------------------------------------------------------

type published struct {
	id  domain.MarketID
	msg kafka.Message
}

type fakePublisher struct {
	sent []published
	err  error
}

func (f *fakePublisher) PublishNormalized(_ context.Context, id domain.MarketID, msg kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, published{id: id, msg: msg})
	return nil
}

// records renders what was published as the wire records a consumer would read,
// which is also what the next process's warm start replays.
func (f *fakePublisher) records(t *testing.T) []*kafka.Delivery {
	t.Helper()
	out := make([]*kafka.Delivery, 0, len(f.sent))
	for i, p := range f.sent {
		rec, ok := p.msg.Payload.(NormalizedMarket)
		if !ok {
			t.Fatalf("published payload %d is %T, want NormalizedMarket", i, p.msg.Payload)
		}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, &kafka.Delivery{
			Topic:     kafka.TopicOddsNormalized,
			Partition: 0,
			Offset:    int64(i),
			Key:       p.id.String(),
			Envelope: kafka.Envelope{
				Version: kafka.EnvelopeVersion, Type: p.msg.Type, ID: p.msg.ID,
				Producer: "ingest", ProducedAt: rec.IngestedAt, ObservedAt: p.msg.ObservedAt,
				Data: data,
			},
		})
	}
	return out
}

func (f *fakePublisher) reset() { f.sent = nil }

type fakeSnapshotter struct {
	deliveries []*kafka.Delivery
	err        error
	reads      int
}

func (f *fakeSnapshotter) Read(ctx context.Context, fn func(context.Context, *kafka.Delivery) error) (kafka.SnapshotStats, error) {
	f.reads++
	if f.err != nil {
		return kafka.SnapshotStats{}, f.err
	}
	var stats kafka.SnapshotStats
	stats.Partitions = 1
	for _, d := range f.deliveries {
		if err := fn(ctx, d); err != nil {
			return stats, err
		}
		if d.Tombstone {
			stats.Tombstones++
		} else {
			stats.Values++
		}
	}
	return stats, nil
}

// clock is a manually advanced time source. The refresh ceiling is otherwise
// untestable without a five-minute sleep, which is a test nobody runs.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time      { return c.now }
func (c *clock) add(d time.Duration) { c.now = c.now.Add(d) }

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

type harness struct {
	n    *Normalizer
	pub  *fakePublisher
	snap *fakeSnapshotter
	clk  *clock
	reg  *prometheus.Registry
}

func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	pub := &fakePublisher{}
	snap := &fakeSnapshotter{}
	clk := &clock{now: testObserved.Add(2 * time.Second)}
	reg := prometheus.NewRegistry()

	dec, err := NewNeutralDecoder(testProvider)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		Provider:    testProvider,
		Decoder:     dec,
		Producer:    pub,
		Snapshotter: snap,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry:    reg,
		Clock:       clk.Now,
	}
	for _, f := range opts {
		f(&o)
	}
	n, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{n: n, pub: pub, snap: snap, clk: clk, reg: reg}
}

// rawDelivery frames one RawEvent the way internal/ingest frames it on
// odds.raw.{provider}: the provider's own bytes as the envelope's data, and the
// provider's observation instant on the envelope.
func rawDelivery(t *testing.T, raw RawEvent, producedAt time.Time) *kafka.Delivery {
	t.Helper()
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	topic, err := kafka.OddsRaw(testProvider)
	if err != nil {
		t.Fatal(err)
	}
	return &kafka.Delivery{
		Topic: topic.Name(), Partition: 0, Offset: 1, Key: raw.ID, Timestamp: producedAt,
		Envelope: kafka.Envelope{
			Version: kafka.EnvelopeVersion, Type: RawMessageType, ID: raw.ID,
			Producer: "ingest", ProducedAt: producedAt, ObservedAt: testObserved,
			Data: body,
		},
	}
}

func counter(t *testing.T, reg *prometheus.Registry, name string, labels prometheus.Labels) float64 {
	t.Helper()
	metrics, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range metrics {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for _, l := range m.GetLabel() {
				if want, ok := labels[l.GetName()]; ok && want != l.GetValue() {
					match = false
				}
			}
			if !match || len(m.GetLabel()) < len(labels) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
			if h := m.GetHistogram(); h != nil {
				return float64(h.GetSampleCount())
			}
		}
	}
	return 0
}

func result(t *testing.T, h *harness, r string) float64 {
	t.Helper()
	return counter(t, h.reg, "sharpline_normalizer_markets_total", prometheus.Labels{"result": r})
}

// -----------------------------------------------------------------------------
// Change detection — the central requirement of this phase
// -----------------------------------------------------------------------------

// TestUnchangedPayloadsAreSuppressed is the claim CLAUDE.md §5 makes:
// "most polls return identical data and MUST NOT GENERATE BUS TRAFFIC."
func TestUnchangedPayloadsAreSuppressed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := rawDelivery(t, baseRaw(), h.clk.now)

	if err := h.n.HandleMessage(ctx, d); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if got, want := len(h.pub.sent), 3; got != want {
		t.Fatalf("first delivery published %d markets, want %d", got, want)
	}

	// Nine more identical polls. Every one of them must be silent.
	for i := 0; i < 9; i++ {
		h.clk.add(time.Second)
		if err := h.n.HandleMessage(ctx, d); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if got, want := len(h.pub.sent), 3; got != want {
		t.Fatalf("published %d markets across ten identical polls, want %d", got, want)
	}

	if got, want := result(t, h, resultPublished), 3.0; got != want {
		t.Errorf("published counter = %v, want %v", got, want)
	}
	if got, want := result(t, h, resultSuppressed), 27.0; got != want {
		t.Errorf("suppressed counter = %v, want %v", got, want)
	}
	// The ratio the gate measures. Ten polls of a static slate: 90% suppressed.
	ratio := result(t, h, resultSuppressed) /
		(result(t, h, resultSuppressed) + result(t, h, resultPublished) + result(t, h, resultRefreshed))
	if ratio < 0.89 {
		t.Errorf("suppression ratio = %.3f, want ≈0.9", ratio)
	}
}

// TestOnlyTheMarketThatMovedIsPublished is the other half: suppression must not
// be so eager that it swallows a real move. Getting this wrong leaves the
// compacted topic serving a price the book has not offered for hours, with no
// error anywhere.
func TestOnlyTheMarketThatMovedIsPublished(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		mutate func(*RawEvent)
	}{
		{"a quote moved", func(r *RawEvent) { r.Books[0].Markets[0].Outcomes[0].Price = 1.71 }},
		{"the line moved", func(r *RawEvent) {
			r.Books[0].Markets[1].Outcomes[0].Point = point(-4)
			r.Books[0].Markets[1].Outcomes[1].Point = point(4)
		}},
		{"a book stopped quoting", func(r *RawEvent) {
			r.Books[0].Markets[2].Outcomes = r.Books[0].Markets[2].Outcomes[:1]
		}},
		{"a new book appeared", func(r *RawEvent) {
			r.Books = append(r.Books, spreadBook("fanduel", -3.5, 3.5))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if err := h.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), h.clk.now)); err != nil {
				t.Fatal(err)
			}
			h.pub.reset()

			moved := baseRaw()
			tc.mutate(&moved)
			h.clk.add(time.Second)
			if err := h.n.HandleMessage(ctx, rawDelivery(t, moved, h.clk.now)); err != nil {
				t.Fatal(err)
			}
			if got := len(h.pub.sent); got != 1 {
				t.Fatalf("published %d markets, want exactly the one that moved", got)
			}
		})
	}
}

// TestTheObservationInstantAloneDoesNotRepublish is the exclusion the whole
// mechanism depends on. The Odds API advances last_update on every refresh
// whether or not a price moved; hashing it would make every poll differ and
// suppression would never fire once.
func TestTheObservationInstantAloneDoesNotRepublish(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), h.clk.now)); err != nil {
		t.Fatal(err)
	}
	h.pub.reset()

	refreshed := baseRaw()
	later := testObserved.Add(90 * time.Second)
	refreshed.Books[0].LastUpdate = later
	for i := range refreshed.Books[0].Markets {
		refreshed.Books[0].Markets[i].LastUpdate = later
	}

	h.clk.add(90 * time.Second)
	if err := h.n.HandleMessage(ctx, rawDelivery(t, refreshed, h.clk.now)); err != nil {
		t.Fatal(err)
	}
	if got := len(h.pub.sent); got != 0 {
		t.Fatalf("published %d markets on a poll where only last_update advanced", got)
	}
}

// TestSuppressionCeilingRepublishes pins the self-healing half: a market that
// has not moved is republished once its last publication is older than
// RefreshAfter, so any defect in the fingerprint corrects itself within one
// interval instead of persisting until the market next moves.
func TestSuppressionCeilingRepublishes(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RefreshAfter = time.Minute })
	ctx := context.Background()
	d := rawDelivery(t, baseRaw(), h.clk.now)

	if err := h.n.HandleMessage(ctx, d); err != nil {
		t.Fatal(err)
	}
	h.pub.reset()

	h.clk.add(59 * time.Second)
	if err := h.n.HandleMessage(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got := len(h.pub.sent); got != 0 {
		t.Fatalf("published %d markets inside the ceiling", got)
	}

	h.clk.add(2 * time.Second)
	if err := h.n.HandleMessage(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got, want := len(h.pub.sent), 3; got != want {
		t.Fatalf("published %d markets past the ceiling, want %d", got, want)
	}
	if got, want := result(t, h, resultRefreshed), 3.0; got != want {
		t.Errorf("refreshed counter = %v, want %v — a ceiling republication must not read as a move", got, want)
	}
}

// TestStaleObservationIsSkipped is the monotonicity guard. odds.normalized is
// compacted, so publishing an older observation makes the LATEST record for that
// key the OLDER state, and every consumer that builds a snapshot from the log
// then serves it.
func TestStaleObservationIsSkipped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	current := baseRaw()
	current.Books[0].LastUpdate = testObserved
	if err := h.n.HandleMessage(ctx, rawDelivery(t, current, h.clk.now)); err != nil {
		t.Fatal(err)
	}
	h.pub.reset()

	// An older payload that ALSO differs, so suppression cannot be what stops it.
	older := baseRaw()
	stale := testObserved.Add(-5 * time.Minute)
	older.Books[0].LastUpdate = stale
	for i := range older.Books[0].Markets {
		older.Books[0].Markets[i].LastUpdate = stale
	}
	older.Books[0].Markets[0].Outcomes[0].Price = 1.42

	h.clk.add(time.Second)
	if err := h.n.HandleMessage(ctx, rawDelivery(t, older, h.clk.now)); err != nil {
		t.Fatal(err)
	}
	if got := len(h.pub.sent); got != 0 {
		t.Fatalf("published %d markets from an observation older than the published state", got)
	}
	if got, want := result(t, h, resultStale), 3.0; got != want {
		t.Errorf("stale counter = %v, want %v", got, want)
	}
}

// -----------------------------------------------------------------------------
// Warm start
// -----------------------------------------------------------------------------

// TestWarmStartRestoresSuppressionAcrossARestart is the phase requirement:
// "STATE MUST SURVIVE A RESTART, or the first poll after every deploy
// republishes the whole slate."
//
// The control half is what makes it a real test. A cold process republishes
// everything; a warm one publishes nothing. Without the control, a suppression
// bug anywhere else would let the warm case pass for the wrong reason.
func TestWarmStartRestoresSuppressionAcrossARestart(t *testing.T) {
	ctx := context.Background()
	first := newHarness(t)
	d := rawDelivery(t, baseRaw(), first.clk.now)
	if err := first.n.HandleMessage(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got, want := len(first.pub.sent), 3; got != want {
		t.Fatalf("the first process published %d markets, want %d", got, want)
	}
	topic := first.pub.records(t)

	cold := newHarness(t)
	if err := cold.n.HandleMessage(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got, want := len(cold.pub.sent), 3; got != want {
		t.Fatalf("control: a cold restart published %d markets, want %d — if this is 0 the "+
			"warm case below proves nothing", got, want)
	}

	warm := newHarness(t)
	warm.snap.deliveries = topic
	if err := warm.n.HandleMessage(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got := len(warm.pub.sent); got != 0 {
		t.Fatalf("a warm restart republished %d markets", got)
	}
	if got, want := result(t, warm, resultSuppressed), 3.0; got != want {
		t.Errorf("suppressed counter = %v, want %v", got, want)
	}
	if got, want := counter(t, warm.reg, "sharpline_normalizer_fingerprints", nil), 3.0; got != want {
		t.Errorf("fingerprints gauge = %v, want %v", got, want)
	}
	if got, want := counter(t, warm.reg, "sharpline_normalizer_warm_start_total",
		prometheus.Labels{"outcome": warmStartOK}), 1.0; got != want {
		t.Errorf("warm_start_total{ok} = %v, want %v", got, want)
	}
	if got := counter(t, warm.reg, "sharpline_normalizer_fingerprint_mismatches_total", nil); got != 0 {
		t.Errorf("fingerprint mismatches = %v; this build's hash disagrees with what it wrote", got)
	}
	if warm.snap.reads != 1 {
		t.Errorf("snapshot reads = %d, want exactly 1", warm.snap.reads)
	}

	// Warm start must not be repeated per record.
	for i := 0; i < 3; i++ {
		if err := warm.n.HandleMessage(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if warm.snap.reads != 1 {
		t.Errorf("snapshot reads = %d after four records, want 1", warm.snap.reads)
	}
}

// TestExplicitWarmSatisfiesTheOnceGuard: a caller that warms eagerly — which is
// what cmd/ingest should do once it has a context to hand — must not pay for a
// second snapshot read on the first delivered record.
func TestExplicitWarmSatisfiesTheOnceGuard(t *testing.T) {
	ctx := context.Background()
	first := newHarness(t)
	if err := first.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), first.clk.now)); err != nil {
		t.Fatal(err)
	}

	warm := newHarness(t)
	warm.snap.deliveries = first.pub.records(t)
	if err := warm.n.Warm(ctx); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if warm.snap.reads != 1 {
		t.Fatalf("snapshot reads after an explicit Warm = %d, want 1", warm.snap.reads)
	}
	if err := warm.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), warm.clk.now)); err != nil {
		t.Fatal(err)
	}
	if warm.snap.reads != 1 {
		t.Errorf("snapshot reads = %d after the first record, want 1", warm.snap.reads)
	}
	if got := len(warm.pub.sent); got != 0 {
		t.Errorf("republished %d markets after an explicit warm start", got)
	}

	broken := newHarness(t)
	broken.snap.err = errors.New("broker unavailable")
	if err := broken.n.Warm(ctx); err == nil {
		t.Error("Warm reported success on a failed read")
	}
}

// TestWarmStartHonoursTombstones: a deleted market must NOT stay suppressed,
// because nothing else will ever republish it.
func TestWarmStartHonoursTombstones(t *testing.T) {
	ctx := context.Background()
	first := newHarness(t)
	if err := first.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), first.clk.now)); err != nil {
		t.Fatal(err)
	}
	topic := first.pub.records(t)

	// Delete the first market, the way an admin suspension or a phase-8 sweep
	// would.
	deleted := topic[0].Key
	topic = append(topic, &kafka.Delivery{
		Topic: kafka.TopicOddsNormalized, Partition: 0, Offset: int64(len(topic)),
		Key: deleted, Tombstone: true, TombstoneReason: "test",
	})

	warm := newHarness(t)
	warm.snap.deliveries = topic
	if err := warm.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), warm.clk.now)); err != nil {
		t.Fatal(err)
	}
	if got, want := len(warm.pub.sent), 1; got != want {
		t.Fatalf("published %d markets, want %d — only the tombstoned one", got, want)
	}
	if got := warm.pub.sent[0].id.String(); got != deleted {
		t.Errorf("republished %s, want the tombstoned %s", got, deleted)
	}
}

// TestWarmStartCountsAnUnusableKey: one unreadable record on the compacted topic
// must not abort the rebuild of every other market's state.
func TestWarmStartCountsAnUnusableKey(t *testing.T) {
	ctx := context.Background()
	first := newHarness(t)
	if err := first.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), first.clk.now)); err != nil {
		t.Fatal(err)
	}
	topic := first.pub.records(t)
	topic = append(topic, &kafka.Delivery{
		Topic: kafka.TopicOddsNormalized, Partition: 0, Offset: 99, Key: "",
		Envelope: kafka.Envelope{Version: kafka.EnvelopeVersion, Type: MessageType, Data: []byte(`{}`)},
	})

	warm := newHarness(t)
	warm.snap.deliveries = topic
	if err := warm.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), warm.clk.now)); err != nil {
		t.Fatal(err)
	}
	if got := len(warm.pub.sent); got != 0 {
		t.Fatalf("published %d markets; one bad record aborted the warm start", got)
	}
	if got := counter(t, warm.reg, "sharpline_normalizer_rejects_total",
		prometheus.Labels{"scope": string(ScopeMarket), "reason": string(ReasonInvalidIdentifier)}); got != 1 {
		t.Errorf("invalid_identifier rejects = %v, want 1", got)
	}
}

// TestWarmStartFailureProceedsCold: refusing to normalize would freeze the board
// for as long as the broker is unhappy, which is worse than republishing the
// slate once.
func TestWarmStartFailureProceedsCold(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(o *Options) { o.WarmStartAttempts = 2 })
	h.snap.err = errors.New("broker unavailable")
	d := rawDelivery(t, baseRaw(), h.clk.now)

	for i := 0; i < 4; i++ {
		if err := h.n.HandleMessage(ctx, d); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		h.clk.add(time.Second)
	}
	if h.snap.reads != 2 {
		t.Errorf("snapshot reads = %d, want the 2-attempt budget", h.snap.reads)
	}
	if got := len(h.pub.sent); got != 3 {
		t.Errorf("published %d markets, want 3 — a cold process publishes the slate once and then suppresses", got)
	}
	if got, want := counter(t, h.reg, "sharpline_normalizer_warm_start_total",
		prometheus.Labels{"outcome": warmStartFailed}), 2.0; got != want {
		t.Errorf("warm_start_total{failed} = %v, want %v", got, want)
	}
}

// TestWarmStartCountsAFingerprintMismatch: a stored fingerprint that disagrees
// with this build's recomputation means the hash or the payload shape changed
// without SchemaVersion being bumped.
func TestWarmStartCountsAFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	first := newHarness(t)
	if err := first.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), first.clk.now)); err != nil {
		t.Fatal(err)
	}
	topic := first.pub.records(t)

	var rec NormalizedMarket
	if err := json.Unmarshal(topic[0].Envelope.Data, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Fingerprint = "deadbeef"
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	topic[0].Envelope.Data = data

	warm := newHarness(t)
	warm.snap.deliveries = topic
	if err := warm.n.HandleMessage(ctx, rawDelivery(t, baseRaw(), warm.clk.now)); err != nil {
		t.Fatal(err)
	}
	if got, want := counter(t, warm.reg, "sharpline_normalizer_fingerprint_mismatches_total", nil), 1.0; got != want {
		t.Errorf("mismatches = %v, want %v", got, want)
	}
	// The RECOMPUTED hash is authoritative, so the market is still suppressed:
	// the record's CONTENT has not changed, only the value someone stored beside it.
	if got := len(warm.pub.sent); got != 0 {
		t.Errorf("published %d markets; the recomputed fingerprint was not used", got)
	}
}

// -----------------------------------------------------------------------------
// Record handling
// -----------------------------------------------------------------------------

func TestHandleMessageRejectsAnUndecodablePayload(t *testing.T) {
	h := newHarness(t)
	d := rawDelivery(t, baseRaw(), h.clk.now)
	d.Envelope.Data = []byte(`{"commence_time":`)

	if err := h.n.HandleMessage(context.Background(), d); err != nil {
		t.Fatalf("a malformed payload halted the pipeline: %v", err)
	}
	if got := len(h.pub.sent); got != 0 {
		t.Fatalf("published %d markets from a malformed payload", got)
	}
	if got := counter(t, h.reg, "sharpline_normalizer_rejects_total",
		prometheus.Labels{"scope": string(ScopeEvent), "reason": string(ReasonDecode)}); got != 1 {
		t.Errorf("decode rejects = %v, want 1", got)
	}
	if got := counter(t, h.reg, "sharpline_normalizer_records_total",
		prometheus.Labels{"outcome": recordRejected}); got != 1 {
		t.Errorf("records_total{rejected} = %v, want 1", got)
	}
}

func TestHandleMessageSkipsAnUnknownEnvelope(t *testing.T) {
	h := newHarness(t)
	d := rawDelivery(t, baseRaw(), h.clk.now)
	d.Envelope.Type = "odds.raw.v2"

	if err := h.n.HandleMessage(context.Background(), d); err != nil {
		t.Fatalf("an unknown envelope halted the pipeline: %v", err)
	}
	if got := len(h.pub.sent); got != 0 {
		t.Fatalf("published %d markets from an envelope this build does not read", got)
	}
	if got := counter(t, h.reg, "sharpline_normalizer_rejects_total",
		prometheus.Labels{"scope": string(ScopeEvent), "reason": string(ReasonUnsupportedMessage)}); got != 1 {
		t.Errorf("unsupported_message rejects = %v, want 1", got)
	}
}

func TestHandleMessageRefusesAForeignTopic(t *testing.T) {
	h := newHarness(t)
	d := rawDelivery(t, baseRaw(), h.clk.now)
	d.Topic = "odds.raw.synthetic"

	err := h.n.HandleMessage(context.Background(), d)
	if err == nil {
		t.Fatal("a record from another provider's topic was accepted; this normalizer holds one decoder " +
			"and would give the bytes the wrong meaning")
	}
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Errorf("error %v does not wrap ErrUnsupportedProvider", err)
	}
}

// TestPublishFailureIsReturned: the raw offset must not commit ahead of a record
// that never reached the broker. internal/platform/kafka's Consumer commits the
// last successfully handled record per partition, so swallowing this would lose
// the market silently on the next restart.
func TestPublishFailureIsReturned(t *testing.T) {
	h := newHarness(t)
	h.pub.err = errors.New("not enough replicas")

	err := h.n.HandleMessage(context.Background(), rawDelivery(t, baseRaw(), h.clk.now))
	if err == nil {
		t.Fatal("a failed produce was swallowed")
	}
	if got, want := result(t, h, resultFailed), 1.0; got != want {
		t.Errorf("failed counter = %v, want %v", got, want)
	}
}

// TestRedeliveryAfterAPartialFailureIsIdempotent: markets already published from
// a payload are suppressed by their own fingerprints on the retry, so the redo
// costs one produce per market that actually failed.
func TestRedeliveryAfterAPartialFailureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	d := rawDelivery(t, baseRaw(), h.clk.now)

	if err := h.n.HandleMessage(ctx, d); err != nil {
		t.Fatal(err)
	}
	before := len(h.pub.sent)

	// The whole record is redelivered, exactly as it would be after an
	// uncommitted offset.
	if err := h.n.HandleMessage(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got := len(h.pub.sent); got != before {
		t.Fatalf("a redelivery produced %d extra records", got-before)
	}
}

// TestTombstoneOnTheRawTopicIsCounted. odds.raw.{provider} is retention-based
// and this pipeline never writes one, so it means something else did.
func TestTombstoneOnTheRawTopicIsCounted(t *testing.T) {
	h := newHarness(t)
	d := rawDelivery(t, baseRaw(), h.clk.now)
	d.Tombstone = true
	d.Envelope = kafka.Envelope{}

	if err := h.n.HandleMessage(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if got := counter(t, h.reg, "sharpline_normalizer_records_total",
		prometheus.Labels{"outcome": recordTombstone}); got != 1 {
		t.Errorf("records_total{tombstone} = %v, want 1", got)
	}
}

// -----------------------------------------------------------------------------
// Wiring and contracts
// -----------------------------------------------------------------------------

// TestRawMessageTypeMatchesTheProducer pins the wire contract this package
// restates rather than imports. A drift here would make the normalizer skip
// every record on the topic while reporting a bounded, plausible reason.
func TestRawMessageTypeMatchesTheProducer(t *testing.T) {
	if RawMessageType != ingest.RawMessageType {
		t.Fatalf("normalizer reads %q, internal/ingest writes %q", RawMessageType, ingest.RawMessageType)
	}
}

// TestNewValidatesItsOptions: configuration fails at construction, loudly,
// rather than at the first record (CLAUDE.md §12).
func TestNewValidatesItsOptions(t *testing.T) {
	dec, err := NewNeutralDecoder(testProvider)
	if err != nil {
		t.Fatal(err)
	}
	valid := Options{
		Provider: testProvider, Decoder: dec, Producer: &fakePublisher{},
		Snapshotter: &fakeSnapshotter{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := New(valid); err != nil {
		t.Fatalf("the valid options were rejected: %v", err)
	}

	other, err := NewNeutralDecoder("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		break_ func(*Options)
	}{
		{"no decoder", func(o *Options) { o.Decoder = nil }},
		{"no producer", func(o *Options) { o.Producer = nil }},
		{"no snapshotter", func(o *Options) { o.Snapshotter = nil }},
		{"no logger", func(o *Options) { o.Logger = nil }},
		{"negative refresh", func(o *Options) { o.RefreshAfter = -time.Second }},
		{"negative attempts", func(o *Options) { o.WarmStartAttempts = -1 }},
		{"empty provider", func(o *Options) { o.Provider = "" }},
		{"the decoder handles another provider", func(o *Options) { o.Decoder = other }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := valid
			tc.break_(&o)
			_, err := New(o)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, ErrInvalidOptions) {
				t.Errorf("error %v does not wrap ErrInvalidOptions", err)
			}
		})
	}
}

// TestSharedContractSeriesRegisterAlongsideTheProviderSet is the mechanical half
// of the argument in metrics.go.
//
// internal/ingest/provider registers sharpline_odds_staleness_seconds and
// sharpline_odds_clock_skew_total for stage="received"; this package emits
// stage="normalized" onto the same series. cmd/ingest builds both sets on ONE
// registry, in that order, so a disagreement about help text or label names is a
// startup failure for the whole service. This test is what turns that into a red
// build instead.
func TestSharedContractSeriesRegisterAlongsideTheProviderSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := provider.NewMetrics(reg); err != nil {
		t.Fatalf("provider metrics: %v", err)
	}
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("normalizer metrics alongside the provider set: %v — the two packages disagree about "+
			"a series they share", err)
	}

	// Both stages must land on ONE series, not on two that happen to share a name.
	m.observePublished(sampleRecord(), sampleRecord().ObservedAt.Add(30*time.Second))
	if got := testutil.CollectAndCount(m.staleness, "sharpline_odds_staleness_seconds"); got == 0 {
		t.Fatal("the shared staleness histogram recorded nothing")
	}

	// THE ORDER IS SYMMETRIC, and this half is the assertion that keeps it so.
	//
	// It used to be asymmetric: internal/ingest/provider registered its
	// collectors directly and treated AlreadyRegisteredError as a failure, so it
	// had to be built FIRST and reversing the two lines in cmd/ingest killed the
	// process at startup with "duplicate metrics collector registration
	// attempted". Nothing in the type system said so. provider.NewMetrics now
	// adopts the shared series the same way this package does, so the reverse
	// order must also succeed and land on ONE series.
	rev := prometheus.NewRegistry()
	if _, err := NewMetrics(rev); err != nil {
		t.Fatalf("normalizer metrics on a fresh registry: %v", err)
	}
	if _, err := provider.NewMetrics(rev); err != nil {
		t.Fatalf("provider metrics alongside the normalizer set: %v — construction order on a "+
			"shared registry is load-bearing again, which is exactly the failure mode the "+
			"adoption path in both NewMetrics functions exists to remove", err)
	}
	// One series, not two that share a name: the reversed registry must expose
	// sharpline_odds_staleness_seconds exactly once.
	families, err := rev.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := 0
	for _, f := range families {
		if f.GetName() == "sharpline_odds_staleness_seconds" {
			names++
		}
	}
	if names > 1 {
		t.Errorf("sharpline_odds_staleness_seconds appears in %d metric families; the two stages "+
			"registered separate collectors rather than sharing one", names)
	}
}

// TestStalenessIsObservedPerPriceAtTheNormalizedStage pins the SLO's unit.
// Observing once per record with the newest instant would report the freshest
// book's age for every book on the market — the number that flatters the
// pipeline most.
func TestStalenessIsObservedPerPriceAtTheNormalizedStage(t *testing.T) {
	h := newHarness(t)
	if err := h.n.HandleMessage(context.Background(), rawDelivery(t, baseRaw(), h.clk.now)); err != nil {
		t.Fatal(err)
	}

	prices := 0
	for _, p := range h.pub.sent {
		rec, ok := p.msg.Payload.(NormalizedMarket)
		if !ok {
			t.Fatalf("payload is %T", p.msg.Payload)
		}
		prices += len(rec.Prices)
	}
	if prices == 0 {
		t.Fatal("nothing was published")
	}

	got := counter(t, h.reg, "sharpline_odds_staleness_seconds",
		prometheus.Labels{"stage": provider.StageNormalized})
	if int(got) != prices {
		t.Errorf("staleness samples = %v, want one per published price (%d)", got, prices)
	}
	if got := counter(t, h.reg, "sharpline_pipeline_latency_seconds",
		prometheus.Labels{"stage": provider.StageNormalized}); int(got) != len(h.pub.sent) {
		t.Errorf("pipeline latency samples = %v, want one per published record (%d)", got, len(h.pub.sent))
	}
}

// TestClockSkewIsClampedAndCounted: a negative sample on a histogram lands in
// the lowest bucket and reads as EXCELLENT freshness, which throws away exactly
// the signal domain.Price.Age and migrations/00003 went out of their way to
// preserve. sharpline-alerts.yml fixes the contract: clamp, and count the clamp.
func TestClockSkewIsClampedAndCounted(t *testing.T) {
	h := newHarness(t)
	// The provider stamped its observation in our future.
	h.clk.now = testObserved.Add(-time.Minute)

	if err := h.n.HandleMessage(context.Background(), rawDelivery(t, baseRaw(), h.clk.now)); err != nil {
		t.Fatal(err)
	}
	if got := counter(t, h.reg, "sharpline_odds_clock_skew_total",
		prometheus.Labels{"provider": testProvider.String(), "stage": provider.StageNormalized}); got == 0 {
		t.Fatal("a future observation instant was clamped silently")
	}
}

// TestPublishedRecordCarriesItsOwnFingerprintAndKey pins what a consumer reads.
func TestPublishedRecordCarriesItsOwnFingerprintAndKey(t *testing.T) {
	h := newHarness(t)
	if err := h.n.HandleMessage(context.Background(), rawDelivery(t, baseRaw(), h.clk.now)); err != nil {
		t.Fatal(err)
	}
	for _, p := range h.pub.sent {
		rec, ok := p.msg.Payload.(NormalizedMarket)
		if !ok {
			t.Fatalf("payload is %T", p.msg.Payload)
		}
		if p.msg.Type != MessageType {
			t.Errorf("message type = %q, want %q", p.msg.Type, MessageType)
		}
		if rec.Market.ID != p.id.String() {
			t.Errorf("record keyed by %q carries market %q", p.id, rec.Market.ID)
		}
		if Fingerprint(rec.Fingerprint) != rec.Hash() {
			t.Errorf("market %s carries fingerprint %q, recomputes to %s",
				rec.Market.ID, rec.Fingerprint, rec.Hash())
		}
		if p.msg.ID != rec.Fingerprint {
			t.Errorf("message id %q is not the fingerprint %q", p.msg.ID, rec.Fingerprint)
		}
		if !p.msg.ObservedAt.Equal(rec.ObservedAt) {
			t.Errorf("envelope observed_at %s != record observed_at %s", p.msg.ObservedAt, rec.ObservedAt)
		}
		if rec.Provider != testProvider.String() {
			t.Errorf("record provider = %q, want %q", rec.Provider, testProvider)
		}
		// ingested_at is the raw envelope's ProducedAt, propagated unchanged.
		if !rec.IngestedAt.Equal(h.clk.now.UTC()) {
			t.Errorf("ingested_at = %s, want the raw envelope's produced_at %s", rec.IngestedAt, h.clk.now)
		}
	}
}

// TestMemoryStoreRoundTrips is the seam's own behaviour, so a Redis-backed
// implementation has something to be tested against.
func TestMemoryStoreRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	id := domain.MarketID("p.e.a.m.spreads")

	if _, ok, err := s.Load(ctx, id); err != nil || ok {
		t.Fatalf("empty store returned (ok=%v, err=%v)", ok, err)
	}
	want := Entry{Fingerprint: "abc", ObservedAt: testObserved, PublishedAt: testObserved.Add(time.Second)}
	if err := s.Store(ctx, id, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load(ctx, id)
	if err != nil || !ok || got != want {
		t.Fatalf("Load = (%+v, %v, %v), want (%+v, true, nil)", got, ok, err, want)
	}
	if n, _ := s.Len(ctx); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Len(ctx); n != 0 {
		t.Fatalf("Len after delete = %d, want 0", n)
	}
	if want.IsZero() {
		t.Error("a populated entry reported itself zero")
	}
	if !(Entry{}).IsZero() {
		t.Error("the zero entry did not report itself zero")
	}
}
