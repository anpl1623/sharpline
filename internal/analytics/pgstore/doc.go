// Package pgstore is the Postgres implementation of the [analytics.Store] seam.
//
// It is a separate package from internal/analytics on purpose, for the reason
// internal/betting/pgstore and internal/auth/pgstore both give about themselves:
// the DETECTION RULES — the expected-value threshold, the arbitrage staleness
// discipline, the steam window and its lead/follower semantics — are testable
// against nothing at all, and keeping them in a package that does not import a
// database driver is what makes that true rather than aspirational. Phase 12
// validates those rules against a Flink SQL job, so the ability to run them over
// a hand-built sequence of observations with no container anywhere is not a
// convenience; it is what makes the comparison possible.
//
// Everything here is SQL, value translation and error wrapping. There is no
// policy in this package, and a review that finds one should move it.
//
// # Three jobs, and nothing else
//
//  1. RUN THE GENERATED QUERIES. Every statement this package executes is a named
//     query in internal/platform/postgres/queries/analytics.sql, which is what
//     keeps the whole database surface inside one `sqlc diff` drift gate and one
//     `make query-plans` index check. There is no hand-written SQL here.
//
//  2. TRANSLATE THE FINDING INTO THE ROW. sqlc parameters carry pgtype.Float8 for
//     a nullable line and []byte for a JSONB column, because that is what the
//     columns are. internal/analytics should not know that, and a column type
//     change should not be a detector change.
//
//  3. WRAP FAILURE WITH ENOUGH CONTEXT TO ACT ON. A constraint violation here is
//     a disagreement between a detector and migrations/00009 about what a finding
//     may look like, and the SQLSTATE plus the constraint name is what says which
//     rule was broken. internal/analytics validates every finding against the same
//     rules BEFORE calling this package (see its validate.go), so a violation
//     reaching Postgres means the two have drifted — which is worth an error
//     message that says so rather than a bare wrap.
//
// # There is no read here, and there will not be one
//
// [analytics.Store] is three writes. The QUERY surface over these tables — the
// +EV board, the live arbitrage list, the steam feed, the CLV history, the
// leaderboard — belongs to `api` and is a different port in a different package,
// because it has different concerns entirely: cursors, filters, league scoping,
// and a public leaderboard that must not leak a column of `users`.
//
// Adding a read here would also invite the one bug this design has been built to
// exclude. Every write below is an UPSERT on a replay key derived from the input
// alone, so there is no check-then-write and therefore no race: two replicas
// recomputing the same finding write the same row, and a detector fix replayed
// over yesterday's log CORRECTS yesterday's rows rather than duplicating them.
// A read seam would make "does this already exist?" expressible, and the moment
// it is expressible somebody writes it.
//
// # Why the arbitrage write is the only one in a transaction
//
// A finding and its outcome set are ONE FACT. migrations/00009 cascades the
// delete from arbitrage_signals to arbitrage_signal_legs, and the recomputation
// path is delete-then-reinsert rather than merge, so a caller that wrote the
// parent and failed before the legs would leave a finding with no outcomes —
// indistinguishable, to a reader, from a finding whose legs have not been written
// yet. The three statements therefore run inside one [postgres.DB.InTx].
//
// The other two writes are single statements and a transaction around one
// statement is the transaction Postgres was going to open anyway.
//
// # InTx is a delegation, and that is not negotiable
//
// [postgres.DB.InTx] is the only helper in this tree that propagates a failed
// COMMIT as an error, rolls back on a context DETACHED from the caller's so a
// dying deadline cannot leave a backend idle in transaction, and re-raises a
// panic after closing the transaction. internal/betting/pgstore's doc.go makes
// the full argument against the hand-rolled `tx, _ := pool.Begin(...)` with
// `defer tx.Rollback(ctx)` idiom, and it applies here unchanged. There is no
// BeginTx in this package.
package pgstore
