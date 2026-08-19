package middleware

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sredis "github.com/anpl1623/sharpline/internal/platform/redis"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// -----------------------------------------------------------------------------
// Chain
// -----------------------------------------------------------------------------

// TestChainRunsInListedOrder guards the reason Chain exists: hand-written
// wrapping reads inside-out, and that is how a rate limiter ends up after
// authentication by accident.
func TestChainRunsInListedOrder(t *testing.T) {
	t.Parallel()

	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mark("first"), nil, mark("second"), mark("third"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "third", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// -----------------------------------------------------------------------------
// Request id
// -----------------------------------------------------------------------------

func TestRequestIDReusesTheProxyValue(t *testing.T) {
	t.Parallel()

	const fromCaddy = "0198bd11-2b6c-7a1e-8a45-1f2c3d4e5f60"

	var seen string
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}), RequestID())

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Header.Set(headerRequestID, fromCaddy)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if seen != fromCaddy {
		t.Fatalf("context id = %q, want the proxy's %q — otherwise the proxy log and the service log cannot be joined", seen, fromCaddy)
	}
	if got := w.Header().Get(headerRequestID); got != fromCaddy {
		t.Fatalf("echoed id = %q, want %q", got, fromCaddy)
	}
}

// TestRequestIDRejectsAHostileValue: the id reaches a response header, a JSON
// log line and an error body. A value carrying CRLF is response splitting.
func TestRequestIDRejectsAHostileValue(t *testing.T) {
	t.Parallel()

	hostile := []string{
		"abc\r\nX-Injected: yes",
		"abc\ndef",
		`"quoted"`,
		strings.Repeat("a", maxRequestIDLen+1),
		"id with spaces",
	}

	for _, v := range hostile {
		var seen string
		h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = RequestIDFrom(r.Context())
		}), RequestID())

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header[headerRequestID] = []string{v}
		h.ServeHTTP(httptest.NewRecorder(), r)

		if seen == v {
			t.Fatalf("a hostile request id survived sanitisation: %q", v)
		}
		if len(seen) != 32 {
			t.Fatalf("a rejected id was not replaced by a generated one: got %q", seen)
		}
	}
}

func TestRequestIDGeneratesDistinctValues(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newRequestID()
		if seen[id] {
			t.Fatalf("newRequestID repeated %q", id)
		}
		seen[id] = true
	}
}

// -----------------------------------------------------------------------------
// Correlation
// -----------------------------------------------------------------------------

func TestCorrelateAttachesRequestID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		LoggerFrom(r.Context()).Info("handler ran")
	}), RequestID(), Correlate(base))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(headerRequestID, "abc123")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if !strings.Contains(buf.String(), `"request_id":"abc123"`) {
		t.Fatalf("handler log line is not correlated: %s", buf.String())
	}
	// A no-op tracer produces an all-zero span context; attaching it would put
	// a field in every line that resolves to nothing in Jaeger.
	if strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("an invalid span context was logged as a trace id: %s", buf.String())
	}
}

// -----------------------------------------------------------------------------
// Panic recovery
// -----------------------------------------------------------------------------

func TestRecoverDoesNotLeakAStackTrace(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("database credentials are /run/secrets/pg_password")
	}), RequestID(), Recover(log, m, nil))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	body := w.Body.String()
	for _, leak := range []string{"goroutine", "middleware_test.go", "runtime.", "/src/", "pg_password"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the 500 body leaked internals (%q): %s", leak, body)
		}
	}
	if !strings.Contains(body, CodeInternal) {
		t.Fatalf("the 500 body is missing its error code: %s", body)
	}

	// The operator, by contrast, must get everything.
	logged := buf.String()
	if !strings.Contains(logged, "goroutine") {
		t.Fatalf("the stack was not logged: %s", logged)
	}
	if !strings.Contains(logged, "pg_password") {
		t.Fatalf("the panic value was not logged: %s", logged)
	}
	if got := counterValue(t, reg, "sharpline_http_panics_total", map[string]string{"route": "unmatched"}); got != 1 {
		t.Fatalf("panics_total = %v, want 1", got)
	}
}

func TestRecoverRepanicsErrAbortHandler(t *testing.T) {
	t.Parallel()

	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), Recover(discardLogger(), nil, nil))

	defer func() {
		v := recover()
		if v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler to be re-panicked", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Fatal("ErrAbortHandler was swallowed")
}

func TestRecoverDoesNotWriteTwice(t *testing.T) {
	t.Parallel()

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":`))
		panic("mid-response")
	}), Recover(discardLogger(), nil, nil))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; the 200 was already on the wire and must not be overwritten", w.Code)
	}
	if strings.Contains(w.Body.String(), CodeInternal) {
		t.Fatalf("an error envelope was appended to a partial body: %s", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// Metrics and the route label
// -----------------------------------------------------------------------------

// TestMetricsRouteLabelIsThePatternNotThePath is the cardinality control: an id
// in the path must never become a time series.
func TestMetricsRouteLabelIsThePatternNotThePath(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/events/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	h := Chain(mux, ResolveRoute(MuxRouteFunc(mux)), m.Observe())

	for _, id := range []string{"01HZA", "01HZB", "01HZC"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events/"+id, nil))
	}
	// One request that matches nothing.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))

	routes := labelValues(t, reg, "sharpline_http_requests_total", "route")
	if len(routes) != 2 {
		t.Fatalf("route label took %d distinct values (%v); three ids must collapse onto one pattern", len(routes), routes)
	}
	if !routes["GET /api/v1/events/{id}"] {
		t.Fatalf("the registered pattern is missing from the label set: %v", routes)
	}
	if !routes["unmatched"] {
		t.Fatalf("an unmatched request must be countable: %v", routes)
	}
	if got := counterValue(t, reg, "sharpline_http_requests_total", map[string]string{
		"route": "GET /api/v1/events/{id}", "status": "200", "method": "GET",
	}); got != 3 {
		t.Fatalf("requests_total for the pattern = %v, want 3", got)
	}
}

// TestMetricsCountsAPanicAsA500 is the ordering claim from the package doc: with
// Observe outside Recover, a panicking request is recorded as the 500 it became
// rather than as a status of 0.
func TestMetricsCountsAPanicAsA500(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), RequestID(), m.Observe(), Recover(discardLogger(), m, nil))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if got := counterValue(t, reg, "sharpline_http_requests_total", map[string]string{"status": "500"}); got != 1 {
		t.Fatalf("requests_total{status=500} = %v, want 1 — Observe must sit OUTSIDE Recover", got)
	}
	if got := counterValue(t, reg, "sharpline_http_requests_total", map[string]string{"status": "0"}); got != 0 {
		t.Fatalf("a request was recorded with status 0: %v", got)
	}
}

func TestMetricsRecordsInFlightAndSize(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 128))
	}), m.Observe())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if got := gaugeValue(t, reg, "sharpline_http_requests_in_flight"); got != 0 {
		t.Fatalf("requests_in_flight = %v after the request completed, want 0", got)
	}
	if got := counterValue(t, reg, "sharpline_http_response_size_bytes_count", map[string]string{"route": "unmatched"}); got != 1 {
		t.Fatalf("response_size_bytes count = %v, want 1", got)
	}
}

func TestNewMetricsRejectsADuplicateRegistration(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	if _, err := NewMetrics(reg); err != nil {
		t.Fatalf("first NewMetrics: %v", err)
	}
	if _, err := NewMetrics(reg); err == nil {
		t.Fatal("a second registration on the same registry succeeded; two chains would report under one series")
	}
}

// -----------------------------------------------------------------------------
// Security headers, CORS, body cap, timeout
// -----------------------------------------------------------------------------

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), SecurityHeaders())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Fatalf("%s = %q, want %q", k, got, v)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP = %q, want default-src 'none' for a JSON API", csp)
	}
	// TLS terminates at the proxy; this service is in no position to assert a
	// scheme.
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS = %q; it belongs at the proxy, not here", got)
	}
}

func TestSecurityHeadersAreOverridableByAHandler(t *testing.T) {
	t.Parallel()

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=30")
		w.WriteHeader(http.StatusOK)
	}), SecurityHeaders())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/leagues", nil))

	if got := w.Header().Get("Cache-Control"); got != "public, max-age=30" {
		t.Fatalf("Cache-Control = %q; a handler must be able to opt genuinely public data into caching", got)
	}
}

func TestCORSDefaultDeniesEverything(t *testing.T) {
	t.Parallel()

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), CORS(CORSOptions{}))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q with an empty allowlist, want empty", got)
	}
	if !strings.Contains(w.Header().Get("Vary"), "Origin") {
		t.Fatal("Vary: Origin must be set even on a rejection, or a shared cache can replay across origins")
	}
}

func TestCORSAllowlist(t *testing.T) {
	t.Parallel()

	opts := CORSOptions{AllowedOrigins: []string{"https://app.sharpline.dev"}, AllowCredentials: true, MaxAgeSeconds: 600}
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), CORS(opts))

	t.Run("allowed origin", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
		r.Header.Set("Origin", "https://app.sharpline.dev")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.sharpline.dev" {
			t.Fatalf("ACAO = %q", got)
		}
		if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Fatalf("ACAC = %q", got)
		}
		if got := w.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, headerRetryAfter) {
			t.Fatalf("Expose-Headers = %q; a client that cannot read Retry-After cannot honour it", got)
		}
	})

	t.Run("a near-miss origin is not allowed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
		r.Header.Set("Origin", "https://app.sharpline.dev.evil.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("a suffix-matching origin was allowed: %q", got)
		}
	})

	t.Run("preflight", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodOptions, "/api/v1/wagers", nil)
		r.Header.Set("Origin", "https://app.sharpline.dev")
		r.Header.Set("Access-Control-Request-Method", http.MethodPost)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != http.StatusNoContent {
			t.Fatalf("preflight status = %d, want 204", w.Code)
		}
		if got := w.Header().Get("Access-Control-Max-Age"); got != "600" {
			t.Fatalf("Max-Age = %q", got)
		}
		if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
			t.Fatalf("Allow-Methods = %q", got)
		}
	})
}

func TestMaxBytesRejectsAnOversizedDeclaredBody(t *testing.T) {
	t.Parallel()

	reached := false
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
	}), MaxBytes(1024, nil))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/wagers", strings.NewReader(strings.Repeat("x", 2048)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if reached {
		t.Fatal("an oversized body reached the handler")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

func TestMaxBytesCapsAnUndeclaredBody(t *testing.T) {
	t.Parallel()

	var readErr error
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		for {
			_, err := r.Body.Read(buf)
			if err != nil {
				readErr = err
				return
			}
		}
	}), MaxBytes(64, nil))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/wagers", strings.NewReader(strings.Repeat("x", 4096)))
	r.ContentLength = -1 // chunked: no declared length
	h.ServeHTTP(httptest.NewRecorder(), r)

	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Fatalf("read error = %v, want *http.MaxBytesError — an undeclared body must still be capped", readErr)
	}
}

func TestTimeoutSetsADeadline(t *testing.T) {
	t.Parallel()

	var deadline time.Time
	var ok bool
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok = r.Context().Deadline()
	}), Timeout(250*time.Millisecond, nil))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if !ok {
		t.Fatal("no deadline on the request context; every downstream I/O call would be unbounded")
	}
	if d := time.Until(deadline); d <= 0 || d > 250*time.Millisecond {
		t.Fatalf("deadline is %s away, want (0, 250ms]", d)
	}
}

func TestTimeoutIsDisabledByANonPositiveValue(t *testing.T) {
	t.Parallel()

	var ok bool
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok = r.Context().Deadline()
	}), Timeout(0, nil))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ws", nil))
	if ok {
		t.Fatal("Timeout(0) still set a deadline")
	}
}

func TestExpiredDeadlineIsCounted(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusGatewayTimeout)
	}), m.Observe(), Timeout(20*time.Millisecond, m))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if got := counterValue(t, reg, "sharpline_http_request_timeouts_total", map[string]string{"route": "unmatched"}); got != 1 {
		t.Fatalf("request_timeouts_total = %v, want 1", got)
	}
}

// -----------------------------------------------------------------------------
// Response recorder
// -----------------------------------------------------------------------------

func TestRecorderPreservesOptionalInterfaces(t *testing.T) {
	t.Parallel()

	rec := newRecorder(httptest.NewRecorder())

	if _, ok := any(rec).(http.Flusher); !ok {
		t.Fatal("recorder is not an http.Flusher")
	}
	if _, ok := any(rec).(http.Hijacker); !ok {
		t.Fatal("recorder is not an http.Hijacker")
	}
	if _, ok := any(rec).(interface{ Unwrap() http.ResponseWriter }); !ok {
		t.Fatal("recorder does not Unwrap; http.ResponseController cannot reach the real writer")
	}
}

func TestRecorderIsNotNested(t *testing.T) {
	t.Parallel()

	first := newRecorder(httptest.NewRecorder())
	if got := ensureRecorder(first); got != first {
		t.Fatal("ensureRecorder wrapped an existing recorder; the byte counts would describe the same bytes twice")
	}
}

func TestRecorderIgnoresASecondWriteHeader(t *testing.T) {
	t.Parallel()

	rec := newRecorder(httptest.NewRecorder())
	rec.WriteHeader(http.StatusTeapot)
	rec.WriteHeader(http.StatusOK)
	if rec.status != http.StatusTeapot {
		t.Fatalf("status = %d, want the first one written (%d)", rec.status, http.StatusTeapot)
	}
}

func TestRecorderDefaultsToOK(t *testing.T) {
	t.Parallel()

	rec := newRecorder(httptest.NewRecorder())
	if _, err := rec.Write([]byte("body")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if rec.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an implicit write", rec.status)
	}
	if rec.written != 4 {
		t.Fatalf("written = %d, want 4", rec.written)
	}
}

// -----------------------------------------------------------------------------
// Stack
// -----------------------------------------------------------------------------

func TestNewStackValidatesOptions(t *testing.T) {
	t.Parallel()

	if _, err := NewStack(StackOptions{Logger: discardLogger()}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("NewStack without Service = %v, want ErrInvalidOptions", err)
	}
	if _, err := NewStack(StackOptions{Service: "api"}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("NewStack without Logger = %v, want ErrInvalidOptions", err)
	}
}

// TestStackEndToEnd exercises the assembled chain: an authenticated request
// through a rate limiter behind a trusted proxy, with metrics, logs and headers
// all describing the same request.
func TestStackEndToEnd(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	reg := prometheus.NewRegistry()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/account", func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r.Context())
		if !ok {
			t.Error("no identity in the handler")
		}
		// Money on the wire is minor units as an INTEGER, never a float.
		_, _ = w.Write([]byte(`{"user_id":"` + id.UserID.String() + `","balance_minor":125000}`))
	})

	ipLim := &fakeLimiter{scope: "ip", decision: sredisDecision(true, 120, 119)}
	userLim := &fakeLimiter{scope: "user", decision: sredisDecision(true, 60, 59)}

	stack, err := NewStack(StackOptions{
		Service:        "api",
		Logger:         log,
		Registry:       reg,
		RouteFunc:      MuxRouteFunc(mux),
		TrustedProxies: mustTrusted(t, "172.18.0.0/16"),
		IPLimiter:      ipLim,
		UserLimiter:    userLim,
		Authenticator:  okAuthenticator(secretToken, testIdentity()),
	})
	if err != nil {
		t.Fatalf("NewStack: %v", err)
	}

	h := stack.Then(mux)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
	r.RemoteAddr = "172.18.0.5:9999"
	r.Header.Set(headerRealIP, "203.0.113.44")
	r.Header.Set("Authorization", "Bearer "+secretToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	// The per-IP limiter saw the forwarded address, not the proxy's.
	if len(ipLim.subjects) != 1 || ipLim.subjects[0] != "203.0.113.44" {
		t.Fatalf("ip limiter subjects = %v, want [203.0.113.44]", ipLim.subjects)
	}
	// The per-user limiter ran AFTER authentication and saw the identity.
	if len(userLim.subjects) != 1 || userLim.subjects[0] != "usr_01HZ" {
		t.Fatalf("user limiter subjects = %v, want [usr_01HZ]", userLim.subjects)
	}
	// The inner (user) limiter's numbers are the ones on the wire.
	if got := w.Header().Get(headerRateLimit); got != "60" {
		t.Fatalf("RateLimit-Limit = %q, want the binding (per-user) bucket's 60", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("security headers missing: %v", w.Header())
	}
	if w.Header().Get(headerRequestID) == "" {
		t.Fatal("no request id echoed")
	}

	if got := counterValue(t, reg, "sharpline_http_requests_total", map[string]string{
		"route": "GET /api/v1/account", "status": "200",
	}); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}

	logged := buf.String()
	if strings.Contains(logged, secretToken) {
		t.Fatalf("the assembled chain logged the token: %s", logged)
	}
	if !strings.Contains(logged, `"user_id":"usr_01HZ"`) {
		t.Fatalf("the access log line is missing the identity: %s", logged)
	}
	if !strings.Contains(logged, `"route":"GET /api/v1/account"`) {
		t.Fatalf("the access log line is missing the route: %s", logged)
	}
}

func TestStackWithoutAnAuthenticatorIsAnonymousAndRequireIdentityRejects(t *testing.T) {
	t.Parallel()

	stack, err := NewStack(StackOptions{Service: "api", Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewStack: %v", err)
	}

	h := stack.Then(Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a protected handler ran with no authenticator wired")
	}), stack.RequireIdentity()))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
	r.Header.Set("Authorization", "Bearer "+secretToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a partially wired service must fail closed", w.Code)
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// sredisDecision builds an allowed decision for the fake limiters, so the
// end-to-end test does not have to spell out four fields it does not assert on.
func sredisDecision(allowed bool, limit, remaining int64) sredis.Decision {
	return sredis.Decision{
		Allowed:   allowed,
		Limit:     limit,
		Remaining: remaining,
		Reset:     time.Second,
	}
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	if m == nil {
		return 0
	}
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	if h := m.GetHistogram(); h != nil {
		return float64(h.GetSampleCount())
	}
	return 0
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, nil)
	if m == nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func findMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	base := strings.TrimSuffix(name, "_count")
	for _, mf := range families {
		if mf.GetName() != name && mf.GetName() != base {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m, labels) {
				return m
			}
		}
	}
	return nil
}

func labelValues(t *testing.T, reg *prometheus.Registry, metric, label string) map[string]bool {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]bool{}
	for _, mf := range families {
		if mf.GetName() != metric {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label {
					out[lp.GetValue()] = true
				}
			}
		}
	}
	return out
}

func matchLabels(m *dto.Metric, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, lp := range m.GetLabel() {
			if lp.GetName() == k && lp.GetValue() == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
