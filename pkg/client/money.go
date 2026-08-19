package client

import (
	"strconv"
	"strings"
)

// MinorUnitsPerMajor is the fixed scale of Sharpline's play-money currency:
// 100 minor units to one major unit.
//
// The server sends it on every balance response
// ([BalanceResponse.MinorUnitsPerMajor]) rather than assuming a client knows
// it, and this constant is here so a caller formatting a stake or a limit —
// neither of which travels with a scale — does not have to invent one.
const MinorUnitsPerMajor = 100

// FormatMinor renders an amount in minor units for DISPLAY: 250000 becomes
// "2500.00".
//
// # There is no float anywhere in this function, and that is the point
//
// CLAUDE.md §12: "All money and stake values are integer minor units. Floating
// point never touches a balance." The obvious implementation —
// `strconv.FormatFloat(float64(minor)/100, ...)` — violates that for values a
// float64 cannot hold exactly, and the schema's bound is 2^53-1, so those
// values are representable and reachable. Integer division and a string join
// are exact for every value the API can send.
//
// This is display only. Never parse the result back and never do arithmetic on
// it; keep the int64 and operate on that.
func FormatMinor(minor int64) string {
	neg := minor < 0

	// Negate in an unsigned width. -minor overflows for math.MinInt64, which is
	// outside the schema's bound but reachable through a zero value or a bug,
	// and a formatter that produces a wrong number on one input is a formatter
	// nobody can trust.
	var abs uint64
	if neg {
		abs = uint64(-(minor + 1)) + 1
	} else {
		abs = uint64(minor)
	}

	major := abs / MinorUnitsPerMajor
	rem := abs % MinorUnitsPerMajor

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatUint(major, 10))
	b.WriteByte('.')
	if rem < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.FormatUint(rem, 10))
	return b.String()
}
