package middleware

import (
	"net/http"
	"strconv"
	"strings"
)

// DefaultMaxRequestBytes caps a request body at 1 MiB, matching the
// `request_body { max_size 1MB }` deploy/proxy/Caddyfile already enforces on
// /api.
//
// Matching rather than exceeding it is the point: two limits that disagree mean
// one of them is dead code, and the dead one is the one nobody notices has
// stopped working. Duplicating it here is defence in depth for the paths that do
// not go through Caddy — a direct pod-to-pod call in Kubernetes, or a future
// ingress that forgets the rule.
const DefaultMaxRequestBytes int64 = 1 << 20

// SecurityHeaders sets the response headers appropriate to a JSON API.
//
// # This duplicates deploy/proxy/Caddyfile, on purpose
//
// Caddy sets X-Content-Type-Options, X-Frame-Options, Referrer-Policy, COOP,
// CORP and Permissions-Policy on every response it proxies, and its `header`
// directive REPLACES rather than appends, so there is no duplication on the wire
// in the normal path. This exists for the paths where Caddy is not in front: a
// direct Service call inside the cluster, a port-forward during debugging, and
// whatever fronts this after the compose stack.
//
// # What is set here and NOT at the proxy
//
//	Content-Security-Policy: default-src 'none'; frame-ancestors 'none'
//	    Correct for a JSON API and impossible at the proxy, which serves the
//	    Next.js app on the same host. The Caddyfile says so explicitly: the app's
//	    CSP needs per-request nonces and is emitted by the app. A JSON endpoint
//	    needs no sources at all, so it declares none — which neutralises the
//	    response if a browser is ever tricked into rendering it.
//
//	Cache-Control: no-store
//	    An API response is specific to one caller and one instant. The odds board
//	    changes continuously and an account response is private. Setting it here
//	    rather than at the proxy means a handler can override it for genuinely
//	    public, genuinely cacheable data by setting its own value — this
//	    middleware writes BEFORE the handler runs, so the handler wins.
//
// HSTS is deliberately absent. TLS terminates at the proxy, so this service
// never sees an https request and is in no position to make a claim about the
// scheme; the Caddyfile sets it, and only on non-loopback hosts.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}

// CORSOptions configures cross-origin access.
//
// # The default is "no CORS at all", and that is correct here
//
// An empty AllowedOrigins emits no CORS headers and answers no preflight, which
// denies every cross-origin request. That is not a stub — it is the right
// configuration for this deployment. CLAUDE.md §7: "The browser talks to the API
// through the reverse proxy, never to a container hostname", and
// deploy/proxy/Caddyfile serves the app at / and the API at /api/* on ONE
// origin. Same-origin requests are not subject to CORS, so a correctly deployed
// Sharpline never needs a single one of these headers.
//
// Anything that turns this on is describing a topology the charter does not have
// — a separately hosted frontend, a third-party integration — and should say so
// in an ADR.
type CORSOptions struct {
	// AllowedOrigins is an exact-match allowlist of scheme://host[:port].
	// Wildcards are not supported and will not be: an origin allowlist that
	// pattern-matches is an origin allowlist that eventually matches something
	// unintended.
	AllowedOrigins []string

	// AllowedMethods defaults to the methods this API answers.
	AllowedMethods []string

	// AllowedHeaders defaults to Authorization, Content-Type and X-Request-Id.
	AllowedHeaders []string

	// ExposedHeaders are the response headers script may read. The rate-limit
	// headers are included by default, because a client that cannot read
	// Retry-After cannot honour it.
	ExposedHeaders []string

	// AllowCredentials permits cookies and Authorization on cross-origin
	// requests. It is INCOMPATIBLE with a wildcard origin by specification, and
	// since this type has no wildcard the combination cannot be expressed.
	AllowCredentials bool

	// MaxAgeSeconds is how long a browser may cache the preflight result.
	MaxAgeSeconds int
}

var (
	defaultCORSMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete}
	defaultCORSHeaders = []string{"Authorization", "Content-Type", "X-Request-Id"}
	defaultCORSExposed = []string{headerRequestID, headerRateLimit, headerRateLimitRemaining, headerRateLimitReset, headerRetryAfter}
)

// CORS applies the cross-origin policy and answers preflight requests.
//
// # Vary: Origin is set on every response, allowed or not
//
// Without it a shared cache can store the response produced for one origin and
// replay it to another — including replaying an allowed response to an origin
// that is not on the list. It is set even when the origin is rejected, because
// the rejection is itself origin-dependent.
func CORS(opts CORSOptions) Middleware {
	allowed := make(map[string]struct{}, len(opts.AllowedOrigins))
	for _, o := range opts.AllowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = struct{}{}
		}
	}
	methods := strings.Join(orDefaultList(opts.AllowedMethods, defaultCORSMethods), ", ")
	headers := strings.Join(orDefaultList(opts.AllowedHeaders, defaultCORSHeaders), ", ")
	exposed := strings.Join(orDefaultList(opts.ExposedHeaders, defaultCORSExposed), ", ")
	maxAge := strconv.Itoa(opts.MaxAgeSeconds)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			isPreflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""

			if origin == "" {
				// Same-origin or a non-browser client. Nothing to negotiate.
				if isPreflight {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Add("Vary", "Origin")

			if _, ok := allowed[origin]; !ok {
				// No Access-Control-Allow-Origin header: the browser blocks the
				// response. A preflight still gets a 204 rather than an error,
				// because the CORS failure the developer needs to see is the
				// missing header, and a 403 here produces a confusing
				// "preflight failed" message that hides it.
				if isPreflight {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			if opts.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if exposed != "" {
				h.Set("Access-Control-Expose-Headers", exposed)
			}

			if isPreflight {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", methods)
				h.Set("Access-Control-Allow-Headers", headers)
				if opts.MaxAgeSeconds > 0 {
					h.Set("Access-Control-Max-Age", maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func orDefaultList(v, fallback []string) []string {
	if len(v) == 0 {
		return fallback
	}
	return v
}

// MaxBytes caps the request body.
//
// Two mechanisms, because neither alone is sufficient:
//
//   - A declared Content-Length over the limit is rejected immediately with 413.
//     This is the common case and it costs nothing: the body is never read, so a
//     hostile client cannot make the server spend bandwidth to find out it will
//     refuse.
//   - The body is wrapped in http.MaxBytesReader regardless, which is what
//     catches a chunked request that declares no length at all — the case the
//     Content-Length check cannot see. It surfaces in the handler as a read
//     error, which the handler renders as its own 400/413.
//
// The wrapper is given w as well as the body, which is what lets net/http know
// the request was too large and suppress the "connection reset by peer" noise
// that otherwise follows a truncated read.
func MaxBytes(limit int64, ew ErrorWriter) Middleware {
	if limit <= 0 {
		limit = DefaultMaxRequestBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeProblemWith(ew, w, r, problemTooLarge)
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientAddr resolves the client address once, per the trusted-proxy rules in
// clientip.go, and puts it in the context.
//
// It runs early so the rate limiter, the access log and any handler that needs
// it all see the SAME answer. Deriving it independently in three places is how
// a limiter ends up bucketing by one address while the log line names another.
func ClientAddr(trusted TrustedProxies) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addr := clientAddr(r, trusted)
			next.ServeHTTP(w, r.WithContext(withClientIP(r.Context(), addr)))
		})
	}
}
