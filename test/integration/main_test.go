// Package integration holds the phase-2 integration tier: the access layer
// exercised against a REAL Postgres 17 + TimescaleDB, spawned by
// testcontainers-go from inside the test container.
//
// CLAUDE.md §10 is the mandate and it is emphatic: "Integration tests use
// testcontainers-go against real Postgres/Redis/Kafka — no mocked databases, and
// no mocked broker either, because the interesting bugs live in consumer-group
// rebalancing and offset handling." §8 puts them in test/.
//
// # What runs where
//
// The image under test is the SAME image at the SAME digest as the compose
// stack's `postgres` service, copied from the contract ledger. A stock postgres
// image has no TimescaleDB, and two of the properties this package exists to
// prove — the chunk interval and the compression policy — do not exist without
// it. Testing against a different engine than the one that will run in
// production defeats the point of using a real database at all.
//
// Containers are SIBLINGS, not children: the Makefile's `test` target mounts the
// host's docker socket into the Go test container (resolved from `docker context
// inspect`, because on this machine the socket is at ~/.docker/run/docker.sock
// and /var/run/docker.sock does not exist at all), so the containers this package
// starts are created on the host daemon alongside the test container rather than
// inside it. Their published ports therefore land on the HOST, which is why
// TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal is set — 127.0.0.1 inside the
// test container is the test container. TestDockerSocketIsUsableFromInsideTheTestContainer
// asserts that whole chain rather than assuming it.
//
// # Container topology
//
//	one SHARED migrated database, started by TestMain, used by every test that
//	only reads and writes rows in a key space it owns; and
//
//	one DEDICATED database per test that must destroy or restart the server —
//	migration reversibility, drift refusal, and the readiness stop/start proof.
//
// A shared server is not a shared fixture. Every test mints its own ids from
// uniqueID/uniqueSlug and, where chunk boundaries matter, its own disjoint time
// window from uniqueTimeWindow, so the tests are order-independent and safe under
// t.Parallel(). Nothing here reads a row another test wrote.
//
// # NO MOCK DATA
//
// The rows in this package are created BY a test, FOR that test, and asserted on
// by the test that created them. Nothing is seeded, nothing is shipped, nothing
// stands in for ingested data, and no canned query result appears anywhere. Where
// a CHECK constraint demands a particular shape — users.password_hash must match
// `$argon2id$%` — the fixture satisfies the shape and the test says so out loud.
//
// # Failure, not skip
//
// These tests FAIL when the docker socket is unreachable; they do not skip. A
// silently skipped integration test is worse than no integration test: it reports
// green while proving nothing, and the CI job that was meant to enforce the
// prime directive becomes decorative.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/platform/migrate"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// postgresImage is the compose stack's `postgres` image, pinned by digest, from
// the contract ledger. Do not float this tag: the chunk-interval and compression
// assertions below are TimescaleDB behaviour, and `postgres:17` has none of it.
const postgresImage = "timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6"

const (
	pgUser     = "sharpline_it"
	pgPassword = "sharpline_it_password"
	pgDatabase = "sharpline_it"
)

// containerStartDeadline bounds one container boot including the image pull on a
// cold cache. TimescaleDB's entrypoint starts the server twice (once to run initdb
// scripts, once for real), which is why the log wait below asks for two
// occurrences.
const containerStartDeadline = 4 * time.Minute

// database is one running Postgres container and how to reach it. A test that needs
// an address surviving a stop/start calls stableDSN instead of using dsn.
type database struct {
	container testcontainers.Container

	// dsn goes through the container's PUBLISHED port on the host, reached via
	// host.docker.internal. This is the normal path and the one the Makefile's
	// TESTCONTAINERS_HOST_OVERRIDE wiring is for.
	dsn string
}

// shared is the one migrated database used by every non-destructive test.
// sharedErr records why it does not exist, so a test fails with the real cause
// rather than a nil-pointer panic twelve frames deep.
var (
	shared    *database
	sharedErr error
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartDeadline+time.Minute)

	db, err := startPostgres(ctx)
	if err != nil {
		sharedErr = fmt.Errorf("start shared database: %w", err)
	} else if err := applyAllMigrations(ctx, db.dsn); err != nil {
		sharedErr = fmt.Errorf("migrate shared database: %w", err)
		terminate(db)
		db = nil
	} else {
		shared = db
	}
	cancel()

	code := m.Run()

	if shared != nil {
		terminate(shared)
	}
	os.Exit(code)
}

// sharedDatabase returns the package's migrated database, failing the calling
// test with the startup error if there is not one.
func sharedDatabase(t *testing.T) *database {
	t.Helper()
	if sharedErr != nil {
		t.Fatalf("the shared database is unavailable, so nothing in this package can run: %v", sharedErr)
	}
	return shared
}

// freshDatabase starts a database of this test's own, with NO migrations applied,
// and terminates it when the test ends. For tests that roll the schema back or
// restart the server, which must not touch anyone else's.
func freshDatabase(t *testing.T) *database {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), containerStartDeadline)
	defer cancel()

	db, err := startPostgres(ctx)
	if err != nil {
		t.Fatalf("start dedicated database: %v", err)
	}
	t.Cleanup(func() { terminate(db) })
	return db
}

// freshMigratedDatabase is freshDatabase with the full migration set applied.
func freshMigratedDatabase(t *testing.T) *database {
	t.Helper()

	db := freshDatabase(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := applyAllMigrations(ctx, db.dsn); err != nil {
		t.Fatalf("migrate dedicated database: %v", err)
	}
	return db
}

func startPostgres(ctx context.Context) (*database, error) {
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     pgUser,
				"POSTGRES_PASSWORD": pgPassword,
				"POSTGRES_DB":       pgDatabase,
				// Telemetry off and tuning off: this is a throwaway on a laptop,
				// and timescaledb-tune rewrites postgresql.conf on first boot,
				// which would make one container's settings differ from the next
				// depending on how much memory the docker VM happened to have.
				"TIMESCALEDB_TELEMETRY": "off",
				"NO_TS_TUNE":            "true",
				// scram-sha-256 everywhere, including local connections, so the
				// engine authenticates the way the compose stack does.
				"POSTGRES_HOST_AUTH_METHOD": "scram-sha-256",
				// libpq defaults for anything exec'd INSIDE the container —
				// pg_dump, in the reversibility test. With scram forced above,
				// even a unix-socket connection needs the password, and passing
				// it through the environment keeps it out of the argv that shows
				// up in pg_stat_activity.
				"PGUSER":     pgUser,
				"PGPASSWORD": pgPassword,
				"PGDATABASE": pgDatabase,
			},
			WaitingFor: wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithDeadline(containerStartDeadline),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	db := &database{container: container}

	host, err := container.Host(ctx)
	if err != nil {
		terminate(db)
		return nil, fmt.Errorf("container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		terminate(db)
		return nil, fmt.Errorf("container mapped port: %w", err)
	}
	db.dsn = dsnFor(host, port.Port())

	return db, nil
}

// stableDSN returns a DSN whose host:port never changes, even across a container
// stop/start, by putting a one-hop TCP relay in front of the container.
//
// # Why this is necessary, measured rather than assumed
//
// Two routes to a container survive a restart badly, and both were tried:
//
//	the PUBLISHED HOST PORT is REALLOCATED on every stop/start. Verified on this
//	daemon (29.6.2): one container's published port moved 55750 -> 55754 -> 55771
//	across two cycles.
//
//	the container's BRIDGE ADDRESS is reassigned under concurrency. A single
//	container held 172.17.0.2 across two cycles when nothing else was starting,
//	which looked stable — but in the real suite, with four other containers being
//	created at the same moment, the restarted container came back on 172.17.0.5
//	having left on 172.17.0.6. Docker's IPAM does not hold the reservation.
//
// A pool holding either address would never recover, and the readiness test would
// report a defect in internal/platform/postgres that does not exist: the pool would be
// dialing an address nothing serves any more.
//
// So the relay owns a stable loopback address inside the test process and RESOLVES THE
// CONTAINER'S CURRENT ENDPOINT ON EVERY NEW CONNECTION. That is not a mock of a
// failure — the container really is stopped and really comes back, and the relay
// simply plays the part a Kubernetes Service or the Caddy front door plays in
// production, where a pod's address changes and the client's does not.
//
// One consequence to be honest about: because the relay always accepts, an outage
// reaches the pool as a truncated handshake (io.EOF) rather than as a refused
// connection. Both are in postgres.IsTransientConnectError's set — io.EOF is there for
// "the server accepted the TCP connection and went away before completing startup",
// which is exactly what this is — so the classification under test is the same one a
// real outage exercises.
func (db *database) stableDSN(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("open the relay listener: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return // the listener was closed by the cleanup above
			}
			go db.relay(downstream)
		}
	}()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("relay listener address is %T, not *net.TCPAddr", listener.Addr())
	}
	return dsnFor("127.0.0.1", strconv.Itoa(addr.Port))
}

// relay copies one connection through to the container's current endpoint. Resolved
// per connection, so a restart is picked up without the test having to tell it.
func (db *database) relay(downstream net.Conn) {
	defer func() { _ = downstream.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	host, err := db.container.Host(ctx)
	if err != nil {
		return
	}
	port, err := db.container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		// The container is stopped, so it has no port mapping. Closing the
		// downstream connection is the honest answer.
		return
	}

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, port.Port()), 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(upstream, downstream)
	}()
	_, _ = io.Copy(downstream, upstream)
	<-done
}

func dsnFor(host, port string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, host, port, pgDatabase)
}

func terminate(db *database) {
	if db == nil || db.container == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = db.container.Terminate(ctx)
}

// -----------------------------------------------------------------------------
// Migrations
// -----------------------------------------------------------------------------

// applyAllMigrations runs `up` through internal/platform/migrate — the same code
// path cmd/migrate takes, over the same embedded filesystem. Not the goose CLI:
// the point is to exercise the binary's own runner, which is what the compose
// service and the Kubernetes Job execute.
func applyAllMigrations(ctx context.Context, dsn string) error {
	summary, err := runMigrate(ctx, dsn, discardLogger(), migrate.Invocation{Command: migrate.CommandUp})
	if err != nil {
		return err
	}
	if summary.VersionAfter <= 0 {
		return fmt.Errorf("migrate up left the schema at version %d", summary.VersionAfter)
	}
	return nil
}

// runMigrate builds a Runner, runs one invocation and closes it. A Runner per
// invocation rather than one reused across a test: Close releases the pool, and
// the advisory lock is session-scoped, so a leaked Runner is a leaked lock holder
// that the next test would queue behind.
func runMigrate(ctx context.Context, dsn string, log *slog.Logger, inv migrate.Invocation) (*migrate.Summary, error) {
	runner, err := migrate.New(migrate.Options{DSN: dsn, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("build migrate runner: %w", err)
	}
	defer func() { _ = runner.Close() }()

	summary, runErr := runner.Run(ctx, inv)
	if runErr != nil {
		return summary, fmt.Errorf("migrate %s: %w", inv.Command, runErr)
	}
	return summary, nil
}

// embeddedMigrationCount is how many migrations the binary carries. Read from the
// embedded filesystem rather than hardcoded, so adding a migration does not
// require editing a number here — but the reversibility test still asserts the
// database ends on the HIGHEST embedded version, which is the property that
// matters.
func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	sources, err := migrate.Sources(migrate.EmbeddedFS())
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("no migrations are embedded; migrations/embed.go is not seeing migrations/*.sql")
	}
	return len(sources)
}

// -----------------------------------------------------------------------------
// Connections
// -----------------------------------------------------------------------------

// connectPool opens a *postgres.DB against dsn with its own Prometheus registry,
// and closes it when the test ends.
//
// Its own registry, never a shared one: internal/platform/postgres deliberately
// fails registration when two pools land on one registry rather than silently
// summing two services' numbers under one series, and the returned registry is
// what the metric assertions read.
func connectPool(t *testing.T, dsn string, mutate ...func(*postgres.Options)) (*postgres.DB, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	opts := postgres.Options{
		DSN:      dsn,
		Service:  "integration",
		Logger:   testLogger(t),
		Registry: reg,
	}
	for _, m := range mutate {
		m(&opts)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	db, err := postgres.Connect(ctx, opts)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(db.Close)
	return db, reg
}

// sharedPool is the pool most tests want: connected to the shared migrated
// database, with its own registry.
func sharedPool(t *testing.T) (*postgres.DB, *prometheus.Registry) {
	t.Helper()
	return connectPool(t, sharedDatabase(t).dsn)
}

// rawConn opens a single pgx connection, outside any pool. Used where a test must
// drive raw SQL and own the transaction boundary itself — the half of the
// double-entry proof that must NOT go through the transaction helper, so that the
// helper's behaviour is compared against the database's rather than assumed from
// it.
func rawConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = conn.Close(closeCtx)
	})
	return conn
}

// -----------------------------------------------------------------------------
// Logging
// -----------------------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// testLogger routes structured output into the test log, so a failure carries the
// access layer's own explanation of what went wrong instead of only the
// assertion. t.Log output is discarded for a passing test, so this costs nothing
// when everything works.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// -----------------------------------------------------------------------------
// Unique key spaces
// -----------------------------------------------------------------------------

// seq is the source of every unique token in this package. A monotonic counter
// rather than randomness: the ids in a failure message are then reproducible
// within a run, and two tests can never collide even under -shuffle.
var seq atomic.Uint64

// nextToken returns a FIXED-WIDTH decimal token, and the width is load-bearing rather
// than cosmetic.
//
// The first version formatted base 36, which gives tokens of different lengths — "b"
// and "bl" — and therefore names where one is a PREFIX of another: "Home b" is a
// prefix of "Home bl". SearchOpenEventsByCompetitorPrefix does prefix matching, so a
// test that expected its own competitor name to match exactly one event matched four.
// Zero-padding to a constant width makes no token a prefix of any other, which
// restores the isolation the counter was there to provide.
func nextToken() string { return fmt.Sprintf("%06d", seq.Add(1)) }

// uniqueID mints a primary key. Every id column in the schema is
// `TEXT CHECK (col ~ '^[A-Za-z0-9._-]{1,128}$')`, which this satisfies by
// construction — prefix must itself be drawn from that charset.
func uniqueID(prefix string) string { return prefix + "-" + nextToken() }

// uniqueSlug mints a slug. The schema's rule is
// `^[a-z0-9][a-z0-9_-]{0,63}$` — lowercase, leading alphanumeric, at most
// domain.MaxSlugLen = 64 characters. Uppercase is REJECTED rather than folded, so
// the prefix must already be lowercase.
func uniqueSlug(prefix string) string { return prefix + "-" + nextToken() }

// uniqueTimeWindow returns the start of a 30-day window no other caller will use.
//
// This is what makes the hypertable tests parallel-safe on a shared server. The
// `prices` hypertable is partitioned into 12-hour chunks, so two tests writing to
// timestamps 30 days apart cannot land in one another's chunks, and a test may
// therefore count the chunks in its own window and compress them without
// disturbing anyone.
//
// The base year is 2001 because `prices_observed_at_sane` requires
// observed_at > 1900-01-01, and because a window far in the past keeps the
// windows of successive runs from creeping into the present.
func uniqueTimeWindow() time.Time {
	base := time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(seq.Add(1)) * 30 * 24 * time.Hour)
}

// -----------------------------------------------------------------------------
// Exec inside the container
// -----------------------------------------------------------------------------

// execInContainer runs a command inside the database container and returns its
// combined output.
//
// This is how pg_dump is reached. pg_dump is not in the golang test image and,
// per the prime directive, must not be installed on the host either — but it is
// already inside the engine's own image, so the reversibility test runs it there.
func execInContainer(ctx context.Context, db *database, cmd ...string) (string, error) {
	code, reader, err := db.container.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return "", fmt.Errorf("exec %v: %w", cmd, err)
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("read output of %v: %w", cmd, err)
	}
	if code != 0 {
		return string(out), fmt.Errorf("exec %v exited %d: %s", cmd, code, out)
	}
	return string(out), nil
}
