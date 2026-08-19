package middleware

import "net/http"

// RouteFunc resolves the ROUTE PATTERN a request matches — "GET /api/v1/events/
// {id}" — as opposed to its path — "GET /api/v1/events/01J8Z...".
//
// # Why the raw path can never be the label
//
// Every HTTP metric in this package is broken down by route. Using r.URL.Path
// would make that label unbounded: one time series per event id, per market id,
// per search string. Prometheus keeps every series it has ever seen in memory
// for the retention window, so an unbounded label on a request counter is not a
// messy graph, it is an out-of-memory kill of the monitoring system by ordinary
// production traffic. The same argument applies to the span name, which is why
// trace.go uses this too.
//
// # Why this is injected rather than read from r.Pattern
//
// Go 1.22's *http.ServeMux does record the matched pattern in r.Pattern — but it
// does so on a CLONE of the request that it passes to the handler. Middleware
// wrapping the mux holds the original, whose Pattern is empty for the whole
// request. MuxRouteFunc solves it by asking the mux to perform the match without
// dispatching.
type RouteFunc func(*http.Request) string

// MuxRouteFunc resolves routes by asking an *http.ServeMux which pattern a
// request matches.
//
// (*ServeMux).Handler performs the full match — method, host, path, wildcards —
// and returns the registered pattern WITHOUT dispatching, so this is the router's
// own answer rather than a reimplementation of its precedence rules that would
// drift the first time a route is added.
//
// The cost is one extra match per request against an in-memory trie. Cardinality
// is bounded by the number of registered patterns plus one: an unmatched request
// returns "", which becomes the single "unmatched" bucket.
func MuxRouteFunc(mux *http.ServeMux) RouteFunc {
	return func(r *http.Request) string {
		_, pattern := mux.Handler(r)
		return pattern
	}
}

// routeLabel renders a route for a metric label, collapsing the unresolved case
// into one bounded bucket.
//
// "unmatched" being a visible, countable value is deliberate: a rising
// sharpline_http_requests_total{route="unmatched"} is either a scanner probing
// paths or a frontend calling an endpoint the API does not have, and both are
// worth seeing.
func routeLabel(route string) string {
	if route == "" {
		return "unmatched"
	}
	return route
}

// ResolveRoute installs the route cell and, when fn is non-nil, resolves the
// pattern immediately.
//
// It runs near the top of the chain so that everything below — the span name,
// the metric labels, the access log line, the panic counter — describes the same
// route. A router that cannot be resolved up front (anything that is not an
// *http.ServeMux) passes fn == nil here and calls SetRoute from inside its own
// dispatch instead; the cell is already in place, so the value still reaches
// every outer layer.
func ResolveRoute(fn RouteFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cell := ensureRouteCell(r.Context())
			if fn != nil {
				cell.set(fn(r))
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
