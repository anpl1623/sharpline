package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrQuotaExhausted means the configured provider budget for the current
// period is spent. It is returned by [Budget.Acquire] and it stops the sweep;
// it never triggers a silent substitution of synthetic data (see doc.go).
var ErrQuotaExhausted = errors.New("scheduler: provider quota exhausted")

// minBucketWait floors the sleep between two attempts to draw from the bucket.
// Without it, a refill rate that produces a sub-microsecond deficit turns the
// wait loop into a spin.
const minBucketWait = time.Millisecond

// Budget is the shared provider-quota limiter CLAUDE.md §5 mandates.
//
// It holds the two mechanisms doc.go describes, in one type because they must
// be decremented together or they drift:
//
//   - a TOKEN BUCKET that paces the burn rate, refilling at Budget/Period and
//     bursting to Quota.Burst credits;
//   - a PERIOD LEDGER counting credits spent against Budget, which is what
//     sharpline_provider_quota_remaining reports.
//
// They are genuinely different quantities and conflating them is the bug this
// comment exists to prevent. The bucket answers "may I issue a request right
// now, or would that burn the month too fast"; the ledger answers "how much of
// the month is left". A caller that only had the bucket would happily spend the
// entire monthly allowance in the first week, because the bucket refills for
// ever.
//
// # Provider-authoritative reconciliation
//
// ADR 0003 "Consequent implementation requirements" #3 requires the gauge to be
// fed from the provider's own `x-requests-remaining` header rather than from a
// local counter, "so the quota gauge is reconciled against the provider's own
// accounting rather than estimated". [Budget.Reconcile] is that path: once an
// adapter has reported a real remaining count, the ledger follows the provider
// and the local estimate is only a floor between reports.
//
// Safe for concurrent use by every league goroutine.
type Budget struct {
	mu sync.Mutex

	// Token bucket.
	capacity float64
	tokens   float64
	rate     float64 // credits per second
	last     time.Time

	// Period ledger.
	limit       int
	period      time.Duration
	periodStart time.Time
	spent       int

	// Provider-reported remaining, authoritative once set.
	reported    int
	hasReported bool

	// Provider-reported LIMIT, adopted with the remaining it arrived with.
	//
	// It exists because Remaining and Limit are the numerator and denominator
	// of one ratio and must therefore come from ONE authority. Adopting the
	// provider's remaining while keeping the locally-configured limit produced
	// sharpline_provider_quota_remaining = 4,999,952 against
	// sharpline_provider_quota_limit = 100,000 on the synthetic path -- a
	// ratio of 5,000%, which makes ProviderQuotaLow (remaining/limit below a
	// threshold) unable to fire at all. Measured on the live stack; this field
	// is the fix.
	//
	// Zero means the provider reported a remaining but no limit, in which case
	// the configured limit stands.
	reportedLimit int

	now func() time.Time
}

// NewBudget builds a limiter from a validated [Quota].
//
// The bucket starts FULL. A process restart therefore does not have to earn
// its first sweep, which matters because a crash-loop that started empty would
// take a full refill interval to show any odds at all and would look like a
// broken adapter rather than a restarted one.
func NewBudget(q Quota, now func() time.Time) (*Budget, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	capacity := float64(q.burst())
	t := now()
	return &Budget{
		capacity:    capacity,
		tokens:      capacity,
		rate:        float64(q.Budget) / q.Period.Seconds(),
		last:        t,
		limit:       q.Budget,
		period:      q.Period,
		periodStart: t,
		now:         now,
	}, nil
}

// Acquire blocks until n credits are available, then spends them.
//
// It returns:
//
//   - nil once the credits are spent;
//   - [ErrQuotaExhausted] immediately if the period ledger has nothing left,
//     without waiting — a caller that blocked here would present as a frozen
//     board with no error, which is the failure mode ADR 0003 explicitly
//     refuses ("never a board that silently shows hour-old prices as if they
//     were live");
//   - ctx.Err() if the caller gave up while waiting for the bucket.
//
// n == 0 is the synthetic provider and is admitted immediately without
// touching either mechanism. That is what lets the limiter stay unconditionally
// in the code path for both adapters, rather than having an off switch that
// only the offline build exercises.
func (b *Budget) Acquire(ctx context.Context, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n <= 0 {
		return nil
	}
	for {
		wait, err := b.tryAcquire(n)
		if err != nil {
			return err
		}
		if wait <= 0 {
			return nil
		}
		if wait < minBucketWait {
			wait = minBucketWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// tryAcquire spends n credits if the bucket allows it, and otherwise reports
// how long to wait for the deficit to refill.
func (b *Budget) tryAcquire(n int) (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.rollPeriodLocked()
	b.refillLocked()

	if b.remainingLocked() < n {
		return 0, fmt.Errorf("%w: %d credits left of %d for the period, %d needed",
			ErrQuotaExhausted, b.remainingLocked(), b.limit, n)
	}

	need := float64(n)
	if b.tokens >= need {
		b.tokens -= need
		b.spent += n
		if b.hasReported {
			b.reported -= n
		}
		return 0, nil
	}

	deficit := need - b.tokens
	return time.Duration(deficit / b.rate * float64(time.Second)), nil
}

// Refund returns n credits that were reserved but not spent — the narrow race
// where the caller acquired and then lost its context before issuing the
// request.
//
// It is deliberately NOT a general "the request failed, give me my credit
// back" API: ADR 0003 is explicit that "the credit is spent when the request is
// issued", so a failed HTTP call has already cost the money and refunding it
// would make the gauge optimistic in exactly the situation where it must not
// be.
func (b *Budget) Refund(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.tokens += float64(n)
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.spent -= n
	if b.spent < 0 {
		b.spent = 0
	}
	if b.hasReported {
		b.reported += n
	}
}

// Reconcile adopts the provider's own remaining-credit count as authoritative,
// together with the limit that count is measured against.
//
// A negative remaining is ignored: it is how an adapter says "the provider did
// not tell me", and treating that as zero would fire ProviderQuotaExhausted on
// every response that happened to omit the header.
//
// limit is adopted ONLY alongside a usable remaining, and a non-positive limit
// leaves the configured one in place. The pair moves together or not at all --
// see reportedLimit for what happens when it does not.
func (b *Budget) Reconcile(remaining, limit int) {
	if remaining < 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reported = remaining
	b.hasReported = true
	if limit > 0 {
		b.reportedLimit = limit
	}
}

// Remaining reports the credits left in the current period: the provider's own
// number once one has been reported, otherwise the local ledger.
//
// This is the value exported as sharpline_provider_quota_remaining.
func (b *Budget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollPeriodLocked()
	return b.remainingLocked()
}

// Limit reports the budget the remaining count is measured against, exported as
// sharpline_provider_quota_limit. ProviderQuotaLow divides one by the other, so
// they must come from the same place -- and this method is what makes that true
// rather than merely asserted: once the provider's own reading is authoritative
// for Remaining, its limit is authoritative here.
//
// A provider that reports a remaining but no limit keeps the configured one,
// which is the closest thing to a same-source pair available in that case.
func (b *Budget) Limit() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hasReported && b.reportedLimit > 0 {
		return b.reportedLimit
	}
	return b.limit
}

// Tokens reports the pacing bucket's current fill, for tests and for debug
// logging. It is NOT the quota gauge — see the type comment.
func (b *Budget) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	return b.tokens
}

// LogValue implements slog.LogValuer so a Budget can be logged whole.
func (b *Budget) LogValue() slog.Value {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollPeriodLocked()
	b.refillLocked()
	return slog.GroupValue(
		slog.Int("limit", b.limit),
		slog.Int("remaining", b.remainingLocked()),
		slog.Int("spent_this_period", b.spent),
		slog.Bool("provider_reported", b.hasReported),
		slog.String("period", b.period.String()),
		slog.Float64("bucket_tokens", b.tokens),
		slog.Float64("bucket_capacity", b.capacity),
	)
}

func (b *Budget) remainingLocked() int {
	if b.hasReported {
		if b.reported < 0 {
			return 0
		}
		return b.reported
	}
	left := b.limit - b.spent
	if left < 0 {
		return 0
	}
	return left
}

// refillLocked adds the credits earned since the last call.
func (b *Budget) refillLocked() {
	now := b.now()
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		// A non-monotonic or repeated clock reading must not mint credits.
		b.last = now
		return
	}
	b.last = now
	b.tokens += elapsed.Seconds() * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
}

// rollPeriodLocked resets the ledger when the configured period has elapsed.
//
// The period runs from process start rather than from the provider's billing
// date, which this process has no way to know. That approximation is exactly
// why [Reconcile] exists and is preferred: once the provider has reported its
// own remaining count, the local roll-over stops mattering.
func (b *Budget) rollPeriodLocked() {
	now := b.now()
	if now.Sub(b.periodStart) < b.period {
		return
	}
	elapsed := now.Sub(b.periodStart)
	periods := elapsed / b.period
	b.periodStart = b.periodStart.Add(periods * b.period)
	b.spent = 0
	b.hasReported = false
	b.reported = 0
	b.reportedLimit = 0
}
