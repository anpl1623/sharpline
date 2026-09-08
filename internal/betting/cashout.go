// Cash-out pricing: what the book will pay a customer to close a live ticket
// early, and the named haircut it takes for doing so.
//
// CLAUDE.md §6 lists "cash-out pricing on live events" under Betting. This file
// QUOTES one. It deliberately does not EXECUTE one — writing the cash_out
// ledger movement and driving the wager to WagerStatusCashedOut is
// internal/settlement's, because that is a state transition on a ticket and
// every other one of those lives there. A package that could both quote and
// settle a cash-out could do both in one transaction at a price of its own
// choosing, which is the shape an operator fraud takes.
//
// # The formula, and why it is quoted off the FAIR price
//
//	cashOut = round(potentialPayout × Π fairProbability(pending legs) × (1 − margin))
//
// with margin = MarginBps / 10000 and the rounding rule taken from the ticket.
//
// The first factor is what the book owes if the ticket wins. The second is the
// fair — devigged, from the sharp reference book per ADR 0006 — probability
// that it still does. Their product is the position's fair value: what it is
// worth to hold. The third is the book's take, and it is a NAMED CONSTANT.
//
// That last part is the decision. Quoting off the OFFERED price would take the
// same money and hide it inside the vig, entangled with the market's own
// margin, where "what did the book charge me to close early" stops having an
// answer — not a hard one, no answer at all, because the offered price's margin
// and the cash-out's haircut are not separable from the outside. A named
// constant can be reviewed, alerted on, put on a dashboard, and argued about.
// The take is the same either way; only its auditability changes, and CLAUDE.md
// §9 makes auditability the point of the whole ledger.
//
// # A RECORDED DISAGREEMENT WITH THE PHASE BRIEF'S FORMULA
//
// The phase 8 brief specifies, verbatim:
//
//	cashOutValue = round(stake * remainingFairDecimal * (1 - margin))
//
// where remainingFairDecimal is the PRODUCT of the fair decimal prices. That
// multiplies where it should divide, and the error is not small. Take a $10
// straight on a coinflip booked at 2.00 with a fair price of 2.00:
//
//	brief:  10 × 2.00 × 0.95 = $19.00
//	here:   (10 × 2.00) × (1/2.00) × 0.95 = $9.50
//
// The position is worth the stake — it is a fair coinflip at fair odds — so
// $9.50 is the stake less the 5% haircut, and $19.00 is nearly the full winning
// payout handed over while the game is still level. In general the brief's form
// returns the correct value times fair²/booked, so it overpays on every ticket
// whose fair price exceeds 1, which is every ticket.
//
// The two forms agree on the ALGEBRA the brief describes — the payout is
// stake × acceptedDecimal, so payout ÷ Πfair is stake × accepted ÷ Πfair — the
// operator is simply inverted. This is implemented correctly and the
// disagreement is recorded here rather than resolved quietly, in the same
// register migrations/00006 used when it found domain.validateTeaser could not
// see a sign error: the finding belongs beside the code, not in a commit
// message nobody reads again.
//
// # What is refused, and why each refusal is not laziness
//
//	terminal wager       Nothing to close. wager.go: "A settled ticket cannot
//	                     be re-graded, cannot be re-settled at a different
//	                     amount, and cannot be cashed out."
//	a lost leg           The ticket cannot win, so the position is worth
//	                     nothing. Falls out of the arithmetic as a zero and is
//	                     then refused for not being positive.
//	a void or pushed leg REFUSED, and this is the one worth defending. A voided
//	                     leg drops out of a parlay, which REPRICES the ticket —
//	                     the stored potential_payout_minor is no longer the
//	                     promise. Repricing it is settlement's job, under the
//	                     ticket's own Rounding, and quoting a cash-out off a
//	                     payout that is known to be wrong is precisely the
//	                     "plausible number of the right magnitude" failure.
//	a stale fair price   A reference quote nobody has refreshed is not a
//	                     current probability, and CLAUDE.md §9 makes odds
//	                     staleness the headline SLO. Pricing an early close off
//	                     a minute-old line during a scoring drive is the single
//	                     most expensive moment to be wrong.
//	a missing fair price A leg the pricer cannot devig has no probability at
//	                     all. Substituting the offered price would silently swap
//	                     one quantity for another, which internal/pricing
//	                     refuses to do for the same reason (ErrNoReferenceBook:
//	                     "It is a refusal, not a fallback").
//	value ≤ 0            A quote of zero is not an offer.
package betting

import (
	"context"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// DefaultCashOutMarginBps is the book's take on an early close, in basis
// points: 500 = 5%.
//
// A named constant rather than a literal, and a percentage of the fair value
// rather than a spread applied to the price, so that "what does closing early
// cost" has a one-line answer. The magnitude is in the range books actually
// charge (commonly 3–8% on a live single, more on a long parlay) and is stated
// as policy, not derived from anything — a derived haircut would need a model
// of the book's own risk position, which is a trading-desk function this
// project explicitly does not have (CLAUDE.md §14).
const DefaultCashOutMarginBps = 500

// MaxCashOutMarginBps bounds the configurable margin at 50%.
//
// It exists to catch a units error — a margin passed as a fraction (0.05) reads
// as 0 bps and gives the whole fair value away; one passed as a percentage (5)
// reads as 0.05% and does the same. Neither is caught by a lower bound, so the
// upper bound is paired with a positivity check in [NewService].
const MaxCashOutMarginBps = 5000

// DefaultMaxFairPriceAge is how old a reference quote may be and still price a
// cash-out.
//
// Ten seconds is deliberately tighter than the placement path's quote horizon.
// A placement is a customer choosing to take a posted number; a cash-out is the
// book quoting a number back, on a live event, where the line moves on every
// possession. The asymmetry is the point: the side making the offer wears the
// staleness risk.
const DefaultMaxFairPriceAge = 10 * time.Second

// basisPointsPerUnit is 10 000 bps to 1. Named so the conversion below reads as
// a unit change rather than as a magic divisor.
const basisPointsPerUnit = 10000.0

// CashOutLeg is one leg of a ticket being valued.
//
// Fair is required while the leg is pending and ignored once it is graded — a
// decided leg's contribution is its outcome, not a probability.
type CashOutLeg struct {
	SelectionID domain.SelectionID
	Status      domain.LegStatus

	// Fair is the devigged decimal price: 1 / fair probability. Not the offered
	// price. See the file header.
	Fair odds.Decimal

	// ObservedAt is when the underlying quote was seen. Compared against
	// MaxFairPriceAge by the caller, not here — [QuoteCashOut] is pure and
	// reads no clock, so staleness arrives as a decision already made.
	ObservedAt time.Time
}

// CashOutParams is the input to [QuoteCashOut].
type CashOutParams struct {
	// PotentialPayout is domain.Wager.PotentialPayout(): the TOTAL RETURN if
	// every leg wins, stake included. Not the profit — wager.go is blunt that
	// "conflating return with profit produces a plausible number of the right
	// magnitude", and a cash-out quoted off the profit would be short by
	// exactly the stake.
	PotentialPayout domain.Money

	// Rounding is the ticket's own rule, from domain.Wager.Rounding(), NOT a
	// mode chosen here. wager.go: "a later recomputation — a partial void
	// repricing a parlay — uses the same rule the ticket was written under
	// rather than picking a fresh one", and money.go adds that "a silent
	// default is how a house edge appears in a ledger that nobody meant to put
	// one in".
	Rounding domain.Rounding

	Legs []CashOutLeg

	// MarginBps is the book's take in basis points. Must be in
	// [0, MaxCashOutMarginBps]; zero is legal and means the book takes nothing,
	// which is a defensible promotional setting and not an error.
	MarginBps int
}

// CashOutQuote is a priced offer to close a ticket early.
type CashOutQuote struct {
	WagerID domain.WagerID

	// Value is what the book will pay, in minor units. Always strictly
	// positive: a non-positive value is refused as [ErrCashOutUnavailable]
	// rather than returned as an offer of nothing.
	Value domain.Money

	// FairValue is the value BEFORE the haircut, so a caller can show the
	// customer both numbers and the difference between them. This is the whole
	// reason the margin is named rather than buried: the book's take is a
	// subtraction the customer can see.
	FairValue domain.Money

	// MarginBps is the take that was applied, echoed so a quote is
	// self-describing in a log line and in an audit row.
	MarginBps int

	// SurvivalProbability is Π over the legs of each one's fair chance of still
	// winning. It is the number that moves during a game, and exposing it makes
	// a quote explainable — "your leg drifted from 60% to 45%" is an answer,
	// "the number went down" is not.
	SurvivalProbability float64

	// QuotedAt is the instant the quote was made. A quote is a snapshot of a
	// moving market and is not an offer that stands.
	QuotedAt time.Time
}

// QuoteCashOut prices a ticket's early close. It is PURE: no clock, no I/O, no
// package state, so every branch below is reachable from a table test.
//
// See the file header for the formula, for the recorded disagreement with the
// brief's version of it, and for every refusal.
func QuoteCashOut(p CashOutParams) (CashOutQuote, error) {
	if !p.PotentialPayout.IsPositive() {
		return CashOutQuote{}, fmt.Errorf("betting: potential payout %s: %w",
			p.PotentialPayout, ErrCashOutUnavailable)
	}
	if !p.Rounding.Valid() {
		return CashOutQuote{}, fmt.Errorf("betting: cash out: %w", domain.ErrUnknownRounding)
	}
	if len(p.Legs) == 0 {
		return CashOutQuote{}, fmt.Errorf("betting: cash out a ticket with no legs: %w", ErrCashOutUnavailable)
	}
	if p.MarginBps < 0 || p.MarginBps > MaxCashOutMarginBps {
		return CashOutQuote{}, fmt.Errorf("betting: cash out margin %d bps is outside [0, %d]: %w",
			p.MarginBps, MaxCashOutMarginBps, ErrInvalidOptions)
	}

	survival, err := survivalProbability(p.Legs)
	if err != nil {
		return CashOutQuote{}, err
	}

	// Fair value first, then the haircut, as two separate multiplications
	// rather than one combined factor. It costs a rounding step and it buys the
	// property the whole design is for: FairValue and Value are both reportable
	// and their difference IS the take, exactly, rather than to within whatever
	// the combined product happened to round to.
	fair, err := p.PotentialPayout.MulFloat(survival, p.Rounding)
	if err != nil {
		return CashOutQuote{}, fmt.Errorf("betting: cash out fair value: %w", err)
	}
	value, err := fair.MulFloat(1-float64(p.MarginBps)/basisPointsPerUnit, p.Rounding)
	if err != nil {
		return CashOutQuote{}, fmt.Errorf("betting: cash out value: %w", err)
	}
	if !value.IsPositive() {
		return CashOutQuote{}, fmt.Errorf("betting: cash out value is %s: %w", value, ErrCashOutUnavailable)
	}

	// domain.Wager.CashOut() refuses a return above the ticket's headline
	// payout, and wager.go explains why that bound is the one that matters: "a
	// return above the maximum is an arithmetic fault caught here rather than
	// an overpayment discovered in a reconciliation". Catching it at the quote
	// means the customer is never shown a number the ticket cannot legally pay.
	if value.Compare(p.PotentialPayout) > 0 {
		return CashOutQuote{}, fmt.Errorf("betting: cash out value %s exceeds the potential payout %s: %w",
			value, p.PotentialPayout, ErrCashOutUnavailable)
	}

	return CashOutQuote{
		Value:               value,
		FairValue:           fair,
		MarginBps:           p.MarginBps,
		SurvivalProbability: survival,
	}, nil
}

// survivalProbability is Π over the legs of each one's fair chance of still
// winning the ticket.
//
//	pending  1 / fairDecimal — the fair probability the leg wins
//	won      1 — it is already in
//	lost     0 — the ticket is dead, and the product collapses
//	void     refused, and push likewise; see the file header
//
// Multiplied in the caller's order, which is safe here in a way it is not for a
// ticket price: this number is used to VALUE a position rather than to quote
// one, so an ULP of reordering cannot make two customers' identical tickets
// differ. odds.ParlayDecimal sorts for the other case, and the difference in
// treatment is deliberate rather than an oversight.
func survivalProbability(legs []CashOutLeg) (float64, error) {
	survival := 1.0
	for _, leg := range legs {
		switch leg.Status {
		case domain.LegStatusPending:
			if err := leg.Fair.Validate(); err != nil {
				return 0, fmt.Errorf("betting: leg %s fair price %g: %w: %w",
					leg.SelectionID, float64(leg.Fair), err, ErrCashOutUnavailable)
			}
			p, err := leg.Fair.Probability()
			if err != nil {
				return 0, fmt.Errorf("betting: leg %s fair probability: %w: %w",
					leg.SelectionID, err, ErrCashOutUnavailable)
			}
			survival *= float64(p)

		case domain.LegStatusWon:
			// Multiplying by 1 is a no-op and is written as one anyway, rather
			// than folded into the default arm, so that every LegStatus is
			// named and a new one added to the domain fails this switch loudly.

		case domain.LegStatusLost:
			survival = 0

		case domain.LegStatusVoid, domain.LegStatusPush:
			return 0, fmt.Errorf("betting: leg %s is %s, so the ticket needs repricing before it can be valued: %w",
				leg.SelectionID, leg.Status, ErrCashOutUnavailable)

		default:
			return 0, fmt.Errorf("betting: leg %s: %w: %w",
				leg.SelectionID, domain.ErrUnknownLegStatus, ErrCashOutUnavailable)
		}
	}
	if survival <= 0 {
		return 0, fmt.Errorf("betting: the ticket can no longer win: %w", ErrCashOutUnavailable)
	}
	return survival, nil
}

// CashOutQuote prices an early close for one wager, reading the current fair
// prices for its pending legs.
//
// It is a READ. Nothing is written, no transaction is opened, and the quote it
// returns is a snapshot rather than an offer that stands — a caller that acts
// on it must re-quote inside whatever transaction performs the close, exactly
// as placement re-reads a leg's price rather than trusting the one on the slip.
func (s *Service) CashOutQuote(ctx context.Context, id domain.WagerID) (CashOutQuote, error) {
	if s.wagers == nil || s.fairPrices == nil {
		return CashOutQuote{}, fmt.Errorf("betting: cash out is not configured: %w", ErrInvalidOptions)
	}

	now := s.now()

	w, err := s.wagers.WagerByID(ctx, id)
	if err != nil {
		return CashOutQuote{}, fmt.Errorf("betting: read wager %s: %w", id, err)
	}
	if w.IsTerminal() {
		return CashOutQuote{}, fmt.Errorf("betting: wager %s is already %s: %w",
			id, w.Status(), ErrCashOutUnavailable)
	}

	legs := w.Legs()
	pending := make([]domain.SelectionID, 0, len(legs))
	for _, leg := range legs {
		if leg.Status() == domain.LegStatusPending {
			pending = append(pending, leg.SelectionID())
		}
	}

	fair := map[domain.SelectionID]FairPrice{}
	if len(pending) > 0 {
		prices, err := s.fairPrices.FairPricesFor(ctx, pending)
		if err != nil {
			return CashOutQuote{}, fmt.Errorf("betting: read fair prices for wager %s: %w", id, err)
		}
		for _, price := range prices {
			fair[price.SelectionID] = price
		}
	}

	quoteLegs := make([]CashOutLeg, len(legs))
	for i, leg := range legs {
		quoteLegs[i] = CashOutLeg{SelectionID: leg.SelectionID(), Status: leg.Status()}
		if leg.Status() != domain.LegStatusPending {
			continue
		}
		price, ok := fair[leg.SelectionID()]
		if !ok {
			// A missing fair price is a refusal and not a fallback. See the
			// file header, and internal/pricing's ErrNoReferenceBook, which
			// makes the same argument about substituting a consensus for a
			// sharp book: "silently swapping one for the other when the
			// reference is missing makes every downstream EV number
			// unattributable."
			return CashOutQuote{}, fmt.Errorf("betting: no fair price for pending leg %s: %w",
				leg.SelectionID(), ErrCashOutUnavailable)
		}
		if now.Sub(price.ObservedAt) > s.maxFairPriceAge {
			return CashOutQuote{}, fmt.Errorf("betting: fair price for leg %s was observed %s ago, the limit is %s: %w",
				leg.SelectionID(), now.Sub(price.ObservedAt), s.maxFairPriceAge, ErrCashOutUnavailable)
		}
		quoteLegs[i].Fair = price.Decimal
		quoteLegs[i].ObservedAt = price.ObservedAt
	}

	quote, err := QuoteCashOut(CashOutParams{
		PotentialPayout: w.PotentialPayout(),
		Rounding:        w.Rounding(),
		Legs:            quoteLegs,
		MarginBps:       s.cashOutMarginBps,
	})
	if err != nil {
		return CashOutQuote{}, err
	}
	quote.WagerID = w.ID()
	quote.QuotedAt = now
	return quote, nil
}
