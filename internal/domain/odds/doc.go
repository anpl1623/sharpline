// Package odds implements the value types and format conversions that sit at the
// centre of Sharpline's pricing (CLAUDE.md §4).
//
// The package is pure. It has zero non-standard-library dependencies, performs no
// I/O, reads no clock, holds no package-level mutable state beyond the sentinel
// error values, and never panics. Every function is deterministic: the same inputs
// always produce the same outputs. Anything that can fail returns an error; nothing
// silently returns NaN, ±Inf, or a half-computed answer.
//
// # The four representations
//
// A price for a selection can be written four ways. All four are modelled as
// distinct named types so that a Decimal can never be passed where a Probability is
// expected:
//
//	American     int64    the US convention: +150, -110
//	Decimal      float64  the European convention: total return per unit staked
//	Fractional   {n, d}   the UK convention: 5/2, 10/11
//	Probability  float64  implied probability in [0, 1]
//
// # Decimal is canonical
//
// Decimal is the canonical internal representation and the one every other package
// in this repository should store, transport, and compute with.
//
//   - It is total over the whole price range, where American is not: American odds
//     are integers, so most real-valued prices are not expressible in it at all.
//   - It is lossless, where Fractional is not: an arbitrary decimal has no exact
//     small-denominator rational form.
//   - It composes multiplicatively, which is what parlay pricing needs: the decimal
//     price of independent legs is the product of the leg prices.
//
// American and Fractional are display formats (CLAUDE.md §6, "odds format toggle").
// Convert to them at the edge, render, and throw the result away. Never round-trip a
// computed price through American or Fractional and then keep using it.
//
// # Conversion formulas
//
// Each is named by what it computes; the derivation is in the doc comment on the
// function itself.
//
//	American → Decimal    A ≥ +100:  d = 1 + A/100        (stake 100 to win A)
//	                      A ≤ -100:  d = 1 + 100/|A|      (stake |A| to win 100)
//	Decimal  → American   d ≥ 2:     A = (d-1)·100
//	                      d <  2:    A = -100/(d-1)
//	Decimal  → Probability           p = 1/d
//	Probability → Decimal            d = 1/p
//	Fractional → Decimal             d = 1 + n/den        (stake den to win n)
//	Decimal → Fractional             best rational approximation of d-1
//
// Note that p = 1/d is the *implied* probability, which includes the book's margin.
// It is not a fair probability and does not sum to 1 across a market. Removing the
// margin is devigging and lives elsewhere in this package. The distinction between
// overround (Σp), booking percentage (100·Σp), and vig/hold (1 - 1/Σp) is a classic
// trap; each of those is defined at its own implementation site, not here.
//
// # +100 and -100 are the same price
//
// American odds are redundant at even money: +100 and -100 both mean "stake one unit
// to win one unit", decimal 2.0. NewAmerican and every conversion that produces an
// American therefore canonicalise -100 to +100, exactly as NewFractional reduces
// 6/4 to 3/2. A raw American(-100) is still accepted as valid input and still
// converts to decimal 2.0; it simply does not survive a round trip unchanged. This
// is the only value for which American → Decimal → American is not the identity, and
// it is tested explicitly.
//
// # Rounding, and what round trips actually guarantee
//
// American odds are integral, so Decimal → American is lossy. The rounding
// convention is round-half-away-from-zero (the semantics of math.Round). It was
// chosen because it is symmetric under the sign of the American price — a favourite
// and its mirror-image underdog round consistently — and because it can never
// produce a value inside the illegal (-100, +100) band: the positive branch is
// bounded below by exactly 100 and the negative branch is strictly below -100.
//
// The guarantees, stated as they are tested rather than as would be convenient:
//
//   - American → Decimal → American is the identity for every legal, canonical
//     American price. This is verified by exhaustive enumeration, not by sampling.
//   - Decimal → American → Decimal has absolute error at most 0.005 in decimal odds.
//     Proof: rounding moves the real-valued American price by at most 0.5. On the
//     positive branch that is 0.5/100 = 0.005 in decimal. On the negative branch the
//     decimal error is 100·|ΔA|/(|A|·|A'|) ≤ 100·0.5/100² = 0.005, since both
//     magnitudes are at least 100.
//   - Decimal ↔ Probability round trips to within a relative error of about 2^-52.
//     It is two divisions, so it is not bit-exact and is not asserted to be.
//   - Fractional → Decimal → Fractional is the identity for every fraction already
//     in lowest terms with denominator ≤ MaxFractionalDenominator.
//
// # Ranges and limits
//
//   - Decimal is valid iff it is finite and strictly greater than 1. Exactly 1.0 means
//     a zero payout and an implied probability of exactly 1; it is not a price.
//   - Probability is valid on the closed interval [0, 1], because 0 and 1 are
//     legitimate probabilities for a settled outcome. Converting either to odds is a
//     separate, narrower requirement: only the open interval (0, 1) is priceable, and
//     0 or 1 returns ErrProbabilityNotPriceable rather than ±Inf or decimal 1.0.
//   - American magnitude is bounded to [MinAmericanMagnitude, MaxAmericanMagnitude],
//     i.e. 100 ≤ |A| ≤ 1,000,000. The lower bound is the real illegal band. The upper
//     bound is a precision bound, not an arbitrary one: the decimal → American step
//     divides by (d-1), which loses relative precision as d approaches 1, and the
//     absolute error in the recovered American price grows as ≈2.2×10⁻¹⁸·A². Round
//     tripping stops being exact around |A| ≈ 4.7×10⁸; the limit is set nearly three
//     orders of magnitude below that, where the worst-case error is ≈2×10⁻⁶ — five
//     decimal places of margin against the 0.5 needed to round correctly. A price of
//     ±1,000,000 is a 10,000-to-1 shot, far beyond anything a book quotes.
//   - Fractional denominators are bounded to MaxFractionalDenominator. Prices shorter
//     than roughly 1/1000 (decimal below ≈1.001) have no fractional form within that
//     bound and return ErrFractionalNotRepresentable; callers rendering a format
//     toggle should fall back to another format rather than treat this as fatal.
//
// # Non-finite input
//
// NaN and ±Inf are rejected at the boundary with ErrNotFinite and are never
// propagated. This matters more than it looks: NaN compares false against every
// bound, so a naive range check silently accepts it and the poison then spreads
// through an entire pricing slate.
//
// # Errors
//
// Sentinel errors are declared in errors.go and are the stable part of the contract;
// match with errors.Is, never on message text. Every error returned from this package
// carries the offending value in its message, is prefixed "odds:" exactly once, and
// wraps its sentinel with %w. Conversions that fail because their *input* is invalid
// return the validation error unchanged rather than re-wrapping it, because the
// validation message already names the value and the violated constraint; adding a
// second layer would only repeat the prefix.
//
// # Display
//
// String is defined on American, Fractional, and Format — types whose in-memory form
// is not self-describing (a bare int64 loses the leading "+", a struct prints as
// "{10 11}"). It is deliberately *not* defined on Decimal or Probability: those are
// float64-kinded, fmt already prints them losslessly, and a String method would
// silently truncate them in logs. Product-facing rendering for all four formats is in
// format.go behind Render and the Render* functions, which do validate.
package odds
