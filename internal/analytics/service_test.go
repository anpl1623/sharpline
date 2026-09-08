package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/analytics/steam"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// recordingStore captures what reached the database, in order.
//
// It is a test double for the SINK and for no data: every finding it captures was
// produced by the real detectors from a record built with the real odds
// functions.
type recordingStore struct {
	ev    []EVSignal
	arb   []ArbitrageSignal
	steam []SteamSignal
	fail  error
}

func (s *recordingStore) RecordEVSignal(_ context.Context, sig EVSignal) error {
	if s.fail != nil {
		return s.fail
	}
	s.ev = append(s.ev, sig)
	return nil
}

func (s *recordingStore) RecordArbitrageSignal(_ context.Context, sig ArbitrageSignal) error {
	if s.fail != nil {
		return s.fail
	}
	s.arb = append(s.arb, sig)
	return nil
}

func (s *recordingStore) RecordSteamSignal(_ context.Context, sig SteamSignal) error {
	if s.fail != nil {
		return s.fail
	}
	s.steam = append(s.steam, sig)
	return nil
}

// recordingPublisher captures what reached the bus.
type recordingPublisher struct {
	msgs []kafka.Message
	fail error
}

func (p *recordingPublisher) PublishEVSignal(_ context.Context, _ domain.MarketID, m kafka.Message) error {
	return p.record(m)
}

func (p *recordingPublisher) PublishArbitrageSignal(_ context.Context, _ domain.MarketID, m kafka.Message) error {
	return p.record(m)
}

func (p *recordingPublisher) PublishSteamSignal(_ context.Context, _ domain.MarketID, m kafka.Message) error {
	return p.record(m)
}

func (p *recordingPublisher) record(m kafka.Message) error {
	if p.fail != nil {
		return p.fail
	}
	p.msgs = append(p.msgs, m)
	return nil
}

// testLogger discards output. The service requires a logger and these tests
// assert on sinks, not on log lines.
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// delivery frames a priced market the way internal/platform/kafka would.
func delivery(t *testing.T, rec pricing.ComputedMarket) *kafka.Delivery {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &kafka.Delivery{
		Topic: kafka.TopicPriceComputed,
		Key:   rec.Market.ID,
		Envelope: kafka.Envelope{
			Version:    kafka.EnvelopeVersion,
			Type:       pricing.MessageType,
			Producer:   "pricer",
			ProducedAt: fixtureAnchor,
			ObservedAt: rec.ObservedAt,
			Data:       json.RawMessage(data),
		},
	}
}

// evMarket is a priced market carrying one comfortable +EV quote.
func evMarket(t *testing.T) pricing.ComputedMarket {
	t.Helper()
	return computedMarket(t, "moneyline", 0,
		quoteSpec{selection: "sel-home", book: "soft", offered: 2.40, fair: 0.50, age: 5 * time.Second},
		quoteSpec{selection: "sel-away", book: "sharp", offered: 2.00, fair: 0.50, age: 5 * time.Second},
	)
}

// TestFindingsReachBothSinks asserts the ordinary path end to end: a priced
// market in, a row and a record out.
func TestFindingsReachBothSinks(t *testing.T) {
	store := &recordingStore{}
	pub := &recordingPublisher{}

	svc, err := New(ServiceOptions{
		Store: store, Publisher: pub, Logger: testLogger(),
		Clock: func() time.Time { return fixtureAnchor },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := svc.HandleMessage(context.Background(), delivery(t, evMarket(t))); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if len(store.ev) != 1 {
		t.Fatalf("store received %d +EV findings, want 1", len(store.ev))
	}
	if len(pub.msgs) != 1 {
		t.Fatalf("bus received %d records, want 1", len(pub.msgs))
	}

	msg := pub.msgs[0]
	switch {
	case msg.Type != MessageTypeEV:
		t.Fatalf("message type = %q, want %q", msg.Type, MessageTypeEV)
	case msg.ID == "":
		t.Fatal("the record carries no id, so a consumer cannot deduplicate a redelivery")
	case len(msg.ID) != signalIDLen:
		t.Fatalf("id is %d characters, want %d", len(msg.ID), signalIDLen)
	// The PROVIDER's instant, not the detector's. A detection timestamp here
	// would report perfect freshness for a finding about a ten-minute-old quote.
	case !msg.ObservedAt.Equal(store.ev[0].QuoteObservedAt):
		t.Fatalf("ObservedAt = %s, want the quote's own instant %s",
			msg.ObservedAt, store.ev[0].QuoteObservedAt)
	}
}

// contendedStore fails the first n +EV writes with [ErrContended] and records
// every attempt, so a test can tell a retry from a redelivery.
//
// It wraps the sentinel in a fmt.Errorf chain because that is how the real
// adapter returns it — pgstore attaches the SQLSTATE and the driver error — and
// a double that returned the bare sentinel would pass even if the service
// compared with == instead of errors.Is.
type contendedStore struct {
	recordingStore
	contendFirst int
	attempts     int
}

func (s *contendedStore) RecordEVSignal(ctx context.Context, sig EVSignal) error {
	s.attempts++
	if s.attempts <= s.contendFirst {
		return fmt.Errorf("pgstore: write ev signal (SQLSTATE 40P01): %w: %w",
			ErrContended, errors.New("deadlock detected"))
	}
	return s.recordingStore.RecordEVSignal(ctx, sig)
}

// TestAContendedStoreWriteIsRetriedInPlace asserts that a write Postgres rolled
// back as a deadlock victim is re-run without redelivering the record.
//
// Both halves matter and neither is sufficient alone. That the finding LANDS is
// what makes the pipeline survive the lock-ordering race phase 9 introduced
// between the signals stage and ingest's catalogue upsert — see [ErrContended].
// That exactly ONE record reaches the bus is what proves the retry happened at
// the persist step rather than by returning the record to the consumer: a
// redelivery would re-run every detector and publish the finding twice, which is
// visible here and would be invisible in production.
func TestAContendedStoreWriteIsRetriedInPlace(t *testing.T) {
	store := &contendedStore{contendFirst: storeAttempts - 1}
	pub := &recordingPublisher{}

	svc, err := New(ServiceOptions{
		Store: store, Publisher: pub, Logger: testLogger(),
		Clock: func() time.Time { return fixtureAnchor },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := svc.HandleMessage(context.Background(), delivery(t, evMarket(t))); err != nil {
		t.Fatalf("HandleMessage: %v; a contended write must not fail the record", err)
	}

	if store.attempts != storeAttempts {
		t.Fatalf("the store saw %d attempts, want %d", store.attempts, storeAttempts)
	}
	if len(store.ev) != 1 {
		t.Fatalf("store holds %d +EV findings, want 1", len(store.ev))
	}
	if len(pub.msgs) != 1 {
		t.Fatalf("bus received %d records, want exactly 1; more than one means the "+
			"record was redelivered rather than the write retried", len(pub.msgs))
	}
}

// TestSustainedContentionIsReportedRatherThanAbsorbed asserts the retry loop is
// bounded.
//
// A loop that kept going would turn sustained lock contention into unbounded
// handler latency, which the consumer answers by fencing the group member — the
// failure mode is worse than the one being avoided, and it is invisible in the
// signals metrics because nothing is ever counted as failed. The budget is spent,
// the error carries the sentinel, and nothing is published.
func TestSustainedContentionIsReportedRatherThanAbsorbed(t *testing.T) {
	store := &contendedStore{contendFirst: storeAttempts + 5}
	pub := &recordingPublisher{}

	svc, err := New(ServiceOptions{
		Store: store, Publisher: pub, Logger: testLogger(),
		Clock: func() time.Time { return fixtureAnchor },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = svc.HandleMessage(context.Background(), delivery(t, evMarket(t)))
	if err == nil {
		t.Fatal("sustained contention was absorbed; the record must fail so its offset stays uncommitted")
	}
	if !errors.Is(err, ErrContended) {
		t.Fatalf("error = %v, want one wrapping ErrContended", err)
	}
	if store.attempts != storeAttempts {
		t.Fatalf("the store saw %d attempts, want the budget %d", store.attempts, storeAttempts)
	}
	if len(pub.msgs) != 0 {
		t.Fatalf("bus received %d records; a finding that was never stored must not be announced",
			len(pub.msgs))
	}
}

// TestMessageIdIsTheReplayKeyDigested asserts that two runs over one record
// produce the same bus identifier.
//
// It is the property that lets a consumer's deduplication agree with the
// database's ON CONFLICT about what "the same finding" means. A clock reading in
// the id would break both at once, and would do it silently: the rows would still
// upsert correctly while the topic grew a duplicate per redelivery.
func TestMessageIdIsTheReplayKeyDigested(t *testing.T) {
	run := func() string {
		pub := &recordingPublisher{}
		svc, err := New(ServiceOptions{
			Publisher: pub, Logger: testLogger(),
			// A DIFFERENT clock on the second run. Nothing in the id may depend
			// on it.
			Clock: func() time.Time { return fixtureAnchor.Add(time.Duration(len(pub.msgs)) * time.Hour) },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := svc.HandleMessage(context.Background(), delivery(t, evMarket(t))); err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
		if len(pub.msgs) != 1 {
			t.Fatalf("bus received %d records, want 1", len(pub.msgs))
		}
		return pub.msgs[0].ID
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("two runs over one record produced different ids: %s and %s", a, b)
	}
}

// TestSinkFailuresAreTransientAndPermanentFailuresAreNot pins which failures
// leave the offset uncommitted.
//
// The distinction is the difference between a self-healing pipeline and a poison
// record that halts a partition: a sink that refused can succeed on redelivery,
// and a payload that will not decode cannot.
func TestSinkFailuresAreTransientAndPermanentFailuresAreNot(t *testing.T) {
	boom := errors.New("sink refused")

	t.Run("a store failure is returned so the record is redelivered", func(t *testing.T) {
		svc, err := New(ServiceOptions{
			Store: &recordingStore{fail: boom}, Logger: testLogger(),
			Clock: func() time.Time { return fixtureAnchor },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := svc.HandleMessage(context.Background(), delivery(t, evMarket(t))); !errors.Is(err, boom) {
			t.Fatalf("HandleMessage = %v, want the sink's error", err)
		}
	})

	t.Run("a publish failure is returned too", func(t *testing.T) {
		svc, err := New(ServiceOptions{
			Publisher: &recordingPublisher{fail: boom}, Logger: testLogger(),
			Clock: func() time.Time { return fixtureAnchor },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := svc.HandleMessage(context.Background(), delivery(t, evMarket(t))); !errors.Is(err, boom) {
			t.Fatalf("HandleMessage = %v, want the sink's error", err)
		}
	})

	t.Run("an unreadable envelope is permanent and returns nil", func(t *testing.T) {
		svc, err := New(ServiceOptions{Logger: testLogger()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		d := delivery(t, evMarket(t))
		d.Envelope.Type = "something.this.build.does.not.read"
		if err := svc.HandleMessage(context.Background(), d); err != nil {
			t.Fatalf("HandleMessage = %v, want nil: redelivery cannot change the bytes on the topic", err)
		}
	})

	t.Run("a missing sink is not a failure", func(t *testing.T) {
		// Both sinks nil. It is no longer the shipped configuration — `pricer`
		// wires a store and a publisher — but it stays supported and stays
		// tested, because a deployment that lost its DSN must degrade to
		// detecting-without-recording rather than turning every record into a
		// failure the consumer answers by stopping or by skipping the board.
		svc, err := New(ServiceOptions{
			Logger: testLogger(),
			Clock:  func() time.Time { return fixtureAnchor },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := svc.HandleMessage(context.Background(), delivery(t, evMarket(t))); err != nil {
			t.Fatalf("HandleMessage = %v, want nil", err)
		}
	})
}

// TestTombstoneReleasesDetectorStateWithoutRetractingFindings pins the two halves
// of the tombstone rule.
//
// A signal is a statement that something happened at an instant, and the market
// ceasing to exist afterwards does not un-happen it — the same reason
// wager.events is retention-based rather than compacted. What must go is the
// steam detector's per-market window state, which would otherwise accumulate for
// the life of the process on a slate that rolls over daily.
func TestTombstoneReleasesDetectorStateWithoutRetractingFindings(t *testing.T) {
	store := &recordingStore{}
	svc, err := New(ServiceOptions{
		Store: store, Logger: testLogger(),
		Clock: func() time.Time { return fixtureAnchor },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	if err := svc.HandleMessage(ctx, delivery(t, evMarket(t))); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if svc.steam.Markets() != 1 {
		t.Fatalf("the detector holds state for %d markets, want 1", svc.steam.Markets())
	}
	before := len(store.ev)

	tomb := &kafka.Delivery{
		Topic:     kafka.TopicPriceComputed,
		Key:       "mkt-1",
		Tombstone: true,
	}
	if err := svc.HandleMessage(ctx, tomb); err != nil {
		t.Fatalf("HandleMessage(tombstone): %v", err)
	}
	if svc.steam.Markets() != 0 {
		t.Fatalf("the detector still holds state for %d markets after a tombstone", svc.steam.Markets())
	}
	if len(store.ev) != before {
		t.Fatalf("a tombstone changed the stored findings: %d, want %d", len(store.ev), before)
	}
}

// TestServiceRefusesAnUnusableConfiguration asserts that a detector threshold
// nobody meant to set is a startup error rather than a stage that consumes every
// record and reports nothing.
func TestServiceRefusesAnUnusableConfiguration(t *testing.T) {
	tests := []struct {
		name string
		opts ServiceOptions
	}{
		{"no logger", ServiceOptions{}},
		{"a negative EV floor", ServiceOptions{Logger: testLogger(), EV: EVConfig{MinEVPercent: -1}}},
		{"an arbitrage spread that can never bind", ServiceOptions{
			Logger: testLogger(),
			Arb:    ArbConfig{MaxLegAge: time.Second, MaxObservedSpread: time.Minute},
		}},
		{"a steam hop longer than its window", ServiceOptions{
			Logger: testLogger(),
			Steam:  steam.Config{Window: time.Minute, Hop: 5 * time.Minute},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatal("the configuration was accepted")
			}
		})
	}
}

// TestSteamShapingAcceptsEveryMarketType is a regression test for a defect that a
// moneyline-only suite cannot see.
//
// steam_signals CARRIES NO LINE COLUMN. A steam finding is a statement about one
// selection's probability over time, and the market's handicap is a property of
// the market rather than of the move. Validating it against the line rule the
// other two tables use would refuse every spread and every total for want of a
// line the table does not have — silently, as a WARN and a dropped finding, on
// exactly the market types a spread-heavy board is made of.
func TestSteamShapingAcceptsEveryMarketType(t *testing.T) {
	svc, err := New(ServiceOptions{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := fixtureAnchor.Add(-3 * time.Minute)
	finding := steam.Finding{
		Market:      "mkt-1",
		Selection:   "sel-home",
		WindowStart: start,
		WindowEnd:   fixtureAnchor,
		Window:      3 * time.Minute,
		Hop:         time.Minute,
		Direction:   steam.DirectionShorten,
		Delta:       0.06,
		Magnitude:   0.06,
		Velocity:    0.02,
		LeadBook:    "sharp",
		LeadMovedAt: start.Add(time.Minute),
		Followers: []steam.Follower{
			{Book: "soft", MovedAt: start.Add(90 * time.Second), Lag: 30 * time.Second, Delta: 0.05},
		},
		ParticipatingBooks:   2,
		Correlation:          1,
		ThresholdMagnitude:   0.05,
		ThresholdVelocity:    0.05 / 3,
		ThresholdCorrelation: 0.5,
		MinFollowers:         1,
		MaxFollowerLag:       2 * time.Minute,
	}

	for _, marketType := range []string{"moneyline", "spread", "total", "player_prop", "futures"} {
		t.Run(marketType, func(t *testing.T) {
			rec := computedMarket(t, marketType, 0,
				quoteSpec{selection: "sel-home", book: "soft", offered: 2.40, fair: 0.50},
			)
			if _, ok := svc.steamSignal(rec, finding, fixtureAnchor); !ok {
				t.Fatalf("a steam finding on a %s market was refused before it reached a sink",
					marketType)
			}
		})
	}

	t.Run("a market type this build does not know is still refused", func(t *testing.T) {
		rec := computedMarket(t, "parlay", 0,
			quoteSpec{selection: "sel-home", book: "soft", offered: 2.40, fair: 0.50},
		)
		if _, ok := svc.steamSignal(rec, finding, fixtureAnchor); ok {
			t.Fatal("an unknown market type was accepted; the composite foreign key would " +
				"then fail at the database and take the whole transaction with it")
		}
	})
}

// lagStore fails the first n +EV writes with [ErrCatalogueLag], recording every
// attempt so a test can tell a retry from a redelivery.
//
// Like contendedStore it wraps the sentinel the way the real adapter does — the
// pgstore attaches the SQLSTATE and the pgx error — so a service that compared
// with == rather than errors.Is would fail here.
type lagStore struct {
	recordingStore
	lagFirst int
	attempts int
}

func (s *lagStore) RecordEVSignal(ctx context.Context, sig EVSignal) error {
	s.attempts++
	if s.attempts <= s.lagFirst {
		return fmt.Errorf("pgstore: write ev signal (SQLSTATE 23503): %w: %w",
			ErrCatalogueLag, errors.New(`violates foreign key constraint "ev_signals_market_fk"`))
	}
	return s.recordingStore.RecordEVSignal(ctx, sig)
}

// TestCatalogueLagIsRetriedInPlace asserts that a write refused because
// `ingest` has not committed the market row yet is re-run rather than lost.
//
// The catalogue is written by a different service consuming the same topic, and
// nothing orders the two, so a market can be priced, published and read here in
// the instant before its parent row lands. The write is an idempotent upsert on
// an input-derived replay key, so re-running it is exactly as safe as re-running
// a deadlock victim. Exactly one bus record is the proof that the retry happened
// at the persist step rather than by returning the record to the consumer.
func TestCatalogueLagIsRetriedInPlace(t *testing.T) {
	store := &lagStore{lagFirst: storeAttempts - 1}
	pub := &recordingPublisher{}

	svc, err := New(ServiceOptions{
		Store: store, Publisher: pub, Logger: testLogger(),
		Clock: func() time.Time { return fixtureAnchor },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := svc.HandleMessage(context.Background(), delivery(t, evMarket(t))); err != nil {
		t.Fatalf("HandleMessage: %v; a catalogue-lag write must not fail the record", err)
	}
	if store.attempts != storeAttempts {
		t.Fatalf("the store saw %d attempts, want %d", store.attempts, storeAttempts)
	}
	if len(store.ev) != 1 {
		t.Fatalf("store holds %d +EV findings, want 1", len(store.ev))
	}
	if len(pub.msgs) != 1 {
		t.Fatalf("bus received %d records, want exactly 1; more than one means the record was "+
			"redelivered rather than the write retried", len(pub.msgs))
	}
}

// TestSustainedCatalogueLagIsDeferredRatherThanFailed asserts what happens when
// the parent still has not landed after the retry budget is spent.
//
// The record must NOT be returned as a failure. `pricer` wires this stage with
// kafka.ErrorPolicySkip, under which a returned error is advanced over anyway
// while the log claims a redelivery that never comes — and under ErrorPolicyStop
// it would halt the entire signals consumer over a transient referential gap,
// which on a cold start is most of the first replay. Nothing is published,
// because nothing was stored, and the finding is re-derived when the market next
// reprices.
//
// This is the phase-9 gate's finding: a first `docker compose up` logged 109
// records as database errors that were neither errors of this stage nor
// recoverable the way the message said.
func TestSustainedCatalogueLagIsDeferredRatherThanFailed(t *testing.T) {
	store := &lagStore{lagFirst: storeAttempts + 5}
	pub := &recordingPublisher{}

	svc, err := New(ServiceOptions{
		Store: store, Publisher: pub, Logger: testLogger(),
		Clock: func() time.Time { return fixtureAnchor },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := svc.HandleMessage(context.Background(), delivery(t, evMarket(t))); err != nil {
		t.Fatalf("HandleMessage = %v, want nil: a catalogue parent that has not committed is "+
			"neither this stage's failure nor something any ErrorPolicy improves", err)
	}
	if store.attempts != storeAttempts {
		t.Fatalf("the store saw %d attempts, want the budget %d", store.attempts, storeAttempts)
	}
	if len(pub.msgs) != 0 {
		t.Fatalf("bus received %d records; a finding that was never stored must not be announced",
			len(pub.msgs))
	}
}

// TestARealSinkFailureBesideACatalogueLagIsStillAFailure is the test that keeps
// the deferral honest.
//
// errors.Join builds a multi-error and errors.Is is satisfied by ANY leaf, so a
// naive check would swallow a genuine store outage that happened to share a
// record with one unanchored finding — an outage nobody could see. Every leaf has
// to be a catalogue lag before the record is deferred.
func TestARealSinkFailureBesideACatalogueLagIsStillAFailure(t *testing.T) {
	if onlyCatalogueLag(errors.Join(
		fmt.Errorf("persist ev: %w", ErrCatalogueLag),
		errors.New("connection refused"),
	)) {
		t.Fatal("a record carrying a real sink failure beside a catalogue lag was deferred")
	}
	if !onlyCatalogueLag(errors.Join(
		fmt.Errorf("persist ev: %w", ErrCatalogueLag),
		fmt.Errorf("persist arb: %w", ErrCatalogueLag),
	)) {
		t.Fatal("a record whose every finding was unanchored was not deferred")
	}
	if onlyCatalogueLag(nil) {
		t.Fatal("a nil error is not a deferral")
	}
	if onlyCatalogueLag(errors.Join()) {
		t.Fatal("an empty join is not a deferral")
	}
}

// fakeConsumer is a [Consumer] that delivers a fixed script and returns.
//
// It is a stand-in for the CONSUMER LOOP and for no data: every record it hands
// over is a real priced market built by this package's fixtures and decoded by
// the real handler.
type fakeConsumer struct {
	deliveries []*kafka.Delivery
	err        error
	handled    int
	sawHandler bool
}

func (c *fakeConsumer) Run(ctx context.Context, h kafka.Handler) error {
	c.sawHandler = h != nil
	for _, d := range c.deliveries {
		if err := h.HandleMessage(ctx, d); err != nil {
			return err
		}
		c.handled++
	}
	return c.err
}

// TestRunDrivesTheConsumerAndReportsReadinessWhileItDoes.
//
// Readiness is the assertion that matters. A replica whose signals consumer has
// exited while its listener is still up would otherwise look entirely healthy
// while the analytics surface silently stopped updating — which is the failure
// the checker exists to make visible, and which is invisible in the signals
// counters because a stage that has stopped simply counts nothing.
func TestRunDrivesTheConsumerAndReportsReadinessWhileItDoes(t *testing.T) {
	store := &recordingStore{}
	pub := &recordingPublisher{}

	var duringRun error
	svc, err := New(ServiceOptions{
		Store: store, Publisher: pub, Logger: testLogger(),
		Clock: func() time.Time { return fixtureAnchor },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.Name() != "analytics" {
		t.Fatalf("checker name is %q; the dashboard and the readiness payload key off it", svc.Name())
	}
	if !errors.Is(svc.Check(context.Background()), ErrNotRunning) {
		t.Fatal("a stage that has not started reported itself ready")
	}

	consumer := &fakeConsumer{
		deliveries: []*kafka.Delivery{delivery(t, evMarket(t))},
	}
	// The check runs INSIDE the handler, which is the only moment Run is
	// demonstrably in flight.
	svc.consumer = probeConsumer{inner: consumer, probe: func() { duringRun = svc.Check(context.Background()) }}

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !consumer.sawHandler {
		t.Fatal("Run did not hand the consumer a handler")
	}
	if consumer.handled != 1 {
		t.Fatalf("the consumer handled %d records, want 1", consumer.handled)
	}
	if duringRun != nil {
		t.Fatalf("Check reported %v while Run was in flight; the readiness probe would take a "+
			"healthy replica out of rotation", duringRun)
	}
	if !errors.Is(svc.Check(context.Background()), ErrNotRunning) {
		t.Fatal("a stage whose consumer has returned still reported itself ready; that is the " +
			"exact condition this checker exists to surface")
	}
	if len(store.ev) == 0 {
		t.Fatal("Run produced no findings from a record that carries one")
	}
}

// probeConsumer runs a callback between joining and the first delivery, so a test
// can observe the service while Run is genuinely in flight.
type probeConsumer struct {
	inner *fakeConsumer
	probe func()
}

func (c probeConsumer) Run(ctx context.Context, h kafka.Handler) error {
	c.probe()
	return c.inner.Run(ctx, h)
}

// TestRunWithoutAConsumerIsRefusedRatherThanIdle.
//
// A stage with no consumer would sit there reporting nothing and looking exactly
// like a quiet slate. CLAUDE.md §12: fail fast and loudly on a bad config.
func TestRunWithoutAConsumerIsRefusedRatherThanIdle(t *testing.T) {
	svc, err := New(ServiceOptions{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Run(context.Background()); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("Run = %v, want ErrNotRunnable", err)
	}
}

// TestASteamFindingIsValidatedAgainstTheSchemaBeforeItIsWritten.
//
// internal/analytics mirrors migration 00009's CHECK constraints in Go on purpose:
// the store writes several findings per record in ONE transaction, so a single
// constraint violation aborts the siblings and PostgreSQL then refuses every
// subsequent statement with 25P02. The error that comes back looks transient and
// no redelivery can fix it, because the bytes on the topic have not changed.
//
// The two rule sets therefore have to agree in both directions, and the cases
// below are the identities the schema states as identities rather than as ranges
// — the ones a hand-built finding is most likely to get subtly wrong.
func TestASteamFindingIsValidatedAgainstTheSchemaBeforeItIsWritten(t *testing.T) {
	base := func() SteamSignal {
		start := fixtureAnchor
		return SteamSignal{
			SchemaVersion:                SchemaVersion,
			MarketID:                     domain.MarketID("mkt"),
			MarketType:                   "moneyline",
			LeagueID:                     domain.LeagueID("lg"),
			SelectionID:                  domain.SelectionID("sel"),
			WindowStart:                  start,
			WindowEnd:                    start.Add(3 * time.Minute),
			WindowSeconds:                180,
			HopSeconds:                   60,
			Direction:                    "shorten",
			DeltaProbability:             0.08,
			MagnitudeProbabilityPoints:   0.08,
			VelocityProbabilityPerMinute: 0.08 / 3,
			DevigMethod:                  "none",
			LeadBookID:                   domain.BookID("lead"),
			LeadMovedAt:                  start.Add(time.Minute),
			Followers: []SteamFollower{
				{BookID: domain.BookID("f1"), MovedAt: start.Add(90 * time.Second), LagSeconds: 30, DeltaProbability: 0.07},
				{BookID: domain.BookID("f2"), MovedAt: start.Add(110 * time.Second), LagSeconds: 50, DeltaProbability: 0.06},
			},
			FollowerCount:         2,
			ParticipatingBooks:    3,
			CrossBookCorrelation:  1,
			ThresholdVelocity:     0.05 / 3,
			ThresholdMagnitude:    0.05,
			ThresholdCorrelation:  0.5,
			MinFollowers:          1,
			MaxFollowerLagSeconds: 120,
			DetectedAt:            start.Add(6 * time.Minute),
		}
	}

	if err := base().validate(); err != nil {
		t.Fatalf("the baseline finding does not validate: %v", err)
	}

	for _, tc := range []struct {
		name   string
		break_ func(*SteamSignal)
		why    string
	}{
		{"direction disagrees with the delta's sign", func(s *SteamSignal) {
			s.Direction = "drift"
		}, "steam_signals_direction_identity"},
		{"magnitude is not the delta's absolute value", func(s *SteamSignal) {
			s.MagnitudeProbabilityPoints = 0.07
		}, "steam_signals_magnitude_identity"},
		{"velocity disagrees in sign with the delta", func(s *SteamSignal) {
			s.VelocityProbabilityPerMinute = -0.02
		}, "steam_signals_velocity_sign"},
		{"the lead moved outside the half-open window", func(s *SteamSignal) {
			s.LeadMovedAt = s.WindowEnd
		}, "the window is [start, end)"},
		{"follower count disagrees with the follower list", func(s *SteamSignal) {
			s.FollowerCount = 3
		}, "steam_signals_follower_count_identity"},
		{"participating books is not followers plus the lead", func(s *SteamSignal) {
			s.ParticipatingBooks = 2
		}, "steam_signals_participating_identity"},
		{"followers are not ordered by ascending lag", func(s *SteamSignal) {
			s.Followers[0].LagSeconds, s.Followers[1].LagSeconds = 50, 30
		}, "a database cannot enforce JSONB array order, so this is the only place it is checked"},
		{"a follower lagged past the bound it claims to meet", func(s *SteamSignal) {
			s.Followers[1].LagSeconds = 500
		}, "max_follower_lag_seconds"},
		{"the finding does not meet its own magnitude threshold", func(s *SteamSignal) {
			s.ThresholdMagnitude = 0.5
		}, "steam_signals_meets_own_thresholds"},
		{"the finding does not meet its own correlation threshold", func(s *SteamSignal) {
			s.ThresholdCorrelation = 1.0
			s.CrossBookCorrelation = 0.6
		}, "steam_signals_meets_own_thresholds"},
		{"the window is inverted", func(s *SteamSignal) {
			s.WindowEnd = s.WindowStart.Add(-time.Minute)
		}, "steam_signals_window_ordered"},
		{"the hop is longer than the window", func(s *SteamSignal) {
			s.HopSeconds = 300
		}, "steam_signals_hop_range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.break_(&s)
			if err := s.validate(); err == nil {
				t.Fatalf("the finding was accepted; the database would refuse it (%s), abort the "+
					"transaction its siblings share, and return an error redelivery cannot fix",
					tc.why)
			}
		})
	}
}
