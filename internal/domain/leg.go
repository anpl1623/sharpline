package domain

import (
	"fmt"
	"time"
)

// LegID identifies a Leg.
type LegID string

// NewLegID validates and returns a LegID.
func NewLegID(s string) (LegID, error) {
	if err := validID(s); err != nil {
		return "", idErr("leg id", s, err)
	}
	return LegID(s), nil
}

// String returns the identifier as a bare string.
func (id LegID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id LegID) IsZero() bool { return id == "" }

// Leg failures.
var (
	ErrUnknownLegStatus = fmt.Errorf("%w: not a defined leg status", ErrInvalid)

	// ErrLegPriceMismatch reports a price that quotes a different selection
	// than the leg claims to back. It is the guard on the copied-price design:
	// a leg whose price belongs to another selection is a mis-booked bet.
	ErrLegPriceMismatch = fmt.Errorf("%w: the leg's price quotes a different selection", ErrInvalid)

	// ErrLegPriceRequired reports a leg built with the zero Price, which no
	// constructor produces. A leg without a booked price is the exact defect
	// this type exists to prevent, so it is refused rather than defaulted.
	ErrLegPriceRequired = fmt.Errorf("%w: a leg must carry the price it was booked at", ErrInvalid)

	// ErrLegNotTeasable reports an attempt to tease a market type that has no
	// line to move. Only spreads and totals can be teased.
	ErrLegNotTeasable = fmt.Errorf("%w: only a spread or total leg can be teased", ErrInvalid)

	// ErrLegLineRequired reports a teased leg whose underlying price carries no
	// line, so there is nothing for the tease to be measured against.
	ErrLegLineRequired = fmt.Errorf("%w: a teased leg's price must carry a line", ErrInvalid)

	// ErrLegNotGraded reports a settlement computation attempted on a leg that
	// is still pending. It is a conflict rather than bad input: the same call a
	// minute after the whistle would have succeeded.
	ErrLegNotGraded = fmt.Errorf("%w: the leg has not been graded", ErrConflict)
)

// LegStatus is the grading lifecycle of a single leg.
//
// The legal transitions are:
//
//	pending → won | lost | void | push
//	won | lost | void | push → (terminal)
//
// Terminal really is terminal, and that is the load-bearing decision. A result
// that is later corrected — an overturned call, a scoring revision, a provider
// sending a wrong final — is NOT modelled by re-grading the leg. It is modelled
// as an [EntryKindAdjustment] transaction in the ledger, which leaves both the
// original grading and the correction on the record. Letting a graded leg
// change status would silently rewrite history in the one part of the system
// whose entire purpose is to be auditable.
//
// s → s is legal, for the same reason it is legal on [EventStatus]: Kafka
// delivers at least once (CLAUDE.md §3), so the settle consumer will be handed
// "this leg won" twice as a matter of routine, and making the redelivery an
// error would force every consumer to special-case a healthy system.
type LegStatus uint8

const (
	// LegStatusUnknown is the invalid zero value.
	LegStatusUnknown LegStatus = iota

	// LegStatusPending means the leg has not been graded.
	LegStatusPending

	// LegStatusWon means the selection came in.
	LegStatusWon

	// LegStatusLost means the selection did not come in.
	LegStatusLost

	// LegStatusVoid means the leg is removed from the wager — the market was
	// voided, the event was cancelled, a player did not take the field. A void
	// leg contributes a multiplier of exactly 1 to a parlay, which is to say
	// the parlay reprices as though the leg had never been added.
	LegStatusVoid

	// LegStatusPush means the result landed exactly on the line — a -3 spread
	// won by 3, a total of 47 landing on 47. Money-wise it is identical to a
	// void (multiplier 1), but it is kept distinct because a push is a GRADED
	// outcome and a void is a CANCELLED one. Collapsing them would make "how
	// often does this user land on the number" unanswerable and would hide a
	// provider that is voiding markets it should be grading.
	LegStatusPush
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (s LegStatus) String() string {
	switch s {
	case LegStatusPending:
		return "pending"
	case LegStatusWon:
		return "won"
	case LegStatusLost:
		return "lost"
	case LegStatusVoid:
		return "void"
	case LegStatusPush:
		return "push"
	default:
		return "unknown"
	}
}

// Valid reports whether s is a defined status.
func (s LegStatus) Valid() bool {
	switch s {
	case LegStatusPending, LegStatusWon, LegStatusLost, LegStatusVoid, LegStatusPush:
		return true
	default:
		return false
	}
}

// ParseLegStatus is the inverse of String for the defined statuses.
func ParseLegStatus(s string) (LegStatus, error) {
	switch s {
	case "pending":
		return LegStatusPending, nil
	case "won":
		return LegStatusWon, nil
	case "lost":
		return LegStatusLost, nil
	case "void":
		return LegStatusVoid, nil
	case "push":
		return LegStatusPush, nil
	default:
		return LegStatusUnknown, fmt.Errorf("leg status %q: %w", sample(s), ErrUnknownLegStatus)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s LegStatus) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("leg status %d: %w", uint8(s), ErrUnknownLegStatus)
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *LegStatus) UnmarshalText(b []byte) error {
	parsed, err := ParseLegStatus(string(b))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// IsTerminal reports whether the leg has been graded and no further transition
// is possible.
func (s LegStatus) IsTerminal() bool {
	switch s {
	case LegStatusWon, LegStatusLost, LegStatusVoid, LegStatusPush:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether next is a legal successor of s.
//
// It is a switch rather than a package-level map for the reason given on
// [EventStatus.CanTransitionTo]: a map would be package-level mutable state
// that any code could edit at run time, which CLAUDE.md §12 forbids.
func (s LegStatus) CanTransitionTo(next LegStatus) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	if s == next {
		return true
	}
	if s == LegStatusPending {
		return next.IsTerminal()
	}
	return false
}

// Leg is one selection inside a wager, holding the price it was booked at.
//
// # The copied price is the whole point
//
// CLAUDE.md §4: "Legs hold the price *at placement time*, never a live
// reference." The emphasis is the charter's. This type carries a [Price]
// VALUE — not a MarketID to look up, not a pointer into a live snapshot, not an
// index into a cache — so there is no expression anywhere in the program that
// can re-resolve a booked leg to a moved line. Price has unexported fields, no
// setters, and every operation on it returns a new value, so once a Leg is
// constructed the number it will be graded and paid at is frozen inside it.
//
// That structural guarantee is worth more than a comment saying "don't do
// that", because the bug it prevents is invisible in review: a leg that reads
// the current line grades correctly in every test where the line never moves,
// and pays the wrong amount exactly when it matters.
//
// # What else is copied, and why
//
// The market type and the selection role are copied too. Neither of them ever
// changes on a live market, so copying them is not about drift — it is about
// self-sufficiency. Grading a leg must not require re-reading the Market it
// came from, because that read is precisely the thing this type exists to make
// impossible. A leg carries everything the grader needs.
//
// The event and market identifiers are kept for the questions that are ABOUT
// the relationship rather than about the money: which legs of this parlay are
// on the same game (same-game correlation, CLAUDE.md §4), and can two legs of
// this ticket both win (two answers to one market cannot). They are never used
// to fetch a price.
//
// # Teasing
//
// A teaser moves each leg's line in the bettor's favour, so a teased leg grades
// at a line the market never quoted. Rather than forge a Price at the moved
// line — which would corrupt the line history and destroy CLV, since the book
// never traded there — the leg keeps the REAL market price it was booked
// against and carries the teased line beside it. [Leg.GradingLine] returns the
// line the leg actually settles at; [Leg.Price] returns what the market was
// when the ticket was written.
type Leg struct {
	id          LegID
	eventID     EventID
	marketID    MarketID
	marketType  MarketType
	selectionID SelectionID
	role        SelectionRole
	price       Price
	teasedLine  Line
	status      LegStatus
	gradedAt    time.Time
	hasGradedAt bool
}

// LegParams is the input to NewLeg.
//
// It takes flat fields rather than the parent entities so that phase 2 can
// rebuild a leg from stored columns without having to resurrect the Market and
// Selection it was booked against — which, for a market that has since been
// deleted or re-lined, it could not do. Use [NewLegFrom] at placement time,
// where the live entities are in hand and can be cross-checked.
type LegParams struct {
	ID LegID

	// EventID and MarketID are the relationship keys. They answer "same game?"
	// and "same question?" and nothing else; no code may use them to look up a
	// price for a booked leg.
	EventID  EventID
	MarketID MarketID

	// MarketType and Role are copied from the market and selection so the leg
	// can be graded without re-reading either.
	MarketType MarketType
	Role       SelectionRole

	SelectionID SelectionID

	// Price is the quote the leg was booked at. It must quote SelectionID.
	Price Price

	// TeasedLine is the moved line a teaser leg grades at. It is absent on
	// every non-teaser leg. See the type comment.
	TeasedLine Line
}

// NewLeg validates its input and returns an immutable Leg in
// [LegStatusPending].
func NewLeg(p LegParams) (Leg, error) {
	if err := validID(string(p.ID)); err != nil {
		return Leg{}, idErr("leg id", string(p.ID), err)
	}
	if err := validID(string(p.EventID)); err != nil {
		return Leg{}, idErr("event id", string(p.EventID), err)
	}
	if err := validID(string(p.MarketID)); err != nil {
		return Leg{}, idErr("market id", string(p.MarketID), err)
	}
	if err := validID(string(p.SelectionID)); err != nil {
		return Leg{}, idErr("selection id", string(p.SelectionID), err)
	}
	if !p.MarketType.Valid() {
		return Leg{}, fmt.Errorf("leg %s: %w", p.ID, ErrUnknownMarketType)
	}
	if !p.Role.Valid() {
		return Leg{}, fmt.Errorf("leg %s: %w", p.ID, ErrUnknownSelectionRole)
	}
	if !p.MarketType.AllowsRole(p.Role) {
		return Leg{}, fmt.Errorf("leg %s is %s on a %s market: %w",
			p.ID, p.Role, p.MarketType, ErrRoleNotApplicable)
	}
	if p.Price.IsZero() {
		return Leg{}, fmt.Errorf("leg %s: %w", p.ID, ErrLegPriceRequired)
	}
	if p.Price.SelectionID() != p.SelectionID {
		return Leg{}, fmt.Errorf("leg %s backs %s but its price quotes %s: %w",
			p.ID, p.SelectionID, p.Price.SelectionID(), ErrLegPriceMismatch)
	}

	leg := Leg{
		id:          p.ID,
		eventID:     p.EventID,
		marketID:    p.MarketID,
		marketType:  p.MarketType,
		selectionID: p.SelectionID,
		role:        p.Role,
		price:       p.Price,
		status:      LegStatusPending,
	}
	if p.TeasedLine.Present() {
		teased, err := leg.WithTeasedLine(p.TeasedLine)
		if err != nil {
			return Leg{}, err
		}
		return teased, nil
	}
	return leg, nil
}

// NewLegFrom builds a leg from the live entities it is booked against, and is
// the constructor the bet-slip path should use.
//
// It runs [ValidatePriceForSelection], so it rejects the two mistakes a flat
// constructor cannot see: a selection that does not answer the given market,
// and — the important one — a price taken at a line the selection is no longer
// trading at. A ticket written against a stale line is not merely out of date,
// it grades the customer at a handicap they never took.
func NewLegFrom(id LegID, m Market, s Selection, p Price) (Leg, error) {
	if err := ValidatePriceForSelection(m, s, p); err != nil {
		return Leg{}, fmt.Errorf("leg %s: %w", id, err)
	}
	return NewLeg(LegParams{
		ID:          id,
		EventID:     m.EventID(),
		MarketID:    m.ID(),
		MarketType:  m.Type(),
		Role:        s.Role(),
		SelectionID: s.ID(),
		Price:       p,
	})
}

// ID returns the leg's identifier.
func (l Leg) ID() LegID { return l.id }

// EventID returns the event this leg is on. It is a relationship key, used for
// same-game detection, never to look up a price.
func (l Leg) EventID() EventID { return l.eventID }

// MarketID returns the market this leg answers.
func (l Leg) MarketID() MarketID { return l.marketID }

// MarketType returns the market type copied at placement.
func (l Leg) MarketType() MarketType { return l.marketType }

// SelectionID returns the selection this leg backs.
func (l Leg) SelectionID() SelectionID { return l.selectionID }

// Role returns the selection role copied at placement.
func (l Leg) Role() SelectionRole { return l.role }

// Price returns the quote the leg was booked at. The value is a copy; there is
// no way to reach the market it came from.
func (l Leg) Price() Price { return l.price }

// QuotedDecimal returns the decimal odds the leg was booked at.
func (l Leg) QuotedDecimal() float64 { return l.price.Decimal() }

// TeasedLine returns the teased line and whether one is set.
func (l Leg) TeasedLine() Line { return l.teasedLine }

// GradingLine returns the line this leg actually settles at: the teased line
// when the leg was teased, otherwise the line the price was quoted at.
//
// Every grading path must read this rather than Price().Line(), for the same
// reason every board must read [EffectiveLine] rather than Market.Line().
func (l Leg) GradingLine() Line {
	if l.teasedLine.Present() {
		return l.teasedLine
	}
	return l.price.Line()
}

// Status returns the leg's grading status.
func (l Leg) Status() LegStatus { return l.status }

// GradedAt returns when the leg was graded and whether it has been. The legs of
// a parlay grade at different times because they are on different games, so
// this is per-leg rather than per-wager.
func (l Leg) GradedAt() (time.Time, bool) { return l.gradedAt, l.hasGradedAt }

// IsZero reports whether l is the zero Leg, which no constructor produces.
func (l Leg) IsZero() bool { return l.id.IsZero() }

// Equal reports whether two legs are identical, price and grading state
// included. It exists so callers never reach for == on a type that embeds a
// time.Time, where == compares monotonic-clock readings and location pointers
// rather than instants.
func (l Leg) Equal(o Leg) bool {
	if l.hasGradedAt != o.hasGradedAt || (l.hasGradedAt && !l.gradedAt.Equal(o.gradedAt)) {
		return false
	}
	return l.id == o.id &&
		l.eventID == o.eventID &&
		l.marketID == o.marketID &&
		l.marketType == o.marketType &&
		l.selectionID == o.selectionID &&
		l.role == o.role &&
		l.price.Equal(o.price) &&
		l.teasedLine.Equal(o.teasedLine) &&
		l.status == o.status
}

// String implements fmt.Stringer.
func (l Leg) String() string {
	if l.IsZero() {
		return "leg(<zero>)"
	}
	return fmt.Sprintf("leg(%s %s %s @%g line=%s %s)",
		l.id, l.selectionID, l.role, l.price.Decimal(), l.GradingLine(), l.status)
}

// WithTeasedLine returns a copy of the leg grading at the given teased line.
//
// It refuses on any market type without a line to move — you cannot tease a
// moneyline, and a book that let you would be giving away the whole edge the
// teaser price is built on — and on a price that carries no line, which would
// leave the tease unmeasurable. It also refuses on a graded leg: moving the
// line after the result is known is not a booking operation.
func (l Leg) WithTeasedLine(teased Line) (Leg, error) {
	if l.marketType != MarketTypeSpread && l.marketType != MarketTypeTotal {
		return Leg{}, fmt.Errorf("leg %s is a %s market: %w", l.id, l.marketType, ErrLegNotTeasable)
	}
	if !teased.Present() {
		return Leg{}, fmt.Errorf("leg %s teased line: %w", l.id, ErrLineRequired)
	}
	if !l.price.Line().Present() {
		return Leg{}, fmt.Errorf("leg %s: %w", l.id, ErrLegLineRequired)
	}
	if l.status.IsTerminal() {
		return Leg{}, fmt.Errorf("leg %s is already %s: %w", l.id, l.status, ErrIllegalTransition)
	}
	next := l
	next.teasedLine = teased
	return next, nil
}

// WithStatus returns a copy of the leg in the next grading status.
//
// It fails with [ErrIllegalTransition] if the edge is not in the lifecycle, and
// with [ErrZeroTime] if at is unset. Re-grading to the status the leg already
// holds is legal and idempotent: it keeps the ORIGINAL gradedAt rather than
// advancing it, so a redelivered settlement event cannot move the recorded
// grading time.
func (l Leg) WithStatus(next LegStatus, at time.Time) (Leg, error) {
	if !next.Valid() {
		return Leg{}, fmt.Errorf("leg %s → %d: %w", l.id, uint8(next), ErrUnknownLegStatus)
	}
	if !l.status.CanTransitionTo(next) {
		return Leg{}, fmt.Errorf("leg %s %s → %s: %w", l.id, l.status, next, ErrIllegalTransition)
	}
	if at.IsZero() {
		return Leg{}, fmt.Errorf("leg %s graded at: %w", l.id, ErrZeroTime)
	}
	if l.status == next {
		return l, nil
	}
	nextLeg := l
	nextLeg.status = next
	if next.IsTerminal() {
		nextLeg.gradedAt = at.UTC()
		nextLeg.hasGradedAt = true
	}
	return nextLeg, nil
}

// GradedMultiplier returns the factor this leg contributes to a multiplicative
// combination — the parlay rule:
//
//	won   → the decimal odds the leg was booked at
//	lost  → 0, which kills the whole ticket
//	void  → 1, so the parlay reprices as if the leg were never added
//	push  → 1, same money, different reason (see [LegStatusPush])
//
// It returns [ErrLegNotGraded] on a pending leg rather than guessing, because
// the only defensible guess — "assume it wins" — is the potential payout, and
// silently returning that from a function named for settlement is how a
// not-yet-final parlay gets paid out.
//
// This is the PARLAY rule and it does not describe a teaser. A teaser's ticket
// price is fixed when the ticket is written and is not the product of its legs;
// how a teaser reduces on a push is house policy and belongs in the settlement
// service, not here.
func (l Leg) GradedMultiplier() (float64, error) {
	switch l.status {
	case LegStatusWon:
		return l.price.Decimal(), nil
	case LegStatusLost:
		return 0, nil
	case LegStatusVoid, LegStatusPush:
		return 1, nil
	default:
		return 0, fmt.Errorf("leg %s is %s: %w", l.id, l.status, ErrLegNotGraded)
	}
}
