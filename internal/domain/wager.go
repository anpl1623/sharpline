package domain

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// Bounds on a wager. Every one of them is a sanity guard against a units error
// or a runaway loop, not a rule of the domain — a real book's limits are policy
// and belong in the betting service where they can be changed without a
// recompile.
const (
	// MaxWagerLegs bounds the legs on one ticket. US books top out around a
	// couple of dozen; 25 is past every real offering and far below anything a
	// malformed slip could produce.
	MaxWagerLegs = 25

	// MaxRoundRobinLegs bounds the SELECTIONS a round robin is built from, and
	// it is the one bound that is load-bearing rather than cosmetic: the ticket
	// count is a binomial coefficient, so this number sits inside an exponential.
	// At 10 selections the largest single size is C(10,5) = 252 tickets and every
	// size together is 2^10 - 11 = 1013. At 20 it would be a million, which is
	// not a large bet, it is a denial of service against the settlement path.
	MaxRoundRobinLegs = 10

	// MaxTeaserPoints bounds the points a teaser may move a line by. Football
	// teasers run 6, 6.5, 7 and — for a "super" teaser — 13; basketball runs 4
	// to 5. 20 clears all of them and still catches a percentage read as points.
	MaxTeaserPoints = 20.0

	// MaxWagerDecimal bounds a TICKET's price, and is deliberately far above
	// [MaxDecimalOdds], which bounds a single quoted market price.
	//
	// The two are not the same quantity. A 20-leg parlay of even-money legs is
	// 2^20 ≈ 1.05e6 in decimal odds, which is a perfectly ordinary ticket and
	// which MaxDecimalOdds (1e5) would wrongly reject. 1e9 leaves three orders
	// of magnitude above that and still catches the failure that actually
	// happens — a payout in minor units assigned to an odds field. A real
	// maximum-payout cap is house policy, not a domain invariant.
	MaxWagerDecimal = 1e9

	// teaserLineTolerance is the ABSOLUTE tolerance used when checking that a
	// teased line sits exactly the promised number of points off the booked one.
	//
	// Absolute rather than relative because the quantity compared is a
	// difference of two lines, and a difference near zero has no meaningful
	// scale to be relative to. The magnitude is justified by the grid: lines are
	// quarter-point multiples and teaser points are half-point multiples, all of
	// which are dyadic fractions and therefore EXACT in float64, so a correct
	// tease differs from the promise by exactly zero. The tolerance exists only
	// to absorb a value that arrived through a decimal string parse. The
	// smallest real mismatch the domain can express is a quarter point, 2.5e8
	// times this tolerance, so no genuinely mis-teased leg can slip through.
	teaserLineTolerance = 1e-9
)

// UserID identifies the person who placed a wager and owns the accounts its
// money moves between.
//
// The domain models no user entity: authentication, email, password hashing and
// 2FA live in internal/auth (CLAUDE.md §8), and nothing in the betting or
// ledger rules needs to know anything about a user beyond identity. Importing a
// whole User aggregate here to reach one field would couple the ledger to the
// auth schema for no gain.
type UserID string

// NewUserID validates and returns a UserID.
func NewUserID(s string) (UserID, error) {
	if err := validID(s); err != nil {
		return "", idErr("user id", s, err)
	}
	return UserID(s), nil
}

// String returns the identifier as a bare string.
func (id UserID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id UserID) IsZero() bool { return id == "" }

// WagerID identifies a Wager.
type WagerID string

// NewWagerID validates and returns a WagerID.
func NewWagerID(s string) (WagerID, error) {
	if err := validID(s); err != nil {
		return "", idErr("wager id", s, err)
	}
	return WagerID(s), nil
}

// String returns the identifier as a bare string.
func (id WagerID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id WagerID) IsZero() bool { return id == "" }

// RoundRobinID identifies a RoundRobin — the parent of the ticket set it
// expands into.
type RoundRobinID string

// NewRoundRobinID validates and returns a RoundRobinID.
func NewRoundRobinID(s string) (RoundRobinID, error) {
	if err := validID(s); err != nil {
		return "", idErr("round robin id", s, err)
	}
	return RoundRobinID(s), nil
}

// String returns the identifier as a bare string.
func (id RoundRobinID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id RoundRobinID) IsZero() bool { return id == "" }

// Wager and round-robin failures.
var (
	ErrUnknownWagerKind   = fmt.Errorf("%w: not a defined wager kind", ErrInvalid)
	ErrUnknownWagerStatus = fmt.Errorf("%w: not a defined wager status", ErrInvalid)

	// ErrLegCount reports a leg count the wager kind does not admit: a straight
	// with two legs, a parlay with one, a ticket past MaxWagerLegs.
	ErrLegCount = fmt.Errorf("%w: the wager kind does not admit this number of legs", ErrInvalid)

	// ErrDuplicateSelection reports the same selection appearing twice on one
	// ticket, which is a slip-building bug rather than a bet.
	ErrDuplicateSelection = fmt.Errorf("%w: the same selection appears twice on one wager", ErrInvalid)

	// ErrDuplicateMarket reports two legs answering the SAME market. They are
	// competing answers to one question — home and away moneyline, over and
	// under the same total — so they cannot both win, and a ticket that
	// requires both to win is dead on arrival. Books refuse it for the same
	// reason.
	ErrDuplicateMarket = fmt.Errorf("%w: two legs answer the same market", ErrInvalid)

	// ErrLegNotPending reports a wager built from an already-graded leg. A
	// ticket is born ungraded; grading happens through Wager.GradeLeg.
	ErrLegNotPending = fmt.Errorf("%w: a wager is placed with ungraded legs", ErrInvalid)

	ErrStakeNotPositive = fmt.Errorf("%w: a stake must be greater than zero", ErrInvalid)

	// ErrWagerOddsOutOfRange reports a ticket price outside
	// (MinDecimalOdds, MaxWagerDecimal].
	ErrWagerOddsOutOfRange = fmt.Errorf("%w: the ticket price is not in the representable range", ErrInvalid)

	// ErrWagerPriceMismatch reports a straight ticket priced differently from
	// the single leg it contains, which can only be an arithmetic or plumbing
	// fault.
	ErrWagerPriceMismatch = fmt.Errorf("%w: a straight's ticket price must equal its leg's price", ErrInvalid)

	// ErrPayoutBelowStake reports a ticket whose winning return is less than
	// the stake. Winning must never cost money.
	ErrPayoutBelowStake = fmt.Errorf("%w: the winning return is less than the stake", ErrInvalid)

	ErrTeaserPointsRequired      = fmt.Errorf("%w: a teaser states the points its lines are moved by", ErrInvalid)
	ErrTeaserPointsNotApplicable = fmt.Errorf("%w: only a teaser carries teaser points", ErrInvalid)

	// ErrTeaserPointsMismatch reports a leg whose teased line is not the
	// promised number of points off the line it was booked at. It is the check
	// that keeps a mis-teased leg from grading at a handicap nobody sold.
	ErrTeaserPointsMismatch = fmt.Errorf("%w: a teased line is not the stated number of points off the booked line", ErrInvalid)

	ErrTeasedLegRequired      = fmt.Errorf("%w: every leg of a teaser carries a teased line", ErrInvalid)
	ErrTeasedLegNotApplicable = fmt.Errorf("%w: only a teaser's legs carry a teased line", ErrInvalid)

	ErrRoundRobinParentRequired      = fmt.Errorf("%w: a round robin ticket names the round robin it came from", ErrInvalid)
	ErrRoundRobinParentNotApplicable = fmt.Errorf("%w: only a round robin ticket names a parent round robin", ErrInvalid)

	// ErrCombinationSize reports a round-robin combination size outside
	// [2, len(legs)].
	ErrCombinationSize = fmt.Errorf("%w: a round robin combination size is at least 2 and at most the selection count", ErrInvalid)

	// ErrReturnAmount reports a settlement amount that contradicts the outcome
	// it is filed under — a loss returning money, a push returning anything
	// other than the stake, a win returning more than the maximum payout.
	ErrReturnAmount = fmt.Errorf("%w: the returned amount contradicts the settled outcome", ErrInvalid)

	// ErrWagerNotSettled reports a settlement-only operation attempted on a
	// wager that is still running. A conflict, not bad input.
	ErrWagerNotSettled = fmt.Errorf("%w: the wager has not been settled", ErrConflict)
)

// WagerKind is the shape of a ticket. CLAUDE.md §6 names all four.
type WagerKind uint8

const (
	// WagerKindUnknown is the invalid zero value.
	WagerKindUnknown WagerKind = iota

	// WagerKindStraight is a single selection.
	WagerKindStraight

	// WagerKindParlay is two or more selections that must all win. Its legs may
	// sit on one event (a same-game parlay), in which case the ticket price is
	// correlation-adjusted and is NOT the product of the leg prices — see the
	// note on [Wager.AcceptedDecimal].
	WagerKindParlay

	// WagerKindRoundRobin is ONE COMBINATION TICKET produced by expanding a
	// [RoundRobin]. It is a parlay in every respect except provenance, and it
	// always names the round robin it came from, which is what makes the
	// relationship the charter asks to be modelled explicitly a checkable
	// invariant rather than a convention: kind is round_robin exactly when a
	// parent is present.
	WagerKindRoundRobin

	// WagerKindTeaser is two or more spread or total selections whose lines are
	// all moved in the bettor's favour by the same number of points, at a
	// reduced ticket price.
	WagerKindTeaser
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (k WagerKind) String() string {
	switch k {
	case WagerKindStraight:
		return "straight"
	case WagerKindParlay:
		return "parlay"
	case WagerKindRoundRobin:
		return "round_robin"
	case WagerKindTeaser:
		return "teaser"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a defined kind.
func (k WagerKind) Valid() bool {
	switch k {
	case WagerKindStraight, WagerKindParlay, WagerKindRoundRobin, WagerKindTeaser:
		return true
	default:
		return false
	}
}

// ParseWagerKind is the inverse of String for the defined kinds.
func ParseWagerKind(s string) (WagerKind, error) {
	switch s {
	case "straight":
		return WagerKindStraight, nil
	case "parlay":
		return WagerKindParlay, nil
	case "round_robin":
		return WagerKindRoundRobin, nil
	case "teaser":
		return WagerKindTeaser, nil
	default:
		return WagerKindUnknown, fmt.Errorf("wager kind %q: %w", sample(s), ErrUnknownWagerKind)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (k WagerKind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("wager kind %d: %w", uint8(k), ErrUnknownWagerKind)
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *WagerKind) UnmarshalText(b []byte) error {
	parsed, err := ParseWagerKind(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// IsMulti reports whether the kind requires more than one leg.
func (k WagerKind) IsMulti() bool {
	switch k {
	case WagerKindParlay, WagerKindRoundRobin, WagerKindTeaser:
		return true
	default:
		return false
	}
}

// WagerStatus is the ticket lifecycle.
//
// The legal transitions are:
//
//	placed → open | won | lost | void | push | cashed_out
//	open   → won | lost | void | push | cashed_out
//	won | lost | void | push | cashed_out → (terminal)
//
// Two shapes of this machine are deliberate and worth defending.
//
// # placed may settle without passing through open
//
// The charter sketches placed → open → settled, and `open` is genuinely useful:
// it is the marker that says "an event on this ticket is underway, price the
// cash-out live". But making it MANDATORY would mean a correct settlement
// depends on an earlier status message having been delivered, and it will not
// always be: Kafka is at-least-once and not ordered across partitions
// (CLAUDE.md §3), a prematch event can be cancelled before it ever goes live,
// and an ingest gap can skip a short event entirely. Requiring the intermediate
// hop would turn a routine reordering into an unsettleable ticket holding a
// customer's stake in escrow. So `open` is a refinement, not a gate.
//
// # terminal is absolutely terminal
//
// A settled ticket cannot be re-graded, cannot be re-settled at a different
// amount, and cannot be cashed out. Results DO get corrected — an overturned
// call, a revised final, a provider misfeed — and the correction is an
// [EntryKindAdjustment] transaction in the ledger, which leaves both the
// original settlement and the fix on the record. A mutable terminal state would
// let a payout be silently rewritten, in the one subsystem whose entire purpose
// is being auditable.
//
// s → s is legal, for the at-least-once reason given on [EventStatus].
type WagerStatus uint8

const (
	// WagerStatusUnknown is the invalid zero value.
	WagerStatusUnknown WagerStatus = iota

	// WagerStatusPlaced means the ticket was accepted and its stake is held in
	// escrow. Nothing on it has started.
	WagerStatusPlaced

	// WagerStatusOpen means at least one event on the ticket is underway, so a
	// live cash-out can be priced against it.
	WagerStatusOpen

	// WagerStatusWon means the ticket was graded a winner and paid.
	WagerStatusWon

	// WagerStatusLost means the ticket was graded a loser. The stake is the
	// house's.
	WagerStatusLost

	// WagerStatusVoid means the ticket will not be graded — cancelled event,
	// voided market, every leg removed — and the stake is returned.
	WagerStatusVoid

	// WagerStatusPush means the ticket graded exactly on the number and the
	// stake is returned. Money-identical to void, kept distinct because one is
	// a result and the other is a cancellation.
	WagerStatusPush

	// WagerStatusCashedOut means the customer took a settlement price before
	// the ticket resolved. The wager is closed at that price whatever the
	// events later do.
	WagerStatusCashedOut
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (s WagerStatus) String() string {
	switch s {
	case WagerStatusPlaced:
		return "placed"
	case WagerStatusOpen:
		return "open"
	case WagerStatusWon:
		return "won"
	case WagerStatusLost:
		return "lost"
	case WagerStatusVoid:
		return "void"
	case WagerStatusPush:
		return "push"
	case WagerStatusCashedOut:
		return "cashed_out"
	default:
		return "unknown"
	}
}

// Valid reports whether s is a defined status.
func (s WagerStatus) Valid() bool {
	switch s {
	case WagerStatusPlaced, WagerStatusOpen, WagerStatusWon, WagerStatusLost,
		WagerStatusVoid, WagerStatusPush, WagerStatusCashedOut:
		return true
	default:
		return false
	}
}

// ParseWagerStatus is the inverse of String for the defined statuses.
func ParseWagerStatus(s string) (WagerStatus, error) {
	switch s {
	case "placed":
		return WagerStatusPlaced, nil
	case "open":
		return WagerStatusOpen, nil
	case "won":
		return WagerStatusWon, nil
	case "lost":
		return WagerStatusLost, nil
	case "void":
		return WagerStatusVoid, nil
	case "push":
		return WagerStatusPush, nil
	case "cashed_out":
		return WagerStatusCashedOut, nil
	default:
		return WagerStatusUnknown, fmt.Errorf("wager status %q: %w", sample(s), ErrUnknownWagerStatus)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s WagerStatus) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("wager status %d: %w", uint8(s), ErrUnknownWagerStatus)
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *WagerStatus) UnmarshalText(b []byte) error {
	parsed, err := ParseWagerStatus(string(b))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// IsTerminal reports whether the ticket is closed and no further transition is
// possible.
func (s WagerStatus) IsTerminal() bool {
	switch s {
	case WagerStatusWon, WagerStatusLost, WagerStatusVoid, WagerStatusPush, WagerStatusCashedOut:
		return true
	default:
		return false
	}
}

// IsGraded reports whether the ticket reached its terminal state by being
// graded against a result, as opposed to by being cashed out.
func (s WagerStatus) IsGraded() bool {
	switch s {
	case WagerStatusWon, WagerStatusLost, WagerStatusVoid, WagerStatusPush:
		return true
	default:
		return false
	}
}

// HoldsEscrow reports whether the ticket's stake is still held against it. It
// is the predicate the exposure metric and the "at risk" figure are built on.
func (s WagerStatus) HoldsEscrow() bool {
	return s == WagerStatusPlaced || s == WagerStatusOpen
}

// CanTransitionTo reports whether next is a legal successor of s. See the type
// comment for why the machine has the shape it does.
func (s WagerStatus) CanTransitionTo(next WagerStatus) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	if s == next {
		return true
	}
	switch s {
	case WagerStatusPlaced:
		return next == WagerStatusOpen || next.IsTerminal()
	case WagerStatusOpen:
		return next.IsTerminal()
	default: // Every terminal status.
		return false
	}
}

// Wager is a placed bet.
//
// Values are immutable. Every state change returns a new Wager: see
// [Wager.Open], [Wager.GradeLeg], [Wager.Settle], and [Wager.CashOut].
//
// # Construction always yields a placed ticket
//
// [NewWager] has no status parameter and always returns [WagerStatusPlaced]
// with every leg pending. There is no way to conjure a settled wager out of a
// struct literal, so the only path to a terminal state is through the
// transition methods, and every check they carry — the legal-edge check, the
// returned-amount check — is unavoidable. Phase 2 rehydrates a stored wager by
// replaying its transitions, which re-validates the row on the way in; that is
// a feature, not a tax.
//
// # The ticket price is stored, not derived
//
// A parlay's price is not always the product of its legs. Same-game legs are
// correlated and are priced with a correlation adjustment (CLAUDE.md §4), a
// teaser's price is a fixed schedule that has nothing to do with the underlying
// prices at all, and a boosted ticket is priced above both. Re-deriving the
// price at settlement would therefore produce a different number than the
// customer was shown and accepted.
//
// So the accepted price is recorded, and the potential payout is computed once,
// at placement, under an explicit [Rounding], and frozen. "To win $X" is a
// promise, and a promise recomputed later is not one.
//
// Deliberately, this type contains no odds math beyond that one multiplication.
// Devigging, EV, Kelly, and correlation-adjusted parlay pricing live in
// internal/domain/odds; duplicating any of it here would create two
// implementations of one formula, and CLAUDE.md §10 is blunt about the cost of
// wrong odds math.
type Wager struct {
	id           WagerID
	userID       UserID
	kind         WagerKind
	legs         []Leg
	stake        Money
	accepted     float64
	rounding     Rounding
	payout       Money
	profit       Money
	teaserPoints float64
	hasTeaser    bool
	roundRobinID RoundRobinID
	status       WagerStatus
	returned     Money
	netReturn    Money
	hasReturned  bool
	placedAt     time.Time
	updatedAt    time.Time
}

// WagerParams is the input to NewWager.
type WagerParams struct {
	ID     WagerID
	UserID UserID
	Kind   WagerKind

	// Legs are the selections on the ticket, each already holding the price it
	// was booked at. The slice is copied, so a later mutation by the caller
	// cannot reach inside a constructed wager.
	Legs []Leg

	// Stake is the amount risked, in minor units. It must be strictly positive.
	Stake Money

	// AcceptedDecimal is the ticket price the customer accepted: total return
	// per unit staked, all legs winning. For a straight it must equal the leg's
	// price. For everything else it is whatever the pricing engine quoted and
	// the customer took — see the type comment.
	AcceptedDecimal float64

	// Rounding is the rule used to collapse stake × price to a whole minor
	// unit. There is no default: settlement rounding is a policy question and a
	// silent default is how a house edge appears in a ledger nobody meant to
	// put one in (see [Money.MulFloat]).
	Rounding Rounding

	// TeaserPoints is the number of points every leg's line is moved by. It is
	// required and strictly positive for [WagerKindTeaser] and must be zero for
	// every other kind.
	TeaserPoints float64

	// RoundRobinID names the round robin this ticket was expanded from. It is
	// required for [WagerKindRoundRobin] and must be unset otherwise.
	RoundRobinID RoundRobinID

	// PlacedAt is when the ticket was accepted. It is normalised to UTC and
	// seeds the monotonicity guard on every later transition.
	PlacedAt time.Time
}

// NewWager validates its input and returns an immutable Wager in
// [WagerStatusPlaced].
func NewWager(p WagerParams) (Wager, error) {
	if err := validID(string(p.ID)); err != nil {
		return Wager{}, idErr("wager id", string(p.ID), err)
	}
	if err := validID(string(p.UserID)); err != nil {
		return Wager{}, idErr("user id", string(p.UserID), err)
	}
	if !p.Kind.Valid() {
		return Wager{}, fmt.Errorf("wager %s: %w", p.ID, ErrUnknownWagerKind)
	}
	legs, err := validateLegSet("wager "+string(p.ID), p.Legs)
	if err != nil {
		return Wager{}, err
	}
	if err := validateLegCount(p.ID, p.Kind, len(legs)); err != nil {
		return Wager{}, err
	}
	if err := validateTeaser(p.ID, p.Kind, p.TeaserPoints, legs); err != nil {
		return Wager{}, err
	}
	if err := validateRoundRobinParent(p.ID, p.Kind, p.RoundRobinID); err != nil {
		return Wager{}, err
	}
	if !p.Stake.IsPositive() {
		return Wager{}, fmt.Errorf("wager %s stake %s: %w", p.ID, p.Stake, ErrStakeNotPositive)
	}
	if err := validateTicketPrice(p.ID, p.Kind, p.AcceptedDecimal, legs); err != nil {
		return Wager{}, err
	}
	if !p.Rounding.Valid() {
		return Wager{}, fmt.Errorf("wager %s: %w", p.ID, ErrUnknownRounding)
	}
	if p.PlacedAt.IsZero() {
		return Wager{}, fmt.Errorf("wager %s placed at: %w", p.ID, ErrZeroTime)
	}

	payout, err := p.Stake.MulFloat(p.AcceptedDecimal, p.Rounding)
	if err != nil {
		return Wager{}, fmt.Errorf("wager %s potential payout: %w", p.ID, err)
	}
	if payout.Compare(p.Stake) < 0 {
		return Wager{}, fmt.Errorf("wager %s returns %s on a %s stake: %w",
			p.ID, payout, p.Stake, ErrPayoutBelowStake)
	}
	profit, err := payout.Sub(p.Stake)
	if err != nil {
		return Wager{}, fmt.Errorf("wager %s potential profit: %w", p.ID, err)
	}

	placed := p.PlacedAt.UTC()
	return Wager{
		id:           p.ID,
		userID:       p.UserID,
		kind:         p.Kind,
		legs:         legs,
		stake:        p.Stake,
		accepted:     p.AcceptedDecimal,
		rounding:     p.Rounding,
		payout:       payout,
		profit:       profit,
		teaserPoints: p.TeaserPoints,
		hasTeaser:    p.Kind == WagerKindTeaser,
		roundRobinID: p.RoundRobinID,
		status:       WagerStatusPlaced,
		placedAt:     placed,
		updatedAt:    placed,
	}, nil
}

// validateLegSet copies the legs and enforces the rules that hold for every
// ticket shape: at least one leg, none of them zero or already graded, and no
// two of them answering the same question.
func validateLegSet(owner string, legs []Leg) ([]Leg, error) {
	if len(legs) == 0 {
		return nil, fmt.Errorf("%s has no legs: %w", owner, ErrLegCount)
	}
	if len(legs) > MaxWagerLegs {
		return nil, fmt.Errorf("%s has %d legs, the maximum is %d: %w",
			owner, len(legs), MaxWagerLegs, ErrLegCount)
	}

	seenSelection := make(map[SelectionID]struct{}, len(legs))
	seenMarket := make(map[MarketID]struct{}, len(legs))
	for _, leg := range legs {
		if leg.IsZero() {
			return nil, fmt.Errorf("%s carries an unconstructed leg: %w", owner, ErrLegPriceRequired)
		}
		if leg.Status() != LegStatusPending {
			return nil, fmt.Errorf("%s leg %s is %s: %w", owner, leg.ID(), leg.Status(), ErrLegNotPending)
		}
		if _, dup := seenSelection[leg.SelectionID()]; dup {
			return nil, fmt.Errorf("%s repeats selection %s: %w",
				owner, leg.SelectionID(), ErrDuplicateSelection)
		}
		if _, dup := seenMarket[leg.MarketID()]; dup {
			return nil, fmt.Errorf("%s has two legs on market %s: %w",
				owner, leg.MarketID(), ErrDuplicateMarket)
		}
		seenSelection[leg.SelectionID()] = struct{}{}
		seenMarket[leg.MarketID()] = struct{}{}
	}
	return slices.Clone(legs), nil
}

// validateLegCount enforces the arity each ticket shape requires.
func validateLegCount(id WagerID, kind WagerKind, n int) error {
	if kind == WagerKindStraight && n != 1 {
		return fmt.Errorf("wager %s is a straight with %d legs: %w", id, n, ErrLegCount)
	}
	if kind.IsMulti() && n < 2 {
		return fmt.Errorf("wager %s is a %s with %d leg(s): %w", id, kind, n, ErrLegCount)
	}
	return nil
}

// validateTeaser enforces the correspondence between the ticket's stated teaser
// points and the teased line on every leg.
//
// The check that earns its place is the last one: that each leg's teased line
// really is the promised number of points off the line it was booked at. Both
// values are individually plausible, so a leg teased by the wrong amount — or
// teased in the wrong DIRECTION, which is the same magnitude with the wrong
// sign and therefore the more dangerous error — is invisible without it.
func validateTeaser(id WagerID, kind WagerKind, points float64, legs []Leg) error {
	if kind != WagerKindTeaser {
		// Comparing a float to zero is an "is this field set" test on raw
		// caller input, not a numeric comparison; see [Price.SameQuoteAs] for
		// the same distinction.
		if points != 0 {
			return fmt.Errorf("wager %s is a %s with %v teaser points: %w",
				id, kind, points, ErrTeaserPointsNotApplicable)
		}
		for _, leg := range legs {
			if leg.TeasedLine().Present() {
				return fmt.Errorf("wager %s is a %s but leg %s is teased: %w",
					id, kind, leg.ID(), ErrTeasedLegNotApplicable)
			}
		}
		return nil
	}

	if math.IsNaN(points) || math.IsInf(points, 0) || points <= 0 || points > MaxTeaserPoints {
		return fmt.Errorf("wager %s teaser points %v: %w", id, points, ErrTeaserPointsRequired)
	}
	for _, leg := range legs {
		teased, ok := leg.TeasedLine().Value()
		if !ok {
			return fmt.Errorf("wager %s leg %s: %w", id, leg.ID(), ErrTeasedLegRequired)
		}
		booked, ok := leg.Price().Line().Value()
		if !ok {
			return fmt.Errorf("wager %s leg %s: %w", id, leg.ID(), ErrLegLineRequired)
		}
		if math.Abs(math.Abs(teased-booked)-points) > teaserLineTolerance {
			return fmt.Errorf("wager %s leg %s moved from %s to %s, not by %v points: %w",
				id, leg.ID(), leg.Price().Line(), leg.TeasedLine(), points, ErrTeaserPointsMismatch)
		}
	}
	return nil
}

// validateRoundRobinParent enforces the biconditional that makes the round
// robin relationship checkable: a ticket is of kind round_robin exactly when it
// names a parent round robin.
func validateRoundRobinParent(id WagerID, kind WagerKind, parent RoundRobinID) error {
	if kind == WagerKindRoundRobin {
		if err := validID(string(parent)); err != nil {
			return fmt.Errorf("wager %s: %w", id, ErrRoundRobinParentRequired)
		}
		return nil
	}
	if !parent.IsZero() {
		return fmt.Errorf("wager %s is a %s naming round robin %s: %w",
			id, kind, parent, ErrRoundRobinParentNotApplicable)
	}
	return nil
}

// validateTicketPrice range-checks the accepted price and, for a straight,
// checks it against the single leg it must equal.
func validateTicketPrice(id WagerID, kind WagerKind, accepted float64, legs []Leg) error {
	if math.IsNaN(accepted) || math.IsInf(accepted, 0) {
		return fmt.Errorf("wager %s price %v: %w", id, accepted, ErrOddsNotFinite)
	}
	if accepted <= MinDecimalOdds || accepted > MaxWagerDecimal {
		return fmt.Errorf("wager %s price %v: %w", id, accepted, ErrWagerOddsOutOfRange)
	}
	if kind != WagerKindStraight {
		return nil
	}
	leg := legs[0].QuotedDecimal()
	// A relative tolerance, matching the convention the rest of the domain's
	// float comparisons use. The two numbers are meant to be the same value
	// travelling by two routes, so anything past a few thousand ULPs is a
	// plumbing fault rather than accumulated rounding.
	scale := math.Max(math.Abs(accepted), math.Abs(leg))
	if math.Abs(accepted-leg)/scale > 1e-12 {
		return fmt.Errorf("wager %s priced at %v on a leg quoted %v: %w",
			id, accepted, leg, ErrWagerPriceMismatch)
	}
	return nil
}

// ID returns the wager's identifier.
func (w Wager) ID() WagerID { return w.id }

// UserID returns the customer who placed the wager.
func (w Wager) UserID() UserID { return w.userID }

// Kind returns the ticket shape.
func (w Wager) Kind() WagerKind { return w.kind }

// Legs returns a copy of the ticket's legs. The copy is not a courtesy: without
// it a caller could grade a leg in place and bypass the wager's state machine.
func (w Wager) Legs() []Leg { return slices.Clone(w.legs) }

// LegCount returns the number of legs, without copying them.
func (w Wager) LegCount() int { return len(w.legs) }

// Leg returns the leg with the given identifier, and whether it is on this
// ticket.
func (w Wager) Leg(id LegID) (Leg, bool) {
	for _, leg := range w.legs {
		if leg.ID() == id {
			return leg, true
		}
	}
	return Leg{}, false
}

// Stake returns the amount risked.
func (w Wager) Stake() Money { return w.stake }

// AcceptedDecimal returns the ticket price the customer accepted: total return
// per unit staked with every leg winning. See the type comment for why it is
// stored rather than derived from the legs.
func (w Wager) AcceptedDecimal() float64 { return w.accepted }

// Rounding returns the rule stake × price was collapsed under, so a later
// recomputation — a partial void repricing a parlay — uses the same rule the
// ticket was written under rather than picking a fresh one.
func (w Wager) Rounding() Rounding { return w.rounding }

// PotentialPayout returns the TOTAL RETURN if every leg wins, stake included.
// It was computed once at placement and is frozen.
func (w Wager) PotentialPayout() Money { return w.payout }

// PotentialProfit returns the NET WINNINGS if every leg wins: payout minus
// stake. The pair is named as bluntly as [Price.PayoutFor] and
// [Price.ProfitFor], and for the same reason — conflating return with profit
// produces a plausible number of the right magnitude.
func (w Wager) PotentialProfit() Money { return w.profit }

// TeaserPoints returns the points each leg's line was moved by, and whether the
// ticket is a teaser.
func (w Wager) TeaserPoints() (float64, bool) { return w.teaserPoints, w.hasTeaser }

// RoundRobinID returns the round robin this ticket was expanded from, and
// whether it came from one.
func (w Wager) RoundRobinID() (RoundRobinID, bool) {
	return w.roundRobinID, w.kind == WagerKindRoundRobin
}

// Status returns the ticket's lifecycle status.
func (w Wager) Status() WagerStatus { return w.status }

// IsTerminal reports whether the ticket is closed.
func (w Wager) IsTerminal() bool { return w.status.IsTerminal() }

// PlacedAt returns when the ticket was accepted, in UTC.
func (w Wager) PlacedAt() time.Time { return w.placedAt }

// UpdatedAt returns the instant of the most recent transition, in UTC.
func (w Wager) UpdatedAt() time.Time { return w.updatedAt }

// SettledAt returns when the ticket closed, and whether it has. It is
// [Wager.UpdatedAt] once terminal; a separate field would be a second copy of
// the same instant and therefore a second thing to keep in agreement.
func (w Wager) SettledAt() (time.Time, bool) {
	if !w.status.IsTerminal() {
		return time.Time{}, false
	}
	return w.updatedAt, true
}

// Returned returns the amount paid back to the customer when the ticket closed,
// and whether it has closed. It is the ONLY authority on what settlement owes:
// a partially-voided parlay returns less than [Wager.PotentialPayout], and a
// cash-out returns whatever price was taken.
func (w Wager) Returned() (Money, bool) { return w.returned, w.hasReturned }

// NetReturn returns the customer's profit or loss on the closed ticket —
// returned minus stake, negative on a loser — and whether the ticket has
// closed. It is the per-wager P&L the leaderboard's ROI is built from.
func (w Wager) NetReturn() (Money, bool) { return w.netReturn, w.hasReturned }

// EventIDs returns the distinct events the ticket has legs on, sorted, so the
// result is stable across calls and across processes.
func (w Wager) EventIDs() []EventID {
	ids := make([]EventID, 0, len(w.legs))
	for _, leg := range w.legs {
		ids = append(ids, leg.EventID())
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// IsSameGame reports whether every leg sits on one event — a same-game parlay,
// whose legs are correlated and whose price therefore is not the product of its
// legs. It is the flag the pricing engine keys its correlation adjustment on
// (CLAUDE.md §4).
func (w Wager) IsSameGame() bool { return len(w.legs) > 1 && len(w.EventIDs()) == 1 }

// AllLegsGraded reports whether every leg has reached a terminal grading
// status, which is the precondition for grading the ticket itself.
func (w Wager) AllLegsGraded() bool {
	for _, leg := range w.legs {
		if !leg.Status().IsTerminal() {
			return false
		}
	}
	return true
}

// IsZero reports whether w is the zero Wager, which no constructor produces.
func (w Wager) IsZero() bool { return w.id.IsZero() }

// String implements fmt.Stringer.
func (w Wager) String() string {
	if w.IsZero() {
		return "wager(<zero>)"
	}
	return fmt.Sprintf("wager(%s %s %dleg stake=%s @%g %s)",
		w.id, w.kind, len(w.legs), w.stake, w.accepted, w.status)
}

// stamp copies the wager with a new UpdatedAt, enforcing monotonicity exactly
// as [Event] does. An update stamped at the current instant is accepted, since
// two events can share one.
func (w Wager) stamp(at time.Time) (Wager, error) {
	if at.IsZero() {
		return Wager{}, fmt.Errorf("wager %s update at: %w", w.id, ErrZeroTime)
	}
	u := at.UTC()
	if u.Before(w.updatedAt) {
		return Wager{}, fmt.Errorf("wager %s: update at %s precedes %s: %w",
			w.id, u.Format(time.RFC3339Nano), w.updatedAt.Format(time.RFC3339Nano), ErrStaleUpdate)
	}
	next := w
	next.legs = slices.Clone(w.legs)
	next.updatedAt = u
	return next, nil
}

// Open returns a copy of the ticket marked as running, which is the signal that
// a live cash-out can be priced against it.
func (w Wager) Open(at time.Time) (Wager, error) {
	if !w.status.CanTransitionTo(WagerStatusOpen) {
		return Wager{}, fmt.Errorf("wager %s %s → %s: %w",
			w.id, w.status, WagerStatusOpen, ErrIllegalTransition)
	}
	next, err := w.stamp(at)
	if err != nil {
		return Wager{}, err
	}
	next.status = WagerStatusOpen
	return next, nil
}

// GradeLeg returns a copy of the ticket with one leg graded.
//
// Legs grade independently and at different times, because they are on
// different games. Grading a leg does not settle the ticket — [Wager.Settle]
// does that, once the settlement service has decided what the ticket as a whole
// is worth.
//
// It refuses on a closed ticket: a settled wager's legs are part of the record.
func (w Wager) GradeLeg(id LegID, next LegStatus, at time.Time) (Wager, error) {
	if w.status.IsTerminal() {
		return Wager{}, fmt.Errorf("wager %s is %s: %w", w.id, w.status, ErrIllegalTransition)
	}
	idx := slices.IndexFunc(w.legs, func(l Leg) bool { return l.ID() == id })
	if idx < 0 {
		return Wager{}, fmt.Errorf("leg %s is not on wager %s: %w", id, w.id, ErrMismatchedParent)
	}
	graded, err := w.legs[idx].WithStatus(next, at)
	if err != nil {
		return Wager{}, fmt.Errorf("wager %s: %w", w.id, err)
	}
	stamped, err := w.stamp(at)
	if err != nil {
		return Wager{}, err
	}
	stamped.legs[idx] = graded
	return stamped, nil
}

// Settle returns a copy of the ticket closed at a graded outcome, recording the
// amount returned to the customer.
//
// next must be one of won, lost, void, or push; a cash-out goes through
// [Wager.CashOut] because it is not a grading. The returned amount is checked
// against the outcome it is filed under:
//
//	lost       → exactly zero
//	void, push → exactly the stake
//	won        → at least the stake and at most the potential payout
//
// Those are integer comparisons on [Money], so they are exact. The upper bound
// on a win is the one that matters: a partially-voided parlay legitimately pays
// LESS than the ticket's headline payout, and nothing legitimately pays more,
// so a return above the maximum is an arithmetic fault caught here rather than
// an overpayment discovered in a reconciliation.
func (w Wager) Settle(next WagerStatus, returned Money, at time.Time) (Wager, error) {
	if !next.IsGraded() {
		return Wager{}, fmt.Errorf("wager %s → %s is not a graded outcome: %w",
			w.id, next, ErrIllegalTransition)
	}
	if !w.status.CanTransitionTo(next) {
		return Wager{}, fmt.Errorf("wager %s %s → %s: %w", w.id, w.status, next, ErrIllegalTransition)
	}
	if err := w.checkReturn(next, returned); err != nil {
		return Wager{}, err
	}
	return w.settleAt(next, returned, at)
}

// CashOut returns a copy of the ticket closed at a cash-out price.
//
// The amount must be strictly positive — a cash-out for nothing is not a
// transaction anyone makes — and at most the potential payout, since no
// settlement price can exceed the maximum the ticket could ever win. It may
// legitimately be below the stake: that is what taking a bad price early means.
//
// Pricing a cash-out is not this package's job. It is a live valuation of the
// remaining legs and belongs in the pricing engine; the domain records the
// price that was taken.
func (w Wager) CashOut(amount Money, at time.Time) (Wager, error) {
	if !w.status.CanTransitionTo(WagerStatusCashedOut) {
		return Wager{}, fmt.Errorf("wager %s %s → %s: %w",
			w.id, w.status, WagerStatusCashedOut, ErrIllegalTransition)
	}
	if !amount.IsPositive() {
		return Wager{}, fmt.Errorf("wager %s cashed out for %s: %w", w.id, amount, ErrReturnAmount)
	}
	if amount.Compare(w.payout) > 0 {
		return Wager{}, fmt.Errorf("wager %s cashed out for %s above its %s maximum: %w",
			w.id, amount, w.payout, ErrReturnAmount)
	}
	return w.settleAt(WagerStatusCashedOut, amount, at)
}

// checkReturn enforces the amount rule for each graded outcome.
func (w Wager) checkReturn(outcome WagerStatus, returned Money) error {
	switch outcome {
	case WagerStatusLost:
		if !returned.IsZero() {
			return fmt.Errorf("wager %s lost but returns %s: %w", w.id, returned, ErrReturnAmount)
		}
	case WagerStatusVoid, WagerStatusPush:
		if returned.Compare(w.stake) != 0 {
			return fmt.Errorf("wager %s %s returns %s on a %s stake: %w",
				w.id, outcome, returned, w.stake, ErrReturnAmount)
		}
	case WagerStatusWon:
		if returned.Compare(w.stake) < 0 {
			return fmt.Errorf("wager %s won but returns %s, below its %s stake: %w",
				w.id, returned, w.stake, ErrReturnAmount)
		}
		if returned.Compare(w.payout) > 0 {
			return fmt.Errorf("wager %s won %s, above its %s maximum: %w",
				w.id, returned, w.payout, ErrReturnAmount)
		}
	default:
		return fmt.Errorf("wager %s → %s: %w", w.id, outcome, ErrIllegalTransition)
	}
	return nil
}

// settleAt applies a terminal status together with the amount returned.
func (w Wager) settleAt(next WagerStatus, returned Money, at time.Time) (Wager, error) {
	net, err := returned.Sub(w.stake)
	if err != nil {
		return Wager{}, fmt.Errorf("wager %s net return: %w", w.id, err)
	}
	stamped, err := w.stamp(at)
	if err != nil {
		return Wager{}, err
	}
	stamped.status = next
	stamped.returned = returned
	stamped.netReturn = net
	stamped.hasReturned = true
	return stamped, nil
}

// RoundRobin is a set of selections and the combination sizes to expand them
// into, together with the stake each resulting ticket carries.
//
// CLAUDE.md §6 lists round robin among the wager kinds and the phase brief asks
// for the relationship to be explicit, so it is a first-class value rather than
// a loop somewhere in the betting service. A "3-team round robin by 2s" is not
// one bet: it is three independent two-leg parlays — AB, AC, BC — each of which
// wins, loses, and settles on its own. Modelling it as one wager would make
// "how much did ticket AC return" unanswerable.
//
// The expansion itself is a pure function of the selections and the sizes, so
// it lives here: [RoundRobin.Combinations] produces the leg sets, and the
// betting service turns each into a [WagerKindRoundRobin] wager naming this
// round robin as its parent.
type RoundRobin struct {
	id                  RoundRobinID
	userID              UserID
	legs                []Leg
	sizes               []int
	stakePerCombination Money
	placedAt            time.Time
}

// RoundRobinParams is the input to NewRoundRobin.
type RoundRobinParams struct {
	ID     RoundRobinID
	UserID UserID

	// Legs are the selections the combinations are drawn from. They are subject
	// to the same duplicate rules as a wager's legs, since every combination is
	// itself a ticket.
	Legs []Leg

	// Sizes are the combination sizes to generate — {2} for "by 2s", {2, 3} for
	// "by 2s and 3s". Each must be at least 2 and at most len(Legs). The slice
	// is sorted and de-duplicated, so {3, 2, 3} and {2, 3} describe the same
	// round robin.
	Sizes []int

	// StakePerCombination is the stake on EACH generated ticket, not the total.
	// That is how books quote it and how customers think about it: "$5 round
	// robin by 2s" on four selections risks $30, not $5.
	StakePerCombination Money

	PlacedAt time.Time
}

// NewRoundRobin validates its input and returns an immutable RoundRobin.
func NewRoundRobin(p RoundRobinParams) (RoundRobin, error) {
	if err := validID(string(p.ID)); err != nil {
		return RoundRobin{}, idErr("round robin id", string(p.ID), err)
	}
	if err := validID(string(p.UserID)); err != nil {
		return RoundRobin{}, idErr("user id", string(p.UserID), err)
	}
	// The leg rules are identical to a wager's, so they are reused rather than
	// restated; a divergence between the two would show up as a round robin
	// that expands into tickets the wager constructor then rejects.
	legs, err := validateLegSet("round robin "+string(p.ID), p.Legs)
	if err != nil {
		return RoundRobin{}, err
	}
	if len(legs) < 2 {
		return RoundRobin{}, fmt.Errorf("round robin %s has %d leg(s): %w", p.ID, len(legs), ErrLegCount)
	}
	if len(legs) > MaxRoundRobinLegs {
		return RoundRobin{}, fmt.Errorf("round robin %s has %d legs, the maximum is %d: %w",
			p.ID, len(legs), MaxRoundRobinLegs, ErrLegCount)
	}

	sizes := slices.Clone(p.Sizes)
	slices.Sort(sizes)
	sizes = slices.Compact(sizes)
	if len(sizes) == 0 {
		return RoundRobin{}, fmt.Errorf("round robin %s names no combination size: %w", p.ID, ErrCombinationSize)
	}
	for _, k := range sizes {
		if k < 2 || k > len(legs) {
			return RoundRobin{}, fmt.Errorf("round robin %s combination size %d over %d selections: %w",
				p.ID, k, len(legs), ErrCombinationSize)
		}
	}

	if !p.StakePerCombination.IsPositive() {
		return RoundRobin{}, fmt.Errorf("round robin %s stake %s: %w",
			p.ID, p.StakePerCombination, ErrStakeNotPositive)
	}
	if p.PlacedAt.IsZero() {
		return RoundRobin{}, fmt.Errorf("round robin %s placed at: %w", p.ID, ErrZeroTime)
	}

	return RoundRobin{
		id:                  p.ID,
		userID:              p.UserID,
		legs:                legs,
		sizes:               sizes,
		stakePerCombination: p.StakePerCombination,
		placedAt:            p.PlacedAt.UTC(),
	}, nil
}

// ID returns the round robin's identifier, which every ticket it expands into
// names as its parent.
func (r RoundRobin) ID() RoundRobinID { return r.id }

// UserID returns the customer who placed it.
func (r RoundRobin) UserID() UserID { return r.userID }

// Legs returns a copy of the selections the combinations are drawn from.
func (r RoundRobin) Legs() []Leg { return slices.Clone(r.legs) }

// Sizes returns a copy of the combination sizes, sorted and de-duplicated.
func (r RoundRobin) Sizes() []int { return slices.Clone(r.sizes) }

// StakePerCombination returns the stake carried by each generated ticket.
func (r RoundRobin) StakePerCombination() Money { return r.stakePerCombination }

// PlacedAt returns when the round robin was accepted, in UTC.
func (r RoundRobin) PlacedAt() time.Time { return r.placedAt }

// IsZero reports whether r is the zero RoundRobin, which no constructor
// produces.
func (r RoundRobin) IsZero() bool { return r.id.IsZero() }

// String implements fmt.Stringer.
func (r RoundRobin) String() string {
	if r.IsZero() {
		return "roundrobin(<zero>)"
	}
	return fmt.Sprintf("roundrobin(%s %dsel sizes=%v stake=%s)",
		r.id, len(r.legs), r.sizes, r.stakePerCombination)
}

// CombinationCount returns how many tickets the round robin expands into: the
// sum of C(n, k) over the stated sizes.
//
// It is computed by counting the generated index sets rather than by evaluating
// the binomial coefficient, so it cannot disagree with [RoundRobin.Combinations]
// and cannot overflow: [MaxRoundRobinLegs] caps the total at 2^10.
func (r RoundRobin) CombinationCount() int {
	total := 0
	for _, k := range r.sizes {
		total += len(combinationIndexes(len(r.legs), k))
	}
	return total
}

// Combinations returns the leg set of every ticket the round robin expands
// into, ordered by size ascending and then lexicographically by selection
// position, so the expansion is deterministic — the same round robin produces
// the same tickets in the same order in every process, which is what makes the
// generated wager identifiers reproducible and the expansion testable.
func (r RoundRobin) Combinations() [][]Leg {
	out := make([][]Leg, 0, r.CombinationCount())
	for _, k := range r.sizes {
		for _, idx := range combinationIndexes(len(r.legs), k) {
			combo := make([]Leg, 0, k)
			for _, i := range idx {
				combo = append(combo, r.legs[i])
			}
			out = append(out, combo)
		}
	}
	return out
}

// TotalStake returns the amount the whole round robin risks: the per-ticket
// stake times the ticket count.
//
// [Money.MulInt] is exact — no rounding is possible on a whole multiplier — so
// the total can never disagree with the sum of the individual tickets' stakes
// by a minor unit, which is the property the ledger's balance check depends on.
func (r RoundRobin) TotalStake() (Money, error) {
	total, err := r.stakePerCombination.MulInt(int64(r.CombinationCount()))
	if err != nil {
		return 0, fmt.Errorf("round robin %s total stake: %w", r.id, err)
	}
	return total, nil
}

// combinationIndexes returns every strictly-increasing k-subset of [0, n) in
// lexicographic order.
//
// The standard odometer: advance the rightmost index that has not hit its
// ceiling, then reset everything to its right to the tightest packing. It
// allocates exactly C(n, k) slices and terminates after exactly that many
// steps, which — with n bounded by [MaxRoundRobinLegs] — is at most 252.
func combinationIndexes(n, k int) [][]int {
	if k < 1 || k > n {
		return nil
	}
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i
	}
	var out [][]int
	for {
		out = append(out, slices.Clone(idx))
		i := k - 1
		for i >= 0 && idx[i] == n-k+i {
			i--
		}
		if i < 0 {
			return out
		}
		idx[i]++
		for j := i + 1; j < k; j++ {
			idx[j] = idx[j-1] + 1
		}
	}
}
