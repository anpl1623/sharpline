package odds

import (
	"fmt"
	"strconv"
	"strings"
)

// Format identifies one of the display conventions a price can be shown in. It
// backs the odds format toggle that CLAUDE.md §6 lists as a core feature, so it is
// product surface rather than a debug helper.
//
// The zero value is FormatUnknown and is deliberately invalid, so a Format that was
// never set fails Valid instead of silently defaulting to American.
type Format uint8

const (
	// FormatUnknown is the invalid zero value.
	FormatUnknown Format = iota
	// FormatAmerican renders prices as +150 / -110.
	FormatAmerican
	// FormatDecimal renders prices as 2.50 / 1.91.
	FormatDecimal
	// FormatFractional renders prices as 3/2 / 10/11.
	FormatFractional
)

const (
	// DefaultDecimalPlaces is the precision Render uses for decimal odds. Two
	// places is the convention every major book displays; callers that need more
	// (Pinnacle-style three-place pricing, or an EV calculation shown to the user)
	// pass their own to RenderDecimal.
	DefaultDecimalPlaces = 2

	// DefaultProbabilityPlaces is the precision RenderProbability uses when it is
	// not told otherwise. Two places resolves the difference between the two sides
	// of a standard -110 market, which is the finest distinction a percentage
	// display needs to carry.
	DefaultProbabilityPlaces = 2

	// shortestRoundTrip is the places argument that asks for the shortest decimal
	// string that parses back to the identical float64.
	shortestRoundTrip = -1
)

// String returns the canonical lowercase name of the format: "american",
// "decimal", "fractional", or "unknown". It is the inverse of ParseFormat for every
// valid value.
func (f Format) String() string {
	switch f {
	case FormatAmerican:
		return "american"
	case FormatDecimal:
		return "decimal"
	case FormatFractional:
		return "fractional"
	case FormatUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Valid reports whether f names a real display format.
func (f Format) Valid() bool {
	switch f {
	case FormatAmerican, FormatDecimal, FormatFractional:
		return true
	case FormatUnknown:
		return false
	default:
		return false
	}
}

// ParseFormat resolves a format name, case-insensitively and ignoring surrounding
// whitespace. It accepts the canonical names plus the regional aliases that a UI or
// a query string is likely to use:
//
//	american    us
//	decimal     eu, euro, european
//	fractional  uk, fraction
//
// An unrecognised name returns ErrUnknownFormat. The empty string is not treated as
// a default; a caller that wants one should apply it explicitly.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "american", "us":
		return FormatAmerican, nil
	case "decimal", "eu", "euro", "european":
		return FormatDecimal, nil
	case "fractional", "fraction", "uk":
		return FormatFractional, nil
	default:
		return FormatUnknown, fmt.Errorf("odds: %q: %w", s, ErrUnknownFormat)
	}
}

// MarshalText implements encoding.TextMarshaler, which is what makes Format work
// unchanged in JSON bodies, query-parameter decoding, and environment config. An
// invalid Format is an error rather than the string "unknown", so a half-initialised
// value cannot be serialised and shipped to a client.
func (f Format) MarshalText() ([]byte, error) {
	if !f.Valid() {
		return nil, fmt.Errorf("odds: format %d: %w", uint8(f), ErrUnknownFormat)
	}
	return []byte(f.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler using the same alias set as
// ParseFormat.
func (f *Format) UnmarshalText(text []byte) error {
	parsed, err := ParseFormat(string(text))
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// String renders an American price with its explicit sign: "+150", "-110", "+100".
// The leading "+" is not decoration — without it a price is ambiguous, since the
// American convention gives opposite meanings to the two signs.
//
// String does not validate. It renders whatever value it is given, including one in
// the illegal band, so that a debugger or a log line shows the real contents rather
// than an error placeholder. Use Render when the output is going to a user.
func (a American) String() string { return RenderAmerican(a) }

// RenderAmerican renders an American price with its explicit sign.
func RenderAmerican(a American) string {
	v := int64(a)
	if v > 0 {
		return "+" + strconv.FormatInt(v, 10)
	}
	return strconv.FormatInt(v, 10)
}

// String renders a fractional price as "5/2", in lowest terms.
//
// Like American.String it does not validate, so an uninitialised value prints as
// "0/0" rather than being hidden.
func (f Fractional) String() string { return RenderFractional(f) }

// RenderFractional renders a fractional price as "5/2", in lowest terms.
//
// The traditional un-reduced ladder spellings some books post — 6/4 for 3/2, 4/6 for
// 2/3 — are a book-specific presentation choice, not a property of the price, and are
// not reproduced.
func RenderFractional(f Fractional) string {
	r := f.Reduce()
	return strconv.FormatInt(r.Numerator, 10) + "/" + strconv.FormatInt(r.Denominator, 10)
}

// RenderDecimal renders decimal odds with a fixed number of places, so 1.909090…
// at two places is "1.91".
//
// A negative places argument asks for the shortest string that parses back to the
// identical float64, which is the right choice for diagnostics where truncation
// would hide a discrepancy.
//
// There is deliberately no String method on Decimal. Decimal is float64-kinded, fmt
// already prints it losslessly, and a String method would quietly truncate every
// price that reached a log line.
func RenderDecimal(d Decimal, places int) string {
	if places < 0 {
		places = shortestRoundTrip
	}
	return strconv.FormatFloat(float64(d), 'f', places, 64)
}

// RenderProbability renders a probability as a percentage with a fixed number of
// places, so 0.5238095… at two places is "52.38%".
//
// As with Decimal there is no String method on Probability, for the same reason.
func RenderProbability(p Probability, places int) string {
	if places < 0 {
		places = shortestRoundTrip
	}
	return strconv.FormatFloat(float64(p)*100, 'f', places, 64) + "%"
}

// Render converts a canonical decimal price into the requested display format and
// renders it. This is the entry point behind the user-facing format toggle.
//
// It takes a Decimal because Decimal is the canonical representation everything else
// in the system stores; a caller holding an American or a Fractional converts to
// Decimal first, which is lossless in that direction.
//
// Unlike the String methods, Render validates: an invalid price, an unset format, or
// a price with no representation in the target format all return an error. In
// particular a very short favourite has no fractional form (see
// Decimal.FractionalApprox), which a format toggle should handle by falling back to
// another format rather than by failing the request.
func Render(d Decimal, format Format) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	switch format {
	case FormatDecimal:
		return RenderDecimal(d, DefaultDecimalPlaces), nil
	case FormatAmerican:
		a, err := d.American()
		if err != nil {
			return "", err
		}
		return RenderAmerican(a), nil
	case FormatFractional:
		f, err := d.Fractional()
		if err != nil {
			return "", err
		}
		return RenderFractional(f), nil
	case FormatUnknown:
		return "", fmt.Errorf("odds: format %d: %w", uint8(format), ErrUnknownFormat)
	default:
		return "", fmt.Errorf("odds: format %d: %w", uint8(format), ErrUnknownFormat)
	}
}
