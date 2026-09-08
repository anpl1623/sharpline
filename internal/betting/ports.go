// Ports: the seams this package reaches the rest of the system through.
//
// Every interface here is declared BY THE CONSUMER (CLAUDE.md §12), which is
// this package. Nothing in internal/platform/postgres, internal/platform/redis
// or internal/pricing declares an interface for betting to depend on, and none
// of them imports this package. The adapters live in internal/betting/pgstore
// and are constructed by the composition root.
//
// # Why the read model is not the sqlc row
//
// internal/httpapi/ports.go makes this argument in full and it holds unchanged
// here: the sqlc row is the TABLE's shape and changes whenever a column does,
// while [Quote], [MarketState] and [Limit] below are the QUESTION's shape —
// parsed domain enums, real Go optionals, and nothing the placement path does
// not read. One mapping function per type is the price of a schema change and a
// service change being independently reviewable.
//
// # One rule that is not negotiable for any implementation of [Tx]
//
// EVERY METHOD ON Tx RUNS INSIDE THE SAME DATABASE TRANSACTION, the one
// [Store.InTx] opened through postgres.InTx. That is not a performance
// preference. migrations/00006 installs the double-entry assertion as
// DEFERRABLE INITIALLY DEFERRED, so an unbalanced ledger write returns SUCCESS
// from every INSERT and fails at COMMIT; only InTx surfaces that. A Tx whose
// methods each took their own connection would report a phantom money movement
// as written, in the one subsystem whose entire purpose is being auditable.
package betting

import (
	"context"
	"net/netip"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// -----------------------------------------------------------------------------
// Read model
// -----------------------------------------------------------------------------

// Quote is one book's current price on one selection, together with everything
// [domain.NewLeg] needs to book it.
//
// The market, event, type and role travel WITH the price on purpose. A leg is
// deliberately self-sufficient (leg.go: "A leg carries everything the grader
// needs"), so the placement path has to gather all of it, and gathering it in
// one round trip per selection rather than four is the difference between a
// three-leg parlay costing three queries and costing twelve.
type Quote struct {
	// Price is THE VALUE THAT IS BOOKED. It is the store's own
	// [domain.Price], read inside the placement transaction, and it is the only
	// route by which a price reaches a leg. Nothing in this package constructs
	// a Price from request input — see doc.go.
	Price domain.Price

	EventID    domain.EventID
	MarketID   domain.MarketID
	MarketType domain.MarketType
	Role       domain.SelectionRole
}

// MarketState is whether this market, and the event under it, will take a bet.
//
// Two statuses rather than one because they are independent: market.go says so
// explicitly ("MarketStatus is the market lifecycle. It is independent of the
// event's"), and a suspended market on a live event is the ordinary case during
// a scoring play.
type MarketState struct {
	MarketID domain.MarketID
	EventID  domain.EventID

	Status      domain.MarketStatus
	EventStatus domain.EventStatus

	// ScheduledStart is carried for the error message, not for the decision.
	// Whether a started event takes bets is EventStatus.AcceptsWagers()'s
	// answer — live betting is a feature (CLAUDE.md §6) — and comparing the
	// wall clock against a kickoff time would refuse every in-play wager.
	ScheduledStart time.Time
}

// Limit is one self-imposed responsible-gaming limit that is IN FORCE.
//
// Amount is minor units and is meaningful only when Kind.IsMoney(). A session
// limit has no amount and is skipped by the placement path entirely; see
// limits.go for the biconditional and why it is stated rather than assumed.
type Limit struct {
	Kind   auth.LimitKind
	Period auth.LimitPeriod
	Amount domain.Money

	// EffectiveFrom is when this row began to bind. It is carried so the
	// evaluator can assert what the store promised rather than trusting it —
	// see [Tx.LimitsInForce].
	EffectiveFrom time.Time
}

// FairPrice is the devigged, no-vig price for one selection, taken from the
// sharp reference book (ADR 0006).
//
// It is the input to cash-out quoting and to nothing else in this package.
// Deriving it is internal/pricing's job — devigging, the reference-book choice,
// and the four devig methods all live there — and repeating any of it here
// would be a second implementation of one formula, which CLAUDE.md §10 is blunt
// about.
type FairPrice struct {
	SelectionID domain.SelectionID

	// Decimal is the FAIR decimal price: 1 / fair probability, margin removed.
	// Not the offered price. Quoting a cash-out off the offered price hides the
	// book's take inside the vig; see doc.go.
	Decimal odds.Decimal

	// ObservedAt is the instant the underlying quote was seen, NOT the instant
	// the fair value was computed. A cash-out is refused when this is older
	// than the configured window, and computing-time would make a stale quote
	// look fresh every time the pricer re-ran.
	ObservedAt time.Time
}

// Ticket is a whole ticket presented for pricing.
//
// Legs carry the prices they will be booked at, already validated by
// [domain.NewLeg], so a pricer never has to guess what a leg means.
type Ticket struct {
	Kind         domain.WagerKind
	Legs         []domain.Leg
	TeaserPoints float64
}

// -----------------------------------------------------------------------------
// Ports
// -----------------------------------------------------------------------------

// Store opens the placement transaction.
//
// It has exactly one method, and that is the point: there is no way to read or
// write anything in this package outside a transaction, so no caller can
// accidentally fold a balance on one connection and insert a wager on another.
type Store interface {
	// InTx runs fn inside ONE database transaction, through postgres.InTx.
	//
	// The implementation MUST use that helper rather than a hand-rolled
	// Begin/Commit. postgres/tx.go states the reason at length: the deferred
	// zero-sum constraint trigger fires at COMMIT, so the commit error is the
	// interesting one, and a helper that drops it reports a rejected ledger
	// movement as written.
	//
	// fn returning an error rolls back and that error is returned. fn returning
	// nil commits, and a failed COMMIT is returned as an error — callers must
	// not treat a nil-returning fn as proof the work landed.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx is everything the placement path does inside one transaction.
//
// Read the file header before implementing it: every method here shares one
// pgx.Tx, and the deferred constraints depend on that.
type Tx interface {
	// UserStatus returns users.status as the raw stored string, WITH THE ROW
	// LOCKED FOR UPDATE.
	//
	// THE LOCK IS LOAD-BEARING AND IS NOT AN OPTIMISATION. Two things later in
	// the transaction are read-then-write over a derived quantity — the limit
	// sums and the balance fold — and neither has a database constraint behind
	// it. ledger_entries has no non-negative CHECK, and it must not have one:
	// the issuance and house accounts go negative by design (ledger.go). So two
	// concurrent placements that each fold the same balance would each see
	// enough money and both commit, and nothing downstream would notice.
	// Locking this customer's own users row serialises their placements and
	// removes the overdraft. It costs nothing in practice — a customer places
	// one bet at a time — and it is taken on the row this transaction has to
	// read anyway.
	//
	// The RAW STRING is returned rather than an auth.UserStatus, deliberately:
	// parsing happens in the service, where an unrecognised value fails closed
	// with ErrAccountNotWagerable. A store that parsed would have to decide what
	// to do with a status its build predates, and the safe answer to that is not
	// a store's to give.
	//
	// A missing user is a programming error at this layer — the caller is
	// already authenticated, so the row exists — and the implementation may
	// return any error for it. The service does not branch on the error, it
	// only wraps it; there is deliberately no ErrUserNotFound to encourage a
	// handler to render "no such account" to somebody holding a valid token.
	UserStatus(ctx context.Context, u domain.UserID) (string, error)

	// Balance returns the derived balance of an account: the fold over
	// ledger_entries exposed by the account_balances view.
	//
	// NOT FOUND MEANS ZERO. The view reports one row per account that has ever
	// been touched, so a brand-new customer with no grant yet produces
	// pgx.ErrNoRows — and migrations/00006 keeps that distinction on purpose,
	// because "touched and nets to nothing" is a different fact from "never
	// touched". The implementation MUST map pgx.ErrNoRows to (0, nil) rather
	// than surfacing it; a placement that fails with "no rows" for a new
	// customer is the bug this sentence exists to prevent.
	Balance(ctx context.Context, a domain.Account) (domain.Money, error)

	// LimitsInForce returns the self-imposed limits BINDING on this user at the
	// instant `at`.
	//
	// The rule the implementation must honour, and why `at` is a parameter
	// rather than now():
	//
	//	WHERE user_id = $1
	//	  AND effective_from <= $2
	//	  AND (superseded_at IS NULL OR superseded_at > $2)
	//
	// migrations/00005 makes user_limits an append-only HISTORY precisely
	// because the real control is asymmetric — a tightening binds immediately,
	// a loosening only after a cooling-off period. Filtering on
	// `superseded_at IS NULL` alone (which is what api.sql's
	// ListCurrentUserLimits does, correctly, for DISPLAY) would return a pending
	// loosening as though it were already in force, and the customer would be
	// unlimited for the whole cooling-off window. That is the one window the
	// control exists to cover.
	//
	// This carries a REQUIREMENT ON THE WRITE SIDE that this package relies on
	// and does not own: when a loosening supersedes a row, the superseded row's
	// superseded_at must be stamped with the NEW row's effective_from, not with
	// now(). Then the predicate above returns the old, tighter row throughout
	// the cooling-off and the new one afterwards, with no gap and no overlap.
	// internal/httpapi's Limits.Set is where that is decided.
	//
	// `at` is passed rather than read from a clock so that the limits, the sums
	// and the wager's placed_at are all one instant. Three reads of time.Now()
	// in one placement put three different instants in one ticket.
	LimitsInForce(ctx context.Context, u domain.UserID, at time.Time) ([]Limit, error)

	// SumEntries returns the SIGNED sum of ledger_entries.amount_minor against
	// one account, restricted to the given entry kinds and to occurred_at >=
	// since.
	//
	// Signed, not absolute: the sign convention is the ledger's own (positive
	// credits, negative debits), and flipping it here would put a second
	// spelling of that convention in the codebase. limits.go negates where the
	// question calls for it and says so at the call site.
	//
	// The kinds are a SET rather than a single value because the loss limit is
	// a net over four of them at once (see limits.go), and taking it as four
	// round trips inside a placement transaction is four where one does.
	//
	// An account with no matching entries sums to zero, not to an error. Both
	// the empty-kinds slice and a nil one are programming errors; the
	// implementation may reject them.
	SumEntries(ctx context.Context, a domain.Account, kinds []domain.EntryKind, since time.Time) (domain.Money, error)

	// QuoteFor returns the newest price for one selection at one book, together
	// with the market, event, type and role needed to book a leg from it.
	//
	// It returns an error wrapping [ErrQuoteUnavailable] when that book has
	// never quoted the selection. It does NOT apply a staleness filter — the
	// service compares the returned Price.ObservedAt against its own configured
	// MaxQuoteAge and raises [ErrStaleQuote], so that "how old is too old" is a
	// reviewable option rather than a constant buried in SQL.
	QuoteFor(ctx context.Context, sel domain.SelectionID, book domain.BookID) (Quote, error)

	// MarketState returns whether this market and its event will take a bet.
	//
	// Returns an error wrapping [ErrMarketNotOpen] when no such market exists,
	// rather than a not-found of its own: from the slip's point of view a market
	// that has vanished and one that is closed are the same refusal, and giving
	// the customer two different sentences for one situation buys nothing.
	MarketState(ctx context.Context, m domain.MarketID) (MarketState, error)

	// InsertRoundRobin writes the round_robins parent row AND its
	// round_robin_sizes children.
	//
	// One method for both because wagers_round_robin_stake_fk is a COMPOSITE
	// foreign key into (id, stake_per_combination_minor) and
	// round_robin_sizes_parent_fk into (id, selection_count): the parent and its
	// sizes are one unit, and a caller that could write half of it could write a
	// round robin whose tickets no size admits.
	//
	// selection_count is len(rr.Legs()); the sizes are rr.Sizes(), already
	// sorted and de-duplicated by the domain constructor.
	InsertRoundRobin(ctx context.Context, rr domain.RoundRobin) error

	// InsertWager writes the wagers row AND every one of its legs.
	//
	// One method for both because migrations/00006 checks the ticket's SHAPE —
	// leg-count arity, teaser correspondence and direction, a straight's price
	// against its leg — in a DEFERRED constraint trigger at COMMIT. A caller
	// that could insert a wager without its legs could commit a ticket on
	// nothing, and the deferred check exists exactly so that the wager row may
	// be written first for the legs' foreign key to resolve.
	//
	// IDEMPOTENCY CONTRACT. On a primary-key collision — SQLSTATE 23505 on
	// wagers_pkey, which is what a replayed submit produces because the id is
	// derived from the idempotency key — the implementation MUST return an
	// error satisfying errors.Is(err, [ErrAlreadyPlaced]). Surfacing the raw
	// pgx error instead turns an exactly-once placement into a 500. Any OTHER
	// unique violation (legs_wager_selection_key, legs_wager_market_key) must
	// NOT be reported as ErrAlreadyPlaced: those mean the slip was malformed,
	// not that it was replayed.
	InsertWager(ctx context.Context, w domain.Wager) error

	// InsertTransaction writes the ledger_transactions row AND its entries, in
	// Transaction.Entries() order — that order is the entry_index, and
	// migrations/00006 keeps it so ORDER BY entry_index rehydrates the domain
	// value exactly.
	//
	// The entries are NOT checked for balance here and must not be: they are
	// balanced by construction (domain.NewTransaction refuses anything else) and
	// verified by the deferred trigger at COMMIT. A third check in the adapter
	// would be a second implementation of the ledger's central invariant.
	InsertTransaction(ctx context.Context, t domain.Transaction) error

	// GrantCredit returns what a GRANT transaction credited to one user's cash
	// account, and when the movement occurred.
	//
	// It answers a REPLAYED top-up and nothing else. The grant's transaction id
	// is derived from (userID, idempotencyKey), so a resubmit collides with
	// ledger_transactions' primary key; the service then reports what the
	// ORIGINAL request credited rather than echoing the amount this request
	// asked for, which is the same rule a replayed placement follows when the
	// slip differs.
	//
	// The implementation must filter on the transaction being a GRANT and on
	// the entry being the USER'S CASH side. Returning the issuance half would
	// report a top-up as a negative amount, and not checking the kind would let
	// a settlement be described to a customer as a grant.
	//
	// Returns an error wrapping [ErrGrantNotFound] when no such movement
	// exists.
	GrantCredit(ctx context.Context, id domain.TransactionID, u domain.UserID) (domain.Money, time.Time, error)

	// WagerByID rehydrates a wager and its legs.
	//
	// Used only on the idempotent replay path, where the INSERT reported
	// [ErrAlreadyPlaced] and the existing ticket is the answer. Returns an error
	// wrapping [ErrWagerNotFound] when the row does not exist — which, on that
	// path, means the store contradicted itself and is reported as such rather
	// than as a customer-facing failure.
	WagerByID(ctx context.Context, id domain.WagerID) (domain.Wager, error)

	// RecordAudit appends one audit_log row INSIDE THIS TRANSACTION.
	//
	// # Why this is a method on Tx and not a port of its own
	//
	// CLAUDE.md §6 requires "an audit log on every state-changing action", and
	// the only version of that requirement worth having is one where the row
	// and the change it describes share a fate. An entry written afterwards on
	// another connection can commit without its wager — the placement rolled
	// back at COMMIT on the deferred zero-sum trigger and the trail now claims
	// a bet that does not exist — or go missing after a committed one, which is
	// the same defect pointed the other way. Both are worse than no row,
	// because a trail with a known gap can be reasoned about and a trail that
	// is quietly wrong cannot.
	//
	// So the audit write is a statement in the placement transaction, like the
	// wager and the ledger entries, and it is on THIS interface because this
	// interface is what owns that transaction. internal/httpapi's Limits.Set
	// already works exactly this way and says why at length.
	//
	// # What is written, and what is deliberately not
	//
	// Only COMMITTED CHANGES. A refusal — self-excluded, limit breached,
	// insufficient funds, price moved — returns an error, the transaction rolls
	// back, and any row written before it goes with it. That is not a hole this
	// method can close: recording a refusal durably needs a connection that
	// survives the rollback, which is a different mechanism with a different
	// failure mode (see internal/httpapi's Audit port, and API.record's comment
	// on why it never fails a request). Every entry that reaches here therefore
	// carries Outcome "success", and [AuditEntry] has no Reason field because a
	// committed change has no rejection reason to give.
	//
	// The implementation MUST NOT swallow the error. A failed audit write fails
	// the placement, on purpose: the customer's alternative is a booked bet
	// with no record of who booked it, and refusing the bet is the recoverable
	// half of that pair.
	RecordAudit(ctx context.Context, e AuditEntry) error
}

// AuditContext is the provenance a state-changing action in this package
// carries into the audit log.
//
// # Why this is declared here and not imported
//
// internal/httpapi declares the same idea for its own handlers, and this
// package cannot import it — httpapi imports betting, so the arrow only points
// one way, and CLAUDE.md §12 puts the declaration at the consumer anyway. The
// caller projects one onto the other at the boundary, which is the same trade
// the read models in this file already make: one small mapping function in
// exchange for the two packages being independently reviewable.
//
// IT HOLDS NO SECRET AND HAS NO FIELD ONE COULD BE PUT IN. Not a token, not a
// password, not an Authorization header. The only client-supplied value is the
// address, which migration 00007 names as the single PII-bearing column in the
// schema and keeps deliberately.
//
// # There is no At field, and that is not an omission
//
// httpapi's AuditContext carries one because the store it feeds has no clock of
// its own. This package does: [NewService] requires one, and every doc comment
// in it insists that ONE placement has ONE instant — placed_at, the limit
// windows, the staleness horizon and the ledger's occurred_at are the same
// value, read once at the top of the operation. Taking the audit row's
// occurred_at from the request would put a second instant in a placement whose
// whole discipline is that there is only one, and would let a caller choose
// when its own action appears to have happened.
type AuditContext struct {
	// RequestID correlates the audit row with the access-log line and with the
	// error body the client received.
	RequestID string

	// ClientIP is resolved by the server's trusted-proxy logic, never read from
	// a header by a handler. Invalid when the peer could not be determined.
	ClientIP netip.Addr

	// TraceID and SpanID are W3C Trace Context ids in lowercase hex, which is
	// what makes the row joinable to a Jaeger trace. Empty when the request
	// carried no sampled span.
	TraceID string
	SpanID  string
}

// AuditEntry is one audit_log row, as this package writes it.
//
// The columns it does not carry are as deliberate as the ones it does:
// `reason` and a `failure` outcome are absent because only committed changes
// reach [Tx.RecordAudit] (see there), and `actor_kind` is fixed at "user"
// because every action in this package is a customer acting on their own
// account — there is no path here an operator or a scheduled job can take.
type AuditEntry struct {
	// Context is the request provenance. Zero is legal and means "no HTTP
	// request produced this", which a test or a future scheduled action would
	// have; the trace and request columns are nullable for exactly that case.
	Context AuditContext

	// OccurredAt is the instant of the CHANGE, taken from the service clock, so
	// the row and the wager or ledger transaction it describes carry the same
	// value. See [AuditContext] on why it does not travel on the request.
	OccurredAt time.Time

	// ActorID is the customer. Never empty: migration 00007 refuses a blank
	// actor because a row with no actor answers none of the questions the table
	// exists to answer.
	ActorID domain.UserID

	// Action is dotted `domain.verb` and Entity names what it acted on. The
	// vocabulary is in audit.go; migration 00007's CHECK constraints bound the
	// shape of both.
	Action     string
	EntityType string
	EntityID   string

	// After holds the CHANGED FIELDS ONLY — a diff, not an entity dump. There
	// is no Before field because every action in this package CREATES the row
	// it audits: a wager and a ledger transaction have no prior state, so a
	// before-image would be an empty object dressed up as information. An
	// action that mutates an existing row (a cash-out driving a ticket
	// terminal) belongs to internal/settlement and audits from there.
	After map[string]any
}

// Wagers is the read-only wager lookup the cash-out path uses.
//
// Separate from [Tx.WagerByID], and deliberately not a transaction: quoting a
// cash-out writes nothing, so wrapping it in one would hold a connection open
// across a pricing read for no invariant.
type Wagers interface {
	WagerByID(ctx context.Context, id domain.WagerID) (domain.Wager, error)
}

// FairPrices reads devigged reference prices, for cash-out quoting.
//
// It takes a set because a cash-out quote needs every pending leg at once and a
// five-leg parlay served one selection at a time is five round trips whose
// per-call overhead dominates a query that is otherwise one index scan.
//
// A selection with no fair price is OMITTED from the result rather than
// reported as an error: the caller has to distinguish "this leg cannot be
// priced" from "the whole read failed", and an error for the first would make
// the two indistinguishable. Missing legs become [ErrCashOutUnavailable].
type FairPrices interface {
	FairPricesFor(ctx context.Context, selections []domain.SelectionID) ([]FairPrice, error)
}

// TicketPricer prices a whole ticket from the legs it will be booked at.
//
// It exists as a port because ticket pricing is NOT a function of the leg
// prices in general, and wager.go says so: "A parlay's price is not always the
// product of its legs. Same-game legs are correlated and are priced with a
// correlation adjustment, a teaser's price is a fixed schedule that has nothing
// to do with the underlying prices at all, and a boosted ticket is priced above
// both."
//
// The price it returns is what gets frozen into wagers.accepted_decimal and is
// therefore permanent. That is why this is a port and not a helper: the shipped
// [IndependentPricer] refuses the two shapes it cannot price correctly rather
// than approximating them, and a book that has a correlation model or a teaser
// ladder supplies its own implementation without this package changing.
//
// The customer never determines this number. It is computed from legs built out
// of quotes re-read inside the placement transaction, then compared against the
// ticket price the customer saw under the same price-move rule the legs get.
type TicketPricer interface {
	// TicketDecimal returns the ticket's total return per unit staked, all legs
	// winning.
	//
	// It must return a value in (domain.MinDecimalOdds, domain.MaxWagerDecimal]
	// — the WAGER bound, not the single-market one, because a 20-leg parlay of
	// even-money legs is 2^20 and MaxDecimalOdds (1e5) would wrongly reject it.
	TicketDecimal(ctx context.Context, t Ticket) (float64, error)
}

// IdempotencyCache is the Redis fast path in front of the primary-key guard.
//
// CLAUDE.md §3 assigns Redis "idempotency keys" and is explicit that it is
// "never the source of truth". Every method here is therefore ALLOWED TO FAIL,
// and a failure is logged and dropped: the placement proceeds, hits Postgres,
// and gets the identical answer, because the derived primary key was doing the
// work all along. A nil cache disables the fast path entirely, which is what a
// unit test wants and what a cold start does.
//
// The cache holds WAGER IDS AND NOTHING ELSE. Caching the ticket body would be
// the obvious optimisation and it would be wrong: a wager's status changes when
// settlement grades it, so a replay hours later would report a settled ticket
// as still open. Ids are immutable; bodies are not.
type IdempotencyCache interface {
	// Lookup returns the wager ids a previous placement recorded under this
	// key, and whether the key was present. A miss is (nil, false, nil); an
	// error means the cache is unhealthy and the caller reads through.
	Lookup(ctx context.Context, u domain.UserID, key string) (ids []domain.WagerID, found bool, err error)

	// Record notes the ids this key placed, with a TTL. Best effort: the return
	// value is logged, never surfaced to the customer, because a cache write
	// failing has no effect on the correctness of a placement that has already
	// committed.
	Record(ctx context.Context, u domain.UserID, key string, ids []domain.WagerID, ttl time.Duration) error
}

// Clock is the only source of "now" in this package.
//
// It is a port so tests can pin time without sleeping, and because ONE
// placement must have ONE instant: placed_at, the limit windows, the staleness
// horizon and the ledger transaction's occurred_at are all the same value, read
// once at the top of Place. Three calls to time.Now() in one placement put
// three instants in one ticket, and the deferred CHECK
// wagers_transitioned_after_placed would eventually notice.
type Clock func() time.Time
