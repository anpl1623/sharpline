package synthetic

import (
	"fmt"
	"math"

	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// From a fair probability vector to the prices a book actually shows.
//
// Three transformations, applied in this order, and each one is load-bearing for
// something downstream:
//
//  1. CLAMP. A probability that reaches 0 or 1 has no price — decimal odds are
//     1/p — and one that merely gets close produces a price outside the range
//     domain.NewPrice accepts. Clamping is what makes a blowout in the 89th
//     minute quotable instead of a rejected snapshot.
//
//  2. VIG. The implied probabilities are inflated to sum to 1 + margin, by the
//     book's own method. Without this the feed would be fair, phase 4's
//     devigging would have nothing to remove, and every one of the four methods
//     would agree — which would make the pricing engine's most interesting
//     property untestable.
//
//  3. TICK. The price is converted to American, floored to the book's quoting
//     granularity, and converted back. This is what makes CHANGE DETECTION real:
//     a market only reprices when the latent process moves far enough to cross a
//     tick, so most polls return identical prices and CLAUDE.md §5's "most polls
//     return identical data and must not generate bus traffic" is a measurable
//     claim rather than an assertion.
//
// Flooring rather than rounding is deliberate in both regimes. A more negative
// American favourite and a smaller positive American underdog are both worse for
// the bettor, so flooring moves the price in the book's favour and the emitted
// market is overround by AT LEAST its configured margin, never less.

// Probability clamps.
//
// The two-sided bounds are what stop a late-game blowout from asking for a price
// nobody can quote. maxFairProb is set so that the softest book's multiplicative
// margin still leaves the quote strictly under certainty: 0.90 × 1.065 = 0.958,
// decimal 1.044, American −2310. A bound much higher than this produces prices
// like −33000, which are legal and absurd.
const (
	minFairProb = 0.10
	maxFairProb = 0.90

	// Field markets (three-way moneylines, outrights) are clamped per selection
	// and renormalised, so the bounds can be far wider: a ten-runner futures
	// field has legitimate 3% shots.
	minFieldProb = 0.005
	maxFieldProb = 0.60

	// powerExponentFloor brackets the search for the power-overround exponent.
	// Σ p_i^k rises to the selection count as k falls to zero, so any bracket
	// whose lower end drives the sum above 1 + margin works; 0.05 clears every
	// margin in books() by an order of magnitude.
	powerExponentFloor = 0.05
)

// clampTwoSided returns the two probabilities of a two-sided market, bounded and
// exactly complementary.
//
// The complement is recomputed rather than clamped independently, because two
// independently clamped values do not sum to one and a market whose fair
// probabilities do not sum to one has a margin before the book has charged any.
func clampTwoSided(p float64) [2]float64 {
	p = clamp(p, minFairProb, maxFairProb)
	return [2]float64{p, 1 - p}
}

// clampField bounds every probability of a many-sided market and renormalises.
//
// Renormalising after clamping can push a value back outside the bound, but only
// by the amount that was clamped away, which the bounds make negligible. The
// alternative — an iterative projection — would be exact and would buy nothing:
// what matters downstream is that the vector is positive and sums to one.
func clampField(p []float64) {
	sum := 0.0
	for i := range p {
		p[i] = clamp(p[i], minFieldProb, maxFieldProb)
		sum += p[i]
	}
	if sum <= 0 {
		// Unreachable: every entry is at least minFieldProb. Handled anyway
		// because the alternative is a division by zero that produces NaN
		// prices, and a NaN price fails a whole snapshot.
		for i := range p {
			p[i] = 1 / float64(len(p))
		}
		return
	}
	for i := range p {
		p[i] /= sum
	}
}

// quote turns a fair probability vector into one book's decimal prices.
//
// fair must be positive and sum to one; the caller has already clamped. The
// result is written into a caller-owned buffer so a market's five books do not
// allocate five slices.
func (a *Adapter) quote(fair, out []float64, b bookDef) error {
	if len(fair) < 2 {
		return fmt.Errorf("synthetic: %w: a market needs at least two selections, got %d",
			provider.ErrInvalidSnapshot, len(fair))
	}
	if err := ApplyMargin(fair, out, b.margin, b.power); err != nil {
		return err
	}
	for i, q := range out {
		if q <= 0 || q >= 1 {
			return fmt.Errorf("synthetic: %w: book %s quoted implied probability %v",
				provider.ErrInvalidSnapshot, b.slug, q)
		}
		d, err := tickDecimal(1/q, b.tickAmerican)
		if err != nil {
			return fmt.Errorf("synthetic: book %s: %w", b.slug, err)
		}
		out[i] = d
	}
	return nil
}

// ApplyMargin inflates a fair probability vector to sum to 1 + margin.
//
// Multiplicative spreads the margin proportionally, which loads it evenly across
// the market. Power raises every probability to a common exponent k < 1, which
// loads proportionally MORE of it onto longshots — the two disagree most exactly
// where CLAUDE.md §4 says the four devig methods disagree, "meaningfully on
// longshots". Mixing both across books() is what puts that disagreement into the
// feed rather than leaving it as a property of the devigger's test suite.
//
// The power exponent is found by the same root solver odds.DevigPower inverts
// with, so a power-quoted market devigs back to the exact fair probabilities it
// was generated from. That is the single most useful property this generator has
// for phase 4: the right answer is known.
//
// # Why this is exported
//
// It is the ONE function that defines what "the generator's latent
// probabilities" means, and internal/pricing's known-answer test is worth
// nothing unless it inverts THIS relation rather than a second copy of it that
// happens to agree today. A duplicate in a test harness would keep passing after
// this function changed, which is the exact failure a known-answer test exists
// to catch. So the quoting model is exported and the inverse is asserted across
// the package boundary.
//
// out is caller-owned and must have the same length as fair.
func ApplyMargin(fair, out []float64, margin float64, power bool) error {
	if !power {
		for i, p := range fair {
			out[i] = p * (1 + margin)
		}
		return nil
	}

	target := 1 + margin
	residual := func(k float64) (float64, error) {
		sum := 0.0
		for _, p := range fair {
			sum += math.Pow(p, k)
		}
		return sum - target, nil
	}
	bracket, err := odds.NewRootBracket(residual, powerExponentFloor, 1)
	if err != nil {
		return fmt.Errorf("synthetic: bracketing the power overround exponent for margin %g: %w", margin, err)
	}
	sol, err := odds.Illinois(residual, bracket, odds.DefaultRootConfig())
	if err != nil {
		return fmt.Errorf("synthetic: solving the power overround exponent for margin %g: %w", margin, err)
	}
	for i, p := range fair {
		out[i] = math.Pow(p, sol.Root)
	}
	return nil
}

// tickDecimal snaps a decimal price to the book's American quoting granularity,
// never in the bettor's favour.
//
// The conversion round-trips through internal/domain/odds rather than doing the
// arithmetic here, because that package is the one place this project is allowed
// to convert between odds formats (CLAUDE.md §10 on what wrong odds math costs).
//
// # Why flooring the rounded value is not enough
//
// odds.Decimal.American is nearest-in-American-space, so it can round a price
// UP — a fair −338.4 becomes −338, a longer price than the book intended — and
// flooring that to the tick does not necessarily undo it. The market then comes
// out UNDER its configured margin, which is the one direction that must be
// impossible: a book that gives away margin by an accident of rounding is a book
// the +EV finder will report a permanent edge against, and the edge will be an
// artefact of this function rather than a property of the market.
//
// So the result is checked as a PROPERTY — the quoted decimal must not exceed
// the fair decimal — and stepped one tick worse until it holds. The step count
// is bounded because a single tick always covers the half-unit that rounding to
// nearest can introduce; the loop is a guard, not an algorithm.
func tickDecimal(d float64, tick int64) (float64, error) {
	if tick < 1 {
		return 0, fmt.Errorf("%w: book tick %d must be at least 1", provider.ErrInvalidSnapshot, tick)
	}
	dec, err := odds.NewDecimal(d)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", provider.ErrInvalidSnapshot, err)
	}
	am, err := dec.American()
	if err != nil {
		return 0, fmt.Errorf("%w: %w", provider.ErrInvalidSnapshot, err)
	}

	// Flooring can never leave the American band: 100 divides 1, 5 and 10, so a
	// positive price in [100, 100+tick) floors to exactly +100 and a negative
	// one only grows in magnitude.
	snapped := floorToTick(int64(am), tick)
	for attempt := 0; attempt < 4; attempt++ {
		v, err := odds.NewAmerican(snapped)
		if err != nil {
			return 0, fmt.Errorf("%w: %w", provider.ErrInvalidSnapshot, err)
		}
		back, err := v.Decimal()
		if err != nil {
			return 0, fmt.Errorf("%w: %w", provider.ErrInvalidSnapshot, err)
		}
		if float64(back) <= d {
			return float64(back), nil
		}
		snapped = worsenAmerican(snapped, tick)
	}
	return 0, fmt.Errorf("%w: could not snap decimal %v to a %d-cent tick without improving it",
		provider.ErrInvalidSnapshot, d, tick)
}

// floorToTick rounds v down to a multiple of tick, toward negative infinity.
//
// Toward negative infinity and not toward zero: for a negative American price
// those are opposite directions, and truncating toward zero would SHORTEN the
// favourite's price in the bettor's favour, quietly giving away the margin the
// book just charged.
func floorToTick(v, tick int64) int64 {
	return floorDiv(v, tick) * tick
}

// worsenAmerican returns the next price DOWN the book's quoting ladder.
//
// The ladder is not the integers: American odds have no values between −100 and
// +100, and +100 and −100 are the same price (decimal 2.0). So the rung below
// +100 on a ten-cent ladder is −110, not +90 — which is exactly the ladder a
// real book posts, and exactly the case that breaks a naive `v -= tick`. It
// broke this generator: odds.Decimal.American canonicalises a price a hair under
// even money to +100, and the correction step then asked for +90, which the
// domain refuses.
func worsenAmerican(v, tick int64) int64 {
	if v > 0 {
		if next := v - tick; next >= 100 {
			return next
		}
		return -(100 + tick)
	}
	return v - tick
}
