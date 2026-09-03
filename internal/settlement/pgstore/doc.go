// Package pgstore is the Postgres implementation of the seams
// internal/settlement declares: [Results] behind settlement.ResultsSource, and
// [Store] behind settlement.Store and settlement.Tx.
//
// It is a separate package from internal/settlement for the reason that package
// gives itself: grading rules and money rules are asserted against a fake in a
// unit test and against a real Postgres in the integration tier, with the same
// code under test, and that is only true while internal/settlement does not
// import a database driver. Everything here is running generated queries,
// translating rows, and classifying failure. There is no grading rule in this
// package, and a review that finds one should move it to grader.go.
//
// # The results feed is not fixture data — but in the deployed stack it is
// # currently EMPTY, and the reason is upstream of this package
//
// [Results.Since] reads the `events` table for rows that reached a terminal
// status. The intent is that those rows are written by internal/ingest/writer
// from payloads the synthetic provider produced: the provider does advance each
// event through scheduled -> live -> ended carrying a domain.Score derived from
// THE SAME latent process that prices its markets
// (internal/ingest/provider/synthetic/model.go), so the score and the line are
// two views of one simulated game rather than two independent inventions. Read
// that way this is a live generator's own output out of the pipeline's own
// storage, rather than a seeded results table — which CLAUDE.md forbids, and
// which would in any case grade wagers against numbers no market ever priced.
//
// THE PROVIDER PRODUCES THAT STATE AND THE PIPELINE DOES NOT CARRY IT. Verified
// against a running stack: `events` holds only 'scheduled' and 'live', and
// score_home/score_away are NULL on every row. The loss is at the wire contract,
// two hops upstream:
//
//   - normalizer.RawEvent (raw.go) has no status, score or completed field, so a
//     provider cannot state a result even when it knows one. The synthetic
//     adapter's Score is dropped at rawEventFor.
//   - normalizer's mapper derives status from timestamps alone — scheduled, or
//     live when an observation postdates the commence time — so no code path
//     produces EventStatusEnded at all.
//   - normalizer/payload.go excludes score and clock from odds.normalized ON
//     PURPOSE and for a good reason (a record carrying a live score would be
//     republished for every market on every score change, which is the bus flood
//     change detection exists to prevent). That argument is sound and is not the
//     thing to overturn.
//
// So ListFinalisedEventsSince returns zero rows for ever, and `settle` polls a
// well-formed query against a table that cannot answer it. Nothing in THIS
// package is wrong — [Results.Since] correctly skips an ended row with no score
// rather than grading a spread against 0-0 — and the four settlement outcomes
// are proven against real Postgres in the integration tier. What is missing is a
// feed.
//
// The fix is not to smuggle the score into odds.normalized. CLAUDE.md §3's event
// flow already shows results as their OWN source into `settle`, and the real
// candidate provider exposes them on a separate scores endpoint with its own
// quota cost — so the seam is a results adapter alongside provider.Adapter,
// written up in an ADR with that quota math, feeding status and final score into
// `events`. Whatever fills the table, this interface does not change and neither
// does internal/settlement.
//
// # Two rules this package exists to honour, and neither is optional
//
// ROWS-AFFECTED IS THE ANSWER, NOT AN OPTIMISATION. [Store]'s GradeLeg and
// SettleWager run UPDATEs whose WHERE clause carries the precondition — the leg
// is still pending, the ticket is not yet terminal. A zero row count means
// somebody else did this work, and it is reported as settlement.ErrLegAlreadyGraded
// or settlement.ErrWagerAlreadySettled. internal/settlement/ports.go states the
// consequence of getting it wrong plainly: "Returning nil from either on a
// zero-row update is the single most dangerous thing an implementation of this
// interface can do, because the ledger write that follows would balance
// perfectly and pay twice."
//
// EVERY LEDGER WRITE GOES THROUGH postgres.DB.InTx. migrations/00006 installs
// the double-entry assertion as DEFERRABLE INITIALLY DEFERRED, so an unbalanced
// movement returns SUCCESS from every INSERT and fails at COMMIT. postgres.InTx
// is the only helper in this tree that propagates a failed commit as an error,
// rolls back on a context detached from the caller's, and closes the transaction
// before re-raising a panic. [Store.InTx] is a delegation to it and nothing
// more; there is no BeginTx in this package.
//
// # Idempotency, and why a collision is not a failure
//
// internal/settlement derives the ledger transaction's identifier
// deterministically from the wager, so a replayed settlement collides on
// ledger_transactions' primary key instead of paying twice. [Store]'s
// InsertTransaction maps that 23505 to settlement.ErrTransactionExists, which
// the service treats as "already settled" rather than as an error. It is the
// same device internal/betting uses for placement — the derived primary key IS
// the idempotency record, so there is no second table and nothing to reconcile.
//
// # Why the wager row is locked and the legs are not
//
// [Store]'s WagerWithLegs runs SELECT ... FOR UPDATE. That is not what prevents
// a double payout — the guarded UPDATE and 00006's write-once returned amount
// already do, in that order — it is what makes two settle replicas grading two
// different events of one parlay WAIT for each other instead of both computing a
// settlement and one losing at COMMIT with a trigger exception.
//
// The legs are read unlocked in the same transaction, because each is guarded by
// its own UPDATE and no writer reaches a leg except through its wager, which
// this transaction now holds. queries/settlement.sql records the lock ORDER for
// the same reason: a settlement transaction takes exactly one row lock and a
// placement transaction takes exactly one, on different relations, so no cycle
// between them is constructible.
package pgstore
