// Prometheus instrumentation for the HTTP surface.
//
// # Metric names are a contract
//
// deploy/observability/prometheus.yml states it: "every application series is
// prefixed `sharpline_`", and deploy/observability/grafana/dashboards plus
// deploy/observability/rules/sharpline-alerts.yml are written against those
// names. Every series below follows that prefix and the `sharpline_http_`
// subsystem.
//
// The phase-0 dashboard has NO HTTP panels — it covers odds staleness, provider
// quota, WebSocket fanout, pricing latency and bus lag; phase 2 added
// `sharpline_db_` and this phase adds `sharpline_redis_` beneath it. So there
// was no existing name to match and none was invented over the top of one: these
// are new, and the PromQL a panel needs is written next to each definition.
//
// # Cardinality, counted rather than hoped for
//
// The largest series here is request_duration_seconds{method,route,status}, at
// 12 buckets + _sum + _count = 14 series per label combination. With ~40 routes,
// the 5 methods this API answers and the ~8 status codes it actually returns,
// the worst case is 40 * 5 * 8 * 14 = 22,400 series per replica — and the real
// figure is a small fraction of that, because a given route answers three or
// four statuses, not eight, and one method, not five. That is comfortable for a
// single Prometheus on the 12 GB deploy target.
//
// The reason it stays bounded is `route` being a PATTERN and never a path; see
// RouteFunc. `status` is the exact code rather than a class because "how many
// 401s" and "how many 403s" are different questions with different answers, and
// collapsing them to 4xx loses exactly the distinction an auth problem shows up
// as.
//
// # Labels this package deliberately does NOT set
//
//   - `service`. prometheus.yml attaches it as a TARGET label on every scrape
//     job; a metric label of the same name would be renamed to
//     `exported_service` on ingest and the two would drift.
//   - the client IP, the user id, the query string, the user agent. All are
//     unbounded, and the first two are personal data in a system that is
//     scraped, federated and retained.
package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "sharpline"
	metricSubsystem = "http"
)

// durationBuckets span the range this API actually operates in: a cached
// catalogue read in single-digit milliseconds at the bottom, and the proxy's
// own response_header_timeout (30s, deploy/proxy/Caddyfile) at the top — a
// request past that boundary has already been abandoned by the proxy, so
// measuring further is measuring nothing.
//
// 0.25 is present for the same reason it is in internal/platform/postgres:
// deploy/postgres/postgresql.conf logs statements slower than 250ms, so a p99
// crossing this boundary should be accompanied by lines in the Postgres log, and
// if it is not, one of the two is lying.
var durationBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 30,
}

// sizeBuckets run from an empty JSON object to the 1 MB body cap the proxy
// enforces on /api.
var sizeBuckets = []float64{
	64, 256, 1024, 4096, 16384, 65536, 262144, 1048576,
}

// Metrics holds every collector this package owns.
//
// It is a value the caller constructs and injects, not a package-level variable:
// CLAUDE.md §12 forbids global mutable state, and it is what lets a test build a
// chain against a throwaway registry instead of leaking counters between tests.
type Metrics struct {
	requests    *prometheus.CounterVec   // method, route, status
	duration    *prometheus.HistogramVec // method, route, status
	inFlight    prometheus.Gauge
	respSize    *prometheus.HistogramVec // route
	rateLimited *prometheus.CounterVec   // scope, route
	failOpen    *prometheus.CounterVec   // scope
	auth        *prometheus.CounterVec   // outcome
	panics      *prometheus.CounterVec   // route
	timeouts    *prometheus.CounterVec   // route
}

// Auth outcome label values. A closed set.
//
// The granularity here is fine where the RESPONSE's is deliberately coarse, and
// that asymmetry is the point: the client must not be able to tell an unknown
// account from a wrong password (see auth.go), but the operator must, or an
// outage in the token verifier is indistinguishable from a wave of expired
// sessions.
const (
	authOK        = "ok"
	authAbsent    = "absent"    // no credential presented; the request is anonymous
	authMalformed = "malformed" // an Authorization header that is not a bearer token
	authRejected  = "rejected"  // a token that did not verify
	authRequired  = "required"  // an endpoint that requires identity received none
)

// NewMetrics builds the collectors and registers them on reg.
//
// reg may be nil, which builds the collectors but registers nothing — correct
// for a unit test. The observe calls stay live and cost a few nanoseconds, so no
// call site needs a nil check.
//
// Registration failure is returned, not swallowed. Two chains sharing one
// registry is a programming error and it fails at startup rather than producing
// two services' worth of numbers under one series.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
		}, labels)
	}

	m := &Metrics{
		requests: counter("requests_total",
			"HTTP requests served, by method, route pattern and status code. "+
				"Panel: sum by (route) (rate(sharpline_http_requests_total[$__rate_interval])). "+
				"Error ratio: sum(rate(sharpline_http_requests_total{status=~\"5..\"}[5m])) / sum(rate(sharpline_http_requests_total[5m])). "+
				"route=\"unmatched\" is a request that matched no registered pattern — a scanner, or a frontend calling an endpoint that does not exist.",
			"method", "route", "status"),

		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "request_duration_seconds",
			Help: "Wall time from entering the middleware chain to the handler returning, by method, route and status. " +
				"Measured OUTSIDE panic recovery, so a panicking request is timed and counted as the 500 it became. " +
				"Panel: histogram_quantile(0.99, sum by (le, route) (rate(sharpline_http_request_duration_seconds_bucket[$__rate_interval]))).",
			Buckets: durationBuckets,
		}, []string{"method", "route", "status"}),

		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "requests_in_flight",
			Help: "Requests currently being served by this replica. Concurrency, not rate: it is the series that " +
				"distinguishes a slow dependency (in-flight climbs, request rate flat) from a traffic spike (both climb).",
		}),

		respSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name:    "response_size_bytes",
			Help:    "Response body bytes written, by route. Top bucket is the 1 MB body cap deploy/proxy/Caddyfile enforces on /api.",
			Buckets: sizeBuckets,
		}, []string{"route"}),

		rateLimited: counter("rate_limited_total",
			"Requests rejected with 429, by limiter scope (ip, user) and route. "+
				"CLAUDE.md §6 requires both scopes and they answer different questions: scope=\"ip\" rising on the login "+
				"route is credential stuffing, scope=\"user\" rising is one account hammering the API. "+
				"Panel: sum by (scope) (rate(sharpline_http_rate_limited_total[$__rate_interval])).",
			"scope", "route"),

		failOpen: counter("rate_limit_fail_open_total",
			"Requests admitted WITHOUT a rate-limit decision because Redis could not be reached. "+
				"This is the alertable series for \"the API is running unprotected\": rate limiting fails open by design "+
				"(a Redis outage must not become an API outage), so the degradation is invisible in the readiness probe "+
				"and visible only here and in sharpline_redis_up. "+
				"Alert: sum(rate(sharpline_http_rate_limit_fail_open_total[5m])) > 0.",
			"scope"),

		auth: counter("auth_total",
			"Authentication outcomes: ok, absent (anonymous request), malformed (an Authorization header that is not "+
				"a bearer token), rejected (a token that did not verify), required (an authenticated endpoint reached "+
				"without an identity). A spike in \"rejected\" with a flat \"ok\" is a verifier problem — a rotated signing "+
				"key, a clock skew — rather than a wave of expired sessions.",
			"outcome"),

		panics: counter("panics_total",
			"Handler panics recovered and converted to 500, by route. Any non-zero rate is a bug. "+
				"Alert: sum(rate(sharpline_http_panics_total[5m])) > 0.",
			"route"),

		timeouts: counter("request_timeouts_total",
			"Requests whose context deadline expired before the handler returned, by route. Distinct from a 5xx: the "+
				"handler was still working when its budget ran out, which points at a slow dependency rather than at a defect.",
			"route"),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.requests, m.duration, m.inFlight, m.respSize,
		m.rateLimited, m.failOpen, m.auth, m.panics, m.timeouts,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("middleware: register collector: %w", err)
		}
	}
	return m, nil
}

// Observe records one request.
//
// It is placed OUTSIDE panic recovery in the canonical chain; see the package
// doc for why that ordering is what makes a panicking request countable as the
// 500 it became rather than as a status of 0.
func (m *Metrics) Observe() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, _ := ensureRouteCell(r.Context())
			r = r.WithContext(ctx)

			rec := ensureRecorder(w)
			start := time.Now()

			m.inFlight.Inc()
			defer func() {
				m.inFlight.Dec()

				elapsed := time.Since(start).Seconds()
				route := routeLabel(RouteFrom(ctx))
				status := strconv.Itoa(rec.status)

				m.requests.WithLabelValues(r.Method, route, status).Inc()
				m.duration.WithLabelValues(r.Method, route, status).Observe(elapsed)
				m.respSize.WithLabelValues(route).Observe(float64(rec.written))

				// request_timeouts_total is NOT incremented here. The deadline
				// lives on a child context created by the Timeout middleware
				// below this one, so ctx here is its parent and its Err() is
				// nil however long the request took. The counter is owned by
				// Timeout, which is the only layer that can see its own
				// deadline fire.
			}()

			next.ServeHTTP(rec, r)
		})
	}
}
