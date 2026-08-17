package domain

import "fmt"

// SelectionRole is the kind of answer a selection gives.
//
// CLAUDE.md §4 describes a selection as "a side, an over/under, or a player
// outcome", and the roles here cover exactly that: home/away/draw are sides,
// over/under is the threshold pair, and outright is a named runner.
//
// There is deliberately no yes/no pair. Books quote "will X score a touchdown"
// as over/under 0.5, because that is what makes it price and grade with the
// identical machinery as every other threshold market. Adding yes/no would add
// a second spelling of an existing concept and a second grading path to keep in
// agreement with the first.
type SelectionRole uint8

const (
	// SelectionRoleUnknown is the invalid zero value.
	SelectionRoleUnknown SelectionRole = iota

	// SelectionRoleHome is the home side of a two-sided market.
	SelectionRoleHome

	// SelectionRoleAway is the away side of a two-sided market.
	SelectionRoleAway

	// SelectionRoleDraw is the tie outcome, which exists in soccer moneylines
	// (the "three-way" market) and not in spread or total markets.
	SelectionRoleDraw

	// SelectionRoleOver is the over side of a threshold market.
	SelectionRoleOver

	// SelectionRoleUnder is the under side of a threshold market.
	SelectionRoleUnder

	// SelectionRoleOutright is a named runner in a futures market or a named
	// outcome in a prop.
	SelectionRoleOutright
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (r SelectionRole) String() string {
	switch r {
	case SelectionRoleHome:
		return "home"
	case SelectionRoleAway:
		return "away"
	case SelectionRoleDraw:
		return "draw"
	case SelectionRoleOver:
		return "over"
	case SelectionRoleUnder:
		return "under"
	case SelectionRoleOutright:
		return "outright"
	default:
		return "unknown"
	}
}

// Valid reports whether r is a defined role.
func (r SelectionRole) Valid() bool {
	switch r {
	case SelectionRoleHome, SelectionRoleAway, SelectionRoleDraw,
		SelectionRoleOver, SelectionRoleUnder, SelectionRoleOutright:
		return true
	default:
		return false
	}
}

// ParseSelectionRole is the inverse of String for the defined roles.
func ParseSelectionRole(s string) (SelectionRole, error) {
	switch s {
	case "home":
		return SelectionRoleHome, nil
	case "away":
		return SelectionRoleAway, nil
	case "draw":
		return SelectionRoleDraw, nil
	case "over":
		return SelectionRoleOver, nil
	case "under":
		return SelectionRoleUnder, nil
	case "outright":
		return SelectionRoleOutright, nil
	default:
		return SelectionRoleUnknown, fmt.Errorf("selection role %q: %w", sample(s), ErrUnknownSelectionRole)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (r SelectionRole) MarshalText() ([]byte, error) {
	if !r.Valid() {
		return nil, fmt.Errorf("selection role %d: %w", uint8(r), ErrUnknownSelectionRole)
	}
	return []byte(r.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *SelectionRole) UnmarshalText(b []byte) error {
	parsed, err := ParseSelectionRole(string(b))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// Opposite returns the role that completes a two-sided pair, and whether one
// exists.
//
// This is not a convenience. Devigging operates on a complementary pair whose
// no-vig probabilities must sum to one (CLAUDE.md §4), and arbitrage detection
// compares a price on one side against the best price on its opposite across
// books. Both need "what is the other side of this" to be a single answer
// rather than a per-call-site convention.
//
// Draw and outright have no opposite: a three-way market's complement is the
// other two selections together, not one of them, and a futures field can have
// forty runners.
func (r SelectionRole) Opposite() (SelectionRole, bool) {
	switch r {
	case SelectionRoleHome:
		return SelectionRoleAway, true
	case SelectionRoleAway:
		return SelectionRoleHome, true
	case SelectionRoleOver:
		return SelectionRoleUnder, true
	case SelectionRoleUnder:
		return SelectionRoleOver, true
	default:
		return SelectionRoleUnknown, false
	}
}

// DisplayOrder returns the sort position of the role on a board, lowest first.
//
// It exists so that every surface renders a market's selections in the same
// order without each one inventing its own comparator. Home above away and over
// above under is the ordering every US book uses.
func (r SelectionRole) DisplayOrder() int {
	switch r {
	case SelectionRoleHome:
		return 0
	case SelectionRoleDraw:
		return 1
	case SelectionRoleAway:
		return 2
	case SelectionRoleOver:
		return 3
	case SelectionRoleUnder:
		return 4
	case SelectionRoleOutright:
		return 5
	default:
		return 6
	}
}

// AllowsRole reports whether a role is an answer this market type admits.
//
// The matrix is the compatibility rule between the two enums, and it lives on
// MarketType because that is the type that constrains the other:
//
//	moneyline   → home, away, draw
//	spread      → home, away
//	total       → over, under
//	player prop → over, under, outright
//	futures     → outright
//
// A spread has no draw because the handicap is quoted in half points precisely
// to eliminate the tie; where a book does quote a whole-number spread, a push
// is a settlement outcome, not a selection anyone can back.
func (t MarketType) AllowsRole(r SelectionRole) bool {
	if !t.Valid() || !r.Valid() {
		return false
	}
	switch t {
	case MarketTypeMoneyline:
		return r == SelectionRoleHome || r == SelectionRoleAway || r == SelectionRoleDraw
	case MarketTypeSpread:
		return r == SelectionRoleHome || r == SelectionRoleAway
	case MarketTypeTotal:
		return r == SelectionRoleOver || r == SelectionRoleUnder
	case MarketTypePlayerProp:
		return r == SelectionRoleOver || r == SelectionRoleUnder || r == SelectionRoleOutright
	case MarketTypeFutures:
		return r == SelectionRoleOutright
	default:
		return false
	}
}

// Selection is one answer to a market's question.
type Selection struct {
	id       SelectionID
	marketID MarketID
	role     SelectionRole
	name     string
}

// SelectionParams is the input to NewSelection.
type SelectionParams struct {
	ID       SelectionID
	MarketID MarketID
	Role     SelectionRole

	// Name is the display label: "Boston Celtics", "Over", "Nikola Jokić". It
	// is the provider's wording, kept rather than derived, because for an
	// outright it is the only thing that identifies the runner.
	Name string
}

// NewSelection validates its input and returns an immutable Selection.
//
// It cannot check the role against the market type, because a Selection does
// not carry its Market — only the parent identifier. That cross-check is
// ValidateSelectionForMarket, which the caller runs at the point where both
// values are in hand.
func NewSelection(p SelectionParams) (Selection, error) {
	if err := validID(string(p.ID)); err != nil {
		return Selection{}, idErr("selection id", string(p.ID), err)
	}
	if err := validID(string(p.MarketID)); err != nil {
		return Selection{}, idErr("market id", string(p.MarketID), err)
	}
	if !p.Role.Valid() {
		return Selection{}, fmt.Errorf("selection %s: %w", p.ID, ErrUnknownSelectionRole)
	}
	name, err := validateName("selection name", p.Name)
	if err != nil {
		return Selection{}, fmt.Errorf("selection %s: %w", p.ID, err)
	}
	return Selection{id: p.ID, marketID: p.MarketID, role: p.Role, name: name}, nil
}

// ID returns the selection's identifier.
func (s Selection) ID() SelectionID { return s.id }

// MarketID returns the identifier of the market this selection answers.
func (s Selection) MarketID() MarketID { return s.marketID }

// Role returns the kind of answer this selection gives.
func (s Selection) Role() SelectionRole { return s.role }

// Name returns the selection's display label.
func (s Selection) Name() string { return s.name }

// BelongsTo reports whether the selection answers the given market.
func (s Selection) BelongsTo(m Market) bool { return !s.marketID.IsZero() && s.marketID == m.ID() }

// IsZero reports whether s is the zero Selection, which no constructor
// produces.
func (s Selection) IsZero() bool { return s.id.IsZero() }

// String implements fmt.Stringer.
func (s Selection) String() string {
	if s.IsZero() {
		return "selection(<zero>)"
	}
	return fmt.Sprintf("selection(%s %s %q)", s.id, s.role, s.name)
}

// ValidateSelectionForMarket checks a selection against the market it claims to
// answer: that it names that market as its parent, and that its role is an
// answer the market's type admits.
func ValidateSelectionForMarket(m Market, s Selection) error {
	if !s.BelongsTo(m) {
		return fmt.Errorf("selection %s names market %s, not %s: %w",
			s.id, s.marketID, m.ID(), ErrMismatchedParent)
	}
	if !m.Type().AllowsRole(s.Role()) {
		return fmt.Errorf("selection %s is %s on a %s market: %w",
			s.id, s.role, m.Type(), ErrRoleNotApplicable)
	}
	return nil
}

// EffectiveLine returns the line the given selection actually trades at.
//
// It applies the home-perspective convention documented on Market: for a
// spread, the home selection trades at the market's line and the away selection
// at its inverse, so a market at -3.5 gives the away side +3.5. For totals and
// player props the threshold is absolute and both sides share it. For
// moneylines and futures there is no line and the result is absent.
//
// Every place that needs a selection's line should call this rather than
// reaching for Market.Line() directly — reading the raw market line and
// forgetting to invert it for the away side is the exact bug the convention
// exists to prevent, and it produces a plausible-looking wrong number rather
// than an error.
func EffectiveLine(m Market, s Selection) (Line, error) {
	if err := ValidateSelectionForMarket(m, s); err != nil {
		return Line{}, err
	}
	if m.Type() == MarketTypeSpread && s.Role() == SelectionRoleAway {
		return m.Line().Invert(), nil
	}
	return m.Line(), nil
}
