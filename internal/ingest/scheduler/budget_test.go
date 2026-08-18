// The quota limiter: CLAUDE.md §5's "token-bucket limiter with the budget as a
// config value", plus the period ledger that answers a different question.
//
// The two mechanisms are tested separately because conflating them is the bug
// budget.go's type comment exists to prevent: the bucket answers "may I issue a
// request right now", the ledger answers "how much of the month is left", and a
// caller with only the bucket would happily spend a month's allowance in a week
// because the bucket refills for ever.
package scheduler_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/ingest/scheduler"
)

// testClock is a manually advanced clock. It exists so the period-rollover and
// refill assertions are exact rather than being a sleep and a tolerance
// (CLAUDE.md §12: the clock is injected, not read globally).
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock(at time.Time) *testClock { return &testClock{at: at} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// mustBudget builds a Budget on a frozen clock.
func mustBudget(t *testing.T, q scheduler.Quota, clock *testClock) *scheduler.Budget {
	t.Helper()
	b, err := scheduler.NewBudget(q, clock.Now)
	if err != nil {
		t.Fatalf("NewBudget(%+v): %v", q, err)
	}
	return b
}

func TestNewBudgetRejectsAnInvalidQuota(t *testing.T) {
	t.Parallel()

	if _, err := scheduler.NewBudget(scheduler.Quota{}, time.Now); !errors.Is(err, scheduler.ErrInvalidConfig) {
		t.Errorf("NewBudget(Quota{}) = %v, want an error wrapping ErrInvalidConfig", err)
	}
}

// TestNewBudgetStartsFull covers the restart argument: a crash-loop that started
// empty would take a full refill interval to show any odds at all and would look
// like a broken adapter rather than a restarted one.
func TestNewBudgetStartsFull(t *testing.T) {
	t.Parallel()

	q := scheduler.Quota{Budget: 300, Period: 30 * 24 * time.Hour, Burst: 10}
	b := mustBudget(t, q, newTestClock(testNow))

	if got := b.Tokens(); got != 10 {
		t.Errorf("Tokens() = %v on a fresh budget, want the full burst (10)", got)
	}
	if got, want := b.Remaining(), q.Budget; got != want {
		t.Errorf("Remaining() = %d on a fresh budget, want %d", got, want)
	}
	if got, want := b.Limit(), q.Budget; got != want {
		t.Errorf("Limit() = %d, want %d", got, want)
	}
}

// TestAcquireZeroIsFree is the synthetic generator's path. It must not touch
// either mechanism, so the limiter can stay unconditionally in the code path for
// both adapters rather than having an off switch only the offline build
// exercises.
func TestAcquireZeroIsFree(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100, Period: time.Hour, Burst: 10}, newTestClock(testNow))

	for i := 0; i < 1000; i++ {
		if err := b.Acquire(context.Background(), 0); err != nil {
			t.Fatalf("Acquire(0) on iteration %d: %v", i, err)
		}
	}
	if got := b.Tokens(); got != 10 {
		t.Errorf("Tokens() = %v after 1000 free acquisitions, want 10", got)
	}
	if got := b.Remaining(); got != 100 {
		t.Errorf("Remaining() = %d after 1000 free acquisitions, want 100", got)
	}
}

func TestAcquireSpendsBothMechanisms(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100, Period: time.Hour, Burst: 10}, newTestClock(testNow))

	if err := b.Acquire(context.Background(), 3); err != nil {
		t.Fatalf("Acquire(3): %v", err)
	}
	if got := b.Tokens(); got != 7 {
		t.Errorf("Tokens() = %v, want 7", got)
	}
	if got := b.Remaining(); got != 97 {
		t.Errorf("Remaining() = %d, want 97", got)
	}
}

// TestAcquireRefusesImmediatelyWhenTheLedgerIsSpent is the ADR 0003 refusal.
//
// The IMMEDIACY is the assertion that matters. A caller that blocked here would
// present as a frozen board with no error, which is exactly the failure mode the
// ADR refuses: "never a board that silently shows hour-old prices as if they
// were live".
func TestAcquireRefusesImmediatelyWhenTheLedgerIsSpent(t *testing.T) {
	t.Parallel()

	// Burst == Budget, so the bucket can never be the thing that blocks; only
	// the ledger can.
	b := mustBudget(t, scheduler.Quota{Budget: 6, Period: time.Hour, Burst: 6}, newTestClock(testNow))

	for i := 0; i < 2; i++ {
		if err := b.Acquire(context.Background(), 3); err != nil {
			t.Fatalf("Acquire(3) #%d: %v", i+1, err)
		}
	}
	if got := b.Remaining(); got != 0 {
		t.Fatalf("Remaining() = %d after spending the whole budget, want 0", got)
	}

	start := time.Now()
	err := b.Acquire(context.Background(), 3)
	elapsed := time.Since(start)

	if !errors.Is(err, scheduler.ErrQuotaExhausted) {
		t.Fatalf("Acquire on an exhausted ledger = %v, want ErrQuotaExhausted", err)
	}
	if elapsed > time.Second {
		t.Errorf("Acquire on an exhausted ledger blocked for %s; it must refuse immediately", elapsed)
	}
}

// TestAcquireWaitsForTheBucketAndThenSucceeds exercises the pacing half against
// a real clock.
//
// Real time rather than the manual clock here, because the wait loop sleeps on a
// real timer: a frozen clock would never refill and the test would be asserting
// that Acquire hangs. The budget is generous and the period tiny so the refill
// rate is high and the wait is milliseconds.
func TestAcquireWaitsForTheBucketAndThenSucceeds(t *testing.T) {
	t.Parallel()

	// 100,000 credits per minute = 1,666/s, so a 2-credit deficit refills in
	// ~1.2ms, which the minBucketWait floor rounds up to 1ms.
	b, err := scheduler.NewBudget(scheduler.Quota{Budget: 100_000, Period: time.Minute, Burst: 2}, time.Now)
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := b.Acquire(ctx, 2); err != nil {
		t.Fatalf("first Acquire(2) drained the full bucket and should not have waited: %v", err)
	}
	if err := b.Acquire(ctx, 2); err != nil {
		t.Fatalf("second Acquire(2) should have waited for a refill and succeeded: %v", err)
	}
	if got := b.Remaining(); got != 100_000-4 {
		t.Errorf("Remaining() = %d, want %d", got, 100_000-4)
	}
}

// TestAcquireHonoursContextCancellationWhileWaiting: a frozen clock never
// refills, so the only way out is the caller's context. That is the third
// documented return value.
func TestAcquireHonoursContextCancellationWhileWaiting(t *testing.T) {
	t.Parallel()

	clock := newTestClock(testNow)
	b := mustBudget(t, scheduler.Quota{Budget: 100_000, Period: 30 * 24 * time.Hour, Burst: 1}, clock)

	if err := b.Acquire(context.Background(), 1); err != nil {
		t.Fatalf("Acquire(1) on a full bucket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := b.Acquire(ctx, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire while blocked on a bucket that never refills = %v, want context.DeadlineExceeded", err)
	}
}

// TestAcquireChecksTheContextBeforeSpending: an already-cancelled caller must
// not have a credit charged to it.
func TestAcquireRefusesAnAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100, Period: time.Hour, Burst: 10}, newTestClock(testNow))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Acquire(ctx, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire with a cancelled context = %v, want context.Canceled", err)
	}
	if got := b.Remaining(); got != 100 {
		t.Errorf("Remaining() = %d after a refused Acquire, want 100 — nothing may be charged", got)
	}
}

// TestRefundReturnsReservedCredits covers the narrow race the method exists for:
// the caller acquired and then lost its context before issuing the request.
func TestRefundReturnsReservedCredits(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100, Period: time.Hour, Burst: 10}, newTestClock(testNow))

	if err := b.Acquire(context.Background(), 4); err != nil {
		t.Fatalf("Acquire(4): %v", err)
	}
	b.Refund(4)

	if got := b.Tokens(); got != 10 {
		t.Errorf("Tokens() = %v after a full refund, want 10", got)
	}
	if got := b.Remaining(); got != 100 {
		t.Errorf("Remaining() = %d after a full refund, want 100", got)
	}
}

// TestRefundCannotMintCredits: neither mechanism may exceed its own ceiling, or
// the limiter becomes a source of quota rather than a bound on it.
func TestRefundCannotMintCredits(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100, Period: time.Hour, Burst: 10}, newTestClock(testNow))

	b.Refund(0)
	b.Refund(-5)
	b.Refund(1000)

	if got := b.Tokens(); got != 10 {
		t.Errorf("Tokens() = %v after an oversized refund, want the capacity (10)", got)
	}
	if got := b.Remaining(); got != 100 {
		t.Errorf("Remaining() = %d after an oversized refund, want the limit (100)", got)
	}
}

// TestReconcileIsAuthoritative is ADR 0003 implementation requirement #3: the
// gauge follows the provider's own x-requests-remaining, not a local estimate.
func TestReconcileIsAuthoritative(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100, Period: time.Hour, Burst: 10}, newTestClock(testNow))

	if err := b.Acquire(context.Background(), 3); err != nil {
		t.Fatalf("Acquire(3): %v", err)
	}
	if got := b.Remaining(); got != 97 {
		t.Fatalf("Remaining() = %d before reconciliation, want the local estimate 97", got)
	}

	// The provider says the local estimate was optimistic by a wide margin.
	b.Reconcile(42, 0)
	if got := b.Remaining(); got != 42 {
		t.Fatalf("Remaining() = %d after Reconcile(42), want 42 — the provider is authoritative", got)
	}

	// Subsequent local spending decrements the reported figure, so the number
	// stays a floor between reports rather than freezing until the next header.
	if err := b.Acquire(context.Background(), 2); err != nil {
		t.Fatalf("Acquire(2): %v", err)
	}
	if got := b.Remaining(); got != 40 {
		t.Errorf("Remaining() = %d after spending 2 against a reported 42, want 40", got)
	}
}

// TestReconcileIgnoresNoAnswer: a negative value is how an adapter says "the
// provider did not tell me". Treating it as zero would fire
// ProviderQuotaExhausted on every response that happened to omit the header.
func TestReconcileIgnoresNoAnswer(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100, Period: time.Hour, Burst: 10}, newTestClock(testNow))

	b.Reconcile(-1, 0)
	if got := b.Remaining(); got != 100 {
		t.Errorf("Remaining() = %d after Reconcile(-1), want the untouched local ledger (100)", got)
	}
}

// TestReconciledExhaustionStillRefuses: once the provider reports zero, the
// board freezes on the provider's word rather than on the local estimate.
func TestReconciledExhaustionStillRefuses(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100_000, Period: time.Hour, Burst: 1000}, newTestClock(testNow))

	b.Reconcile(1, 0)
	if err := b.Acquire(context.Background(), 3); !errors.Is(err, scheduler.ErrQuotaExhausted) {
		t.Fatalf("Acquire(3) against a provider-reported 1 credit = %v, want ErrQuotaExhausted", err)
	}
}

// TestPeriodRollsOver: the ledger resets when the configured period elapses, and
// the provider-reported override is cleared with it — a stale remaining count
// from the previous billing period must not survive into the new one.
func TestPeriodRollsOver(t *testing.T) {
	t.Parallel()

	clock := newTestClock(testNow)
	b := mustBudget(t, scheduler.Quota{Budget: 6, Period: time.Hour, Burst: 6}, clock)

	for i := 0; i < 2; i++ {
		if err := b.Acquire(context.Background(), 3); err != nil {
			t.Fatalf("Acquire(3) #%d: %v", i+1, err)
		}
	}
	b.Reconcile(0, 0)
	if got := b.Remaining(); got != 0 {
		t.Fatalf("Remaining() = %d, want 0", got)
	}

	clock.Advance(time.Hour)
	if got := b.Remaining(); got != 6 {
		t.Errorf("Remaining() = %d after the period elapsed, want the full budget (6)", got)
	}
	if err := b.Acquire(context.Background(), 3); err != nil {
		t.Errorf("Acquire(3) in the new period: %v", err)
	}
}

// TestRefillDoesNotMintOnANonMonotonicClock: a repeated or backwards reading
// must not create tokens.
func TestRefillDoesNotMintOnANonMonotonicClock(t *testing.T) {
	t.Parallel()

	clock := newTestClock(testNow)
	b := mustBudget(t, scheduler.Quota{Budget: 100_000, Period: time.Minute, Burst: 10}, clock)

	if err := b.Acquire(context.Background(), 10); err != nil {
		t.Fatalf("Acquire(10): %v", err)
	}
	clock.Advance(-time.Hour)
	if got := b.Tokens(); got > 0.0001 {
		t.Errorf("Tokens() = %v after the clock went backwards, want ~0", got)
	}
}

// TestRefillIsProportionalToElapsedTime pins the pacing rate.
func TestRefillIsProportionalToElapsedTime(t *testing.T) {
	t.Parallel()

	clock := newTestClock(testNow)
	// 3,600 credits an hour = 1 credit a second.
	b := mustBudget(t, scheduler.Quota{Budget: 3600, Period: time.Hour, Burst: 100}, clock)

	if err := b.Acquire(context.Background(), 100); err != nil {
		t.Fatalf("Acquire(100): %v", err)
	}
	clock.Advance(30 * time.Second)
	if got := b.Tokens(); math.Abs(got-30) > 1e-6 {
		t.Errorf("Tokens() = %v after 30s at 1 credit/s, want 30", got)
	}

	// And it clamps at the capacity rather than accumulating for ever.
	clock.Advance(time.Hour)
	if got := b.Tokens(); got != 100 {
		t.Errorf("Tokens() = %v after an hour, want the capacity (100)", got)
	}
}

// TestBudgetIsSafeForConcurrentLeagues. Every league goroutine shares one
// Budget; this is the assertion `make test-race` is for.
func TestBudgetIsSafeForConcurrentLeagues(t *testing.T) {
	t.Parallel()

	const (
		goroutines = 32
		perG       = 25
		credits    = 1
	)
	b := mustBudget(t,
		scheduler.Quota{Budget: 100_000, Period: time.Hour, Burst: goroutines * perG * credits},
		newTestClock(testNow))

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				if err := b.Acquire(context.Background(), credits); err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	want := 100_000 - goroutines*perG*credits
	if got := b.Remaining(); got != want {
		t.Errorf("Remaining() = %d after %d concurrent acquisitions, want %d",
			got, goroutines*perG, want)
	}
}

// TestBudgetLogValue: the limiter is logged whole at startup and at shutdown, so
// the fields an operator reads are part of its surface.
func TestBudgetLogValue(t *testing.T) {
	t.Parallel()

	b := mustBudget(t, scheduler.Quota{Budget: 100, Period: time.Hour, Burst: 10}, newTestClock(testNow))
	if err := b.Acquire(context.Background(), 4); err != nil {
		t.Fatalf("Acquire(4): %v", err)
	}

	attrs := map[string]bool{}
	for _, a := range b.LogValue().Group() {
		attrs[a.Key] = true
	}
	for _, key := range []string{
		"limit", "remaining", "spent_this_period", "provider_reported",
		"period", "bucket_tokens", "bucket_capacity",
	} {
		if !attrs[key] {
			t.Errorf("Budget.LogValue() has no %q attribute", key)
		}
	}
}

// TestReconciledRemainingAndLimitComeFromOneAuthority.
//
// Budget.Limit's doc says the two gauges "must come from the same place",
// because ProviderQuotaLow divides one by the other. Until this test existed
// that was an assertion in a comment, and it was false in production: the live
// stack reported sharpline_provider_quota_remaining = 4,999,952 against
// sharpline_provider_quota_limit = 100,000 — the provider's remaining measured
// against the LOCAL limit, a ratio of 5,000%. An alert defined as
// remaining/limit < 0.1 cannot fire from that, so the quota alert was silently
// dead on the only path that had a quota reading.
func TestReconciledRemainingAndLimitComeFromOneAuthority(t *testing.T) {
	t.Parallel()

	b, err := scheduler.NewBudget(
		scheduler.Quota{Budget: 100_000, Period: 30 * 24 * time.Hour, Burst: 1_000},
		func() time.Time { return time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC) },
	)
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}

	// Before any provider reading, both sides are the configured budget.
	if got, want := b.Limit(), 100_000; got != want {
		t.Fatalf("Limit() = %d before any reading, want the configured %d", got, want)
	}

	// The provider reports a budget fifty times larger than the local guess,
	// which is exactly the synthetic adapter's case.
	b.Reconcile(4_999_952, 5_000_000)

	if got, want := b.Remaining(), 4_999_952; got != want {
		t.Errorf("Remaining() = %d, want the provider's %d", got, want)
	}
	if got, want := b.Limit(), 5_000_000; got != want {
		t.Errorf("Limit() = %d, want the provider's %d — the two gauges must come from ONE "+
			"authority or their ratio is meaningless", got, want)
	}
	if r := float64(b.Remaining()) / float64(b.Limit()); r > 1.0 {
		t.Errorf("remaining/limit = %.4f, which is above 1.0; ProviderQuotaLow can never fire "+
			"against a ratio that starts above its own threshold", r)
	}
}

// TestReconcileWithoutALimitKeepsTheConfiguredOne.
//
// A provider that sends a remaining but no plan size is a real case — The Odds
// API's headers carry `x-requests-remaining` and `x-requests-used` but the plan
// size only follows from their sum. Keeping the configured limit is the closest
// same-source pair available, and it must not be clobbered with a zero.
func TestReconcileWithoutALimitKeepsTheConfiguredOne(t *testing.T) {
	t.Parallel()

	b, err := scheduler.NewBudget(
		scheduler.Quota{Budget: 100_000, Period: 30 * 24 * time.Hour, Burst: 1_000},
		func() time.Time { return time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC) },
	)
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}

	b.Reconcile(90_000, 0)

	if got, want := b.Remaining(), 90_000; got != want {
		t.Errorf("Remaining() = %d, want %d", got, want)
	}
	if got, want := b.Limit(), 100_000; got != want {
		t.Errorf("Limit() = %d, want the configured %d — a zero limit is 'the provider did not "+
			"say', not 'the budget is zero'", got, want)
	}
}
