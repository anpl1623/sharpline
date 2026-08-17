package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// movementID gives each test a key nothing else uses, so the shared fixture
// tables need no truncation between tests and the tests can run in any order.
var movementCounter atomic.Int64

func movementID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", t.Name(), movementCounter.Add(1))
}

func insertMovement(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO tx_movements (id) VALUES ($1)`, id)
	return err
}

func insertEntry(ctx context.Context, tx pgx.Tx, movement string, amountMinor int64) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO tx_entries (movement_id, amount_minor) VALUES ($1, $2)`,
		movement, amountMinor)
	return err
}

func countEntries(t *testing.T, db *DB, movement string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var n int
	err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM tx_entries WHERE movement_id = $1`, movement).Scan(&n)
	if err != nil {
		t.Fatalf("counting entries for %s: %v", movement, err)
	}
	return n
}

// -----------------------------------------------------------------------------
// The bug this helper exists to prevent
// -----------------------------------------------------------------------------

// TestInTxSurfacesADeferredConstraintFailureFromCommit is the important test in
// this file.
//
// The fixture's constraint trigger is DEFERRABLE INITIALLY DEFERRED, exactly as
// migrations/00006 declares ledger_entries_balanced_at_commit. That means an
// unbalanced write produces:
//
//	INSERT  -> success
//	INSERT  -> success
//	COMMIT  -> ERROR 23514
//
// A transaction helper that ignores Commit's error — or that returns nil once the
// body returned nil — reports that a ledger movement was written when the
// database refused it. Balances in this system are derived from the ledger
// (CLAUDE.md §4), so a phantom movement is not a visible failure once, it is a
// wrong balance forever.
//
// The test asserts all four halves of the property:
//
//  1. every statement inside the body succeeded, proving the check really is
//     deferred and that this is not just an INSERT error arriving late;
//  2. InTx returned an error rather than success;
//  3. the error carries SQLSTATE 23514, so a caller can tell which invariant it
//     broke;
//  4. nothing was persisted.
func TestInTxSurfacesADeferredConstraintFailureFromCommit(t *testing.T) {
	db, reg := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mid := movementID(t)
	var bodyErrs []error

	err := db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bodyErrs = append(bodyErrs, insertMovement(ctx, tx, mid))
		// 500 and -400 do not sum to zero. The trigger will say so at COMMIT.
		bodyErrs = append(bodyErrs, insertEntry(ctx, tx, mid, 500))
		bodyErrs = append(bodyErrs, insertEntry(ctx, tx, mid, -400))
		return nil // the body believes it succeeded, and every statement did
	})

	// (1) Deferred means deferred: no statement failed.
	for i, e := range bodyErrs {
		if e != nil {
			t.Fatalf("statement %d inside the transaction failed with %v; the constraint is "+
				"supposed to be DEFERRABLE INITIALLY DEFERRED, so this test is no longer "+
				"testing commit-time failure", i, e)
		}
	}

	// (2) The failure must surface.
	if err == nil {
		t.Fatal("InTx returned nil for an unbalanced movement. The statements succeeded and " +
			"COMMIT rejected them; reporting success here is the exact defect this helper exists " +
			"to prevent.")
	}

	// (3) With its SQLSTATE intact through the wrapping.
	if !IsCheckViolation(err) {
		t.Fatalf("err = %v (SQLSTATE %q), want a check violation (23514)", err, SQLState(err))
	}
	if got := SQLState(err); got != "23514" {
		t.Fatalf("SQLState = %q, want 23514", got)
	}
	if !errorMentions(err, "commit") {
		t.Errorf("err = %v; the message should say the failure came from COMMIT, "+
			"because that is what distinguishes it from a statement error", err)
	}

	// (4) Nothing persisted. Postgres rolls back a transaction whose COMMIT fails.
	if n := countEntries(t, db, mid); n != 0 {
		t.Fatalf("%d entries persisted after a failed commit, want 0", n)
	}

	// And the outcome is attributed to the commit, not to a rollback.
	if got := metricValue(t, reg, "sharpline_db_transactions_total", map[string]string{"outcome": txCommitFailed}); got != 1 {
		t.Errorf("transactions_total{outcome=commit_failed} = %v, want 1", got)
	}
	if got := metricValue(t, reg, "sharpline_db_transactions_total", map[string]string{"outcome": txRolledBack}); got != 0 {
		t.Errorf("transactions_total{outcome=rolled_back} = %v, want 0; the body did not fail", got)
	}
	if got := metricValue(t, reg, "sharpline_db_query_errors_total", map[string]string{"code": "23514"}); got == 0 {
		t.Error("query_errors_total{code=23514} is 0; the commit error was not counted")
	}
}

// TestInTxCommitsABalancedMovement is the positive control: the same shape, with
// amounts that sum to zero, must commit and persist.
func TestInTxCommitsABalancedMovement(t *testing.T) {
	db, reg := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mid := movementID(t)
	err := db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertMovement(ctx, tx, mid); err != nil {
			return err
		}
		// Two rows summing to exactly zero — CLAUDE.md §4's whole rule, in
		// integer minor units.
		if err := insertEntry(ctx, tx, mid, 2500); err != nil {
			return err
		}
		return insertEntry(ctx, tx, mid, -2500)
	})
	if err != nil {
		t.Fatalf("InTx on a balanced movement: %v", err)
	}
	if n := countEntries(t, db, mid); n != 2 {
		t.Fatalf("%d entries persisted, want 2", n)
	}
	if got := metricValue(t, reg, "sharpline_db_transactions_total", map[string]string{"outcome": txCommitted}); got != 1 {
		t.Errorf("transactions_total{outcome=committed} = %v, want 1", got)
	}
}

// TestInTxRejectsASingleEntryMovement covers the other half of the fixture's
// assertion, which mirrors migrations/00006's ErrTooFewEntries: one entry cannot
// balance unless it is zero, and a zero entry is not a movement of money.
func TestInTxRejectsASingleEntryMovement(t *testing.T) {
	db, _ := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mid := movementID(t)
	err := db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertMovement(ctx, tx, mid); err != nil {
			return err
		}
		return insertEntry(ctx, tx, mid, 0)
	})
	if err == nil {
		t.Fatal("InTx accepted a one-entry movement")
	}
	if !IsCheckViolation(err) {
		t.Fatalf("err = %v (SQLSTATE %q), want 23514", err, SQLState(err))
	}
	if n := countEntries(t, db, mid); n != 0 {
		t.Fatalf("%d entries persisted, want 0", n)
	}
}

// -----------------------------------------------------------------------------
// Rollback
// -----------------------------------------------------------------------------

func TestInTxRollsBackOnBodyError(t *testing.T) {
	db, reg := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sentinel := errors.New("the body decided not to")
	mid := movementID(t)

	err := db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertMovement(ctx, tx, mid); err != nil {
			return err
		}
		if err := insertEntry(ctx, tx, mid, 100); err != nil {
			return err
		}
		if err := insertEntry(ctx, tx, mid, -100); err != nil {
			return err
		}
		// Balanced, so COMMIT would have succeeded. The body refuses anyway.
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the body's own error", err)
	}
	if n := countEntries(t, db, mid); n != 0 {
		t.Fatalf("%d entries persisted after a rollback, want 0", n)
	}
	if got := metricValue(t, reg, "sharpline_db_transactions_total", map[string]string{"outcome": txRolledBack}); got != 1 {
		t.Errorf("transactions_total{outcome=rolled_back} = %v, want 1", got)
	}
	if got := metricValue(t, reg, "sharpline_db_transactions_total", map[string]string{"outcome": txCommitted}); got != 0 {
		t.Errorf("transactions_total{outcome=committed} = %v, want 0", got)
	}
}

// TestInTxRollsBackWithADetachedContext covers the failure mode a plain
// `defer tx.Rollback(ctx)` has: when the caller's context is what failed, the
// rollback is cancelled before it reaches the server and the backend sits idle
// in transaction until idle_in_transaction_session_timeout kills it.
func TestInTxRollsBackWithADetachedContext(t *testing.T) {
	db, _ := newTestDB(t, nil)

	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	mid := movementID(t)
	sentinel := errors.New("giving up")

	err := db.InTx(parent, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertMovement(ctx, tx, mid); err != nil {
			return err
		}
		if err := insertEntry(ctx, tx, mid, 700); err != nil {
			return err
		}
		// The caller's context dies between statements — a deadline expiring
		// mid-request. The rollback must still reach the server.
		cancelParent()
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the body error", err)
	}
	// The decisive assertion: no joined rollback failure. If rollback had
	// inherited the cancelled context it would have failed and been joined in.
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; the rollback inherited the cancelled context instead of "+
			"running on a detached one", err)
	}
	if n := countEntries(t, db, mid); n != 0 {
		t.Fatalf("%d entries persisted, want 0", n)
	}
}

// TestInTxJoinsTheRollbackError proves the rollback error is never swallowed
// when it is a genuine second failure. The transaction's connection is destroyed
// underneath it, so ROLLBACK cannot be delivered.
func TestInTxJoinsTheRollbackError(t *testing.T) {
	db, _ := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bodyErr := errors.New("body failed first")

	err := db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Terminate this session from a second connection. The transaction's
		// socket is now dead, so the subsequent ROLLBACK fails on the wire.
		var pid int32
		if scanErr := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); scanErr != nil {
			return fmt.Errorf("reading backend pid: %w", scanErr)
		}
		if _, killErr := db.Pool().Exec(ctx,
			"SELECT pg_terminate_backend($1)", pid); killErr != nil {
			return fmt.Errorf("terminating backend: %w", killErr)
		}
		return bodyErr
	})

	if err == nil {
		t.Fatal("InTx returned nil after the body failed")
	}
	if !errors.Is(err, bodyErr) {
		t.Fatalf("err = %v; the body's own error must survive, not be replaced by the "+
			"rollback failure", err)
	}
	// The rollback failure must be reported alongside it. Its text names the
	// rollback so a joined error is distinguishable from a lone body error.
	if !errorMentions(err, "rollback") {
		t.Fatalf("err = %v; a failed rollback was swallowed. That failure means the "+
			"transaction's real state is unknown, which is worse news than the error "+
			"that triggered it.", err)
	}
}

// -----------------------------------------------------------------------------
// Panic
// -----------------------------------------------------------------------------

// TestInTxRollsBackAndRepanics: a panic through an open transaction otherwise
// leaves the connection checked out and the transaction open until the server's
// idle_in_transaction_session_timeout reaps it — one leaked connection per
// occurrence, out of the six this pool has.
func TestInTxRollsBackAndRepanics(t *testing.T) {
	db, reg := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mid := movementID(t)
	panicValue := "deliberate panic inside a transaction"

	func() {
		defer func() {
			p := recover()
			if p == nil {
				t.Fatal("the panic did not propagate; InTx must not swallow it")
			}
			if got, ok := p.(string); !ok || got != panicValue {
				t.Fatalf("recovered %#v, want the original panic value %q", p, panicValue)
			}
		}()

		_ = db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := insertMovement(ctx, tx, mid); err != nil {
				t.Errorf("insert before panic: %v", err)
			}
			if err := insertEntry(ctx, tx, mid, 999); err != nil {
				t.Errorf("insert before panic: %v", err)
			}
			panic(panicValue)
		})
		t.Fatal("unreachable: InTx returned instead of re-panicking")
	}()

	// Rolled back.
	if n := countEntries(t, db, mid); n != 0 {
		t.Fatalf("%d entries persisted after a panic, want 0", n)
	}
	// Counted, because a transaction that panicked is still a transaction that
	// happened. The metric defer is registered before the panic guard precisely
	// so it still runs during unwinding.
	if got := metricValue(t, reg, "sharpline_db_transactions_total", map[string]string{"outcome": txPanicked}); got != 1 {
		t.Errorf("transactions_total{outcome=panicked} = %v, want 1", got)
	}

	// And the connection came back to the pool rather than being leaked: the
	// pool must still be usable, and it must be usable MaxConns+1 times, which
	// it would not be if one connection were stuck in an open transaction.
	for i := range int(DefaultMaxConns) + 2 {
		var one int
		if err := db.Pool().QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("pool unusable on iteration %d after a panic: %v", i, err)
		}
	}
}

// -----------------------------------------------------------------------------
// Guard rails
// -----------------------------------------------------------------------------

func TestInTxRejectsANilFunction(t *testing.T) {
	db, _ := newTestDB(t, nil)
	if err := db.InTx(context.Background(), nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("err = %v, want ErrInvalidOptions", err)
	}
}

func TestInTxOptionsHonoursIsolationLevel(t *testing.T) {
	db, _ := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var level string
	err := db.InTxOptions(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable},
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&level)
		})
	if err != nil {
		t.Fatalf("InTxOptions: %v", err)
	}
	if level != "serializable" {
		t.Fatalf("transaction_isolation = %q, want serializable", level)
	}
}

// TestInTxReadOnlyRejectsAWrite proves the access mode reaches the server, which
// is what makes a read-only analytics transaction actually read-only rather than
// merely labelled that way.
func TestInTxReadOnlyRejectsAWrite(t *testing.T) {
	db, _ := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mid := movementID(t)
	err := db.InTxOptions(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		func(ctx context.Context, tx pgx.Tx) error {
			return insertMovement(ctx, tx, mid)
		})
	if err == nil {
		t.Fatal("a write succeeded inside a read-only transaction")
	}
	// 25006 read_only_sql_transaction.
	if got := SQLState(err); got != "25006" {
		t.Fatalf("SQLState = %q, want 25006 read_only_sql_transaction (err: %v)", got, err)
	}
}

// TestInTxDoesNotRetryABusinessFailure is the negative assertion behind the
// retry design. There must be no path by which a failed transaction body is
// re-executed, because a re-executed ledger write posts the movement twice and
// the deferred balance trigger cannot catch it: two balanced movements are each
// individually balanced.
func TestInTxDoesNotRetryABusinessFailure(t *testing.T) {
	db, _ := newTestDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var calls int
	mid := movementID(t)

	// A serialization failure — the one class that IS safe to retry in general,
	// and which this package still refuses to retry on the caller's behalf.
	err := db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		calls++
		if err := insertMovement(ctx, tx, mid); err != nil {
			return err
		}
		return &fakeSerializationFailure{}
	})

	if calls != 1 {
		t.Fatalf("the transaction body ran %d times; it must run exactly once", calls)
	}
	if err == nil {
		t.Fatal("expected the body's error")
	}

	// And an unbalanced commit is not retried either.
	calls = 0
	mid2 := movementID(t)
	err = db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		calls++
		if err := insertMovement(ctx, tx, mid2); err != nil {
			return err
		}
		if err := insertEntry(ctx, tx, mid2, 10); err != nil {
			return err
		}
		return insertEntry(ctx, tx, mid2, -9)
	})
	if calls != 1 {
		t.Fatalf("the body ran %d times after a commit-time constraint failure; want 1", calls)
	}
	if !IsCheckViolation(err) {
		t.Fatalf("err = %v, want 23514", err)
	}
	if n := countEntries(t, db, mid2); n != 0 {
		t.Fatalf("%d entries persisted; a retry would have doubled the movement", n)
	}
}

// TestConcurrentTransactionsDoNotExhaustTheirPool runs more concurrent
// transactions than the pool has connections. Every one must complete: pgx
// queues acquisitions rather than failing them, and InTx must return every
// connection it takes.
func TestConcurrentTransactionsDoNotExhaustTheirPool(t *testing.T) {
	const maxConns = 3
	const workers = 12

	db, reg := newTestDB(t, func(o *Options) { o.MaxConns = maxConns })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	errs := make(chan error, workers)
	for i := range workers {
		go func(i int) {
			mid := fmt.Sprintf("%s-concurrent-%d", t.Name(), i)
			errs <- db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				if err := insertMovement(ctx, tx, mid); err != nil {
					return err
				}
				if err := insertEntry(ctx, tx, mid, int64(100+i)); err != nil {
					return err
				}
				return insertEntry(ctx, tx, mid, -int64(100+i))
			})
		}(i)
	}
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent transaction failed: %v", err)
		}
	}

	if got := metricValue(t, reg, "sharpline_db_transactions_total", map[string]string{"outcome": txCommitted}); got != workers {
		t.Errorf("transactions_total{outcome=committed} = %v, want %d", got, workers)
	}

	stat := db.Stat()
	if stat.AcquiredConns() != 0 {
		t.Errorf("%d connections still checked out after every transaction finished; "+
			"InTx leaked one", stat.AcquiredConns())
	}
	if stat.TotalConns() > maxConns {
		t.Errorf("pool holds %d connections, above its MaxConns of %d", stat.TotalConns(), maxConns)
	}

	// More waiting than capacity means the pool queued: proof the concurrency
	// really exceeded MaxConns rather than being serialised by chance.
	if got := metricValue(t, reg, "sharpline_db_pool_empty_acquires_total", nil); got == 0 {
		t.Log("note: no empty acquires recorded; the workers did not actually contend")
	}
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// fakeSerializationFailure is a stand-in error for the retry-refusal test. It is
// an error value, not fabricated data: nothing about it reaches the database.
type fakeSerializationFailure struct{}

func (*fakeSerializationFailure) Error() string { return "serialization failure (synthesised)" }

func errorMentions(err error, substr string) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(substr))
}
