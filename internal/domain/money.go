package domain

import (
	"fmt"
	"math"
	"strconv"
)

// Money is a signed count of minor units — cents, not dollars.
//
// CLAUDE.md §12: "All money and stake values are integer minor units. Floating
// point never touches a balance." Money(12345) is 123.45. Every stake, payout,
// void, adjustment, and ledger entry in the system is a Money.
//
// # Why there is no currency
//
// Money carries no currency tag, and that is a decision rather than an
// omission. CLAUDE.md §0 states the system holds no real funds: play money,
// no payment processing, no custody. A single implicit denomination therefore
// has no counterexample to model, and it buys a real property — the
// double-entry invariant ("every stake, payout, void, and adjustment is two
// rows that sum to zero", §4) is checkable with nothing but integer addition.
//
// The alternative is worse than it looks. A Currency field with no FX rate
// source produces a type that *appears* multi-currency while being unable to
// answer "what is USD 5 plus EUR 3", so every arithmetic method would have to
// return an error on mismatch and every caller would have to handle a case that
// can never occur in this system. That is ceremony bought with no safety.
//
// If real denominations ever arrive they arrive with an FX source and an ADR,
// and the change is additive: a Currency field, a mismatch check inside Add and
// Sub, and a compiler-driven sweep of the call sites. Deferring it costs a day
// then; carrying it costs every call site now.
type Money int64

const (
	// MinorUnitsPerMajor is the scale: 100 minor units to one major unit.
	MinorUnitsPerMajor int64 = 100

	// MaxSafeMoney and MinSafeMoney bound every Money the package will produce.
	//
	// The bound is 2^53-1 rather than math.MaxInt64 and the reason is a
	// coincidence worth exploiting: 2^53-1 is simultaneously
	//
	//   - the largest integer float64 represents exactly, so MulFloat cannot
	//     silently lose an integer to mantissa truncation, and
	//   - JavaScript's Number.MAX_SAFE_INTEGER, so a Money crossing the wire as
	//     a JSON number survives JSON.parse in the Next.js frontend intact.
	//
	// Refusing values above it is honest: past that point the arithmetic is no
	// longer exact and a balance that is not exact is not a balance. The cap is
	// ~90 trillion major units, which no play-money account will approach.
	MaxSafeMoney Money = 1<<53 - 1
	MinSafeMoney Money = -(1<<53 - 1)

	// ZeroMoney is the additive identity, named so call sites read as intent
	// rather than as a bare literal.
	ZeroMoney Money = 0
)

// Rounding names a rule for collapsing a fractional minor unit to a whole one.
//
// There is no default. The zero value is invalid and every float-consuming
// operation rejects it, which forces the rounding decision to be written at the
// call site where it can be reviewed. Settlement rounding is a policy question,
// not an implementation detail, and a silent default is how a house edge
// appears in a ledger that nobody meant to put there.
type Rounding uint8

const (
	// RoundingUnknown is the invalid zero value.
	RoundingUnknown Rounding = iota

	// RoundHalfAwayFromZero rounds to the nearest minor unit and breaks ties
	// away from zero. This is "commercial rounding" and what a human means by
	// "round to the nearest cent".
	RoundHalfAwayFromZero

	// RoundHalfToEven rounds to the nearest minor unit and breaks ties toward
	// the even neighbour — banker's rounding. Over a large number of
	// settlements it has no directional bias, which RoundHalfAwayFromZero does.
	// Prefer it wherever many roundings accumulate into one aggregate.
	RoundHalfToEven

	// RoundTowardZero truncates the fraction. Applied to a positive payout this
	// favours the house; applied to a negative one it favours the customer.
	// Choose it only when a rule explicitly calls for truncation.
	RoundTowardZero
)

// String implements fmt.Stringer.
func (r Rounding) String() string {
	switch r {
	case RoundHalfAwayFromZero:
		return "half_away_from_zero"
	case RoundHalfToEven:
		return "half_to_even"
	case RoundTowardZero:
		return "toward_zero"
	default:
		return "unknown"
	}
}

// Valid reports whether r is one of the defined rounding modes.
func (r Rounding) Valid() bool {
	switch r {
	case RoundHalfAwayFromZero, RoundHalfToEven, RoundTowardZero:
		return true
	default:
		return false
	}
}

// FromMinorUnits builds a Money from a count of minor units.
//
// It returns an error outside [MinSafeMoney, MaxSafeMoney] rather than
// accepting a value the rest of the package cannot operate on exactly.
func FromMinorUnits(n int64) (Money, error) {
	m := Money(n)
	if m > MaxSafeMoney || m < MinSafeMoney {
		return 0, fmt.Errorf("%d minor units: %w", n, ErrMoneyOverflow)
	}
	return m, nil
}

// FromMajorUnits builds a Money from a whole count of major units: 5 → 5.00.
func FromMajorUnits(n int64) (Money, error) {
	if n > int64(MaxSafeMoney)/MinorUnitsPerMajor || n < int64(MinSafeMoney)/MinorUnitsPerMajor {
		return 0, fmt.Errorf("%d major units: %w", n, ErrMoneyOverflow)
	}
	return Money(n * MinorUnitsPerMajor), nil
}

// MinorUnits returns the raw signed count of minor units.
func (m Money) MinorUnits() int64 { return int64(m) }

// Split returns the major-unit and minor-unit components of the magnitude,
// together with the sign (-1, 0, or +1). It is the formatting primitive, and it
// never allocates a float.
func (m Money) Split() (major, minor int64, sign int) {
	u := m.magnitude()
	switch {
	case m > 0:
		sign = 1
	case m < 0:
		sign = -1
	}
	return int64(u / uint64(MinorUnitsPerMajor)), int64(u % uint64(MinorUnitsPerMajor)), sign
}

// magnitude returns |m| as a uint64, which is the only representation that does
// not overflow for math.MinInt64.
func (m Money) magnitude() uint64 {
	if m < 0 {
		return uint64(-(int64(m) + 1)) + 1
	}
	return uint64(m)
}

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (m Money) IsPositive() bool { return m > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (m Money) IsNegative() bool { return m < 0 }

// Compare returns -1, 0, or +1 as m sorts before, with, or after o.
func (m Money) Compare(o Money) int {
	switch {
	case m < o:
		return -1
	case m > o:
		return 1
	default:
		return 0
	}
}

// Add returns m+o, or ErrMoneyOverflow if the sum leaves the safe range.
//
// The int64 wrap is caught before the range check by the sign-bit identity
// ((m^s) & (o^s)) < 0, which is true exactly when the operands share a sign
// that the sum does not.
func (m Money) Add(o Money) (Money, error) {
	s := m + o
	if (m^s)&(o^s) < 0 {
		return 0, fmt.Errorf("%s + %s: %w", m, o, ErrMoneyOverflow)
	}
	if s > MaxSafeMoney || s < MinSafeMoney {
		return 0, fmt.Errorf("%s + %s: %w", m, o, ErrMoneyOverflow)
	}
	return s, nil
}

// Sub returns m-o, or ErrMoneyOverflow if the difference leaves the safe range.
func (m Money) Sub(o Money) (Money, error) {
	d := m - o
	if (m^o)&(m^d) < 0 {
		return 0, fmt.Errorf("%s - %s: %w", m, o, ErrMoneyOverflow)
	}
	if d > MaxSafeMoney || d < MinSafeMoney {
		return 0, fmt.Errorf("%s - %s: %w", m, o, ErrMoneyOverflow)
	}
	return d, nil
}

// Neg returns -m. It is the ledger's workhorse: a double-entry pair is an
// amount and its Neg, and the pair sums to zero by construction.
func (m Money) Neg() (Money, error) {
	if m == math.MinInt64 {
		return 0, fmt.Errorf("negate %d minor units: %w", int64(m), ErrMoneyOverflow)
	}
	n := -m
	if n > MaxSafeMoney || n < MinSafeMoney {
		return 0, fmt.Errorf("negate %s: %w", m, ErrMoneyOverflow)
	}
	return n, nil
}

// Abs returns |m|.
func (m Money) Abs() (Money, error) {
	if m < 0 {
		return m.Neg()
	}
	if m > MaxSafeMoney {
		return 0, fmt.Errorf("abs %d minor units: %w", int64(m), ErrMoneyOverflow)
	}
	return m, nil
}

// MulInt returns m*k. It is exact — no rounding is possible — which makes it
// the right operation for whole multipliers such as a round robin's
// combination count.
//
// The product is computed on unsigned magnitudes and the sign reapplied at the
// end. That avoids both of the classic int64 multiplication hazards: the wrap
// that a naive product hides, and the panic that `p/k` raises when k is -1 and
// p is math.MinInt64.
func (m Money) MulInt(k int64) (Money, error) {
	if m == 0 || k == 0 {
		return ZeroMoney, nil
	}
	if m > MaxSafeMoney || m < MinSafeMoney {
		return 0, fmt.Errorf("%d minor units × %d: %w", int64(m), k, ErrMoneyOverflow)
	}
	am := m.magnitude()
	ak := absUint64(k)
	if am > uint64(MaxSafeMoney)/ak {
		return 0, fmt.Errorf("%s × %d: %w", m, k, ErrMoneyOverflow)
	}
	product := am * ak
	if (m < 0) != (k < 0) {
		return Money(-int64(product)), nil
	}
	return Money(product), nil
}

// absUint64 returns |k| without the math.MinInt64 negation overflow.
func absUint64(k int64) uint64 {
	if k < 0 {
		return uint64(-(k + 1)) + 1
	}
	return uint64(k)
}

// DivMod divides m by k and returns both the quotient and the remainder, in
// minor units, with the remainder taking the sign of m (Go's integer division
// semantics).
//
// Returning the remainder is the point. Splitting a round-robin stake across
// combinations, or a void across legs, must not lose minor units to truncation;
// forcing the caller to receive the remainder means the loss cannot happen
// silently. quotient*k + remainder == m always holds.
func (m Money) DivMod(k int64) (quotient, remainder Money, err error) {
	if k == 0 {
		return 0, 0, fmt.Errorf("%s ÷ 0: %w", m, ErrMoneyDivideByZero)
	}
	if k == -1 {
		q, nerr := m.Neg()
		if nerr != nil {
			return 0, 0, nerr
		}
		return q, 0, nil
	}
	return Money(int64(m) / k), Money(int64(m) % k), nil
}

// MulFloat multiplies m by a float64 factor and rounds the product to a whole
// minor unit under the given mode.
//
// THIS IS THE ONLY FUNCTION IN THE PACKAGE THAT LETS A float64 DETERMINE A
// Money VALUE. CLAUDE.md §12 permits floats for odds and probabilities and
// forbids them in balances; stake × decimal-odds is the one place those two
// worlds must meet, so it is concentrated here where it can be reviewed, and it
// refuses to proceed without an explicit rounding decision.
//
// Errors: ErrMoneyNotFinite for a NaN or infinite factor, ErrUnknownRounding
// for an unset mode, ErrMoneyOverflow if either the receiver or the rounded
// product leaves [MinSafeMoney, MaxSafeMoney].
func (m Money) MulFloat(factor float64, r Rounding) (Money, error) {
	if !r.Valid() {
		return 0, fmt.Errorf("%s × %v: %w", m, factor, ErrUnknownRounding)
	}
	if math.IsNaN(factor) || math.IsInf(factor, 0) {
		return 0, fmt.Errorf("%s × %v: %w", m, factor, ErrMoneyNotFinite)
	}
	if m > MaxSafeMoney || m < MinSafeMoney {
		return 0, fmt.Errorf("%d minor units × %v: %w", int64(m), factor, ErrMoneyOverflow)
	}

	// float64(m) is exact because |m| <= 2^53-1 was just checked.
	p := float64(m) * factor
	if math.IsNaN(p) || math.IsInf(p, 0) {
		return 0, fmt.Errorf("%s × %v: %w", m, factor, ErrMoneyOverflow)
	}

	var rounded float64
	switch r {
	case RoundHalfAwayFromZero:
		rounded = math.Round(p) // math.Round breaks ties away from zero.
	case RoundHalfToEven:
		rounded = math.RoundToEven(p)
	case RoundTowardZero:
		rounded = math.Trunc(p)
	default:
		return 0, fmt.Errorf("%s × %v: %w", m, factor, ErrUnknownRounding)
	}

	// Compare in float space before converting: converting a float64 outside
	// the int64 range is implementation-defined in Go, so the guard has to come
	// first rather than after.
	if rounded > float64(MaxSafeMoney) || rounded < float64(MinSafeMoney) {
		return 0, fmt.Errorf("%s × %v: %w", m, factor, ErrMoneyOverflow)
	}
	return Money(rounded), nil
}

// Float64ForDisplayOnly converts to major units as a float64.
//
// The name is the warning. The result is lossy above 2^53 minor units and
// carries binary-fraction error for any amount whose cents are not a dyadic
// fraction, so it must never be stored, summed, compared for equality, or
// converted back into a Money. Its only legitimate use is handing a number to a
// chart or a formatter that demands one. Everything that has to be right uses
// MinorUnits or String.
func (m Money) Float64ForDisplayOnly() float64 {
	return float64(m) / float64(MinorUnitsPerMajor)
}

// String renders the amount in major units with exactly two decimal places and
// a leading minus for negatives: "123.45", "-0.07", "0.00".
//
// It is computed entirely in integer arithmetic — no float is constructed —
// so the rendering is exact for every representable Money.
func (m Money) String() string {
	major, minor, sign := m.Split()
	if sign < 0 {
		return "-" + strconv.FormatInt(major, 10) + "." + twoDigits(minor)
	}
	return strconv.FormatInt(major, 10) + "." + twoDigits(minor)
}

// twoDigits renders 0..99 zero-padded to two characters.
func twoDigits(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// ParseMoney parses a signed decimal amount in major units into minor units.
//
// The grammar is deliberately narrow:
//
//	money  := sign? digits ( '.' frac )?
//	sign   := '+' | '-'
//	digits := [0-9]+
//	frac   := [0-9] | [0-9][0-9]
//
// Accepted: "0", "12", "12.3" (→ 1230), "12.34", "-0.07", "+5".
// Rejected: "" and " 12" (ErrMoneySyntax), ".5" and "12." (ErrMoneySyntax —
// at least one integer digit and, if a point appears, at least one fraction
// digit), "$12" and "1,234" (ErrMoneySyntax — currency symbols and grouping
// are a presentation concern), "12.345" (ErrMoneyPrecision).
//
// Rejecting excess precision instead of rounding it is the important choice. A
// parser that turns "12.345" into 12.34 or 12.35 without saying so puts a
// rounding decision somewhere nobody will ever look at it.
func ParseMoney(s string) (Money, error) {
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}

	var mag uint64
	intDigits := 0
	const maxDiv10 = math.MaxUint64 / 10
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		d := uint64(s[i] - '0')
		if mag > maxDiv10 || (mag == maxDiv10 && d > math.MaxUint64%10) {
			return 0, fmt.Errorf("parse money %q: %w", sample(s), ErrMoneyOverflow)
		}
		mag = mag*10 + d
		i++
		intDigits++
	}
	if intDigits == 0 {
		return 0, fmt.Errorf("parse money %q: %w", sample(s), ErrMoneySyntax)
	}

	frac := int64(0)
	fracDigits := 0
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			if fracDigits == 2 {
				return 0, fmt.Errorf("parse money %q: %w", sample(s), ErrMoneyPrecision)
			}
			frac = frac*10 + int64(s[i]-'0')
			i++
			fracDigits++
		}
		if fracDigits == 0 {
			return 0, fmt.Errorf("parse money %q: %w", sample(s), ErrMoneySyntax)
		}
	}
	if i != len(s) {
		return 0, fmt.Errorf("parse money %q: %w", sample(s), ErrMoneySyntax)
	}
	if fracDigits == 1 {
		frac *= 10 // "12.3" means 12.30, not 12.03.
	}

	if mag > math.MaxUint64/uint64(MinorUnitsPerMajor) {
		return 0, fmt.Errorf("parse money %q: %w", sample(s), ErrMoneyOverflow)
	}
	total := mag * uint64(MinorUnitsPerMajor)
	if total > math.MaxUint64-uint64(frac) {
		return 0, fmt.Errorf("parse money %q: %w", sample(s), ErrMoneyOverflow)
	}
	total += uint64(frac)

	if total > uint64(MaxSafeMoney) {
		return 0, fmt.Errorf("parse money %q: %w", sample(s), ErrMoneyOverflow)
	}
	if neg {
		return Money(-int64(total)), nil
	}
	return Money(total), nil
}

// SumMoney adds any number of amounts, failing on the first overflow.
//
// It is the check the double-entry ledger runs: the entries of a balanced
// transaction sum to exactly ZeroMoney (CLAUDE.md §4).
func SumMoney(values ...Money) (Money, error) {
	total := ZeroMoney
	for i, v := range values {
		next, err := total.Add(v)
		if err != nil {
			return 0, fmt.Errorf("sum at index %d: %w", i, err)
		}
		total = next
	}
	return total, nil
}
