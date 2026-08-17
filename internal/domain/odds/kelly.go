package odds

import (
	"errors"
	"fmt"
	"math"
)

// This file implements CLAUDE.md §4's "Kelly / fractional-Kelly stake sizing" and
// the sizing half of §6's "Bankroll and Kelly staking calculator".
//
// # Fractions here, money elsewhere
//
// Every function in this file returns a FRACTION OF BANKROLL as a float64. None of
// them returns a stake, and this package deliberately does not know that
// internal/domain.Money exists.
//
// That is a decision, not an oversight, and the reasoning is worth recording
// because the obvious alternative looks tidier. The Money type, its
// overflow-checked arithmetic, and its one float escape hatch — MulFloat(factor,
// Rounding) — all live in internal/domain. Wiring a Money-denominated staking
// helper into this file would mean importing that package, and two facts rule it
// out:
//
// The reason is NOT that the import is unavailable. clv.go already imports
// internal/domain for the identifier types a closing line has to name, so the edge
// exists and adding a second use of it would cost nothing structurally. An earlier
// draft of this comment argued from a "zero non-standard-library dependencies"
// claim in doc.go that clv.go had already made false; that argument is withdrawn
// rather than quietly kept. What rules the helper out is narrower and still true:
//
//   - A stake is a decision, not a calculation. Turning a fraction of bankroll into
//     an integer number of minor units requires choosing a rounding mode, and the
//     choice is not neutral — truncation can never overpay, half-away-from-zero
//     can. That belongs to the code placing the wager, which knows whose money it
//     is, and not to a function whose entire job is the arithmetic of edge.
//   - Every function here is a pure function of two floats. A Money-denominated
//     variant would take a bankroll, a rounding mode and a set of limits, and would
//     be the first thing in this package that could fail for a reason that has
//     nothing to do with odds. Keeping that out is what makes the file exhaustively
//     testable against closed forms.
//
// So the seam is drawn at the fraction. The call site — internal/pricing and
// internal/betting, in later phases — owns the crossing:
//
//	f, err := odds.FractionalKelly(q, d, odds.QuarterKelly)
//	if err != nil {
//	    return err
//	}
//	stake, err := bankroll.MulFloat(f, domain.RoundTowardZero)
//
// Two properties make that two-liner safe, and both are asserted in this
// package's tests so the caller can rely on them:
//
//   - Every fraction returned here lies in [0, 1]. The stake can therefore never
//     exceed the bankroll, whatever the price and whatever the probability.
//   - domain.RoundTowardZero truncates. Applied to a non-negative product it
//     rounds DOWN, so the integer stake is never more than the real-valued Kelly
//     stake. Rounding to nearest would let it exceed Kelly by up to half a minor
//     unit, and over-betting Kelly is strictly worse than under-betting it: the
//     growth curve is asymmetric and falls away faster above the optimum than
//     below.
//
// Do not substitute another Rounding mode at the call site without reading that
// second point again.

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

// These sit here rather than in errors.go because errors.go is the conversion
// layer's error set and these are meaningless outside staking. They follow the
// same contract as the rest of the package: bare messages, wrapped with %w at the
// call site, matched with errors.Is and never on message text.
var (
	// ErrKellyMultiplierOutOfRange reports a fractional-Kelly multiplier outside
	// the half-open interval (0, 1]. Zero is excluded because "stake nothing" is
	// not a Kelly policy and is almost always a caller passing an unset field;
	// anything above 1 is leveraging past the Kelly optimum, which reduces the
	// long-run growth rate and eventually drives it negative.
	ErrKellyMultiplierOutOfRange = errors.New("kelly multiplier must lie in the interval (0, 1]")

	// ErrBankrollFractionOutOfRange reports a bankroll fraction outside the closed
	// interval [0, 1]. Unlike the Kelly multiplier, zero is admitted here: not
	// betting is a legitimate point on the growth curve and is its natural
	// baseline.
	ErrBankrollFractionOutOfRange = errors.New("bankroll fraction must lie in the interval [0, 1]")

	// ErrCertainRuin reports a request to evaluate the growth rate of staking the
	// entire bankroll on an outcome that is not certain. The honest answer is
	// negative infinity — one loss ends the sequence and the logarithm of zero
	// wealth is undefined — and returning that as a float64 would leak an infinity
	// into a caller's arithmetic. It is reported as an error instead.
	ErrCertainRuin = errors.New("staking the entire bankroll on an uncertain outcome guarantees eventual ruin")
)

// -----------------------------------------------------------------------------
// Standard bankroll policies
// -----------------------------------------------------------------------------

// Multipliers for [FractionalKelly]. They are conventions, not constraints — any
// value in (0, 1] is accepted — but naming the three that are actually used stops
// bare 0.25 literals from spreading through the staking code.
//
// Full Kelly maximises the expected logarithm of bankroll and is, in the long run
// and with a *correct* probability estimate, unbeatable. Practitioners stake a
// fraction of it anyway, for a reason that matters here: Kelly's optimality is
// exquisitely sensitive to the accuracy of q. A probability estimate that is too
// high by a few points turns full Kelly into systematic over-betting, and the
// growth curve punishes over-betting far harder than under-betting. Half Kelly
// gives up roughly a quarter of the growth rate for roughly half the volatility
// and a large margin against a mis-estimated edge; quarter Kelly is the common
// choice when the probability comes from a devigged market rather than a validated
// model, which is exactly this system's situation.
const (
	FullKelly    = 1.0
	HalfKelly    = 0.5
	QuarterKelly = 0.25
)

// -----------------------------------------------------------------------------
// Kelly
// -----------------------------------------------------------------------------

// Kelly returns the full Kelly stake as a FRACTION OF BANKROLL — a float64 in
// [0, 1], never a money amount. See the file's opening comment for how to turn it
// into an integer stake.
//
// # Formula
//
// Writing b = d − 1 for the profit per unit staked on a win, the classical Kelly
// criterion is
//
//	f* = (b·q − (1 − q)) / b
//
// which expands and regroups to the form implemented here:
//
//	f* = (q·d − 1) / (d − 1)
//
// The numerator is exactly [ExpectedValue], so Kelly is "expected value per unit
// staked, divided by the profit per unit staked". That reading is the useful one:
// it says a given edge justifies a larger stake at a short price than at a long
// one, because the same edge is being collected with less variance.
//
// # No bet is a valid answer, and it is the usual one
//
// f* ≤ 0 means the price is not worth taking. This returns exactly zero rather
// than a negative fraction. A negative Kelly fraction is not "bet small", it is
// "take the other side", and the other side is a different selection with its own
// price and its own margin — it is emphatically not this wager at a negative
// stake. Returning a negative number here would eventually be multiplied by a
// bankroll and produce a negative stake, which the ledger would have to reject
// somewhere much further from the mistake.
//
// # Kelly is exactly zero at zero edge
//
// CLAUDE.md §4 names this invariant, and it holds exactly rather than
// approximately: for every legal decimal price d,
//
//	Kelly(BreakevenProbability(d), d) == 0
//
// with no tolerance. The proof is short and worth writing down, because the
// obvious implementation does not have this property.
//
// BreakevenProbability(d) computes q = fl(1/d), the correctly-rounded reciprocal.
// By the definition of round-to-nearest, fl(1/d) = (1/d)·(1 + ε) with
// |ε| ≤ u/(1+u) < u, where u = 2⁻⁵³. The exact real product is therefore
// d · fl(1/d) = 1 + ε. Rounding that back to a double: the doubles adjacent to 1
// from above are 1 and 1 + 2⁻⁵², so the rounding tie point above 1 sits at exactly
// 1 + 2⁻⁵³. Since 1 + ε < 1 + 2⁻⁵³ strictly, it rounds down to 1. Hence
//
//	fl(d · fl(1/d)) ∈ { 1 − 2⁻⁵³, 1 }   for every d whose reciprocal is normal
//
// so it is never above 1, and the guard below — a test on the gross return, taken
// *before* any subtraction — returns zero in both cases. Note the guard is on q·d
// against 1, not on the computed EV against zero: the product cannot be contracted
// with anything, so this argument does not depend on whether the compiler fuses
// the later subtraction.
//
// This is strictly stronger than what [ExpectedValue] can promise. When the gross
// return lands on the lower of the two values, EV at the break-even price is
// −2⁻⁵³ rather than 0 — negative, correctly, but not zero. Kelly collapses both
// cases to an exact zero because "stake nothing" has to be exact: a fraction of
// 1e-16 is not a rounding curiosity once it reaches a bankroll and a ledger.
//
// The one honest gap: the relative-error bound above assumes fl(1/d) is a normal
// double. It becomes subnormal for d greater than about 4.5e307, where the bound
// degrades to an absolute one and the product can round to just above 1. The
// resulting fraction is on the order of 1e-324 and multiplies any realistic
// bankroll to zero minor units, so the practical consequence is nil — but the
// invariant is stated as proven for normal reciprocals rather than claimed
// universally.
//
// # Kelly never exceeds the bankroll
//
// f* ≤ 1 is a theorem, not just a clamp. Since q ≤ 1, the real inequality
// q·d ≤ d holds, and rounding to nearest is monotone, so fl(q·d) ≤ d and
// fl(fl(q·d) − 1) ≤ fl(d − 1). The quotient of a numerator no larger than its
// denominator, correctly rounded, is at most fl(1) = 1.
//
// The cap below is therefore a no-op on every input, and is kept anyway: it costs
// one comparison, it is the only line that would contain the damage if a future
// edit broke that chain, and it would clamp an infinity if one ever appeared. It
// is written as min rather than as an if, because a branch that is provably never
// taken is a permanent hole in the coverage that CLAUDE.md §10 requires of this
// package — and "unreachable, so we stopped measuring it" is exactly how a branch
// that has quietly become reachable goes unnoticed.
//
// f* = 1 is reached, exactly, at q = 1: a certain winner justifies the whole
// bankroll, which is the correct answer to the question asked and the reason a
// caller must never feed this a probability it is not actually certain of.
//
// # No edge threshold
//
// Like [ExpectedValue], this applies no minimum-edge floor. A one-basis-point edge
// produces a genuine, tiny Kelly fraction. Deciding that an edge is inside the
// noise of the probability estimate is a risk policy for the staking layer.
//
// Errors: the validation errors of q and d.
func Kelly(q Probability, d Decimal) (float64, error) {
	if err := q.Validate(); err != nil {
		return 0, err
	}
	if err := d.Validate(); err != nil {
		return 0, err
	}

	// Branch on the gross return rather than on the expected value. See the doc
	// comment: fl(q·d) ≤ 1 is provable at the break-even probability, and a bare
	// multiplication cannot be contracted, so the zero-at-zero-edge invariant does
	// not rest on how the compiler treats the subtraction below. q and d are
	// validated, so the product is finite and the comparison cannot see a NaN.
	g := grossReturn(q, d)
	if g <= 1 {
		return 0, nil
	}

	// d > 1 is established, so b > 0. For d in (1, 2) the subtraction is exact by
	// Sterbenz's lemma, which is what stops an extremely short price from
	// producing a zero denominator.
	b := float64(d) - 1
	f := (g - 1) / b

	// A no-op for every input, by the monotonicity argument in the doc comment.
	return min(f, 1), nil
}

// FractionalKelly returns [Kelly] scaled by multiplier — still a FRACTION OF
// BANKROLL in [0, 1], never a money amount.
//
// multiplier is the fraction OF KELLY to stake, not the fraction of bankroll:
// pass [HalfKelly] for half-Kelly staking. It must lie in (0, 1]. The two
// "fractions" in play are the reason both are spelled out here — the argument
// scales the result, it is not the result.
//
// Scaling is applied to the finished Kelly fraction rather than folded into the
// formula, so every guarantee [Kelly] makes survives: a break-even price returns
// exactly zero at any multiplier, and the result cannot exceed the bankroll
// because both operands lie in [0, 1] and rounding to nearest is monotone.
//
// The multiplier is validated before the price is, because passing an unset or
// percentage-scaled multiplier is by far the likelier caller error and the message
// should name it rather than a price that may be perfectly fine.
//
// Errors: ErrNotFinite for a NaN or infinite multiplier;
// ErrKellyMultiplierOutOfRange outside (0, 1]; otherwise the validation errors of
// q and d.
func FractionalKelly(q Probability, d Decimal, multiplier float64) (float64, error) {
	if math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		return 0, fmt.Errorf("odds: kelly multiplier %v: %w", multiplier, ErrNotFinite)
	}
	if multiplier <= 0 || multiplier > 1 {
		return 0, fmt.Errorf("odds: kelly multiplier %g: %w", multiplier, ErrKellyMultiplierOutOfRange)
	}

	f, err := Kelly(q, d)
	if err != nil {
		return 0, err
	}
	return f * multiplier, nil
}

// -----------------------------------------------------------------------------
// Why Kelly is the optimum
// -----------------------------------------------------------------------------

// GrowthRate returns the expected logarithmic growth rate of a bankroll per wager,
// for a stake of the given fraction of bankroll on an outcome of fair probability
// q at decimal price d:
//
//	g(f) = q·ln(1 + f·(d − 1)) + (1 − q)·ln(1 − f)
//
// A bankroll compounding at rate g multiplies by e^g per wager, so g is the
// quantity a long-run bettor is actually maximising — not expected profit, which
// is maximised by staking everything and which therefore guarantees ruin.
//
// [Kelly] returns the f that maximises this function. That relationship is what
// makes GrowthRate worth exporting rather than leaving as a comment: it gives
// CLAUDE.md §6's bankroll calculator the curve to plot beside the recommended
// stake, it lets a caller quantify what half-Kelly actually costs in growth, and
// it gives the test suite a way to verify the Kelly formula against its own
// definition instead of against a restatement of itself. A property test that
// asserts g(f*) ≥ g(f) for arbitrary competing f is a far stronger statement about
// correctness than any table of expected values.
//
// # Domain and edges
//
// fraction lies in the closed interval [0, 1]. Zero is admitted and returns
// exactly 0 — not betting neither grows nor shrinks a bankroll — which is the
// baseline every other value is judged against.
//
// The two logarithms are evaluated with math.Log1p rather than math.Log of a sum.
// The arguments are 1 + f·b and 1 − f, and realistic fractional-Kelly stakes make
// both of those very close to 1, which is precisely the region where forming the
// sum first discards the significant digits. Log1p keeps them.
//
// Each term is skipped rather than computed when its probability weight is zero,
// because 0·ln(x) is 0 in mathematics and NaN in IEEE-754 when x is zero: at q = 1
// the losing branch would evaluate 0·ln(1−f), and at f = 1 that is 0·(−∞) = NaN.
// Skipping is not an optimisation.
//
// fraction = 1 with q < 1 is rejected with ErrCertainRuin rather than returning
// negative infinity. Staking everything on an outcome that can lose ends the
// sequence on the first loss; −∞ is the correct value and a terrible return type.
// fraction = 1 with q = 1 is fine and returns ln(d).
//
// Errors: the validation errors of q and d; ErrNotFinite for a non-finite
// fraction; ErrBankrollFractionOutOfRange outside [0, 1]; ErrCertainRuin as above.
func GrowthRate(q Probability, d Decimal, fraction float64) (float64, error) {
	if err := q.Validate(); err != nil {
		return 0, err
	}
	if err := d.Validate(); err != nil {
		return 0, err
	}
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) {
		return 0, fmt.Errorf("odds: bankroll fraction %v: %w", fraction, ErrNotFinite)
	}
	if fraction < 0 || fraction > 1 {
		return 0, fmt.Errorf("odds: bankroll fraction %g: %w", fraction, ErrBankrollFractionOutOfRange)
	}

	qf := float64(q)
	if fraction >= 1 && qf < 1 {
		return 0, fmt.Errorf("odds: bankroll fraction %g at probability %g: %w",
			fraction, qf, ErrCertainRuin)
	}

	b := float64(d) - 1
	growth := 0.0
	if qf > 0 {
		growth += qf * math.Log1p(fraction*b)
	}
	if qf < 1 {
		growth += (1 - qf) * math.Log1p(-fraction)
	}
	return growth, nil
}
