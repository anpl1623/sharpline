package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// maxUserAgentLen bounds the one free-text field that reaches a log line. A
// header is attacker-controlled and net/http will happily carry kilobytes of it;
// truncating at the boundary keeps one hostile client from multiplying the log
// volume of every request it makes.
const maxUserAgentLen = 200

// requestFields is the CLOSED set of request facts the access logger may see.
//
// # This type is the security control
//
// The rule is "structured access logging that cannot log a credential", and
// "cannot" is doing the work. A logger handed an *http.Request can reach
// r.Header — and one slog.Any("headers", r.Header) added in a hurry during an
// incident leaks every bearer token, every session cookie and every
// Proxy-Authorization line into whatever the logs are shipped to, permanently
// and retroactively.
//
// So the access logger is not handed an *http.Request. fieldsFor is the only
// function that touches one, it returns this struct, and log.LogAttrs sees only
// this struct. There is no code path from a header to a log line, rather than a
// convention that there should not be one. accesslog_test.go proves it against a
// request carrying a real-looking token and cookie.
//
// # What is excluded, and why
//
//	Authorization, Cookie, Set-Cookie, Proxy-Authorization
//	    Credentials. deploy/proxy/Caddyfile deletes exactly these four from
//	    Caddy's own JSON access log; this is the same list, enforced the same
//	    way, one hop later.
//
//	the query string
//	    Not a credential by design, but it is by accident: a badly written
//	    client puts a token in a URL, an OAuth-style flow puts one there on
//	    purpose, and a search endpoint puts whatever the user typed there.
//	    Logging only the path costs a little debuggability and removes a whole
//	    class of leak. The route pattern is logged instead, which is what a
//	    latency investigation actually wants.
//
//	the request and response bodies
//	    A login body is a password. A wager body is a user's position.
type requestFields struct {
	method    string
	path      string
	route     string
	proto     string
	clientIP  string
	userAgent string
	referer   string
	userID    string
	bytesIn   int64
}

// fieldsFor is the only place in this package that reads from an *http.Request
// for logging purposes. Keep it that way.
func fieldsFor(r *http.Request) requestFields {
	f := requestFields{
		method:    r.Method,
		path:      r.URL.Path, // .Path, never .RequestURI — no query string.
		route:     RouteFrom(r.Context()),
		proto:     r.Proto,
		userAgent: truncate(r.UserAgent(), maxUserAgentLen),
		referer:   truncate(r.Referer(), maxUserAgentLen),
		bytesIn:   r.ContentLength,
	}
	if addr, ok := ClientIPFrom(r.Context()); ok {
		f.clientIP = addr.String()
	}
	if id, ok := IdentityFrom(r.Context()); ok {
		f.userID = id.UserID.String()
	}
	return f
}

// attrs renders the fields for slog. Empty values are dropped so an anonymous
// request does not carry an empty user_id and a curl request does not carry an
// empty referer.
func (f requestFields) attrs() []slog.Attr {
	out := make([]slog.Attr, 0, 9)
	out = append(out,
		slog.String("method", f.method),
		slog.String("path", f.path),
	)
	if f.route != "" {
		out = append(out, slog.String("route", f.route))
	}
	if f.clientIP != "" {
		out = append(out, slog.String("client_ip", f.clientIP))
	}
	if f.userID != "" {
		out = append(out, slog.String("user_id", f.userID))
	}
	if f.userAgent != "" {
		out = append(out, slog.String("user_agent", f.userAgent))
	}
	if f.referer != "" {
		out = append(out, slog.String("referer", f.referer))
	}
	if f.bytesIn > 0 {
		out = append(out, slog.Int64("bytes_in", f.bytesIn))
	}
	out = append(out, slog.String("proto", f.proto))
	return out
}

// AccessLog writes one structured line per request.
//
// # Level by status class
//
// 5xx logs at ERROR, 4xx at WARN, everything else at INFO. That is not cosmetic:
// SHARPLINE_LOG_LEVEL is an operational control, and a deployment that raises it
// to warn to cut volume must still see every failed request. Logging everything
// at INFO would make that setting turn off the errors along with the noise.
//
// # One line per request, after the fact
//
// Not a "started"/"finished" pair. A started line doubles the log volume of the
// busiest service in the system to convey information that is already implicit
// in the finished line, and it is only genuinely useful for requests that never
// finish — which are exactly the ones the in-flight gauge and the timeout
// counter already describe.
//
// The identity is read AFTER the handler returns, so a request authenticated
// deeper in the chain still logs its user_id.
func AccessLog(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Install the identity cell HERE, above the point where
			// Authenticate runs, so an identity established below is visible to
			// the deferred log line. See identityCell.
			ctx, _ := ensureIdentityCell(r.Context())
			r = r.WithContext(ctx)

			rec := ensureRecorder(w)
			start := time.Now()

			defer func() {
				elapsed := time.Since(start)

				// Prefer the request-scoped logger, which already carries
				// request_id and the trace ids. Falling back to the injected
				// base logger keeps AccessLog usable standalone.
				log := base
				if l, ok := r.Context().Value(loggerKey{}).(*slog.Logger); ok && l != nil {
					log = l
				}

				attrs := fieldsFor(r).attrs()
				attrs = append(attrs,
					slog.Int("status", rec.status),
					slog.Int64("bytes_out", rec.written),
					slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
				)

				log.LogAttrs(r.Context(), levelFor(rec.status), "http request", attrs...)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

func levelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// truncate bounds a free-text field, marking that it was cut so a truncated
// value is not mistaken for a complete one.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
