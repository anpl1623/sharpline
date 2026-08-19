package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
)

// maxStackBytes bounds the stack captured on a panic. 16 KB is roughly 120
// frames, which is more than any handler in this system has and small enough
// that a panic loop cannot fill a disk before the alert fires.
const maxStackBytes = 16 << 10

// Recover turns a handler panic into a 500 and logs it.
//
// # The stack goes to the log and never to the client
//
// A Go stack trace names package paths, file paths, line numbers and function
// names. Returned in a response body it is a free map of the service's internals
// to anyone who can make it crash, and it is exactly the artefact that turns "I
// found a panic" into "I found where to look next". So the client gets the
// generic 500 envelope with the request id, and the operator gets the stack in a
// log line correlated by that same id.
//
// # Why this is not enough on its own
//
// CLAUDE.md §12 says "never panic outside main", and that rule is the actual
// control. This is a net, not a policy: it keeps one bad request from taking the
// process down with it and, crucially, keeps net/http from silently closing the
// connection with no record of why. Every increment of sharpline_http_panics_total
// is a bug to fix, which is why the metric exists and why its help text says any
// non-zero rate is a defect.
//
// # http.ErrAbortHandler is re-panicked, deliberately
//
// net/http documents it as the way a handler aborts a response without being
// reported as an error — it is how a hijacked or streamed response says "stop
// now". Swallowing it here would convert a normal abort into a spurious 500 and
// a spurious panic count, and would try to write a body onto a connection the
// handler has already given up on.
func Recover(base *slog.Logger, m *Metrics, ew ErrorWriter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := ensureRecorder(w)

			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}

				buf := make([]byte, maxStackBytes)
				n := runtime.Stack(buf, false)

				log := base
				if l, ok := r.Context().Value(loggerKey{}).(*slog.Logger); ok && l != nil {
					log = l
				}

				f := fieldsFor(r)
				log.LogAttrs(r.Context(), slog.LevelError, "panic recovered in http handler",
					slog.String("method", f.method),
					slog.String("path", f.path),
					slog.String("route", routeLabel(f.route)),
					slog.String("client_ip", f.clientIP),
					slog.String("user_id", f.userID),
					// %v rather than %+v: the panic value is whatever the
					// handler passed, and a %+v on a struct carrying request
					// data would print that data into the log.
					slog.String("panic", fmt.Sprintf("%v", v)),
					slog.String("stack", string(buf[:n])),
				)

				if m != nil {
					m.panics.WithLabelValues(routeLabel(f.route)).Inc()
				}

				// If the handler already wrote a status, the response is on the
				// wire and a second WriteHeader would be ignored by net/http and
				// logged as a superfluous call. The truncated body is the best
				// available outcome and the log line above is the record of it.
				if rec.wroteHeader {
					return
				}
				writeProblemWith(ew, rec, r, problemInternal)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}
