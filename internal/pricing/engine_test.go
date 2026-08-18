package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// relTol is the repository's relative tolerance for comparing two computed
// floats. Its justification is in harness_test.go's approxRel.
const relTol = 1e-12

// threeWayRoles is the only market shape whose moneyline admits a draw, which is
// what makes it the three-way case CLAUDE.md §4's devig discussion is about.
var threeWayRoles = []domain.SelectionRole{
	domain.SelectionRoleHome, domain.SelectionRoleAway, domain.SelectionRoleDraw,
}

var twoWayRoles = []domain.SelectionRole{
	domain.SelectionRoleHome, domain.SelectionRoleAway,
}

// TestPowerQuotedMarketRecoversItsLatentProbabilities is the sharpest statement
// this phase can make, and it is the test the phase brief asks for.
//
// The synthetic provider quotes three-way markets by applying a POWER margin:
// every latent probability is raised to a common exponent k chosen so the
// implied probabilities sum to 1 + margin. odds.DevigPower inverts exactly that
// relation. So a market generated this way has a KNOWN RIGHT ANSWER, and the
// engine — which is never told the latent probabilities, only the prices —
// has to get back to it.
//
// Anything less than recovery here means the margin is being removed by a model
// that does not match the model that added it, and every expected value computed
// downstream is wrong by that difference. CLAUDE.md §10: "Wrong odds math is the
// one bug class that destroys the project's credibility."
func TestPowerQuotedMarketRecoversItsLatentProbabilities(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		latent []float64
		margin float64
	}{
		{"balanced three-way", []float64{0.42, 0.33, 0.25}, 0.020},
		{"heavy favourite", []float64{0.78, 0.14, 0.08}, 0.020},
		{"longshot present", []float64{0.880, 0.095, 0.025}, 0.045},
		{"soft book margin", []float64{0.51, 0.28, 0.21}, 0.065},
		{"near-uniform", []float64{0.34, 0.33, 0.33}, 0.038},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			implied := powerMargin(t, tc.latent, tc.margin)

			// The generator's own contract, restated as a precondition: the
			// quoted market really does carry the margin it was asked for. If
			// this fails the test below is proving nothing.
			sum := 0.0
			for _, p := range implied {
				sum += p
			}
			if !approxRel(sum, 1+tc.margin, 1e-9) {
				t.Fatalf("power margin produced Σp = %.15f, want %.15f", sum, 1+tc.margin)
			}

			rec := marketFixture{
				id:         "mkt-power",
				selections: threeWayRoles,
				books: []bookFixture{
					{slug: "sharpline", prices: decimalsOf(implied)},
				},
			}.build(t)

			engine := mustEngine(t, Options{
				ReferenceBooks: []domain.Slug{"sharpline"},
				DevigMethod:    odds.MethodPower,
			})
			out, err := engine.Price(context.Background(), rec)
			if err != nil {
				t.Fatalf("Price: %v", err)
			}

			if out.Fair.Method != odds.MethodPower {
				t.Fatalf("fair value came from %s, want power", out.Fair.Method)
			}
			if out.Fair.Fallback {
				t.Fatal("fell back to the multiplicative method; the power devig should have succeeded")
			}
			if got := len(out.Fair.Selections); got != len(tc.latent) {
				t.Fatalf("got %d fair selections, want %d", got, len(tc.latent))
			}

			worst := 0.0
			for i, fs := range out.Fair.Selections {
				got := float64(fs.Probability)
				if rel := math.Abs(got-tc.latent[i]) / tc.latent[i]; rel > worst {
					worst = rel
				}
				if !approxRel(got, tc.latent[i], relTol) {
					t.Errorf("selection %d: fair probability %.17g, want %.17g (relative error %.3g)",
						i, got, tc.latent[i], math.Abs(got-tc.latent[i])/tc.latent[i])
				}
			}
			// Logged so the round-trip error is a REPORTED NUMBER rather than a
			// pass/fail bit. A tolerance that is never approached is a tolerance
			// nobody can tell has quietly started to matter.
			t.Logf("worst relative recovery error %.3g (tolerance %.0e)", worst, relTol)

			// The fair PRICE is the fair probability's reciprocal, and it is the
			// number the board shows. Checking it separately catches an error in
			// the conversion that the probability assertion would not.
			for i, fs := range out.Fair.Selections {
				want := 1 / tc.latent[i]
				if !approxRel(float64(fs.Decimal), want, relTol) {
					t.Errorf("selection %d: fair decimal %.17g, want %.17g", i, float64(fs.Decimal), want)
				}
			}

			// The three margin quantities describe the market that was built.
			if !approxRel(out.Fair.Margin.ImpliedSum, 1+tc.margin, 1e-9) {
				t.Errorf("implied sum %.15f, want %.15f", out.Fair.Margin.ImpliedSum, 1+tc.margin)
			}
		})
	}
}

// TestTwoWayMultiplicativeMarketRecoversItsLatentProbabilities is the same claim
// for the other relation the synthetic book set uses.
//
// Two-way synthetic markets are quoted with a MULTIPLICATIVE margin, so the
// method that inverts them exactly is the multiplicative devig — and running the
// power devig over the same market does NOT recover the latent probabilities.
// Knowing which relation produced a market is the whole reason the method is
// selectable and recorded.
func TestTwoWayMultiplicativeMarketRecoversItsLatentProbabilities(t *testing.T) {
	t.Parallel()

	latent := []float64{0.6125, 0.3875}
	const margin = 0.038
	implied := multiplicativeMargin(t, latent, margin)

	rec := marketFixture{
		id:         "mkt-mult",
		selections: twoWayRoles,
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}.build(t)

	engine := mustEngine(t, Options{
		ReferenceBooks: []domain.Slug{"sharpline"},
		DevigMethod:    odds.MethodMultiplicative,
	})
	out, err := engine.Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	for i, fs := range out.Fair.Selections {
		if !approxRel(float64(fs.Probability), latent[i], relTol) {
			t.Errorf("selection %d: fair probability %.17g, want %.17g",
				i, float64(fs.Probability), latent[i])
		}
	}
}

// TestReferenceBookHasNoPositiveEdgeAgainstItsOwnFairValue is the invariant that
// makes a devig self-checking.
//
// The fair probabilities are extracted FROM the reference book's own prices, so
// the reference book cannot be offering value against them: on an overround
// market every quoted implied probability is at least its fair probability, so
// every expected value is at most zero. A positive one would mean the devig
// returned probabilities that do not correspond to the prices they came from.
//
// Asserted across all four methods, because it is a property of devigging and
// not of any one model.
func TestReferenceBookHasNoPositiveEdgeAgainstItsOwnFairValue(t *testing.T) {
	t.Parallel()

	markets := []struct {
		name   string
		roles  []domain.SelectionRole
		latent []float64
		margin float64
	}{
		{"three-way", threeWayRoles, []float64{0.42, 0.33, 0.25}, 0.020},
		{"two-way", twoWayRoles, []float64{0.6125, 0.3875}, 0.045},
		{"favourite and longshot", threeWayRoles, []float64{0.80, 0.155, 0.045}, 0.055},
	}

	for _, m := range markets {
		for _, method := range odds.DevigMethods() {
			t.Run(m.name+"/"+method.String(), func(t *testing.T) {
				t.Parallel()

				implied := powerMargin(t, m.latent, m.margin)
				rec := marketFixture{
					id:         "mkt-self",
					selections: m.roles,
					books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
				}.build(t)

				engine := mustEngine(t, Options{
					ReferenceBooks: []domain.Slug{"sharpline"},
					DevigMethod:    method,
				})
				out, err := engine.Price(context.Background(), rec)
				if err != nil {
					t.Fatalf("Price: %v", err)
				}

				book := out.Books[0]
				if !book.Reference {
					t.Fatalf("book %s is not marked as the reference", book.Slug)
				}
				for _, q := range book.Quotes {
					if q.Status != QuoteStatusPriced {
						t.Fatalf("reference quote on %s was not scored: %s", q.SelectionID, q.Status)
					}
					// A tolerance rather than <= 0 exactly: the fair
					// probabilities are the output of a bracketed root solve, so
					// the product q·d at the break-even point is correct to
					// within a few ULPs and may land a hair above 1.
					if q.ExpectedValue > relTol {
						t.Errorf("reference book has expected value %+.17g on %s under %s; "+
							"a book cannot have an edge against fair value devigged from its own prices",
							q.ExpectedValue, q.SelectionID, out.Fair.Method)
					}
					if q.Kelly != 0 {
						t.Errorf("reference book has a non-zero Kelly fraction %.17g on %s; "+
							"Kelly must be exactly zero at a non-positive edge", q.Kelly, q.SelectionID)
					}
				}
			})
		}
	}
}

// TestMarginTripleKeepsThreeDistinctQuantities pins the relation between the
// three numbers phase 1 refuses to conflate. Reporting one under another's name
// mis-states a book's margin by a relative 5% on a normal market.
func TestMarginTripleKeepsThreeDistinctQuantities(t *testing.T) {
	t.Parallel()

	const margin = 0.045
	implied := powerMargin(t, []float64{0.5, 0.3, 0.2}, margin)
	rec := marketFixture{
		id:         "mkt-margin",
		selections: threeWayRoles,
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}.build(t)

	out, err := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"sharpline"}}).
		Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}

	m := out.Fair.Margin
	if !approxRel(m.BookingPercentage, 100*m.ImpliedSum, relTol) {
		t.Errorf("booking percentage %.15f, want 100·S = %.15f", m.BookingPercentage, 100*m.ImpliedSum)
	}
	if !approxRel(m.Overround, m.ImpliedSum-1, relTol) {
		t.Errorf("overround %.15f, want S−1 = %.15f", m.Overround, m.ImpliedSum-1)
	}
	if !approxRel(m.Vig, (m.ImpliedSum-1)/m.ImpliedSum, relTol) {
		t.Errorf("vig %.15f, want (S−1)/S = %.15f", m.Vig, (m.ImpliedSum-1)/m.ImpliedSum)
	}
	if m.Vig >= m.Overround {
		t.Errorf("vig %.15f is not strictly below overround %.15f on an overround book", m.Vig, m.Overround)
	}
	if m.Selections != 3 {
		t.Errorf("margin covers %d selections, want 3", m.Selections)
	}
}

// TestQuoteAtADifferentLineIsNotScored is the CLV rule applied one step earlier.
//
// odds.CLV refuses to compare a wager's placement price against a closing price
// across a moved line. The same category error is available here, and it is
// worse because it manufactures edges: a book quoting the same PRICE at a
// friendlier LINE looks like free money and is a different bet.
func TestQuoteAtADifferentLineIsNotScored(t *testing.T) {
	t.Parallel()

	latent := []float64{0.52, 0.48}
	refImplied := multiplicativeMargin(t, latent, 0.02)

	// The challenger is MISPRICED on the home side and mean on the away side —
	// implied 0.98·latent and 1.06·latent — which is what a genuine +EV
	// opportunity looks like: a book is not beatable because its margin is
	// small, it is beatable because one of its sides is wrong. Its market is
	// still overround overall (Σ ≈ 1.019), so nothing here is an arbitrage.
	softImplied := []float64{latent[0] * 0.98, latent[1] * 1.06}

	minus35, err := domain.NewLine(-3.5)
	if err != nil {
		t.Fatalf("NewLine: %v", err)
	}
	plus35, err := domain.NewLine(3.5)
	if err != nil {
		t.Fatalf("NewLine: %v", err)
	}
	minus30, err := domain.NewLine(-3.0)
	if err != nil {
		t.Fatalf("NewLine: %v", err)
	}
	plus30, err := domain.NewLine(3.0)
	if err != nil {
		t.Fatalf("NewLine: %v", err)
	}

	rec := marketFixture{
		id:         "mkt-line",
		selections: twoWayRoles,
		line:       minus35,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(refImplied), lines: []domain.Line{minus35, plus35}},
			{slug: "matched", prices: decimalsOf(softImplied), lines: []domain.Line{minus35, plus35}},
			{slug: "moved", prices: decimalsOf(softImplied), lines: []domain.Line{minus30, plus30}},
		},
	}.build(t)
	rec.Market.Type = domain.MarketTypeSpread.String()

	// The reference market was built with a MULTIPLICATIVE margin, so the
	// multiplicative devig inverts it exactly and the fair probabilities are the
	// latent ones. That makes the expected values below arithmetic the test can
	// state rather than an artefact of which devig model was configured.
	out, err := mustEngine(t, Options{
		ReferenceBooks: []domain.Slug{"sharpline"},
		DevigMethod:    odds.MethodMultiplicative,
	}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}

	byslug := map[domain.Slug]BookAssessment{}
	for _, b := range out.Books {
		byslug[b.Slug] = b
	}

	matched := byslug["matched"]
	best, ok := matched.BestEdge()
	if !ok {
		t.Fatal("the book quoting AT the reference line produced no scored quote")
	}
	for _, q := range matched.Quotes {
		if q.Status != QuoteStatusPriced {
			t.Errorf("matched book quote on %s: status %s, want priced", q.SelectionID, q.Status)
		}
	}
	if best.ExpectedValue <= 0 {
		t.Errorf("the mispriced side of a book quoting at the reference line should show positive EV, "+
			"got %+.6g", best.ExpectedValue)
	}
	if best.Kelly <= 0 {
		t.Errorf("a positive edge must produce a positive Kelly fraction, got %g", best.Kelly)
	}
	if want := best.Kelly * DefaultKellyMultiplier; !approxRel(best.FractionalKelly, want, relTol) {
		t.Errorf("fractional Kelly %g, want %g (quarter of %g)", best.FractionalKelly, want, best.Kelly)
	}

	moved := byslug["moved"]
	for _, q := range moved.Quotes {
		if q.Status != QuoteStatusLineMismatch {
			t.Errorf("book at a different line: status %s, want line_mismatch", q.Status)
		}
		if q.ExpectedValue != 0 || q.Kelly != 0 || q.Edge != 0 {
			t.Errorf("a quote at a different line carries EV %+g, edge %+g, Kelly %g; "+
				"none of the three is comparable across a moved line",
				q.ExpectedValue, q.Edge, q.Kelly)
		}
		// The price and line themselves must survive — the middles detector
		// needs exactly this disagreement.
		if !q.Line.Equal(minus30) && !q.Line.Equal(plus30) {
			t.Errorf("quote lost its line: %s", q.Line)
		}
	}
	if _, ok := moved.BestEdge(); ok {
		t.Error("a book quoting only at a different line reported a best edge")
	}
}

// TestStaleBookIsCarriedButNotScored exercises the challenger half of the
// staleness policy.
func TestStaleBookIsCarriedButNotScored(t *testing.T) {
	t.Parallel()

	latent := []float64{0.55, 0.45}
	implied := multiplicativeMargin(t, latent, 0.02)
	generous := multiplicativeMargin(t, latent, 0.005)

	rec := marketFixture{
		id:         "mkt-stale-book",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(implied)},
			{
				slug:       "lagging",
				prices:     decimalsOf(generous),
				observedAt: fixtureEpoch.Add(-30 * time.Minute),
			},
		},
	}.build(t)

	out, err := mustEngine(t, Options{
		ReferenceBooks: []domain.Slug{"sharpline"},
		MaxQuoteAge:    10 * time.Minute,
	}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}

	var lagging BookAssessment
	for _, b := range out.Books {
		if b.Slug == "lagging" {
			lagging = b
		}
	}
	if lagging.Eligible {
		t.Fatal("a book 30 minutes behind the anchor was admitted under a 10-minute policy")
	}
	if lagging.AgeSeconds < 1800 {
		t.Errorf("stale book age %g s, want at least 1800", lagging.AgeSeconds)
	}
	if lagging.Margin.Selections != 2 {
		t.Error("a stale book lost its margin; the last known market is still a fact about the book")
	}
	for _, q := range lagging.Quotes {
		if q.Status != QuoteStatusStale {
			t.Errorf("stale book quote status %s, want stale", q.Status)
		}
		if q.Decimal <= 1 {
			t.Error("a stale book lost its price; the board should still be able to show the last line")
		}
		if q.ExpectedValue != 0 || q.Kelly != 0 {
			t.Errorf("a stale quote carries EV %+g and Kelly %g; an edge against a price nobody "+
				"is offering reads as an opportunity and is not one", q.ExpectedValue, q.Kelly)
		}
	}
}

// TestStaleReferenceRefusesTheWholeMarket exercises the reference half. It is
// the sharper of the two: the fair value rests entirely on this one book, so an
// old quote does not cost one book, it invalidates the market.
func TestStaleReferenceRefusesTheWholeMarket(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-stale-ref",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(implied), observedAt: fixtureEpoch.Add(-time.Hour)},
		},
	}.build(t)

	_, err := mustEngine(t, Options{
		ReferenceBooks:  []domain.Slug{"sharpline"},
		MaxReferenceAge: 5 * time.Minute,
	}).Price(context.Background(), rec)
	if !errors.Is(err, ErrReferenceStale) {
		t.Fatalf("got %v, want ErrReferenceStale", err)
	}
}

// TestMarketWithNoDesignatedSharpBookIsRefused is the behaviour CLAUDE.md §6
// requires and the one most tempting to soften: with no sharp book there is no
// fair value, and a consensus of whoever happens to be quoting is a different
// quantity wearing the same name.
func TestMarketWithNoDesignatedSharpBookIsRefused(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.05)
	rec := marketFixture{
		id:         "mkt-no-ref",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "softbook", prices: decimalsOf(implied)},
			{slug: "othersoft", prices: decimalsOf(implied)},
		},
	}.build(t)

	_, err := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"sharpline"}}).
		Price(context.Background(), rec)
	if !errors.Is(err, ErrNoReferenceBook) {
		t.Fatalf("got %v, want ErrNoReferenceBook", err)
	}
}

// TestIncompleteReferenceIsRefused: a partial market has no margin to remove,
// and devigging one manufactures a near-certainty on the side that was quoted.
func TestIncompleteReferenceIsRefused(t *testing.T) {
	t.Parallel()

	implied := powerMargin(t, []float64{0.42, 0.33, 0.25}, 0.02)
	rec := marketFixture{
		id:         "mkt-partial",
		selections: threeWayRoles,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(implied), omit: []int{2}},
		},
	}.build(t)

	_, err := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"sharpline"}}).
		Price(context.Background(), rec)
	if !errors.Is(err, ErrIncompleteReference) {
		t.Fatalf("got %v, want ErrIncompleteReference", err)
	}
}

// TestDevigFallbackIsRecordedAndNotSilent.
//
// The additive method subtracts an equal slice of the margin from every
// selection, so a long enough shot goes to zero or below and the method refuses.
// The engine falls back to multiplicative — the only total method — and must say
// so on the record, because multiplicative is the method that is wrong about
// longshots and this is by construction a market with one.
func TestDevigFallbackIsRecordedAndNotSilent(t *testing.T) {
	t.Parallel()

	// A 500-1 outsider. The additive method subtracts (S−1)/n ≈ 0.0067 from every
	// implied probability, and this one's is about 0.0031, so the subtraction
	// takes it below zero and odds.DevigAdditive refuses — which is exactly the
	// failure mode CLAUDE.md §4 means when it says the methods "disagree
	// meaningfully on longshots".
	latent := []float64{0.920, 0.078, 0.002}
	implied := powerMargin(t, latent, 0.02)

	rec := marketFixture{
		id:         "mkt-fallback",
		selections: threeWayRoles,
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}.build(t)

	out, err := mustEngine(t, Options{
		ReferenceBooks: []domain.Slug{"sharpline"},
		DevigMethod:    odds.MethodAdditive,
	}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if !out.Fair.Fallback {
		t.Fatal("additive should have refused a market with a 200-1 shot and fallen back")
	}
	if out.Fair.RequestedMethod != odds.MethodAdditive {
		t.Errorf("requested method recorded as %s, want additive", out.Fair.RequestedMethod)
	}
	if out.Fair.Method != odds.MethodMultiplicative {
		t.Errorf("fallback method recorded as %s, want multiplicative", out.Fair.Method)
	}
}

// TestMethodDisagreementGrowsWithLongshots is the measurable form of CLAUDE.md
// §4's claim that the four methods "disagree meaningfully on longshots".
//
// It is asserted as a comparison between two markets rather than against a fixed
// threshold, because the magnitude depends on the margin and the shape of the
// market and a hard number would be a fact about these particular fixtures.
func TestMethodDisagreementGrowsWithLongshots(t *testing.T) {
	t.Parallel()

	engine := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"sharpline"}})

	price := func(latent []float64) FairValue {
		t.Helper()
		implied := powerMargin(t, latent, 0.045)
		rec := marketFixture{
			id:         "mkt-spread-of-methods",
			selections: threeWayRoles,
			books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
		}.build(t)
		out, err := engine.Price(context.Background(), rec)
		if err != nil {
			t.Fatalf("Price: %v", err)
		}
		return out.Fair
	}

	balanced := price([]float64{0.34, 0.33, 0.33})
	skewed := price([]float64{0.90, 0.08, 0.02})

	if balanced.Disagreement < 0 || skewed.Disagreement < 0 {
		t.Fatal("method comparison is off by default; it is the only error bar on a fair probability")
	}
	if balanced.MethodsCompared < 2 || skewed.MethodsCompared < 2 {
		t.Fatalf("compared %d and %d methods; a disagreement over fewer than two is not a comparison",
			balanced.MethodsCompared, skewed.MethodsCompared)
	}
	if skewed.Disagreement <= balanced.Disagreement {
		t.Errorf("disagreement on a longshot market (%.6g) is not greater than on a balanced one (%.6g)",
			skewed.Disagreement, balanced.Disagreement)
	}
}

// TestEngineIsAPureFunctionOfTheRecord is a REQUIREMENT of the service seam, not
// a nicety: the service suppresses a republication whose input fingerprint has
// not changed, which is only sound if two calls over one record agree.
//
// Compared as marshalled JSON rather than with reflect.DeepEqual so that a
// future field which is equal-but-not-identical — a map, a time in a different
// location — cannot pass.
func TestEngineIsAPureFunctionOfTheRecord(t *testing.T) {
	t.Parallel()

	implied := powerMargin(t, []float64{0.42, 0.33, 0.25}, 0.02)
	soft := powerMargin(t, []float64{0.40, 0.34, 0.26}, 0.055)
	rec := marketFixture{
		id:         "mkt-pure",
		selections: threeWayRoles,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(implied)},
			{slug: "lowtide", prices: decimalsOf(soft)},
		},
	}.build(t)

	engine := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"sharpline"}})

	var encoded [3][]byte
	for i := range encoded {
		out, err := engine.Price(context.Background(), rec)
		if err != nil {
			t.Fatalf("Price: %v", err)
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		encoded[i] = b
		time.Sleep(time.Millisecond)
	}
	for i := 1; i < len(encoded); i++ {
		if string(encoded[i]) != string(encoded[0]) {
			t.Fatalf("pricing the same record twice produced different output:\n%s\n%s",
				encoded[0], encoded[i])
		}
	}
}

// TestPriceHonoursACancelledContext. The engine does no I/O, so the check exists
// to stop a whole backlog of queued markets being priced after the process has
// been told to stop.
func TestPriceHonoursACancelledContext(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-ctx",
		selections: twoWayRoles,
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}.build(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"sharpline"}}).Price(ctx, rec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// TestSofterBookShowsPositiveEVAgainstTheSharpBook is the +EV finder's core
// claim, in one market: a book charging a wider margin on the same latent
// probabilities is offering worse prices, so at least one of its selections must
// price WORSE than fair and none may price better — and the finder must report
// the negative, not invent a positive.
func TestSofterBookCannotShowValueAgainstTheSharpBook(t *testing.T) {
	t.Parallel()

	latent := []float64{0.42, 0.33, 0.25}
	sharp := powerMargin(t, latent, 0.020)
	soft := powerMargin(t, latent, 0.065)

	rec := marketFixture{
		id:         "mkt-soft",
		selections: threeWayRoles,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(sharp)},
			{slug: "lowtide", prices: decimalsOf(soft)},
		},
	}.build(t)

	out, err := mustEngine(t, Options{
		ReferenceBooks: []domain.Slug{"sharpline"},
		DevigMethod:    odds.MethodPower,
	}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}

	for _, b := range out.Books {
		if b.Slug != "lowtide" {
			continue
		}
		for _, q := range b.Quotes {
			if q.Status != QuoteStatusPriced {
				t.Fatalf("soft book quote not scored: %s", q.Status)
			}
			if q.ExpectedValue >= 0 {
				t.Errorf("a book charging 6.5%% against a 2.0%% reference on identical latent "+
					"probabilities cannot offer value; got EV %+.6g on %s",
					q.ExpectedValue, q.SelectionID)
			}
		}
		if b.Margin.Overround <= out.Fair.Margin.Overround {
			t.Errorf("soft book overround %.6g is not above the reference's %.6g",
				b.Margin.Overround, out.Fair.Margin.Overround)
		}
	}

	// Nothing on this market is +EV, and the finder must say so rather than
	// returning the least-bad price as though it were a signal.
	if _, q, ok := out.BestEdge(); ok && q.ExpectedValue > 0 {
		t.Errorf("BestEdge reported a positive edge of %+.6g on a market where every book "+
			"is at or wider than the reference's margin", q.ExpectedValue)
	}
}
