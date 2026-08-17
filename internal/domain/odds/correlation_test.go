package odds

import (
	"errors"
	"math"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// -----------------------------------------------------------------------------
// Float comparison policy
// -----------------------------------------------------------------------------
//
// This file uses closeTo/assertClose from convert_test.go, which compare with a
// relative tolerance scaled by the larger magnitude but never scaled below 1. For
// quantities in [0, 1] — every probability here — that degrades to an absolute
// tolerance, which is the right comparison: an error of 1e-13 in a probability of
// 1e-6 does not matter to a price, and demanding relative precision there would be
// asserting something the algorithm never promised.
//
// Nothing here compares floats with ==, except where exactness is the property
// under test and is argued for at the assertion.

const (
	// tolNormal bounds the standard normal CDF and its inverse against published
	// reference values. math.Erfc is accurate to under one ULP and AS 241 to about
	// 1e-16 relative, so the algorithms are three to four orders tighter than this.
	// The slack is for the reference values themselves: they are transcribed to
	// sixteen or seventeen significant digits from published tables, and 1e-12
	// tolerates a transcription that is right to thirteen. It cannot absorb a wrong
	// coefficient — a single mistyped AS 241 coefficient moves the result by 1e-3
	// or more.
	tolNormal = 1e-12

	// tolBivariate bounds the bivariate normal CDF against exact closed forms.
	// Genz reports about 1e-15 accuracy for BVND across the parameter space; 1e-13
	// is two orders of headroom and is still ten orders below any difference that
	// could change a displayed price.
	tolBivariate = 1e-13

	// tolQuadrature bounds the bivariate normal CDF against the independent
	// composite-Simpson cross-check below. It is looser than tolBivariate because
	// the reference itself is numerical: 200,000 subintervals accumulate roughly
	// 1e-13 of summation error, and Simpson's own truncation error at that step
	// size is around 1e-18. 1e-9 is four orders above the summation floor.
	tolQuadrature = 1e-9
)

// -----------------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------------

func mustNormalCDF(t *testing.T, x float64) float64 {
	t.Helper()
	v, err := NormalCDF(x)
	if err != nil {
		t.Fatalf("NormalCDF(%v): %v", x, err)
	}
	return v
}

func mustBivariate(t *testing.T, h, k, r float64) float64 {
	t.Helper()
	v, err := BivariateNormalCDF(h, k, r)
	if err != nil {
		t.Fatalf("BivariateNormalCDF(%v, %v, %v): %v", h, k, r, err)
	}
	return v
}

func mustCorrelationMatrix(t *testing.T, rows [][]float64) CorrelationMatrix {
	t.Helper()
	c, err := NewCorrelationMatrix(rows)
	if err != nil {
		t.Fatalf("NewCorrelationMatrix(%v): %v", rows, err)
	}
	return c
}

// equicorrelated builds the n×n matrix with 1 on the diagonal and rho everywhere
// else. Its eigenvalues have a closed form, which several tests rely on.
func equicorrelated(n int, rho float64) [][]float64 {
	rows := make([][]float64, n)
	for i := range rows {
		rows[i] = make([]float64, n)
		for j := range rows[i] {
			if i == j {
				rows[i][j] = 1
			} else {
				rows[i][j] = rho
			}
		}
	}
	return rows
}

// bivariateByQuadrature is an independent evaluation of Φ₂(h, k; r), used to
// cross-check BivariateNormalCDF.
//
// It integrates the conditioning identity
//
//	Φ₂(h, k; r) = ∫_{-∞}^{h} φ(x)·Φ((k − r·x)/√(1−r²)) dx
//
// which follows from Y | X = x ~ N(r·x, 1−r²), by composite Simpson's rule over
// [-12, h]. This shares no formulation, no substitution and no constant with
// Genz's BVND — that algorithm integrates over the correlation parameter with a
// Gauss-Legendre rule — so agreement is evidence about the implementation rather
// than a restatement of it.
//
// The truncation at -12 discards Φ(-12) ≈ 1.8e-33 of mass, thirteen orders below
// the tolerance. Callers must keep |r| < 1; the endpoints have exact closed forms
// and are checked separately.
func bivariateByQuadrature(h, k, r float64) float64 {
	const (
		lo         = -12.0
		partitions = 200_000 // even, as Simpson's rule requires
	)
	if h <= lo {
		return 0
	}

	denom := math.Sqrt(1 - r*r)
	integrand := func(x float64) float64 {
		density := math.Exp(-x*x/2) / math.Sqrt(2*math.Pi)
		return density * 0.5 * math.Erfc(-((k-r*x)/denom)/math.Sqrt2)
	}

	step := (h - lo) / partitions
	sum := integrand(lo) + integrand(h)
	for i := 1; i < partitions; i++ {
		weight := 2.0
		if i%2 == 1 {
			weight = 4.0
		}
		sum += weight * integrand(lo+float64(i)*step)
	}
	return sum * step / 3
}

// -----------------------------------------------------------------------------
// The standard normal CDF
// -----------------------------------------------------------------------------

// TestNormalCDFAgainstPublishedValues checks Φ at the points whose values are the
// most widely published numbers in statistics: the one-, two- and three-sigma
// coverage probabilities.
//
// Every expectation is derived from the published two-sided coverage rather than
// from a one-sided table, so the arithmetic path to the expectation differs from
// the arithmetic path in the code: the code computes ½·erfc(−x/√2), the table
// computes (1 + coverage)/2.
func TestNormalCDFAgainstPublishedValues(t *testing.T) {
	// Two-sided coverage of ±kσ for the standard normal, to sixteen digits.
	const (
		oneSigma   = 0.6826894921370859
		twoSigma   = 0.9544997361036416
		threeSigma = 0.9973002039367398
	)

	cases := []struct {
		name string
		x    float64
		want float64
	}{
		{"median", 0, 0.5},
		{"+1σ", 1, (1 + oneSigma) / 2},
		{"-1σ", -1, (1 - oneSigma) / 2},
		{"+2σ", 2, (1 + twoSigma) / 2},
		{"-2σ", -2, (1 - twoSigma) / 2},
		{"+3σ", 3, (1 + threeSigma) / 2},
		{"-3σ", -3, (1 - threeSigma) / 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertClose(t, "NormalCDF", mustNormalCDF(t, c.x), c.want, tolNormal)
		})
	}
}

// TestNormalCDFIsSymmetricAndMonotone asserts the two structural properties that
// no table can be transcribed wrongly into: Φ(x) + Φ(-x) = 1, and Φ is strictly
// increasing.
func TestNormalCDFIsSymmetricAndMonotone(t *testing.T) {
	previous := math.Inf(-1)
	for x := -8.0; x <= 8.0; x += 0.125 {
		got := mustNormalCDF(t, x)
		assertClose(t, "Φ(x) + Φ(-x)", got+mustNormalCDF(t, -x), 1, relTolExact)
		if got <= previous {
			t.Fatalf("Φ is not strictly increasing at x = %g: %.17g after %.17g", x, got, previous)
		}
		previous = got
	}
}

func TestNormalCDFEdgeCases(t *testing.T) {
	if got := mustNormalCDF(t, math.Inf(1)); got != 1 {
		t.Errorf("Φ(+Inf) = %v, want exactly 1", got)
	}
	if got := mustNormalCDF(t, math.Inf(-1)); got != 0 {
		t.Errorf("Φ(-Inf) = %v, want exactly 0", got)
	}
	if _, err := NormalCDF(math.NaN()); !errors.Is(err, ErrNotFinite) {
		t.Errorf("NormalCDF(NaN) error = %v, want ErrNotFinite", err)
	}
}

// -----------------------------------------------------------------------------
// The inverse standard normal CDF (AS 241)
// -----------------------------------------------------------------------------

// TestNormalQuantileAgainstPublishedCriticalValues checks Φ⁻¹ against the normal
// critical values every statistics reference tabulates, and against the inverses
// of the sigma coverages used above.
//
// Between them these cover the central branch of AS 241 (|p - ½| ≤ 0.425) and the
// first tail branch; the third branch is covered separately below.
func TestNormalQuantileAgainstPublishedCriticalValues(t *testing.T) {
	cases := []struct {
		name string
		p    float64
		want float64
	}{
		{"median", 0.5, 0},
		{"z_0.90", 0.90, 1.2815515655446004},
		{"z_0.95", 0.95, 1.6448536269514722},
		{"z_0.975", 0.975, 1.959963984540054},
		{"z_0.99", 0.99, 2.3263478740408408},
		{"z_0.995", 0.995, 2.5758293035489004},
		{"z_0.025 (mirror)", 0.025, -1.959963984540054},
		{"z_0.005 (mirror)", 0.005, -2.5758293035489004},
		{"inverse of Φ(1)", 0.8413447460685429, 1},
		{"inverse of Φ(2)", 0.9772498680518208, 2},
		{"inverse of Φ(3)", 0.9986501019683699, 3},
		{"inverse of Φ(-1)", 0.15865525393145705, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalQuantile(Probability(c.p))
			if err != nil {
				t.Fatalf("NormalQuantile(%v): %v", c.p, err)
			}
			assertClose(t, "NormalQuantile", got, c.want, tolNormal)
		})
	}
}

// TestNormalQuantileCoversEveryBranch exercises all three rational approximations
// of AS 241 and asserts the round trip Φ(Φ⁻¹(p)) = p in each.
//
// The branch boundaries are |p - ½| = 0.425 — i.e. p = 0.075 and p = 0.925 — and
// a tail area of e⁻²⁵ ≈ 1.3888e-11, where √(−log p) crosses 5. The table names
// which branch each row is aimed at so that a future edit to the boundaries breaks
// a test with a legible name.
func TestNormalQuantileCoversEveryBranch(t *testing.T) {
	cases := []struct {
		branch string
		p      float64
	}{
		{"central", 0.5},
		{"central", 0.076},
		{"central", 0.924},
		{"central boundary", 0.075},
		{"central boundary", 0.925},
		{"moderate tail", 0.074},
		{"moderate tail", 0.926},
		{"moderate tail", 1e-9},
		{"moderate tail", 1 - 1e-9},
		{"extreme tail", 1e-12},
		{"extreme tail", 1e-100},
		{"extreme tail", 1e-300},
	}
	for _, c := range cases {
		t.Run(c.branch, func(t *testing.T) {
			z, err := NormalQuantile(Probability(c.p))
			if err != nil {
				t.Fatalf("NormalQuantile(%g): %v", c.p, err)
			}
			back := mustNormalCDF(t, z)
			// Relative, because the tail probabilities span 300 orders of magnitude
			// and an absolute tolerance is meaningless at 1e-300.
			if rel := math.Abs(back-c.p) / c.p; rel > 1e-9 {
				t.Errorf("Φ(Φ⁻¹(%g)) = %.17g, relative error %.3g", c.p, back, rel)
			}
		})
	}
}

// TestNormalQuantileIsMonotone asserts that Φ⁻¹ is strictly increasing across the
// branch boundaries, which is where a coefficient transcription error would show
// up as a discontinuity rather than as a wrong value.
func TestNormalQuantileIsMonotone(t *testing.T) {
	previous := math.Inf(-1)
	for _, p := range []float64{
		1e-13, 1e-12, 1.3e-11, 1.5e-11, 1e-6, 0.001, 0.0749, 0.075, 0.0751,
		0.2, 0.4, 0.5, 0.6, 0.8, 0.9249, 0.925, 0.9251, 0.99, 0.999999,
	} {
		z, err := NormalQuantile(Probability(p))
		if err != nil {
			t.Fatalf("NormalQuantile(%g): %v", p, err)
		}
		if z <= previous {
			t.Fatalf("Φ⁻¹ is not strictly increasing at p = %g: %.17g after %.17g", p, z, previous)
		}
		previous = z
	}
}

func TestNormalQuantileRejectsUnusableInput(t *testing.T) {
	cases := []struct {
		name string
		p    float64
		want error
	}{
		{"below zero", -0.1, ErrProbabilityOutOfRange},
		{"above one", 1.1, ErrProbabilityOutOfRange},
		{"not a number", math.NaN(), ErrNotFinite},
		{"infinite", math.Inf(1), ErrNotFinite},
		{"exactly zero implies -Inf", 0, ErrNotFinite},
		{"exactly one implies +Inf", 1, ErrNotFinite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NormalQuantile(Probability(c.p))
			if !errors.Is(err, c.want) {
				t.Errorf("NormalQuantile(%v) error = %v, want %v", c.p, err, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The bivariate normal CDF
// -----------------------------------------------------------------------------

// TestBivariateNormalMatchesSheppardsFormula checks Φ₂ at the origin against the
// exact closed form
//
//	Φ₂(0, 0; ρ) = ¼ + arcsin(ρ)/(2π)
//
// found by W. F. Sheppard, "On the application of the theory of error to cases of
// normal distribution and normal correlation", Philosophical Transactions of the
// Royal Society A, Vol. 192 (1899), pp. 101-167. It is the strongest available
// reference: an exact identity rather than a table, evaluated at every correlation
// including all four algorithm branch points (|ρ| = 0.3, 0.75 switch the
// Gauss-Legendre order; |ρ| = 0.925 switches the entire formulation).
func TestBivariateNormalMatchesSheppardsFormula(t *testing.T) {
	for _, rho := range []float64{
		-1, -0.999, -0.99, -0.9375, -0.925, -0.9, -0.8, -0.75, -0.5, -0.3, -0.29,
		0,
		0.29, 0.3, 0.5, 0.75, 0.8, 0.9, 0.925, 0.9375, 0.99, 0.999, 1,
	} {
		want := 0.25 + math.Asin(rho)/(2*math.Pi)
		got := mustBivariate(t, 0, 0, rho)
		if !closeTo(got, want, tolBivariate) {
			t.Errorf("Φ₂(0, 0; %g) = %.17g, Sheppard's formula says %.17g (|diff| = %.3g)",
				rho, got, want, math.Abs(got-want))
		}
	}
}

// TestBivariateNormalExactSpecialCases checks the four cases where Φ₂ collapses to
// a closed form in the univariate CDF: independence, the two Fréchet-Hoeffding
// endpoints, and an effectively infinite limit which must reproduce the marginal.
func TestBivariateNormalExactSpecialCases(t *testing.T) {
	limits := []float64{-2.5, -1, -0.25, 0, 0.25, 1, 2.5}

	t.Run("independence is the product of the marginals", func(t *testing.T) {
		for _, h := range limits {
			for _, k := range limits {
				want := mustNormalCDF(t, h) * mustNormalCDF(t, k)
				assertClose(t, "Φ₂(h,k;0)", mustBivariate(t, h, k, 0), want, tolBivariate)
			}
		}
	})

	t.Run("perfect positive correlation is the smaller marginal", func(t *testing.T) {
		for _, h := range limits {
			for _, k := range limits {
				want := math.Min(mustNormalCDF(t, h), mustNormalCDF(t, k))
				assertClose(t, "Φ₂(h,k;1)", mustBivariate(t, h, k, 1), want, tolBivariate)
			}
		}
	})

	t.Run("perfect negative correlation is the union bound", func(t *testing.T) {
		for _, h := range limits {
			for _, k := range limits {
				want := math.Max(0, mustNormalCDF(t, h)+mustNormalCDF(t, k)-1)
				assertClose(t, "Φ₂(h,k;-1)", mustBivariate(t, h, k, -1), want, tolBivariate)
			}
		}
	})

	t.Run("an unbounded limit reproduces the marginal", func(t *testing.T) {
		// Φ(10) = 1 - 7.6e-24, so the second constraint is inactive to well beyond
		// double precision.
		for _, rho := range []float64{-0.99, -0.5, 0, 0.5, 0.99} {
			for _, h := range limits {
				assertClose(t, "Φ₂(h,10;ρ)", mustBivariate(t, h, 10, rho), mustNormalCDF(t, h), tolBivariate)
				assertClose(t, "Φ₂(10,k;ρ)", mustBivariate(t, 10, h, rho), mustNormalCDF(t, h), tolBivariate)
			}
		}
	})
}

// TestBivariateNormalStructuralIdentities checks symmetry in its arguments and the
// reflection identity Φ₂(-h, -k; ρ) = 1 - Φ(h) - Φ(k) + Φ₂(h, k; ρ), which follows
// from (X, Y) and (-X, -Y) having the same distribution.
func TestBivariateNormalStructuralIdentities(t *testing.T) {
	for _, rho := range []float64{-0.95, -0.6, -0.2, 0.2, 0.6, 0.95} {
		for _, h := range []float64{-2, -0.5, 0.5, 2} {
			for _, k := range []float64{-1.5, 0, 1.5} {
				direct := mustBivariate(t, h, k, rho)
				assertClose(t, "argument symmetry", mustBivariate(t, k, h, rho), direct, tolBivariate)

				want := 1 - mustNormalCDF(t, h) - mustNormalCDF(t, k) + direct
				assertClose(t, "reflection", mustBivariate(t, -h, -k, rho), want, tolBivariate)
			}
		}
	}
}

// TestBivariateNormalAgainstIndependentQuadrature cross-checks Φ₂ against a
// composite Simpson integration of the conditioning identity, which shares nothing
// with Genz's algorithm but the answer. See bivariateByQuadrature.
func TestBivariateNormalAgainstIndependentQuadrature(t *testing.T) {
	if testing.Short() {
		t.Skip("200,000-point quadrature per case")
	}
	for _, rho := range []float64{-0.99, -0.93, -0.9, -0.5, -0.1, 0.1, 0.5, 0.9, 0.93, 0.99} {
		for _, h := range []float64{-2, -0.5, 0, 1.25} {
			for _, k := range []float64{-1.75, 0, 0.75} {
				got := mustBivariate(t, h, k, rho)
				want := bivariateByQuadrature(h, k, rho)
				if !closeTo(got, want, tolQuadrature) {
					t.Errorf("Φ₂(%g, %g; %g) = %.17g, independent quadrature says %.17g (|diff| = %.3g)",
						h, k, rho, got, want, math.Abs(got-want))
				}
			}
		}
	}
}

// TestBivariateNormalIsIncreasingInCorrelation asserts Slepian's inequality, which
// is the property the whole correlation adjustment rests on: raising ρ raises the
// probability that both legs win. If this were ever violated, a positively
// correlated same-game parlay would price above the independent product instead of
// below it.
func TestBivariateNormalIsIncreasingInCorrelation(t *testing.T) {
	// Forty evenly spaced correlations from -1 to 1, built by integer arithmetic so
	// the endpoints are exact rather than the result of accumulated addition.
	const steps = 40
	for _, h := range []float64{-1.5, -0.4, 0, 0.4, 1.5} {
		for _, k := range []float64{-1.2, 0, 1.2} {
			previous := math.Inf(-1)
			for i := 0; i <= steps; i++ {
				rho := -1 + 2*float64(i)/steps
				got := mustBivariate(t, h, k, rho)
				if got < previous-tolBivariate {
					t.Fatalf("Φ₂(%g, %g; ·) decreased at ρ = %g: %.17g after %.17g", h, k, rho, got, previous)
				}
				previous = got
			}
		}
	}
}

// TestBivariateNormalStaysInTheUnitInterval checks that no branch, including the
// asymptotic expansion used for |ρ| ≥ 0.925, can produce a value outside [0, 1] or
// a value outside the Fréchet-Hoeffding bounds.
func TestBivariateNormalStaysInTheUnitInterval(t *testing.T) {
	const steps = 100
	for i := 0; i <= steps; i++ {
		r := -1 + 2*float64(i)/steps
		for _, h := range []float64{-6, -3, -1, 0, 1, 3, 6} {
			for _, k := range []float64{-6, -2, 0, 2, 6} {
				got := mustBivariate(t, h, k, r)
				lower := math.Max(0, mustNormalCDF(t, h)+mustNormalCDF(t, k)-1)
				upper := math.Min(mustNormalCDF(t, h), mustNormalCDF(t, k))
				if got < lower-tolBivariate || got > upper+tolBivariate {
					t.Fatalf("Φ₂(%g, %g; %g) = %.17g, outside the Fréchet interval [%.17g, %.17g]",
						h, k, r, got, lower, upper)
				}
			}
		}
	}
}

func TestBivariateNormalHandlesInfiniteLimits(t *testing.T) {
	inf, ninf := math.Inf(1), math.Inf(-1)
	cases := []struct {
		name    string
		h, k, r float64
		want    float64
	}{
		{"h = -Inf", ninf, 0.5, 0.6, 0},
		{"k = -Inf", 0.5, ninf, -0.6, 0},
		{"both -Inf", ninf, ninf, 0, 0},
		{"both +Inf", inf, inf, 0.9, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustBivariate(t, c.h, c.k, c.r); got != c.want {
				t.Errorf("Φ₂ = %v, want exactly %v", got, c.want)
			}
		})
	}

	t.Run("h = +Inf collapses to the k marginal", func(t *testing.T) {
		assertClose(t, "Φ₂(+Inf, 0.3; 0.7)", mustBivariate(t, inf, 0.3, 0.7), mustNormalCDF(t, 0.3), relTolExact)
	})
	t.Run("k = +Inf collapses to the h marginal", func(t *testing.T) {
		assertClose(t, "Φ₂(-0.4, +Inf; -0.7)", mustBivariate(t, -0.4, inf, -0.7), mustNormalCDF(t, -0.4), relTolExact)
	})
}

func TestBivariateNormalRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		h, k, r float64
		want    error
	}{
		{"h is NaN", math.NaN(), 0, 0, ErrNotFinite},
		{"k is NaN", 0, math.NaN(), 0, ErrNotFinite},
		{"correlation is NaN", 0, 0, math.NaN(), ErrNotFinite},
		{"correlation is infinite", 0, 0, math.Inf(1), ErrNotFinite},
		{"correlation above 1", 0, 0, 1.0000001, ErrCorrelationOutOfRange},
		{"correlation below -1", 0, 0, -1.0000001, ErrCorrelationOutOfRange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := BivariateNormalCDF(c.h, c.k, c.r); !errors.Is(err, c.want) {
				t.Errorf("BivariateNormalCDF(%v, %v, %v) error = %v, want %v", c.h, c.k, c.r, err, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// CorrelationMatrix construction and validation
// -----------------------------------------------------------------------------

func TestCorrelationMatrixRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		rows [][]float64
		want error
	}{
		{"empty", nil, ErrCorrelationShape},
		{"ragged", [][]float64{{1, 0.2}, {0.2}}, ErrCorrelationShape},
		{"not square", [][]float64{{1, 0.2, 0}, {0.2, 1, 0}}, ErrCorrelationShape},
		{"entry is NaN", [][]float64{{1, math.NaN()}, {math.NaN(), 1}}, ErrNotFinite},
		{"entry is infinite", [][]float64{{1, math.Inf(1)}, {math.Inf(1), 1}}, ErrNotFinite},
		{"entry above 1", [][]float64{{1, 1.5}, {1.5, 1}}, ErrCorrelationOutOfRange},
		{"entry below -1", [][]float64{{1, -1.5}, {-1.5, 1}}, ErrCorrelationOutOfRange},
		{"diagonal is not 1", [][]float64{{0.9, 0.2}, {0.2, 1}}, ErrCorrelationDiagonal},
		{"covariance passed as correlation", [][]float64{{4, 0.2}, {0.2, 1}}, ErrCorrelationDiagonal},
		{"asymmetric", [][]float64{{1, 0.4}, {0.1, 1}}, ErrCorrelationNotSymmetric},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewCorrelationMatrix(c.rows); !errors.Is(err, c.want) {
				t.Errorf("NewCorrelationMatrix error = %v, want %v", err, c.want)
			}
		})
	}

	t.Run("beyond the dimension bound", func(t *testing.T) {
		rows := equicorrelated(MaxCorrelationDimension+1, 0)
		if _, err := NewCorrelationMatrix(rows); !errors.Is(err, ErrCorrelationShape) {
			t.Errorf("NewCorrelationMatrix error = %v, want ErrCorrelationShape", err)
		}
	})
}

// TestCorrelationMatrixRejectsSymmetricButNotPositiveSemiDefinite is the check
// that separates a real validator from an entrywise one. Both matrices below are
// symmetric, have a unit diagonal, and have every entry comfortably inside
// [-1, 1]; neither describes a dependence structure any set of random variables
// can have.
//
// The first is the textbook intransitivity: A moves with B, B moves with C, and A
// is claimed independent of C. Its eigenvalues are known in closed form — a
// symmetric tridiagonal Toeplitz matrix with diagonal a and off-diagonal b has
// eigenvalues a + 2b·cos(kπ/(n+1)) — giving 1 ± 0.9√2 and 1, so the smallest is
// about -0.2728. The test asserts that number as well as the rejection, because a
// validator that rejects for the wrong reason is not a validator.
func TestCorrelationMatrixRejectsSymmetricButNotPositiveSemiDefinite(t *testing.T) {
	intransitive := [][]float64{
		{1, 0.9, 0},
		{0.9, 1, 0.9},
		{0, 0.9, 1},
	}
	if _, err := NewCorrelationMatrix(intransitive); !errors.Is(err, ErrCorrelationNotPositiveSemiDefinite) {
		t.Fatalf("NewCorrelationMatrix(intransitive) error = %v, want ErrCorrelationNotPositiveSemiDefinite", err)
	}

	eigen, err := jacobiEigenvalues(intransitive)
	if err != nil {
		t.Fatalf("jacobiEigenvalues: %v", err)
	}
	want := []float64{1 - 0.9*math.Sqrt2, 1, 1 + 0.9*math.Sqrt2}
	for i := range want {
		assertClose(t, "eigenvalue", eigen[i], want[i], relTolChain)
	}

	sign := [][]float64{
		{1, 0.9, -0.9},
		{0.9, 1, 0.9},
		{-0.9, 0.9, 1},
	}
	if _, err := NewCorrelationMatrix(sign); !errors.Is(err, ErrCorrelationNotPositiveSemiDefinite) {
		t.Errorf("NewCorrelationMatrix(sign-inconsistent) error = %v, want ErrCorrelationNotPositiveSemiDefinite", err)
	}

	// The equicorrelated family is positive semi-definite exactly for
	// ρ ≥ -1/(n-1), where the smallest eigenvalue 1 + (n-1)ρ reaches zero. The
	// boundary must be accepted and anything past it rejected.
	const n = 3
	boundary := -1.0 / (n - 1)
	mustCorrelationMatrix(t, equicorrelated(n, boundary))
	if _, err := NewCorrelationMatrix(equicorrelated(n, boundary-1e-6)); !errors.Is(err, ErrCorrelationNotPositiveSemiDefinite) {
		t.Errorf("NewCorrelationMatrix(ρ just past the boundary) error = %v, want ErrCorrelationNotPositiveSemiDefinite", err)
	}
}

// TestCorrelationMatrixAcceptsSingularBoundaryCases checks that positive
// SEMI-definite really means semi: a rank-deficient matrix is a legitimate
// correlation structure (two legs that are the same event, or exact complements)
// and must not be rejected as if it were indefinite.
func TestCorrelationMatrixAcceptsSingularBoundaryCases(t *testing.T) {
	for _, rows := range [][][]float64{
		{{1, 1}, {1, 1}},
		{{1, -1}, {-1, 1}},
		{{1, 1, 1}, {1, 1, 1}, {1, 1, 1}},
	} {
		c := mustCorrelationMatrix(t, rows)
		eigen, err := c.Eigenvalues()
		if err != nil {
			t.Fatalf("Eigenvalues: %v", err)
		}
		if eigen[0] < -psdTolerance {
			t.Errorf("accepted a matrix with smallest eigenvalue %g", eigen[0])
		}
	}
}

// TestCorrelationMatrixSnapsWithinTolerance checks that a matrix which is
// symmetric and unit-diagonal only to within float round-off is accepted and then
// stored exactly, so nothing downstream sees the discrepancy.
func TestCorrelationMatrixSnapsWithinTolerance(t *testing.T) {
	c := mustCorrelationMatrix(t, [][]float64{
		{1 + 1e-14, 0.4},
		{0.4 + 1e-14, 1 - 1e-14},
	})
	for i := range 2 {
		if got, _ := c.At(i, i); got != 1 {
			t.Errorf("diagonal (%d,%d) = %.17g, want exactly 1", i, i, got)
		}
	}
	up, _ := c.At(0, 1)
	lo, _ := c.At(1, 0)
	if up != lo {
		t.Errorf("stored matrix is not exactly symmetric: %.17g vs %.17g", up, lo)
	}

	// An off-diagonal a hair past ±1 — what a covariance divided by two standard
	// deviations produces for a perfectly correlated pair — is snapped rather than
	// rejected, at both ends.
	for _, sign := range []float64{1, -1} {
		snapped := mustCorrelationMatrix(t, [][]float64{
			{1, sign * (1 + 1e-14)},
			{sign * (1 + 1e-14), 1},
		})
		if got, _ := snapped.At(0, 1); got != sign {
			t.Errorf("off-diagonal %.17g was not snapped to %v", got, sign)
		}
	}
}

func TestIdentityCorrelation(t *testing.T) {
	c, err := IdentityCorrelation(4)
	if err != nil {
		t.Fatalf("IdentityCorrelation(4): %v", err)
	}
	if c.N() != 4 || !c.IsIdentity() || c.IsZero() || !c.CopulaIsExact() {
		t.Fatalf("IdentityCorrelation(4): N=%d IsIdentity=%v IsZero=%v CopulaIsExact=%v",
			c.N(), c.IsIdentity(), c.IsZero(), c.CopulaIsExact())
	}
	for _, n := range []int{0, -1, MaxCorrelationDimension + 1} {
		if _, err := IdentityCorrelation(n); !errors.Is(err, ErrCorrelationShape) {
			t.Errorf("IdentityCorrelation(%d) error = %v, want ErrCorrelationShape", n, err)
		}
	}
}

func TestNewCorrelationMatrixFromPairs(t *testing.T) {
	c, err := NewCorrelationMatrixFromPairs(3, []PairCorrelation{
		{I: 0, J: 1, Rho: 0.35},
		{I: 2, J: 1, Rho: -0.2},
	})
	if err != nil {
		t.Fatalf("NewCorrelationMatrixFromPairs: %v", err)
	}
	want := [][]float64{
		{1, 0.35, 0},
		{0.35, 1, -0.2},
		{0, -0.2, 1},
	}
	for i := range 3 {
		for j := range 3 {
			got, ok := c.At(i, j)
			if !ok {
				t.Fatalf("At(%d,%d) reported out of range", i, j)
			}
			assertClose(t, "entry", got, want[i][j], relTolExact)
		}
	}

	t.Run("an unnamed pair defaults to independence", func(t *testing.T) {
		if got, _ := c.At(0, 2); got != 0 {
			t.Errorf("unnamed pair = %v, want exactly 0", got)
		}
	})

	errorCases := []struct {
		name  string
		n     int
		pairs []PairCorrelation
		want  error
	}{
		{"dimension zero", 0, nil, ErrCorrelationShape},
		{"dimension beyond the bound", MaxCorrelationDimension + 1, nil, ErrCorrelationShape},
		{"index out of range", 2, []PairCorrelation{{I: 0, J: 5, Rho: 0.1}}, ErrCorrelationIndex},
		{"negative index", 2, []PairCorrelation{{I: -1, J: 1, Rho: 0.1}}, ErrCorrelationIndex},
		{"diagonal named", 2, []PairCorrelation{{I: 1, J: 1, Rho: 0.1}}, ErrCorrelationIndex},
		{
			"pair repeated in the other order", 2,
			[]PairCorrelation{{I: 0, J: 1, Rho: 0.1}, {I: 1, J: 0, Rho: 0.2}},
			ErrCorrelationIndex,
		},
		{"out of range coefficient", 2, []PairCorrelation{{I: 0, J: 1, Rho: 2}}, ErrCorrelationOutOfRange},
		{
			"individually plausible but jointly impossible", 3,
			[]PairCorrelation{{I: 0, J: 1, Rho: 0.9}, {I: 1, J: 2, Rho: 0.9}},
			ErrCorrelationNotPositiveSemiDefinite,
		},
	}
	for _, c := range errorCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewCorrelationMatrixFromPairs(c.n, c.pairs); !errors.Is(err, c.want) {
				t.Errorf("NewCorrelationMatrixFromPairs error = %v, want %v", err, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// CorrelationMatrix accessors
// -----------------------------------------------------------------------------

func TestCorrelationMatrixIsImmutable(t *testing.T) {
	input := [][]float64{{1, 0.5}, {0.5, 1}}
	c := mustCorrelationMatrix(t, input)

	input[0][1] = 0.99 // mutate the constructor argument
	if got, _ := c.At(0, 1); got != 0.5 {
		t.Errorf("mutating the input changed the matrix: At(0,1) = %v", got)
	}

	rows := c.Rows()
	rows[0][1] = -0.99 // mutate the exported copy
	if got, _ := c.At(0, 1); got != 0.5 {
		t.Errorf("mutating Rows() changed the matrix: At(0,1) = %v", got)
	}
}

func TestCorrelationMatrixZeroValue(t *testing.T) {
	var c CorrelationMatrix
	if !c.IsZero() || c.N() != 0 {
		t.Fatalf("zero value: IsZero=%v N=%d", c.IsZero(), c.N())
	}
	if c.IsIdentity() {
		t.Error("the zero value must not report itself as the identity")
	}
	if c.CopulaIsExact() {
		t.Error("the zero value must not report an exact copula")
	}
	if c.Rows() != nil {
		t.Error("Rows on the zero value should be nil")
	}
	if _, ok := c.At(0, 0); ok {
		t.Error("At on the zero value reported success")
	}
	if _, err := c.Eigenvalues(); !errors.Is(err, ErrCorrelationShape) {
		t.Error("Eigenvalues on the zero value should fail")
	}
	if _, err := c.Cholesky(); !errors.Is(err, ErrCorrelationShape) {
		t.Error("Cholesky on the zero value should fail")
	}
	if _, err := c.Submatrix([]int{0}); !errors.Is(err, ErrCorrelationShape) {
		t.Error("Submatrix on the zero value should fail")
	}
}

func TestCorrelationMatrixAtBounds(t *testing.T) {
	c := mustCorrelationMatrix(t, equicorrelated(2, 0.4))
	for _, idx := range [][2]int{{-1, 0}, {0, -1}, {2, 0}, {0, 2}} {
		if v, ok := c.At(idx[0], idx[1]); ok || v != 0 {
			t.Errorf("At(%d,%d) = (%v, %v), want (0, false)", idx[0], idx[1], v, ok)
		}
	}
}

func TestSubmatrix(t *testing.T) {
	c := mustCorrelationMatrix(t, [][]float64{
		{1, 0.1, 0.2, 0.3},
		{0.1, 1, 0.4, 0.5},
		{0.2, 0.4, 1, 0.6},
		{0.3, 0.5, 0.6, 1},
	})

	sub, err := c.Submatrix([]int{1, 3})
	if err != nil {
		t.Fatalf("Submatrix: %v", err)
	}
	if sub.N() != 2 {
		t.Fatalf("Submatrix N = %d, want 2", sub.N())
	}
	if got, _ := sub.At(0, 1); got != 0.5 {
		t.Errorf("Submatrix At(0,1) = %v, want 0.5", got)
	}

	t.Run("order follows the index order", func(t *testing.T) {
		reversed, err := c.Submatrix([]int{3, 1})
		if err != nil {
			t.Fatalf("Submatrix: %v", err)
		}
		a, _ := reversed.At(0, 1)
		b, _ := sub.At(0, 1)
		if a != b {
			t.Errorf("reversed submatrix entry = %v, want %v", a, b)
		}
	})

	errorCases := []struct {
		name    string
		indices []int
		want    error
	}{
		{"empty", nil, ErrCorrelationShape},
		{"too many", []int{0, 1, 2, 3, 0}, ErrCorrelationShape},
		{"out of range", []int{0, 4}, ErrCorrelationIndex},
		{"negative", []int{-1}, ErrCorrelationIndex},
		{"repeated", []int{1, 1}, ErrCorrelationIndex},
	}
	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Submatrix(tc.indices); !errors.Is(err, tc.want) {
				t.Errorf("Submatrix(%v) error = %v, want %v", tc.indices, err, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Eigenvalues and Cholesky
// -----------------------------------------------------------------------------

// TestEigenvaluesAgainstClosedForms checks the Jacobi iteration against the two
// families whose spectra are known exactly.
//
// The equicorrelated matrix (1 on the diagonal, ρ elsewhere) has eigenvalue
// 1 + (n−1)ρ once and 1 − ρ with multiplicity n−1: the all-ones vector is an
// eigenvector, and every vector summing to zero is an eigenvector of the other
// eigenvalue. The symmetric tridiagonal Toeplitz matrix with diagonal a and
// off-diagonal b has eigenvalues a + 2b·cos(kπ/(n+1)), a standard result.
func TestEigenvaluesAgainstClosedForms(t *testing.T) {
	t.Run("equicorrelated", func(t *testing.T) {
		for _, n := range []int{2, 3, 5, 8} {
			for _, rho := range []float64{-0.1, 0, 0.25, 0.6, 0.95} {
				c := mustCorrelationMatrix(t, equicorrelated(n, rho))
				eigen, err := c.Eigenvalues()
				if err != nil {
					t.Fatalf("Eigenvalues: %v", err)
				}
				want := make([]float64, n)
				want[0] = 1 + float64(n-1)*rho
				for i := 1; i < n; i++ {
					want[i] = 1 - rho
				}
				slices.Sort(want)
				for i := range n {
					assertClose(t, "eigenvalue", eigen[i], want[i], relTolChain)
				}
			}
		}
	})

	t.Run("tridiagonal toeplitz", func(t *testing.T) {
		const (
			n = 5
			b = 0.4
		)
		rows := make([][]float64, n)
		for i := range rows {
			rows[i] = make([]float64, n)
			rows[i][i] = 1
			if i > 0 {
				rows[i][i-1] = b
			}
			if i < n-1 {
				rows[i][i+1] = b
			}
		}
		c := mustCorrelationMatrix(t, rows)
		eigen, err := c.Eigenvalues()
		if err != nil {
			t.Fatalf("Eigenvalues: %v", err)
		}
		for i := range n {
			// k runs n…1 as the eigenvalues run ascending.
			k := float64(n - i)
			want := 1 + 2*b*math.Cos(k*math.Pi/float64(n+1))
			assertClose(t, "eigenvalue", eigen[i], want, relTolChain)
		}
	})

	t.Run("the trace equals the dimension", func(t *testing.T) {
		c := mustCorrelationMatrix(t, [][]float64{
			{1, 0.3, -0.2},
			{0.3, 1, 0.45},
			{-0.2, 0.45, 1},
		})
		eigen, err := c.Eigenvalues()
		if err != nil {
			t.Fatalf("Eigenvalues: %v", err)
		}
		sum := 0.0
		for _, e := range eigen {
			sum += e
		}
		assertClose(t, "trace", sum, 3, relTolChain)
	})
}

func TestJacobiEigenvaluesDegenerateInput(t *testing.T) {
	if _, err := jacobiEigenvalues(nil); !errors.Is(err, ErrCorrelationShape) {
		t.Errorf("jacobiEigenvalues(nil) error = %v, want ErrCorrelationShape", err)
	}
	eigen, err := jacobiEigenvalues([][]float64{{1}})
	if err != nil {
		t.Fatalf("jacobiEigenvalues(1×1): %v", err)
	}
	if len(eigen) != 1 || eigen[0] != 1 {
		t.Errorf("jacobiEigenvalues(1×1) = %v, want [1]", eigen)
	}
}

// TestCholeskyReconstructsTheMatrix asserts L·Lᵀ = R, for a positive definite
// matrix and for a singular one where a pivot is zeroed.
func TestCholeskyReconstructsTheMatrix(t *testing.T) {
	for _, rows := range [][][]float64{
		equicorrelated(4, 0.3),
		{{1, 0.8, -0.4}, {0.8, 1, -0.2}, {-0.4, -0.2, 1}},
		{{1, 1}, {1, 1}},                    // singular: the second pivot is zero
		{{1, -1, 0}, {-1, 1, 0}, {0, 0, 1}}, // singular with an independent third leg
	} {
		c := mustCorrelationMatrix(t, rows)
		l, err := c.Cholesky()
		if err != nil {
			t.Fatalf("Cholesky: %v", err)
		}
		n := c.N()
		for i := range n {
			for j := range n {
				sum := 0.0
				for k := range n {
					sum += l[i][k] * l[j][k]
				}
				want, _ := c.At(i, j)
				assertClose(t, "L·Lᵀ entry", sum, want, relTolChain)
			}
			for j := i + 1; j < n; j++ {
				if l[i][j] != 0 {
					t.Errorf("L is not lower triangular: L[%d][%d] = %v", i, j, l[i][j])
				}
			}
		}
	}
}

// TestCholeskyRejectsAnIndefinitePivot reaches the defensive branch that the
// public constructor makes unreachable, by assembling a CorrelationMatrix value
// directly. NewCorrelationMatrix would reject this input; the branch exists so
// that a future caller who obtains a matrix by some other route still cannot get a
// silently meaningless factor.
func TestCholeskyRejectsAnIndefinitePivot(t *testing.T) {
	indefinite := CorrelationMatrix{n: 2, data: []float64{1, 2, 2, 1}}
	if _, err := indefinite.Cholesky(); !errors.Is(err, ErrCorrelationNotPositiveSemiDefinite) {
		t.Errorf("Cholesky of an indefinite matrix: error = %v, want ErrCorrelationNotPositiveSemiDefinite", err)
	}
}

// -----------------------------------------------------------------------------
// Property-based tests
// -----------------------------------------------------------------------------

// TestPropertyNormalQuantileRoundTrip asserts Φ(Φ⁻¹(p)) = p to within a relative
// error of 1e-9 across the whole priceable range, on arbitrary inputs rather than
// a chosen table.
func TestPropertyNormalQuantileRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := rapid.Float64Range(1e-12, 1-1e-12).Draw(t, "p")
		z, err := NormalQuantile(Probability(p))
		if err != nil {
			t.Fatalf("NormalQuantile(%g): %v", p, err)
		}
		back, err := NormalCDF(z)
		if err != nil {
			t.Fatalf("NormalCDF(%g): %v", z, err)
		}
		if rel := math.Abs(back-p) / p; rel > 1e-9 {
			t.Fatalf("Φ(Φ⁻¹(%.17g)) = %.17g, relative error %.3g", p, back, rel)
		}
	})
}

// TestPropertyBivariateNormalRespectsItsBounds asserts on arbitrary inputs that Φ₂
// never leaves the Fréchet-Hoeffding interval and is symmetric in its arguments.
func TestPropertyBivariateNormalRespectsItsBounds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		h := rapid.Float64Range(-6, 6).Draw(t, "h")
		k := rapid.Float64Range(-6, 6).Draw(t, "k")
		rho := rapid.Float64Range(-1, 1).Draw(t, "rho")

		got, err := BivariateNormalCDF(h, k, rho)
		if err != nil {
			t.Fatalf("BivariateNormalCDF: %v", err)
		}
		swapped, err := BivariateNormalCDF(k, h, rho)
		if err != nil {
			t.Fatalf("BivariateNormalCDF swapped: %v", err)
		}
		if !closeTo(got, swapped, tolBivariate) {
			t.Fatalf("Φ₂(%g,%g;%g) = %.17g but Φ₂(%g,%g;%g) = %.17g", h, k, rho, got, k, h, rho, swapped)
		}

		ph, err := NormalCDF(h)
		if err != nil {
			t.Fatalf("NormalCDF: %v", err)
		}
		pk, err := NormalCDF(k)
		if err != nil {
			t.Fatalf("NormalCDF: %v", err)
		}
		lower := math.Max(0, ph+pk-1)
		upper := math.Min(ph, pk)
		if got < lower-tolBivariate || got > upper+tolBivariate {
			t.Fatalf("Φ₂(%g,%g;%g) = %.17g, outside [%.17g, %.17g]", h, k, rho, got, lower, upper)
		}
	})
}

// TestPropertyGramMatricesAreAlwaysAccepted asserts the converse of the
// positive-semi-definiteness check: a matrix built the way a real correlation
// estimate is built — normalise a Gram matrix A·Aᵀ, which is positive
// semi-definite by construction — is always accepted, with a smallest eigenvalue
// at or above the tolerance. A validator that rejects valid matrices is as broken
// as one that accepts invalid ones, and only this direction catches that.
func TestPropertyGramMatricesAreAlwaysAccepted(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 6).Draw(t, "n")
		a := make([][]float64, n)
		for i := range a {
			a[i] = make([]float64, n)
			for j := range a[i] {
				a[i][j] = rapid.Float64Range(-3, 3).Draw(t, "a")
			}
		}

		gram := make([][]float64, n)
		for i := range gram {
			gram[i] = make([]float64, n)
			for j := range gram[i] {
				for k := range n {
					gram[i][j] += a[i][k] * a[j][k]
				}
			}
		}
		for i := range n {
			if gram[i][i] <= 1e-6 {
				t.Skip("degenerate row: the normalisation would divide by ~0")
			}
		}

		rows := make([][]float64, n)
		for i := range rows {
			rows[i] = make([]float64, n)
			for j := range rows[i] {
				rows[i][j] = gram[i][j] / math.Sqrt(gram[i][i]*gram[j][j])
			}
		}

		c, err := NewCorrelationMatrix(rows)
		if err != nil {
			t.Fatalf("rejected a normalised Gram matrix: %v", err)
		}
		eigen, err := c.Eigenvalues()
		if err != nil {
			t.Fatalf("Eigenvalues: %v", err)
		}
		if eigen[0] < -psdTolerance {
			t.Fatalf("smallest eigenvalue %g is below the tolerance", eigen[0])
		}
		if !closeTo(sumOf(eigen), float64(n), 1e-10) {
			t.Fatalf("eigenvalues sum to %.17g, want %d", sumOf(eigen), n)
		}
	})
}

func sumOf(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

// TestLatticeAccuracyProfile measures what the lattice quadrature actually
// achieves rather than asserting what it ought to, and logs it. The numbers it
// prints are what orthantRelTol, orthantLatticePoints and orthantMinBatches are
// set from, and the first place to look if a future change to any of them costs
// accuracy.
func TestLatticeAccuracyProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the batch loop repeatedly")
	}
	for _, n := range []int{3, 4, 6, 10, 25} {
		for _, rho := range []float64{0.1, 0.5, 0.9} {
			c := mustCorrelationMatrix(t, equicorrelated(n, rho))
			thresholds := make([]float64, n)
			for i := range thresholds {
				thresholds[i] = 0.25 * float64(i-n/2)
			}
			value, err := MultivariateNormalCDF(thresholds, c)
			if err != nil {
				t.Errorf("n=%2d ρ=%.1f did not converge: %v", n, rho, err)
				continue
			}
			t.Logf("n=%2d ρ=%.1f  Φ_n = %.10f", n, rho, value)
		}
	}

	// A smooth reference integrand with a closed-form value. ∫₀¹ e^{-v²} dv is
	// √π·erf(1)/2, and the integrand is the product over dimensions, so the exact
	// answer is that constant raised to the dimension. This measures the bare
	// quadrature free of the orthant transformation, against a number that is known
	// rather than simulated.
	oneDimensional := math.Sqrt(math.Pi) * math.Erf(1) / 2
	product := func(u []float64) float64 {
		value := 1.0
		for _, v := range u {
			value *= math.Exp(-v * v)
		}
		return value
	}

	for _, dimension := range []int{2, 3, 5, 9} {
		want := math.Pow(oneDimensional, float64(dimension))
		value, spread, err := latticeEstimate(dimension, product, orthantMinBatches, orthantMaxBatches, orthantRelTol, orthantAbsTol)
		if err != nil {
			t.Errorf("smooth %d-D reference did not converge: %v", dimension, err)
			continue
		}
		relative := math.Abs(value-want) / want
		t.Logf("smooth %2d-D reference: value=%.12f exact=%.12f true relative error=%.3g reported three-sigma spread=%.3g",
			dimension, value, want, relative, spread)

		// The bound is against the TRUE error, not the reported spread. The spread
		// is a spread across shifts rather than a proven bound, so asserting on it
		// would be asserting that the estimator agrees with itself.
		if relative > 5*orthantRelTol {
			t.Errorf("smooth %d-D reference: true relative error %.3g is more than five times the %g stopping rule",
				dimension, relative, orthantRelTol)
		}
	}

	// A 24-dimensional product of Gaussians is deliberately harder than any orthant
	// integrand this package produces — it has no decay toward the corners of the
	// cube and a value three orders of magnitude below the two-dimensional case —
	// and the quadrature does not reach the stopping rule on it inside the cap.
	// That is the behaviour worth pinning: on a problem it cannot solve to target
	// it refuses, rather than returning the estimate it happens to have. The
	// 25-leg orthant above, which is what a caller can actually construct,
	// converges.
	if _, spread, err := latticeEstimate(24, product, orthantMinBatches, orthantMaxBatches, orthantRelTol, orthantAbsTol); !errors.Is(err, ErrOrthantNotConverged) {
		t.Errorf("smooth 24-D reference: error = %v (spread %.3g), want ErrOrthantNotConverged", err, spread)
	}
}

// TestLatticeEstimateReportsNonConvergence checks the iteration cap. An
// unreachable tolerance must produce ErrOrthantNotConverged rather than a quietly
// unconverged answer, and the dimension guard must reject a request it cannot
// generate constants for.
func TestLatticeEstimateReportsNonConvergence(t *testing.T) {
	f := func(u []float64) float64 { return u[0] }

	_, _, err := latticeEstimate(1, f, 2, 4, 0, 0)
	if !errors.Is(err, ErrOrthantNotConverged) {
		t.Errorf("latticeEstimate with a zero tolerance: error = %v, want ErrOrthantNotConverged", err)
	}

	for _, dimension := range []int{0, -1, orthantMaxDimension + 1} {
		if _, _, err := latticeEstimate(dimension, f, 2, 4, 1, 1); !errors.Is(err, ErrCorrelationShape) {
			t.Errorf("latticeEstimate(%d dimensions): error = %v, want ErrCorrelationShape", dimension, err)
		}
	}
}

// -----------------------------------------------------------------------------
// The multivariate normal orthant probability
// -----------------------------------------------------------------------------

// TestMultivariateNormalCDFAgainstTheIndependentClosedForm is the strongest
// available correctness check on the lattice quadrature: under the identity
// correlation matrix the orthant probability is exactly the product of the
// marginals, and MultivariateNormalCDF does not special-case the identity — it
// integrates it like anything else. Agreement therefore says the separation of
// variables, the Cholesky, the reordering, the lattice and the stopping rule are
// all doing what they claim, measured against a number known in closed form.
func TestMultivariateNormalCDFAgainstTheIndependentClosedForm(t *testing.T) {
	for _, thresholds := range [][]float64{
		{0, 0, 0},
		{-0.5, 0.25, 1.0},
		{1.5, -1.5, 0.5, 2},
		{-1, -0.5, 0, 0.5, 1, 1.5},
	} {
		identity, err := IdentityCorrelation(len(thresholds))
		if err != nil {
			t.Fatalf("IdentityCorrelation: %v", err)
		}
		want := 1.0
		for _, x := range thresholds {
			want *= mustNormalCDF(t, x)
		}
		got, err := MultivariateNormalCDF(thresholds, identity)
		if err != nil {
			t.Fatalf("MultivariateNormalCDF(%v): %v", thresholds, err)
		}
		relative := math.Abs(got-want) / want
		t.Logf("%d independent constraints: quadrature=%.10f closed form=%.10f relative error=%.3g",
			len(thresholds), got, want, relative)
		if relative > orthantRelTol {
			t.Errorf("Φ_%d(%v; I) = %.12g, closed form %.12g, relative error %.3g beyond %g",
				len(thresholds), thresholds, got, want, relative, orthantRelTol)
		}
	}
}

// TestMultivariateNormalCDFDispatchesTheClosedFormCases checks that one and two
// dimensions go to the exact routines rather than through the quadrature.
func TestMultivariateNormalCDFDispatchesTheClosedFormCases(t *testing.T) {
	one, err := IdentityCorrelation(1)
	if err != nil {
		t.Fatalf("IdentityCorrelation: %v", err)
	}
	got, err := MultivariateNormalCDF([]float64{0.75}, one)
	if err != nil {
		t.Fatalf("MultivariateNormalCDF: %v", err)
	}
	if want := mustNormalCDF(t, 0.75); got != want {
		t.Errorf("one dimension = %.17g, want exactly Φ(0.75) = %.17g", got, want)
	}

	two := mustCorrelationMatrix(t, [][]float64{{1, 0.42}, {0.42, 1}})
	got, err = MultivariateNormalCDF([]float64{-0.3, 1.1}, two)
	if err != nil {
		t.Fatalf("MultivariateNormalCDF: %v", err)
	}
	if want := mustBivariate(t, -0.3, 1.1, 0.42); got != want {
		t.Errorf("two dimensions = %.17g, want exactly Φ₂ = %.17g", got, want)
	}
}

// TestMultivariateNormalCDFInfiniteThresholds checks the two limits: a −∞
// constraint empties the orthant, and a +∞ constraint drops out, leaving the
// probability over the principal submatrix of the constraints that bind.
func TestMultivariateNormalCDFInfiniteThresholds(t *testing.T) {
	c := mustCorrelationMatrix(t, [][]float64{
		{1, 0.3, -0.2},
		{0.3, 1, 0.45},
		{-0.2, 0.45, 1},
	})
	inf, ninf := math.Inf(1), math.Inf(-1)

	t.Run("an impossible constraint empties the orthant", func(t *testing.T) {
		got, err := MultivariateNormalCDF([]float64{0.5, ninf, 0.2}, c)
		if err != nil {
			t.Fatalf("MultivariateNormalCDF: %v", err)
		}
		if got != 0 {
			t.Errorf("got %v, want exactly 0", got)
		}
	})

	t.Run("an inactive constraint drops out", func(t *testing.T) {
		got, err := MultivariateNormalCDF([]float64{0.5, inf, 0.2}, c)
		if err != nil {
			t.Fatalf("MultivariateNormalCDF: %v", err)
		}
		// Legs 0 and 2 with their own correlation, exactly.
		want := mustBivariate(t, 0.5, 0.2, -0.2)
		if got != want {
			t.Errorf("got %.17g, want exactly Φ₂ over the binding pair %.17g", got, want)
		}
	})

	t.Run("every constraint inactive is a certainty", func(t *testing.T) {
		got, err := MultivariateNormalCDF([]float64{inf, inf, inf}, c)
		if err != nil {
			t.Fatalf("MultivariateNormalCDF: %v", err)
		}
		if got != 1 {
			t.Errorf("got %v, want exactly 1", got)
		}
	})
}

func TestMultivariateNormalCDFRejectsBadInput(t *testing.T) {
	c := mustCorrelationMatrix(t, equicorrelated(3, 0.2))

	if _, err := MultivariateNormalCDF([]float64{0, math.NaN(), 0}, c); !errors.Is(err, ErrNotFinite) {
		t.Errorf("a NaN threshold: error = %v, want ErrNotFinite", err)
	}
	if _, err := MultivariateNormalCDF([]float64{0, 0}, c); !errors.Is(err, ErrCorrelationShape) {
		t.Errorf("a threshold count mismatch: error = %v, want ErrCorrelationShape", err)
	}
	var zero CorrelationMatrix
	if _, err := MultivariateNormalCDF([]float64{0}, zero); !errors.Is(err, ErrCorrelationShape) {
		t.Errorf("an unconstructed matrix: error = %v, want ErrCorrelationShape", err)
	}
}

// TestClampToOpenUnit covers the guard that keeps the quadrature's inner loop from
// handing an endpoint to the normal quantile. Every clamped value must still be a
// legal quantile argument.
func TestClampToOpenUnit(t *testing.T) {
	for _, p := range []float64{
		0, 1, -1, 2, math.NaN(), math.Inf(1), math.Inf(-1),
		1e-320, 1 - 1e-17, 0.5, 1e-8, 1 - 1e-8,
	} {
		clamped := clampToOpenUnit(p)
		if !(clamped > 0 && clamped < 1) {
			t.Fatalf("clampToOpenUnit(%v) = %v, which is not strictly inside (0, 1)", p, clamped)
		}
		if _, err := NormalQuantile(Probability(clamped)); err != nil {
			t.Errorf("clampToOpenUnit(%v) = %v, which NormalQuantile rejects: %v", p, clamped, err)
		}
	}

	// A value already strictly inside is returned unchanged.
	for _, p := range []float64{0.25, 0.5, 0.999} {
		if got := clampToOpenUnit(p); got != p {
			t.Errorf("clampToOpenUnit(%v) = %v, want it untouched", p, got)
		}
	}
}
