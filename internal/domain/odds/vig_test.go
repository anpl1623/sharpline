package odds

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// -----------------------------------------------------------------------------
// Float comparison policy for this file
// -----------------------------------------------------------------------------
//
// Nothing here compares float64 with ==. Every assertion goes through closeTo /
// assertClose, declared in convert_test.go, at relTolChain (1e-12).
//
// relTolChain rather than the tighter relTolExact, for a specific reason: the
// overround is S - 1, a difference of two numbers close to 1. The subtraction is
// exact by Sterbenz's lemma, but it does not shrink the absolute error already
// present in S — it only shrinks the magnitude, by a factor of about fifty on a
// normal market. So a value of S accurate to a couple of ULP yields an overround
// whose *relative* accuracy is nearer 1e-14, and asserting 1e-15 relative on it
// would be asserting something untrue about IEEE-754 rather than something true
// about the code.
//
// Note that closeTo scales by max(1, |got|, |want|), so for the sub-unit
// quantities in this file — overround, vig, excess, share — relTolChain acts as an
// absolute bound of 1e-12. Every published margin in these tests is at least 1e-2,
// ten orders of magnitude larger, so the tolerance cannot absorb a wrong formula.

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// decimalsOf builds a market from decimal prices, failing the test if any is
// invalid. Failing here rather than returning an error keeps the intent of a test
// case visible: an invalid fixture is a broken test, not a case under test.
func decimalsOf(t *testing.T, vs ...float64) []Decimal {
	t.Helper()
	out := make([]Decimal, len(vs))
	for i, v := range vs {
		d, err := NewDecimal(v)
		if err != nil {
			t.Fatalf("fixture NewDecimal(%v): %v", v, err)
		}
		out[i] = d
	}
	return out
}

// americansOf builds a market from American prices, which is the form US books
// publish and therefore the form the source data is quoted in.
func americansOf(t *testing.T, vs ...int64) []Decimal {
	t.Helper()
	out := make([]Decimal, len(vs))
	for i, v := range vs {
		a, err := NewAmerican(v)
		if err != nil {
			t.Fatalf("fixture NewAmerican(%d): %v", v, err)
		}
		d, err := a.Decimal()
		if err != nil {
			t.Fatalf("fixture American(%d).Decimal(): %v", v, err)
		}
		out[i] = d
	}
	return out
}

// fractionalsOf builds a market from fractional prices, which is the form UK books
// publish. Wikipedia's worked example is quoted fractionally, so the fixture is
// built the same way rather than pre-converted by hand.
func fractionalsOf(t *testing.T, pairs ...[2]int64) []Decimal {
	t.Helper()
	out := make([]Decimal, len(pairs))
	for i, p := range pairs {
		f, err := NewFractional(p[0], p[1])
		if err != nil {
			t.Fatalf("fixture NewFractional(%d, %d): %v", p[0], p[1], err)
		}
		d, err := f.Decimal()
		if err != nil {
			t.Fatalf("fixture Fractional(%d/%d).Decimal(): %v", p[0], p[1], err)
		}
		out[i] = d
	}
	return out
}

// mustMargin computes a margin or fails the test.
func mustMargin(t *testing.T, prices []Decimal) Margin {
	t.Helper()
	m, err := NewMargin(prices)
	if err != nil {
		t.Fatalf("NewMargin: %v", err)
	}
	return m
}

// mustContributions computes an attribution or fails the test.
func mustContributions(t *testing.T, prices []Decimal, a Attribution) []SelectionVig {
	t.Helper()
	vs, err := VigContributions(prices, a)
	if err != nil {
		t.Fatalf("VigContributions(%s): %v", a, err)
	}
	return vs
}

// sumSelection totals one field across a market's contributions, using the same
// compensated summation the implementation uses so the assertion is about the
// invariant rather than about the test's own accumulation error.
func sumSelection(vs []SelectionVig, pick func(SelectionVig) float64) float64 {
	xs := make([]float64, len(vs))
	for i, v := range vs {
		xs[i] = pick(v)
	}
	return neumaierSum(xs)
}

func fieldImplied(v SelectionVig) float64  { return v.Implied }
func fieldFair(v SelectionVig) float64     { return v.Fair }
func fieldExcess(v SelectionVig) float64   { return v.Excess }
func fieldShare(v SelectionVig) float64    { return v.Share }
func fieldRelative(v SelectionVig) float64 { return v.RelativeMargin }

// -----------------------------------------------------------------------------
// Real published market 1: the standard US point-spread price
// -----------------------------------------------------------------------------

// TestStandardTwoWayJuiceSeparatesTheThreeNumbers works the market every US book
// posts on every point spread and total: both sides at -110.
//
// Source: -110 is the standard point-spread and total juice across the US market;
// it is a published price level, not a quote attributed to any fixture. The
// expected values are exact rationals, and the arithmetic is written out so a
// reviewer can check it without running or trusting the code:
//
//	-110      → d = 1 + 100/110 = 21/11 = 1.909090…
//	q         = 1/d = 11/21             = 0.5238095238…
//	S         = 2 × 11/21 = 22/21       = 1.0476190476…
//	booking   = 100 × 22/21 = 2200/21   = 104.7619047619…
//	overround = 22/21 - 1 = 1/21        = 0.0476190476…
//	vig       = (1/21)/(22/21) = 1/22   = 0.0454545454…
//
// Every expectation below is written as that rational, not as a decimal literal,
// so the test states the mathematics rather than a transcription of it. The test's
// arithmetic path (a single division of small integers) is also different from the
// implementation's (reciprocal, compensated sum, subtract, divide), so agreement is
// evidence rather than restatement.
func TestStandardTwoWayJuiceSeparatesTheThreeNumbers(t *testing.T) {
	market := americansOf(t, -110, -110)
	m := mustMargin(t, market)

	if m.Selections != 2 {
		t.Errorf("Selections = %d, want 2", m.Selections)
	}
	assertClose(t, "S", m.ImpliedSum, 22.0/21.0, relTolChain)
	assertClose(t, "booking percentage", m.BookingPercentage, 2200.0/21.0, relTolChain)
	assertClose(t, "overround", m.Overround, 1.0/21.0, relTolChain)
	assertClose(t, "vig", m.Vig, 1.0/22.0, relTolChain)

	// The three standalone functions must agree with the summary, since a caller
	// may reach for either.
	sum, err := ImpliedSum(market)
	if err != nil {
		t.Fatalf("ImpliedSum: %v", err)
	}
	assertClose(t, "ImpliedSum", sum, m.ImpliedSum, relTolChain)

	booking, err := BookingPercentage(market)
	if err != nil {
		t.Fatalf("BookingPercentage: %v", err)
	}
	assertClose(t, "BookingPercentage", booking, m.BookingPercentage, relTolChain)

	over, err := Overround(market)
	if err != nil {
		t.Fatalf("Overround: %v", err)
	}
	assertClose(t, "Overround", over, m.Overround, relTolChain)

	vig, err := Vig(market)
	if err != nil {
		t.Fatalf("Vig: %v", err)
	}
	assertClose(t, "Vig", vig, m.Vig, relTolChain)

	// 104.76, 0.0476 and 0.0455 are three different numbers, which is the entire
	// point of this file. Assert the separations explicitly rather than trusting
	// that three assertions above happened to differ.
	if !(m.Vig < m.Overround) {
		t.Errorf("vig %g is not strictly less than overround %g on an overround market", m.Vig, m.Overround)
	}
	// overround - vig = (S-1) - (S-1)/S = (S-1)²/S = (1/21)²·(21/22) = 1/462.
	assertClose(t, "overround - vig", m.Overround-m.Vig, 1.0/462.0, relTolChain)
	// overround / vig = S, an identity that holds for every market and is the
	// cleanest statement of how the two differ.
	assertClose(t, "overround / vig", m.Overround/m.Vig, m.ImpliedSum, relTolChain)
	// The booking percentage carries the same information on a scale 100× larger.
	assertClose(t, "booking/100 - 1", m.BookingPercentage/100-1, m.Overround, relTolChain)

	if !m.IsOverround() {
		t.Errorf("a -110/-110 market is not classified overround: %s", m)
	}
}

// -----------------------------------------------------------------------------
// Real published market 2: Wikipedia's worked three-way football book
// -----------------------------------------------------------------------------
//
// Source: Wikipedia, "Mathematics of bookmaking", worked example. The article
// states a fair book of Evens / 2-1 / 5-1 (50%, 33⅓%, 16⅔%) and the bookmaker's
// adjusted book of 4-6 / 6-4 / 4-1 (60%, 40%, 20%), which it describes as "a 'book'
// of 120%", giving "16⅔% profit on turnover (20.00/120.00)".
//
// That last phrase is the distinction this file exists to make, stated by the
// source itself: the book is 120% (booking percentage), the overround is 20
// percentage points, and the profit on turnover is 20/120 = 1/6 (the vig). Three
// numbers, one market.
//
//	4-6 → d = 1 + 4/6 = 5/3 = 1.666…  q = 3/5 = 0.6
//	6-4 → d = 1 + 6/4 = 5/2 = 2.5     q = 2/5 = 0.4
//	4-1 → d = 1 + 4/1 = 5.0           q = 1/5 = 0.2
//	S = 0.6 + 0.4 + 0.2 = 1.2   booking = 120   overround = 0.2   vig = 0.2/1.2 = 1/6

// wikipedia120Book is the article's bookmaker-adjusted three-way book.
func wikipedia120Book(t *testing.T) []Decimal {
	t.Helper()
	return fractionalsOf(t, [2]int64{4, 6}, [2]int64{6, 4}, [2]int64{4, 1})
}

// wikipediaFairBook is the article's stated 100% book for the same match.
func wikipediaFairBook(t *testing.T) []Decimal {
	t.Helper()
	return fractionalsOf(t, [2]int64{1, 1}, [2]int64{2, 1}, [2]int64{5, 1})
}

func TestWikipediaThreeWayBook(t *testing.T) {
	market := wikipedia120Book(t)
	m := mustMargin(t, market)

	if m.Selections != 3 {
		t.Errorf("Selections = %d, want 3", m.Selections)
	}
	assertClose(t, "S", m.ImpliedSum, 1.2, relTolChain)
	assertClose(t, "booking percentage", m.BookingPercentage, 120, relTolChain)
	assertClose(t, "overround", m.Overround, 0.2, relTolChain)
	assertClose(t, "vig (profit on turnover, 20.00/120.00)", m.Vig, 1.0/6.0, relTolChain)

	if !m.IsOverround() {
		t.Errorf("a 120%% book is not classified overround: %s", m)
	}
	// The article's own numbers make the overround/hold gap unmissable: 20% against
	// 16⅔%, an overstatement of a fifth if the two are confused.
	assertClose(t, "overround - vig", m.Overround-m.Vig, 0.2-1.0/6.0, relTolChain)
}

// TestWikipediaFairBookIsClassifiedFair is the S = 1 edge case, and it is worked
// on a market that a published source calls fair rather than one constructed to be.
//
// It is also the concrete demonstration that comparing S against 1.0 with == is
// wrong. The three implied probabilities are 1/2, 1/3 and 1/6, which sum to exactly
// 1 in the reals and do not in float64: summed naively left to right they come to
// 1 - 2^-53. A classifier using equality would report the article's fair book as
// underround. FairMarketTolerance exists for exactly this.
func TestWikipediaFairBookIsClassifiedFair(t *testing.T) {
	market := wikipediaFairBook(t)
	m := mustMargin(t, market)

	assertClose(t, "S", m.ImpliedSum, 1, relTolChain)
	assertClose(t, "booking percentage", m.BookingPercentage, 100, relTolChain)
	assertClose(t, "overround", m.Overround, 0, relTolChain)
	assertClose(t, "vig", m.Vig, 0, relTolChain)

	if !m.IsFair() {
		t.Errorf("the article's 100%% book is not classified fair: %s", m)
	}
	if m.IsOverround() || m.IsUnderround() {
		t.Errorf("a fair book is also classified over/underround: %s", m)
	}

	// The vig is a division by S, and S = 1 exactly is not a division problem: the
	// numerator is zero, the denominator is one. Nothing here may be NaN or Inf.
	if math.IsNaN(m.Vig) || math.IsInf(m.Vig, 0) {
		t.Errorf("vig on a fair market is %v, want a finite zero", m.Vig)
	}

	// Compensated versus naive summation, on real numbers rather than a contrived
	// input. The claim tested is the documented one — that compensation gets at
	// least as close to the true sum — not a bit pattern, which would be asserting
	// float equality by another name.
	qs := make([]float64, len(market))
	for i, d := range market {
		p, err := d.Probability()
		if err != nil {
			t.Fatalf("Probability: %v", err)
		}
		qs[i] = float64(p)
	}
	var naive float64
	for _, q := range qs {
		naive += q
	}
	compensated := neumaierSum(qs)
	t.Logf("1/2 + 1/3 + 1/6 in float64: naive = %.20g, compensated = %.20g", naive, compensated)
	if math.Abs(compensated-1) > math.Abs(naive-1) {
		t.Errorf("compensated sum %.20g is further from 1 than the naive sum %.20g", compensated, naive)
	}
	// The documented Neumaier bound is about 2 ULP independent of term count. One
	// ULP at magnitude 1 is 2^-52.
	if math.Abs(compensated-1) > 2*math.Ldexp(1, -52) {
		t.Errorf("compensated sum %.20g is further than 2 ULP from 1", compensated)
	}
}

// -----------------------------------------------------------------------------
// Real published market 3: a heavily lopsided two-way
// -----------------------------------------------------------------------------
//
// -2000 and +1000 are both standard American price rungs, routinely posted on
// mismatched moneylines. The pair is assembled here to exercise a lopsided market;
// it is not attributed to a fixture.
//
//	-2000 → d = 1 + 100/2000 = 1.05   q = 1/1.05 = 20/21 = 0.9523809523…
//	+1000 → d = 1 + 1000/100 = 11.0   q = 1/11          = 0.0909090909…
//	S         = 20/21 + 1/11 = (220 + 21)/231 = 241/231 = 1.0432900432…
//	booking   = 24100/231                               = 104.3290043290…
//	overround = 241/231 - 1 = 10/231                    = 0.0432900432…
//	vig       = (10/231)/(241/231) = 10/241             = 0.0414937759…

func lopsidedTwoWay(t *testing.T) []Decimal {
	t.Helper()
	return americansOf(t, -2000, 1000)
}

func TestLopsidedTwoWayMarket(t *testing.T) {
	m := mustMargin(t, lopsidedTwoWay(t))

	assertClose(t, "S", m.ImpliedSum, 241.0/231.0, relTolChain)
	assertClose(t, "booking percentage", m.BookingPercentage, 24100.0/231.0, relTolChain)
	assertClose(t, "overround", m.Overround, 10.0/231.0, relTolChain)
	assertClose(t, "vig", m.Vig, 10.0/241.0, relTolChain)
	if !m.IsOverround() {
		t.Errorf("a -2000/+1000 market is not classified overround: %s", m)
	}
}

// -----------------------------------------------------------------------------
// Underround: an arbitrage inside one book's own market
// -----------------------------------------------------------------------------

// TestUnderroundMarketIsNotAnError works a two-way whose implied probabilities sum
// to less than 1. Books post pairs like this on boosted and promotional markets and
// on a stale side of a market that has moved; +105 and -100 are both standard
// American rungs, assembled here to exercise S < 1 rather than attributed to a
// fixture. (-100 and +100 are the same price; NewAmerican folds -100 onto +100.)
//
//	+105 → d = 2.05  q = 1/2.05 = 20/41 = 0.4878048780…
//	+100 → d = 2.00  q = 1/2    = 1/2   = 0.5
//	S         = 20/41 + 1/2 = (40 + 41)/82 = 81/82 = 0.9878048780…
//	booking   = 8100/82                            = 98.7804878048…
//	overround = 81/82 - 1 = -1/82                  = -0.0121951219…
//	vig       = (-1/82)/(81/82) = -1/81            = -0.0123456790…
//
// Staking in proportion to the implied probabilities costs 0.9878 units and returns
// 1 whichever way it lands: a 1.23% guaranteed profit, which the negative vig
// states directly.
func TestUnderroundMarketIsNotAnError(t *testing.T) {
	market := americansOf(t, 105, -100)
	m, err := NewMargin(market)
	if err != nil {
		t.Fatalf("an underround market must not be an error: %v", err)
	}

	assertClose(t, "S", m.ImpliedSum, 81.0/82.0, relTolChain)
	assertClose(t, "booking percentage", m.BookingPercentage, 8100.0/82.0, relTolChain)
	assertClose(t, "overround", m.Overround, -1.0/82.0, relTolChain)
	assertClose(t, "vig", m.Vig, -1.0/81.0, relTolChain)

	if !m.IsUnderround() {
		t.Errorf("S = %g is not classified underround: %s", m.ImpliedSum, m)
	}
	if m.IsOverround() || m.IsFair() {
		t.Errorf("an underround market is also classified overround or fair: %s", m)
	}

	// Dividing by S < 1 magnifies rather than damps, so the hold is *more* negative
	// than the overround here — the reverse of the overround case, and a second
	// reason the two numbers must not be used interchangeably.
	if !(m.Vig < m.Overround) {
		t.Errorf("vig %g is not below overround %g on an underround market", m.Vig, m.Overround)
	}

	// The shares still apportion the (negative) overround and still sum to 1.
	vs := mustContributions(t, market, AttributionProportional)
	assertClose(t, "Σ fair", sumSelection(vs, fieldFair), 1, relTolChain)
	assertClose(t, "Σ excess", sumSelection(vs, fieldExcess), m.Overround, relTolChain)
	assertClose(t, "Σ share", sumSelection(vs, fieldShare), 1, relTolChain)
	// fair = q/S: 0.5·82/81 = 41/81 for the pick'em side, (20/41)·(82/81) = 40/81
	// for the +105 side.
	assertClose(t, "fair(+105)", vs[0].Fair, 40.0/81.0, relTolChain)
	assertClose(t, "fair(+100)", vs[1].Fair, 41.0/81.0, relTolChain)
	for i, v := range vs {
		if v.Excess >= 0 {
			t.Errorf("selection %d excess = %g, want negative on an underround market", i, v.Excess)
		}
		if v.Share <= 0 {
			t.Errorf("selection %d share = %g, want positive: numerator and denominator share a sign", i, v.Share)
		}
	}
}

// TestExactlyEvenMarketIsNotADivisionProblem uses the one market whose implied
// probabilities sum to exactly 1.0 in float64 with no rounding at all: two sides at
// decimal 2.0, so q = 0.5 + 0.5. S is bitwise 1, the overround is bitwise 0, and
// the vig is 0/1.
//
// A market with no juice is not something a book posts, so this is a constructed
// fixture and is labelled as one. It is here because "S exactly 1.0" is the edge
// the arithmetic has to survive, and the cited fair book above cannot supply it —
// that one lands a ULP away, which is the complementary case.
func TestExactlyEvenMarketIsNotADivisionProblem(t *testing.T) {
	market := decimalsOf(t, 2.0, 2.0)
	m := mustMargin(t, market)

	assertClose(t, "S", m.ImpliedSum, 1, relTolChain)
	assertClose(t, "booking percentage", m.BookingPercentage, 100, relTolChain)
	assertClose(t, "overround", m.Overround, 0, relTolChain)
	assertClose(t, "vig", m.Vig, 0, relTolChain)
	if !m.IsFair() {
		t.Errorf("a 2.0/2.0 market is not classified fair: %s", m)
	}

	for _, a := range []Attribution{AttributionProportional, AttributionUniform} {
		vs := mustContributions(t, market, a)
		assertClose(t, a.String()+" Σ fair", sumSelection(vs, fieldFair), 1, relTolChain)
		for i, v := range vs {
			assertClose(t, a.String()+" fair", v.Fair, 0.5, relTolChain)
			assertClose(t, a.String()+" excess", v.Excess, 0, relTolChain)
			// Share is 0/0 on a fair market and is defined to be zero rather than
			// NaN. This is the assertion that pins that decision down.
			if v.Share != 0 {
				t.Errorf("%s selection %d share = %v, want exactly 0 on a fair market", a, i, v.Share)
			}
			if math.IsNaN(v.RelativeMargin) || math.IsInf(v.RelativeMargin, 0) {
				t.Errorf("%s selection %d relative margin = %v, want finite", a, i, v.RelativeMargin)
			}
		}
	}
}

// TestClassificationIsMutuallyExclusive asserts that exactly one of the three
// classifiers is true for every market, including markets sitting right on the edge
// of FairMarketTolerance.
func TestClassificationIsMutuallyExclusive(t *testing.T) {
	sums := []float64{
		0.5, 0.9, 0.98, 1 - 2*FairMarketTolerance, 1 - FairMarketTolerance,
		1, 1 + FairMarketTolerance, 1 + 2*FairMarketTolerance, 1.02, 1.2, 3.0,
	}
	for _, s := range sums {
		m, err := MarginFromSum(2, s)
		if err != nil {
			t.Fatalf("MarginFromSum(2, %g): %v", s, err)
		}
		n := 0
		for _, b := range []bool{m.IsFair(), m.IsOverround(), m.IsUnderround()} {
			if b {
				n++
			}
		}
		if n != 1 {
			t.Errorf("S = %g: %d of 3 classifiers true (fair=%v over=%v under=%v)",
				s, n, m.IsFair(), m.IsOverround(), m.IsUnderround())
		}
	}
}

// -----------------------------------------------------------------------------
// Attribution
// -----------------------------------------------------------------------------

// TestProportionalAttributionReproducesWikipediasFairBook is the strongest single
// piece of evidence in this file that the proportional convention is implemented
// correctly, because both the input and the expected output are published.
//
// The article's bookmaker book is 60/40/20 and its stated fair book for the same
// match is 50/33⅓/16⅔. Those differ by exactly the factor 1.2 = S, which is the
// definition of proportional attribution. Devigging the quoted book proportionally
// must therefore return the article's fair book to the last decimal it prints.
func TestProportionalAttributionReproducesWikipediasFairBook(t *testing.T) {
	market := wikipedia120Book(t)
	m := mustMargin(t, market)
	vs := mustContributions(t, market, AttributionProportional)

	if len(vs) != 3 {
		t.Fatalf("got %d contributions, want 3", len(vs))
	}

	// Quoted: 60%, 40%, 20%.
	assertClose(t, "implied(4-6)", vs[0].Implied, 0.6, relTolChain)
	assertClose(t, "implied(6-4)", vs[1].Implied, 0.4, relTolChain)
	assertClose(t, "implied(4-1)", vs[2].Implied, 0.2, relTolChain)

	// Fair, per the article: Evens = 1/2, 2-1 = 1/3, 5-1 = 1/6.
	assertClose(t, "fair(4-6) → evens", vs[0].Fair, 1.0/2.0, relTolChain)
	assertClose(t, "fair(6-4) → 2-1", vs[1].Fair, 1.0/3.0, relTolChain)
	assertClose(t, "fair(4-1) → 5-1", vs[2].Fair, 1.0/6.0, relTolChain)

	// Excess: 0.1, 1/15, 1/30 — summing to the 0.2 overround.
	assertClose(t, "excess(4-6)", vs[0].Excess, 0.1, relTolChain)
	assertClose(t, "excess(6-4)", vs[1].Excess, 1.0/15.0, relTolChain)
	assertClose(t, "excess(4-1)", vs[2].Excess, 1.0/30.0, relTolChain)
	assertClose(t, "Σ excess", sumSelection(vs, fieldExcess), m.Overround, relTolChain)

	// Share of the overround equals the fair probability under this convention,
	// and the shares sum to 1.
	assertClose(t, "share(4-6)", vs[0].Share, 1.0/2.0, relTolChain)
	assertClose(t, "share(6-4)", vs[1].Share, 1.0/3.0, relTolChain)
	assertClose(t, "share(4-1)", vs[2].Share, 1.0/6.0, relTolChain)
	assertClose(t, "Σ share", sumSelection(vs, fieldShare), 1, relTolChain)
	assertClose(t, "Σ fair", sumSelection(vs, fieldFair), 1, relTolChain)

	// The defining property of the proportional convention: every selection carries
	// the same relative margin, and that margin is exactly the overround. The
	// asymmetry between selections lives entirely in the absolute excess, which is
	// three times larger on the favourite than on the outsider.
	for i, v := range vs {
		assertClose(t, "relative margin", v.RelativeMargin, m.Overround, relTolChain)
		if v.Fair <= 0 || v.Fair >= 1 {
			t.Errorf("selection %d fair = %g, want strictly inside (0, 1)", i, v.Fair)
		}
	}
	if !(vs[0].Excess > vs[2].Excess) {
		t.Errorf("favourite excess %g does not exceed outsider excess %g", vs[0].Excess, vs[2].Excess)
	}
}

// TestUniformAttributionCarriesTheLongshotBias works the same cited market under
// the other convention, where every selection surrenders the same absolute slice of
// the overround: (S-1)/n = 0.2/3 = 1/15 each.
//
//	fair(4-6) = 0.6 - 1/15 = 8/15   relative margin = (1/15)/(8/15) = 1/8  = 0.125
//	fair(6-4) = 0.4 - 1/15 = 5/15   relative margin = (1/15)/(5/15) = 1/5  = 0.2
//	fair(4-1) = 0.2 - 1/15 = 2/15   relative margin = (1/15)/(2/15) = 1/2  = 0.5
//
// The 4-1 outsider carries four times the relative margin of the 4-6 favourite.
// That is the favourite–longshot direction, and it is the asymmetry a +EV finder
// keys on: the two conventions put the outsider's fair probability at 16.67% and
// 13.33% respectively, a quarter apart.
func TestUniformAttributionCarriesTheLongshotBias(t *testing.T) {
	market := wikipedia120Book(t)
	m := mustMargin(t, market)
	vs := mustContributions(t, market, AttributionUniform)

	assertClose(t, "fair(4-6)", vs[0].Fair, 8.0/15.0, relTolChain)
	assertClose(t, "fair(6-4)", vs[1].Fair, 5.0/15.0, relTolChain)
	assertClose(t, "fair(4-1)", vs[2].Fair, 2.0/15.0, relTolChain)
	assertClose(t, "Σ fair", sumSelection(vs, fieldFair), 1, relTolChain)

	for i, v := range vs {
		assertClose(t, "excess", v.Excess, 1.0/15.0, relTolChain)
		assertClose(t, "share", v.Share, 1.0/3.0, relTolChain)
		if v.Fair <= 0 || v.Fair >= 1 {
			t.Errorf("selection %d fair = %g, want strictly inside (0, 1)", i, v.Fair)
		}
	}
	assertClose(t, "Σ excess", sumSelection(vs, fieldExcess), m.Overround, relTolChain)
	assertClose(t, "Σ share", sumSelection(vs, fieldShare), 1, relTolChain)

	assertClose(t, "relative margin(4-6)", vs[0].RelativeMargin, 0.125, relTolChain)
	assertClose(t, "relative margin(6-4)", vs[1].RelativeMargin, 0.2, relTolChain)
	assertClose(t, "relative margin(4-1)", vs[2].RelativeMargin, 0.5, relTolChain)

	// Monotone in price length: the longer the shot, the more relative margin.
	for i := 1; i < len(vs); i++ {
		if !(vs[i].RelativeMargin > vs[i-1].RelativeMargin) {
			t.Errorf("relative margin is not increasing with price length: %v", vs)
		}
	}
}

// TestAttributionsAgreeOnSymmetricMarketsAndDisagreeOnLopsidedOnes pins the reason
// the convention has to be named by the caller.
func TestAttributionsAgreeOnSymmetricMarketsAndDisagreeOnLopsidedOnes(t *testing.T) {
	// A -110/-110 market is symmetric, so proportional and uniform coincide: each
	// side surrenders (1/21)/2 = 1/42 either way.
	symmetric := americansOf(t, -110, -110)
	prop := mustContributions(t, symmetric, AttributionProportional)
	unif := mustContributions(t, symmetric, AttributionUniform)
	for i := range prop {
		assertClose(t, "symmetric fair", prop[i].Fair, unif[i].Fair, relTolChain)
		assertClose(t, "symmetric excess", prop[i].Excess, 1.0/42.0, relTolChain)
		assertClose(t, "symmetric excess agreement", prop[i].Excess, unif[i].Excess, relTolChain)
	}

	// A -2000/+1000 market is not symmetric, and the conventions part company.
	//
	//	proportional: fair = q/S    → 220/241 = 0.912863…, 21/241 = 0.087137…
	//	              relative margin = 10/231 = 0.043290… on both sides
	//	uniform:      slice = (10/231)/2 = 5/231
	//	              fair = 215/231 = 0.930736…, 16/231 = 0.069264…
	//	              relative margin = 5/215 = 1/43 = 0.023256…, 5/16 = 0.3125
	//	              ratio = (5/16)/(1/43) = 215/16 = 13.4375
	lopsided := lopsidedTwoWay(t)
	m := mustMargin(t, lopsided)
	prop = mustContributions(t, lopsided, AttributionProportional)
	unif = mustContributions(t, lopsided, AttributionUniform)

	assertClose(t, "proportional fair(-2000)", prop[0].Fair, 220.0/241.0, relTolChain)
	assertClose(t, "proportional fair(+1000)", prop[1].Fair, 21.0/241.0, relTolChain)
	assertClose(t, "proportional relative margin(-2000)", prop[0].RelativeMargin, m.Overround, relTolChain)
	assertClose(t, "proportional relative margin(+1000)", prop[1].RelativeMargin, m.Overround, relTolChain)

	assertClose(t, "uniform fair(-2000)", unif[0].Fair, 215.0/231.0, relTolChain)
	assertClose(t, "uniform fair(+1000)", unif[1].Fair, 16.0/231.0, relTolChain)
	assertClose(t, "uniform excess", unif[0].Excess, 5.0/231.0, relTolChain)
	assertClose(t, "uniform relative margin(-2000)", unif[0].RelativeMargin, 1.0/43.0, relTolChain)
	assertClose(t, "uniform relative margin(+1000)", unif[1].RelativeMargin, 5.0/16.0, relTolChain)
	assertClose(t, "uniform longshot/favourite relative margin",
		unif[1].RelativeMargin/unif[0].RelativeMargin, 215.0/16.0, relTolChain)

	// The underdog's fair probability differs by more than 20% of its own value
	// between the two conventions, which is the whole reason the choice cannot be
	// left implicit.
	gap := math.Abs(prop[1].Fair-unif[1].Fair) / prop[1].Fair
	if gap < 0.2 {
		t.Errorf("conventions differ by only %.1f%% on the underdog; expected a material disagreement", gap*100)
	}
	t.Logf("underdog fair probability: proportional %.6f, uniform %.6f (%.1f%% apart)",
		prop[1].Fair, unif[1].Fair, gap*100)
}

// TestUniformAttributionCanBeUndefined exercises the documented limitation: an
// equal absolute slice of the overround can be larger than a long shot's entire
// implied probability.
//
// The shape is real — outright and futures markets pair a short favourite with a
// very long tail at double-digit margins — though the prices here are chosen to
// land the failure rather than quoted from a book:
//
//	1.25   q = 0.8     1000.0 is a 999/1 shot, q = 0.001
//	S = 0.8 + 0.4 + 0.001 = 1.201, slice = 0.201/3 = 0.067 > 0.001
//	→ the outsider's fair probability would be -0.066, which is not a probability.
func TestUniformAttributionCanBeUndefined(t *testing.T) {
	market := decimalsOf(t, 1.25, 2.5, 1000.0)

	if _, err := VigContributions(market, AttributionUniform); !errors.Is(err, ErrAttributionUndefined) {
		t.Errorf("uniform attribution on a long-tailed market: error = %v, want ErrAttributionUndefined", err)
	} else if !strings.Contains(err.Error(), "selection 2") {
		t.Errorf("error %q does not name the offending selection", err)
	}

	// The proportional convention is total and must still succeed on the same
	// market: q/S is positive for every positive q.
	vs := mustContributions(t, market, AttributionProportional)
	assertClose(t, "Σ fair", sumSelection(vs, fieldFair), 1, relTolChain)
	for i, v := range vs {
		if v.Fair <= 0 {
			t.Errorf("selection %d fair = %g, want strictly positive", i, v.Fair)
		}
	}
}

func TestVigContributionsDoesNotMutateItsInput(t *testing.T) {
	market := wikipedia120Book(t)
	before := make([]Decimal, len(market))
	copy(before, market)

	if _, err := VigContributions(market, AttributionUniform); err != nil {
		t.Fatalf("VigContributions: %v", err)
	}
	for i := range market {
		if market[i] != before[i] {
			t.Errorf("input price %d changed from %v to %v", i, before[i], market[i])
		}
	}
}

func TestAttributionStringAndValid(t *testing.T) {
	cases := []struct {
		a     Attribution
		name  string
		valid bool
	}{
		{AttributionUnknown, "unknown", false},
		{AttributionProportional, "proportional", true},
		{AttributionUniform, "uniform", true},
		{Attribution(200), "unknown", false},
	}
	for _, c := range cases {
		if got := c.a.String(); got != c.name {
			t.Errorf("Attribution(%d).String() = %q, want %q", uint8(c.a), got, c.name)
		}
		if got := c.a.Valid(); got != c.valid {
			t.Errorf("Attribution(%d).Valid() = %v, want %v", uint8(c.a), got, c.valid)
		}
	}
}

func TestUnknownAttributionIsRejected(t *testing.T) {
	market := americansOf(t, -110, -110)
	for _, a := range []Attribution{AttributionUnknown, Attribution(9)} {
		_, err := VigContributions(market, a)
		if !errors.Is(err, ErrUnknownAttribution) {
			t.Errorf("VigContributions with attribution %d: error = %v, want ErrUnknownAttribution", uint8(a), err)
		}
	}
}

// -----------------------------------------------------------------------------
// Constructors from probabilities and from a bare sum
// -----------------------------------------------------------------------------

// TestMarginFromProbabilitiesChecksDevigOutput covers the constructor's stated
// purpose: a devigged market must come back fair.
func TestMarginFromProbabilitiesChecksDevigOutput(t *testing.T) {
	// The fair book Wikipedia states for its worked example.
	fair := []Probability{0.5, 1.0 / 3.0, 1.0 / 6.0}
	m, err := MarginFromProbabilities(fair)
	if err != nil {
		t.Fatalf("MarginFromProbabilities: %v", err)
	}
	assertClose(t, "S", m.ImpliedSum, 1, relTolChain)
	assertClose(t, "vig", m.Vig, 0, relTolChain)
	if !m.IsFair() {
		t.Errorf("a devigged market is not classified fair: %s", m)
	}

	// The quoted book, given as probabilities, must reproduce the margin computed
	// from its prices.
	quoted := []Probability{0.6, 0.4, 0.2}
	fromProbs, err := MarginFromProbabilities(quoted)
	if err != nil {
		t.Fatalf("MarginFromProbabilities: %v", err)
	}
	fromPrices := mustMargin(t, wikipedia120Book(t))
	assertClose(t, "S from probabilities vs prices", fromProbs.ImpliedSum, fromPrices.ImpliedSum, relTolChain)
	assertClose(t, "vig from probabilities vs prices", fromProbs.Vig, fromPrices.Vig, relTolChain)

	// 0 and 1 are valid probabilities — a settled market — and must be summarisable
	// rather than rejected, even though neither is priceable.
	settled, err := MarginFromProbabilities([]Probability{1, 0})
	if err != nil {
		t.Fatalf("MarginFromProbabilities on a settled market: %v", err)
	}
	if !settled.IsFair() {
		t.Errorf("a settled market {1, 0} is not fair: %s", settled)
	}

	// An all-zero market has no sum to divide by and must fail loudly.
	if _, err := MarginFromProbabilities([]Probability{0, 0}); !errors.Is(err, ErrImpliedSumNotPositive) {
		t.Errorf("MarginFromProbabilities({0,0}): error = %v, want ErrImpliedSumNotPositive", err)
	}
}

func TestMarginFromSumEdgeCases(t *testing.T) {
	cases := []struct {
		name       string
		selections int
		sum        float64
		want       error
	}{
		{"valid", 2, 1.05, nil},
		{"fair", 2, 1, nil},
		{"underround", 2, 0.98, nil},
		{"one selection", 1, 1.05, ErrTooFewSelections},
		{"zero selections", 0, 1.05, ErrTooFewSelections},
		{"negative selections", -3, 1.05, ErrTooFewSelections},
		{"NaN", 2, math.NaN(), ErrNotFinite},
		{"+Inf", 2, math.Inf(1), ErrNotFinite},
		{"-Inf", 2, math.Inf(-1), ErrNotFinite},
		{"zero sum", 2, 0, ErrImpliedSumNotPositive},
		{"negative sum", 2, -0.5, ErrImpliedSumNotPositive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := MarginFromSum(c.selections, c.sum)
			if c.want == nil {
				if err != nil {
					t.Fatalf("MarginFromSum(%d, %g): %v", c.selections, c.sum, err)
				}
				assertClose(t, "S", m.ImpliedSum, c.sum, relTolChain)
				assertClose(t, "overround", m.Overround, c.sum-1, relTolChain)
				assertClose(t, "vig", m.Vig, (c.sum-1)/c.sum, relTolChain)
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("MarginFromSum(%d, %g): error = %v, want %v", c.selections, c.sum, err, c.want)
			}
			if m != (Margin{}) {
				t.Errorf("failed MarginFromSum returned a non-zero Margin: %+v", m)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Rejections
// -----------------------------------------------------------------------------

// TestTooFewSelectionsIsRejectedEverywhere covers n < 2 on every entry point that
// takes a market, because a single price is not a market and its "overround" would
// be a meaningless statement about one number.
func TestTooFewSelectionsIsRejectedEverywhere(t *testing.T) {
	short := [][]Decimal{nil, {}, decimalsOf(t, 1.91)}
	for _, market := range short {
		if _, err := NewMargin(market); !errors.Is(err, ErrTooFewSelections) {
			t.Errorf("NewMargin(len %d): error = %v, want ErrTooFewSelections", len(market), err)
		}
		if _, err := ImpliedSum(market); !errors.Is(err, ErrTooFewSelections) {
			t.Errorf("ImpliedSum(len %d): error = %v, want ErrTooFewSelections", len(market), err)
		}
		if _, err := BookingPercentage(market); !errors.Is(err, ErrTooFewSelections) {
			t.Errorf("BookingPercentage(len %d): error = %v, want ErrTooFewSelections", len(market), err)
		}
		if _, err := Overround(market); !errors.Is(err, ErrTooFewSelections) {
			t.Errorf("Overround(len %d): error = %v, want ErrTooFewSelections", len(market), err)
		}
		if _, err := Vig(market); !errors.Is(err, ErrTooFewSelections) {
			t.Errorf("Vig(len %d): error = %v, want ErrTooFewSelections", len(market), err)
		}
		if _, err := VigContributions(market, AttributionProportional); !errors.Is(err, ErrTooFewSelections) {
			t.Errorf("VigContributions(len %d): error = %v, want ErrTooFewSelections", len(market), err)
		}
	}
	if _, err := MarginFromProbabilities([]Probability{0.5}); !errors.Is(err, ErrTooFewSelections) {
		t.Errorf("MarginFromProbabilities(len 1): error = %v, want ErrTooFewSelections", err)
	}
}

// TestInvalidPricesAreRejectedWithTheirIndex checks the whole rejection contract at
// once: the sentinel survives, the message names which selection was bad, and the
// package's "odds:" prefix appears exactly once however deep the wrapping went.
func TestInvalidPricesAreRejectedWithTheirIndex(t *testing.T) {
	good, err := NewDecimal(1.91)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	cases := []struct {
		name string
		bad  Decimal
		want error
	}{
		{"NaN", Decimal(math.NaN()), ErrNotFinite},
		{"+Inf", Decimal(math.Inf(1)), ErrNotFinite},
		{"-Inf", Decimal(math.Inf(-1)), ErrNotFinite},
		{"exactly 1.0 (zero payout)", Decimal(1), ErrDecimalOutOfRange},
		{"below 1", Decimal(0.5), ErrDecimalOutOfRange},
		{"zero", Decimal(0), ErrDecimalOutOfRange},
		{"negative", Decimal(-2.5), ErrDecimalOutOfRange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			market := []Decimal{good, c.bad, good}
			_, err := NewMargin(market)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewMargin: error = %v, want %v", err, c.want)
			}
			if !strings.Contains(err.Error(), "selection 1") {
				t.Errorf("error %q does not name the offending selection", err)
			}
			if n := strings.Count(err.Error(), "odds:"); n != 1 {
				t.Errorf("error %q carries the package prefix %d times, want exactly 1", err, n)
			}
			// Every other entry point must reject it identically.
			if _, err := VigContributions(market, AttributionProportional); !errors.Is(err, c.want) {
				t.Errorf("VigContributions: error = %v, want %v", err, c.want)
			}
			if _, err := ImpliedSum(market); !errors.Is(err, c.want) {
				t.Errorf("ImpliedSum: error = %v, want %v", err, c.want)
			}
		})
	}

	// The probability path has its own validator and its own out-of-range sentinel.
	for _, bad := range []Probability{Probability(math.NaN()), Probability(-0.1), Probability(1.5)} {
		_, err := MarginFromProbabilities([]Probability{0.5, bad})
		if err == nil {
			t.Fatalf("MarginFromProbabilities accepted %v", float64(bad))
		}
		if !strings.Contains(err.Error(), "selection 1") {
			t.Errorf("error %q does not name the offending selection", err)
		}
		if n := strings.Count(err.Error(), "odds:"); n != 1 {
			t.Errorf("error %q carries the package prefix %d times, want exactly 1", err, n)
		}
	}
	if _, err := MarginFromProbabilities([]Probability{0.5, Probability(math.NaN())}); !errors.Is(err, ErrNotFinite) {
		t.Errorf("NaN probability: error = %v, want ErrNotFinite", err)
	}
	if _, err := MarginFromProbabilities([]Probability{0.5, 1.5}); !errors.Is(err, ErrProbabilityOutOfRange) {
		t.Errorf("out-of-range probability: error = %v, want ErrProbabilityOutOfRange", err)
	}
}

// TestFailedCallsReturnZeroValues asserts that a rejected call never hands back a
// half-computed answer alongside its error.
func TestFailedCallsReturnZeroValues(t *testing.T) {
	bad := []Decimal{Decimal(1), Decimal(1)}

	if m, err := NewMargin(bad); err == nil || m != (Margin{}) {
		t.Errorf("NewMargin returned (%+v, %v), want (zero, error)", m, err)
	}
	if s, err := ImpliedSum(bad); err == nil || s != 0 {
		t.Errorf("ImpliedSum returned (%v, %v), want (0, error)", s, err)
	}
	if v, err := Overround(bad); err == nil || v != 0 {
		t.Errorf("Overround returned (%v, %v), want (0, error)", v, err)
	}
	if v, err := Vig(bad); err == nil || v != 0 {
		t.Errorf("Vig returned (%v, %v), want (0, error)", v, err)
	}
	if v, err := BookingPercentage(bad); err == nil || v != 0 {
		t.Errorf("BookingPercentage returned (%v, %v), want (0, error)", v, err)
	}
	if vs, err := VigContributions(bad, AttributionProportional); err == nil || vs != nil {
		t.Errorf("VigContributions returned (%v, %v), want (nil, error)", vs, err)
	}
}

func TestMarginStringLabelsEveryNumber(t *testing.T) {
	got := mustMargin(t, americansOf(t, -110, -110)).String()
	for _, want := range []string{"selections", "S=", "booking", "overround", "vig"} {
		if !strings.Contains(got, want) {
			t.Errorf("Margin.String() = %q, missing %q", got, want)
		}
	}
	t.Logf("-110/-110: %s", got)
}

// -----------------------------------------------------------------------------
// Invariants over many markets
// -----------------------------------------------------------------------------

// The invariants below are asserted by two generators with different strengths,
// against one shared statement of the algebra so the two cannot drift:
//
//   - TestPropertyMarketMarginInvariants uses pgregory.net/rapid, per CLAUDE.md §4.
//     Its value is adversarial generation and shrinking: a violation is reduced to
//     a minimal counterexample and written to testdata/rapid for replay.
//   - TestMarketInvariantsHoldOverSeededMarkets uses a seeded PRNG with a
//     log-uniform price distribution. Its value is distributional control — it can
//     assert that both sides of S = 1 were actually exercised, which a
//     shrink-oriented generator cannot promise.
//
// Neither subsumes the other, so both are kept and the assertions are shared.

// marketFailer is the intersection of *testing.T and *rapid.T that the shared
// invariant checks need.
//
// Helper is deliberately absent from the set: *rapid.T does not export it, so
// requiring it would make the helper unusable from a property test. The cost is
// that a failure reports the line inside the helper rather than the call site,
// which is why every message below names the market it was checking.
type marketFailer interface {
	Fatalf(format string, args ...any)
}

// closeOrFail is the shared assertion. It uses closeTo at relTolChain for the
// reasons argued at the top of this file.
func closeOrFail(f marketFailer, what string, got, want float64) {
	if !closeTo(got, want, relTolChain) {
		f.Fatalf("%s = %.17g, want %.17g (|diff| = %.3g, tolerance %g)",
			what, got, want, math.Abs(got-want), relTolChain)
	}
}

// marketOutcome is what checking one market produced.
type marketOutcome struct {
	Margin Margin
	// UniformUndefined records that AttributionUniform legitimately refused this
	// market, which is a documented outcome and not a failure.
	UniformUndefined bool
}

// assertMarketInvariants checks every algebraic property that must hold for every
// market, and is the single statement of those properties.
//
// The properties, and why each is worth asserting rather than assuming:
//
//   - The three headline numbers restate their definitions. In particular the vig
//     is checked against 1 - 1/S, the algebraically identical form the
//     implementation deliberately does *not* use; agreement to 1e-12 shows the
//     better-conditioned form is equivalent, not merely different.
//   - Exactly one classifier is true, so no market falls between the three.
//   - Σ fair = 1 under both conventions. This is what makes an attribution a devig
//     rather than an arbitrary apportionment.
//   - Σ excess = the overround, so the apportionment is exhaustive and no margin
//     is invented or lost.
//   - Σ share = 1, checked only away from the fair-market limit where the ratio is
//     0/0 and its neighbourhood is dominated by rounding.
//   - Every fair probability lies strictly inside (0, 1), because a devig that
//     emits a number outside that range has not produced a probability.
//   - Nothing anywhere is NaN or ±Inf.
func assertMarketInvariants(f marketFailer, prices []Decimal) marketOutcome {
	n := len(prices)

	m, err := NewMargin(prices)
	if err != nil {
		f.Fatalf("NewMargin(%v): %v", prices, err)
		return marketOutcome{}
	}
	if math.IsNaN(m.ImpliedSum) || math.IsInf(m.ImpliedSum, 0) || m.ImpliedSum <= 0 {
		f.Fatalf("S = %v for %v", m.ImpliedSum, prices)
		return marketOutcome{}
	}
	if m.Selections != n {
		f.Fatalf("Selections = %d, want %d", m.Selections, n)
		return marketOutcome{}
	}

	closeOrFail(f, "booking = 100·S", m.BookingPercentage, 100*m.ImpliedSum)
	closeOrFail(f, "overround = S-1", m.Overround, m.ImpliedSum-1)
	closeOrFail(f, "vig = 1 - 1/S", m.Vig, 1-1/m.ImpliedSum)

	classifications := 0
	for _, b := range []bool{m.IsFair(), m.IsOverround(), m.IsUnderround()} {
		if b {
			classifications++
		}
	}
	if classifications != 1 {
		f.Fatalf("%d of 3 classifiers true at S = %g", classifications, m.ImpliedSum)
		return marketOutcome{}
	}
	// Dividing by S shrinks the overround when S > 1 and magnifies it when S < 1,
	// and in both directions the result lands below the overround.
	if !m.IsFair() && !(m.Vig < m.Overround) {
		f.Fatalf("vig %g is not below overround %g at S = %g", m.Vig, m.Overround, m.ImpliedSum)
		return marketOutcome{}
	}

	out := marketOutcome{Margin: m}

	for _, a := range []Attribution{AttributionProportional, AttributionUniform} {
		vs, err := VigContributions(prices, a)
		if err != nil {
			// Only the uniform convention may fail, and only this one way.
			if a == AttributionUniform && errors.Is(err, ErrAttributionUndefined) {
				out.UniformUndefined = true
				continue
			}
			f.Fatalf("VigContributions(%s) on %v: %v", a, prices, err)
			return out
		}
		if len(vs) != n {
			f.Fatalf("%s returned %d contributions, want %d", a, len(vs), n)
			return out
		}

		for i, v := range vs {
			for _, field := range []struct {
				name string
				val  float64
			}{
				{"implied", v.Implied}, {"fair", v.Fair}, {"excess", v.Excess},
				{"share", v.Share}, {"relative margin", v.RelativeMargin},
			} {
				if math.IsNaN(field.val) || math.IsInf(field.val, 0) {
					f.Fatalf("%s selection %d %s = %v at S = %g", a, i, field.name, field.val, m.ImpliedSum)
					return out
				}
			}
			if v.Fair <= 0 || v.Fair >= 1 {
				f.Fatalf("%s selection %d fair = %g, want strictly inside (0, 1) at S = %g",
					a, i, v.Fair, m.ImpliedSum)
				return out
			}
			closeOrFail(f, "excess = implied - fair", v.Excess, v.Implied-v.Fair)
		}

		closeOrFail(f, a.String()+" Σ implied = S", sumSelection(vs, fieldImplied), m.ImpliedSum)
		closeOrFail(f, a.String()+" Σ fair = 1", sumSelection(vs, fieldFair), 1)
		closeOrFail(f, a.String()+" Σ excess = overround", sumSelection(vs, fieldExcess), m.Overround)

		// Share is a ratio of two quantities that both vanish as a market
		// approaches fair, so it is only meaningfully checkable away from that
		// limit. Below 1e-6 of overround the ratio is dominated by rounding and the
		// assertion would be testing float noise rather than the code.
		if math.Abs(m.Overround) > 1e-6 {
			closeOrFail(f, a.String()+" Σ share = 1", sumSelection(vs, fieldShare), 1)
		}

		if a == AttributionProportional {
			// The defining property of the convention: every selection carries the
			// same relative margin, and it is exactly the overround. It follows
			// that the total is n times the overround.
			for i, v := range vs {
				if !closeTo(v.RelativeMargin, m.Overround, relTolChain) {
					f.Fatalf("proportional selection %d relative margin %.17g != overround %.17g at S = %g",
						i, v.RelativeMargin, m.Overround, m.ImpliedSum)
					return out
				}
			}
			closeOrFail(f, "proportional Σ relative margin",
				sumSelection(vs, fieldRelative), float64(n)*m.Overround)
			continue
		}

		// Uniform: every selection surrenders the same absolute slice, and the
		// relative margin grows in magnitude as the price lengthens.
		//
		// Magnitude, not signed value, and the distinction is not pedantry:
		// relative margin is slice/(q - slice), and on an underround market the
		// slice is negative, so the signed quantity runs the other way while
		// |slice|/(q - slice) still falls monotonically in q. An earlier version of
		// this assertion checked the signed value and was wrong on exactly those
		// markets. The additive slack absorbs float noise on markets close enough
		// to fair that both magnitudes are near zero.
		slice := m.Overround / float64(n)
		for i, v := range vs {
			closeOrFail(f, "uniform excess", v.Excess, slice)
			if i == 0 || vs[i-1].Implied <= v.Implied {
				continue
			}
			if math.Abs(vs[i-1].RelativeMargin) > math.Abs(v.RelativeMargin)+relTolChain {
				f.Fatalf("uniform |relative margin| is not monotone in price length: "+
					"implied %g → %g but margin %g → %g at S = %g",
					vs[i-1].Implied, v.Implied,
					vs[i-1].RelativeMargin, v.RelativeMargin, m.ImpliedSum)
				return out
			}
		}
	}
	return out
}

// TestPropertyMarketMarginInvariants is the property-based test CLAUDE.md §4 calls
// for on this package.
//
// It draws implied probabilities and inverts them to prices, rather than drawing
// prices directly, so that a shrunk counterexample reads as the probabilities that
// broke the invariant — the language every formula in this file is written in. The
// range 0.02 to 0.98 spans roughly +4900 to -4900 in American terms and straddles
// S = 1 in both directions once summed.
func TestPropertyMarketMarginInvariants(t *testing.T) {
	overround, underround, fair, uniformUndefined := 0, 0, 0, 0

	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(MinMarketSelections, 12).Draw(t, "selections")
		prices := make([]Decimal, n)
		for i := range prices {
			q := rapid.Float64Range(0.02, 0.98).Draw(t, fmt.Sprintf("q%d", i))
			d, err := NewDecimal(1 / q)
			if err != nil {
				t.Fatalf("implied probability %v is not priceable: %v", q, err)
			}
			prices[i] = d
		}

		out := assertMarketInvariants(t, prices)
		switch {
		case out.Margin.IsOverround():
			overround++
		case out.Margin.IsUnderround():
			underround++
		default:
			fair++
		}
		if out.UniformUndefined {
			uniformUndefined++
		}
	})

	t.Logf("rapid: %d overround, %d underround, %d fair, %d uniform attributions undefined",
		overround, underround, fair, uniformUndefined)
}

// TestMarketInvariantsHoldOverSeededMarkets sweeps the same invariants over a
// seeded, log-uniform distribution of prices.
//
// It exists alongside the rapid test rather than being replaced by it because it
// can assert something rapid cannot: that both sides of S = 1 were actually
// generated. A property test that happened to draw only overround markets would
// leave every underround assertion vacuous and still pass, silently.
//
// The seed is fixed, so a failure reported by CI reproduces verbatim on a laptop.
func TestMarketInvariantsHoldOverSeededMarkets(t *testing.T) {
	const iterations = 2000
	rng := rand.New(rand.NewPCG(0x5A17DEC0DE, 0x0DD5))

	overroundSeen, underroundSeen, uniformUndefined := 0, 0, 0

	for iter := 0; iter < iterations; iter++ {
		n := 2 + rng.IntN(11) // 2..12 selections
		prices := make([]Decimal, n)
		for i := range prices {
			// Log-uniform over decimal 1.02 to 51: a linear draw would concentrate
			// on longshots and rarely produce a market a book would post.
			d := 1 + math.Exp(math.Log(0.02)+rng.Float64()*(math.Log(50)-math.Log(0.02)))
			p, err := NewDecimal(d)
			if err != nil {
				t.Fatalf("iteration %d: generated invalid decimal %v: %v", iter, d, err)
			}
			prices[i] = p
		}

		out := assertMarketInvariants(t, prices)
		switch {
		case out.Margin.IsOverround():
			overroundSeen++
		case out.Margin.IsUnderround():
			underroundSeen++
		}
		if out.UniformUndefined {
			uniformUndefined++
		}
	}

	t.Logf("%d seeded markets: %d overround, %d underround, %d uniform attributions undefined",
		iterations, overroundSeen, underroundSeen, uniformUndefined)
	if overroundSeen == 0 || underroundSeen == 0 {
		t.Errorf("generator produced %d overround and %d underround markets; both must be exercised",
			overroundSeen, underroundSeen)
	}
}

// TestNeumaierSumMatchesNaiveOnWellConditionedInput guards the summation helper
// itself: on inputs where naive addition is already accurate, compensation must not
// change the answer, and the compensation must actually be applied on inputs where
// it does.
func TestNeumaierSumMatchesNaiveOnWellConditionedInput(t *testing.T) {
	cases := [][]float64{
		{},
		{0.5},
		{0.5, 0.5},
		{0.25, 0.25, 0.25, 0.25},
		{0.6, 0.4, 0.2},
	}
	for _, xs := range cases {
		var naive float64
		for _, x := range xs {
			naive += x
		}
		got := neumaierSum(xs)
		assertClose(t, "neumaierSum", got, naive, relTolChain)
	}

	// And the case that shows the compensation is load-bearing rather than
	// decorative: one term of 1 followed by 1000 terms of 2^-60.
	//
	// The exact total is 1 + 1000·2^-60 = 1 + 8.6736e-16, which is 3.90625 ULP above
	// 1. Each individual small term is 8.67e-19, far below the half-ULP of 1.11e-16
	// needed to move a double at magnitude 1, so naive addition discards every one
	// of them and returns exactly 1. Compensated summation banks the discarded bits
	// and returns 1 + 4·2^-52, the double nearest the true total.
	//
	// Two orderings are checked because the interesting failure mode is order
	// dependence: naive summation gives two different answers for the same multiset
	// depending on whether the large term comes first, and compensated summation
	// gives one.
	const smallTerms = 1000
	small := math.Ldexp(1, -60)
	exact := 1 + smallTerms*small

	largeFirst := make([]float64, 0, smallTerms+1)
	largeFirst = append(largeFirst, 1)
	for i := 0; i < smallTerms; i++ {
		largeFirst = append(largeFirst, small)
	}
	smallFirst := make([]float64, 0, smallTerms+1)
	for i := 0; i < smallTerms; i++ {
		smallFirst = append(smallFirst, small)
	}
	smallFirst = append(smallFirst, 1)

	naiveSum := func(xs []float64) float64 {
		var s float64
		for _, x := range xs {
			s += x
		}
		return s
	}

	naiveLarge, naiveSmall := naiveSum(largeFirst), naiveSum(smallFirst)
	compLarge, compSmall := neumaierSum(largeFirst), neumaierSum(smallFirst)
	t.Logf("1 then %d×2^-60: naive = %.20g, compensated = %.20g", smallTerms, naiveLarge, compLarge)
	t.Logf("%d×2^-60 then 1: naive = %.20g, compensated = %.20g", smallTerms, naiveSmall, compSmall)
	t.Logf("exact total = %.20g", exact)

	// A deliberate exact comparison, which the file's no-== policy permits only
	// where exactness is the property under test. It is: for this input the two
	// orderings agree bitwise under compensation and differ under naive addition.
	// Note the scope — compensated summation is not order-independent in general,
	// and this asserts the concrete behaviour on this input, not a theorem.
	if compLarge != compSmall {
		t.Errorf("compensated sum depends on term order: %.20g vs %.20g", compLarge, compSmall)
	}
	// The compensation must actually be doing something: on the ordering naive
	// summation gets wrong, it must be strictly closer to the true total.
	if !(math.Abs(compLarge-exact) < math.Abs(naiveLarge-exact)) {
		t.Errorf("compensated %.20g is not strictly closer to %.20g than naive %.20g",
			compLarge, exact, naiveLarge)
	}
	// And it must be within a couple of ULP of the true total, which is the
	// documented Neumaier bound. One ULP just above 1 is 2^-52.
	if math.Abs(compLarge-exact) > 2*math.Ldexp(1, -52) {
		t.Errorf("compensated %.20g is further than 2 ULP from the true total %.20g", compLarge, exact)
	}
}
