package redis

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newTestLimiter(t *testing.T, c *Client, scope string, limit Limit) *RateLimiter {
	t.Helper()
	l, err := NewRateLimiter(RateLimiterOptions{Client: c, Scope: scope, Limit: limit})
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	return l
}

func TestLimitValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		limit Limit
		ok    bool
	}{
		{"valid", Limit{Requests: 60, Window: time.Minute}, true},
		{"valid with burst", Limit{Requests: 60, Window: time.Minute, Burst: 10}, true},
		{"zero requests", Limit{Window: time.Minute}, false},
		{"negative requests", Limit{Requests: -1, Window: time.Minute}, false},
		{"zero window", Limit{Requests: 60}, false},
		{"negative burst", Limit{Requests: 60, Window: time.Minute, Burst: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.limit.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidLimit) {
				t.Fatalf("Validate() = %v, want ErrInvalidLimit", err)
			}
		})
	}
}

// TestBucketAdmitsExactlyBurstThenRejects is the core behavioural claim: the
// bucket starts FULL, so a caller that has been idle gets Burst requests
// instantly and the next one is refused.
func TestBucketAdmitsExactlyBurstThenRejects(t *testing.T) {
	c := newTestClient(t)
	// One request per second sustained, five instantaneous. A one-second refill
	// makes the arithmetic checkable by hand.
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 1, Window: time.Second, Burst: 5})

	ctx := context.Background()
	subject := "203.0.113.7"

	for i := 1; i <= 5; i++ {
		d, err := lim.Allow(ctx, subject)
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("request %d of 5 rejected; the bucket must start full", i)
		}
		if want := int64(5 - i); d.Remaining != want {
			t.Fatalf("request %d: Remaining = %d, want %d", i, d.Remaining, want)
		}
		if d.Limit != 5 {
			t.Fatalf("request %d: Limit = %d, want 5", i, d.Limit)
		}
		if d.RetryAfter != 0 {
			t.Fatalf("request %d: RetryAfter = %s on an allowed request, want 0", i, d.RetryAfter)
		}
	}

	d, err := lim.Allow(ctx, subject)
	if err != nil {
		t.Fatalf("Allow #6: %v", err)
	}
	if d.Allowed {
		t.Fatal("request 6 was admitted; the bucket capacity is 5")
	}
	if d.Remaining != 0 {
		t.Fatalf("Remaining = %d on a rejection, want 0", d.Remaining)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Second {
		t.Fatalf("RetryAfter = %s, want (0, 1s] at 1 token per second", d.RetryAfter)
	}
	if got := d.RetryAfterSeconds(); got != 1 {
		t.Fatalf("RetryAfterSeconds() = %d, want 1 (never 0 — that tells a client to retry into the same rejection)", got)
	}
}

// TestBucketRefills proves the refill is continuous rather than a window that
// resets: after waiting for one token's worth of time, exactly one more request
// is admitted.
func TestBucketRefills(t *testing.T) {
	c := newTestClient(t)
	// 20 tokens/sec = one token every 50ms, so the wait below is short enough to
	// keep the test fast and long enough not to be flaky.
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 20, Window: time.Second, Burst: 2})

	ctx := context.Background()
	subject := "198.51.100.9"

	for i := 0; i < 2; i++ {
		if d, err := lim.Allow(ctx, subject); err != nil || !d.Allowed {
			t.Fatalf("priming request %d: allowed=%v err=%v", i, d.Allowed, err)
		}
	}
	if d, _ := lim.Allow(ctx, subject); d.Allowed {
		t.Fatal("bucket should be empty")
	}

	time.Sleep(150 * time.Millisecond) // ~3 tokens' worth, capped at the burst of 2

	d, err := lim.Allow(ctx, subject)
	if err != nil {
		t.Fatalf("Allow after refill: %v", err)
	}
	if !d.Allowed {
		t.Fatal("request after the refill interval was rejected; the bucket is not refilling")
	}
}

// TestBucketNeverExceedsBurst proves the cap: an arbitrarily long idle period
// does not accumulate more than Burst tokens, so a caller cannot bank an hour of
// unused allowance and spend it in one second.
//
// The assertion is on ONE decision after the idle period rather than on a loop
// of them, and that is deliberate: at 1000 tokens/second the bucket refills
// faster than a loop of round trips can drain it, so a loop would measure the
// network rather than the algorithm. One call is exact — 2 tokens remained, 100
// milliseconds is 100 tokens' worth of refill, and the answer must still be
// capacity minus the one just spent.
func TestBucketNeverExceedsBurst(t *testing.T) {
	c := newTestClient(t)
	const burst = 3
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 1000, Window: time.Second, Burst: burst})

	ctx := context.Background()
	subject := "192.0.2.11"

	d, err := lim.Allow(ctx, subject)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if d.Remaining != burst-1 {
		t.Fatalf("Remaining after the first request = %d, want %d", d.Remaining, burst-1)
	}

	time.Sleep(100 * time.Millisecond)

	d, err = lim.Allow(ctx, subject)
	if err != nil {
		t.Fatalf("Allow after idle: %v", err)
	}
	if d.Remaining != burst-1 {
		t.Fatalf("Remaining after a 100ms idle at 1000 tokens/s = %d, want %d — the bucket accumulated past its capacity",
			d.Remaining, burst-1)
	}
}

// TestBucketsAreIndependentPerSubject is what makes "per user AND per IP"
// meaningful: exhausting one subject must not affect another.
func TestBucketsAreIndependentPerSubject(t *testing.T) {
	c := newTestClient(t)
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 1, Window: time.Minute, Burst: 1})

	ctx := context.Background()

	if d, _ := lim.Allow(ctx, "a"); !d.Allowed {
		t.Fatal("first request for subject a rejected")
	}
	if d, _ := lim.Allow(ctx, "a"); d.Allowed {
		t.Fatal("second request for subject a admitted")
	}
	if d, _ := lim.Allow(ctx, "b"); !d.Allowed {
		t.Fatal("subject b was rejected because subject a exhausted its bucket")
	}
}

// TestScopesAreIndependent proves the per-IP and per-user limiters do not share
// a bucket even for the same subject string.
func TestScopesAreIndependent(t *testing.T) {
	c := newTestClient(t)
	ipLim := newTestLimiter(t, c, "ip", Limit{Requests: 1, Window: time.Minute, Burst: 1})
	userLim := newTestLimiter(t, c, "user", Limit{Requests: 1, Window: time.Minute, Burst: 1})

	ctx := context.Background()
	const same = "identical-subject"

	if d, _ := ipLim.Allow(ctx, same); !d.Allowed {
		t.Fatal("ip scope: first request rejected")
	}
	if d, _ := ipLim.Allow(ctx, same); d.Allowed {
		t.Fatal("ip scope: second request admitted")
	}
	if d, _ := userLim.Allow(ctx, same); !d.Allowed {
		t.Fatal("user scope shares a bucket with the ip scope")
	}
}

// TestBucketKeyExpires proves the memory bound: an idle subject's key
// disappears, so a per-IP limit across the internet does not grow without limit.
func TestBucketKeyExpires(t *testing.T) {
	c := newTestClient(t)
	// 20/sec with a burst of 1 refills fully in 50ms, so the key's TTL is 50ms.
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 20, Window: time.Second, Burst: 1})

	ctx := context.Background()
	subject := "198.51.100.55"
	if _, err := lim.Allow(ctx, subject); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	key := c.Key("rl", "ip", subject)
	ttl, err := c.Redis().PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl <= 0 || ttl > time.Second {
		t.Fatalf("PTTL = %s, want a positive TTL under a second", ttl)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, err := c.Redis().Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("EXISTS: %v", err)
		}
		if n == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the bucket key never expired; an idle subject would occupy memory for ever")
}

// TestAllowNRejectsAnImpossibleCost: a request that can never fit in the bucket
// is a configuration error, and reporting it is better than rejecting it for
// ever with a Retry-After that never comes true.
func TestAllowNRejectsAnImpossibleCost(t *testing.T) {
	c := newTestClient(t)
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 10, Window: time.Second, Burst: 5})

	if _, err := lim.AllowN(context.Background(), "x", 6); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("AllowN(cost=6, burst=5) = %v, want ErrInvalidLimit", err)
	}
	if _, err := lim.AllowN(context.Background(), "x", 0); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("AllowN(cost=0) = %v, want ErrInvalidLimit", err)
	}
}

func TestResetClearsTheBucket(t *testing.T) {
	c := newTestClient(t)
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 1, Window: time.Hour, Burst: 1})

	ctx := context.Background()
	if d, _ := lim.Allow(ctx, "z"); !d.Allowed {
		t.Fatal("first request rejected")
	}
	if d, _ := lim.Allow(ctx, "z"); d.Allowed {
		t.Fatal("second request admitted")
	}
	if err := lim.Reset(ctx, "z"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if d, _ := lim.Allow(ctx, "z"); !d.Allowed {
		t.Fatal("request after Reset rejected")
	}
}

// TestLimiterIsAtomicUnderConcurrency is the reason the decision is a Lua script
// rather than three commands from Go. CLAUDE.md §9 runs several api replicas
// behind an Ingress, so concurrent decisions against one bucket are the normal
// case; a read-modify-write would let two of them both see the pre-decrement
// value and both admit.
func TestLimiterIsAtomicUnderConcurrency(t *testing.T) {
	c := newTestClient(t, func(o *Options) { o.PoolSize = 16 })

	const capacity = 20
	const callers = 100

	lim := newTestLimiter(t, c, "ip", Limit{
		// A one-hour window means the refill during the test is negligible, so
		// the admitted count must be exactly the capacity.
		Requests: capacity, Window: time.Hour, Burst: capacity,
	})

	ctx := context.Background()
	results := make(chan bool, callers)
	start := make(chan struct{})

	for i := 0; i < callers; i++ {
		go func() {
			<-start
			d, err := lim.Allow(ctx, "contended")
			if err != nil {
				results <- false
				return
			}
			results <- d.Allowed
		}()
	}
	close(start)

	allowed := 0
	for i := 0; i < callers; i++ {
		if <-results {
			allowed++
		}
	}

	if allowed != capacity {
		t.Fatalf("%d of %d concurrent requests admitted against a capacity of %d; the decision is not atomic",
			allowed, callers, capacity)
	}
}

func TestLimiterMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := newTestClient(t, func(o *Options) { o.Registry = reg })
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 1, Window: time.Hour, Burst: 1})

	ctx := context.Background()
	if _, err := lim.Allow(ctx, "m"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if _, err := lim.Allow(ctx, "m"); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	if got := counterValue(t, reg, "sharpline_redis_rate_limit_decisions_total",
		map[string]string{"scope": "ip", "decision": "allowed"}); got != 1 {
		t.Fatalf("decisions_total{decision=allowed} = %v, want 1", got)
	}
	if got := counterValue(t, reg, "sharpline_redis_rate_limit_decisions_total",
		map[string]string{"scope": "ip", "decision": "limited"}); got != 1 {
		t.Fatalf("decisions_total{decision=limited} = %v, want 1", got)
	}
	if got := counterValue(t, reg, "sharpline_redis_rate_limit_duration_seconds_count",
		map[string]string{"scope": "ip"}); got != 2 {
		t.Fatalf("duration_seconds_count = %v, want 2", got)
	}
}

// TestAllowErrorsWhenRedisIsUnreachable proves the limiter REPORTS rather than
// DECIDES: the fail-open/fail-closed policy belongs to the caller, and a limiter
// that silently allowed on error would make that policy unimplementable.
func TestAllowErrorsWhenRedisIsUnreachable(t *testing.T) {
	c := newTestClient(t)
	lim := newTestLimiter(t, c, "ip", Limit{Requests: 1, Window: time.Second, Burst: 1})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	d, err := lim.Allow(context.Background(), "gone")
	if err == nil {
		t.Fatal("Allow returned nil error against a closed client; the caller cannot apply a policy it is not told about")
	}
	if d.Allowed {
		t.Fatal("the zero Decision reported Allowed; an undecidable request must not look like an admitted one")
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestNewRateLimiterRejectsBadOptions(t *testing.T) {
	t.Parallel()

	c := &Client{keyPrefix: "sharpline"}
	good := Limit{Requests: 1, Window: time.Second}

	cases := []struct {
		name string
		opts RateLimiterOptions
		want error
	}{
		{"nil client", RateLimiterOptions{Scope: "ip", Limit: good}, ErrInvalidOptions},
		{"empty scope", RateLimiterOptions{Client: c, Limit: good}, ErrInvalidOptions},
		{"scope with a colon", RateLimiterOptions{Client: c, Scope: "ip:v4", Limit: good}, ErrInvalidOptions},
		{"bad limit", RateLimiterOptions{Client: c, Scope: "ip"}, ErrInvalidLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRateLimiter(tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewRateLimiter = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLimitString(t *testing.T) {
	t.Parallel()
	got := Limit{Requests: 120, Window: time.Minute, Burst: 30}.String()
	want := fmt.Sprintf("120 per %s (burst 30)", time.Minute)
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
