package theoddsapi

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The provider's own quota accounting headers. Documented on every endpoint
// (guides/v4 §Response Headers; declared as integers in the OpenAPI document's
// response `headers` block).
const (
	HeaderRequestsRemaining = "x-requests-remaining"
	HeaderRequestsUsed      = "x-requests-used"
	HeaderRequestsLast      = "x-requests-last"
)

// DefaultBudgetWindow is the period the monthly credit allowance refills over.
//
// ADR 0003's arithmetic is all "credits per month" against a 30-day month, and
// the provider states the quota "resets" monthly without publishing the reset
// instant. A 30-day sliding refill is therefore an approximation of a calendar
// reset — chosen because it is the conservative one: it never grants credits
// the calendar month has not reached, whereas assuming a reset date and being
// wrong would burn a month's budget in a day. The provider's own
// x-requests-remaining header corrects the approximation on the very first
// response, which is why the approximation is affordable.
const DefaultBudgetWindow = 30 * 24 * time.Hour

// DocumentedRequestsPerSecond is the provider's published frequency limit:
// "The current rate limit is 30 requests per second on paid usage plans"
// (https://the-odds-api.com/guide/rate-limit.html, retrieved 2026-08-17).
//
// It is a CEILING, not a target. The same page warns that "even if you send
// requests below the rate limit, you might still encounter 429 errors", so the
// configured default sits well underneath it.
const DocumentedRequestsPerSecond = 30.0

// bucket is a classic token bucket: it holds up to capacity tokens and refills
// at refillPerSec.
//
// Fractional tokens are kept deliberately. The monthly credit bucket refills at
// 100000/(30·86400) = 0.0386 credits per second, so an integer bucket would
// round every refill to zero and the bucket would never fill at all.
type bucket struct {
	capacity     float64
	refillPerSec float64
	tokens       float64
	last         time.Time
}

// advance refills the bucket up to now. It is idempotent for a non-advancing
// clock and monotonic-safe: a clock that goes backwards adds nothing rather
// than removing tokens.
func (b *bucket) advance(now time.Time) {
	if b.last.IsZero() {
		b.last = now
		return
	}
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	b.last = now
	b.tokens = math.Min(b.capacity, b.tokens+elapsed.Seconds()*b.refillPerSec)
}

// waitFor returns how long until the bucket holds n tokens, and whether it can
// ever hold that many. A request larger than capacity is unsatisfiable at any
// delay, which is a configuration error rather than a wait.
func (b *bucket) waitFor(n float64) (time.Duration, bool) {
	if n > b.capacity {
		return 0, false
	}
	if b.tokens >= n {
		return 0, true
	}
	if b.refillPerSec <= 0 {
		return 0, false
	}
	deficit := n - b.tokens
	return time.Duration(deficit / b.refillPerSec * float64(time.Second)), true
}

// Quota is a point-in-time view of provider credit accounting.
type Quota struct {
	// Remaining is the credits left until the quota resets. Authoritative when
	// FromProvider is true.
	Remaining int64

	// Used is credits consumed since the last reset, from
	// x-requests-used. Zero when the provider has not reported it.
	Used int64

	// LastCost is what the provider charged for the most recent call, from
	// x-requests-last. This is the number that makes the local cost model
	// checkable rather than assumed.
	LastCost int64

	// FromProvider distinguishes the provider's own accounting from this
	// package's local estimate. ADR 0003 requires the gauge be fed from the
	// provider's number; this flag is what makes a fallback to the estimate
	// visible instead of silent.
	FromProvider bool

	// ObservedAt is when the provider's numbers were last read. Zero while
	// FromProvider is false.
	ObservedAt time.Time

	// LocalEstimate is MonthlyCredits minus everything this process has spent.
	// Reported alongside the provider's number precisely so the two can be
	// compared: they diverge when another process shares the key, when the
	// month rolls over, or when a call cost something other than predicted.
	LocalEstimate int64

	// CreditTokens is the monthly bucket's current level. It is NOT the same
	// quantity as Remaining: Remaining is how many credits the subscription has
	// left, CreditTokens is how many this limiter is presently willing to
	// spend. The pacing lives in the gap between them.
	CreditTokens float64

	// Limit is the configured monthly budget, the denominator the quota alert
	// divides by.
	Limit int64
}

// Limiter is the token-bucket limiter CLAUDE.md §5 requires, plus the quota
// reconciliation ADR 0003 requires.
//
// It holds TWO buckets because the provider imposes two unrelated limits, and
// one bucket cannot express both — see the package doc. Reserve consumes from
// both atomically or from neither, so a request refused on credits does not
// silently burn a frequency token.
//
// Nothing here sleeps. Reserve reports how long the caller would have to wait
// and lets the caller decide, because the polling scheduler owns cadence and a
// limiter that blocks inside an adapter turns a budget problem into a hung
// poller.
type Limiter struct {
	mu        sync.Mutex
	credits   bucket
	frequency bucket

	monthlyCredits int64
	localSpent     int64

	remaining     int64
	used          int64
	lastCost      int64
	fromProvider  bool
	observedAt    time.Time
	headerMissing int64

	now func() time.Time
}

// LimiterConfig configures the two buckets. Every field is a config value
// because CLAUDE.md §5 says the budget must be one — retuning for a different
// subscription tier must not require a code change.
type LimiterConfig struct {
	// MonthlyCredits is the subscription's credit allowance. ADR 0003's
	// recommended tier is 100,000.
	MonthlyCredits int64

	// CreditBurst is the monthly bucket's capacity: the most credits that may
	// be spent back to back after a quiet period. Defaults to one day's worth
	// (MonthlyCredits × 24h / BudgetWindow), which lets the poller catch up
	// after an outage without letting it spend a week's budget in an hour.
	CreditBurst int64

	// BudgetWindow is the period MonthlyCredits refills over. Defaults to
	// DefaultBudgetWindow.
	BudgetWindow time.Duration

	// RequestsPerSecond bounds request frequency, independently of cost.
	// Defaults to 5 — a sixth of the provider's documented 30/s ceiling,
	// because that page warns 429s occur near the limit.
	RequestsPerSecond float64

	// RequestBurst is the frequency bucket's capacity in requests. Defaults to
	// 10, enough for a multi-league sweep to fire together.
	RequestBurst int

	// Now is the clock. Defaults to time.Now; tests inject a fake so bucket
	// refill is deterministic rather than timing-dependent.
	Now func() time.Time
}

// NewLimiter builds a limiter from cfg, applying defaults.
//
// Both buckets start FULL. Starting the credit bucket empty would mean a
// freshly deployed ingest could not poll for hours; starting it at capacity
// bounds the initial burst at CreditBurst, which is one day's budget, and the
// provider's own remaining count clamps it downwards on the first response if
// the real subscription has less than that left.
func NewLimiter(cfg LimiterConfig) *Limiter {
	window := cfg.BudgetWindow
	if window <= 0 {
		window = DefaultBudgetWindow
	}
	monthly := cfg.MonthlyCredits
	if monthly < 0 {
		monthly = 0
	}
	burst := cfg.CreditBurst
	if burst <= 0 {
		burst = int64(math.Ceil(float64(monthly) * float64(24*time.Hour) / float64(window)))
	}
	if burst <= 0 {
		burst = 1
	}
	if burst > monthly && monthly > 0 {
		burst = monthly
	}
	rps := cfg.RequestsPerSecond
	if rps <= 0 {
		rps = defaultRequestsPerSecond
	}
	reqBurst := cfg.RequestBurst
	if reqBurst <= 0 {
		reqBurst = defaultRequestBurst
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	start := now()
	return &Limiter{
		credits: bucket{
			capacity:     float64(burst),
			refillPerSec: float64(monthly) / window.Seconds(),
			tokens:       float64(burst),
			last:         start,
		},
		frequency: bucket{
			capacity:     float64(reqBurst),
			refillPerSec: rps,
			tokens:       float64(reqBurst),
			last:         start,
		},
		monthlyCredits: monthly,
		remaining:      monthly,
		now:            now,
	}
}

// Default limiter tuning. Both are well under the provider's documented
// ceiling, per its own advice that 429s occur near the limit.
const (
	defaultRequestsPerSecond = 5.0
	defaultRequestBurst      = 10
)

// Reserve takes cost credits and one request slot, or returns a *BudgetError
// describing which bucket refused and for how long.
//
// cost is the PREDICTED cost — `markets × regions` for a sweep, 0 for the free
// catalogue endpoints. The prediction is corrected against x-requests-last on
// the response, because the provider bills some calls differently from the
// naive formula (an event-odds call is charged on markets RETURNED, and the
// docs state a call returning no events is not charged at all).
//
// A zero-cost call still takes a frequency slot. Free does not mean unlimited:
// the catalogue endpoints count against the 30/s limit like everything else.
func (l *Limiter) Reserve(cost int) error {
	if cost < 0 {
		cost = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.credits.advance(now)
	l.frequency.advance(now)

	// Check both before consuming either, so a refusal by one does not spend
	// the other's token.
	if wait, ok := l.frequency.waitFor(1); !ok || wait > 0 {
		return &BudgetError{Limiter: "frequency", Cost: cost, RetryAfter: wait}
	}
	if cost > 0 {
		if wait, ok := l.credits.waitFor(float64(cost)); !ok || wait > 0 {
			return &BudgetError{Limiter: "credits", Cost: cost, RetryAfter: wait}
		}
	}

	l.frequency.tokens--
	if cost > 0 {
		l.credits.tokens -= float64(cost)
		l.localSpent += int64(cost)
		if !l.fromProvider {
			// Only move the fallback estimate while it is what the gauge is
			// reporting. Once the provider's number is in hand it is
			// authoritative, and the local estimate is kept for comparison and
			// updated from the header instead.
			l.remaining = l.monthlyCredits - l.localSpent
			if l.remaining < 0 {
				l.remaining = 0
			}
		}
	}
	return nil
}

// Refund returns credits reserved for a request that the provider did not
// charge for.
//
// This is not hypothetical bookkeeping: guides/v4 states "If no events are
// returned, the request will not count against the usage quota". Without a
// refund path an out-of-season league would drain the month's budget polling
// an empty slate — the exact failure the adaptive-polling backoff exists to
// avoid, reintroduced by the limiter.
func (l *Limiter) Refund(credits int) {
	if credits <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.credits.tokens = math.Min(l.credits.capacity, l.credits.tokens+float64(credits))
	l.localSpent -= int64(credits)
	if l.localSpent < 0 {
		l.localSpent = 0
	}
	if !l.fromProvider {
		l.remaining = l.monthlyCredits - l.localSpent
	}
}

// HeaderObservation is what the provider's accounting headers said on one
// response, and which of them were present at all.
type HeaderObservation struct {
	Remaining     int64
	HaveRemaining bool
	Used          int64
	HaveUsed      bool
	LastCost      int64
	HaveLastCost  bool
}

// ObserveHeaders reconciles the limiter against the provider's own accounting.
//
// ADR 0003 requirement 3: "Feed the Prometheus quota gauge from
// x-requests-remaining, the provider's own number, not from a local counter.
// […] using the response header makes it authoritative and drift-proof."
//
// Reconciliation is one-directional and downwards only. If the provider says
// fewer credits remain than the bucket is willing to spend, the bucket is
// clamped — we must not spend credits the subscription does not have. If the
// provider says MORE remain, the bucket is left alone, because the bucket is
// not tracking the subscription balance, it is pacing spend across the month.
// Raising it to match would defeat the pacing entirely on the 1st of the month.
//
// The returned observation reports which headers were present, so a provider
// that stops sending them shows up in a counter rather than silently reverting
// the gauge to an estimate.
func (l *Limiter) ObserveHeaders(h http.Header) HeaderObservation {
	remaining, haveRemaining := parseQuotaHeader(h, HeaderRequestsRemaining)
	used, haveUsed := parseQuotaHeader(h, HeaderRequestsUsed)
	last, haveLast := parseQuotaHeader(h, HeaderRequestsLast)

	obs := HeaderObservation{
		Remaining:     remaining,
		HaveRemaining: haveRemaining,
		Used:          used,
		HaveUsed:      haveUsed,
		LastCost:      last,
		HaveLastCost:  haveLast,
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if haveLast {
		l.lastCost = last
	}
	if !haveRemaining {
		l.headerMissing++
		return obs
	}

	l.remaining = remaining
	if haveUsed {
		l.used = used
	}
	l.fromProvider = true
	l.observedAt = l.now()

	if float64(remaining) < l.credits.tokens {
		l.credits.tokens = math.Max(0, float64(remaining))
	}
	return obs
}

// FrequencyTokens reports the per-second bucket's current level, for the
// budget_tokens gauge.
func (l *Limiter) FrequencyTokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frequency.advance(l.now())
	return l.frequency.tokens
}

// Quota returns the current accounting view, for the metrics and for tests.
func (l *Limiter) Quota() Quota {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.credits.advance(l.now())

	estimate := l.monthlyCredits - l.localSpent
	if estimate < 0 {
		estimate = 0
	}
	return Quota{
		Remaining:     l.remaining,
		Used:          l.used,
		LastCost:      l.lastCost,
		FromProvider:  l.fromProvider,
		ObservedAt:    l.observedAt,
		LocalEstimate: estimate,
		CreditTokens:  l.credits.tokens,
		Limit:         l.monthlyCredits,
	}
}

// HeaderMissingCount reports how many responses arrived without the provider's
// quota headers. Non-zero means the gauge is running on the local estimate.
func (l *Limiter) HeaderMissingCount() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.headerMissing
}

// parseQuotaHeader reads one of the provider's integer accounting headers.
//
// The provider declares these as integers, but has been observed by others to
// send decimals for fractional plans, so a float is parsed and truncated rather
// than the whole header being discarded on a "5000.0". A header that parses as
// neither is treated as absent, which falls the gauge back to the local
// estimate — the honest outcome, since a garbled number is worse than a known
// approximation.
func parseQuotaHeader(h http.Header, name string) (int64, bool) {
	raw := h.Get(name)
	if raw == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, true
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return int64(f), true
	}
	return 0, false
}

// SweepCost is the credit cost of one /v4/sports/{sport}/odds call.
//
// guides/v4 §Usage Quota Costs: "cost = [number of markets specified] x
// [number of regions specified]". The provider also documents that a group of
// ten named bookmakers counts as one region-equivalent and takes precedence
// over `regions` when both are given — ADR 0003 requirement 1 prefers
// `bookmakers` for exactly that reason, so the region-equivalent count is
// derived from the bookmaker list when one is configured.
func SweepCost(markets, regions, bookmakers int) int {
	if markets < 1 {
		// The provider defaults `markets` to h2h when it is omitted, and bills
		// for it.
		markets = 1
	}
	equivalents := regions
	if bookmakers > 0 {
		// "Every group of 10 bookmakers counts as 1 request. […] Specifying
		// between 11 and 20 bookmakers is the equivalent of 2 regions."
		equivalents = (bookmakers + 9) / 10
	}
	if equivalents < 1 {
		equivalents = 1
	}
	return markets * equivalents
}
