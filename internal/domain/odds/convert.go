package odds

import (
	"fmt"
	"math"
)

// -----------------------------------------------------------------------------
// Value types
// -----------------------------------------------------------------------------

// American is a price in the US convention: a positive value is the profit on a
// stake of 100, a negative value is the stake required to profit 100. It is an
// int64 rather than a plain int so that the type is the same width on every
// platform, matching the int64 rule that CLAUDE.md §12 already imposes on money.
//
// Legal values are the integers with MinAmericanMagnitude ≤ |A| ≤
// MaxAmericanMagnitude. Nothing in the open interval (-100, +100) is a price, 0
// least of all.
type American int64

// Decimal is a price in the European convention: the total return, stake included,
// per unit staked. It is the canonical representation in this system; see the
// package documentation for why.
//
// Legal values are the finite float64 strictly greater than 1.
type Decimal float64

// Probability is an implied or fair probability. Legal values are the finite
// float64 in the closed interval [0, 1]. Note that only the open interval (0, 1) can
// be expressed as odds.
type Probability float64

// Fractional is a price in the UK convention: stake Denominator to win Numerator,
// stake returned. Both fields are strictly positive in a valid value, and a value
// produced by this package is always in lowest terms.
//
// The zero value is deliberately invalid, so a Fractional that was never initialised
// fails Validate rather than silently behaving as 0/0.
type Fractional struct {
	Numerator   int64
	Denominator int64
}

// -----------------------------------------------------------------------------
// Bounds
// -----------------------------------------------------------------------------

const (
	// MinAmericanMagnitude is the smallest legal absolute value of an American
	// price. Below it the price would be inside the (-100, +100) band, which
	// denotes nothing: at exactly ±100 the stake and the profit are equal, and
	// there is no way to write "less than even money" in the American convention
	// other than by flipping the sign.
	MinAmericanMagnitude int64 = 100

	// MaxAmericanMagnitude is the largest legal absolute value of an American
	// price. See the package documentation for the precision argument behind the
	// specific number; in short, exactness of the American → Decimal → American
	// round trip degrades as ≈2.2e-18·A² and this bound keeps the worst case around
	// 2e-6, five decimal places inside the 0.5 that correct rounding needs.
	MaxAmericanMagnitude int64 = 1_000_000

	// MaxFractionalDenominator bounds the denominator that Decimal.Fractional will
	// produce. 1000 covers the whole conventional betting ladder — 1/100 and 1/500
	// are the shortest fractions any book posts — while keeping the continued
	// fraction recurrence to a handful of terms.
	MaxFractionalDenominator int64 = 1000

	// MaxFractionalNumerator bounds the numerator that Decimal.Fractional will
	// produce. It exists to keep the int64 continued fraction recurrence provably
	// free of overflow; 1e12 is a trillion-to-one shot, so the bound never binds on
	// a real price.
	MaxFractionalNumerator int64 = 1_000_000_000_000
)

const (
	// evenDecimal is the decimal price at which the American convention flips sign.
	// A price of exactly 2.0 is even money and canonicalises to American +100.
	evenDecimal = 2.0

	// maxContinuedFractionTerms caps the continued fraction expansion in
	// bestRationalApproximation. Convergent denominators grow at least as fast as
	// the Fibonacci numbers, so a denominator bound of 1000 is reached within about
	// 16 terms; 64 is a generous ceiling that guarantees termination even if a
	// pathological float64 input produces a degenerate expansion.
	maxContinuedFractionTerms = 64

	// fractionalTolerance is the convergence criterion for
	// bestRationalApproximation: the expansion stops as soon as the current
	// convergent is within this absolute distance of the target.
	//
	// It is loose relative to float64 precision on purpose. Computing d-1 for a
	// price such as decimal 1.1 yields 0.10000000000000009, not the double nearest
	// 0.1, because the subtraction exposes the low bits that were rounded away when
	// 1 was added. A tolerance at the 1e-16 level would refuse to stop at 1/10 and
	// would keep expanding into noise terms. 1e-12 is four orders of magnitude above
	// that noise, and still six orders below the 1e-6 minimum separation between two
	// distinct rationals with denominators ≤ 1000, so it can never stop early on the
	// wrong fraction.
	fractionalTolerance = 1e-12
)

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

// Validate reports whether a is a legal American price, returning an error
// wrapping ErrAmericanOutOfRange if it is not.
func (a American) Validate() error {
	v := int64(a)
	switch {
	case v > -MinAmericanMagnitude && v < MinAmericanMagnitude:
		return fmt.Errorf("odds: american %d lies inside the illegal (-%d, +%d) band: %w",
			v, MinAmericanMagnitude, MinAmericanMagnitude, ErrAmericanOutOfRange)
	case v > MaxAmericanMagnitude || v < -MaxAmericanMagnitude:
		return fmt.Errorf("odds: american %d exceeds the representable magnitude %d: %w",
			v, MaxAmericanMagnitude, ErrAmericanOutOfRange)
	default:
		return nil
	}
}

// Valid reports whether a is a legal American price.
func (a American) Valid() bool { return a.Validate() == nil }

// Validate reports whether d is a legal decimal price, returning an error wrapping
// ErrNotFinite or ErrDecimalOutOfRange if it is not.
//
// The finiteness check comes first because NaN compares false against every bound:
// the range test alone would report "not greater than 1", which is true but useless.
func (d Decimal) Validate() error {
	f := float64(d)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("odds: decimal %v: %w", f, ErrNotFinite)
	}
	if f <= 1 {
		return fmt.Errorf("odds: decimal %g: %w", f, ErrDecimalOutOfRange)
	}
	return nil
}

// Valid reports whether d is a legal decimal price.
func (d Decimal) Valid() bool { return d.Validate() == nil }

// Validate reports whether p lies in the closed interval [0, 1], returning an error
// wrapping ErrNotFinite or ErrProbabilityOutOfRange if it does not.
//
// Note that 0 and 1 are valid probabilities and are accepted here. They are not
// priceable; that narrower check belongs to the conversions, which return
// ErrProbabilityNotPriceable.
func (p Probability) Validate() error {
	f := float64(p)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("odds: probability %v: %w", f, ErrNotFinite)
	}
	if f < 0 || f > 1 {
		return fmt.Errorf("odds: probability %g: %w", f, ErrProbabilityOutOfRange)
	}
	return nil
}

// Valid reports whether p lies in the closed interval [0, 1].
func (p Probability) Valid() bool { return p.Validate() == nil }

// Validate reports whether f is a legal fractional price, returning an error
// wrapping ErrFractionalNumerator or ErrFractionalDenominator if it is not.
func (f Fractional) Validate() error {
	if f.Numerator <= 0 {
		return fmt.Errorf("odds: fractional %d/%d: %w", f.Numerator, f.Denominator, ErrFractionalNumerator)
	}
	if f.Denominator <= 0 {
		return fmt.Errorf("odds: fractional %d/%d: %w", f.Numerator, f.Denominator, ErrFractionalDenominator)
	}
	return nil
}

// Valid reports whether f is a legal fractional price.
func (f Fractional) Valid() bool { return f.Validate() == nil }

// -----------------------------------------------------------------------------
// Constructors: validate, then canonicalise
// -----------------------------------------------------------------------------

// NewAmerican validates v and returns it in canonical form. The only
// canonicalisation is folding -100 onto +100; see Canonical.
func NewAmerican(v int64) (American, error) {
	a := American(v)
	if err := a.Validate(); err != nil {
		return 0, err
	}
	return a.Canonical(), nil
}

// NewDecimal validates v and returns it as a Decimal. There is nothing to
// canonicalise: every legal decimal price is already in its unique form.
func NewDecimal(v float64) (Decimal, error) {
	d := Decimal(v)
	if err := d.Validate(); err != nil {
		return 0, err
	}
	return d, nil
}

// NewProbability validates v and returns it as a Probability.
func NewProbability(v float64) (Probability, error) {
	p := Probability(v)
	if err := p.Validate(); err != nil {
		return 0, err
	}
	return p, nil
}

// NewFractional validates num/den and returns the fraction in lowest terms, so
// NewFractional(6, 4) yields 3/2.
func NewFractional(num, den int64) (Fractional, error) {
	f := Fractional{Numerator: num, Denominator: den}
	if err := f.Validate(); err != nil {
		return Fractional{}, err
	}
	return f.Reduce(), nil
}

// Canonical folds the redundant price -100 onto +100.
//
// American odds are ambiguous at even money: +100 and -100 both mean "stake one
// unit to win one unit", decimal 2.0. Every other price has exactly one American
// spelling. Collapsing the pair on construction is what makes
// American → Decimal → American a total identity; without it that round trip would
// have a single exception, and every caller would have to know about it.
//
// Canonical is a no-op on every other value, including invalid ones.
func (a American) Canonical() American {
	if a == American(-MinAmericanMagnitude) {
		return American(MinAmericanMagnitude)
	}
	return a
}

// Reduce returns f divided through by the greatest common divisor of its numerator
// and denominator, so 6/4 becomes 3/2. A value that is not strictly positive in
// both fields is returned unchanged, because there is no meaningful reduction of it
// and Reduce must not fail.
func (f Fractional) Reduce() Fractional {
	if f.Numerator <= 0 || f.Denominator <= 0 {
		return f
	}
	g := gcd(f.Numerator, f.Denominator)
	return Fractional{Numerator: f.Numerator / g, Denominator: f.Denominator / g}
}

// gcd returns the greatest common divisor of two strictly positive integers by the
// Euclidean algorithm. Both arguments must be > 0, which every call site enforces.
func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// -----------------------------------------------------------------------------
// American conversions
// -----------------------------------------------------------------------------

// Decimal converts an American price to decimal odds.
//
// A positive American price A is the profit on a stake of 100, so the total return
// per unit staked is (100 + A)/100 = 1 + A/100. A negative price A is the stake
// needed to profit 100, so the total return per unit staked is
// (|A| + 100)/|A| = 1 + 100/|A|.
//
// Both branches divide by a constant or by |A| ≥ 100, so neither can divide by zero
// once Validate has passed.
func (a American) Decimal() (Decimal, error) {
	if err := a.Validate(); err != nil {
		return 0, err
	}
	v := float64(a)
	if v > 0 {
		return Decimal(1 + v/100), nil
	}
	return Decimal(1 + 100/-v), nil
}

// Probability converts an American price to its implied probability, via decimal.
// The result includes the book's margin; it is not a fair probability.
func (a American) Probability() (Probability, error) {
	d, err := a.Decimal()
	if err != nil {
		return 0, err
	}
	return d.Probability()
}

// Fractional converts an American price to fractional odds. See Decimal.Fractional
// for the approximation guarantee and its limits.
func (a American) Fractional() (Fractional, error) {
	d, err := a.Decimal()
	if err != nil {
		return Fractional{}, err
	}
	return d.Fractional()
}

// -----------------------------------------------------------------------------
// Decimal conversions
// -----------------------------------------------------------------------------

// Probability converts decimal odds to the implied probability p = 1/d.
//
// The result is guaranteed to lie strictly inside (0, 1): d > 1 forces 1/d < 1, and
// d is finite so 1/d cannot underflow to zero — the reciprocal of the largest
// float64 is still a nonzero subnormal.
func (d Decimal) Probability() (Probability, error) {
	if err := d.Validate(); err != nil {
		return 0, err
	}
	return Probability(1 / float64(d)), nil
}

// American converts decimal odds to the nearest American price.
//
// Inverting the two branches of American.Decimal: for d ≥ 2 the price is a dog, and
// A = (d-1)·100 is the profit on a stake of 100. For d < 2 the price is a favourite,
// and A = -100/(d-1) is the stake that profits 100.
//
// The conversion is lossy, because American prices are integers. The real-valued
// result is rounded half away from zero. That rounding cannot land inside the
// illegal band: the first branch is bounded below by exactly 100, and the second is
// strictly below -100 because d-1 < 1 forces 100/(d-1) > 100. It can land on exactly
// -100, which Canonical then folds to +100.
//
// The division by d-1 is safe. Validate has established d > 1, and for d in (1, 2)
// the subtraction d-1 is exact by Sterbenz's lemma, so it yields the smallest
// positive normal difference rather than zero. Extremely short prices therefore
// produce a very large magnitude rather than an infinity, and are rejected by the
// range check as ErrAmericanOutOfRange.
func (d Decimal) American() (American, error) {
	if err := d.Validate(); err != nil {
		return 0, err
	}
	f := float64(d)

	var exact float64
	if f >= evenDecimal {
		exact = (f - 1) * 100
	} else {
		exact = -100 / (f - 1)
	}
	if math.IsNaN(exact) || math.IsInf(exact, 0) {
		return 0, fmt.Errorf("odds: decimal %g has no finite american price: %w", f, ErrAmericanOutOfRange)
	}

	rounded := math.Round(exact)
	if rounded > float64(MaxAmericanMagnitude) || rounded < -float64(MaxAmericanMagnitude) {
		return 0, fmt.Errorf("odds: decimal %g converts to american %.0f, beyond the representable magnitude %d: %w",
			f, rounded, MaxAmericanMagnitude, ErrAmericanOutOfRange)
	}

	a := American(int64(rounded)).Canonical()
	if err := a.Validate(); err != nil {
		return 0, err
	}
	return a, nil
}

// Fractional converts decimal odds to fractional odds, returning the best rational
// approximation of d-1. It is exact for every price that has a fraction in lowest
// terms with denominator ≤ MaxFractionalDenominator, which covers the entire
// conventional betting ladder; it is an approximation otherwise.
//
// Use FractionalApprox when the caller needs to know how good the approximation is.
func (d Decimal) Fractional() (Fractional, error) {
	f, _, err := d.FractionalApprox()
	return f, err
}

// FractionalApprox converts decimal odds to fractional odds and additionally
// reports the absolute error |Numerator/Denominator - (d-1)| of the result, so a
// caller can decide whether to render "5/2" or "≈5/2".
//
// # Algorithm and convergence criterion
//
// The target is x = d-1, the profit per unit staked. The simple continued fraction
// of x is expanded term by term and the convergents p_k/q_k are accumulated with the
// standard recurrence p_k = a_k·p_{k-1} + p_{k-2}, q_k = a_k·q_{k-1} + q_{k-2}.
// Expansion stops at the first of:
//
//   - |p_k/q_k - x| ≤ fractionalTolerance — converged;
//   - the next convergent would exceed MaxFractionalDenominator or
//     MaxFractionalNumerator, or would overflow int64 — bounded;
//   - the remainder reaches exactly zero, meaning x is rational and fully expanded;
//   - maxContinuedFractionTerms terms — a backstop that cannot be reached in
//     practice, since convergent denominators grow at least as fast as the Fibonacci
//     numbers and the denominator bound is hit within about sixteen terms.
//
// The last accepted convergent is returned in all cases. It is never a partial
// answer: every convergent is itself a complete, fully reduced rational
// approximation, and every convergent p/q of x satisfies |x - p/q| < 1/q². Unlike an
// iterative root finder there is no half-solved state to leak, which is why this
// function does not report non-convergence as an error.
//
// Two honest caveats. First, only convergents are considered, not semiconvergents,
// so for a small number of inputs a marginally closer fraction with a larger but
// still admissible denominator exists; the 1/q² bound holds regardless. Second, if
// the bounds are hit before any convergent with a positive numerator is found — which
// happens for prices shorter than roughly 1/1000 — there is no fractional form at all
// and the function returns ErrFractionalNotRepresentable.
//
// # Display convention
//
// The result is in lowest terms. The traditional un-reduced ladder spellings that
// some books post, such as 6/4 for 3/2 or 4/6 for 2/3, are a presentation choice
// belonging to the UI layer, not a property of the price, and are not reproduced
// here. Snapping a computed price — a no-vig fair value, say — onto a display ladder
// would misrepresent the number, so this package does not do it.
func (d Decimal) FractionalApprox() (Fractional, float64, error) {
	if err := d.Validate(); err != nil {
		return Fractional{}, 0, err
	}
	x := float64(d) - 1

	f := bestRationalApproximation(x, MaxFractionalDenominator, MaxFractionalNumerator, fractionalTolerance)
	if err := f.Validate(); err != nil {
		return Fractional{}, 0, fmt.Errorf(
			"odds: decimal %g needs a fraction outside numerator ≤ %d, denominator ≤ %d: %w",
			float64(d), MaxFractionalNumerator, MaxFractionalDenominator, ErrFractionalNotRepresentable)
	}

	approx := float64(f.Numerator) / float64(f.Denominator)
	return f, math.Abs(approx - x), nil
}

// -----------------------------------------------------------------------------
// Probability conversions
// -----------------------------------------------------------------------------

// Decimal converts a probability to the decimal odds that express it, d = 1/p.
//
// Only the open interval (0, 1) is priceable. Exactly 0 implies infinite odds and
// exactly 1 implies decimal 1.0, a zero payout; both return
// ErrProbabilityNotPriceable rather than an infinity or an invalid price. So do the
// handful of values so close to either endpoint that 1/p rounds onto one of those
// degenerate results — the double immediately below 1, for instance, has a
// reciprocal that rounds to exactly 1.0. That is why the computed result is
// validated rather than assumed.
func (p Probability) Decimal() (Decimal, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	f := float64(p)
	if f <= 0 {
		return 0, fmt.Errorf("odds: probability %g implies infinite odds: %w", f, ErrProbabilityNotPriceable)
	}
	if f >= 1 {
		return 0, fmt.Errorf("odds: probability %g implies a zero payout: %w", f, ErrProbabilityNotPriceable)
	}

	d := Decimal(1 / f)
	if err := d.Validate(); err != nil {
		return 0, fmt.Errorf("odds: probability %g is too close to certainty to price: %w", f, ErrProbabilityNotPriceable)
	}
	return d, nil
}

// American converts a probability to the nearest American price, via decimal.
func (p Probability) American() (American, error) {
	d, err := p.Decimal()
	if err != nil {
		return 0, err
	}
	return d.American()
}

// Fractional converts a probability to fractional odds, via decimal.
func (p Probability) Fractional() (Fractional, error) {
	d, err := p.Decimal()
	if err != nil {
		return Fractional{}, err
	}
	return d.Fractional()
}

// -----------------------------------------------------------------------------
// Fractional conversions
// -----------------------------------------------------------------------------

// Decimal converts a fractional price to decimal odds, d = 1 + Numerator/Denominator.
// Staking Denominator returns Denominator + Numerator, so the return per unit staked
// is 1 + Numerator/Denominator.
//
// The result is validated: an absurdly short fraction such as 1/(2^62) has a ratio
// small enough that adding 1 rounds straight back to 1.0, which is not a price.
func (f Fractional) Decimal() (Decimal, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	d := Decimal(1 + float64(f.Numerator)/float64(f.Denominator))
	if err := d.Validate(); err != nil {
		return 0, fmt.Errorf("odds: fractional %d/%d is too short to express as a decimal price: %w",
			f.Numerator, f.Denominator, ErrDecimalOutOfRange)
	}
	return d, nil
}

// American converts a fractional price to the nearest American price, via decimal.
func (f Fractional) American() (American, error) {
	d, err := f.Decimal()
	if err != nil {
		return 0, err
	}
	return d.American()
}

// Probability converts a fractional price to its implied probability, via decimal.
func (f Fractional) Probability() (Probability, error) {
	d, err := f.Decimal()
	if err != nil {
		return 0, err
	}
	return d.Probability()
}

// -----------------------------------------------------------------------------
// Continued fraction machinery
// -----------------------------------------------------------------------------

// bestRationalApproximation returns the last convergent of the simple continued
// fraction of x whose numerator and denominator stay inside maxNum and maxDen,
// stopping early once the convergent is within tol of x.
//
// x must be non-negative; every call site derives it from a validated Decimal, so
// x > 0. A negative or non-finite x returns the invalid zero-numerator fraction,
// which the caller rejects.
//
// The returned fraction is always in lowest terms: consecutive convergents of a
// continued fraction satisfy p_k·q_{k-1} - p_{k-1}·q_k = ±1, which forces
// gcd(p_k, q_k) = 1.
//
// See Decimal.FractionalApprox for the full convergence contract.
func bestRationalApproximation(x float64, maxDen, maxNum int64, tol float64) Fractional {
	best := Fractional{Numerator: 0, Denominator: 1}
	if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
		return best
	}

	// p_{-2}, p_{-1} and q_{-2}, q_{-1} seed the standard convergent recurrence.
	numPrev2, numPrev1 := int64(0), int64(1)
	denPrev2, denPrev1 := int64(1), int64(0)

	remainder := x
	for i := 0; i < maxContinuedFractionTerms; i++ {
		term := math.Floor(remainder)
		// Guard the float64 → int64 conversion: out-of-range conversions are
		// implementation-defined in Go, so the term is bounded before it is cast.
		if term < 0 || term > float64(maxNum) {
			break
		}
		termInt := int64(term)

		num, numOK := safeMulAdd(termInt, numPrev1, numPrev2)
		den, denOK := safeMulAdd(termInt, denPrev1, denPrev2)
		if !numOK || !denOK || num > maxNum || den > maxDen || den <= 0 {
			break
		}

		best = Fractional{Numerator: num, Denominator: den}
		if math.Abs(float64(num)/float64(den)-x) <= tol {
			break
		}

		frac := remainder - term
		if frac <= 0 {
			// x is rational and the expansion is complete; best is exact.
			break
		}
		next := 1 / frac
		if math.IsInf(next, 0) || math.IsNaN(next) {
			break
		}

		numPrev2, numPrev1 = numPrev1, num
		denPrev2, denPrev1 = denPrev1, den
		remainder = next
	}

	return best
}

// safeMulAdd returns a*b + c and reports whether the result fits in an int64. All
// three arguments must be non-negative; a negative argument reports failure rather
// than returning a wrapped value.
func safeMulAdd(a, b, c int64) (int64, bool) {
	if a < 0 || b < 0 || c < 0 {
		return 0, false
	}
	if a != 0 && b > (math.MaxInt64-c)/a {
		return 0, false
	}
	return a*b + c, true
}
