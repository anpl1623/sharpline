// Instrumentation for the Postgres access layer: Prometheus metrics and
// OpenTelemetry spans.
//
// Both are emitted from ONE place — a pgx tracer installed on the pool's
// connection config — because pgx routes every statement any caller makes
// through it. That matters more than it sounds: sqlc-generated code is handed
// the raw *pgxpool.Pool (its DBTX interface needs pgx's own types), so this
// package cannot wrap Query/Exec and count them there. A wrapper would measure
// only the calls that went through the wrapper, which is the kind of metric
// that reads plausibly and is wrong. The tracer measures all of them.
//
// # Metric names are a contract
//
// deploy/observability/prometheus.yml states it: "every application series is
// prefixed `sharpline_`", and the Grafana dashboard plus
// deploy/observability/rules/sharpline-alerts.yml are written against those
// names. Every series below follows that prefix and the `sharpline_db_`
// subsystem.
//
// The phase-0 dashboard has NO database panels — it covers odds staleness,
// provider quota, WebSocket fanout, pricing latency and bus lag, and nothing
// else. So there was no existing name to match and none was invented over the
// top of one: these names are new, and the exact PromQL a dashboard panel needs
// is written next to each definition so the panel author does not have to guess.
//
// # Labels this package deliberately does NOT set
//
//   - `service`. prometheus.yml attaches `service` as a TARGET label on every
//     scrape job. A metric label of the same name would be renamed to
//     `exported_service` on ingest and the two would drift.
//   - the SQL text. Raw statements as a label value is unbounded cardinality;
//     see operationName for what is used instead.
//   - query arguments, anywhere, in metrics or in spans. Arguments carry user
//     data. pgx keeps them separate from the statement text and so does this.
package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Metric namespace and subsystem. Together they produce the `sharpline_db_`
// prefix on every series this package exports.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "db"
)

// Outcome label values. A closed set — every one of them appears in at least
// one code path below, and nothing else is ever written to these labels.
const (
	outcomeOK    = "ok"
	outcomeError = "error"

	txCommitted    = "committed"
	txRolledBack   = "rolled_back"
	txCommitFailed = "commit_failed"
	txBeginFailed  = "begin_failed"
	txPanicked     = "panicked"

	connectOK        = "ok"
	connectRetryable = "retryable"
	connectFatal     = "fatal"
)

// Histogram buckets.
//
// queryBuckets includes 0.25 on purpose: deploy/postgres/postgresql.conf sets
// log_min_duration_statement = 250ms, so the bucket boundary and the server's
// slow-query log agree on where "slow" starts. A p99 that crosses 0.25 should be
// accompanied by lines in the Postgres log, and if it is not, one of the two is
// lying.
var queryBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// txBuckets runs out to 60s because that is
// idle_in_transaction_session_timeout in postgresql.conf: a transaction landing
// in the top bucket is one the server is about to kill.
var txBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

// pingBuckets is tighter than queryBuckets: the readiness probe's whole budget
// is httpx.DefaultReadinessTimeout (3s), so anything beyond that is a timeout
// rather than a measurement.
var pingBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3,
}

// metrics holds every collector this package owns. It is a value on DB, not a
// package-level variable: CLAUDE.md §12 forbids global mutable state, and two
// pools in one process must not silently share counters.
type metrics struct {
	queryDuration   *prometheus.HistogramVec // operation, outcome
	queryErrors     *prometheus.CounterVec   // operation, code
	txDuration      *prometheus.HistogramVec // outcome
	txTotal         *prometheus.CounterVec   // outcome
	pingDuration    prometheus.Histogram
	up              prometheus.Gauge
	connectAttempts *prometheus.CounterVec // outcome
}

// newMetrics builds the collectors and registers them on reg.
//
// The pool-statistics collector is deliberately NOT registered here: it needs a
// pool that does not exist yet at this point in Connect, so DB.registerPoolStats
// owns it. An earlier revision registered it in both places, and the duplicate
// registration failed every Connect — loudly, at startup, which is the behaviour
// the fail-fast rule buys.
//
// reg may be nil, which builds the collectors but registers nothing. That is
// the right behaviour for a unit test and for a one-shot job (migrate) that
// serves no /metrics endpoint — the observe calls stay live and cost a few
// nanoseconds, so no call site needs a nil check.
//
// Registration failure is returned, not swallowed. Two pools sharing one
// registry is a programming error and it fails at startup rather than producing
// two services' worth of numbers under one series.
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		queryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "query_duration_seconds",
			Help: "Duration of database statements, by sqlc query name (or SQL verb) and outcome. " +
				"Panel: histogram_quantile(0.99, sum by (le, operation) (rate(sharpline_db_query_duration_seconds_bucket[$__rate_interval]))).",
			Buckets: queryBuckets,
		}, []string{"operation", "outcome"}),

		queryErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "query_errors_total",
			Help: "Database statements that returned an error, by operation and SQLSTATE. " +
				"code=\"23514\" is the deferred double-entry ledger constraint rejecting an unbalanced movement.",
		}, []string{"operation", "code"}),

		txDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "transaction_duration_seconds",
			Help: "Wall time from BEGIN to COMMIT/ROLLBACK for transactions run through InTx, by outcome. " +
				"Buckets reach 60s because that is idle_in_transaction_session_timeout.",
			Buckets: txBuckets,
		}, []string{"outcome"}),

		txTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "transactions_total",
			Help: "Transactions run through InTx by outcome: committed, rolled_back, commit_failed, begin_failed, panicked. " +
				"outcome=\"commit_failed\" is where a DEFERRABLE INITIALLY DEFERRED constraint violation lands — " +
				"the statements all succeeded and COMMIT rejected them.",
		}, []string{"outcome"}),

		pingDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "ping_duration_seconds",
			Help: "Duration of the readiness round trip: acquire a pooled connection and execute a statement on it. " +
				"Includes pool acquisition, so it goes up when the pool saturates as well as when the server slows down.",
			Buckets: pingBuckets,
		}),

		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "up",
			Help: "1 if the most recent readiness check reached the database, 0 if it did not. " +
				"Distinguishes \"the service is down\" (up{component=\"backend\"} == 0) from " +
				"\"the service is up and its database is not\".",
		}),

		connectAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "connect_attempts_total",
			Help: "Startup connectivity attempts by outcome: ok, retryable (a transient failure that was retried), " +
				"fatal (a failure that was not retried — bad credentials, missing database, protocol violation).",
		}, []string{"outcome"}),
	}

	if reg == nil {
		return m, nil
	}

	collectors := []prometheus.Collector{
		m.queryDuration,
		m.queryErrors,
		m.txDuration,
		m.txTotal,
		m.pingDuration,
		m.up,
		m.connectAttempts,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("postgres: register metrics collector: %w", err)
		}
	}
	return m, nil
}

func (m *metrics) observeQuery(operation string, d time.Duration, err error) {
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
		m.queryErrors.WithLabelValues(operation, sqlStateOrClass(err)).Inc()
	}
	m.queryDuration.WithLabelValues(operation, outcome).Observe(d.Seconds())
}

func (m *metrics) observeTx(outcome string, d time.Duration) {
	m.txTotal.WithLabelValues(outcome).Inc()
	m.txDuration.WithLabelValues(outcome).Observe(d.Seconds())
}

func (m *metrics) observePing(d time.Duration, err error) {
	m.pingDuration.Observe(d.Seconds())
	if err != nil {
		m.up.Set(0)
		return
	}
	m.up.Set(1)
}

func (m *metrics) observeConnectAttempt(outcome string) {
	m.connectAttempts.WithLabelValues(outcome).Inc()
}

// -----------------------------------------------------------------------------
// Pool statistics
// -----------------------------------------------------------------------------

// poolStatsCollector exports pgxpool's own counters at scrape time.
//
// It is a prometheus.Collector rather than a goroutine that samples on a timer,
// because pgxpool.Stat is already an exact cumulative snapshot: sampling it on
// an interval that is not the scrape interval only adds aliasing.
//
// # Why acquire wait is a counter of seconds and not a histogram
//
// pgx exposes cumulative wait (Stat.AcquireDuration) and a count
// (Stat.AcquireCount), and offers no hook that brackets the wait, so a histogram
// of acquire latency is not constructible without wrapping every acquisition —
// and this package cannot wrap them all, because sqlc calls the pool directly.
// A seconds-total/count pair covers 100% of acquisitions exactly:
//
//	mean wait = rate(sharpline_db_pool_acquire_wait_seconds_total[5m])
//	          / rate(sharpline_db_pool_acquires_total[5m])
//
// A histogram covering only some acquisitions would give a p99 that looks
// authoritative and is not. Correct and coarse beats precise and partial.
type poolStatsCollector struct {
	stat func() *pgxpool.Stat

	connections   *prometheus.Desc
	maxConns      *prometheus.Desc
	acquireWait   *prometheus.Desc
	acquires      *prometheus.Desc
	emptyAcquires *prometheus.Desc
	canceled      *prometheus.Desc
	newConns      *prometheus.Desc
	destroyed     *prometheus.Desc
}

func newPoolStatsCollector(stat func() *pgxpool.Stat) *poolStatsCollector {
	name := func(s string) string {
		return prometheus.BuildFQName(metricNamespace, metricSubsystem, s)
	}
	return &poolStatsCollector{
		stat: stat,
		connections: prometheus.NewDesc(name("pool_connections"),
			"Connections in the pool by state: acquired (checked out), idle (available), constructing (being opened). "+
				"Utilisation panel: sharpline_db_pool_connections{state=\"acquired\"} / sharpline_db_pool_connections_max.",
			[]string{"state"}, nil),
		maxConns: prometheus.NewDesc(name("pool_connections_max"),
			"Configured ceiling on connections this pool will open. The sum of this across every replica of every "+
				"service must stay under the server's max_connections — see the arithmetic in postgres.go.",
			nil, nil),
		acquireWait: prometheus.NewDesc(name("pool_acquire_wait_seconds_total"),
			"Cumulative time callers spent waiting for a pooled connection. Divide the rate of this by the rate of "+
				"sharpline_db_pool_acquires_total for mean wait.",
			nil, nil),
		acquires: prometheus.NewDesc(name("pool_acquires_total"),
			"Connections successfully acquired from the pool.", nil, nil),
		emptyAcquires: prometheus.NewDesc(name("pool_empty_acquires_total"),
			"Acquisitions that found no idle connection and had to wait. A rising rate against a flat "+
				"pool_connections_max means the pool is undersized for the offered concurrency.",
			nil, nil),
		canceled: prometheus.NewDesc(name("pool_canceled_acquires_total"),
			"Acquisitions abandoned because the caller's context was cancelled or its deadline expired. "+
				"Non-zero means requests are timing out waiting for the database, not on it.",
			nil, nil),
		newConns: prometheus.NewDesc(name("pool_new_connections_total"),
			"Connections opened to the server. postgresql.conf logs every connect/disconnect and treats churn as a "+
				"defect signal: pools are meant to be long-lived, so a high rate here means one is being rebuilt.",
			nil, nil),
		destroyed: prometheus.NewDesc(name("pool_destroyed_connections_total"),
			"Pooled connections closed, by reason: max_lifetime (aged out, expected and jittered), "+
				"max_idle (idle too long, expected when traffic ebbs).",
			[]string{"reason"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *poolStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.connections
	ch <- c.maxConns
	ch <- c.acquireWait
	ch <- c.acquires
	ch <- c.emptyAcquires
	ch <- c.canceled
	ch <- c.newConns
	ch <- c.destroyed
}

// Collect implements prometheus.Collector. A nil stat function, or a nil
// snapshot from a closed pool, emits nothing — which shows up as a gap in the
// graph rather than as a fabricated zero.
func (c *poolStatsCollector) Collect(ch chan<- prometheus.Metric) {
	if c.stat == nil {
		return
	}
	s := c.stat()
	if s == nil {
		return
	}

	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
	}

	gauge(c.connections, float64(s.AcquiredConns()), "acquired")
	gauge(c.connections, float64(s.IdleConns()), "idle")
	gauge(c.connections, float64(s.ConstructingConns()), "constructing")
	gauge(c.maxConns, float64(s.MaxConns()))

	counter(c.acquireWait, s.AcquireDuration().Seconds())
	counter(c.acquires, float64(s.AcquireCount()))
	counter(c.emptyAcquires, float64(s.EmptyAcquireCount()))
	counter(c.canceled, float64(s.CanceledAcquireCount()))
	counter(c.newConns, float64(s.NewConnsCount()))
	counter(c.destroyed, float64(s.MaxLifetimeDestroyCount()), "max_lifetime")
	counter(c.destroyed, float64(s.MaxIdleDestroyCount()), "max_idle")
}

// -----------------------------------------------------------------------------
// pgx tracer: spans and per-statement metrics
// -----------------------------------------------------------------------------

// Span attribute keys, following OpenTelemetry semantic conventions for
// database client spans. They are written as literals rather than imported from
// a semconv/vN.NN.N package so that a semconv version bump cannot silently
// rename a series a Grafana panel or a Jaeger query depends on.
const (
	attrDBSystem     = "db.system"
	attrDBName       = "db.name"
	attrDBUser       = "db.user"
	attrDBStatement  = "db.statement"
	attrDBOperation  = "db.operation"
	attrDBStatusCode = "db.response.status_code"
	attrDBRows       = "db.rows_affected"
	attrServerAddr   = "server.address"
	attrServerPort   = "server.port"

	dbSystemPostgreSQL = "postgresql"
)

// tracerName identifies this instrumentation library in the trace. CLAUDE.md §9
// wants a single odds update followable from ingest through pricer to stream;
// this is the name under which the database hops in that trace appear.
const tracerName = "github.com/anpl1623/sharpline/internal/platform/postgres"

// queryTracer implements pgx.QueryTracer, pgx.BatchTracer and
// pgx.CopyFromTracer. pgx type-asserts the single ConnConfig.Tracer value for
// each of those interfaces, so one value covers Query/Exec (what sqlc emits),
// batches (sqlc's :batch* variants) and CopyFrom (the bulk path the price
// hypertable writer will want).
type queryTracer struct {
	tracer  trace.Tracer
	metrics *metrics
}

// traceState is what TraceXStart hands to TraceXEnd through the context.
type traceState struct {
	start     time.Time
	operation string
}

// traceStateKey is an unexported context key type, so nothing outside this
// package can collide with it or read it.
type traceStateKey struct{}

func (t *queryTracer) begin(ctx context.Context, conn *pgx.Conn, operation, statement string) context.Context {
	ctx, _ = t.tracer.Start(ctx, operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(connAttributes(conn)...),
		trace.WithAttributes(
			attribute.String(attrDBOperation, operation),
			// The statement only. pgx keeps arguments out of this string and
			// they are never added: arguments are user data.
			attribute.String(attrDBStatement, statement),
		),
	)
	return context.WithValue(ctx, traceStateKey{}, traceState{start: time.Now(), operation: operation})
}

// end closes the span, records the outcome on it, and observes the statement
// metrics. rows is negative when the operation reports no row count.
func (t *queryTracer) end(ctx context.Context, rows int64, err error) {
	span := trace.SpanFromContext(ctx)

	st, ok := ctx.Value(traceStateKey{}).(traceState)
	if !ok {
		// begin was not called on this context: nothing measured, nothing to
		// report. Never invent a duration.
		span.End()
		return
	}

	if rows >= 0 {
		span.SetAttributes(attribute.Int64(attrDBRows, rows))
	}
	if err != nil {
		if code := sqlStateOrClass(err); code != "" {
			span.SetAttributes(attribute.String(attrDBStatusCode, code))
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()

	t.metrics.observeQuery(st.operation, time.Since(st.start), err)
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *queryTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return t.begin(ctx, conn, operationName(data.SQL), data.SQL)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	t.end(ctx, rowsAffected(data.CommandTag, data.Err), data.Err)
}

// TraceBatchStart implements pgx.BatchTracer.
func (t *queryTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	n := 0
	if data.Batch != nil {
		n = data.Batch.Len()
	}
	ctx = t.begin(ctx, conn, operationBatch, operationBatch)
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("db.batch.size", n))
	return ctx
}

// TraceBatchQuery implements pgx.BatchTracer. It does not open a span per
// queued statement — a 500-statement batch would produce 500 spans and bury the
// trace — but it does count each statement, because a batch that fails needs
// the SQLSTATE of the statement that failed.
func (t *queryTracer) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	t.metrics.observeQuery(operationName(data.SQL), 0, data.Err)
}

// TraceBatchEnd implements pgx.BatchTracer.
func (t *queryTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	t.end(ctx, -1, data.Err)
}

// TraceCopyFromStart implements pgx.CopyFromTracer.
func (t *queryTracer) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	table := data.TableName.Sanitize()
	return t.begin(ctx, conn, operationCopyFrom+" "+table, operationCopyFrom+" "+table)
}

// TraceCopyFromEnd implements pgx.CopyFromTracer.
func (t *queryTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	t.end(ctx, rowsAffected(data.CommandTag, data.Err), data.Err)
}

// Compile-time proof that pgx will find all three tracer interfaces on this one
// value. pgx discovers them by type assertion, so a signature that drifts fails
// silently at runtime — it just stops tracing. These make it fail at build time.
var (
	_ pgx.QueryTracer    = (*queryTracer)(nil)
	_ pgx.BatchTracer    = (*queryTracer)(nil)
	_ pgx.CopyFromTracer = (*queryTracer)(nil)
)

func connAttributes(conn *pgx.Conn) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(attrDBSystem, dbSystemPostgreSQL)}
	if conn == nil {
		return attrs
	}
	cfg := conn.Config()
	if cfg == nil {
		return attrs
	}
	return append(attrs,
		attribute.String(attrDBName, cfg.Database),
		attribute.String(attrDBUser, cfg.User),
		attribute.String(attrServerAddr, cfg.Host),
		attribute.Int(attrServerPort, int(cfg.Port)),
	)
}

// rowsAffected returns -1 when there is no meaningful row count, so end can
// tell "zero rows" from "not reported".
func rowsAffected(tag pgconn.CommandTag, err error) int64 {
	if err != nil {
		return -1
	}
	return tag.RowsAffected()
}

// -----------------------------------------------------------------------------
// Operation labels
// -----------------------------------------------------------------------------

const (
	operationBatch    = "batch"
	operationCopyFrom = "copy_from"
	operationPing     = "ping"
	operationOther    = "other"

	// maxOperationLen caps a label value. sqlc query names are short; a cap
	// stops a malformed leading comment from producing a giant label.
	maxOperationLen = 64
)

// sqlVerbs is the allowlist that bounds the cardinality of the `operation`
// label for statements that are not sqlc-generated. Anything else becomes
// "other" rather than becoming a new series.
var sqlVerbs = map[string]string{
	"SELECT": "select", "INSERT": "insert", "UPDATE": "update", "DELETE": "delete",
	"WITH": "with", "COPY": "copy", "MERGE": "merge", "CALL": "call", "DO": "do",
	"BEGIN": "begin", "COMMIT": "commit", "ROLLBACK": "rollback",
	"SAVEPOINT": "savepoint", "RELEASE": "release", "SET": "set", "SHOW": "show",
	"EXPLAIN": "explain", "ANALYZE": "analyze", "VACUUM": "vacuum",
	"CREATE": "create", "ALTER": "alter", "DROP": "drop", "TRUNCATE": "truncate",
	"COMMENT": "comment", "GRANT": "grant", "REVOKE": "revoke", "REFRESH": "refresh",
	"LOCK": "lock", "LISTEN": "listen", "NOTIFY": "notify", "UNLISTEN": "unlisten",
	"PREPARE": "prepare", "EXECUTE": "execute", "DEALLOCATE": "deallocate",
	"DECLARE": "declare", "FETCH": "fetch", "CLOSE": "close", "MOVE": "move",
	"VALUES": "values", "TABLE": "table", "CHECKPOINT": "checkpoint",
	"REINDEX": "reindex", "CLUSTER": "cluster",
}

// operationName derives a BOUNDED metric label and span name from a statement.
//
// Preference order:
//
//  1. The sqlc query name. sqlc keeps `-- name: GetOpenWagers :many` as the
//     first line of every generated query string, which is exactly the
//     low-cardinality, human-meaningful identifier wanted here.
//  2. The leading SQL verb, from the allowlist above.
//  3. "other".
//
// Raw SQL is never a label value: every distinct statement would become a
// separate series and the cardinality is unbounded by construction.
func operationName(sql string) string {
	s := strings.TrimSpace(sql)

	// (1) sqlc's name comment, before comment stripping, since it IS a comment.
	if name, ok := sqlcQueryName(s); ok {
		return name
	}

	// Strip leading line and block comments — pgconn.Ping sends "-- ping", and
	// hand-written SQL is often preceded by a note.
	s = stripLeadingComments(s)
	if s == "" || s == ";" {
		// pgx spells a ping as the empty statement ";".
		return operationPing
	}

	// (2) leading verb.
	verb := s
	if i := strings.IndexFunc(verb, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '(' || r == ';'
	}); i > 0 {
		verb = verb[:i]
	}
	if op, ok := sqlVerbs[strings.ToUpper(verb)]; ok {
		return op
	}
	return operationOther
}

// sqlcQueryName extracts Foo from a leading "-- name: Foo :one".
func sqlcQueryName(s string) (string, bool) {
	const marker = "-- name:"
	if !strings.HasPrefix(s, marker) {
		return "", false
	}
	rest := strings.TrimSpace(s[len(marker):])
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		rest = rest[:i]
	}
	// Keep only identifier characters, so a mangled comment cannot inject a
	// label value with quotes or newlines in it.
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return -1
		}
	}, rest)
	if name == "" {
		return "", false
	}
	if len(name) > maxOperationLen {
		name = name[:maxOperationLen]
	}
	return name, true
}

// stripLeadingComments removes `-- line` and `/* block */` comments and the
// whitespace around them from the front of a statement.
func stripLeadingComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexAny(s, "\r\n"); i >= 0 {
				s = s[i+1:]
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = s[i+2:]
				continue
			}
			return ""
		default:
			return s
		}
	}
}

// -----------------------------------------------------------------------------
// Closed-pool guard
// -----------------------------------------------------------------------------

// closedFlag makes Stat() safe to call from a scrape that races Close().
type closedFlag struct{ v atomic.Bool }

func (f *closedFlag) set()        { f.v.Store(true) }
func (f *closedFlag) isSet() bool { return f.v.Load() }
