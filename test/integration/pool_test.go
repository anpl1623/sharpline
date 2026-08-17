package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// TestReadinessFailsWhenTheDatabaseGoesAwayAndRecoversWhenItReturns stops and starts
// the container, rather than mocking a failure.
//
// The pool dials database.stableDSN — see its doc comment for the measured reason a
// restarted container is reachable at neither its old published port nor its old bridge
// address, and why a one-hop relay is the honest way to give the pool the stable
// endpoint a Kubernetes Service would.
//
// # What is being asserted
//
// Readiness is a REAL ROUND TRIP, not a boolean latched at startup. That distinction
// is the whole point of the file it lives in, and it is unfalsifiable without actually
// taking the database away: a latched flag and a live probe are indistinguishable
// while the database is up.
func TestReadinessFailsWhenTheDatabaseGoesAwayAndRecoversWhenItReturns(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	db := freshDatabase(t)

	pool, reg := connectPool(t, db.stableDSN(t), func(o *postgres.Options) {
		// A short ping timeout so an unreachable server is reported in about a
		// second rather than sitting on the default. This is the same knob httpx's
		// readiness probe budget composes with.
		o.PingTimeout = 2 * time.Second
	})

	// ---- healthy -----------------------------------------------------------
	if err := pool.Check(ctx); err != nil {
		t.Fatalf("readiness check against a healthy database: %v", err)
	}
	if name := pool.Name(); name != "postgres" {
		t.Errorf("the checker's name is %q, want \"postgres\"; it is the key this dependency "+
			"appears under in the /readyz payload", name)
	}
	assertGauge(t, reg, "sharpline_db_up", 1)

	// ---- stop the container ------------------------------------------------
	stopTimeout := 20 * time.Second
	if err := db.container.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stop the database container: %v", err)
	}

	checkErr := pool.Check(ctx)
	if checkErr == nil {
		t.Fatal("the readiness check PASSED against a stopped database. " +
			"Readiness must be a live round trip; a value latched at startup answers " +
			"\"did this process once manage to connect\", which is not the question a load balancer asks.")
	}
	t.Logf("readiness while stopped (expected): %v", checkErr)

	// The failure is classified as transient, which is what distinguishes "wait for
	// it" from "this configuration is wrong". A stopped container's removed port
	// mapping produces ECONNREFUSED.
	if !postgres.IsTransientConnectError(checkErr) {
		t.Errorf("a stopped database is not classified as a transient connection failure: %v\n"+
			"Startup against a not-yet-ready StatefulSet depends on this classification.", checkErr)
	}
	// And it is NOT an auth failure, which would make Connect fail fast instead of
	// retrying.
	if errors.Is(checkErr, postgres.ErrUnauthorized) {
		t.Errorf("an unreachable database was reported as a credential failure: %v", checkErr)
	}
	assertGauge(t, reg, "sharpline_db_up", 0)

	// ---- start it again ----------------------------------------------------
	if err := db.container.Start(ctx); err != nil {
		t.Fatalf("start the database container again: %v", err)
	}

	// Recovery is polled rather than asserted once: the pool has to discard the
	// connections it lost and build new ones, and Postgres itself needs a moment
	// after the container starts.
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = pool.Check(ctx); lastErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context expired while waiting for recovery: %v", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr != nil {
		t.Fatalf("the readiness check never recovered after the database came back: %v\n"+
			"The pool must discard dead connections and build new ones; it is not required to be "+
			"reconstructed by the caller.", lastErr)
	}
	assertGauge(t, reg, "sharpline_db_up", 1)

	// A real statement works, not just the probe — the pool's connections are
	// genuinely usable again rather than merely answering a ping.
	var one int
	if err := pool.Pool().QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("query after recovery: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 returned %d", one)
	}
}

// TestConnectRetriesATransientFailureUntilTheDatabaseArrives is the startup half.
//
// The scenario is the one CLAUDE.md §9 creates on purpose: in Kubernetes there is no
// `depends_on`, so a service's pod starts while Postgres is still replaying WAL or has
// no endpoint yet. Connect must ride that out. The test manufactures it honestly — the
// container is stopped when Connect is called and started 1.5 seconds later — and
// asserts both that Connect succeeded and that it got there by RETRYING, which the
// `retryable` counter is the only evidence of.
func TestConnectRetriesATransientFailureUntilTheDatabaseArrives(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	db := freshDatabase(t)

	stopTimeout := 20 * time.Second
	if err := db.container.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stop the database container: %v", err)
	}

	started := make(chan error, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		started <- db.container.Start(context.WithoutCancel(ctx))
	}()

	dsn := db.stableDSN(t)

	connectStart := time.Now()
	pool, reg := connectPool(t, dsn, func(o *postgres.Options) {
		// A generous attempt budget with a short base backoff: the point is to
		// observe retries, not to test the default schedule.
		o.ConnectAttempts = 40
		o.ConnectBackoff = 250 * time.Millisecond
		o.ConnectBackoffMax = time.Second
		o.PingTimeout = 2 * time.Second
	})
	elapsed := time.Since(connectStart)

	if err := <-started; err != nil {
		t.Fatalf("start the container: %v", err)
	}

	// connectPool fatals on failure, so reaching here means Connect succeeded.
	if err := pool.Check(ctx); err != nil {
		t.Fatalf("the pool Connect returned is not usable: %v", err)
	}

	retried, found := counterValue(t, reg, "sharpline_db_connect_attempts_total",
		map[string]string{"outcome": "retryable"})
	if !found || retried < 1 {
		t.Errorf("Connect succeeded in %s without recording a single retryable attempt.\n"+
			"Either it did not actually retry — in which case the container was already back before the "+
			"first probe and this test proved nothing — or the metric is not being incremented.", elapsed)
	} else {
		t.Logf("Connect rode out the outage in %s across %g retryable attempt(s)", elapsed, retried)
	}
	assertCounter(t, reg, "sharpline_db_connect_attempts_total", map[string]string{"outcome": "ok"}, 1)
	assertCounter(t, reg, "sharpline_db_connect_attempts_total", map[string]string{"outcome": "fatal"}, 0)
}

// TestConnectFailsFastOnAMissingDatabaseRatherThanRetrying is the complement, and it
// is what makes the retry classification a decision rather than a blanket.
//
// SQLSTATE 3D000 (invalid_catalog_name) is deliberately NOT in the retryable set: "the
// Postgres entrypoint creates the database before it accepts connections from the
// network", so a missing database is a configuration error and waiting does not fix
// it. Retrying it would burn the whole probe budget and then report a timeout, burying
// the real cause — which is the opposite of CLAUDE.md §12's fail-fast-and-loudly rule.
func TestConnectFailsFastOnAMissingDatabaseRatherThanRetrying(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A database that does not exist, on a server that does — so the failure is
	// unambiguously 3D000 and not a dial failure wearing its clothes.
	dsn := replaceDatabase(t, sharedDatabase(t).dsn, "sharpline_does_not_exist")

	reg := prometheus.NewRegistry()
	start := time.Now()
	pool, err := postgres.Connect(ctx, postgres.Options{
		DSN:      dsn,
		Service:  "integration",
		Logger:   testLogger(t),
		Registry: reg,
		// A budget large enough that a retry loop would be obvious in the elapsed
		// time: 12 attempts with a 1s floor could not finish in under 10 seconds.
		ConnectAttempts:   12,
		ConnectBackoff:    time.Second,
		ConnectBackoffMax: time.Second,
	})
	elapsed := time.Since(start)
	if pool != nil {
		pool.Close()
	}

	if err == nil {
		t.Fatal("Connect succeeded against a database that does not exist")
	}
	if !errors.Is(err, postgres.ErrUnauthorized) {
		t.Errorf("error is not ErrUnauthorized: %v", err)
	}
	if got := postgres.SQLState(err); got != "3D000" {
		t.Errorf("SQLSTATE is %q, want 3D000 (invalid_catalog_name): %v", got, err)
	}
	if postgres.IsTransientConnectError(err) {
		t.Errorf("a missing database is classified as transient, so startup would retry it 12 times "+
			"and then report a timeout instead of the real cause: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Connect took %s to report a missing database; with a 1s backoff floor that means it "+
			"retried rather than failing fast", elapsed)
	}
	assertCounter(t, reg, "sharpline_db_connect_attempts_total", map[string]string{"outcome": "fatal"}, 1)
	assertCounter(t, reg, "sharpline_db_connect_attempts_total", map[string]string{"outcome": "retryable"}, 0)
}

// TestConstraintViolationsAreNeverClassifiedAsRetryable is the assertion that keeps a
// retry loop away from the ledger.
//
// Every error below is produced by a real statement against the real schema, not
// constructed. The classification matters because a retried ledger write is a
// DOUBLE-APPLIED MOVEMENT: the first attempt may have committed and the caller may
// simply not know it. That is also why SQLSTATE 08007 (transaction_resolution_unknown)
// is excluded from the retryable set even though the rest of class 08 is in it.
func TestConstraintViolationsAreNeverClassifiedAsRetryable(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)

	cases := []struct {
		name      string
		wantState string
		classify  func(error) bool
		produce   func(t *testing.T) error
	}{
		{
			name:      "check violation on a price out of range",
			wantState: "23514",
			classify:  postgres.IsCheckViolation,
			produce: func(t *testing.T) error {
				// prices_decimal_odds_range: strictly greater than 1.0. Decimal odds
				// of 0.5 would mean a bet returning less than the stake.
				_, err := conn.Exec(ctx, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, 0.5, NULL, now(), now())`, mkt.HomeSelection, cat.BookID)
				return err
			},
		},
		{
			name:      "unique violation on a duplicate primary key",
			wantState: "23505",
			classify:  postgres.IsUniqueViolation,
			produce: func(t *testing.T) error {
				_, err := conn.Exec(ctx,
					`INSERT INTO sports (id, slug, name) VALUES ($1, $2, $3)`,
					cat.SportID, uniqueSlug("dup"), "Duplicate Sport")
				return err
			},
		},
		{
			name:      "foreign key violation on a price for a selection that does not exist",
			wantState: "23503",
			classify:  postgres.IsForeignKeyViolation,
			produce: func(t *testing.T) error {
				_, err := conn.Exec(ctx, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, 2.0, NULL, now(), now())`,
					selectionID(t, uniqueID("ghost")), cat.BookID)
				return err
			},
		},
		{
			name:      "not-null violation",
			wantState: "23502",
			classify:  postgres.IsNotNullViolation,
			produce: func(t *testing.T) error {
				_, err := conn.Exec(ctx,
					`INSERT INTO sports (id, slug, name) VALUES ($1, $2, NULL)`,
					uniqueID("sport"), uniqueSlug("sport"))
				return err
			},
		},
		{
			name:      "the append-only guard",
			wantState: "23001",
			classify:  func(err error) bool { return postgres.SQLState(err) == "23001" },
			produce: func(t *testing.T) error {
				observed := time.Now().UTC()
				mustExec(t, ctx, conn, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, 2.5, NULL, $3, $3)`, mkt.AwaySelection, cat.BookID, observed)

				_, err := conn.Exec(ctx, `
UPDATE prices SET decimal_odds = 2.6
 WHERE selection_id = $1 AND book_id = $2 AND observed_at = $3`,
					mkt.AwaySelection, cat.BookID, observed)
				return err
			},
		},
		{
			name:      "the deferred ledger balance assertion, arriving from COMMIT",
			wantState: "23514",
			classify:  postgres.IsCheckViolation,
			produce: func(t *testing.T) error {
				m := grantMovement(t, user, domain.Money(4_000))
				m.Entries[0].Amount = domain.Money(-3_000)
				return pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
					return writeMovementRaw(ctx, tx, m)
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.produce(t)
			if err == nil {
				t.Fatal("the statement succeeded; there is no error to classify")
			}
			if got := postgres.SQLState(err); got != tc.wantState {
				t.Errorf("SQLSTATE is %q, want %q: %v", got, tc.wantState, err)
			}
			if !tc.classify(err) {
				t.Errorf("the matching predicate does not recognise this error: %v", err)
			}

			// THE ASSERTION. None of these may be retried.
			if postgres.IsTransientConnectError(err) {
				t.Errorf("this error is classified as a transient connection failure, so a startup probe "+
					"or a retry loop would repeat it. For a ledger write that is a double-applied "+
					"movement: %v", err)
			}
			// And none of them is a serialization failure, which is the ONE class a
			// caller may deliberately retry — with an idempotency key.
			if postgres.IsSerializationFailure(err) {
				t.Errorf("this error is classified as a serialization failure, which callers are told "+
					"they may retry: %v", err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// replaceDatabase rewrites the database name in a postgres:// DSN.
func replaceDatabase(t *testing.T, dsn, database string) string {
	t.Helper()

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, database)
}
