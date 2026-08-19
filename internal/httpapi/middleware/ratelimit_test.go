package middleware

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
	sredis "github.com/anpl1623/sharpline/internal/platform/redis"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeLimiter stands in for *redis.RateLimiter so the middleware's POLICY can be
// tested without a broker.
//
// The algorithm itself is tested against a real Redis in
// internal/platform/redis/ratelimit_test.go — this fake deliberately does not
// reimplement it, because a fake that reimplements the thing under test proves
// only that the fake agrees with itself.
type fakeLimiter struct {
	scope    string
	decision sredis.Decision
	err      error
	calls    int
	subjects []string
}

func (f *fakeLimiter) Scope() string { return f.scope }

func (f *fakeLimiter) Allow(_ context.Context, subject string) (sredis.Decision, error) {
	f.calls++
	f.subjects = append(f.subjects, subject)
	return f.decision, f.err
}

func rateLimitHandler(t *testing.T, opts RateLimitOptions, reached *bool) http.Handler {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	mw, err := RateLimit(opts)
	if err != nil {
		t.Fatalf("RateLimit: %v", err)
	}
	return Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}), RequestID(), ClientAddr(nil), mw)
}

func TestRateLimitRejectionCarriesRetryAfterAndTheStandardHeaders(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	lim := &fakeLimiter{scope: "ip", decision: sredis.Decision{
		Allowed:    false,
		Limit:      60,
		Remaining:  0,
		RetryAfter: 1500 * 1000 * 1000, // 1.5s
		Reset:      4 * 1000 * 1000 * 1000,
	}}

	reached := false
	h := rateLimitHandler(t, RateLimitOptions{Limiter: lim, Subject: IPSubject, Metrics: m}, &reached)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if reached {
		t.Fatal("a rejected request reached the handler")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}

	// Retry-After rounds UP: 1.5s must become 2, never 1 (which would send the
	// client back into the same rejection).
	if got := w.Header().Get(headerRetryAfter); got != "2" {
		t.Fatalf("Retry-After = %q, want \"2\" (1.5s rounded up)", got)
	}
	if got := w.Header().Get(headerRateLimit); got != "60" {
		t.Fatalf("RateLimit-Limit = %q, want \"60\"", got)
	}
	if got := w.Header().Get(headerRateLimitRemaining); got != "0" {
		t.Fatalf("RateLimit-Remaining = %q, want \"0\"", got)
	}
	if got := w.Header().Get(headerRateLimitReset); got != "4" {
		t.Fatalf("RateLimit-Reset = %q, want \"4\"", got)
	}
	if got := w.Header().Get(headerRateLimitPolicy); !strings.Contains(got, "scope=ip") {
		t.Fatalf("RateLimit-Policy = %q, want it to name the binding scope", got)
	}
	if got := counterValue(t, reg, "sharpline_http_rate_limited_total",
		map[string]string{"scope": "ip", "route": "unmatched"}); got != 1 {
		t.Fatalf("rate_limited_total = %v, want 1", got)
	}
}

func TestRateLimitAdmittedRequestStillCarriesHeaders(t *testing.T) {
	t.Parallel()

	lim := &fakeLimiter{scope: "ip", decision: sredis.Decision{
		Allowed: true, Limit: 60, Remaining: 59, Reset: 1000 * 1000 * 1000,
	}}
	reached := false
	h := rateLimitHandler(t, RateLimitOptions{Limiter: lim, Subject: IPSubject}, &reached)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if !reached {
		t.Fatal("an admitted request did not reach the handler")
	}
	if got := w.Header().Get(headerRateLimitRemaining); got != "59" {
		t.Fatalf("RateLimit-Remaining = %q, want \"59\"", got)
	}
	if got := w.Header().Get(headerRetryAfter); got != "" {
		t.Fatalf("Retry-After = %q on an admitted request, want empty", got)
	}
}

// TestRateLimitFailsOpenByDefault: a Redis outage must degrade the system, not
// take it down. CLAUDE.md §3 puts Redis outside the source of truth; a component
// that can refuse every request is inside it.
func TestRateLimitFailsOpenByDefault(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	lim := &fakeLimiter{scope: "ip", err: errors.New("redis: rate limit ip: dial tcp: connection refused")}
	reached := false
	h := rateLimitHandler(t, RateLimitOptions{Limiter: lim, Subject: IPSubject, Logger: log, Metrics: m}, &reached)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if !reached {
		t.Fatal("a Redis outage became an API outage; the default must be fail-open")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The degradation must not be silent — that is the whole justification for
	// failing open.
	if got := counterValue(t, reg, "sharpline_http_rate_limit_fail_open_total",
		map[string]string{"scope": "ip"}); got != 1 {
		t.Fatalf("rate_limit_fail_open_total = %v, want 1", got)
	}
	if !strings.Contains(buf.String(), "failing open") {
		t.Fatalf("no warning was logged for a fail-open admission: %s", buf.String())
	}
	// The subject must NOT be logged: one line per request during an outage
	// would be a bulk export of who was using the system.
	if strings.Contains(buf.String(), "192.0.2.1") {
		t.Fatalf("the fail-open log line named the subject: %s", buf.String())
	}
	// No fabricated headers: reporting a limit that was never consulted would
	// have the client pace itself against fiction.
	if got := w.Header().Get(headerRateLimit); got != "" {
		t.Fatalf("RateLimit-Limit = %q on a fail-open admission, want empty", got)
	}
}

func TestRateLimitFailClosedIsOptIn(t *testing.T) {
	t.Parallel()

	lim := &fakeLimiter{scope: "login", err: errors.New("redis unreachable")}
	reached := false
	h := rateLimitHandler(t, RateLimitOptions{
		Limiter: lim, Subject: IPSubject, FailClosed: true,
	}, &reached)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil))

	if reached {
		t.Fatal("FailClosed admitted a request it could not decide on")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	// No Retry-After: there is no bucket to report and inventing a number would
	// tell the client to come back at a moment nothing was computed from.
	if got := w.Header().Get(headerRetryAfter); got != "" {
		t.Fatalf("Retry-After = %q with no decision, want empty", got)
	}
}

// TestUserSubjectExemptsAnonymous: bucketing every logged-out visitor under one
// shared key would let any one of them deny service to all of them.
func TestUserSubjectExemptsAnonymous(t *testing.T) {
	t.Parallel()

	lim := &fakeLimiter{scope: "user", decision: sredis.Decision{Allowed: true, Limit: 10, Remaining: 9}}
	reached := false
	h := rateLimitHandler(t, RateLimitOptions{Limiter: lim, Subject: UserSubject}, &reached)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if !reached {
		t.Fatal("an anonymous request was blocked by the per-user limiter")
	}
	if lim.calls != 0 {
		t.Fatalf("the per-user limiter was consulted %d times for an anonymous request, want 0", lim.calls)
	}
}

func TestUserSubjectKeysByUserID(t *testing.T) {
	t.Parallel()

	lim := &fakeLimiter{scope: "user", decision: sredis.Decision{Allowed: true, Limit: 10, Remaining: 9}}
	mw, err := RateLimit(RateLimitOptions{Limiter: lim, Subject: UserSubject, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("RateLimit: %v", err)
	}
	establish := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), Identity{UserID: domain.UserID("usr_9")})))
		})
	}
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), establish, mw)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/account", nil))

	if len(lim.subjects) != 1 || lim.subjects[0] != "usr_9" {
		t.Fatalf("subjects = %v, want [usr_9]", lim.subjects)
	}
}

// TestIPSubjectBucketsIPv6By64: per-address IPv6 limiting is defeated by
// incrementing the host part of a delegated prefix.
func TestIPSubjectBucketsIPv6By64(t *testing.T) {
	t.Parallel()

	lim := &fakeLimiter{scope: "ip", decision: sredis.Decision{Allowed: true, Limit: 10, Remaining: 9}}
	mw, err := RateLimit(RateLimitOptions{Limiter: lim, Subject: IPSubject, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("RateLimit: %v", err)
	}
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), ClientAddr(nil), mw)

	for _, host := range []string{"[2001:db8:1:2::1]:1", "[2001:db8:1:2::dead]:2", "[2001:db8:1:2:ffff::9]:3"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
		r.RemoteAddr = host
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	for i, s := range lim.subjects {
		if s != "2001:db8:1:2::/64" {
			t.Fatalf("subject %d = %q, want the /64 prefix — three addresses in one delegation must share one bucket", i, s)
		}
	}
}

func TestRateLimitExemption(t *testing.T) {
	t.Parallel()

	lim := &fakeLimiter{scope: "ip", decision: sredis.Decision{Allowed: false}}
	reached := false
	h := rateLimitHandler(t, RateLimitOptions{
		Limiter: lim, Subject: IPSubject,
		Exempt: func(r *http.Request) bool { return r.URL.Path == "/api/v1/version" },
	}, &reached)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	if !reached {
		t.Fatal("an exempt path was rate limited")
	}
	if lim.calls != 0 {
		t.Fatalf("the limiter was consulted %d times for an exempt path, want 0", lim.calls)
	}
}

func TestRateLimitRejectsBadOptions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts RateLimitOptions
	}{
		{"nil limiter", RateLimitOptions{Subject: IPSubject, Logger: discardLogger()}},
		{"nil subject", RateLimitOptions{Limiter: &fakeLimiter{}, Logger: discardLogger()}},
		{"nil logger", RateLimitOptions{Limiter: &fakeLimiter{}, Subject: IPSubject}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := RateLimit(tc.opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("RateLimit = %v, want ErrInvalidOptions", err)
			}
		})
	}
}
