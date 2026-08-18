package pricing

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// TestOptionsFailAtConstructionNotAtTheFirstRecord. CLAUDE.md §12: "Config via
// environment variables with a typed struct and startup validation — fail fast
// and loudly on a bad config." A pricer that started with a nonsense Kelly
// multiplier and only discovered it on the first market would fail in a handler,
// where the failure looks like a data problem.
func TestOptionsFailAtConstructionNotAtTheFirstRecord(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		o    Options
		want string
	}{
		{"undefined devig method", Options{DevigMethod: odds.DevigMethod(99)}, "devig method"},
		{"undefined attribution", Options{Attribution: odds.Attribution(99)}, "attribution"},
		{"kelly multiplier above one", Options{KellyMultiplier: 1.5}, "kelly multiplier"},
		{"negative kelly multiplier", Options{KellyMultiplier: -0.25}, "kelly multiplier"},
		{"negative reference age", Options{MaxReferenceAge: -time.Second}, "MaxReferenceAge"},
		{"negative quote age", Options{MaxQuoteAge: -time.Second}, "MaxQuoteAge"},
		{"unusable book slug", Options{ReferenceBooks: []domain.Slug{"Not A Slug"}}, "ReferenceBooks[0]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewEngine(tc.o)
			if !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("got %v, want ErrInvalidOptions", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending field %q", err, tc.want)
			}
		})
	}
}

// TestZeroOptionsResolveToTheDocumentedDefaults. Each default is argued in
// engine.go with its alternatives; this pins that the argument and the code
// agree, which a doc comment alone cannot.
func TestZeroOptionsResolveToTheDocumentedDefaults(t *testing.T) {
	t.Parallel()

	e := mustEngine(t, Options{})
	if e.DevigMethod() != DefaultDevigMethod {
		t.Errorf("devig method %s, want %s", e.DevigMethod(), DefaultDevigMethod)
	}
	if e.attribution != DefaultAttribution {
		t.Errorf("attribution %s, want %s", e.attribution, DefaultAttribution)
	}
	if e.kelly != DefaultKellyMultiplier {
		t.Errorf("kelly multiplier %g, want %g", e.kelly, DefaultKellyMultiplier)
	}
	if e.maxRefAge != DefaultMaxReferenceAge {
		t.Errorf("MaxReferenceAge %s, want %s", e.maxRefAge, DefaultMaxReferenceAge)
	}
	if e.maxQuoteAge != DefaultMaxQuoteAge {
		t.Errorf("MaxQuoteAge %s, want %s", e.maxQuoteAge, DefaultMaxQuoteAge)
	}
	if !e.compare {
		t.Error("method comparison is off by default; the zero value must leave it on")
	}

	// The staleness policy is deliberately asymmetric: the reference is the one
	// input the whole fair value rests on, so it gets the tighter bound.
	if DefaultMaxReferenceAge >= DefaultMaxQuoteAge {
		t.Errorf("reference bound %s is not tighter than the challenger bound %s",
			DefaultMaxReferenceAge, DefaultMaxQuoteAge)
	}
	// Both are sized against the normalizer's suppression ceiling rather than
	// against a number that sounds fast; a bound below it would disqualify a
	// market that is working normally and simply has not moved.
	if DefaultMaxReferenceAge < 5*time.Minute {
		t.Errorf("MaxReferenceAge %s is below the normalizer's 5-minute refresh ceiling, so a "+
			"market that has merely not moved would be treated as stale", DefaultMaxReferenceAge)
	}
}

// TestEngineDoesNotShareItsPreferenceListWithCallers. CLAUDE.md §12 forbids
// global mutable state, and an accessor handing back the engine's own slice lets
// a caller reorder every future resolution.
func TestEngineDoesNotShareItsPreferenceListWithCallers(t *testing.T) {
	t.Parallel()

	prefer := []domain.Slug{"first", "second"}
	e := mustEngine(t, Options{ReferenceBooks: prefer})

	// Mutating the caller's original slice must not reach the engine.
	prefer[0] = "hijacked"
	if got := e.ReferenceBooks(); got[0] != "first" {
		t.Errorf("engine adopted a mutation of the caller's slice: %v", got)
	}

	// Mutating what the accessor returns must not reach it either.
	returned := e.ReferenceBooks()
	returned[0] = "hijacked"
	if got := e.ReferenceBooks(); got[0] != "first" {
		t.Errorf("engine handed out its own slice: %v", got)
	}
}

// TestEngineMetricsRegisterOnceAndFailLoudlyOnAConflict.
//
// One process may legitimately build more than one Engine, and duplicate
// registration must fail its startup rather than its code review — the reason
// every other package in this repository injects a registry instead of using the
// default one.
func TestEngineMetricsRegisterOnceAndFailLoudlyOnAConflict(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	if _, err := NewEngine(Options{Registry: reg}); err != nil {
		t.Fatalf("first engine: %v", err)
	}
	if _, err := NewEngine(Options{Registry: reg}); err == nil {
		t.Fatal("a second engine registered its collectors on the same registry without complaint")
	}

	// A nil registry builds the collectors unregistered, so a unit test and any
	// process with no /metrics endpoint pay nothing and need no nil checks.
	if _, err := NewEngine(Options{}); err != nil {
		t.Fatalf("nil registry: %v", err)
	}
}

// TestEngineMetricFamiliesCarryTheExpectedNames.
//
// The names are not the frozen contract series — metrics.go owns
// sharpline_pricing_duration_seconds — but they ARE what a dashboard panel or an
// alert added later would be written against, and a rename that slipped through
// would break such a panel silently, because Prometheus answers a query for a
// series that does not exist with no data rather than with an error.
func TestEngineMetricFamiliesCarryTheExpectedNames(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	engine := mustEngine(t, Options{
		Registry:       reg,
		ReferenceBooks: []domain.Slug{"sharpline"},
	})

	implied := powerMargin(t, []float64{0.42, 0.33, 0.25}, 0.02)
	rec := marketFixture{
		id:         "mkt-metrics",
		selections: threeWayRoles,
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}.build(t)
	if _, err := engine.Price(t.Context(), rec); err != nil {
		t.Fatalf("Price: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}

	for _, name := range []string{
		"sharpline_pricer_fair_value_total",
		"sharpline_pricer_reference_book_total",
		"sharpline_pricer_books_total",
		"sharpline_pricer_quotes_total",
		"sharpline_pricer_devig_disagreement",
		"sharpline_pricer_reference_overround",
		"sharpline_pricer_quote_age_seconds",
	} {
		if !seen[name] {
			t.Errorf("metric family %s is absent after pricing a market", name)
		}
	}

	// The subsystem is `pricer` and not `pricing` precisely so nothing here can
	// collide with the frozen contract series the dashboard reads.
	if seen["sharpline_pricing_duration_seconds"] {
		t.Error("the engine emitted the service's contract series; two collectors observing one " +
			"histogram from inside and outside the same call would double its count")
	}
}

// TestFairValueAndSnapshotAccessors covers the small read paths the arbitrage
// and middles scanners address a market through.
func TestFairValueAndSnapshotAccessors(t *testing.T) {
	t.Parallel()

	implied := powerMargin(t, []float64{0.42, 0.33, 0.25}, 0.02)
	rec := marketFixture{
		id:         "mkt-accessors",
		selections: threeWayRoles,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(implied)},
			{slug: "lowtide", prices: decimalsOf(powerMargin(t, []float64{0.42, 0.33, 0.25}, 0.065))},
		},
	}.build(t)

	snap, err := NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	if _, ok := snap.Book("book-lowtide"); !ok {
		t.Error("Book could not find a book the snapshot carries")
	}
	if _, ok := snap.Book("book-absent"); ok {
		t.Error("Book found a book the snapshot does not carry")
	}
	if !snap.Priceable() {
		t.Error("a three-selection market is not priceable")
	}

	out, err := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"sharpline"}}).
		Price(t.Context(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}

	ps := out.Fair.Probabilities()
	if len(ps) != len(out.Fair.Selections) {
		t.Fatalf("Probabilities returned %d entries for %d selections",
			len(ps), len(out.Fair.Selections))
	}
	sum := 0.0
	for i, p := range ps {
		if p != out.Fair.Selections[i].Probability {
			t.Errorf("Probabilities[%d] disagrees with Selections[%d]", i, i)
		}
		sum += float64(p)
	}
	if !approxRel(sum, 1, 1e-9) {
		t.Errorf("probabilities sum to %.17g, want 1", sum)
	}
}

// TestMarketWithOneSelectionIsNotPriceable. odds.MinMarketSelections is two: a
// one-sided market has no margin, because the margin is the excess of Σ 1/d
// over 1 and one price cannot exceed certainty by a meaningful amount.
func TestMarketWithOneSelectionIsNotPriceable(t *testing.T) {
	t.Parallel()

	rec := marketFixture{
		id:         "mkt-single",
		selections: []domain.SelectionRole{domain.SelectionRoleHome},
		books:      []bookFixture{{slug: "sharpline", prices: []float64{1.9}}},
	}.build(t)

	_, err := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"sharpline"}}).
		Price(t.Context(), rec)
	if !errors.Is(err, ErrMarketNotPriceable) {
		t.Fatalf("got %v, want ErrMarketNotPriceable", err)
	}
}
