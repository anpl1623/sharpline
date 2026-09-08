// The small guards every finding passes before it reaches a sink.
//
// # Why this file exists at all
//
// migrations/00009 puts CHECK constraints on every signal table, and those
// constraints are the executable half of the semantics — an edge at or above
// 100%, a line on a moneyline, a non-finite velocity are all rejected by the
// database. This file applies the SAME rules one layer earlier.
//
// That is not a second implementation of a rule. It is the same rule at a point
// where a violation costs ONE FINDING instead of a whole record: the store
// writes several findings per priced market, a constraint violation aborts the
// transaction they share, and PostgreSQL then refuses every subsequent statement
// in it with 25P02 until it is rolled back. One malformed edge would therefore
// take every other finding on the market down with it and return an error that
// looks transient and that redelivery cannot fix, because the bytes on the topic
// have not changed.
//
// THE TWO SETS OF RULES MUST AGREE. A constraint added to a signals table
// without a line here reintroduces exactly that failure mode; a check here that
// the database does not enforce is a rule nothing guarantees for a writer that is
// not this package. Both directions are worth a moment in review.
package analytics

import (
	"fmt"
	"math"

	"github.com/anpl1623/sharpline/internal/domain"
)

// maxDecimalOdds is the upper bound migrations/00009 puts on every decimal-odds
// column: `> 1.0 AND <= 100000.0`.
//
// It is restated here rather than imported because the constraint lives in SQL
// and there is nothing to import. 100000 decimal is +9999900 American — far
// past any price a real book quotes, and chosen in the migration as the point
// beyond which a value is a parsing accident rather than a longshot.
const maxDecimalOdds = 100000.0

// finite reports whether every value is a real number.
//
// NaN and ±Inf are the two ways a float64 can carry the RESULT of an arithmetic
// failure while still looking like a measurement, and a database DOUBLE
// PRECISION column accepts both. Every numeric CHECK in migrations/00009 is
// written to reject them (a comparison against NaN is false, so `> 0` excludes
// it), and this is the same statement made once instead of per field.
func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// marketTypeKnown reports whether a market type is one of the five this build and
// migrations/00009 both admit.
//
// It exists separately from [lineRule] because steam_signals CARRIES NO LINE
// COLUMN AT ALL: a steam finding is a statement about one selection's probability
// over time, and the market's handicap is a property of the market rather than of
// the move. Applying the line rule to it would refuse every spread and every
// total for want of a line the table does not have — a defect that a
// moneyline-only test would never see.
//
// The type is still checked, because a market type this build does not know is a
// type whose composite foreign key to markets(id, type) would fail at the
// database, and failing here costs one finding where failing there costs the
// transaction its siblings share.
func marketTypeKnown(marketType string) error {
	switch marketType {
	case "moneyline", "futures", "spread", "total", "player_prop":
		return nil
	default:
		return fmt.Errorf("%w: market type %q is not one of the five this build knows",
			ErrInvalidConfig, marketType)
	}
}

// lineRule mirrors the `*_line_rule` CHECK constraints: which market types must
// carry a line, which must not, and which is unconstrained.
//
// The rule is the domain's, not the schema's — [domain.Market] fixes it and
// migrations/00003 encodes it for `markets` — and phase 9's tables restate it
// because a signal carries the line it was found at rather than joining to the
// market for it.
//
//	moneyline, futures   MUST NOT carry a line. There is no handicap to carry.
//	spread               MUST carry one, and it may legitimately be zero (a pick
//	                     'em) or negative (the favourite's side), so the only
//	                     check is presence.
//	total                MUST carry one and it MUST be strictly positive: a total
//	                     is an amount of scoring, and "under −2.5 goals" is not a
//	                     market.
//	player_prop          Unconstrained. Some props are thresholds with a line and
//	                     some are yes/no questions without one, and the schema
//	                     declines to guess which.
//
// An unrecognised market type is REFUSED rather than allowed through. The
// database's CASE has the same `ELSE FALSE`, and for the same reason: a type
// this build does not know is a type whose line convention this build cannot
// assert, and admitting it would write a row nobody can interpret.
func lineRule(marketType string, line domain.Line) error {
	v, present := line.Value()
	switch marketType {
	case "moneyline", "futures":
		if present {
			return fmt.Errorf("%w: market type %q carries line %v; a moneyline and a futures market "+
				"have no handicap", ErrInvalidConfig, marketType, v)
		}
	case "spread":
		if !present {
			return fmt.Errorf("%w: market type %q carries no line", ErrInvalidConfig, marketType)
		}
		if !finite(v) {
			return fmt.Errorf("%w: market type %q carries a non-finite line", ErrInvalidConfig, marketType)
		}
	case "total":
		if !present {
			return fmt.Errorf("%w: market type %q carries no line", ErrInvalidConfig, marketType)
		}
		if !finite(v) || v <= 0 {
			return fmt.Errorf("%w: market type %q carries line %v; a total must be strictly positive",
				ErrInvalidConfig, marketType, v)
		}
	case "player_prop":
		if present && !finite(v) {
			return fmt.Errorf("%w: market type %q carries a non-finite line", ErrInvalidConfig, marketType)
		}
	default:
		return fmt.Errorf("%w: market type %q is not one of the five this build knows",
			ErrInvalidConfig, marketType)
	}
	return nil
}
