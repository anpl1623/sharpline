package domain

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Line is an optional handicap or threshold.
//
// CLAUDE.md §4 says a market "carries a market type and a line where
// applicable", and the whole design of this type is the phrase "where
// applicable". A moneyline has no line; a pick'em spread has a line and that
// line is exactly 0.0, which is a real, meaningful, frequently-traded value. A
// bare float64 cannot tell those two apart, and the failure mode is silent — a
// moneyline renders as "PK" and a pick'em looks like an absent field.
//
// So absence is carried in a separate bool, and the zero Line is absent, which
// makes the safe state the default. Reading a Line is deliberately awkward:
//
//	if v, ok := m.Line().Value(); ok { … }
//
// The comma-ok shape is the same one Go uses for map lookups and type
// assertions, so it is familiar, and it makes ignoring absence a visible act
// rather than an omission.
//
// Line is comparable with ==, and that is safe rather than accidental: NewLine
// rejects NaN, so no Line can hold the one float64 value that is not equal to
// itself.
type Line struct {
	value   float64
	present bool
}

// NoLine returns the absent Line. It is identical to the zero value and exists
// so that call sites read as a decision rather than as a forgotten field.
func NoLine() Line { return Line{} }

// NewLine returns a present Line holding v.
//
// NaN and infinity are rejected — they are what a division-by-zero or a failed
// parse produces upstream, and letting one into a market means it propagates
// through every comparison silently, since NaN compares false against
// everything including itself.
func NewLine(v float64) (Line, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return Line{}, fmt.Errorf("line %v: %w", v, ErrLineNotFinite)
	}
	return Line{value: v, present: true}, nil
}

// Present reports whether a line is set.
func (l Line) Present() bool { return l.present }

// Value returns the line and whether it is set.
func (l Line) Value() (float64, bool) { return l.value, l.present }

// ValueOr returns the line, or fallback when it is absent. Use it only where a
// sensible fallback genuinely exists; where one does not, use Value.
func (l Line) ValueOr(fallback float64) float64 {
	if !l.present {
		return fallback
	}
	return l.value
}

// Invert returns the line negated, preserving presence. It converts between the
// two sides of a handicap: a home line of -3.5 is an away line of +3.5. An
// absent line inverts to an absent line.
func (l Line) Invert() Line {
	if !l.present {
		return Line{}
	}
	// Negating 0.0 yields -0.0, which compares equal to 0.0 but formats as
	// "-0". A pick'em must invert to a pick'em that renders as "0".
	if l.value == 0 {
		return Line{value: 0, present: true}
	}
	return Line{value: -l.value, present: true}
}

// Equal reports exact equality, absence included. Two absent lines are equal;
// an absent line never equals a present one.
func (l Line) Equal(o Line) bool { return l == o }

// String implements fmt.Stringer. An absent line renders as "none" rather than
// as the empty string, so that it is visible in a log line.
func (l Line) String() string {
	if !l.present {
		return "none"
	}
	return strconv.FormatFloat(l.value, 'f', -1, 64)
}

// SignedString renders the line with an explicit sign, which is how a handicap
// is displayed on a board: "+3.5", "-7", "0" for a pick'em.
func (l Line) SignedString() string {
	if !l.present {
		return "none"
	}
	if l.value > 0 {
		return "+" + strconv.FormatFloat(l.value, 'f', -1, 64)
	}
	return strconv.FormatFloat(l.value, 'f', -1, 64)
}

// MarshalJSON encodes a present line as a JSON number and an absent one as
// null. Without this the distinction the type exists to preserve would be lost
// the moment a market crossed the bus or the wire.
func (l Line) MarshalJSON() ([]byte, error) {
	if !l.present {
		return []byte("null"), nil
	}
	return []byte(strconv.FormatFloat(l.value, 'f', -1, 64)), nil
}

// UnmarshalJSON decodes null as absent and a JSON number as present. Like the
// enums' UnmarshalText it takes a pointer receiver because it is a construction
// path, not a mutation of an already-valid value.
func (l *Line) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*l = Line{}
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("line %q: %w", sample(s), ErrLineSyntax)
	}
	parsed, err := NewLine(v)
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}

// LineRule states whether a market type takes a line.
type LineRule uint8

const (
	// LineRuleUnknown is the invalid zero value.
	LineRuleUnknown LineRule = iota
	// LineRuleForbidden means the market type must not carry a line.
	LineRuleForbidden
	// LineRuleRequired means the market type must carry a line.
	LineRuleRequired
	// LineRuleOptional means the market type may carry a line.
	LineRuleOptional
)

// String implements fmt.Stringer.
func (r LineRule) String() string {
	switch r {
	case LineRuleForbidden:
		return "forbidden"
	case LineRuleRequired:
		return "required"
	case LineRuleOptional:
		return "optional"
	default:
		return "unknown"
	}
}

// MarketType is the question a market asks.
//
// The set is exactly the five CLAUDE.md §4 names — moneyline, spread, total,
// player prop, futures — and no more. Team totals, alternate lines, and
// period markets are all real and all tempting, but adding a type here without
// a corresponding pricing and grading rule elsewhere produces a market the
// system can quote and cannot settle.
type MarketType uint8

const (
	// MarketTypeUnknown is the invalid zero value.
	MarketTypeUnknown MarketType = iota

	// MarketTypeMoneyline asks who wins outright. No line.
	MarketTypeMoneyline

	// MarketTypeSpread asks who wins after a handicap. The line is required and
	// may be zero (a pick'em).
	MarketTypeSpread

	// MarketTypeTotal asks whether combined scoring lands over or under a
	// threshold. The line is required and must be positive.
	MarketTypeTotal

	// MarketTypePlayerProp asks about an individual's performance. The line is
	// optional: "over 24.5 points" carries one, "first to score" does not.
	MarketTypePlayerProp

	// MarketTypeFutures asks who wins a competition. No line.
	MarketTypeFutures
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (t MarketType) String() string {
	switch t {
	case MarketTypeMoneyline:
		return "moneyline"
	case MarketTypeSpread:
		return "spread"
	case MarketTypeTotal:
		return "total"
	case MarketTypePlayerProp:
		return "player_prop"
	case MarketTypeFutures:
		return "futures"
	default:
		return "unknown"
	}
}

// Valid reports whether t is a defined market type.
func (t MarketType) Valid() bool {
	switch t {
	case MarketTypeMoneyline, MarketTypeSpread, MarketTypeTotal, MarketTypePlayerProp, MarketTypeFutures:
		return true
	default:
		return false
	}
}

// ParseMarketType is the inverse of String for the defined types.
func ParseMarketType(s string) (MarketType, error) {
	switch s {
	case "moneyline":
		return MarketTypeMoneyline, nil
	case "spread":
		return MarketTypeSpread, nil
	case "total":
		return MarketTypeTotal, nil
	case "player_prop":
		return MarketTypePlayerProp, nil
	case "futures":
		return MarketTypeFutures, nil
	default:
		return MarketTypeUnknown, fmt.Errorf("market type %q: %w", sample(s), ErrUnknownMarketType)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (t MarketType) MarshalText() ([]byte, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("market type %d: %w", uint8(t), ErrUnknownMarketType)
	}
	return []byte(t.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *MarketType) UnmarshalText(b []byte) error {
	parsed, err := ParseMarketType(string(b))
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// LineRule returns whether this market type takes a line.
func (t MarketType) LineRule() LineRule {
	switch t {
	case MarketTypeMoneyline, MarketTypeFutures:
		return LineRuleForbidden
	case MarketTypeSpread, MarketTypeTotal:
		return LineRuleRequired
	case MarketTypePlayerProp:
		return LineRuleOptional
	default:
		return LineRuleUnknown
	}
}

// NeedsSubject reports whether this market type names the individual it is
// about. Only player props do.
func (t MarketType) NeedsSubject() bool { return t == MarketTypePlayerProp }

// validateLine applies the type's line rule to a candidate line.
func (t MarketType) validateLine(l Line) error {
	switch t.LineRule() {
	case LineRuleForbidden:
		if l.Present() {
			return fmt.Errorf("%s market: %w", t, ErrLineNotApplicable)
		}
	case LineRuleRequired:
		if !l.Present() {
			return fmt.Errorf("%s market: %w", t, ErrLineRequired)
		}
	case LineRuleOptional:
		// Either state is acceptable.
	default:
		return fmt.Errorf("market type %d: %w", uint8(t), ErrUnknownMarketType)
	}

	// A total is a threshold on combined scoring, which is non-negative by
	// construction, so a zero or negative total is a parse error rather than a
	// tradeable market. A spread has no such restriction: zero is a pick'em and
	// negative is the favourite's handicap.
	if t == MarketTypeTotal {
		if v, ok := l.Value(); ok && v <= 0 {
			return fmt.Errorf("total line %v: %w", v, ErrLineNotPositive)
		}
	}
	return nil
}

// MarketStatus is the market lifecycle. It is independent of the event's:
// CLAUDE.md §6 gives an admin the power to suspend one market while the rest of
// the event trades on.
//
// The legal transitions are:
//
//	open      → suspended | closed | voided
//	suspended → open | closed | voided
//	closed    → settled | voided
//	settled   → (terminal)
//	voided    → (terminal)
//
// There is no edge back from closed to open. Closing is what a market does when
// its event starts or its outcome is determined; reopening it would let a wager
// be placed on a known result, which is the single worst bug this state machine
// can have. A market closed in error is replaced, not reopened.
type MarketStatus uint8

const (
	// MarketStatusUnknown is the invalid zero value.
	MarketStatusUnknown MarketStatus = iota

	// MarketStatusOpen means the market accepts wagers.
	MarketStatusOpen

	// MarketStatusSuspended means the market is temporarily not accepting
	// wagers — a line move in progress, an injury, an operator hold.
	MarketStatusSuspended

	// MarketStatusClosed means the market accepts no further wagers and is
	// awaiting a result.
	MarketStatusClosed

	// MarketStatusSettled means the market has been graded. Terminal.
	MarketStatusSettled

	// MarketStatusVoided means the market will not be graded and every wager on
	// it is returned. Terminal.
	MarketStatusVoided
)

// String implements fmt.Stringer.
func (s MarketStatus) String() string {
	switch s {
	case MarketStatusOpen:
		return "open"
	case MarketStatusSuspended:
		return "suspended"
	case MarketStatusClosed:
		return "closed"
	case MarketStatusSettled:
		return "settled"
	case MarketStatusVoided:
		return "voided"
	default:
		return "unknown"
	}
}

// Valid reports whether s is a defined market status.
func (s MarketStatus) Valid() bool {
	switch s {
	case MarketStatusOpen, MarketStatusSuspended, MarketStatusClosed, MarketStatusSettled, MarketStatusVoided:
		return true
	default:
		return false
	}
}

// ParseMarketStatus is the inverse of String for the defined statuses.
func ParseMarketStatus(s string) (MarketStatus, error) {
	switch s {
	case "open":
		return MarketStatusOpen, nil
	case "suspended":
		return MarketStatusSuspended, nil
	case "closed":
		return MarketStatusClosed, nil
	case "settled":
		return MarketStatusSettled, nil
	case "voided":
		return MarketStatusVoided, nil
	default:
		return MarketStatusUnknown, fmt.Errorf("market status %q: %w", sample(s), ErrUnknownMarketStatus)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s MarketStatus) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("market status %d: %w", uint8(s), ErrUnknownMarketStatus)
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *MarketStatus) UnmarshalText(b []byte) error {
	parsed, err := ParseMarketStatus(string(b))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// IsTerminal reports whether no further transition is possible.
func (s MarketStatus) IsTerminal() bool {
	return s == MarketStatusSettled || s == MarketStatusVoided
}

// AcceptsWagers reports whether the market takes new wagers.
func (s MarketStatus) AcceptsWagers() bool { return s == MarketStatusOpen }

// CanTransitionTo reports whether next is a legal successor of s. As with
// EventStatus, s → s is legal so that at-least-once redelivery is a no-op
// rather than an error.
func (s MarketStatus) CanTransitionTo(next MarketStatus) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	if s == next {
		return true
	}
	switch s {
	case MarketStatusOpen:
		return next == MarketStatusSuspended ||
			next == MarketStatusClosed ||
			next == MarketStatusVoided
	case MarketStatusSuspended:
		return next == MarketStatusOpen ||
			next == MarketStatusClosed ||
			next == MarketStatusVoided
	case MarketStatusClosed:
		return next == MarketStatusSettled ||
			next == MarketStatusVoided
	default: // settled and voided are terminal.
		return false
	}
}

// Market is a question about an event that selections answer and prices quote.
//
// # The line-perspective convention
//
// For a spread, Line is stated FROM THE HOME SIDE'S PERSPECTIVE. A market with
// Line -3.5 means the home competitor gives 3.5 points; the away side of that
// same market is +3.5, obtained by Line.Invert(). This is the convention every
// US book publishes and every provider feed uses, and it is stated here because
// the alternative — storing a line per selection — duplicates one number across
// two rows that can then disagree.
//
// For a total, Line is absolute: over and under share the identical threshold,
// and Invert is never applied. For a player prop, Line is likewise absolute.
//
// Use EffectiveLine to get the line a specific selection trades at; it applies
// this convention so that no call site has to remember it.
type Market struct {
	id        MarketID
	eventID   EventID
	typ       MarketType
	line      Line
	subject   string
	status    MarketStatus
	updatedAt time.Time
}

// MarketParams is the input to NewMarket.
type MarketParams struct {
	ID      MarketID
	EventID EventID
	Type    MarketType

	// Line must satisfy Type.LineRule(). For a spread it is the home side's
	// handicap; see the Market type comment.
	Line Line

	// Subject names the individual a player prop is about ("LeBron James"). It
	// is required for MarketTypePlayerProp and must be empty for every other
	// type, where the subject is implied by the event or by the selection.
	Subject string

	Status MarketStatus

	// UpdatedAt stamps the observation, and is the monotonicity guard for
	// out-of-order bus delivery.
	UpdatedAt time.Time
}

// NewMarket validates its input and returns an immutable Market.
func NewMarket(p MarketParams) (Market, error) {
	if err := validID(string(p.ID)); err != nil {
		return Market{}, idErr("market id", string(p.ID), err)
	}
	if err := validID(string(p.EventID)); err != nil {
		return Market{}, idErr("event id", string(p.EventID), err)
	}
	if !p.Type.Valid() {
		return Market{}, fmt.Errorf("market %s: %w", p.ID, ErrUnknownMarketType)
	}
	if !p.Status.Valid() {
		return Market{}, fmt.Errorf("market %s: %w", p.ID, ErrUnknownMarketStatus)
	}
	if err := p.Type.validateLine(p.Line); err != nil {
		return Market{}, fmt.Errorf("market %s: %w", p.ID, err)
	}

	subject := ""
	if p.Type.NeedsSubject() {
		s, err := validateName("market subject", p.Subject)
		if err != nil {
			return Market{}, fmt.Errorf("market %s: %w", p.ID, ErrSubjectRequired)
		}
		subject = s
	} else if p.Subject != "" {
		return Market{}, fmt.Errorf("market %s is a %s market: %w", p.ID, p.Type, ErrSubjectNotApplicable)
	}

	if p.UpdatedAt.IsZero() {
		return Market{}, fmt.Errorf("market %s updated at: %w", p.ID, ErrZeroTime)
	}

	return Market{
		id:        p.ID,
		eventID:   p.EventID,
		typ:       p.Type,
		line:      p.Line,
		subject:   subject,
		status:    p.Status,
		updatedAt: p.UpdatedAt.UTC(),
	}, nil
}

// ID returns the market's identifier.
func (m Market) ID() MarketID { return m.id }

// EventID returns the identifier of the event this market is about.
func (m Market) EventID() EventID { return m.eventID }

// Type returns the market type.
func (m Market) Type() MarketType { return m.typ }

// Line returns the market's line, which may be absent.
func (m Market) Line() Line { return m.line }

// Subject returns the individual a player prop is about, empty for every other
// market type.
func (m Market) Subject() string { return m.subject }

// Status returns the market's lifecycle status.
func (m Market) Status() MarketStatus { return m.status }

// UpdatedAt returns the observation instant this value carries, in UTC.
func (m Market) UpdatedAt() time.Time { return m.updatedAt }

// AcceptsWagers reports whether the market's own status permits new wagers. A
// bet slip must also check the event — see EventStatus.AcceptsWagers.
func (m Market) AcceptsWagers() bool { return m.status.AcceptsWagers() }

// BelongsTo reports whether the market is about the given event.
func (m Market) BelongsTo(e Event) bool { return !m.eventID.IsZero() && m.eventID == e.ID() }

// WithStatus returns a copy of the market in the next status.
func (m Market) WithStatus(next MarketStatus, at time.Time) (Market, error) {
	if !next.Valid() {
		return Market{}, fmt.Errorf("market %s → %d: %w", m.id, uint8(next), ErrUnknownMarketStatus)
	}
	if !m.status.CanTransitionTo(next) {
		return Market{}, fmt.Errorf("market %s %s → %s: %w", m.id, m.status, next, ErrIllegalTransition)
	}
	stamped, err := m.stamp(at)
	if err != nil {
		return Market{}, err
	}
	stamped.status = next
	return stamped, nil
}

// WithLine returns a copy of the market carrying a new line.
//
// Lines move — that movement is the dataset CLAUDE.md §3 calls "the interesting
// dataset" — so this is a routine operation, not an exceptional one. The new
// line must satisfy the type's rule, so a spread can never become lineless by
// way of an update.
func (m Market) WithLine(l Line, at time.Time) (Market, error) {
	if err := m.typ.validateLine(l); err != nil {
		return Market{}, fmt.Errorf("market %s: %w", m.id, err)
	}
	stamped, err := m.stamp(at)
	if err != nil {
		return Market{}, err
	}
	stamped.line = l
	return stamped, nil
}

// stamp copies the market with a new UpdatedAt, enforcing monotonicity.
func (m Market) stamp(at time.Time) (Market, error) {
	if at.IsZero() {
		return Market{}, fmt.Errorf("market %s update at: %w", m.id, ErrZeroTime)
	}
	u := at.UTC()
	if u.Before(m.updatedAt) {
		return Market{}, fmt.Errorf("market %s: update at %s precedes %s: %w",
			m.id, u.Format(time.RFC3339Nano), m.updatedAt.Format(time.RFC3339Nano), ErrStaleUpdate)
	}
	next := m
	next.updatedAt = u
	return next, nil
}

// IsZero reports whether m is the zero Market, which no constructor produces.
func (m Market) IsZero() bool { return m.id.IsZero() }

// String implements fmt.Stringer.
func (m Market) String() string {
	if m.IsZero() {
		return "market(<zero>)"
	}
	return fmt.Sprintf("market(%s %s line=%s %s)", m.id, m.typ, m.line, m.status)
}
