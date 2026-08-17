package odds

import "errors"

// Sentinel errors for the odds package (CLAUDE.md §12: "sentinel errors in the
// domain package"). Every error this package returns wraps exactly one of these
// with %w, so callers match with errors.Is and never on message text.
//
// The messages are deliberately bare: the call site adds the "odds:" prefix and
// the offending value, so the prefix appears exactly once no matter how deep the
// wrapping goes.
var (
	// ErrNotFinite reports a NaN or ±Inf where a real number was required. It is
	// checked before any range comparison, because NaN compares false against every
	// bound and would otherwise slip through a naive check and poison a whole slate.
	ErrNotFinite = errors.New("value is not a finite number")

	// ErrAmericanOutOfRange reports an American price outside the legal set, which
	// is the integers satisfying MinAmericanMagnitude ≤ |A| ≤ MaxAmericanMagnitude.
	// It covers both failure modes: the illegal (-100, +100) band, where no such
	// price exists, and magnitudes past the representable ceiling.
	ErrAmericanOutOfRange = errors.New("american odds magnitude outside the representable range")

	// ErrDecimalOutOfRange reports a decimal price that is not strictly greater
	// than 1. Exactly 1.0 is a zero payout and an implied probability of exactly 1;
	// below 1.0 the bettor loses money on a winning wager. Neither is a price.
	ErrDecimalOutOfRange = errors.New("decimal odds must be strictly greater than 1")

	// ErrProbabilityOutOfRange reports a probability outside the closed interval
	// [0, 1].
	ErrProbabilityOutOfRange = errors.New("probability must lie in the interval [0, 1]")

	// ErrProbabilityNotPriceable reports a probability that is inside [0, 1] and so
	// perfectly valid as a probability, but has no finite odds representation. That
	// is exactly 0 (which implies infinite decimal odds), exactly 1 (which implies
	// decimal odds of 1, a zero payout), and the handful of values so close to
	// either end that 1/p rounds to one of those degenerate results.
	ErrProbabilityNotPriceable = errors.New("probability has no finite odds representation")

	// ErrFractionalNumerator reports a fractional numerator that is not strictly
	// positive. A numerator of 0 means the bet returns the stake and nothing more.
	ErrFractionalNumerator = errors.New("fractional numerator must be strictly positive")

	// ErrFractionalDenominator reports a fractional denominator that is not strictly
	// positive. A denominator of 0 is a division by zero, not a price.
	ErrFractionalDenominator = errors.New("fractional denominator must be strictly positive")

	// ErrFractionalNotRepresentable reports a decimal price for which no fraction
	// with a numerator and denominator inside this package's bounds approximates
	// d-1. In practice this means a price shorter than roughly 1/1000 (decimal below
	// about 1.001) or longer than MaxFractionalNumerator to one. Callers rendering a
	// format toggle should fall back to another format rather than treat it as fatal.
	ErrFractionalNotRepresentable = errors.New("decimal odds have no fractional representation within the supported bounds")

	// ErrUnknownFormat reports an unrecognised odds display format.
	ErrUnknownFormat = errors.New("unknown odds display format")
)
