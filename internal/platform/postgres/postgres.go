// Package postgres is the Postgres access layer every Sharpline service shares:
// a constructor-injected pgx v5 connection pool, a transaction helper that makes
// the correct thing the easy thing, a readiness check that performs a real round
// trip, retry on transient connection failure only, Prometheus metrics and
// OpenTelemetry spans.
//
// # pgx, not database/sql
//
// Two independent reasons, both load-bearing.
//
// pgx is pure Go. CLAUDE.md's prime directive requires CGO_ENABLED=0 so the
// service binaries are static and the runtime image can be
// gcr.io/distroless/static:nonroot with no shell and no libc. This is the same
// constraint that puts franz-go in the charter instead of confluent-kafka-go.
// Anything binding libpq breaks the image, and breaking the image breaks the
// directive.
//
// pgx speaks the wire protocol directly, so Postgres types arrive as Postgres
// types. database/sql would flatten TimescaleDB hypertable rows, arrays,
// numerics and timestamptz through driver.Value, and this schema is full of
// values where that loses information — see migrations/00006, where the ledger's
// balance assertion sums NUMERIC precisely because BIGINT would overflow.
//
// # No repository interfaces here
//
// CLAUDE.md §12: "Interfaces are declared by the consumer, not the producer."
// This package exports a concrete pool and concrete helpers. The ingest, betting
// and settlement packages each declare the small interface they need, over the
// sqlc-generated queries or over pgx directly. Pool returns *pgxpool.Pool
// because that is what sqlc's generated DBTX parameter accepts; the pgx.Tx that
// InTx hands to a callback accepts it too, which is the entire point of the
// pattern.
//
// # Pool sizing — the arithmetic
//
// deploy/postgres/postgresql.conf sets max_connections = 100 with
// superuser_reserved_connections = 3, and says in as many words that raising it
// "means re-doing that arithmetic, not just editing this line". The work_mem
// budget in that file is computed against 100. So 100 is a hard ceiling and the
// arithmetic is here.
//
//	max_connections                                     100
//	- superuser_reserved_connections                     -3
//	                                                   ----
//	  available to application clients                   97
//
// Reserved out of that 97, and therefore not available to long-lived pools:
//
//	migrate (Job / compose one-shot, MaxConns 2)         -2   overlaps a rolling upgrade of api and settle
//	tools container: psql, goose, make migrate-status    -4
//	operator headroom: pg_dump, EXPLAIN, pg_stat_*       -4
//	                                                   ----
//	  budget for long-lived service pools                87
//
// Worst-case replica census. The real target is 2 OCPU / 12 GB (Oracle Ampere
// A1.Flex), and CLAUDE.md §9 puts an HPA on pricer and on stream, so "how many
// replicas at once" is not 1:
//
//	service | opens a pool?                      | worst-case replicas
//	--------+------------------------------------+--------------------
//	api     | yes (config.API RequirePostgres)   | 3  (2 steady + maxSurge during a rollout)
//	settle  | yes (config.Settle RequirePostgres)| 2  (1 steady + 1 during a rollout)
//	ingest  | yes (§3 "timescale writer")        | 2
//	pricer  | yes (reads reference lines)        | 4  (HPA ceiling on 2 OCPU)
//	stream  | NO — subscription state is Redis   | 0
//	        |                                    | ---
//	        | DB-touching replicas               | 11
//
//	87 budget / 11 replicas = 7.9 connections per replica, so per-pool MaxConns
//	must be <= 7 for the worst case to fit.
//
// DefaultMaxConns is 6, one below that ceiling:
//
//	11 replicas x 6                     = 66
//	+ migrate 2 + tools 4 + operator 4  = 76
//	                                    ----
//	  76 <= 97 available                    OK, 21 connections of headroom,
//	                                        which is another 3 replicas
//
// Six is not merely safe, it is already generous. The classic sizing heuristic —
// connections ~= (2 x cores) + effective_spindle_count — gives 2x2+1 = 5
// *usefully concurrent* connections against this server IN TOTAL, across every
// client. Postgres has no internal pooler: past that point extra connections buy
// context switches and lock contention, not throughput. A per-replica pool of 6
// exists to absorb burst concurrency and to keep a warm connection per goroutine
// batch, not to raise steady-state throughput. Oversizing a client pool is how a
// fast database is turned into a slow one.
//
// The number is configurable three ways, in this precedence:
//
//  1. Options.MaxConns — programmatic, wins over everything.
//  2. `pool_max_conns` in SHARPLINE_POSTGRES_DSN — pgxpool parses pool_* query
//     parameters natively, so per-service tuning needs no new environment
//     variable and no change to the typed config.
//  3. DefaultMaxConns.
//
// The invariant phase 10's Helm values must preserve:
//
//	sum over services of (maxReplicas x pool_max_conns) + 10 <= max_connections
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Sentinel errors. CLAUDE.md §12 puts domain sentinels in the domain package;
// these are platform-level, matched with errors.Is by callers and tests, and
// follow the precedent already set by internal/platform/config and
// internal/platform/httpx.
var (
	// ErrInvalidOptions means Options could not produce a usable pool. Returned
	// before any network I/O is attempted.
	ErrInvalidOptions = errors.New("postgres: invalid options")

	// ErrUnavailable means the database could not be reached within the
	// configured retry budget. Every attempt failed transiently.
	ErrUnavailable = errors.New("postgres: database unreachable")

	// ErrUnauthorized means the server rejected the credentials or the target
	// database does not exist. Never retried: no amount of waiting fixes a
	// wrong password.
	ErrUnauthorized = errors.New("postgres: server rejected the connection")

	// ErrClosed means the pool has been closed.
	ErrClosed = errors.New("postgres: pool is closed")
)

// Pool geometry defaults. See the package doc for the arithmetic behind
// DefaultMaxConns; the rest are explained at their use sites.
const (
	// DefaultMaxConns is the per-service ceiling derived in the package doc.
	DefaultMaxConns int32 = 6

	// DefaultMinConns keeps exactly one connection warm. Not zero, because a
	// cold pool pays TCP + TLS + SCRAM + session setup on the first request of
	// every idle period; not MaxConns, because 11 replicas holding 6 idle
	// connections each would reserve 66 of the server's 97 permanently.
	// postgresql.conf logs every connect and disconnect and calls churn a
	// defect signal — this is the setting that keeps that log quiet.
	DefaultMinConns int32 = 1

	// DefaultMaxConnLifetime bounds how long a connection may live, so a rolling
	// Postgres restart or a failover drains through natural expiry instead of
	// pinning every client to a backend that is going away.
	DefaultMaxConnLifetime = 30 * time.Minute

	// DefaultMaxConnLifetimeJitter spreads expiry. Without it every connection
	// opened during the same startup burst expires in the same second and the
	// pool reconnects in lockstep — a self-inflicted thundering herd.
	DefaultMaxConnLifetimeJitter = 5 * time.Minute

	// DefaultMaxConnIdleTime returns capacity to the server when traffic ebbs.
	DefaultMaxConnIdleTime = 5 * time.Minute

	// DefaultHealthCheckPeriod is how often pgx prunes dead connections and
	// tops the pool back up to MinConns.
	DefaultHealthCheckPeriod = time.Minute

	// DefaultConnectTimeout bounds ONE dial+handshake attempt, not the whole
	// retry budget.
	DefaultConnectTimeout = 5 * time.Second

	// DefaultPingTimeout bounds the readiness round trip. Deliberately below
	// httpx.DefaultReadinessTimeout (3s) so /readyz reports "database" as the
	// failing check instead of the whole probe timing out with no detail.
	DefaultPingTimeout = 2 * time.Second

	// DefaultRollbackTimeout bounds a rollback issued on a context that is
	// already cancelled. See tx.go.
	DefaultRollbackTimeout = 5 * time.Second
)

// Connect retry defaults. Worst case with these values is
// 0.25+0.5+1+2+4+5+5+5 = 22.75s of backoff plus up to 8x5s of dial timeout.
//
// The budget is sized for Kubernetes, not for compose. Compose gates api on
// `postgres` being service_healthy so the race barely exists there; a
// StatefulSet has no such gate, and a Postgres pod that is still replaying WAL
// answers 57P03 cannot_connect_now for as long as recovery takes.
const (
	// DefaultConnectAttempts counts the first try, so N attempts means N-1
	// retries.
	DefaultConnectAttempts = 8

	// DefaultConnectBackoff is the first sleep; it doubles per attempt.
	DefaultConnectBackoff = 250 * time.Millisecond

	// DefaultConnectBackoffMax caps the doubling.
	DefaultConnectBackoffMax = 5 * time.Second
)

// checkerName is the identifier this pool reports in the /readyz payload.
const checkerName = "postgres"

// Options configures the pool. Everything except DSN and Service has a
// documented default; nothing here is read from the process environment, because
// configuration is loaded once by internal/platform/config and injected
// (CLAUDE.md §12).
type Options struct {
	// DSN is SHARPLINE_POSTGRES_DSN. Both forms internal/platform/config
	// accepts work: the URL form and the libpq keyword/value form. pgxpool's
	// own pool_* parameters are honoured in either.
	DSN string

	// Service is the binary name. It becomes the connection's
	// application_name, which is the join key between a slow-query line in the
	// Postgres log and the slog line that caused it — deploy/postgres/postgresql.conf
	// puts `app=%a` in log_line_prefix precisely so this works.
	Service string

	// Logger receives lifecycle events. Required: a pool that cannot report its
	// own retries is a pool whose startup failures are invisible.
	Logger *slog.Logger

	// Registry is where metrics are registered. nil registers nothing, which is
	// correct for unit tests and for migrate, which serves no /metrics.
	// Services pass httpx.Server.Registry().
	Registry prometheus.Registerer

	// TracerProvider supplies the tracer for query spans. nil uses the OTel
	// global provider, which is a no-op until a cmd/ entrypoint installs a real
	// one — so an un-instrumented binary costs nothing and a traced one needs
	// no change here.
	TracerProvider trace.TracerProvider

	// Pool geometry. Zero means "the DSN's pool_* parameter if it has one,
	// otherwise the Default* constant".
	MaxConns              int32
	MinConns              int32
	MaxConnLifetime       time.Duration
	MaxConnLifetimeJitter time.Duration
	MaxConnIdleTime       time.Duration
	HealthCheckPeriod     time.Duration

	// ConnectTimeout bounds one dial and handshake.
	ConnectTimeout time.Duration

	// PingTimeout bounds the readiness round trip.
	PingTimeout time.Duration

	// RollbackTimeout bounds a rollback issued after the caller's context has
	// already been cancelled.
	RollbackTimeout time.Duration

	// ConnectAttempts, ConnectBackoff and ConnectBackoffMax bound startup
	// retries. Retries apply ONLY to establishing connectivity — see
	// IsTransientConnectError.
	ConnectAttempts   int
	ConnectBackoff    time.Duration
	ConnectBackoffMax time.Duration

	// StatementTimeout, when positive, sets a session statement_timeout as
	// defence in depth. It defaults to unset on purpose:
	// deploy/postgres/postgresql.conf sets statement_timeout = 0 server-side
	// and states why — "Serving-path timeouts belong on the client side, where
	// CLAUDE.md §12 already requires 'every external call has a timeout' via
	// context.Context". Setting this contradicts that decision, so it is opt-in.
	StatementTimeout time.Duration
}

// DB is a Postgres connection pool with instrumentation, a transaction helper
// and a readiness check. It is safe for concurrent use.
type DB struct {
	pool    *pgxpool.Pool
	log     *slog.Logger
	metrics *metrics
	closed  closedFlag

	pingTimeout     time.Duration
	rollbackTimeout time.Duration
}

// Connect builds the pool and proves it can reach the database before
// returning, retrying transient failures. A non-nil DB is connected; the caller
// does not have to probe it first.
//
// ctx bounds the whole startup sequence including every retry. Cancelling it
// aborts immediately.
func Connect(ctx context.Context, opts Options) (*DB, error) {
	cfg, err := buildConfig(opts)
	if err != nil {
		return nil, err
	}

	log := opts.Logger

	m, err := newMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}

	tp := opts.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	cfg.ConnConfig.Tracer = &queryTracer{
		tracer:  tp.Tracer(tracerName),
		metrics: m,
	}

	// The pool creates its MinConns in a background goroutine seeded with this
	// context. Detached from ctx so a startup deadline does not cancel the warm
	// pool a few milliseconds after Connect succeeds.
	pool, err := pgxpool.NewWithConfig(context.WithoutCancel(ctx), cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	db := &DB{
		pool:            pool,
		log:             log,
		metrics:         m,
		pingTimeout:     positiveOr(opts.PingTimeout, DefaultPingTimeout),
		rollbackTimeout: positiveOr(opts.RollbackTimeout, DefaultRollbackTimeout),
	}

	// Late-bind the stats source now that the pool exists. The collector reads
	// it through db.Stat, which is nil-safe once the pool is closed.
	if err := db.registerPoolStats(opts.Registry); err != nil {
		db.Close()
		return nil, err
	}

	// db.Close, not pool.Close, on both failure paths below. The collector is
	// already registered on the caller's registry by this point and a failed
	// Connect does not unregister it; setting the closed flag is what makes the
	// next scrape emit nothing instead of reporting the frozen statistics of a
	// pool that no longer exists.
	if err := db.awaitReady(ctx, opts); err != nil {
		db.Close()
		return nil, err
	}

	stat := pool.Stat()
	log.Info("postgres pool ready",
		slog.String("host", cfg.ConnConfig.Host),
		slog.Int("port", int(cfg.ConnConfig.Port)),
		slog.String("database", cfg.ConnConfig.Database),
		slog.String("user", cfg.ConnConfig.User),
		slog.String("application_name", cfg.ConnConfig.RuntimeParams["application_name"]),
		slog.Int("max_conns", int(stat.MaxConns())),
		slog.Int("min_conns", int(cfg.MinConns)),
		slog.String("max_conn_lifetime", cfg.MaxConnLifetime.String()),
		slog.String("max_conn_idle_time", cfg.MaxConnIdleTime.String()),
	)
	return db, nil
}

// registerPoolStats attaches the pool-statistics collector. It is separate from
// newMetrics only because the collector needs a pool that does not exist yet
// when the rest of the collectors are built.
func (db *DB) registerPoolStats(reg prometheus.Registerer) error {
	if reg == nil {
		return nil
	}
	c := newPoolStatsCollector(db.Stat)
	if err := reg.Register(c); err != nil {
		return fmt.Errorf("postgres: register pool stats collector: %w", err)
	}
	return nil
}

// buildConfig parses the DSN and resolves the pool geometry.
func buildConfig(opts Options) (*pgxpool.Config, error) {
	switch {
	case strings.TrimSpace(opts.DSN) == "":
		return nil, fmt.Errorf("%w: DSN is empty", ErrInvalidOptions)
	case opts.Service == "":
		return nil, fmt.Errorf("%w: Service is empty", ErrInvalidOptions)
	case opts.Logger == nil:
		return nil, fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	case opts.MaxConns < 0:
		return nil, fmt.Errorf("%w: MaxConns is %d", ErrInvalidOptions, opts.MaxConns)
	case opts.MinConns < 0:
		return nil, fmt.Errorf("%w: MinConns is %d", ErrInvalidOptions, opts.MinConns)
	}

	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		// The DSN may carry a password; never echo it. config.redactDSN does the
		// same for the same reason.
		return nil, fmt.Errorf("%w: DSN is not parseable by pgxpool: %w", ErrInvalidOptions, err)
	}

	// MaxConns: Options wins, then whatever the DSN said, then the default.
	// ParseConfig has already applied its own fallback (greater of 4 and
	// NumCPU), which is a laptop-shaped number and not the one derived in the
	// package doc — so it is only kept when the DSN asked for it explicitly.
	switch {
	case opts.MaxConns > 0:
		cfg.MaxConns = opts.MaxConns
	case dsnSetsPoolParam(opts.DSN, "pool_max_conns"):
		// keep ParseConfig's value
	default:
		cfg.MaxConns = DefaultMaxConns
	}

	switch {
	case opts.MinConns > 0:
		cfg.MinConns = opts.MinConns
	case dsnSetsPoolParam(opts.DSN, "pool_min_conns"):
		// keep ParseConfig's value
	default:
		cfg.MinConns = DefaultMinConns
	}

	if cfg.MinConns > cfg.MaxConns {
		return nil, fmt.Errorf("%w: MinConns %d exceeds MaxConns %d",
			ErrInvalidOptions, cfg.MinConns, cfg.MaxConns)
	}

	cfg.MaxConnLifetime = durationOr(opts.MaxConnLifetime,
		dsnSetsPoolParam(opts.DSN, "pool_max_conn_lifetime"), cfg.MaxConnLifetime, DefaultMaxConnLifetime)
	cfg.MaxConnLifetimeJitter = durationOr(opts.MaxConnLifetimeJitter,
		dsnSetsPoolParam(opts.DSN, "pool_max_conn_lifetime_jitter"), cfg.MaxConnLifetimeJitter, DefaultMaxConnLifetimeJitter)
	cfg.MaxConnIdleTime = durationOr(opts.MaxConnIdleTime,
		dsnSetsPoolParam(opts.DSN, "pool_max_conn_idle_time"), cfg.MaxConnIdleTime, DefaultMaxConnIdleTime)
	cfg.HealthCheckPeriod = durationOr(opts.HealthCheckPeriod,
		dsnSetsPoolParam(opts.DSN, "pool_health_check_period"), cfg.HealthCheckPeriod, DefaultHealthCheckPeriod)

	cfg.ConnConfig.ConnectTimeout = positiveOr(opts.ConnectTimeout, DefaultConnectTimeout)

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = make(map[string]string, 3)
	}
	// application_name and timezone are only defaulted, never overridden: a DSN
	// that states them is stating them deliberately.
	if cfg.ConnConfig.RuntimeParams["application_name"] == "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = opts.Service
	}
	if cfg.ConnConfig.RuntimeParams["timezone"] == "" {
		// The server is already UTC (postgresql.conf), and phase 12's
		// event-time joins depend on it staying that way end to end. Stating it
		// per session means a differently-configured server cannot silently
		// change the meaning of a timestamp.
		cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	}
	if opts.StatementTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] =
			fmt.Sprintf("%d", opts.StatementTimeout.Milliseconds())
	}

	return cfg, nil
}

// dsnSetsPoolParam reports whether the DSN mentions a pgxpool parameter. Both
// DSN forms put it in the same shape ("pool_max_conns=8" as a URL query
// parameter or as a keyword/value pair), so a substring test is sufficient and
// avoids re-implementing two DSN grammars.
func dsnSetsPoolParam(dsn, param string) bool {
	return strings.Contains(dsn, param+"=")
}

// awaitReady proves connectivity, retrying transient failures with exponential
// backoff and equal jitter.
//
// # What is retried, and why nothing else is
//
// Only the connectivity probe. It is idempotent by construction — it acquires a
// connection and executes the empty statement — so retrying it cannot apply
// anything twice.
//
// There is deliberately no exported retry helper in this package. No
// WithRetry(fn), no RetryTx, nothing that takes a caller's function and runs it
// again. A transaction that failed may or may not have committed: 08007
// transaction_resolution_unknown says so in the SQLSTATE, and a network drop
// between the client's COMMIT and the server's acknowledgement says it without
// one. Re-running a ledger write in that state posts the movement twice, and the
// schema's deferred balance trigger will not catch it because two balanced
// movements are individually balanced. The only safe retry of a business
// transaction is one the caller makes deliberately, with an idempotency key,
// knowing what it is re-running — which is why that decision is left with the
// betting and settlement packages and cannot be inherited by accident from here.
func (db *DB) awaitReady(ctx context.Context, opts Options) error {
	attempts := opts.ConnectAttempts
	if attempts <= 0 {
		attempts = DefaultConnectAttempts
	}
	backoff := positiveOr(opts.ConnectBackoff, DefaultConnectBackoff)
	backoffMax := positiveOr(opts.ConnectBackoffMax, DefaultConnectBackoffMax)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("postgres: connect aborted after %d attempt(s): %w", attempt-1,
				errors.Join(err, lastErr))
		}

		err := db.ping(ctx)
		if err == nil {
			db.metrics.observeConnectAttempt(connectOK)
			return nil
		}
		lastErr = err

		if !IsTransientConnectError(err) {
			db.metrics.observeConnectAttempt(connectFatal)
			db.log.Error("postgres unreachable and the failure is not transient; not retrying",
				slog.Int("attempt", attempt),
				slog.String("sqlstate", SQLState(err)),
				slog.String("error", err.Error()),
			)
			if isAuthOrCatalogError(err) {
				return fmt.Errorf("%w: %w", ErrUnauthorized, err)
			}
			return fmt.Errorf("postgres: connect: %w", err)
		}

		db.metrics.observeConnectAttempt(connectRetryable)

		if attempt == attempts {
			break
		}

		delay := jitter(backoffFor(backoff, backoffMax, attempt))
		db.log.Warn("postgres unreachable, retrying",
			slog.Int("attempt", attempt),
			slog.Int("of", attempts),
			slog.String("sqlstate", SQLState(err)),
			slog.String("retry_in", delay.String()),
			slog.String("error", err.Error()),
		)
		if err := sleep(ctx, delay); err != nil {
			return fmt.Errorf("postgres: connect aborted while backing off: %w",
				errors.Join(err, lastErr))
		}
	}

	return fmt.Errorf("%w after %d attempt(s): %w", ErrUnavailable, attempts, lastErr)
}

// backoffFor returns base * 2^(attempt-1), capped, without overflowing.
func backoffFor(base, max time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// jitter applies equal jitter: half the delay is fixed, half is random. Full
// jitter would sometimes return near-zero and hammer a server that is still
// starting; no jitter would make every replica of every service retry in the
// same millisecond.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// sleep waits for d or until ctx is done, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Pool exposes the underlying pgx pool.
//
// This is the value sqlc-generated code takes: `queries := db.New(pg.Pool())`.
// It is exported rather than wrapped because sqlc's generated DBTX parameter
// requires pgx's own Exec/Query/QueryRow signatures, and a wrapper that
// re-implemented them would only be measured on the paths that went through the
// wrapper. Instrumentation lives on the connection config instead, so every
// statement is counted no matter which handle issued it.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Stat returns a snapshot of pool statistics, or nil once the pool is closed.
// The metrics collector reads it at scrape time.
func (db *DB) Stat() *pgxpool.Stat {
	if db.closed.isSet() {
		return nil
	}
	return db.pool.Stat()
}

// Close releases every connection. It is idempotent.
func (db *DB) Close() {
	if db.closed.isSet() {
		return
	}
	db.closed.set()
	db.pool.Close()
	db.log.Info("postgres pool closed")
}

// Acquire checks out a connection for a caller that needs session state — a
// LISTEN, an advisory lock, a temporary table. The caller MUST Release it;
// prefer InTx or Pool() for anything that does not need session affinity.
func (db *DB) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if db.closed.isSet() {
		return nil, ErrClosed
	}
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: acquire connection: %w", err)
	}
	return conn, nil
}

// ping performs the readiness round trip and records it. It is the only
// operation awaitReady retries.
//
// # It deliberately does not produce a query span
//
// pgxpool.Pool.Ping acquires a pooled connection (so saturation is detected)
// and then calls pgx.Conn.Ping, which delegates straight to
// pgconn.PgConn.Ping — `pgConn.Exec(ctx, "-- ping")` at the protocol layer,
// below the pgx tracer. Verified against pgx v5.10.0, not assumed.
//
// The consequence is worth stating so nobody later hunts for the missing series:
// a readiness probe appears in sharpline_db_ping_duration_seconds and
// sharpline_db_up, and NOT in sharpline_db_query_duration_seconds. That is the
// behaviour to keep. A probe every few seconds on every replica would otherwise
// emit a span per probe, and diluting the trace stream with health checks works
// directly against CLAUDE.md §9's goal of following one odds update from ingest
// through pricer to stream.
func (db *DB) ping(ctx context.Context) error {
	if db.closed.isSet() {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, db.pingTimeout)
	defer cancel()

	start := time.Now()
	err := db.pool.Ping(ctx)
	db.metrics.observePing(time.Since(start), err)
	return err
}

// -----------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------

// SQLState returns the five-character SQLSTATE of a Postgres error, or "" if
// err did not come from the server. Useful for metric labels and for tests that
// assert on a specific constraint.
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Retryable SQLSTATEs, and the reason each one is here.
//
//	08000 connection_exception                              the connection failed
//	08001 sqlclient_unable_to_establish_sqlconnection        the dial did not complete
//	08003 connection_does_not_exist                          the session is gone; a new one will not be
//	08004 sqlserver_rejected_establishment_of_sqlconnection  the server declined this connection
//	08006 connection_failure                                 the connection dropped
//	57P01 admin_shutdown                                     the server was told to stop; it comes back
//	57P02 crash_shutdown                                     the server crashed; it comes back
//	57P03 cannot_connect_now                                 STARTING UP or in recovery. The single most
//	                                                         common one in this system: a StatefulSet
//	                                                         Postgres replaying WAL answers exactly this,
//	                                                         and Kubernetes offers no depends_on to wait
//	                                                         behind the way compose does.
//	53300 too_many_connections                               transient saturation. Also the symptom of a
//	                                                         pool-sizing mistake, which is why it is
//	                                                         counted separately in the metrics rather
//	                                                         than only retried.
//	53400 configuration_limit_exceeded                       as above.
//
// Explicitly NOT here:
//
//	08007 transaction_resolution_unknown  The commit's fate is unknown. Retrying
//	                                     may apply a ledger movement twice. This
//	                                     exclusion is the whole reason this list
//	                                     is enumerated rather than "class 08".
//	08P01 protocol_violation              A client/server mismatch or a bug.
//	                                     Waiting does not fix either.
//	28000 invalid_authorization_specification
//	28P01 invalid_password                Wrong credentials. Fail fast and loudly
//	                                     (CLAUDE.md §12) rather than retrying
//	                                     until a probe budget expires and the
//	                                     real cause is buried.
//	3D000 invalid_catalog_name            The database does not exist. That is a
//	                                     configuration error, not a race: the
//	                                     Postgres entrypoint creates the
//	                                     database before it accepts connections
//	                                     from the network.
//	40001 serialization_failure           Retryable in general — but at the
//	40P01 deadlock_detected               TRANSACTION level, by a caller that
//	                                     knows its transaction is idempotent.
//	                                     Never here. See IsSerializationFailure.
//	53100 disk_full                       Needs an operator, not a retry.
//	53200 out_of_memory
//	42xxx syntax/semantic errors          Bugs.
var retryableSQLStates = map[string]struct{}{
	"08000": {}, "08001": {}, "08003": {}, "08004": {}, "08006": {},
	"57P01": {}, "57P02": {}, "57P03": {},
	"53300": {}, "53400": {},
}

// authOrCatalogSQLStates are the fail-fast credential and target errors, split
// out so the caller gets ErrUnauthorized and not a generic connect failure.
var authOrCatalogSQLStates = map[string]struct{}{
	"28000": {}, "28P01": {}, "3D000": {},
}

// IsTransientConnectError reports whether err is a failure to establish or keep
// a connection that is expected to succeed on a later attempt.
//
// It answers ONE question — "should the connectivity probe be tried again?" — and
// it is the only retry predicate in this package's startup path. It deliberately
// does not report on business failures: a check violation, a unique violation, a
// serialization failure and an unknown transaction resolution all return false.
//
// Order matters. A server-side error is classified purely by its SQLSTATE, and
// that test runs first, because a rejected password arrives wrapped in a
// *pgconn.ConnectError whose outer shape is indistinguishable from a refused
// dial. Treating the wrapper as transient would retry a wrong password eight
// times and then report a timeout.
func IsTransientConnectError(err error) bool {
	if err == nil {
		return false
	}

	// A server-side error decides on its own SQLSTATE, whatever it is wrapped
	// in. errors.As walks the chain, so this also unwraps *pgconn.ConnectError.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		_, ok := retryableSQLStates[pgErr.Code]
		return ok
	}

	// The caller gave up. Not transient — respect the cancellation.
	if errors.Is(err, context.Canceled) {
		return false
	}

	// A deadline that expired is a dial or handshake that did not finish in
	// time. Per-attempt deadlines are how ConnectTimeout is spelled, and
	// awaitReady checks the parent context separately before each attempt, so
	// this cannot loop past a caller-imposed deadline.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// The pool itself is gone. Nothing to retry.
	//
	// puddle.ErrClosedPool is the sentinel pgxpool surfaces from its underlying
	// resource pool; it is matched by identity through its own exported value,
	// not reconstructed locally. An earlier revision declared a private
	// errors.New("closed pool") and compared against that, which can never
	// match — errors.Is compares identity, so two separately constructed errors
	// with the same text are different errors. That check was dead code.
	if errors.Is(err, ErrClosed) || errors.Is(err, puddle.ErrClosedPool) {
		return false
	}

	// Socket-level failures, before any statement was sent.
	for _, e := range []error{
		syscall.ECONNREFUSED, // nothing listening yet — the compose/k8s startup race
		syscall.ECONNRESET,   // connection torn down mid-handshake
		syscall.ECONNABORTED,
		syscall.EPIPE,        // wrote to a closed socket
		syscall.EHOSTUNREACH, // routing not converged
		syscall.ENETUNREACH,
		syscall.ENETDOWN,
		syscall.ETIMEDOUT,
	} {
		if errors.Is(err, e) {
			return true
		}
	}

	// Truncated handshake: the server accepted the TCP connection and went away
	// before completing startup. Seen every time Postgres is mid-init.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// Service DNS that has not propagated yet. In Kubernetes a Service name
	// resolves only once the pod has an endpoint, so this is a normal startup
	// state, not a misconfiguration — a permanently wrong hostname simply
	// exhausts the retry budget and then fails loudly.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	// Anything else the net package calls a timeout.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}

	// A *net.OpError with no recognised cause is still a socket failure.
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// isAuthOrCatalogError reports whether the server rejected the credentials or
// the named database does not exist.
func isAuthOrCatalogError(err error) bool {
	_, ok := authOrCatalogSQLStates[SQLState(err)]
	return ok
}

// IsSerializationFailure reports whether err is a serialization failure (40001)
// or a detected deadlock (40P01).
//
// It exists so a caller CAN retry, and this package deliberately does not do it
// for them. Both errors guarantee the transaction was rolled back, so re-running
// it is safe — but only the caller knows whether re-running its own function is
// safe, because the function may have sent an email, produced to Kafka or
// mutated in-memory state between the failing statement and the return. A retry
// loop hidden in the platform layer would re-run all of that invisibly.
func IsSerializationFailure(err error) bool {
	switch SQLState(err) {
	case "40001", "40P01":
		return true
	default:
		return false
	}
}

// IsUniqueViolation reports a unique or primary key violation (23505). The
// expected, non-exceptional answer when an idempotency key has already been
// used.
func IsUniqueViolation(err error) bool { return SQLState(err) == "23505" }

// IsCheckViolation reports a CHECK constraint failure (23514).
//
// This schema uses it for two very different things and the caller must
// distinguish them by message, not by code:
//
//   - every enum column, which is TEXT + a named CHECK rather than a native
//     enum type (phase 2a chose that for reversibility — a native enum has no
//     DROP VALUE);
//   - the deferred double-entry ledger assertion in migrations/00006, which
//     RAISEs with ERRCODE = 'check_violation' when a movement does not sum to
//     zero. That one surfaces from COMMIT, not from the INSERT — see tx.go.
func IsCheckViolation(err error) bool { return SQLState(err) == "23514" }

// IsForeignKeyViolation reports a foreign key violation (23503).
func IsForeignKeyViolation(err error) bool { return SQLState(err) == "23503" }

// IsNotNullViolation reports a NOT NULL violation (23502).
func IsNotNullViolation(err error) bool { return SQLState(err) == "23502" }

// sqlStateOrClass returns a bounded metric label for an error: the SQLSTATE when
// the server produced one, otherwise a small fixed set of client-side causes.
// Never the error text — that is unbounded.
func sqlStateOrClass(err error) string {
	if err == nil {
		return ""
	}
	if code := SQLState(err); code != "" {
		return code
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, pgx.ErrNoRows):
		return "no_rows"
	case errors.Is(err, pgx.ErrTxClosed):
		return "tx_closed"
	case errors.Is(err, ErrClosed):
		return "pool_closed"
	case IsTransientConnectError(err):
		return "connection"
	default:
		return "unknown"
	}
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// positiveOr resolves a non-positive duration to its default.
func positiveOr(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

// durationOr implements the three-way precedence used for pool geometry:
// an explicit option, then a DSN parameter, then the package default.
func durationOr(opt time.Duration, dsnSet bool, parsed, fallback time.Duration) time.Duration {
	switch {
	case opt > 0:
		return opt
	case dsnSet:
		return parsed
	default:
		return fallback
	}
}
