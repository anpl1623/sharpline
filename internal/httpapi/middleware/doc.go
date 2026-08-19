// Package middleware is the HTTP middleware chain the Sharpline REST API is
// wrapped in: request identity and trace correlation, structured access
// logging, Prometheus metrics, panic recovery, security headers, CORS, a
// request body cap, a per-request deadline, bearer-token authentication, and
// distributed rate limiting per IP and per user.
//
// Nothing here is global. The logger, the metrics registry, the tracer
// provider, the rate limiters and the authenticator are all constructor-injected
// (CLAUDE.md §12), and every interface this package depends on is declared here,
// by the consumer, rather than by the package that satisfies it.
//
// # The chain, in order, and why that order
//
// NewStack assembles the links in exactly this sequence. Each position is a
// decision; several of them are the difference between an observable system and
// a silently wrong one.
//
//  1. RequestID — outermost, because every line any inner layer logs must carry
//     it, including the panic recovery line, which is the one you most want to
//     correlate. It reuses Caddy's X-Request-Id when present
//     (deploy/proxy/Caddyfile sets it from {http.request.uuid} on every /api
//     hop) so one id spans the proxy log and the service log.
//
//  2. ResolveRoute — resolves the route PATTERN once, so the span name, the
//     metric labels and the log line all describe the same route. See RouteFunc
//     for why a raw path can never be used in its place.
//
//  3. ClientAddr — resolves the client address once, per the trusted-proxy
//     rules in clientip.go, so the rate limiter and the access log cannot
//     disagree about who made the request.
//
//  4. Trace — extracts W3C traceparent and starts the server span, so CLAUDE.md
//     §9's "traces spanning ingest -> pricer -> stream" continues into the API
//     rather than starting a fresh, unattached trace here.
//
//  5. Correlate — derives the request-scoped *slog.Logger carrying request_id,
//     trace_id and span_id and puts it in the context. This is what makes §9's
//     "structured JSON logging via log/slog with trace correlation" true for
//     handler code as well as for this package, and it is what lets an audit row
//     (CLAUDE.md §6, Platform) be joined to a Jaeger trace after the fact.
//
//  6. Metrics.Observe — OUTSIDE Recover, deliberately. If it were inside, a
//     panicking handler would unwind through the metrics defer before Recover
//     had written anything, and every panic would be recorded with a status of
//
//  0. Outside, Recover has already turned the panic into a 500 by the time
//     the defer runs, so sharpline_http_requests_total counts it as the 500 it
//     became.
//
//  7. AccessLog — outside Recover for the same reason: a panic must produce one
//     line reporting 500, not one reporting 0. It also installs the identity
//     cell, so a request authenticated further down the chain still logs its
//     user id.
//
//  8. Recover — inside the three observability layers so its own log line is
//     correlated, and outside everything that could panic.
//
//  9. SecurityHeaders — before CORS, so a preflight response carries them too.
//
//  10. CORS — short-circuits OPTIONS preflights. Placed before the rate limiter
//     so a preflight costs no Redis round trip: it is browser-generated,
//     cacheable via Access-Control-Max-Age, and in this deployment a dead branch
//     anyway, because the browser reaches the API through the same proxy origin
//     as the app (CLAUDE.md §7) and the allowlist is empty by default.
//
//  11. RateLimit (scope "ip") — BEFORE authentication, because the thing it
//     exists to stop is an unauthenticated flood: a limiter that only engages
//     after a token has been parsed and verified is a limiter an attacker never
//     reaches. It is also the control that bounds credential stuffing against
//     the login endpoint.
//
//  12. MaxBytes — caps the request body before any handler reads it.
//
//  13. Timeout — puts a deadline on the request context, so every downstream
//     I/O call inherits one (CLAUDE.md §12).
//
//  14. Authenticate — validates the bearer token and puts the Identity in the
//     context. Fails CLOSED: a token that is present and does not verify is a
//     401, never a pass-through to an anonymous handler, INCLUDING when the
//     failure is internal to the verifier.
//
//  15. RateLimit (scope "user") — AFTER authentication, because it needs the
//     identity. CLAUDE.md §6 requires both this and the per-IP limit, and they
//     are genuinely different controls: per IP stops an unauthenticated flood
//     from one source; per user stops one account abusing the API from a
//     thousand sources.
//
// # What this package will never log
//
// No Authorization header. No Cookie or Set-Cookie header. No request or
// response body. No query string. No TOTP secret, no password, no token of any
// kind.
//
// That is enforced structurally rather than remembered. The access logger never
// receives an *http.Request: requestFields extracts a fixed, closed set of
// scalar fields and the log call sees only that struct, so there is no code path
// through which a header could reach a log line. accesslog_test.go asserts it
// against a request carrying a real-looking bearer token and session cookie, so
// anyone who adds header logging later breaks a test rather than leaking every
// token in production.
//
// The same rule governs spans: trace.go sets a fixed attribute list and never
// iterates headers.
package middleware
