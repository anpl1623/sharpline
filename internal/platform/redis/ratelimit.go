package redis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// ErrInvalidLimit means a Limit could not describe a usable bucket.
var ErrInvalidLimit = errors.New("redis: invalid rate limit")

// DefaultRateLimitTimeout bounds ONE limiter decision.
//
// It is short because of where this runs: the limiter is consulted on the hot
// path of every HTTP request, before the handler does any work. A limiter that
// blocks for a second has already cost the caller more than the request it was
// protecting, and — since the caller's policy on a limiter error is to fail open
// (see internal/httpapi/middleware) — waiting longer does not even buy a
// different answer. 100ms is roughly two orders of magnitude above the p99 of an
// O(1) script against an in-memory server on the same network.
const DefaultRateLimitTimeout = 100 * time.Millisecond

// Limit describes one token bucket.
//
// The algorithm is a TOKEN BUCKET, stated precisely because "rate limited" is
// meaningless without the shape:
//
//   - the bucket holds at most Burst tokens and starts full;
//   - it refills continuously at Requests/Window tokens per second;
//   - each request costs one token;
//   - a request is allowed if and only if the bucket holds at least one token
//     after the refill for the elapsed time is applied, and it then consumes
//     one.
//
// Consequences worth being explicit about, because they are the behaviour an
// operator will observe:
//
//   - A caller that has been idle can make Burst requests instantly. That is
//     the point of a burst allowance: a browser opening the odds board issues a
//     handful of requests at once and must not be punished for it.
//   - Sustained throughput converges on exactly Requests per Window regardless
//     of the arrival pattern. There is no window boundary to game — the classic
//     fixed-window flaw where 2×Requests land in the two milliseconds either
//     side of a rollover does not exist here.
//   - Memory is one small hash per active subject, expired automatically once
//     the bucket would be full again (an idle subject is indistinguishable from
//     a new one, so the row has no value). A sliding-window LOG would give the
//     same precision at O(Requests) memory per subject, which for a per-IP limit
//     across an internet's worth of source addresses is not a trade worth making.
type Limit struct {
	// Requests is the sustained number of requests allowed per Window.
	Requests int64
	// Window is the period Requests is measured over.
	Window time.Duration
	// Burst is the bucket capacity — the largest instantaneous burst. Zero
	// means Requests, i.e. a full window's worth.
	Burst int64
}

// Validate reports whether the limit describes a usable bucket. Configuration
// is validated at startup and fails loudly (CLAUDE.md §12), so a limiter is
// never constructed around a limit that would silently allow everything.
func (l Limit) Validate() error {
	switch {
	case l.Requests <= 0:
		return fmt.Errorf("%w: Requests is %d, want > 0", ErrInvalidLimit, l.Requests)
	case l.Window <= 0:
		return fmt.Errorf("%w: Window is %s, want > 0", ErrInvalidLimit, l.Window)
	case l.Burst < 0:
		return fmt.Errorf("%w: Burst is %d, want >= 0", ErrInvalidLimit, l.Burst)
	case l.ratePerSecond() <= 0:
		return fmt.Errorf("%w: %d requests per %s rounds to a zero refill rate", ErrInvalidLimit, l.Requests, l.Window)
	}
	return nil
}

// ratePerSecond is the continuous refill rate.
func (l Limit) ratePerSecond() float64 {
	if l.Window <= 0 {
		return 0
	}
	return float64(l.Requests) / l.Window.Seconds()
}

// capacity is the bucket size, defaulting to a full window's worth.
func (l Limit) capacity() int64 {
	if l.Burst > 0 {
		return l.Burst
	}
	return l.Requests
}

// String renders the limit the way the RateLimit-Policy header and a log line
// want it.
func (l Limit) String() string {
	return strconv.FormatInt(l.Requests, 10) + " per " + l.Window.String() +
		" (burst " + strconv.FormatInt(l.capacity(), 10) + ")"
}

// Decision is the outcome of one limiter consultation.
//
// Every field is what an HTTP response needs, computed server-side so the
// caller does no arithmetic of its own — two callers doing the same arithmetic
// slightly differently is how a Retry-After ends up disagreeing with the bucket
// that produced it.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool

	// Limit is the bucket capacity, for the RateLimit-Limit header.
	Limit int64

	// Remaining is the whole tokens left after this decision, for
	// RateLimit-Remaining. Floored, so it never promises a request that would
	// be rejected.
	Remaining int64

	// RetryAfter is how long until one token is available. Zero when Allowed.
	// It is the Retry-After header, rounded UP to a whole second by
	// RetryAfterSeconds because Retry-After has one-second resolution and
	// rounding down would invite an immediate second rejection.
	RetryAfter time.Duration

	// Reset is how long until the bucket is full again, for RateLimit-Reset.
	Reset time.Duration
}

// RetryAfterSeconds renders RetryAfter for the Retry-After header: whole
// seconds, always at least 1 when the request was rejected.
//
// Retry-After: 0 is legal and useless — it tells a client to retry immediately
// into the same rejection. The floor of 1 is what makes a well-behaved client
// actually back off.
func (d Decision) RetryAfterSeconds() int64 {
	if d.Allowed {
		return 0
	}
	s := int64(math.Ceil(d.RetryAfter.Seconds()))
	if s < 1 {
		return 1
	}
	return s
}

// ResetSeconds renders Reset for the RateLimit-Reset header: whole seconds,
// rounded up.
func (d Decision) ResetSeconds() int64 {
	s := int64(math.Ceil(d.Reset.Seconds()))
	if s < 0 {
		return 0
	}
	return s
}

// tokenBucketScript is the whole algorithm, executed atomically on the server.
//
// # Why a script and not GETSET/INCR/EXPIRE from Go
//
// Read-modify-write across three commands is a race between replicas: two api
// pods reading the same bucket concurrently both see the pre-decrement value and
// both allow. CLAUDE.md §9 runs several api pods behind an Ingress, so that race
// is the normal case, not the exotic one. A script is one atomic step and one
// round trip.
//
// # Why the server's clock and not the caller's
//
// redis.call('TIME') is read INSIDE the script, so every replica measures
// elapsed time against the same clock. Passing the caller's time.Now() would
// make the bucket's refill rate a function of the worst clock skew in the
// deployment — and a pod whose clock runs fast would hand itself free tokens.
// Redis has replicated command EFFECTS rather than the script itself since 5.0,
// so a non-deterministic script is fully supported on the pinned Redis 7 image.
//
// # Storage
//
// One hash per subject: field 'n' is the token count (fractional, formatted to
// six decimal places so a sub-token refill is not lost to rounding on every
// call), field 't' is the millisecond timestamp of the last refill. PEXPIRE is
// reset on every call to the time the bucket would be full again, because a full
// bucket is indistinguishable from a bucket that never existed — so keeping it
// costs memory and buys nothing.
//
// Returns {allowed, remaining, retry_after_ms, reset_ms}. Lua->RESP conversion
// truncates numbers to integers, so every returned value is integral by
// construction.
var tokenBucketScript = goredis.NewScript(`
local key      = KEYS[1]
local capacity = tonumber(ARGV[1])
local rate     = tonumber(ARGV[2])
local cost     = tonumber(ARGV[3])

local t   = redis.call('TIME')
local now = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)

local stored = redis.call('HMGET', key, 'n', 't')
local tokens = tonumber(stored[1])
local last   = tonumber(stored[2])

if tokens == nil or last == nil then
  tokens = capacity
  last   = now
end

local elapsed = now - last
if elapsed < 0 then elapsed = 0 end

tokens = tokens + ((elapsed / 1000.0) * rate)
if tokens > capacity then tokens = capacity end

local allowed = 0
if tokens >= cost then
  allowed = 1
  tokens  = tokens - cost
end

local retry_ms = 0
if allowed == 0 then
  retry_ms = math.ceil(((cost - tokens) / rate) * 1000)
end

local reset_ms = math.ceil(((capacity - tokens) / rate) * 1000)
if reset_ms < 1 then reset_ms = 1 end

redis.call('HSET', key, 'n', string.format('%.6f', tokens), 't', now)
redis.call('PEXPIRE', key, reset_ms)

return {allowed, math.floor(tokens), retry_ms, reset_ms}
`)

// RateLimiter is a distributed token bucket over Redis.
//
// CLAUDE.md §6 requires rate limiting "per user and per IP", and CLAUDE.md §9
// runs several api replicas behind an Ingress, so the counter cannot live in a
// process. One limiter is constructed per scope; they share the Client and
// therefore the connection pool and the metric collectors.
//
// # Behaviour when Redis is unreachable
//
// Allow returns an error. It does NOT decide, because the right answer is a
// policy question and the policy belongs to the caller, not to a platform
// package: `stream` might legitimately refuse a connection it cannot account
// for, while the api serves the request unthrottled. internal/httpapi/middleware
// makes that choice explicitly, counts it, and logs it. Nothing here fails
// silently in either direction.
type RateLimiter struct {
	client  *Client
	scope   string
	limit   Limit
	timeout time.Duration
}

// RateLimiterOptions configures a RateLimiter.
type RateLimiterOptions struct {
	// Client is the Redis connection. Required.
	Client *Client

	// Scope names what is being limited — "ip", "user", "login". It is the
	// first key segment AND the `scope` metric label, so it must come from a
	// closed set the caller controls, never from request data.
	Scope string

	// Limit is the bucket shape. Required and validated.
	Limit Limit

	// Timeout bounds one decision. Zero means DefaultRateLimitTimeout.
	Timeout time.Duration
}

// NewRateLimiter builds a limiter. It performs no I/O.
func NewRateLimiter(opts RateLimiterOptions) (*RateLimiter, error) {
	switch {
	case opts.Client == nil:
		return nil, fmt.Errorf("%w: Client is nil", ErrInvalidOptions)
	case opts.Scope == "":
		return nil, fmt.Errorf("%w: Scope is empty", ErrInvalidOptions)
	case opts.Scope != sanitiseKeyPart(opts.Scope):
		// A scope becomes a metric label as well as a key segment. Rejecting a
		// scope that would have to be sanitised keeps the label and the key
		// segment identical, so a graph and a redis-cli session agree.
		return nil, fmt.Errorf("%w: Scope %q must match [A-Za-z0-9._-]{1,96}", ErrInvalidOptions, opts.Scope)
	}
	if err := opts.Limit.Validate(); err != nil {
		return nil, err
	}

	return &RateLimiter{
		client:  opts.Client,
		scope:   opts.Scope,
		limit:   opts.Limit,
		timeout: positiveOr(opts.Timeout, DefaultRateLimitTimeout),
	}, nil
}

// Scope reports what this limiter limits.
func (l *RateLimiter) Scope() string { return l.scope }

// Limit reports the configured bucket shape.
func (l *RateLimiter) Limit() Limit { return l.limit }

// Allow consumes one token for subject and reports the decision.
//
// subject is the thing being limited — a client IP, a user id. It is sanitised
// into the key (see Client.Key), so a subject carrying a colon cannot address
// another scope's bucket.
//
// It carries its own timeout on top of ctx (CLAUDE.md §12: "every external call
// has a timeout"), so a caller that forgets one still cannot block a request on
// an unreachable Redis. Whichever deadline is nearer wins.
//
// An error means the decision could not be made — Redis was unreachable, the
// script failed, the deadline expired. The returned Decision is the zero value
// and must not be treated as a rejection; see the type comment for whose policy
// that is.
func (l *RateLimiter) Allow(ctx context.Context, subject string) (Decision, error) {
	return l.AllowN(ctx, subject, 1)
}

// AllowN consumes n tokens for subject.
//
// It exists for the request whose cost is not one — a bulk endpoint, a
// WebSocket subscription that fans out to many markets. n must be positive and
// must not exceed the bucket capacity: a request that can never fit would
// otherwise be rejected for ever with a Retry-After that never comes true, which
// is a configuration error and is reported as one rather than served.
func (l *RateLimiter) AllowN(ctx context.Context, subject string, n int64) (Decision, error) {
	if n <= 0 {
		return Decision{}, fmt.Errorf("%w: cost is %d, want > 0", ErrInvalidLimit, n)
	}
	capacity := l.limit.capacity()
	if n > capacity {
		return Decision{}, fmt.Errorf("%w: cost %d exceeds burst capacity %d, so it can never be admitted",
			ErrInvalidLimit, n, capacity)
	}
	if l.client.closed.isSet() {
		return Decision{}, ErrClosed
	}

	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	key := l.client.Key("rl", l.scope, subject)

	start := time.Now()
	res, err := tokenBucketScript.Run(ctx, l.client.rdb,
		[]string{key},
		capacity,
		l.limit.ratePerSecond(),
		n,
	).Result()
	l.client.metrics.rateLimitDuration.WithLabelValues(l.scope).Observe(time.Since(start).Seconds())

	if err != nil {
		l.client.metrics.rateLimitDecisions.WithLabelValues(l.scope, "error").Inc()
		return Decision{}, fmt.Errorf("redis: rate limit %s: %w", l.scope, err)
	}

	d, err := decodeDecision(res, capacity)
	if err != nil {
		l.client.metrics.rateLimitDecisions.WithLabelValues(l.scope, "error").Inc()
		return Decision{}, fmt.Errorf("redis: rate limit %s: %w", l.scope, err)
	}

	decision := "allowed"
	if !d.Allowed {
		decision = "limited"
	}
	l.client.metrics.rateLimitDecisions.WithLabelValues(l.scope, decision).Inc()
	return d, nil
}

// Reset discards a subject's bucket, so its next request starts from a full
// one.
//
// It exists for the operator path and for tests. It is deliberately NOT called
// on a successful login or any other application event: a limiter an
// unauthenticated caller can clear by succeeding at something is not a limiter.
func (l *RateLimiter) Reset(ctx context.Context, subject string) error {
	if l.client.closed.isSet() {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	if err := l.client.rdb.Del(ctx, l.client.Key("rl", l.scope, subject)).Err(); err != nil {
		return fmt.Errorf("redis: reset rate limit %s: %w", l.scope, err)
	}
	return nil
}

// decodeDecision converts the script's four-element reply.
//
// The reply shape is a contract between this function and the Lua above; a
// mismatch is a programming error and is reported rather than guessed at,
// because guessing would mean silently allowing every request.
func decodeDecision(v any, capacity int64) (Decision, error) {
	vals, ok := v.([]any)
	if !ok || len(vals) != 4 {
		return Decision{}, fmt.Errorf("token bucket returned %T with %d elements, want 4", v, lengthOf(v))
	}

	nums := make([]int64, 4)
	for i, raw := range vals {
		n, ok := raw.(int64)
		if !ok {
			return Decision{}, fmt.Errorf("token bucket element %d is %T, want int64", i, raw)
		}
		nums[i] = n
	}

	remaining := nums[1]
	if remaining < 0 {
		remaining = 0
	}
	return Decision{
		Allowed:    nums[0] == 1,
		Limit:      capacity,
		Remaining:  remaining,
		RetryAfter: time.Duration(nums[2]) * time.Millisecond,
		Reset:      time.Duration(nums[3]) * time.Millisecond,
	}, nil
}

func lengthOf(v any) int {
	if s, ok := v.([]any); ok {
		return len(s)
	}
	return 0
}
