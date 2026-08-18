package synthetic

import (
	"errors"
	"math"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// TestTickDecimalNeverImprovesThePrice is the regression test for the defect
// that shipped in the first build of this package.
//
// odds.Decimal.American rounds to the NEAREST American price, which can round a
// price up, and flooring that result to the book's tick does not always undo it.
// Markets then came out under their configured margin — a book giving away its
// hold through an accident of rounding, which the +EV finder would have reported
// as a permanent, entirely artificial edge.
func TestTickDecimalNeverImprovesThePrice(t *testing.T) {
	inputs := []float64{
		1.0001, 1.01, 1.04, 1.25, 1.5, 1.909, 1.95, 1.9999, 2.0, 2.0001,
		2.05, 2.5, 3.0, 4.75, 11.0, 51.0, 201.0,
	}
	for _, tick := range []int64{1, 5, 10} {
		for _, d := range inputs {
			got, err := tickDecimal(d, tick)
			if err != nil {
				t.Fatalf("tickDecimal(%v, %d): %v", d, tick, err)
			}
			if got > d {
				t.Errorf("tickDecimal(%v, %d) = %v, which is a BETTER price than the fair one", d, tick, got)
			}
			if got <= 1 {
				t.Errorf("tickDecimal(%v, %d) = %v, outside the legal decimal range", d, tick, got)
			}
			// The result must be a price the book could actually post: an exact
			// multiple of its tick in American space.
			am, err := odds.Decimal(got).American()
			if err != nil {
				t.Fatalf("tickDecimal(%v, %d) produced %v, which is not an American price: %v", d, tick, got, err)
			}
			if int64(am)%tick != 0 {
				t.Errorf("tickDecimal(%v, %d) = %v (American %d), not on the %d-cent ladder", d, tick, got, am, tick)
			}
		}
	}
}

// TestTickDecimalCrossesTheEvenMoneyGap pins the specific ladder rung the bug
// fell off. There is no American price between −100 and +100, so the rung below
// even money on a ten-cent ladder is −110, not +90.
func TestTickDecimalCrossesTheEvenMoneyGap(t *testing.T) {
	got, err := tickDecimal(1.9999, 10)
	if err != nil {
		t.Fatalf("tickDecimal: %v", err)
	}
	want := 1 + 100.0/110.0 // American −110
	if !approx(got, want, 1e-12) {
		t.Fatalf("tickDecimal(1.9999, 10) = %v, want %v (American −110)", got, want)
	}
}

func TestWorsenAmericanWalksTheLadder(t *testing.T) {
	cases := []struct{ v, tick, want int64 }{
		{110, 10, 100},
		{100, 10, -110},
		{100, 5, -105},
		{100, 1, -101},
		{-110, 10, -120},
		{-101, 1, -102},
	}
	for _, c := range cases {
		if got := worsenAmerican(c.v, c.tick); got != c.want {
			t.Errorf("worsenAmerican(%d, %d) = %d, want %d", c.v, c.tick, got, c.want)
		}
	}
}

func TestTickDecimalRejectsAnUnusableTick(t *testing.T) {
	if _, err := tickDecimal(1.9, 0); !errors.Is(err, provider.ErrInvalidSnapshot) {
		t.Fatalf("error = %v, want ErrInvalidSnapshot", err)
	}
}

// TestQuoteRefusesAOneSidedMarket guards the assumption every margin model here
// makes: a margin is defined over a market, and a market has at least two
// answers. A single-selection "market" would devig to probability one.
func TestQuoteRefusesAOneSidedMarket(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	out := make([]float64, 1)
	err := a.quote([]float64{1.0}, out, books()[0])
	if !errors.Is(err, provider.ErrInvalidSnapshot) {
		t.Fatalf("error = %v, want ErrInvalidSnapshot", err)
	}
}

// TestQuoteRefusesAnImpossibleProbability checks the guard between the vig step
// and the price conversion. A quoted implied probability at or above one has no
// decimal price at all, and letting it through would surface as a rejected
// snapshot far from the market that produced it.
func TestQuoteRefusesAnImpossibleProbability(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	out := make([]float64, 2)
	// 0.99 with a multiplicative 5.5% margin quotes at 1.045 implied.
	err := a.quote([]float64{0.99, 0.01}, out, bookDef{slug: "probe", margin: 0.055, tickAmerican: 1})
	if !errors.Is(err, provider.ErrInvalidSnapshot) {
		t.Fatalf("error = %v, want ErrInvalidSnapshot", err)
	}
}

func TestClampTwoSidedBoundsAndComplements(t *testing.T) {
	for _, p := range []float64{-1, 0, 0.001, 0.05, 0.5, 0.95, 0.999, 1, 2} {
		got := clampTwoSided(p)
		if got[0] < minFairProb || got[0] > maxFairProb {
			t.Errorf("clampTwoSided(%v)[0] = %v, outside [%v, %v]", p, got[0], minFairProb, maxFairProb)
		}
		if !approx(got[0]+got[1], 1, 1e-15) {
			t.Errorf("clampTwoSided(%v) sums to %v, want 1", p, got[0]+got[1])
		}
	}
}

func TestClampFieldNormalisesAndBounds(t *testing.T) {
	in := []float64{0.9, 0.0001, -0.5, 0.3}
	clampField(in)
	sum := 0.0
	for _, v := range in {
		if v <= 0 {
			t.Errorf("clampField left a non-positive probability %v", v)
		}
		sum += v
	}
	if !approx(sum, 1, 1e-12) {
		t.Errorf("clampField sum = %v, want 1", sum)
	}
}

// TestProbabilityModelsRejectZeroDispersion checks the guard that keeps a
// degenerate contest — one with no uncertainty left — from producing a division
// by zero and a NaN price rather than a clamped one.
func TestProbabilityModelsRejectZeroDispersion(t *testing.T) {
	if _, err := moneylineProbs(0, 0, false); err == nil {
		t.Error("moneylineProbs accepted zero dispersion")
	}
	if _, err := moneylineProbs(0, 0, true); err == nil {
		t.Error("three-way moneylineProbs accepted zero dispersion")
	}
	if _, err := spreadProbs(0, 0, 0); err == nil {
		t.Error("spreadProbs accepted zero dispersion")
	}
	if _, err := thresholdProbs(0, 0, 1); err == nil {
		t.Error("thresholdProbs accepted zero dispersion")
	}
}

// TestThreeWayMoneylineSumsToOne checks the derivation that makes the draw price
// move with the fixture instead of being a fixed share.
func TestThreeWayMoneylineSumsToOne(t *testing.T) {
	for _, mean := range []float64{-2, -0.5, 0, 0.5, 2} {
		p, err := moneylineProbs(mean, 1.4, true)
		if err != nil {
			t.Fatalf("moneylineProbs: %v", err)
		}
		if len(p) != 3 {
			t.Fatalf("three-way market has %d selections", len(p))
		}
		if !approx(p[0]+p[1]+p[2], 1, 1e-12) {
			t.Errorf("mean %v: probabilities sum to %v", mean, p[0]+p[1]+p[2])
		}
	}
	// A balanced fixture must have a fatter draw than a mismatch: that is the
	// whole reason the draw is derived from the margin rather than assigned.
	level, _ := moneylineProbs(0, 1.4, true)
	lopsided, _ := moneylineProbs(2.5, 1.4, true)
	if level[1] <= lopsided[1] {
		t.Errorf("draw probability %v on a level fixture is not above %v on a mismatch", level[1], lopsided[1])
	}
}

func TestThresholdLineFloorsAtHalfAPoint(t *testing.T) {
	cases := map[float64]float64{-3: 0.5, 0: 0.5, 0.1: 0.5, 0.4: 0.5, 2.72: 2.5, 228.3: 228.5, 44.9: 45}
	for in, want := range cases {
		if got := thresholdLine(in); got != want {
			t.Errorf("thresholdLine(%v) = %v, want %v", in, got, want)
		}
	}
	// The floor is not cosmetic: domain rejects a total at or below zero.
	if _, err := domain.NewMarket(domain.MarketParams{
		ID: "probe", EventID: "probe-ev", Type: domain.MarketTypeTotal,
		Line: mustLine(t, thresholdLine(-3)), Status: domain.MarketStatusOpen, UpdatedAt: testNow,
	}); err != nil {
		t.Fatalf("a floored total line is still rejected by the domain: %v", err)
	}
}

func mustLine(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("NewLine(%v): %v", v, err)
	}
	return l
}

func TestSteamRampHoldsThenFades(t *testing.T) {
	const (
		full   = int64(steamFullBlocks * steamBlockSteps)
		window = int64(steamBlocks * steamBlockSteps)
	)
	if got := steamRamp(-1); got != 0 {
		t.Errorf("steamRamp(-1) = %v, a jump cannot act before it happens", got)
	}
	if got := steamRamp(0); got != 1 {
		t.Errorf("steamRamp(0) = %v, want full amplitude at the jump", got)
	}
	if got := steamRamp(full); got != 1 {
		t.Errorf("steamRamp(full) = %v, want the move still permanent", got)
	}
	if got := steamRamp(window); got != 0 {
		t.Errorf("steamRamp(window) = %v, want zero at the scan edge", got)
	}
	mid := steamRamp((full + window) / 2)
	if mid <= 0 || mid >= 1 {
		t.Errorf("steamRamp mid-fade = %v, want a value strictly between 0 and 1", mid)
	}
}

func TestCostOfAMarketlessScopeIsOne(t *testing.T) {
	// A scope with no markets never reaches Fetch — Scope.Validate rejects it —
	// but Cost is called by the scheduler's budget BEFORE the request, so it must
	// not report that a request is free.
	a := newTestAdapter(t, testSeed, testNow)
	if got := a.Cost(provider.Scope{League: leagues()[0].leagueID()}); got != 1 {
		t.Fatalf("Cost of a marketless scope = %d, want 1", got)
	}
}

func TestOutrightProbsHandlesAWideStrengthField(t *testing.T) {
	// The overflow guard: without subtracting the maximum, a strong field
	// exponentiates to +Inf and every runner prices at NaN.
	strength := []float64{800, 799, -800, 0}
	p := outrightProbs(strength)
	sum := 0.0
	for _, v := range p {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("outrightProbs produced %v", v)
		}
		sum += v
	}
	if !approx(sum, 1, 1e-12) {
		t.Fatalf("outrightProbs sum = %v, want 1", sum)
	}
	if len(outrightProbs(nil)) != 0 {
		t.Fatal("outrightProbs(nil) should be empty")
	}
}
