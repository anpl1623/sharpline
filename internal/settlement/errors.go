package settlement

import "errors"

// Sentinel errors.
//
// CLAUDE.md §12 puts the DOMAIN's sentinels in the domain package, and this file
// deliberately declares none of those. domain.ErrIllegalTransition,
// domain.ErrReturnAmount, domain.ErrUnbalancedTransaction and
// domain.ErrLegNotGraded are the vocabulary for "this money movement is not
// legal", they are already exhaustively tested there, and a parallel spelling
// here would give callers two errors to match for one condition.
//
// What is here is the vocabulary for the things the SERVICE can fail at:
// wiring, the results feed, and the four store preconditions that a concurrent
// settle replica can take away. Match with errors.Is, never on message text.
var (
	// ErrInvalidOptions is returned by every constructor in this package when
	// its options do not validate. Configuration fails at construction, loudly,
	// rather than at the first result — a settle service that started with no
	// publisher would grade tickets and move money with no audit trail, and the
	// first sign of it would be an empty topic nobody was looking at.
	ErrInvalidOptions = errors.New("settlement: invalid options")

	// ErrNotRunning is what the readiness checker reports before Run has
	// started the poll loop and after it has stopped.
	//
	// It answers a question the Postgres checker does not. That one reports
	// that the database is reachable; this reports that THIS PROCESS is
	// actually settling. A replica whose loop has exited but whose listener is
	// still up would otherwise look healthy while every finished game sat
	// ungraded and every customer's stake sat in escrow.
	ErrNotRunning = errors.New("settlement: service is not running")

	// ErrUnusableResult reports a row the results source should not have
	// returned: a status that is not terminal, an ended event with no final
	// score, a scored event that is not ended, a malformed identifier, or no
	// finalisation instant.
	//
	// It is PERMANENT for that row. Re-reading the same row cannot change it,
	// so settlement counts it, logs it once, and moves the cursor past it
	// rather than blocking every later result behind it. A source producing
	// these is producing a bug, and the metric is where that becomes visible.
	ErrUnusableResult = errors.New("settlement: results source returned a row that is not a result")

	// ErrUnusableLeg reports a pending-leg row that cannot be graded: a
	// malformed identifier, an unrecognised market type or selection role, or a
	// role the market type does not admit.
	//
	// Also permanent, and also skipped rather than retried — but unlike an
	// unusable result it is never skipped SILENTLY. A leg the grader cannot
	// read is a customer's stake that will sit in escrow for ever, so it is
	// logged at ERROR with the leg and wager identifiers on it.
	ErrUnusableLeg = errors.New("settlement: pending leg cannot be graded")

	// ErrUngradableMarket reports a market type this build has no grading rule
	// for at all.
	//
	// It is distinct from a market the grader deliberately VOIDS. A player prop
	// is voided, with a reason, because the results feed genuinely does not
	// carry the statistic it asks about — that is a decision, and it is
	// recorded as one. This sentinel is for a market type that reached the
	// grader without a decision having been made about it, which can only mean
	// domain.MarketType grew a member and grader.go was not updated. Refusing
	// is the only safe answer: guessing would pay or confiscate on a rule
	// nobody wrote.
	ErrUngradableMarket = errors.New("settlement: no grading rule for this market type")

	// ErrLegAlreadyGraded reports a [Tx.GradeLeg] that matched no row because
	// the leg was no longer pending.
	//
	// It is a CONFLICT, not a failure: another settle replica graded it between
	// this transaction's read and its write. The correct response is to abandon
	// this attempt at the wager and let the next poll re-read it, because the
	// hydrated ticket this transaction is computing a payout from no longer
	// describes the row in the database.
	ErrLegAlreadyGraded = errors.New("settlement: leg was already graded")

	// ErrWagerAlreadySettled reports a [Tx.SettleWager] that matched no row
	// because the ticket had already reached a terminal status.
	//
	// The same conflict, one level up, and the same response. It is the
	// in-Go counterpart of the wagers_assert_transition trigger's refusal, and
	// it exists so that the common case — a redelivered result, a second
	// replica — costs a rolled-back transaction rather than a raised exception
	// from PL/pgSQL with a SQLSTATE the caller has to decode.
	ErrWagerAlreadySettled = errors.New("settlement: wager was already settled")

	// ErrWagerNotFound reports a [Tx.WagerWithLegs] for a ticket that does not
	// exist. Genuinely exceptional: the identifier came off a leg row read in
	// the same transaction, and legs.wager_id is a foreign key with ON DELETE
	// RESTRICT onto a table whose DELETE trigger refuses outright.
	ErrWagerNotFound = errors.New("settlement: wager not found")

	// ErrTransactionExists reports a [Tx.InsertTransaction] that collided on
	// the primary key.
	//
	// This is the idempotency guard doing its job, not an error in the ordinary
	// sense. settle.go derives the ledger transaction's identifier
	// deterministically from the wager identifier precisely so that a replayed
	// settlement collides here instead of writing a second, perfectly balanced,
	// entirely duplicate payout. See [SettlementTransactionID].
	ErrTransactionExists = errors.New("settlement: a ledger transaction already exists for this wager")

	// ErrNoTeaserSchedule reports a teaser that cannot be repriced because one
	// of its legs voided or pushed.
	//
	// It is not raised as a failure — settle.go catches it and refunds the
	// ticket — but it is a named condition rather than an inline branch, so the
	// house policy is greppable and testable. grader.go and settle.go both
	// carry the argument for why a refund is the only rule this system can
	// honestly apply.
	ErrNoTeaserSchedule = errors.New("settlement: a teaser with a removed leg cannot be repriced")
)
