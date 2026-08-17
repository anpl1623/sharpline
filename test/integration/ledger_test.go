package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// The double-entry constraint, from three directions.
//
// This is the highest-value file in the package, and the reason is CLAUDE.md §4:
// "Balances are derived, never stored as a mutable field." A derived balance is
// only as good as the rows it folds, so an unbalanced movement that reaches disk is
// not a bug that shows up once — it is a wrong balance forever, for every read, with
// no stored value to correct.
//
// The constraint that prevents it (migration 00006's
// ledger_entries_balanced_at_commit) is DEFERRABLE INITIALLY DEFERRED. That is
// necessary — the two halves of a movement arrive as two INSERTs, so a row-level
// check could never see both — and it has a consequence that must be tested rather
// than assumed: EVERY INSERT RETURNS SUCCESS. The rejection happens at COMMIT.
//
// Any code path that reports success after a failed COMMIT therefore corrupts the
// ledger silently. So the same violation is driven three ways:
//
//	raw SQL          — establishes what the DATABASE does, independent of any of
//	                   our code. The reference answer.
//	postgres.InTx    — the transaction helper. It must surface the commit error,
//	                   not swallow it. This is the test that matters.
//	sqlc + InTx      — the path phase 8 will actually write, through the generated
//	                   InsertLedgerTransaction/InsertLedgerEntry.
//
// All three must agree, and the balanced case must commit through all three.

// ledgerMovement is one balanced (or deliberately unbalanced) money movement, as a
// value the tests below can build variations of.
type ledgerMovement struct {
	TransactionID domain.TransactionID
	Kind          string
	WagerID       *domain.WagerID
	OccurredAt    time.Time
	Entries       []ledgerEntry
}

type ledgerEntry struct {
	AccountKind string
	UserID      *domain.UserID
	Amount      domain.Money
}

// grantMovement is a play-money grant: issuance is debited, the customer's cash
// account is credited, and the two sum to zero.
//
// `grant` is the one entry kind whose transaction must NOT carry a wager
// (ledger_transactions_wager_matches_kind), which makes it the cheapest balanced
// movement to build — no wager fixture required.
func grantMovement(t *testing.T, user domain.UserID, amount domain.Money) ledgerMovement {
	t.Helper()

	debit, err := amount.Neg()
	if err != nil {
		t.Fatalf("negate %s: %v", amount, err)
	}
	return ledgerMovement{
		TransactionID: transactionID(t, uniqueID("txn")),
		Kind:          domain.EntryKindGrant.String(),
		OccurredAt:    time.Now().UTC().Truncate(time.Microsecond),
		Entries: []ledgerEntry{
			// issuance carries no owner: ledger_entries_owner_matches_account_kind
			// requires account_user_id IS NULL for the house and issuance
			// singletons and NOT NULL for the two customer account kinds.
			{AccountKind: domain.AccountKindIssuance.String(), Amount: debit},
			{AccountKind: domain.AccountKindUserCash.String(), UserID: &user, Amount: amount},
		},
	}
}

// writeMovementRaw inserts a movement's header and every entry through raw SQL on
// x, returning the error of each INSERT so a test can assert that they all
// SUCCEEDED even when the movement is unbalanced.
func writeMovementRaw(ctx context.Context, x execer, m ledgerMovement) error {
	if _, err := x.Exec(ctx, `
INSERT INTO ledger_transactions (id, kind, wager_id, occurred_at)
VALUES ($1, $2, $3, $4)`, m.TransactionID, m.Kind, m.WagerID, m.OccurredAt); err != nil {
		return fmt.Errorf("insert transaction header: %w", err)
	}

	for i, e := range m.Entries {
		if _, err := x.Exec(ctx, `
INSERT INTO ledger_entries (transaction_id, entry_index, account_kind, account_user_id,
                            amount_minor, kind, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			m.TransactionID, int32(i), e.AccountKind, e.UserID,
			e.Amount.MinorUnits(), m.Kind, m.OccurredAt); err != nil {
			return fmt.Errorf("insert entry %d: %w", i, err)
		}
	}
	return nil
}

// writeMovementGenerated is writeMovementRaw through the sqlc-generated queries.
func writeMovementGenerated(ctx context.Context, q *gen.Queries, m ledgerMovement) error {
	if err := q.InsertLedgerTransaction(ctx, gen.InsertLedgerTransactionParams{
		ID:         m.TransactionID,
		Kind:       m.Kind,
		WagerID:    m.WagerID,
		OccurredAt: m.OccurredAt,
	}); err != nil {
		return fmt.Errorf("InsertLedgerTransaction: %w", err)
	}

	for i, e := range m.Entries {
		if err := q.InsertLedgerEntry(ctx, gen.InsertLedgerEntryParams{
			TransactionID: m.TransactionID,
			EntryIndex:    int32(i),
			AccountKind:   e.AccountKind,
			AccountUserID: e.UserID,
			AmountMinor:   e.Amount,
			Kind:          m.Kind,
			OccurredAt:    m.OccurredAt,
		}); err != nil {
			return fmt.Errorf("InsertLedgerEntry(%d): %w", i, err)
		}
	}
	return nil
}

// TestTheDatabaseRejectsAnUnbalancedLedgerTransactionAtCommit is the reference
// answer: what raw SQL against the real engine does, with none of our code in the
// path.
func TestTheDatabaseRejectsAnUnbalancedLedgerTransactionAtCommit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn := rawConn(t, sharedDatabase(t).dsn)
	user := newUser(t, ctx, conn)

	cases := []struct {
		name    string
		mutate  func(*ledgerMovement)
		wantMsg string
	}{
		{
			name: "two entries that do not sum to zero",
			mutate: func(m *ledgerMovement) {
				// 1000 credited, 900 debited. 100 minor units invented from
				// nothing — the exact defect the constraint exists to refuse.
				m.Entries[0].Amount = domain.Money(-900)
			},
			wantMsg: "UNBALANCED",
		},
		{
			name: "one entry only",
			mutate: func(m *ledgerMovement) {
				m.Entries = m.Entries[:1]
			},
			// "One entry cannot balance unless it is zero, and a zero entry is
			// not a movement of money" — and a zero entry is separately refused
			// by ledger_entries_amount_range.
			wantMsg: "entr(ies)",
		},
		{
			name: "header with no entries at all",
			mutate: func(m *ledgerMovement) {
				m.Entries = nil
			},
			wantMsg: "entr(ies)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := grantMovement(t, user, domain.Money(1000))
			tc.mutate(&m)

			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			// EVERY statement must succeed. This half of the assertion is what
			// proves the constraint is genuinely deferred; if an INSERT failed
			// here, the constraint would be firing per row and a balanced
			// two-statement movement could never be written at all.
			if err := writeMovementRaw(ctx, tx, m); err != nil {
				t.Fatalf("a statement inside the transaction failed: %v\n"+
					"The balance assertion is supposed to be DEFERRABLE INITIALLY DEFERRED, "+
					"so every INSERT must succeed and only COMMIT may refuse.", err)
			}

			commitErr := tx.Commit(ctx)
			if commitErr == nil {
				t.Fatal("COMMIT succeeded on an unbalanced ledger transaction. " +
					"Balances are derived from these rows, so this is a wrong balance forever.")
			}
			if got := postgres.SQLState(commitErr); got != "23514" {
				t.Errorf("COMMIT failed with SQLSTATE %q, want 23514 (check_violation): %v", got, commitErr)
			}
			if !strings.Contains(commitErr.Error(), tc.wantMsg) {
				t.Errorf("COMMIT error does not mention %q, so an operator cannot tell which rule broke: %v",
					tc.wantMsg, commitErr)
			}

			// Nothing persisted. Postgres rolls back a transaction whose COMMIT
			// fails, so the failure is complete as well as final.
			if n := scalarInt(t, ctx, conn,
				`SELECT count(*) FROM ledger_transactions WHERE id = $1`, m.TransactionID); n != 0 {
				t.Errorf("%d ledger_transactions rows survived a failed COMMIT", n)
			}
			if n := scalarInt(t, ctx, conn,
				`SELECT count(*) FROM ledger_entries WHERE transaction_id = $1`, m.TransactionID); n != 0 {
				t.Errorf("%d ledger_entries rows survived a failed COMMIT", n)
			}
		})
	}
}

// TestInTxSurfacesTheDeferredLedgerViolationRaisedByCommit is the test this whole
// file exists for.
//
// postgres.InTx runs a callback and commits when it returns nil. The callback here
// returns nil — every statement in it succeeded — and the movement is still invalid.
// If InTx returned nil in that situation, phase 8 would report a settled wager and a
// paid customer for a movement the database refused, and because balances are
// derived there would be no stored value anywhere that disagreed.
//
// Four things are asserted, and all four matter:
//
//	the callback saw no error       — proving the deferral is real
//	InTx returned an error          — proving it did not swallow the commit failure
//	SQLSTATE 23514 survived         — proving the caller can tell WHICH invariant broke
//	nothing persisted               — proving the failure was complete
func TestInTxSurfacesTheDeferredLedgerViolationRaisedByCommit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	pool, reg := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	user := newUser(t, ctx, conn)

	m := grantMovement(t, user, domain.Money(2500))
	m.Entries[0].Amount = domain.Money(-2499) // one minor unit short

	var statementErrs []error
	calls := 0

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		calls++
		if err := writeMovementRaw(ctx, tx, m); err != nil {
			statementErrs = append(statementErrs, err)
		}
		// Deliberately nil: the callback believes it succeeded, because as far as
		// the statements are concerned it did.
		return nil
	})

	if len(statementErrs) != 0 {
		t.Fatalf("statements inside the transaction failed: %v", statementErrs)
	}
	if err == nil {
		t.Fatal("InTx returned nil for a transaction whose COMMIT was rejected. " +
			"A helper that swallows a commit-time constraint violation reports a ledger movement as " +
			"written when the database refused it — and because balances are derived (CLAUDE.md §4), " +
			"that is a wrong balance forever rather than a visible failure once.")
	}
	if !postgres.IsCheckViolation(err) {
		t.Errorf("InTx error is not a check violation (SQLSTATE %q): %v", postgres.SQLState(err), err)
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Errorf("InTx error does not say the failure came from COMMIT, which is the one fact that "+
			"tells a caller its statements were fine and the SET of them was not: %v", err)
	}
	if !strings.Contains(err.Error(), "UNBALANCED") {
		t.Errorf("the database's own message was lost on the way out of InTx: %v", err)
	}

	// InTx must NOT retry. A hidden retry of a ledger write is a double-applied
	// movement, which is why internal/platform/postgres has no retry helper at
	// all — the exclusion of SQLSTATE 08007 from the retryable set is the same
	// decision.
	if calls != 1 {
		t.Errorf("InTx invoked the callback %d times; a ledger write must never be retried silently", calls)
	}

	if n := scalarInt(t, ctx, conn,
		`SELECT count(*) FROM ledger_entries WHERE transaction_id = $1`, m.TransactionID); n != 0 {
		t.Errorf("%d entries persisted despite the failed commit", n)
	}

	// The outcome is observable. This is the series a Grafana alert would fire on,
	// and it must be `commit_failed` rather than `rolled_back` — the two mean
	// different things and only one of them is a rejected invariant.
	assertCounter(t, reg, "sharpline_db_transactions_total",
		map[string]string{"outcome": "commit_failed"}, 1)
	assertCounter(t, reg, "sharpline_db_transactions_total",
		map[string]string{"outcome": "committed"}, 0)
}

// TestInTxCommitsABalancedLedgerMovementThroughTheGeneratedQueries is the positive
// half, driven the way phase 8 will drive it: sqlc's generated queries bound to the
// transaction InTx owns.
func TestInTxCommitsABalancedLedgerMovementThroughTheGeneratedQueries(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	pool, reg := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	queries := gen.New(pool.Pool())
	user := newUser(t, ctx, conn)

	const amount = domain.Money(123_456)
	m := grantMovement(t, user, amount)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return writeMovementGenerated(ctx, queries.WithTx(tx), m)
	}); err != nil {
		t.Fatalf("InTx on a balanced movement: %v", err)
	}

	assertCounter(t, reg, "sharpline_db_transactions_total",
		map[string]string{"outcome": "committed"}, 1)

	// Both halves are on disk, with the entry ordinals the domain built them in.
	if n := scalarInt(t, ctx, conn,
		`SELECT count(*) FROM ledger_entries WHERE transaction_id = $1`, m.TransactionID); n != 2 {
		t.Fatalf("%d entries committed, want 2", n)
	}
	if sum := scalarInt64(t, ctx, conn,
		`SELECT sum(amount_minor) FROM ledger_entries WHERE transaction_id = $1`, m.TransactionID); sum != 0 {
		t.Errorf("committed entries sum to %d minor units, want 0", sum)
	}

	// And the balance is readable through the generated query, as domain.Money.
	balance, err := queries.GetAccountBalance(ctx, gen.GetAccountBalanceParams{
		AccountKind:   domain.AccountKindUserCash.String(),
		AccountUserID: &user,
	})
	if err != nil {
		t.Fatalf("GetAccountBalance: %v", err)
	}
	if balance.BalanceMinor != amount {
		t.Errorf("derived balance is %s, want %s", balance.BalanceMinor, amount)
	}
	if balance.EntryCount != 1 {
		t.Errorf("account_balances reports %d entries on this account, want 1", balance.EntryCount)
	}
}

// TestTheDerivedBalanceMatchesAnIndependentFoldOverTheEntries closes the loop on
// CLAUDE.md §4.
//
// account_balances is a plain view, so it cannot be stale — but "cannot be stale" is
// a property of the definition, and the definition could still be wrong. The
// assertion here is that the number the database folds equals the number Go folds
// from the same rows, using domain.Money's overflow-checked integer arithmetic.
//
// Floating point never enters it. That is the point of §12's "all money and stake
// values are integer minor units": a fold of nine movements in float64 and a fold in
// int64 would agree here and disagree at scale, and the failure would be silent.
func TestTheDerivedBalanceMatchesAnIndependentFoldOverTheEntries(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)
	queries := gen.New(pool.Pool())

	user := newUser(t, ctx, conn)

	// A brand-new account has NO ROW in the view, and that is deliberate rather
	// than a gap: "this account was touched and nets to nothing" is a different
	// fact from "this account does not exist". A caller must map ErrNoRows to
	// zero, so the test pins the behaviour it has to map.
	_, err := queries.GetAccountBalance(ctx, gen.GetAccountBalanceParams{
		AccountKind:   domain.AccountKindUserCash.String(),
		AccountUserID: &user,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetAccountBalance on an untouched account returned %v, want pgx.ErrNoRows "+
			"(the view reports one row per account that has ever been touched)", err)
	}

	// Amounts chosen so the running total is never a round number and the last
	// movement takes it back down — a fold that dropped or double-counted one
	// entry cannot coincidentally match.
	amounts := []domain.Money{500_00, 1_23, 99_999, 7, 250_000, 3_141_59, 1, 86_400, 12_345}
	signs := []int64{+1, +1, +1, +1, -1, +1, -1, +1, -1}

	var wantBalance domain.Money
	for i, a := range amounts {
		amount := a
		if signs[i] < 0 {
			// A negative grant is not a thing; use an adjustment, which is the one
			// kind ledger_transactions_wager_matches_kind lets carry either a
			// wager or none, and which is what an operator correction really is.
			neg, err := a.Neg()
			if err != nil {
				t.Fatalf("negate %s: %v", a, err)
			}
			amount = neg
		}

		other, err := amount.Neg()
		if err != nil {
			t.Fatalf("negate %s: %v", amount, err)
		}

		kind := domain.EntryKindGrant.String()
		if signs[i] < 0 {
			kind = domain.EntryKindAdjustment.String()
		}
		m := ledgerMovement{
			TransactionID: transactionID(t, uniqueID("txn")),
			Kind:          kind,
			OccurredAt:    time.Now().UTC().Add(time.Duration(i) * time.Second).Truncate(time.Microsecond),
			Entries: []ledgerEntry{
				{AccountKind: domain.AccountKindIssuance.String(), Amount: other},
				{AccountKind: domain.AccountKindUserCash.String(), UserID: &user, Amount: amount},
			},
		}
		if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return writeMovementGenerated(ctx, queries.WithTx(tx), m)
		}); err != nil {
			t.Fatalf("write movement %d: %v", i, err)
		}

		wantBalance, err = wantBalance.Add(amount)
		if err != nil {
			t.Fatalf("fold movement %d: %v", i, err)
		}
	}

	// 1. What the database derives.
	derived, err := queries.GetAccountBalance(ctx, gen.GetAccountBalanceParams{
		AccountKind:   domain.AccountKindUserCash.String(),
		AccountUserID: &user,
	})
	if err != nil {
		t.Fatalf("GetAccountBalance: %v", err)
	}

	// 2. What Go folds, independently, from the raw entries — read as int64 minor
	//    units and summed through domain.Money, not through the view.
	rows, err := conn.Query(ctx, `
SELECT amount_minor
  FROM ledger_entries
 WHERE account_kind = $1 AND account_user_id = $2
 ORDER BY occurred_at, transaction_id, entry_index`,
		domain.AccountKindUserCash.String(), user)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	defer rows.Close()

	var folded domain.Money
	entries := 0
	for rows.Next() {
		var minor int64
		if err := rows.Scan(&minor); err != nil {
			t.Fatalf("scan entry: %v", err)
		}
		folded, err = folded.Add(domain.Money(minor))
		if err != nil {
			t.Fatalf("fold entry %d: %v", entries, err)
		}
		entries++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate entries: %v", err)
	}

	if entries != len(amounts) {
		t.Fatalf("folded %d entries, wrote %d movements", entries, len(amounts))
	}
	if folded != wantBalance {
		t.Fatalf("the Go fold of the stored rows is %s, but the fold of what was written is %s; "+
			"a movement did not land as written", folded, wantBalance)
	}
	if derived.BalanceMinor != folded {
		t.Errorf("account_balances derives %s; an independent fold over the same entries gives %s.\n"+
			"The view is the ONLY correct source for a balance, so a disagreement here is an overdraft waiting to happen.",
			derived.BalanceMinor, folded)
	}
	if derived.EntryCount != int64(entries) {
		t.Errorf("account_balances counts %d entries, the fold saw %d", derived.EntryCount, entries)
	}

	// 3. And the ledger as a whole is still sound.
	//
	// ledger_integrity is migration 00006's independent audit of the guarantee the
	// deferred trigger provides — "if it ever reports non-zero, a constraint has
	// been disabled". It is asserted globally rather than for this user because
	// the invariant is global: no test in this package can commit an unbalanced
	// movement, so a non-zero reading here indicts the schema, not the caller.
	var total int64
	var txCount, entryCount, unbalanced, empty int64
	err = conn.QueryRow(ctx, `
SELECT total_minor, transaction_count, entry_count, unbalanced_transactions, empty_transactions
  FROM ledger_integrity`).Scan(&total, &txCount, &entryCount, &unbalanced, &empty)
	if err != nil {
		t.Fatalf("read ledger_integrity: %v", err)
	}
	if total != 0 {
		t.Errorf("ledger_integrity.total_minor = %d, want 0: money has been created or destroyed", total)
	}
	if unbalanced != 0 {
		t.Errorf("ledger_integrity.unbalanced_transactions = %d, want 0", unbalanced)
	}
	if empty != 0 {
		t.Errorf("ledger_integrity.empty_transactions = %d, want 0", empty)
	}
	if txCount == 0 || entryCount == 0 {
		t.Errorf("ledger_integrity reports %d transactions and %d entries; this test alone wrote %d and %d, "+
			"so the view is not reading the tables the test wrote to",
			txCount, entryCount, len(amounts), len(amounts)*2)
	}
}

// TestAnUnbalancedMovementThroughTheGeneratedQueriesIsAlsoRejected closes the third
// direction: the generated code has no more privilege than raw SQL does.
func TestAnUnbalancedMovementThroughTheGeneratedQueriesIsAlsoRejected(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)
	queries := gen.New(pool.Pool())

	user := newUser(t, ctx, conn)
	m := grantMovement(t, user, domain.Money(10_000))
	m.Entries = append(m.Entries, ledgerEntry{
		// A third entry, unmatched: the sum is now +1 rather than 0. Three entries
		// is a legitimate shape (a settlement can have more than two halves), so
		// this is the case a "count == 2" check would wave through.
		AccountKind: domain.AccountKindHouse.String(),
		Amount:      domain.Money(1),
	})

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return writeMovementGenerated(ctx, queries.WithTx(tx), m)
	})
	if err == nil {
		t.Fatal("a three-entry movement summing to +1 committed through the generated queries")
	}
	if !postgres.IsCheckViolation(err) {
		t.Errorf("error is not a check violation (SQLSTATE %q): %v", postgres.SQLState(err), err)
	}
	if n := scalarInt(t, ctx, conn,
		`SELECT count(*) FROM ledger_entries WHERE transaction_id = $1`, m.TransactionID); n != 0 {
		t.Errorf("%d entries persisted", n)
	}
}
