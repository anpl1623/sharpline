package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// The engine under test is the SAME image and the SAME digest as the compose
// stack's `postgres` service, copied verbatim from the contract ledger. A stock
// postgres image has no TimescaleDB, and testing the access layer against a
// different engine than the one it will run against defeats the point of using a
// real database at all (CLAUDE.md §10: "no mocked databases").
const postgresImage = "timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6"

const (
	testUser     = "sharpline_test"
	testPassword = "sharpline_test_password"
	testDatabase = "sharpline_test"
)

// testDSN is populated by TestMain. containerErr records why it is not, so an
// integration test fails with the real cause instead of a nil-pointer panic.
var (
	testDSN      string
	containerErr error
)

// TestMain starts one Postgres container for the whole package and creates the
// test-owned fixture schema in it.
//
// One container, not one per test: startup dominates the runtime, and every test
// below isolates itself with unique row keys rather than by needing a fresh
// server.
//
// NOTE ON FIXTURES AND THE NO-MOCK-DATA RULE: the tables created here are
// created BY the tests, FOR the tests, and every row in them is inserted and
// asserted on by the test that inserted it. Nothing here is shipped, seeded into
// the real database, or read by application code. What it deliberately is NOT is
// a stand-in for the real schema — it is a minimal replica of ONE property of
// migrations/00006, the DEFERRABLE INITIALLY DEFERRED balance assertion, because
// that property is what tx.go exists to handle correctly. An end-to-end test
// against the full migrated schema belongs in test/ with the migration runner.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	terminate, err := startPostgres(ctx)
	if err != nil {
		containerErr = err
	}

	code := m.Run()

	if terminate != nil {
		terminate()
	}
	os.Exit(code)
}

func startPostgres(ctx context.Context) (func(), error) {
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":             testUser,
				"POSTGRES_PASSWORD":         testPassword,
				"POSTGRES_DB":               testDatabase,
				"TIMESCALEDB_TELEMETRY":     "off",
				"NO_TS_TUNE":                "true",
				"POSTGRES_HOST_AUTH_METHOD": "scram-sha-256",
			},
			WaitingFor: wait.ForAll(
				// Twice: the entrypoint starts the server once to run initdb
				// scripts and once for real.
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithDeadline(4 * time.Minute),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}
	terminate := func() {
		_ = container.Terminate(context.Background())
	}

	host, err := container.Host(ctx)
	if err != nil {
		terminate()
		return nil, fmt.Errorf("container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		terminate()
		return nil, fmt.Errorf("container port: %w", err)
	}

	testDSN = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		testUser, testPassword, host, port.Port(), testDatabase)

	if err := createFixture(ctx, testDSN); err != nil {
		terminate()
		return nil, err
	}
	return terminate, nil
}

// fixtureDDL mirrors the ONE property of migrations/00006 that tx.go must handle:
// a two-row movement whose amounts must sum to zero, asserted by a
// DEFERRABLE INITIALLY DEFERRED constraint trigger raising SQLSTATE 23514.
//
// Same deferral, same ERRCODE, same "at least two entries summing to exactly
// zero" rule as ledger_assert_transaction_balanced(). Owned by this test file.
const fixtureDDL = `
CREATE TABLE tx_movements (
    id text PRIMARY KEY
);

CREATE TABLE tx_entries (
    id           bigserial PRIMARY KEY,
    movement_id  text   NOT NULL REFERENCES tx_movements(id),
    amount_minor bigint NOT NULL
);

CREATE FUNCTION tx_assert_balanced() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
    total NUMERIC;
    n     INTEGER;
BEGIN
    SELECT coalesce(sum(amount_minor), 0), count(*)
      INTO total, n
      FROM tx_entries
     WHERE movement_id = NEW.movement_id;

    IF n < 2 THEN
        RAISE EXCEPTION 'movement % has % entr(ies); a balanced movement has at least two',
            NEW.movement_id, n
            USING ERRCODE = 'check_violation';
    END IF;

    IF total <> 0 THEN
        RAISE EXCEPTION 'movement % is UNBALANCED: % entries summing to % minor units',
            NEW.movement_id, n, total
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END
$fn$;

CREATE CONSTRAINT TRIGGER tx_entries_balanced_at_commit
    AFTER INSERT ON tx_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION tx_assert_balanced();
`

func createFixture(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to create fixture: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, fixtureDDL); err != nil {
		return fmt.Errorf("create fixture schema: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func requireContainer(t *testing.T) string {
	t.Helper()
	if containerErr != nil {
		t.Fatalf("postgres test container unavailable: %v\n"+
			"These are integration tests and they do not skip. Run them the way the "+
			"charter requires: `make test PKG=./internal/platform/postgres/...`, which "+
			"mounts the Docker socket for testcontainers.", containerErr)
	}
	return testDSN
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	// Discard by default: a passing test should be silent. Swap io.Discard for
	// os.Stderr while triaging.
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newTestDB connects a pool against the shared container. mutate lets a test
// adjust Options before Connect runs.
func newTestDB(t *testing.T, mutate func(*Options)) (*DB, *prometheus.Registry) {
	t.Helper()
	dsn := requireContainer(t)
	reg := prometheus.NewRegistry()

	opts := Options{
		DSN:      dsn,
		Service:  "postgres-test",
		Logger:   testLogger(t),
		Registry: reg,
	}
	if mutate != nil {
		mutate(&opts)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := Connect(ctx, opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db, reg
}

// -----------------------------------------------------------------------------
// Pool sizing — the arithmetic in the package doc, asserted
// -----------------------------------------------------------------------------

// TestDefaultMaxConnsFitsTheServerBudget mechanically checks the arithmetic
// written in the package doc. If someone raises DefaultMaxConns, or
// postgresql.conf lowers max_connections, this fails and the doc has to be
// re-derived rather than quietly becoming fiction.
func TestDefaultMaxConnsFitsTheServerBudget(t *testing.T) {
	const (
		// deploy/postgres/postgresql.conf.
		maxConnections               = 100
		superuserReservedConnections = 3

		// Reserved out of the remainder — see the package doc.
		migrateJobConns  = 2
		toolsConns       = 4
		operatorHeadroom = 4

		// Worst-case DB-touching replica census at the phase-10 HPA maxima on
		// 2 OCPU / 12 GB: api 3 + settle 2 + ingest 2 + pricer 4, stream 0.
		worstCaseReplicas = 3 + 2 + 2 + 4
	)

	available := maxConnections - superuserReservedConnections
	reserved := migrateJobConns + toolsConns + operatorHeadroom
	poolBudget := available - reserved

	demand := worstCaseReplicas * int(DefaultMaxConns)

	if demand > poolBudget {
		t.Fatalf("worst-case pool demand %d (%d replicas x DefaultMaxConns %d) exceeds the "+
			"%d connections left after reservations; the package doc's arithmetic no longer holds",
			demand, worstCaseReplicas, DefaultMaxConns, poolBudget)
	}
	if total := demand + reserved; total > available {
		t.Fatalf("total demand %d exceeds the %d connections available to non-superusers", total, available)
	}
	if DefaultMinConns > DefaultMaxConns {
		t.Fatalf("DefaultMinConns %d exceeds DefaultMaxConns %d", DefaultMinConns, DefaultMaxConns)
	}
	// A pool that pins its whole ceiling idle on every replica defeats the
	// budget: 11 replicas x MaxConns would be permanently allocated.
	if DefaultMinConns > 1 {
		t.Fatalf("DefaultMinConns is %d; anything above 1 reserves %d connections per replica "+
			"permanently and the census above stops being worst-case",
			DefaultMinConns, DefaultMinConns)
	}
}

func TestBuildConfigPoolGeometryPrecedence(t *testing.T) {
	base := "postgres://u:p@h:5432/d?sslmode=disable"

	t.Run("default when neither option nor DSN says", func(t *testing.T) {
		cfg, err := buildConfig(Options{DSN: base, Service: "api", Logger: testLogger(t)})
		if err != nil {
			t.Fatalf("buildConfig: %v", err)
		}
		if cfg.MaxConns != DefaultMaxConns {
			t.Errorf("MaxConns = %d, want DefaultMaxConns %d", cfg.MaxConns, DefaultMaxConns)
		}
		if cfg.MinConns != DefaultMinConns {
			t.Errorf("MinConns = %d, want DefaultMinConns %d", cfg.MinConns, DefaultMinConns)
		}
		if cfg.MaxConnLifetime != DefaultMaxConnLifetime {
			t.Errorf("MaxConnLifetime = %s, want %s", cfg.MaxConnLifetime, DefaultMaxConnLifetime)
		}
		if cfg.MaxConnLifetimeJitter != DefaultMaxConnLifetimeJitter {
			t.Errorf("MaxConnLifetimeJitter = %s, want %s", cfg.MaxConnLifetimeJitter, DefaultMaxConnLifetimeJitter)
		}
	})

	t.Run("DSN pool_max_conns is honoured", func(t *testing.T) {
		cfg, err := buildConfig(Options{
			DSN:     base + "&pool_max_conns=17&pool_min_conns=3",
			Service: "api",
			Logger:  testLogger(t),
		})
		if err != nil {
			t.Fatalf("buildConfig: %v", err)
		}
		if cfg.MaxConns != 17 {
			t.Errorf("MaxConns = %d, want 17 from the DSN", cfg.MaxConns)
		}
		if cfg.MinConns != 3 {
			t.Errorf("MinConns = %d, want 3 from the DSN", cfg.MinConns)
		}
	})

	t.Run("Options wins over the DSN", func(t *testing.T) {
		cfg, err := buildConfig(Options{
			DSN:      base + "&pool_max_conns=17",
			Service:  "api",
			Logger:   testLogger(t),
			MaxConns: 3,
		})
		if err != nil {
			t.Fatalf("buildConfig: %v", err)
		}
		if cfg.MaxConns != 3 {
			t.Errorf("MaxConns = %d, want 3 from Options", cfg.MaxConns)
		}
	})

	t.Run("keyword/value DSN form is supported", func(t *testing.T) {
		cfg, err := buildConfig(Options{
			DSN:     "host=h port=5432 user=u password=p dbname=d sslmode=disable pool_max_conns=9",
			Service: "settle",
			Logger:  testLogger(t),
		})
		if err != nil {
			t.Fatalf("buildConfig: %v", err)
		}
		if cfg.MaxConns != 9 {
			t.Errorf("MaxConns = %d, want 9 from the keyword/value DSN", cfg.MaxConns)
		}
	})

	t.Run("application_name defaults to the service", func(t *testing.T) {
		cfg, err := buildConfig(Options{DSN: base, Service: "ingest", Logger: testLogger(t)})
		if err != nil {
			t.Fatalf("buildConfig: %v", err)
		}
		if got := cfg.ConnConfig.RuntimeParams["application_name"]; got != "ingest" {
			t.Errorf("application_name = %q, want %q; postgresql.conf's log_line_prefix "+
				"uses app=%%a as the join key between a slow-query line and a slog line", got, "ingest")
		}
		if got := cfg.ConnConfig.RuntimeParams["timezone"]; got != "UTC" {
			t.Errorf("timezone = %q, want UTC", got)
		}
		if _, ok := cfg.ConnConfig.RuntimeParams["statement_timeout"]; ok {
			t.Error("statement_timeout was set; postgresql.conf delegates serving-path " +
				"timeouts to context.Context and this must stay opt-in")
		}
	})

	t.Run("explicit statement timeout is milliseconds", func(t *testing.T) {
		cfg, err := buildConfig(Options{
			DSN: base, Service: "api", Logger: testLogger(t),
			StatementTimeout: 1500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("buildConfig: %v", err)
		}
		if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "1500" {
			t.Errorf("statement_timeout = %q, want %q", got, "1500")
		}
	})
}

func TestBuildConfigRejectsBadOptions(t *testing.T) {
	good := "postgres://u:p@h:5432/d"
	tests := []struct {
		name string
		opts Options
	}{
		{"empty DSN", Options{Service: "api", Logger: slog.Default()}},
		{"blank DSN", Options{DSN: "   ", Service: "api", Logger: slog.Default()}},
		{"no service", Options{DSN: good, Logger: slog.Default()}},
		{"no logger", Options{DSN: good, Service: "api"}},
		{"negative MaxConns", Options{DSN: good, Service: "api", Logger: slog.Default(), MaxConns: -1}},
		{"negative MinConns", Options{DSN: good, Service: "api", Logger: slog.Default(), MinConns: -1}},
		{"MinConns above MaxConns", Options{DSN: good, Service: "api", Logger: slog.Default(), MaxConns: 2, MinConns: 5}},
		{"unparseable DSN", Options{DSN: "postgres://u:p@h:notaport/d", Service: "api", Logger: slog.Default()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildConfig(tc.opts)
			if !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("err = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

// TestBuildConfigDoesNotEchoThePassword guards the same property
// config.redactDSN guards: a bad DSN must not put the credential in a log line
// or an error string.
func TestBuildConfigDoesNotEchoThePassword(t *testing.T) {
	const secret = "sup3r-s3cret-p4ssword"
	_, err := buildConfig(Options{
		DSN:     "postgres://u:" + secret + "@h:notaport/d",
		Service: "api",
		Logger:  slog.Default(),
	})
	if err == nil {
		t.Fatal("expected an error for an unparseable DSN")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error text contains the password: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Retry classification
// -----------------------------------------------------------------------------

func TestIsTransientConnectError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// Retryable SQLSTATEs.
		{"57P03 cannot_connect_now", &pgconn.PgError{Code: "57P03"}, true},
		{"57P01 admin_shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"57P02 crash_shutdown", &pgconn.PgError{Code: "57P02"}, true},
		{"08000 connection_exception", &pgconn.PgError{Code: "08000"}, true},
		{"08001 cannot establish", &pgconn.PgError{Code: "08001"}, true},
		{"08003 does not exist", &pgconn.PgError{Code: "08003"}, true},
		{"08004 rejected", &pgconn.PgError{Code: "08004"}, true},
		{"08006 connection_failure", &pgconn.PgError{Code: "08006"}, true},
		{"53300 too_many_connections", &pgconn.PgError{Code: "53300"}, true},
		{"53400 configuration_limit_exceeded", &pgconn.PgError{Code: "53400"}, true},
		{"wrapped retryable SQLSTATE", fmt.Errorf("dial: %w", &pgconn.PgError{Code: "57P03"}), true},

		// THE exclusion that matters: an unknown transaction resolution must
		// never be retried, because retrying it can post a ledger movement
		// twice.
		{"08007 transaction_resolution_unknown", &pgconn.PgError{Code: "08007"}, false},

		// Fail fast: no amount of waiting fixes these.
		{"28P01 invalid_password", &pgconn.PgError{Code: "28P01"}, false},
		{"28000 invalid_authorization", &pgconn.PgError{Code: "28000"}, false},
		{"3D000 invalid_catalog_name", &pgconn.PgError{Code: "3D000"}, false},
		{"08P01 protocol_violation", &pgconn.PgError{Code: "08P01"}, false},
		{"53100 disk_full", &pgconn.PgError{Code: "53100"}, false},
		{"53200 out_of_memory", &pgconn.PgError{Code: "53200"}, false},

		// Business failures are never connection failures.
		{"23514 check_violation (the ledger constraint)", &pgconn.PgError{Code: "23514"}, false},
		{"23505 unique_violation", &pgconn.PgError{Code: "23505"}, false},
		{"23503 foreign_key_violation", &pgconn.PgError{Code: "23503"}, false},
		{"40001 serialization_failure", &pgconn.PgError{Code: "40001"}, false},
		{"40P01 deadlock_detected", &pgconn.PgError{Code: "40P01"}, false},
		{"42601 syntax_error", &pgconn.PgError{Code: "42601"}, false},
		{"P0001 raise_exception", &pgconn.PgError{Code: "P0001"}, false},

		// Socket level.
		{"ECONNREFUSED", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"ECONNRESET", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"EHOSTUNREACH", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, true},
		{"ETIMEDOUT", &net.OpError{Op: "dial", Err: syscall.ETIMEDOUT}, true},
		{"EPIPE", syscall.EPIPE, true},
		{"unexpected EOF mid-handshake", io.ErrUnexpectedEOF, true},
		{"EOF", io.EOF, true},
		{"DNS not resolving yet", &net.DNSError{Err: "no such host", Name: "postgres", IsNotFound: true}, true},

		// Client-side.
		{"deadline exceeded is a dial that did not finish", context.DeadlineExceeded, true},
		{"canceled means the caller gave up", context.Canceled, false},
		{"closed pool", ErrClosed, false},
		{"nil", nil, false},
		{"unrelated error", errors.New("something else entirely"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientConnectError(tc.err); got != tc.want {
				t.Fatalf("IsTransientConnectError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorPredicates(t *testing.T) {
	tests := []struct {
		name  string
		code  string
		check func(error) bool
		want  bool
	}{
		{"unique", "23505", IsUniqueViolation, true},
		{"not unique", "23514", IsUniqueViolation, false},
		{"check", "23514", IsCheckViolation, true},
		{"not check", "23505", IsCheckViolation, false},
		{"fk", "23503", IsForeignKeyViolation, true},
		{"not null", "23502", IsNotNullViolation, true},
		{"serialization", "40001", IsSerializationFailure, true},
		{"deadlock", "40P01", IsSerializationFailure, true},
		{"not serialization", "23514", IsSerializationFailure, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: tc.code})
			if got := tc.check(err); got != tc.want {
				t.Fatalf("predicate on %s = %v, want %v", tc.code, got, tc.want)
			}
		})
	}

	if got := SQLState(errors.New("not from postgres")); got != "" {
		t.Errorf("SQLState of a non-pg error = %q, want empty", got)
	}
	if got := SQLState(fmt.Errorf("x: %w", &pgconn.PgError{Code: "23514"})); got != "23514" {
		t.Errorf("SQLState = %q, want 23514", got)
	}
}

func TestSQLStateOrClassIsBounded(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&pgconn.PgError{Code: "23514"}, "23514"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{pgx.ErrNoRows, "no_rows"},
		{pgx.ErrTxClosed, "tx_closed"},
		{ErrClosed, "pool_closed"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connection"},
		{errors.New("a very long and entirely unique message 12345"), "unknown"},
		{nil, ""},
	}
	for _, tc := range tests {
		if got := sqlStateOrClass(tc.err); got != tc.want {
			t.Errorf("sqlStateOrClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestBackoffFor(t *testing.T) {
	base := 250 * time.Millisecond
	max := 5 * time.Second
	want := []time.Duration{
		250 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		5 * time.Second,
		5 * time.Second,
		5 * time.Second,
	}
	for i, w := range want {
		attempt := i + 1
		if got := backoffFor(base, max, attempt); got != w {
			t.Errorf("backoffFor(attempt %d) = %s, want %s", attempt, got, w)
		}
	}

	// Must not overflow or loop forever on an absurd attempt count.
	if got := backoffFor(base, max, 200); got != max {
		t.Errorf("backoffFor(attempt 200) = %s, want the cap %s", got, max)
	}
}

func TestJitterStaysInTheUpperHalf(t *testing.T) {
	d := time.Second
	for range 500 {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%s) = %s, want within [%s, %s]", d, got, d/2, d)
		}
	}
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %s, want 0", got)
	}
}

func TestSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("sleep took %s on a cancelled context", elapsed)
	}
}

// -----------------------------------------------------------------------------
// Operation labels
// -----------------------------------------------------------------------------

func TestOperationNameIsBounded(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			"sqlc query name",
			"-- name: GetOpenWagers :many\nSELECT id FROM wagers WHERE status = $1",
			"GetOpenWagers",
		},
		{"sqlc one", "-- name: InsertPrice :one\nINSERT INTO prices ...", "InsertPrice"},
		{"sqlc exec", "-- name: SuspendMarket :exec\nUPDATE markets SET ...", "SuspendMarket"},
		{"pgx ping", ";", operationPing},
		{"pgconn ping comment", "-- ping", operationPing},
		{"plain select", "SELECT 1", "select"},
		{"lowercase insert", "insert into t values (1)", "insert"},
		{"leading whitespace and newline", "\n\t  UPDATE markets SET x = 1", "update"},
		{"CTE", "WITH recent AS (SELECT 1) SELECT * FROM recent", "with"},
		{"copy", "COPY prices FROM STDIN", "copy"},
		{"leading comment then verb", "/* pool warmup */ SELECT 1", "select"},
		{"line comment then verb", "-- warm the plan cache\nSELECT 1", "select"},
		{"unknown verb becomes other", "FLOOBLE the widgets", operationOther},
		{"empty", "", operationPing},
		{"paren-prefixed select", "(SELECT 1)", operationOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := operationName(tc.sql); got != tc.want {
				t.Fatalf("operationName(%q) = %q, want %q", tc.sql, got, tc.want)
			}
		})
	}
}

// TestOperationNameNeverReturnsRawSQL is the cardinality guard. A label value
// derived from arbitrary SQL would create one Prometheus series per distinct
// statement, which is unbounded by construction.
func TestOperationNameNeverReturnsRawSQL(t *testing.T) {
	inputs := []string{
		"SELECT * FROM prices WHERE market_id = 'abc-123' AND observed_at > now()",
		"-- name: " + strings.Repeat("A", 500) + " :one\nSELECT 1",
		"-- name: Has-Dashes And Spaces :one\nSELECT 1",
		"-- name:\nSELECT 1",
		"\x00\x01binary garbage",
		strings.Repeat("SELECT 1; ", 100),
	}
	for _, sql := range inputs {
		got := operationName(sql)
		if len(got) > maxOperationLen {
			t.Errorf("operationName(%.30q...) is %d chars, over the %d cap", sql, len(got), maxOperationLen)
		}
		if strings.ContainsAny(got, " \t\r\n\"'\x00") {
			t.Errorf("operationName(%.30q...) = %q contains characters that do not belong in a label", sql, got)
		}
	}
}

// -----------------------------------------------------------------------------
// Connect: retry versus fail fast
// -----------------------------------------------------------------------------

func TestConnectFailsFastOnBadCredentials(t *testing.T) {
	dsn := requireContainer(t)
	bad := strings.Replace(dsn, testPassword, "definitely-not-the-password", 1)
	if bad == dsn {
		t.Fatal("failed to construct a bad-password DSN")
	}

	reg := prometheus.NewRegistry()
	start := time.Now()
	db, err := Connect(context.Background(), Options{
		DSN:      bad,
		Service:  "postgres-test",
		Logger:   testLogger(t),
		Registry: reg,
		// Generous budget on purpose: if a wrong password were treated as
		// transient, this test would take ~23s and the assertion below would
		// catch it.
		ConnectAttempts: DefaultConnectAttempts,
	})
	elapsed := time.Since(start)

	if err == nil {
		db.Close()
		t.Fatal("Connect succeeded with a wrong password")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v; a rejected password must not be reported as unavailability", err)
	}
	if got := SQLState(err); got != "28P01" {
		t.Errorf("SQLState = %q, want 28P01 invalid_password", got)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s; a non-transient failure must not consume the retry budget", elapsed)
	}

	if got := metricValue(t, reg, "sharpline_db_connect_attempts_total", map[string]string{"outcome": connectFatal}); got != 1 {
		t.Errorf("connect_attempts_total{outcome=fatal} = %v, want 1", got)
	}
	if got := metricValue(t, reg, "sharpline_db_connect_attempts_total", map[string]string{"outcome": connectRetryable}); got != 0 {
		t.Errorf("connect_attempts_total{outcome=retryable} = %v, want 0", got)
	}
}

func TestConnectRetriesThenReportsUnavailable(t *testing.T) {
	requireContainer(t)

	reg := prometheus.NewRegistry()
	const attempts = 3

	// Port 1 on the loopback of whatever container this test runs in: nothing
	// listens there, so every attempt is a fast ECONNREFUSED — a real transient
	// connection failure, not a simulated one.
	_, err := Connect(context.Background(), Options{
		DSN:               "postgres://u:p@127.0.0.1:1/d?sslmode=disable",
		Service:           "postgres-test",
		Logger:            testLogger(t),
		Registry:          reg,
		ConnectAttempts:   attempts,
		ConnectBackoff:    5 * time.Millisecond,
		ConnectBackoffMax: 10 * time.Millisecond,
		ConnectTimeout:    2 * time.Second,
	})
	if err == nil {
		t.Fatal("Connect succeeded against a port with nothing listening")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if got := metricValue(t, reg, "sharpline_db_connect_attempts_total", map[string]string{"outcome": connectRetryable}); got != attempts {
		t.Errorf("connect_attempts_total{outcome=retryable} = %v, want %d", got, attempts)
	}
	if got := metricValue(t, reg, "sharpline_db_up", nil); got != 0 {
		t.Errorf("sharpline_db_up = %v, want 0", got)
	}
}

func TestConnectAbortsOnCancelledContext(t *testing.T) {
	requireContainer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Connect(ctx, Options{
		DSN:     "postgres://u:p@127.0.0.1:1/d?sslmode=disable",
		Service: "postgres-test",
		Logger:  testLogger(t),
	})
	if err == nil {
		t.Fatal("Connect succeeded on a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestConnectAppliesResolvedGeometry(t *testing.T) {
	db, _ := newTestDB(t, func(o *Options) { o.MaxConns = 3 })

	stat := db.Stat()
	if stat == nil {
		t.Fatal("Stat returned nil on a live pool")
	}
	if stat.MaxConns() != 3 {
		t.Errorf("MaxConns = %d, want 3", stat.MaxConns())
	}
}

// -----------------------------------------------------------------------------
// Readiness
// -----------------------------------------------------------------------------

func TestReadinessIsARealRoundTrip(t *testing.T) {
	db, reg := newTestDB(t, nil)

	if got := db.Name(); got != checkerName {
		t.Errorf("Name() = %q, want %q", got, checkerName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.Check(ctx); err != nil {
		t.Fatalf("Check on a live database: %v", err)
	}
	if got := metricValue(t, reg, "sharpline_db_up", nil); got != 1 {
		t.Errorf("sharpline_db_up = %v after a successful check, want 1", got)
	}

	// A real round trip, measured. Connect's own probe plus this Check means at
	// least two observations.
	if got := histogramCount(t, reg, "sharpline_db_ping_duration_seconds", nil); got < 2 {
		t.Errorf("ping_duration_seconds count = %d, want at least 2 (Connect's probe plus this Check); "+
			"readiness is not performing a round trip", got)
	}

	// And it does NOT appear in the query metrics, because pgxpool.Pool.Ping
	// delegates to pgconn.PgConn.Ping, which executes "-- ping" below the pgx
	// tracer. Pinned here so the absence is a documented property rather than
	// something a future reader mistakes for broken instrumentation — and so
	// that if pgx ever routes Ping through the tracer, this test says so instead
	// of a spans-per-probe regression landing silently.
	if got := histogramCount(t, reg, "sharpline_db_query_duration_seconds",
		map[string]string{"operation": operationPing}); got != 0 {
		t.Errorf("query_duration_seconds{operation=ping} = %d, want 0: pgx routes Ping below "+
			"the tracer, and a span per readiness probe would dilute the trace stream", got)
	}

	// After Close the answer must be an honest ErrClosed, not a latched "ready".
	db.Close()
	if err := db.Check(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Check after Close = %v, want ErrClosed", err)
	}
	if db.Stat() != nil {
		t.Error("Stat on a closed pool returned a snapshot; the collector would export stale numbers")
	}
	// Idempotent.
	db.Close()
}

func TestAcquireAfterCloseFails(t *testing.T) {
	db, _ := newTestDB(t, nil)
	db.Close()

	if _, err := db.Acquire(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire after Close = %v, want ErrClosed", err)
	}
	if err := db.InTx(context.Background(), func(context.Context, pgx.Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("InTx after Close = %v, want ErrClosed", err)
	}
}

func TestAcquireReleaseRoundTrip(t *testing.T) {
	db, _ := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		conn.Release()
		t.Fatalf("query on acquired connection: %v", err)
	}
	conn.Release()
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}
}

// TestApplicationNameReachesTheServer proves the join key postgresql.conf's
// log_line_prefix depends on is actually set on the wire, not merely in a Go
// struct.
func TestApplicationNameReachesTheServer(t *testing.T) {
	db, _ := newTestDB(t, func(o *Options) { o.Service = "settle" })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var appName string
	if err := db.Pool().QueryRow(ctx, "SHOW application_name").Scan(&appName); err != nil {
		t.Fatalf("SHOW application_name: %v", err)
	}
	if appName != "settle" {
		t.Fatalf("application_name = %q, want %q", appName, "settle")
	}

	var tz string
	if err := db.Pool().QueryRow(ctx, "SHOW timezone").Scan(&tz); err != nil {
		t.Fatalf("SHOW timezone: %v", err)
	}
	if tz != "UTC" {
		t.Fatalf("timezone = %q, want UTC", tz)
	}
}

// -----------------------------------------------------------------------------
// The metric-name contract
// -----------------------------------------------------------------------------

// TestMetricNamesAreTheContract freezes the exported series names.
//
// deploy/observability/prometheus.yml states the rule: "every application series
// is prefixed `sharpline_`", and the Grafana dashboard and alert rules are
// written against those names. The phase-0 dashboard has no database panels, so
// these names are new rather than adopted — which makes pinning them here more
// important, not less: a rename after a panel is written shows "No data" forever
// and nothing else fails.
func TestMetricNamesAreTheContract(t *testing.T) {
	db, reg := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Exercise every code path that owns a metric, so Gather returns all of
	// them: a CounterVec or HistogramVec with no observed label set exports
	// nothing at all.
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	var one int
	if err := db.Pool().QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	// A deliberate error, to populate query_errors_total.
	if _, err := db.Pool().Exec(ctx, "SELECT * FROM a_table_that_does_not_exist"); err == nil {
		t.Fatal("expected an error selecting from a missing table")
	}
	if err := db.InTx(ctx, func(context.Context, pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("committing InTx: %v", err)
	}
	if err := db.InTx(ctx, func(context.Context, pgx.Tx) error { return errors.New("no") }); err == nil {
		t.Fatal("expected the body error back")
	}

	want := []string{
		"sharpline_db_connect_attempts_total",
		"sharpline_db_ping_duration_seconds",
		"sharpline_db_pool_acquire_wait_seconds_total",
		"sharpline_db_pool_acquires_total",
		"sharpline_db_pool_canceled_acquires_total",
		"sharpline_db_pool_connections",
		"sharpline_db_pool_connections_max",
		"sharpline_db_pool_destroyed_connections_total",
		"sharpline_db_pool_empty_acquires_total",
		"sharpline_db_pool_new_connections_total",
		"sharpline_db_query_duration_seconds",
		"sharpline_db_query_errors_total",
		"sharpline_db_transaction_duration_seconds",
		"sharpline_db_transactions_total",
		"sharpline_db_up",
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got []string
	for _, f := range families {
		name := f.GetName()
		if !strings.HasPrefix(name, "sharpline_") {
			t.Errorf("series %q is not prefixed sharpline_; prometheus.yml makes that prefix a contract", name)
		}
		got = append(got, name)
	}
	sort.Strings(got)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("exported series changed.\n got:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// No `service` label anywhere: prometheus.yml attaches it as a TARGET label
	// on every scrape job, and a metric label of the same name is renamed to
	// exported_service on ingest.
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "service" {
					t.Errorf("%s carries a `service` label; prometheus.yml already sets that as a target label", f.GetName())
				}
			}
		}
	}
}

func TestPoolStatsCollectorToleratesNil(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newPoolStatsCollector(nil))
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("Gather with a nil stat function: %v", err)
	}

	reg2 := prometheus.NewRegistry()
	reg2.MustRegister(newPoolStatsCollector(func() *pgxpool.Stat { return nil }))
	families, err := reg2.Gather()
	if err != nil {
		t.Fatalf("Gather with a nil snapshot: %v", err)
	}
	if len(families) != 0 {
		t.Fatalf("a closed pool exported %d families; want none, so the graph shows a gap "+
			"rather than a fabricated zero", len(families))
	}
}

func TestTwoPoolsOnOneRegistryFailLoudly(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := newMetrics(reg); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if _, err := newMetrics(reg); err == nil {
		t.Fatal("second registration on the same registry succeeded; two pools would " +
			"silently add their numbers together under one series")
	}
}

func TestNilRegistryRegistersNothing(t *testing.T) {
	m, err := newMetrics(nil)
	if err != nil {
		t.Fatalf("newMetrics(nil): %v", err)
	}
	// The observe calls must remain safe so no call site needs a nil check.
	m.observeQuery("select", time.Millisecond, nil)
	m.observeTx(txCommitted, time.Millisecond)
	m.observePing(time.Millisecond, errors.New("down"))
	m.observeConnectAttempt(connectOK)
}

// -----------------------------------------------------------------------------
// Metric readers
// -----------------------------------------------------------------------------

// metricValue returns the value of a gauge or counter matching labels, or 0 when
// no such series exists.
func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m.GetLabel(), labels) {
				continue
			}
			switch {
			case m.Counter != nil:
				return m.Counter.GetValue()
			case m.Gauge != nil:
				return m.Gauge.GetValue()
			case m.Untyped != nil:
				return m.Untyped.GetValue()
			}
		}
	}
	return 0
}

// histogramCount returns the observation count of a histogram matching labels.
func histogramCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.Histogram == nil || !labelsMatch(m.GetLabel(), labels) {
				continue
			}
			return m.Histogram.GetSampleCount()
		}
	}
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, l := range got {
			if l.GetName() == k && l.GetValue() == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// OpenTelemetry spans
// -----------------------------------------------------------------------------

// TestQueriesProduceSpans is the evidence behind CLAUDE.md §9's "traces spanning
// ingest -> pricer -> stream so a single odds update can be followed end to end".
// A database hop that produces no span is a gap in that trace, and the default
// OTel provider is a no-op, so "the code calls tracer.Start" proves nothing on
// its own. This runs a real SDK provider with a recorder attached and reads the
// spans back.
func TestQueriesProduceSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	db, _ := newTestDB(t, func(o *Options) { o.TracerProvider = provider })

	// A parent span, so the linkage that makes an end-to-end trace possible is
	// asserted and not assumed.
	ctx, parent := provider.Tracer("test").Start(context.Background(), "caller")

	var one int
	if err := db.Pool().QueryRow(ctx,
		"-- name: SelectOne :one\nSELECT 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, "SELECT * FROM no_such_table"); err == nil {
		t.Fatal("expected an error from a missing table")
	}
	parent.End()

	spans := recorder.Ended()

	var ok, failed *tracetest.SpanStub
	for i := range spans {
		stub := tracetest.SpanStubFromReadOnlySpan(spans[i])
		switch stub.Name {
		case "SelectOne":
			ok = &stub
		case "select":
			failed = &stub
		}
	}

	if ok == nil {
		var names []string
		for i := range spans {
			names = append(names, spans[i].Name())
		}
		t.Fatalf("no span named SelectOne; recorded spans: %v. The sqlc query name must "+
			"become the span name, otherwise every database span in Jaeger reads "+
			"\"select\" and the trace is unreadable", names)
	}

	if ok.SpanKind != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", ok.SpanKind)
	}
	if ok.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Error("the query span is not a child of the caller's span; an unparented " +
			"database span cannot appear in an end-to-end trace")
	}

	attrs := map[string]string{}
	for _, kv := range ok.Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	for key, want := range map[string]string{
		attrDBSystem:    dbSystemPostgreSQL,
		attrDBName:      testDatabase,
		attrDBUser:      testUser,
		attrDBOperation: "SelectOne",
	} {
		if got := attrs[key]; got != want {
			t.Errorf("span attribute %s = %q, want %q", key, got, want)
		}
	}
	if !strings.Contains(attrs[attrDBStatement], "SELECT 1") {
		t.Errorf("span attribute %s = %q, want the statement text", attrDBStatement, attrs[attrDBStatement])
	}
	if _, present := attrs[attrServerAddr]; !present {
		t.Errorf("span is missing %s", attrServerAddr)
	}

	// The failing query's span must carry the error and the SQLSTATE, because
	// that is what makes a trace actionable rather than merely present.
	if failed == nil {
		t.Fatal("no span recorded for the failing statement")
	}
	if failed.Status.Code != codes.Error {
		t.Errorf("failing span status = %v, want Error", failed.Status.Code)
	}
	var sqlstate string
	for _, kv := range failed.Attributes {
		if string(kv.Key) == attrDBStatusCode {
			sqlstate = kv.Value.String()
		}
	}
	// 42P01 undefined_table.
	if sqlstate != "42P01" {
		t.Errorf("failing span %s = %q, want 42P01 undefined_table", attrDBStatusCode, sqlstate)
	}
	if len(failed.Events) == 0 {
		t.Error("failing span records no exception event; RecordError did not fire")
	}
}

// sqlcDBTX restates the interface sqlc v1.31.1 generates (see
// internal/platform/postgres/gen/db.go). It is redeclared here rather than
// imported so this package does not take a dependency on generated code, but the
// assertions below are the real point: BOTH handles this package hands out must
// satisfy it, or sqlc-generated queries cannot be used inside a transaction.
//
// That is the whole reason Pool returns *pgxpool.Pool instead of a wrapper, and
// the reason TxFunc receives a pgx.Tx instead of a bespoke type. If a future
// change breaks either, it breaks here rather than at every call site.
type sqlcDBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

var (
	_ sqlcDBTX = (*pgxpool.Pool)(nil)
	_ sqlcDBTX = (*pgxpool.Conn)(nil)
	_ sqlcDBTX = (pgx.Tx)(nil)
)
