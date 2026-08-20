// Sentinel errors, and the four classes a caller can branch on without knowing
// any of them.
//
// internal/domain takes the same shape and states the reason (errors.go there):
// a caller needs three granularities — "what kind of failure is this", "which
// failure exactly", and "the message" — and getting them from wrapping rather
// than from type switches keeps this package ignorant of HTTP.
//
// Every sentinel below wraps EXACTLY ONE class, and every error this package
// returns wraps a sentinel. Match with errors.Is, never on message text.
package betting

import (
	"errors"
	"fmt"
)

// The four classes. An HTTP layer maps these, not the sentinels:
//
//	errors.Is(err, betting.ErrInvalidSlip)  → 400  the slip cannot be a ticket
//	errors.Is(err, betting.ErrNotPermitted) → 403  this account may not
//	errors.Is(err, betting.ErrUnaffordable) → 422  this account cannot fund it
//	errors.Is(err, betting.ErrMarketMoved)  → 409  not at this price, not now
//
// The 403/422 split is not cosmetic. "You have hit your self-imposed daily
// stake limit" and "you do not have the money" are different facts about the
// same rejected slip, and a customer who is told the wrong one will retry the
// wrong fix — which, for a responsible-gaming control, is the failure that
// matters.
var (
	// ErrInvalidSlip is the root of every failure that makes the SLIP not a
	// placeable ticket: empty, duplicated, mis-sized, mis-staked. Nothing about
	// the market or the account is involved; the same slip would be refused at
	// any instant, for any customer.
	ErrInvalidSlip = errors.New("betting: the slip is not a placeable ticket")

	// ErrNotPermitted is the root of every failure that is about the ACCOUNT
	// rather than the slip or the market: status and self-imposed limits. The
	// slip is well-formed and the market is open; this customer may not have it.
	ErrNotPermitted = errors.New("betting: this account may not place this wager")

	// ErrUnaffordable is the root of the funding failures. Separate from
	// ErrNotPermitted because a customer can act on it — the account is in good
	// standing and the bet is allowed, there is simply not enough balance.
	ErrUnaffordable = errors.New("betting: this account cannot fund this wager")

	// ErrMarketMoved is the root of every failure that is about the MARKET at
	// this instant: the price moved, the market closed, the event started, the
	// quote went stale. Every one of them is retryable in the useful sense —
	// re-quote, show the customer the new number, submit again.
	ErrMarketMoved = errors.New("betting: the market is not offering this bet at this price")
)

// Slip-shape failures. Each of these is decidable without any I/O, which is why
// [Slip.Validate] returns them and the placement transaction never has to.
var (
	// ErrSlipEmpty means the slip carries no legs. A ticket on nothing.
	ErrSlipEmpty = fmt.Errorf("%w: the slip has no selections", ErrInvalidSlip)

	// ErrTooManyLegs means the slip exceeds domain.MaxWagerLegs (25), which is
	// also odds.MaxParlayLegs, so a longer ticket could not be priced either.
	ErrTooManyLegs = fmt.Errorf("%w: the slip has more legs than a ticket may carry", ErrInvalidSlip)

	// ErrDuplicateSelection means one selection appears twice on the slip.
	// legs_wager_selection_key makes it unstorable; refusing it here turns a
	// 23505 into a sentence the customer can act on.
	ErrDuplicateSelection = fmt.Errorf("%w: the same selection appears twice on the slip", ErrInvalidSlip)

	// ErrDuplicateMarket means two legs answer the same question — home and
	// away moneyline, over and under one total. They cannot both win, so the
	// ticket is dead on arrival. legs_wager_market_key refuses it too.
	//
	// Unlike ErrDuplicateSelection this one is NOT decidable from the slip
	// alone: a slip names selections, and which market a selection answers is
	// read inside the transaction. It is raised from placement, not validation.
	ErrDuplicateMarket = fmt.Errorf("%w: two selections on the slip answer the same market", ErrInvalidSlip)

	// ErrLegCountForKind means the leg count does not match the wager kind: a
	// straight with two legs, a parlay with one.
	ErrLegCountForKind = fmt.Errorf("%w: this wager kind does not admit that number of selections", ErrInvalidSlip)

	// ErrStakeNotPositive means the stake is zero or negative. Money is minor
	// units, so this is an exact test.
	ErrStakeNotPositive = fmt.Errorf("%w: the stake must be greater than zero", ErrInvalidSlip)

	// ErrTeaserPoints means a teaser named no points, named points outside
	// (0, domain.MaxTeaserPoints], or a non-teaser named points anyway.
	ErrTeaserPoints = fmt.Errorf("%w: teaser points are required on a teaser and on nothing else", ErrInvalidSlip)

	// ErrTeaserMarketType means a teaser leg is on a market with no line to
	// move. leg.go: "you cannot tease a moneyline, and a book that let you
	// would be giving away the whole edge the teaser price is built on".
	ErrTeaserMarketType = fmt.Errorf("%w: only a spread or total selection can be teased", ErrInvalidSlip)

	// ErrRoundRobinSizes means the combination sizes are absent, or one of them
	// is below 2 or above the selection count.
	ErrRoundRobinSizes = fmt.Errorf("%w: a round robin combination size is at least 2 and at most the selection count", ErrInvalidSlip)

	// ErrIdempotencyKeyRequired means the request carried no Idempotency-Key.
	//
	// It is REQUIRED rather than optional, and that is a deliberate refusal to
	// be convenient: without a key the wager id cannot be derived, so a retried
	// submit — which the network will produce eventually whether the client
	// meant it or not — books a second bet. A placement endpoint that accepts a
	// request with no key has an at-least-once money path.
	ErrIdempotencyKeyRequired = fmt.Errorf("%w: a placement must carry an idempotency key", ErrInvalidSlip)

	// ErrIdempotencyKeyInvalid means the key is empty after trimming, or longer
	// than MaxIdempotencyKeyLen. The key is hashed, so its charset is
	// unconstrained; only its length is.
	ErrIdempotencyKeyInvalid = fmt.Errorf("%w: the idempotency key is empty or too long", ErrInvalidSlip)

	// ErrSameGameUnsupported means the slip parlays two legs of one event and
	// the configured TicketPricer will not price correlated legs.
	//
	// This is a refusal to MISPRICE, not a missing feature. See doc.go.
	ErrSameGameUnsupported = fmt.Errorf("%w: this book does not price a same-game parlay", ErrInvalidSlip)

	// ErrTeaserUnsupported means the configured TicketPricer has no teaser
	// ladder. odds/parlay.go explains why one cannot be derived; inventing one
	// would be fabricated data.
	ErrTeaserUnsupported = fmt.Errorf("%w: this book does not price a teaser", ErrInvalidSlip)
)

// Account failures. Every one of these is read INSIDE the placement
// transaction, against a locked users row.
var (
	// ErrSelfExcluded means users.status is 'self_excluded'.
	//
	// It is its own sentinel rather than a case of ErrAccountNotWagerable
	// because it is the one status a customer chose, and the response it earns
	// is different in kind: a suspended account is told to contact support, a
	// self-excluded one is told when the exclusion ends and how to manage it.
	// Collapsing the two would make the responsible-gaming path read as a
	// punishment.
	ErrSelfExcluded = fmt.Errorf("%w: the account is self-excluded", ErrNotPermitted)

	// ErrAccountNotWagerable means users.status is 'suspended' or 'closed', or
	// is a value this build does not recognise.
	//
	// An unrecognised status FAILS CLOSED. auth.UserStatus has an invalid zero
	// value precisely so a status that was never set cannot read as "active",
	// and the same reasoning applies to one that arrived from a database whose
	// CHECK constraint has since grown a value this binary predates.
	ErrAccountNotWagerable = fmt.Errorf("%w: the account may not place wagers", ErrNotPermitted)

	// ErrLimitExceeded means a self-imposed responsible-gaming limit would be
	// breached by this stake. The error wraps a *LimitBreach, so a caller that
	// wants to render "you have used $180 of your $200 daily stake limit" can
	// errors.As it out.
	ErrLimitExceeded = fmt.Errorf("%w: a self-imposed limit would be exceeded", ErrNotPermitted)

	// ErrInsufficientFunds means the customer's cash balance, folded from
	// ledger_entries under the placement lock, is below the total stake. It
	// wraps a *ShortFall.
	ErrInsufficientFunds = fmt.Errorf("%w: the balance does not cover the stake", ErrUnaffordable)
)

// Market failures. Read inside the placement transaction, after the quote.
var (
	// ErrPriceMoved means a leg's current quote differs from the one the
	// customer saw, and the leg carried no acceptance of the new number. It
	// wraps a *PriceMove carrying both quotes.
	ErrPriceMoved = fmt.Errorf("%w: the price moved since the slip was built", ErrMarketMoved)

	// ErrPriceMovedNotAccepted means the leg DID carry an acceptance, and the
	// acceptance names a price that is no longer current either — the line
	// moved twice while the customer was deciding.
	//
	// It wraps ErrPriceMoved, so a caller that only cares "the price moved" can
	// match the one sentinel and get both cases. It is distinguished because
	// the two need different interfaces: the first shows the customer a new
	// number for the first time, the second tells them the number they just
	// agreed to is already gone.
	ErrPriceMovedNotAccepted = fmt.Errorf("%w: the accepted price is no longer current", ErrPriceMoved)

	// ErrMarketNotOpen means domain.MarketStatus.AcceptsWagers() is false — the
	// market is suspended, closed or settled.
	ErrMarketNotOpen = fmt.Errorf("%w: the market is not accepting wagers", ErrMarketMoved)

	// ErrEventStarted means domain.EventStatus.AcceptsWagers() is false. The
	// name is the common case (the game is over or was cancelled); the check is
	// the general one.
	ErrEventStarted = fmt.Errorf("%w: the event is not accepting wagers", ErrMarketMoved)

	// ErrStaleQuote means the current quote's observation instant is older than
	// the configured MaxQuoteAge.
	//
	// This is the placement-path expression of CLAUDE.md §9's headline SLO. A
	// quote nobody has refreshed in a minute is not a current line, it is
	// history, and booking against it is booking at a number the book may no
	// longer be willing to lay.
	ErrStaleQuote = fmt.Errorf("%w: the current quote is too old to bet against", ErrMarketMoved)

	// ErrQuoteUnavailable means no book quoted this selection at all, or the
	// requested book did not.
	ErrQuoteUnavailable = fmt.Errorf("%w: no current quote for this selection", ErrMarketMoved)

	// ErrCashOutUnavailable means a cash-out cannot be quoted for this ticket
	// right now: it is terminal, a leg is void or pushed, a reference price is
	// stale or missing, or the computed value is not positive. The wrapped
	// message names which.
	ErrCashOutUnavailable = fmt.Errorf("%w: this wager cannot be cashed out", ErrMarketMoved)
)

// Errors that are not refusals.
var (
	// ErrAlreadyPlaced is INFORMATIONAL AND NOT A FAILURE, and it never leaves
	// [Service.Place].
	//
	// It is the contract between this package and whatever implements [Tx]:
	// InsertWager MUST return an error satisfying errors.Is(err,
	// ErrAlreadyPlaced) when the row already exists — SQLSTATE 23505 on
	// wagers_pkey — rather than surfacing a raw pgx error. That is the whole
	// mechanism described in doc.go: because the wager id is derived from the
	// idempotency key, a replayed submit collides with the primary key, and the
	// collision IS the answer. Place catches it, reads the wager back, and
	// returns it with Placement.Replayed set.
	//
	// Declaring it here rather than in the store package is deliberate:
	// CLAUDE.md §12 puts the interface with the consumer, and an error that is
	// part of an interface's contract is part of the interface.
	ErrAlreadyPlaced = errors.New("betting: this idempotency key already placed this wager")

	// ErrWagerNotFound means no wager exists with the given id. It is returned
	// by cash-out quoting, and by the idempotent read-back when the store
	// reported a duplicate and then could not produce the row — which is a
	// store bug rather than a customer one, and is wrapped as such.
	ErrWagerNotFound = errors.New("betting: no such wager")

	// ErrInvalidOptions is returned by [NewService] when its options do not
	// validate. Configuration fails at construction, loudly, rather than at the
	// first placement. internal/pricing declares the same sentinel for the same
	// reason.
	ErrInvalidOptions = errors.New("betting: invalid options")

	// ErrInvalidGrantAmount is a play-money top-up that is not a positive
	// amount within [MaxGrantAmount].
	//
	// It is a CUSTOMER-CORRECTABLE error and earns a 4xx, not a 500: the
	// amount came off a request. Zero is included deliberately rather than
	// treated as a harmless no-op, because ledger_entries refuses a zero amount
	// by CHECK, so a "successful" zero grant would be a success message for a
	// transaction the database would have rejected.
	ErrInvalidGrantAmount = errors.New("betting: invalid grant amount")

	// ErrGrantNotFound is a derived grant transaction id that names no
	// movement.
	//
	// Reachable only on the replay path, and only when the INSERT reported a
	// duplicate key and the read-back then found nothing. That combination is
	// not a customer error and not a race — the duplicate proves the row is
	// committed — so it means the id derivation and the read-back disagree,
	// which is a bug in this package rather than something to retry.
	ErrGrantNotFound = errors.New("betting: grant transaction not found")
)

// PriceMove is the detail behind [ErrPriceMoved]: what the customer saw and
// what the book is offering now, for one leg.
//
// It is a struct error rather than a formatted message because the bet slip has
// to RENDER both numbers side by side — "you saw 1.91, it is now 1.87, accept?"
// — and re-parsing them out of an error string is the kind of thing that works
// until a price contains a comma.
type PriceMove struct {
	SelectionID string
	BookID      string

	// SeenDecimal and SeenLine are what the customer had on screen.
	SeenDecimal float64
	SeenLine    string

	// CurrentDecimal and CurrentLine are what the book is offering now, read
	// inside the placement transaction.
	CurrentDecimal float64
	CurrentLine    string

	// Improved reports whether the move is in the customer's favour: a longer
	// price at an unchanged line. It is false whenever the line moved at all,
	// because "better" is not defined across a line move — see slip.go.
	Improved bool

	// Accepted reports whether the leg carried an [Acceptance] that failed to
	// match. It selects which of the two sentinels this wraps.
	Accepted bool
}

func (m *PriceMove) Error() string {
	return fmt.Sprintf("selection %s at book %s: seen %g@%s, now %g@%s",
		m.SelectionID, m.BookID, m.SeenDecimal, m.SeenLine, m.CurrentDecimal, m.CurrentLine)
}

// Unwrap selects [ErrPriceMovedNotAccepted] when the leg carried an acceptance
// that no longer matches, and [ErrPriceMoved] otherwise. Since the former wraps
// the latter, errors.Is(err, ErrPriceMoved) is true either way.
func (m *PriceMove) Unwrap() error {
	if m.Accepted {
		return ErrPriceMovedNotAccepted
	}
	return ErrPriceMoved
}

// LimitBreach is the detail behind [ErrLimitExceeded]: which limit, how much of
// it is already used, and what this slip would have added.
//
// Used and Requested are both minor units and both non-negative. A caller
// rendering "you have used $180 of your $200 daily stake limit" needs all
// three, and computing any of them a second time in the presentation layer is
// how the number on the screen ends up disagreeing with the number that said no.
type LimitBreach struct {
	Kind      string
	Period    string
	Limit     int64
	Used      int64
	Requested int64

	// WindowStart is the beginning of the period the sums were taken over, so a
	// caller can say when the headroom returns.
	WindowStart string
}

func (b *LimitBreach) Error() string {
	return fmt.Sprintf("%s limit per %s is %d minor units, %d already used since %s, this slip adds %d",
		b.Kind, b.Period, b.Limit, b.Used, b.WindowStart, b.Requested)
}

// Unwrap returns [ErrLimitExceeded].
func (b *LimitBreach) Unwrap() error { return ErrLimitExceeded }

// ShortFall is the detail behind [ErrInsufficientFunds].
//
// Available is the FOLD over ledger_entries taken under the placement lock, not
// a cached figure — CLAUDE.md §4: "Balances are derived, never stored as a
// mutable field." A slip validated against a stale balance is an overdraft.
type ShortFall struct {
	Available int64
	Required  int64
}

func (s *ShortFall) Error() string {
	return fmt.Sprintf("balance is %d minor units, the slip stakes %d", s.Available, s.Required)
}

// Unwrap returns [ErrInsufficientFunds].
func (s *ShortFall) Unwrap() error { return ErrInsufficientFunds }
