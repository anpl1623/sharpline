// Tests for the closing-line-value pass.
//
// # The assertion this file exists for
//
// CLV MUST NOT BE ABLE TO FAIL A SETTLEMENT. Most of what is below is an attempt
// to break that in the ways it could plausibly be broken: a measurer that always
// refuses, a bus that never acknowledges, a store that always errors, a queue
// that cannot be advanced. In every one of them [CLVPass.Run] must return nil,
// the loop must keep running, and nothing must reach the settlement path — which
// it cannot, because settle.go holds no reference to any of this and the compiler
// says so.
//
// The second theme is the one migrations/00009 states: ABSENCE IS MEANINGFUL. A
// leg that cannot be measured must produce NO ROW and a counted reason, never a
// row of zeros, because a leaderboard cannot tell "we could not measure it" from
// "it measured zero".
//
// Nothing here touches a database or a broker.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/anpl1623/sharpline/internal/analytics/clv"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

var (
	clvKickoff  = time.Date(2026, 8, 20, 19, 0, 0, 0, time.UTC)
	clvBookedAt = clvKickoff.Add(-2 * time.Hour)
	clvClosedAt = clvKickoff.Add(-time.Minute)
	clvGradedAt = clvKickoff.Add(3 * time.Hour)
	clvNow      = clvGradedAt.Add(time.Minute)
)

// clvLeg builds one work-queue leg.
func clvLeg(id string, gradedAt time.Time) clv.Leg {
	return clv.Leg{
		LegID:       domain.LegID(id),
		WagerID:     "wgr-1",
		UserID:      "usr-1",
		EventID:     "evt-1",
		MarketID:    "mkt-1",
		MarketType:  domain.MarketTypeMoneyline,
		SelectionID: "sel-home",
		Book:        "book-sharpline",
		Decimal:     odds.Decimal(2.10),
		ObservedAt:  clvBookedAt,
		Status:      domain.LegStatusWon,
		GradedAt:    gradedAt,
	}
}

// clvFairSnapshot builds a devigged two-way market.
func clvFairSnapshot(t *testing.T, book string, home, away float64, at time.Time) odds.FairMarketSnapshot {
	t.Helper()
	snap, err := odds.NewFairMarketSnapshot(odds.FairMarketSnapshotParams{
		Market:     "mkt-1",
		Book:       domain.BookID(book),
		Line:       domain.NoLine(),
		ObservedAt: at,
		Fair: []odds.FairSelection{
			{Selection: "sel-home", Fair: odds.Probability(home)},
			{Selection: "sel-away", Fair: odds.Probability(away)},
		},
	})
	if err != nil {
		t.Fatalf("odds.NewFairMarketSnapshot: %v", err)
	}
	return snap
}

// clvMeasurement builds a real measurement by running the domain's own
// evaluation, so every identity [LegCLV.Validate] re-checks holds by
// construction rather than by the test author having done the arithmetic.
func clvMeasurement(t *testing.T, leg clv.Leg) clv.Measurement {
	t.Helper()
	taken := clvFairSnapshot(t, string(leg.Book), 0.46, 0.54, clvBookedAt)
	closing := clvFairSnapshot(t, string(leg.Book), 0.51, 0.49, clvClosedAt)

	result, err := odds.EvaluateCLV(taken, closing, leg.SelectionID)
	if err != nil {
		t.Fatalf("odds.EvaluateCLV: %v", err)
	}
	if !result.Beat {
		t.Fatalf("fixture should beat the close; probability CLV = %v", result.ProbabilityCLV)
	}
	return clv.Measurement{
		Leg:         leg,
		LeagueID:    "lg-1",
		ClosingBook: leg.Book,
		DevigMethod: odds.MethodMultiplicative,
		Result:      result,
		ComputedAt:  clvNow,
	}
}

// -----------------------------------------------------------------------------
// Fakes
// -----------------------------------------------------------------------------

// clvFakeMeasurer answers per leg id, so one pass can mix measured, unmeasurable
// and failing legs — which is the shape the walk-advances test needs.
type clvFakeMeasurer struct {
	byLeg    map[domain.LegID]func(clv.Leg) (clv.Measurement, error)
	fallback func(clv.Leg) (clv.Measurement, error)
	calls    []domain.LegID
}

func (m *clvFakeMeasurer) Measure(_ context.Context, leg clv.Leg) (clv.Measurement, error) {
	m.calls = append(m.calls, leg.LegID)
	if fn, ok := m.byLeg[leg.LegID]; ok {
		return fn(leg)
	}
	if m.fallback != nil {
		return m.fallback(leg)
	}
	return clv.Measurement{}, errors.New("clvFakeMeasurer: no answer configured")
}

// clvFakeStore is a work queue plus a recording writer.
//
// The queue answers the [from, to) window the way the real statement does,
// oldest-first and bounded, so the walk's stepping logic is exercised rather than
// bypassed. Legs move out of the queue only when a measurement is WRITTEN, which
// is what makes the "publish failed, so no row" assertions meaningful.
type clvFakeStore struct {
	queue    []clv.Leg
	written  map[domain.LegID]clv.Measurement
	order    []string
	readErr  error
	writeErr error
	reads    int
}

func newCLVFakeStore(legs ...clv.Leg) *clvFakeStore {
	return &clvFakeStore{queue: legs, written: map[domain.LegID]clv.Measurement{}}
}

func (s *clvFakeStore) GradedLegsAwaitingCLV(
	_ context.Context, from, to time.Time, limit int,
) ([]clv.Leg, error) {
	s.reads++
	if s.readErr != nil {
		return nil, s.readErr
	}
	out := make([]clv.Leg, 0, limit)
	for _, leg := range s.queue {
		if _, done := s.written[leg.LegID]; done {
			continue
		}
		if leg.GradedAt.Before(from) || !leg.GradedAt.Before(to) {
			continue
		}
		out = append(out, leg)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *clvFakeStore) WriteLegCLV(_ context.Context, m clv.Measurement) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.order = append(s.order, "write:"+m.Leg.LegID.String())
	s.written[m.Leg.LegID] = m
	return nil
}

// clvFakePublisher records what reached signals.clv.
type clvFakePublisher struct {
	sent  []LegCLV
	order *[]string
	err   error
}

func (p *clvFakePublisher) PublishCLVSignal(
	_ context.Context, _ domain.WagerID, msg kafka.Message,
) error {
	if p.err != nil {
		return p.err
	}
	rec, ok := msg.Payload.(LegCLV)
	if !ok {
		return errors.New("clvFakePublisher: payload is not a LegCLV")
	}
	p.sent = append(p.sent, rec)
	if p.order != nil {
		*p.order = append(*p.order, "publish:"+rec.LegID.String())
	}
	return nil
}

// clvHarness wires a pass over the fakes with a frozen clock and unregistered
// metrics.
type clvHarness struct {
	pass      *CLVPass
	measurer  *clvFakeMeasurer
	store     *clvFakeStore
	publisher *clvFakePublisher
	metrics   *CLVMetrics
}

func newCLVHarness(t *testing.T, measurer *clvFakeMeasurer, store *clvFakeStore, pub *clvFakePublisher) *clvHarness {
	t.Helper()
	pub.order = &store.order

	pass, err := NewCLVPass(CLVOptions{
		Measurer:  measurer,
		Store:     store,
		Publisher: pub,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     func() time.Time { return clvNow },
	})
	if err != nil {
		t.Fatalf("NewCLVPass: %v", err)
	}
	return &clvHarness{pass: pass, measurer: measurer, store: store, publisher: pub, metrics: pass.Metrics()}
}

func (h *clvHarness) legCount(t *testing.T, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(h.metrics.legs.WithLabelValues(outcome))
}

func (h *clvHarness) reasonCount(t *testing.T, reason clv.Reason) float64 {
	t.Helper()
	return testutil.ToFloat64(h.metrics.unmeasurable.WithLabelValues(reason.String()))
}

// -----------------------------------------------------------------------------
// The happy path, and the publish-before-persist ordering
// -----------------------------------------------------------------------------

func TestCLVPassPublishesThenPersists(t *testing.T) {
	leg := clvLeg("leg-1", clvGradedAt)
	store := newCLVFakeStore(leg)
	pub := &clvFakePublisher{}
	h := newCLVHarness(t, &clvFakeMeasurer{
		fallback: func(l clv.Leg) (clv.Measurement, error) { return clvMeasurement(t, l), nil },
	}, store, pub)

	h.pass.pass(context.Background())

	if len(pub.sent) != 1 {
		t.Fatalf("published %d records, want 1", len(pub.sent))
	}
	if _, ok := store.written[leg.LegID]; !ok {
		t.Fatal("no row was written for a measured leg")
	}
	// The order is the interlock: a row written before a successful publish takes
	// the leg off the queue for ever and loses a signal nothing re-derives.
	want := []string{"publish:leg-1", "write:leg-1"}
	if len(store.order) != len(want) {
		t.Fatalf("call order = %v, want %v", store.order, want)
	}
	for i := range want {
		if store.order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", store.order, want)
		}
	}
	if got := h.legCount(t, clvLegMeasured); got != 1 {
		t.Errorf("measured legs = %v, want 1", got)
	}
}

// TestCLVPassPublishedRecordIsSelfDescribing pins the fields that make the record
// reproducible by a second implementation: the devig method and both declared
// lookbacks. A record without them is a number a phase-12 job cannot be checked
// against, because a disagreement could be about the arithmetic or about which
// quote was the close.
func TestCLVPassPublishedRecordIsSelfDescribing(t *testing.T) {
	leg := clvLeg("leg-1", clvGradedAt)
	store := newCLVFakeStore(leg)
	pub := &clvFakePublisher{}
	h := newCLVHarness(t, &clvFakeMeasurer{
		fallback: func(l clv.Leg) (clv.Measurement, error) { return clvMeasurement(t, l), nil },
	}, store, pub)

	h.pass.pass(context.Background())

	if len(pub.sent) != 1 {
		t.Fatalf("published %d records, want 1", len(pub.sent))
	}
	rec := pub.sent[0]
	if err := rec.Validate(); err != nil {
		t.Fatalf("published record does not validate: %v", err)
	}
	if rec.SchemaVersion != CLVSchemaVersion {
		t.Errorf("schema version = %d, want %d", rec.SchemaVersion, CLVSchemaVersion)
	}
	if rec.DevigMethod != odds.MethodMultiplicative {
		t.Errorf("devig method = %s, want multiplicative", rec.DevigMethod)
	}
	if rec.ClosingLookbackSeconds != clv.DefaultClosingLookback.Seconds() {
		t.Errorf("closing lookback = %v s, want %v s",
			rec.ClosingLookbackSeconds, clv.DefaultClosingLookback.Seconds())
	}
	if rec.TakenLookbackSeconds != clv.DefaultTakenLookback.Seconds() {
		t.Errorf("taken lookback = %v s, want %v s",
			rec.TakenLookbackSeconds, clv.DefaultTakenLookback.Seconds())
	}
	if !rec.GradedAt.Equal(clvGradedAt) {
		t.Errorf("graded at = %s, want the result's finalisation instant %s", rec.GradedAt, clvGradedAt)
	}
}

// -----------------------------------------------------------------------------
// Failures, and what each of them costs
// -----------------------------------------------------------------------------

func TestCLVPassPublishFailureWritesNoRow(t *testing.T) {
	leg := clvLeg("leg-1", clvGradedAt)
	store := newCLVFakeStore(leg)
	pub := &clvFakePublisher{err: errors.New("broker unavailable")}
	h := newCLVHarness(t, &clvFakeMeasurer{
		fallback: func(l clv.Leg) (clv.Measurement, error) { return clvMeasurement(t, l), nil },
	}, store, pub)

	h.pass.pass(context.Background())

	if len(store.written) != 0 {
		t.Fatal("a row was written for a measurement whose signal was never published")
	}
	if got := h.legCount(t, clvLegFailed); got != 1 {
		t.Errorf("failed legs = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.publishFailures); got != 1 {
		t.Errorf("publish failures = %v, want 1", got)
	}
	// The leg is still on the queue, so the next pass retries it.
	legs, err := store.GradedLegsAwaitingCLV(context.Background(),
		clvNow.Add(-DefaultCLVRetryWindow), clvNow, DefaultCLVBatch)
	if err != nil {
		t.Fatalf("GradedLegsAwaitingCLV: %v", err)
	}
	if len(legs) != 1 {
		t.Errorf("queue holds %d legs after a failed publish, want 1", len(legs))
	}
}

func TestCLVPassStoreFailureIsCountedAndRetried(t *testing.T) {
	leg := clvLeg("leg-1", clvGradedAt)
	store := newCLVFakeStore(leg)
	store.writeErr = errors.New("deadlock detected")
	pub := &clvFakePublisher{}
	h := newCLVHarness(t, &clvFakeMeasurer{
		fallback: func(l clv.Leg) (clv.Measurement, error) { return clvMeasurement(t, l), nil },
	}, store, pub)

	h.pass.pass(context.Background())

	if got := h.legCount(t, clvLegFailed); got != 1 {
		t.Errorf("failed legs = %v, want 1", got)
	}
	// The signal WAS published. A duplicate on the next pass is the accepted
	// failure here, because both the topic and the row absorb it.
	if len(pub.sent) != 1 {
		t.Errorf("published %d records, want 1", len(pub.sent))
	}
}

func TestCLVPassUnmeasurableLegWritesNothingAndCountsTheReason(t *testing.T) {
	for _, reason := range []clv.Reason{
		clv.ReasonCloseBeforeTake,
		clv.ReasonClosingIncomplete,
		clv.ReasonOutcomeSetChanged,
		clv.ReasonNoClose,
	} {
		t.Run(reason.String(), func(t *testing.T) {
			leg := clvLeg("leg-1", clvGradedAt)
			store := newCLVFakeStore(leg)
			pub := &clvFakePublisher{}
			h := newCLVHarness(t, &clvFakeMeasurer{
				fallback: func(l clv.Leg) (clv.Measurement, error) {
					return clv.Measurement{}, &clv.UnmeasurableError{Leg: l.LegID, Reason: reason}
				},
			}, store, pub)

			h.pass.pass(context.Background())

			// ABSENCE IS MEANINGFUL: no row, not a row of zeros.
			if len(store.written) != 0 {
				t.Error("a row was written for a leg that could not be measured")
			}
			if len(pub.sent) != 0 {
				t.Error("a record was published for a leg that could not be measured")
			}
			if got := h.legCount(t, clvLegUnmeasurable); got != 1 {
				t.Errorf("unmeasurable legs = %v, want 1", got)
			}
			if got := h.reasonCount(t, reason); got != 1 {
				t.Errorf("reason %s = %v, want 1", reason, got)
			}
			// An exclusion is not a failure and must not be counted as one.
			if got := h.legCount(t, clvLegFailed); got != 0 {
				t.Errorf("failed legs = %v, want 0; an exclusion was counted as a failure", got)
			}
		})
	}
}

func TestCLVPassUnusableLegIsNotAnExclusion(t *testing.T) {
	leg := clvLeg("leg-1", clvGradedAt)
	store := newCLVFakeStore(leg)
	pub := &clvFakePublisher{}
	h := newCLVHarness(t, &clvFakeMeasurer{
		fallback: func(clv.Leg) (clv.Measurement, error) {
			return clv.Measurement{}, fmt.Errorf("%w: market type is unknown", clv.ErrUnusableLeg)
		},
	}, store, pub)

	h.pass.pass(context.Background())

	if got := h.legCount(t, clvLegUnusable); got != 1 {
		t.Errorf("unusable legs = %v, want 1", got)
	}
	if got := h.legCount(t, clvLegUnmeasurable); got != 0 {
		t.Errorf("unmeasurable legs = %v, want 0; a defect was filed as an analytics exclusion", got)
	}
}

// TestCLVPassQueueReadFailureIsCountedNotFatal proves a failed read costs a pass
// rather than the loop, and leaves nothing half-done.
func TestCLVPassQueueReadFailureIsCountedNotFatal(t *testing.T) {
	store := newCLVFakeStore(clvLeg("leg-1", clvGradedAt))
	store.readErr = errors.New("connection reset")
	pub := &clvFakePublisher{}
	h := newCLVHarness(t, &clvFakeMeasurer{}, store, pub)

	h.pass.pass(context.Background())

	if got := testutil.ToFloat64(h.metrics.passes.WithLabelValues(clvPassFailed)); got != 1 {
		t.Errorf("failed passes = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.passes.WithLabelValues(clvPassOK)); got != 0 {
		t.Errorf("ok passes = %v, want 0", got)
	}
	if len(store.written) != 0 || len(pub.sent) != 0 {
		t.Error("a failed queue read left work behind it")
	}
}

// -----------------------------------------------------------------------------
// The walk
// -----------------------------------------------------------------------------

// TestCLVPassWalksPastUnmeasurableLegs is the starvation test.
//
// The queue is ordered oldest-first and a leg that cannot be measured never
// leaves it. So a pass that read one batch and stopped would spend every pass on
// the same permanently-unmeasurable legs and never reach anything graded after
// them — for the whole retry window, which is a day.
//
// The fixture makes that failure certain if it exists: TWO stuck legs and a batch
// of two, so the first read is filled entirely by legs that will still be there
// on the next read, and the measurable leg is only reachable by stepping the
// window forward.
func TestCLVPassWalksPastUnmeasurableLegs(t *testing.T) {
	stuckA := clvLeg("leg-stuck-a", clvGradedAt.Add(-2*time.Minute))
	stuckB := clvLeg("leg-stuck-b", clvGradedAt.Add(-time.Minute))
	fresh := clvLeg("leg-fresh", clvGradedAt)
	store := newCLVFakeStore(stuckA, stuckB, fresh)
	pub := &clvFakePublisher{}

	stick := func(l clv.Leg) (clv.Measurement, error) {
		return clv.Measurement{}, &clv.UnmeasurableError{Leg: l.LegID, Reason: clv.ReasonCloseBeforeTake}
	}
	measurer := &clvFakeMeasurer{
		byLeg: map[domain.LegID]func(clv.Leg) (clv.Measurement, error){
			stuckA.LegID: stick,
			stuckB.LegID: stick,
			fresh.LegID:  func(l clv.Leg) (clv.Measurement, error) { return clvMeasurement(t, l), nil },
		},
	}

	pass, err := NewCLVPass(CLVOptions{
		Measurer:  measurer,
		Store:     store,
		Publisher: pub,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     func() time.Time { return clvNow },
		Batch:     2,
	})
	if err != nil {
		t.Fatalf("NewCLVPass: %v", err)
	}
	pub.order = &store.order

	pass.pass(context.Background())

	if _, ok := store.written[fresh.LegID]; !ok {
		t.Fatal("the leg behind a full batch of permanently unmeasurable ones was never measured")
	}
	if _, ok := store.written[stuckA.LegID]; ok {
		t.Fatal("a row was written for a leg that could not be measured")
	}
	if _, ok := store.written[stuckB.LegID]; ok {
		t.Fatal("a row was written for a leg that could not be measured")
	}
}

// TestCLVPassTerminatesOnASaturatedBatch covers the degenerate case the walk has
// to survive: a full batch entirely filled by legs sharing one grading instant,
// which the lower bound cannot be advanced past. The pass must stop rather than
// loop for ever re-reading the same rows.
func TestCLVPassTerminatesOnASaturatedBatch(t *testing.T) {
	// Two legs at the SAME instant with a batch of one: after the first, the
	// bound advances to that same instant and the read returns the same leg again.
	a := clvLeg("leg-a", clvGradedAt)
	b := clvLeg("leg-b", clvGradedAt)
	store := newCLVFakeStore(a, b)
	pub := &clvFakePublisher{}

	measurer := &clvFakeMeasurer{
		fallback: func(l clv.Leg) (clv.Measurement, error) {
			return clv.Measurement{}, &clv.UnmeasurableError{Leg: l.LegID, Reason: clv.ReasonCloseBeforeTake}
		},
	}
	pass, err := NewCLVPass(CLVOptions{
		Measurer:  measurer,
		Store:     store,
		Publisher: pub,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     func() time.Time { return clvNow },
		Batch:     1,
	})
	if err != nil {
		t.Fatalf("NewCLVPass: %v", err)
	}

	done := make(chan struct{})
	go func() {
		pass.pass(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not terminate on a saturated batch")
	}

	if store.reads > 8 {
		t.Errorf("pass made %d queue reads on a two-leg saturated batch; it is spinning", store.reads)
	}
}

// TestCLVPassWindowIsBoundedBelow pins the sliding window, which is what stops a
// permanently unmeasurable leg being retried for ever.
func TestCLVPassWindowIsBoundedBelow(t *testing.T) {
	old := clvLeg("leg-old", clvNow.Add(-DefaultCLVRetryWindow-time.Hour))
	recent := clvLeg("leg-recent", clvGradedAt)
	store := newCLVFakeStore(old, recent)
	pub := &clvFakePublisher{}
	h := newCLVHarness(t, &clvFakeMeasurer{
		fallback: func(l clv.Leg) (clv.Measurement, error) { return clvMeasurement(t, l), nil },
	}, store, pub)

	h.pass.pass(context.Background())

	if _, ok := store.written[recent.LegID]; !ok {
		t.Error("a leg inside the retry window was not measured")
	}
	if _, ok := store.written[old.LegID]; ok {
		t.Error("a leg older than the retry window was still being retried")
	}
}

// -----------------------------------------------------------------------------
// Nothing here can fail anything else
// -----------------------------------------------------------------------------

// TestCLVPassRunSurvivesTotalFailure is the file's headline assertion. Every
// dependency refuses, and Run must still report success and keep looping: this
// pass has no failure that should take the settle process — and therefore the
// settlement loop — down with it.
func TestCLVPassRunSurvivesTotalFailure(t *testing.T) {
	store := newCLVFakeStore(clvLeg("leg-1", clvGradedAt))
	store.readErr = errors.New("everything is on fire")
	pub := &clvFakePublisher{err: errors.New("so is the broker")}

	pass, err := NewCLVPass(CLVOptions{
		Measurer: &clvFakeMeasurer{
			fallback: func(clv.Leg) (clv.Measurement, error) {
				return clv.Measurement{}, errors.New("and the measurer")
			},
		},
		Store:        store,
		Publisher:    pub,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:        func() time.Time { return clvNow },
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewCLVPass: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := pass.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil; a CLV failure must never take the settle process down", err)
	}
	if pass.Running() {
		t.Error("Running() is still true after Run returned")
	}
	if store.reads < 2 {
		t.Errorf("the loop made %d reads in 50ms at a 1ms cadence; it stopped after the first failure",
			store.reads)
	}
}

// TestCLVPassIsNotAReadinessDependency guards the decision by construction. If
// *CLVPass ever satisfies httpx.Checker, a composition root can add it to the
// readiness set without noticing, and a wedged measurement would then take the
// replica out of rotation and stop it settling.
func TestCLVPassIsNotAReadinessDependency(t *testing.T) {
	var pass any = &CLVPass{}
	type checker interface {
		Name() string
		Check(context.Context) error
	}
	if _, ok := pass.(checker); ok {
		t.Fatal("*CLVPass satisfies the readiness checker interface; a wedged CLV measurement " +
			"can now take a settle replica out of rotation and stop it grading finished games")
	}
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

func TestNewCLVPassRefusesIncompleteOptions(t *testing.T) {
	base := func() CLVOptions {
		return CLVOptions{
			Measurer:  &clvFakeMeasurer{},
			Store:     newCLVFakeStore(),
			Publisher: &clvFakePublisher{},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	tests := []struct {
		name   string
		mutate func(*CLVOptions)
	}{
		{"no measurer", func(o *CLVOptions) { o.Measurer = nil }},
		{"no store", func(o *CLVOptions) { o.Store = nil }},
		{"no publisher", func(o *CLVOptions) { o.Publisher = nil }},
		{"no logger", func(o *CLVOptions) { o.Logger = nil }},
		{"negative poll interval", func(o *CLVOptions) { o.PollInterval = -time.Second }},
		{"negative batch", func(o *CLVOptions) { o.Batch = -1 }},
		{"negative retry window", func(o *CLVOptions) { o.RetryWindow = -time.Hour }},
		{"negative timeout", func(o *CLVOptions) { o.Timeout = -time.Second }},
		{"negative closing lookback", func(o *CLVOptions) { o.ClosingLookback = -time.Hour }},
		{"negative taken lookback", func(o *CLVOptions) { o.TakenLookback = -time.Hour }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			tc.mutate(&opts)
			if _, err := NewCLVPass(opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("NewCLVPass = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The published record's own invariants
// -----------------------------------------------------------------------------

// TestLegCLVValidateRejectsIncoherentRecords covers the four identities the
// record re-checks on decoded values. Each is a rule migrations/00009 also
// asserts in SQL, and a record that fails one is a record a second implementation
// would be validated against and misled by.
func TestLegCLVValidateRejectsIncoherentRecords(t *testing.T) {
	sound := func(t *testing.T) LegCLV {
		t.Helper()
		leg := clvLeg("leg-1", clvGradedAt)
		return newLegCLV(clvMeasurement(t, leg), clv.DefaultClosingLookback, clv.DefaultTakenLookback)
	}

	if err := sound(t).Validate(); err != nil {
		t.Fatalf("the sound fixture does not validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*LegCLV)
	}{
		{"wrong schema version", func(c *LegCLV) { c.SchemaVersion = CLVSchemaVersion + 1 }},
		{"probability CLV is not the difference of the two fair probabilities",
			func(c *LegCLV) { c.ProbabilityCLV += 0.01 }},
		{"magnitude is not the absolute percentage", func(c *LegCLV) { c.Magnitude = -c.Magnitude - 1 }},
		{"beat_close contradicts the probability CLV", func(c *LegCLV) { c.BeatClose = !c.BeatClose }},
		{"voided contradicts the leg status", func(c *LegCLV) { c.Voided = !c.Voided }},
		{"a push is reported as void", func(c *LegCLV) {
			c.LegStatus = domain.LegStatusPush
			c.Voided = true
		}},
		{"ungraded leg", func(c *LegCLV) { c.LegStatus = domain.LegStatusPending }},
		{"unknown devig method", func(c *LegCLV) { c.DevigMethod = odds.MethodUnknown }},
		{"close precedes the take", func(c *LegCLV) { c.ClosedAt = c.TakenAt.Add(-time.Second) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := sound(t)
			tc.mutate(&rec)
			if err := rec.Validate(); err == nil {
				t.Fatal("Validate accepted an incoherent record")
			}
		})
	}
}

// TestLegCLVAcceptsAPush pins the one status rule that is easy to get backwards:
// a PUSH is not void and is ranked at full weight, because excluding it would
// make a bettor's CLV depend on the scoreboard.
func TestLegCLVAcceptsAPush(t *testing.T) {
	leg := clvLeg("leg-1", clvGradedAt)
	leg.Status = domain.LegStatusPush
	rec := newLegCLV(clvMeasurement(t, leg), clv.DefaultClosingLookback, clv.DefaultTakenLookback)

	if rec.Voided {
		t.Fatal("a pushed leg was recorded as void")
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("Validate rejected a pushed leg: %v", err)
	}
}
