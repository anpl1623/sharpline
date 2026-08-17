package odds

import (
	"errors"
	"math"
	"testing"
)

// -----------------------------------------------------------------------------
// Float comparison policy
// -----------------------------------------------------------------------------
//
// Nothing in this file compares float64 with ==, except where exactness is the
// property under test and is separately argued for.

const (
	// relTolExact bounds a value produced by a handful of correctly-rounded
	// IEEE-754 double operations. One such operation carries a relative error of at
	// most 2^-53 ≈ 1.11e-16; the deepest expression compared here is four
	// operations, so the true bound is under 5e-16. 1e-15 sits just above that.
	//
	// It is also more than ten orders of magnitude tighter than any error a wrong
	// formula could hide behind: the two closest standard prices in the table below,
	// -110 and -105, differ by 8e-3 in implied probability, and even -110 against a
	// hypothetical -109 differs by 4e-4. A tolerance of 1e-15 cannot absorb a real
	// mistake.
	relTolExact = 1e-15

	// relTolChain is for values that pass through several conversions. It is three
	// orders looser than relTolExact purely to leave headroom for accumulation, and
	// is still nine orders tighter than the smallest meaningful price difference.
	relTolChain = 1e-12

	// decimalRoundTripBound is the proven worst-case absolute error of
	// Decimal → American → Decimal, in decimal odds. Rounding to the nearest integer
	// moves the real-valued American price by at most 0.5. On the positive branch
	// that is 0.5/100 = 0.005 of decimal. On the negative branch the decimal error is
	// 100·|ΔA|/(|A|·|A'|) ≤ 100·0.5/100² = 0.005, since both magnitudes are at least
	// 100. The slack added on top absorbs float noise only; it is 1e-9, six orders
	// below the bound itself, so it cannot mask a rounding bug.
	decimalRoundTripBound = 0.005
	decimalRoundTripSlack = 1e-9
)

// closeTo reports whether got and want agree to within relTol, scaled by the larger
// magnitude but never scaled below 1 — so the comparison degrades gracefully to an
// absolute tolerance near zero instead of demanding impossible relative precision.
func closeTo(got, want, relTol float64) bool {
	if got == want {
		return true
	}
	if math.IsNaN(got) || math.IsNaN(want) {
		return false
	}
	scale := math.Max(1, math.Max(math.Abs(got), math.Abs(want)))
	return math.Abs(got-want) <= relTol*scale
}

func assertClose(t *testing.T, what string, got, want, relTol float64) {
	t.Helper()
	if !closeTo(got, want, relTol) {
		t.Errorf("%s = %.17g, want %.17g (|diff| = %.3g, tolerance %.3g)",
			what, got, want, math.Abs(got-want), relTol)
	}
}

// -----------------------------------------------------------------------------
// The standard price table
// -----------------------------------------------------------------------------

// price is one standard sportsbook price written four ways.
//
// The expectations are stated as the mechanics of the wager — risk `stake` units to
// win `profit` units — and the decimal and probability columns are *derived* from
// those two numbers by an arithmetic path that is deliberately different from the
// one the code under test uses. The code computes 1 + 100/|A|; this table computes
// (stake+profit)/stake. Those are equal mathematically but are different float
// operations, so agreement is evidence about the implementation rather than a
// restatement of it.
//
// Every row is a price in the standard published form: the -110 point-spread juice,
// the -105 reduced-juice variant, the even-money and ladder rungs, and longshots out
// to the representable ceiling. None of it is invented market data — these are
// format identities, not quotes attributed to any event.
type price struct {
	name     string
	stake    float64 // units risked
	profit   float64 // units won, on top of the returned stake
	american int64
	fracNum  int64
	fracDen  int64
	// noFraction marks a price whose profit/stake ratio needs a denominator beyond
	// MaxFractionalDenominator, so it has no fractional rendering at all.
	noFraction bool
}

// decimal is the total return per unit staked.
func (p price) decimal() float64 { return (p.stake + p.profit) / p.stake }

// probability is the implied probability: the stake as a fraction of the return.
func (p price) probability() float64 { return p.stake / (p.stake + p.profit) }

var standardPrices = []price{
	{name: "-110 standard point-spread juice", stake: 110, profit: 100, american: -110, fracNum: 10, fracDen: 11},
	{name: "-105 reduced juice", stake: 105, profit: 100, american: -105, fracNum: 20, fracDen: 21},
	{name: "+100 even money", stake: 100, profit: 100, american: 100, fracNum: 1, fracDen: 1},
	{name: "+110", stake: 100, profit: 110, american: 110, fracNum: 11, fracDen: 10},
	{name: "-125", stake: 125, profit: 100, american: -125, fracNum: 4, fracDen: 5},
	{name: "-150", stake: 150, profit: 100, american: -150, fracNum: 2, fracDen: 3},
	{name: "+150", stake: 100, profit: 150, american: 150, fracNum: 3, fracDen: 2},
	{name: "-200", stake: 200, profit: 100, american: -200, fracNum: 1, fracDen: 2},
	{name: "+200", stake: 100, profit: 200, american: 200, fracNum: 2, fracDen: 1},
	{name: "-250", stake: 250, profit: 100, american: -250, fracNum: 2, fracDen: 5},
	{name: "+250", stake: 100, profit: 250, american: 250, fracNum: 5, fracDen: 2},
	{name: "-275", stake: 275, profit: 100, american: -275, fracNum: 4, fracDen: 11},
	{name: "-500 heavy favourite (1/5)", stake: 500, profit: 100, american: -500, fracNum: 1, fracDen: 5},
	{name: "+500", stake: 100, profit: 500, american: 500, fracNum: 5, fracDen: 1},
	{name: "-1000", stake: 1000, profit: 100, american: -1000, fracNum: 1, fracDen: 10},
	{name: "+1000", stake: 100, profit: 1000, american: 1000, fracNum: 10, fracDen: 1},
	{name: "+2500 longshot (25/1)", stake: 100, profit: 2500, american: 2500, fracNum: 25, fracDen: 1},
	{name: "-10000", stake: 10000, profit: 100, american: -10000, fracNum: 1, fracDen: 100},
	{name: "+10000 (100/1)", stake: 100, profit: 10000, american: 10000, fracNum: 100, fracDen: 1},
	{name: "+1000000 representable ceiling", stake: 100, profit: 1_000_000, american: 1_000_000, fracNum: 10000, fracDen: 1},
	{
		name: "-1000000 representable floor", stake: 1_000_000, profit: 100, american: -1_000_000,
		noFraction: true, // 1/10000 needs a denominator ten times the supported bound
	},
}

// -----------------------------------------------------------------------------
// Conversions across the standard table
// -----------------------------------------------------------------------------

func TestAmericanConversions(t *testing.T) {
	for _, row := range standardPrices {
		t.Run(row.name, func(t *testing.T) {
			a, err := NewAmerican(row.american)
			if err != nil {
				t.Fatalf("NewAmerican(%d) returned %v", row.american, err)
			}

			d, err := a.Decimal()
			if err != nil {
				t.Fatalf("American(%d).Decimal() returned %v", row.american, err)
			}
			assertClose(t, "decimal", float64(d), row.decimal(), relTolExact)

			p, err := a.Probability()
			if err != nil {
				t.Fatalf("American(%d).Probability() returned %v", row.american, err)
			}
			assertClose(t, "probability", float64(p), row.probability(), relTolExact)

			f, err := a.Fractional()
			switch {
			case row.noFraction:
				if !errors.Is(err, ErrFractionalNotRepresentable) {
					t.Fatalf("American(%d).Fractional() = %v, %v; want ErrFractionalNotRepresentable",
						row.american, f, err)
				}
			case err != nil:
				t.Fatalf("American(%d).Fractional() returned %v", row.american, err)
			case f.Numerator != row.fracNum || f.Denominator != row.fracDen:
				t.Fatalf("American(%d).Fractional() = %d/%d, want %d/%d",
					row.american, f.Numerator, f.Denominator, row.fracNum, row.fracDen)
			}
		})
	}
}

func TestDecimalConversions(t *testing.T) {
	for _, row := range standardPrices {
		t.Run(row.name, func(t *testing.T) {
			d, err := NewDecimal(row.decimal())
			if err != nil {
				t.Fatalf("NewDecimal(%.17g) returned %v", row.decimal(), err)
			}

			a, err := d.American()
			if err != nil {
				t.Fatalf("Decimal(%.17g).American() returned %v", row.decimal(), err)
			}
			if int64(a) != row.american {
				t.Errorf("Decimal(%.17g).American() = %d, want %d", row.decimal(), int64(a), row.american)
			}

			p, err := d.Probability()
			if err != nil {
				t.Fatalf("Decimal(%.17g).Probability() returned %v", row.decimal(), err)
			}
			assertClose(t, "probability", float64(p), row.probability(), relTolExact)

			f, err := d.Fractional()
			switch {
			case row.noFraction:
				if !errors.Is(err, ErrFractionalNotRepresentable) {
					t.Fatalf("Decimal(%.17g).Fractional() = %v, %v; want ErrFractionalNotRepresentable",
						row.decimal(), f, err)
				}
			case err != nil:
				t.Fatalf("Decimal(%.17g).Fractional() returned %v", row.decimal(), err)
			case f.Numerator != row.fracNum || f.Denominator != row.fracDen:
				t.Fatalf("Decimal(%.17g).Fractional() = %d/%d, want %d/%d",
					row.decimal(), f.Numerator, f.Denominator, row.fracNum, row.fracDen)
			}
		})
	}
}

func TestProbabilityConversions(t *testing.T) {
	for _, row := range standardPrices {
		t.Run(row.name, func(t *testing.T) {
			p, err := NewProbability(row.probability())
			if err != nil {
				t.Fatalf("NewProbability(%.17g) returned %v", row.probability(), err)
			}

			d, err := p.Decimal()
			if err != nil {
				t.Fatalf("Probability(%.17g).Decimal() returned %v", row.probability(), err)
			}
			assertClose(t, "decimal", float64(d), row.decimal(), relTolChain)

			a, err := p.American()
			if err != nil {
				t.Fatalf("Probability(%.17g).American() returned %v", row.probability(), err)
			}
			if int64(a) != row.american {
				t.Errorf("Probability(%.17g).American() = %d, want %d", row.probability(), int64(a), row.american)
			}

			f, err := p.Fractional()
			switch {
			case row.noFraction:
				if !errors.Is(err, ErrFractionalNotRepresentable) {
					t.Fatalf("Probability(%.17g).Fractional() = %v, %v; want ErrFractionalNotRepresentable",
						row.probability(), f, err)
				}
			case err != nil:
				t.Fatalf("Probability(%.17g).Fractional() returned %v", row.probability(), err)
			case f.Numerator != row.fracNum || f.Denominator != row.fracDen:
				t.Fatalf("Probability(%.17g).Fractional() = %d/%d, want %d/%d",
					row.probability(), f.Numerator, f.Denominator, row.fracNum, row.fracDen)
			}
		})
	}
}

func TestFractionalConversions(t *testing.T) {
	for _, row := range standardPrices {
		if row.noFraction {
			continue
		}
		t.Run(row.name, func(t *testing.T) {
			f, err := NewFractional(row.fracNum, row.fracDen)
			if err != nil {
				t.Fatalf("NewFractional(%d, %d) returned %v", row.fracNum, row.fracDen, err)
			}

			d, err := f.Decimal()
			if err != nil {
				t.Fatalf("Fractional(%s).Decimal() returned %v", f, err)
			}
			assertClose(t, "decimal", float64(d), row.decimal(), relTolExact)

			p, err := f.Probability()
			if err != nil {
				t.Fatalf("Fractional(%s).Probability() returned %v", f, err)
			}
			assertClose(t, "probability", float64(p), row.probability(), relTolExact)

			a, err := f.American()
			if err != nil {
				t.Fatalf("Fractional(%s).American() returned %v", f, err)
			}
			if int64(a) != row.american {
				t.Errorf("Fractional(%s).American() = %d, want %d", f, int64(a), row.american)
			}
		})
	}
}

// TestImpliedProbabilityOfStandardJuice pins the single number every sportsbook
// engineer knows by heart, as an independent cross-check that p = 1/d is the right
// way round. A two-sided -110 market has an overround of 4.76% and a hold of 4.55%;
// if the reciprocal were inverted, both would come out negative.
func TestImpliedProbabilityOfStandardJuice(t *testing.T) {
	a, err := NewAmerican(-110)
	if err != nil {
		t.Fatalf("NewAmerican(-110) returned %v", err)
	}
	p, err := a.Probability()
	if err != nil {
		t.Fatalf("Probability() returned %v", err)
	}
	assertClose(t, "implied probability of -110", float64(p), 0.5238095238095238, relTolExact)

	overround := 2 * float64(p)
	assertClose(t, "overround of a -110/-110 market", overround, 1.0476190476190477, relTolExact)
	assertClose(t, "hold of a -110/-110 market", 1-1/overround, 0.045454545454545456, relTolExact)
}

// -----------------------------------------------------------------------------
// Round trips: what is actually true, not what would be convenient
// -----------------------------------------------------------------------------

// TestAmericanDecimalRoundTripIsExhaustivelyExact enumerates every legal canonical
// American price and asserts that American → Decimal → American is the identity.
// This is a proof by exhaustion over the whole domain, not a sample.
func TestAmericanDecimalRoundTripIsExhaustivelyExact(t *testing.T) {
	checked := 0
	for magnitude := MinAmericanMagnitude; magnitude <= MaxAmericanMagnitude; magnitude++ {
		for _, v := range [...]int64{magnitude, -magnitude} {
			want, err := NewAmerican(v)
			if err != nil {
				t.Fatalf("NewAmerican(%d) returned %v", v, err)
			}
			d, err := want.Decimal()
			if err != nil {
				t.Fatalf("American(%d).Decimal() returned %v", v, err)
			}
			got, err := d.American()
			if err != nil {
				t.Fatalf("American(%d) -> decimal %.17g -> American returned %v", v, float64(d), err)
			}
			if got != want {
				t.Fatalf("round trip American(%d) -> decimal %.17g -> American(%d); want %d",
					v, float64(d), int64(got), int64(want))
			}
			checked++
		}
	}
	// -100 is folded onto +100 by NewAmerican, so the canonical domain has one
	// fewer member than the raw legal range.
	wantChecked := 2 * int(MaxAmericanMagnitude-MinAmericanMagnitude+1)
	if checked != wantChecked {
		t.Fatalf("enumerated %d prices, expected %d", checked, wantChecked)
	}
}

// TestMinusOneHundredIsTheOnlyRoundTripException documents the one place the
// American round trip is not the identity, rather than hiding it. +100 and -100 are
// the same price; the canonical spelling is +100.
func TestMinusOneHundredIsTheOnlyRoundTripException(t *testing.T) {
	raw := American(-100)
	if err := raw.Validate(); err != nil {
		t.Fatalf("American(-100) should be a valid input price, got %v", err)
	}

	d, err := raw.Decimal()
	if err != nil {
		t.Fatalf("American(-100).Decimal() returned %v", err)
	}
	if float64(d) != 2.0 {
		t.Fatalf("American(-100).Decimal() = %.17g, want exactly 2.0", float64(d))
	}

	back, err := d.American()
	if err != nil {
		t.Fatalf("Decimal(2.0).American() returned %v", err)
	}
	if back != American(100) {
		t.Fatalf("Decimal(2.0).American() = %d, want +100", int64(back))
	}
	if raw.Canonical() != American(100) {
		t.Fatalf("American(-100).Canonical() = %d, want +100", int64(raw.Canonical()))
	}

	// +100 is the fixed point; every other price is its own canonical form.
	for _, v := range []int64{-101, -110, 100, 150, 1_000_000, -1_000_000} {
		if American(v).Canonical() != American(v) {
			t.Errorf("American(%d).Canonical() changed the value; only -100 should be folded", v)
		}
	}
}

// TestDecimalAmericanRoundTripErrorIsBounded asserts the *provable* bound rather
// than pretending the round trip is exact. American odds are integers, so almost no
// decimal survives the trip unchanged; what survives is the 0.005 error bound
// derived in the package documentation.
func TestDecimalAmericanRoundTripErrorIsBounded(t *testing.T) {
	// Sweep the representable decimal range on a deterministic grid: dense where
	// the branch boundary and the rounding are most awkward (just either side of
	// 2.0), then geometric out to the ceiling.
	var samples []float64
	for i := 0; i <= 2000; i++ {
		samples = append(samples, 1.0001+float64(i)*(1.0-0.0001)/2000) // 1.0001 .. ~2.0
	}
	for i := 0; i <= 2000; i++ {
		samples = append(samples, 2.0+float64(i)*(10001.0-2.0)/2000) // 2.0 .. 10001
	}
	// Values that sit exactly on a rounding tie, where half-away-from-zero decides.
	samples = append(samples, 2.005, 1.995, 2.0, 1.9999999, 2.0000001, 10001.0, 1.0001)

	worst := 0.0
	for _, v := range samples {
		d, err := NewDecimal(v)
		if err != nil {
			t.Fatalf("NewDecimal(%.17g) returned %v", v, err)
		}
		a, err := d.American()
		if err != nil {
			t.Fatalf("Decimal(%.17g).American() returned %v", v, err)
		}
		back, err := a.Decimal()
		if err != nil {
			t.Fatalf("American(%d).Decimal() returned %v", int64(a), err)
		}
		diff := math.Abs(float64(back) - v)
		if diff > worst {
			worst = diff
		}
		if diff > decimalRoundTripBound+decimalRoundTripSlack {
			t.Fatalf("decimal %.17g -> american %d -> decimal %.17g: error %.6g exceeds the %.6g bound",
				v, int64(a), float64(back), diff, decimalRoundTripBound)
		}
	}
	// The bound must be tight, not merely true: a bound nobody approaches would
	// pass even if the rounding were broken in the safe direction.
	if worst < decimalRoundTripBound/2 {
		t.Errorf("worst observed round-trip error %.6g is far below the %.6g bound; "+
			"either the sweep misses the worst case or the bound is not tight",
			worst, decimalRoundTripBound)
	}
}

// TestDecimalProbabilityRoundTrip checks the two-division round trip to the
// precision it actually has. It is not bit-exact and is not asserted to be.
func TestDecimalProbabilityRoundTrip(t *testing.T) {
	for i := 1; i < 10000; i++ {
		v := 1.0 + float64(i)/1000 // 1.001 .. ~11
		d, err := NewDecimal(v)
		if err != nil {
			t.Fatalf("NewDecimal(%.17g) returned %v", v, err)
		}
		p, err := d.Probability()
		if err != nil {
			t.Fatalf("Decimal(%.17g).Probability() returned %v", v, err)
		}
		if float64(p) <= 0 || float64(p) >= 1 {
			t.Fatalf("Decimal(%.17g).Probability() = %.17g, want strictly inside (0, 1)", v, float64(p))
		}
		back, err := p.Decimal()
		if err != nil {
			t.Fatalf("Probability(%.17g).Decimal() returned %v", float64(p), err)
		}
		assertClose(t, "decimal round trip", float64(back), v, relTolExact)
	}
}

// TestFractionalRoundTripIsExactWithinBounds enumerates fractions in lowest terms
// and asserts Fractional → Decimal → Fractional is the identity for every one of
// them. This is the guarantee the package documentation claims; it is checked over
// the whole supported denominator range rather than sampled.
func TestFractionalRoundTripIsExactWithinBounds(t *testing.T) {
	checked := 0
	for den := int64(1); den <= MaxFractionalDenominator; den++ {
		for num := int64(1); num <= 300; num++ {
			if gcd(num, den) != 1 {
				continue // not in lowest terms; NewFractional would reduce it away
			}
			f := Fractional{Numerator: num, Denominator: den}
			d, err := f.Decimal()
			if err != nil {
				t.Fatalf("Fractional(%s).Decimal() returned %v", f, err)
			}
			back, err := d.Fractional()
			if err != nil {
				t.Fatalf("Fractional(%s) -> decimal %.17g -> Fractional returned %v", f, float64(d), err)
			}
			if back != f {
				t.Fatalf("round trip Fractional(%s) -> decimal %.17g -> Fractional(%s)", f, float64(d), back)
			}
			checked++
		}
	}
	if checked < 100_000 {
		t.Fatalf("only %d fractions enumerated; the sweep is not covering the domain", checked)
	}
}

// TestFractionalApproximationGuarantee asserts the properties the continued
// fraction actually promises, over decimals that mostly have no exact small
// fraction: lowest terms, a denominator inside the bound, and the classic
// convergent error bound |x - p/q| < 1/q².
func TestFractionalApproximationGuarantee(t *testing.T) {
	for i := 1; i <= 20000; i++ {
		// An irrational-ish sweep: the golden ratio step never lands on a short
		// rational, so this exercises the approximation path rather than the exact one.
		v := 1.001 + math.Mod(float64(i)*0.6180339887498949, 1)*8
		d, err := NewDecimal(v)
		if err != nil {
			t.Fatalf("NewDecimal(%.17g) returned %v", v, err)
		}
		f, gotErr, err := d.FractionalApprox()
		if err != nil {
			t.Fatalf("Decimal(%.17g).FractionalApprox() returned %v", v, err)
		}
		if err := f.Validate(); err != nil {
			t.Fatalf("Decimal(%.17g).FractionalApprox() produced invalid %s: %v", v, f, err)
		}
		if f.Denominator > MaxFractionalDenominator {
			t.Fatalf("Decimal(%.17g) produced denominator %d, above the %d bound",
				v, f.Denominator, MaxFractionalDenominator)
		}
		if g := gcd(f.Numerator, f.Denominator); g != 1 {
			t.Fatalf("Decimal(%.17g) produced %s, not in lowest terms (gcd %d)", v, f, g)
		}

		x := float64(d) - 1
		actual := math.Abs(float64(f.Numerator)/float64(f.Denominator) - x)
		assertClose(t, "reported approximation error", gotErr, actual, relTolChain)

		// Every convergent p/q of x satisfies |x - p/q| < 1/(q·q_next) ≤ 1/q².
		// The relative slack absorbs float noise in the term extraction only; at
		// 1e-9 it is far too small to hide a broken recurrence.
		q := float64(f.Denominator)
		if bound := 1 / (q * q); actual > bound*(1+1e-9) {
			t.Fatalf("Decimal(%.17g) -> %s: error %.6g violates the convergent bound 1/q² = %.6g",
				v, f, actual, bound)
		}
	}
}

// -----------------------------------------------------------------------------
// Fractions whose American form is not an integer
// -----------------------------------------------------------------------------

// TestLossyFractionalToAmerican covers ladder rungs that do not land on a whole
// American price, so the rounding convention is visible rather than incidental.
func TestLossyFractionalToAmerican(t *testing.T) {
	cases := []struct {
		num, den int64
		// exactAmerican is the real-valued American price, computed from the
		// mechanics: stake `den` to win `num` is stake 100·den/num to win 100.
		exactAmerican float64
		want          int64
	}{
		{num: 8, den: 13, exactAmerican: -162.5, want: -163}, // ties round away from zero
		{num: 8, den: 15, exactAmerican: -187.5, want: -188}, // ties round away from zero
		{num: 13, den: 8, exactAmerican: 162.5, want: 163},   // and symmetrically on the dog side
		{num: 15, den: 8, exactAmerican: 187.5, want: 188},   //
		{num: 10, den: 3, exactAmerican: 1000.0 / 3, want: 333},
		{num: 3, den: 10, exactAmerican: -1000.0 / 3, want: -333},
	}
	for _, c := range cases {
		f, err := NewFractional(c.num, c.den)
		if err != nil {
			t.Fatalf("NewFractional(%d, %d) returned %v", c.num, c.den, err)
		}
		// Cross-check the stated exact price against the wager mechanics.
		var mechanics float64
		if c.num >= c.den {
			mechanics = 100 * float64(c.num) / float64(c.den)
		} else {
			mechanics = -100 * float64(c.den) / float64(c.num)
		}
		assertClose(t, "table's exact american for "+f.String(), c.exactAmerican, mechanics, relTolExact)

		got, err := f.American()
		if err != nil {
			t.Fatalf("Fractional(%s).American() returned %v", f, err)
		}
		if int64(got) != c.want {
			t.Errorf("Fractional(%s).American() = %d, want %d (exact %.6g, rounded half away from zero)",
				f, int64(got), c.want, c.exactAmerican)
		}
	}
}

// -----------------------------------------------------------------------------
// Edge cases: every guarded division, every boundary
// -----------------------------------------------------------------------------

func TestAmericanEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want error // nil means valid
	}{
		{name: "zero is not a price", in: 0, want: ErrAmericanOutOfRange},
		{name: "one", in: 1, want: ErrAmericanOutOfRange},
		{name: "minus one", in: -1, want: ErrAmericanOutOfRange},
		{name: "ninety nine", in: 99, want: ErrAmericanOutOfRange},
		{name: "minus ninety nine", in: -99, want: ErrAmericanOutOfRange},
		{name: "plus one hundred is the lower boundary", in: 100},
		{name: "minus one hundred is the lower boundary", in: -100},
		{name: "plus one hundred and one", in: 101},
		{name: "minus one hundred and one", in: -101},
		{name: "positive ceiling", in: 1_000_000},
		{name: "negative ceiling", in: -1_000_000},
		{name: "one past the positive ceiling", in: 1_000_001, want: ErrAmericanOutOfRange},
		{name: "one past the negative ceiling", in: -1_000_001, want: ErrAmericanOutOfRange},
		{name: "max int64 does not overflow the check", in: math.MaxInt64, want: ErrAmericanOutOfRange},
		{name: "min int64 does not overflow the check", in: math.MinInt64, want: ErrAmericanOutOfRange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewAmerican(c.in)
			if c.want == nil {
				if err != nil {
					t.Fatalf("NewAmerican(%d) = %v, want no error", c.in, err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("NewAmerican(%d) = %v, want %v", c.in, err, c.want)
			}
			// A rejected value must not also produce a usable conversion.
			if _, err := American(c.in).Decimal(); !errors.Is(err, c.want) {
				t.Fatalf("American(%d).Decimal() = %v, want %v", c.in, err, c.want)
			}
		})
	}
}

func TestDecimalEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want error
	}{
		{name: "NaN is rejected, never propagated", in: math.NaN(), want: ErrNotFinite},
		{name: "positive infinity", in: math.Inf(1), want: ErrNotFinite},
		{name: "negative infinity", in: math.Inf(-1), want: ErrNotFinite},
		{name: "exactly one is a zero payout", in: 1.0, want: ErrDecimalOutOfRange},
		{name: "just below one", in: math.Nextafter(1, 0), want: ErrDecimalOutOfRange},
		{name: "zero", in: 0, want: ErrDecimalOutOfRange},
		{name: "negative", in: -1.5, want: ErrDecimalOutOfRange},
		{name: "smallest representable price above one", in: math.Nextafter(1, 2)},
		{name: "even money", in: 2.0},
		{name: "very long price", in: 1e12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewDecimal(c.in)
			if c.want == nil {
				if err != nil {
					t.Fatalf("NewDecimal(%v) = %v, want no error", c.in, err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("NewDecimal(%v) = %v, want %v", c.in, err, c.want)
			}
			if _, err := Decimal(c.in).Probability(); !errors.Is(err, c.want) {
				t.Fatalf("Decimal(%v).Probability() = %v, want %v", c.in, err, c.want)
			}
		})
	}
}

// TestDecimalTooShortOrTooLongForAmerican covers the two ends where a perfectly
// valid decimal price has no American representation, and confirms the guarded
// division never yields an infinity.
func TestDecimalTooShortOrTooLongForAmerican(t *testing.T) {
	cases := []struct {
		name string
		in   float64
	}{
		{name: "one ulp above one: the d-1 division must not blow up", in: math.Nextafter(1, 2)},
		{name: "far shorter than the ceiling allows", in: 1.00001},
		{name: "just inside the short end", in: 1.00009},
		{name: "far longer than the ceiling allows", in: 20000},
		{name: "absurdly long", in: 1e12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := NewDecimal(c.in)
			if err != nil {
				t.Fatalf("NewDecimal(%v) returned %v", c.in, err)
			}
			a, err := d.American()
			if !errors.Is(err, ErrAmericanOutOfRange) {
				t.Fatalf("Decimal(%v).American() = %d, %v; want ErrAmericanOutOfRange", c.in, int64(a), err)
			}
			if a != 0 {
				t.Errorf("Decimal(%v).American() returned %d alongside an error; failed conversions must return the zero value",
					c.in, int64(a))
			}
		})
	}

	// The boundary itself must still convert.
	for _, v := range []float64{1.0001, 10001} {
		d, err := NewDecimal(v)
		if err != nil {
			t.Fatalf("NewDecimal(%v) returned %v", v, err)
		}
		if _, err := d.American(); err != nil {
			t.Errorf("Decimal(%v).American() returned %v; the ceiling itself must be representable", v, err)
		}
	}
}

func TestProbabilityEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		// validErr is the error from NewProbability; nil means the value is a
		// legitimate probability.
		validErr error
		// priceErr is the error from Probability.Decimal; nil means it is priceable.
		priceErr error
	}{
		{name: "NaN", in: math.NaN(), validErr: ErrNotFinite, priceErr: ErrNotFinite},
		{name: "positive infinity", in: math.Inf(1), validErr: ErrNotFinite, priceErr: ErrNotFinite},
		{name: "negative infinity", in: math.Inf(-1), validErr: ErrNotFinite, priceErr: ErrNotFinite},
		{name: "below zero", in: -0.0001, validErr: ErrProbabilityOutOfRange, priceErr: ErrProbabilityOutOfRange},
		{name: "above one", in: 1.0001, validErr: ErrProbabilityOutOfRange, priceErr: ErrProbabilityOutOfRange},
		{name: "exactly zero is a valid probability but not a price", in: 0, priceErr: ErrProbabilityNotPriceable},
		{name: "exactly one is a valid probability but not a price", in: 1, priceErr: ErrProbabilityNotPriceable},
		{
			// 1/(1-2⁻⁵³) = 1 + 2⁻⁵³ + 2⁻¹⁰⁶ + …, which sits just ABOVE the midpoint
			// between 1.0 and the next double up, so it rounds to 1+2⁻⁵² rather than
			// onto 1.0. Priceable, if only barely: the thinnest price the type can
			// express. No double below 1 has a reciprocal of exactly 1.0.
			name: "one ulp below one is priceable, at the thinnest representable price",
			in:   math.Nextafter(1, 0),
		},
		{
			// The reciprocal overflows: 1/5e-324 is +Inf, so there is no
			// representable price. The low end of (0,1) is the only end that can
			// fail this way — anything under ~5.6e-309 (1/math.MaxFloat64) does.
			name:     "smallest positive subnormal is a valid probability but overflows to an unrepresentable price",
			in:       math.SmallestNonzeroFloat64,
			priceErr: ErrProbabilityNotPriceable,
		},
		{name: "even money", in: 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewProbability(c.in)
			if c.validErr == nil {
				if err != nil {
					t.Fatalf("NewProbability(%v) = %v, want no error", c.in, err)
				}
			} else if !errors.Is(err, c.validErr) {
				t.Fatalf("NewProbability(%v) = %v, want %v", c.in, err, c.validErr)
			}

			d, err := Probability(c.in).Decimal()
			if c.priceErr == nil {
				if err != nil {
					t.Fatalf("Probability(%v).Decimal() = %v, want no error", c.in, err)
				}
				if err := d.Validate(); err != nil {
					t.Fatalf("Probability(%v).Decimal() produced the invalid decimal %v: %v", c.in, float64(d), err)
				}
				return
			}
			if !errors.Is(err, c.priceErr) {
				t.Fatalf("Probability(%v).Decimal() = %v, %v; want %v", c.in, float64(d), err, c.priceErr)
			}
			if d != 0 {
				t.Errorf("Probability(%v).Decimal() returned %v alongside an error", c.in, float64(d))
			}
		})
	}
}

func TestFractionalEdgeCases(t *testing.T) {
	cases := []struct {
		name     string
		num, den int64
		want     error
	}{
		{name: "zero value struct is invalid", want: ErrFractionalNumerator},
		{name: "zero numerator returns the stake and nothing more", num: 0, den: 1, want: ErrFractionalNumerator},
		{name: "zero denominator is a division by zero", num: 1, den: 0, want: ErrFractionalDenominator},
		{name: "negative numerator", num: -1, den: 2, want: ErrFractionalNumerator},
		{name: "negative denominator", num: 1, den: -2, want: ErrFractionalDenominator},
		{name: "both negative", num: -1, den: -2, want: ErrFractionalNumerator},
		{name: "min int64 numerator does not overflow", num: math.MinInt64, den: 1, want: ErrFractionalNumerator},
		{name: "min int64 denominator does not overflow", num: 1, den: math.MinInt64, want: ErrFractionalDenominator},
		{name: "even money", num: 1, den: 1},
		{name: "max int64 numerator is a valid, absurd price", num: math.MaxInt64, den: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewFractional(c.num, c.den)
			if c.want == nil {
				if err != nil {
					t.Fatalf("NewFractional(%d, %d) = %v, want no error", c.num, c.den, err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("NewFractional(%d, %d) = %v, want %v", c.num, c.den, err, c.want)
			}
			if _, err := (Fractional{c.num, c.den}).Decimal(); !errors.Is(err, c.want) {
				t.Fatalf("Fractional(%d/%d).Decimal() = %v, want %v", c.num, c.den, err, c.want)
			}
		})
	}
}

// TestFractionalTooShortToBeADecimal covers the case where the ratio is small
// enough that 1 + n/d rounds straight back to 1.0.
func TestFractionalTooShortToBeADecimal(t *testing.T) {
	f := Fractional{Numerator: 1, Denominator: math.MaxInt64}
	if err := f.Validate(); err != nil {
		t.Fatalf("Fractional(%s) should be structurally valid: %v", f, err)
	}
	if _, err := f.Decimal(); !errors.Is(err, ErrDecimalOutOfRange) {
		t.Fatalf("Fractional(%s).Decimal() = %v, want ErrDecimalOutOfRange", f, err)
	}
}

func TestReduceAndCanonicalise(t *testing.T) {
	cases := []struct {
		inNum, inDen     int64
		wantNum, wantDen int64
	}{
		{6, 4, 3, 2},         // the traditional 6/4 is stored as 3/2
		{4, 6, 2, 3},         // and 4/6 as 2/3
		{100, 275, 4, 11},    // -275
		{2000, 2100, 20, 21}, // -105
		{10, 11, 10, 11},     // already reduced
		{1, 1, 1, 1},
		{300, 100, 3, 1},
	}
	for _, c := range cases {
		got, err := NewFractional(c.inNum, c.inDen)
		if err != nil {
			t.Fatalf("NewFractional(%d, %d) returned %v", c.inNum, c.inDen, err)
		}
		if got.Numerator != c.wantNum || got.Denominator != c.wantDen {
			t.Errorf("NewFractional(%d, %d) = %s, want %d/%d", c.inNum, c.inDen, got, c.wantNum, c.wantDen)
		}
	}

	// Reduce must be total: it never panics and never fails on an invalid value.
	for _, f := range []Fractional{{}, {0, 0}, {-6, 4}, {6, -4}, {math.MinInt64, math.MinInt64}} {
		if got := f.Reduce(); got != f {
			t.Errorf("Fractional{%d, %d}.Reduce() = %s; invalid values must be returned unchanged",
				f.Numerator, f.Denominator, got)
		}
	}
}

// -----------------------------------------------------------------------------
// Invariants over the whole valid domain
// -----------------------------------------------------------------------------

// TestNoConversionEverReturnsNonFinite is the poison check: no successful
// conversion may return NaN or ±Inf, on any input at all, valid or not.
func TestNoConversionEverReturnsNonFinite(t *testing.T) {
	decimals := []float64{
		math.NaN(), math.Inf(1), math.Inf(-1), -1, 0, 1,
		math.Nextafter(1, 2), 1.0000001, 1.01, 1.5, 2, 2.5, 26, 10001, 1e12, 1e300, math.MaxFloat64,
	}
	for _, v := range decimals {
		d := Decimal(v)
		if p, err := d.Probability(); err == nil && (math.IsNaN(float64(p)) || math.IsInf(float64(p), 0)) {
			t.Errorf("Decimal(%v).Probability() succeeded with the non-finite result %v", v, float64(p))
		}
		// American is an integer, so there is no non-finite value to leak; the
		// equivalent poison check is that a success is always inside the legal set.
		if a, err := d.American(); err == nil {
			if err := a.Validate(); err != nil {
				t.Errorf("Decimal(%v).American() succeeded with the illegal price %d", v, int64(a))
			}
		}
		if f, err := d.Fractional(); err == nil {
			if err := f.Validate(); err != nil {
				t.Errorf("Decimal(%v).Fractional() succeeded with the invalid fraction %s", v, f)
			}
		}
		if f, e, err := d.FractionalApprox(); err == nil && (math.IsNaN(e) || math.IsInf(e, 0)) {
			t.Errorf("Decimal(%v).FractionalApprox() reported the non-finite error %v for %s", v, e, f)
		}
	}

	probabilities := []float64{
		// math.Copysign(0, -1) rather than the literal -0.0: in Go the literal is
		// constant-folded to positive zero (staticcheck SA4026), so it would not
		// actually exercise the negative-zero case this table exists to cover.
		math.NaN(), math.Inf(1), math.Inf(-1), -1, math.Copysign(0, -1), 0, math.SmallestNonzeroFloat64,
		1e-300, 0.0001, 0.5, 0.9999, math.Nextafter(1, 0), 1, 1.5,
	}
	for _, v := range probabilities {
		p := Probability(v)
		if d, err := p.Decimal(); err == nil && (math.IsNaN(float64(d)) || math.IsInf(float64(d), 0)) {
			t.Errorf("Probability(%v).Decimal() succeeded with the non-finite result %v", v, float64(d))
		}
	}

	americans := []int64{
		math.MinInt64, -1_000_001, -1_000_000, -101, -100, -99, 0, 99, 100, 101, 1_000_000, 1_000_001, math.MaxInt64,
	}
	for _, v := range americans {
		a := American(v)
		if d, err := a.Decimal(); err == nil && (math.IsNaN(float64(d)) || math.IsInf(float64(d), 0)) {
			t.Errorf("American(%d).Decimal() succeeded with the non-finite result %v", v, float64(d))
		}
		if p, err := a.Probability(); err == nil && (math.IsNaN(float64(p)) || math.IsInf(float64(p), 0)) {
			t.Errorf("American(%d).Probability() succeeded with the non-finite result %v", v, float64(p))
		}
	}
}

// TestProbabilityIsMonotonicallyDecreasingInDecimal checks the direction of the
// relationship, which is the failure a sign or reciprocal error would produce and
// which no single-value test can catch.
func TestProbabilityIsMonotonicallyDecreasingInDecimal(t *testing.T) {
	prev := math.Inf(1)
	for i := 1; i <= 20000; i++ {
		v := 1.0 + float64(i)/2000 // 1.0005 .. 11
		d, err := NewDecimal(v)
		if err != nil {
			t.Fatalf("NewDecimal(%.17g) returned %v", v, err)
		}
		p, err := d.Probability()
		if err != nil {
			t.Fatalf("Decimal(%.17g).Probability() returned %v", v, err)
		}
		if float64(p) >= prev {
			t.Fatalf("probability did not decrease as decimal odds rose: at d = %.17g got p = %.17g, previous %.17g",
				v, float64(p), prev)
		}
		prev = float64(p)
	}
}

// TestSafeMulAddRejectsOverflow exercises the guard that keeps the continued
// fraction recurrence inside int64.
func TestSafeMulAddRejectsOverflow(t *testing.T) {
	cases := []struct {
		a, b, c int64
		want    int64
		ok      bool
	}{
		{a: 0, b: 0, c: 0, want: 0, ok: true},
		{a: 3, b: 4, c: 5, want: 17, ok: true},
		{a: 0, b: math.MaxInt64, c: 7, want: 7, ok: true},
		{a: math.MaxInt64, b: 1, c: 0, want: math.MaxInt64, ok: true},
		{a: math.MaxInt64, b: 1, c: 1, ok: false},
		{a: math.MaxInt64, b: 2, c: 0, ok: false},
		{a: 1 << 40, b: 1 << 40, c: 0, ok: false},
		{a: -1, b: 2, c: 3, ok: false},
		{a: 1, b: -2, c: 3, ok: false},
		{a: 1, b: 2, c: -3, ok: false},
	}
	for _, c := range cases {
		got, ok := safeMulAdd(c.a, c.b, c.c)
		if ok != c.ok {
			t.Errorf("safeMulAdd(%d, %d, %d) ok = %v, want %v", c.a, c.b, c.c, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("safeMulAdd(%d, %d, %d) = %d, want %d", c.a, c.b, c.c, got, c.want)
		}
	}
}

// TestBestRationalApproximationRejectsGarbage confirms the helper degrades to the
// invalid zero-numerator fraction rather than panicking or returning nonsense.
func TestBestRationalApproximationRejectsGarbage(t *testing.T) {
	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, -0.5} {
		got := bestRationalApproximation(x, MaxFractionalDenominator, MaxFractionalNumerator, fractionalTolerance)
		if got.Valid() {
			t.Errorf("bestRationalApproximation(%v) = %s, want an invalid fraction the caller rejects", x, got)
		}
	}
}
