package odds

import (
	"errors"
	"math"
	"testing"

	"pgregory.net/rapid"
)

// -----------------------------------------------------------------------------
// Conventions used by this file and by kelly_test.go
// -----------------------------------------------------------------------------
//
// Float comparison. This file reuses closeTo, assertClose, relTolExact (1e-15) and
// relTolChain (1e-12) from convert_test.go rather than declaring a second
// comparator in the same package. relTolExact bounds a value produced by a handful
// of correctly-rounded operations; relTolChain leaves headroom for a longer chain.
// Both are many orders of magnitude tighter than the smallest price difference the
// domain can express, so neither can absorb a wrong formula.
//
// Exact ==. It appears in exactly three tests, each of which has exactness as the
// property under test and a written proof in the doc comment of the function being
// tested: ExpectedValue and Kelly are exactly zero at the break-even price, and
// MinimumDecimalForEdge at a target of zero is bit-identical to FairDecimal.
// Nowhere else.
//
// Data. The worked examples are standard published sportsbook prices — the -110
// point-spread juice, the ladder rungs, longshots — written as the mechanics of
// the wager, and the expected answers are computed by hand in the comment beside
// each row so a reviewer can check them without running or trusting the code.
// standardPrices, shared with convert_test.go, derives its decimal and probability
// columns from stake and profit by a different arithmetic path than the code under
// test uses, so agreement is evidence rather than a restatement.

// fairProb builds a Probability or fails the test.
func fairProb(t *testing.T, v float64) Probability {
	t.Helper()
	p, err := NewProbability(v)
	if err != nil {
		t.Fatalf("NewProbability(%v): %v", v, err)
	}
	return p
}

// decOdds builds a Decimal or fails the test.
func decOdds(t *testing.T, v float64) Decimal {
	t.Helper()
	d, err := NewDecimal(v)
	if err != nil {
		t.Fatalf("NewDecimal(%v): %v", v, err)
	}
	return d
}

// sweepDecimals returns a broad set of legal decimal prices for the invariant
// sweeps: every rung of the standard published ladder, a dense walk across the
// range a book actually quotes, the shortest representable price, and long prices
// out to the last magnitude whose reciprocal is still a normal double.
//
// The upper end stops at 4e307 deliberately. Past roughly 4.5e307 the reciprocal
// 1/d becomes subnormal, the relative-error bound behind the exact-zero proof in
// Kelly's doc comment degrades to an absolute one, and the invariant is no longer
// claimed. That boundary is covered by its own test rather than being quietly
// excluded here.
func sweepDecimals(t *testing.T) []Decimal {
	t.Helper()

	out := make([]Decimal, 0, 4200)
	for _, row := range standardPrices {
		out = append(out, decOdds(t, row.decimal()))
	}
	// A dense walk from just above evens to a 400-to-1 longshot.
	for i := 0; i < 4000; i++ {
		out = append(out, decOdds(t, 1.01+float64(i)*0.1))
	}
	for _, v := range []float64{
		math.Nextafter(1, 2), // the shortest price that exists at all
		1 + 1e-9, 1 + 1e-6, 1.0001, 1.001,
		1.5, 2, 3, math.Pi, 10, 100, 1e3, 1e6, 1e12, 1e60, 1e150, 4e307,
	} {
		out = append(out, decOdds(t, v))
	}
	return out
}

// -----------------------------------------------------------------------------
// Fair value
// -----------------------------------------------------------------------------

func TestFairDecimalFromAFairProbability(t *testing.T) {
	cases := []struct {
		name string
		q    float64
		want float64
	}{
		// 1/0.5 = 2, which is +100. A devigged coin flip is even money.
		{"50% is even money", 0.5, 2},
		// The two-way -110 market devigs to 0.5 a side, so the fair price of a
		// standard point spread is +100 and the 4.55% the book keeps is the whole
		// of its margin.
		{"52.38% is the -110 price itself", 11.0 / 21.0, 21.0 / 11.0},
		// 1/0.8 = 1.25, which is -400.
		{"80% is -400", 0.8, 1.25},
		// 1/0.25 = 4, which is +300.
		{"25% is +300", 0.25, 4},
		// 1/0.04 = 25, which is +2400.
		{"4% is +2400", 0.04, 25},
		// 1/0.999 = 1.001001001..., an extreme but legal favourite.
		{"99.9% is a 1.001 price", 0.999, 1000.0 / 999.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FairDecimal(fairProb(t, c.q))
			if err != nil {
				t.Fatalf("FairDecimal(%v): %v", c.q, err)
			}
			assertClose(t, "fair decimal", float64(got), c.want, relTolExact)
		})
	}
}

func TestBreakevenProbabilityOfEveryStandardPrice(t *testing.T) {
	for _, row := range standardPrices {
		t.Run(row.name, func(t *testing.T) {
			// The table states the wager as "risk stake to win profit", so the
			// break-even probability is stake/(stake+profit) — the same number the
			// function computes as 1/d, by a different route.
			got, err := BreakevenProbability(decOdds(t, row.decimal()))
			if err != nil {
				t.Fatalf("BreakevenProbability: %v", err)
			}
			assertClose(t, "breakeven probability", float64(got), row.probability(), relTolExact)
		})
	}
}

func TestBreakevenProbabilityOfTheStandardJuice(t *testing.T) {
	// The single most published price in US sports betting. Risk 110 to win 100:
	// break-even is 110/210 = 11/21 = 0.523809523809..., i.e. a bettor must win
	// 52.38% of -110 wagers to come out level.
	d := decOdds(t, 210.0/110.0)
	got, err := BreakevenProbability(d)
	if err != nil {
		t.Fatalf("BreakevenProbability: %v", err)
	}
	assertClose(t, "-110 breakeven", float64(got), 11.0/21.0, relTolExact)

	// And the fair price of a market where both sides are truly 50/50 is +100, so
	// the book's hold on a balanced -110/-110 market is
	//   1 - 1/(2 x 11/21) = 1 - 21/22 = 1/22 = 4.5454...%.
	hold := 1 - 1/(2*float64(got))
	assertClose(t, "-110 two-way hold", hold, 1.0/22.0, relTolExact)
}

func TestFairDecimalAndBreakevenProbabilityAreInverses(t *testing.T) {
	for _, row := range standardPrices {
		t.Run(row.name, func(t *testing.T) {
			d := decOdds(t, row.decimal())

			q, err := BreakevenProbability(d)
			if err != nil {
				t.Fatalf("BreakevenProbability: %v", err)
			}
			back, err := FairDecimal(q)
			if err != nil {
				t.Fatalf("FairDecimal: %v", err)
			}
			assertClose(t, "d -> q -> d", float64(back), float64(d), relTolChain)
		})
	}
}

// -----------------------------------------------------------------------------
// Expected value
// -----------------------------------------------------------------------------

func TestExpectedValueWorkedExamples(t *testing.T) {
	cases := []struct {
		name string
		q    float64
		d    float64
		want float64 // expected profit per unit staked
	}{
		{
			// The canonical teaching example. 0.55 x 2 - 1 = 1.10 - 1 = 0.10.
			"55% at +100", 0.55, 2, 0.10,
		},
		{
			// A true coin flip taken at the standard juice. d = 210/110 = 21/11.
			// 0.5 x 21/11 - 1 = 21/22 - 1 = -1/22 = -0.0454545...
			// This is the -110 hold, arrived at from the bettor's side.
			"50% at -110", 0.5, 210.0 / 110.0, -1.0 / 22.0,
		},
		{
			// 0.55 x 21/11 - 1: 0.55 x 21 = 11.55, 11.55/11 = 1.05, minus 1 = 0.05.
			// A 55% shot at -110 is a 5% edge.
			"55% at -110", 0.55, 210.0 / 110.0, 0.05,
		},
		{
			// +350 is d = 4.5. 0.25 x 4.5 - 1 = 1.125 - 1 = 0.125.
			"25% at +350", 0.25, 4.5, 0.125,
		},
		{
			// -400 is d = 1.25 and implies exactly 0.8, so this is break-even:
			// 0.8 x 1.25 - 1 = 1 - 1 = 0.
			"80% at -400 is break-even", 0.8, 1.25, 0,
		},
		{
			// +1100 is d = 12. 0.10 x 12 - 1 = 1.2 - 1 = 0.2.
			"10% at +1100", 0.10, 12, 0.2,
		},
		{
			// +4000 is d = 41. 0.02 x 41 - 1 = 0.82 - 1 = -0.18. Longshots at a
			// realistic probability are heavily negative; this is the shape of the
			// favourite-longshot bias.
			"2% at +4000", 0.02, 41, -0.18,
		},
		{
			// A stake on an outcome that cannot happen loses the whole stake.
			"impossible outcome loses the stake", 0, 2.5, -1,
		},
		{
			// A certain winner returns d, so the profit is d - 1.
			"certain outcome wins the profit", 1, 2.5, 1.5,
		},
	}

	// relTolChain, not relTolExact, and for a stated reason rather than because it
	// is more comfortable. Both EV and Edge finish with "something near 1, minus
	// 1", and that subtraction amplifies the relative error of the intermediate by
	// 1/|result|. A -110 price has EV of -1/22 at a coin flip, so a half-ULP in
	// the product becomes 22 half-ULPs in the answer; at a genuinely thin 0.5%
	// edge the amplification is 200x. relTolChain is 1e-12 relative, still nine
	// orders of magnitude tighter than the ~4e-4 gap between adjacent quoted
	// prices, so it cannot absorb a wrong formula -- only the cancellation that is
	// inherent to the shape of the expression.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, d := fairProb(t, c.q), decOdds(t, c.d)

			ev, err := ExpectedValue(q, d)
			if err != nil {
				t.Fatalf("ExpectedValue: %v", err)
			}
			assertClose(t, "EV", ev, c.want, relTolChain)

			pct, err := ExpectedValuePercent(q, d)
			if err != nil {
				t.Fatalf("ExpectedValuePercent: %v", err)
			}
			assertClose(t, "EV%", pct, c.want*100, relTolChain)
		})
	}
}

// oneULPBelowOne is 2^-53, the gap between 1 and the double immediately below it,
// and the exact magnitude by which the expected value at a break-even price can
// miss zero. See ExpectedValue's doc comment for the derivation: the gross return
// rounds to one of exactly two doubles, 1 or 1 - 2^-53.
var oneULPBelowOne = math.Ldexp(1, -53)

// TestExpectedValueIsNeverPositiveAtTheBreakevenPrice asserts the one-sided
// guarantee that is actually provable, and asserts the exact bound on the other
// side rather than a hand-waved tolerance.
//
// The naive expectation is that EV is exactly zero here. It is not, and finding
// that out is the point of sweeping four thousand prices: fl(1/d) is the
// correctly-rounded reciprocal, so d x fl(1/d) is 1 + eps with |eps| < 2^-53, and
// rounding lands on either 1 or its predecessor. At d = 1.71 it lands on the
// predecessor and the expected value is -1.1102230246251565e-16.
//
// What matters is the sign. A break-even price must never look profitable, or the
// +EV finder recommends a stake on a wager with no edge at all. That direction is
// asserted with no tolerance, and Kelly turns it into an exact zero -- see
// TestKellyIsExactlyZeroAtTheBreakevenPrice.
func TestExpectedValueIsNeverPositiveAtTheBreakevenPrice(t *testing.T) {
	prices := sweepDecimals(t)
	exact := 0
	for _, d := range prices {
		q, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability(%v): %v", float64(d), err)
		}
		ev, err := ExpectedValue(q, d)
		if err != nil {
			t.Fatalf("ExpectedValue(%v, %v): %v", float64(q), float64(d), err)
		}
		if ev > 0 {
			t.Fatalf("ExpectedValue(1/%v, %v) = %.17g, a positive edge on a break-even price",
				float64(d), float64(d), ev)
		}
		if ev < -oneULPBelowOne {
			t.Fatalf("ExpectedValue(1/%v, %v) = %.17g, further below zero than the proven bound %.17g",
				float64(d), float64(d), ev, -oneULPBelowOne)
		}
		if ev == 0 {
			exact++
		}
	}
	t.Logf("checked %d prices: %d land on exactly zero, %d on -2^-53, none positive",
		len(prices), exact, len(prices)-exact)
}

func TestExpectedValuePercentIsExactlyOneHundredTimesExpectedValue(t *testing.T) {
	for _, d := range sweepDecimals(t) {
		for _, qv := range []float64{0, 0.01, 0.25, 0.5, 0.75, 0.99, 1} {
			q := fairProb(t, qv)

			ev, err := ExpectedValue(q, d)
			if err != nil {
				t.Fatalf("ExpectedValue: %v", err)
			}
			pct, err := ExpectedValuePercent(q, d)
			if err != nil {
				// The only legitimate divergence: the multiple is representable
				// but one hundred times it is not. Anything else is a defect.
				if !errors.Is(err, ErrNotFinite) {
					t.Fatalf("ExpectedValuePercent: %v", err)
				}
				if !math.IsInf(ev*100, 0) {
					t.Fatalf("ExpectedValuePercent rejected q=%v d=%v whose percentage %v is finite",
						qv, float64(d), ev*100)
				}
				continue
			}
			// Scaling by 100 is a single correctly-rounded multiplication, so the
			// two agree to within one operation's worth of error.
			assertClose(t, "EV% vs 100*EV", pct, ev*100, relTolExact)
		}
	}
}

func TestExpectedValueIsMonotonic(t *testing.T) {
	// A longer price is worth more at a fixed belief, and a higher belief is worth
	// more at a fixed price. Both are strict, and a formula with a sign error or a
	// transposed operand fails one of them immediately.
	q := fairProb(t, 0.4)
	prev := math.Inf(-1)
	for i := 0; i < 500; i++ {
		d := decOdds(t, 1.05+float64(i)*0.05)
		ev, err := ExpectedValue(q, d)
		if err != nil {
			t.Fatalf("ExpectedValue: %v", err)
		}
		if ev <= prev {
			t.Fatalf("EV at d=%v is %v, not greater than %v at the previous price", float64(d), ev, prev)
		}
		prev = ev
	}

	d := decOdds(t, 2.5)
	prev = math.Inf(-1)
	for i := 0; i <= 100; i++ {
		q := fairProb(t, float64(i)/100)
		ev, err := ExpectedValue(q, d)
		if err != nil {
			t.Fatalf("ExpectedValue: %v", err)
		}
		if ev <= prev {
			t.Fatalf("EV at q=%v is %v, not greater than %v at the previous probability", float64(q), ev, prev)
		}
		prev = ev
	}
}

// -----------------------------------------------------------------------------
// Edge
// -----------------------------------------------------------------------------

func TestEdgeWorkedExamples(t *testing.T) {
	cases := []struct {
		name string
		q    float64
		p    float64
		want float64
	}{
		{
			// -110 implies 11/21. 0.55 / (11/21) = 0.55 x 21/11 = 11.55/11 = 1.05,
			// minus 1 = 0.05. Note this is the same 5% that TestExpectedValue-
			// WorkedExamples computes for 55% at -110, which is the identity the
			// Edge doc comment describes.
			"55% against -110", 0.55, 11.0 / 21.0, 0.05,
		},
		{
			// 0.5 / (11/21) = 10.5/11 = 0.954545..., minus 1 = -0.0454545...
			"50% against -110", 0.5, 11.0 / 21.0, -1.0 / 22.0,
		},
		{
			// 0.6/0.5 = 1.2, minus 1 = 0.2. Twenty percent over the market.
			"60% against an even-money price", 0.6, 0.5, 0.2,
		},
		{
			// A book that has priced an outcome as certain leaves nothing on the
			// table: 0.5/1 - 1 = -0.5.
			"50% against a certainty", 0.5, 1, -0.5,
		},
		{
			// Believing exactly what the price says is zero edge by construction.
			"the price against itself", 11.0 / 21.0, 11.0 / 21.0, 0,
		},
	}
	// relTolChain for the cancellation reason argued in TestExpectedValueWorked-
	// Examples. The "50% against -110" row is the concrete case: 0.5/(11/21) is
	// 0.9545454545454546, and subtracting 1 from it leaves 4.5e-2 carrying the
	// rounding error of a quantity twenty-two times larger.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, p := fairProb(t, c.q), fairProb(t, c.p)

			e, err := Edge(q, p)
			if err != nil {
				t.Fatalf("Edge: %v", err)
			}
			assertClose(t, "edge", e, c.want, relTolChain)

			pct, err := EdgePercent(q, p)
			if err != nil {
				t.Fatalf("EdgePercent: %v", err)
			}
			assertClose(t, "edge%", pct, c.want*100, relTolChain)
		})
	}
}

// TestEdgeEqualsExpectedValueWhenThePriceIsItsOwnReference is the identity the
// Edge doc comment claims: substituting p = 1/d turns q/p - 1 into q*d - 1.
//
// Agreement is asserted to a tolerance rather than exactly, and that is the honest
// statement: Edge divides by p where ExpectedValue multiplies by d = fl(1/p), so
// the two round differently in the last place.
func TestEdgeEqualsExpectedValueWhenThePriceIsItsOwnReference(t *testing.T) {
	for _, row := range standardPrices {
		d := decOdds(t, row.decimal())
		p, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability: %v", err)
		}

		for _, qv := range []float64{0.05, 0.25, 0.4, 0.5, 0.5238095238095238, 0.6, 0.8, 0.95} {
			q := fairProb(t, qv)

			e, err := Edge(q, p)
			if err != nil {
				t.Fatalf("Edge: %v", err)
			}
			ev, err := ExpectedValue(q, d)
			if err != nil {
				t.Fatalf("ExpectedValue: %v", err)
			}
			assertClose(t, row.name+" edge vs EV", e, ev, relTolChain)
		}
	}
}

func TestEdgeRejectsAnImpossibleReferencePrice(t *testing.T) {
	// p = 0 is a legal probability but the quotient is undefined, and no book
	// offers a price on an outcome it believes cannot happen.
	_, err := Edge(fairProb(t, 0.5), fairProb(t, 0))
	if !errors.Is(err, ErrProbabilityNotPriceable) {
		t.Fatalf("Edge(0.5, 0) error = %v, want ErrProbabilityNotPriceable", err)
	}
}

func TestEdgeRejectsAnOverflowingQuotient(t *testing.T) {
	// A subnormal reference probability has no representable reciprocal, so q/p
	// overflows. The failure is in the result, not the input, and must be reported
	// rather than returned as +Inf.
	tiny := fairProb(t, math.SmallestNonzeroFloat64)
	_, err := Edge(fairProb(t, 1), tiny)
	if !errors.Is(err, ErrNotFinite) {
		t.Fatalf("Edge(1, %v) error = %v, want ErrNotFinite", float64(tiny), err)
	}
}

// TestPercentageScalingRejectsOverflow covers a case that only turned up because
// the finiteness sweep below runs over the whole legal Decimal range rather than
// over plausible prices.
//
// Decimal admits values past 1.7e306, and expected value is bounded above by d-1,
// so multiplying by 100 can reach infinity. Neither input is out of range, so an
// argument check would not have caught it; the result has to be checked. An
// infinity escaping here would spread silently through every downstream average.
func TestPercentageScalingRejectsOverflow(t *testing.T) {
	// A certain winner at a price of 1e307 has an expected value of ~1e307, and
	// one hundred times that is not representable.
	if _, err := ExpectedValuePercent(fairProb(t, 1), decOdds(t, 1e307)); !errors.Is(err, ErrNotFinite) {
		t.Errorf("ExpectedValuePercent error = %v, want ErrNotFinite", err)
	}
	// The raw multiple is still fine -- only the percentage overflows.
	if _, err := ExpectedValue(fairProb(t, 1), decOdds(t, 1e307)); err != nil {
		t.Errorf("ExpectedValue at the same inputs: %v", err)
	}

	// The same overflow through the probability-space route.
	if _, err := EdgePercent(fairProb(t, 1), fairProb(t, 1e-307)); !errors.Is(err, ErrNotFinite) {
		t.Errorf("EdgePercent error = %v, want ErrNotFinite", err)
	}
	if _, err := Edge(fairProb(t, 1), fairProb(t, 1e-307)); err != nil {
		t.Errorf("Edge at the same inputs: %v", err)
	}
}

// -----------------------------------------------------------------------------
// The inverse question: what price do I need?
// -----------------------------------------------------------------------------

func TestMinimumDecimalForEdgeWorkedExamples(t *testing.T) {
	cases := []struct {
		name   string
		q      float64
		target float64
		want   float64
	}{
		{
			// (1 + 0.05)/0.55 = 1.05/0.55 = 105/55 = 21/11 = 1.909090... = -110.
			// Read back: to clear a 5% edge on a 55% shot you must be getting at
			// least -110, which is exactly what the EV worked example computes in
			// the other direction.
			"55% shot needs -110 for a 5% edge", 0.55, 0.05, 21.0 / 11.0,
		},
		{
			// (1 + 0)/0.5 = 2. At zero target the answer is the fair price.
			"50% shot at zero target is even money", 0.5, 0, 2,
		},
		{
			// (1 + 0.125)/0.25 = 1.125/0.25 = 4.5, i.e. +350.
			"25% shot needs +350 for a 12.5% edge", 0.25, 0.125, 4.5,
		},
		{
			// A negative target asks how much worse than fair is tolerable.
			// (1 - 0.0454545...)/0.5 = 0.954545.../0.5 = 1.909090... = -110.
			// A coin flip at -110 loses exactly the standard hold, which is the
			// same statement as the 50%-at--110 EV row.
			"50% shot at the -110 hold", 0.5, -1.0 / 22.0, 21.0 / 11.0,
		},
		{
			// (1 + 1)/0.5 = 4. Demanding a 100% edge on a coin flip means holding
			// out for +300.
			"50% shot needs +300 to double the stake in expectation", 0.5, 1, 4,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := MinimumDecimalForEdge(fairProb(t, c.q), c.target)
			if err != nil {
				t.Fatalf("MinimumDecimalForEdge: %v", err)
			}
			assertClose(t, "minimum decimal", float64(got), c.want, relTolExact)
		})
	}
}

// TestMinimumDecimalForEdgeAtZeroIsTheFairPrice uses == deliberately. Both sides
// evaluate fl(1/q) — (1+0) is exactly 1, so the division is the identical
// operation — and asserting bit equality is what pins the two entry points to one
// implementation of the reciprocal.
func TestMinimumDecimalForEdgeAtZeroIsTheFairPrice(t *testing.T) {
	for i := 1; i < 1000; i++ {
		q := fairProb(t, float64(i)/1000)

		fair, err := FairDecimal(q)
		if err != nil {
			t.Fatalf("FairDecimal(%v): %v", float64(q), err)
		}
		threshold, err := MinimumDecimalForEdge(q, 0)
		if err != nil {
			t.Fatalf("MinimumDecimalForEdge(%v, 0): %v", float64(q), err)
		}
		if threshold != fair {
			t.Fatalf("q=%v: MinimumDecimalForEdge at zero = %.17g, FairDecimal = %.17g",
				float64(q), float64(threshold), float64(fair))
		}
	}
}

func TestMinimumDecimalForEdgeRoundTripsThroughExpectedValue(t *testing.T) {
	// Taking the threshold price back through ExpectedValue must recover the
	// target. This is the round trip that catches an inverted formula, which a
	// single worked example can miss when the numbers happen to be symmetric.
	checked := 0
	for _, qv := range []float64{0.02, 0.1, 0.25, 0.4, 0.5, 0.55, 0.75, 0.9, 0.98} {
		for _, target := range []float64{-0.5, -0.05, 0, 0.01, 0.05, 0.25, 1, 10} {
			q := fairProb(t, qv)

			d, err := MinimumDecimalForEdge(q, target)
			if err != nil {
				// A negative target on a short-priced favourite asks for a
				// threshold at or below decimal 1, which is not a price. That is
				// the documented contract, not a failure: q = 0.75 at a target of
				// -0.5 wants (1-0.5)/0.75 = 0.667.
				if !errors.Is(err, ErrDecimalOutOfRange) {
					t.Fatalf("MinimumDecimalForEdge(%v, %v): %v", qv, target, err)
				}
				if 1+target > qv {
					t.Fatalf("MinimumDecimalForEdge(%v, %v) rejected a threshold of %v, which is a legal price",
						qv, target, (1+target)/qv)
				}
				continue
			}
			ev, err := ExpectedValue(q, d)
			if err != nil {
				t.Fatalf("ExpectedValue: %v", err)
			}
			assertClose(t, "recovered target edge", ev, target, relTolChain)
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("every combination was rejected; the round trip was never exercised")
	}
}

func TestMinimumDecimalForEdgeRejectsImpossibleRequests(t *testing.T) {
	cases := []struct {
		name   string
		q      float64
		target float64
		want   error
	}{
		{"no price gives an edge on an impossible outcome", 0, 0.05, ErrProbabilityNotPriceable},
		{"a target of -1 wipes out the return", 0.5, -1, ErrDecimalOutOfRange},
		{"a target below -1 is a negative price", 0.5, -1.5, ErrDecimalOutOfRange},
		{"a certainty is already fully priced", 1, 0, ErrDecimalOutOfRange},
		{"NaN target", 0.5, math.NaN(), ErrNotFinite},
		{"infinite target", 0.5, math.Inf(1), ErrNotFinite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := MinimumDecimalForEdge(Probability(c.q), c.target)
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Invalid input
// -----------------------------------------------------------------------------

func TestEVFunctionsRejectInvalidInput(t *testing.T) {
	badProbabilities := []struct {
		name string
		v    float64
		want error
	}{
		{"NaN", math.NaN(), ErrNotFinite},
		{"+Inf", math.Inf(1), ErrNotFinite},
		{"-Inf", math.Inf(-1), ErrNotFinite},
		{"negative", -0.01, ErrProbabilityOutOfRange},
		{"above one", 1.01, ErrProbabilityOutOfRange},
	}
	badDecimals := []struct {
		name string
		v    float64
		want error
	}{
		{"NaN", math.NaN(), ErrNotFinite},
		{"+Inf", math.Inf(1), ErrNotFinite},
		{"exactly one", 1, ErrDecimalOutOfRange},
		{"below one", 0.5, ErrDecimalOutOfRange},
		{"zero", 0, ErrDecimalOutOfRange},
		{"negative", -2, ErrDecimalOutOfRange},
	}

	for _, bp := range badProbabilities {
		t.Run("probability/"+bp.name, func(t *testing.T) {
			if _, err := ExpectedValue(Probability(bp.v), 2); !errors.Is(err, bp.want) {
				t.Errorf("ExpectedValue = %v, want %v", err, bp.want)
			}
			if _, err := ExpectedValuePercent(Probability(bp.v), 2); !errors.Is(err, bp.want) {
				t.Errorf("ExpectedValuePercent = %v, want %v", err, bp.want)
			}
			if _, err := Edge(Probability(bp.v), 0.5); !errors.Is(err, bp.want) {
				t.Errorf("Edge (fair side) = %v, want %v", err, bp.want)
			}
			if _, err := Edge(0.5, Probability(bp.v)); !errors.Is(err, bp.want) {
				t.Errorf("Edge (reference side) = %v, want %v", err, bp.want)
			}
			if _, err := EdgePercent(Probability(bp.v), 0.5); !errors.Is(err, bp.want) {
				t.Errorf("EdgePercent = %v, want %v", err, bp.want)
			}
			if _, err := FairDecimal(Probability(bp.v)); !errors.Is(err, bp.want) {
				t.Errorf("FairDecimal = %v, want %v", err, bp.want)
			}
			if _, err := MinimumDecimalForEdge(Probability(bp.v), 0.05); !errors.Is(err, bp.want) {
				t.Errorf("MinimumDecimalForEdge = %v, want %v", err, bp.want)
			}
		})
	}

	for _, bd := range badDecimals {
		t.Run("decimal/"+bd.name, func(t *testing.T) {
			if _, err := ExpectedValue(0.5, Decimal(bd.v)); !errors.Is(err, bd.want) {
				t.Errorf("ExpectedValue = %v, want %v", err, bd.want)
			}
			if _, err := ExpectedValuePercent(0.5, Decimal(bd.v)); !errors.Is(err, bd.want) {
				t.Errorf("ExpectedValuePercent = %v, want %v", err, bd.want)
			}
			if _, err := BreakevenProbability(Decimal(bd.v)); !errors.Is(err, bd.want) {
				t.Errorf("BreakevenProbability = %v, want %v", err, bd.want)
			}
		})
	}
}

// TestNoEVFunctionEverReturnsNonFinite sweeps the whole legal input space of every
// exported function in ev.go and asserts that a successful call never hands back a
// NaN or an infinity. This is the failure mode that matters most in practice: a
// single NaN entering a slate propagates through every aggregate that touches it,
// and it does so silently because NaN compares false against every bound.
func TestNoEVFunctionEverReturnsNonFinite(t *testing.T) {
	probabilities := []float64{
		0, math.SmallestNonzeroFloat64, 1e-300, 1e-12, 0.001, 0.25, 0.5, 0.75,
		0.999, 1 - 1e-12, math.Nextafter(1, 0), 1,
	}

	checked := 0
	for _, d := range sweepDecimals(t) {
		for _, qv := range probabilities {
			q := Probability(qv)

			if ev, err := ExpectedValue(q, d); err == nil {
				assertFinite(t, "ExpectedValue", ev)
				checked++
			}
			if pct, err := ExpectedValuePercent(q, d); err == nil {
				assertFinite(t, "ExpectedValuePercent", pct)
				checked++
			}
			if reference, err := BreakevenProbability(d); err == nil {
				if e, err := Edge(q, reference); err == nil {
					assertFinite(t, "Edge", e)
					checked++
				}
			}
			if fair, err := FairDecimal(q); err == nil {
				assertFinite(t, "FairDecimal", float64(fair))
				checked++
			}
			if bp, err := BreakevenProbability(d); err == nil {
				assertFinite(t, "BreakevenProbability", float64(bp))
				checked++
			}
			for _, target := range []float64{-0.9, 0, 0.05, 100} {
				if md, err := MinimumDecimalForEdge(q, target); err == nil {
					assertFinite(t, "MinimumDecimalForEdge", float64(md))
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("swept nothing; the guard would pass vacuously")
	}
	t.Logf("checked %d successful results, none non-finite", checked)
}

// assertFinite fails the test if v is a NaN or an infinity.
func assertFinite(t *testing.T, what string, v float64) {
	t.Helper()
	if math.IsNaN(v) || math.IsInf(v, 0) {
		t.Fatalf("%s returned a non-finite value: %v", what, v)
	}
}

// -----------------------------------------------------------------------------
// Property-based tests (CLAUDE.md §4)
// -----------------------------------------------------------------------------

// The ranges below are deliberately the realistic ones — a decimal price from just
// inside evens out to a 500-to-1 longshot, and a probability strictly inside
// (0, 1) — because that is where an implementation error has to be caught. The
// degenerate ends of the domain are covered exhaustively by the table tests above,
// which can assert exact answers there rather than relations.

func TestPropertyExpectedValueIsBoundedByItsExtremes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		qv := rapid.Float64Range(0, 1).Draw(t, "q")
		dv := rapid.Float64Range(1.0001, 501).Draw(t, "d")

		ev, err := ExpectedValue(Probability(qv), Decimal(dv))
		if err != nil {
			t.Fatalf("ExpectedValue(%v, %v): %v", qv, dv, err)
		}
		// Losing the stake is the floor; taking the full profit is the ceiling.
		if ev < -1 || ev > dv-1 {
			t.Fatalf("EV %v outside [-1, %v] for q=%v d=%v", ev, dv-1, qv, dv)
		}
	})
}

func TestPropertyEdgeAndExpectedValueAgree(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		qv := rapid.Float64Range(1e-6, 1).Draw(t, "q")
		dv := rapid.Float64Range(1.0001, 501).Draw(t, "d")
		d := Decimal(dv)

		p, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability(%v): %v", dv, err)
		}
		ev, err := ExpectedValue(Probability(qv), d)
		if err != nil {
			t.Fatalf("ExpectedValue: %v", err)
		}
		e, err := Edge(Probability(qv), p)
		if err != nil {
			t.Fatalf("Edge: %v", err)
		}
		if !closeTo(e, ev, relTolChain) {
			t.Fatalf("q=%v d=%v: edge %.17g and EV %.17g disagree beyond %g", qv, dv, e, ev, relTolChain)
		}
	})
}

func TestPropertyFairPriceRoundTrips(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		qv := rapid.Float64Range(1e-9, 1-1e-9).Draw(t, "q")
		q := Probability(qv)

		d, err := FairDecimal(q)
		if err != nil {
			t.Fatalf("FairDecimal(%v): %v", qv, err)
		}
		back, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability(%v): %v", float64(d), err)
		}
		if !closeTo(float64(back), qv, relTolChain) {
			t.Fatalf("q=%v round trips to %v", qv, float64(back))
		}
		// And a wager at the fair price is never profitable, missing zero by at
		// most the single unit in the last place the reciprocal can cost.
		ev, err := ExpectedValue(back, d)
		if err != nil {
			t.Fatalf("ExpectedValue: %v", err)
		}
		if ev > 0 || ev < -oneULPBelowOne {
			t.Fatalf("EV at the fair price of q=%v is %.17g, want within [-2^-53, 0]", qv, ev)
		}
		// Kelly, unlike EV, is exactly zero there.
		f, err := Kelly(back, d)
		if err != nil {
			t.Fatalf("Kelly: %v", err)
		}
		if f != 0 {
			t.Fatalf("Kelly at the fair price of q=%v is %.17g, want exactly 0", qv, f)
		}
	})
}

func TestPropertyMinimumDecimalForEdgeDeliversTheTarget(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		qv := rapid.Float64Range(1e-4, 1).Draw(t, "q")
		target := rapid.Float64Range(-0.9, 50).Draw(t, "target")

		d, err := MinimumDecimalForEdge(Probability(qv), target)
		if err != nil {
			// Legitimate for a q close to 1 with a negative target: the threshold
			// price falls to or below 1 and is not a price. The error is the
			// contract, so accept it and move on.
			return
		}
		ev, err := ExpectedValue(Probability(qv), d)
		if err != nil {
			t.Fatalf("ExpectedValue: %v", err)
		}
		if !closeTo(ev, target, relTolChain) {
			t.Fatalf("q=%v target=%v: threshold price %v yields EV %.17g", qv, target, float64(d), ev)
		}
	})
}
