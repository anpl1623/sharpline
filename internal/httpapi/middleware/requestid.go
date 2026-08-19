package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

// maxRequestIDLen bounds an inbound id. 128 is far more than any generator
// needs and small enough that a hostile header cannot bloat every log line for
// the lifetime of the request.
const maxRequestIDLen = 128

// RequestID assigns every request a stable identifier, puts it in the context
// and echoes it in the response.
//
// # Reusing the proxy's id rather than minting a new one
//
// deploy/proxy/Caddyfile sets `header_up X-Request-Id {http.request.uuid}` on
// the /api route and logs the same value under `request_id`. Adopting it is what
// makes one identifier span the proxy's access log and this service's slog
// output; minting a fresh one here would give the same request two ids and make
// the join impossible exactly when it is needed.
//
// # The inbound value is still not trusted
//
// It is sanitised before use. An id reaches three places a hostile value could
// do damage: a response header (CRLF is response splitting), a JSON log line,
// and the error body. Restricting it to a conservative character set and a
// length closes all three at the point of entry rather than at each use. A value
// that fails the check is replaced by a generated one — never rejected with an
// error, because a malformed correlation header is not a reason to fail a
// request.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitiseRequestID(r.Header.Get(headerRequestID))
			if id == "" {
				id = newRequestID()
			}

			// Echoed so a browser network trace joins to the server logs.
			w.Header().Set(headerRequestID, id)

			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sanitiseRequestID returns s if it is a safe correlation id, otherwise "".
func sanitiseRequestID(s string) string {
	if s == "" || len(s) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == ':':
		default:
			return ""
		}
	}
	return s
}

// newRequestID mints an opaque 128-bit id.
//
// crypto/rand rather than math/rand because the id appears in error bodies
// returned to clients: a predictable sequence would let one caller guess
// another's id and quote it at a support channel. rand.Read cannot fail on any
// platform Go supports — it panics internally rather than returning an error —
// so there is no failure branch to write here.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Correlate derives the request-scoped logger and puts it in the context.
//
// It must run AFTER RequestID and Trace, because it reads what both of them
// establish. Every line a handler writes through LoggerFrom(ctx) then carries
// request_id, trace_id and span_id, which is CLAUDE.md §9's "structured JSON
// logging via log/slog with trace correlation" made true for application code
// rather than only for this package.
//
// trace_id and span_id are attached only when the span is recording and its
// context is valid. Attaching the all-zero id that a no-op tracer produces
// would put a field in every log line that looks like a trace and resolves to
// nothing in Jaeger.
func Correlate(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			attrs := make([]any, 0, 3)
			if id := RequestIDFrom(ctx); id != "" {
				attrs = append(attrs, slog.String("request_id", id))
			}
			if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
				attrs = append(attrs,
					slog.String("trace_id", sc.TraceID().String()),
					slog.String("span_id", sc.SpanID().String()),
				)
			}

			log := base
			if len(attrs) > 0 {
				log = base.With(attrs...)
			}
			next.ServeHTTP(w, r.WithContext(withLogger(ctx, log)))
		})
	}
}
