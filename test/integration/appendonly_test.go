package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// Append-only enforcement on `prices` and the two ledger tables.
//
// # THE TRAP THIS TEST IS BUILT AROUND
//
// The guards are BEFORE UPDATE / BEFORE DELETE ... FOR EACH ROW triggers. A FOR EACH
// ROW trigger fires ONCE PER AFFECTED ROW, so a statement that matches nothing fires
// nothing and returns success. `UPDATE prices SET decimal_odds = 2` against an empty
// table therefore SUCCEEDS, and a test written against an empty table proves the
// exact opposite of what it claims: it passes whether the trigger exists or not.
//
// Phase 2a's manual gate hit that false pass — all 21 tables were empty. So every
// case below writes its own row first, asserts the row is there, and only then
// attempts the mutation. The vacuous case is included on purpose, asserted to
// SUCCEED, so that the reason the rest of the file is shaped this way is visible
// rather than folkloric.
//
// # SQLSTATE
//
// These guards raise restrict_violation (23001), NOT check_violation (23514). Worth
// pinning: 23514 is what the ledger's balance assertion raises, and a caller that
// matched only on 23514 to mean "the ledger refused this" would silently mishandle
// an attempted rewrite. postgres.IsCheckViolation must be FALSE here.

func TestPricesRejectsUpdateAndDeleteOnRowsThatExist(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	conn := rawConn(t, db.dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)

	// now(), not a historical window: a chunk containing the present is newer than
	// the policy's compress_after (7 days), so it can never be compressed out from
	// under the test. An UPDATE against a compressed chunk fails for a DIFFERENT
	// reason, and this test must fail only for the trigger's reason.
	observed := time.Now().UTC().Truncate(time.Microsecond)
	const originalOdds = 1.9500000000000002

	mustExec(t, ctx, conn, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, $3, NULL, $4, $4)`, mkt.HomeSelection, cat.BookID, originalOdds, observed)

	// The row exists. Everything below depends on it.
	if n := scalarInt(t, ctx, conn,
		`SELECT count(*) FROM prices WHERE selection_id = $1 AND observed_at = $2`,
		mkt.HomeSelection, observed); n != 1 {
		t.Fatalf("%d matching price rows before the test starts, want 1", n)
	}

	t.Run("UPDATE matching a real row is rejected", func(t *testing.T) {
		_, err := conn.Exec(ctx, `
UPDATE prices SET decimal_odds = 3.0
 WHERE selection_id = $1 AND book_id = $2 AND observed_at = $3`,
			mkt.HomeSelection, cat.BookID, observed)
		assertAppendOnlyRefusal(t, err, "prices", "immutable")
	})

	t.Run("DELETE matching a real row is rejected", func(t *testing.T) {
		_, err := conn.Exec(ctx, `
DELETE FROM prices
 WHERE selection_id = $1 AND book_id = $2 AND observed_at = $3`,
			mkt.HomeSelection, cat.BookID, observed)
		assertAppendOnlyRefusal(t, err, "prices", "immutable")
	})

	t.Run("the row is unchanged and still present", func(t *testing.T) {
		var odds float64
		err := conn.QueryRow(ctx, `
SELECT decimal_odds FROM prices
 WHERE selection_id = $1 AND book_id = $2 AND observed_at = $3`,
			mkt.HomeSelection, cat.BookID, observed).Scan(&odds)
		if err != nil {
			t.Fatalf("the row is gone after a rejected DELETE: %v", err)
		}
		if odds != originalOdds {
			t.Errorf("decimal_odds is now %v, want %v: the rejected UPDATE changed the row anyway",
				odds, originalOdds)
		}
	})

	t.Run("an UPDATE matching zero rows SUCCEEDS, which is why this test writes a row first", func(t *testing.T) {
		// This is the false pass phase 2a's manual gate hit. It is asserted rather
		// than avoided, because the day someone "simplifies" this file back to
		// running against an empty table, this case is the note explaining what
		// they broke.
		tag, err := conn.Exec(ctx, `
UPDATE prices SET decimal_odds = 3.0 WHERE selection_id = $1`,
			selectionID(t, uniqueID("no-such-selection")))
		if err != nil {
			t.Fatalf("an UPDATE matching zero rows failed: %v\n"+
				"That would be a surprise: a FOR EACH ROW trigger cannot fire on a statement with no affected rows.", err)
		}
		if n := tag.RowsAffected(); n != 0 {
			t.Fatalf("the zero-row UPDATE affected %d rows", n)
		}
	})
}

func TestLedgerEntriesAndTransactionsRejectUpdateAndDeleteOnRowsThatExist(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	user := newUser(t, ctx, conn)
	m := grantMovement(t, user, domain.Money(7_500))

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return writeMovementRaw(ctx, tx, m)
	}); err != nil {
		t.Fatalf("write the movement this test will try to rewrite: %v", err)
	}

	if n := scalarInt(t, ctx, conn,
		`SELECT count(*) FROM ledger_entries WHERE transaction_id = $1`, m.TransactionID); n != 2 {
		t.Fatalf("%d entries committed, want 2; the rest of this test needs real rows to attack", n)
	}

	t.Run("entries reject UPDATE", func(t *testing.T) {
		_, err := conn.Exec(ctx, `
UPDATE ledger_entries SET amount_minor = amount_minor + 1 WHERE transaction_id = $1`, m.TransactionID)
		assertAppendOnlyRefusal(t, err, "ledger_entries", "append-only")
	})

	t.Run("entries reject DELETE", func(t *testing.T) {
		_, err := conn.Exec(ctx, `DELETE FROM ledger_entries WHERE transaction_id = $1`, m.TransactionID)
		assertAppendOnlyRefusal(t, err, "ledger_entries", "append-only")
	})

	t.Run("transactions reject UPDATE", func(t *testing.T) {
		_, err := conn.Exec(ctx, `
UPDATE ledger_transactions SET kind = 'adjustment' WHERE id = $1`, m.TransactionID)
		assertAppendOnlyRefusal(t, err, "ledger_transactions", "append-only")
	})

	t.Run("transactions reject DELETE", func(t *testing.T) {
		_, err := conn.Exec(ctx, `DELETE FROM ledger_transactions WHERE id = $1`, m.TransactionID)
		assertAppendOnlyRefusal(t, err, "ledger_transactions", "append-only")
	})

	t.Run("both halves survive intact", func(t *testing.T) {
		if n := scalarInt(t, ctx, conn,
			`SELECT count(*) FROM ledger_entries WHERE transaction_id = $1`, m.TransactionID); n != 2 {
			t.Errorf("%d entries remain, want 2", n)
		}
		if sum := scalarInt64(t, ctx, conn,
			`SELECT coalesce(sum(amount_minor), -1) FROM ledger_entries WHERE transaction_id = $1`,
			m.TransactionID); sum != 0 {
			t.Errorf("the surviving entries sum to %d, want 0: a rejected UPDATE changed an amount", sum)
		}
	})

	// A rejected UPDATE inside a transaction must not be swallowable either: the
	// helper reports it, and nothing commits.
	t.Run("InTx reports the refusal rather than committing", func(t *testing.T) {
		err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`UPDATE ledger_entries SET amount_minor = 0 WHERE transaction_id = $1`, m.TransactionID)
			return execErr
		})
		if err == nil {
			t.Fatal("InTx returned nil for a callback whose UPDATE the database refused")
		}
		if got := postgres.SQLState(err); got != "23001" {
			t.Errorf("InTx error SQLSTATE is %q, want 23001", got)
		}
	})
}

// TestAppendOnlyTablesRejectTruncate covers the statement-level half of the guards.
//
// TRUNCATE is the one operation that could destroy every other test's rows, so it
// runs inside a transaction that is ALWAYS rolled back. TRUNCATE is transactional in
// Postgres, so even if a future regression removed the trigger and the truncate
// succeeded, the rollback restores the tables and the only visible effect is this
// test failing — which is exactly the signal wanted. Concurrent tests block on the
// ACCESS EXCLUSIVE lock for the few milliseconds the transaction is open rather than
// seeing missing rows.
func TestAppendOnlyTablesRejectTruncate(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn := rawConn(t, sharedDatabase(t).dsn)

	// CASCADE on every statement, and the reason is a finding rather than a
	// stylistic choice.
	//
	// A BARE `TRUNCATE ledger_transactions` never reaches
	// ledger_transactions_no_truncate at all: ledger_entries holds a foreign key
	// into it, so Postgres refuses the statement first with SQLSTATE 0A000
	// ("cannot truncate a table referenced in a foreign key constraint") and no
	// trigger fires. The guard is only exercised by `TRUNCATE ... CASCADE` or by
	// naming both tables in one statement — which are also the two forms a person
	// trying to clear the ledger would actually reach for once the bare form told
	// them to. Both are asserted: the FK-only refusal in its own subtest below, so
	// the 0A000 path is on the record too.
	for _, table := range []string{"prices", "ledger_entries", "ledger_transactions", "audit_log"} {
		t.Run(table, func(t *testing.T) {
			err := attemptTruncate(t, ctx, conn, "TRUNCATE "+table+" CASCADE")
			if err == nil {
				t.Fatalf("TRUNCATE %s CASCADE succeeded. The statement-level guard is missing; "+
					"this transaction was rolled back, so the data is intact, but in production "+
					"one statement would have erased the table.", table)
			}
			if got := postgres.SQLState(err); got != "23001" {
				t.Errorf("TRUNCATE %s CASCADE failed with SQLSTATE %q, want 23001 (restrict_violation): %v",
					table, got, err)
			}
			if !strings.Contains(err.Error(), "TRUNCATE") {
				t.Errorf("the refusal does not name the operation: %v", err)
			}
		})
	}

	t.Run("a bare TRUNCATE of ledger_transactions is refused by the foreign key first", func(t *testing.T) {
		err := attemptTruncate(t, ctx, conn, "TRUNCATE ledger_transactions")
		if err == nil {
			t.Fatal("a bare TRUNCATE ledger_transactions succeeded")
		}
		if got := postgres.SQLState(err); got != "0A000" {
			t.Errorf("SQLSTATE is %q, want 0A000: %v\n"+
				"If this is now 23001 the trigger is being reached directly, which is fine — "+
				"but the CASCADE comment above should be updated to say so.", got, err)
		}
	})
}

// attemptTruncate runs a TRUNCATE inside a transaction that is ALWAYS rolled back,
// and returns the statement's error.
//
// TRUNCATE is the one operation in this package that could destroy every other
// test's rows, so it never runs outside a transaction. TRUNCATE is transactional in
// Postgres, so even if a future regression removed the guard and the statement
// succeeded, the rollback restores the tables and the only visible effect is this
// test failing — which is exactly the signal wanted. Concurrent tests block on the
// ACCESS EXCLUSIVE lock for the few milliseconds the transaction is open rather than
// observing missing rows.
func attemptTruncate(t *testing.T, ctx context.Context, conn *pgx.Conn, statement string) error {
	t.Helper()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Registered before the TRUNCATE is attempted, so no path out of this function
	// leaves it uncommitted-but-open.
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			t.Errorf("rollback after %q: %v", statement, rbErr)
		}
	}()

	_, err = tx.Exec(ctx, statement)
	return err
}

// assertAppendOnlyRefusal checks the shape of a refusal from one of the append-only
// guards: the right SQLSTATE, a message that names the rule, and — the part worth
// stating — that it is NOT a check violation.
func assertAppendOnlyRefusal(t *testing.T, err error, table, wantPhrase string) {
	t.Helper()

	if err == nil {
		t.Fatalf("the statement against %s succeeded; the append-only guard did not fire", table)
	}
	if got := postgres.SQLState(err); got != "23001" {
		t.Errorf("%s refusal has SQLSTATE %q, want 23001 (restrict_violation): %v", table, got, err)
	}
	if postgres.IsCheckViolation(err) {
		t.Errorf("%s refusal is classified as a check violation. 23514 is the LEDGER BALANCE "+
			"assertion's code; a caller that treats an attempted rewrite as a balance failure will "+
			"handle it wrongly: %v", table, err)
	}
	if !strings.Contains(err.Error(), wantPhrase) {
		t.Errorf("%s refusal does not mention %q, so the message does not say which rule refused: %v",
			table, wantPhrase, err)
	}
	// And it must never be mistaken for something worth retrying.
	if postgres.IsTransientConnectError(err) {
		t.Errorf("%s refusal is classified as a transient connection error; retrying it would loop forever: %v",
			table, err)
	}
}
