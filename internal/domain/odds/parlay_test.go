package odds

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// -----------------------------------------------------------------------------
// What the numbers in this file are, and are not
// -----------------------------------------------------------------------------
//
// Prices. Every leg price used below is a standard published price written as its
// exact rational form — 21/11 is -110, the point-spread juice every book posts;
// 5/2 is +150; 3/2 is -200. These are format identities, not quotes attributed to
// any event on any date. Expectations are derived from the rationals rather than
// from decimal literals, so the expected value and the computed value reach the
// same number by different float paths.
//
// Correlations. The coefficients in these tables are TEST INPUTS chosen to
// exercise the arithmetic across its range. They are not empirical estimates and
// no claim is made that any real pair of legs exhibits them. Correlation is an
// input to this package by design (see correlation.go); measuring it from observed
// history is the analytics phase's job, and inventing a number here and calling it
// measured would be exactly the fabrication the project forbids.

const (
	// juice is the decimal form of -110: risk 11 to win 10.
	juice = 21.0 / 11.0
)

// -----------------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------------

func mustDecimals(t *testing.T, values ...float64) []Decimal {
	t.Helper()
	out := make([]Decimal, len(values))
	for i, v := range values {
		d, err := NewDecimal(v)
		if err != nil {
			t.Fatalf("NewDecimal(%v): %v", v, err)
		}
		out[i] = d
	}
	return out
}

func mustProbabilities(t *testing.T, values ...float64) []Probability {
	t.Helper()
	out := make([]Probability, len(values))
	for i, v := range values {
		p, err := NewProbability(v)
		if err != nil {
			t.Fatalf("NewProbability(%v): %v", v, err)
		}
		out[i] = p
	}
	return out
}

func mustIdentity(t *testing.T, n int) CorrelationMatrix {
	t.Helper()
	c, err := IdentityCorrelation(n)
	if err != nil {
		t.Fatalf("IdentityCorrelation(%d): %v", n, err)
	}
	return c
}

func mustJoint(t *testing.T, marginals []Probability, c CorrelationMatrix) float64 {
	t.Helper()
	p, err := GaussianCopulaJoint(marginals, c)
	if err != nil {
		t.Fatalf("GaussianCopulaJoint: %v", err)
	}
	return float64(p)
}

// monteCarloJoint estimates P(every leg wins) under the same Gaussian copula by
// simulation, and returns the estimate together with its standard error.
//
// It is the independent reference the n-leg quadrature is measured against, where
// no closed form exists. The construction is the definition of the model rather
// than a restatement of the implementation: draw z ~ N(0, I), correlate it as L·z
// with L the Cholesky factor of the correlation matrix, and count the draws where
// every component falls below its threshold. It shares no quadrature rule, no
// transformation and no constant with MultivariateNormalCDF.
//
// The generator is seeded, so the test is deterministic: the same seed produces
// the same estimate on every run and on every platform, which is what makes it
// usable as a test oracle rather than a flaky approximation.
func monteCarloJoint(t *testing.T, marginals []Probability, c CorrelationMatrix, samples int, seed uint64) (estimate, stderr float64) {
	t.Helper()

	factor, err := c.Cholesky()
	if err != nil {
		t.Fatalf("Cholesky: %v", err)
	}
	n := len(marginals)
	thresholds := make([]float64, n)
	for i, p := range marginals {
		thresholds[i], err = NormalQuantile(p)
		if err != nil {
			t.Fatalf("NormalQuantile(%v): %v", p, err)
		}
	}

	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	z := make([]float64, n)
	hits := 0
	for range samples {
		for i := range z {
			z[i] = rng.NormFloat64()
		}
		won := true
		for i := range n {
			v := 0.0
			for k := 0; k <= i; k++ {
				v += factor[i][k] * z[k]
			}
			if v > thresholds[i] {
				won = false
				break
			}
		}
		if won {
			hits++
		}
	}

	estimate = float64(hits) / float64(samples)
	return estimate, math.Sqrt(estimate * (1 - estimate) / float64(samples))
}

// -----------------------------------------------------------------------------
// Independent parlays
// -----------------------------------------------------------------------------

// TestIndependentParlayIsTheProductOfItsLegs checks the multiplicative rule at the
// leg counts a bet slip actually carries, using the standard -110 price.
//
// The expectations are exact rationals: two legs of 21/11 pay 441/121, three pay
// 9261/1331, four pay 194481/14641. Those are the true-odds parlay payouts every
// parlay table publishes — 441/121 = 3.6446…, i.e. +264 in American, against the
// 13/5 = +260 a fixed parlay card pays. The gap between the two is the parlay
// card's margin, and computing the true side of it correctly is this function's
// entire job.
func TestIndependentParlayIsTheProductOfItsLegs(t *testing.T) {
	cases := []struct {
		name string
		legs []float64
		want float64
	}{
		{"two -110 legs", []float64{juice, juice}, 441.0 / 121.0},
		{"three -110 legs", []float64{juice, juice, juice}, 9261.0 / 1331.0},
		{"four -110 legs", []float64{juice, juice, juice, juice}, 194481.0 / 14641.0},
		{"one leg is the leg itself", []float64{juice}, juice},
		{"mixed ladder: -110, +150, -200", []float64{juice, 5.0 / 2.0, 3.0 / 2.0}, (21.0 / 11.0) * (5.0 / 2.0) * (3.0 / 2.0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParlayDecimal(mustDecimals(t, c.legs...))
			if err != nil {
				t.Fatalf("ParlayDecimal: %v", err)
			}
			assertClose(t, "ParlayDecimal", float64(got), c.want, relTolExact)
		})
	}
}

// TestTwoLegParlayAtStandardJuiceRendersAsThePublishedPrice pins the headline
// number: two -110 legs is +264 in American odds, and its implied probability is
// (100/210)² — the square of the implied probability of a single -110 leg.
func TestTwoLegParlayAtStandardJuiceRendersAsThePublishedPrice(t *testing.T) {
	price, err := ParlayDecimal(mustDecimals(t, juice, juice))
	if err != nil {
		t.Fatalf("ParlayDecimal: %v", err)
	}

	american, err := price.American()
	if err != nil {
		t.Fatalf("American: %v", err)
	}
	if american != 264 {
		t.Errorf("two -110 legs = %s, want +264", RenderAmerican(american))
	}

	implied, err := price.Probability()
	if err != nil {
		t.Fatalf("Probability: %v", err)
	}
	single := 110.0 / 210.0
	assertClose(t, "implied probability", float64(implied), single*single, relTolExact)
}

func TestParlayDecimalRejectsBadInput(t *testing.T) {
	valid := mustDecimals(t, juice, juice)

	if _, err := ParlayDecimal(nil); !errors.Is(err, ErrNoLegs) {
		t.Errorf("ParlayDecimal(nil) error = %v, want ErrNoLegs", err)
	}

	tooMany := make([]Decimal, MaxParlayLegs+1)
	for i := range tooMany {
		tooMany[i] = valid[0]
	}
	if _, err := ParlayDecimal(tooMany); !errors.Is(err, ErrTooManyLegs) {
		t.Errorf("ParlayDecimal(%d legs) error = %v, want ErrTooManyLegs", len(tooMany), err)
	}

	if _, err := ParlayDecimal([]Decimal{Decimal(1.0)}); !errors.Is(err, ErrDecimalOutOfRange) {
		t.Errorf("ParlayDecimal with a 1.0 leg error = %v, want ErrDecimalOutOfRange", err)
	}
	if _, err := ParlayDecimal([]Decimal{Decimal(math.NaN())}); !errors.Is(err, ErrNotFinite) {
		t.Errorf("ParlayDecimal with a NaN leg error = %v, want ErrNotFinite", err)
	}

	t.Run("a parlay long enough to overflow reports it", func(t *testing.T) {
		huge := mustDecimals(t, 1e300, 1e300)
		if _, err := ParlayDecimal(huge); !errors.Is(err, ErrNotFinite) {
			t.Errorf("ParlayDecimal(overflowing) error = %v, want ErrNotFinite", err)
		}
	})
}

func TestJointProbabilityIndependent(t *testing.T) {
	marginals := mustProbabilities(t, 0.5, 0.4, 0.25)
	got, err := JointProbabilityIndependent(marginals)
	if err != nil {
		t.Fatalf("JointProbabilityIndependent: %v", err)
	}
	assertClose(t, "joint", float64(got), 0.5*0.4*0.25, relTolExact)

	if _, err := JointProbabilityIndependent(nil); !errors.Is(err, ErrNoLegs) {
		t.Errorf("JointProbabilityIndependent(nil) error = %v, want ErrNoLegs", err)
	}
	if _, err := JointProbabilityIndependent([]Probability{Probability(1.5)}); !errors.Is(err, ErrProbabilityOutOfRange) {
		t.Errorf("JointProbabilityIndependent(1.5) error = %v, want ErrProbabilityOutOfRange", err)
	}
}

// -----------------------------------------------------------------------------
// The copula invariants
// -----------------------------------------------------------------------------

// TestZeroCorrelationReproducesTheIndependentProductExactly asserts the invariant
// with the word "exactly" in it, using == rather than a tolerance.
//
// Exactness is not decoration. The independent path is what a cross-game parlay
// takes, and if turning correlation on and setting it to zero moved the price by an
// ULP, every price in the system would depend on which code path produced it and
// two paths that must agree would disagree in the last digit forever. The
// implementation guarantees it by short-circuiting on the identity rather than by
// computing a product of numerically-one brackets.
func TestZeroCorrelationReproducesTheIndependentProductExactly(t *testing.T) {
	for _, legs := range [][]float64{
		{juice, juice},
		{juice, 5.0 / 2.0, 3.0 / 2.0},
		{1.5, 2.75, 4.0, 1.2, 8.5},
	} {
		prices := mustDecimals(t, legs...)
		identity := mustIdentity(t, len(prices))

		independent, err := ParlayDecimal(prices)
		if err != nil {
			t.Fatalf("ParlayDecimal: %v", err)
		}
		correlated, err := CorrelatedParlayDecimal(prices, identity)
		if err != nil {
			t.Fatalf("CorrelatedParlayDecimal: %v", err)
		}
		if correlated != independent {
			t.Errorf("identity correlation moved the price: %.17g vs %.17g", float64(correlated), float64(independent))
		}

		marginals, err := impliedMarginals(prices)
		if err != nil {
			t.Fatalf("impliedMarginals: %v", err)
		}
		product, err := JointProbabilityIndependent(marginals)
		if err != nil {
			t.Fatalf("JointProbabilityIndependent: %v", err)
		}
		copula, err := GaussianCopulaJoint(marginals, identity)
		if err != nil {
			t.Fatalf("GaussianCopulaJoint: %v", err)
		}
		if copula != product {
			t.Errorf("identity correlation moved the probability: %.17g vs %.17g", float64(copula), float64(product))
		}
	}
}

// TestNearZeroCorrelationApproachesTheIndependentProduct checks that the exactness
// above is not achieved by the short circuit alone: a correlation of 1e-10 takes
// the full copula path and still lands on the independent product.
func TestNearZeroCorrelationApproachesTheIndependentProduct(t *testing.T) {
	marginals := mustProbabilities(t, 0.55, 0.48, 0.61)
	c := mustCorrelationMatrix(t, equicorrelated(3, 1e-10))
	if c.IsIdentity() {
		t.Fatal("a matrix with a 1e-10 off-diagonal must not report itself as the identity")
	}

	product := 0.55 * 0.48 * 0.61
	assertClose(t, "joint", mustJoint(t, marginals, c), product, 1e-9)
}

// TestOneLegParlayEqualsThatLeg checks the degenerate case at both the probability
// and the price level, with and without a correlation matrix.
func TestOneLegParlayEqualsThatLeg(t *testing.T) {
	marginals := mustProbabilities(t, 0.4237)
	identity := mustIdentity(t, 1)

	if got := mustJoint(t, marginals, identity); got != 0.4237 {
		t.Errorf("one-leg joint = %.17g, want exactly 0.4237", got)
	}

	prices := mustDecimals(t, 2.36)
	price, err := CorrelatedParlayDecimal(prices, identity)
	if err != nil {
		t.Fatalf("CorrelatedParlayDecimal: %v", err)
	}
	if price != prices[0] {
		t.Errorf("one-leg parlay price = %.17g, want exactly %.17g", float64(price), float64(prices[0]))
	}
}

// TestTwoLegCopulaIsTheBivariateNormal asserts the exactness claim at two legs: the
// joint probability is Φ₂(Φ⁻¹(p₁), Φ⁻¹(p₂); ρ) and nothing else.
func TestTwoLegCopulaIsTheBivariateNormal(t *testing.T) {
	for _, rho := range []float64{-0.95, -0.4, -0.05, 0.05, 0.4, 0.95} {
		for _, pair := range [][2]float64{{0.5, 0.5}, {0.2, 0.8}, {0.05, 0.95}, {0.9, 0.9}} {
			marginals := mustProbabilities(t, pair[0], pair[1])
			c := mustCorrelationMatrix(t, equicorrelated(2, rho))

			t1, err := NormalQuantile(marginals[0])
			if err != nil {
				t.Fatalf("NormalQuantile: %v", err)
			}
			t2, err := NormalQuantile(marginals[1])
			if err != nil {
				t.Fatalf("NormalQuantile: %v", err)
			}
			want := mustBivariate(t, t1, t2, rho)

			// Not bit-equality: boundedJoint projects a result that overshoots the
			// Fréchet-Hoeffding bound back onto it, which at ρ = 0.95 with a 0.05 leg
			// moves the raw Φ₂ by about 7e-17. That projection is deliberate — the
			// bound is a hard mathematical ceiling and Φ₂ is only accurate to ~1e-15
			// — so the assertion is to the accuracy Φ₂ actually promises.
			got := mustJoint(t, marginals, c)
			if !closeTo(got, want, tolBivariate) {
				t.Errorf("copula joint at ρ=%g, p=(%g,%g) = %.17g, want the bivariate normal %.17g",
					rho, pair[0], pair[1], got, want)
			}
			if ceiling := math.Min(pair[0], pair[1]); got > ceiling {
				t.Errorf("copula joint %.17g exceeds the Fréchet ceiling %.17g", got, ceiling)
			}
		}
	}
}

// TestPositiveCorrelationRaisesTheJointAndShortensThePrice is the product-level
// invariant this whole file exists for. Legs that move together win together more
// often than independence predicts, so the joint probability rises and the price
// must fall; legs that move against each other do the reverse.
//
// Getting the sign wrong here would not crash anything. It would quietly quote
// same-game parlays above their true price and report the difference as an edge.
func TestPositiveCorrelationRaisesTheJointAndShortensThePrice(t *testing.T) {
	for _, legCount := range []int{2, 3, 4, 5} {
		values := make([]float64, legCount)
		for i := range values {
			values[i] = juice
		}
		prices := mustDecimals(t, values...)
		marginals, err := impliedMarginals(prices)
		if err != nil {
			t.Fatalf("impliedMarginals: %v", err)
		}

		independentJoint := mustJoint(t, marginals, mustIdentity(t, legCount))
		independentPrice, err := ParlayDecimal(prices)
		if err != nil {
			t.Fatalf("ParlayDecimal: %v", err)
		}

		for _, rho := range []float64{0.05, 0.1, 0.2, 0.3} {
			positive := mustCorrelationMatrix(t, equicorrelated(legCount, rho))
			joint := mustJoint(t, marginals, positive)
			if joint <= independentJoint {
				t.Errorf("%d legs at ρ=+%g: joint %.17g did not rise above the independent %.17g",
					legCount, rho, joint, independentJoint)
			}
			price, err := CorrelatedParlayDecimal(prices, positive)
			if err != nil {
				t.Fatalf("CorrelatedParlayDecimal: %v", err)
			}
			if price >= independentPrice {
				t.Errorf("%d legs at ρ=+%g: price %.17g did not fall below the independent %.17g",
					legCount, rho, float64(price), float64(independentPrice))
			}

			// The equicorrelated family stays positive semi-definite only down to
			// ρ = -1/(n-1). The negative side is tested at 80% of that, which keeps
			// the matrix comfortably inside the region rather than on its edge; the
			// edge itself is a documented hard case for the quadrature and is
			// exercised by TestNearSingularCorrelationIsReportedNotGuessed.
			negativeRho := math.Max(-rho, -0.8/float64(legCount-1))
			negative := mustCorrelationMatrix(t, equicorrelated(legCount, negativeRho))
			joint = mustJoint(t, marginals, negative)
			if joint >= independentJoint {
				t.Errorf("%d legs at ρ=%g: joint %.17g did not fall below the independent %.17g",
					legCount, negativeRho, joint, independentJoint)
			}
			price, err = CorrelatedParlayDecimal(prices, negative)
			if err != nil {
				t.Fatalf("CorrelatedParlayDecimal: %v", err)
			}
			if price <= independentPrice {
				t.Errorf("%d legs at ρ=%g: price %.17g did not rise above the independent %.17g",
					legCount, negativeRho, float64(price), float64(independentPrice))
			}
		}
	}
}

// TestJointProbabilityStaysInTheUnitInterval sweeps leg counts, marginals and
// correlations and asserts the result is always a probability, and always inside
// the Fréchet-Hoeffding interval — never a number outside [0, 1], never a NaN.
//
// Zero is a legitimate answer at the extreme negative end: with ρ = -0.99 between
// two 5% legs the two are very nearly mutually exclusive and the joint underflows.
// The test therefore asserts strict positivity only at correlations where a
// non-trivial answer is expected.
func TestJointProbabilityStaysInTheUnitInterval(t *testing.T) {
	for _, legCount := range []int{2, 3, 4, 6} {
		for _, p := range []float64{0.05, 0.3, 0.5, 0.75, 0.95} {
			values := make([]float64, legCount)
			for i := range values {
				values[i] = p
			}
			marginals := mustProbabilities(t, values...)

			maxNegative := -0.8 / float64(legCount-1)
			for _, rho := range []float64{maxNegative, -0.05, 0, 0.05, 0.3, 0.6, 0.9} {
				if rho < maxNegative {
					continue
				}
				c := mustCorrelationMatrix(t, equicorrelated(legCount, rho))
				joint, err := GaussianCopulaJoint(marginals, c)
				if err != nil {
					t.Errorf("%d legs at p=%g, ρ=%g: unexpected error %v", legCount, p, rho, err)
					continue
				}
				upper := p
				lower := math.Max(0, float64(legCount)*p-float64(legCount-1))
				if float64(joint) < lower-frechetSlack || float64(joint) > upper+frechetSlack {
					t.Errorf("%d legs at p=%g, ρ=%g: joint %.17g is outside [%g, %g]",
						legCount, p, rho, float64(joint), lower, upper)
				}
				// Strict positivity is only asserted where a non-degenerate answer
				// exists: six 5% legs pushed apart by negative correlation have a
				// joint probability far below the smallest double this quadrature
				// can resolve, and zero is the honest answer there.
				if independent := math.Pow(p, float64(legCount)); independent > 1e-6 && rho >= -0.5 && joint <= 0 {
					t.Errorf("%d legs at p=%g, ρ=%g: joint is zero", legCount, p, rho)
				}
			}
		}
	}
}

// -----------------------------------------------------------------------------
// The accuracy of the n-leg approximation, measured rather than asserted
// -----------------------------------------------------------------------------

// TestCopulaAccuracyAgainstSeededMonteCarlo measures the n-leg quadrature's error
// against a seeded simulation of the same model, and logs the measured numbers so
// the accuracy claim in the documentation is checkable rather than asserted.
//
// The tolerance is the sum of a quadrature budget and four Monte Carlo standard
// errors. Four standard errors on a binomial proportion is a two-sided 99.994%
// interval, so the test cannot fail on sampling noise alone, and both the seed and
// the quadrature are deterministic so it cannot fail intermittently at all.
//
// The quadrature budget is 5e-4 in absolute probability. It is far looser than the
// 1e-7 the integration converges to, because at two million samples the reference
// is the imprecise side of the comparison — this test bounds the quadrature, it
// does not measure it. Its real job is catching an error of the size the pairwise
// approximation this implementation replaced was producing: six to twenty-two per
// cent of the joint probability, or 0.006 to 0.036 absolute, which is one to two
// orders of magnitude past this budget.
func TestCopulaAccuracyAgainstSeededMonteCarlo(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-million sample simulation")
	}

	const (
		quadratureBudget = 5e-4
		samples          = 2_000_000
	)

	cases := []struct {
		name      string
		marginals []float64
		rho       float64
		seed      uint64
	}{
		{"three legs, weak positive", []float64{0.5, 0.55, 0.6}, 0.15, 1},
		{"three legs, moderate positive", []float64{0.45, 0.5, 0.65}, 0.3, 2},
		{"three legs, negative", []float64{0.5, 0.5, 0.5}, -0.25, 3},
		{"four legs, weak positive", []float64{0.5, 0.5, 0.55, 0.6}, 0.15, 4},
		{"four legs, moderate positive", []float64{0.4, 0.5, 0.6, 0.7}, 0.3, 5},
		{"five legs, weak positive", []float64{0.5, 0.52, 0.55, 0.58, 0.6}, 0.12, 6},
		{"six legs at even money, weak positive", []float64{0.5, 0.5, 0.5, 0.5, 0.5, 0.5}, 0.1, 7},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			marginals := mustProbabilities(t, c.marginals...)
			matrix := mustCorrelationMatrix(t, equicorrelated(len(marginals), c.rho))

			approximation := mustJoint(t, marginals, matrix)
			reference, stderr := monteCarloJoint(t, marginals, matrix, samples, c.seed)
			independent := mustJoint(t, marginals, mustIdentity(t, len(marginals)))

			difference := approximation - reference
			tolerance := quadratureBudget + 4*stderr
			t.Logf("legs=%d ρ=%+.2f  independent=%.6f  quadrature=%.6f  monte carlo=%.6f ± %.6f  error=%+.6f (budget %.6f)",
				len(marginals), c.rho, independent, approximation, reference, stderr, difference, tolerance)

			if math.Abs(difference) > tolerance {
				t.Errorf("quadrature is off by %.6f, beyond %.6f", difference, tolerance)
			}
			// The correction must also point the right way: the computed value should
			// land on the same side of the independent product as the truth does.
			if (approximation-independent)*(reference-independent) < 0 {
				t.Errorf("the correction has the wrong sign: quadrature %+.6f, truth %+.6f",
					approximation-independent, reference-independent)
			}
		})
	}
}

// TestStrongCorrelationApproachesTheComonotoneLimit checks the regime that broke
// the pairwise approximation this implementation replaced: five even-money legs at
// ρ = 0.9. The pairwise product climbed to 6.78 there — not a probability at all.
// The quadrature must instead approach the comonotone limit from below, since as
// ρ → 1 every leg becomes the same event and the joint tends to the common
// marginal.
func TestStrongCorrelationApproachesTheComonotoneLimit(t *testing.T) {
	marginals := mustProbabilities(t, 0.5, 0.5, 0.5, 0.5, 0.5)

	previous := 0.0
	for _, rho := range []float64{0.2, 0.5, 0.8, 0.9, 0.95, 0.99} {
		matrix := mustCorrelationMatrix(t, equicorrelated(5, rho))
		joint := mustJoint(t, marginals, matrix)
		if joint > 0.5 {
			t.Errorf("ρ=%g: joint %.17g exceeds the comonotone limit 0.5", rho, joint)
		}
		if joint <= previous {
			t.Errorf("ρ=%g: joint %.6f did not rise above the previous %.6f", rho, joint, previous)
		}
		previous = joint
		t.Logf("five even-money legs at ρ=%.2f: joint = %.6f (independent 0.031250, comonotone 0.5)", rho, joint)
	}

	if previous < 0.4 {
		t.Errorf("at ρ=0.99 the joint reached only %.6f, well short of the comonotone limit 0.5", previous)
	}
}

// TestNearSingularCorrelationNowPrices covers the edge of the positive
// semi-definite region.
//
// The equicorrelated family is positive semi-definite only down to ρ = -1/(n-1), and
// at that edge the correlation matrix is singular: every leg is as antagonistic to
// every other as a joint distribution permits. The Cholesky factor's last pivot
// collapses to zero, so the last variable is a deterministic combination of the
// earlier ones and its factor in Genz's integrand becomes a {0, 1} indicator rather
// than a normal CDF. A lattice rule is fast on a smooth integrand and slow on a
// discontinuous one.
//
// # What this test used to assert, and why it changed
//
// It used to assert that this case CANNOT be priced — that the quadrature returns
// ErrOrthantNotConverged — on the reasoning that no real slate produces a matrix on
// this edge, so the cost of refusing was nil. The property suite falsified the
// premise: it drew three legs at 0.125, 0.2998 and 0.9556 with ρ = -0.5, which is
// this same edge at n = 3 and is an entirely ordinary same-game shape, and the
// system refused to quote it. The batch cap was raised (see orthantMaxBatches) until
// the stopping rule could be met. The tolerance was NOT loosened, so the accuracy
// demanded of a delivered price is unchanged.
//
// The honesty property the old assertion protected — refusing rather than returning
// whatever estimate the loop happened to hold — is not lost with it. It is pinned
// directly and unconditionally on latticeEstimate itself, against a tolerance no
// budget can meet, in TestLatticeEstimateReportsNonConvergence.
func TestNearSingularCorrelationNowPrices(t *testing.T) {
	const legs = 5
	marginals := mustProbabilities(t, 0.524, 0.524, 0.524, 0.524, 0.524)
	edge := mustCorrelationMatrix(t, equicorrelated(legs, -0.99/float64(legs-1)))

	joint, err := GaussianCopulaJoint(marginals, edge)
	if err != nil {
		t.Fatalf("on the positive semi-definite edge: %v", err)
	}

	// Checked against the Fréchet-Hoeffding interval, which bounds every joint
	// probability under every dependence structure and needs no quadrature: with
	// Σp = 2.62 over five legs the lower bound max(0, Σp − (n−1)) is 0 and the upper
	// bound min p is 0.524. Maximal mutual antagonism puts the truth near the floor.
	if joint <= 0 || float64(joint) > 0.524 {
		t.Fatalf("joint = %.17g, outside the Fréchet interval (0, 0.524]", float64(joint))
	}
	// And well below the independent product 0.524⁵ = 0.0395, because negative
	// correlation makes all five winning together rarer than independence implies.
	independent := math.Pow(0.524, legs)
	if float64(joint) >= independent {
		t.Errorf("joint %.8g is not below the independent product %.8g at ρ = %.4f",
			float64(joint), independent, -0.99/float64(legs-1))
	}
	t.Logf("five -110 legs at ρ = %.4f: joint = %.8f (independent %.8f)",
		-0.99/float64(legs-1), float64(joint), independent)

	t.Run("stepping back inside the region prices normally too", func(t *testing.T) {
		inside := mustCorrelationMatrix(t, equicorrelated(legs, -0.8/float64(legs-1)))
		looser, err := GaussianCopulaJoint(marginals, inside)
		if err != nil {
			t.Fatalf("at 80%% of the edge: %v", err)
		}
		// Slepian: less negative correlation cannot lower the joint.
		if float64(looser) < float64(joint) {
			t.Errorf("relaxing ρ from %.4f to %.4f lowered the joint from %.8g to %.8g",
				-0.99/float64(legs-1), -0.8/float64(legs-1), float64(joint), float64(looser))
		}
		t.Logf("five -110 legs at ρ = %.4f: joint = %.8f", -0.8/float64(legs-1), float64(looser))
	})
}

// TestBoundedJointRejectsNonFiniteInput reaches the defensive branch in
// boundedJoint that the public API cannot: the log-space accumulation is bounded
// well away from overflow for every input the validators admit, so a NaN or an
// infinity can only arrive from a future change. The branch is exercised directly.
func TestBoundedJointRejectsNonFiniteInput(t *testing.T) {
	marginals := mustProbabilities(t, 0.5, 0.5, 0.5)
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := boundedJoint(bad, marginals); !errors.Is(err, ErrParlayNotPriceable) {
			t.Errorf("boundedJoint(%v) error = %v, want ErrParlayNotPriceable", bad, err)
		}
	}
}

// -----------------------------------------------------------------------------
// Degenerate marginals
// -----------------------------------------------------------------------------

// TestDegenerateMarginals checks the two exact-equality special cases: an
// impossible leg makes the parlay impossible, and a certain leg drops out.
func TestDegenerateMarginals(t *testing.T) {
	c := mustCorrelationMatrix(t, equicorrelated(3, 0.3))

	t.Run("an impossible leg makes the parlay impossible", func(t *testing.T) {
		marginals := []Probability{Probability(0.5), Probability(0), Probability(0.7)}
		if got := mustJoint(t, marginals, c); got != 0 {
			t.Errorf("joint = %.17g, want exactly 0", got)
		}
	})

	t.Run("a certain leg drops out", func(t *testing.T) {
		pair := mustProbabilities(t, 0.5, 0.7)
		withCertain := []Probability{pair[0], Probability(1), pair[1]}

		twoLeg := mustCorrelationMatrix(t, [][]float64{{1, 0.3}, {0.3, 1}})
		want := mustJoint(t, pair, twoLeg)
		got := mustJoint(t, withCertain, c)
		assertClose(t, "joint with a certain leg", got, want, relTolExact)
	})

	t.Run("every leg certain is a certainty", func(t *testing.T) {
		all := []Probability{Probability(1), Probability(1), Probability(1)}
		if got := mustJoint(t, all, c); got != 1 {
			t.Errorf("joint = %.17g, want exactly 1", got)
		}
	})

	t.Run("one uncertain leg among certainties is that leg", func(t *testing.T) {
		mixed := []Probability{Probability(1), Probability(0.37), Probability(1)}
		if got := mustJoint(t, mixed, c); got != 0.37 {
			t.Errorf("joint = %.17g, want exactly 0.37", got)
		}
	})

	t.Run("two uncorrelated survivors multiply exactly", func(t *testing.T) {
		// Legs 0 and 2 are uncorrelated with each other, leg 1 is certain and drops
		// out, so the answer must be the exact product rather than Φ₂ at ρ = 0.
		matrix := mustCorrelationMatrix(t, [][]float64{
			{1, 0.4, 0},
			{0.4, 1, 0.4},
			{0, 0.4, 1},
		})
		marginals := []Probability{Probability(0.5), Probability(1), Probability(0.25)}
		if got := mustJoint(t, marginals, matrix); got != 0.125 {
			t.Errorf("joint = %.17g, want exactly 0.125", got)
		}
	})

	t.Run("a mutually exclusive pair makes the parlay impossible", func(t *testing.T) {
		// ρ = -1 between legs 0 and 1 with p₀ = p₁ = 0.5 means exactly one of them
		// wins, so no parlay containing both can.
		matrix := mustCorrelationMatrix(t, [][]float64{
			{1, -1, 0},
			{-1, 1, 0},
			{0, 0, 1},
		})
		marginals := mustProbabilities(t, 0.5, 0.5, 0.6)
		if got := mustJoint(t, marginals, matrix); got != 0 {
			t.Errorf("joint = %.17g, want exactly 0", got)
		}
	})
}

func TestCorrelatedParlayRejectsBadInput(t *testing.T) {
	prices := mustDecimals(t, juice, juice, juice)

	t.Run("an unconstructed matrix", func(t *testing.T) {
		var zero CorrelationMatrix
		if _, err := CorrelatedParlayDecimal(prices, zero); !errors.Is(err, ErrCorrelationShape) {
			t.Errorf("error = %v, want ErrCorrelationShape", err)
		}
	})

	t.Run("dimension mismatch", func(t *testing.T) {
		if _, err := CorrelatedParlayDecimal(prices, mustIdentity(t, 2)); !errors.Is(err, ErrLegCountMismatch) {
			t.Errorf("error = %v, want ErrLegCountMismatch", err)
		}
		marginals, err := impliedMarginals(prices)
		if err != nil {
			t.Fatalf("impliedMarginals: %v", err)
		}
		if _, err := GaussianCopulaJoint(marginals, mustIdentity(t, 4)); !errors.Is(err, ErrLegCountMismatch) {
			t.Errorf("error = %v, want ErrLegCountMismatch", err)
		}
	})

	t.Run("no legs", func(t *testing.T) {
		if _, err := CorrelatedParlayDecimal(nil, mustIdentity(t, 1)); !errors.Is(err, ErrNoLegs) {
			t.Errorf("error = %v, want ErrNoLegs", err)
		}
		if _, err := GaussianCopulaJoint(nil, mustIdentity(t, 1)); !errors.Is(err, ErrNoLegs) {
			t.Errorf("error = %v, want ErrNoLegs", err)
		}
	})

	t.Run("too many legs", func(t *testing.T) {
		many := make([]Probability, MaxParlayLegs+1)
		for i := range many {
			many[i] = Probability(0.5)
		}
		if _, err := GaussianCopulaJoint(many, mustIdentity(t, 2)); !errors.Is(err, ErrTooManyLegs) {
			t.Errorf("error = %v, want ErrTooManyLegs", err)
		}
	})

	t.Run("an impossible joint has no price", func(t *testing.T) {
		matrix := mustCorrelationMatrix(t, [][]float64{{1, -1}, {-1, 1}})
		if _, err := CorrelatedParlayDecimal(mustDecimals(t, 2, 2), matrix); !errors.Is(err, ErrProbabilityNotPriceable) {
			t.Errorf("error = %v, want ErrProbabilityNotPriceable", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Quotes
// -----------------------------------------------------------------------------

func TestQuoteParlay(t *testing.T) {
	prices := mustDecimals(t, juice, juice, juice)
	correlated := mustCorrelationMatrix(t, equicorrelated(3, 0.25))

	quote, err := QuoteParlay(prices, correlated)
	if err != nil {
		t.Fatalf("QuoteParlay: %v", err)
	}
	if quote.Legs != 3 {
		t.Errorf("Legs = %d, want 3", quote.Legs)
	}
	if quote.Exact {
		t.Error("a three-leg correlated quote must not report itself as exact")
	}
	if quote.CorrelatedDecimal >= quote.IndependentDecimal {
		t.Errorf("positive correlation did not shorten the price: %.17g vs %.17g",
			float64(quote.CorrelatedDecimal), float64(quote.IndependentDecimal))
	}
	if quote.CorrelatedProbability <= quote.IndependentProbability {
		t.Errorf("positive correlation did not raise the probability: %.17g vs %.17g",
			float64(quote.CorrelatedProbability), float64(quote.IndependentProbability))
	}
	if haircut := quote.CorrelationHaircut(); haircut <= 0 || haircut >= 1 {
		t.Errorf("CorrelationHaircut = %v, want a fraction in (0, 1)", haircut)
	}
	assertClose(t, "independent price is the reciprocal of the independent probability",
		float64(quote.IndependentDecimal), 1/float64(quote.IndependentProbability), relTolChain)

	t.Run("negative correlation lengthens the price and reports a negative haircut", func(t *testing.T) {
		negative := mustCorrelationMatrix(t, equicorrelated(3, -0.25))
		q, err := QuoteParlay(prices, negative)
		if err != nil {
			t.Fatalf("QuoteParlay: %v", err)
		}
		if q.CorrelationHaircut() >= 0 {
			t.Errorf("CorrelationHaircut = %v, want a negative fraction", q.CorrelationHaircut())
		}
	})

	t.Run("two legs are exact", func(t *testing.T) {
		q, err := QuoteParlay(mustDecimals(t, juice, juice), mustCorrelationMatrix(t, equicorrelated(2, 0.25)))
		if err != nil {
			t.Fatalf("QuoteParlay: %v", err)
		}
		if !q.Exact {
			t.Error("a two-leg quote must report itself as exact")
		}
	})

	t.Run("the zero quote has a zero haircut rather than a division by zero", func(t *testing.T) {
		var q ParlayQuote
		if got := q.CorrelationHaircut(); got != 0 {
			t.Errorf("CorrelationHaircut on the zero value = %v, want 0", got)
		}
	})

	t.Run("errors propagate", func(t *testing.T) {
		if _, err := QuoteParlay(nil, mustIdentity(t, 1)); !errors.Is(err, ErrNoLegs) {
			t.Errorf("error = %v, want ErrNoLegs", err)
		}
		if _, err := QuoteParlay(prices, mustIdentity(t, 2)); !errors.Is(err, ErrLegCountMismatch) {
			t.Errorf("error = %v, want ErrLegCountMismatch", err)
		}
		exclusive := mustCorrelationMatrix(t, [][]float64{{1, -1}, {-1, 1}})
		if _, err := QuoteParlay(mustDecimals(t, 2, 2), exclusive); !errors.Is(err, ErrProbabilityNotPriceable) {
			t.Errorf("error = %v, want ErrProbabilityNotPriceable", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Round robins
// -----------------------------------------------------------------------------

func TestCombinations(t *testing.T) {
	got, err := Combinations(4, 2)
	if err != nil {
		t.Fatalf("Combinations(4, 2): %v", err)
	}
	want := [][]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}}
	if len(got) != len(want) {
		t.Fatalf("Combinations(4, 2) returned %d combinations, want %d", len(got), len(want))
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("combination %d = %v, want %v (lexicographic order)", i, got[i], want[i])
		}
	}

	t.Run("each combination is its own slice", func(t *testing.T) {
		all, err := Combinations(5, 3)
		if err != nil {
			t.Fatalf("Combinations(5, 3): %v", err)
		}
		all[0][0] = 99
		if all[1][0] == 99 {
			t.Error("combinations alias one another")
		}
	})

	t.Run("counts match the binomial coefficient", func(t *testing.T) {
		for n := 1; n <= 12; n++ {
			for k := 1; k <= n; k++ {
				all, err := Combinations(n, k)
				if err != nil {
					t.Fatalf("Combinations(%d, %d): %v", n, k, err)
				}
				want, err := binomial(n, k)
				if err != nil {
					t.Fatalf("binomial(%d, %d): %v", n, k, err)
				}
				if len(all) != want {
					t.Errorf("Combinations(%d, %d) returned %d, want C(%d,%d) = %d", n, k, len(all), n, k, want)
				}
				for _, combination := range all {
					if !slices.IsSorted(combination) {
						t.Errorf("combination %v is not ascending", combination)
					}
				}
			}
		}
	})

	errorCases := []struct {
		name string
		n, k int
		want error
	}{
		{"no items", 0, 1, ErrNoLegs},
		{"beyond the leg bound", MaxParlayLegs + 1, 2, ErrTooManyLegs},
		{"size zero", 4, 0, ErrCombinationSize},
		{"size beyond the leg count", 4, 5, ErrCombinationSize},
		{"too many combinations", 25, 12, ErrTooManyCombinations},
	}
	for _, c := range errorCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Combinations(c.n, c.k); !errors.Is(err, c.want) {
				t.Errorf("Combinations(%d, %d) error = %v, want %v", c.n, c.k, err, c.want)
			}
		})
	}
}

func TestBinomial(t *testing.T) {
	cases := []struct {
		n, k, want int
	}{
		{1, 0, 1}, {1, 1, 1},
		{5, 2, 10}, {5, 3, 10},
		{10, 5, 252},
		{12, 6, 924},
		{20, 3, 1140},
	}
	for _, c := range cases {
		got, err := binomial(c.n, c.k)
		if err != nil {
			t.Fatalf("binomial(%d, %d): %v", c.n, c.k, err)
		}
		if got != c.want {
			t.Errorf("binomial(%d, %d) = %d, want %d", c.n, c.k, got, c.want)
		}
	}
	if _, err := binomial(3, 4); !errors.Is(err, ErrCombinationSize) {
		t.Errorf("binomial(3, 4) error = %v, want ErrCombinationSize", err)
	}
	if _, err := binomial(3, -1); !errors.Is(err, ErrCombinationSize) {
		t.Errorf("binomial(3, -1) error = %v, want ErrCombinationSize", err)
	}
}

// TestRoundRobin checks the combinatorial expansion, the per-ticket correlation
// restriction, and the ordering contract.
func TestRoundRobin(t *testing.T) {
	prices := mustDecimals(t, juice, 5.0/2.0, 3.0/2.0, 2.0)
	matrix := mustCorrelationMatrix(t, [][]float64{
		{1, 0.2, 0, 0},
		{0.2, 1, 0, 0},
		{0, 0, 1, 0.1},
		{0, 0, 0.1, 1},
	})

	tickets, err := RoundRobin(prices, []int{2, 3}, matrix)
	if err != nil {
		t.Fatalf("RoundRobin: %v", err)
	}
	wantCount, err := RoundRobinTicketCount(4, []int{2, 3})
	if err != nil {
		t.Fatalf("RoundRobinTicketCount: %v", err)
	}
	if wantCount != 10 { // C(4,2) + C(4,3) = 6 + 4
		t.Fatalf("RoundRobinTicketCount(4, [2 3]) = %d, want 10", wantCount)
	}
	if len(tickets) != wantCount {
		t.Fatalf("RoundRobin produced %d tickets, want %d", len(tickets), wantCount)
	}

	t.Run("sizes come back in the order requested", func(t *testing.T) {
		for i, ticket := range tickets {
			wantSize := 2
			if i >= 6 {
				wantSize = 3
			}
			if len(ticket.Legs) != wantSize {
				t.Errorf("ticket %d covers %d legs, want %d", i, len(ticket.Legs), wantSize)
			}
		}
	})

	t.Run("each ticket is priced with its own correlation block", func(t *testing.T) {
		for _, ticket := range tickets {
			subLegs := make([]Decimal, len(ticket.Legs))
			for i, idx := range ticket.Legs {
				subLegs[i] = prices[idx]
			}
			sub, err := matrix.Submatrix(ticket.Legs)
			if err != nil {
				t.Fatalf("Submatrix: %v", err)
			}
			want, err := CorrelatedParlayDecimal(subLegs, sub)
			if err != nil {
				t.Fatalf("CorrelatedParlayDecimal: %v", err)
			}
			if ticket.Decimal != want {
				t.Errorf("ticket %v priced at %.17g, want %.17g", ticket.Legs, float64(ticket.Decimal), float64(want))
			}
		}
	})

	t.Run("the correlated pair prices below its independent product", func(t *testing.T) {
		var pair RoundRobinTicket
		for _, ticket := range tickets {
			if slices.Equal(ticket.Legs, []int{0, 1}) {
				pair = ticket
			}
		}
		independent, err := ParlayDecimal([]Decimal{prices[0], prices[1]})
		if err != nil {
			t.Fatalf("ParlayDecimal: %v", err)
		}
		if pair.Decimal >= independent {
			t.Errorf("the ρ=0.2 pair priced at %.17g, not below the independent %.17g",
				float64(pair.Decimal), float64(independent))
		}
	})

	t.Run("an uncorrelated pair prices exactly at the product", func(t *testing.T) {
		for _, ticket := range tickets {
			if !slices.Equal(ticket.Legs, []int{0, 2}) {
				continue
			}
			independent, err := ParlayDecimal([]Decimal{prices[0], prices[2]})
			if err != nil {
				t.Fatalf("ParlayDecimal: %v", err)
			}
			if ticket.Decimal != independent {
				t.Errorf("the uncorrelated pair priced at %.17g, want exactly %.17g",
					float64(ticket.Decimal), float64(independent))
			}
		}
	})
}

func TestRoundRobinTicketCount(t *testing.T) {
	cases := []struct {
		name  string
		n     int
		sizes []int
		want  int
	}{
		{"five by twos", 5, []int{2}, 10},
		{"five by twos and threes", 5, []int{2, 3}, 20},
		{"four by threes", 4, []int{3}, 4},
		{"three by ones is a set of singles", 3, []int{1}, 3},
		{"n by n is one parlay", 6, []int{6}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RoundRobinTicketCount(c.n, c.sizes)
			if err != nil {
				t.Fatalf("RoundRobinTicketCount: %v", err)
			}
			if got != c.want {
				t.Errorf("RoundRobinTicketCount(%d, %v) = %d, want %d", c.n, c.sizes, got, c.want)
			}
		})
	}

	errorCases := []struct {
		name  string
		n     int
		sizes []int
		want  error
	}{
		{"no legs", 0, []int{2}, ErrNoLegs},
		{"beyond the leg bound", MaxParlayLegs + 1, []int{2}, ErrTooManyLegs},
		{"no sizes", 5, nil, ErrCombinationSize},
		{"size zero", 5, []int{0}, ErrCombinationSize},
		{"size beyond the leg count", 5, []int{6}, ErrCombinationSize},
		{"size repeated", 5, []int{2, 2}, ErrCombinationSize},
		{"too many tickets", 25, []int{12}, ErrTooManyCombinations},
		{"too many tickets in aggregate", 20, []int{8, 9, 10, 11, 12}, ErrTooManyCombinations},
	}
	for _, c := range errorCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := RoundRobinTicketCount(c.n, c.sizes); !errors.Is(err, c.want) {
				t.Errorf("RoundRobinTicketCount(%d, %v) error = %v, want %v", c.n, c.sizes, err, c.want)
			}
		})
	}
}

func TestRoundRobinRejectsBadInput(t *testing.T) {
	prices := mustDecimals(t, juice, juice, juice)

	if _, err := RoundRobin(nil, []int{2}, mustIdentity(t, 3)); !errors.Is(err, ErrNoLegs) {
		t.Errorf("RoundRobin(nil) error = %v, want ErrNoLegs", err)
	}
	if _, err := RoundRobin(prices, []int{2}, mustIdentity(t, 2)); !errors.Is(err, ErrLegCountMismatch) {
		t.Errorf("RoundRobin with a mismatched matrix error = %v, want ErrLegCountMismatch", err)
	}
	if _, err := RoundRobin(prices, []int{4}, mustIdentity(t, 3)); !errors.Is(err, ErrCombinationSize) {
		t.Errorf("RoundRobin with an oversized combination error = %v, want ErrCombinationSize", err)
	}

	t.Run("an unpriceable combination fails loudly", func(t *testing.T) {
		exclusive := mustCorrelationMatrix(t, [][]float64{
			{1, -1, 0},
			{-1, 1, 0},
			{0, 0, 1},
		})
		if _, err := RoundRobin(mustDecimals(t, 2, 2, 2), []int{2}, exclusive); !errors.Is(err, ErrProbabilityNotPriceable) {
			t.Errorf("error = %v, want ErrProbabilityNotPriceable", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Teasers
// -----------------------------------------------------------------------------

func TestTeaserLegValidation(t *testing.T) {
	valid := TeaserLeg{Points: 6, OriginalPrice: Decimal(2.0), TeasedPrice: Decimal(juice)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid teaser leg was rejected: %v", err)
	}

	cases := []struct {
		name string
		leg  TeaserLeg
		want error
	}{
		{"points not a number", TeaserLeg{Points: math.NaN(), OriginalPrice: 2, TeasedPrice: 1.8}, ErrNotFinite},
		{"points infinite", TeaserLeg{Points: math.Inf(1), OriginalPrice: 2, TeasedPrice: 1.8}, ErrNotFinite},
		{"points zero", TeaserLeg{Points: 0, OriginalPrice: 2, TeasedPrice: 1.8}, ErrTeaserPoints},
		{"points negative", TeaserLeg{Points: -6, OriginalPrice: 2, TeasedPrice: 1.8}, ErrTeaserPoints},
		{"points beyond the bound", TeaserLeg{Points: MaxTeaserPoints + 1, OriginalPrice: 2, TeasedPrice: 1.8}, ErrTeaserPoints},
		{"original price invalid", TeaserLeg{Points: 6, OriginalPrice: 1, TeasedPrice: 1.8}, ErrDecimalOutOfRange},
		{"teased price invalid", TeaserLeg{Points: 6, OriginalPrice: 2, TeasedPrice: 1}, ErrDecimalOutOfRange},
		{"teased price is longer", TeaserLeg{Points: 6, OriginalPrice: 1.8, TeasedPrice: 2.4}, ErrTeaserNotFavourable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.leg.Validate(); !errors.Is(err, c.want) {
				t.Errorf("Validate error = %v, want %v", err, c.want)
			}
		})
	}

	t.Run("an unchanged price is allowed", func(t *testing.T) {
		// A half point off a dead number can round to the same posted price. That is
		// a degenerate but legal teaser leg; only a LONGER teased price is a bug.
		leg := TeaserLeg{Points: 0.5, OriginalPrice: Decimal(1.91), TeasedPrice: Decimal(1.91)}
		if err := leg.Validate(); err != nil {
			t.Errorf("an unchanged teased price was rejected: %v", err)
		}
	})
}

// TestTeaserPricing checks that a teaser is priced as a correlated parlay of its
// caller-supplied teased prices, and that correlation between two legs of one game
// shortens it exactly as it shortens any same-game parlay.
func TestTeaserPricing(t *testing.T) {
	// A two-team teaser, both legs in one game, each teased leg priced at -110.
	legs := []TeaserLeg{
		{Points: 6, OriginalPrice: Decimal(2.5), TeasedPrice: Decimal(juice)},
		{Points: 6, OriginalPrice: Decimal(2.4), TeasedPrice: Decimal(juice)},
	}

	independent, err := TeaserDecimal(legs, mustIdentity(t, 2))
	if err != nil {
		t.Fatalf("TeaserDecimal: %v", err)
	}
	assertClose(t, "independent teaser price", float64(independent), juice*juice, relTolExact)

	correlated, err := TeaserDecimal(legs, mustCorrelationMatrix(t, equicorrelated(2, 0.35)))
	if err != nil {
		t.Fatalf("TeaserDecimal: %v", err)
	}
	if correlated >= independent {
		t.Errorf("a positively correlated teaser priced at %.17g, not below the independent %.17g",
			float64(correlated), float64(independent))
	}

	probability, err := TeaserProbability(legs, mustIdentity(t, 2))
	if err != nil {
		t.Fatalf("TeaserProbability: %v", err)
	}
	assertClose(t, "independent teaser probability", float64(probability), (11.0/21.0)*(11.0/21.0), relTolExact)

	t.Run("errors propagate", func(t *testing.T) {
		if _, err := TeaserDecimal(nil, mustIdentity(t, 1)); !errors.Is(err, ErrNoLegs) {
			t.Errorf("error = %v, want ErrNoLegs", err)
		}
		if _, err := TeaserProbability(nil, mustIdentity(t, 1)); !errors.Is(err, ErrNoLegs) {
			t.Errorf("error = %v, want ErrNoLegs", err)
		}

		tooMany := make([]TeaserLeg, MaxParlayLegs+1)
		for i := range tooMany {
			tooMany[i] = legs[0]
		}
		if _, err := TeaserDecimal(tooMany, mustIdentity(t, 2)); !errors.Is(err, ErrTooManyLegs) {
			t.Errorf("error = %v, want ErrTooManyLegs", err)
		}

		bad := []TeaserLeg{legs[0], {Points: 6, OriginalPrice: Decimal(1.8), TeasedPrice: Decimal(2.4)}}
		if _, err := TeaserDecimal(bad, mustIdentity(t, 2)); !errors.Is(err, ErrTeaserNotFavourable) {
			t.Errorf("error = %v, want ErrTeaserNotFavourable", err)
		}
		if _, err := TeaserProbability(bad, mustIdentity(t, 2)); !errors.Is(err, ErrTeaserNotFavourable) {
			t.Errorf("error = %v, want ErrTeaserNotFavourable", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Property-based tests
// -----------------------------------------------------------------------------

// TestPropertyParlayIsNeverShorterThanItsLongestLeg asserts the structural fact
// that makes a parlay a parlay: adding a leg can only lengthen the price, because
// every leg price is strictly above 1.
func TestPropertyParlayIsNeverShorterThanItsLongestLeg(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(t, "n")
		legs := make([]Decimal, n)
		longest := 0.0
		for i := range legs {
			v := rapid.Float64Range(1.01, 50).Draw(t, "leg")
			legs[i] = Decimal(v)
			longest = math.Max(longest, v)
		}

		price, err := ParlayDecimal(legs)
		if err != nil {
			t.Fatalf("ParlayDecimal: %v", err)
		}
		if float64(price) < longest {
			t.Fatalf("parlay price %.17g is shorter than its longest leg %.17g", float64(price), longest)
		}
	})
}

// TestPropertyCopulaJointRespectsItsBounds asserts on arbitrary inputs that the
// copula either returns a probability inside the Fréchet-Hoeffding interval or
// refuses with ErrParlayNotPriceable — never a number outside it, and never a
// silent NaN.
func TestPropertyCopulaJointRespectsItsBounds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 6).Draw(t, "n")
		marginals := make([]Probability, n)
		for i := range marginals {
			marginals[i] = Probability(rapid.Float64Range(0.01, 0.99).Draw(t, "p"))
		}
		// The equicorrelated family is positive semi-definite exactly on
		// [-1/(n-1), 1], which keeps the draw inside the valid region without
		// rejecting most of it.
		lowest := -1.0
		if n > 1 {
			lowest = -1 / float64(n-1)
		}
		rho := rapid.Float64Range(lowest, 1).Draw(t, "rho")

		c, err := NewCorrelationMatrix(equicorrelated(n, rho))
		if err != nil {
			t.Skipf("correlation matrix rejected at the boundary: %v", err)
		}

		joint, err := GaussianCopulaJoint(marginals, c)
		if err != nil {
			if !errors.Is(err, ErrParlayNotPriceable) {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}

		upper, sum := 1.0, 0.0
		for _, p := range marginals {
			upper = math.Min(upper, float64(p))
			sum += float64(p)
		}
		lower := math.Max(0, sum-float64(n-1))
		if float64(joint) < lower-frechetSlack || float64(joint) > upper+frechetSlack {
			t.Fatalf("joint %.17g is outside [%.17g, %.17g] for %v at ρ=%g", float64(joint), lower, upper, marginals, rho)
		}
	})
}

// TestPropertyCopulaIsPermutationInvariant asserts that relabelling the legs — and
// permuting the correlation matrix to match — cannot change the answer. A parlay
// is a set of legs, not a sequence, and an implementation that depended on the
// order would be wrong in a way no single example would reliably expose.
func TestPropertyCopulaIsPermutationInvariant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 6).Draw(t, "n")

		marginals := make([]Probability, n)
		for i := range marginals {
			marginals[i] = Probability(rapid.Float64Range(0.05, 0.95).Draw(t, "p"))
		}

		// A positive semi-definite matrix by construction: a one-factor structure
		// ρ_ij = λ_i·λ_j with |λ| ≤ 1 is the Gram matrix of unit vectors, and it is
		// the standard shape a single shared driver (game script, pace, weather)
		// induces across legs of one event.
		loadings := make([]float64, n)
		for i := range loadings {
			loadings[i] = rapid.Float64Range(-0.9, 0.9).Draw(t, "loading")
		}
		rows := make([][]float64, n)
		for i := range rows {
			rows[i] = make([]float64, n)
			for j := range rows[i] {
				if i == j {
					rows[i][j] = 1
				} else {
					rows[i][j] = loadings[i] * loadings[j]
				}
			}
		}
		c, err := NewCorrelationMatrix(rows)
		if err != nil {
			t.Skipf("one-factor matrix rejected: %v", err)
		}

		direct, err := GaussianCopulaJoint(marginals, c)
		if err != nil {
			t.Skipf("not priceable: %v", err)
		}

		order := rapid.Permutation(indexRange(n)).Draw(t, "order")
		permutedMarginals := make([]Probability, n)
		for i, idx := range order {
			permutedMarginals[i] = marginals[idx]
		}
		permutedMatrix, err := c.Submatrix(order)
		if err != nil {
			t.Fatalf("Submatrix: %v", err)
		}
		permuted, err := GaussianCopulaJoint(permutedMarginals, permutedMatrix)
		if err != nil {
			t.Fatalf("the permuted parlay failed where the original succeeded: %v", err)
		}

		// Bit-identity, not a tolerance. MultivariateNormalCDF canonicalises the leg
		// order from the thresholds and the correlation structure alone (see
		// canonicalOrder), so the two calls run the identical lattice path and must
		// agree in every bit. Asserting a tolerance here would let a regression that
		// reintroduced argument-order dependence pass as long as it stayed inside
		// the quadrature's error, which is exactly the defect this test exists to
		// catch — the discrepancy that motivated the canonicalisation was 3e-8.
		if float64(direct) != float64(permuted) {
			t.Fatalf("permuting the legs changed the joint: %.17g vs %.17g under %v",
				float64(direct), float64(permuted), order)
		}
	})
}

// TestPropertyTwoLegJointIsMonotoneInCorrelation asserts Slepian's inequality at
// the parlay level: raising the correlation between two legs cannot lower the
// probability that both win.
func TestPropertyTwoLegJointIsMonotoneInCorrelation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p1 := rapid.Float64Range(0.02, 0.98).Draw(t, "p1")
		p2 := rapid.Float64Range(0.02, 0.98).Draw(t, "p2")
		low := rapid.Float64Range(-1, 1).Draw(t, "low")
		high := rapid.Float64Range(low, 1).Draw(t, "high")

		marginals := []Probability{Probability(p1), Probability(p2)}
		lowMatrix, err := NewCorrelationMatrix(equicorrelated(2, low))
		if err != nil {
			t.Fatalf("NewCorrelationMatrix: %v", err)
		}
		highMatrix, err := NewCorrelationMatrix(equicorrelated(2, high))
		if err != nil {
			t.Fatalf("NewCorrelationMatrix: %v", err)
		}

		lowJoint, err := GaussianCopulaJoint(marginals, lowMatrix)
		if err != nil {
			t.Fatalf("GaussianCopulaJoint: %v", err)
		}
		highJoint, err := GaussianCopulaJoint(marginals, highMatrix)
		if err != nil {
			t.Fatalf("GaussianCopulaJoint: %v", err)
		}
		if float64(highJoint) < float64(lowJoint)-tolBivariate {
			t.Fatalf("raising ρ from %g to %g lowered the joint from %.17g to %.17g",
				low, high, float64(lowJoint), float64(highJoint))
		}
	})
}

func indexRange(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// TestUnexportedHelpersRejectBadInput reaches the defensive branches that the
// exported validators make unreachable. They exist because an unexported helper is
// one refactor away from being called from somewhere that has not validated, and a
// branch that has never executed is a branch nobody knows works.
func TestUnexportedHelpersRejectBadInput(t *testing.T) {
	t.Run("impliedMarginals on an invalid leg", func(t *testing.T) {
		// Decimal(1.0) is a zero payout, not a price. validateLegPrices catches it
		// on every exported path, so this is the only way to exercise the branch.
		if _, err := impliedMarginals([]Decimal{Decimal(juice), Decimal(1)}); !errors.Is(err, ErrDecimalOutOfRange) {
			t.Errorf("error = %v, want ErrDecimalOutOfRange", err)
		}
	})

	t.Run("boundedJoint on an impossible probability", func(t *testing.T) {
		// Two legs at 0.5 cannot have a joint probability of 0.9.
		marginals := mustProbabilities(t, 0.5, 0.5)
		if _, err := boundedJoint(0.9, marginals); !errors.Is(err, ErrParlayNotPriceable) {
			t.Errorf("error = %v, want ErrParlayNotPriceable", err)
		}
		// Nor one below what the union bound forces.
		certain := mustProbabilities(t, 0.9, 0.9)
		if _, err := boundedJoint(0.1, certain); !errors.Is(err, ErrParlayNotPriceable) {
			t.Errorf("error = %v, want ErrParlayNotPriceable", err)
		}
		// Inside the slack it is clamped onto the bound instead.
		clamped, err := boundedJoint(0.5+frechetSlack/2, marginals)
		if err != nil {
			t.Fatalf("a result inside the slack should be clamped, not rejected: %v", err)
		}
		if clamped != 0.5 {
			t.Errorf("clamped to %.17g, want exactly the bound 0.5", float64(clamped))
		}
	})

	t.Run("teaser probability propagates a matrix mismatch", func(t *testing.T) {
		legs := []TeaserLeg{
			{Points: 6, OriginalPrice: Decimal(2.5), TeasedPrice: Decimal(juice)},
			{Points: 6, OriginalPrice: Decimal(2.4), TeasedPrice: Decimal(juice)},
		}
		if _, err := TeaserProbability(legs, mustIdentity(t, 3)); !errors.Is(err, ErrLegCountMismatch) {
			t.Errorf("error = %v, want ErrLegCountMismatch", err)
		}
	})
}
