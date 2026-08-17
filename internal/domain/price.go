package domain

import (
	"fmt"
	"math"
	"time"
)

// Bounds on decimal odds.
const (
	// MinDecimalOdds is an EXCLUSIVE lower bound.
	//
	// Decimal odds are the total return per unit staked, so 1.0 means the stake
	// comes back and nothing else — a market nobody can win money on — and its
	// implied probability is exactly 1.0, which divides by zero in the no-vig
	// and Kelly formulas downstream. Anything at or below 1.0 is a data error,
	// not a long price.
	MinDecimalOdds = 1.0

	// MaxDecimalOdds is an INCLUSIVE upper bound, and it is a sanity guard
	// rather than a rule of the domain.
	//
	// The longest prices real books publish are around +100000, or decimal
	// 1001. A hundred times that leaves every legitimate futures price
	// comfortably inside the range while still catching the failure that
	// actually happens: an adapter reading an American price as a decimal one,
	// or a cents field as an odds field.
	MaxDecimalOdds = 100000.0
)

// Price is the odds a book quoted for a selection at an instant.
//
// CLAUDE.md §4: "Immutable; a new price is a new row. This is the hypertable."
// Nothing on this type mutates and nothing sets a field after construction; a
// line move produces a second Price, and the sequence of them is the line
// history that CLV, steam detection, and the movement charts are all computed
// from.
//
// # Identity
//
// There is no PriceID. A price is identified by (SelectionID, BookID,
// ObservedAt), which is exactly the TimescaleDB hypertable's natural key. A
// surrogate key would add a uniqueness constraint to maintain and a column to
// index without answering any question the natural key does not.
//
// # Why decimal odds
//
// The quote is stored as decimal odds in a float64. Decimal is the only format
// that is total over the useful range: American odds are undefined between -100
// and +100 and have a sign discontinuity at even money, and fractional odds
// need a rational rather than a float. Conversion to the other formats, and all
// of devigging, EV, and Kelly, live in internal/domain/odds — none of that math
// is repeated here, because two implementations of one formula eventually
// disagree and CLAUDE.md §10 is explicit about what wrong odds math costs.
//
// # Why the line is on the price
//
// A price carries the line it was quoted at, not just a pointer to a selection
// whose market's line moves. Without it the time series is unusable for its
// headline purpose: "-3.5 at 1.91" followed by "-4 at 1.95" is a line move,
// while the same two prices with the line stripped look like a pure odds move,
// and CLV computed against a closing price at a different line is not CLV.
type Price struct {
	selectionID SelectionID
	bookID      BookID
	decimal     float64
	line        Line
	observedAt  time.Time
}

// PriceParams is the input to NewPrice.
type PriceParams struct {
	SelectionID SelectionID
	BookID      BookID

	// Decimal is the total return per unit staked, strictly greater than
	// MinDecimalOdds and at most MaxDecimalOdds.
	Decimal float64

	// Line is the handicap or threshold the quote was made at, from the
	// selection's own perspective — the value EffectiveLine returns, already
	// inverted for an away spread. It is absent for moneylines and futures.
	Line Line

	// ObservedAt is when the quote was seen. It is normalised to UTC and is the
	// hypertable's time dimension, so it must be an observation instant and
	// never an insertion instant.
	ObservedAt time.Time
}

// NewPrice validates its input and returns an immutable Price.
func NewPrice(p PriceParams) (Price, error) {
	if err := validID(string(p.SelectionID)); err != nil {
		return Price{}, idErr("selection id", string(p.SelectionID), err)
	}
	if err := validID(string(p.BookID)); err != nil {
		return Price{}, idErr("book id", string(p.BookID), err)
	}
	if math.IsNaN(p.Decimal) || math.IsInf(p.Decimal, 0) {
		return Price{}, fmt.Errorf("price for selection %s at book %s: %w",
			p.SelectionID, p.BookID, ErrOddsNotFinite)
	}
	if p.Decimal <= MinDecimalOdds || p.Decimal > MaxDecimalOdds {
		return Price{}, fmt.Errorf("price %v for selection %s at book %s: %w",
			p.Decimal, p.SelectionID, p.BookID, ErrOddsOutOfRange)
	}
	if p.ObservedAt.IsZero() {
		return Price{}, fmt.Errorf("price for selection %s at book %s observed at: %w",
			p.SelectionID, p.BookID, ErrZeroTime)
	}
	return Price{
		selectionID: p.SelectionID,
		bookID:      p.BookID,
		decimal:     p.Decimal,
		line:        p.Line,
		observedAt:  p.ObservedAt.UTC(),
	}, nil
}

// SelectionID returns the selection this price quotes.
func (p Price) SelectionID() SelectionID { return p.selectionID }

// BookID returns the book that quoted this price.
func (p Price) BookID() BookID { return p.bookID }

// Decimal returns the decimal odds: total return per unit staked.
func (p Price) Decimal() float64 { return p.decimal }

// Line returns the line the quote was made at, which may be absent.
func (p Price) Line() Line { return p.line }

// ObservedAt returns the instant the quote was seen, in UTC.
func (p Price) ObservedAt() time.Time { return p.observedAt }

// Age returns how long ago the quote was observed relative to now.
//
// It takes the instant as a parameter — this package reads no clock. The result
// may be negative, which means the observation is stamped in the future; that
// is clock skew between the ingester and the caller, and returning it rather
// than clamping to zero is what lets a monitor detect the skew instead of
// silently reporting healthy staleness.
//
// Odds staleness is the project's headline SLO (CLAUDE.md §9), so this is the
// function the metric is built on.
func (p Price) Age(now time.Time) time.Duration { return now.Sub(p.observedAt) }

// IsStale reports whether the quote is older than ttl at the instant now. A
// non-positive ttl makes every quote with a positive age stale.
func (p Price) IsStale(now time.Time, ttl time.Duration) bool {
	return p.Age(now) > ttl
}

// SameQuoteAs reports whether two prices carry the identical quote, ignoring
// when each was observed.
//
// This is the change-detection primitive CLAUDE.md §5 requires: "most polls
// return identical data and must not generate bus traffic". Comparing the
// float64 for exact equality is correct here and not a float-comparison
// mistake — the question is "did the provider send a different number", which
// is a question about bytes, not about numeric closeness. A tolerance would
// suppress genuine one-tick moves, which are exactly the moves steam detection
// is looking for.
func (p Price) SameQuoteAs(o Price) bool {
	return p.selectionID == o.selectionID &&
		p.bookID == o.bookID &&
		p.decimal == o.decimal &&
		p.line == o.line
}

// Equal reports full equality, observation instant included.
func (p Price) Equal(o Price) bool {
	return p.SameQuoteAs(o) && p.observedAt.Equal(o.observedAt)
}

// IsNewerThan reports whether p was observed strictly after o. Prices for one
// selection and book form a time series, and this is its ordering.
func (p Price) IsNewerThan(o Price) bool { return p.observedAt.After(o.observedAt) }

// PayoutFor returns the TOTAL RETURN on a winning stake: stake × decimal odds,
// which includes the stake itself. A 100.00 stake at 2.50 pays out 250.00.
//
// This and ProfitFor are separated and named this bluntly because conflating
// return with profit is the most common arithmetic error in this domain, and it
// is invisible — both answers are plausible numbers of the right magnitude.
//
// The rounding mode is required rather than defaulted: see Money.MulFloat.
func (p Price) PayoutFor(stake Money, r Rounding) (Money, error) {
	payout, err := stake.MulFloat(p.decimal, r)
	if err != nil {
		return 0, fmt.Errorf("payout for %s at %v: %w", stake, p.decimal, err)
	}
	return payout, nil
}

// ProfitFor returns the NET WINNINGS on a winning stake: payout minus stake.
// A 100.00 stake at 2.50 profits 150.00.
func (p Price) ProfitFor(stake Money, r Rounding) (Money, error) {
	payout, err := p.PayoutFor(stake, r)
	if err != nil {
		return 0, err
	}
	profit, err := payout.Sub(stake)
	if err != nil {
		return 0, fmt.Errorf("profit for %s at %v: %w", stake, p.decimal, err)
	}
	return profit, nil
}

// IsZero reports whether p is the zero Price, which no constructor produces.
func (p Price) IsZero() bool { return p.selectionID.IsZero() }

// String implements fmt.Stringer.
func (p Price) String() string {
	if p.IsZero() {
		return "price(<zero>)"
	}
	return fmt.Sprintf("price(%s@%s %g line=%s %s)",
		p.selectionID, p.bookID, p.decimal, p.line, p.observedAt.Format(time.RFC3339Nano))
}

// ValidatePriceForSelection checks a price against the market and selection it
// claims to quote: that the selection answers the market, that the price quotes
// that selection, and that the price was taken at the line the selection is
// currently trading at.
//
// The last check is the one worth having. A price whose line has drifted from
// its market's is not merely stale, it is *wrong* — settling a wager against it
// grades a bet at a handicap the customer never took — and the mismatch is
// otherwise invisible because both values are individually valid.
func ValidatePriceForSelection(m Market, s Selection, p Price) error {
	effective, err := EffectiveLine(m, s)
	if err != nil {
		return err
	}
	if p.SelectionID() != s.ID() {
		return fmt.Errorf("price quotes selection %s, not %s: %w",
			p.SelectionID(), s.ID(), ErrMismatchedParent)
	}
	if !p.Line().Equal(effective) {
		return fmt.Errorf("price at line %s but selection %s trades at %s: %w",
			p.Line(), s.ID(), effective, ErrLineMismatch)
	}
	return nil
}
