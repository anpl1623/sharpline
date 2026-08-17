package odds

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"pgregory.net/rapid"
)

// Conventions for this file are stated at the top of ev_test.go: closeTo,
// assertClose, relTolExact and relTolChain are shared with convert_test.go, and
// exact == appears only where exactness is the property under test and is proven
// in the doc comment of the function under test.

// -----------------------------------------------------------------------------
// Kelly, by hand
// -----------------------------------------------------------------------------

func TestKellyWorkedExamples(t *testing.T) {
	cases := []struct {
		name string
		q    float64
		d    float64
		want float64
	}{
		{
			// The canonical textbook case, and the one to check first.
			//   f* = (0.55 x 2 - 1)/(2 - 1) = 0.10/1 = 0.10
			// A 55% shot at even money justifies staking a tenth of the bankroll.
			"55% at +100 stakes a tenth", 0.55, 2, 0.10,
		},
		{
			// d = 210/110 = 21/11.
			//   numerator   = 0.6 x 21/11 - 1 = 12.6/11 - 11/11 = 1.6/11
			//   denominator = 21/11 - 1                          = 10/11
			//   f*          = (1.6/11) / (10/11) = 1.6/10 = 0.16
			"60% at -110 stakes 16%", 0.6, 210.0 / 110.0, 0.16,
		},
		{
			// +350 is d = 4.5.
			//   f* = (0.25 x 4.5 - 1)/(4.5 - 1) = 0.125/3.5 = 125/3500 = 1/28
			// Same 12.5% edge as the "55% at -110" row carries 5%, yet the stake is
			// far smaller: a long price collects the edge with much more variance.
			"25% at +350 stakes one twenty-eighth", 0.25, 4.5, 1.0 / 28.0,
		},
		{
			// +1100 is d = 12.
			//   f* = (0.10 x 12 - 1)/(12 - 1) = 0.2/11 = 2/110 = 1/55
			"10% at +1100 stakes one fifty-fifth", 0.10, 12, 1.0 / 55.0,
		},
		{
			// -400 is d = 1.25.
			//   f* = (0.9 x 1.25 - 1)/(1.25 - 1) = 0.125/0.25 = 0.5
			// Half the bankroll on a single wager. Full Kelly on short prices is
			// enormous, which is the practical argument for the fractional variant.
			"90% at -400 stakes half the bankroll", 0.9, 1.25, 0.5,
		},
		{
			//   f* = (0.525 x 2 - 1)/(2 - 1) = 0.05/1 = 0.05
			"52.5% at +100 stakes a twentieth", 0.525, 2, 0.05,
		},
		{
			// A true coin flip at the standard juice is a losing bet, so the
			// correct stake is nothing at all -- not a small stake, and certainly
			// not a negative one.
			"50% at -110 is no bet", 0.5, 210.0 / 110.0, 0,
		},
		{
			// -400 implies exactly 0.8, so this is precisely break-even.
			"80% at -400 is exactly no bet", 0.8, 1.25, 0,
		},
		{
			// A stake on an outcome that cannot happen is never justified.
			"impossible outcome is no bet", 0, 2, 0,
		},
		{
			//   f* = (1 x 2 - 1)/(2 - 1) = 1/1 = 1
			// A certain winner justifies the whole bankroll. This is the correct
			// answer to the question asked and the reason a caller must never pass
			// a probability it is not genuinely certain of.
			"certain outcome stakes everything", 1, 2, 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := Kelly(fairProb(t, c.q), decOdds(t, c.d))
			if err != nil {
				t.Fatalf("Kelly: %v", err)
			}
			assertClose(t, "kelly fraction", f, c.want, relTolExact)
		})
	}
}

// TestKellyEqualsExpectedValueOverProfit checks the identity in Kelly's doc
// comment -- that the numerator of the Kelly fraction is exactly ExpectedValue --
// against every standard published price. It is the cheapest way to catch a
// transposed operand, because the two functions reach the numerator by different
// code paths.
func TestKellyEqualsExpectedValueOverProfit(t *testing.T) {
	for _, row := range standardPrices {
		d := decOdds(t, row.decimal())

		for _, qv := range []float64{0.05, 0.25, 0.5, 0.6, 0.8, 0.95, 0.999} {
			q := fairProb(t, qv)

			f, err := Kelly(q, d)
			if err != nil {
				t.Fatalf("Kelly: %v", err)
			}
			ev, err := ExpectedValue(q, d)
			if err != nil {
				t.Fatalf("ExpectedValue: %v", err)
			}

			want := 0.0
			if ev > 0 {
				want = ev / (float64(d) - 1)
			}
			assertClose(t, row.name+" kelly", f, want, relTolChain)
		}
	}
}

// -----------------------------------------------------------------------------
// The invariants CLAUDE.md §4 names
// -----------------------------------------------------------------------------

// TestKellyIsExactlyZeroAtTheBreakevenPrice is the "Kelly is zero at zero edge"
// invariant, asserted with no tolerance. See Kelly's doc comment for the proof
// that fl(d x fl(1/d)) <= 1 whenever the reciprocal is normal, which is what makes
// the gross-return guard fire.
//
// A tolerance here would be worse than useless: it would pass an implementation
// that recommended staking 1e-17 of a bankroll on a price carrying no edge, and
// the defect would only surface as an unexplained rounding of stakes far
// downstream.
func TestKellyIsExactlyZeroAtTheBreakevenPrice(t *testing.T) {
	prices := sweepDecimals(t)
	for _, d := range prices {
		q, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability(%v): %v", float64(d), err)
		}
		f, err := Kelly(q, d)
		if err != nil {
			t.Fatalf("Kelly(%v, %v): %v", float64(q), float64(d), err)
		}
		if f != 0 {
			t.Fatalf("Kelly(1/%v, %v) = %.17g, want exactly 0", float64(d), float64(d), f)
		}

		// Scaling zero by any multiplier is still zero, so fractional Kelly
		// inherits the invariant unchanged.
		for _, m := range []float64{FullKelly, HalfKelly, QuarterKelly, 0.001} {
			ff, err := FractionalKelly(q, d, m)
			if err != nil {
				t.Fatalf("FractionalKelly: %v", err)
			}
			if ff != 0 {
				t.Fatalf("FractionalKelly(1/%v, %v, %v) = %.17g, want exactly 0",
					float64(d), float64(d), m, ff)
			}
		}
	}
	t.Logf("checked %d prices, all exactly zero Kelly at their own break-even probability", len(prices))
}

// TestKellyAtTheSubnormalReciprocalBoundary documents the one gap in the proof
// honestly rather than hiding it behind a carefully chosen sweep range.
//
// Above roughly 4.5e307 the reciprocal 1/d is subnormal, the relative-error bound
// the proof rests on degrades to an absolute one, and fl(d x fl(1/d)) may land
// just above 1. The exact-zero invariant is therefore not claimed there. What is
// claimed, and asserted here, is that the consequence is nil: any fraction that
// survives is smaller than 1e-300 and multiplies every representable bankroll to
// zero minor units.
func TestKellyAtTheSubnormalReciprocalBoundary(t *testing.T) {
	for _, dv := range []float64{1e308, 1.5e308, math.MaxFloat64} {
		d := decOdds(t, dv)
		q, err := BreakevenProbability(d)
		if err != nil {
			t.Fatalf("BreakevenProbability(%v): %v", dv, err)
		}
		f, err := Kelly(q, d)
		if err != nil {
			t.Fatalf("Kelly(%v, %v): %v", float64(q), dv, err)
		}
		if f < 0 || f > 1e-300 {
			t.Fatalf("Kelly at d=%v returned %.17g, want 0 <= f <= 1e-300", dv, f)
		}
		// The property that actually protects the ledger: a bankroll at the
		// int64 ceiling still rounds down to a zero stake.
		if stake := math.Trunc(float64(math.MaxInt64) * f); stake != 0 {
			t.Fatalf("Kelly at d=%v stakes %v minor units of a maximal bankroll", dv, stake)
		}
	}
}

func TestKellyIsExactlyOneAtCertainty(t *testing.T) {
	// A certain winner justifies the whole bankroll, and the arithmetic delivers
	// exactly 1 rather than 0.9999999999999999: the numerator and denominator are
	// the identical expression fl(d - 1) when q is 1.
	certain := fairProb(t, 1)
	for _, d := range sweepDecimals(t) {
		f, err := Kelly(certain, d)
		if err != nil {
			t.Fatalf("Kelly(1, %v): %v", float64(d), err)
		}
		if f != 1 {
			t.Fatalf("Kelly(1, %v) = %.17g, want exactly 1", float64(d), f)
		}
	}
}

func TestKellyNeverLeavesTheUnitInterval(t *testing.T) {
	probabilities := []float64{
		0, math.SmallestNonzeroFloat64, 1e-300, 1e-9, 0.01, 0.25, 0.5, 0.5238095238095238,
		0.75, 0.9, 0.999, 1 - 1e-12, math.Nextafter(1, 0), 1,
	}

	checked := 0
	for _, d := range sweepDecimals(t) {
		for _, qv := range probabilities {
			f, err := Kelly(Probability(qv), d)
			if err != nil {
				t.Fatalf("Kelly(%v, %v): %v", qv, float64(d), err)
			}
			if math.IsNaN(f) || math.IsInf(f, 0) {
				t.Fatalf("Kelly(%v, %v) = %v, a non-finite fraction", qv, float64(d), f)
			}
			if f < 0 || f > 1 {
				t.Fatalf("Kelly(%v, %v) = %.17g, outside [0, 1]", qv, float64(d), f)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("swept nothing; the guard would pass vacuously")
	}
	t.Logf("checked %d (q, d) pairs, every fraction inside [0, 1]", checked)
}

func TestKellyIsPositiveExactlyWhenExpectedValueIs(t *testing.T) {
	// "Stake something" and "this price is +EV" must be the same predicate. If
	// they ever disagree the staking layer either passes on a live edge or backs a
	// losing price, and both failures are silent.
	for _, d := range sweepDecimals(t) {
		for _, qv := range []float64{0, 0.1, 0.3, 0.5, 0.52, 0.7, 0.9, 1} {
			q := Probability(qv)

			f, err := Kelly(q, d)
			if err != nil {
				t.Fatalf("Kelly: %v", err)
			}
			ev, err := ExpectedValue(q, d)
			if err != nil {
				t.Fatalf("ExpectedValue: %v", err)
			}
			if (f > 0) != (ev > 0) {
				t.Fatalf("q=%v d=%v: kelly %.17g and EV %.17g disagree on whether to bet",
					qv, float64(d), f, ev)
			}
		}
	}
}

func TestKellyIsNonDecreasingInProbability(t *testing.T) {
	// Believing an outcome more likely can never justify a smaller stake. Below
	// break-even the fraction is pinned at zero, so the relation is
	// non-decreasing rather than strictly increasing.
	for _, d := range []float64{1.05, 1.25, 210.0 / 110.0, 2, 4.5, 12, 101} {
		price := decOdds(t, d)
		prev := -1.0
		for i := 0; i <= 10000; i++ {
			f, err := Kelly(fairProb(t, float64(i)/10000), price)
			if err != nil {
				t.Fatalf("Kelly: %v", err)
			}
			if f < prev {
				t.Fatalf("d=%v: kelly fell from %.17g to %.17g as q rose to %v",
					d, prev, f, float64(i)/10000)
			}
			prev = f
		}
	}
}

// -----------------------------------------------------------------------------
// Fractional Kelly
// -----------------------------------------------------------------------------

func TestFractionalKellyScalesTheFullFraction(t *testing.T) {
	multipliers := []float64{FullKelly, HalfKelly, QuarterKelly, 0.75, 0.1, 0.01, 1e-9}

	for _, d := range []float64{1.25, 210.0 / 110.0, 2, 4.5, 12, 41} {
		price := decOdds(t, d)
		for _, qv := range []float64{0.05, 0.3, 0.5, 0.55, 0.7, 0.9, 1} {
			q := fairProb(t, qv)

			full, err := Kelly(q, price)
			if err != nil {
				t.Fatalf("Kelly: %v", err)
			}
			for _, m := range multipliers {
				got, err := FractionalKelly(q, price, m)
				if err != nil {
					t.Fatalf("FractionalKelly: %v", err)
				}
				assertClose(t, "fractional kelly", got, full*m, relTolExact)
				if got > full {
					t.Fatalf("q=%v d=%v m=%v: fractional kelly %.17g exceeds full kelly %.17g",
						qv, d, m, got, full)
				}
				if got < 0 || got > 1 {
					t.Fatalf("q=%v d=%v m=%v: fractional kelly %.17g outside [0, 1]", qv, d, m, got)
				}
			}
		}
	}
}

func TestFullKellyMultiplierIsTheIdentity(t *testing.T) {
	for _, d := range sweepDecimals(t) {
		for _, qv := range []float64{0, 0.25, 0.5, 0.9, 1} {
			q := Probability(qv)

			full, err := Kelly(q, d)
			if err != nil {
				t.Fatalf("Kelly: %v", err)
			}
			scaled, err := FractionalKelly(q, d, FullKelly)
			if err != nil {
				t.Fatalf("FractionalKelly: %v", err)
			}
			// Multiplying by exactly 1.0 is exact in IEEE-754, so this is one of
			// the few places an equality assertion is the right one.
			if scaled != full {
				t.Fatalf("q=%v d=%v: FullKelly scaling changed %.17g into %.17g",
					qv, float64(d), full, scaled)
			}
		}
	}
}

func TestFractionalKellyRejectsABadMultiplier(t *testing.T) {
	cases := []struct {
		name string
		m    float64
		want error
	}{
		{"zero is not a staking policy", 0, ErrKellyMultiplierOutOfRange},
		{"negative", -0.25, ErrKellyMultiplierOutOfRange},
		{"above one is leverage past the optimum", 1.5, ErrKellyMultiplierOutOfRange},
		{"a percentage passed by mistake", 25, ErrKellyMultiplierOutOfRange},
		{"NaN", math.NaN(), ErrNotFinite},
		{"+Inf", math.Inf(1), ErrNotFinite},
		{"-Inf", math.Inf(-1), ErrNotFinite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := FractionalKelly(0.55, 2, c.m)
			if !errors.Is(err, c.want) {
				t.Fatalf("FractionalKelly multiplier %v: error = %v, want %v", c.m, err, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Growth rate, and why Kelly is the optimum
// -----------------------------------------------------------------------------

func TestGrowthRateWorkedExamples(t *testing.T) {
	cases := []struct {
		name     string
		q        float64
		d        float64
		fraction float64
		want     float64
	}{
		{
			// At the Kelly optimum for a 55% shot at even money:
			//   g = 0.55 x ln(1.10) + 0.45 x ln(0.90)
			//     = 0.55 x  0.09531017980  + 0.45 x (-0.10536051566)
			//     = 0.05242059889          -        0.04741223205
			//     = 0.00500836684
			// Half a percent of compounded growth per wager.
			"55% at +100, staked at Kelly", 0.55, 2, 0.10, 0.0050083668463568,
		},
		{
			// Twice the Kelly stake on the same wager:
			//   g = 0.55 x ln(1.20) + 0.45 x ln(0.80)
			//     = 0.55 x  0.18232155679 + 0.45 x (-0.22314355131)
			//     = 0.10027685624         -        0.10041459809
			//     = -0.00013774185
			// Negative. Staking double Kelly on a genuine 5% edge is worse than not
			// betting at all -- the concrete form of the asymmetry that makes
			// fractional Kelly the sane default.
			"55% at +100, staked at double Kelly", 0.55, 2, 0.20, -0.00013774185471936,
		},
		{
			// Not betting neither grows nor shrinks a bankroll.
			"no stake is no growth", 0.55, 2, 0, 0,
		},
		{
			// A certainty at even money doubles the bankroll every wager:
			//   g = 1 x ln(1 + 1 x 1) = ln 2 = 0.69314718056
			"certainty at +100 staked in full", 1, 2, 1, math.Ln2,
		},
		{
			// Betting a 50/50 at -110 at the Kelly-optimal stake of zero is flat;
			// staking anything is negative. Here a tenth of the bankroll:
			//   g = 0.5 x ln(1 + 0.1 x 10/11) + 0.5 x ln(0.9)
			//     = 0.5 x ln(1.0909090909...) + 0.5 x (-0.10536051566)
			//     = 0.5 x  0.08701137698      -        0.05268025783
			//     = 0.04350568849             -        0.05268025783
			//     = -0.00917456934
			"50% at -110, staked at a tenth", 0.5, 210.0 / 110.0, 0.10, -0.0091745693,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := GrowthRate(fairProb(t, c.q), decOdds(t, c.d), c.fraction)
			if err != nil {
				t.Fatalf("GrowthRate: %v", err)
			}
			assertClose(t, "growth rate", got, c.want, 1e-9)

			// Recompute independently with math.Log of a sum, which is a different
			// arithmetic path from the implementation's math.Log1p. Agreement is
			// evidence about the formula rather than a restatement of it.
			b := c.d - 1
			independent := 0.0
			if c.q > 0 {
				independent += c.q * math.Log(1+c.fraction*b)
			}
			if c.q < 1 {
				independent += (1 - c.q) * math.Log(1-c.fraction)
			}
			assertClose(t, "growth rate vs math.Log path", got, independent, relTolChain)
		})
	}
}

func TestGrowthRateIsExactlyZeroAtZeroStake(t *testing.T) {
	// Not betting must be exactly flat, not 1e-17 of drift. Anything else would
	// make the "is this wager worth taking" comparison depend on noise.
	for _, d := range sweepDecimals(t) {
		for _, qv := range []float64{0, 0.25, 0.5, 0.75, 1} {
			g, err := GrowthRate(Probability(qv), d, 0)
			if err != nil {
				t.Fatalf("GrowthRate: %v", err)
			}
			if g != 0 {
				t.Fatalf("GrowthRate(%v, %v, 0) = %.17g, want exactly 0", qv, float64(d), g)
			}
		}
	}
}

// TestKellyMaximisesTheGrowthRate is the strongest correctness statement available
// for the Kelly formula: rather than comparing it against a restatement of itself,
// it checks the property the formula exists to satisfy. If f* is off in either
// direction, some competing fraction on the grid beats it.
func TestKellyMaximisesTheGrowthRate(t *testing.T) {
	// The slack is absolute. Growth rates here are bounded above by ln(101) < 4.7
	// and below by (1-q)|ln(1-f)| < 14 at the grid's outermost fraction, so 1e-12
	// absolute is tighter than 1e-13 relative -- consistent with the package's
	// 1e-12 relative convention, and roughly a thousand times the ~1e-15 floating
	// point noise floor of the two logarithms.
	const slack = 1e-12

	for _, dv := range []float64{1.05, 1.25, 210.0 / 110.0, 2, 3, 4.5, 12, 41, 101} {
		d := decOdds(t, dv)
		for _, qv := range []float64{0.01, 0.1, 0.25, 0.4, 0.5, 0.55, 0.6, 0.75, 0.9, 0.99} {
			q := fairProb(t, qv)

			star, err := Kelly(q, d)
			if err != nil {
				t.Fatalf("Kelly: %v", err)
			}
			best, err := GrowthRate(q, d, star)
			if err != nil {
				t.Fatalf("GrowthRate at f*: %v", err)
			}

			for i := 0; i <= 999; i++ {
				f := float64(i) / 1000
				g, err := GrowthRate(q, d, f)
				if err != nil {
					t.Fatalf("GrowthRate at f=%v: %v", f, err)
				}
				if g > best+slack {
					t.Fatalf("q=%v d=%v: staking %v grows at %.17g, beating the Kelly stake %.17g at %.17g",
						qv, dv, f, g, star, best)
				}
			}
		}
	}
}

func TestGrowthRateRejectsCertainRuin(t *testing.T) {
	// Staking everything on an outcome that can lose has a growth rate of negative
	// infinity. That is the correct value and an unacceptable return, so it is an
	// error.
	_, err := GrowthRate(fairProb(t, 0.999), decOdds(t, 2), 1)
	if !errors.Is(err, ErrCertainRuin) {
		t.Fatalf("GrowthRate(0.999, 2, 1) error = %v, want ErrCertainRuin", err)
	}

	// The same stake on a certainty is fine: there is no losing branch.
	g, err := GrowthRate(fairProb(t, 1), decOdds(t, 2), 1)
	if err != nil {
		t.Fatalf("GrowthRate(1, 2, 1): %v", err)
	}
	assertClose(t, "growth at certainty", g, math.Ln2, relTolExact)
}

func TestGrowthRateRejectsABadFraction(t *testing.T) {
	cases := []struct {
		name string
		f    float64
		want error
	}{
		{"negative", -0.01, ErrBankrollFractionOutOfRange},
		{"above one", 1.01, ErrBankrollFractionOutOfRange},
		{"a percentage passed by mistake", 25, ErrBankrollFractionOutOfRange},
		{"NaN", math.NaN(), ErrNotFinite},
		{"+Inf", math.Inf(1), ErrNotFinite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := GrowthRate(0.55, 2, c.f)
			if !errors.Is(err, c.want) {
				t.Fatalf("GrowthRate fraction %v: error = %v, want %v", c.f, err, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Invalid input
// -----------------------------------------------------------------------------

func TestStakingFunctionsRejectInvalidInput(t *testing.T) {
	badProbabilities := []struct {
		name string
		v    float64
		want error
	}{
		{"NaN", math.NaN(), ErrNotFinite},
		{"+Inf", math.Inf(1), ErrNotFinite},
		{"negative", -1e-9, ErrProbabilityOutOfRange},
		{"above one", 1 + 1e-9, ErrProbabilityOutOfRange},
	}
	badDecimals := []struct {
		name string
		v    float64
		want error
	}{
		{"NaN", math.NaN(), ErrNotFinite},
		{"-Inf", math.Inf(-1), ErrNotFinite},
		{"exactly one", 1, ErrDecimalOutOfRange},
		{"below one", 0.99, ErrDecimalOutOfRange},
		{"negative", -3, ErrDecimalOutOfRange},
	}

	for _, bp := range badProbabilities {
		t.Run("probability/"+bp.name, func(t *testing.T) {
			if _, err := Kelly(Probability(bp.v), 2); !errors.Is(err, bp.want) {
				t.Errorf("Kelly = %v, want %v", err, bp.want)
			}
			if _, err := FractionalKelly(Probability(bp.v), 2, HalfKelly); !errors.Is(err, bp.want) {
				t.Errorf("FractionalKelly = %v, want %v", err, bp.want)
			}
			if _, err := GrowthRate(Probability(bp.v), 2, 0.1); !errors.Is(err, bp.want) {
				t.Errorf("GrowthRate = %v, want %v", err, bp.want)
			}
		})
	}

	for _, bd := range badDecimals {
		t.Run("decimal/"+bd.name, func(t *testing.T) {
			if _, err := Kelly(0.55, Decimal(bd.v)); !errors.Is(err, bd.want) {
				t.Errorf("Kelly = %v, want %v", err, bd.want)
			}
			if _, err := FractionalKelly(0.55, Decimal(bd.v), HalfKelly); !errors.Is(err, bd.want) {
				t.Errorf("FractionalKelly = %v, want %v", err, bd.want)
			}
			if _, err := GrowthRate(0.55, Decimal(bd.v), 0.1); !errors.Is(err, bd.want) {
				t.Errorf("GrowthRate = %v, want %v", err, bd.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The money seam
// -----------------------------------------------------------------------------

// TestKellyFractionKeepsAnIntegerStakeInsideTheBankroll asserts the contract this
// package owes its callers at the fraction-to-money boundary described at the top
// of kelly.go.
//
// The truncation below mirrors the RoundTowardZero branch of
// internal/domain.Money.MulFloat, which is math.Trunc(float64(m) * factor). The
// mirror is deliberate and its limits are worth stating: this file cannot import
// internal/domain without falsifying this package's "zero non-standard-library
// dependencies" guarantee, so what is asserted here is the property of the
// FRACTION that makes the conversion safe -- not the behaviour of Money itself,
// which belongs with the staking helper in internal/domain.
func TestKellyFractionKeepsAnIntegerStakeInsideTheBankroll(t *testing.T) {
	// Bankrolls in minor units: one cent, a dollar, a hundred, a thousand, and a
	// large but exactly representable balance.
	bankrolls := []int64{1, 7, 100, 10_000, 100_000, 1_000_000, 999_999_999, 1 << 50}

	checked := 0
	for _, bankroll := range bankrolls {
		for _, dv := range []float64{1.05, 1.25, 210.0 / 110.0, 2, 4.5, 12, 101, 1001} {
			d := decOdds(t, dv)
			for _, qv := range []float64{0, 0.1, 0.25, 0.4, 0.5, 0.55, 0.6, 0.8, 0.95, 1} {
				q := fairProb(t, qv)
				for _, m := range []float64{FullKelly, HalfKelly, QuarterKelly} {
					f, err := FractionalKelly(q, d, m)
					if err != nil {
						t.Fatalf("FractionalKelly: %v", err)
					}

					exact := float64(bankroll) * f
					stake := int64(math.Trunc(exact))

					if stake < 0 {
						t.Fatalf("bankroll=%d q=%v d=%v m=%v: negative stake %d",
							bankroll, qv, dv, m, stake)
					}
					if stake > bankroll {
						t.Fatalf("bankroll=%d q=%v d=%v m=%v: stake %d exceeds the bankroll",
							bankroll, qv, dv, m, stake)
					}
					if float64(stake) > exact {
						t.Fatalf("bankroll=%d q=%v d=%v m=%v: rounded stake %d exceeds the theoretical stake %.17g",
							bankroll, qv, dv, m, stake, exact)
					}
					if exact-float64(stake) >= 1 {
						t.Fatalf("bankroll=%d q=%v d=%v m=%v: rounding lost %v minor units, not less than one",
							bankroll, qv, dv, m, exact-float64(stake))
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("swept nothing; the guard would pass vacuously")
	}
	t.Logf("checked %d bankroll/price/probability/multiplier combinations", checked)
}

func TestQuarterKellyOnAWorkedBankroll(t *testing.T) {
	// A thousand dollars of play money, in minor units, on the canonical wager.
	//   f*      = (0.55 x 2 - 1)/(2 - 1)     = 0.10
	//   quarter = 0.10 x 0.25                = 0.025
	//   stake   = 100000 x 0.025             = 2500 minor units = $25.00
	const bankroll int64 = 100_000

	f, err := FractionalKelly(fairProb(t, 0.55), decOdds(t, 2), QuarterKelly)
	if err != nil {
		t.Fatalf("FractionalKelly: %v", err)
	}
	assertClose(t, "quarter-kelly fraction", f, 0.025, relTolExact)

	if stake := int64(math.Trunc(float64(bankroll) * f)); stake != 2500 {
		t.Fatalf("quarter-kelly stake = %d minor units, want 2500", stake)
	}
}

// -----------------------------------------------------------------------------
// Property-based tests (CLAUDE.md §4)
// -----------------------------------------------------------------------------

func TestPropertyKellyStaysInTheUnitInterval(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		qv := rapid.Float64Range(0, 1).Draw(t, "q")
		dv := rapid.Float64Range(1.0001, 1001).Draw(t, "d")

		f, err := Kelly(Probability(qv), Decimal(dv))
		if err != nil {
			t.Fatalf("Kelly(%v, %v): %v", qv, dv, err)
		}
		if math.IsNaN(f) || f < 0 || f > 1 {
			t.Fatalf("Kelly(%v, %v) = %.17g, outside [0, 1]", qv, dv, f)
		}
	})
}

func TestPropertyKellyMaximisesTheGrowthRate(t *testing.T) {
	// See TestKellyMaximisesTheGrowthRate for the slack argument; the generated
	// ranges here are bounded so the same bound applies.
	const slack = 1e-12

	rapid.Check(t, func(t *rapid.T) {
		qv := rapid.Float64Range(0.001, 0.999).Draw(t, "q")
		dv := rapid.Float64Range(1.01, 101).Draw(t, "d")
		alt := rapid.Float64Range(0, 0.999).Draw(t, "competing fraction")
		q, d := Probability(qv), Decimal(dv)

		star, err := Kelly(q, d)
		if err != nil {
			t.Fatalf("Kelly: %v", err)
		}
		if star >= 1 {
			// GrowthRate is undefined at a full stake on an uncertain outcome. The
			// generator's bounds make this unreachable; the guard documents why
			// rather than relying on the reader to re-derive it.
			return
		}

		best, err := GrowthRate(q, d, star)
		if err != nil {
			t.Fatalf("GrowthRate at f*=%v: %v", star, err)
		}
		other, err := GrowthRate(q, d, alt)
		if err != nil {
			t.Fatalf("GrowthRate at f=%v: %v", alt, err)
		}
		if other > best+slack {
			t.Fatalf("q=%v d=%v: staking %v grows at %.17g, beating the Kelly stake %.17g at %.17g",
				qv, dv, alt, other, star, best)
		}
	})
}

func TestPropertyFractionalKellyNeverExceedsFullKelly(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		qv := rapid.Float64Range(0, 1).Draw(t, "q")
		dv := rapid.Float64Range(1.0001, 1001).Draw(t, "d")
		m := rapid.Float64Range(1e-9, 1).Draw(t, "multiplier")
		q, d := Probability(qv), Decimal(dv)

		full, err := Kelly(q, d)
		if err != nil {
			t.Fatalf("Kelly: %v", err)
		}
		scaled, err := FractionalKelly(q, d, m)
		if err != nil {
			t.Fatalf("FractionalKelly: %v", err)
		}
		if scaled > full {
			t.Fatalf("q=%v d=%v m=%v: %.17g exceeds full kelly %.17g", qv, dv, m, scaled, full)
		}
		if scaled < 0 || scaled > 1 {
			t.Fatalf("q=%v d=%v m=%v: %.17g outside [0, 1]", qv, dv, m, scaled)
		}
		if !closeTo(scaled, full*m, relTolExact) {
			t.Fatalf("q=%v d=%v m=%v: %.17g is not %.17g scaled", qv, dv, m, scaled, full)
		}
	})
}

func TestPropertyAnIntegerStakeNeverExceedsTheKellyStake(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		qv := rapid.Float64Range(0, 1).Draw(t, "q")
		dv := rapid.Float64Range(1.0001, 1001).Draw(t, "d")
		// Bankrolls up to 2^53-1 minor units, the range over which
		// internal/domain.Money converts to float64 exactly.
		bankroll := rapid.Int64Range(0, 1<<53-1).Draw(t, "bankroll minor units")

		f, err := Kelly(Probability(qv), Decimal(dv))
		if err != nil {
			t.Fatalf("Kelly: %v", err)
		}

		exact := float64(bankroll) * f
		stake := int64(math.Trunc(exact))
		if stake < 0 || stake > bankroll {
			t.Fatalf("bankroll=%d q=%v d=%v: stake %d outside [0, bankroll]", bankroll, qv, dv, stake)
		}
		if float64(stake) > exact {
			t.Fatalf("bankroll=%d q=%v d=%v: stake %d exceeds the theoretical %.17g",
				bankroll, qv, dv, stake, exact)
		}
	})
}

// -----------------------------------------------------------------------------
// Documentation examples
// -----------------------------------------------------------------------------

func ExampleKelly() {
	// A 55% shot offered at +100, which is decimal 2.0.
	full, _ := Kelly(0.55, 2)
	quarter, _ := FractionalKelly(0.55, 2, QuarterKelly)

	fmt.Printf("full %.4f, quarter %.4f\n", full, quarter)
	// Output:
	// full 0.1000, quarter 0.0250
}

func ExampleExpectedValuePercent() {
	// The standard -110 point-spread price is decimal 210/110. A true coin flip
	// taken at it loses the book's hold.
	price := Decimal(210.0 / 110.0)

	breakeven, _ := BreakevenProbability(price)
	coinFlip, _ := ExpectedValuePercent(0.5, price)
	edged, _ := ExpectedValuePercent(0.55, price)

	fmt.Printf("breakeven %.4f, 50%% gives %.4f%%, 55%% gives %.4f%%\n",
		float64(breakeven), coinFlip, edged)
	// Output:
	// breakeven 0.5238, 50% gives -4.5455%, 55% gives 5.0000%
}
