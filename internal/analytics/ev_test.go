package analytics

import (
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// TestRankingIsDeterministicUnderPermutation is a phase-9 gate item, and the
// reason it is one is worth stating: AN UNSTABLE SORT IS A DIFFERENT ANSWER IN
// SQL.
//
// Phase 12 rewrites this finder as a Flink SQL job and compares the two outputs.
// SQL has no notion of "the order the rows happened to be in", so a comparator
// that left ties to the input order would produce a Go answer that changed
// between runs and a SQL answer that could never match it — and the diff would
// look like a bug in the SQL rather than an under-specified contract in the Go.
//
// The comparator's tuple is therefore total: (ExpectedValuePercent,
// QuoteObservedAt, SelectionID, BookID), all descending, which is
// ev_signals_rank_idx's exact column list. The fixture below deliberately
// contains two quotes with an IDENTICAL expected value and identical instants, so
// the tie-break on selection and book is exercised rather than merely present.
func TestRankingIsDeterministicUnderPermutation(t *testing.T) {
	// Two books offering the same price on the same fair value produce the same
	// expected value to the last bit, which is exactly the tie the comparator has
	// to break.
	base := []quoteSpec{
		{selection: "sel-home", book: "book-a", offered: 2.20, fair: 0.50},
		{selection: "sel-home", book: "book-b", offered: 2.20, fair: 0.50},
		{selection: "sel-away", book: "book-a", offered: 2.20, fair: 0.50},
		{selection: "sel-away", book: "book-c", offered: 2.60, fair: 0.45},
		{selection: "sel-home", book: "book-c", offered: 2.05, fair: 0.52},
	}

	finder, err := NewEVFinder(EVConfig{})
	if err != nil {
		t.Fatalf("NewEVFinder: %v", err)
	}

	want, _ := finder.Scan(computedMarket(t, "moneyline", 0, base...), fixtureAnchor)
	if len(want) < 4 {
		t.Fatalf("the fixture produced %d findings; it is meant to produce several so the "+
			"tie-breaks are exercised", len(want))
	}

	permutations := [][]int{
		{4, 3, 2, 1, 0},
		{2, 0, 4, 1, 3},
		{1, 4, 0, 3, 2},
		{3, 2, 1, 0, 4},
	}
	for _, perm := range permutations {
		shuffled := make([]quoteSpec, len(base))
		for i, j := range perm {
			shuffled[i] = base[j]
		}
		got, _ := finder.Scan(computedMarket(t, "moneyline", 0, shuffled...), fixtureAnchor)

		if len(got) != len(want) {
			t.Fatalf("permutation %v changed the finding count: %d vs %d", perm, len(got), len(want))
		}
		for i := range want {
			if got[i].SelectionID != want[i].SelectionID || got[i].BookID != want[i].BookID {
				t.Fatalf("permutation %v changed the ranking at position %d: got %s/%s, want %s/%s\n"+
					"the comparator's tuple must be TOTAL, or the top N this package emits is not "+
					"the top N the API pages through",
					perm, i, got[i].SelectionID, got[i].BookID, want[i].SelectionID, want[i].BookID)
			}
			if got[i].ExpectedValuePercent != want[i].ExpectedValuePercent {
				t.Fatalf("permutation %v changed a value at position %d", perm, i)
			}
		}
	}

	// The ranking must also be MONOTONE, or "best first" is a claim rather than a
	// property.
	for i := 1; i < len(want); i++ {
		if want[i].ExpectedValuePercent > want[i-1].ExpectedValuePercent {
			t.Fatalf("findings are not in descending expected-value order at position %d", i)
		}
	}
}

// TestGatesRefuseForTheStatedReason walks every branch of the finder's decision,
// because a threshold nobody can see firing is a threshold nobody can tune — and
// because the reason strings become Prometheus label values that the dashboard
// reads.
func TestGatesRefuseForTheStatedReason(t *testing.T) {
	tests := []struct {
		name         string
		cfg          EVConfig
		disagreement float64
		spec         quoteSpec
		want         EVReason
	}{
		{
			name:         "a scored quote well above the floor is a signal",
			disagreement: 0,
			spec:         quoteSpec{selection: "s", book: "b", offered: 2.40, fair: 0.50},
			want:         EVReasonSignal,
		},
		{
			name:         "internal/pricing's own refusal is never second-guessed",
			disagreement: 0,
			spec: quoteSpec{
				selection: "s", book: "b", offered: 2.40, fair: 0.50,
				status: pricing.QuoteStatusLineMismatch,
			},
			want: EVReasonNotPriced,
		},
		{
			name:         "a fair price has no edge",
			disagreement: 0,
			spec:         quoteSpec{selection: "s", book: "b", offered: 2.00, fair: 0.50},
			want:         EVReasonNotPositive,
		},
		{
			name:         "a positive but tiny edge is inside the devig's own noise",
			disagreement: 0,
			spec:         quoteSpec{selection: "s", book: "b", offered: 2.005, fair: 0.50},
			want:         EVReasonBelowThreshold,
		},
		{
			name:         "a quote nobody is still offering is not an opportunity",
			disagreement: 0,
			spec: quoteSpec{
				selection: "s", book: "b", offered: 2.40, fair: 0.50,
				age: DefaultMaxEVQuoteAge + time.Second,
			},
			want: EVReasonStale,
		},
		{
			name: "an edge smaller than the fair value's own error bar is not a signal",
			// 20% EV at d = 2.40 needs an error bar under 0.083 probability points
			// to survive; 0.10 is comfortably larger, so the gate binds.
			disagreement: 0.10,
			spec:         quoteSpec{selection: "s", book: "b", offered: 2.40, fair: 0.50},
			want:         EVReasonInsideErrorBar,
		},
		{
			name:         "an unmeasured error bar does not bind",
			disagreement: -1,
			spec:         quoteSpec{selection: "s", book: "b", offered: 2.40, fair: 0.50},
			want:         EVReasonSignal,
		},
		{
			name:         "a disabled error-bar gate does not bind",
			cfg:          EVConfig{DisableErrorBarGate: true},
			disagreement: 0.10,
			spec:         quoteSpec{selection: "s", book: "b", offered: 2.40, fair: 0.50},
			want:         EVReasonSignal,
		},
		{
			name:         "an edge at or above 100% is refused before it reaches a constraint",
			disagreement: 0,
			// q/p − 1 = 0.90/0.4 − 1 = 1.25, past the (0, 1) the column admits.
			spec: quoteSpec{selection: "s", book: "b", offered: 2.50, fair: 0.90},
			want: EVReasonOutOfRange,
		},
		{
			name:         "a moneyline carrying a line contradicts its market type",
			disagreement: 0,
			spec: quoteSpec{
				selection: "s", book: "b", offered: 2.40, fair: 0.50,
				line: mustLine(t, -3.5),
			},
			want: EVReasonOutOfRange,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			finder, err := NewEVFinder(tc.cfg)
			if err != nil {
				t.Fatalf("NewEVFinder: %v", err)
			}
			_, stats := finder.Scan(computedMarket(t, "moneyline", tc.disagreement, tc.spec), fixtureAnchor)
			if stats.Examined != 1 {
				t.Fatalf("examined = %d, want 1", stats.Examined)
			}
			if got := stats.Reasons[tc.want]; got != 1 {
				t.Fatalf("reason %q counted %d times, want 1; reasons were %v",
					tc.want, got, stats.Reasons)
			}
		})
	}
}

// TestEveryFindingCarriesTheBoundsItWasProducedUnder asserts the property that
// makes a stored population interpretable across a re-tuning.
//
// migrations/00009 CHECKs expected_value_percent >= threshold_ev_percent, so a
// finding that did not carry its own threshold would either be unwritable or
// would be written against a threshold somebody else's configuration produced.
func TestEveryFindingCarriesTheBoundsItWasProducedUnder(t *testing.T) {
	cfg := EVConfig{MinEVPercent: 2.5, MaxQuoteAge: 45 * time.Second, KellyFraction: 0.5}
	finder, err := NewEVFinder(cfg)
	if err != nil {
		t.Fatalf("NewEVFinder: %v", err)
	}

	out, _ := finder.Scan(computedMarket(t, "moneyline", 0,
		quoteSpec{selection: "s", book: "b", offered: 2.40, fair: 0.50, age: 10 * time.Second},
	), fixtureAnchor)
	if len(out) != 1 {
		t.Fatalf("got %d findings, want 1", len(out))
	}
	sig := out[0]

	switch {
	case sig.ThresholdEVPercent != 2.5:
		t.Fatalf("threshold = %v, want 2.5", sig.ThresholdEVPercent)
	case sig.MaxQuoteAgeSeconds != 45:
		t.Fatalf("max quote age = %v, want 45", sig.MaxQuoteAgeSeconds)
	case sig.KellyFraction != 0.5:
		t.Fatalf("kelly fraction = %v, want 0.5", sig.KellyFraction)
	case sig.ExpectedValuePercent < sig.ThresholdEVPercent:
		t.Fatalf("the finding does not meet the threshold it claims")
	case sig.DetectedAt != fixtureAnchor:
		t.Fatalf("detected_at = %s, want the injected instant", sig.DetectedAt)
	}
}

// TestNegativeQuoteAgePassesTheFreshnessBound pins a deliberate asymmetry.
//
// A provider clock running ahead of ours produces a NEGATIVE age.
// [domain.Price.Age] returns it rather than clamping, precisely so a monitor can
// see the skew, and every age comparison in this package lets it through. A
// finder that clamped would hide a monitoring problem behind a suppressed signal.
func TestNegativeQuoteAgePassesTheFreshnessBound(t *testing.T) {
	finder, err := NewEVFinder(EVConfig{})
	if err != nil {
		t.Fatalf("NewEVFinder: %v", err)
	}
	out, _ := finder.Scan(computedMarket(t, "moneyline", 0,
		quoteSpec{selection: "s", book: "b", offered: 2.40, fair: 0.50, age: -30 * time.Second},
	), fixtureAnchor)
	if len(out) != 1 {
		t.Fatalf("got %d findings, want 1: a quote stamped in the future is a clock-skew "+
			"problem, not a stale price", len(out))
	}
	if out[0].QuoteAgeSeconds >= 0 {
		t.Fatalf("age = %v, want it reported negative rather than clamped", out[0].QuoteAgeSeconds)
	}
}

// TestCapKeepsTheStrongestAndCountsTheRest asserts that the per-market cap is
// applied AFTER ranking, which is the only ordering that makes it a cap on the
// weakest rather than on whichever findings happened to be scanned last.
func TestCapKeepsTheStrongestAndCountsTheRest(t *testing.T) {
	finder, err := NewEVFinder(EVConfig{MaxSignalsPerMarket: 2})
	if err != nil {
		t.Fatalf("NewEVFinder: %v", err)
	}
	out, stats := finder.Scan(computedMarket(t, "moneyline", 0,
		quoteSpec{selection: "s", book: "weak", offered: 2.10, fair: 0.50},
		quoteSpec{selection: "s", book: "strong", offered: 2.60, fair: 0.50},
		quoteSpec{selection: "s", book: "middle", offered: 2.30, fair: 0.50},
	), fixtureAnchor)

	if len(out) != 2 {
		t.Fatalf("got %d findings, want 2", len(out))
	}
	if out[0].BookID != domain.BookID("strong") || out[1].BookID != domain.BookID("middle") {
		t.Fatalf("the cap kept %s and %s, want strong and middle", out[0].BookID, out[1].BookID)
	}
	if stats.Reasons[EVReasonCapped] != 1 {
		t.Fatalf("capped = %d, want 1", stats.Reasons[EVReasonCapped])
	}
	if stats.Signals != 2 {
		t.Fatalf("signals = %d, want 2", stats.Signals)
	}
}

// TestConfigRefusesWhatItCannotMean asserts that a configuration mistake is a
// startup error rather than a detector that quietly reports nothing.
func TestConfigRefusesWhatItCannotMean(t *testing.T) {
	tests := []struct {
		name string
		cfg  EVConfig
	}{
		{"a negative floor makes positive EV a name", EVConfig{MinEVPercent: -1}},
		{"a negative freshness bound", EVConfig{MaxQuoteAge: -time.Second}},
		{"a negative error-bar multiple", EVConfig{MinEdgeToErrorBar: -1}},
		{"a Kelly multiplier above one", EVConfig{KellyFraction: 1.5}},
		{"a non-finite floor rejects everything silently", EVConfig{MinEVPercent: math.NaN()}},
		{"a negative cap", EVConfig{MaxSignalsPerMarket: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewEVFinder(tc.cfg); err == nil {
				t.Fatal("the configuration was accepted")
			}
		})
	}
}

// mustLine builds a present line or fails the test.
func mustLine(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("NewLine(%v): %v", v, err)
	}
	return l
}
