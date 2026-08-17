package odds

import (
	"errors"
	"fmt"
	"math"
)

// -----------------------------------------------------------------------------
// Three numbers that are routinely confused
// -----------------------------------------------------------------------------
//
// The package documentation defers the definition of a market's margin to this
// file, because the words are used interchangeably in the wild and are not
// interchangeable in arithmetic. This is that definition, and it is the one the
// rest of the repository must use.
//
// Let a market quote n prices d_1..d_n. Each carries an implied probability
// q_i = 1/d_i, which includes the book's margin. Write their sum as
//
//	S = Σ q_i
//
// Then three distinct numbers are all commonly called "the vig":
//
//	booking percentage   100·S          104.76  for a -110/-110 pair
//	overround            S - 1          0.0476  for the same market
//	vig / hold / margin  (S-1)/S        0.0455  for the same market
//
// They are three different values with three different meanings:
//
//   - The booking percentage is the number the trading desk publishes. It is the
//     only quantity here expressed as a percentage rather than a fraction, because
//     that is how it is universally written; "a 105% book" is a sentence people say.
//   - The overround is how far the quoted probabilities overshoot certainty. It is
//     the excess probability the book has sold, and it is what the per-selection
//     attribution below apportions.
//   - The vig is the book's expected profit as a fraction of *turnover*, assuming
//     stakes are apportioned so that every outcome pays the same. Staking S units
//     in total returns exactly 1 unit whichever way the market lands, so the profit
//     is S-1 on a turnover of S. It is always strictly smaller in magnitude than
//     the overround for an overround market, because it divides by S > 1.
//
// The 4.76 / 4.55 gap on a standard point-spread market is small enough to look
// like a rounding difference and large enough to be a real mispricing when it
// compounds across a slate, which is exactly why it must not be left to a comment.
//
// A market with S < 1 is *underround*: the book has sold less than certainty and a
// proportional stake across every selection is a guaranteed profit — an arbitrage
// inside one book's own market. It is rare, it is real, and it is deliberately not
// an error here. Overround and Vig simply go negative.
//
// Units, stated once: BookingPercentage is a percentage (104.76). Overround and
// Vig are fractions (0.0476, 0.0455), never percentages. Nothing in this file
// returns a mixture.

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

var (
	// ErrTooFewSelections reports a market with fewer than MinMarketSelections
	// prices. Every quantity in this file is a property of a *market* — a set of
	// mutually exclusive, collectively exhaustive outcomes — and a single price is
	// not a market. Its implied probability is a fact about that price alone, and
	// calling the excess over 1 an overround would be meaningless.
	ErrTooFewSelections = errors.New("a market needs at least two selections")

	// ErrImpliedSumNotPositive reports an implied probability sum that is zero or
	// negative. It cannot arise from a set of valid prices, every one of which has
	// a strictly positive implied probability; it exists because MarginFromSum and
	// MarginFromProbabilities accept a sum that was computed elsewhere, and
	// dividing by it is how the vig is defined.
	ErrImpliedSumNotPositive = errors.New("the sum of implied probabilities must be strictly positive")

	// ErrUnknownAttribution reports an unrecognised vig attribution convention.
	ErrUnknownAttribution = errors.New("unknown vig attribution convention")

	// ErrAttributionUndefined reports an attribution that leaves a selection with a
	// fair probability of zero or less, which is not a probability and cannot be
	// priced. It is a genuine limitation of the uniform convention rather than a
	// defect: subtracting an equal slice of the overround from every selection
	// subtracts more than a sufficiently long shot has, which happens on real
	// outright markets that pair a short favourite with a 999/1 tail.
	ErrAttributionUndefined = errors.New("attribution leaves a selection with a non-positive fair probability")
)

// -----------------------------------------------------------------------------
// Bounds
// -----------------------------------------------------------------------------

const (
	// MinMarketSelections is the smallest number of prices that constitutes a
	// market. Two: a moneyline, a spread, a total. Three-way soccer, player props
	// with a long tail, and outright futures are all larger.
	MinMarketSelections = 2

	// FairMarketTolerance is the half-width of the band around S = 1 inside which a
	// market is classified as fair rather than over- or underround.
	//
	// A tolerance is required, not optional: exact equality against 1.0 is wrong
	// here as a matter of arithmetic, not style. Wikipedia's own worked "100% book"
	// — evens, 2-1 and 5-1 — has implied probabilities 1/2 + 1/3 + 1/6, and summing
	// those three float64 values left to right yields 1 - 2^-53, not 1. A market
	// that is fair by construction would be reported as underround.
	//
	// The magnitude matches the convention the rest of this domain uses. One ULP at
	// magnitude 1 is 2^-52 ≈ 2.2e-16, so 1e-12 is roughly 4,500 ULPs — ample room
	// for the accumulated rounding of a few hundred divisions and additions. It is
	// also four orders of magnitude below the tightest margin any real book runs:
	// a sharp two-way at 2% hold sits at S - 1 ≈ 0.02, and even a hypothetical
	// 0.01% hold sits at 1e-4, a hundred million times this tolerance. The band
	// cannot swallow a margin anyone would trade on.
	FairMarketTolerance = 1e-12
)

// -----------------------------------------------------------------------------
// Margin
// -----------------------------------------------------------------------------

// Margin is one market's pricing margin, carrying all three of the confusable
// numbers so that a caller never has to derive one from another and never has to
// guess which convention a bare float64 was in.
//
// The zero value is not a valid margin; obtain one from NewMargin,
// MarginFromProbabilities, or MarginFromSum. The fields are exported for the same
// reason Fractional's are — this is a value type, not an entity — and callers are
// expected to read them, not to assemble one by hand.
type Margin struct {
	// Selections is n, the number of prices the market quotes.
	Selections int

	// ImpliedSum is S = Σ 1/d_i. Strictly positive. Greater than 1 on an overround
	// market, less than 1 on an underround one.
	ImpliedSum float64

	// BookingPercentage is 100·S, the "105% book" the trading desk publishes. It is
	// the only field expressed as a percentage.
	BookingPercentage float64

	// Overround is S - 1, expressed as a fraction. Negative on an underround
	// market. This is the total excess probability the attribution apportions.
	Overround float64

	// Vig is (S-1)/S, expressed as a fraction: the book's profit as a share of
	// turnover on a balanced book. Also called the hold, the margin, or the juice.
	// Negative on an underround market, where the "book" loses to a proportional
	// stake.
	Vig float64
}

// NewMargin computes the margin of a market from its quoted decimal prices.
//
// Every price is validated; an invalid one fails with the index of the offending
// selection. Prices are converted with the package's single definition of implied
// probability, Decimal.Probability, so this function cannot drift from it.
func NewMargin(prices []Decimal) (Margin, error) {
	qs, err := probabilitiesFromPrices(prices)
	if err != nil {
		return Margin{}, err
	}
	return newMarginFrom(len(qs), neumaierSum(qs))
}

// MarginFromProbabilities computes the margin of a market from probabilities that
// were derived elsewhere.
//
// Its main use is checking work: a devigged market should come back with an
// ImpliedSum of 1 and a Vig of 0 to within FairMarketTolerance, and if it does not,
// the devig is wrong. Note that 0 and 1 are valid Probability values and are
// accepted, so a settled market is summarisable.
func MarginFromProbabilities(ps []Probability) (Margin, error) {
	qs, err := validatedProbabilities(ps)
	if err != nil {
		return Margin{}, err
	}
	return newMarginFrom(len(qs), neumaierSum(qs))
}

// MarginFromSum computes the margin of a market whose implied probability sum is
// already known, for callers that computed S on another pass and should not pay to
// recompute it or risk computing it differently.
//
// selections is carried for reporting and must still be at least
// MinMarketSelections; sum must be finite and strictly positive.
func MarginFromSum(selections int, sum float64) (Margin, error) {
	return newMarginFrom(selections, sum)
}

// newMarginFrom is the single place the three quantities are computed, so the
// standalone functions below and every constructor above cannot disagree.
//
// Two deliberate choices about the arithmetic:
//
// Overround is computed as S-1 rather than by rescaling the booking percentage.
// For S in [0.5, 2] — every market anyone quotes — the subtraction is exact by
// Sterbenz's lemma, so the overround carries no rounding error of its own at all.
//
// Vig is computed as (S-1)/S rather than the algebraically identical 1 - 1/S. The
// two forms are not numerically identical. In the second, 1/S is correct to within
// half an ULP, so it carries an absolute error of about 1.1e-16; subtracting it
// from 1 keeps that absolute error but the result is small, so the *relative* error
// blows up as the margin shrinks — on a 1% hold it is about 1e-14, on a 0.01% hold
// about 1e-12. The first form instead computes an exact numerator (see above) and
// rounds once, in the division: a relative error of half an ULP regardless of how
// thin the margin is. Thin margins are precisely where a sharp book lives, so the
// better-conditioned form is the one implemented.
func newMarginFrom(selections int, sum float64) (Margin, error) {
	if selections < MinMarketSelections {
		return Margin{}, fmt.Errorf("odds: market has %d selection(s), need at least %d: %w",
			selections, MinMarketSelections, ErrTooFewSelections)
	}
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return Margin{}, fmt.Errorf("odds: implied probability sum %v: %w", sum, ErrNotFinite)
	}
	if sum <= 0 {
		return Margin{}, fmt.Errorf("odds: implied probability sum %g: %w", sum, ErrImpliedSumNotPositive)
	}

	overround := sum - 1
	return Margin{
		Selections:        selections,
		ImpliedSum:        sum,
		BookingPercentage: 100 * sum,
		Overround:         overround,
		Vig:               overround / sum,
	}, nil
}

// IsOverround reports whether the market sells more than certainty, S > 1. This is
// the normal case: it is how a book makes money.
func (m Margin) IsOverround() bool { return m.ImpliedSum-1 > FairMarketTolerance }

// IsUnderround reports whether the market sells less than certainty, S < 1.
//
// This is a genuine arbitrage inside a single book's own market: staking in
// proportion to each selection's implied probability returns more than it costs,
// whichever way the market lands. It is rare — it comes from odds boosts, stale
// legs of a market that moved, and outright pricing mistakes — but it is real, and
// finding it is a feature (CLAUDE.md §6), not an error to be swallowed.
func (m Margin) IsUnderround() bool { return 1-m.ImpliedSum > FairMarketTolerance }

// IsFair reports whether the market's implied probabilities sum to 1 within
// FairMarketTolerance, so there is no margin in either direction. A devigged market
// is fair by construction.
//
// Exactly one of IsFair, IsOverround, and IsUnderround is true for any Margin
// produced by a constructor in this file.
func (m Margin) IsFair() bool { return math.Abs(m.ImpliedSum-1) <= FairMarketTolerance }

// String renders the margin with every number labelled by which of the three it is,
// because an unlabelled 0.0476 next to an unlabelled 0.0455 is the whole problem.
func (m Margin) String() string {
	return fmt.Sprintf("%d selections: S=%.6f, booking %.4f%%, overround %+.6f, vig %+.6f",
		m.Selections, m.ImpliedSum, m.BookingPercentage, m.Overround, m.Vig)
}

// -----------------------------------------------------------------------------
// The three numbers, individually
// -----------------------------------------------------------------------------

// ImpliedSum returns S = Σ 1/d_i, the sum of the implied probabilities of every
// price the market quotes.
//
// This is the quantity every other number in this file is derived from, and the one
// the devigging methods start from. It is not itself a margin: on a fair market it
// is 1, not 0.
func ImpliedSum(prices []Decimal) (float64, error) {
	qs, err := probabilitiesFromPrices(prices)
	if err != nil {
		return 0, err
	}
	return neumaierSum(qs), nil
}

// BookingPercentage returns 100·S, the book percentage as the trading desk writes
// it: 104.76 for a standard -110/-110 pair, 120 for Wikipedia's worked football
// book.
//
// It is a percentage. A fair market returns 100, not 0 and not 1. Subtracting 100
// gives the overround in percentage points, which is 100× the fraction Overround
// returns — a factor-of-100 discrepancy that is the single most common way these
// numbers get mixed up.
func BookingPercentage(prices []Decimal) (float64, error) {
	m, err := NewMargin(prices)
	if err != nil {
		return 0, err
	}
	return m.BookingPercentage, nil
}

// Overround returns S - 1 as a fraction: 0.047619… for a standard -110/-110 pair.
//
// It is the excess probability the book has sold beyond certainty. It is not the
// book's profit margin — dividing it by S gives that, and Vig is that function.
// A negative result means the market is underround; see Margin.IsUnderround.
func Overround(prices []Decimal) (float64, error) {
	m, err := NewMargin(prices)
	if err != nil {
		return 0, err
	}
	return m.Overround, nil
}

// Vig returns (S-1)/S as a fraction: 0.045454… for a standard -110/-110 pair. It is
// also called the hold, the margin, the juice, or the vigorish.
//
// It is the book's expected profit as a share of turnover when the book is
// balanced. Stake q_i·k on selection i for any k and the total outlay is k·S while
// the return is k whichever way the market lands, so the profit is k(S-1) on a
// turnover of kS.
//
// This is strictly smaller than Overround for an overround market, because it
// divides by S > 1. Reporting the overround as the hold overstates a -110/-110
// market's margin by 4.8% of its own value, and the error grows with the margin:
// on Wikipedia's 120% book the overround is 0.20 and the hold is 0.1667, an
// overstatement of a fifth.
//
// A negative result is an underround market, an arbitrage rather than a margin, and
// is not an error.
func Vig(prices []Decimal) (float64, error) {
	m, err := NewMargin(prices)
	if err != nil {
		return 0, err
	}
	return m.Vig, nil
}

// -----------------------------------------------------------------------------
// Per-selection attribution
// -----------------------------------------------------------------------------

// Attribution names the convention by which a market's overround is apportioned
// across its selections.
//
// It has to be named explicitly because the answer to "how much of the hold does
// this side carry" is not determined by the quoted prices alone. Two conventions
// are in common use, they agree exactly on a symmetric market, and they disagree
// substantially on a lopsided one — which is where the money is. Requiring the
// caller to name one prevents an unstated assumption from propagating into an EV
// calculation.
//
// The zero value is AttributionUnknown and is invalid, so an unset Attribution
// fails rather than silently defaulting.
type Attribution uint8

const (
	// AttributionUnknown is the invalid zero value.
	AttributionUnknown Attribution = iota

	// AttributionProportional apportions the overround in proportion to each
	// selection's quoted implied probability: fair_i = q_i / S.
	//
	// Every price is shaded by the same multiplicative factor S, so every selection
	// carries the same *relative* margin — exactly the overround, S-1 — and the
	// asymmetry lives entirely in the absolute excess, which is larger on the
	// favourite because the favourite has more probability to shade.
	//
	// This is the convention Wikipedia's worked example uses: its 120% book of
	// 60/40/20 is precisely 1.2× its stated fair book of 50/33⅓/16⅔.
	AttributionProportional

	// AttributionUniform apportions the overround equally in absolute terms:
	// fair_i = q_i - (S-1)/n.
	//
	// Every selection carries the same absolute slice of the excess, so the
	// *relative* margin grows in magnitude as the price lengthens — on a
	// -2000/+1000 pair the underdog carries 13.4 times the relative margin of the
	// favourite. That is the direction the favourite–longshot bias actually runs in
	// observed markets, which is why this convention exists alongside the
	// proportional one. (In magnitude, because on an underround market the slice is
	// negative and the signed quantity runs the other way.)
	//
	// It is not total: subtracting an equal slice from a sufficiently long shot
	// drives its fair probability to zero or below, which returns
	// ErrAttributionUndefined rather than a negative probability.
	AttributionUniform
)

// String returns the canonical lowercase name of the convention.
func (a Attribution) String() string {
	switch a {
	case AttributionProportional:
		return "proportional"
	case AttributionUniform:
		return "uniform"
	case AttributionUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Valid reports whether a names a real attribution convention.
func (a Attribution) Valid() bool {
	switch a {
	case AttributionProportional, AttributionUniform:
		return true
	case AttributionUnknown:
		return false
	default:
		return false
	}
}

// SelectionVig is one selection's share of its market's margin, under a stated
// Attribution.
//
// The five numbers answer five different questions, and conflating them is the
// per-selection version of conflating overround with hold.
type SelectionVig struct {
	// Implied is q_i = 1/d_i, the probability the quoted price implies. It includes
	// the margin and is what the book is charging.
	Implied float64

	// Fair is the probability left after this selection's share of the margin is
	// removed, under the chosen attribution. Strictly inside (0, 1). Across the
	// market these sum to 1.
	Fair float64

	// Excess is Implied - Fair: the absolute probability margin this selection
	// carries. Across the market these sum to the market's Overround. Negative on
	// an underround market, where the book is giving margin away.
	Excess float64

	// Share is this selection's fraction of the total overround, Excess/(S-1).
	// Across the market these sum to 1, and each is positive on both overround and
	// underround markets, since numerator and denominator share a sign.
	//
	// It is 0 for every selection on a fair market. There is no overround to
	// apportion there, so the ratio is 0/0; reporting a share of zero is the honest
	// answer and, unlike NaN, it does not poison whatever it is multiplied into.
	// Check Margin.IsFair before reading a Share as a proportion.
	Share float64

	// RelativeMargin is Excess/Fair: the margin as a fraction of the fair
	// probability, which is the form that is comparable across selections of wildly
	// different lengths. Under AttributionProportional it is the same value for
	// every selection — exactly the market's overround. Under AttributionUniform it
	// grows in magnitude as the price lengthens.
	RelativeMargin float64
}

// VigContributions apportions a market's overround across its selections under the
// stated convention, returning one SelectionVig per input price, in the same order.
//
// The input slice is not modified and the result is freshly allocated.
//
// # Which convention to pass
//
// AttributionProportional is the safe default and the one that always succeeds. It
// is the multiplicative devig, expressed per selection.
//
// AttributionUniform is the additive devig, and is the one that models the
// favourite–longshot bias — books do not shade a 999/1 outsider by the same
// multiple they shade a -2000 favourite. It can fail; see ErrAttributionUndefined.
//
// The two are worth comparing rather than choosing between. On Wikipedia's 120%
// football book the outsider's fair probability is 16.67% proportionally and 13.33%
// uniformly — a quarter apart, and enough to flip the sign of an EV calculation on
// that selection. A +EV finder that reports an edge without naming which convention
// produced it is reporting an opinion, not a measurement.
func VigContributions(prices []Decimal, a Attribution) ([]SelectionVig, error) {
	if !a.Valid() {
		return nil, fmt.Errorf("odds: attribution %d: %w", uint8(a), ErrUnknownAttribution)
	}
	qs, err := probabilitiesFromPrices(prices)
	if err != nil {
		return nil, err
	}
	// This error is unreachable from here and is checked anyway. probabilitiesFromPrices
	// has already established that there are at least MinMarketSelections prices and
	// that every implied probability is finite and strictly positive, so the sum is
	// finite (it is bounded above by the number of selections) and strictly positive
	// — the two things newMarginFrom rejects. The branch is the reason this function
	// does not reach 100% statement coverage; the alternative is to inline the three
	// formulas here, which would give this file two definitions of the vig, and one
	// uncovered defensive return is a much smaller problem than that.
	m, err := newMarginFrom(len(qs), neumaierSum(qs))
	if err != nil {
		return nil, err
	}

	// The equal slice each selection surrenders under the uniform convention. It is
	// computed once, outside the loop, so that every selection surrenders bitwise
	// the same amount and the excesses provably sum to the overround.
	uniformSlice := m.Overround / float64(m.Selections)

	// Whether there is an overround to apportion at all. Evaluated once so that
	// every Share in the result agrees about it.
	fair := m.IsFair()

	out := make([]SelectionVig, len(qs))
	for i, q := range qs {
		// Attribution.Valid and this switch enumerate the same set. A convention
		// added to one and not the other leaves fairProb at zero and is caught by
		// the guard immediately below, so the omission cannot reach a caller as a
		// silently wrong number.
		var fairProb float64
		switch a {
		case AttributionProportional:
			fairProb = q / m.ImpliedSum
		case AttributionUniform:
			fairProb = q - uniformSlice
		}
		if math.IsNaN(fairProb) || fairProb <= 0 {
			return nil, fmt.Errorf(
				"odds: %s attribution of overround %g across %d selections leaves selection %d (implied %g) with fair probability %g: %w",
				a, m.Overround, m.Selections, i, q, fairProb, ErrAttributionUndefined)
		}

		excess := q - fairProb
		share := 0.0
		if !fair {
			share = excess / m.Overround
		}
		out[i] = SelectionVig{
			Implied:        q,
			Fair:           fairProb,
			Excess:         excess,
			Share:          share,
			RelativeMargin: excess / fairProb,
		}
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Internals
// -----------------------------------------------------------------------------

// probabilitiesFromPrices validates a market's prices and returns their implied
// probabilities in input order.
func probabilitiesFromPrices(prices []Decimal) ([]float64, error) {
	if len(prices) < MinMarketSelections {
		return nil, fmt.Errorf("odds: market has %d selection(s), need at least %d: %w",
			len(prices), MinMarketSelections, ErrTooFewSelections)
	}
	qs := make([]float64, len(prices))
	for i, d := range prices {
		// Decimal.Probability rather than an inline 1/d: the implied probability of
		// a price is defined in exactly one place in this package, and a second
		// definition here would be free to drift from it.
		p, err := d.Probability()
		if err != nil {
			return nil, wrapSelectionValue(i, float64(d), err)
		}
		qs[i] = float64(p)
	}
	return qs, nil
}

// validatedProbabilities validates a market's probabilities and returns them as
// plain float64 in input order.
func validatedProbabilities(ps []Probability) ([]float64, error) {
	if len(ps) < MinMarketSelections {
		return nil, fmt.Errorf("odds: market has %d selection(s), need at least %d: %w",
			len(ps), MinMarketSelections, ErrTooFewSelections)
	}
	qs := make([]float64, len(ps))
	for i, p := range ps {
		if err := p.Validate(); err != nil {
			return nil, wrapSelectionValue(i, float64(p), err)
		}
		qs[i] = float64(p)
	}
	return qs, nil
}

// wrapSelectionValue restates a validation failure with the index of the offending
// selection.
//
// The sentinel is unwrapped and re-wrapped rather than nested, because the package
// contract is that the "odds:" prefix appears exactly once in a message and
// Validate's own message already carries it. errors.Is is unaffected either way.
// If the incoming error does not wrap anything — which no validator in this package
// does today — it is wrapped as-is rather than discarded, since a doubled prefix is
// a cosmetic problem and a lost cause is not.
func wrapSelectionValue(i int, value float64, err error) error {
	return fmt.Errorf("odds: market selection %d has value %v: %w", i, value, unprefixed(err))
}

// neumaierSum returns the sum of xs using Neumaier's variant of compensated
// summation.
//
// Naive left-to-right summation of n terms carries a worst-case relative error of
// about n·2^-53. Compensated summation tracks the low-order bits each addition
// discards and adds them back once at the end, which bounds the error at about
// 2·2^-53 independently of n. Neumaier's variant is used rather than plain Kahan
// because it stays correct when a later term is larger in magnitude than the
// running sum — which happens the moment a market lists a heavy favourite after a
// string of longshots, an ordering no caller should have to think about.
//
// The accuracy is not theatre. Wikipedia's worked "100% book" of evens, 2-1 and 5-1
// has implied probabilities 1/2, 1/3 and 1/6; summed naively in float64 those come
// to 1 - 2^-53, not 1. Compensated, they come to 1 exactly. Neither result is
// *wrong* at the tolerances this package works to, but the compensated one does not
// require the reader of a fair-market test to know about the last bit.
func neumaierSum(xs []float64) float64 {
	var sum, comp float64
	for _, x := range xs {
		t := sum + x
		if math.Abs(sum) >= math.Abs(x) {
			// sum is the larger magnitude, so its low bits are the ones lost.
			comp += (sum - t) + x
		} else {
			comp += (x - t) + sum
		}
		sum = t
	}
	return sum + comp
}
