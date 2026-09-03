// The seams this package reaches the rest of the system through, and the
// neutral values that cross them.
//
// Every interface here is declared BY THE CONSUMER (CLAUDE.md §12), which is
// this package, and every one of them is as narrow as the code that calls it.
// Nothing in internal/platform/postgres declares an interface for settlement to
// depend on, and internal/settlement/pgstore — which implements these — does not
// appear anywhere in this package's import graph. That is what lets settle.go's
// money rules be asserted against a fake in a unit test and against a real
// Postgres in the integration tier, with the same code under test.
//
// # Why [Store.InTx] hands back a [Tx] and not a pgx.Tx
//
// postgres.TxFunc takes a pgx.Tx, because postgres.InTx's whole job is to hand a
// caller the driver's transaction with the rollback, the panic guard and the
// commit-error propagation already handled. Declaring the seam in those terms
// here would drag pgx into this package and, worse, into every test double: a
// fake would have to implement forty methods of pgx.Tx to assert one money rule.
//
// So [Store] is declared over [Tx] — this package's own five-method view of what
// a settlement transaction does — and internal/settlement/pgstore is the adapter
// that calls postgres.DB.InTx and constructs a [Tx] over the pgx.Tx inside it.
// The adapter is a dozen lines and it is the ONLY place in the settlement path
// that knows a driver exists. internal/betting declares its own seam the same way
// and for the same reason.
//
// This does not weaken the rule that every ledger write goes through
// postgres.InTx. It strengthens it: there is no other way for this package to
// obtain a [Tx] at all, so there is no expression in settle.go that can write a
// ledger row outside a transaction. That matters more here than anywhere else in
// the system, because migrations/00006 installs the zero-sum assertion as a
// DEFERRABLE INITIALLY DEFERRED constraint trigger — an unbalanced ledger write
// returns SUCCESS from every INSERT and fails at COMMIT, and postgres.InTx is the
// only helper in the tree that surfaces a failed COMMIT as an error rather than
// as silence.
package settlement

import (
	"context"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// The results feed
// -----------------------------------------------------------------------------

// Result is one event's outcome, as settlement needs it.
//
// It is deliberately not domain.Event. An event carries a league, competitors, a
// game clock, a scheduled start and a market tree, and settlement needs exactly
// three facts out of all of that: which event, how it finished, and when that
// became true. A port that took the whole aggregate would make every fake in
// this package's tests construct a valid Event to assert a spread push.
type Result struct {
	// EventID is the contest this result belongs to.
	EventID domain.EventID

	// Status is the event's terminal status. Only [domain.EventStatusEnded],
	// [domain.EventStatusSettled] and [domain.EventStatusCancelled] are
	// results; every other status describes an event that has not finished, and
	// a source that returns one is reporting a bug in its own query rather than
	// a result.
	//
	// Postponed is deliberately NOT a result. domain.EventStatus admits
	// `postponed → scheduled`, so a postponed event is one that will be played
	// later — voiding its wagers would cancel bets on a game that is still
	// going to happen.
	Status domain.EventStatus

	// Score is the final score. Present exactly when [Result.IsScored] is true.
	Score domain.Score

	// HasScore reports whether Score is set. It mirrors domain.Event.Score()'s
	// own (value, ok) pair rather than using a sentinel score, because 0-0 is a
	// real and common final in several of the sports in scope.
	HasScore bool

	// FinalisedAt is the PROVIDER's observation instant for the terminal status
	// — events.observed_at, not events.updated_at and not the settle service's
	// clock. It is the ordering key [ResultsSource.Since] pages by and the
	// instant every leg graded from this result is stamped with, so that a
	// redelivered or replayed result re-applies the ORIGINAL grading time
	// instead of advancing it (domain.Leg.WithStatus's guarantee, which the
	// legs_assert_transition trigger also enforces).
	FinalisedAt time.Time
}

// IsScored reports whether the event was played to a final score, which is the
// precondition for grading anything against it.
func (r Result) IsScored() bool {
	return r.HasScore && (r.Status == domain.EventStatusEnded || r.Status == domain.EventStatusSettled)
}

// IsCancelled reports whether the event will never produce a result, so every
// leg riding on it voids and every stake comes back.
func (r Result) IsCancelled() bool { return r.Status == domain.EventStatusCancelled }

// Validate reports whether the result is one settlement can act on.
//
// It is checked on the way IN, at the boundary, rather than trusted: a
// [ResultsSource] is an adapter over a table this package does not own, and a
// row that says "ended" with no score would otherwise grade every spread on the
// event against a 0-0 zero value. That is not a hypothetical — the events table
// permits a scoreless ended row (migrations/00002 notes that
// events_score_all_or_nothing constrains the PAIR, not its presence), which is
// exactly why the check lives here and not in a comment.
func (r Result) Validate() error {
	if err := validEventID(r.EventID); err != nil {
		return err
	}
	if r.FinalisedAt.IsZero() {
		return fmt.Errorf("%w: event %s has no finalisation instant", ErrUnusableResult, r.EventID)
	}
	switch {
	case r.IsScored():
		return nil
	case r.IsCancelled():
		return nil
	case r.HasScore:
		return fmt.Errorf("%w: event %s is %s and carries a score; only an ended or settled "+
			"event has a final one", ErrUnusableResult, r.EventID, r.Status)
	default:
		return fmt.Errorf("%w: event %s is %s with no final score", ErrUnusableResult, r.EventID, r.Status)
	}
}

// String implements fmt.Stringer.
func (r Result) String() string {
	if r.HasScore {
		return fmt.Sprintf("result(%s %s %s)", r.EventID, r.Status, r.Score)
	}
	return fmt.Sprintf("result(%s %s)", r.EventID, r.Status)
}

// ResultsSource is the results feed.
//
// doc.go carries the argument for what the shipped implementation reads and why
// that is a live generator's own output rather than fixture data. The contract
// an implementation must honour:
//
//   - Every returned [Result] satisfies [Result.Validate]. A row that does not
//     is skipped by the source, not handed on — settlement counts what it is
//     given and a source that leaks junk makes the count meaningless.
//   - Results are ordered by [Result.FinalisedAt] ASCENDING, and the boundary is
//     INCLUSIVE: a result finalised exactly at watermark is returned. Settlement
//     advances its cursor to the last result of a fully-processed batch, so an
//     exclusive boundary would drop every result sharing that instant — and a
//     provider poll finalises a whole slate at one observation instant, so ties
//     at the boundary are the common case rather than the rare one.
//   - At most limit results. limit is strictly positive; a source that returns
//     more makes the poll's memory unbounded.
//   - It is a READ. Since must not write, and must not have side effects a
//     retry would double.
//
// Because the boundary is inclusive, settlement re-reads the last batch's final
// instant on every poll and relies on grading being idempotent to absorb it.
// That is a deliberate trade: re-grading an already-graded leg costs one query
// that matches zero rows, whereas losing a result costs a customer their stake.
type ResultsSource interface {
	// Since returns results that became final at or after watermark, oldest
	// first, at most limit of them.
	Since(ctx context.Context, watermark time.Time, limit int) ([]Result, error)
}

// -----------------------------------------------------------------------------
// Persistence
// -----------------------------------------------------------------------------

// LegRef is one ungraded leg, carrying everything [Grade] needs to decide it.
//
// # Why the grading inputs travel on the ref rather than being read off the
// hydrated domain.Leg
//
// They could be: [Tx.WagerWithLegs] returns full domain.Leg values and leg.go
// says a leg "carries everything the grader needs". The ref carries them anyway
// for one reason that is not stylistic — [LegRef.DrawQuoted]. A three-way
// moneyline grades a tie completely differently from a two-way one, and that
// fact is a property of the MARKET (does a draw selection exist?) which
// domain.Leg deliberately does not copy. Reading it requires a catalogue query,
// so it has to arrive with the pending-leg list; and once one grading input
// arrives that way, splitting the rest across two sources would mean a reader of
// grader.go could not tell where any given input came from.
//
// The hydrated wager is still loaded, and is still the authority on the LEG'S
// STATUS and on the ticket's money. The ref is grading INPUT only.
type LegRef struct {
	// LegID identifies the leg. It is the key [Tx.GradeLeg] writes and the key
	// domain.Wager.GradeLeg matches against the hydrated ticket.
	LegID domain.LegID

	// WagerID is the ticket the leg is on. It is what settlement groups by:
	// one transaction per wager, never one per leg, because a parlay's outcome
	// is a function of all its legs at once.
	WagerID domain.WagerID

	// EventID is the event the leg rides on, copied so a caller holding a mixed
	// batch can tell which result decides which leg without a second lookup.
	EventID domain.EventID

	// MarketType and Role are legs.market_type and legs.role, copied at
	// placement and pinned to the market by a composite foreign key. Together
	// they name the QUESTION the leg answers.
	MarketType domain.MarketType
	Role       domain.SelectionRole

	// GradingLine is domain.Leg.GradingLine(): COALESCE(teased_line, price_line).
	// It is already the teased number where one exists and already inverted for
	// an away spread, so the grader applies it as given and never re-derives it.
	// Reading legs.price_line directly here would grade a teaser at the line the
	// market quoted rather than at the one the customer bought.
	GradingLine domain.Line

	// DrawQuoted reports whether the moneyline market this leg answers also
	// quotes a draw — that is, whether a selection with role `draw` exists on
	// legs.market_id.
	//
	// It decides what a tie means, and it is the one grading input a leg cannot
	// answer for itself. On a two-way moneyline a tie is a PUSH and the stake
	// comes back; on a three-way one it is a LOSS for both sides and the draw
	// selection wins. The synthetic provider quotes three-way moneylines for the
	// leagues whose sport admits a draw (internal/ingest/provider/synthetic/
	// markets.go), so both shapes are live in this system and neither may be
	// assumed.
	//
	// It is false, and ignored, for every market type other than moneyline.
	DrawQuoted bool
}

// Validate reports whether the ref is one the grader can act on. Like
// [Result.Validate] it is checked at the boundary, because a ref assembled from
// a row with a mis-mapped enum would otherwise reach [Grade] and be voided as
// "an unrecognised market type" rather than reported as the plumbing fault it is.
func (l LegRef) Validate() error {
	if err := validLegID(l.LegID); err != nil {
		return err
	}
	if err := validWagerID(l.WagerID); err != nil {
		return err
	}
	if err := validEventID(l.EventID); err != nil {
		return err
	}
	if !l.MarketType.Valid() {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, domain.ErrUnknownMarketType)
	}
	if !l.Role.Valid() {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, domain.ErrUnknownSelectionRole)
	}
	if !l.MarketType.AllowsRole(l.Role) {
		return fmt.Errorf("%w: leg %s is %s on a %s market: %w",
			ErrUnusableLeg, l.LegID, l.Role, l.MarketType, domain.ErrRoleNotApplicable)
	}
	return nil
}

// String implements fmt.Stringer.
func (l LegRef) String() string {
	return fmt.Sprintf("legref(%s on %s %s %s line=%s)",
		l.LegID, l.WagerID, l.MarketType, l.Role, l.GradingLine)
}

// Store is the persistence seam.
//
// Two methods. One opens a transaction; the other answers the only question
// settlement asks outside of one.
type Store interface {
	// OldestUnsettledAt reports the placement instant of the earliest wager
	// that still holds escrow — status `placed` or `open` — and whether there
	// is one.
	//
	// It seeds the results cursor at startup, and it is the reason this package
	// needs no cursor table. A ticket cannot be waiting on a result that was
	// already final when the ticket was written, so the earliest open
	// placement is a sound lower bound on "the oldest result settlement could
	// still care about". Everything older has either been graded or was never
	// bet on, and re-reading it would be work with no possible outcome.
	//
	// Settlement subtracts [ResumeLookback] from the answer before using it.
	// The two instants come from different clocks — the placement instant is
	// this system's, the result instant is the provider's — so a bound that was
	// exactly right in principle would be off by whatever skew exists between
	// them. The lookback is what turns a correct-in-theory bound into a
	// correct-in-practice one, and it is cheap because re-reading a result whose
	// legs are already graded matches zero pending legs.
	//
	// found is false when nothing is open, which is the state on a fresh
	// database. Settlement then starts from the current instant: there is no
	// ticket that a historical result could pay.
	OldestUnsettledAt(ctx context.Context) (at time.Time, found bool, err error)

	// InTx runs fn inside one database transaction, committing when it returns
	// nil and rolling back when it returns an error.
	//
	// The implementation MUST delegate to postgres.DB.InTx rather than
	// hand-rolling Begin/Commit. See the file comment: the ledger's zero-sum
	// assertion is deferred to COMMIT, and a helper that does not propagate the
	// commit error reports a movement as written that the database refused.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx is what one settlement transaction does.
//
// Five methods, in the order settle.go calls them. Every one of them takes the
// transaction's own context, which carries the deadline and the trace span the
// statement should hang from.
//
// # Rows-affected semantics, stated once here because they are the contract
//
// Two of these methods are conditional UPDATEs whose WHERE clause encodes a
// precondition. For both, matching ZERO rows is a real outcome that must be
// reported as a sentinel and never as success:
//
//   - [Tx.GradeLeg] matches only a leg that is still pending. Zero rows means
//     something else graded it first, and the caller must not assume its own
//     grading was applied.
//   - [Tx.SettleWager] matches only a ticket that is not yet terminal. Zero rows
//     means it was settled concurrently, and the caller must not write a ledger
//     movement for a payout somebody else already made.
//
// Returning nil from either on a zero-row update is the single most dangerous
// thing an implementation of this interface can do, because the ledger write
// that follows would balance perfectly and pay twice.
type Tx interface {
	// PendingLegsForEvent returns every ungraded leg riding on one event, at
	// most limit of them, ordered by wager so a caller can group without a sort.
	//
	// An event with no open legs returns an empty slice and a nil error — that
	// is the ordinary case for a result nobody bet on, and it is not a
	// not-found condition. There is no sentinel here at all: "nobody has money
	// on this game" is not an error.
	PendingLegsForEvent(ctx context.Context, id domain.EventID, limit int) ([]LegRef, error)

	// WagerWithLegs loads one ticket and every leg on it, rehydrated through
	// the domain constructors so the value returned has passed every invariant
	// domain.NewWager enforces.
	//
	// It returns [ErrWagerNotFound] when no such ticket exists. A missing wager
	// really is exceptional here: the identifier came from a leg row this
	// transaction just read, and legs.wager_id is a foreign key.
	//
	// The implementation SHOULD lock the wager row (SELECT ... FOR UPDATE), so
	// that two settle replicas grading two different events of one parlay
	// serialise on it. Without the lock they still cannot double-pay — the
	// wagers_assert_transition trigger makes the returned amount write-once and
	// the second COMMIT fails — but they fail noisily at commit rather than
	// waiting quietly for a row lock, and a noisy failure that the system was
	// designed to avoid reads as a defect for as long as it takes somebody to
	// re-derive this paragraph.
	WagerWithLegs(ctx context.Context, id domain.WagerID) (domain.Wager, error)

	// GradeLeg writes one leg's terminal grading: status and graded_at
	// together, conditional on the leg still being pending.
	//
	// It returns [ErrLegAlreadyGraded] when the UPDATE matched no row. See the
	// interface comment for why that cannot be reported as success.
	GradeLeg(ctx context.Context, id domain.LegID, status domain.LegStatus, at time.Time) error

	// SettleWager writes the ticket's terminal status, returned amount, net
	// return and transition instant, conditional on the ticket not already
	// being terminal.
	//
	// It takes the settled domain.Wager rather than loose columns so that the
	// four values it writes cannot be assembled inconsistently at the call
	// site: domain.Wager.Settle computes net return from returned and stake and
	// refuses an amount that contradicts the outcome, and the row's own
	// wagers_net_return_identity and wagers_return_matches_outcome constraints
	// re-check the same arithmetic on arrival.
	//
	// It returns [ErrWagerAlreadySettled] when the UPDATE matched no row.
	SettleWager(ctx context.Context, w domain.Wager) error

	// InsertTransaction writes a balanced ledger movement: the
	// ledger_transactions row and every one of its ledger_entries rows.
	//
	// The entries are written in the SAME transaction as the header and as the
	// wager update, which is not an optimisation — it is the only arrangement
	// under which the deferred zero-sum assertion means anything.
	//
	// It returns [ErrTransactionExists] on a primary-key collision. That is the
	// idempotency guard, not a failure: settle.go derives the transaction
	// identifier deterministically from the wager, so a replayed settlement
	// collides here instead of paying twice.
	InsertTransaction(ctx context.Context, t domain.Transaction) error
}

// -----------------------------------------------------------------------------
// The audit trail
// -----------------------------------------------------------------------------

// AuditPublisher writes the settlement audit trail to wager.events.
//
// One method, and it is deliberately the SYNCHRONOUS one.
// kafka.AuditProducer.PublishWagerEvent blocks until the broker has acknowledged
// the record on every in-sync replica, retries without bound, and has no
// asynchronous sibling — internal/platform/kafka/producer.go states the reason
// plainly: "a lost wager event is recoverable by NOTHING". This package depends
// on exactly that property. doc.go carries the ordering rule that makes it
// load-bearing.
//
// The odds producer must never be substituted here. It is configured for the
// opposite posture — bounded retries, fire-and-forget available, compacted
// topics — because a lost odds record is re-derived by the next provider poll.
// A lost settlement is not re-derived by anything.
type AuditPublisher interface {
	PublishWagerEvent(ctx context.Context, id domain.WagerID, msg kafka.Message) error
}

// Compile-time proof that the shipped type satisfies the declaration. It is here
// rather than at the composition root because a mismatch should break THIS
// package's build, where the interface is declared.
//
// There is deliberately no equivalent line for [Store], [Tx] or [ResultsSource]:
// their implementations live in internal/settlement/pgstore, which imports this
// package, so an assertion here would be an import cycle. Each is asserted in
// pgstore instead.
var _ AuditPublisher = (*kafka.AuditProducer)(nil)

// -----------------------------------------------------------------------------
// Identifier validation
// -----------------------------------------------------------------------------

// The three helpers below re-run the domain's own identifier validation on a
// value that already has the right type. That is not redundant: domain.EventID
// and friends are defined string types, so a zero value or a hand-built literal
// carries the type without ever having passed New*ID. Everything crossing these
// ports arrives from a database row through an adapter this package does not
// own, which is precisely where such a value comes from.

func validEventID(id domain.EventID) error {
	if _, err := domain.NewEventID(string(id)); err != nil {
		return fmt.Errorf("%w: %w", ErrUnusableResult, err)
	}
	return nil
}

func validLegID(id domain.LegID) error {
	if _, err := domain.NewLegID(string(id)); err != nil {
		return fmt.Errorf("%w: %w", ErrUnusableLeg, err)
	}
	return nil
}

func validWagerID(id domain.WagerID) error {
	if _, err := domain.NewWagerID(string(id)); err != nil {
		return fmt.Errorf("%w: %w", ErrUnusableLeg, err)
	}
	return nil
}
