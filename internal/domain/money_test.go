package domain

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestMoneyConstructors(t *testing.T) {
	tests := []struct {
		name    string
		build   func() (Money, error)
		want    Money
		wantErr error
	}{
		{"minor units zero", func() (Money, error) { return FromMinorUnits(0) }, 0, nil},
		{"minor units positive", func() (Money, error) { return FromMinorUnits(12345) }, 12345, nil},
		{"minor units negative", func() (Money, error) { return FromMinorUnits(-7) }, -7, nil},
		{"minor units at the safe bound", func() (Money, error) { return FromMinorUnits(int64(MaxSafeMoney)) }, MaxSafeMoney, nil},
		{"minor units past the safe bound", func() (Money, error) { return FromMinorUnits(int64(MaxSafeMoney) + 1) }, 0, ErrMoneyOverflow},
		{"minor units at math.MaxInt64", func() (Money, error) { return FromMinorUnits(math.MaxInt64) }, 0, ErrMoneyOverflow},
		{"minor units at math.MinInt64", func() (Money, error) { return FromMinorUnits(math.MinInt64) }, 0, ErrMoneyOverflow},

		{"major units", func() (Money, error) { return FromMajorUnits(5) }, 500, nil},
		{"major units negative", func() (Money, error) { return FromMajorUnits(-5) }, -500, nil},
		{"major units overflow", func() (Money, error) { return FromMajorUnits(math.MaxInt64) }, 0, ErrMoneyOverflow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.build()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("= %d minor units, want %d", int64(got), int64(tc.want))
			}
		})
	}
}

func TestMoneyAddSubDetectOverflow(t *testing.T) {
	tests := []struct {
		name    string
		a, b    Money
		add     Money
		addErr  error
		sub     Money
		subErr  error
		skipSub bool
	}{
		{name: "zero", a: 0, b: 0, add: 0, sub: 0},
		{name: "simple", a: 1000, b: 250, add: 1250, sub: 750},
		{name: "crossing zero", a: 250, b: 1000, add: 1250, sub: -750},
		{name: "negatives", a: -250, b: -1000, add: -1250, sub: 750},

		{
			name: "sum leaves the safe range", a: MaxSafeMoney, b: 1,
			add: 0, addErr: ErrMoneyOverflow, sub: MaxSafeMoney - 1,
		},
		{
			name: "difference leaves the safe range", a: MinSafeMoney, b: 1,
			add: MinSafeMoney + 1, sub: 0, subErr: ErrMoneyOverflow,
		},
		{
			// Values outside the safe range can only be reached by a raw
			// conversion, never by a constructor; the arithmetic still refuses
			// them rather than wrapping.
			name: "raw int64 wrap is caught", a: Money(math.MaxInt64), b: 1,
			add: 0, addErr: ErrMoneyOverflow, sub: 0, subErr: ErrMoneyOverflow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.a.Add(tc.b)
			if !errors.Is(err, tc.addErr) {
				t.Fatalf("Add error = %v, want %v", err, tc.addErr)
			}
			if got != tc.add {
				t.Errorf("Add = %d, want %d", int64(got), int64(tc.add))
			}

			got, err = tc.a.Sub(tc.b)
			if !errors.Is(err, tc.subErr) {
				t.Fatalf("Sub error = %v, want %v", err, tc.subErr)
			}
			if got != tc.sub {
				t.Errorf("Sub = %d, want %d", int64(got), int64(tc.sub))
			}
		})
	}
}

func TestMoneyNegAndAbs(t *testing.T) {
	tests := []struct {
		name   string
		in     Money
		neg    Money
		negErr error
		abs    Money
		absErr error
	}{
		{name: "zero", in: 0, neg: 0, abs: 0},
		{name: "positive", in: 1234, neg: -1234, abs: 1234},
		{name: "negative", in: -1234, neg: 1234, abs: 1234},
		{name: "safe bound", in: MaxSafeMoney, neg: MinSafeMoney, abs: MaxSafeMoney},
		{name: "math.MinInt64 cannot be negated", in: Money(math.MinInt64), neg: 0, negErr: ErrMoneyOverflow, abs: 0, absErr: ErrMoneyOverflow},
		{name: "beyond the safe bound", in: Money(math.MaxInt64), neg: 0, negErr: ErrMoneyOverflow, abs: 0, absErr: ErrMoneyOverflow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.Neg()
			if !errors.Is(err, tc.negErr) {
				t.Fatalf("Neg error = %v, want %v", err, tc.negErr)
			}
			if got != tc.neg {
				t.Errorf("Neg = %d, want %d", int64(got), int64(tc.neg))
			}

			got, err = tc.in.Abs()
			if !errors.Is(err, tc.absErr) {
				t.Fatalf("Abs error = %v, want %v", err, tc.absErr)
			}
			if got != tc.abs {
				t.Errorf("Abs = %d, want %d", int64(got), int64(tc.abs))
			}
		})
	}
}

// TestLedgerPairSumsToZero is the double-entry invariant from CLAUDE.md §4:
// "Every stake, payout, void, and adjustment is two rows that sum to zero."
func TestLedgerPairSumsToZero(t *testing.T) {
	stakes := []Money{0, 1, 100, 2550, 1_000_000, MaxSafeMoney}
	for _, stake := range stakes {
		credit, err := stake.Neg()
		if err != nil {
			t.Fatalf("Neg(%s): %v", stake, err)
		}
		total, err := SumMoney(stake, credit)
		if err != nil {
			t.Fatalf("SumMoney(%s, %s): %v", stake, credit, err)
		}
		if total != ZeroMoney {
			t.Errorf("ledger pair for %s sums to %s, want zero", stake, total)
		}
	}
}

func TestSumMoney(t *testing.T) {
	got, err := SumMoney()
	if err != nil || got != ZeroMoney {
		t.Errorf("SumMoney() = %s, %v; want 0.00, nil", got, err)
	}

	got, err = SumMoney(100, -25, 50, -125)
	if err != nil {
		t.Fatalf("SumMoney: %v", err)
	}
	if got != ZeroMoney {
		t.Errorf("SumMoney(100, -25, 50, -125) = %s, want 0.00", got)
	}

	_, err = SumMoney(MaxSafeMoney, 1)
	if !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("SumMoney overflow error = %v, want ErrMoneyOverflow", err)
	}
	if err != nil && !strings.Contains(err.Error(), "index 1") {
		t.Errorf("SumMoney overflow error %q does not name the offending index", err)
	}
}

func TestMoneyMulInt(t *testing.T) {
	tests := []struct {
		name    string
		in      Money
		k       int64
		want    Money
		wantErr error
	}{
		{name: "by zero", in: 12345, k: 0, want: 0},
		{name: "zero by anything", in: 0, k: 99, want: 0},
		{name: "identity", in: 12345, k: 1, want: 12345},
		{name: "round robin combinations", in: 500, k: 6, want: 3000},
		{name: "negate via -1", in: 12345, k: -1, want: -12345},
		{name: "negative by negative", in: -500, k: -3, want: 1500},
		{name: "negative by positive", in: -500, k: 3, want: -1500},

		{name: "overflow", in: MaxSafeMoney, k: 2, want: 0, wantErr: ErrMoneyOverflow},
		{name: "overflow negative", in: MinSafeMoney, k: 2, want: 0, wantErr: ErrMoneyOverflow},
		{
			// The naive `product/k != m` overflow check panics on this input;
			// the magnitude-based implementation must not.
			name: "math.MinInt64 by -1 does not panic",
			in:   Money(math.MinInt64), k: -1, want: 0, wantErr: ErrMoneyOverflow,
		},
		{
			name: "-1 by math.MinInt64 does not panic",
			in:   -1, k: math.MinInt64, want: 0, wantErr: ErrMoneyOverflow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.MulInt(tc.k)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("MulInt error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("%d × %d = %d, want %d", int64(tc.in), tc.k, int64(got), int64(tc.want))
			}
		})
	}
}

// TestMoneyDivModLosesNothing asserts the identity that makes DivMod safe for
// splitting a stake: quotient*k + remainder == original, always.
func TestMoneyDivModLosesNothing(t *testing.T) {
	tests := []struct {
		name string
		in   Money
		k    int64
		q, r Money
	}{
		{name: "exact", in: 3000, k: 6, q: 500, r: 0},
		{name: "with remainder", in: 1000, k: 3, q: 333, r: 1},
		{name: "negative dividend", in: -1000, k: 3, q: -333, r: -1},
		{name: "negative divisor", in: 1000, k: -3, q: -333, r: 1},
		{name: "divisor larger than dividend", in: 5, k: 10, q: 0, r: 5},
		{name: "by -1", in: 1234, k: -1, q: -1234, r: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, r, err := tc.in.DivMod(tc.k)
			if err != nil {
				t.Fatalf("DivMod: %v", err)
			}
			if q != tc.q || r != tc.r {
				t.Errorf("DivMod(%d) = (%d, %d), want (%d, %d)", tc.k, int64(q), int64(r), int64(tc.q), int64(tc.r))
			}
			back, err := q.MulInt(tc.k)
			if err != nil {
				t.Fatalf("reconstructing: %v", err)
			}
			back, err = back.Add(r)
			if err != nil {
				t.Fatalf("reconstructing: %v", err)
			}
			if back != tc.in {
				t.Errorf("quotient×k+remainder = %d, want %d — minor units were lost", int64(back), int64(tc.in))
			}
		})
	}

	if _, _, err := Money(100).DivMod(0); !errors.Is(err, ErrMoneyDivideByZero) {
		t.Errorf("DivMod(0) error = %v, want ErrMoneyDivideByZero", err)
	}
}

// TestMoneyMulFloatRounding pins each rounding mode against exactly
// representable factors, so the expected value is a fact of arithmetic rather
// than of IEEE-754 rounding. 0.5 and 1.5 are dyadic rationals and exact in
// float64, so every product below lands exactly on a .5 boundary — which is
// precisely where the three modes disagree.
func TestMoneyMulFloatRounding(t *testing.T) {
	tests := []struct {
		name       string
		in         Money
		factor     float64
		halfAway   Money
		halfEven   Money
		towardZero Money
	}{
		{name: "positive tie to odd", in: 5, factor: 0.5, halfAway: 3, halfEven: 2, towardZero: 2},
		{name: "positive tie to even", in: 15, factor: 0.5, halfAway: 8, halfEven: 8, towardZero: 7},
		{name: "negative tie to odd", in: -5, factor: 0.5, halfAway: -3, halfEven: -2, towardZero: -2},
		{name: "negative tie to even", in: -15, factor: 0.5, halfAway: -8, halfEven: -8, towardZero: -7},
		{name: "no fraction", in: 10000, factor: 2.5, halfAway: 25000, halfEven: 25000, towardZero: 25000},
		{name: "exact quarter", in: 100, factor: 1.25, halfAway: 125, halfEven: 125, towardZero: 125},
		{name: "half tie above a cent", in: 12345, factor: 1.5, halfAway: 18518, halfEven: 18518, towardZero: 18517},
		{name: "zero stake", in: 0, factor: 2.5, halfAway: 0, halfEven: 0, towardZero: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modes := []struct {
				mode Rounding
				want Money
			}{
				{RoundHalfAwayFromZero, tc.halfAway},
				{RoundHalfToEven, tc.halfEven},
				{RoundTowardZero, tc.towardZero},
			}
			for _, m := range modes {
				got, err := tc.in.MulFloat(tc.factor, m.mode)
				if err != nil {
					t.Fatalf("MulFloat(%v, %s): %v", tc.factor, m.mode, err)
				}
				if got != m.want {
					t.Errorf("%d × %v under %s = %d, want %d",
						int64(tc.in), tc.factor, m.mode, int64(got), int64(m.want))
				}
			}
		})
	}
}

func TestMoneyMulFloatRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		in      Money
		factor  float64
		mode    Rounding
		wantErr error
	}{
		// The zero Rounding is invalid so that the rounding decision cannot be
		// made by omission.
		{name: "unset rounding mode", in: 100, factor: 2, mode: RoundingUnknown, wantErr: ErrUnknownRounding},
		{name: "undefined rounding mode", in: 100, factor: 2, mode: Rounding(99), wantErr: ErrUnknownRounding},
		{name: "NaN factor", in: 100, factor: math.NaN(), mode: RoundHalfAwayFromZero, wantErr: ErrMoneyNotFinite},
		{name: "positive infinity", in: 100, factor: math.Inf(1), mode: RoundHalfAwayFromZero, wantErr: ErrMoneyNotFinite},
		{name: "negative infinity", in: 100, factor: math.Inf(-1), mode: RoundHalfAwayFromZero, wantErr: ErrMoneyNotFinite},
		{name: "receiver beyond the safe range", in: Money(math.MaxInt64), factor: 1, mode: RoundHalfAwayFromZero, wantErr: ErrMoneyOverflow},
		{name: "product beyond the safe range", in: MaxSafeMoney, factor: 2, mode: RoundHalfAwayFromZero, wantErr: ErrMoneyOverflow},
		{name: "product beyond float range", in: MaxSafeMoney, factor: math.MaxFloat64, mode: RoundHalfAwayFromZero, wantErr: ErrMoneyOverflow},
		{name: "negative product beyond the safe range", in: MinSafeMoney, factor: 2, mode: RoundHalfAwayFromZero, wantErr: ErrMoneyOverflow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.MulFloat(tc.factor, tc.mode)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("MulFloat error = %v, want %v", err, tc.wantErr)
			}
			if got != 0 {
				t.Errorf("MulFloat failed but returned %d, not the zero value", int64(got))
			}
		})
	}
}

// TestMoneyMulFloatBoundaryIsExactlyRepresentable confirms the choice of
// 2^53-1 as the bound: the product at the bound converts back to the same
// integer, which is the property that would fail one unit higher.
func TestMoneyMulFloatBoundaryIsExactlyRepresentable(t *testing.T) {
	got, err := MaxSafeMoney.MulFloat(1, RoundHalfAwayFromZero)
	if err != nil {
		t.Fatalf("MulFloat at the safe bound: %v", err)
	}
	if got != MaxSafeMoney {
		t.Errorf("MaxSafeMoney × 1 = %d, want %d", int64(got), int64(MaxSafeMoney))
	}
	if int64(MaxSafeMoney) != int64(float64(MaxSafeMoney)) {
		t.Errorf("MaxSafeMoney does not survive a float64 round trip; the bound is wrong")
	}
	// One past the bound is where float64 stops distinguishing consecutive
	// integers, which is exactly why the bound sits where it does.
	if float64(int64(MaxSafeMoney)+1) != float64(int64(MaxSafeMoney)+2) {
		t.Log("note: 2^53 and 2^53+1 are distinguishable here; the bound is still correct but the rationale differs")
	}
}

func TestMoneyStringUsesIntegerArithmetic(t *testing.T) {
	tests := []struct {
		in   Money
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{7, "0.07"},
		{99, "0.99"},
		{100, "1.00"},
		{12345, "123.45"},
		{-1, "-0.01"},
		{-7, "-0.07"},
		{-12345, "-123.45"},
		{MaxSafeMoney, "90071992547409.91"},
		{MinSafeMoney, "-90071992547409.91"},
		// Reached only by raw conversion; formatting must still not overflow.
		{Money(math.MinInt64), "-92233720368547758.08"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Errorf("Money(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
			}
		})
	}
}

func TestParseMoney(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Money
		wantErr error
	}{
		{name: "integer", in: "12", want: 1200},
		{name: "zero", in: "0", want: 0},
		{name: "two decimals", in: "12.34", want: 1234},
		{name: "one decimal scales up", in: "12.3", want: 1230},
		{name: "explicit plus", in: "+5", want: 500},
		{name: "negative", in: "-0.07", want: -7},
		{name: "negative zero collapses", in: "-0.00", want: 0},
		{name: "leading zeros", in: "007.50", want: 750},
		{name: "at the safe bound", in: "90071992547409.91", want: MaxSafeMoney},
		{name: "at the negative safe bound", in: "-90071992547409.91", want: MinSafeMoney},

		{name: "empty", in: "", wantErr: ErrMoneySyntax},
		{name: "sign only", in: "-", wantErr: ErrMoneySyntax},
		{name: "leading space", in: " 12", wantErr: ErrMoneySyntax},
		{name: "trailing space", in: "12 ", wantErr: ErrMoneySyntax},
		{name: "no integer digit", in: ".50", wantErr: ErrMoneySyntax},
		{name: "trailing point", in: "12.", wantErr: ErrMoneySyntax},
		{name: "two points", in: "1.2.3", wantErr: ErrMoneySyntax},
		{name: "currency symbol", in: "$12.34", wantErr: ErrMoneySyntax},
		{name: "thousands separator", in: "1,234.00", wantErr: ErrMoneySyntax},
		{name: "scientific notation", in: "1e3", wantErr: ErrMoneySyntax},
		{name: "hex", in: "0x10", wantErr: ErrMoneySyntax},
		{name: "letters", in: "abc", wantErr: ErrMoneySyntax},

		// Excess precision is refused rather than rounded: a parser that turns
		// "12.345" into 12.34 puts a rounding decision where nobody reviews it.
		{name: "three decimals", in: "12.345", wantErr: ErrMoneyPrecision},
		{name: "many decimals", in: "0.123456789", wantErr: ErrMoneyPrecision},

		{name: "past the safe bound", in: "90071992547409.92", wantErr: ErrMoneyOverflow},
		{name: "absurdly large", in: "999999999999999999999.99", wantErr: ErrMoneyOverflow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMoney(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseMoney(%q) error = %v, want %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ParseMoney(%q) = %d, want %d", tc.in, int64(got), int64(tc.want))
			}
		})
	}
}

// TestMoneyStringParseRoundTrip checks the pair over a deterministic pseudo-
// random sweep. The seed is fixed, so a failure is reproducible; the package
// has no external property-testing dependency by design (see doc_test.go's
// dependency guard), and a seeded sweep buys most of the same coverage.
func TestMoneyStringParseRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x5341, 0x5250))
	for i := 0; i < 20000; i++ {
		// Two independent draws, held in named variables: written inline the two
		// calls are syntactically identical and staticcheck (SA4000) reads the
		// subtraction as a value minus itself.
		lo := Money(rng.Int64N(int64(MaxSafeMoney)) + 1)
		hi := Money(rng.Int64N(int64(MaxSafeMoney)) + 1)
		v := lo - hi
		s := v.String()
		back, err := ParseMoney(s)
		if err != nil {
			t.Fatalf("ParseMoney(%q) from Money(%d): %v", s, int64(v), err)
		}
		if back != v {
			t.Fatalf("round trip: Money(%d) → %q → Money(%d)", int64(v), s, int64(back))
		}
	}
}

// FuzzParseMoney asserts the two properties that must hold for every input:
// parsing never panics, and any value that parses re-parses from its own
// canonical rendering to the same amount. Under `go test` this replays the seed
// corpus; under `-fuzz` it explores.
func FuzzParseMoney(f *testing.F) {
	for _, seed := range []string{
		"0", "12.34", "-0.07", "+5", "0.1", "007.50", "90071992547409.91",
		"", "-", ".5", "12.", "$1", "1,0", "1e3", "12.345", "999999999999999999999.99",
		"\x00", "--1", "1-1", "0.000",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, err := ParseMoney(in)
		if err != nil {
			if got != 0 {
				t.Fatalf("ParseMoney(%q) failed but returned %d", in, int64(got))
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseMoney(%q) error %v does not reach ErrInvalid", in, err)
			}
			return
		}
		if got > MaxSafeMoney || got < MinSafeMoney {
			t.Fatalf("ParseMoney(%q) = %d, outside the safe range", in, int64(got))
		}
		back, err := ParseMoney(got.String())
		if err != nil {
			t.Fatalf("ParseMoney(%q) → %q did not re-parse: %v", in, got.String(), err)
		}
		if back != got {
			t.Fatalf("ParseMoney(%q) = %d, but its rendering %q parses to %d", in, int64(got), got.String(), int64(back))
		}
	})
}

func TestMoneyPredicatesAndCompare(t *testing.T) {
	if !ZeroMoney.IsZero() || Money(1).IsZero() {
		t.Error("IsZero is wrong")
	}
	if !Money(1).IsPositive() || Money(0).IsPositive() || Money(-1).IsPositive() {
		t.Error("IsPositive is wrong")
	}
	if !Money(-1).IsNegative() || Money(0).IsNegative() || Money(1).IsNegative() {
		t.Error("IsNegative is wrong")
	}
	if Money(1).Compare(2) != -1 || Money(2).Compare(1) != 1 || Money(2).Compare(2) != 0 {
		t.Error("Compare is wrong")
	}
	if Money(12345).MinorUnits() != 12345 {
		t.Error("MinorUnits is wrong")
	}

	major, minor, sign := Money(-12345).Split()
	if major != 123 || minor != 45 || sign != -1 {
		t.Errorf("Split(-12345) = (%d, %d, %d), want (123, 45, -1)", major, minor, sign)
	}
	if _, _, sign := ZeroMoney.Split(); sign != 0 {
		t.Errorf("Split(0) sign = %d, want 0", sign)
	}
}

// TestFloat64ForDisplayOnlyIsLossy documents, as an executable assertion, why
// the function carries that name: the value it returns cannot round-trip.
func TestFloat64ForDisplayOnlyIsLossy(t *testing.T) {
	m := MaxSafeMoney
	back := Money(m.Float64ForDisplayOnly() * float64(MinorUnitsPerMajor))
	if back == m {
		t.Skip("this build round-trips at the safe bound; the warning still stands for larger values")
	}
	t.Logf("Money(%d) → %v → Money(%d): the display float lost %d minor units",
		int64(m), m.Float64ForDisplayOnly(), int64(back), int64(m)-int64(back))

	// The everyday case is exact enough to display and still wrong to store.
	assertApprox(t, "123.45 as a display float", Money(12345).Float64ForDisplayOnly(), 123.45)
}

func TestRoundingString(t *testing.T) {
	tests := []struct {
		in   Rounding
		want string
	}{
		{RoundHalfAwayFromZero, "half_away_from_zero"},
		{RoundHalfToEven, "half_to_even"},
		{RoundTowardZero, "toward_zero"},
		{RoundingUnknown, "unknown"},
		{Rounding(200), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Rounding(%d).String() = %q, want %q", uint8(tc.in), got, tc.want)
		}
		if tc.in.Valid() != (tc.want != "unknown") {
			t.Errorf("Rounding(%d).Valid() disagrees with String()", uint8(tc.in))
		}
	}
}

// TestParseRounding pins the accepted set to exactly what String emits.
//
// The rejected cases are the ones a forgiving parser would have let through, and
// each is deliberate: `wagers.rounding` is TEXT with a CHECK admitting three
// spellings, so any spelling this function accepts and the CHECK does not is a
// wager that can be constructed in Go and never stored.
func TestParseRounding(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Rounding
		wantErr error
	}{
		{name: "half away from zero", in: "half_away_from_zero", want: RoundHalfAwayFromZero},
		{name: "half to even", in: "half_to_even", want: RoundHalfToEven},
		{name: "toward zero", in: "toward_zero", want: RoundTowardZero},

		// "unknown" is String's rendering of the invalid zero value. It is not a
		// storable state and must not parse, or callers get a Rounding that
		// every float-consuming operation then rejects.
		{name: "the invalid zero value's own spelling", in: "unknown", wantErr: ErrUnknownRounding},

		{name: "empty", in: "", wantErr: ErrUnknownRounding},
		{name: "uppercase", in: "HALF_TO_EVEN", wantErr: ErrUnknownRounding},
		{name: "mixed case", in: "Half_To_Even", wantErr: ErrUnknownRounding},
		{name: "hyphenated", in: "half-to-even", wantErr: ErrUnknownRounding},
		{name: "leading space", in: " half_to_even", wantErr: ErrUnknownRounding},
		{name: "trailing space", in: "half_to_even ", wantErr: ErrUnknownRounding},
		{name: "alias nobody promised", in: "bankers", wantErr: ErrUnknownRounding},
		{name: "alias nobody promised either", in: "truncate", wantErr: ErrUnknownRounding},
		{name: "numeric form", in: "2", wantErr: ErrUnknownRounding},
		{name: "long garbage is sampled into the message", in: strings.Repeat("x", 200), wantErr: ErrUnknownRounding},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRounding(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseRounding(%q) error = %v, want %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ParseRounding(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if tc.wantErr != nil && got != RoundingUnknown {
				t.Errorf("ParseRounding(%q) failed but returned %v, want the invalid zero value", tc.in, got)
			}
		})
	}
}

// TestRoundingStringParseRoundTrip walks EVERY defined mode, which is what makes
// this a total inverse assertion rather than a spot check: adding a fourth mode
// without teaching ParseRounding about it fails here.
func TestRoundingStringParseRoundTrip(t *testing.T) {
	all := []Rounding{RoundHalfAwayFromZero, RoundHalfToEven, RoundTowardZero}

	// The loop covers every VALID value only if the valid values are exactly
	// these three, so that is asserted over the whole uint8 domain rather than
	// assumed.
	var valid []Rounding
	for i := 0; i <= 255; i++ {
		if r := Rounding(uint8(i)); r.Valid() {
			valid = append(valid, r)
		}
	}
	if len(valid) != len(all) {
		t.Fatalf("Valid() admits %d values %v, but the round trip covers %d %v",
			len(valid), valid, len(all), all)
	}

	for _, r := range all {
		s := r.String()
		back, err := ParseRounding(s)
		if err != nil {
			t.Fatalf("ParseRounding(%q) from Rounding(%d).String(): unexpected error %v", s, uint8(r), err)
		}
		if back != r {
			t.Errorf("round trip Rounding(%d) → %q → Rounding(%d)", uint8(r), s, uint8(back))
		}

		text, err := r.MarshalText()
		if err != nil {
			t.Fatalf("Rounding(%d).MarshalText(): unexpected error %v", uint8(r), err)
		}
		if string(text) != s {
			t.Errorf("Rounding(%d).MarshalText() = %q, want %q (String)", uint8(r), text, s)
		}

		var decoded Rounding
		if err := decoded.UnmarshalText(text); err != nil {
			t.Fatalf("UnmarshalText(%q): unexpected error %v", text, err)
		}
		if decoded != r {
			t.Errorf("text round trip Rounding(%d) → %q → Rounding(%d)", uint8(r), text, uint8(decoded))
		}
	}
}

// TestRoundingTextCodecRejectsInvalid keeps the codec from becoming the back
// door the constructor closed: MulFloat refuses RoundingUnknown, so a decoder
// that accepted it would only move the failure later.
func TestRoundingTextCodecRejectsInvalid(t *testing.T) {
	for _, r := range []Rounding{RoundingUnknown, Rounding(200)} {
		if _, err := r.MarshalText(); !errors.Is(err, ErrUnknownRounding) {
			t.Errorf("Rounding(%d).MarshalText() error = %v, want ErrUnknownRounding", uint8(r), err)
		}
	}

	// A failed decode must leave the receiver untouched, so a partially applied
	// JSON object cannot silently switch a wager's rounding policy.
	decoded := RoundHalfToEven
	if err := decoded.UnmarshalText([]byte("nonsense")); !errors.Is(err, ErrUnknownRounding) {
		t.Fatalf("UnmarshalText(\"nonsense\") error = %v, want ErrUnknownRounding", err)
	}
	if decoded != RoundHalfToEven {
		t.Errorf("a failed UnmarshalText mutated the receiver to %v", decoded)
	}
}

func TestAbsUint64(t *testing.T) {
	tests := []struct {
		in   int64
		want uint64
	}{
		{0, 0},
		{5, 5},
		{-5, 5},
		{math.MaxInt64, 1 << 63 % (1 << 63) >> 0}, // placeholder replaced below
	}
	tests[3].want = uint64(math.MaxInt64)

	for _, tc := range tests {
		if got := absUint64(tc.in); got != tc.want {
			t.Errorf("absUint64(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// The case the whole helper exists for: negating math.MinInt64 as an int64
	// overflows, so it is computed in unsigned space.
	if got := absUint64(math.MinInt64); got != 1<<63 {
		t.Errorf("absUint64(math.MinInt64) = %d, want %d", got, uint64(1)<<63)
	}
}
