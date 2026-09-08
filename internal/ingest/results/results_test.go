package results

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"the zero value is a working poller", Config{}, true},
		{"negative interval", Config{Interval: -time.Second}, false},
		{"negative settle delay", Config{SettleDelay: -time.Hour}, false},
		{"negative batch size", Config{BatchSize: -1}, false},
		{"negative poll timeout", Config{PollTimeout: -time.Second}, false},
		{
			// The inversion that would make "backed off" mean "polled sooner".
			name: "backoff ceiling below the interval",
			cfg:  Config{Interval: time.Minute, ErrorBackoffMax: time.Second},
			ok:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.withDefaults().Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("Validate() accepted a configuration the poller cannot run")
				}
				if !errors.Is(err, ErrInvalidOptions) {
					t.Errorf("Validate() = %v, want it to wrap ErrInvalidOptions", err)
				}
			}
		})
	}
}

// TestALongIntervalRaisesTheBackoffCeiling is the one config interaction an
// operator can trip from the outside. SHARPLINE_INGEST_RESULTS_INTERVAL is a
// knob; ErrorBackoffMax is not. Defaulting the ceiling to a flat five minutes
// would refuse startup for anybody who set a ten-minute cadence, with nothing
// available to fix it.
func TestALongIntervalRaisesTheBackoffCeiling(t *testing.T) {
	t.Parallel()

	cfg := Config{Interval: 10 * time.Minute}.withDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an interval longer than the default backoff ceiling was refused: %v", err)
	}
	if cfg.ErrorBackoffMax < cfg.Interval {
		t.Errorf("ErrorBackoffMax = %s, below Interval %s", cfg.ErrorBackoffMax, cfg.Interval)
	}
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	src := newFakeProvider(nil)

	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"no provider", Options{Store: store, Logger: discardLogger()}},
		{"no store", Options{Provider: src, Logger: discardLogger()}},
		{"no logger", Options{Provider: src, Store: store}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("New = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

// TestDelayBacksOffAndIsCapped. The doubling is computed by shifting, and the
// property that matters is that it cannot overflow into a negative duration
// after a long outage — a negative timer fires immediately, which turns a
// backoff into a retry storm at the worst possible moment.
func TestDelayBacksOffAndIsCapped(t *testing.T) {
	t.Parallel()

	p := newPoller(t, newFakeStore(), newFakeProvider(nil), Config{})

	if got := p.delay(0); got != p.cfg.Interval {
		t.Errorf("delay(0) = %s, want the interval %s", got, p.cfg.Interval)
	}
	if got := p.delay(1); got != p.cfg.Interval {
		t.Errorf("delay(1) = %s, want the interval %s (the first failure waits one interval)",
			got, p.cfg.Interval)
	}
	if p.delay(2) <= p.delay(1) {
		t.Error("the second consecutive failure did not back off further than the first")
	}
	// Far past the shift bound, which is where an accumulating implementation
	// would go negative.
	for _, failures := range []int{20, 1_000, 1_000_000} {
		got := p.delay(failures)
		if got <= 0 {
			t.Errorf("delay(%d) = %s; a non-positive delay fires immediately", failures, got)
		}
		if got > p.cfg.ErrorBackoffMax {
			t.Errorf("delay(%d) = %s, above the ceiling %s", failures, got, p.cfg.ErrorBackoffMax)
		}
	}
}

// -----------------------------------------------------------------------------
// One tick
// -----------------------------------------------------------------------------

// TestTickComputesTheHorizonFromTheInjectedClock. The horizon is the caller's,
// never the database's: queries/results.sql refuses to read now() precisely so
// that this number is testable at a fixed instant.
func TestTickComputesTheHorizonFromTheInjectedClock(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	p := newPoller(t, store, newFakeProvider(nil), Config{SettleDelay: 3 * time.Hour, BatchSize: 17})

	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	q := store.lastQuery(t)
	want := testNow.Add(-3 * time.Hour)
	if !q.FinishedBefore.Equal(want) {
		t.Errorf("horizon = %s, want %s (now − SettleDelay)", q.FinishedBefore, want)
	}
	if q.Limit != 17 {
		t.Errorf("limit = %d, want the configured batch size 17", q.Limit)
	}
}

// TestAnEmptyQueueDoesNotCallTheProvider. Most ticks find nothing, and a
// provider call per empty tick would spend quota — a real one is a separate
// billed endpoint — to be told what the database already said.
func TestAnEmptyQueueDoesNotCallTheProvider(t *testing.T) {
	t.Parallel()

	src := newFakeProvider(nil)
	p := newPoller(t, newFakeStore(), src, Config{})

	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if src.calls() != 0 {
		t.Errorf("the provider was asked %d times about an empty queue", src.calls())
	}
}

// TestTickRecordsTheProvidersResult is the happy path, end to end through one
// tick: the queue is read, the window reaches back over the contest, its
// provider key is resolved onto the row, and the provider's own instant reaches
// the store unchanged.
func TestTickRecordsTheProvidersResult(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	ended := testNow.Add(-90 * time.Minute)
	store := newFakeStore(pending)
	src := newFakeProvider(func(w provider.ResultWindow) ([]provider.FinalResult, error) {
		if !w.Covers(ended) {
			t.Errorf("window = %s, which does not cover the instant the contest ended (%s)", w, ended)
		}
		// The provider answers in its OWN identifier space. Nothing it states is
		// a domain identifier, and the poller is what crosses between them.
		return []provider.FinalResult{endedResult(t, "syn-nba-1", ended)}, nil
	})

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	got, ok := store.result(pending.EventID)
	if !ok {
		t.Fatal("no result was recorded")
	}
	if !got.FinalisedAt.Equal(ended) {
		// The whole point of FinalisedAt. Stamping the tick's own clock would
		// restamp a customer's settled ticket with the instant of a redelivery
		// and would make the settlement-lag histogram read zero for ever.
		t.Errorf("stored instant = %s, want the provider's %s", got.FinalisedAt, ended)
	}
	if got.Score.Home() != 104 || got.Score.Away() != 99 {
		t.Errorf("stored score = %s, want 104-99", got.Score)
	}
}

// TestAReplayedResultIsNotAnError is the property that lets the poller keep no
// memo of what it has recorded. The second tick writes nothing and must report
// success: a zero row count means "already recorded", not "lost".
func TestAReplayedResultIsNotAnError(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	store := newFakeStore(pending)
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return []provider.FinalResult{endedResult(t, "syn-nba-1", testNow.Add(-90*time.Minute))}, nil
	})
	p := newPoller(t, store, src, Config{})

	for i := range 3 {
		if err := p.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
}

// TestAnUnsolicitedResultIsDroppedAndDoesNotFailTheTick.
//
// Two shapes, one rule. A contest the work queue was not waiting on is ORDINARY
// under a window query — the provider is answering "what finished", and it
// covers contests this deployment never ingested — while a second statement
// about one contest in one answer is an adapter bug. Both are unattributable,
// both are dropped, and neither fails the tick: writing a terminal status onto a
// row the poller cannot confidently name is the one mistake that cannot be taken
// back, because settle would grade on it, and halting every other customer's
// settlement over it would be the worse failure. The counter is what a reviewer
// looks at.
func TestAnUnsolicitedResultIsDroppedAndDoesNotFailTheTick(t *testing.T) {
	t.Parallel()

	asked := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	store := newFakeStore(asked)
	// The stranger is a row that EXISTS in the table, so the only thing stopping
	// it being written is the poller's own check rather than the store's.
	stranger := eventIDFor(t, "syn-nba-2")
	store.rows[stranger] = testNow.Add(-5 * time.Hour)

	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return []provider.FinalResult{
			endedResult(t, "syn-nba-2", testNow.Add(-2*time.Hour)),
			endedResult(t, "syn-nba-1", testNow.Add(-90*time.Minute)),
			// A duplicate of one the queue IS waiting on: the second copy is
			// as unattributable as an outcome for a contest nobody holds.
			endedResult(t, "syn-nba-1", testNow.Add(-80*time.Minute)),
		}, nil
	})

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if _, ok := store.result(stranger); ok {
		t.Error("a result the work queue was not waiting on was written")
	}
	got, ok := store.result(asked.EventID)
	if !ok {
		t.Fatal("the result the queue was waiting on was not recorded")
	}
	if !got.FinalisedAt.Equal(testNow.Add(-90 * time.Minute)) {
		t.Errorf("the duplicate overwrote the first result: stored %s", got.FinalisedAt)
	}
}

// TestAnUnusableResultIsNotWritten. The failure this catches is an `ended`
// result with no score, which the events table would store happily — the schema
// constrains the score PAIR, not its presence — and which would then grade every
// spread on the contest against a 0-0 zero value. A plausible wrong number is
// worse than an error, so it is an error.
func TestAnUnusableResultIsNotWritten(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	store := newFakeStore(pending)
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return []provider.FinalResult{{
			EventKey:    "syn-nba-1",
			Status:      domain.EventStatusEnded,
			HasScore:    false,
			FinalisedAt: testNow.Add(-time.Hour),
		}}, nil
	})

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err == nil {
		t.Fatal("a scoreless ended result was accepted")
	}
	if _, ok := store.result(pending.EventID); ok {
		t.Error("a scoreless ended result reached the store")
	}
}

// TestAFailedWriteDoesNotAbandonTheBatch. Each contest is independent — a row
// that cannot be written says nothing about the next one — and stopping at the
// first failure would let one bad event hold up every other customer's
// settlement behind it.
func TestAFailedWriteDoesNotAbandonTheBatch(t *testing.T) {
	t.Parallel()

	first := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	second := pendingEvent(t, "syn-nba-2", 4*time.Hour)
	store := newFakeStore(first, second)
	store.writeErrs[first.EventID] = errors.New("connection reset")

	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return []provider.FinalResult{
			endedResult(t, "syn-nba-1", testNow.Add(-2*time.Hour)),
			endedResult(t, "syn-nba-2", testNow.Add(-2*time.Hour)),
		}, nil
	})

	p := newPoller(t, store, src, Config{})
	err := p.tick(context.Background())
	if err == nil {
		t.Fatal("a failed write was reported as a successful tick; the loop would not back off")
	}
	if _, ok := store.result(second.EventID); !ok {
		t.Error("the batch was abandoned at the first failure; the second contest never settled")
	}
}

// TestAnOutOfOrderResultIsDeclinedWithoutError. A redelivery, a replay or two
// ingest replicas racing must not overwrite a newer observation with an older
// one — and being declined by that guard is a steady state, not a fault.
func TestAnOutOfOrderResultIsDeclinedWithoutError(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	store := newFakeStore(pending)
	// Finalised BEFORE the row's stored observation: the odds path has seen this
	// contest alive more recently than the result claims it ended.
	stale := pending.ObservedAt.Add(-time.Hour)
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return []provider.FinalResult{endedResult(t, "syn-nba-1", stale)}, nil
	})

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("an out-of-order result was reported as a failure: %v", err)
	}
	if _, ok := store.result(pending.EventID); ok {
		t.Error("an older observation overwrote a newer one")
	}
}

// TestAResultForAContestNeverIngestedIsNotAnError. The statement is an UPDATE,
// so there is no row to write and no route by which a result can create a
// contest. Zero rows is the correct answer and not a failure.
func TestAResultForAContestNeverIngestedIsNotAnError(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	store := newFakeStore(pending)
	delete(store.rows, pending.EventID) // queued, but the row has since gone

	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return []provider.FinalResult{endedResult(t, "syn-nba-1", testNow.Add(-time.Hour))}, nil
	})

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func TestAFailedQueueReadFailsTheTick(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.listErr = errors.New("connection reset")
	src := newFakeProvider(nil)

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err == nil {
		t.Fatal("a failed work-queue read was reported as a successful tick")
	}
	if src.calls() != 0 {
		t.Error("the provider was asked about a queue that could not be read")
	}
}

// TestAProviderFailureFailsTheTickAndWritesNothing. The provider is the half of
// this loop that can be down while everything else is fine; the tick must fail
// so the loop backs off, and nothing may be written on a guess.
func TestAProviderFailureFailsTheTickAndWritesNothing(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	store := newFakeStore(pending)
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return nil, provider.Newf("results", provider.NameSynthetic,
			provider.DispositionRetryable, provider.ErrUnavailable, "scores endpoint is down")
	})

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err == nil {
		t.Fatal("a provider failure was reported as a successful tick")
	}
	if _, ok := store.result(pending.EventID); ok {
		t.Error("a result was written for a provider call that failed")
	}
}

// -----------------------------------------------------------------------------
// The loop
// -----------------------------------------------------------------------------

// TestRunStopsOnAProviderWithNoResultsEndpoint. This is the state a deployment
// with ODDS_API_KEY set is in today. It must stop the loop — retrying a
// capability that cannot appear at run time is a permanent no-op — and it must
// return nil, because the odds board is healthy and taking the process down to
// fix a settlement outage is strictly worse than the settlement outage.
func TestRunStopsOnAProviderWithNoResultsEndpoint(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return nil, provider.Newf("results", provider.NameSynthetic,
			provider.DispositionFatal, provider.ErrNotSupported, "prices only")
	})
	p := newPoller(t, newFakeStore(pending), src, Config{})

	// No cancellation: the test would hang if Run did not stop itself.
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run = %v, want nil; an odds-only provider is not a process failure", err)
	}
	if src.calls() != 1 {
		t.Errorf("the provider was asked %d times; a capability that cannot appear is asked once",
			src.calls())
	}
}

func TestRunReturnsNilOnCancellation(t *testing.T) {
	t.Parallel()

	p := newPoller(t, newFakeStore(), newFakeProvider(nil), Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run on a cancelled context = %v, want nil", err)
	}
}

// TestRunPollsImmediately. A deploy otherwise leaves every contest that finished
// during the restart waiting an extra interval for no reason, and the immediate
// first tick is what makes a restart the fix for a stalled feed rather than a
// further delay.
func TestRunPollsImmediately(t *testing.T) {
	t.Parallel()

	polled := make(chan struct{}, 1)
	store := newFakeStore(pendingEvent(t, "syn-nba-1", 4*time.Hour))
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return nil, nil
	})
	// An interval far longer than the test could wait for, so a pass can only
	// mean the FIRST tick fired without waiting for it.
	p := newPoller(t, store, src, Config{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	select {
	case <-polled:
	case <-time.After(5 * time.Second):
		t.Fatal("the first tick did not fire immediately")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

// TestRunRefusesASecondConcurrentLoop. One poller is one loop; two would double
// every write's contention for no extra throughput, since the writes are guarded
// and idempotent.
func TestRunRefusesASecondConcurrentLoop(t *testing.T) {
	t.Parallel()

	// The provider call is the signal that the first loop has claimed the flag:
	// Run claims it before the first tick can reach the provider, so a receive
	// here means the second call below is racing nothing.
	polled := make(chan struct{}, 1)
	store := newFakeStore(pendingEvent(t, "syn-nba-1", 4*time.Hour))
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return nil, nil
	})
	p := newPoller(t, store, src, Config{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- p.Run(ctx) }()

	select {
	case <-polled:
	case <-time.After(5 * time.Second):
		t.Fatal("the first loop never started")
	}

	// Cancelled, so a poller that wrongly admitted a second loop returns
	// promptly instead of hanging this test for the whole timeout.
	second, stop := context.WithCancel(context.Background())
	stop()
	if err := p.Run(second); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("a second concurrent Run = %v, want ErrAlreadyRunning", err)
	}

	cancel()
	if err := <-first; err != nil {
		t.Errorf("the first Run = %v, want nil", err)
	}
}

// TestProviderReportsTheSource. cmd/ingest asserts this against the odds
// adapter's name at startup, which is what stops a deployment taking its prices
// from one source and its outcomes from another.
func TestProviderReportsTheSource(t *testing.T) {
	t.Parallel()

	p := newPoller(t, newFakeStore(), newFakeProvider(nil), Config{})
	if got := p.Provider(); got != provider.NameSynthetic {
		t.Errorf("Provider() = %s, want %s", got, provider.NameSynthetic)
	}
}

// TestTheWindowCoversEveryContestOnTheWorkQueue. The window is what replaced a
// per-contest query, and it is only sound because a contest cannot finish before
// it begins: the oldest queued start is therefore a lower bound on every queued
// contest's finishing instant. A window that opened later than that would strand
// exactly the contests that had been waiting longest, silently.
func TestTheWindowCoversEveryContestOnTheWorkQueue(t *testing.T) {
	t.Parallel()

	// Deliberately NOT in queue order, so a poller that trusted the ORDER BY
	// rather than taking the minimum would ask about the wrong span.
	recent := pendingEvent(t, "syn-nba-recent", 3*time.Hour)
	oldest := pendingEvent(t, "syn-nba-oldest", 40*time.Hour)
	store := newFakeStore(recent, oldest)
	src := newFakeProvider(nil)

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	w := src.lastWindow(t)
	if err := w.Validate(); err != nil {
		t.Fatalf("the poller asked a malformed window %s: %v", w, err)
	}
	for _, e := range []PendingEvent{recent, oldest} {
		if !w.Covers(e.ScheduledStart) {
			t.Errorf("window %s does not reach back to %s, the start of a queued contest",
				w, e.ScheduledStart)
		}
	}
	if !w.Until.Equal(testNow) {
		t.Errorf("window ends at %s, want the tick's own clock reading %s", w.Until, testNow)
	}
	if !w.Since.Equal(oldest.ScheduledStart) {
		t.Errorf("window begins at %s, want the oldest queued start %s", w.Since, oldest.ScheduledStart)
	}
}

// TestAResultIsResolvedIntoTheIdentifierTheDatabaseHolds is the crossing this
// package exists to make, asserted at the seam.
//
// The provider states `syn-nba-1`; the row is `synthetic.e.syn-nba-1`. Nothing
// downstream of the poller knows the provider's spelling and nothing upstream of
// it knows the database's, so if this derivation is not applied here it is
// applied nowhere — which is precisely the state that left 325 queried contests
// unresolved across 135 polls with no error anywhere.
func TestAResultIsResolvedIntoTheIdentifierTheDatabaseHolds(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	if pending.EventID.String() == "syn-nba-1" {
		t.Fatal("the derivation is the identity here; this test would prove nothing")
	}
	store := newFakeStore(pending)
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return []provider.FinalResult{endedResult(t, "syn-nba-1", testNow.Add(-time.Hour))}, nil
	})

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, ok := store.result(pending.EventID); !ok {
		t.Fatalf("the outcome of %q was not recorded onto %s, the identifier the database holds "+
			"for it", "syn-nba-1", pending.EventID)
	}
}

// TestAnOutcomeWithNoEventKeyIsDroppedWithoutFailingTheTick. It is
// unattributable in the same way an unsolicited one is — there is no identifier
// to derive — and writing a terminal status onto whatever an empty key happens
// to derive would put one row's result on every such contest.
func TestAnOutcomeWithNoEventKeyIsDroppedWithoutFailingTheTick(t *testing.T) {
	t.Parallel()

	pending := pendingEvent(t, "syn-nba-1", 4*time.Hour)
	store := newFakeStore(pending)
	src := newFakeProvider(func(provider.ResultWindow) ([]provider.FinalResult, error) {
		return []provider.FinalResult{
			{
				Status:      domain.EventStatusEnded,
				Score:       mustScore(t, 1, 0),
				HasScore:    true,
				FinalisedAt: testNow.Add(-time.Hour),
			},
			endedResult(t, "syn-nba-1", testNow.Add(-time.Hour)),
		}, nil
	})

	p := newPoller(t, store, src, Config{})
	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("an unattributable outcome failed the tick: %v", err)
	}
	if _, ok := store.result(pending.EventID); !ok {
		t.Error("the attributable result in the same batch was not recorded")
	}
}
