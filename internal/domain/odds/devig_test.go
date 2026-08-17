package odds

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Float comparison policy in this file follows convert_test.go: closeTo and
// assertClose from that file are reused rather than redefined, and nothing is
// compared with == except where exactness is the property under test and is
// argued for at the assertion.

const (
	// devigSumTolerance bounds |Σq - 1| after the shared renormalisation step.
	//
	// Renormalisation divides each value by the float64 sum of all of them, so the
	// only error left is the rounding of that sum and of the n divisions: at most
	// about n ULPs, i.e. n·1.1e-16. 1e-14 covers markets up to roughly 90
	// selections with the full ULP budget spent, which is every market in the test
	// suite and every market a book actually posts on one screen. It is also eleven
	// orders of magnitude below the smallest probability difference the domain
	// cares about, so it cannot absorb a real normalisation bug.
	devigSumTolerance = 1e-14

	// devigReferenceTolerance is used when comparing against numbers produced by an
	// independent implementation of the same method.
	//
	// It is deliberately looser than this package's own precision, because the
	// reference values carry the reference implementation's convergence threshold,
	// not ours. What the comparison is really testing is the *formula*: a
	// transcription error in Shin's equations moves a probability in the third
	// decimal place, not the ninth. Agreement to 1e-9 rules out every wrong
	// formulation while leaving room for two different root-finding schemes to stop
	// in slightly different places.
	devigReferenceTolerance = 1e-9

	// devigPermutationTolerance is the *relative* bound on how far a result may move
	// when the selections are relabelled. It is applied through closeTo, so for a
	// probability it degrades to an absolute 1e-12 and for a larger parameter it
	// scales with the value.
	//
	// It is not zero, and the reason is IEEE-754 rather than this code. Floating
	// point addition is commutative but not associative, so a sum accumulated left
	// to right depends on the order of its terms. Two permutations of one market
	// therefore produce overrounds a ULP apart. For the two closed-form methods that
	// is the end of it and the outputs differ by a few ULPs. For power and Shin it
	// is not: the residual function itself is a sum, so the two permutations present
	// the solver with objectives that differ in their last bits, and their roots
	// differ correspondingly. The measured worst case is about 2e-14 on an exponent
	// near 3.4 — roughly 5e-15 relative, which then propagates into the
	// probabilities amplified by |ln p|. 1e-12 relative sits two orders above that
	// and still nine orders below the finest difference this domain can express.
	//
	// Making it exactly zero would mean summing in a canonical order — sorting the
	// inputs before accumulating. That is deliberately not done: selection order is
	// already stable in this system (SelectionRole carries a display order), so the
	// permutation case does not arise in production; a sort would make every devig
	// O(n log n); and the reported overround would then stop matching the sum a
	// caller computes for themselves. Determinism for a *fixed* input order is the
	// property that actually matters, and it is asserted bit-for-bit by
	// TestDevigIsDeterministic.
	devigPermutationTolerance = 1e-12

	// devigStrictOrderGap is the minimum separation between two implied
	// probabilities at which order preservation is asserted *strictly*.
	//
	// Below it the assertion is only that order is not reversed. The reason is
	// honest rather than defensive: every method here maps p through correctly
	// rounded float64 arithmetic, which is monotone non-decreasing but not strictly
	// monotone — two implied probabilities a few ULPs apart can round to the same
	// output. A gap of 1e-12 is nine orders above that rounding floor and nine
	// orders below the finest gap a real market produces (two selections at -110
	// and -109 differ by 4e-4 in implied probability), so the strict assertion
	// applies to every case that could exist in production.
	devigStrictOrderGap = 1e-12
)

// -----------------------------------------------------------------------------
// Market fixtures
// -----------------------------------------------------------------------------

// market is a set of prices for one market, written in the American convention
// because that is how they are published.
//
// None of these are attributed to a specific event on a specific date — that claim
// would need a recorded provider payload, which is what the ingest phase's golden
// files are for. They are price *shapes*: the -110/-110 point-spread juice, a
// standard two-way moneyline, a three-way board carrying a +2500 longshot, and a
// twelve-runner futures board. What is being tested is the arithmetic, and the
// arithmetic does not know what the teams are called.
type market struct {
	name     string
	american []int64
}

func (m market) implied(t *testing.T) []Probability {
	t.Helper()
	out := make([]Probability, len(m.american))
	for i, a := range m.american {
		am, err := NewAmerican(a)
		if err != nil {
			t.Fatalf("%s: NewAmerican(%d): %v", m.name, a, err)
		}
		p, err := am.Probability()
		if err != nil {
			t.Fatalf("%s: American(%d).Probability(): %v", m.name, a, err)
		}
		out[i] = p
	}
	return out
}

func (m market) overround(t *testing.T) float64 {
	t.Helper()
	sum := 0.0
	for _, p := range m.implied(t) {
		sum += float64(p)
	}
	return sum
}

// balancedTwoWay is the standard point-spread market. Both sides are the identical
// float64, which is what makes the "exactly 0.5" assertions provable rather than
// approximate.
var balancedTwoWay = market{name: "-110/-110 point spread", american: []int64{-110, -110}}

// unbalancedTwoWay is an ordinary two-way moneyline with a favourite.
var unbalancedTwoWay = market{name: "-140/+120 moneyline", american: []int64{-140, 120}}

// threeWayWithLongshot is the acceptance-criterion market: three selections, one of
// them a +2500 longshot, at an overround (~4.9%) typical of a real three-way board.
// It is where the four methods are required to disagree.
var threeWayWithLongshot = market{name: "-350/+330/+2500 three-way", american: []int64{-350, 330, 2500}}

// futuresBoard is a twelve-runner outright market at a ~14.5% overround. Its
// shortest price is +10000, which is below the per-selection deduction the additive
// method computes — so it is the case where additive is not merely inaccurate but
// undefined.
var futuresBoard = market{
	name:     "twelve-runner futures board",
	american: []int64{250, 400, 550, 700, 900, 1200, 1600, 2000, 2500, 3300, 5000, 10000},
}

func allMarkets() []market {
	return []market{balancedTwoWay, unbalancedTwoWay, threeWayWithLongshot, futuresBoard}
}

// mustDevig runs a method and fails the test if it errors.
func mustDevig(t *testing.T, m DevigMethod, implied []Probability) DevigResult {
	t.Helper()
	r, err := Devig(m, implied)
	if err != nil {
		t.Fatalf("Devig(%s): %v", m, err)
	}
	return r
}

// -----------------------------------------------------------------------------
// The mandatory invariants, asserted for every method on every market
// -----------------------------------------------------------------------------

// TestEveryMethodSatisfiesTheCoreInvariants is the sweep the whole file rests on.
// Whatever else a devigging method models, its output must be a probability
// distribution over the selections that preserves their order.
func TestEveryMethodSatisfiesTheCoreInvariants(t *testing.T) {
	for _, mk := range allMarkets() {
		implied := mk.implied(t)
		for _, method := range DevigMethods() {
			t.Run(mk.name+"/"+method.String(), func(t *testing.T) {
				res, err := Devig(method, implied)
				if err != nil {
					// Additive on the futures board is a documented, tested failure
					// rather than an unexpected one; it has its own test below.
					if method == MethodAdditive && errors.Is(err, ErrDevigAdditiveNonPositive) {
						t.Skipf("additive is undefined on this market by construction: %v", err)
					}
					t.Fatalf("Devig(%s): %v", method, err)
				}

				if got := len(res.Probabilities); got != len(implied) {
					t.Fatalf("got %d probabilities for %d selections", got, len(implied))
				}
				if res.Method != method {
					t.Errorf("result reports method %s, want %s", res.Method, method)
				}

				// Invariant 1: the fair probabilities sum to 1.
				sum := 0.0
				for _, q := range res.Probabilities {
					sum += float64(q)
				}
				if math.Abs(sum-1) > devigSumTolerance {
					t.Errorf("Σq = %.17g, off 1 by %.3g (tolerance %.3g)", sum, sum-1, devigSumTolerance)
				}

				// Invariant 2: every fair probability is strictly inside (0, 1).
				for i, q := range res.Probabilities {
					f := float64(q)
					if !(f > 0 && f < 1) {
						t.Errorf("q[%d] = %.17g, want strictly inside (0, 1)", i, f)
					}
					if math.IsNaN(f) || math.IsInf(f, 0) {
						t.Errorf("q[%d] = %v", i, f)
					}
				}

				// Invariant 3: order is preserved.
				assertOrderPreserved(t, implied, res.Probabilities)

				// Invariant 4: devigging never inflates the market. Every fair
				// probability is at most its implied one, because a book with margin
				// overstates every outcome it prices.
				for i := range implied {
					if float64(res.Probabilities[i]) > float64(implied[i])+devigSumTolerance {
						t.Errorf("q[%d] = %.17g exceeds the implied p[%d] = %.17g on a market with overround %g",
							i, float64(res.Probabilities[i]), i, float64(implied[i]), res.Overround)
					}
				}

				// The reported overround must be the actual sum of the inputs.
				wantS := 0.0
				for _, p := range implied {
					wantS += float64(p)
				}
				assertClose(t, "reported overround", res.Overround, wantS, relTolExact)

				hold, err := res.Vig()
				if err != nil {
					t.Fatalf("Vig: %v", err)
				}
				t.Logf("S = %.6f, hold = %.4f%%, parameter = %.12g, %d solver iterations",
					res.Overround, 100*hold, res.Parameter, res.Iterations)
			})
		}
	}
}

// assertOrderPreserved checks that the devigged probabilities rank the selections
// exactly as the implied ones did. See devigStrictOrderGap for why the strict form
// is only asserted above a gap.
func assertOrderPreserved(t *testing.T, implied, fair []Probability) {
	t.Helper()
	for i := range implied {
		for j := range implied {
			pi, pj := float64(implied[i]), float64(implied[j])
			qi, qj := float64(fair[i]), float64(fair[j])
			if pi > pj && qi < qj {
				t.Errorf("order reversed: p[%d] = %g > p[%d] = %g but q[%d] = %g < q[%d] = %g",
					i, pi, j, pj, i, qi, j, qj)
			}
			if pi-pj > devigStrictOrderGap && !(qi > qj) {
				t.Errorf("strict order lost: p[%d] - p[%d] = %g but q[%d] = %.17g is not above q[%d] = %.17g",
					i, j, pi-pj, i, qi, j, qj)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// The hard exactness test
// -----------------------------------------------------------------------------

// TestSymmetricMarketDevigsToExactlyOneHalf is the strictest assertion in the file
// and the one that uses == deliberately.
//
// A -110/-110 market is symmetric: both sides carry the identical float64 implied
// probability. Every method here is a symmetric function of its inputs, so both
// selections must come out identical bit for bit, and two identical positive values
// renormalised by their own sum give exactly 0.5 — the sum 2q is exact (doubling
// never rounds) and q/(2q) is a correctly rounded division whose true quotient is
// representable.
//
// The chain is worth stating because it is what makes the test fair rather than
// lucky. Multiplicative computes p/(2p) directly. Additive computes p - (2p-1)/2,
// where 2p-1 is exact by Sterbenz's lemma and p - 0.5 is exact by the same, so the
// result is exactly 0.5 before renormalisation even runs. Power and Shin both go
// through a root solve whose output is *not* exact — but both then apply the same
// symmetric transform to two identical inputs, so the two outputs are bit-identical
// whatever the solver returned, and the renormalisation converts that identity into
// exactly 0.5. Approximate equality here would hide a broken symmetry; exact
// equality is the property.
func TestSymmetricMarketDevigsToExactlyOneHalf(t *testing.T) {
	implied := balancedTwoWay.implied(t)
	if implied[0] != implied[1] {
		t.Fatalf("fixture is not symmetric: %v vs %v", implied[0], implied[1])
	}

	for _, method := range DevigMethods() {
		t.Run(method.String(), func(t *testing.T) {
			res := mustDevig(t, method, implied)
			for i, q := range res.Probabilities {
				if q != Probability(0.5) {
					t.Errorf("q[%d] = %.17g, want exactly 0.5 (off by %.3g)", i, float64(q), float64(q)-0.5)
				}
			}
		})
	}
}

// TestSymmetricTwoWayShinInsiderShareHasAClosedForm checks the solver against
// algebra rather than against another implementation.
//
// For a symmetric two-way market the Shin equation collapses. With p on both sides,
// S = 2p and c = 4p²/S = 2p = S, so u(z) = sqrt(z² + S(1-z)) and the n = 2
// constraint u = 1 becomes
//
//	z² - S·z + (S - 1) = 0  ⟺  (z - 1)(z - (S-1)) = 0
//
// The two roots are the degenerate z = 1 and the answer z = S - 1. For -110/-110,
// S = 220/210, so z is exactly 1/21. Recovering that number is evidence about both
// the equation and the leftmost-root bracketing, since the wrong root is also
// sitting there in plain sight.
func TestSymmetricTwoWayShinInsiderShareHasAClosedForm(t *testing.T) {
	implied := balancedTwoWay.implied(t)
	res := mustDevig(t, MethodShin, implied)

	want := res.Overround - 1
	assertClose(t, "shin z for a symmetric two-way market", res.Parameter, want, relTolChain)

	const oneTwentyFirst = 1.0 / 21.0
	assertClose(t, "shin z for -110/-110", res.Parameter, oneTwentyFirst, relTolChain)

	if res.Parameter <= 0 || res.Parameter >= 1 {
		t.Errorf("z = %g, outside the admissible [0, 1)", res.Parameter)
	}
	t.Logf("z = %.17g, closed form S-1 = %.17g, 1/21 = %.17g", res.Parameter, want, oneTwentyFirst)
}

// -----------------------------------------------------------------------------
// Cross-implementation check on Shin
// -----------------------------------------------------------------------------

// TestShinAgreesWithThePublishedReferenceImplementation is the single most
// important test of the formula itself.
//
// Shin's method circulates in several algebraically equivalent but typographically
// different forms, and a wrong transcription produces numbers that look entirely
// plausible. The only way to rule that out is to reproduce a worked example
// computed by somebody else's code from somebody else's reading of the papers.
//
// The example is the one published in the README of the Python `shin` package
// (github.com/mberk/shin), which implements the Jullien & Salanié (1994) iterative
// procedure — a completely different numerical scheme from the bracketed root
// solve here. Input odds 2.6, 2.4, 4.3 give:
//
//	probabilities  0.37299406033208965, 0.4047794109200184, 0.2222265287474275
//	z              0.01694251276407055
//
// The same formulation appears in the CRAN `implied` package's shin_func. Two
// independent implementations, three independent numerical schemes, one set of
// numbers.
func TestShinAgreesWithThePublishedReferenceImplementation(t *testing.T) {
	prices := []Decimal{2.6, 2.4, 4.3}
	want := []float64{0.37299406033208965, 0.4047794109200184, 0.2222265287474275}
	const wantZ = 0.01694251276407055

	res, err := DevigPrices(MethodShin, prices)
	if err != nil {
		t.Fatalf("DevigPrices(shin): %v", err)
	}

	worst := 0.0
	for i, q := range res.Probabilities {
		diff := math.Abs(float64(q) - want[i])
		if diff > worst {
			worst = diff
		}
		if diff > devigReferenceTolerance {
			t.Errorf("q[%d] = %.17g, reference %.17g (|diff| = %.3g, tolerance %.3g)",
				i, float64(q), want[i], diff, devigReferenceTolerance)
		}
	}
	if d := math.Abs(res.Parameter - wantZ); d > devigReferenceTolerance {
		t.Errorf("z = %.17g, reference %.17g (|diff| = %.3g, tolerance %.3g)",
			res.Parameter, wantZ, d, devigReferenceTolerance)
	}
	t.Logf("worst probability disagreement with the reference implementation: %.3g", worst)
	t.Logf("z = %.17g against the reference's %.17g (|diff| = %.3g)",
		res.Parameter, wantZ, math.Abs(res.Parameter-wantZ))
}

// -----------------------------------------------------------------------------
// Shin's equation, checked against its own definition
// -----------------------------------------------------------------------------

// TestShinSolutionSatisfiesTheModelEquations closes the loop that a cross-check
// against another implementation cannot: it takes the z and the q this package
// produced and substitutes them back into Shin's model, independently of the code
// path that computed them.
//
// Two identities are checked. First the constraint the solver was asked to satisfy,
//
//	Σ sqrt(z² + 4(1-z)p_i²/S) = 2 + (n-2)z
//
// which is where the general form differs from the two-way special case that is
// widely quoted as "= 2". Second the inverse relation obtained by squaring the
// definition of q_i,
//
//	p_i² = S·[ (1-z)q_i² + z·q_i ]
//
// which reconstructs each *input* price from the fair probability and the insider
// share. Getting every p_i back is a far stronger statement than the sum
// constraint, which n-1 wrong answers could also satisfy.
func TestShinSolutionSatisfiesTheModelEquations(t *testing.T) {
	for _, mk := range allMarkets() {
		t.Run(mk.name, func(t *testing.T) {
			implied := mk.implied(t)
			res := mustDevig(t, MethodShin, implied)
			z, s := res.Parameter, res.Overround
			n := float64(len(implied))

			total := 0.0
			for _, p := range implied {
				f := float64(p)
				total += math.Sqrt(z*z + 4*(1-z)*f*f/s)
			}
			assertClose(t, "Σ u_i(z)", total, 2+(n-2)*z, relTolChain)

			for i, p := range implied {
				f := float64(p)
				q := float64(res.Probabilities[i])
				reconstructed := s * ((1-z)*q*q + z*q)
				assertClose(t, fmt.Sprintf("p[%d]² reconstructed from q and z", i), reconstructed, f*f, relTolChain)
			}
		})
	}
}

// TestShinStableFormMatchesThePublishedForm asserts that the algebraic
// rearrangement the implementation actually evaluates,
//
//	q_i = 2p_i² / (S(u_i + z))
//
// agrees with the form as published,
//
//	q_i = (u_i - z) / (2(1-z))
//
// on markets where the published form is still numerically well conditioned. The
// rearrangement exists because the published one cancels catastrophically and
// divides by zero as z → 1; this test is what stops that optimisation from silently
// being a different formula.
func TestShinStableFormMatchesThePublishedForm(t *testing.T) {
	for _, mk := range allMarkets() {
		t.Run(mk.name, func(t *testing.T) {
			implied := mk.implied(t)
			res := mustDevig(t, MethodShin, implied)
			z, s := res.Parameter, res.Overround

			// The published form is unnormalised, so compare after normalising it the
			// same way the implementation normalises its own output.
			published := make([]float64, len(implied))
			sum := 0.0
			for i, p := range implied {
				f := float64(p)
				u := math.Sqrt(z*z + 4*(1-z)*f*f/s)
				published[i] = (u - z) / (2 * (1 - z))
				sum += published[i]
			}
			for i := range published {
				assertClose(t, fmt.Sprintf("q[%d] published form", i),
					float64(res.Probabilities[i]), published[i]/sum, relTolChain)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Method-specific identities
// -----------------------------------------------------------------------------

// TestMultiplicativePreservesRatiosExactly pins the defining property of the
// method: dividing every probability by the same number cannot change any ratio
// between them.
func TestMultiplicativePreservesRatiosExactly(t *testing.T) {
	for _, mk := range allMarkets() {
		t.Run(mk.name, func(t *testing.T) {
			implied := mk.implied(t)
			res := mustDevig(t, MethodMultiplicative, implied)
			for i := range implied {
				for j := range implied {
					want := float64(implied[i]) / float64(implied[j])
					got := float64(res.Probabilities[i]) / float64(res.Probabilities[j])
					assertClose(t, fmt.Sprintf("q[%d]/q[%d]", i, j), got, want, relTolExact)
				}
			}
		})
	}
}

// TestAdditivePreservesDifferencesExactly pins the mirror-image property: a flat
// subtraction cannot change any difference between two probabilities.
func TestAdditivePreservesDifferencesExactly(t *testing.T) {
	for _, mk := range []market{balancedTwoWay, unbalancedTwoWay, threeWayWithLongshot} {
		t.Run(mk.name, func(t *testing.T) {
			implied := mk.implied(t)
			res := mustDevig(t, MethodAdditive, implied)
			for i := range implied {
				for j := range implied {
					want := float64(implied[i]) - float64(implied[j])
					got := float64(res.Probabilities[i]) - float64(res.Probabilities[j])
					assertClose(t, fmt.Sprintf("q[%d]-q[%d]", i, j), got, want, relTolChain)
				}
			}
			// The reported parameter is the per-selection deduction.
			assertClose(t, "additive share", res.Parameter,
				(res.Overround-1)/float64(len(implied)), relTolChain)
		})
	}
}

// TestPowerOutputIsTheImpliedProbabilityRaisedToTheReportedExponent asserts the
// reported parameter really is the exponent that produced the numbers, and that the
// exponent solves the equation it is supposed to solve.
func TestPowerOutputIsTheImpliedProbabilityRaisedToTheReportedExponent(t *testing.T) {
	for _, mk := range allMarkets() {
		t.Run(mk.name, func(t *testing.T) {
			implied := mk.implied(t)
			res := mustDevig(t, MethodPower, implied)
			k := res.Parameter

			// Σ p^k = 1 is the defining equation.
			total := 0.0
			for _, p := range implied {
				total += math.Pow(float64(p), k)
			}
			assertClose(t, "Σ p^k", total, 1, relTolChain)

			// And each output is p^k, up to the renormalisation scale.
			for i, p := range implied {
				assertClose(t, fmt.Sprintf("q[%d]", i),
					float64(res.Probabilities[i]), math.Pow(float64(p), k)/total, relTolChain)
			}

			// A book with margin needs an exponent above 1 to shrink the sum onto 1.
			if res.Overround > 1 && k <= 1 {
				t.Errorf("overround %g needs k > 1, got k = %g", res.Overround, k)
			}
			t.Logf("k = %.15g", k)
		})
	}
}

// TestShinAndAdditiveCoincideOnTwoWayMarkets checks an algebraic identity rather
// than a numerical coincidence, and it is worth stating because it explains a
// result that otherwise looks like a bug: on a two-way market two of the four
// methods return byte-identical answers.
//
// Proof. Squaring the definition of q_i gives p_i² = S[(1-z)q_i² + z·q_i]. Take the
// difference of the two equations, using q_1 + q_2 = 1 so that q_1² - q_2² =
// q_1 - q_2:
//
//	p_1² - p_2² = S[(1-z)(q_1-q_2) + z(q_1-q_2)] = S(q_1 - q_2)
//
// while the left side factors as (p_1-p_2)(p_1+p_2) = S(p_1 - p_2). So
// q_1 - q_2 = p_1 - p_2 for *any* z — which, combined with q_1 + q_2 = 1, pins
// q_i = p_i - (S-1)/2, exactly the additive answer. The sum of the two equations
// then determines the z that realises it. Shin's model only earns its keep from
// three selections upwards.
//
// The identity is confirmed independently: "For two-outcome markets, Shin's method
// is equivalent to the Additive Method" — Outlier, "How to Devig Odds".
func TestShinAndAdditiveCoincideOnTwoWayMarkets(t *testing.T) {
	twoWay := []market{
		balancedTwoWay,
		unbalancedTwoWay,
		{name: "-200/+170", american: []int64{-200, 170}},
		{name: "-105/-105 reduced juice", american: []int64{-105, -105}},
		{name: "-900/+600 heavy favourite", american: []int64{-900, 600}},
		{name: "+100/-120", american: []int64{100, -120}},
	}
	for _, mk := range twoWay {
		t.Run(mk.name, func(t *testing.T) {
			implied := mk.implied(t)
			shin := mustDevig(t, MethodShin, implied)
			additive := mustDevig(t, MethodAdditive, implied)

			diff, err := shin.MaxAbsDiff(additive)
			if err != nil {
				t.Fatalf("MaxAbsDiff: %v", err)
			}
			if diff > relTolChain {
				t.Errorf("shin and additive differ by %.3g on a two-way market, want agreement to %.3g",
					diff, relTolChain)
			}
			t.Logf("S = %.8f, z = %.12g, worst disagreement %.3g", shin.Overround, shin.Parameter, diff)
		})
	}
}

// TestShinAndAdditiveDivergeOnThreeWayMarkets is the necessary companion to the
// test above: it shows the two-way agreement is a property of n = 2 and not a sign
// that Shin has quietly been implemented as additive.
func TestShinAndAdditiveDivergeOnThreeWayMarkets(t *testing.T) {
	implied := threeWayWithLongshot.implied(t)
	shin := mustDevig(t, MethodShin, implied)
	additive := mustDevig(t, MethodAdditive, implied)

	diff, err := shin.MaxAbsDiff(additive)
	if err != nil {
		t.Fatalf("MaxAbsDiff: %v", err)
	}
	// The two differ by ~4.6e-3 in probability on this board. 1e-4 is a threshold
	// that a genuine three-way divergence clears by more than an order of magnitude,
	// while an accidental "shin == additive" implementation would fail it by twelve.
	const minimumDivergence = 1e-4
	if diff < minimumDivergence {
		t.Errorf("shin and additive differ by only %.3g on a three-way market: shin may not be implemented at all",
			diff)
	}
	t.Logf("three-way divergence between shin and additive: %.3g", diff)
}

// -----------------------------------------------------------------------------
// The acceptance criterion: agreement on a balanced two-way, divergence on a longshot
// -----------------------------------------------------------------------------

// TestMethodsAgreeOnABalancedTwoWayMarket establishes the baseline half of the
// claim. On an ordinary two-way moneyline the choice of method barely matters, and
// a +EV finder that switched methods here would see almost nothing move.
func TestMethodsAgreeOnABalancedTwoWayMarket(t *testing.T) {
	implied := unbalancedTwoWay.implied(t)
	results := devigAllOrFail(t, implied)

	worst := worstPairwiseDivergence(t, results)
	// Measured spread is ~3.5e-3 in probability, i.e. a third of a percentage point.
	// 6e-3 is a ceiling that the real value clears comfortably while still failing
	// if any method started producing longshot-scale disagreement on a coin flip.
	const maxAgreementSpread = 6e-3
	if worst > maxAgreementSpread {
		t.Errorf("methods disagree by %.3g on a balanced two-way market, want at most %.3g",
			worst, maxAgreementSpread)
	}
	t.Log("\n" + renderComparison(t, "-140/+120 two-way moneyline", implied, results))
	t.Logf("worst pairwise disagreement: %.4g", worst)
}

// TestMethodsDivergeOnALongshot is the phase brief's acceptance criterion, stated
// as an assertion rather than as prose.
//
// The market is a three-way board whose third selection is priced at +2500. All
// four methods run; the table shows what each of them thinks that longshot is
// really worth, both as a probability and as the fair American price a bettor would
// need to beat.
//
// The threshold is expressed as a *relative* spread on the longshot, because that
// is the number that matters commercially: an absolute disagreement of 0.014 is
// negligible on a coin flip and enormous on a 3.7% shot. The measured relative
// spread here is about 39%, meaning the fair price of the same longshot ranges from
// roughly +2630 to +4400 depending only on which method was chosen. 25% is asserted:
// well below the observed value, and far above anything that could arise from four
// correct implementations of the *same* model.
func TestMethodsDivergeOnALongshot(t *testing.T) {
	const minimumRelativeSpread = 0.25

	implied := threeWayWithLongshot.implied(t)
	results := devigAllOrFail(t, implied)

	// The longshot is the shortest implied probability in the market.
	longshot := 0
	for i, p := range implied {
		if p < implied[longshot] {
			longshot = i
		}
	}

	lo, hi := math.Inf(1), math.Inf(-1)
	for _, r := range results {
		q := float64(r.Probabilities[longshot])
		lo = math.Min(lo, q)
		hi = math.Max(hi, q)
	}
	relative := (hi - lo) / hi

	t.Log("\n" + renderComparison(t, threeWayWithLongshot.name, implied, results))
	t.Logf("longshot (selection %d, %+d): fair probability ranges over [%.6f, %.6f], "+
		"an absolute spread of %.6f and a relative spread of %.1f%%",
		longshot, threeWayWithLongshot.american[longshot], lo, hi, hi-lo, 100*relative)

	if relative < minimumRelativeSpread {
		t.Errorf("the four methods agree to within %.1f%% on a +2500 longshot; the phase brief requires "+
			"a demonstrated divergence of at least %.0f%%, so at least one method is not doing what it claims",
			100*relative, 100*minimumRelativeSpread)
	}

	// Direction, not just magnitude. Multiplicative is documented as leaving the
	// most probability on a longshot and additive the least, with power and Shin
	// between them; if that ordering broke, two methods would have been swapped.
	byMethod := map[DevigMethod]float64{}
	for _, r := range results {
		byMethod[r.Method] = float64(r.Probabilities[longshot])
	}
	mult, add := byMethod[MethodMultiplicative], byMethod[MethodAdditive]
	pow, shin := byMethod[MethodPower], byMethod[MethodShin]
	if !(mult > pow && pow > shin && shin > add) {
		t.Errorf("longshot fair probabilities are multiplicative %.6f, power %.6f, shin %.6f, additive %.6f; "+
			"the documented ordering multiplicative > power > shin > additive does not hold",
			mult, pow, shin, add)
	}
}

// devigAllOrFail runs every method and fails if any of them cannot price the market.
func devigAllOrFail(t *testing.T, implied []Probability) []DevigResult {
	t.Helper()
	out := make([]DevigResult, 0, 4)
	for _, c := range DevigCompare(implied) {
		if c.Err != nil {
			t.Fatalf("Devig(%s): %v", c.Method, c.Err)
		}
		out = append(out, c.Result)
	}
	return out
}

// worstPairwiseDivergence is the largest absolute probability difference between
// any two methods on any selection.
func worstPairwiseDivergence(t *testing.T, results []DevigResult) float64 {
	t.Helper()
	worst := 0.0
	for i := range results {
		for j := i + 1; j < len(results); j++ {
			d, err := results[i].MaxAbsDiff(results[j])
			if err != nil {
				t.Fatalf("MaxAbsDiff(%s, %s): %v", results[i].Method, results[j].Method, err)
			}
			worst = math.Max(worst, d)
		}
	}
	return worst
}

// renderComparison formats the four methods side by side, in probabilities and in
// fair American prices, so the divergence is legible in the test log rather than
// only assertable.
func renderComparison(t *testing.T, title string, implied []Probability, results []DevigResult) string {
	t.Helper()

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", title)
	fmt.Fprintf(&b, "%-16s", "selection")
	for i := range implied {
		fmt.Fprintf(&b, "  %18s", fmt.Sprintf("#%d", i))
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%-16s", "implied p")
	for _, p := range implied {
		fmt.Fprintf(&b, "  %18.6f", float64(p))
	}
	b.WriteByte('\n')

	for _, r := range results {
		fmt.Fprintf(&b, "%-16s", r.Method.String())
		prices, err := r.Decimals()
		if err != nil {
			t.Fatalf("Decimals(%s): %v", r.Method, err)
		}
		for i, q := range r.Probabilities {
			american, err := prices[i].American()
			if err != nil {
				fmt.Fprintf(&b, "  %18.6f", float64(q))
				continue
			}
			fmt.Fprintf(&b, "  %10.6f %+7d", float64(q), int64(american))
		}
		fmt.Fprintf(&b, "   [param %.10g]\n", r.Parameter)
	}
	return b.String()
}

// -----------------------------------------------------------------------------
// Already-fair markets
// -----------------------------------------------------------------------------

// TestAFairMarketIsReturnedUnchanged asserts that a market carrying no margin comes
// back as it went in, under every method. The fixtures are dyadic on purpose: their
// implied probabilities sum to exactly 1.0 in float64, so "already fair" is a fact
// about the input rather than an approximation, and any change in the output is
// entirely attributable to the method.
func TestAFairMarketIsReturnedUnchanged(t *testing.T) {
	fair := [][]float64{
		{0.5, 0.5},
		{0.25, 0.25, 0.5},
		{0.25, 0.25, 0.25, 0.25},
		{0.5, 0.25, 0.125, 0.0625, 0.0625},
		{0.75, 0.125, 0.0625, 0.0625},
	}

	for _, raw := range fair {
		implied := make([]Probability, len(raw))
		sum := 0.0
		for i, v := range raw {
			p, err := NewProbability(v)
			if err != nil {
				t.Fatalf("NewProbability(%v): %v", v, err)
			}
			implied[i] = p
			sum += v
		}
		if sum != 1 {
			t.Fatalf("fixture %v sums to %.17g, not exactly 1: the test would not be testing what it claims", raw, sum)
		}

		for _, method := range DevigMethods() {
			t.Run(fmt.Sprintf("%v/%s", raw, method), func(t *testing.T) {
				res := mustDevig(t, method, implied)
				for i := range raw {
					assertClose(t, fmt.Sprintf("q[%d]", i), float64(res.Probabilities[i]), raw[i], relTolExact)
				}
				if res.Iterations != 0 {
					t.Errorf("iterations = %d on a market that needed no solve", res.Iterations)
				}
				switch method {
				case MethodPower:
					if res.Parameter != 1 {
						t.Errorf("k = %v on a fair market, want exactly 1", res.Parameter)
					}
				case MethodShin:
					if res.Parameter != 0 {
						t.Errorf("z = %v on a fair market, want exactly 0", res.Parameter)
					}
				case MethodAdditive:
					if res.Parameter != 0 {
						t.Errorf("share = %v on a fair market, want exactly 0", res.Parameter)
					}
				case MethodMultiplicative, MethodUnknown:
					// No parameter to check.
				}
			})
		}
	}
}

// TestDeviggingIsIdempotent asserts that devigging an already-devigged market
// changes nothing. It follows from the fair-market property — the output of any
// method sums to 1, so feeding it back in is feeding in a fair market — and it is
// the invariant a caller is most likely to violate accidentally, by devigging a
// price that some earlier layer already devigged.
func TestDeviggingIsIdempotent(t *testing.T) {
	for _, mk := range allMarkets() {
		for _, method := range DevigMethods() {
			t.Run(mk.name+"/"+method.String(), func(t *testing.T) {
				implied := mk.implied(t)
				once, err := Devig(method, implied)
				if err != nil {
					t.Skipf("method cannot price this market: %v", err)
				}
				twice, err := Devig(method, once.Probabilities)
				if err != nil {
					t.Fatalf("second application: %v", err)
				}
				diff, err := once.MaxAbsDiff(twice)
				if err != nil {
					t.Fatalf("MaxAbsDiff: %v", err)
				}
				if diff > devigSumTolerance {
					t.Errorf("devigging twice moved the answer by %.3g", diff)
				}
			})
		}
	}
}

// -----------------------------------------------------------------------------
// Documented failure modes
// -----------------------------------------------------------------------------

// TestAdditiveRefusesToProduceANonPositiveProbability covers the method's known
// unsoundness. On a wide board the flat per-selection deduction exceeds the
// shortest price, and the honest response is an error: a clamped zero would become
// an infinite fair price and then a spectacular fake edge in the +EV finder.
func TestAdditiveRefusesToProduceANonPositiveProbability(t *testing.T) {
	implied := futuresBoard.implied(t)
	s := futuresBoard.overround(t)
	share := (s - 1) / float64(len(implied))

	shortest := float64(implied[0])
	for _, p := range implied {
		shortest = math.Min(shortest, float64(p))
	}
	if shortest >= share {
		t.Fatalf("fixture no longer triggers the failure: shortest implied %g, per-selection deduction %g",
			shortest, share)
	}
	t.Logf("overround %g over %d selections deducts %g from each, against a shortest implied probability of %g",
		s, len(implied), share, shortest)

	_, err := Devig(MethodAdditive, implied)
	if !errors.Is(err, ErrDevigAdditiveNonPositive) {
		t.Fatalf("err = %v, want ErrDevigAdditiveNonPositive", err)
	}

	// The other three must be unaffected: one method's unsoundness is not the
	// market's.
	for _, method := range []DevigMethod{MethodMultiplicative, MethodPower, MethodShin} {
		if _, err := Devig(method, implied); err != nil {
			t.Errorf("Devig(%s) on the same market: %v", method, err)
		}
	}

	// And DevigCompare must report the failure per method rather than collapsing.
	failures := 0
	for _, c := range DevigCompare(implied) {
		if c.Err != nil {
			failures++
			if c.Method != MethodAdditive {
				t.Errorf("unexpected failure for %s: %v", c.Method, c.Err)
			}
		}
	}
	if failures != 1 {
		t.Errorf("DevigCompare reported %d failures, want exactly 1", failures)
	}
}

// TestShinRefusesAMarketWithNoMargin covers the other model-level failure. Shin
// explains the margin as insider risk, so a book summing to less than 1 has nothing
// for the model to explain and there is no admissible z in [0, 1).
//
// The other three methods are pure normalisations and remain perfectly well defined
// on such a market, which is asserted here so the asymmetry is a documented
// decision rather than an accident.
func TestShinRefusesAMarketWithNoMargin(t *testing.T) {
	// A two-way market where both sides are priced generously enough that the
	// implied probabilities sum below 1 — a book offering an arbitrage against
	// itself. Rare, but reachable from a stale line.
	implied := []Probability{0.48, 0.48}

	if _, err := Devig(MethodShin, implied); !errors.Is(err, ErrDevigNoShinSolution) {
		t.Errorf("shin: err = %v, want ErrDevigNoShinSolution", err)
	}

	for _, method := range []DevigMethod{MethodMultiplicative, MethodAdditive, MethodPower} {
		t.Run(method.String(), func(t *testing.T) {
			res := mustDevig(t, method, implied)
			for i, q := range res.Probabilities {
				assertClose(t, fmt.Sprintf("q[%d]", i), float64(q), 0.5, relTolChain)
			}
			if method == MethodPower && res.Parameter >= 1 {
				t.Errorf("k = %g on a market summing below 1, want k < 1", res.Parameter)
			}
		})
	}
}

// TestPowerHandlesAnExtremeFavourite exercises the analytic upper bracket at the
// edge of the package's own price range. A -1,000,000 American price is decimal
// 1.0001 and implied probability 0.99990001, which pushes the power exponent's
// upper bound to about 1.4e4 — the regime where a fixed bracket would fail and a
// doubling search would take longest.
func TestPowerHandlesAnExtremeFavourite(t *testing.T) {
	implied := market{name: "extreme favourite", american: []int64{-1_000_000, 100_000}}.implied(t)

	res := mustDevig(t, MethodPower, implied)
	total := 0.0
	for _, p := range implied {
		total += math.Pow(float64(p), res.Parameter)
	}
	assertClose(t, "Σ p^k", total, 1, relTolChain)
	t.Logf("k = %.15g over %d iterations", res.Parameter, res.Iterations)
}

// -----------------------------------------------------------------------------
// Input validation
// -----------------------------------------------------------------------------

// TestDevigRejectsBadInput sweeps every way a caller can hand in something that is
// not a market. The contract is that each returns an error naming the reason;
// nothing panics, and nothing comes back as NaN.
func TestDevigRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		implied []Probability
		want    error
	}{
		{"nil", nil, ErrDevigTooFewSelections},
		{"empty", []Probability{}, ErrDevigTooFewSelections},
		{"single selection", []Probability{0.5}, ErrDevigTooFewSelections},
		{"NaN", []Probability{Probability(math.NaN()), 0.5}, ErrNotFinite},
		{"positive infinity", []Probability{Probability(math.Inf(1)), 0.5}, ErrNotFinite},
		{"negative infinity", []Probability{Probability(math.Inf(-1)), 0.5}, ErrNotFinite},
		{"negative probability", []Probability{-0.1, 0.6}, ErrProbabilityOutOfRange},
		{"probability above 1", []Probability{1.5, 0.6}, ErrProbabilityOutOfRange},
		{"exactly zero", []Probability{0, 0.6}, ErrProbabilityNotPriceable},
		{"exactly one", []Probability{1, 0.6}, ErrProbabilityNotPriceable},
	}

	for _, c := range cases {
		for _, method := range DevigMethods() {
			t.Run(c.name+"/"+method.String(), func(t *testing.T) {
				res, err := Devig(method, c.implied)
				if !errors.Is(err, c.want) {
					t.Fatalf("err = %v, want %v", err, c.want)
				}
				if res.Probabilities != nil {
					t.Errorf("returned %v alongside the error, want the zero value", res.Probabilities)
				}
			})
		}
	}
}

// TestDevigRejectsAnOversizedMarket covers the sanity bound on selection count.
func TestDevigRejectsAnOversizedMarket(t *testing.T) {
	implied := make([]Probability, maxDevigSelections+1)
	for i := range implied {
		implied[i] = 0.5
	}
	if _, err := Devig(MethodMultiplicative, implied); !errors.Is(err, ErrDevigTooFewSelections) {
		t.Errorf("err = %v, want ErrDevigTooFewSelections", err)
	}
}

// TestDevigDoesNotMutateItsInput asserts the purity requirement. A method that
// devigged in place would corrupt the caller's market and, worse, would make a
// second call return something different from the first.
func TestDevigDoesNotMutateItsInput(t *testing.T) {
	for _, method := range DevigMethods() {
		t.Run(method.String(), func(t *testing.T) {
			implied := threeWayWithLongshot.implied(t)
			before := make([]Probability, len(implied))
			copy(before, implied)

			res := mustDevig(t, method, implied)

			for i := range implied {
				if implied[i] != before[i] {
					t.Errorf("input[%d] changed from %v to %v", i, before[i], implied[i])
				}
			}
			// The result must not alias the input either, or a later write through
			// one would silently change the other.
			if len(res.Probabilities) > 0 && &res.Probabilities[0] == &implied[0] {
				t.Error("result aliases the input slice")
			}
		})
	}
}

// TestDevigIsDeterministic asserts bit-for-bit reproducibility across calls. It is
// what makes a devigged price cacheable and a golden-file test meaningful, and it
// would fail immediately if any method read a clock, a map iteration order, or
// package-level mutable state.
func TestDevigIsDeterministic(t *testing.T) {
	for _, mk := range allMarkets() {
		for _, method := range DevigMethods() {
			implied := mk.implied(t)
			first, err := Devig(method, implied)
			if err != nil {
				continue
			}
			for run := 0; run < 8; run++ {
				again, err := Devig(method, implied)
				if err != nil {
					t.Fatalf("%s/%s: run %d failed after run 0 succeeded: %v", mk.name, method, run, err)
				}
				for i := range first.Probabilities {
					if again.Probabilities[i] != first.Probabilities[i] {
						t.Fatalf("%s/%s: q[%d] = %.17g on run %d, %.17g on run 0",
							mk.name, method, i, float64(again.Probabilities[i]), run, float64(first.Probabilities[i]))
					}
				}
				if again.Parameter != first.Parameter || again.Iterations != first.Iterations {
					t.Fatalf("%s/%s: parameter/iterations differ between runs", mk.name, method)
				}
			}
		}
	}
}

// -----------------------------------------------------------------------------
// API surface
// -----------------------------------------------------------------------------

// TestDevigMethodStringAndValid covers the enum's naming and its deliberately
// invalid zero value.
func TestDevigMethodStringAndValid(t *testing.T) {
	cases := []struct {
		m     DevigMethod
		name  string
		valid bool
	}{
		{MethodUnknown, "unknown", false},
		{MethodMultiplicative, "multiplicative", true},
		{MethodAdditive, "additive", true},
		{MethodPower, "power", true},
		{MethodShin, "shin", true},
		{DevigMethod(99), "unknown", false},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.name {
			t.Errorf("DevigMethod(%d).String() = %q, want %q", uint8(c.m), got, c.name)
		}
		if got := c.m.Valid(); got != c.valid {
			t.Errorf("DevigMethod(%d).Valid() = %v, want %v", uint8(c.m), got, c.valid)
		}
	}
	if (DevigMethod(0)) != MethodUnknown {
		t.Error("the zero value is not MethodUnknown")
	}
}

// TestParseDevigMethod covers the canonical names, the aliases, and the rejections.
func TestParseDevigMethod(t *testing.T) {
	ok := map[string]DevigMethod{
		"multiplicative": MethodMultiplicative,
		"MULTIPLICATIVE": MethodMultiplicative,
		"  proportional": MethodMultiplicative,
		"basic":          MethodMultiplicative,
		"additive":       MethodAdditive,
		"Balanced":       MethodAdditive,
		"power":          MethodPower,
		"logarithmic":    MethodPower,
		"shin  ":         MethodShin,
		"Shin":           MethodShin,
	}
	for in, want := range ok {
		got, err := ParseDevigMethod(in)
		if err != nil {
			t.Errorf("ParseDevigMethod(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseDevigMethod(%q) = %v, want %v", in, got, want)
		}
	}

	for _, bad := range []string{"", "  ", "unknown", "vig", "shins", "multiplicativ", "0"} {
		got, err := ParseDevigMethod(bad)
		if !errors.Is(err, ErrUnknownDevigMethod) {
			t.Errorf("ParseDevigMethod(%q): err = %v, want ErrUnknownDevigMethod", bad, err)
		}
		if got != MethodUnknown {
			t.Errorf("ParseDevigMethod(%q) = %v, want MethodUnknown", bad, got)
		}
	}
}

// TestDevigMethodTextMarshalling covers the encoding.TextMarshaler pair, including
// the rule that an invalid method fails to marshal rather than emitting "unknown"
// and shipping a half-initialised value to a client.
func TestDevigMethodTextMarshalling(t *testing.T) {
	for _, m := range DevigMethods() {
		b, err := m.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", m, err)
		}
		var back DevigMethod
		if err := back.UnmarshalText(b); err != nil {
			t.Fatalf("UnmarshalText(%q): %v", b, err)
		}
		if back != m {
			t.Errorf("round trip: %v -> %q -> %v", m, b, back)
		}
	}

	for _, m := range []DevigMethod{MethodUnknown, DevigMethod(200)} {
		if _, err := m.MarshalText(); !errors.Is(err, ErrUnknownDevigMethod) {
			t.Errorf("MarshalText(%d): err = %v, want ErrUnknownDevigMethod", uint8(m), err)
		}
	}
	var target DevigMethod
	if err := target.UnmarshalText([]byte("nonsense")); !errors.Is(err, ErrUnknownDevigMethod) {
		t.Errorf("UnmarshalText: err = %v, want ErrUnknownDevigMethod", err)
	}
}

// TestDevigMethodsReturnsAFreshSlice asserts the canonical order cannot be
// reordered for everybody by one caller sorting it in place.
func TestDevigMethodsReturnsAFreshSlice(t *testing.T) {
	first := DevigMethods()
	want := []DevigMethod{MethodMultiplicative, MethodAdditive, MethodPower, MethodShin}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("DevigMethods()[%d] = %v, want %v", i, first[i], want[i])
		}
	}
	first[0] = MethodShin
	if DevigMethods()[0] != MethodMultiplicative {
		t.Error("mutating the returned slice changed the canonical order")
	}
}

// TestDevigRejectsAnUnknownMethod covers the dispatch's own validation.
func TestDevigRejectsAnUnknownMethod(t *testing.T) {
	implied := unbalancedTwoWay.implied(t)
	for _, m := range []DevigMethod{MethodUnknown, DevigMethod(42)} {
		if _, err := Devig(m, implied); !errors.Is(err, ErrUnknownDevigMethod) {
			t.Errorf("Devig(%d): err = %v, want ErrUnknownDevigMethod", uint8(m), err)
		}
		if _, err := DevigPrices(m, []Decimal{2, 2}); !errors.Is(err, ErrUnknownDevigMethod) {
			t.Errorf("DevigPrices(%d): err = %v, want ErrUnknownDevigMethod", uint8(m), err)
		}
	}
}

// TestDevigPricesMatchesDevig asserts the decimal-price entry point is a pure
// convenience wrapper and not a second, subtly different code path.
func TestDevigPricesMatchesDevig(t *testing.T) {
	implied := threeWayWithLongshot.implied(t)
	prices := make([]Decimal, len(implied))
	for i, p := range implied {
		d, err := p.Decimal()
		if err != nil {
			t.Fatalf("Probability.Decimal: %v", err)
		}
		prices[i] = d
	}

	for _, method := range DevigMethods() {
		fromProb := mustDevig(t, method, implied)
		fromPrice, err := DevigPrices(method, prices)
		if err != nil {
			t.Fatalf("DevigPrices(%s): %v", method, err)
		}
		diff, err := fromProb.MaxAbsDiff(fromPrice)
		if err != nil {
			t.Fatalf("MaxAbsDiff: %v", err)
		}
		// Not bit-identical: the price path makes an extra round trip through
		// p -> d -> p, two divisions that each round. relTolChain covers it.
		if diff > relTolChain {
			t.Errorf("%s: probability and price entry points differ by %.3g", method, diff)
		}
	}
}

// TestDevigPricesRejectsInvalidPrices covers the conversion failure path.
func TestDevigPricesRejectsInvalidPrices(t *testing.T) {
	for _, bad := range [][]Decimal{
		{1.0, 2.0},
		{2.0, 0.5},
		{Decimal(math.NaN()), 2.0},
		{Decimal(math.Inf(1)), 2.0},
	} {
		if _, err := DevigPrices(MethodMultiplicative, bad); err == nil {
			t.Errorf("DevigPrices(%v) = nil error, want a rejection", bad)
		}
	}
}

// TestDevigResultVigIsTheHoldNotTheOverround pins the distinction the package
// documentation calls a classic trap. For -110/-110 the overround is 4.762% and the
// hold is 4.545%; reporting one under the other's name misstates a book's margin by
// a relative 5%.
func TestDevigResultVigIsTheHoldNotTheOverround(t *testing.T) {
	res := mustDevig(t, MethodMultiplicative, balancedTwoWay.implied(t))

	// Overround: two sides at 110/210 each, so S = 220/210 = 22/21.
	assertClose(t, "overround", res.Overround, 22.0/21.0, relTolExact)
	// Hold: (S-1)/S = 1 - 21/22 = 1/22 = 0.0454545…
	hold, err := res.Vig()
	if err != nil {
		t.Fatalf("Vig: %v", err)
	}
	assertClose(t, "hold", hold, 1.0/22.0, relTolExact)

	if math.Abs(hold-(res.Overround-1)) < 1e-4 {
		t.Error("hold and overround are indistinguishable; one of them is computed wrongly")
	}

	// One definition of the hold in the package. DevigResult.Vig delegates to the
	// Margin constructor rather than carrying a second copy of the formula, so the
	// two must agree bit for bit; a tolerance here would let a second implementation
	// reappear.
	margin, err := MarginFromSum(len(res.Probabilities), res.Overround)
	if err != nil {
		t.Fatalf("MarginFromSum: %v", err)
	}
	if hold != margin.Vig {
		t.Errorf("DevigResult.Vig() = %.17g but Margin.Vig = %.17g; the package holds two formulas",
			hold, margin.Vig)
	}
}

// TestDevigResultVigReportsFailureInsteadOfNaN pins the contract in doc.go — nothing
// in this package silently returns NaN — at the one place it used to be broken.
//
// The zero DevigResult is not a hypothetical: DevigCompare pairs exactly that value
// with every method that could not price the market, so a caller fanning its output
// into a dashboard or a JSON body had a NaN reaching the wire with nothing marking
// it as absent. Every degenerate Overround must now be reportable.
func TestDevigResultVigReportsFailureInsteadOfNaN(t *testing.T) {
	priced := mustDevig(t, MethodMultiplicative, balancedTwoWay.implied(t))

	cases := []struct {
		name   string
		result DevigResult
		want   error
	}{
		{"zero value", DevigResult{}, ErrTooFewSelections},
		{"one selection", DevigResult{Probabilities: priced.Probabilities[:1], Overround: 1.05}, ErrTooFewSelections},
		{"zero overround", DevigResult{Probabilities: priced.Probabilities, Overround: 0}, ErrImpliedSumNotPositive},
		{"negative overround", DevigResult{Probabilities: priced.Probabilities, Overround: -1}, ErrImpliedSumNotPositive},
		{"NaN overround", DevigResult{Probabilities: priced.Probabilities, Overround: math.NaN()}, ErrNotFinite},
		{"infinite overround", DevigResult{Probabilities: priced.Probabilities, Overround: math.Inf(1)}, ErrNotFinite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.result.Vig()
			if !errors.Is(err, c.want) {
				t.Fatalf("Vig() error = %v, want %v", err, c.want)
			}
			if got != 0 {
				t.Errorf("Vig() returned %v alongside an error; it must return the zero value", got)
			}
			if math.IsNaN(got) {
				t.Error("Vig() returned NaN")
			}
		})
	}

	// And the specific path the audit found: every failed entry of DevigCompare.
	// 1e-5 against 0.90 makes additive subtract more than the longshot carries.
	comparison := DevigCompare(mustProbabilities(t, 0.90, 0.11, 1e-5))
	failures := 0
	for _, entry := range comparison {
		got, err := entry.Result.Vig()
		if entry.Err != nil {
			failures++
			if err == nil {
				t.Errorf("%s failed to price the market but Vig() reported no error", entry.Method)
			}
			if math.IsNaN(got) {
				t.Errorf("%s: Vig() on the zero result returned NaN", entry.Method)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s priced the market but Vig() errored: %v", entry.Method, err)
		}
	}
	if failures == 0 {
		t.Fatal("no method failed on this market, so the path under test was never exercised")
	}
}

// TestDevigResultDecimalsRoundTrip asserts the fair prices really are the
// reciprocals of the fair probabilities.
func TestDevigResultDecimalsRoundTrip(t *testing.T) {
	res := mustDevig(t, MethodShin, threeWayWithLongshot.implied(t))
	prices, err := res.Decimals()
	if err != nil {
		t.Fatalf("Decimals: %v", err)
	}
	if len(prices) != len(res.Probabilities) {
		t.Fatalf("got %d prices for %d probabilities", len(prices), len(res.Probabilities))
	}
	for i, d := range prices {
		back, err := d.Probability()
		if err != nil {
			t.Fatalf("Decimal.Probability: %v", err)
		}
		assertClose(t, fmt.Sprintf("price %d round trip", i), float64(back), float64(res.Probabilities[i]), relTolChain)
	}
}

// TestMaxAbsDiffRejectsMismatchedMarkets covers the comparison's own validation.
func TestMaxAbsDiffRejectsMismatchedMarkets(t *testing.T) {
	two := mustDevig(t, MethodMultiplicative, unbalancedTwoWay.implied(t))
	three := mustDevig(t, MethodMultiplicative, threeWayWithLongshot.implied(t))

	if _, err := two.MaxAbsDiff(three); !errors.Is(err, ErrDevigLengthMismatch) {
		t.Errorf("err = %v, want ErrDevigLengthMismatch", err)
	}
	if _, err := (DevigResult{}).MaxAbsDiff(DevigResult{}); !errors.Is(err, ErrDevigLengthMismatch) {
		t.Errorf("empty results: err = %v, want ErrDevigLengthMismatch", err)
	}
	self, err := two.MaxAbsDiff(two)
	if err != nil {
		t.Fatalf("self comparison: %v", err)
	}
	if self != 0 {
		t.Errorf("a result compared with itself differs by %g, want exactly 0", self)
	}
}

// TestDevigCompareCoversEveryMethodInOrder asserts the comparison helper is
// complete and ordered, since phases 4 and 9 index into it.
func TestDevigCompareCoversEveryMethodInOrder(t *testing.T) {
	got := DevigCompare(threeWayWithLongshot.implied(t))
	want := DevigMethods()
	if len(got) != len(want) {
		t.Fatalf("got %d comparisons, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Method != want[i] {
			t.Errorf("comparison %d is %v, want %v", i, got[i].Method, want[i])
		}
		if got[i].Err != nil {
			t.Errorf("comparison %d (%v): %v", i, got[i].Method, got[i].Err)
		}
		if got[i].Result.Method != want[i] {
			t.Errorf("comparison %d carries a result for %v", i, got[i].Result.Method)
		}
	}

	// On bad input every entry fails, and none of them panics.
	for _, c := range DevigCompare(nil) {
		if c.Err == nil {
			t.Errorf("%v returned no error on a nil market", c.Method)
		}
	}
}

// -----------------------------------------------------------------------------
// Internal guards
// -----------------------------------------------------------------------------
//
// The tests below reach past the public API to exercise guards that the public API
// cannot reach, because the arguments that would trigger them are ruled out by
// validation earlier in the call chain. They are covered rather than deleted: each
// one protects an invariant that is true today because of an argument made
// elsewhere in this file, and an argument is exactly the kind of thing a later
// change invalidates silently.
//
// Six guards remain genuinely unreachable through any input, and are deliberately
// left uncovered rather than deleted to flatter a coverage number. Each is an
// `if err != nil { return }` protecting a condition that a proof elsewhere rules
// out, and every one of those proofs is the kind that a later edit can invalidate
// in silence. The full list, so that the gap is accounted for rather than merely
// noticed:
//
//  1. devigInputs' "sum is not finite" check. At most maxDevigSelections values,
//     each strictly below 1, cannot sum past 1024.
//  2. DevigPower's handling of a powerExponentUpperBound failure. That function only
//     fails for p_max ≥ 1 or p_max = 0, both rejected by devigInputs first. The
//     failure itself is covered directly by TestPowerExponentUpperBoundGuards.
//  3. powerExponentUpperBound's check that the computed bound is finite. Reaching it
//     needs -ln(p_max) to be positive but denormal, and the smallest positive value
//     -ln returns for a double below 1 is about 1.1e-16, leaving the quotient near
//     1e16.
//  4. DevigPower's handling of a NewRootBracket failure. The bracket is
//     [1, 2·ln(n)/(-ln p_max) + 2] for S > 1 and [0, 1] for S < 1, and the sign
//     change at both ends is proved in DevigPower's documentation.
//  5. and 6. The ErrRootNoConvergence paths in DevigPower and DevigShin. Both
//     objectives are proved to have a bracketed root, and 200 iterations is three
//     times what plain bisection needs to exhaust float64 precision on any bracket
//     either method can construct.
//
// Everything else in devig.go and solver.go is executed by this suite.

// TestPowerExponentUpperBoundGuards covers the two rejections in the analytic
// bracket, using inputs the public entry point filters out before they arrive.
func TestPowerExponentUpperBoundGuards(t *testing.T) {
	cases := []struct {
		name string
		p    []float64
	}{
		{"a certainty gives -ln(1) = 0 and an infinite bound", []float64{1, 0.5}},
		{"an impossibility gives -ln(0) = +Inf", []float64{0, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := powerExponentUpperBound(c.p); !errors.Is(err, ErrRootNoBracket) {
				t.Errorf("err = %v, want ErrRootNoBracket", err)
			}
		})
	}

	// And the ordinary path returns a bound that really does drive the sum below 1,
	// which is the whole claim the bracket rests on.
	p := []float64{0.777778, 0.232558, 0.038462}
	hi, err := powerExponentUpperBound(p)
	if err != nil {
		t.Fatalf("powerExponentUpperBound: %v", err)
	}
	if got := powerSum(p, hi); got >= 1 {
		t.Errorf("Σp^%g = %g, want strictly below 1", hi, got)
	}
}

// TestFinishDevigRejectsCorruptedOutput covers the guards that stand between a
// method that has gone wrong and a result that looks fine. They matter because the
// shared renormalisation step would otherwise launder any vector of positive
// numbers into something that sums to 1.
func TestFinishDevigRejectsCorruptedOutput(t *testing.T) {
	cases := []struct {
		name string
		q    []float64
		want error
	}{
		{"NaN", []float64{math.NaN(), 0.5}, ErrNotFinite},
		{"infinity", []float64{math.Inf(1), 0.5}, ErrNotFinite},
		{"zero", []float64{0, 1}, ErrProbabilityNotPriceable},
		{"negative", []float64{-0.25, 1.25}, ErrProbabilityNotPriceable},
		{"sums well above 1", []float64{0.9, 0.9}, ErrDevigNotNormalised},
		{"sums well below 1", []float64{0.1, 0.1}, ErrDevigNotNormalised},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := finishDevig(MethodMultiplicative, c.q, 1.05, 0, 0)
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}

	// The renormalisation guard found by the property test: a market spanning more
	// than 2^52 in price collapses the long side onto exactly 1.
	if _, err := finishDevig(MethodMultiplicative, []float64{1, 5e-17}, 1.0, 0, 0); !errors.Is(err, ErrProbabilityNotPriceable) {
		t.Errorf("degenerate renormalisation: err = %v, want ErrProbabilityNotPriceable", err)
	}

	// And a well-formed input still passes, so the guards are not simply rejecting
	// everything.
	res, err := finishDevig(MethodPower, []float64{0.4, 0.6}, 1.05, 1.2, 7)
	if err != nil {
		t.Fatalf("finishDevig on valid input: %v", err)
	}
	if res.Method != MethodPower || res.Parameter != 1.2 || res.Iterations != 7 {
		t.Errorf("metadata not carried through: %+v", res)
	}
}

// TestDevigMultiplicativeAcceptsAWideButRepresentableMarket is the companion to the
// degenerate case above: it fixes where the boundary actually is, so a future
// tightening of the guard would be caught. A 1e-12 implied probability against a
// 0.9 one spans twelve orders of magnitude — far wider than any real board — and
// must still price.
func TestDevigMultiplicativeAcceptsAWideButRepresentableMarket(t *testing.T) {
	res := mustDevig(t, MethodMultiplicative, []Probability{0.9, 1e-12})
	for i, q := range res.Probabilities {
		if !(q > 0 && q < 1) {
			t.Errorf("q[%d] = %.17g", i, float64(q))
		}
	}
}

// TestDecimalsRejectsAnUnpriceableFairProbability covers the conversion failure in
// DevigResult.Decimals. DevigResult has exported fields, so a caller can construct
// one directly and this path is reachable from outside the package.
func TestDecimalsRejectsAnUnpriceableFairProbability(t *testing.T) {
	// A subnormal probability is a legal Probability but 1/p overflows, so it has no
	// decimal price.
	res := DevigResult{
		Method:        MethodMultiplicative,
		Probabilities: []Probability{Probability(1e-320), Probability(0.5)},
		Overround:     1,
	}
	if _, err := res.Decimals(); !errors.Is(err, ErrProbabilityNotPriceable) {
		t.Errorf("err = %v, want ErrProbabilityNotPriceable", err)
	}
}

// TestShinRefusesADegenerateAllInsiderMarket drives the insider share past the
// search ceiling. A two-way market at implied 0.99999999 a side has S ≈ 2, and the
// closed form z = S - 1 puts the root above shinMaxZ — the corner where Shin's
// model says essentially every bettor is informed and stops meaning anything.
//
// The assertion is on ErrDevigNoShinSolution rather than on the root finder's own
// ErrRootNoBracket, because a caller choosing a fallback method is asking a
// modelling question, not a numerical one. Both sentinels are present.
func TestShinRefusesADegenerateAllInsiderMarket(t *testing.T) {
	implied := []Probability{0.99999999, 0.99999999}

	_, err := Devig(MethodShin, implied)
	if !errors.Is(err, ErrDevigNoShinSolution) {
		t.Fatalf("err = %v, want ErrDevigNoShinSolution", err)
	}
	if !errors.Is(err, ErrRootNoBracket) {
		t.Errorf("err = %v, want it to also carry the root finder's ErrRootNoBracket", err)
	}

	// The closed form for a symmetric two-way market puts z above the ceiling, which
	// is what makes this fixture the right one.
	if z := 2*0.99999999 - 1; z <= shinMaxZ {
		t.Errorf("fixture no longer exceeds the ceiling: z = %g, shinMaxZ = %g", z, shinMaxZ)
	}

	// The other three methods are unaffected: this is a Shin-specific degeneracy.
	for _, method := range []DevigMethod{MethodMultiplicative, MethodAdditive, MethodPower} {
		if _, err := Devig(method, implied); err != nil {
			t.Errorf("Devig(%s) on the same market: %v", method, err)
		}
	}
}

// -----------------------------------------------------------------------------
// Property-based tests
// -----------------------------------------------------------------------------

// generateMarket draws a realistic bookmaker's market: a true probability
// distribution over n selections, marked up by a margin.
//
// Constructing it this way rather than drawing n independent probabilities is what
// keeps the generator useful. Independent draws almost never sum to something above
// 1, so the overwhelming majority of cases would be rejected and the properties
// would be exercised on a handful of accidents. Here every draw is a valid market
// with a known overround by construction, and no case is ever discarded.
func generateMarket(t *rapid.T) []Probability {
	n := rapid.IntRange(2, 10).Draw(t, "selections")

	weights := make([]float64, n)
	total := 0.0
	for i := range weights {
		// The floor of 0.05 bounds how lopsided a market can be, which in turn bounds
		// the largest true probability at 1/(1+0.05·(n-1)) ≤ 0.9524. That is what
		// guarantees a positive admissible margin below, so the generator never has
		// to reject a draw.
		weights[i] = rapid.Float64Range(0.05, 1).Draw(t, fmt.Sprintf("weight%d", i))
		total += weights[i]
	}

	trueMax := 0.0
	for i := range weights {
		weights[i] /= total
		trueMax = math.Max(trueMax, weights[i])
	}

	// Cap the margin so that no marked-up probability reaches 1, which would not be
	// a price. maxMargin is at least 0.049 for every draw the generator can make.
	maxMargin := 0.999/trueMax - 1
	margin := rapid.Float64Range(0.001, 0.30).Draw(t, "margin")
	margin = math.Min(margin, maxMargin)

	out := make([]Probability, n)
	for i, w := range weights {
		p, err := NewProbability(w * (1 + margin))
		if err != nil {
			t.Fatalf("generated an invalid probability %v: %v", w*(1+margin), err)
		}
		out[i] = p
	}
	return out
}

// TestPropertyEveryMethodProducesADistribution is the invariant sweep run over
// generated markets rather than the fixed fixtures: sum to 1, strictly inside
// (0, 1), and order preserved.
func TestPropertyEveryMethodProducesADistribution(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		implied := generateMarket(rt)
		for _, method := range DevigMethods() {
			res, err := Devig(method, implied)
			if err != nil {
				// Additive is genuinely undefined on some generated markets, and Shin
				// on some degenerate ones. Both are documented; what must never happen
				// is a result that is wrong rather than absent.
				if errors.Is(err, ErrDevigAdditiveNonPositive) || errors.Is(err, ErrDevigNoShinSolution) {
					continue
				}
				rt.Fatalf("%s on %v: %v", method, implied, err)
			}

			sum := 0.0
			for i, q := range res.Probabilities {
				f := float64(q)
				if !(f > 0 && f < 1) {
					rt.Fatalf("%s: q[%d] = %.17g is outside (0, 1) for market %v", method, i, f, implied)
				}
				sum += f
			}
			if math.Abs(sum-1) > devigSumTolerance {
				rt.Fatalf("%s: Σq = %.17g for market %v", method, sum, implied)
			}

			for i := range implied {
				for j := range implied {
					if float64(implied[i])-float64(implied[j]) > devigStrictOrderGap &&
						!(res.Probabilities[i] > res.Probabilities[j]) {
						rt.Fatalf("%s: order lost between selections %d and %d on market %v", method, i, j, implied)
					}
				}
			}
		}
	})
}

// TestPropertyDeviggingIsPermutationEquivariant asserts that relabelling the
// selections permutes the answer and changes nothing else of substance. It would
// catch any method that depended on the order of its inputs for a structural
// reason — a solver seeded from the first element, or an asymmetric accumulation.
//
// The assertion is up to devigPermutationTolerance rather than bit-exact, because
// summing the overround left to right is genuinely order-dependent in IEEE-754. See
// that constant for why the residual ULP is accepted rather than engineered away.
func TestPropertyDeviggingIsPermutationEquivariant(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		implied := generateMarket(rt)
		n := len(implied)

		// A rotation is a permutation, and one integer describes it completely, which
		// keeps the shrinking output readable when a case fails.
		shift := rapid.IntRange(1, n-1).Draw(rt, "shift")
		rotated := make([]Probability, n)
		for i := range implied {
			rotated[(i+shift)%n] = implied[i]
		}

		for _, method := range DevigMethods() {
			base, baseErr := Devig(method, implied)
			perm, permErr := Devig(method, rotated)

			if (baseErr == nil) != (permErr == nil) {
				rt.Fatalf("%s: permuting the market changed whether it can be priced (%v vs %v)",
					method, baseErr, permErr)
			}
			if baseErr != nil {
				continue
			}
			for i := range base.Probabilities {
				got := float64(perm.Probabilities[(i+shift)%n])
				want := float64(base.Probabilities[i])
				if !closeTo(got, want, devigPermutationTolerance) {
					rt.Fatalf("%s: q[%d] = %.17g but the permuted market gives %.17g (|diff| = %.3g)",
						method, i, want, got, math.Abs(got-want))
				}
			}
			if !closeTo(perm.Parameter, base.Parameter, devigPermutationTolerance) {
				rt.Fatalf("%s: parameter %.17g under permutation, %.17g without (|diff| = %.3g)",
					method, perm.Parameter, base.Parameter, math.Abs(perm.Parameter-base.Parameter))
			}
		}
	})
}

// TestPropertyDeviggingIsIdempotent is the fair-market property restated over
// generated input: the output of any method sums to 1, so devigging it again must
// be a no-op.
func TestPropertyDeviggingIsIdempotent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		implied := generateMarket(rt)
		for _, method := range DevigMethods() {
			once, err := Devig(method, implied)
			if err != nil {
				continue
			}
			twice, err := Devig(method, once.Probabilities)
			if err != nil {
				rt.Fatalf("%s: a devigged market could not be devigged again: %v", method, err)
			}
			diff, err := once.MaxAbsDiff(twice)
			if err != nil {
				rt.Fatalf("MaxAbsDiff: %v", err)
			}
			if diff > devigSumTolerance {
				rt.Fatalf("%s: devigging twice moved the answer by %.3g on market %v", method, diff, implied)
			}
		}
	})
}

// TestPropertyMultiplicativeIsTheRatioPreservingMethod asserts on generated markets
// that only multiplicative leaves ratios alone. Half of the value is the negative
// direction: if the other three ever agreed with it exactly, they would not be
// doing anything.
func TestPropertyMultiplicativeIsTheRatioPreservingMethod(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		implied := generateMarket(rt)
		res, err := Devig(MethodMultiplicative, implied)
		if err != nil {
			rt.Fatalf("multiplicative failed on %v: %v", implied, err)
		}
		for i := range implied {
			for j := range implied {
				want := float64(implied[i]) / float64(implied[j])
				got := float64(res.Probabilities[i]) / float64(res.Probabilities[j])
				if !closeTo(got, want, relTolExact) {
					rt.Fatalf("ratio %d/%d = %.17g, want %.17g", i, j, got, want)
				}
			}
		}
	})
}

// TestPropertyDevigNeverPanicsOnArbitraryInput drives every method with completely
// unconstrained float64, which rapid generates including NaN, ±Inf, subnormals, and
// values far outside [0, 1].
//
// The contract is total: an error or a valid distribution, never a panic and never
// a NaN dressed up as a probability.
func TestPropertyDevigNeverPanicsOnArbitraryInput(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 6).Draw(rt, "selections")
		implied := make([]Probability, n)
		for i := range implied {
			implied[i] = Probability(rapid.Float64().Draw(rt, fmt.Sprintf("p%d", i)))
		}

		for _, method := range append(DevigMethods(), MethodUnknown, DevigMethod(7)) {
			res, err := Devig(method, implied)
			if err != nil {
				if res.Probabilities != nil {
					rt.Fatalf("%s returned probabilities alongside an error", method)
				}
				continue
			}
			sum := 0.0
			for i, q := range res.Probabilities {
				f := float64(q)
				if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f >= 1 {
					rt.Fatalf("%s accepted %v and returned q[%d] = %v", method, implied, i, f)
				}
				sum += f
			}
			if math.Abs(sum-1) > devigSumTolerance {
				rt.Fatalf("%s accepted %v and returned probabilities summing to %.17g", method, implied, sum)
			}
		}
	})
}

// TestPropertyPowerExponentTracksTheOverround asserts the sign relationship that
// makes the power method's bracketing correct: a book with margin needs an exponent
// above 1, and a book summing below 1 needs one below.
func TestPropertyPowerExponentTracksTheOverround(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		implied := generateMarket(rt)
		res, err := Devig(MethodPower, implied)
		if err != nil {
			rt.Fatalf("power failed on %v: %v", implied, err)
		}
		switch {
		case res.Overround > 1+fairOverroundTolerance && res.Parameter <= 1:
			rt.Fatalf("overround %g gave k = %g, want k > 1", res.Overround, res.Parameter)
		case res.Overround < 1-fairOverroundTolerance && res.Parameter >= 1:
			rt.Fatalf("overround %g gave k = %g, want k < 1", res.Overround, res.Parameter)
		}
	})
}
