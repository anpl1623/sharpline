// Transaction helper.
//
// The goal of this file is narrow and worth stating: make the correct thing the
// easy thing. Hand-rolled `tx, _ := pool.Begin(...)` followed by a `defer
// tx.Rollback(ctx)` is the idiom everybody writes, and it is wrong in three ways
// that all matter here.
//
//  1. It discards the rollback error. When rollback is the thing that failed,
//     the caller learns nothing and the connection is quietly poisoned.
//  2. It rolls back with the caller's context. If that context is what failed —
//     a deadline expiring mid-transaction is the common case — the rollback is
//     cancelled before it reaches the server, and the backend sits idle in
//     transaction holding the xmin horizon until
//     idle_in_transaction_session_timeout (60s, deploy/postgres/postgresql.conf)
//     kills it.
//  3. It does nothing about a panic. A panic through an open transaction leaves
//     the connection checked out and the transaction open for the same 60s.
//
// InTx fixes all three, and one more thing that is specific to this schema.
//
// # The deferred constraint, and why Commit's error is the interesting one
//
// migrations/00006 installs the double-entry assertion as
//
//	CREATE CONSTRAINT TRIGGER ledger_entries_balanced_at_commit
//	    AFTER INSERT OR UPDATE OR DELETE ON ledger_entries
//	    DEFERRABLE INITIALLY DEFERRED
//	    FOR EACH ROW EXECUTE FUNCTION ledger_assert_transaction_balanced();
//
// DEFERRABLE INITIALLY DEFERRED is not an optimisation, it is the design: a
// balanced movement is two rows, and the first INSERT of any two-row movement is
// transiently unbalanced. A NOT DEFERRABLE trigger would reject every correct
// write. Deferring it moves the check to COMMIT.
//
// The consequence for this file: **an unbalanced ledger write returns SUCCESS
// from every INSERT and fails at COMMIT.** A helper that ignores Commit's error,
// or that returns nil after a failed commit, reports a ledger movement as
// written when the database rejected it — and the double-entry ledger is the
// system's source of truth (CLAUDE.md §4: "Balances are derived, never stored as
// a mutable field", which means a phantom movement is silently wrong forever
// rather than visibly wrong once).
//
// InTx therefore propagates the commit error faithfully and counts it under
// outcome="commit_failed". tx_test.go asserts exactly this, against a real
// Postgres with a real DEFERRABLE INITIALLY DEFERRED constraint trigger, because
// it is the subtle bug this helper exists to prevent.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// TxFunc is the body of a transaction.
//
// The pgx.Tx it receives satisfies sqlc's generated DBTX parameter, so
// `queries.WithTx(tx)` inside the callback is the intended usage and the reason
// this signature is shaped the way it is.
//
// Contract:
//
//   - Return an error to roll back. Return nil to commit.
//   - Do NOT call Commit or Rollback. InTx owns the transaction's lifetime; a
//     callback that ends the transaction itself makes the outcome ambiguous, and
//     InTx will say so loudly rather than guess.
//   - Use the ctx passed in, not a captured one. It carries the caller's
//     deadline and the trace span this statement should hang from.
type TxFunc func(ctx context.Context, tx pgx.Tx) error

// InTx runs fn inside a transaction at the server's default isolation level
// (READ COMMITTED, per deploy/postgres/postgresql.conf, which does not override
// default_transaction_isolation).
//
// Read committed is sufficient for the ledger: the deferred balance assertion
// runs inside the writing transaction and sees that transaction's own rows, so
// two concurrent balanced movements are each independently valid. Anything that
// needs a stronger guarantee — reading a balance and writing based on it — must
// say so with InTxOptions and pgx.Serializable, and must be prepared for 40001
// (see IsSerializationFailure).
//
// Behaviour:
//
//	fn returns nil       -> COMMIT. A failed COMMIT is returned as an error.
//	fn returns an error  -> ROLLBACK, and fn's error is returned. If the rollback
//	                        ALSO fails, both errors are returned joined, never
//	                        one instead of the other.
//	fn panics            -> ROLLBACK, then the panic is re-raised unchanged.
func (db *DB) InTx(ctx context.Context, fn TxFunc) error {
	return db.InTxOptions(ctx, pgx.TxOptions{}, fn)
}

// InTxOptions is InTx with explicit transaction options: isolation level, access
// mode, deferrable mode.
func (db *DB) InTxOptions(ctx context.Context, txOpts pgx.TxOptions, fn TxFunc) error {
	if fn == nil {
		return fmt.Errorf("%w: nil transaction function", ErrInvalidOptions)
	}
	if db.closed.isSet() {
		return ErrClosed
	}

	start := time.Now()

	// Pessimistic default: if this function leaves by any route that does not
	// set an outcome, it left before BEGIN returned.
	outcome := txBeginFailed

	// Registered FIRST, so it runs LAST — after the panic guard below has
	// recorded outcome = panicked, and while the runtime is still unwinding.
	// A transaction that panics is still a transaction that happened.
	defer func() {
		db.metrics.observeTx(outcome, time.Since(start))
	}()

	tx, err := db.pool.BeginTx(ctx, txOpts)
	if err != nil {
		return fmt.Errorf("postgres: begin transaction: %w", err)
	}

	// Registered SECOND, so it runs FIRST. Nothing between BEGIN and here can
	// panic, so there is no window where an open transaction is unguarded.
	//
	// The panic is re-raised, not converted to an error: CLAUDE.md §12 says
	// "never panic outside main", so a panic reaching here is a bug in the
	// callback, and swallowing it would hide the bug and hand the caller a
	// database error for a nil-map write. What this guard buys is that the
	// transaction is closed and the connection returned before the process
	// unwinds — the panic may be recovered further up (an HTTP handler), and
	// leaving a poisoned connection in the pool would turn one bad request into
	// a leaked connection per occurrence.
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		outcome = txPanicked
		if rbErr := db.rollback(ctx, tx); rbErr != nil {
			db.log.Error("rollback after panic in transaction body failed",
				slog.String("error", rbErr.Error()),
				slog.Any("panic", p),
			)
		}
		panic(p)
	}()

	if fnErr := fn(ctx, tx); fnErr != nil {
		outcome = txRolledBack
		if rbErr := db.rollback(ctx, tx); rbErr != nil {
			// Both errors, joined. The rollback failure is not noise: it means
			// the transaction's actual state is unknown, which is strictly worse
			// news than the failure that triggered it.
			return errors.Join(
				fmt.Errorf("postgres: transaction body: %w", fnErr),
				fmt.Errorf("postgres: rollback after that failure: %w", rbErr),
			)
		}
		return fmt.Errorf("postgres: transaction body: %w", fnErr)
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		outcome = txCommitFailed

		// This is the path the deferred ledger constraint takes. Every statement
		// in the body succeeded; the server rejected the set of them at COMMIT.
		// Nothing was written — Postgres rolls back a transaction whose COMMIT
		// fails — so the error is final and complete, and it is returned rather
		// than being flattened into a generic failure, because its SQLSTATE
		// (23514 for the balance assertion) is what tells the caller which
		// invariant it broke.
		//
		// One exception is worth naming: 08007 transaction_resolution_unknown
		// means the outcome is genuinely unknown, and IsTransientConnectError
		// deliberately does NOT classify it as retryable for that reason.
		db.log.Error("transaction commit rejected",
			slog.String("sqlstate", SQLState(commitErr)),
			slog.String("error", commitErr.Error()),
		)
		return fmt.Errorf("postgres: commit: %w", commitErr)
	}

	outcome = txCommitted
	return nil
}

// rollback ends a transaction that must not be committed.
//
// The context is ALWAYS detached from the caller's, with its own timeout. Not
// "detached when the caller's context is already cancelled" — always, because
// the common failure is a deadline that has nearly expired rather than one that
// already has, and a rollback that is cancelled 5ms into its round trip leaves
// exactly the idle-in-transaction backend this function exists to avoid. The
// cost is bounded: at most RollbackTimeout (5s by default) added to a shutdown,
// against httpx's 15s drain budget.
func (db *DB) rollback(ctx context.Context, tx pgx.Tx) error {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), db.rollbackTimeout)
	defer cancel()

	err := tx.Rollback(rbCtx)
	switch {
	case err == nil:
		return nil

	case errors.Is(err, pgx.ErrTxClosed):
		// The callback ended the transaction itself, against the TxFunc
		// contract. Not returned as an error — the caller already has a real
		// error to look at on this path — but logged loudly, because if the
		// callback COMMITTED and then returned an error, the caller is about to
		// be told the transaction failed when its writes are durable. That is
		// the one way this helper can lie, and it requires the callback to
		// violate its documented contract to get there.
		db.log.Error("transaction body committed or rolled back the transaction itself; "+
			"the reported outcome may not match what is in the database",
			slog.String("contract", "TxFunc must not call Commit or Rollback"),
		)
		return nil

	default:
		return fmt.Errorf("postgres: rollback: %w", err)
	}
}
