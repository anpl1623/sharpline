package analytics

import (
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// twoWayArb is the fixture every staleness test varies: a genuine under-round
// pair across two books, worth about 2.3% of the outlay.
func twoWayArb(t *testing.T, oldest, spread time.Duration) pricing.ArbitrageRef {
	t.Helper()
	return arbitrageRef(t, oldest, spread,
		arbLeg{selection: "sel-home", role: "home", book: "book-a", decimal: 2.10, stake: 0.4878, age: oldest},
		arbLeg{selection: "sel-away", role: "away", book: "book-b", decimal: 2.05, stake: 0.5122, age: oldest - spread},
	)
}

// TestStalenessBoundSuppressesAStaleArbitrage is a phase-9 gate item.
//
// Decision 5 of the phase-9 brief is the whole reason: the phase-4 gate measured
// 68 live arbitrages across 1,065 records with the leg-age bound binding
// constantly, because most cross-book "arbitrage" is one book not having moved
// yet. A finder that reported them all would be worse than no finder — it teaches
// whoever reads the board that the board is wrong.
//
// The test asserts BOTH directions on the same fixture. A bound that suppressed
// everything would pass a suppression-only test.
func TestStalenessBoundSuppressesAStaleArbitrage(t *testing.T) {
	tests := []struct {
		name   string
		cfg    ArbConfig
		oldest time.Duration
		spread time.Duration
		want   ArbReason
	}{
		{
			name:   "fresh legs observed close together are a signal",
			oldest: 20 * time.Second,
			spread: 5 * time.Second,
			want:   ArbReasonSignal,
		},
		{
			name:   "a leg older than the bound is one book that has not moved yet",
			oldest: DefaultMaxArbLegAge + time.Second,
			spread: 5 * time.Second,
			want:   ArbReasonStaleLeg,
		},
		{
			name:   "legs observed too far apart describe two instants of the market, not one",
			oldest: 50 * time.Second,
			spread: DefaultMaxArbSpread + time.Second,
			want:   ArbReasonWideSpread,
		},
		{
			name:   "a leg exactly on the bound is inside it",
			oldest: DefaultMaxArbLegAge,
			spread: DefaultMaxArbSpread,
			want:   ArbReasonSignal,
		},
		{
			name:   "a tighter configured bound suppresses a finding the default admits",
			cfg:    ArbConfig{MaxLegAge: 10 * time.Second, MaxObservedSpread: 5 * time.Second},
			oldest: 20 * time.Second,
			spread: 2 * time.Second,
			want:   ArbReasonStaleLeg,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			surface, err := NewArbSurface(tc.cfg)
			if err != nil {
				t.Fatalf("NewArbSurface: %v", err)
			}
			rec := computedMarket(t, "moneyline", 0)
			rec.Arbitrage = []pricing.ArbitrageRef{twoWayArb(t, tc.oldest, tc.spread)}

			out, stats := surface.Scan(rec, fixtureAnchor)
			if stats.Examined != 1 {
				t.Fatalf("examined = %d, want 1", stats.Examined)
			}
			if got := stats.Reasons[tc.want]; got != 1 {
				t.Fatalf("reason %q counted %d times, want 1; reasons were %v",
					tc.want, got, stats.Reasons)
			}
			if tc.want != ArbReasonSignal {
				if len(out) != 0 {
					t.Fatalf("a suppressed finding still reached the output")
				}
				return
			}

			if len(out) != 1 {
				t.Fatalf("got %d findings, want 1", len(out))
			}
			sig := out[0]

			// THE EVIDENCE MUST TRAVEL WITH THE FINDING. A consumer that cannot see
			// how stale the legs are cannot judge it, which is the whole of decision
			// 5, and migrations/00009 CHECKs the finding against the bounds it
			// carries.
			switch {
			case sig.OldestLegAgeSeconds != tc.oldest.Seconds():
				t.Fatalf("oldest leg age = %v, want %v", sig.OldestLegAgeSeconds, tc.oldest.Seconds())
			case sig.ObservedSpreadSeconds != tc.spread.Seconds():
				t.Fatalf("observed spread = %v, want %v", sig.ObservedSpreadSeconds, tc.spread.Seconds())
			case sig.OldestLegAgeSeconds > sig.MaxLegAgeSeconds:
				t.Fatalf("the finding does not meet the leg-age bound it claims")
			case sig.ObservedSpreadSeconds > sig.MaxObservedSpreadSeconds:
				t.Fatalf("the finding does not meet the spread bound it claims")
			}
		})
	}
}

// TestThinReturnsAndSingleBookFindings pins the other two gates, including the
// one that looks backwards.
func TestThinReturnsAndSingleBookFindings(t *testing.T) {
	t.Run("a return inside the quoting granularity is not worth reporting", func(t *testing.T) {
		surface, err := NewArbSurface(ArbConfig{})
		if err != nil {
			t.Fatalf("NewArbSurface: %v", err)
		}
		rec := computedMarket(t, "moneyline", 0)
		// 1/2.005 + 1/2.000 = 0.99875, a return of 0.00125 — a tenth of the
		// default floor and inside one tick on a 10-cent American grid.
		rec.Arbitrage = []pricing.ArbitrageRef{arbitrageRef(t, 10*time.Second, time.Second,
			arbLeg{selection: "sel-home", role: "home", book: "a", decimal: 2.005, stake: 0.4994, age: 10 * time.Second},
			arbLeg{selection: "sel-away", role: "away", book: "b", decimal: 2.000, stake: 0.5006, age: 9 * time.Second},
		)}
		_, stats := surface.Scan(rec, fixtureAnchor)
		if stats.Reasons[ArbReasonThinReturn] != 1 {
			t.Fatalf("reasons = %v, want one thin_return", stats.Reasons)
		}
	})

	t.Run("a single book under-rounding its own market is kept", func(t *testing.T) {
		// The default MinDistinctBooks is 1, and arb.go argues that this is the
		// STRONGER finding rather than the weaker one: one book, one refresh, one
		// instant, so there is no cross-book staleness explanation available to
		// it at all.
		surface, err := NewArbSurface(ArbConfig{})
		if err != nil {
			t.Fatalf("NewArbSurface: %v", err)
		}
		rec := computedMarket(t, "moneyline", 0)
		rec.Arbitrage = []pricing.ArbitrageRef{arbitrageRef(t, 10*time.Second, 0,
			arbLeg{selection: "sel-home", role: "home", book: "only", decimal: 2.10, stake: 0.4878, age: 10 * time.Second},
			arbLeg{selection: "sel-away", role: "away", book: "only", decimal: 2.05, stake: 0.5122, age: 10 * time.Second},
		)}
		out, stats := surface.Scan(rec, fixtureAnchor)
		if len(out) != 1 {
			t.Fatalf("got %d findings, want 1; reasons were %v", len(out), stats.Reasons)
		}
		if out[0].DistinctBooks != 1 {
			t.Fatalf("distinct books = %d, want 1", out[0].DistinctBooks)
		}
	})
}

// TestFingerprintIsAStableFunctionOfTheLegSet pins the digest phase 12 has to
// reproduce.
//
// (market_id, observed_at) is not unique — one market with several books can
// yield more than one under-round combination at a single instant — so the
// fingerprint is what separates two findings and what makes a recomputation of
// ONE finding an update rather than an insert. Get it wrong in one direction and
// a replay duplicates everything; wrong in the other and two different findings
// collapse into one and the second silently overwrites the first.
func TestFingerprintIsAStableFunctionOfTheLegSet(t *testing.T) {
	legs := []ArbitrageSignalLeg{
		{LegIndex: 0, SelectionID: "sel-home", BookID: "a", DecimalOdds: 2.10, Line: mustLine(t, -3.5)},
		{LegIndex: 1, SelectionID: "sel-away", BookID: "b", DecimalOdds: 2.05, Line: mustLine(t, 3.5)},
	}
	want := FingerprintArbitrageLegs(legs)

	t.Run("the leg order does not change the digest", func(t *testing.T) {
		// The legs are sorted by selection before hashing precisely because Go's
		// map order and a SQL engine's collect order are both unspecified.
		reversed := []ArbitrageSignalLeg{legs[1], legs[0]}
		if got := FingerprintArbitrageLegs(reversed); got != want {
			t.Fatalf("reordering the legs changed the digest:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("the leg index does not change the digest", func(t *testing.T) {
		renumbered := []ArbitrageSignalLeg{
			{LegIndex: 7, SelectionID: "sel-home", BookID: "a", DecimalOdds: 2.10, Line: mustLine(t, -3.5)},
			{LegIndex: 9, SelectionID: "sel-away", BookID: "b", DecimalOdds: 2.05, Line: mustLine(t, 3.5)},
		}
		if got := FingerprintArbitrageLegs(renumbered); got != want {
			t.Fatalf("the index is presentation, not identity")
		}
	})

	t.Run("nothing outside the four leg fields changes the digest", func(t *testing.T) {
		// Ages and stake fractions are consequences of the legs and of the
		// instant they were measured at. Folding one in would make a
		// recomputation that fixed a rounding bug produce a NEW finding rather
		// than a correction to the old one.
		noisy := []ArbitrageSignalLeg{
			{
				LegIndex: 0, SelectionID: "sel-home", BookID: "a", DecimalOdds: 2.10,
				Line: mustLine(t, -3.5), StakeFraction: 0.9, AgeSeconds: 400,
				ObservedAt: fixtureAnchor.Add(-time.Hour), Role: "home",
			},
			{
				LegIndex: 1, SelectionID: "sel-away", BookID: "b", DecimalOdds: 2.05,
				Line: mustLine(t, 3.5), StakeFraction: 0.1, AgeSeconds: 1,
				ObservedAt: fixtureAnchor, Role: "away",
			},
		}
		if got := FingerprintArbitrageLegs(noisy); got != want {
			t.Fatalf("a field outside (selection, book, decimal, line) reached the digest")
		}
	})

	t.Run("a different price is a different finding", func(t *testing.T) {
		moved := []ArbitrageSignalLeg{
			{LegIndex: 0, SelectionID: "sel-home", BookID: "a", DecimalOdds: 2.11, Line: mustLine(t, -3.5)},
			legs[1],
		}
		if got := FingerprintArbitrageLegs(moved); got == want {
			t.Fatal("a price change did not change the digest, so the two findings would collide")
		}
	})

	t.Run("an absent line is distinguishable from a present one", func(t *testing.T) {
		// The absent case contributes a sentinel rather than nothing. Skipping the
		// field would make the digest depend on how a SQL engine renders a NULL
		// inside a concatenation, which is exactly the cross-language ambiguity the
		// digest exists to remove.
		unlined := []ArbitrageSignalLeg{
			{LegIndex: 0, SelectionID: "sel-home", BookID: "a", DecimalOdds: 2.10},
			{LegIndex: 1, SelectionID: "sel-away", BookID: "b", DecimalOdds: 2.05},
		}
		if got := FingerprintArbitrageLegs(unlined); got == want {
			t.Fatal("a market with no line digested the same as one with a line")
		}
	})

	t.Run("the digest is lowercase hex inside the column's bounds", func(t *testing.T) {
		if len(want) != 64 {
			t.Fatalf("digest is %d characters; migrations/00009 admits 16 to 64", len(want))
		}
		for _, r := range want {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				t.Fatalf("digest contains %q, outside lowercase hex", r)
			}
		}
	})
}

// TestArbitrageRankingIsTotal asserts the ordering matches
// arbitrage_signals_rank_idx, and that ties are broken rather than left to the
// input order.
func TestArbitrageRankingIsTotal(t *testing.T) {
	surface, err := NewArbSurface(ArbConfig{})
	if err != nil {
		t.Fatalf("NewArbSurface: %v", err)
	}
	rec := computedMarket(t, "moneyline", 0)
	rec.Arbitrage = []pricing.ArbitrageRef{
		arbitrageRef(t, 10*time.Second, time.Second,
			arbLeg{selection: "sel-home", role: "home", book: "a", decimal: 2.10, stake: 0.49, age: 10 * time.Second},
			arbLeg{selection: "sel-away", role: "away", book: "b", decimal: 2.05, stake: 0.51, age: 9 * time.Second},
		),
		arbitrageRef(t, 10*time.Second, time.Second,
			arbLeg{selection: "sel-home", role: "home", book: "c", decimal: 2.30, stake: 0.47, age: 10 * time.Second},
			arbLeg{selection: "sel-away", role: "away", book: "d", decimal: 2.20, stake: 0.53, age: 9 * time.Second},
		),
	}

	out, _ := surface.Scan(rec, fixtureAnchor)
	if len(out) != 2 {
		t.Fatalf("got %d findings, want 2", len(out))
	}
	if out[0].ReturnFraction <= out[1].ReturnFraction {
		t.Fatalf("findings are not in descending return order")
	}

	// Reversing the input must not change the answer.
	rec.Arbitrage[0], rec.Arbitrage[1] = rec.Arbitrage[1], rec.Arbitrage[0]
	swapped, _ := surface.Scan(rec, fixtureAnchor)
	for i := range out {
		if swapped[i].LegsFingerprint != out[i].LegsFingerprint {
			t.Fatalf("reversing the input changed the ranking at position %d", i)
		}
	}
}

// TestArbConfigRefusesWhatItCannotMean asserts a configuration mistake is a
// startup error.
func TestArbConfigRefusesWhatItCannotMean(t *testing.T) {
	tests := []struct {
		name string
		cfg  ArbConfig
	}{
		{"a negative leg age", ArbConfig{MaxLegAge: -time.Second}},
		{"a spread wider than the age bound can never bind", ArbConfig{
			MaxLegAge: 10 * time.Second, MaxObservedSpread: 30 * time.Second,
		}},
		{"a negative return floor", ArbConfig{MinReturn: -1}},
		{"a negative book count", ArbConfig{MinDistinctBooks: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewArbSurface(tc.cfg); err == nil {
				t.Fatal("the configuration was accepted")
			}
		})
	}
}

// TestLegRoleAndLineRulesMatchTheSchema asserts the leg-level guards that stop a
// single malformed finding aborting the transaction its siblings share.
func TestLegRoleAndLineRulesMatchTheSchema(t *testing.T) {
	tests := []struct {
		name       string
		marketType string
		leg        ArbitrageSignalLeg
		wantErr    bool
	}{
		{
			name:       "a home leg on a moneyline with no line",
			marketType: "moneyline",
			leg:        ArbitrageSignalLeg{Role: "home", DecimalOdds: 2, StakeFraction: 0.5},
		},
		{
			name:       "an invented role",
			marketType: "moneyline",
			leg:        ArbitrageSignalLeg{Role: "sideways", DecimalOdds: 2, StakeFraction: 0.5},
			wantErr:    true,
		},
		{
			name:       "a total at a non-positive line",
			marketType: "total",
			leg: ArbitrageSignalLeg{
				Role: "under", DecimalOdds: 2, StakeFraction: 0.5, Line: mustLine(t, -2.5),
			},
			wantErr: true,
		},
		{
			name:       "a stake fraction of one leaves nothing for the other leg",
			marketType: "moneyline",
			leg:        ArbitrageSignalLeg{Role: "home", DecimalOdds: 2, StakeFraction: 1},
			wantErr:    true,
		},
		{
			name:       "a decimal price at evens is not a price",
			marketType: "moneyline",
			leg:        ArbitrageSignalLeg{Role: "home", DecimalOdds: odds.Decimal(1), StakeFraction: 0.5},
			wantErr:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.leg.validate(tc.marketType)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validate = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestLineRuleMatchesMigration walks the five market types against the CHECK
// constraint migrations/00009 writes, because a Go rule that has drifted from the
// SQL one turns a dropped finding into an aborted transaction.
func TestLineRuleMatchesMigration(t *testing.T) {
	tests := []struct {
		marketType string
		line       domain.Line
		wantErr    bool
	}{
		{"moneyline", domain.NoLine(), false},
		{"moneyline", mustLine(t, -3.5), true},
		{"futures", domain.NoLine(), false},
		{"futures", mustLine(t, 0), true},
		{"spread", mustLine(t, -3.5), false},
		{"spread", mustLine(t, 0), false}, // a pick 'em is a real handicap
		{"spread", domain.NoLine(), true},
		{"total", mustLine(t, 228.5), false},
		{"total", mustLine(t, 0), true},
		{"total", domain.NoLine(), true},
		{"player_prop", domain.NoLine(), false},
		{"player_prop", mustLine(t, 21.5), false},
		{"parlay", domain.NoLine(), true}, // not one of the five
	}
	for _, tc := range tests {
		t.Run(tc.marketType+"/"+tc.line.String(), func(t *testing.T) {
			err := lineRule(tc.marketType, tc.line)
			if tc.wantErr != (err != nil) {
				t.Fatalf("lineRule(%q, %s) = %v, wantErr = %v",
					tc.marketType, tc.line, err, tc.wantErr)
			}
		})
	}
}
