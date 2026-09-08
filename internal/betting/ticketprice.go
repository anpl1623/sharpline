// The shipped [TicketPricer]: a straight is its leg's price, a parlay of
// independent legs is the product, and the two shapes that cannot be priced
// correctly from leg prices alone are REFUSED rather than approximated.
//
// # Why a refusal is the right failure mode here and almost nowhere else
//
// The number this returns is frozen into wagers.accepted_decimal, and
// migrations/00006 makes that column immutable by trigger the moment the row
// commits. wager.go explains why it is stored rather than derived — "'To win
// $X' is a promise, and a promise recomputed later is not one" — which has a
// consequence for a mispricing: it is not a display bug that a redeploy fixes,
// it is a permanent obligation to a customer, computed once, wrong.
//
// It is also wrong in the direction nobody audits. Pricing correlated legs as
// independent OVERPRICES the ticket, so the customer is happy, the book is
// quietly short, and the only symptom is a house account that drifts against a
// model nobody is comparing it to. wager.go names the same class of error on
// the profit/payout confusion: "conflating return with profit produces a
// plausible number of the right magnitude".
//
// So this pricer says no twice, and the seam it says no through is an interface
// a real pricing engine implements without this package changing.
package betting

import (
	"context"
	"fmt"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// IndependentPricer prices tickets whose legs are on different events.
//
// It is the reference implementation, in the same sense CLAUDE.md §11 uses for
// phase 9's Go analytics: complete, correct on the cases it accepts, and
// explicit about the cases it declines. It holds no state and has no
// dependencies, so the zero value is usable and the composition root does not
// have to construct it.
type IndependentPricer struct{}

// TicketDecimal implements [TicketPricer].
//
//	straight     the single leg's price. domain.validateTicketPrice requires
//	             exact equality, so anything else is refused at construction.
//	parlay       odds.ParlayDecimal — the product, refused for same-game legs.
//	round robin  the same, since one combination of a round robin IS a parlay.
//	teaser       refused; see [ErrTeaserUnsupported].
func (IndependentPricer) TicketDecimal(_ context.Context, t Ticket) (float64, error) {
	if len(t.Legs) == 0 {
		return 0, fmt.Errorf("betting: price a ticket with no legs: %w", ErrSlipEmpty)
	}

	switch t.Kind {
	case domain.WagerKindStraight:
		if len(t.Legs) != 1 {
			return 0, fmt.Errorf("betting: price a straight with %d legs: %w", len(t.Legs), ErrLegCountForKind)
		}
		return t.Legs[0].Price().Decimal(), nil

	case domain.WagerKindTeaser:
		// odds/parlay.go, on TeaserLeg: "This package cannot derive TeasedPrice
		// from OriginalPrice. Doing so requires a model of how the sport's
		// margins are distributed — for football, the mass concentrated on the
		// key numbers 3 and 7 ... That distribution is empirical. Estimating it
		// belongs to the analytics phase, and inventing one here would be
		// fabricated data of exactly the kind the project forbids."
		//
		// A teaser's posted price is a fixed ladder by team count and point
		// count. This repository does not have one, and a plausible-looking
		// ladder written from memory is fabricated data that would be baked
		// permanently into every teaser ticket ever placed.
		return 0, fmt.Errorf("betting: %w", ErrTeaserUnsupported)

	case domain.WagerKindParlay, domain.WagerKindRoundRobin:
		return independentParlayDecimal(t.Legs)

	default:
		return 0, fmt.Errorf("betting: price a ticket: %w", domain.ErrUnknownWagerKind)
	}
}

// independentParlayDecimal is the product of the leg prices, refused when two
// legs share an event.
//
// CLAUDE.md §4 requires "Parlay pricing with correlation adjustment for
// same-game legs", and odds.CorrelatedParlayDecimal implements exactly that —
// given a correlation matrix, which is an empirical input this repository does
// not have either. odds/parlay.go is explicit that ParlayDecimal is "correct
// only for legs in different events", so applying it to a same-game slip would
// not be an approximation, it would be using a function outside its stated
// domain.
//
// The refusal is by EVENT ID rather than by any correlation heuristic, because
// domain.Wager.IsSameGame() draws the line in the same place and the two must
// agree: a ticket this pricer accepted and the domain then reported as
// same-game would be a ticket priced by a rule its own type says does not apply.
func independentParlayDecimal(legs []domain.Leg) (float64, error) {
	if len(legs) < 2 {
		return 0, fmt.Errorf("betting: price a parlay with %d leg(s): %w", len(legs), ErrLegCountForKind)
	}

	events := make(map[domain.EventID]struct{}, len(legs))
	prices := make([]odds.Decimal, len(legs))
	for i, leg := range legs {
		if _, dup := events[leg.EventID()]; dup {
			return 0, fmt.Errorf("betting: legs %s and an earlier one are both on event %s: %w",
				leg.SelectionID(), leg.EventID(), ErrSameGameUnsupported)
		}
		events[leg.EventID()] = struct{}{}

		d, err := odds.NewDecimal(leg.Price().Decimal())
		if err != nil {
			return 0, fmt.Errorf("betting: leg %s price %g: %w", leg.ID(), leg.Price().Decimal(), err)
		}
		prices[i] = d
	}

	// odds.ParlayDecimal multiplies in ascending price order rather than in the
	// caller's, "so that reordering the bet slip cannot move the quote by an
	// ULP". That property is worth having here specifically: two customers who
	// built the same parlay in a different order must be quoted the same number
	// down to the last bit, or the ticket price becomes a function of the click
	// sequence.
	product, err := odds.ParlayDecimal(prices)
	if err != nil {
		return 0, fmt.Errorf("betting: price a %d-leg parlay: %w", len(legs), err)
	}
	return float64(product), nil
}
