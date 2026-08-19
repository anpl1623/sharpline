package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	sredis "github.com/anpl1623/sharpline/internal/platform/redis"
)

// Rate-limit response headers.
//
// # Which spelling, and why
//
// These are the draft-ietf-httpapi-ratelimit-headers names in their widely
// deployed form. Two other spellings exist and neither was chosen:
//
//	X-RateLimit-*    the de-facto original. Deprecated by convention; the
//	                 X- prefix has been discouraged for header fields since
//	                 RFC 6648.
//	RateLimit: ...   the single structured-field form from draft-08. Newer,
//	                 barely implemented, and unparseable by the HTTP clients a
//	                 reviewer is likely to point at this API.
//
// Retry-After is the one that actually matters and it is not a draft at all: it
// is RFC 9110 §10.2.3, and it is what a well-behaved client and every HTTP
// library's retry helper look at. It is set on every 429 this package writes.
const (
	headerRateLimit          = "RateLimit-Limit"
	headerRateLimitRemaining = "RateLimit-Remaining"
	headerRateLimitReset     = "RateLimit-Reset"
	headerRateLimitPolicy    = "RateLimit-Policy"
	headerRetryAfter         = "Retry-After"
)

// Limiter is the rate-limiting capability this package needs.
//
// Declared here, by the consumer (CLAUDE.md §12). *redis.RateLimiter satisfies
// it; so does an in-memory fake in a test, which is what lets the middleware be
// tested without a broker.
type Limiter interface {
	// Scope names what is being limited — "ip", "user". It is a metric label,
	// so it must come from a closed set.
	Scope() string
	// Allow consumes one token for subject. An error means no decision could be
	// made; see RateLimitOptions.FailClosed for what happens then.
	Allow(ctx context.Context, subject string) (sredis.Decision, error)
}

// SubjectFunc derives the thing being limited from a request.
//
// Returning ok == false EXEMPTS the request. Only two callers use that: the
// per-user limiter skips anonymous requests (the per-IP limiter already covers
// them), and an explicitly configured exemption skips an operator path.
type SubjectFunc func(*http.Request) (subject string, ok bool)

// IPSubject limits by client address, bucketing IPv6 by /64.
//
// The /64 is not fussiness: a residential IPv6 line is delegated a /64 or
// larger, so per-address limiting is defeated by incrementing the host part —
// an effectively unlimited supply of fresh buckets from one subscriber. See
// rateLimitSubject.
//
// It never exempts. An address that could not be determined collapses into a
// single "unknown" bucket rather than bypassing the limit.
func IPSubject(r *http.Request) (string, bool) {
	addr, _ := ClientIPFrom(r.Context())
	return rateLimitSubject(addr), true
}

// UserSubject limits by authenticated user, and exempts anonymous requests.
//
// Exempting the anonymous case is correct rather than lax: an unauthenticated
// request has already passed the per-IP limiter, and bucketing every anonymous
// caller under one shared "anonymous" key would let one of them exhaust the
// limit for all of them — a trivially cheap denial of service against every
// logged-out visitor.
func UserSubject(r *http.Request) (string, bool) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		return "", false
	}
	return id.UserID.String(), true
}

// RateLimitOptions configures one rate-limiting middleware. One is constructed
// per scope; CLAUDE.md §6 requires at least two — per IP and per user.
type RateLimitOptions struct {
	// Limiter performs the decision. Required.
	Limiter Limiter

	// Subject derives the limited identity. Required.
	Subject SubjectFunc

	// Logger receives fail-open warnings. Required.
	Logger *slog.Logger

	// Metrics records rejections and fail-open admissions. Optional.
	Metrics *Metrics

	// ErrorWriter renders the 429. Optional; WriteProblem is the default.
	ErrorWriter ErrorWriter

	// FailClosed makes an undecidable request a 429 instead of admitting it.
	//
	// FALSE — fail open — is the default, and that is a deliberate policy
	// choice rather than an oversight:
	//
	//   * Rate limiting is an AVAILABILITY control. Failing closed converts a
	//     Redis outage into a total API outage — and because every replica
	//     shares the one Redis, it takes all of them down at once. That is a
	//     strictly worse failure than briefly serving unthrottled traffic.
	//   * CLAUDE.md §3 says Redis is "never the source of truth". A component
	//     that can single-handedly refuse every request is the source of truth
	//     about whether the system works.
	//   * The degradation is not silent. Every admission counts in
	//     sharpline_http_rate_limit_fail_open_total, which exists to be alerted
	//     on, and sharpline_redis_up is already 0 by then.
	//
	// Set it true for a limiter guarding something where unlimited attempts are
	// themselves the danger — a login endpoint, a password reset — where the
	// availability argument runs the other way.
	FailClosed bool

	// Exempt, when non-nil and returning true, skips the limiter entirely.
	// Intended for a health or version path that a load balancer polls.
	Exempt func(*http.Request) bool
}

// RateLimit enforces one token bucket per request.
//
// # Headers
//
// On every decision it sets RateLimit-Limit, RateLimit-Remaining and
// RateLimit-Reset, and on a rejection also Retry-After. When two limiters apply
// to one request the INNER one wins, because it writes last — which is the right
// semantic: for an authenticated caller the per-user bucket is the one that
// governs them, and for an anonymous caller the per-IP bucket is the only one
// that ran. RateLimit-Policy names the scope so the numbers are attributable.
//
// Nothing is set on a fail-open admission. Reporting a limit that was not
// actually consulted would be a fabricated number, and a client that trusts it
// would pace itself against fiction.
func RateLimit(opts RateLimitOptions) (Middleware, error) {
	switch {
	case opts.Limiter == nil:
		return nil, invalidOption("Limiter is nil")
	case opts.Subject == nil:
		return nil, invalidOption("Subject is nil")
	case opts.Logger == nil:
		return nil, invalidOption("Logger is nil")
	}

	lim := opts.Limiter
	scope := lim.Scope()
	subjectOf := opts.Subject
	log := opts.Logger
	m := opts.Metrics
	ew := opts.ErrorWriter
	failClosed := opts.FailClosed
	exempt := opts.Exempt

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}

			subject, ok := subjectOf(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			decision, err := lim.Allow(r.Context(), subject)
			if err != nil {
				handleLimiterError(w, r, handleLimiterErrorArgs{
					err:        err,
					scope:      scope,
					failClosed: failClosed,
					log:        log,
					metrics:    m,
					errorWrite: ew,
					next:       next,
				})
				return
			}

			setRateLimitHeaders(w, scope, decision)

			if !decision.Allowed {
				if m != nil {
					m.rateLimited.WithLabelValues(scope, routeLabel(RouteFrom(r.Context()))).Inc()
				}
				w.Header().Set(headerRetryAfter, strconv.FormatInt(decision.RetryAfterSeconds(), 10))
				writeProblemWith(ew, w, r, problemRateLimited)
				return
			}

			next.ServeHTTP(w, r)
		})
	}, nil
}

type handleLimiterErrorArgs struct {
	err        error
	scope      string
	failClosed bool
	log        *slog.Logger
	metrics    *Metrics
	errorWrite ErrorWriter
	next       http.Handler
}

// handleLimiterError applies the configured policy when no decision could be
// made.
//
// The subject is deliberately absent from the log line. It is a client IP or a
// user id, and a Redis outage produces one of these lines per request — which
// would turn a dependency failure into a bulk export of who was using the system
// at the time.
func handleLimiterError(w http.ResponseWriter, r *http.Request, a handleLimiterErrorArgs) {
	log := a.log
	if l, ok := r.Context().Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		log = l
	}

	if a.failClosed {
		log.LogAttrs(r.Context(), slog.LevelError, "rate limiter unavailable, failing closed",
			slog.String("scope", a.scope),
			slog.String("route", routeLabel(RouteFrom(r.Context()))),
			slog.String("error", a.err.Error()),
		)
		if a.metrics != nil {
			a.metrics.rateLimited.WithLabelValues(a.scope, routeLabel(RouteFrom(r.Context()))).Inc()
		}
		// No Retry-After: there is no bucket to report and inventing a number
		// would tell the client to come back at a moment nothing was computed
		// from.
		writeProblemWith(a.errorWrite, w, r, problemRateLimited)
		return
	}

	log.LogAttrs(r.Context(), slog.LevelWarn, "rate limiter unavailable, failing open",
		slog.String("scope", a.scope),
		slog.String("route", routeLabel(RouteFrom(r.Context()))),
		slog.String("error", a.err.Error()),
	)
	if a.metrics != nil {
		a.metrics.failOpen.WithLabelValues(a.scope).Inc()
	}
	a.next.ServeHTTP(w, r)
}

func setRateLimitHeaders(w http.ResponseWriter, scope string, d sredis.Decision) {
	h := w.Header()
	h.Set(headerRateLimit, strconv.FormatInt(d.Limit, 10))
	h.Set(headerRateLimitRemaining, strconv.FormatInt(d.Remaining, 10))
	h.Set(headerRateLimitReset, strconv.FormatInt(d.ResetSeconds(), 10))
	h.Set(headerRateLimitPolicy, strconv.FormatInt(d.Limit, 10)+";scope="+scope)
}
