// Fixtures shared by this package's tests.
//
// Everything below builds a [pricing.ComputedMarket] the way the pricer would
// have: the expected value, edge and Kelly on every quote are computed with
// internal/domain/odds's OWN functions rather than typed in as literals, so a
// fixture cannot assert a relationship the real pipeline does not produce. A
// hand-written 4.2% edge that the arithmetic would never have generated is a
// fixture that tests the test.
//
// These are test fixtures and nothing here is seeded data. CLAUDE.md's "no mock
// data" rule is about surfaces that pretend to show real findings; a _test.go
// file constructing an input to a pure function is the opposite of that, and an
// empty analytics board on a running system remains the correct output.
package analytics

import (
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// fixtureAnchor is the instant every fixture's ages are measured against. It is a
// fixed date rather than time.Now for the reason the synthetic generator gives
// about its own clock: a determinism contract stated over clock readings cannot
// be asserted against the wall clock.
var fixtureAnchor = time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)

// quoteSpec is one book's quote on one selection, stated the way a test wants to
// state it: what the book offers, what the sharp book's fair probability is, and
// how stale the quote is.
type quoteSpec struct {
	selection domain.SelectionID
	book      domain.BookID
	offered   float64 // decimal odds
	fair      float64 // the reference book's fair probability for this selection
	age       time.Duration
	line      domain.Line
	status    pricing.QuoteStatus // zero value means priced
}

// computedMarket assembles a priced market from the specs, scoring each quote
// with internal/domain/odds exactly as internal/pricing would.
//
// disagreement becomes [pricing.FairValue.Disagreement], the cross-method error
// bar the +EV finder's third gate is measured against. A NEGATIVE value means
// "not computed", which is what internal/pricing writes when the method
// comparison is switched off.
func computedMarket(t *testing.T, marketType string, disagreement float64, specs ...quoteSpec) pricing.ComputedMarket {
	t.Helper()

	byBook := map[domain.BookID]*pricing.BookAssessment{}
	var order []domain.BookID

	for _, sp := range specs {
		b := byBook[sp.book]
		if b == nil {
			b = &pricing.BookAssessment{
				BookID:   sp.book,
				Slug:     domain.Slug(sp.book),
				Name:     string(sp.book),
				Kind:     domain.BookKindSynthetic,
				Eligible: true,
				Complete: true,
			}
			byBook[sp.book] = b
			order = append(order, sp.book)
		}
		b.Quotes = append(b.Quotes, scoreQuote(t, sp))
	}

	rec := pricing.ComputedMarket{
		SchemaVersion: pricing.SchemaVersion,
		Provider:      "synthetic",
		Market:        normalizer.MarketRef{ID: "mkt-1", EventID: "evt-1", Type: marketType},
		League:        normalizer.LeagueRef{ID: "lg-1"},
		Reference: pricing.ReferenceRef{
			BookID: "sharp",
			Slug:   "sharp",
			Kind:   domain.BookKindSynthetic,
			// A record whose reference has no stated provenance fails
			// ComputedMarket.Validate, so the fixture states one rather than
			// leaving the zero value the type deliberately makes invalid.
			Source:     pricing.ReferenceSourceCatalogue,
			ObservedAt: fixtureAnchor,
		},
		Fair: pricing.FairValue{
			Method:          odds.MethodShin,
			RequestedMethod: odds.MethodShin,
			Attribution:     odds.AttributionProportional.String(),
			Disagreement:    disagreement,
			Selections:      fairSelections(t, specs...),
		},
		ObservedAt: fixtureAnchor,
	}
	for _, id := range order {
		rec.Books = append(rec.Books, *byBook[id])
	}
	return rec
}

// scoreQuote computes one quote's assessment with the real odds functions.
func scoreQuote(t *testing.T, sp quoteSpec) pricing.QuoteAssessment {
	t.Helper()

	d, err := odds.NewDecimal(sp.offered)
	if err != nil {
		t.Fatalf("NewDecimal(%v): %v", sp.offered, err)
	}
	implied, err := d.Probability()
	if err != nil {
		t.Fatalf("Probability(%v): %v", d, err)
	}
	q, err := odds.NewProbability(sp.fair)
	if err != nil {
		t.Fatalf("NewProbability(%v): %v", sp.fair, err)
	}
	fairDecimal, err := q.Decimal()
	if err != nil {
		t.Fatalf("Decimal(%v): %v", q, err)
	}

	status := sp.status
	if status == "" {
		status = pricing.QuoteStatusPriced
	}

	out := pricing.QuoteAssessment{
		SelectionID:     sp.selection,
		Status:          status,
		Decimal:         d,
		Implied:         implied,
		Line:            sp.line,
		ObservedAt:      fixtureAnchor.Add(-sp.age),
		AgeSeconds:      sp.age.Seconds(),
		FairProbability: q,
		FairDecimal:     fairDecimal,
	}
	if status != pricing.QuoteStatusPriced {
		return out
	}

	must := func(v float64, err error) float64 {
		t.Helper()
		if err != nil {
			t.Fatalf("scoring quote %s/%s: %v", sp.selection, sp.book, err)
		}
		return v
	}
	out.ExpectedValue = must(odds.ExpectedValue(q, d))
	out.ExpectedValuePercent = must(odds.ExpectedValuePercent(q, d))
	out.Edge = must(odds.Edge(q, implied))
	out.EdgePercent = must(odds.EdgePercent(q, implied))
	out.Kelly = must(odds.Kelly(q, d))
	out.FractionalKelly = must(odds.FractionalKelly(q, d, pricing.DefaultKellyMultiplier))
	return out
}

// arbLeg is one leg of a fixture arbitrage.
type arbLeg struct {
	selection domain.SelectionID
	role      string
	book      domain.BookID
	decimal   float64
	stake     float64
	age       time.Duration
}

// arbitrageRef assembles a [pricing.ArbitrageRef] the way internal/pricing's
// scanner would, deriving the margin and the return from the leg prices rather
// than stating them, so a fixture cannot claim a return its own legs do not
// imply.
func arbitrageRef(t *testing.T, oldest, spread time.Duration, legs ...arbLeg) pricing.ArbitrageRef {
	t.Helper()

	sum := 0.0
	out := make([]pricing.ArbitrageLegRef, 0, len(legs))
	for _, l := range legs {
		sum += 1 / l.decimal
		out = append(out, pricing.ArbitrageLegRef{
			SelectionID:   l.selection,
			Role:          l.role,
			BookID:        l.book,
			Decimal:       l.decimal,
			StakeFraction: l.stake,
			ObservedAt:    fixtureAnchor.Add(-l.age),
			AgeSeconds:    l.age.Seconds(),
		})
	}
	books := map[domain.BookID]struct{}{}
	for _, l := range legs {
		books[l.book] = struct{}{}
	}

	return pricing.ArbitrageRef{
		Legs: out,
		Margin: pricing.Margin{
			Selections: len(legs),
			ImpliedSum: sum,
			Overround:  sum - 1,
		},
		Return:                (1 - sum) / sum,
		DistinctBooks:         len(books),
		ObservedSpreadSeconds: spread.Seconds(),
		ObservedAt:            fixtureAnchor.Add(-oldest),
		OldestLegAgeSeconds:   oldest.Seconds(),
	}
}

// fairSelections builds the market's fair-value list from the specs.
//
// [pricing.ComputedMarket.Validate] refuses a record with fewer than
// [odds.MinMarketSelections] fair selections, so a fixture that omitted them
// would be refused by the service before any detector saw it — which is correct
// behaviour and would make every service-level test assert nothing.
//
// One entry per DISTINCT selection, in first-seen order, taking the fair
// probability from the first spec that mentions it. Every spec for one selection
// carries the same fair probability by construction: the fair value is the
// reference book's, and there is one reference book.
func fairSelections(t *testing.T, specs ...quoteSpec) []pricing.FairSelection {
	t.Helper()

	seen := map[domain.SelectionID]bool{}
	var out []pricing.FairSelection
	for _, sp := range specs {
		if seen[sp.selection] {
			continue
		}
		seen[sp.selection] = true

		q, err := odds.NewProbability(sp.fair)
		if err != nil {
			t.Fatalf("NewProbability(%v): %v", sp.fair, err)
		}
		d, err := q.Decimal()
		if err != nil {
			t.Fatalf("Decimal(%v): %v", q, err)
		}
		// The role is positional. domain.SelectionRole's zero value is invalid and
		// refuses to marshal, so a fixture that left it unset would fail to
		// serialise rather than fail an assertion — which is the type doing its
		// job, and is why the fixture states one.
		role := domain.SelectionRoleOutright
		switch len(out) {
		case 0:
			role = domain.SelectionRoleHome
		case 1:
			role = domain.SelectionRoleAway
		}
		out = append(out, pricing.FairSelection{
			SelectionID: sp.selection,
			Role:        role,
			Name:        string(sp.selection),
			Probability: q,
			Decimal:     d,
		})
	}
	return out
}
