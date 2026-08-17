package odds

import (
	"fmt"
	"math"
)

// This file implements the value half of CLAUDE.md §4: "No-vig fair odds and fair
// probability" and "Expected value %, edge". Stake sizing is in kelly.go.
//
// Everything here is a pure function of value types. Nothing reads a clock, no
// function has package-level state, and no function panics. Non-finite input is
// rejected at the boundary by the same Validate methods the conversions use, so
// NaN never reaches an arithmetic expression.
//
// # Vocabulary, fixed once
//
// Two probabilities appear throughout and confusing them is the defining bug of
// this domain:
//
//	p  the book's RAW IMPLIED probability, p = 1/d. It includes the margin, and
//	   across a market the p values sum to more than 1.
//	q  a FAIR probability. The margin has been removed — by devigging a market, by
//	   a model, or by a sharp reference book — and across a market the q values sum
//	   to 1.
//
// Every function below takes q where it means "what we believe" and p or d where
// it means "what is on offer". A function fed p in place of q computes the edge of
// a price against itself, which is always zero or negative; that is not an error
// and is not rejected, because it is exactly the "no edge here" answer the +EV
// finder needs for the overwhelming majority of the slate.

// -----------------------------------------------------------------------------
// Fair value
// -----------------------------------------------------------------------------

// FairDecimal returns the fair decimal price implied by a fair probability:
//
//	d_fair = 1 / q
//
// This is the price at which a wager on an outcome of true probability q has zero
// expected value — the break-even price. It is the number the +EV finder
// (CLAUDE.md §6) compares a book's offer against: an offer longer than d_fair is
// positive expected value, an offer shorter is negative.
//
// The input is a fair probability, meaning the margin has already been removed.
// Feeding it a raw implied probability returns the price it came from and reports
// no edge, which is arithmetically correct and analytically useless.
//
// It delegates to [Probability.Decimal] rather than writing 1/q again. That is
// deliberate: internal/domain's package documentation warns that "two
// implementations of the same formula will diverge", and this package is the one
// place in the repository where that would be fatal. FairDecimal exists for the
// name — "fair odds" is the vocabulary the pricing service and the API speak — not
// for a second copy of the arithmetic.
//
// Errors: ErrNotFinite or ErrProbabilityOutOfRange for a q outside [0, 1];
// ErrProbabilityNotPriceable for q of exactly 0 (infinite odds) or exactly 1 (a
// zero payout), and for a q so close to 0 that 1/q overflows.
func FairDecimal(q Probability) (Decimal, error) {
	return q.Decimal()
}

// BreakevenProbability returns the probability at which a decimal price is exactly
// break-even:
//
//	q_breakeven = 1 / d
//
// It is the inverse of [FairDecimal] and is numerically identical to the price's
// raw implied probability — the same number wearing a different hat. The two names
// exist because they answer different questions. "Implied probability" describes
// the price ("this book is charging as though this were a 52.4% shot"). "Breakeven
// probability" describes the decision ("I need to be right more than 52.4% of the
// time for this to profit"), and it is the label CLAUDE.md §6's bankroll
// calculator puts on the number.
//
// Like [FairDecimal] it delegates, here to [Decimal.Probability], so there is
// exactly one implementation of the reciprocal in this package.
//
// Errors: ErrNotFinite or ErrDecimalOutOfRange for a d that is not a finite value
// strictly greater than 1.
func BreakevenProbability(d Decimal) (Probability, error) {
	return d.Probability()
}

// -----------------------------------------------------------------------------
// Expected value
// -----------------------------------------------------------------------------

// ExpectedValue returns the expected PROFIT PER UNIT STAKED of backing an outcome
// of fair probability q at decimal price d. It is a multiple, not a percentage:
// 0.05 means five percent. Use [ExpectedValuePercent] for the percentage.
//
// # Derivation
//
// A unit stake at decimal price d returns d in total when it wins — the stake plus
// d-1 of profit — and returns nothing when it loses, forfeiting the unit staked.
// So the expected profit is
//
//	EV = q·(d − 1) − (1 − q)·1
//	   = q·d − q − 1 + q
//	   = q·d − 1
//
// The second form is the one implemented: it is one multiply and one subtract
// rather than four operations, so it accumulates less rounding error, and it makes
// the break-even condition q·d = 1 immediate.
//
// # Range
//
// EV lies in [−1, d−1]. It is −1 exactly when q = 0 (the stake is certainly lost)
// and d−1 exactly when q = 1 (the profit is certain).
//
// # At the break-even price it is never positive
//
// Fed q = BreakevenProbability(d), the answer is zero or one unit in the last
// place below it — never above. That one-sided guarantee, not an exact zero, is
// what is actually provable, and it is what [Kelly] needs.
//
// BreakevenProbability(d) is fl(1/d), the correctly-rounded reciprocal, so
// fl(1/d) = (1/d)(1 + ε) with |ε| < 2⁻⁵³ and the exact real product d·fl(1/d) is
// 1 + ε. Rounding that back: the doubles bracketing it are 1 − 2⁻⁵³, 1, and
// 1 + 2⁻⁵², so the tie point above 1 sits at 1 + 2⁻⁵³ and the exact product is
// strictly below it. Hence
//
//	fl(d · fl(1/d)) ∈ { 1 − 2⁻⁵³, 1 }
//
// and subtracting 1 from either is exact. So EV at the break-even price is either
// exactly 0 or exactly −2⁻⁵³ ≈ −1.11e-16, and never a positive value that would
// invite a stake on a price carrying no edge.
//
// Which of the two comes out is a property of the individual price and is not
// worth predicting; what matters is the sign. [Kelly] converts the guarantee into
// an exact zero by testing the gross return against 1 before it subtracts.
//
// The bound assumes fl(1/d) is a normal double. It becomes subnormal above about
// 4.5e307, where the argument degrades; see Kelly's doc comment for why the
// practical consequence is nil.
//
// # Why the arithmetic is written the way it is
//
// Two details are load-bearing and must survive editing.
//
// First, the product q·d is computed inside [grossReturn], behind an explicit
// float64 conversion that acts as a rounding barrier. Without it the compiler is
// permitted — and on arm64 actually chooses — to contract q·d − 1 into a single
// fused multiply-subtract with only one rounding. That is a *more* accurate
// answer, but a different one, and it differs between the arm64 development
// machine and the amd64 server. A pricing number that changes with the host
// architecture is not reproducible, and CLV comparisons stored in Postgres would
// disagree across a deployment.
//
// Second, the exact break-even case is short-circuited. When the gross return
// rounds to exactly 1 this returns a literal zero rather than computing 1 − 1, so
// that answer holds whatever the compiler does with the subtraction. The
// comparison against 1 is a test for one exact, representable value on the way to
// an exact answer — not an equality test on two computed floats, which this
// package never does.
//
// # What this deliberately does not do
//
// It applies no minimum-edge threshold. An EV of 1e-16 is returned as 1e-16 rather
// than being flattened to zero. Deciding that an edge is too small to act on is a
// risk policy that belongs to the +EV finder and to the staking rules, where it can
// be configured and reviewed; burying a magic epsilon in the arithmetic would make
// that policy invisible.
//
// Errors: the validation errors of q and d. Note that q of exactly 0 or exactly 1
// is accepted here — both are meaningful beliefs about an outcome, and neither
// requires converting q to a price.
func ExpectedValue(q Probability, d Decimal) (float64, error) {
	if err := q.Validate(); err != nil {
		return 0, err
	}
	if err := d.Validate(); err != nil {
		return 0, err
	}

	g := grossReturn(q, d)
	if g == 1 {
		// Exactly break-even. See the doc comment: returning the literal makes the
		// zero independent of floating-point contraction in the subtraction below.
		return 0, nil
	}
	return g - 1, nil
}

// ExpectedValuePercent returns [ExpectedValue] expressed as a PERCENTAGE: 5.0
// means five percent, where ExpectedValue would return 0.05.
//
// The pair exists because "EV" is used for both readings in the betting
// literature and the ambiguity is a routine source of stake-sizing errors that are
// off by a factor of one hundred. Neither name is abbreviated and neither is the
// default; a caller has to say which it wants.
//
// It scales the result of ExpectedValue rather than computing (q·d − 1)·100
// directly, so the sign guarantee at the break-even price carries over unchanged.
//
// The scaling is checked. EV is bounded above by d−1, and Decimal admits prices
// past 1.7e306, so multiplying by 100 can overflow to infinity on a price no book
// would ever post. That is reported rather than returned: an infinity here would
// spread through every aggregate downstream, and it is precisely the case a range
// check on the input would not have caught.
//
// Errors: the validation errors of q and d; ErrNotFinite if the percentage
// overflows.
func ExpectedValuePercent(q Probability, d Decimal) (float64, error) {
	ev, err := ExpectedValue(q, d)
	if err != nil {
		return 0, err
	}

	pct := ev * 100
	if math.IsInf(pct, 0) {
		return 0, fmt.Errorf("odds: expected value %v at decimal %g overflows as a percentage: %w",
			ev, float64(d), ErrNotFinite)
	}
	return pct, nil
}

// Edge returns the proportional advantage of a fair probability q over a book's
// raw implied probability p:
//
//	Edge = q/p − 1
//
// It is a multiple, not a percentage: 0.05 means the outcome is five percent more
// likely than the price says. Read it as "we are 5% over the market".
//
// # Relationship to expected value
//
// Edge and [ExpectedValue] are the same number whenever p is the implied
// probability of the very price being bet. Substituting p = 1/d, which is exactly
// what [Decimal.Probability] computes:
//
//	Edge = q/p − 1 = q/(1/d) − 1 = q·d − 1 = EV
//
// This identity is worth internalising rather than rediscovering: the "percentage
// edge" a trader quotes in probability space and the "expected value" a bettor
// quotes in stake space are one quantity. It is also why [Kelly] can be written
// with either in its numerator.
//
// The two functions are kept separate for two reasons. Edge accepts a p from
// anywhere — a second book's line, the same book's opener, a closing price — where
// ExpectedValue is tied to the price on the ticket. And the identity is exact only
// in real arithmetic: Edge divides by p where ExpectedValue multiplies by
// d = fl(1/p), so in float64 the two can differ in the last unit in the last
// place. The tests assert agreement to a relative tolerance, never equality.
//
// # Domain
//
// p of exactly 0 is rejected: it is a legal probability but the quotient is
// undefined, and a book cannot offer a price on an outcome it believes impossible.
// p of exactly 1 is accepted and yields q − 1 ≤ 0, which is the correct answer —
// there is no edge to be had against a book that has priced an outcome as certain.
// A p small enough that q/p overflows to infinity is rejected rather than returned.
//
// Errors: the validation errors of q and p; ErrProbabilityNotPriceable for p = 0;
// ErrNotFinite if the quotient overflows.
func Edge(q, p Probability) (float64, error) {
	if err := q.Validate(); err != nil {
		return 0, err
	}
	if err := p.Validate(); err != nil {
		return 0, err
	}

	pf := float64(p)
	if pf <= 0 {
		return 0, fmt.Errorf("odds: edge against implied probability %g is undefined: %w",
			pf, ErrProbabilityNotPriceable)
	}

	ratio := float64(q) / pf
	if math.IsInf(ratio, 0) {
		// Reachable only for a subnormal p, where 1/p is not representable. The
		// quotient, not the input, is what failed, so the message says so.
		return 0, fmt.Errorf("odds: edge of %g over implied probability %g overflows: %w",
			float64(q), pf, ErrNotFinite)
	}
	if ratio == 1 {
		// Exactly break-even, for the same reason ExpectedValue short-circuits.
		return 0, nil
	}
	return ratio - 1, nil
}

// EdgePercent returns [Edge] expressed as a PERCENTAGE: 5.0 means five percent,
// where Edge would return 0.05. See [ExpectedValuePercent] for why both spellings
// are provided rather than one implied convention, and for why the scaling is
// range-checked rather than assumed safe.
//
// Errors: those of Edge; ErrNotFinite if the percentage overflows.
func EdgePercent(q, p Probability) (float64, error) {
	e, err := Edge(q, p)
	if err != nil {
		return 0, err
	}

	pct := e * 100
	if math.IsInf(pct, 0) {
		return 0, fmt.Errorf("odds: edge %v over implied probability %g overflows as a percentage: %w",
			e, float64(p), ErrNotFinite)
	}
	return pct, nil
}

// -----------------------------------------------------------------------------
// The staking calculator's inverse questions
// -----------------------------------------------------------------------------

// MinimumDecimalForEdge returns the shortest decimal price at which backing an
// outcome of fair probability q still earns at least targetEdge per unit staked.
//
// targetEdge is a MULTIPLE, not a percentage: pass 0.03 for a three percent edge.
// Because edge and expected value are the same quantity (see [Edge]), the answer
// is the same whichever of the two the caller has in mind.
//
// # Derivation
//
//	q·d − 1 ≥ t   ⟺   q·d ≥ 1 + t   ⟺   d ≥ (1 + t)/q
//
// so the threshold price is d_min = (1 + t)/q. At t = 0 this is 1/q, the fair
// price, and the function agrees with [FairDecimal] bit for bit.
//
// This is the query behind a "show me every price worth taking" screen and behind
// the alerting side of the +EV finder: compute it once per selection from the
// devigged probability, then compare each book's offer against a single number
// instead of recomputing EV per book.
//
// # Domain
//
// A negative targetEdge is meaningful — it asks how much worse than fair a price
// may be before it is rejected — and is accepted. A targetEdge of −1 or below is
// not: it would make the threshold price zero or negative, which is not a price.
// A q of exactly 0 is rejected, because no finite price gives an edge on an
// outcome believed impossible.
//
// Errors: the validation errors of q; ErrNotFinite for a non-finite targetEdge;
// ErrProbabilityNotPriceable for q = 0; ErrDecimalOutOfRange if the threshold is
// not a legal price, which happens for targetEdge ≤ −1 and for a q so close to 1
// that the fair price rounds onto 1.0.
func MinimumDecimalForEdge(q Probability, targetEdge float64) (Decimal, error) {
	if err := q.Validate(); err != nil {
		return 0, err
	}
	if math.IsNaN(targetEdge) || math.IsInf(targetEdge, 0) {
		return 0, fmt.Errorf("odds: target edge %v: %w", targetEdge, ErrNotFinite)
	}

	qf := float64(q)
	if qf <= 0 {
		return 0, fmt.Errorf("odds: no finite price gives an edge on probability %g: %w",
			qf, ErrProbabilityNotPriceable)
	}

	d := Decimal((1 + targetEdge) / qf)
	if err := d.Validate(); err != nil {
		return 0, fmt.Errorf(
			"odds: probability %g at target edge %g needs decimal price %v, which is not a legal price: %w",
			qf, targetEdge, float64(d), unprefixed(err))
	}
	return d, nil
}

// -----------------------------------------------------------------------------
// Shared arithmetic
// -----------------------------------------------------------------------------

// grossReturn returns the expected GROSS return per unit staked, q·d — the stake
// plus the profit, not the profit alone. Subtracting one gives the expected value;
// comparing it against one is the break-even test.
//
// It is factored out so that [ExpectedValue] and [Kelly] share one multiplication
// rather than each writing their own, and so that the rounding barrier below is
// stated in exactly one place.
//
// The result is always finite for validated inputs: q lies in [0, 1] and d is
// finite, so the product lies in [0, d] and cannot overflow.
//
// # The conversion is not redundant
//
// The outer float64(...) around the product looks like a no-op and is not. The Go
// specification permits an implementation to fuse a multiply and a following
// add or subtract into one operation with a single rounding, "possibly across
// statements", and states that an explicit floating-point conversion "rounds to
// the precision of the target type, preventing fusion that would discard that
// rounding". Callers subtract 1 from this result; without the barrier that
// subtraction is contracted into a fused multiply-subtract on arm64 and left
// unfused on amd64, producing two different answers for the same inputs on the
// development Mac and the deployment server. Deleting this conversion silently
// makes the pricing engine architecture-dependent.
func grossReturn(q Probability, d Decimal) float64 {
	return float64(float64(q) * float64(d))
}
