// Package settlement is the settle service: it reads the results feed, grades
// every ungraded leg riding on a finished event, decides what each affected
// ticket is worth, writes the double-entry ledger movement that pays it, and
// publishes the settlement to wager.events.
//
// CLAUDE.md §3 gives it one line in the event flow
//
//	results ──▶ settle ──▶ [wager.events] ──▶ ledger ──▶ stream ──▶ browser
//
// and one responsibility in the service table: "Consumes results feed, grades
// open wagers, writes ledger entries, emits settlement events."
//
// It is the last service in the system to touch a customer's money, and it is
// the only one that touches it without a customer present. Everything below is
// organised around that: a wrong price on the board is visible and corrects
// itself on the next poll, and a wrong settlement is a silent, permanent,
// unrecoverable transfer.
//
// # What is here
//
//	ports.go     the seams — the results feed, the store, the audit publisher —
//	             and the neutral values that cross them
//	grader.go    PURE grading: market type, role, grading line and final score
//	             in, a domain.LegStatus out. No I/O, no clock, no state.
//	settle.go    the poll loop, the per-wager transaction, and the money rules
//	             for what a graded ticket returns
//	payload.go   the wager.events document
//	metrics.go   the Prometheus contract
//
// The odds mathematics is NOT here, and neither is the ledger's balance rule.
// domain.Leg.GradedMultiplier is the parlay rule, domain.Wager.Settle is the
// returned-amount rule, and domain.NewSettlementTransaction is the three-entry
// movement that balances by algebra rather than by four per-outcome formulas.
// This package composes those; it re-derives none of them. CLAUDE.md §10 is
// blunt about why: two implementations of one money formula eventually disagree,
// and here the disagreement is a payout.
//
// # THE RESULTS FEED IS PIPELINE OUTPUT, NOT FIXTURE DATA
//
// This is the decision most likely to be misread, so it is stated in full.
//
// [ResultsSource] is a port, and the shipped implementation polls the `events`
// table for events that have reached status `ended` with a final score, or
// status `cancelled`. That sounds, at a glance, like reading seeded rows. It is
// not, and the distinction is exact.
//
// The synthetic provider (internal/ingest/provider/synthetic) is a stochastic
// market maker, not a fixture file. CLAUDE.md §5 specifies it as one — "a
// synthetic provider that runs a stochastic market maker generating realistic
// line movement, steam moves, and book disagreement" — and its model.go carries
// a single latent process per event from which BOTH the prices it quotes AND the
// score it reports are derived: newEventState advances an event through
// scheduled → live → ended against the wall clock, and scoreAt evaluates the
// score at the fraction of the contest that has elapsed. The moneyline the
// customer bet is a function of that latent state; so is the final score the bet
// is graded against. They are two views of one simulated game, which is exactly
// the relationship a real book has with a real one.
//
// internal/ingest/writer then persists that score and that status to `events`,
// through the same normalizer, the same Kafka topic and the same transaction
// discipline as every price in the system. So by the time settlement reads it,
// a final score has travelled the entire pipeline: generator → ingest →
// odds.raw → normalizer → odds.normalized → writer → Postgres.
//
// That is why reading `events` is reading the feed rather than reading a
// fixture. Nothing in this package, in its tests' shipped path, or in the
// running system writes a score for settlement's benefit. If the generator is
// stopped, no results appear and nothing settles — which is the correct
// behaviour and is itself the evidence that the data is not fabricated.
//
// The alternative designs were considered and rejected. A separate `results`
// topic would need a producer that re-derived the same finals from the same
// generator, which is a second copy of one fact. A provider results endpoint
// does not exist until a provider is chosen (CLAUDE.md §13, open decision 1),
// and the whole point of the [ResultsSource] port is that the real adapter drops
// in behind it without settle.go changing at all.
//
// What a synthetic feed can and cannot grade is stated honestly in grader.go
// rather than papered over: a player prop asks about an individual's
// performance and this feed carries a team score, so a player-prop leg is
// VOIDED with a reason instead of guessed at.
//
// # THE AUDIT TRAIL IS PUBLISHED BEFORE THE TRANSACTION COMMITS
//
// The wager.events publish happens INSIDE the database transaction, as its last
// statement, before COMMIT. A publish failure therefore returns an error from
// the transaction body, postgres.InTx rolls back, and the settlement does not
// happen.
//
// The ordering is the whole point. Publishing after the commit would mean a
// broker outage produces a settled wager, a paid customer, a moved ledger — and
// no event on the audit trail. CLAUDE.md §3 calls wager.events "the settlement
// audit trail"; an audit trail that is missing an event that happened is worse
// than no audit trail, because it is trusted. Publishing before the commit
// inverts which way the system fails: the worst case becomes an event on the
// topic for a settlement that was rolled back, which is a DUPLICATE-looking
// record that the ledger contradicts, and the ledger is the source of truth
// (CLAUDE.md §4). A consumer reconciling the two finds the discrepancy; a
// consumer reading a topic with a hole in it finds nothing.
//
// kafka.AuditProducer is built for exactly this posture and no other producer in
// the tree is: PublishWagerEvent is synchronous with no asynchronous sibling, it
// retries without bound, and its only limit is the caller's context deadline.
// internal/platform/kafka/producer.go says why in its own words — "retrying for
// ever with a synchronous Publish means the caller stays blocked and can refuse
// to commit the surrounding database transaction, which is the correct failure".
// This package is the caller that refuses.
//
// The residual window is named rather than hidden. Between the broker's
// acknowledgement and the COMMIT there is a moment in which the event exists and
// the settlement does not, and Kafka transactions cannot span Postgres. The
// closing move is a transactional outbox — write the event to a Postgres table
// in the same transaction and relay it afterwards — and the producer's own
// comment already anticipates that as a phase 8 decision. It is deliberately NOT
// taken here: an outbox adds a table, a relay process and an at-least-once
// delivery path, and it converts a window measured in milliseconds into a
// permanent second moving part. The window as it stands is closed by
// reconciliation, and the payload carries everything reconciliation needs.
//
// # SETTLEMENT IS IDEMPOTENT BY CONSTRUCTION, IN THREE INDEPENDENT PLACES
//
// A result will be delivered twice. The cursor's boundary is inclusive on
// purpose, two replicas may hold the same event, and a restart re-reads from
// the oldest open ticket. So "grade this wager" happens more than once as a
// matter of routine, and none of the three guards below is the primary one —
// each catches what the others cannot.
//
//  1. The LEG update is conditional on the leg still being pending, and
//     [Tx.GradeLeg] reports a zero-row update as [ErrLegAlreadyGraded]. The
//     legs_assert_transition trigger refuses the same write from any other
//     client, including `make psql`.
//
//  2. The WAGER update is conditional on the ticket not being terminal, and the
//     wagers_assert_transition trigger makes the returned amount write-once with
//     its own exception. A second settlement is a hard error at the database,
//     not a silent overwrite.
//
//  3. The LEDGER transaction identifier is DERIVED DETERMINISTICALLY from the
//     wager identifier ([SettlementTransactionID]), so a replay attempts to
//     insert the same primary key and collides. This is the guard that matters
//     most, because it is the only one that would still hold if the first two
//     were somehow bypassed — a duplicate payout would otherwise be a perfectly
//     balanced, entirely legal-looking pair of ledger rows that no constraint in
//     the schema would object to.
//
// The three are cheap and they are not redundant. Two of them are inside one
// transaction and would fail together; the third is a different table with a
// different key. Money is the one place in this system where belt and braces is
// the correct engineering, not a smell.
//
// # A FAILED TRANSACTION IS NEVER RETRIED IN PLACE
//
// There is no retry loop around the per-wager transaction. postgres.InTx has
// already rolled back by the time the error is returned, and the next poll
// re-reads the same result and tries again from a fresh read — which is a retry,
// but a retry that re-derives its inputs rather than re-applying a stale
// computation.
//
// The distinction is not pedantry. A settlement computed from a wager loaded
// before another replica graded one of its legs is arithmetically wrong, and
// re-issuing it against a database that has moved on is how a double payout
// happens. postgres.IsTransientConnectError exists and gates retries elsewhere;
// it is deliberately not consulted here, because even a genuine connection
// failure leaves the transaction's outcome unknown in the 08007 case, and the
// only safe response to an unknown outcome on a ledger write is to start over.
//
// # SHUTDOWN DRAINS, IT DOES NOT KILL
//
// Run observes its context for the POLL boundary, not for the transaction
// boundary. A settlement already in flight when SIGTERM arrives runs to
// completion under a detached context with its own deadline, because the
// alternative — cancelling a context mid-transaction — is precisely the failure
// postgres/tx.go's rollback helper was written to survive, and doing it
// deliberately once per deploy is not a good use of that helper.
package settlement
