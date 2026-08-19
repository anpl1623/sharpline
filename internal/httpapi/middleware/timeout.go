package middleware

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// DefaultRequestTimeout is the per-request budget.
//
// 15s is chosen against the two deadlines that already exist around it:
//
//	deploy/proxy/Caddyfile   response_header_timeout 30s on the /api route —
//	                         "an API handler that has not produced response
//	                         headers in 30s is broken".
//	httpx.DefaultWriteTimeout 30s on the service's own listener.
//
// Sitting below both means the handler's context expires FIRST, so the request
// fails with a cause the service knows about and can log, rather than being
// severed by a proxy that knows only that nothing arrived. A deadline that fires
// after the layer above it has already given up is a deadline that never fires.
const DefaultRequestTimeout = 15 * time.Second

// Timeout puts a deadline on the request context.
//
// # Why a context deadline and not http.TimeoutHandler
//
// CLAUDE.md §12 requires that "every external call has a timeout", and it
// requires it via context.Context — which is exactly what this gives every
// downstream call for free: internal/platform/postgres, internal/platform/redis
// and internal/platform/kafka all take a ctx and all honour its deadline, so one
// deadline set here bounds the whole request's I/O without any handler doing
// anything.
//
// http.TimeoutHandler would additionally guarantee a 503 to the client at the
// deadline, and it was rejected for two reasons. It buffers the entire response
// in memory to be able to discard it, which defeats streaming and doubles the
// memory cost of a large catalogue page. And it makes the ResponseWriter
// non-hijackable and non-flushable, which is a trap for the moment any handler
// behind this chain needs either.
//
// The backstop for a handler that ignores its context is real and already
// present: httpx.DefaultWriteTimeout (30s) on the listener severs the
// connection, and the proxy's response_header_timeout does the same one hop out.
// A handler that reaches either of those is a handler that ignored its context,
// which is a defect — and it is a visible one, counted in
// sharpline_http_request_timeouts_total.
//
// A non-positive timeout disables the deadline. That is for the one case where
// it is correct — a handler serving something genuinely long-lived — and it is
// deliberately a per-mount decision, not a global default.
// The expiry counter lives HERE rather than in the metrics middleware, and that
// placement is forced: the deadline is on a CHILD context created inside this
// function, so an outer middleware holds the parent and would see ctx.Err() ==
// nil for every request no matter how long it took. Only the layer that set the
// deadline can tell whether it fired.
func Timeout(d time.Duration, m *Metrics) Middleware {
	if d <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			next.ServeHTTP(w, r.WithContext(ctx))

			// Counted even when the handler still managed to answer: the
			// interesting signal is that the budget was exhausted, not that the
			// client saw an error.
			if m != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				m.timeouts.WithLabelValues(routeLabel(RouteFrom(ctx))).Inc()
			}
		})
	}
}
