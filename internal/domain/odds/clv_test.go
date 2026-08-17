package odds

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/anpl1623/sharpline/internal/domain"
)

// -----------------------------------------------------------------------------
// Float comparison policy for this file
// -----------------------------------------------------------------------------
//
// Nothing below compares two computed floats with ==, except where bit-for-bit
// agreement between two spellings of the SAME deterministic computation is the
// property under test; each of those is argued for at its assertion.

const (
	// clvTol is the relative tolerance for a CLV value that came out of a short
	// chain of correctly-rounded double operations: two reciprocals, a division,
	// a subtraction and a multiplication, so at most five roundings and a true
	// bound under 6e-16.
	//
	// 1e-12 is roughly 4,500 ULPs, matching the convention the rest of this
	// domain's tests use, and is nine orders of magnitude below the smallest
	// difference the domain can express: one cent of decimal odds at even money
	// moves the implied probability by 2.5e-3 and the percentage CLV by ~0.5.
	// The tolerance cannot absorb a wrong formula.
	clvTol = 1e-12

	// clvAggTol is the looser tolerance for a comparison between two summation
	// ORDERS of the same aggregate. It is the number the phase-12 Flink SQL job
	// is validated against, because Flink's aggregation order varies with
	// parallelism and no tighter agreement is achievable there.
	clvAggTol = 1e-9
)

// clvNear reports whether got and want agree to within relTol, scaled by the
// larger magnitude but never below 1, so the comparison degrades to an absolute
// tolerance near zero instead of demanding impossible relative precision of a
// value that is legitimately 0.
func clvNear(got, want, relTol float64) bool {
	if got == want {
		return true
	}
	if math.IsNaN(got) || math.IsNaN(want) || math.IsInf(got, 0) || math.IsInf(want, 0) {
		return false
	}
	scale := math.Max(1, math.Max(math.Abs(got), math.Abs(want)))
	return math.Abs(got-want) <= relTol*scale
}

// clvTB is the slice of the testing API the assertions below need, so that one
// helper serves both *testing.T and rapid's *rapid.T.
type clvTB interface {
	Helper()
	Errorf(format string, args ...any)
}

func clvAssertNear(t clvTB, what string, got, want, relTol float64) {
	t.Helper()
	if !clvNear(got, want, relTol) {
		t.Errorf("%s = %.17g, want %.17g (relative tolerance %g)", what, got, want, relTol)
	}
}

// clvAt returns a fixed instant offset from a literal base. This package reads
// no clock, so every temporal assertion is against a literal.
func clvAt(offset time.Duration) time.Time {
	return time.Date(2026, time.August, 16, 18, 0, 0, 0, time.UTC).Add(offset)
}

func clvMustLine(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("domain.NewLine(%v): %v", v, err)
	}
	return l
}

// -----------------------------------------------------------------------------
// Price data
// -----------------------------------------------------------------------------
//
// The prices below are the standard published rungs of the American odds ladder
// (-110, -105, -120, +100) and of the European 1X2 ladder (2.10, 3.40, 3.80).
// They are real price points in the sense that matters here — -110 is the
// canonical US juice price and every book posts it — but no assertion in this
// file claims to reproduce a specific quote from a specific book on a specific
// date. That claim needs a recorded provider payload, which is what the ingest
// phase's golden files are for.

const (
	// clvDecMinus110 is decimal for American -110: 1 + 100/110 = 21/11.
	clvDecMinus110 = 21.0 / 11.0
	// clvDecMinus105 is decimal for American -105: 1 + 100/105 = 41/21.
	clvDecMinus105 = 41.0 / 21.0
	// clvDecMinus120 is decimal for American -120: 1 + 100/120 = 11/6.
	clvDecMinus120 = 11.0 / 6.0
	// clvDecPlus100 is decimal for American +100, even money.
	clvDecPlus100 = 2.0
)

// clvDevig removes a book's margin multiplicatively: p_i = (1/d_i) / Σ_j(1/d_j).
//
// This is TEST code, not a second implementation of the package's devig: it
// exists to manufacture inputs, and its output is independently cross-checked
// against hand-derived rationals in TestCLVDevigHelperMatchesHandArithmetic.
// The multiplicative method is used because it is the one whose answers can be
// written down exactly as fractions.
func clvDevig(t *testing.T, decimals ...float64) []Probability {
	t.Helper()
	if len(decimals) < clvMinSelections {
		t.Fatalf("clvDevig needs at least %d prices, got %d", clvMinSelections, len(decimals))
	}
	implied := make([]float64, len(decimals))
	var overround float64
	for i, d := range decimals {
		if d <= 1 {
			t.Fatalf("clvDevig: %v is not a decimal price", d)
		}
		implied[i] = 1 / d
		overround += implied[i]
	}
	out := make([]Probability, len(decimals))
	for i, p := range implied {
		out[i] = Probability(p / overround)
	}
	return out
}

// clvSnapshot builds a FairMarketSnapshot from decimal prices, devigging them on
// the way in, and fails the test if the result is not constructible.
func clvSnapshot(t *testing.T, market domain.MarketID, book domain.BookID, line domain.Line,
	at time.Time, ids []domain.SelectionID, decimals ...float64,
) FairMarketSnapshot {
	t.Helper()
	if len(ids) != len(decimals) {
		t.Fatalf("clvSnapshot: %d ids against %d prices", len(ids), len(decimals))
	}
	fair := clvDevig(t, decimals...)
	sel := make([]FairSelection, len(ids))
	for i := range ids {
		sel[i] = FairSelection{Selection: ids[i], Fair: fair[i]}
	}
	snap, err := NewFairMarketSnapshot(FairMarketSnapshotParams{
		Market: market, Book: book, Line: line, ObservedAt: at, Fair: sel,
	})
	if err != nil {
		t.Fatalf("NewFairMarketSnapshot: %v", err)
	}
	return snap
}

// clvTwoWay is the common case: a two-selection market with a line.
func clvTwoWay(t *testing.T, book domain.BookID, at time.Time, line domain.Line, home, away float64) FairMarketSnapshot {
	t.Helper()
	return clvSnapshot(t, "mkt-nba-spread", book, line, at,
		[]domain.SelectionID{"sel-home", "sel-away"}, home, away)
}

// -----------------------------------------------------------------------------
// The devig helper is itself checked against hand arithmetic
// -----------------------------------------------------------------------------

// TestCLVDevigHelperMatchesHandArithmetic pins the test fixture to arithmetic
// that can be done on paper, so that a later failure is attributable to the code
// under test rather than to the fixture.
//
//	-110 / -110  →  implied 11/21 each, Σ = 22/21, fair 1/2 and 1/2
//	-120 / +100  →  implied 6/11 and 1/2, Σ = 23/22, fair 12/23 and 11/23
//	-105 / -105  →  implied 21/41 each, Σ = 42/41, fair 1/2 and 1/2
func TestCLVDevigHelperMatchesHandArithmetic(t *testing.T) {
	cases := []struct {
		name    string
		prices  []float64
		want    []float64
		wantSum float64
	}{
		{"-110/-110", []float64{clvDecMinus110, clvDecMinus110}, []float64{0.5, 0.5}, 1},
		{"-105/-105", []float64{clvDecMinus105, clvDecMinus105}, []float64{0.5, 0.5}, 1},
		{"-120/+100", []float64{clvDecMinus120, clvDecPlus100}, []float64{12.0 / 23.0, 11.0 / 23.0}, 1},
		{"+100/-120", []float64{clvDecPlus100, clvDecMinus120}, []float64{11.0 / 23.0, 12.0 / 23.0}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clvDevig(t, c.prices...)
			var sum float64
			for i, p := range got {
				clvAssertNear(t, "fair probability", float64(p), c.want[i], clvTol)
				sum += float64(p)
			}
			clvAssertNear(t, "sum", sum, c.wantSum, clvTol)
		})
	}
}

// TestStandardJuiceOverroundAndHold records the two numbers the worked example
// in clv.go rests on, so that the example cannot drift from the arithmetic.
//
//	-110 both sides: Σp = 22/21 ≈ 1.047619, hold = 1 − 1/Σ ≈ 4.5455%
//	-105 both sides: Σp = 42/41 ≈ 1.024390, hold = 1 − 1/Σ ≈ 2.3810%
func TestStandardJuiceOverroundAndHold(t *testing.T) {
	for _, c := range []struct {
		name          string
		decimal       float64
		wantOverround float64
		wantHoldPct   float64
	}{
		{"-110", clvDecMinus110, 22.0 / 21.0, 100 * (1 - 21.0/22.0)},
		{"-105", clvDecMinus105, 42.0 / 41.0, 100 * (1 - 41.0/42.0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := Decimal(c.decimal).Probability()
			if err != nil {
				t.Fatalf("Probability: %v", err)
			}
			sum := 2 * float64(p)
			clvAssertNear(t, "overround", sum, c.wantOverround, clvTol)
			clvAssertNear(t, "hold %", 100*(1-1/sum), c.wantHoldPct, clvTol)
		})
	}
}

// -----------------------------------------------------------------------------
// The headline measures
// -----------------------------------------------------------------------------

// TestProbabilityCLV covers the sign convention and the endpoints.
func TestProbabilityCLV(t *testing.T) {
	cases := []struct {
		name           string
		taken, closing float64
		want           float64
		wantErr        error
	}{
		{name: "market moved to you", taken: 0.5, closing: 12.0 / 23.0, want: 1.0 / 46.0},
		{name: "market moved against you", taken: 0.5, closing: 11.0 / 23.0, want: -1.0 / 46.0},
		{name: "no movement", taken: 0.5, closing: 0.5, want: 0},
		{name: "degenerate endpoints are arithmetic, not prices", taken: 0, closing: 1, want: 1},
		{name: "taken is NaN", taken: math.NaN(), closing: 0.5, wantErr: ErrNotFinite},
		{name: "closing is +Inf", taken: 0.5, closing: math.Inf(1), wantErr: ErrNotFinite},
		{name: "taken above 1", taken: 1.5, closing: 0.5, wantErr: ErrProbabilityOutOfRange},
		{name: "closing below 0", taken: 0.5, closing: -0.1, wantErr: ErrProbabilityOutOfRange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ProbabilityCLV(Probability(c.taken), Probability(c.closing))
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("error = %v, want one wrapping %v", err, c.wantErr)
				}
				if got != 0 {
					t.Errorf("value on error = %v, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			clvAssertNear(t, "ProbabilityCLV", got, c.want, clvTol)
		})
	}
}

// TestPercentCLV covers the sign convention, the exact-zero case and overflow.
func TestPercentCLV(t *testing.T) {
	cases := []struct {
		name           string
		taken, closing float64
		want           float64
		wantErr        error
	}{
		// Fair 2.0 taken against a fair close of 23/12: 2/(23/12) − 1 = 1/23.
		{name: "beat the close", taken: 2.0, closing: 23.0 / 12.0, want: 100.0 / 23.0},
		// Fair 23/11 taken against a fair close of 2.0 is the mirror image.
		{name: "beaten by the close", taken: 2.0, closing: 23.0 / 11.0, want: 100 * (22.0/23.0 - 1)},
		{name: "identical prices", taken: clvDecMinus110, closing: clvDecMinus110, want: 0},
		// The classic error, quantified: comparing RAW -110 against RAW -105
		// reports a 2.22% loss on a line whose fair price never moved.
		{name: "raw prices conflate a vig change with a line move",
			taken: clvDecMinus110, closing: clvDecMinus105, want: 100 * (441.0/451.0 - 1)},
		{name: "taken is exactly 1", taken: 1, closing: 2, wantErr: ErrDecimalOutOfRange},
		{name: "closing is exactly 1", taken: 2, closing: 1, wantErr: ErrDecimalOutOfRange},
		{name: "taken is NaN", taken: math.NaN(), closing: 2, wantErr: ErrNotFinite},
		{name: "closing is +Inf", taken: 2, closing: math.Inf(1), wantErr: ErrNotFinite},
		// Decimal has no upper bound of its own, so the ratio can overflow:
		// MaxFloat64/2 is ~9e307 and multiplying by 100 leaves the range.
		{name: "ratio overflows", taken: math.MaxFloat64, closing: 2, wantErr: ErrNotFinite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := PercentCLV(Decimal(c.taken), Decimal(c.closing))
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("error = %v, want one wrapping %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			clvAssertNear(t, "PercentCLV", got, c.want, clvTol)
		})
	}
}

// TestPercentCLVOfIdenticalPricesIsExactlyZero asserts bit-exact zero rather
// than near-zero. d/d is exactly 1.0 for every finite non-zero double, so
// (d/d − 1)·100 is exactly +0.0. A near-zero result would mean the function
// stopped being a single division.
func TestPercentCLVOfIdenticalPricesIsExactlyZero(t *testing.T) {
	for _, d := range []float64{clvDecMinus110, clvDecMinus105, clvDecPlus100, 1.001, 1001} {
		got, err := PercentCLV(Decimal(d), Decimal(d))
		if err != nil {
			t.Fatalf("PercentCLV(%v, %v): %v", d, d, err)
		}
		if got != 0 || math.Signbit(got) {
			t.Errorf("PercentCLV(%v, %v) = %v, want exactly +0", d, d, got)
		}
	}
}

// TestBeatTheClose covers the boolean, the dead band and the magnitude.
func TestBeatTheClose(t *testing.T) {
	cases := []struct {
		name           string
		taken, closing float64
		wantBeat       bool
		wantMagnitude  float64
		wantErr        error
	}{
		{name: "beat", taken: 0.5, closing: 12.0 / 23.0, wantBeat: true, wantMagnitude: 100.0 / 23.0},
		{name: "beaten", taken: 0.5, closing: 11.0 / 23.0, wantBeat: false, wantMagnitude: 100 * (1 - 22.0/23.0)},
		{name: "exact tie", taken: 0.5, closing: 0.5, wantBeat: false, wantMagnitude: 0},
		{name: "inside the tie band", taken: 0.5, closing: 0.5 + CLVTieBand/2, wantBeat: false},
		{name: "just outside the tie band", taken: 0.5, closing: 0.5 + 10*CLVTieBand, wantBeat: true},
		{name: "taken not priceable", taken: 0, closing: 0.5, wantErr: ErrProbabilityNotPriceable},
		{name: "closing not priceable", taken: 0.5, closing: 1, wantErr: ErrProbabilityNotPriceable},
		{name: "taken out of range", taken: 2, closing: 0.5, wantErr: ErrProbabilityOutOfRange},
		// Both probabilities are priceable, but a fair price of 1e307 against a
		// fair price of 2 puts the percentage past the top of the float64 range.
		{name: "the percentage overflows", taken: 1e-307, closing: 0.5, wantErr: ErrNotFinite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			beat, magnitude, err := BeatTheClose(Probability(c.taken), Probability(c.closing))
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("error = %v, want one wrapping %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if beat != c.wantBeat {
				t.Errorf("beat = %t, want %t", beat, c.wantBeat)
			}
			if magnitude < 0 {
				t.Errorf("magnitude = %v, must never be negative", magnitude)
			}
			if c.wantMagnitude != 0 || c.name == "exact tie" {
				clvAssertNear(t, "magnitude", magnitude, c.wantMagnitude, clvTol)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// FairMarketSnapshot construction
// -----------------------------------------------------------------------------

func TestNewFairMarketSnapshotRejectsBadInput(t *testing.T) {
	ok := []FairSelection{{Selection: "sel-home", Fair: 0.5}, {Selection: "sel-away", Fair: 0.5}}

	cases := []struct {
		name   string
		params FairMarketSnapshotParams
		want   error
	}{
		{"no market id", FairMarketSnapshotParams{Book: "book-1", ObservedAt: clvAt(0), Fair: ok}, ErrCLVMissingIdentity},
		{"no book id", FairMarketSnapshotParams{Market: "mkt-1", ObservedAt: clvAt(0), Fair: ok}, ErrCLVMissingIdentity},
		{"zero observation", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", Fair: ok}, ErrCLVZeroObservation},
		{"no selections", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0)}, ErrCLVMarketIncomplete},
		{"one selection", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
			Fair: []FairSelection{{Selection: "sel-home", Fair: 1}}}, ErrCLVMarketIncomplete},
		{"selection without an id", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
			Fair: []FairSelection{{Fair: 0.5}, {Selection: "sel-away", Fair: 0.5}}}, ErrCLVMissingIdentity},
		{"duplicate selection", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
			Fair: []FairSelection{{Selection: "sel-home", Fair: 0.5}, {Selection: "sel-home", Fair: 0.5}}}, ErrCLVDuplicateSelection},
		{"probability out of range", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
			Fair: []FairSelection{{Selection: "sel-home", Fair: 1.5}, {Selection: "sel-away", Fair: -0.5}}}, ErrProbabilityOutOfRange},
		{"probability not finite", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
			Fair: []FairSelection{{Selection: "sel-home", Fair: Probability(math.NaN())}, {Selection: "sel-away", Fair: 0.5}}}, ErrNotFinite},
		// The check the whole design exists for: raw -110/-110 implied
		// probabilities sum to 22/21, so a vigged book cannot become a snapshot.
		{"still vigged", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
			Fair: []FairSelection{
				{Selection: "sel-home", Fair: Probability(1 / clvDecMinus110)},
				{Selection: "sel-away", Fair: Probability(1 / clvDecMinus110)},
			}}, ErrCLVNotDevigged},
		{"sum falls short of one", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
			Fair: []FairSelection{{Selection: "sel-home", Fair: 0.4}, {Selection: "sel-away", Fair: 0.5}}}, ErrCLVNotDevigged},
		{"sum misses by just over the tolerance", FairMarketSnapshotParams{Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
			Fair: []FairSelection{{Selection: "sel-home", Fair: 0.5}, {Selection: "sel-away", Fair: Probability(0.5 + 2*CLVDevigTolerance)}}}, ErrCLVNotDevigged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NewFairMarketSnapshot(c.params)
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want one wrapping %v", err, c.want)
			}
			if !got.IsZero() {
				t.Errorf("a rejected snapshot is not the zero value: %s", got)
			}
			if !strings.HasPrefix(err.Error(), "odds: ") {
				t.Errorf("error %q does not carry the package prefix", err)
			}
			if strings.Count(err.Error(), "odds: ") != 1 {
				t.Errorf("error %q repeats the package prefix", err)
			}
		})
	}
}

// TestNewFairMarketSnapshotAcceptsAResidualInsideTheTolerance checks the other
// side of the sum-to-one gate: an iterative devig that converges to within
// CLVDevigTolerance is admitted rather than rejected for float noise.
func TestNewFairMarketSnapshotAcceptsAResidualInsideTheTolerance(t *testing.T) {
	_, err := NewFairMarketSnapshot(FairMarketSnapshotParams{
		Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0),
		Fair: []FairSelection{
			{Selection: "sel-home", Fair: 0.5},
			{Selection: "sel-away", Fair: Probability(0.5 - CLVDevigTolerance/2)},
		},
	})
	if err != nil {
		t.Fatalf("a residual of half the tolerance was rejected: %v", err)
	}
}

func TestFairMarketSnapshotAccessors(t *testing.T) {
	at := clvAt(0)
	line := clvMustLine(t, -3.5)
	snap := clvTwoWay(t, "book-dk", at, line, clvDecMinus110, clvDecMinus110)

	if snap.Market() != "mkt-nba-spread" {
		t.Errorf("Market = %s", snap.Market())
	}
	if snap.Book() != "book-dk" {
		t.Errorf("Book = %s", snap.Book())
	}
	if !snap.Line().Equal(line) {
		t.Errorf("Line = %s, want %s", snap.Line(), line)
	}
	if !snap.ObservedAt().Equal(at) {
		t.Errorf("ObservedAt = %s, want %s", snap.ObservedAt(), at)
	}
	if snap.ObservedAt().Location() != time.UTC {
		t.Errorf("ObservedAt was not normalised to UTC: %s", snap.ObservedAt())
	}
	if snap.IsZero() {
		t.Error("a constructed snapshot reports IsZero")
	}
	if got := len(snap.Selections()); got != 2 {
		t.Errorf("Selections length = %d, want 2", got)
	}
	if p, ok := snap.FairFor("sel-home"); !ok || float64(p) != 0.5 {
		t.Errorf("FairFor(sel-home) = %v, %t; want 0.5, true", p, ok)
	}
	if _, ok := snap.FairFor("sel-nobody"); ok {
		t.Error("FairFor reported a selection the snapshot does not price")
	}
}

// TestFairMarketSnapshotIsAValue asserts the two aliasing holes are closed: the
// slice handed to the constructor and the slice handed back by Selections are
// both copies, so a snapshot cannot be mutated into violating its sum-to-one
// invariant after the fact.
func TestFairMarketSnapshotIsAValue(t *testing.T) {
	input := []FairSelection{{Selection: "sel-home", Fair: 0.5}, {Selection: "sel-away", Fair: 0.5}}
	snap, err := NewFairMarketSnapshot(FairMarketSnapshotParams{
		Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0), Fair: input,
	})
	if err != nil {
		t.Fatalf("NewFairMarketSnapshot: %v", err)
	}

	input[0] = FairSelection{Selection: "sel-tampered", Fair: 0.99}
	if p, ok := snap.FairFor("sel-home"); !ok || float64(p) != 0.5 {
		t.Errorf("mutating the caller's slice changed the snapshot: FairFor(sel-home) = %v, %t", p, ok)
	}

	out := snap.Selections()
	out[1] = FairSelection{Selection: "sel-tampered", Fair: 0.99}
	if p, ok := snap.FairFor("sel-away"); !ok || float64(p) != 0.5 {
		t.Errorf("mutating the returned slice changed the snapshot: FairFor(sel-away) = %v, %t", p, ok)
	}
}

func TestFairMarketSnapshotString(t *testing.T) {
	if got := (FairMarketSnapshot{}).String(); got != "fairmarket(<zero>)" {
		t.Errorf("zero String = %q", got)
	}
	snap := clvTwoWay(t, "book-dk", clvAt(0), clvMustLine(t, -3.5), clvDecMinus110, clvDecMinus110)
	got := snap.String()
	for _, want := range []string{"mkt-nba-spread", "book-dk", "-3.5", "n=2", "2026-08-16T18:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("String = %q, missing %q", got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// EvaluateCLV — the worked cases
// -----------------------------------------------------------------------------

// TestEvaluateCLVWorkedCases walks the three outcomes with hand-derived
// constants.
//
// Taken:  -110 / -110  →  fair 1/2 and 1/2, fair decimal 2.0 both sides.
// Close A: -120 / +100 →  fair 12/23 and 11/23. The home side shortened, so a
//
//	home bet beat the close by 1/46 in probability and by 100/23 % in return.
//
// Close B: +100 / -120 →  the mirror image; the home bet was beaten.
// Close C: -105 / -105 →  fair 1/2 and 1/2. The book halved its margin and the
//
//	fair line did not move at all, so CLV is exactly zero.
func TestEvaluateCLVWorkedCases(t *testing.T) {
	line := clvMustLine(t, -3.5)
	taken := clvTwoWay(t, "book-dk", clvAt(0), line, clvDecMinus110, clvDecMinus110)

	cases := []struct {
		name            string
		homeClose       float64
		awayClose       float64
		wantProbability float64
		wantPercent     float64
		wantBeat        bool
	}{
		{"market moved to the home side", clvDecMinus120, clvDecPlus100, 1.0 / 46.0, 100.0 / 23.0, true},
		{"market moved away from the home side", clvDecPlus100, clvDecMinus120, -1.0 / 46.0, 100 * (22.0/23.0 - 1), false},
		{"only the book's margin moved", clvDecMinus105, clvDecMinus105, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			closing := clvTwoWay(t, "book-pinnacle", clvAt(3*time.Hour), line, c.homeClose, c.awayClose)

			got, err := EvaluateCLV(taken, closing, "sel-home")
			if err != nil {
				t.Fatalf("EvaluateCLV: %v", err)
			}

			clvAssertNear(t, "ProbabilityCLV", got.ProbabilityCLV, c.wantProbability, clvTol)
			clvAssertNear(t, "PercentCLV", got.PercentCLV, c.wantPercent, clvTol)
			clvAssertNear(t, "Magnitude", got.Magnitude, math.Abs(c.wantPercent), clvTol)
			if got.Beat != c.wantBeat {
				t.Errorf("Beat = %t, want %t", got.Beat, c.wantBeat)
			}
			if got.LineMoved {
				t.Error("LineMoved set on a comparison at one line")
			}

			// The record must carry everything needed to audit the number.
			if got.Market != "mkt-nba-spread" || got.Selection != "sel-home" {
				t.Errorf("identity = %s/%s", got.Market, got.Selection)
			}
			if got.TakenBook != "book-dk" || got.ClosingBook != "book-pinnacle" {
				t.Errorf("books = %s and %s; scoring one book against another must be allowed",
					got.TakenBook, got.ClosingBook)
			}
			if !got.Line.Equal(line) || !got.ClosingLine.Equal(line) {
				t.Errorf("lines = %s and %s, want %s", got.Line, got.ClosingLine, line)
			}
			if !got.TakenAt.Equal(clvAt(0)) || !got.ClosedAt.Equal(clvAt(3*time.Hour)) {
				t.Errorf("instants = %s and %s", got.TakenAt, got.ClosedAt)
			}
			clvAssertNear(t, "TakenPrice", float64(got.TakenPrice), 2.0, clvTol)
			clvAssertNear(t, "1/TakenFair", 1/float64(got.TakenFair), float64(got.TakenPrice), clvTol)
			clvAssertNear(t, "1/ClosingFair", 1/float64(got.ClosingFair), float64(got.ClosingPrice), clvTol)
		})
	}
}

// TestEvaluateCLVIsSymmetricAcrossTheTwoSides checks the other selection of the
// same market: if the home side beat the close, the away side was beaten by it.
func TestEvaluateCLVIsSymmetricAcrossTheTwoSides(t *testing.T) {
	line := clvMustLine(t, -3.5)
	taken := clvTwoWay(t, "book-dk", clvAt(0), line, clvDecMinus110, clvDecMinus110)
	closing := clvTwoWay(t, "book-dk", clvAt(time.Hour), line, clvDecMinus120, clvDecPlus100)

	home, err := EvaluateCLV(taken, closing, "sel-home")
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	away, err := EvaluateCLV(taken, closing, "sel-away")
	if err != nil {
		t.Fatalf("away: %v", err)
	}

	// The two fair distributions each sum to 1, so the probability CLV of the two
	// sides of a two-way market must sum to exactly zero.
	clvAssertNear(t, "home + away probability CLV", home.ProbabilityCLV+away.ProbabilityCLV, 0, clvTol)
	if !home.Beat || away.Beat {
		t.Errorf("beat flags = home %t, away %t; want true and false", home.Beat, away.Beat)
	}
}

// TestEvaluateCLVOnAThreeWayMarket exercises a market with more than two
// selections, using the standard European 1X2 decimal ladder.
func TestEvaluateCLVOnAThreeWayMarket(t *testing.T) {
	ids := []domain.SelectionID{"sel-home", "sel-draw", "sel-away"}
	taken := clvSnapshot(t, "mkt-epl-1x2", "book-dk", domain.NoLine(), clvAt(0), ids, 2.10, 3.40, 3.80)
	closing := clvSnapshot(t, "mkt-epl-1x2", "book-pinnacle", domain.NoLine(), clvAt(2*time.Hour), ids, 1.95, 3.50, 4.20)

	// The home price shortened from 2.10 to 1.95, so a home bet beat the close
	// and the away bet, which drifted from 3.80 to 4.20, did not.
	home, err := EvaluateCLV(taken, closing, "sel-home")
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	away, err := EvaluateCLV(taken, closing, "sel-away")
	if err != nil {
		t.Fatalf("away: %v", err)
	}
	if !home.Beat {
		t.Errorf("home CLV = %+.6f %%, want a beat", home.PercentCLV)
	}
	if away.Beat {
		t.Errorf("away CLV = %+.6f %%, want no beat", away.PercentCLV)
	}
	// Across a complete market the probability CLVs must sum to exactly zero,
	// because both distributions sum to 1.
	draw, err := EvaluateCLV(taken, closing, "sel-draw")
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	clvAssertNear(t, "sum of the three probability CLVs",
		home.ProbabilityCLV+draw.ProbabilityCLV+away.ProbabilityCLV, 0, clvTol)
}

// TestEvaluateCLVOnAMoneylineWithNoLine checks that an absent line is a
// first-class case and not an accident of the zero value.
func TestEvaluateCLVOnAMoneylineWithNoLine(t *testing.T) {
	taken := clvSnapshot(t, "mkt-nba-ml", "book-dk", domain.NoLine(), clvAt(0),
		[]domain.SelectionID{"sel-home", "sel-away"}, clvDecMinus110, clvDecMinus110)
	closing := clvSnapshot(t, "mkt-nba-ml", "book-dk", domain.NoLine(), clvAt(time.Hour),
		[]domain.SelectionID{"sel-home", "sel-away"}, clvDecMinus120, clvDecPlus100)

	got, err := EvaluateCLV(taken, closing, "sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLV: %v", err)
	}
	if got.Line.Present() || got.ClosingLine.Present() {
		t.Errorf("lines = %s and %s, want both absent", got.Line, got.ClosingLine)
	}
	clvAssertNear(t, "ProbabilityCLV", got.ProbabilityCLV, 1.0/46.0, clvTol)
}

// -----------------------------------------------------------------------------
// EvaluateCLV — the refusals
// -----------------------------------------------------------------------------

func TestEvaluateCLVRefusesUncomparablePairs(t *testing.T) {
	line := clvMustLine(t, -3.0)
	base := func() FairMarketSnapshot {
		return clvTwoWay(t, "book-dk", clvAt(0), line, clvDecMinus110, clvDecMinus110)
	}

	cases := []struct {
		name           string
		taken, closing FairMarketSnapshot
		selection      domain.SelectionID
		want           error
	}{
		{
			name: "taken snapshot is the zero value", taken: FairMarketSnapshot{}, closing: base(),
			selection: "sel-home", want: ErrCLVMissingIdentity,
		},
		{
			name: "closing snapshot is the zero value", taken: base(), closing: FairMarketSnapshot{},
			selection: "sel-home", want: ErrCLVMissingIdentity,
		},
		{
			name: "no selection", taken: base(), closing: base(), selection: "", want: ErrCLVMissingIdentity,
		},
		{
			name:  "different markets",
			taken: base(),
			closing: clvSnapshot(t, "mkt-other", "book-dk", line, clvAt(time.Hour),
				[]domain.SelectionID{"sel-home", "sel-away"}, clvDecMinus110, clvDecMinus110),
			selection: "sel-home", want: ErrCLVMarketMismatch,
		},
		{
			name: "selection absent from the closing snapshot", taken: base(),
			closing: clvSnapshot(t, "mkt-nba-spread", "book-dk", line, clvAt(time.Hour),
				[]domain.SelectionID{"sel-home", "sel-substitute"}, clvDecMinus110, clvDecMinus110),
			selection: "sel-away", want: ErrCLVSelectionAbsent,
		},
		{
			name: "selection absent from the taken snapshot",
			taken: clvSnapshot(t, "mkt-nba-spread", "book-dk", line, clvAt(0),
				[]domain.SelectionID{"sel-home", "sel-substitute"}, clvDecMinus110, clvDecMinus110),
			closing:   base(),
			selection: "sel-away", want: ErrCLVSelectionAbsent,
		},
		{
			name: "the outcome set changed while keeping its size", taken: base(),
			closing: clvSnapshot(t, "mkt-nba-spread", "book-dk", line, clvAt(time.Hour),
				[]domain.SelectionID{"sel-home", "sel-substitute"}, clvDecMinus110, clvDecMinus110),
			selection: "sel-home", want: ErrCLVOutcomeSetChanged,
		},
		{
			name: "a selection was added before the close", taken: base(),
			closing: clvSnapshot(t, "mkt-nba-spread", "book-dk", line, clvAt(time.Hour),
				[]domain.SelectionID{"sel-home", "sel-away", "sel-draw"}, 2.60, 3.20, 3.40),
			selection: "sel-home", want: ErrCLVOutcomeSetChanged,
		},
		{
			name: "the line moved", taken: base(),
			closing:   clvTwoWay(t, "book-dk", clvAt(time.Hour), clvMustLine(t, -3.5), clvDecMinus110, clvDecMinus110),
			selection: "sel-home", want: ErrCLVLineMoved,
		},
		{
			// A pick'em of 0.0 is a real line; an absent line is not the same
			// thing, and domain.Line is the type that keeps them apart.
			name:  "a pick'em is not an absent line",
			taken: clvTwoWay(t, "book-dk", clvAt(0), clvMustLine(t, 0), clvDecMinus110, clvDecMinus110),
			closing: clvTwoWay(t, "book-dk", clvAt(time.Hour), domain.NoLine(),
				clvDecMinus110, clvDecMinus110),
			selection: "sel-home", want: ErrCLVLineMoved,
		},
		{
			name:      "the close precedes the wager",
			taken:     clvTwoWay(t, "book-dk", clvAt(time.Hour), line, clvDecMinus110, clvDecMinus110),
			closing:   clvTwoWay(t, "book-dk", clvAt(0), line, clvDecMinus110, clvDecMinus110),
			selection: "sel-home", want: ErrCLVClosingBeforeTaken,
		},
		{
			name: "the taken fair probability has no price",
			taken: func() FairMarketSnapshot {
				s, err := NewFairMarketSnapshot(FairMarketSnapshotParams{
					Market: "mkt-nba-spread", Book: "book-dk", Line: line, ObservedAt: clvAt(0),
					Fair: []FairSelection{{Selection: "sel-home", Fair: 0}, {Selection: "sel-away", Fair: 1}},
				})
				if err != nil {
					t.Fatalf("degenerate snapshot: %v", err)
				}
				return s
			}(),
			closing:   base(),
			selection: "sel-home", want: ErrProbabilityNotPriceable,
		},
		{
			name:  "the closing fair probability has no price",
			taken: base(),
			closing: func() FairMarketSnapshot {
				s, err := NewFairMarketSnapshot(FairMarketSnapshotParams{
					Market: "mkt-nba-spread", Book: "book-dk", Line: line, ObservedAt: clvAt(time.Hour),
					Fair: []FairSelection{{Selection: "sel-home", Fair: 1}, {Selection: "sel-away", Fair: 0}},
				})
				if err != nil {
					t.Fatalf("degenerate snapshot: %v", err)
				}
				return s
			}(),
			selection: "sel-home", want: ErrProbabilityNotPriceable,
		},
		{
			// A fair probability of 1e-307 is priceable — 1/p is 1e307, a finite
			// decimal — but the ratio against a fair close of 2.0, times 100,
			// leaves the float64 range. No real market reaches here; the guard
			// exists because odds.Decimal carries no upper bound of its own.
			name: "the percentage overflows",
			taken: func() FairMarketSnapshot {
				s, err := NewFairMarketSnapshot(FairMarketSnapshotParams{
					Market: "mkt-nba-spread", Book: "book-dk", Line: line, ObservedAt: clvAt(0),
					Fair: []FairSelection{{Selection: "sel-home", Fair: 1e-307}, {Selection: "sel-away", Fair: 1}},
				})
				if err != nil {
					t.Fatalf("extreme snapshot: %v", err)
				}
				return s
			}(),
			closing:   base(),
			selection: "sel-home", want: ErrNotFinite,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := EvaluateCLV(c.taken, c.closing, c.selection)
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want one wrapping %v", err, c.want)
			}
			if !got.IsZero() {
				t.Errorf("a refused evaluation returned a populated result: %s", got)
			}
			if !strings.HasPrefix(err.Error(), "odds: ") {
				t.Errorf("error %q does not carry the package prefix", err)
			}
			if strings.Count(err.Error(), "odds: ") != 1 {
				t.Errorf("error %q repeats the package prefix", err)
			}
		})
	}
}

// TestEvaluateCLVAcceptsASimultaneousClose allows the wager struck at the
// closing instant: only a close strictly BEFORE the wager is a data error.
func TestEvaluateCLVAcceptsASimultaneousClose(t *testing.T) {
	line := clvMustLine(t, -3.0)
	at := clvAt(0)
	taken := clvTwoWay(t, "book-dk", at, line, clvDecMinus110, clvDecMinus110)
	closing := clvTwoWay(t, "book-dk", at, line, clvDecMinus120, clvDecPlus100)

	if _, err := EvaluateCLV(taken, closing, "sel-home"); err != nil {
		t.Fatalf("a close stamped at the same instant was refused: %v", err)
	}
}

// TestEvaluateCLVAcrossLineMove checks the acknowledged form: it computes, it
// stamps LineMoved, and it still enforces every other rule.
func TestEvaluateCLVAcrossLineMove(t *testing.T) {
	taken := clvTwoWay(t, "book-dk", clvAt(0), clvMustLine(t, -3.0), clvDecMinus110, clvDecMinus110)
	closing := clvTwoWay(t, "book-dk", clvAt(time.Hour), clvMustLine(t, -3.5), clvDecMinus120, clvDecPlus100)

	got, err := EvaluateCLVAcrossLineMove(taken, closing, "sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLVAcrossLineMove: %v", err)
	}
	if !got.LineMoved {
		t.Error("LineMoved not set on a comparison across a line move")
	}
	if got.Line.Equal(got.ClosingLine) {
		t.Errorf("lines = %s and %s, want them to differ", got.Line, got.ClosingLine)
	}
	clvAssertNear(t, "ProbabilityCLV", got.ProbabilityCLV, 1.0/46.0, clvTol)

	// The strict form is the one that refuses.
	if _, err := EvaluateCLV(taken, closing, "sel-home"); !errors.Is(err, ErrCLVLineMoved) {
		t.Fatalf("EvaluateCLV error = %v, want one wrapping ErrCLVLineMoved", err)
	}

	// Every other rule still applies to the acknowledged form.
	other := clvSnapshot(t, "mkt-other", "book-dk", clvMustLine(t, -3.5), clvAt(time.Hour),
		[]domain.SelectionID{"sel-home", "sel-away"}, clvDecMinus110, clvDecMinus110)
	if _, err := EvaluateCLVAcrossLineMove(taken, other, "sel-home"); !errors.Is(err, ErrCLVMarketMismatch) {
		t.Fatalf("error = %v, want one wrapping ErrCLVMarketMismatch", err)
	}
}

func TestCLVResultString(t *testing.T) {
	if got := (CLVResult{}).String(); got != "clv(<zero>)" {
		t.Errorf("zero String = %q", got)
	}
	taken := clvTwoWay(t, "book-dk", clvAt(0), clvMustLine(t, -3.5), clvDecMinus110, clvDecMinus110)
	closing := clvTwoWay(t, "book-dk", clvAt(time.Hour), clvMustLine(t, -3.5), clvDecMinus120, clvDecPlus100)
	r, err := EvaluateCLV(taken, closing, "sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLV: %v", err)
	}
	got := r.String()
	for _, want := range []string{"mkt-nba-spread", "sel-home", "-3.5", "beat=true", "moved=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("String = %q, missing %q", got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// Aggregation
// -----------------------------------------------------------------------------

// clvResultFor builds one evaluated result by moving the home price from -110 to
// the given close.
func clvResultFor(t *testing.T, market domain.MarketID, homeClose, awayClose float64) CLVResult {
	t.Helper()
	line := clvMustLine(t, -3.5)
	taken := clvSnapshot(t, market, "book-dk", line, clvAt(0),
		[]domain.SelectionID{"sel-home", "sel-away"}, clvDecMinus110, clvDecMinus110)
	closing := clvSnapshot(t, market, "book-pinnacle", line, clvAt(time.Hour),
		[]domain.SelectionID{"sel-home", "sel-away"}, homeClose, awayClose)
	r, err := EvaluateCLV(taken, closing, "sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLV: %v", err)
	}
	return r
}

// TestAggregateCLV walks the whole convention in one table: one winner, one
// exact par, one loser, one void and one line-moved sample.
//
// All three counted samples were taken at a fair 2.0, so the winner scores
// (2/(23/12) − 1) = +1/23 and the loser (2/(23/11) − 1) = −1/23; they cancel and
// the mean is zero. That cancellation is a property of this fixture, not of the
// percentage measure — see TestPercentCLVSwapIdentity for why the measure is not
// antisymmetric in general — so the expected mean is derived from the three
// samples rather than asserted to be zero on principle.
func TestAggregateCLV(t *testing.T) {
	winner := clvResultFor(t, "mkt-1", clvDecMinus120, clvDecPlus100)
	par := clvResultFor(t, "mkt-2", clvDecMinus105, clvDecMinus105)
	loser := clvResultFor(t, "mkt-3", clvDecPlus100, clvDecMinus120)

	movedTaken := clvTwoWay(t, "book-dk", clvAt(0), clvMustLine(t, -3.0), clvDecMinus110, clvDecMinus110)
	movedClosing := clvTwoWay(t, "book-dk", clvAt(time.Hour), clvMustLine(t, -3.5), clvDecMinus120, clvDecPlus100)
	moved, err := EvaluateCLVAcrossLineMove(movedTaken, movedClosing, "sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLVAcrossLineMove: %v", err)
	}

	samples := []CLVSample{
		{Result: winner},
		{Result: par},
		{Result: loser},
		{Result: winner, Void: true},
		{Result: moved},
	}

	agg, err := AggregateCLV(samples)
	if err != nil {
		t.Fatalf("AggregateCLV: %v", err)
	}

	if agg.Samples != 5 {
		t.Errorf("Samples = %d, want 5", agg.Samples)
	}
	if agg.Counted != 3 {
		t.Errorf("Counted = %d, want 3", agg.Counted)
	}
	if agg.VoidExcluded != 1 {
		t.Errorf("VoidExcluded = %d, want 1", agg.VoidExcluded)
	}
	if agg.LineMovedExcluded != 1 {
		t.Errorf("LineMovedExcluded = %d, want 1", agg.LineMovedExcluded)
	}
	if agg.BeatCount != 1 {
		t.Errorf("BeatCount = %d, want 1", agg.BeatCount)
	}
	clvAssertNear(t, "BeatRate", agg.BeatRate, 1.0/3.0, clvTol)

	// The two probability CLVs are exact mirrors (+1/46 and −1/46) and the par
	// sample is exactly zero, so the mean probability CLV is zero.
	clvAssertNear(t, "MeanProbabilityCLV", agg.MeanProbabilityCLV, 0, clvTol)
	wantPercent := (winner.PercentCLV + par.PercentCLV + loser.PercentCLV) / 3
	clvAssertNear(t, "MeanPercentCLV", agg.MeanPercentCLV, wantPercent, clvTol)

	// A void sample must not have leaked into any statistic: had it counted, the
	// beat rate would be 1/2 and the mean would be positive.
	if agg.BeatRate > 0.5 {
		t.Errorf("BeatRate = %v; a voided wager appears to have been counted", agg.BeatRate)
	}
}

func TestAggregateCLVSingleSampleIsItself(t *testing.T) {
	one := clvResultFor(t, "mkt-1", clvDecMinus120, clvDecPlus100)
	agg, err := AggregateCLV([]CLVSample{{Result: one}})
	if err != nil {
		t.Fatalf("AggregateCLV: %v", err)
	}
	if agg.MeanProbabilityCLV != one.ProbabilityCLV || agg.MeanPercentCLV != one.PercentCLV {
		// Exact equality is correct here: the mean of one value is that value
		// divided by 1.0, which IEEE-754 guarantees is the value itself.
		t.Errorf("mean of one sample = (%v, %v), want (%v, %v)",
			agg.MeanProbabilityCLV, agg.MeanPercentCLV, one.ProbabilityCLV, one.PercentCLV)
	}
	if agg.BeatRate != 1 || agg.BeatCount != 1 || agg.Counted != 1 {
		t.Errorf("counts = %+v", agg)
	}
}

func TestAggregateCLVRefusals(t *testing.T) {
	good := clvResultFor(t, "mkt-1", clvDecMinus120, clvDecPlus100)

	movedTaken := clvTwoWay(t, "book-dk", clvAt(0), clvMustLine(t, -3.0), clvDecMinus110, clvDecMinus110)
	movedClosing := clvTwoWay(t, "book-dk", clvAt(time.Hour), clvMustLine(t, -3.5), clvDecMinus120, clvDecPlus100)
	moved, err := EvaluateCLVAcrossLineMove(movedTaken, movedClosing, "sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLVAcrossLineMove: %v", err)
	}

	notFinite := good
	notFinite.PercentCLV = math.Inf(1)
	notANumber := good
	notANumber.ProbabilityCLV = math.NaN()

	cases := []struct {
		name    string
		samples []CLVSample
		want    error
	}{
		{"nil slice", nil, ErrCLVNoSamples},
		{"empty slice", []CLVSample{}, ErrCLVNoSamples},
		{"everything void", []CLVSample{{Result: good, Void: true}, {Result: good, Void: true}}, ErrCLVNoSamples},
		{"everything line-moved", []CLVSample{{Result: moved}, {Result: moved}}, ErrCLVNoSamples},
		{"void and line-moved together", []CLVSample{{Result: good, Void: true}, {Result: moved}}, ErrCLVNoSamples},
		{"a result that never came from EvaluateCLV", []CLVSample{{Result: good}, {}}, ErrCLVMissingIdentity},
		{"a non-finite percentage", []CLVSample{{Result: notFinite}}, ErrNotFinite},
		{"a NaN probability", []CLVSample{{Result: notANumber}}, ErrNotFinite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agg, err := AggregateCLV(c.samples)
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want one wrapping %v", err, c.want)
			}
			if agg != (CLVAggregate{}) {
				t.Errorf("a failed aggregate returned %+v, want the zero value", agg)
			}
			if !strings.HasPrefix(err.Error(), "odds: ") {
				t.Errorf("error %q does not carry the package prefix", err)
			}
		})
	}
}

// TestAggregateCLVVoidBeatsLineMovedInTheAccounting pins which bucket a sample
// lands in when it qualifies for both, so the two counters cannot double-count.
func TestAggregateCLVVoidBeatsLineMovedInTheAccounting(t *testing.T) {
	movedTaken := clvTwoWay(t, "book-dk", clvAt(0), clvMustLine(t, -3.0), clvDecMinus110, clvDecMinus110)
	movedClosing := clvTwoWay(t, "book-dk", clvAt(time.Hour), clvMustLine(t, -3.5), clvDecMinus120, clvDecPlus100)
	moved, err := EvaluateCLVAcrossLineMove(movedTaken, movedClosing, "sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLVAcrossLineMove: %v", err)
	}
	good := clvResultFor(t, "mkt-1", clvDecMinus120, clvDecPlus100)

	agg, err := AggregateCLV([]CLVSample{{Result: moved, Void: true}, {Result: good}})
	if err != nil {
		t.Fatalf("AggregateCLV: %v", err)
	}
	if agg.VoidExcluded != 1 || agg.LineMovedExcluded != 0 {
		t.Errorf("void %d, line-moved %d; a sample must be counted in exactly one bucket",
			agg.VoidExcluded, agg.LineMovedExcluded)
	}
	if agg.Samples != agg.Counted+agg.VoidExcluded+agg.LineMovedExcluded {
		t.Errorf("the buckets do not partition the samples: %+v", agg)
	}
}

// -----------------------------------------------------------------------------
// Property tests
// -----------------------------------------------------------------------------
//
// CLAUDE.md §4 requires property-based tests on this package, so the invariants
// below are stated with pgregory.net/rapid rather than as fixed cases: rapid
// generates, biases toward the edges of each range, and shrinks a counterexample
// to its minimal form, which is what makes a failure readable. They assert what
// the worked cases cannot — that the two measures never disagree about
// direction, that swapping the arguments does what the algebra says, and that no
// valid input produces a non-finite result.
//
// The one exception is TestAggregateCLVIsOrderInvariant, which needs a fixed
// corpus permuted many ways rather than a fresh draw each time, and uses a
// seeded math/rand/v2 generator so the permutations reproduce exactly.

const (
	// clvMinDrawnFair and clvMaxDrawnFair bound the generated fair
	// probabilities away from 0 and 1. Both endpoints are valid probabilities
	// and neither is priceable, so drawing them would exercise
	// Probability.Decimal's rejection path rather than the CLV algebra these
	// properties are about; that path has its own cases above.
	clvMinDrawnFair = 1e-6
	clvMaxDrawnFair = 1 - 1e-6

	// clvMinDrawnDecimal and clvMaxDrawnDecimal bound the generated decimal
	// prices. The span covers every price a book posts short of a deep futures
	// longshot, and the lower bound is held clear of 1 for the reason argued at
	// TestPropertyPercentCLVSwapIdentity.
	clvMinDrawnDecimal = 1.05
	clvMaxDrawnDecimal = 51.0
)

// TestPropertyCLVMeasuresAgreeOnDirection is the invariant that makes the two
// measures interchangeable for the purpose of ranking: they are monotone
// transforms of the same comparison, so their signs can never differ. It also
// pins BeatTheClose to ProbabilityCLV and the reported magnitude to |PercentCLV|,
// and asserts that valid input never produces NaN or an infinity.
func TestPropertyCLVMeasuresAgreeOnDirection(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		taken := Probability(rapid.Float64Range(clvMinDrawnFair, clvMaxDrawnFair).Draw(rt, "taken"))
		closing := Probability(rapid.Float64Range(clvMinDrawnFair, clvMaxDrawnFair).Draw(rt, "closing"))

		probability, err := ProbabilityCLV(taken, closing)
		if err != nil {
			rt.Fatalf("ProbabilityCLV(%v, %v): %v", float64(taken), float64(closing), err)
		}
		takenPrice, err := taken.Decimal()
		if err != nil {
			rt.Fatalf("taken.Decimal(): %v", err)
		}
		closingPrice, err := closing.Decimal()
		if err != nil {
			rt.Fatalf("closing.Decimal(): %v", err)
		}
		percent, err := PercentCLV(takenPrice, closingPrice)
		if err != nil {
			rt.Fatalf("PercentCLV: %v", err)
		}

		if !clvFinite(probability) || !clvFinite(percent) {
			rt.Fatalf("non-finite result from valid input: %v and %v", probability, percent)
		}
		// Signs are compared only outside the tie band, where a flip would be
		// float noise rather than a disagreement about the direction of the move.
		if math.Abs(probability) > CLVTieBand && math.Signbit(probability) != math.Signbit(percent) {
			rt.Fatalf("taken %v, closing %v: probability CLV %v and percent CLV %v disagree on sign",
				float64(taken), float64(closing), probability, percent)
		}

		beat, magnitude, err := BeatTheClose(taken, closing)
		if err != nil {
			rt.Fatalf("BeatTheClose: %v", err)
		}
		if beat != (probability > CLVTieBand) {
			rt.Fatalf("taken %v, closing %v: beat = %t but probability CLV = %v",
				float64(taken), float64(closing), beat, probability)
		}
		if magnitude != math.Abs(percent) {
			rt.Fatalf("magnitude %v is not |percent CLV| %v", magnitude, math.Abs(percent))
		}
	})
}

// TestPropertyProbabilityCLVIsExactlyAntisymmetric asserts p_c − p_t == −(p_t −
// p_c) to the bit. IEEE-754 subtraction is correctly rounded and negation is
// exact, so the two must agree exactly; anything looser would hide a formula
// that is not a plain subtraction.
func TestPropertyProbabilityCLVIsExactlyAntisymmetric(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := Probability(rapid.Float64Range(0, 1).Draw(rt, "a"))
		b := Probability(rapid.Float64Range(0, 1).Draw(rt, "b"))

		forward, err := ProbabilityCLV(a, b)
		if err != nil {
			rt.Fatalf("forward: %v", err)
		}
		reverse, err := ProbabilityCLV(b, a)
		if err != nil {
			rt.Fatalf("reverse: %v", err)
		}
		if forward != -reverse {
			rt.Fatalf("a %v, b %v: %v is not the negation of %v",
				float64(a), float64(b), forward, reverse)
		}
	})
}

// TestPropertyPercentCLVSwapIdentity asserts the algebra of the percentage
// measure, and in doing so pins the fact that it is NOT antisymmetric — the
// property that catches anyone who assumes it is. With x = d_t/d_c − 1, swapping
// gives d_c/d_t − 1 = −x/(1+x). Taking a price 10% better than the close is not
// the mirror image of taking one 10% worse.
//
// The generated prices are bounded to [1.05, 51] rather than the full legal
// range because the assertion recomputes 1+x, and when d_t/d_c approaches zero
// that addition cancels catastrophically: x is then −1 plus something tiny, and
// recovering the ratio from it loses as many digits as the ratio is small. That
// is a property of the identity used to check the answer, not of the code under
// test. Over this range the ratio never falls below 1.05/51 ≈ 0.021, so the
// cancellation costs under two digits and the comparison stays far inside
// clvTol.
func TestPropertyPercentCLVSwapIdentity(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := Decimal(rapid.Float64Range(clvMinDrawnDecimal, clvMaxDrawnDecimal).Draw(rt, "taken"))
		b := Decimal(rapid.Float64Range(clvMinDrawnDecimal, clvMaxDrawnDecimal).Draw(rt, "closing"))

		forward, err := PercentCLV(a, b)
		if err != nil {
			rt.Fatalf("forward: %v", err)
		}
		reverse, err := PercentCLV(b, a)
		if err != nil {
			rt.Fatalf("reverse: %v", err)
		}

		x := forward / 100
		clvAssertNear(rt, "swapped percent CLV", reverse/100, -x/(1+x), clvTol)
	})
}

// TestPropertyPercentCLVIsStrictlyIncreasingInTheTakenPrice: for a fixed close, a
// longer price taken is always a better result. A formula with a sign error or an
// inverted ratio fails this immediately.
//
// The two taken prices are separated by at least 0.1% relative, which is four
// orders of magnitude above float64 noise, so the inequality is strict rather
// than approximate.
func TestPropertyPercentCLVIsStrictlyIncreasingInTheTakenPrice(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		closing := Decimal(rapid.Float64Range(clvMinDrawnDecimal, clvMaxDrawnDecimal).Draw(rt, "closing"))
		lower := Decimal(rapid.Float64Range(clvMinDrawnDecimal, clvMaxDrawnDecimal).Draw(rt, "lower"))
		grow := rapid.Float64Range(1e-3, 1).Draw(rt, "grow")
		higher := Decimal(float64(lower) * (1 + grow))

		a, err := PercentCLV(lower, closing)
		if err != nil {
			rt.Fatalf("lower: %v", err)
		}
		b, err := PercentCLV(higher, closing)
		if err != nil {
			rt.Fatalf("higher: %v", err)
		}
		if b <= a {
			rt.Fatalf("closing %v: taking %v scored %v but the longer %v scored %v",
				float64(closing), float64(lower), a, float64(higher), b)
		}
	})
}

// TestPropertyEvaluateCLVFieldsAreSelfConsistent asserts that the record's
// scalar fields are exactly what recomputing from its own reported prices gives.
// Exact equality is the point: these are two spellings of one deterministic
// computation, and a near-miss would mean the record and the number disagree
// about which prices were compared.
func TestPropertyEvaluateCLVFieldsAreSelfConsistent(t *testing.T) {
	line := clvMustLine(t, -3.5)
	ids := []domain.SelectionID{"sel-home", "sel-away"}

	rapid.Check(t, func(rt *rapid.T) {
		price := func(label string) float64 {
			return rapid.Float64Range(clvMinDrawnDecimal, clvMaxDrawnDecimal).Draw(rt, label)
		}
		taken := clvSnapshot(t, "mkt-1", "book-dk", line, clvAt(0), ids,
			price("homeTaken"), price("awayTaken"))
		closing := clvSnapshot(t, "mkt-1", "book-pin", line, clvAt(time.Hour), ids,
			price("homeClose"), price("awayClose"))

		got, err := EvaluateCLV(taken, closing, "sel-home")
		if err != nil {
			rt.Fatalf("EvaluateCLV: %v", err)
		}

		wantProbability, err := ProbabilityCLV(got.TakenFair, got.ClosingFair)
		if err != nil {
			rt.Fatalf("ProbabilityCLV: %v", err)
		}
		if got.ProbabilityCLV != wantProbability {
			rt.Fatalf("ProbabilityCLV %v does not match its own fields %v",
				got.ProbabilityCLV, wantProbability)
		}
		wantPercent, err := PercentCLV(got.TakenPrice, got.ClosingPrice)
		if err != nil {
			rt.Fatalf("PercentCLV: %v", err)
		}
		if got.PercentCLV != wantPercent {
			rt.Fatalf("PercentCLV %v does not match its own prices %v", got.PercentCLV, wantPercent)
		}
		if got.Magnitude != math.Abs(got.PercentCLV) {
			rt.Fatalf("Magnitude %v is not |PercentCLV| %v", got.Magnitude, got.PercentCLV)
		}
		if got.Beat != (got.ProbabilityCLV > CLVTieBand) {
			rt.Fatalf("Beat %t contradicts ProbabilityCLV %v", got.Beat, got.ProbabilityCLV)
		}
		// 1/p and its price must be reciprocals, which is what lets the phase-12
		// SQL job compute either form and get the same answer.
		clvAssertNear(rt, "1/TakenFair", 1/float64(got.TakenFair), float64(got.TakenPrice), clvTol)
		clvAssertNear(rt, "1/ClosingFair", 1/float64(got.ClosingFair), float64(got.ClosingPrice), clvTol)
	})
}

// clvRand returns a deterministic generator for the one test below that permutes
// a fixed corpus. PCG with two literal seeds reproduces across runs,
// architectures and Go versions.
func clvRand() *rand.Rand { return rand.New(rand.NewPCG(0x5150_0DD5, 0xC10_5EED)) }

// TestAggregateCLVIsOrderInvariant checks the property the phase-12 Flink job
// depends on: a stream engine will not present the samples in any particular
// order, so the aggregate must not depend on one. Compensated summation makes
// the residual far smaller than clvAggTol, which is the tolerance the two
// implementations are compared at.
func TestAggregateCLVIsOrderInvariant(t *testing.T) {
	r := clvRand()
	line := clvMustLine(t, -3.5)
	ids := []domain.SelectionID{"sel-home", "sel-away"}

	samples := make([]CLVSample, 0, 500)
	for i := 0; i < 500; i++ {
		taken := clvSnapshot(t, "mkt-1", "book-dk", line, clvAt(0), ids,
			1.05+r.Float64()*8, 1.05+r.Float64()*8)
		closing := clvSnapshot(t, "mkt-1", "book-pin", line, clvAt(time.Hour), ids,
			1.05+r.Float64()*8, 1.05+r.Float64()*8)
		got, err := EvaluateCLV(taken, closing, "sel-home")
		if err != nil {
			t.Fatalf("EvaluateCLV: %v", err)
		}
		samples = append(samples, CLVSample{Result: got})
	}

	first, err := AggregateCLV(samples)
	if err != nil {
		t.Fatalf("AggregateCLV: %v", err)
	}
	for trial := 0; trial < 20; trial++ {
		shuffled := make([]CLVSample, len(samples))
		copy(shuffled, samples)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		got, err := AggregateCLV(shuffled)
		if err != nil {
			t.Fatalf("AggregateCLV(shuffled): %v", err)
		}
		clvAssertNear(t, "MeanProbabilityCLV under reordering", got.MeanProbabilityCLV, first.MeanProbabilityCLV, clvAggTol)
		clvAssertNear(t, "MeanPercentCLV under reordering", got.MeanPercentCLV, first.MeanPercentCLV, clvAggTol)
		if got.BeatCount != first.BeatCount || got.Counted != first.Counted {
			t.Fatalf("counts changed under reordering: %+v against %+v", got, first)
		}
	}
}

// -----------------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------------

// TestCLVValidateFairAgreesWithProbabilityValidate pins the one place this file
// deliberately repeats logic that lives elsewhere: clvValidateFair re-implements
// the two bounds tests so that its message can name the selection, and this is
// the assertion that stops the two drifting apart.
func TestCLVValidateFairAgreesWithProbabilityValidate(t *testing.T) {
	values := []float64{
		math.NaN(), math.Inf(1), math.Inf(-1),
		-1, -1e-300, 0, 1e-300, 0.25, 0.5, 12.0 / 23.0, 1 - 1e-16, 1, 1 + 1e-16, 1.5, 1e308,
	}
	for _, v := range values {
		p := Probability(v)
		canonical := p.Validate()
		mine := clvValidateFair(p, "sel-home")
		if (canonical == nil) != (mine == nil) {
			t.Fatalf("%v: Probability.Validate = %v but clvValidateFair = %v", v, canonical, mine)
		}
		if canonical == nil {
			continue
		}
		for _, sentinel := range []error{ErrNotFinite, ErrProbabilityOutOfRange} {
			if errors.Is(canonical, sentinel) != errors.Is(mine, sentinel) {
				t.Errorf("%v: the two disagree about %v", v, sentinel)
			}
		}
		if !strings.Contains(mine.Error(), "sel-home") {
			t.Errorf("%v: %q does not name the selection", v, mine)
		}
	}
}

// TestCLVPriceErrPreservesTheSentinel covers both branches of the unwrapping,
// including the bare-error fallback that the public API cannot reach.
func TestCLVPriceErrPreservesTheSentinel(t *testing.T) {
	_, wrapped := Probability(1).Decimal()
	if wrapped == nil {
		t.Fatal("Probability(1).Decimal() unexpectedly succeeded")
	}
	got := clvPriceErr("taken", "mkt-1", "sel-home", 1, wrapped)
	if !errors.Is(got, ErrProbabilityNotPriceable) {
		t.Errorf("wrapped error lost its sentinel: %v", got)
	}
	if strings.Count(got.Error(), "odds: ") != 1 {
		t.Errorf("error %q repeats the package prefix", got)
	}

	bare := errors.New("no sentinel underneath")
	if got := clvPriceErr("closing", "mkt-1", "sel-home", 0.5, bare); !errors.Is(got, bare) {
		t.Errorf("a bare error was not preserved: %v", got)
	}
}

func TestCLVFinite(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want bool
	}{
		{0, true}, {-1.5, true}, {math.MaxFloat64, true},
		{math.NaN(), false}, {math.Inf(1), false}, {math.Inf(-1), false},
	} {
		if got := clvFinite(c.in); got != c.want {
			t.Errorf("clvFinite(%v) = %t, want %t", c.in, got, c.want)
		}
	}
}

// TestCLVSumCompensates is the case that separates compensated from naive
// summation: 1e16 + 1 − 1e16 is exactly 1, but adding 1 to 1e16 in float64 is a
// no-op, so a naive accumulator returns 0.
func TestCLVSumCompensates(t *testing.T) {
	var s clvSum
	if got := s.total(); got != 0 {
		t.Errorf("the zero accumulator totals %v, want 0", got)
	}

	var naive float64
	for _, x := range []float64{1e16, 1, -1e16} {
		s.add(x)
		naive += x
	}
	if got := s.total(); got != 1 {
		t.Errorf("compensated total = %v, want exactly 1", got)
	}
	if naive == 1 {
		t.Error("naive summation happened to be exact; the test no longer proves anything")
	}

	// The other branch of the magnitude test: a large term arriving after a
	// small running sum.
	var t2 clvSum
	for _, x := range []float64{1, 1e16, -1e16} {
		t2.add(x)
	}
	if got := t2.total(); got != 1 {
		t.Errorf("compensated total = %v, want exactly 1", got)
	}
}

// TestCLVSameOutcomeSet covers the helper's two negative branches directly, so
// that neither is reachable only through a longer path.
func TestCLVSameOutcomeSet(t *testing.T) {
	line := clvMustLine(t, -3.5)
	two := clvSnapshot(t, "mkt-1", "book-dk", line, clvAt(0),
		[]domain.SelectionID{"sel-home", "sel-away"}, clvDecMinus110, clvDecMinus110)
	three := clvSnapshot(t, "mkt-1", "book-dk", domain.NoLine(), clvAt(0),
		[]domain.SelectionID{"sel-home", "sel-draw", "sel-away"}, 2.60, 3.20, 3.40)
	swapped := clvSnapshot(t, "mkt-1", "book-dk", line, clvAt(0),
		[]domain.SelectionID{"sel-away", "sel-home"}, clvDecMinus110, clvDecMinus110)
	different := clvSnapshot(t, "mkt-1", "book-dk", line, clvAt(0),
		[]domain.SelectionID{"sel-home", "sel-substitute"}, clvDecMinus110, clvDecMinus110)

	if !clvSameOutcomeSet(two, two) {
		t.Error("a snapshot does not match itself")
	}
	if !clvSameOutcomeSet(two, swapped) {
		t.Error("set comparison is sensitive to ordering; it must not be")
	}
	if clvSameOutcomeSet(two, three) {
		t.Error("sets of different sizes matched")
	}
	if clvSameOutcomeSet(two, different) {
		t.Error("sets of the same size but different membership matched")
	}
}

// -----------------------------------------------------------------------------
// Composition with the devig implementation in this package
// -----------------------------------------------------------------------------

// TestEveryDevigMethodProducesAConstructibleSnapshot is the integration point
// this file's whole design depends on: the output of the package's real
// devigging must pass the sum-to-one gate that NewFairMarketSnapshot applies.
//
// If it ever does not, one of the two tolerances moved and CLV silently stops
// being computable — a failure that would otherwise surface as an unexplained
// error in the settle service rather than here. The four methods disagree
// meaningfully on longshots by design, so this asserts constructibility for all
// four and pins the numbers only for the multiplicative method, whose answers
// can be written down as exact fractions.
func TestEveryDevigMethodProducesAConstructibleSnapshot(t *testing.T) {
	// A -110/-110 two-way market and a 2.10/3.40/3.80 three-way market: the
	// standard rungs of the American and European ladders.
	markets := [][]float64{
		{clvDecMinus110, clvDecMinus110},
		{2.10, 3.40, 3.80},
		{clvDecMinus120, clvDecPlus100},
	}
	ids := []domain.SelectionID{"sel-a", "sel-b", "sel-c"}

	for _, method := range DevigMethods() {
		for i, prices := range markets {
			t.Run(method.String()+"/"+string(rune('A'+i)), func(t *testing.T) {
				decimals := make([]Decimal, len(prices))
				for j, d := range prices {
					decimals[j] = Decimal(d)
				}
				result, err := DevigPrices(method, decimals)
				if err != nil {
					t.Fatalf("DevigPrices(%s): %v", method, err)
				}

				fair := make([]FairSelection, len(result.Probabilities))
				for j, p := range result.Probabilities {
					fair[j] = FairSelection{Selection: ids[j], Fair: p}
				}
				if _, err := NewFairMarketSnapshot(FairMarketSnapshotParams{
					Market: "mkt-1", Book: "book-1", ObservedAt: clvAt(0), Fair: fair,
				}); err != nil {
					t.Fatalf("%s devig output failed the CLV sum-to-one gate: %v", method, err)
				}
			})
		}
	}
}

// TestCLVEndToEndThroughTheRealDevig walks the whole path the settle service
// will: raw quoted prices at both instants, devigged by the package's own
// multiplicative method, into a CLV number that matches the hand arithmetic in
// TestEvaluateCLVWorkedCases.
func TestCLVEndToEndThroughTheRealDevig(t *testing.T) {
	line := clvMustLine(t, -3.5)
	ids := []domain.SelectionID{"sel-home", "sel-away"}

	snapshot := func(book domain.BookID, at time.Time, prices ...float64) FairMarketSnapshot {
		t.Helper()
		decimals := make([]Decimal, len(prices))
		for i, d := range prices {
			decimals[i] = Decimal(d)
		}
		result, err := DevigPrices(MethodMultiplicative, decimals)
		if err != nil {
			t.Fatalf("DevigPrices: %v", err)
		}
		fair := make([]FairSelection, len(result.Probabilities))
		for i, p := range result.Probabilities {
			fair[i] = FairSelection{Selection: ids[i], Fair: p}
		}
		snap, err := NewFairMarketSnapshot(FairMarketSnapshotParams{
			Market: "mkt-nba-spread", Book: book, Line: line, ObservedAt: at, Fair: fair,
		})
		if err != nil {
			t.Fatalf("NewFairMarketSnapshot: %v", err)
		}
		return snap
	}

	// Struck at -110/-110, closed at -120/+100.
	got, err := EvaluateCLV(
		snapshot("book-dk", clvAt(0), clvDecMinus110, clvDecMinus110),
		snapshot("book-pinnacle", clvAt(3*time.Hour), clvDecMinus120, clvDecPlus100),
		"sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLV: %v", err)
	}
	clvAssertNear(t, "ProbabilityCLV", got.ProbabilityCLV, 1.0/46.0, clvTol)
	clvAssertNear(t, "PercentCLV", got.PercentCLV, 100.0/23.0, clvTol)
	if !got.Beat {
		t.Error("Beat = false on a price that beat the close")
	}

	// The same wager scored against a close that only tightened the margin: the
	// fair line never moved, so CLV must be exactly zero even though the raw
	// closing price of -105 is visibly better than the -110 that was taken.
	flat, err := EvaluateCLV(
		snapshot("book-dk", clvAt(0), clvDecMinus110, clvDecMinus110),
		snapshot("book-dk", clvAt(3*time.Hour), clvDecMinus105, clvDecMinus105),
		"sel-home")
	if err != nil {
		t.Fatalf("EvaluateCLV: %v", err)
	}
	if flat.ProbabilityCLV != 0 || flat.PercentCLV != 0 {
		t.Errorf("a pure margin change scored (%v, %v), want exactly zero on both measures",
			flat.ProbabilityCLV, flat.PercentCLV)
	}
	if flat.Beat {
		t.Error("Beat = true on a line that did not move")
	}

	// And the number the naive raw comparison would have produced, recorded so
	// that the size of the error this design prevents is on the page.
	naive, err := PercentCLV(Decimal(clvDecMinus110), Decimal(clvDecMinus105))
	if err != nil {
		t.Fatalf("PercentCLV: %v", err)
	}
	clvAssertNear(t, "naive raw-price CLV", naive, 100*(441.0/451.0-1), clvTol)
	if naive > -2 {
		t.Errorf("naive raw-price CLV = %v; the worked example in clv.go claims about -2.22%%", naive)
	}
}
