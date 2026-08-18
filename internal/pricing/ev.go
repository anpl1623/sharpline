// Scoring every book against the reference book's fair value.
//
// # A price is only comparable at the same line
//
// This is the single most important rule in this file and it is not a detail.
// Phase 1 built the same rule into odds.CLV, which REFUSES to compare a wager's
// placement price against a closing price when the line moved between them,
// because "home −3.5" and "home −3.0" are answers to different questions and a
// number computed across them is not an edge, it is a category error.
//
// The same thing is true here, one step earlier. Fair value is derived from the
// reference book's market at the reference book's line. Scoring a challenger's
// −3.0 quote against it prices a bet nobody can place. So a quote whose line
// differs from the reference's line on the same selection gets NO expected
// value, NO edge and NO Kelly fraction — it is marked [QuoteStatusLineMismatch]
// and carried, because a line disagreement is real market information and is
// precisely the input the middles detector wants. It is simply not an +EV
// signal, and reporting it as one would manufacture edges out of book
// disagreement about the line rather than about the price.
//
// # A stale price is not actionable either
//
// A book whose oldest quote on this market is older than MaxQuoteAge is
// disqualified. Its prices are still carried — the board should be able to show
// the last line a book was seen at — but its expected values are suppressed,
// because an EV against a price that is no longer offered is a number that reads
// as an opportunity and is not one. Ages are measured from the record's own
// anchor, never from the wall clock; state.go explains why.
//
// # The reference book scores itself, on purpose
//
// It is not excluded from the assessment. Its EV against its own devigged fair
// value is a self-check with a known sign: a book cannot have a positive edge
// against the fair probabilities extracted from its own prices, so every
// reference quote must come out at EV ≤ 0, and at exactly 0 only on a fair
// market. That invariant is asserted in the tests and it is the cheapest
// available detector of a devig that has gone wrong.
package pricing

import (
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// QuoteStatus says whether one book's quote on one selection was scored, and if
// not, why not. It is a bounded set because it becomes a Prometheus label value.
type QuoteStatus string

// The statuses. Every value is written by exactly one branch of assessQuote.
const (
	// QuoteStatusPriced means the quote was scored against fair value.
	QuoteStatusPriced QuoteStatus = "priced"

	// QuoteStatusStale means the quote's book was disqualified by age.
	QuoteStatusStale QuoteStatus = "stale"

	// QuoteStatusLineMismatch means the book quoted a different line from the
	// reference book on this selection, so there is no comparable fair value.
	// See the file comment.
	QuoteStatusLineMismatch QuoteStatus = "line_mismatch"

	// QuoteStatusNoFairValue means the market carries no fair value for this
	// selection. Reachable only when the reference book is incomplete, which
	// resolveReference already refuses, so it exists to make that impossible
	// rather than merely improbable.
	QuoteStatusNoFairValue QuoteStatus = "no_fair_value"

	// QuoteStatusUnpriceable means the arithmetic itself refused the quote — a
	// price at the edge of the representable range, an overflow scaling a
	// percentage. Counted, never silently dropped.
	QuoteStatusUnpriceable QuoteStatus = "unpriceable"
)

// QuoteAssessment is one book's quote on one selection, scored.
type QuoteAssessment struct {
	SelectionID domain.SelectionID `json:"selection_id"`

	// Status says whether the value fields below are populated.
	Status QuoteStatus `json:"status"`

	// Decimal is the book's quoted price and Implied is the probability it
	// implies, margin included.
	Decimal odds.Decimal     `json:"decimal"`
	Implied odds.Probability `json:"implied"`

	// Line is the line THIS book quoted on this selection.
	Line domain.Line `json:"line"`

	// ObservedAt is the provider's observation instant for this quote, and
	// AgeSeconds is its age at the record's anchor.
	//
	// AgeSeconds MAY BE NEGATIVE, when the provider's clock ran ahead of ours.
	// It is reported rather than clamped for the reason domain.Price.Age gives —
	// "a monitor can detect the skew instead of silently reporting healthy
	// staleness". The clamp belongs in the histogram, where a negative sample
	// would land in the lowest bucket and read as excellent, and the service
	// counts it there against sharpline_odds_clock_skew_total.
	ObservedAt time.Time `json:"observed_at"`
	AgeSeconds float64   `json:"age_seconds"`

	// FairProbability and FairDecimal are the reference book's fair value for
	// this selection, restated on the quote so a consumer never has to join the
	// two halves of the record to explain a number.
	FairProbability odds.Probability `json:"fair_probability"`
	FairDecimal     odds.Decimal     `json:"fair_decimal"`

	// ExpectedValue is the expected profit per unit staked at this price given
	// the fair probability: q·d − 1. ExpectedValuePercent is the same number as
	// a percentage. Both spellings are carried because phase 1 refuses to pick
	// one, and the ambiguity between them is a routine factor-of-100 error.
	ExpectedValue        float64 `json:"expected_value"`
	ExpectedValuePercent float64 `json:"expected_value_percent"`

	// Edge is q/p − 1, the proportional advantage of the fair probability over
	// this book's implied probability, and EdgePercent is it as a percentage.
	//
	// Edge and ExpectedValue are the same quantity here — p is exactly this
	// price's implied probability, and odds.Edge's doc proves q/p − 1 = q·d − 1
	// under that substitution. They are both carried anyway because they are
	// computed by different routes (a division against a multiplication) and can
	// differ in the last unit in the last place, and a consumer comparing them
	// is comparing this package's arithmetic against itself.
	Edge        float64 `json:"edge"`
	EdgePercent float64 `json:"edge_percent"`

	// Kelly is the full Kelly stake as a FRACTION OF BANKROLL, and
	// FractionalKelly is it scaled by the configured multiplier. Neither is a
	// money amount: the Money-denominated staking helper lives in
	// internal/domain, and that split is what keeps the import direction
	// odds → domain and avoids a cycle. Both are exactly zero at zero edge.
	Kelly           float64 `json:"kelly"`
	FractionalKelly float64 `json:"fractional_kelly"`

	// AttributedExcess and AttributedShare are this selection's share of THIS
	// BOOK's own margin, under the record's stated Attribution. They describe
	// the book's pricing, not its edge, and are populated for every book whose
	// market is complete — including books whose quote could not be scored.
	AttributedExcess float64 `json:"attributed_excess"`
	AttributedShare  float64 `json:"attributed_share"`
}

// BookAssessment is one book's whole market, scored.
type BookAssessment struct {
	BookID domain.BookID   `json:"book_id"`
	Slug   domain.Slug     `json:"slug"`
	Name   string          `json:"name"`
	Kind   domain.BookKind `json:"kind"`

	// Reference reports that this is the book the fair value was derived from.
	Reference bool `json:"reference"`

	// Complete reports that the book quoted every selection. Margin is
	// populated only when it did; see fairvalue.go on why a partial market has
	// no margin to speak of.
	Complete bool `json:"complete"`

	// Eligible reports that the book passed the staleness policy. An ineligible
	// book's quotes carry prices and lines but no expected values.
	Eligible bool `json:"eligible"`

	// Margin is this book's own margin triple over this market: booking
	// percentage, overround and vig, kept as the three distinct quantities phase
	// 1 insists they are. Zero-valued when Complete is false — check Complete,
	// not the fields.
	Margin Margin `json:"margin"`

	// OldestObservedAt and NewestObservedAt bracket the book's quotes, and
	// AgeSeconds is the oldest quote's age at the record's anchor: the number
	// the staleness policy judges. It may be negative under provider clock skew;
	// see QuoteAssessment.AgeSeconds.
	OldestObservedAt time.Time `json:"oldest_observed_at"`
	NewestObservedAt time.Time `json:"newest_observed_at"`
	AgeSeconds       float64   `json:"age_seconds"`

	// Quotes are the book's scored quotes, in the market's selection order.
	Quotes []QuoteAssessment `json:"quotes"`
}

// BestEdge returns the largest expected value across this book's priced quotes,
// and whether there was one. It is the number the +EV finder sorts on, computed
// here so that phase 9 and the API cannot each derive it slightly differently.
func (b BookAssessment) BestEdge() (QuoteAssessment, bool) {
	var best QuoteAssessment
	found := false
	for _, q := range b.Quotes {
		if q.Status != QuoteStatusPriced {
			continue
		}
		if !found || q.ExpectedValue > best.ExpectedValue {
			best, found = q, true
		}
	}
	return best, found
}

// assessBook scores one book's market against the fair value.
//
// fairBy indexes the fair values by selection, and refLine gives the reference
// book's line on each selection — the two things a scored quote has to be
// checked against. Passing them prepared rather than searching per quote keeps
// this linear in the number of quotes rather than quadratic, which matters on a
// futures market with a forty-runner field.
func assessBook(
	s MarketSnapshot,
	b BookState,
	ref BookState,
	fv FairValue,
	fairBy map[domain.SelectionID]FairSelection,
	kellyMultiplier float64,
	attribution odds.Attribution,
	maxAge time.Duration,
) BookAssessment {
	anchor := s.Anchor()
	age := b.Age(anchor)

	out := BookAssessment{
		BookID:           b.Book.ID(),
		Slug:             b.Book.Slug(),
		Name:             b.Book.Name(),
		Kind:             b.Book.Kind(),
		Reference:        b.Book.ID() == ref.Book.ID(),
		Complete:         b.Complete,
		Eligible:         age <= maxAge,
		OldestObservedAt: b.OldestAt,
		NewestObservedAt: b.NewestAt,
		AgeSeconds:       age.Seconds(),
		Quotes:           make([]QuoteAssessment, 0, len(b.Quotes)),
	}

	// The margin and the per-selection attribution are properties of the book's
	// own prices, so they are computed even for an ineligible book: "this book's
	// last known market held 6.5%" is a true and useful statement about a stale
	// board, where "this book offers 4% EV" is not.
	var contributions []odds.SelectionVig
	if prices, ok := b.Decimals(); ok {
		if m, err := odds.NewMargin(prices); err == nil {
			out.Margin = marginFrom(m)
		}
		if c, err := odds.VigContributions(prices, attribution); err == nil {
			contributions = c
		}
	}

	for i, q := range b.Quotes {
		qa := assessQuote(q, anchor, fairBy, ref, out.Eligible, kellyMultiplier)
		if contributions != nil {
			qa.AttributedExcess = contributions[i].Excess
			qa.AttributedShare = contributions[i].Share
		}
		out.Quotes = append(out.Quotes, qa)
	}
	return out
}

// assessQuote scores one quote. It never returns an error: a quote that cannot
// be scored is carried with a status saying why, because dropping it would
// change the book's apparent market without anything reporting it.
func assessQuote(
	q BookQuote,
	anchor time.Time,
	fairBy map[domain.SelectionID]FairSelection,
	ref BookState,
	eligible bool,
	kellyMultiplier float64,
) QuoteAssessment {
	out := QuoteAssessment{
		SelectionID: q.SelectionID,
		Decimal:     q.Decimal,
		Implied:     q.Implied,
		Line:        q.Line,
		ObservedAt:  q.ObservedAt,
		AgeSeconds:  anchor.Sub(q.ObservedAt).Seconds(),
	}

	fair, ok := fairBy[q.SelectionID]
	if !ok {
		out.Status = QuoteStatusNoFairValue
		return out
	}
	out.FairProbability = fair.Probability
	out.FairDecimal = fair.Decimal

	if !eligible {
		out.Status = QuoteStatusStale
		return out
	}

	// The reference book's line on THIS selection, not the market's consensus
	// line. The consensus is a mode across books (normalizer/mapper.go) and the
	// reference book may not be quoting at it; the fair value came from the
	// reference book's own market, so the reference book's own line is what a
	// challenger has to match.
	refQuote, ok := ref.Quote(q.SelectionID)
	if !ok {
		out.Status = QuoteStatusNoFairValue
		return out
	}
	if !q.Line.Equal(refQuote.Line) {
		out.Status = QuoteStatusLineMismatch
		return out
	}

	ev, err := odds.ExpectedValue(fair.Probability, q.Decimal)
	if err != nil {
		out.Status = QuoteStatusUnpriceable
		return out
	}
	evPct, err := odds.ExpectedValuePercent(fair.Probability, q.Decimal)
	if err != nil {
		out.Status = QuoteStatusUnpriceable
		return out
	}
	edge, err := odds.Edge(fair.Probability, q.Implied)
	if err != nil {
		out.Status = QuoteStatusUnpriceable
		return out
	}
	edgePct, err := odds.EdgePercent(fair.Probability, q.Implied)
	if err != nil {
		out.Status = QuoteStatusUnpriceable
		return out
	}
	kelly, err := odds.Kelly(fair.Probability, q.Decimal)
	if err != nil {
		out.Status = QuoteStatusUnpriceable
		return out
	}
	// FractionalKelly rejects a multiplier outside (0, 1]; Options.validate has
	// already established that it is inside, so this cannot fail for a quote
	// whose full Kelly succeeded. Checked rather than assumed, because the two
	// validations live in different files.
	fractional, err := odds.FractionalKelly(fair.Probability, q.Decimal, kellyMultiplier)
	if err != nil {
		out.Status = QuoteStatusUnpriceable
		return out
	}

	out.Status = QuoteStatusPriced
	out.ExpectedValue = ev
	out.ExpectedValuePercent = evPct
	out.Edge = edge
	out.EdgePercent = edgePct
	out.Kelly = kelly
	out.FractionalKelly = fractional
	return out
}

// fairIndex indexes a market's fair values by selection.
func fairIndex(fv FairValue) map[domain.SelectionID]FairSelection {
	out := make(map[domain.SelectionID]FairSelection, len(fv.Selections))
	for _, f := range fv.Selections {
		out[f.SelectionID] = f
	}
	return out
}

// assertQuoteCount is a construction-time invariant check used by the tests and
// by assessBook's caller: a complete book must produce one assessment per
// selection. It is a function rather than an inline comparison so the message is
// written once.
func assertQuoteCount(b BookAssessment, selections int) error {
	if b.Complete && len(b.Quotes) != selections {
		return fmt.Errorf("pricing: book %s is complete but produced %d assessments for %d selections",
			b.Slug, len(b.Quotes), selections)
	}
	return nil
}
