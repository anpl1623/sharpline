package odds

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// Property-based tests for the odds package, driven by pgregory.net/rapid.
//
// CLAUDE.md §4 names this library and three invariants verbatim — "probabilities
// sum to 1 after devig, round-trip conversions are lossless, Kelly is zero at zero
// edge" — and §10 asks for effectively 100% coverage here because "wrong odds math
// is the one bug class that destroys the project's credibility".
//
// # How this file differs from the property tests already scattered across the
// # package
//
// Several files already carry rapid checks over their own function's inputs. This
// file is deliberately not a duplicate of them. Its job is the invariants that span
// functions, and — the load-bearing difference — its generators cover the REALISTIC
// domain rather than a convenient slice of it:
//
//   - American prices from a -1,000,000 house favourite through a +1,000,000
//     futures shot, sampled so the dense -300…+300 core of a real board and the
//     extremes both get hit;
//   - markets from a two-way spread to a 30-runner futures field, with weights
//     spanning three orders of magnitude so one generated market can hold a -2000
//     favourite beside a +50000 outsider. devig_test.go's own generateMarket caps at
//     10 selections and 20:1 weight spread, which never reaches that shape;
//   - bankrolls across nine orders of magnitude, from a 100-minor-unit stake to the
//     2^53-1 ceiling internal/domain.Money imposes.
//
// # On the word "lossless"
//
// The charter's phrasing is loose and asserting it literally would be asserting
// something false. American and Fractional are LOSSY DISPLAY FORMATS — the package
// documentation says so, and it follows from American being an integer: decimal
// 2.0049 and 2.0051 are two distinct prices and both convert to +100 and +101
// respectively, but the decimal recovered from either is not the decimal that went
// in. What is actually true, and what this file asserts, is:
//
//	American → Decimal → American   is a TOTAL IDENTITY (exact, every legal input)
//	Fractional → Decimal → Fractional  is EXACT on the ladder (denominator ≤ 1000)
//	Decimal → American → Decimal    has a PROVEN ERROR BOUND of 0.005 in decimal,
//	                                and lands on the NEAREST legal American price
//	Decimal → Probability → Decimal is exact to a few ULPs
//
// Asserting a false "lossless everywhere" would either fail or, worse, be quietly
// weakened until it passed.
//
// # Naming
//
// Every test here is prefixed TestRapid and every helper prop, because this file
// shares a package with roughly 300 existing test functions and a collision is a
// compile error rather than a warning. closeTo, equicorrelated and sumOf are
// deliberately REUSED from convert_test.go and correlation_test.go rather than
// re-declared: two spellings of "are these close" is exactly the duplication this
// package's documentation warns about.

// -----------------------------------------------------------------------------
// Tolerances
// -----------------------------------------------------------------------------

const (
	// propRelTol is the relative tolerance for a quantity that reached its value
	// through a short chain of correctly-rounded double operations.
	//
	// One ULP of a float64 is ~2.2e-16 relative (2^-53), so 1e-12 is roughly 4,500
	// ULPs. It is the convention the rest of this domain's tests use — see the block
	// comment on floatTolerance in internal/domain/doc_test.go and relTolChain in
	// convert_test.go — and it is adopted here rather than re-derived so that a
	// reader comparing two files does not have to reconcile two epsilons.
	//
	// It cannot absorb a real mistake. The smallest difference this domain can
	// express is one cent of decimal odds, 0.01 near evens, i.e. ~5e-3 relative:
	// nine orders of magnitude above this tolerance.
	propRelTol = 1e-12

	// propSumTol bounds |Σq - 1| after devigging. It is ABSOLUTE, not relative,
	// which is the same thing here because the quantity is 1 by construction.
	//
	// Every method finishes by dividing through by its own float64 sum, so the
	// residual left is the rounding of that sum plus n divisions: at most about n
	// ULPs, i.e. 30 · 1.1e-16 ≈ 3.3e-15 for the largest market this file generates.
	// 1e-12 is 300× that headroom and still ten orders below the smallest
	// probability difference the domain cares about.
	propSumTol = 1e-12

	// propDecimalAmericanBound is the PROVEN worst-case absolute error of
	// Decimal → American → Decimal, in decimal odds.
	//
	// Rounding a real-valued American price to the nearest integer moves it by at
	// most 0.5. On the dog branch A = (d-1)·100, so Δd = 0.5/100 = 0.005. On the
	// favourite branch d = 1 + 100/|A| with |A| ≥ 100, so Δd = 100·0.5/(|A|·|A'|) ≤
	// 100·0.5/100² = 0.005. The bound is therefore 0.005 on both sides, and it is
	// tight — a price sitting exactly half a tick from the grid attains it.
	//
	// propBoundSlack is added only to absorb float noise in the comparison itself.
	// At 1e-9 it is six orders of magnitude below the bound, so it cannot mask a
	// rounding bug: a conversion that rounded the wrong way would miss by a whole
	// tick, 0.01, which is 10,000,000× the slack.
	propDecimalAmericanBound = 0.005
	propBoundSlack           = 1e-9

	// propOrthantSlack bounds a comparison between two lattice-quadrature orthant
	// probabilities at three or more legs.
	//
	// MultivariateNormalCDF is Genz's shifted-lattice rule, not a closed form. The
	// package's own TestLatticeAccuracyProfile measures its error against an
	// independent seeded simulation at up to ~4e-4 in absolute probability, and
	// parlay.go's frechetSlack comment cites the same figure. A monotonicity
	// comparison between two such evaluations can therefore be violated by up to
	// twice that from quadrature noise alone.
	//
	// 1e-3 is that measured envelope rounded up. It is deliberately NOT tighter:
	// a tighter bound would make this test flaky rather than correct. It is also
	// deliberately not looser — a genuine monotonicity break in a Gaussian copula
	// (Slepian's inequality failing) moves the joint by whole percentage points,
	// which is ten to a hundred times this slack.
	//
	// At two legs the copula is BivariateNormalCDF, a closed form accurate to about
	// 1e-15, so the two-leg case is held to propRelTol instead. Both are asserted;
	// see TestRapidCopulaIsMonotoneInCorrelation.
	propOrthantSlack = 1e-3

	// propOrderInversionSlack bounds how far a devigged market may deviate from
	// preserving the order of its inputs.
	//
	// # Why this is not zero, and why it is not a knob that was turned until the
	// # test went green
	//
	// Three of the four methods preserve order BITWISE, for a reason that needs no
	// tolerance: IEEE-754 rounding is monotone, and multiplicative (divide by S),
	// additive (subtract a constant) and power (a monotone Pow followed by a divide)
	// are compositions of monotone unary operations. They can tie, never invert.
	//
	// Shin is different, and deliberately so. devig.go evaluates the stable form
	//
	//	q_i = 2·p_i² / ( S·(u_i + z) )
	//
	// rather than the published (u_i - z)/(2(1-z)), because the published form
	// cancels catastrophically as z → 1. That rearrangement is exact in real
	// arithmetic but it puts p on BOTH sides of a quotient: the numerator grows as p²
	// and the denominator grows through u_i. A quotient of two independently-rounded
	// increasing quantities is not a monotone function of p in float64, so two
	// selections whose implied probabilities differ by a couple of ULPs can come back
	// inverted by about one ULP.
	//
	// This was found, not assumed. At 50,000 checks rapid produced a 19-runner market
	// containing p = 0.021693594896296152 and p = 0.021693594896296145 — two ULPs
	// apart — whose Shin fair probabilities came back 0.020481129806726079 and
	// 0.020481129806726082, inverted by 3e-18, a relative 1.5e-16.
	//
	// The bound is therefore derived from the arithmetic rather than fitted to that
	// observation. Each q passes through roughly six correctly-rounded operations
	// (a square, two multiplies, an add, a sqrt, a divide, then the renormalising
	// divide), so its absolute error is at most ~6·2^-53·q; two of them can disagree
	// by twice that. 2e-15 is about nine ULPs, comfortably above the ~1.3e-15 the
	// chain can produce and eleven orders of magnitude below any inversion a wrong
	// formula would cause — a transposed Shin coefficient moves a probability in the
	// third decimal place, not the sixteenth.
	//
	// The slack does NOT weaken the property where it matters, because the strict
	// form is asserted separately for every pair whose inputs are meaningfully
	// distinct; see propMeaningfulGap.
	propOrderInversionSlack = 2e-15

	// propMeaningfulGap is the relative separation above which two implied
	// probabilities are different prices rather than the same price twice.
	//
	// It is the domain's own float comparison tolerance, 1e-12, and it is enormous
	// compared to anything a real board can produce: the finest American tick near an
	// implied probability of 0.02 is +4900 against +4901, which separates them by a
	// relative 2e-4. Two quotes closer than 1e-12 cannot come from a price feed; they
	// can only come from a generator. Above this gap, order preservation is asserted
	// STRICTLY, with no tolerance at all.
	propMeaningfulGap = 1e-12

	// propMaxSelections is the largest generated market. CLAUDE.md's feature surface
	// includes futures, and a 30-runner outright field — a golf major cut down to the
	// live board, a Formula 1 grid, an NFL division winner market — is the realistic
	// upper end. Nothing here needs the 1024 the devig input validator permits.
	propMaxSelections = 30
)

// -----------------------------------------------------------------------------
// Generators
// -----------------------------------------------------------------------------

// propAmerican draws a legal American price across the whole realistic board.
//
// The four magnitude bands are sampled with equal weight rather than one flat range,
// because a flat draw over [100, 1_000_000] would put 99.9% of its mass past +1000
// and essentially never generate the -300…+300 prices that make up almost every line
// on a real screen. The bands overlap on purpose: the core is reachable from all
// four, so it is oversampled, which is where the interesting rounding lives.
func propAmerican() *rapid.Generator[American] {
	return rapid.Custom(func(t *rapid.T) American {
		var magnitude int64
		switch rapid.IntRange(0, 3).Draw(t, "americanBand") {
		case 0:
			magnitude = rapid.Int64Range(100, 300).Draw(t, "americanCore")
		case 1:
			magnitude = rapid.Int64Range(100, 10_000).Draw(t, "americanBoard")
		case 2:
			magnitude = rapid.Int64Range(100, 50_000).Draw(t, "americanFutures")
		default:
			magnitude = rapid.Int64Range(100, MaxAmericanMagnitude).Draw(t, "americanExtreme")
		}
		if rapid.Bool().Draw(t, "americanFavourite") {
			magnitude = -magnitude
		}
		a, err := NewAmerican(magnitude)
		if err != nil {
			t.Fatalf("NewAmerican(%d) rejected a value inside its own bounds: %v", magnitude, err)
		}
		return a
	})
}

// propDecimal draws a legal decimal price.
//
// Half the draws come off the American grid, because that is the shape of every
// price this system will ingest from a US book, and half from the continuum, because
// a devigged fair price or a parlay product is not on any grid. The continuum range
// runs from 1.0000001 — a house favourite so short it has no American form, which
// exercises the ErrAmericanOutOfRange path — up to 1e6.
func propDecimal() *rapid.Generator[Decimal] {
	return rapid.Custom(func(t *rapid.T) Decimal {
		if rapid.Bool().Draw(t, "decimalOnTheAmericanGrid") {
			a := propAmerican().Draw(t, "decimalFromAmerican")
			d, err := a.Decimal()
			if err != nil {
				t.Fatalf("american %d has no decimal form: %v", int64(a), err)
			}
			return d
		}
		v := rapid.Float64Range(1.0000001, 1e6).Draw(t, "decimalContinuum")
		d, err := NewDecimal(v)
		if err != nil {
			t.Fatalf("NewDecimal(%v): %v", v, err)
		}
		return d
	})
}

// propPriceableProbability draws a probability strictly inside (0, 1), i.e. one that
// has a finite decimal price in both directions.
//
// The bounds are 1e-6 and 1-1e-6: a millionth is a +99,999,900 shot and a
// -99,999,900 lock, both already an order of magnitude past anything MaxDecimalOdds
// admits, so nothing realistic is excluded and no draw is unpriceable.
func propPriceableProbability() *rapid.Generator[Probability] {
	return rapid.Custom(func(t *rapid.T) Probability {
		v := rapid.Float64Range(1e-6, 1-1e-6).Draw(t, "probability")
		p, err := NewProbability(v)
		if err != nil {
			t.Fatalf("NewProbability(%v): %v", v, err)
		}
		return p
	})
}

// propMarket is one generated market: the raw implied probabilities a book's prices
// carry, together with the fair distribution and margin they were built from.
type propMarket struct {
	implied []Probability
	sum     float64
	margin  float64
}

// propGenerateMarket draws a market that is always constructible: at least two
// selections, every implied probability strictly inside (0, 1), and an implied sum
// strictly greater than 1.
//
// # Why the construction is arithmetic rather than rejection sampling
//
// The naive generator — draw probabilities, hope they sum past 1, retry otherwise —
// wastes draws and, worse, shrinks badly: rapid's shrinker cannot make progress
// through a filter that rejects most of its neighbourhood, so a counterexample comes
// back barely reduced. So the margin is drawn from the headroom the fair
// distribution leaves rather than being drawn and checked.
//
// Concretely: let f be a fair distribution (Σf = 1) and p_i = f_i·(1+m). Then Σp =
// 1+m > 1 for any m > 0, and max(p) < 1 exactly when m < 1/max(f) - 1. Drawing m from
// [floor, 0.9·headroom] therefore cannot produce an invalid market, for any f.
//
// The 0.9 factor keeps max(p) at most 0.9 + 0.1·max(f) < 1, which matters for a
// second reason: DevigPower's bracket needs -ln(max p) to be comfortably above zero,
// and a max(p) within a few ULPs of 1 would legitimately return ErrRootNoBracket.
// Generating that case would be generating a market no book posts.
//
// The exponential weight draw spans three orders of magnitude (0.02 to 20), so one
// market can carry a 1000:1 spread between its shortest and longest price — a
// -2000 favourite beside a +50000 outsider on the same futures board.
func propGenerateMarket() *rapid.Generator[propMarket] {
	return rapid.Custom(func(t *rapid.T) propMarket {
		n := rapid.IntRange(MinMarketSelections, propMaxSelections).Draw(t, "selections")

		weights := make([]float64, n)
		total := 0.0
		for i := range weights {
			// Drawn in log space so the distribution over magnitudes is uniform;
			// drawing linearly over [0.02, 20] would put almost all mass at the top
			// and never generate a genuine longshot.
			lg := rapid.Float64Range(math.Log(0.02), math.Log(20)).Draw(t, fmt.Sprintf("weight%d", i))
			weights[i] = math.Exp(lg)
			total += weights[i]
		}

		fair := make([]float64, n)
		maxFair := 0.0
		for i, w := range weights {
			fair[i] = w / total
			maxFair = math.Max(maxFair, fair[i])
		}

		headroom := 1/maxFair - 1
		ceiling := math.Min(0.35, 0.9*headroom)
		floor := math.Min(0.002, 0.5*ceiling)
		margin := rapid.Float64Range(floor, ceiling).Draw(t, "margin")

		implied := make([]Probability, n)
		raw := make([]float64, n)
		for i, f := range fair {
			raw[i] = f * (1 + margin)
			implied[i] = Probability(raw[i])
		}
		return propMarket{implied: implied, sum: sumOf(raw), margin: margin}
	})
}

// propDecimals converts a market's implied probabilities to the prices that carry
// them.
func propDecimals(t *rapid.T, implied []Probability) []Decimal {
	out := make([]Decimal, len(implied))
	for i, p := range implied {
		d, err := p.Decimal()
		if err != nil {
			t.Fatalf("selection %d implied %v has no price: %v", i, float64(p), err)
		}
		out[i] = d
	}
	return out
}

// propClose fails the check unless got and want agree to within relTol, reusing
// convert_test.go's closeTo so this file cannot drift from the rest of the package's
// idea of "close".
func propClose(t *rapid.T, what string, got, want, relTol float64) {
	t.Helper()
	if !closeTo(got, want, relTol) {
		t.Fatalf("%s = %.17g, want %.17g (|diff| = %.3g, relative tolerance %.3g)",
			what, got, want, math.Abs(got-want), relTol)
	}
}

// -----------------------------------------------------------------------------
// Devigging: the charter's first named invariant
// -----------------------------------------------------------------------------

// TestRapidDevigProducesADistribution asserts, for all four methods over markets
// from two-way to 30-runner, the three properties the phase brief names:
//
//	Σq = 1 to within propSumTol
//	every q strictly inside (0, 1)
//	input ordering preserved
//
// plus the two the result type promises: Overround is the raw sum S, and Vig is the
// hold 1-1/S rather than the overround S-1. Those two are asserted here because
// conflating them is documented in devig.go as the classic trap, and a property test
// over arbitrary markets is where a conflation shows up as a systematic 5% error.
//
// # Which failures are legitimate
//
// Not every method can price every market, and refusing is the correct behaviour:
//
//	multiplicative  never fails on a valid market — it is a division by S
//	additive        ErrDevigAdditiveNonPositive once (S-1)/n exceeds the shortest
//	                price in the market, which is routine on a long futures board
//	power           no documented failure on a market this generator can produce
//	shin            ErrDevigNoShinSolution on a degenerate all-insider fit
//
// Anything else is reported as a failure naming the method and the error, so a new
// failure mode surfaces rather than being absorbed.
func TestRapidDevigProducesADistribution(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		market := propGenerateMarket().Draw(t, "market")
		n := len(market.implied)

		// A copy taken before the call, compared after it: every devigging function
		// documents that it does not modify its input, and a property test over
		// arbitrary markets is the cheapest place to hold it to that.
		before := make([]Probability, n)
		copy(before, market.implied)

		for _, method := range DevigMethods() {
			res, err := Devig(method, market.implied)
			if err != nil {
				switch {
				case method == MethodAdditive && errors.Is(err, ErrDevigAdditiveNonPositive):
				case method == MethodShin && errors.Is(err, ErrDevigNoShinSolution):
				default:
					t.Fatalf("%s on a %d-selection market with implied sum %.17g: %v",
						method, n, market.sum, err)
				}
				continue
			}

			if got := len(res.Probabilities); got != n {
				t.Fatalf("%s returned %d probabilities for a %d-selection market", method, got, n)
			}
			if res.Method != method {
				t.Fatalf("Devig(%s) reported method %s", method, res.Method)
			}

			// Invariant 1: the fair probabilities sum to 1.
			fair := make([]float64, n)
			for i, q := range res.Probabilities {
				fair[i] = float64(q)
			}
			if diff := math.Abs(sumOf(fair) - 1); diff > propSumTol {
				t.Fatalf("%s over %d selections: Σq - 1 = %.3g, beyond %.3g (implied sum %.17g)",
					method, n, sumOf(fair)-1, propSumTol, market.sum)
			}

			// Invariant 2: every fair probability is a real probability, strictly
			// inside the open interval. Zero would price at infinity and one would
			// price at a zero payout; both are documented as impossible outputs.
			for i, q := range fair {
				if !(q > 0 && q < 1) {
					t.Fatalf("%s selection %d: fair probability %.17g is not strictly inside (0, 1)",
						method, i, q)
				}
			}

			// Invariant 3: ordering is preserved. Every one of the four methods is a
			// strictly increasing map of p in real arithmetic, so a shorter price can
			// never come back longer than a longer one.
			//
			// Two assertions, because two different things are true:
			//
			//   - for inputs that are meaningfully distinct — a relative gap above
			//     propMeaningfulGap, which every pair of real quotes clears by eight
			//     orders of magnitude — the order must hold STRICTLY, no tolerance;
			//   - for inputs closer than that, which no price feed can produce, an
			//     inversion is permitted only if it is itself within the rounding
			//     noise of the computation. See propOrderInversionSlack for why that
			//     case exists at all and why it is Shin-specific.
			for i := range fair {
				for j := range fair {
					pi, pj := float64(market.implied[i]), float64(market.implied[j])
					if pi <= pj {
						continue
					}
					if (pi-pj)/pi > propMeaningfulGap {
						if !(fair[i] > fair[j]) {
							t.Fatalf("%s reversed the order of two distinct prices: p[%d] = %.17g > p[%d] = %.17g (relative gap %.3g) but q[%d] = %.17g is not above q[%d] = %.17g",
								method, i, pi, j, pj, (pi-pj)/pi, i, fair[i], j, fair[j])
						}
						continue
					}
					if inversion := fair[j] - fair[i]; inversion > propOrderInversionSlack*math.Max(fair[i], fair[j]) {
						t.Fatalf("%s reversed the order: p[%d] = %.17g > p[%d] = %.17g but q[%d] = %.17g < q[%d] = %.17g, an inversion of %.3g (relative %.3g), beyond the %.3g rounding noise of the computation",
							method, i, pi, j, pj, i, fair[i], j, fair[j],
							inversion, inversion/math.Max(fair[i], fair[j]), propOrderInversionSlack)
					}
				}
			}

			// The two margin numbers, which are constantly confused.
			propClose(t, method.String()+" Overround", res.Overround, market.sum, propRelTol)
			hold, err := res.Vig()
			if err != nil {
				t.Fatalf("%s: Vig on a successfully devigged market: %v", method, err)
			}
			propClose(t, method.String()+" Vig", hold, 1-1/market.sum, propRelTol)
			if market.sum > 1 && !(hold < res.Overround-1+propRelTol) {
				t.Fatalf("%s: hold %.17g is not below overround %.17g on an overround book",
					method, hold, res.Overround-1)
			}
		}

		for i := range before {
			if before[i] != market.implied[i] {
				t.Fatalf("devigging mutated its input at selection %d: %v became %v",
					i, float64(before[i]), float64(market.implied[i]))
			}
		}
	})
}

// TestRapidMultiplicativePreservesRatiosExactly asserts the defining property of the
// multiplicative method over arbitrary markets: dividing every selection by the same
// S leaves every ratio between selections untouched, q_i/q_j = p_i/p_j.
//
// This is the property that makes multiplicative the baseline the other three are
// judged against, and the property that makes it wrong on longshots — it cannot
// model the favourite-longshot bias, because modelling it means changing exactly
// these ratios.
func TestRapidMultiplicativePreservesRatiosExactly(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		market := propGenerateMarket().Draw(t, "market")

		res, err := DevigMultiplicative(market.implied)
		if err != nil {
			t.Fatalf("multiplicative devigging failed on a valid market: %v", err)
		}
		for i := range res.Probabilities {
			for j := range res.Probabilities {
				pRatio := float64(market.implied[i]) / float64(market.implied[j])
				qRatio := float64(res.Probabilities[i]) / float64(res.Probabilities[j])
				propClose(t, fmt.Sprintf("q[%d]/q[%d]", i, j), qRatio, pRatio, propRelTol)
			}
		}
	})
}

// TestRapidDevigIsIdempotentOnItsOwnOutput asserts that devigging an already-fair
// market returns it unchanged. A fair market has no margin to remove, so a second
// pass must be the identity; a method that shaved a little more off on every pass
// would be a method that drifts every time a fair price is re-derived.
func TestRapidDevigIsIdempotentOnItsOwnOutput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		market := propGenerateMarket().Draw(t, "market")

		for _, method := range DevigMethods() {
			once, err := Devig(method, market.implied)
			if err != nil {
				continue // Legitimate refusal; TestRapidDevigProducesADistribution classifies these.
			}
			twice, err := Devig(method, once.Probabilities)
			if err != nil {
				t.Fatalf("%s refused its own fair output: %v", method, err)
			}
			for i := range once.Probabilities {
				propClose(t, fmt.Sprintf("%s q[%d] after a second pass", method, i),
					float64(twice.Probabilities[i]), float64(once.Probabilities[i]), propRelTol)
			}
		}
	})
}

// -----------------------------------------------------------------------------
// The vig algebra
// -----------------------------------------------------------------------------

// TestRapidMarginAlgebraHolds asserts the relationships between the three
// constantly-confused margin numbers for any valid market:
//
//	BookingPercentage = 100·S
//	Overround         = S - 1
//	Vig               = (S-1)/S = 1 - 1/S
//
// and that the standalone functions agree bit for bit with the Margin fields, since
// both routes are meant to be one implementation. It also asserts the three
// classifications are mutually exclusive and exhaustive, which is what lets a caller
// branch on them without an else-case.
func TestRapidMarginAlgebraHolds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		market := propGenerateMarket().Draw(t, "market")
		prices := propDecimals(t, market.implied)

		m, err := NewMargin(prices)
		if err != nil {
			t.Fatalf("NewMargin over %d valid prices: %v", len(prices), err)
		}

		if m.Selections != len(prices) {
			t.Fatalf("Margin.Selections = %d, want %d", m.Selections, len(prices))
		}
		propClose(t, "BookingPercentage", m.BookingPercentage, 100*m.ImpliedSum, propRelTol)
		propClose(t, "Overround", m.Overround, m.ImpliedSum-1, propRelTol)
		propClose(t, "Vig", m.Vig, m.Overround/m.ImpliedSum, propRelTol)
		propClose(t, "Vig as 1-1/S", m.Vig, 1-1/m.ImpliedSum, propRelTol)

		// The hold is strictly smaller than the overround on an overround book.
		// Reporting one under the other's name overstates a -110/-110 market's
		// margin by a relative 5%, which devig.go calls out by name.
		if m.IsOverround() && !(m.Vig < m.Overround) {
			t.Fatalf("hold %.17g is not below overround %.17g at S = %.17g",
				m.Vig, m.Overround, m.ImpliedSum)
		}

		// The standalone functions are meant to be the same computation, so they are
		// compared with == rather than a tolerance: a difference of even one ULP
		// would mean two implementations exist.
		sum, err := ImpliedSum(prices)
		if err != nil {
			t.Fatalf("ImpliedSum: %v", err)
		}
		booking, err := BookingPercentage(prices)
		if err != nil {
			t.Fatalf("BookingPercentage: %v", err)
		}
		over, err := Overround(prices)
		if err != nil {
			t.Fatalf("Overround: %v", err)
		}
		hold, err := Vig(prices)
		if err != nil {
			t.Fatalf("Vig: %v", err)
		}
		if sum != m.ImpliedSum || booking != m.BookingPercentage || over != m.Overround || hold != m.Vig {
			t.Fatalf("standalone (S=%.17g, booking=%.17g, over=%.17g, hold=%.17g) disagrees with Margin (S=%.17g, booking=%.17g, over=%.17g, hold=%.17g)",
				sum, booking, over, hold, m.ImpliedSum, m.BookingPercentage, m.Overround, m.Vig)
		}

		exclusive := 0
		for _, b := range []bool{m.IsOverround(), m.IsFair(), m.IsUnderround()} {
			if b {
				exclusive++
			}
		}
		if exclusive != 1 {
			t.Fatalf("market at S = %.17g satisfies %d of the three classifications, want exactly 1",
				m.ImpliedSum, exclusive)
		}
	})
}

// TestRapidVigContributionsAccountForTheWholeMargin asserts the per-selection
// attribution adds back up: the fair probabilities sum to 1, the excesses sum to the
// market's overround, and the shares sum to 1. An attribution that loses margin
// between selections is an attribution that will under-report a book's hold on
// exactly the lopsided markets where the money is.
func TestRapidVigContributionsAccountForTheWholeMargin(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		market := propGenerateMarket().Draw(t, "market")
		prices := propDecimals(t, market.implied)
		attribution := rapid.SampledFrom([]Attribution{
			AttributionProportional, AttributionUniform,
		}).Draw(t, "attribution")

		contributions, err := VigContributions(prices, attribution)
		if err != nil {
			// AttributionUniform is the additive devig and fails on the same markets
			// for the same reason: an equal slice of the margin can exceed the
			// shortest price in a long field.
			if attribution == AttributionUniform && errors.Is(err, ErrAttributionUndefined) {
				return
			}
			t.Fatalf("%s attribution over %d valid prices: %v", attribution, len(prices), err)
		}

		m, err := NewMargin(prices)
		if err != nil {
			t.Fatalf("NewMargin: %v", err)
		}

		fair := make([]float64, len(contributions))
		excess := make([]float64, len(contributions))
		share := make([]float64, len(contributions))
		for i, c := range contributions {
			if !(c.Fair > 0 && c.Fair < 1) {
				t.Fatalf("selection %d fair probability %.17g is not strictly inside (0, 1)", i, c.Fair)
			}
			// Excess is defined as Implied - Fair, so this is an identity check on
			// the struct rather than a re-derivation, and == is the right test.
			if c.Excess != c.Implied-c.Fair {
				t.Fatalf("selection %d: Excess %.17g != Implied %.17g - Fair %.17g",
					i, c.Excess, c.Implied, c.Fair)
			}
			fair[i] = c.Fair
			excess[i] = c.Excess
			share[i] = c.Share
		}

		propClose(t, "Σ fair", sumOf(fair), 1, propSumTol)
		propClose(t, "Σ excess", sumOf(excess), m.Overround, propRelTol)
		if !m.IsFair() {
			propClose(t, "Σ share", sumOf(share), 1, propRelTol)
		}

		// Under the proportional convention every selection carries the same margin
		// relative to its own fair probability, and that common value is the
		// overround itself. Under the uniform convention it is not — that difference
		// is the favourite-longshot bias, and it is the reason both exist.
		if attribution == AttributionProportional {
			for i, c := range contributions {
				propClose(t, fmt.Sprintf("selection %d relative margin", i),
					c.RelativeMargin, m.Overround, propRelTol)
			}
		}
	})
}

// -----------------------------------------------------------------------------
// Conversions: what is actually true, asserted as such
// -----------------------------------------------------------------------------

// TestRapidAmericanDecimalRoundTripIsATotalIdentity asserts that American → Decimal
// → American returns the original price EXACTLY, for every legal American price.
//
// This is an integer comparison, so == is the correct test and no tolerance appears.
// The identity is total only because NewAmerican folds -100 onto +100: both spell
// decimal 2.0, and without the fold this round trip would have exactly one
// exception that every caller would have to know about.
func TestRapidAmericanDecimalRoundTripIsATotalIdentity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := propAmerican().Draw(t, "american")

		d, err := a.Decimal()
		if err != nil {
			t.Fatalf("american %d has no decimal form: %v", int64(a), err)
		}
		back, err := d.American()
		if err != nil {
			t.Fatalf("decimal %.17g (from american %d) has no american form: %v",
				float64(d), int64(a), err)
		}
		if back != a {
			t.Fatalf("american %d → decimal %.17g → american %d: the round trip is not the identity",
				int64(a), float64(d), int64(back))
		}
	})
}

// TestRapidDecimalAmericanRoundTripErrorIsBounded asserts the honest property in the
// other direction. American prices are integers, so Decimal → American → Decimal
// CANNOT be lossless and asserting that it is would be asserting something false.
//
// Two things are true instead, and both are asserted:
//
//  1. the recovered price differs from the original by at most 0.005 in decimal,
//     which is half an American tick and is proven in propDecimalAmericanBound's
//     comment;
//  2. the American price chosen is the NEAREST one — half away from zero — in
//     AMERICAN space.
//
// The second is the statement that catches a rounding direction bug, which the first
// would not: truncating instead of rounding stays inside 0.01 but is systematically
// wrong.
//
// # Nearest in American space is not nearest in decimal space, and that is fine
//
// An earlier draft of this test asserted that no neighbouring American price maps
// back closer in DECIMAL. At 50,000 checks rapid falsified it in twenty thousand:
// decimal 1.975616455078125 converts to American -102 (whose decimal is 0.0047757
// away) while -103 is 0.0047427 away.
//
// That is not a bug, it is the two branches having different geometry. Above evens
// A = (d-1)·100 is affine in d, so the two orderings coincide and the neighbour
// check is exact. Below evens A = -100/(d-1) is convex in d, the American grid maps
// to unevenly spaced decimals, and a d sitting almost exactly halfway between two
// American prices in A-space is slightly nearer the longer one in d-space. Rounding
// the real-valued American — which is what Decimal.American does, what every book
// does, and what makes the reverse round trip a total identity — is the right
// choice. So the property is asserted in the metric each branch is affine in, and
// the branch split is stated rather than smoothed over.
//
// A price outside the representable band returns ErrAmericanOutOfRange, which is the
// correct answer and not a failure; the generator reaches that band deliberately.
func TestRapidDecimalAmericanRoundTripErrorIsBounded(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := propDecimal().Draw(t, "decimal")

		a, err := d.American()
		if err != nil {
			if !errors.Is(err, ErrAmericanOutOfRange) {
				t.Fatalf("decimal %.17g: unexpected conversion error %v", float64(d), err)
			}
			return
		}
		back, err := a.Decimal()
		if err != nil {
			t.Fatalf("american %d, produced from decimal %.17g, has no decimal form: %v",
				int64(a), float64(d), err)
		}

		got := math.Abs(float64(back) - float64(d))
		if got > propDecimalAmericanBound+propBoundSlack {
			t.Fatalf("decimal %.17g → american %d → decimal %.17g: error %.3g exceeds the half-tick bound %.3g",
				float64(d), int64(a), float64(back), got, propDecimalAmericanBound)
		}

		// The rounding decision, checked in the metric the American grid is uniform
		// in. Restating the branch formula here is the point rather than a
		// duplication: what is under test is which way the real-valued price was
		// rounded, and that cannot be checked without knowing what it was rounded
		// from. The 0.005 bound above is the independent check on the formula
		// itself.
		var exact float64
		if float64(d) >= 2 {
			exact = (float64(d) - 1) * 100
		} else {
			exact = -100 / (float64(d) - 1)
		}
		// Compared against the pre-canonical price: Canonical folds -100 onto +100,
		// so a d a hair under 2 legitimately reports +100 where the real-valued
		// answer is -100.
		signed := float64(a)
		if a == American(MinAmericanMagnitude) && float64(d) < 2 {
			signed = -float64(MinAmericanMagnitude)
		}
		if offset := math.Abs(signed - exact); offset > 0.5+propBoundSlack {
			t.Fatalf("decimal %.17g has real-valued american %.6f but converted to %d, %.6f away — more than half a tick",
				float64(d), exact, int64(a), offset)
		}

		// Above evens the American grid is affine in decimal, so nearest-in-American
		// and nearest-in-decimal coincide exactly and the stronger neighbour check
		// applies. Below evens it does not; see this test's doc comment.
		if float64(d) >= 2 {
			for _, candidate := range []int64{int64(a) - 2, int64(a) - 1, int64(a) + 1, int64(a) + 2} {
				alt, err := NewAmerican(candidate)
				if err != nil || alt == a {
					continue
				}
				altDecimal, err := alt.Decimal()
				if err != nil {
					continue
				}
				if altError := math.Abs(float64(altDecimal) - float64(d)); altError < got-propBoundSlack {
					t.Fatalf("decimal %.17g rounded to american %d (error %.3g) but american %d is closer (error %.3g)",
						float64(d), int64(a), got, int64(alt), altError)
				}
			}
		}
	})
}

// TestRapidDecimalProbabilityRoundTrip asserts that the reciprocal round trips in
// both directions to within a few ULPs. Neither direction is bit-exact — fl(1/fl(1/d))
// can differ from d in the last place — so a tolerance is correct here and exactness
// is not the property.
func TestRapidDecimalProbabilityRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := propDecimal().Draw(t, "decimal")

		p, err := d.Probability()
		if err != nil {
			t.Fatalf("decimal %.17g has no implied probability: %v", float64(d), err)
		}
		if !(float64(p) > 0 && float64(p) < 1) {
			t.Fatalf("decimal %.17g implies probability %.17g, outside the open unit interval",
				float64(d), float64(p))
		}
		back, err := p.Decimal()
		if err != nil {
			t.Fatalf("probability %.17g has no price: %v", float64(p), err)
		}
		propClose(t, "decimal → probability → decimal", float64(back), float64(d), propRelTol)

		// FairDecimal and BreakevenProbability are documented as delegating to these
		// exact conversions rather than reimplementing 1/x. Asserted with == because
		// a single ULP of disagreement would mean the delegation was replaced by a
		// second copy of the formula.
		fair, err := FairDecimal(p)
		if err != nil {
			t.Fatalf("FairDecimal(%.17g): %v", float64(p), err)
		}
		breakeven, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability(%.17g): %v", float64(d), err)
		}
		if fair != back || breakeven != p {
			t.Fatalf("FairDecimal/BreakevenProbability diverged from the conversions they delegate to: fair %.17g vs %.17g, breakeven %.17g vs %.17g",
				float64(fair), float64(back), float64(breakeven), float64(p))
		}
	})
}

// TestRapidProbabilityDecimalRoundTrip is the same check started from a probability.
func TestRapidProbabilityDecimalRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := propPriceableProbability().Draw(t, "probability")

		d, err := p.Decimal()
		if err != nil {
			t.Fatalf("probability %.17g has no price: %v", float64(p), err)
		}
		if !(float64(d) > 1) {
			t.Fatalf("probability %.17g priced at %.17g, which is not above evens-with-no-profit",
				float64(p), float64(d))
		}
		back, err := d.Probability()
		if err != nil {
			t.Fatalf("decimal %.17g has no implied probability: %v", float64(d), err)
		}
		propClose(t, "probability → decimal → probability", float64(back), float64(p), propRelTol)
	})
}

// TestRapidFractionalRoundTripIsExactOnTheLadder asserts that a fraction whose
// denominator is inside MaxFractionalDenominator survives Fractional → Decimal →
// Fractional exactly, in lowest terms.
//
// This is a true "lossless" claim and it is asserted with integer equality, no
// tolerance. It holds because two distinct rationals with denominators at most 1000
// differ by at least 1/1000² = 1e-6, which is six orders of magnitude above the
// 1e-12 convergence tolerance the continued fraction stops on — so the expansion
// cannot stop on the wrong fraction.
//
// # Why the numerator is capped at 1000 and not at MaxFractionalNumerator
//
// The bound is not about the algorithm, it is about the float64 that carries the
// value between the two conversions. The round trip computes d = 1 + n/den and then
// recovers n/den as d-1, and that subtraction carries an absolute error of about
// 1.1e-16·d. For n/den near 1e6 that error is ~1e-10, a hundred times the
// convergence tolerance, so the expansion would legitimately stop on a different
// fraction. Capping the ratio at 1000 keeps the carried error near 1e-13, an order
// of magnitude inside the tolerance. Beyond that ratio the honest property is the
// bounded one, which TestRapidFractionalApproximationReportsItsOwnError asserts.
func TestRapidFractionalRoundTripIsExactOnTheLadder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		num := rapid.Int64Range(1, 1000).Draw(t, "numerator")
		den := rapid.Int64Range(1, MaxFractionalDenominator).Draw(t, "denominator")

		f, err := NewFractional(num, den)
		if err != nil {
			t.Fatalf("NewFractional(%d, %d): %v", num, den, err)
		}
		d, err := f.Decimal()
		if err != nil {
			t.Fatalf("fractional %s has no decimal form: %v", f, err)
		}
		back, err := d.Fractional()
		if err != nil {
			t.Fatalf("decimal %.17g (from %s) has no fractional form: %v", float64(d), f, err)
		}
		if back != f {
			t.Fatalf("%s → decimal %.17g → %s: the ladder round trip is not exact",
				f, float64(d), back)
		}
	})
}

// TestRapidFractionalApproximationReportsItsOwnError asserts that FractionalApprox's
// reported error is the error it actually made. A converter that returned a
// plausible fraction and an optimistic error estimate would let the UI print "5/2"
// where "≈5/2" was warranted, which is a small lie about a price.
func TestRapidFractionalApproximationReportsItsOwnError(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := propDecimal().Draw(t, "decimal")

		f, reported, err := d.FractionalApprox()
		if err != nil {
			// Prices shorter than roughly 1/1000 have no fractional form at all.
			if !errors.Is(err, ErrFractionalNotRepresentable) {
				t.Fatalf("decimal %.17g: unexpected fractional error %v", float64(d), err)
			}
			return
		}
		if err := f.Validate(); err != nil {
			t.Fatalf("decimal %.17g produced an invalid fraction %s: %v", float64(d), f, err)
		}
		if f.Reduce() != f {
			t.Fatalf("decimal %.17g produced %s, which is not in lowest terms", float64(d), f)
		}
		if f.Denominator > MaxFractionalDenominator || f.Numerator > MaxFractionalNumerator {
			t.Fatalf("decimal %.17g produced %s, outside the stated bounds %d/%d",
				float64(d), f, MaxFractionalNumerator, MaxFractionalDenominator)
		}

		actual := math.Abs(float64(f.Numerator)/float64(f.Denominator) - (float64(d) - 1))
		propClose(t, fmt.Sprintf("reported error of %s for decimal %.17g", f, float64(d)),
			reported, actual, propRelTol)

		// Every convergent p/q of a continued fraction satisfies |x - p/q| < 1/q².
		// The bound is the whole reason a convergent is an acceptable answer, so it
		// is worth asserting rather than trusting.
		q := float64(f.Denominator)
		if actual >= 1/(q*q)+propBoundSlack {
			t.Fatalf("decimal %.17g → %s: error %.3g violates the convergent bound 1/q² = %.3g",
				float64(d), f, actual, 1/(q*q))
		}
	})
}

// -----------------------------------------------------------------------------
// Expected value
// -----------------------------------------------------------------------------

// TestRapidExpectedValueSignAgreesWithGrossReturn asserts the sign relationship the
// phase brief names: EV is positive exactly when q·d exceeds 1.
//
// The gross return is recomputed here in the same shape the package uses —
// float64(q)·float64(d) — because the point is to check the SIGN decision, not to
// reimplement the formula with different rounding and then complain about the last
// bit.
func TestRapidExpectedValueSignAgreesWithGrossReturn(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := propPriceableProbability().Draw(t, "q")
		d := propDecimal().Draw(t, "d")

		ev, err := ExpectedValue(q, d)
		if err != nil {
			t.Fatalf("ExpectedValue(%.17g, %.17g): %v", float64(q), float64(d), err)
		}
		if math.IsNaN(ev) || math.IsInf(ev, 0) {
			t.Fatalf("ExpectedValue(%.17g, %.17g) = %v, which is not a number", float64(q), float64(d), ev)
		}

		gross := float64(q) * float64(d)
		switch {
		case gross > 1 && !(ev > 0):
			t.Fatalf("q·d = %.17g > 1 but EV = %.17g is not positive", gross, ev)
		case gross < 1 && !(ev < 0):
			t.Fatalf("q·d = %.17g < 1 but EV = %.17g is not negative", gross, ev)
		case gross == 1 && ev != 0:
			t.Fatalf("q·d = 1 exactly but EV = %.17g is not zero", ev)
		}

		// EV lies in [-1, d-1]: it is -1 when the stake is certainly lost and d-1
		// when the profit is certain.
		if ev < -1-propRelTol || ev > float64(d)-1+propRelTol {
			t.Fatalf("EV = %.17g lies outside [-1, %.17g]", ev, float64(d)-1)
		}

		// The percentage spelling is exactly one hundred times the fraction. The two
		// exist because confusing them is an off-by-100 stake-sizing error.
		pct, err := ExpectedValuePercent(q, d)
		if err != nil {
			t.Fatalf("ExpectedValuePercent(%.17g, %.17g): %v", float64(q), float64(d), err)
		}
		propClose(t, "ExpectedValuePercent", pct, ev*100, propRelTol)
	})
}

// TestRapidExpectedValueIsNeverPositiveAtTheBreakevenPrice asserts the one-sided
// guarantee ev.go actually proves, rather than the two-sided one it would be
// convenient to assume.
//
// The phase brief asks for "exactly zero at the breakeven probability". That is
// stronger than what is true and stronger than what the implementation claims:
// ExpectedValue's doc comment says the answer at q = BreakevenProbability(d) is
// "zero or one unit in the last place below it — never above", and that one-sided
// bound is what Kelly's zero-at-zero-edge guarantee actually rests on. Asserting an
// exact zero would be asserting a stronger property than the code provides.
//
// So the assertion is: never positive, and never further below zero than a couple of
// ULPs. The test additionally counts how often it IS exactly zero and logs it, so
// the gap between the brief's wording and the code's guarantee is visible rather
// than buried.
func TestRapidExpectedValueIsNeverPositiveAtTheBreakevenPrice(t *testing.T) {
	var checked, exact int
	rapid.Check(t, func(t *rapid.T) {
		d := propDecimal().Draw(t, "d")

		q, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability(%.17g): %v", float64(d), err)
		}
		ev, err := ExpectedValue(q, d)
		if err != nil {
			t.Fatalf("ExpectedValue at breakeven: %v", err)
		}

		checked++
		if ev == 0 {
			exact++
		}
		if ev > 0 {
			t.Fatalf("decimal %.17g at its own breakeven probability %.17g reports a POSITIVE expected value %.17g; the +EV finder would manufacture an edge out of nothing",
				float64(d), float64(q), ev)
		}
		// Two ULPs at magnitude 1 is 4.5e-16. Anything below that is the documented
		// rounding of fl(1/d); anything larger is a formula error.
		if ev < -4*math.Ldexp(1, -53) {
			t.Fatalf("decimal %.17g at breakeven reports EV = %.17g, further below zero than rounding explains",
				float64(d), ev)
		}

		// Kelly's guarantee at the same point IS exact, because it branches on the
		// gross return rather than on the subtraction.
		f, err := Kelly(q, d)
		if err != nil {
			t.Fatalf("Kelly at breakeven: %v", err)
		}
		if f != 0 {
			t.Fatalf("decimal %.17g at its own breakeven probability recommends staking %.17g of bankroll; the documented answer is exactly zero",
				float64(d), f)
		}
	})
	t.Logf("expected value at the breakeven price was exactly zero in %d of %d cases; the remainder were within one ULP below, which is the documented guarantee",
		exact, checked)
}

// TestRapidEdgeAgreesWithExpectedValue asserts the identity ev.go derives: the
// "percentage edge" a trader quotes in probability space and the "expected value" a
// bettor quotes in stake space are one quantity, q/p - 1 = q·d - 1.
//
// The identity is exact only in real arithmetic — Edge divides by p where
// ExpectedValue multiplies by d = fl(1/p) — so this is a tolerance comparison and
// deliberately not an equality.
func TestRapidEdgeAgreesWithExpectedValue(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := propPriceableProbability().Draw(t, "q")
		d := propDecimal().Draw(t, "d")

		p, err := d.Probability()
		if err != nil {
			t.Fatalf("decimal %.17g has no implied probability: %v", float64(d), err)
		}
		edge, err := Edge(q, p)
		if err != nil {
			t.Fatalf("Edge(%.17g, %.17g): %v", float64(q), float64(p), err)
		}
		ev, err := ExpectedValue(q, d)
		if err != nil {
			t.Fatalf("ExpectedValue(%.17g, %.17g): %v", float64(q), float64(d), err)
		}

		// Compared on the gross quantities rather than on the edges themselves: at
		// an edge near zero the two differ by a few ULPs of 1, which is an enormous
		// RELATIVE difference on a value of 1e-17 and a meaningless one on the
		// quantity anybody cares about. Adding 1 back puts both on the scale the
		// rounding actually happened at.
		propClose(t, "1 + Edge vs 1 + EV", 1+edge, 1+ev, propRelTol)

		pctEdge, err := EdgePercent(q, p)
		if err != nil {
			t.Fatalf("EdgePercent: %v", err)
		}
		propClose(t, "EdgePercent", pctEdge, edge*100, propRelTol)
	})
}

// -----------------------------------------------------------------------------
// Kelly: the charter's third named invariant
// -----------------------------------------------------------------------------

// TestRapidKellyInvariants asserts everything the phase brief names about staking:
// zero at zero edge, monotone increasing in edge, never above 1, never negative, and
// the integer minor-unit stake never exceeding the theoretical fractional one.
//
// # The integer stake
//
// internal/domain.Money is int64 minor units (CLAUDE.md §12) and the domain package
// cannot be imported here without inverting the dependency — internal/domain/odds is
// the leaf. So the conversion is reproduced in the shape Money.MulFloat with
// RoundTowardZero performs it: multiply by the fraction, truncate. Bankrolls are
// drawn up to 2^53-1, the ceiling Money imposes precisely so that this multiplication
// is exact rather than approximate.
//
// Truncation is the mode the property holds under. Money.MulFloat also offers
// RoundHalfAwayFromZero and RoundHalfToEven, and under either of those an integer
// stake can legitimately exceed the fractional one by up to half a minor unit. The
// property is about truncation and is asserted as such rather than being weakened to
// cover all three.
func TestRapidKellyInvariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := propDecimal().Draw(t, "d")
		q := propPriceableProbability().Draw(t, "q")

		f, err := Kelly(q, d)
		if err != nil {
			t.Fatalf("Kelly(%.17g, %.17g): %v", float64(q), float64(d), err)
		}

		// Never negative, never above the whole bankroll.
		if !(f >= 0 && f <= 1) {
			t.Fatalf("Kelly(%.17g, %.17g) = %.17g, outside [0, 1]", float64(q), float64(d), f)
		}

		// Zero exactly when the price is not worth taking. Both directions.
		breakeven, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability: %v", err)
		}
		if float64(q) <= float64(breakeven) && f != 0 {
			t.Fatalf("q = %.17g is at or below the breakeven %.17g at decimal %.17g, but Kelly recommends %.17g",
				float64(q), float64(breakeven), float64(d), f)
		}

		// Fractional Kelly scales the finished fraction and can never exceed it.
		multiplier := rapid.Float64Range(math.SmallestNonzeroFloat64, 1).Draw(t, "kellyMultiplier")
		scaled, err := FractionalKelly(q, d, multiplier)
		if err != nil {
			t.Fatalf("FractionalKelly(%.17g, %.17g, %.17g): %v",
				float64(q), float64(d), multiplier, err)
		}
		if scaled > f {
			t.Fatalf("FractionalKelly at multiplier %.17g returned %.17g, above full Kelly %.17g",
				multiplier, scaled, f)
		}
		if f == 0 && scaled != 0 {
			t.Fatalf("full Kelly is zero but the %.17g-Kelly stake is %.17g", multiplier, scaled)
		}
		propClose(t, "FractionalKelly", scaled, f*multiplier, propRelTol)

		// The integer minor-unit stake. Bankrolls span nine orders of magnitude:
		// 100 minor units (one major unit) up to Money's 2^53-1 ceiling.
		bankroll := rapid.Int64Range(100, 1<<53-1).Draw(t, "bankrollMinorUnits")
		theoretical := float64(bankroll) * f
		stake := int64(math.Trunc(theoretical))
		if stake < 0 || stake > bankroll {
			t.Fatalf("bankroll %d at Kelly %.17g yields a stake of %d, outside [0, bankroll]",
				bankroll, f, stake)
		}
		if float64(stake) > theoretical {
			t.Fatalf("bankroll %d at Kelly %.17g yields an integer stake of %d, above the theoretical %.17g",
				bankroll, f, stake, theoretical)
		}
	})
}

// TestRapidKellyIsMonotoneInEdge asserts that a better probability at the same price
// never recommends a smaller stake. This is the property that makes Kelly usable as
// a ranking as well as a size: if it were not monotone, a marginally better estimate
// could recommend a marginally smaller bet, and no staking policy built on it would
// be coherent.
func TestRapidKellyIsMonotoneInEdge(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := propDecimal().Draw(t, "d")
		lo := propPriceableProbability().Draw(t, "qLow")
		hi := propPriceableProbability().Draw(t, "qHigh")
		if float64(lo) > float64(hi) {
			lo, hi = hi, lo
		}

		fLo, err := Kelly(lo, d)
		if err != nil {
			t.Fatalf("Kelly at the lower probability: %v", err)
		}
		fHi, err := Kelly(hi, d)
		if err != nil {
			t.Fatalf("Kelly at the higher probability: %v", err)
		}
		if fHi < fLo {
			t.Fatalf("at decimal %.17g, raising the probability from %.17g to %.17g LOWERED the Kelly stake from %.17g to %.17g",
				float64(d), float64(lo), float64(hi), fLo, fHi)
		}
	})
}

// TestRapidKellyMaximisesTheGrowthRate asserts Kelly against its own definition
// rather than against a restatement of its formula: the fraction it returns must
// maximise g(f) = q·ln(1 + f·(d-1)) + (1-q)·ln(1 - f), the expected log growth rate
// of the bankroll.
//
// This is the strongest available statement about the function. A transcription
// error in the formula would still round-trip against an algebraic rearrangement of
// itself; it would not survive being compared against the objective it is supposed
// to optimise.
//
// The competing fraction is capped strictly below 1 because g(1) is negative
// infinity for any q < 1 — staking the whole bankroll on something that can lose ends
// the sequence — which GrowthRate correctly refuses with ErrCertainRuin rather than
// returning -Inf.
func TestRapidKellyMaximisesTheGrowthRate(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := propDecimal().Draw(t, "d")
		q := propPriceableProbability().Draw(t, "q")
		competitor := rapid.Float64Range(0, 1-1e-9).Draw(t, "competingFraction")

		optimal, err := Kelly(q, d)
		if err != nil {
			t.Fatalf("Kelly(%.17g, %.17g): %v", float64(q), float64(d), err)
		}
		gOptimal, err := GrowthRate(q, d, optimal)
		if err != nil {
			t.Fatalf("GrowthRate at the Kelly fraction %.17g: %v", optimal, err)
		}
		gCompetitor, err := GrowthRate(q, d, competitor)
		if err != nil {
			t.Fatalf("GrowthRate at the competing fraction %.17g: %v", competitor, err)
		}

		// The tolerance is relative to the scale of the growth rates being compared,
		// not absolute: g is unbounded below (a bad stake at a long price approaches
		// -infinity) while the optimum is a small positive number, so an absolute
		// epsilon would be meaningless at one end and vacuous at the other.
		scale := math.Max(1, math.Max(math.Abs(gOptimal), math.Abs(gCompetitor)))
		if gCompetitor > gOptimal+propRelTol*scale {
			t.Fatalf("at q = %.17g, decimal %.17g: staking %.17g grows the bankroll at %.17g, faster than the Kelly fraction %.17g at %.17g",
				float64(q), float64(d), competitor, gCompetitor, optimal, gOptimal)
		}

		// Not betting neither grows nor shrinks a bankroll, exactly.
		zero, err := GrowthRate(q, d, 0)
		if err != nil {
			t.Fatalf("GrowthRate at zero stake: %v", err)
		}
		if zero != 0 {
			t.Fatalf("GrowthRate at a zero stake = %.17g, want exactly 0", zero)
		}
		if gOptimal < 0 {
			t.Fatalf("the Kelly fraction %.17g has a negative growth rate %.17g, worse than not betting",
				optimal, gOptimal)
		}
	})
}

// -----------------------------------------------------------------------------
// Parlay pricing and correlation
// -----------------------------------------------------------------------------

// propParlayLegs draws a short parlay's marginal probabilities.
//
// Bounded at four legs and at probabilities no shorter than 0.02 for a stated
// reason: past two legs the copula is evaluated by lattice quadrature, whose cost
// grows with both leg count and how far into the tail the thresholds sit. A
// 25-leg same-game parlay of 100-to-1 shots is representable and is covered by the
// package's own worked tests; generating it a few hundred times per property here
// would trade minutes of wall clock for coverage those tests already have.
func propParlayLegs() *rapid.Generator[[]Probability] {
	return rapid.Custom(func(t *rapid.T) []Probability {
		n := rapid.IntRange(2, 4).Draw(t, "parlayLegs")
		out := make([]Probability, n)
		for i := range out {
			v := rapid.Float64Range(0.02, 0.98).Draw(t, fmt.Sprintf("leg%d", i))
			p, err := NewProbability(v)
			if err != nil {
				t.Fatalf("NewProbability(%v): %v", v, err)
			}
			out[i] = p
		}
		return out
	})
}

// TestRapidZeroCorrelationReproducesTheIndependentProduct asserts the exactness the
// package claims: with the identity matrix the copula short-circuits to the product
// of the marginals and is bit-identical to it, not merely close.
//
// Equality is the correct test here precisely because the documentation promises
// bit-identity. A tolerance would let a regression that started computing a product
// of ones through the quadrature pass silently, at a cost of several milliseconds
// per parlay quote and a few ULPs of drift.
func TestRapidZeroCorrelationReproducesTheIndependentProduct(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		marginals := propParlayLegs().Draw(t, "marginals")

		identity, err := IdentityCorrelation(len(marginals))
		if err != nil {
			t.Fatalf("IdentityCorrelation(%d): %v", len(marginals), err)
		}
		independent, err := JointProbabilityIndependent(marginals)
		if err != nil {
			t.Fatalf("JointProbabilityIndependent: %v", err)
		}
		correlated, err := GaussianCopulaJoint(marginals, identity)
		if err != nil {
			t.Fatalf("GaussianCopulaJoint under the identity: %v", err)
		}
		if correlated != independent {
			t.Fatalf("under zero correlation the copula returned %.17g, not the independent product %.17g",
				float64(correlated), float64(independent))
		}

		// And the product itself is the product. The reference multiplies the
		// marginals in ascending order because that is the order JointProbability-
		// Independent documents (see orderedProduct) — float64 multiplication is not
		// associative, so "the product" is only a well-defined bit pattern once an
		// order is fixed. The sort here is written out rather than calling the
		// production helper, so the two paths remain independent.
		sorted := make([]float64, len(marginals))
		for i, p := range marginals {
			sorted[i] = float64(p)
		}
		slices.Sort(sorted)
		want := 1.0
		for _, p := range sorted {
			want *= p
		}
		if float64(independent) != want {
			t.Fatalf("JointProbabilityIndependent = %.17g, want the plain product %.17g",
				float64(independent), want)
		}

		// The parlay price is the product of the leg prices, for the same reason:
		// a parlay is a rollover, so it composes multiplicatively.
		legs := make([]Decimal, len(marginals))
		product := 1.0
		for i, p := range marginals {
			d, err := p.Decimal()
			if err != nil {
				t.Fatalf("leg %d: %v", i, err)
			}
			legs[i] = d
			product *= float64(d)
		}
		priced, err := ParlayDecimal(legs)
		if err != nil {
			t.Fatalf("ParlayDecimal: %v", err)
		}
		propClose(t, "ParlayDecimal", float64(priced), product, propRelTol)
	})
}

// TestRapidCopulaIsMonotoneInCorrelation asserts that raising the correlation raises
// the joint probability — Slepian's inequality for the Gaussian orthant, and the
// reason a same-game parlay must be priced shorter than the product of its legs.
//
// A book that priced correlated legs as independent would be selling a ticket for
// less than it is worth, every time, on exactly the same-game parlays that are the
// most-bet product on the board. This is the property that stops that.
//
// # Two tolerances, both justified
//
// At two legs the copula is BivariateNormalCDF, a closed form accurate to about
// 1e-15, so the comparison is held to propRelTol. At three or more it is a lattice
// quadrature whose measured error reaches ~4e-4, so the comparison is held to
// propOrthantSlack. Using the loose bound everywhere would throw away the strength
// of the exact case; using the tight bound everywhere would make the test flaky.
func TestRapidCopulaIsMonotoneInCorrelation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		marginals := propParlayLegs().Draw(t, "marginals")
		n := len(marginals)

		// Equicorrelation with rho in [0, 0.95] is positive semi-definite for every
		// n, so no draw is rejected. Non-negative because the property under test is
		// the positive-correlation one; negative correlation is the mirror image and
		// is covered by the package's own tests.
		rhoA := rapid.Float64Range(0, 0.95).Draw(t, "rhoA")
		rhoB := rapid.Float64Range(0, 0.95).Draw(t, "rhoB")
		if rhoA > rhoB {
			rhoA, rhoB = rhoB, rhoA
		}

		jointAt := func(rho float64) float64 {
			c, err := NewCorrelationMatrix(equicorrelated(n, rho))
			if err != nil {
				t.Fatalf("equicorrelated(%d, %v) is not a valid correlation matrix: %v", n, rho, err)
			}
			j, err := GaussianCopulaJoint(marginals, c)
			if err != nil {
				t.Fatalf("GaussianCopulaJoint at rho = %v over %d legs: %v", rho, n, err)
			}
			return float64(j)
		}

		lower, upper := jointAt(rhoA), jointAt(rhoB)

		slack := propOrthantSlack
		if n == 2 {
			slack = propRelTol
		}
		if upper < lower-slack {
			t.Fatalf("raising the correlation from %v to %v over %d legs LOWERED the joint probability from %.17g to %.17g (slack %.3g)",
				rhoA, rhoB, n, lower, upper, slack)
		}

		// Both are probabilities, and both respect the Fréchet-Hoeffding bounds that
		// hold under any dependence structure whatsoever.
		frechetUpper := 1.0
		total := 0.0
		for _, p := range marginals {
			frechetUpper = math.Min(frechetUpper, float64(p))
			total += float64(p)
		}
		frechetLower := math.Max(0, total-float64(n-1))
		for _, j := range []float64{lower, upper} {
			if j < frechetLower-propBoundSlack || j > frechetUpper+propBoundSlack {
				t.Fatalf("joint probability %.17g lies outside the Fréchet-Hoeffding interval [%.17g, %.17g]",
					j, frechetLower, frechetUpper)
			}
			if !(j > 0 && j <= 1) {
				t.Fatalf("joint probability %.17g is not inside (0, 1]", j)
			}
		}
	})
}

// TestRapidCorrelationShortensTheParlayPrice asserts the same fact in price space,
// which is the form the bet slip actually shows: positive correlation raises the
// joint probability, so the correlated price is never longer than the independent
// one, and the haircut QuoteParlay reports is never negative.
func TestRapidCorrelationShortensTheParlayPrice(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		marginals := propParlayLegs().Draw(t, "marginals")
		n := len(marginals)
		rho := rapid.Float64Range(0, 0.95).Draw(t, "rho")

		legs := make([]Decimal, n)
		for i, p := range marginals {
			d, err := p.Decimal()
			if err != nil {
				t.Fatalf("leg %d: %v", i, err)
			}
			legs[i] = d
		}
		c, err := NewCorrelationMatrix(equicorrelated(n, rho))
		if err != nil {
			t.Fatalf("equicorrelated(%d, %v): %v", n, rho, err)
		}

		quote, err := QuoteParlay(legs, c)
		if err != nil {
			t.Fatalf("QuoteParlay over %d legs at rho = %v: %v", n, rho, err)
		}

		if quote.Legs != n {
			t.Fatalf("QuoteParlay reported %d legs, want %d", quote.Legs, n)
		}
		// The correlated probability is at least the independent one, so the
		// correlated price is at most the independent one. The slack is the
		// quadrature's, expressed in price space by dividing through by the joint
		// probability, since d = 1/p.
		if float64(quote.CorrelatedProbability) < float64(quote.IndependentProbability)-propOrthantSlack {
			t.Fatalf("at rho = %v over %d legs the correlated probability %.17g fell below the independent %.17g",
				rho, n, float64(quote.CorrelatedProbability), float64(quote.IndependentProbability))
		}
		if haircut := quote.CorrelationHaircut(); haircut < -propOrthantSlack {
			t.Fatalf("at rho = %v over %d legs the correlation haircut is %.17g, i.e. the correlated ticket pays MORE than the independent one",
				rho, n, haircut)
		}
		if quote.Exact != c.CopulaIsExact() {
			t.Fatalf("QuoteParlay reported Exact = %v but the matrix reports CopulaIsExact = %v",
				quote.Exact, c.CopulaIsExact())
		}

		// A parlay is never shorter than its longest leg: rolling a winning leg onto
		// another cannot reduce the return.
		longest := 0.0
		for _, d := range legs {
			longest = math.Max(longest, float64(d))
		}
		if float64(quote.CorrelatedDecimal) < longest-propOrthantSlack*float64(quote.CorrelatedDecimal) {
			t.Fatalf("a %d-leg parlay priced at %.17g is shorter than its longest leg %.17g",
				n, float64(quote.CorrelatedDecimal), longest)
		}
	})
}
