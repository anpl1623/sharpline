// Removing the margin, and the three numbers that describe it.
//
// # The three margin quantities stay three numbers
//
// Phase 1 is emphatic that booking percentage (100·S), overround (S−1) and vig
// ((S−1)/S) are DISTINCT and that conflating them is "a standard way to mis-state
// a book's margin by a relative 5%". odds.Margin carries all three together for
// exactly that reason, so this package transports the struct rather than
// unpacking one field and naming it "margin".
//
// # Why the fair value comes from ONE book
//
// doc.go argues it at length. The mechanical consequence lives here: devigging
// requires a COMPLETE market, because the margin is the excess of Σ 1/d over 1
// and a partial market has no such excess. Merging several books' quotes into a
// synthetic complete market to devig would produce a booking percentage no book
// ever posted, and the "fair" probabilities removed from it would be an artefact
// of which books happened to be present.
//
// # Per-selection margin comes from the devig, not from a second convention
//
// odds.VigContributions apportions a market's overround under an explicit
// Attribution — proportional (the multiplicative devig, per selection) or uniform
// (the additive devig, per selection). It is used here for EVERY book, because it
// is the only way to say which side a book is charging most on without devigging
// a book we are not treating as sharp.
//
// It is NOT used to produce the reference book's fair probabilities. Those come
// from the configured DevigMethod, and if Shin or power produced the fair value
// while a proportional attribution produced the per-selection split, the two
// halves of one record would disagree about the same market. So the reference
// book's per-selection excess is computed from its OWN devig — implied minus
// fair — which is the correct attribution under the method that was actually
// used, and the Attribution-based split is carried alongside as the
// cross-book-comparable view it is.
package pricing

import (
	"fmt"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// FairSelection is one selection's no-vig fair value, derived from the reference
// book's quote on it.
type FairSelection struct {
	SelectionID domain.SelectionID   `json:"selection_id"`
	Role        domain.SelectionRole `json:"role"`
	Name        string               `json:"name"`

	// Probability is the fair probability after the reference book's margin is
	// removed. Across the market these sum to 1.
	Probability odds.Probability `json:"probability"`

	// Decimal is the no-vig fair price: the price at which this selection would
	// carry no margin at all. It is 1/Probability, through the package's single
	// definition.
	Decimal odds.Decimal `json:"decimal"`

	// ReferenceDecimal and ReferenceImplied are the reference book's own quote
	// and the probability it implies, margin included. They are carried so a
	// consumer can see the before and after without holding the source record.
	ReferenceDecimal odds.Decimal     `json:"reference_decimal"`
	ReferenceImplied odds.Probability `json:"reference_implied"`

	// Excess is ReferenceImplied − Probability: this selection's absolute share
	// of the reference book's margin, under the devig method that was actually
	// used. Across the market these sum to the reference book's Overround.
	Excess float64 `json:"excess"`

	// RelativeMargin is Excess/Probability — the margin as a fraction of the
	// fair probability, which is the form comparable across selections of wildly
	// different lengths. It is the number that shows a longshot being shaded
	// harder than a favourite, and therefore the number that shows the four
	// devig methods disagreeing.
	RelativeMargin float64 `json:"relative_margin"`

	// AttributedExcess and AttributedShare are the same selection's margin under
	// the record's stated Attribution rather than under its devig method. They
	// are the cross-book-comparable form: every book on the record carries them
	// (see BookAssessment), because a book we are not treating as sharp is not
	// devigged and has no other per-selection margin to report.
	//
	// Both are zero when the attribution was undefined for this market —
	// odds.AttributionUniform is not total, and a long enough shot drives its
	// fair probability to zero or below.
	AttributedExcess float64 `json:"attributed_excess"`
	AttributedShare  float64 `json:"attributed_share"`
}

// FairValue is one market's no-vig fair value and the provenance of it.
type FairValue struct {
	// Method is the devig model that ACTUALLY produced Selections. It is not
	// necessarily RequestedMethod; see Fallback.
	Method odds.DevigMethod `json:"method"`

	// RequestedMethod is the method the engine was configured with.
	RequestedMethod odds.DevigMethod `json:"requested_method"`

	// Fallback reports that RequestedMethod refused this market and Method was
	// used instead. Recorded rather than hidden: a fair price produced by a
	// different model from the one the operator configured is a fair price the
	// operator needs to know about.
	Fallback bool `json:"fallback"`

	// Attribution names the per-selection margin convention the AttributedExcess
	// and AttributedShare fields were computed under. It is a string on the wire
	// because odds.Attribution is an unexported-safe uint8 with no text
	// marshaller, and a numeric attribution on a JSON record would be unreadable
	// in kafka-ui — which is where a wrong one would be noticed.
	Attribution string `json:"attribution"`

	// Parameter is the method's fitted free parameter — the power exponent k,
	// Shin's insider share z, additive's per-selection subtraction, or 0 for
	// multiplicative, which has none. Carried because CLAUDE.md §6's analytics
	// surface is the reason phase 1 exposed it.
	Parameter float64 `json:"parameter"`

	// Iterations is the root-solver step count: 0 for the closed-form methods
	// and for a market already fair.
	Iterations int `json:"iterations"`

	// Margin is the reference book's margin triple over this market.
	Margin Margin `json:"margin"`

	// Selections are the fair values, in the market's selection order.
	Selections []FairSelection `json:"selections"`

	// Disagreement is the largest absolute probability difference between the
	// method that was used and any other method that could also price this
	// market — the number CLAUDE.md §4 is describing when it says the four
	// "disagree meaningfully on longshots".
	//
	// It is the honest error bar on a fair probability: an EV of 1% on a market
	// where the four methods span 3 percentage points is not a signal, and a
	// consumer that cannot see the spread cannot tell the difference. Negative
	// means it was not computed (Options.SkipMethodComparison).
	Disagreement float64 `json:"disagreement"`

	// MethodsCompared is how many of the four methods priced this market
	// successfully. Additive in particular fails on a board with a big longshot,
	// so a Disagreement over two methods and one over four are different claims.
	MethodsCompared int `json:"methods_compared"`
}

// Probabilities returns the fair probabilities in selection order.
func (f FairValue) Probabilities() []odds.Probability {
	out := make([]odds.Probability, len(f.Selections))
	for i, s := range f.Selections {
		out[i] = s.Probability
	}
	return out
}

// computeFairValue devigs the reference book's quotes.
//
// The fallback is deliberate and narrow: exactly one retry, always to
// multiplicative, and only when the configured method refused the market.
// Multiplicative is the fallback because it is TOTAL — q_i = p_i/S maps a
// positive vector summing above zero onto a positive vector summing to one, with
// no root to find and no way to go negative — so a market that multiplicative
// also refuses is a market whose prices are not a market. That it is the method
// devig.go calls "the worst possible silent default" is the reason it is only
// ever reached explicitly, recorded on the record, and counted.
func computeFairValue(
	s MarketSnapshot,
	ref BookState,
	method odds.DevigMethod,
	attribution odds.Attribution,
	compare bool,
) (FairValue, error) {
	prices, ok := ref.Decimals()
	if !ok {
		return FairValue{}, fmt.Errorf("pricing: market %s: reference book %s quoted %d of %d selections: %w",
			s.Market.ID(), ref.Book.Slug(), len(ref.Quotes), len(s.Selections), ErrIncompleteReference)
	}

	margin, err := odds.NewMargin(prices)
	if err != nil {
		return FairValue{}, fmt.Errorf("pricing: market %s: reference book %s margin: %w",
			s.Market.ID(), ref.Book.Slug(), err)
	}

	implied := make([]odds.Probability, len(ref.Quotes))
	for i, q := range ref.Quotes {
		implied[i] = q.Implied
	}

	fv := FairValue{
		RequestedMethod: method,
		Attribution:     attribution.String(),
		Margin:          marginFrom(margin),
		Disagreement:    -1,
	}

	res, err := odds.Devig(method, implied)
	if err != nil {
		fallback, ferr := odds.Devig(odds.MethodMultiplicative, implied)
		if ferr != nil {
			return FairValue{}, fmt.Errorf(
				"pricing: market %s: reference book %s could not be devigged by %s (%v) "+
					"nor by the multiplicative fallback: %w",
				s.Market.ID(), ref.Book.Slug(), method, err, ferr)
		}
		res = fallback
		fv.Fallback = true
	}

	fv.Method = res.Method
	fv.Parameter = res.Parameter
	fv.Iterations = res.Iterations

	fairDecimals, err := res.Decimals()
	if err != nil {
		return FairValue{}, fmt.Errorf("pricing: market %s: fair prices from %s devig: %w",
			s.Market.ID(), res.Method, err)
	}

	// The attribution view, for the cross-book-comparable per-selection split.
	// It is allowed to fail — uniform attribution is undefined on a long enough
	// shot — without failing the market, because the devig above has already
	// produced the fair values this record is really about.
	contributions, cerr := odds.VigContributions(prices, attribution)

	fv.Selections = make([]FairSelection, len(ref.Quotes))
	for i, q := range ref.Quotes {
		sel := s.Selections[i]
		fair := res.Probabilities[i]
		excess := float64(q.Implied) - float64(fair)

		fs := FairSelection{
			SelectionID:      sel.ID(),
			Role:             sel.Role(),
			Name:             sel.Name(),
			Probability:      fair,
			Decimal:          fairDecimals[i],
			ReferenceDecimal: q.Decimal,
			ReferenceImplied: q.Implied,
			Excess:           excess,
			// fair is strictly inside (0,1) by DevigResult's contract, so this
			// division cannot produce a NaN or an infinity.
			RelativeMargin: excess / float64(fair),
		}
		if cerr == nil {
			// Where the attribution agrees with the devig — proportional against
			// a multiplicative devig — these are the same numbers, and that is a
			// property worth being able to observe rather than a redundancy.
			fs.AttributedExcess = contributions[i].Excess
			fs.AttributedShare = contributions[i].Share
		}
		fv.Selections[i] = fs
	}

	if compare {
		fv.Disagreement, fv.MethodsCompared = methodDisagreement(implied, res)
	}
	return fv, nil
}

// methodDisagreement measures how far the other devig methods land from the one
// that was used, and over how many methods.
//
// A method that refuses the market is skipped rather than counted as agreeing.
// Collapsing a refusal into a zero difference would report perfect agreement on
// precisely the markets where the methods differ most — additive fails when a
// selection is long enough that subtracting an equal slice drives it negative,
// which is the longshot case CLAUDE.md §4 says the methods disagree on.
func methodDisagreement(implied []odds.Probability, used odds.DevigResult) (float64, int) {
	worst := 0.0
	counted := 0
	for _, c := range odds.DevigCompare(implied) {
		if c.Err != nil {
			continue
		}
		counted++
		if c.Method == used.Method {
			continue
		}
		d, err := used.MaxAbsDiff(c.Result)
		if err != nil {
			// Unreachable: both results describe the same market and therefore
			// have the same length. Skipped rather than propagated, because a
			// diagnostic number must never be able to fail a market's price.
			continue
		}
		if d > worst {
			worst = d
		}
	}
	return worst, counted
}
