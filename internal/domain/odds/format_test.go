package odds

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"testing"
)

// -----------------------------------------------------------------------------
// Format enum
// -----------------------------------------------------------------------------

func TestFormatStringAndValid(t *testing.T) {
	cases := []struct {
		in    Format
		want  string
		valid bool
	}{
		{in: FormatAmerican, want: "american", valid: true},
		{in: FormatDecimal, want: "decimal", valid: true},
		{in: FormatFractional, want: "fractional", valid: true},
		{in: FormatUnknown, want: "unknown"},
		{in: Format(0), want: "unknown"},
		{in: Format(200), want: "unknown"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Format(%d).String() = %q, want %q", uint8(c.in), got, c.want)
		}
		if got := c.in.Valid(); got != c.valid {
			t.Errorf("Format(%d).Valid() = %v, want %v", uint8(c.in), got, c.valid)
		}
	}
}

// TestFormatZeroValueIsInvalid pins the deliberate choice that an unset Format
// fails rather than silently defaulting to American — a struct that forgot to set
// the field must not quietly render US odds to a European user.
func TestFormatZeroValueIsInvalid(t *testing.T) {
	var f Format
	if f.Valid() {
		t.Fatal("the zero Format must be invalid")
	}
	if _, err := f.MarshalText(); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("Format(0).MarshalText() error = %v, want ErrUnknownFormat", err)
	}
	if _, err := Render(2.5, f); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("Render(2.5, Format(0)) error = %v, want ErrUnknownFormat", err)
	}
}

func TestParseFormat(t *testing.T) {
	cases := []struct {
		in   string
		want Format
	}{
		{in: "american", want: FormatAmerican},
		{in: "American", want: FormatAmerican},
		{in: "  AMERICAN  ", want: FormatAmerican},
		{in: "us", want: FormatAmerican},
		{in: "decimal", want: FormatDecimal},
		{in: "eu", want: FormatDecimal},
		{in: "euro", want: FormatDecimal},
		{in: "european", want: FormatDecimal},
		{in: "fractional", want: FormatFractional},
		{in: "fraction", want: FormatFractional},
		{in: "uk", want: FormatFractional},
	}
	for _, c := range cases {
		got, err := ParseFormat(c.in)
		if err != nil {
			t.Errorf("ParseFormat(%q) returned %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseFormat(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "   ", "unknown", "hongkong", "malay", "indonesian", "americ", "5/2"} {
		got, err := ParseFormat(bad)
		if !errors.Is(err, ErrUnknownFormat) {
			t.Errorf("ParseFormat(%q) = %v, %v; want ErrUnknownFormat", bad, got, err)
		}
		if got != FormatUnknown {
			t.Errorf("ParseFormat(%q) returned %v alongside an error; want FormatUnknown", bad, got)
		}
	}
}

// TestFormatStringParseRoundTrip: String is the inverse of ParseFormat for every
// valid value, so a format can survive a trip through a query string or a JSON body.
func TestFormatStringParseRoundTrip(t *testing.T) {
	for _, f := range []Format{FormatAmerican, FormatDecimal, FormatFractional} {
		got, err := ParseFormat(f.String())
		if err != nil {
			t.Fatalf("ParseFormat(%q) returned %v", f.String(), err)
		}
		if got != f {
			t.Errorf("round trip %v -> %q -> %v", f, f.String(), got)
		}
	}
}

func TestFormatTextMarshalling(t *testing.T) {
	for _, f := range []Format{FormatAmerican, FormatDecimal, FormatFractional} {
		b, err := f.MarshalText()
		if err != nil {
			t.Fatalf("Format(%v).MarshalText() returned %v", f, err)
		}
		var got Format
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("UnmarshalText(%q) returned %v", b, err)
		}
		if got != f {
			t.Errorf("text round trip %v -> %q -> %v", f, b, got)
		}
	}

	// The TextMarshaler hook is what makes Format work inside a JSON payload,
	// which is how the frontend toggle will carry it.
	type preferences struct {
		OddsFormat Format `json:"oddsFormat"`
	}
	encoded, err := json.Marshal(preferences{OddsFormat: FormatFractional})
	if err != nil {
		t.Fatalf("json.Marshal returned %v", err)
	}
	if string(encoded) != `{"oddsFormat":"fractional"}` {
		t.Errorf("json.Marshal = %s, want {\"oddsFormat\":\"fractional\"}", encoded)
	}
	var decoded preferences
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned %v", err)
	}
	if decoded.OddsFormat != FormatFractional {
		t.Errorf("json round trip produced %v, want %v", decoded.OddsFormat, FormatFractional)
	}

	if err := json.Unmarshal([]byte(`{"oddsFormat":"klingon"}`), &decoded); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("decoding an unknown format returned %v, want ErrUnknownFormat", err)
	}
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// TestRenderAmericanAlwaysCarriesItsSign: the leading "+" is load-bearing. Without
// it "150" is ambiguous, since the two signs mean opposite things in the American
// convention.
func TestRenderAmericanAlwaysCarriesItsSign(t *testing.T) {
	cases := []struct {
		in   American
		want string
	}{
		{in: 100, want: "+100"},
		{in: 110, want: "+110"},
		{in: 150, want: "+150"},
		{in: 2500, want: "+2500"},
		{in: 1_000_000, want: "+1000000"},
		{in: -105, want: "-105"},
		{in: -110, want: "-110"},
		{in: -1_000_000, want: "-1000000"},
		// String does not validate; it renders what it is given so a log or a
		// debugger shows the real contents rather than a placeholder.
		{in: 0, want: "0"},
		{in: 50, want: "+50"},
	}
	for _, c := range cases {
		if got := RenderAmerican(c.in); got != c.want {
			t.Errorf("RenderAmerican(%d) = %q, want %q", int64(c.in), got, c.want)
		}
		if got := c.in.String(); got != c.want {
			t.Errorf("American(%d).String() = %q, want %q", int64(c.in), got, c.want)
		}
	}
}

func TestRenderFractional(t *testing.T) {
	cases := []struct {
		in   Fractional
		want string
	}{
		{in: Fractional{Numerator: 5, Denominator: 2}, want: "5/2"},
		{in: Fractional{Numerator: 10, Denominator: 11}, want: "10/11"},
		{in: Fractional{Numerator: 1, Denominator: 5}, want: "1/5"},
		{in: Fractional{Numerator: 25, Denominator: 1}, want: "25/1"},
		{in: Fractional{Numerator: 1, Denominator: 1}, want: "1/1"},
		// Rendered in lowest terms: the traditional 6/4 and 4/6 spellings are a
		// book presentation choice, not a property of the price.
		{in: Fractional{Numerator: 6, Denominator: 4}, want: "3/2"},
		{in: Fractional{Numerator: 4, Denominator: 6}, want: "2/3"},
		// Invalid values render faithfully rather than being hidden.
		{in: Fractional{}, want: "0/0"},
		{in: Fractional{Numerator: 1, Denominator: 0}, want: "1/0"},
	}
	for _, c := range cases {
		if got := RenderFractional(c.in); got != c.want {
			t.Errorf("RenderFractional(%d/%d) = %q, want %q", c.in.Numerator, c.in.Denominator, got, c.want)
		}
		if got := c.in.String(); got != c.want {
			t.Errorf("Fractional{%d, %d}.String() = %q, want %q", c.in.Numerator, c.in.Denominator, got, c.want)
		}
	}
}

func TestRenderDecimalPrecision(t *testing.T) {
	// The decimal price of -110 is 1.909090…, which is the interesting case: it
	// exposes what the rounding at each precision actually does.
	a, err := NewAmerican(-110)
	if err != nil {
		t.Fatalf("NewAmerican(-110) returned %v", err)
	}
	d, err := a.Decimal()
	if err != nil {
		t.Fatalf("Decimal() returned %v", err)
	}

	cases := []struct {
		places int
		want   string
	}{
		{places: 0, want: "2"},
		{places: 1, want: "1.9"},
		{places: 2, want: "1.91"},
		{places: 3, want: "1.909"},
		{places: 4, want: "1.9091"},
	}
	for _, c := range cases {
		if got := RenderDecimal(d, c.places); got != c.want {
			t.Errorf("RenderDecimal(%.17g, %d) = %q, want %q", float64(d), c.places, got, c.want)
		}
	}

	// A negative places argument asks for the shortest string that parses back to
	// the identical float64. The property is round-trip identity, so that is what
	// is asserted rather than a hand-copied literal.
	for _, v := range []Decimal{d, 2.5, 26, 1.01, 1.0001, 10001} {
		s := RenderDecimal(v, -1)
		parsed, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("RenderDecimal(%.17g, -1) = %q, which does not parse: %v", float64(v), s, err)
		}
		if parsed != float64(v) {
			t.Errorf("RenderDecimal(%.17g, -1) = %q, which parses back to %.17g", float64(v), s, parsed)
		}
	}

	// The default precision is the book convention, two places.
	if got := RenderDecimal(2.5, DefaultDecimalPlaces); got != "2.50" {
		t.Errorf("RenderDecimal(2.5, %d) = %q, want %q", DefaultDecimalPlaces, got, "2.50")
	}
	if got := RenderDecimal(26, DefaultDecimalPlaces); got != "26.00" {
		t.Errorf("RenderDecimal(26, %d) = %q, want %q", DefaultDecimalPlaces, got, "26.00")
	}
}

func TestRenderProbability(t *testing.T) {
	a, err := NewAmerican(-110)
	if err != nil {
		t.Fatalf("NewAmerican(-110) returned %v", err)
	}
	p, err := a.Probability()
	if err != nil {
		t.Fatalf("Probability() returned %v", err)
	}

	cases := []struct {
		places int
		want   string
	}{
		{places: 0, want: "52%"},
		{places: 1, want: "52.4%"},
		{places: 2, want: "52.38%"},
		{places: 4, want: "52.3810%"},
	}
	for _, c := range cases {
		if got := RenderProbability(p, c.places); got != c.want {
			t.Errorf("RenderProbability(%.17g, %d) = %q, want %q", float64(p), c.places, got, c.want)
		}
	}

	if got := RenderProbability(0.5, DefaultProbabilityPlaces); got != "50.00%" {
		t.Errorf("RenderProbability(0.5, %d) = %q, want %q", DefaultProbabilityPlaces, got, "50.00%")
	}
	// Two places is enough to separate the two sides of a standard -110 market
	// from even money, which is the point of the default.
	if RenderProbability(p, DefaultProbabilityPlaces) == RenderProbability(0.5, DefaultProbabilityPlaces) {
		t.Error("the default probability precision cannot distinguish -110 from even money")
	}
}

// -----------------------------------------------------------------------------
// Render: the user-facing format toggle
// -----------------------------------------------------------------------------

func TestRenderAcrossFormats(t *testing.T) {
	cases := []struct {
		name       string
		american   int64
		american_s string
		decimal_s  string
		fraction_s string
	}{
		{name: "standard juice", american: -110, american_s: "-110", decimal_s: "1.91", fraction_s: "10/11"},
		{name: "reduced juice", american: -105, american_s: "-105", decimal_s: "1.95", fraction_s: "20/21"},
		{name: "even money", american: 100, american_s: "+100", decimal_s: "2.00", fraction_s: "1/1"},
		{name: "short favourite", american: -500, american_s: "-500", decimal_s: "1.20", fraction_s: "1/5"},
		{name: "modest dog", american: 150, american_s: "+150", decimal_s: "2.50", fraction_s: "3/2"},
		{name: "longshot", american: 2500, american_s: "+2500", decimal_s: "26.00", fraction_s: "25/1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := NewAmerican(c.american)
			if err != nil {
				t.Fatalf("NewAmerican(%d) returned %v", c.american, err)
			}
			d, err := a.Decimal()
			if err != nil {
				t.Fatalf("Decimal() returned %v", err)
			}

			for _, tc := range []struct {
				format Format
				want   string
			}{
				{FormatAmerican, c.american_s},
				{FormatDecimal, c.decimal_s},
				{FormatFractional, c.fraction_s},
			} {
				got, err := Render(d, tc.format)
				if err != nil {
					t.Fatalf("Render(%.17g, %v) returned %v", float64(d), tc.format, err)
				}
				if got != tc.want {
					t.Errorf("Render(%.17g, %v) = %q, want %q", float64(d), tc.format, got, tc.want)
				}
			}
		})
	}
}

func TestRenderRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		d      Decimal
		format Format
		want   error
	}{
		{name: "decimal of exactly one", d: 1, format: FormatDecimal, want: ErrDecimalOutOfRange},
		{name: "zero decimal", d: 0, format: FormatAmerican, want: ErrDecimalOutOfRange},
		{name: "unset format", d: 2.5, format: FormatUnknown, want: ErrUnknownFormat},
		{name: "format out of the enum", d: 2.5, format: Format(99), want: ErrUnknownFormat},
		{
			name:   "a price too short to have an american form",
			d:      1.00001,
			format: FormatAmerican,
			want:   ErrAmericanOutOfRange,
		},
		{
			name:   "a price too short to have a fractional form",
			d:      1.0001,
			format: FormatFractional,
			want:   ErrFractionalNotRepresentable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Render(c.d, c.format)
			if !errors.Is(err, c.want) {
				t.Fatalf("Render(%v, %v) = %q, %v; want %v", float64(c.d), c.format, got, err, c.want)
			}
			if got != "" {
				t.Errorf("Render returned %q alongside an error; a failed render must return the empty string", got)
			}
		})
	}
}

// TestRenderFallbackForUnrepresentablePrices documents the intended caller
// behaviour for the format toggle: an extreme favourite has no fractional form, and
// the UI is expected to fall back rather than to fail the request.
func TestRenderFallbackForUnrepresentablePrices(t *testing.T) {
	d := Decimal(1.0001) // American -1000000
	if _, err := Render(d, FormatFractional); !errors.Is(err, ErrFractionalNotRepresentable) {
		t.Fatalf("Render(1.0001, fractional) error = %v, want ErrFractionalNotRepresentable", err)
	}
	for _, fallback := range []Format{FormatDecimal, FormatAmerican} {
		if _, err := Render(d, fallback); err != nil {
			t.Errorf("Render(1.0001, %v) returned %v; the fallback formats must succeed", fallback, err)
		}
	}
}

// TestRenderersNeverPanicOnGarbage: String has no error return, so it must be
// total. Anything a caller can construct must render to something.
func TestRenderersNeverPanicOnGarbage(t *testing.T) {
	for _, a := range []American{0, 1, -1, 99, -99, 100, -100, math.MaxInt64, math.MinInt64} {
		if a.String() == "" {
			t.Errorf("American(%d).String() produced the empty string", int64(a))
		}
	}
	for _, f := range []Fractional{{}, {0, 1}, {1, 0}, {-1, -1}, {-6, 4}} {
		if f.String() == "" {
			t.Errorf("Fractional{%d, %d}.String() produced the empty string", f.Numerator, f.Denominator)
		}
	}
	for _, d := range []Decimal{0, 1, -1, 2.5} {
		if RenderDecimal(d, DefaultDecimalPlaces) == "" {
			t.Errorf("RenderDecimal(%v) produced the empty string", float64(d))
		}
	}
	for _, p := range []Probability{-1, 0, 0.5, 1, 2} {
		if RenderProbability(p, DefaultProbabilityPlaces) == "" {
			t.Errorf("RenderProbability(%v) produced the empty string", float64(p))
		}
	}
}
