// Instrumentation for the Redis access layer: Prometheus metrics and
// OpenTelemetry spans.
//
// Both are emitted from ONE place — a go-redis Hook installed on the client —
// because go-redis routes every command any caller makes through it. That
// matters more than it sounds: this package exports the raw client (see
// Client.Redis) so consumers can declare their own small interfaces over it, so
// there is no wrapper to count calls in. A wrapper would measure only the calls
// that went through the wrapper, which is the kind of metric that reads
// plausibly and is wrong. The hook measures all of them.
//
// # Metric names are a contract
//
// deploy/observability/prometheus.yml states it: "every application series is
// prefixed `sharpline_`", and the Grafana dashboard plus
// deploy/observability/rules/sharpline-alerts.yml are written against those
// names. Every series below follows that prefix and the `sharpline_redis_`
// subsystem.
//
// The phase-0 dashboard has NO Redis panels — it covers odds staleness,
// provider quota, WebSocket fanout, pricing latency and bus lag, and phase 2
// added the `sharpline_db_` family beneath it. So there was no existing name to
// match and none was invented over the top of one: these are new, and the exact
// PromQL a dashboard panel needs is written next to each definition so the panel
// author does not have to guess.
//
// # Labels this package deliberately does NOT set
//
//   - `service`. prometheus.yml attaches `service` as a TARGET label on every
//     scrape job. A metric label of the same name would be renamed to
//     `exported_service` on ingest and the two would drift.
//   - the key. Keys carry user identifiers — a client IP, a user id, a market
//     id — so a key label is both unbounded cardinality and a privacy leak into
//     a system that is scraped, federated and retained. The command name is
//     bounded and is what a latency graph actually needs.
//   - command arguments, anywhere, in metrics or in spans. Arguments carry user
//     data and, for AUTH, the password itself. See commandHook.ProcessHook.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Metric namespace and subsystem. Together they produce the `sharpline_redis_`
// prefix on every series this package exports.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "redis"

	// tracerName is the instrumentation scope every span from this package
	// carries.
	tracerName = "github.com/anpl1623/sharpline/internal/platform/redis"
)

// Outcome label values. A closed set — every one appears in at least one code
// path below and nothing else is ever written to these labels.
const (
	outcomeOK    = "ok"
	outcomeError = "error"

	connectOK        = "ok"
	connectRetryable = "retryable"
	connectFatal     = "fatal"

	// Error kinds. Coarse on purpose: fine-grained server error text is
	// unbounded, and these four are the ones that lead to different actions.
	errKindMiss      = "miss"      // redis.Nil — the key does not exist
	errKindTimeout   = "timeout"   // the caller's or the socket's deadline
	errKindConnect   = "connect"   // could not reach the server
	errKindServer    = "server"    // the server answered with an error
	errKindCancelled = "cancelled" // the caller went away
)

// commandBuckets are tighter than the database equivalents because every
// command this system issues is O(1) against an in-memory server on the same
// network. A sample past 100ms is a pathology, not a slow query, and the top
// bucket is deliberately near DefaultReadTimeout so a timeout and a slow
// command are distinguishable on the graph.
var commandBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1,
}

// pingBuckets is tighter still: the readiness probe's whole budget is
// httpx.DefaultReadinessTimeout (3s) and this client's PingTimeout (1s), so
// anything beyond that is a timeout rather than a measurement.
var pingBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1,
}

// metrics holds every collector this package owns. It is a value on Client, not
// a package-level variable: CLAUDE.md §12 forbids global mutable state, and two
// clients in one process must not silently share counters.
type metrics struct {
	commandDuration *prometheus.HistogramVec // command, outcome
	commandErrors   *prometheus.CounterVec   // command, kind
	pingDuration    prometheus.Histogram
	up              prometheus.Gauge
	connectAttempts *prometheus.CounterVec // outcome

	// Rate-limiter series. They live here rather than on RateLimiter so that a
	// process holding several limiters (per-IP and per-user are two) registers
	// one set of collectors, not one per limiter.
	rateLimitDecisions *prometheus.CounterVec   // scope, decision
	rateLimitDuration  *prometheus.HistogramVec // scope
}

// newMetrics builds the collectors and registers them on reg.
//
// reg may be nil, which builds the collectors but registers nothing. That is
// the right behaviour for a unit test and for a binary that serves no /metrics
// — the observe calls stay live and cost a few nanoseconds, so no call site
// needs a nil check.
//
// Registration failure is returned, not swallowed. Two clients sharing one
// registry is a programming error and it fails at startup rather than producing
// two services' worth of numbers under one series.
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		commandDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "command_duration_seconds",
			Help: "Duration of Redis commands, by command name and outcome. Excludes the readiness ping, " +
				"which has its own series so a probe every few seconds cannot dominate the histogram. " +
				"Panel: histogram_quantile(0.99, sum by (le, command) (rate(sharpline_redis_command_duration_seconds_bucket[$__rate_interval]))).",
			Buckets: commandBuckets,
		}, []string{"command", "outcome"}),

		commandErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "command_errors_total",
			Help: "Redis commands that returned an error, by command and kind: miss (the key does not exist — " +
				"a normal operating state, counted so a cache hit rate is computable), timeout, connect, " +
				"server, cancelled.",
		}, []string{"command", "kind"}),

		pingDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "ping_duration_seconds",
			Help: "Duration of the readiness round trip: acquire a pooled connection and PING. " +
				"Includes pool acquisition, so it goes up when the pool saturates as well as when the server slows down.",
			Buckets: pingBuckets,
		}),

		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "up",
			Help: "1 if the most recent readiness check reached Redis, 0 if it did not. " +
				"Alert on this rather than on readiness: `api` deliberately keeps serving with Redis down " +
				"(rate limiting fails open), so a Redis outage is invisible in the probe and visible here.",
		}),

		connectAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "connect_attempts_total",
			Help: "Startup connectivity attempts by outcome: ok, retryable (a transient failure that was retried), " +
				"fatal (a failure that was not retried — wrong password, unknown ACL user).",
		}, []string{"outcome"}),

		rateLimitDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "rate_limit_decisions_total",
			Help: "Token-bucket decisions by scope (ip, user, ...) and decision (allowed, limited, error). " +
				"decision=\"error\" is Redis being unreachable; what the caller does about it is the caller's " +
				"policy — internal/httpapi/middleware fails open and counts that separately. " +
				"Panel: sum by (scope) (rate(sharpline_redis_rate_limit_decisions_total{decision=\"limited\"}[$__rate_interval])).",
		}, []string{"scope", "decision"}),

		rateLimitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "rate_limit_duration_seconds",
			Help: "Wall time of one rate-limit decision, by scope. This is on the hot path of every HTTP request, " +
				"so it is the series that says whether the limiter is costing more than it is protecting.",
			Buckets: commandBuckets,
		}, []string{"scope"}),
	}

	if reg == nil {
		return m, nil
	}

	for _, c := range []prometheus.Collector{
		m.commandDuration,
		m.commandErrors,
		m.pingDuration,
		m.up,
		m.connectAttempts,
		m.rateLimitDecisions,
		m.rateLimitDuration,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("redis: register collector: %w", err)
		}
	}
	return m, nil
}

// -----------------------------------------------------------------------------
// Pool statistics
// -----------------------------------------------------------------------------

// poolStatsCollector exports go-redis' pool counters on scrape rather than
// mirroring them into gauges on a ticker. Same design as
// internal/platform/postgres' collector and for the same reason: a ticker that
// stops (because the client was closed) leaves the last values frozen on the
// graph, which is indistinguishable from a pool that stopped changing.
type poolStatsCollector struct {
	stats func() *goredis.PoolStats

	conns    *prometheus.Desc
	hits     *prometheus.Desc
	misses   *prometheus.Desc
	timeouts *prometheus.Desc
	stale    *prometheus.Desc
}

func newPoolStatsCollector(stats func() *goredis.PoolStats) *poolStatsCollector {
	const ns, sub = metricNamespace, metricSubsystem
	return &poolStatsCollector{
		stats: stats,
		conns: prometheus.NewDesc(
			prometheus.BuildFQName(ns, sub, "pool_connections"),
			"Connections in the client pool by state: total, idle. "+
				"total saturating at PoolSize together with a rising pool_timeouts_total is the signature of a pool that is too small.",
			[]string{"state"}, nil),
		hits: prometheus.NewDesc(
			prometheus.BuildFQName(ns, sub, "pool_hits_total"),
			"Times a command found a free connection already in the pool.",
			nil, nil),
		misses: prometheus.NewDesc(
			prometheus.BuildFQName(ns, sub, "pool_misses_total"),
			"Times a command had to open a new connection because none was free.",
			nil, nil),
		timeouts: prometheus.NewDesc(
			prometheus.BuildFQName(ns, sub, "pool_timeouts_total"),
			"Times a command waited PoolTimeout for a connection and gave up. Non-zero means the pool is the bottleneck, not the server.",
			nil, nil),
		stale: prometheus.NewDesc(
			prometheus.BuildFQName(ns, sub, "pool_stale_connections_total"),
			"Connections removed for exceeding ConnMaxIdleTime or ConnMaxLifetime.",
			nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *poolStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.conns
	ch <- c.hits
	ch <- c.misses
	ch <- c.timeouts
	ch <- c.stale
}

// Collect implements prometheus.Collector. A closed client reports nothing at
// all, rather than reporting the last statistics of a pool that no longer
// exists.
func (c *poolStatsCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stats()
	if s == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.TotalConns), "total")
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.IdleConns), "idle")
	ch <- prometheus.MustNewConstMetric(c.hits, prometheus.CounterValue, float64(s.Hits))
	ch <- prometheus.MustNewConstMetric(c.misses, prometheus.CounterValue, float64(s.Misses))
	ch <- prometheus.MustNewConstMetric(c.timeouts, prometheus.CounterValue, float64(s.Timeouts))
	ch <- prometheus.MustNewConstMetric(c.stale, prometheus.CounterValue, float64(s.StaleConns))
}

// -----------------------------------------------------------------------------
// Command hook
// -----------------------------------------------------------------------------

// noInstrumentationKey marks a context whose commands must not be measured or
// traced.
type noInstrumentationKey struct{}

// withoutInstrumentation suppresses the hook for commands issued on the
// returned context.
//
// Exactly one caller uses it: the readiness probe. A probe fires every few
// seconds per replica for ever, so a span per probe would swamp the traces
// CLAUDE.md §9 actually wants — the ones following one odds update from ingest
// to the browser — and a sample per probe would dominate a histogram that is
// supposed to describe application traffic. The probe has its own dedicated
// series (sharpline_redis_ping_duration_seconds) instead. Same decision, and
// same reasoning, as internal/platform/postgres and internal/platform/kafka.
func withoutInstrumentation(ctx context.Context) context.Context {
	return context.WithValue(ctx, noInstrumentationKey{}, true)
}

func instrumentationSuppressed(ctx context.Context) bool {
	v, _ := ctx.Value(noInstrumentationKey{}).(bool)
	return v
}

// commandHook is the single place every Redis command is measured and traced.
type commandHook struct {
	tracer  trace.Tracer
	metrics *metrics
}

var _ goredis.Hook = (*commandHook)(nil)

// DialHook implements goredis.Hook. Dials are not instrumented individually —
// the pool statistics already describe connection churn, and a span per dial
// during a reconnect storm is noise, not signal.
func (h *commandHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return next
}

// ProcessHook implements goredis.Hook: one span and one histogram sample per
// command.
//
// # Only the command name leaves this function
//
// cmd.Args() holds the arguments, and for AUTH the second argument is the
// password. Nothing here reads past cmd.Args()[0], and cmd.String() — which
// renders the whole command, arguments included — is never called. That is the
// structural half of "never log a credential": there is no code path that could
// emit one, rather than a rule somebody has to remember. The unit test asserts
// it against a real AUTH command.
func (h *commandHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if instrumentationSuppressed(ctx) {
			return next(ctx, cmd)
		}

		name := commandName(cmd)

		ctx, span := h.tracer.Start(ctx, "redis "+name,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("db.system", "redis"),
				attribute.String("db.operation", name),
			),
		)

		start := time.Now()
		err := next(ctx, cmd)
		elapsed := time.Since(start)

		h.record(name, elapsed, err)

		switch {
		case err == nil:
			// nothing to record
		case errors.Is(err, goredis.Nil):
			// A miss is not a failure. Marking the span with an error status
			// would light up every cache read as broken in Jaeger.
			span.SetAttributes(attribute.Bool("db.redis.key_missing", true))
		default:
			// The message may name the command and the server, never an
			// argument: go-redis builds server errors from the RESP error line,
			// which the server writes without echoing arguments.
			span.RecordError(err)
			span.SetStatus(codes.Error, errorKind(err))
		}
		span.End()
		return err
	}
}

// ProcessPipelineHook implements goredis.Hook. A pipeline is measured as one
// unit under the synthetic command name "pipeline": the individual commands
// share a single round trip, so attributing the latency to each of them
// separately would report the same milliseconds N times.
func (h *commandHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if instrumentationSuppressed(ctx) {
			return next(ctx, cmds)
		}

		ctx, span := h.tracer.Start(ctx, "redis pipeline",
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("db.system", "redis"),
				attribute.String("db.operation", "pipeline"),
				attribute.Int("db.redis.commands", len(cmds)),
			),
		)

		start := time.Now()
		err := next(ctx, cmds)
		h.record("pipeline", time.Since(start), err)

		if err != nil && !errors.Is(err, goredis.Nil) {
			span.RecordError(err)
			span.SetStatus(codes.Error, errorKind(err))
		}
		span.End()
		return err
	}
}

func (h *commandHook) record(name string, elapsed time.Duration, err error) {
	outcome := outcomeOK
	if err != nil && !errors.Is(err, goredis.Nil) {
		outcome = outcomeError
	}
	h.metrics.commandDuration.WithLabelValues(name, outcome).Observe(elapsed.Seconds())
	if err != nil {
		h.metrics.commandErrors.WithLabelValues(name, errorKind(err)).Inc()
	}
}

// commandName extracts the bounded, non-secret label: the command verb.
//
// It reads cmd.Args()[0] and nothing else, uppercases it, and refuses anything
// that is not a plausible command word — an argument slice can be built by a
// caller, so this must not become a cardinality hole.
func commandName(cmd goredis.Cmder) string {
	args := cmd.Args()
	if len(args) == 0 {
		return "unknown"
	}
	s, ok := args[0].(string)
	if !ok {
		return "unknown"
	}
	if len(s) == 0 || len(s) > 24 {
		return "unknown"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			continue
		}
		return "unknown"
	}
	return strings.ToUpper(s)
}

// errorKind maps an error onto one of the five bounded kinds. Coarse on
// purpose: server error text is unbounded and these are the distinctions that
// lead to different actions.
func errorKind(err error) string {
	switch {
	case err == nil:
		return outcomeOK
	case errors.Is(err, goredis.Nil):
		return errKindMiss
	case errors.Is(err, context.Canceled):
		return errKindCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return errKindTimeout
	}

	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errKindTimeout
	}

	var redisErr goredis.Error
	if errors.As(err, &redisErr) {
		return errKindServer
	}
	return errKindConnect
}
