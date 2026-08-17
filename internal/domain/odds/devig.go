package odds

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Devigging: recovering the fair probabilities a bookmaker's prices imply, once the
// bookmaker's margin has been taken back out. CLAUDE.md §4 requires all four of the
// standard methods, "because they disagree meaningfully on longshots" — and that
// disagreement is the point, not an inconvenience. A +EV finder that devigs one way
// and calls the answer truth is a +EV finder that is confidently wrong on exactly
// the prices where the money is.
//
// # The problem
//
// Take a market's raw implied probabilities p_i = 1/d_i, one per selection. They do
// not sum to 1. They sum to
//
//	S = Σ p_i > 1
//
// which is the *overround*. The excess is the bookmaker's margin. Devigging maps
// p_1..p_n to fair probabilities q_1..q_n that do sum to 1. What makes it a genuine
// modelling question rather than arithmetic is that the constraint Σq = 1 is one
// equation and there are n unknowns: how the margin is distributed across the
// selections is an assumption, and the four methods are four different assumptions.
//
// # Three numbers that are constantly confused
//
// Stated once, here, because the odds package documentation already flags this as a
// classic trap and the three differ by enough to matter:
//
//	overround        S - 1        the excess of the implied probabilities over 1
//	booking percent  100·S        the same thing as a percentage of the book
//	vig / hold       1 - 1/S      the share of handle kept on a perfectly balanced book
//
// A market at -110/-110 has S = 1.0476…, an overround of 4.76%, a booking percentage
// of 104.76%, and a hold of 4.55%. DevigResult reports S as Overround and the hold
// from DevigResult.Vig.
//
// # A shared post-step: exact renormalisation
//
// Every method here finishes by dividing through by the sum of its own output. For
// multiplicative that is the method itself; for the other three it removes a residual
// of at most ~1e-13 left by float rounding or by the root solver's stopping
// criterion. It is applied uniformly so that one invariant holds unconditionally:
// the returned probabilities sum to 1 as exactly as float64 allows, and a market that
// is symmetric in its inputs comes back exactly symmetric — a -110/-110 market devigs
// to precisely 0.5 and 0.5 under all four methods, with no epsilon involved.
//
// The residual is checked *before* it is divided out, against devigResidualBound. A
// solver that failed to converge therefore surfaces as an error rather than being
// laundered into a plausible-looking answer by the renormalisation.

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

// See the note in solver.go for why these live here rather than in errors.go.
var (
	// ErrDevigTooFewSelections reports a market with fewer than two selections.
	// Devigging one price is not defined: with n = 1 the only vector summing to 1
	// is q = 1, which discards the price entirely.
	ErrDevigTooFewSelections = errors.New("devigging needs at least two selections")

	// ErrDevigAdditiveNonPositive reports that the additive method produced a
	// probability that is not strictly positive. This is not a bug: subtracting an
	// equal share of the margin from every selection is unsound once the margin per
	// selection exceeds the shortest price in the market, which is routine on a
	// long futures board. The condition is detected and refused rather than being
	// clamped, because a clamped zero would silently become an infinite fair price.
	ErrDevigAdditiveNonPositive = errors.New("additive devigging drove a probability to zero or below")

	// ErrDevigNoShinSolution reports that Shin's model has no admissible insider
	// share z for this market. It happens in two ways: the book sums to less than 1,
	// so there is no margin for insiders to explain, or the equation's root lies
	// beyond shinMaxZ, which means essentially all money is insider money and the
	// model has degenerated.
	ErrDevigNoShinSolution = errors.New("shin devigging has no admissible insider share")

	// ErrDevigNotNormalised reports that a method produced probabilities whose sum
	// was too far from 1 to be explained by float rounding. It is the guard that
	// stops a non-converged solve being rescued by the final renormalisation, and
	// reaching it means a numerical assumption in this file has been violated.
	ErrDevigNotNormalised = errors.New("devigged probabilities did not sum to 1")

	// ErrUnknownDevigMethod reports an unrecognised devigging method.
	ErrUnknownDevigMethod = errors.New("unknown devigging method")

	// ErrDevigLengthMismatch reports an attempt to compare two results that do not
	// describe the same market, i.e. that hold different numbers of selections.
	ErrDevigLengthMismatch = errors.New("devig results describe markets of different sizes")
)

// -----------------------------------------------------------------------------
// Tuning constants
// -----------------------------------------------------------------------------

const (
	// fairOverroundTolerance is how close S may be to 1 before the market is
	// treated as already fair. Within it, the power exponent is taken to be exactly
	// 1 and Shin's insider share exactly 0, and every method reduces to dividing
	// through by S.
	//
	// 1e-9 is seven orders of magnitude below the thinnest hold any real book
	// posts — a sharp book on a major line holds around 1.5%, so S - 1 ≈ 0.03 — and
	// below the resolution of a quoted price: moving one side of a two-way market
	// by a single 0.001 decimal tick near 2.0 changes S by about 2.5e-4. So no
	// market this bound captures is distinguishable from a fair one, while the
	// bound is comfortably above the ~1e-16 float noise in computing S itself,
	// which is what stops a genuinely fair market whose float sum lands a ULP below
	// 1 from being rejected by Shin as a negative-margin book.
	fairOverroundTolerance = 1e-9

	// devigResidualBound is the largest |Σq - 1| accepted before renormalisation.
	//
	// The methods' actual residuals are far smaller. Multiplicative and additive
	// accumulate a few ULPs per term, so ~1e-15 for any market size this system
	// prices. Power and Shin inherit the root solver's stopping criterion: for Shin
	// the bracket is inside [0, 1) so the residual is bounded by n·XTolerance ≈
	// 6e-13 at n = 64, and for power the two scales cancel — the analytic upper
	// bracket grows as 1/|ln p_max| while the residual's sensitivity to k falls as
	// |ln p_max|, leaving a residual near XTolerance·2·ln(n) ≈ 1e-13 regardless of
	// how short the shortest price is. 1e-9 therefore sits four orders above the
	// worst genuine case and eight orders below a difference the domain would care
	// about, which is the definition of a guard that only fires on real breakage.
	devigResidualBound = 1e-9

	// shinMaxZ is the top of the interval searched for Shin's insider share.
	//
	// z = 1 is always an exact root of the Shin equation — it is the degenerate
	// "every bettor is an insider" fixed point, not an answer — so the search must
	// stop short of it. 1 - 1e-6 leaves the residual at the upper end around 1e-6,
	// eight orders above the solver's residual tolerance, so the degenerate root
	// cannot be mistaken for convergence. A market whose true z exceeds this is
	// pathological (for a two-way market z → 1 requires S → 2, a 100% overround)
	// and returns ErrDevigNoShinSolution rather than a number.
	shinMaxZ = 1 - 1e-6

	// shinBracketSamples is the grid resolution FindRootBracket uses on [0, shinMaxZ].
	// The Shin residual is not proven to have a unique root there, so the leftmost
	// sign change is located by scanning rather than assumed; 256 samples put the
	// grid spacing at about 0.0039 in z, two orders finer than the z of any market
	// with a plausible overround (z is close to S - 1 for a two-way market, so a 5%
	// book has z ≈ 0.05, spanning a dozen cells).
	shinBracketSamples = 256

	// maxDevigSelections caps the market size. It is a sanity bound rather than a
	// modelling one: the widest markets this system prices are futures boards with
	// a few hundred entrants, and the bound keeps every "n ULPs" argument above
	// honest by keeping n small.
	maxDevigSelections = 1024
)

// -----------------------------------------------------------------------------
// Method enumeration
// -----------------------------------------------------------------------------

// DevigMethod names one of the four margin-removal models. It is an enum rather
// than four separate entry points because the pricing engine (phase 4) and the +EV
// finder (phase 9) both need to select a method at runtime and to run all four side
// by side over the same market.
//
// The zero value is MethodUnknown and is deliberately invalid, so a method that was
// never set fails rather than silently defaulting to multiplicative — which, being
// the method that is wrong about longshots, is the worst possible silent default.
type DevigMethod uint8

const (
	// MethodUnknown is the invalid zero value.
	MethodUnknown DevigMethod = iota

	// MethodMultiplicative scales every implied probability by the same factor:
	// q_i = p_i / S. See DevigMultiplicative.
	MethodMultiplicative

	// MethodAdditive subtracts an equal share of the margin from every selection:
	// q_i = p_i - (S-1)/n. See DevigAdditive.
	MethodAdditive

	// MethodPower raises every implied probability to a common exponent:
	// q_i = p_i^k with Σ p_i^k = 1. See DevigPower.
	MethodPower

	// MethodShin models the margin as the bookmaker's defence against a share z of
	// insider money. See DevigShin.
	MethodShin
)

// DevigMethods returns the four methods in a stable order — multiplicative,
// additive, power, Shin — running from the crudest model to the most structured.
// The slice is freshly allocated on every call, so a caller cannot reorder the
// canonical sequence for everyone else.
func DevigMethods() []DevigMethod {
	return []DevigMethod{MethodMultiplicative, MethodAdditive, MethodPower, MethodShin}
}

// String returns the canonical lowercase name of the method. It is the inverse of
// ParseDevigMethod for every valid value.
func (m DevigMethod) String() string {
	switch m {
	case MethodMultiplicative:
		return "multiplicative"
	case MethodAdditive:
		return "additive"
	case MethodPower:
		return "power"
	case MethodShin:
		return "shin"
	case MethodUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Valid reports whether m names a real devigging method.
func (m DevigMethod) Valid() bool {
	switch m {
	case MethodMultiplicative, MethodAdditive, MethodPower, MethodShin:
		return true
	case MethodUnknown:
		return false
	default:
		return false
	}
}

// ParseDevigMethod resolves a method name, case-insensitively and ignoring
// surrounding whitespace. It accepts the canonical names plus the aliases in
// common use:
//
//	multiplicative  proportional, basic
//	additive        balanced
//	power           logarithmic
//	shin            (no alias)
//
// The empty string is not treated as a default; a caller that wants one applies it
// explicitly.
func ParseDevigMethod(s string) (DevigMethod, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "multiplicative", "proportional", "basic":
		return MethodMultiplicative, nil
	case "additive", "balanced":
		return MethodAdditive, nil
	case "power", "logarithmic":
		return MethodPower, nil
	case "shin":
		return MethodShin, nil
	default:
		return MethodUnknown, fmt.Errorf("odds: %q: %w", s, ErrUnknownDevigMethod)
	}
}

// MarshalText implements encoding.TextMarshaler, so a method round-trips through
// JSON, query parameters, and environment config. An invalid method is an error
// rather than the string "unknown", so a half-initialised value cannot be
// serialised and shipped to a client.
func (m DevigMethod) MarshalText() ([]byte, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("odds: devig method %d: %w", uint8(m), ErrUnknownDevigMethod)
	}
	return []byte(m.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler using the same alias set as
// ParseDevigMethod.
func (m *DevigMethod) UnmarshalText(text []byte) error {
	parsed, err := ParseDevigMethod(string(text))
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// -----------------------------------------------------------------------------
// Result
// -----------------------------------------------------------------------------

// DevigResult is one market's fair probabilities together with the parameters that
// produced them, so a caller can report *why* a fair price is what it is rather
// than only what it is.
//
// Probabilities is freshly allocated by every devigging call and never aliases the
// caller's input, so the result is safe to retain and the input is never mutated.
type DevigResult struct {
	// Method is the model that produced Probabilities.
	Method DevigMethod

	// Probabilities are the fair probabilities, in the same order as the input, one
	// per selection. They sum to 1 and each lies strictly inside (0, 1).
	Probabilities []Probability

	// Overround is S = Σ p_i, the sum of the raw implied probabilities. It is the
	// booking sum, not the hold; see Vig.
	Overround float64

	// Parameter is the method's fitted free parameter, for diagnostics and for the
	// analytics surface CLAUDE.md §6 calls for:
	//
	//	multiplicative  0            the method has no parameter beyond Overround
	//	additive        (S-1)/n      the probability subtracted from every selection
	//	power           k            the exponent, with Σ p_i^k = 1
	//	shin            z            the fitted share of insider money
	Parameter float64

	// Iterations is the number of root-solver steps taken: zero for the two
	// closed-form methods, and zero for power and Shin when the market was already
	// fair to within fairOverroundTolerance and no solve was needed.
	Iterations int
}

// Vig returns the bookmaker's hold: the fraction of total handle retained on a
// perfectly balanced book, (S-1)/S.
//
// It is *not* the overround S - 1, and the two differ by enough to matter — a
// -110/-110 market has an overround of 4.762% and a hold of 4.545%. Reporting one
// under the other's name is a standard way to mis-state a book's margin by a
// relative 5%.
//
// # Why it returns an error, and why it delegates
//
// It used to return math.NaN() on a non-positive or non-finite Overround, and the
// zero DevigResult is exactly that state — which is the value DevigCompare pairs
// with every failed method. A caller fanning DevigCompare's output into a dashboard,
// a JSON body or a Prometheus gauge therefore shipped a NaN with nothing to signal
// it, against this package's stated contract (see doc.go: nothing silently returns
// NaN, ±Inf, or a half-computed answer). The failure is now reportable.
//
// The arithmetic is not done here. It delegates to MarginFromSum so that the hold
// has exactly one definition in the package: (S-1)/S, computed on an exactly
// representable numerator, rather than the algebraically identical but far worse
// conditioned 1 - 1/S this method used to evaluate. newMarginFrom carries the
// conditioning argument in full; the short version is that the two forms disagree by
// a relative 2e-11 on a 0.01% hold, and a thin margin is where a sharp book lives.
//
// The selection count comes from Probabilities, so the zero value fails with
// ErrTooFewSelections rather than producing a number for a market that was never
// priced.
func (r DevigResult) Vig() (float64, error) {
	m, err := MarginFromSum(len(r.Probabilities), r.Overround)
	if err != nil {
		return 0, err
	}
	return m.Vig, nil
}

// Decimals converts the fair probabilities to fair decimal prices, one per
// selection. These are the no-vig prices: the prices at which the market would
// carry no margin at all.
func (r DevigResult) Decimals() ([]Decimal, error) {
	out := make([]Decimal, len(r.Probabilities))
	for i, p := range r.Probabilities {
		d, err := p.Decimal()
		if err != nil {
			return nil, fmt.Errorf("odds: fair probability %g for selection %d has no price: %w",
				float64(p), i, unprefixed(err))
		}
		out[i] = d
	}
	return out, nil
}

// MaxAbsDiff returns the largest absolute difference between this result's
// probabilities and another's, selection by selection. It is how the four methods
// are compared: the number quantifies how much the choice of method moves the fair
// probability, which is small on a balanced two-way market and large on a longshot.
//
// Both results must cover the same market, i.e. have the same length.
func (r DevigResult) MaxAbsDiff(other DevigResult) (float64, error) {
	if len(r.Probabilities) != len(other.Probabilities) {
		return 0, fmt.Errorf("odds: cannot compare a %d-selection result with a %d-selection one: %w",
			len(r.Probabilities), len(other.Probabilities), ErrDevigLengthMismatch)
	}
	if len(r.Probabilities) == 0 {
		return 0, fmt.Errorf("odds: cannot compare empty results: %w", ErrDevigLengthMismatch)
	}
	worst := 0.0
	for i := range r.Probabilities {
		d := math.Abs(float64(r.Probabilities[i]) - float64(other.Probabilities[i]))
		if d > worst {
			worst = d
		}
	}
	return worst, nil
}

// DevigComparison is one method's outcome when every method is run over the same
// market. Err is non-nil when that particular method could not price the market —
// most often additive on a board containing a big longshot — and Result is then the
// zero value. One method failing never suppresses the others.
type DevigComparison struct {
	Method DevigMethod
	Result DevigResult
	Err    error
}

// -----------------------------------------------------------------------------
// Dispatch
// -----------------------------------------------------------------------------

// Devig removes the bookmaker's margin from a market's raw implied probabilities
// using the named method, and returns the fair probabilities.
//
// The input is the market's implied probabilities in selection order, each strictly
// inside (0, 1); use Decimal.Probability to obtain them from prices, or DevigPrices
// to skip the step. The input slice is not modified.
func Devig(method DevigMethod, implied []Probability) (DevigResult, error) {
	switch method {
	case MethodMultiplicative:
		return DevigMultiplicative(implied)
	case MethodAdditive:
		return DevigAdditive(implied)
	case MethodPower:
		return DevigPower(implied)
	case MethodShin:
		return DevigShin(implied)
	case MethodUnknown:
		return DevigResult{}, fmt.Errorf("odds: devig method %d: %w", uint8(method), ErrUnknownDevigMethod)
	default:
		return DevigResult{}, fmt.Errorf("odds: devig method %d: %w", uint8(method), ErrUnknownDevigMethod)
	}
}

// DevigPrices is Devig starting from decimal prices instead of probabilities. It is
// the ergonomic entry point for callers holding a market's quoted prices, and it
// applies the same validation as the underlying conversion.
func DevigPrices(method DevigMethod, prices []Decimal) (DevigResult, error) {
	implied := make([]Probability, len(prices))
	for i, d := range prices {
		p, err := d.Probability()
		if err != nil {
			return DevigResult{}, fmt.Errorf("odds: price %g for selection %d: %w",
				float64(d), i, unprefixed(err))
		}
		implied[i] = p
	}
	return Devig(method, implied)
}

// DevigCompare runs every method in DevigMethods order over the same market and
// returns one entry per method, each carrying either a result or the reason that
// method could not price this market.
//
// It never returns an error of its own: a market on which additive goes negative is
// still perfectly priceable by the other three, and collapsing that into a single
// failure would throw away the comparison that phases 4 and 9 exist to make.
func DevigCompare(implied []Probability) []DevigComparison {
	methods := DevigMethods()
	out := make([]DevigComparison, 0, len(methods))
	for _, m := range methods {
		res, err := Devig(m, implied)
		out = append(out, DevigComparison{Method: m, Result: res, Err: err})
	}
	return out
}

// -----------------------------------------------------------------------------
// Multiplicative
// -----------------------------------------------------------------------------

// DevigMultiplicative removes the margin by scaling every implied probability by
// the same factor:
//
//	q_i = p_i / S,  S = Σ p_i
//
// # The assumption, and where it fails
//
// The margin is charged in proportion to each selection's implied probability, so
// every selection is overpriced by the same *relative* amount and the ratios between
// selections survive untouched: q_i/q_j = p_i/p_j exactly.
//
// That ratio-preservation is the method's defining property and also its defect.
// Bookmakers do not charge proportionally. The favourite-longshot bias — documented
// across every betting market that has been studied — means the margin loaded onto a
// 25-to-1 shot is far more than 25 times the margin on a 1-to-1 shot in relative
// terms. Multiplicative therefore leaves too much probability on the longshot and
// takes too little off the favourite, which is exactly backwards for a +EV finder,
// because longshots are where an overestimated fair probability manufactures fake
// edges.
//
// It is included because it is the baseline every other method is judged against,
// because it is the only method with no free parameter and no failure mode, and
// because on a balanced two-way market the four methods agree closely enough that
// its simplicity wins.
func DevigMultiplicative(implied []Probability) (DevigResult, error) {
	p, sum, err := devigInputs(implied)
	if err != nil {
		return DevigResult{}, err
	}

	q := make([]float64, len(p))
	for i, v := range p {
		q[i] = v / sum
	}
	return finishDevig(MethodMultiplicative, q, sum, 0, 0)
}

// -----------------------------------------------------------------------------
// Additive
// -----------------------------------------------------------------------------

// DevigAdditive removes the margin by subtracting an equal share of it from every
// selection:
//
//	q_i = p_i - (S-1)/n
//
// # The assumption, and where it fails
//
// The margin is charged as a flat probability surcharge, identical in absolute terms
// on every selection. Differences between selections survive untouched:
// q_i - q_j = p_i - p_j exactly, which is the mirror image of multiplicative's
// ratio-preservation.
//
// In relative terms a flat surcharge falls far harder on short prices, so additive
// takes a much larger relative bite out of longshots than multiplicative does. That
// is directionally the right correction for the favourite-longshot bias, and it is
// why additive and multiplicative bracket the other two methods on a longshot.
//
// The failure is not subtle. Once (S-1)/n exceeds the smallest implied probability
// in the market, the method produces a non-positive probability — which is not a
// long price but no price at all, since a fair probability of zero implies infinite
// odds. On a futures board of thirty entrants with a 20% overround the per-selection
// deduction is 0.0067, which wipes out every selection priced longer than about
// 150-to-1. This is detected and returned as ErrDevigAdditiveNonPositive; nothing is
// clamped, because a clamped zero would propagate as an infinite fair price and a
// spectacular fake edge.
//
// # A result worth knowing
//
// On a two-way market additive and Shin coincide exactly. That is not a coincidence
// of implementation but an algebraic identity, proved and tested in devig_test.go.
func DevigAdditive(implied []Probability) (DevigResult, error) {
	p, sum, err := devigInputs(implied)
	if err != nil {
		return DevigResult{}, err
	}

	n := float64(len(p))
	share := (sum - 1) / n

	q := make([]float64, len(p))
	for i, v := range p {
		q[i] = v - share
		if q[i] <= 0 {
			return DevigResult{}, fmt.Errorf(
				"odds: additive devig of a %d-selection market with overround %g subtracts %g from each selection, "+
					"driving selection %d (implied %g) to %g: %w",
				len(p), sum, share, i, v, q[i], ErrDevigAdditiveNonPositive)
		}
	}
	return finishDevig(MethodAdditive, q, sum, share, 0)
}

// -----------------------------------------------------------------------------
// Power
// -----------------------------------------------------------------------------

// DevigPower removes the margin by raising every implied probability to a common
// exponent:
//
//	q_i = p_i^k,  with k chosen so that Σ p_i^k = 1
//
// # Convention
//
// Two spellings of this method are in circulation: q_i = p_i^k and q_i = p_i^(1/k).
// They are the same method with reciprocal parameters. This implementation uses the
// exponent form, so k > 1 on a book with margin (S > 1) and k < 1 on the rare book
// that sums to less than 1. DevigResult.Parameter is that k, so a caller comparing
// against a source that reports the reciprocal must invert it.
//
// # The assumption
//
// The bookmaker's margin is applied as a power transform of the true probabilities.
// Because x ↦ x^k with k > 1 compresses small x proportionally harder than large x —
// halving a probability of 0.04 costs it far more of itself than halving one of
// 0.8 — the method removes proportionally more margin from longshots. It lands
// between multiplicative and additive, which is where the empirical work on the
// favourite-longshot bias puts the truth.
//
// # Existence and uniqueness of k
//
// Both are provable rather than assumed, which is what makes a bracketed solve safe.
// Write g(k) = Σ p_i^k. Every p_i lies strictly inside (0, 1), so ln p_i < 0 and
//
//	g'(k) = Σ p_i^k · ln p_i < 0
//
// for every k: g is strictly decreasing, so it crosses 1 at most once. For existence,
// g(0) = n ≥ 2 > 1 and g(1) = S, so when S > 1 the root lies above 1 and when S < 1
// it lies in (0, 1). An upper bound is available in closed form: with p_max the
// largest implied probability, g(k) ≤ n·p_max^k, so any
//
//	k > ln(n) / (-ln p_max)
//
// already forces g(k) < 1. The implementation takes twice that plus two and then
// *verifies* the sign change rather than trusting the algebra — see
// NewRootBracket.
//
// # Convergence
//
// The root is found by Illinois with DefaultRootConfig: stop when |Σ p_i^k - 1| ≤
// 1e-14 or the bracket has shrunk to a relative width of 1e-14, and fail with
// ErrRootNoConvergence after 200 iterations. There is no path that returns an
// unconverged k.
func DevigPower(implied []Probability) (DevigResult, error) {
	p, sum, err := devigInputs(implied)
	if err != nil {
		return DevigResult{}, err
	}

	// An already-fair book has k = 1 exactly. Short-circuiting it is not just an
	// optimisation: at S = 1 the bracket [1, kHi] would have a root exactly on its
	// lower endpoint, and for S below 1 the bracket has to be flipped to [0, 1].
	// Handling the fair case explicitly keeps both branches unambiguous.
	if math.Abs(sum-1) <= fairOverroundTolerance {
		q := make([]float64, len(p))
		copy(q, p)
		return finishDevig(MethodPower, q, sum, 1, 0)
	}

	residual := func(k float64) (float64, error) { return powerSum(p, k) - 1, nil }

	// S > 1 puts the root above k = 1 and needs an upper bound; S < 1 puts it in
	// (0, 1), where g(0) = n > 1 and g(1) = S < 1 bracket it with no search at all.
	lo, hi := 0.0, 1.0
	if sum > 1 {
		bound, err := powerExponentUpperBound(p)
		if err != nil {
			return DevigResult{}, err
		}
		lo, hi = 1, bound
	}

	bracket, err := NewRootBracket(residual, lo, hi)
	if err != nil {
		return DevigResult{}, fmt.Errorf("odds: bracketing the power exponent on [%g, %g] for overround %g: %w",
			lo, hi, sum, err)
	}

	sol, err := Illinois(residual, bracket, DefaultRootConfig())
	if err != nil {
		return DevigResult{}, fmt.Errorf("odds: solving Σp^k = 1 for a %d-selection market with overround %g: %w",
			len(p), sum, err)
	}

	q := make([]float64, len(p))
	for i, v := range p {
		q[i] = math.Pow(v, sol.Root)
	}
	return finishDevig(MethodPower, q, sum, sol.Root, sol.Iterations)
}

// powerSum returns Σ p_i^k.
func powerSum(p []float64, k float64) float64 {
	total := 0.0
	for _, v := range p {
		total += math.Pow(v, k)
	}
	return total
}

// powerExponentUpperBound returns a k that is guaranteed to make Σ p_i^k < 1, using
// the bound in DevigPower's documentation: Σ p_i^k ≤ n·p_max^k, which is below 1
// once k > ln(n)/(-ln p_max). The returned value doubles that and adds two, so the
// bracket has slack rather than sitting on the boundary.
//
// The bound is only unusable when p_max is so close to 1 that -ln(p_max) underflows
// towards zero and the quotient overflows. Every input has already been validated as
// strictly below 1, so that needs a price within a few ULPs of decimal 1.0; it is
// rejected rather than allowed to produce an infinite bracket.
func powerExponentUpperBound(p []float64) (float64, error) {
	pMax := 0.0
	for _, v := range p {
		if v > pMax {
			pMax = v
		}
	}
	denom := -math.Log(pMax)
	if !(denom > 0) || math.IsInf(denom, 0) {
		return 0, fmt.Errorf("odds: shortest price has implied probability %g, too close to certainty to bound the power exponent: %w",
			pMax, ErrRootNoBracket)
	}
	hi := 2*math.Log(float64(len(p)))/denom + 2
	if math.IsNaN(hi) || math.IsInf(hi, 0) {
		return 0, fmt.Errorf("odds: power exponent upper bound is %v for a %d-selection market with shortest implied probability %g: %w",
			hi, len(p), pMax, ErrRootNoBracket)
	}
	return hi, nil
}

// -----------------------------------------------------------------------------
// Shin
// -----------------------------------------------------------------------------

// DevigShin removes the margin using Shin's model of insider trading.
//
// # The model
//
// Shin (1992, 1993) explains the bookmaker's margin as a defence rather than a fee.
// A share z of the money wagered comes from insiders who know the outcome; the
// remaining 1-z is uninformed and is spread across the selections in proportion to
// the quoted prices. The bookmaker sets prices so that the losses to the informed
// share are recovered from the uninformed one. Because a longshot's price is far
// more valuable to an insider, this produces a margin that falls disproportionately
// on long prices — the favourite-longshot bias emerges from the model rather than
// being fitted to it, which is what distinguishes Shin from the other three.
//
// # The equations, and which formulation this is
//
// Shin's method circulates in several algebraically equivalent but typographically
// different forms, and a wrong transcription produces plausible, wrong numbers
// silently. The form implemented here is the one in the CRAN `implied` package's
// shin_func, cross-checked against the worked example published by the Python `shin`
// package (see devig_test.go, which asserts against that example's numbers).
//
// With raw implied probabilities p_i and S = Σ p_i:
//
//	u_i(z) = sqrt( z² + 4(1-z)·p_i²/S )
//	q_i    = ( u_i(z) - z ) / ( 2(1-z) )
//
// and z is the root of Σ q_i = 1, which rearranges to
//
//	Σ u_i(z) = 2 + (n-2)·z
//
// The right-hand side is worth stating explicitly, because it is the single easiest
// thing to get wrong. It is 2 only for a two-way market. Sources that quote the
// constraint as "Σ u_i = 2" are quoting the n = 2 special case; using it for a
// three-way market gives a z that is wrong and probabilities that still look
// reasonable. Note also that p_i here is the *raw* implied probability, not p_i/S:
// the division by S inside the square root is the whole of the normalisation.
//
// # Existence of a root, proved
//
// f(z) = Σ u_i(z) - 2 - (n-2)z on [0, 1].
//
//   - f(0) = Σ 2p_i/√S - 2 = 2(√S - 1) > 0 whenever S > 1.
//   - f(1) = n - 2 - (n-2) = 0, so z = 1 is always a root — the degenerate one, where
//     every bettor is an insider. It is not an answer and the search stops short of it.
//   - u_i'(1) = 1 - 2p_i²/S, so f'(1) = 2(1 - Σp_i²/S). Every p_i is strictly inside
//     (0, 1), so p_i² < p_i and therefore Σp_i² < Σp_i = S, making f'(1) > 0 for every
//     market. f is increasing as it arrives at zero from the left, so f < 0 on some
//     left-neighbourhood of 1.
//
// f is positive at 0 and negative just below 1, so a root exists strictly inside
// (0, 1). Uniqueness is not claimed, which is why the bracket is found by scanning
// for the *leftmost* sign change rather than by asserting one.
//
// # Why the probability is not computed as written
//
// The published form (u_i - z)/(2(1-z)) has two numerical weaknesses: the numerator
// cancels catastrophically as z → 1, where u_i → 1 too, and the denominator vanishes
// there. Both disappear under an exact algebraic rearrangement. From the definition
// of u_i, u_i² - z² = 4(1-z)p_i²/S, so (u_i - z)(u_i + z) = 4(1-z)p_i²/S, and
//
//	q_i = (u_i - z) / (2(1-z)) = 2·p_i² / ( S·(u_i + z) )
//
// The right-hand side is a sum of non-negative quantities over a strictly positive
// denominator: no cancellation, no singularity at z = 1, and q_i > 0 by construction
// rather than by argument. That is the form evaluated.
//
// # Sources
//
//   - H. S. Shin, "Prices of State Contingent Claims with Insider Traders, and the
//     Favourite-Longshot Bias", The Economic Journal 102 (1992), 426-435.
//   - H. S. Shin, "Measuring the Incidence of Insider Trading in a Market for
//     State-Contingent Claims", The Economic Journal 103 (1993), 1141-1153.
//   - J. C. Lindstrøm, `implied` (CRAN), R/implied_probabilities.R — shin_func, the
//     formulation transcribed above.
func DevigShin(implied []Probability) (DevigResult, error) {
	p, sum, err := devigInputs(implied)
	if err != nil {
		return DevigResult{}, err
	}

	if sum < 1-fairOverroundTolerance {
		return DevigResult{}, fmt.Errorf(
			"odds: a %d-selection market whose implied probabilities sum to %g carries no margin for insiders to explain: %w",
			len(p), sum, ErrDevigNoShinSolution)
	}
	// A fair book is the z = 0 corner of the model, where u_i = 2p_i/√S and the
	// stable form collapses to q_i = p_i/S — Shin agreeing exactly with
	// multiplicative in the limit, as it must.
	if sum <= 1+fairOverroundTolerance {
		q := make([]float64, len(p))
		for i, v := range p {
			q[i] = v / sum
		}
		return finishDevig(MethodShin, q, sum, 0, 0)
	}

	n := float64(len(p))
	// c_i = 4·p_i²/S, hoisted because the residual is evaluated a few hundred times
	// while scanning for the bracket.
	c := make([]float64, len(p))
	for i, v := range p {
		c[i] = 4 * v * v / sum
	}

	residual := func(z float64) (float64, error) {
		total := 0.0
		for _, ci := range c {
			total += math.Sqrt(z*z + ci*(1-z))
		}
		return total - 2 - (n-2)*z, nil
	}

	bracket, err := FindRootBracket(residual, 0, shinMaxZ, shinBracketSamples)
	if err != nil {
		// No sign change on [0, shinMaxZ] means the root has been pushed past the
		// search ceiling and into the degenerate all-insider corner. That is a
		// statement about the model, not about the root finder, so the error carries
		// ErrDevigNoShinSolution as well as the solver's own sentinel — a caller
		// deciding whether to fall back to another method matches on the former.
		return DevigResult{}, fmt.Errorf(
			"odds: locating the insider share for a %d-selection market with overround %g: %w: %w",
			len(p), sum, err, ErrDevigNoShinSolution)
	}

	sol, err := Illinois(residual, bracket, DefaultRootConfig())
	if err != nil {
		return DevigResult{}, fmt.Errorf(
			"odds: solving the shin equation for a %d-selection market with overround %g: %w", len(p), sum, err)
	}
	z := sol.Root

	q := make([]float64, len(p))
	for i, v := range p {
		u := math.Sqrt(z*z + c[i]*(1-z))
		q[i] = 2 * v * v / (sum * (u + z))
	}
	return finishDevig(MethodShin, q, sum, z, sol.Iterations)
}

// -----------------------------------------------------------------------------
// Shared plumbing
// -----------------------------------------------------------------------------

// devigInputs validates a market's implied probabilities and returns them as plain
// float64 alongside their sum.
//
// Validation is stricter than Probability.Validate: that accepts the closed interval
// [0, 1], because 0 and 1 are legitimate probabilities for a settled outcome, but a
// *quoted* selection cannot be either. Exactly 0 has no finite price, exactly 1 has a
// zero payout, and both would make every method here divide by zero or take the
// logarithm of zero. They are rejected with ErrProbabilityNotPriceable, the sentinel
// that already means precisely this.
func devigInputs(implied []Probability) ([]float64, float64, error) {
	if len(implied) < 2 {
		return nil, 0, fmt.Errorf("odds: market has %d selection(s): %w", len(implied), ErrDevigTooFewSelections)
	}
	if len(implied) > maxDevigSelections {
		return nil, 0, fmt.Errorf("odds: market has %d selections, beyond the supported maximum of %d: %w",
			len(implied), maxDevigSelections, ErrDevigTooFewSelections)
	}

	p := make([]float64, len(implied))
	for i, v := range implied {
		if err := v.Validate(); err != nil {
			return nil, 0, fmt.Errorf("odds: selection %d has implied probability %g: %w",
				i, float64(v), unprefixed(err))
		}
		f := float64(v)
		if f <= 0 {
			return nil, 0, fmt.Errorf("odds: selection %d has implied probability %g, which has no finite price: %w",
				i, f, ErrProbabilityNotPriceable)
		}
		if f >= 1 {
			return nil, 0, fmt.Errorf("odds: selection %d has implied probability %g, which implies a zero payout: %w",
				i, f, ErrProbabilityNotPriceable)
		}
		p[i] = f
	}

	// Compensated, not naive. vig.go computes the same quantity with neumaierSum and
	// argues the case there; the two files must not report different sums for one
	// market, or a reader comparing Margin.ImpliedSum against DevigResult.Overround
	// in a log sees them disagree. The canonical example is the exactly fair book of
	// evens, 2-1 and 5-1: naively 1/2 + 1/3 + 1/6 comes to 1 - 2^-53, so
	// MarginFromProbabilities called it fair and DevigMultiplicative reported an
	// overround of 0.99999999999999989 and a hold of -2.2e-16.
	sum := neumaierSum(p)

	// Unreachable given the per-element bounds — n ≤ 1024 values each below 1 cannot
	// overflow — but asserted rather than assumed, because every downstream division
	// by sum depends on it.
	if math.IsNaN(sum) || math.IsInf(sum, 0) || sum <= 0 {
		return nil, 0, fmt.Errorf("odds: implied probabilities sum to %v: %w", sum, ErrNotFinite)
	}
	return p, sum, nil
}

// finishDevig performs the shared final step: check that the method's raw output
// really does sum to 1, renormalise it so that it does so as exactly as float64
// allows, and package it as a DevigResult.
//
// The order matters. Checking before renormalising is what makes ErrDevigNotNormalised
// a real guard: renormalisation forces any vector of positive numbers to sum to 1, so
// a solve that quietly failed would otherwise emerge looking like a valid answer.
func finishDevig(method DevigMethod, q []float64, overround, parameter float64, iterations int) (DevigResult, error) {
	raw := 0.0
	for i, v := range q {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return DevigResult{}, fmt.Errorf("odds: %s devig produced %v for selection %d: %w", method, v, i, ErrNotFinite)
		}
		if v <= 0 {
			return DevigResult{}, fmt.Errorf("odds: %s devig produced %g for selection %d: %w",
				method, v, i, ErrProbabilityNotPriceable)
		}
		raw += v
	}
	if math.Abs(raw-1) > devigResidualBound {
		return DevigResult{}, fmt.Errorf("odds: %s devig produced probabilities summing to %.17g, off by %.3g: %w",
			method, raw, raw-1, ErrDevigNotNormalised)
	}

	out := make([]Probability, len(q))
	for i, v := range q {
		scaled := v / raw
		// In exact arithmetic every scaled value is strictly inside (0, 1), because
		// there are at least two strictly positive terms and each is therefore
		// strictly below their sum. In float64 that argument fails at the extremes:
		// a market spanning more than 2^52 in price — say an implied probability of
		// 0.5 alongside one of 5.6e-17 — has a short side that vanishes entirely when
		// the sum is accumulated, and the long side renormalises to exactly 1.
		//
		// Exactly 1 is a valid Probability (a settled outcome is certain) but not a
		// priceable one: it means a zero payout, and 0 at the other end means infinite
		// odds. Both are refused here rather than being passed on, because a fair
		// probability of 1 propagates into the pricing engine as a guaranteed winner
		// and into the +EV finder as an infinite edge. The strict test is written out
		// rather than delegated to NewProbability, which accepts the closed interval.
		//
		// A property-based test found this; the exact-arithmetic argument above had
		// looked airtight.
		//
		// The test also subsumes what NewProbability would check, which is why the
		// conversion below is a plain one: NaN fails `scaled > 0`, ±Inf fails one of
		// the two comparisons, and (0, 1) is strictly inside the closed interval
		// NewProbability accepts. Calling it as well would add an error path that no
		// input can reach.
		if !(scaled > 0 && scaled < 1) {
			return DevigResult{}, fmt.Errorf(
				"odds: %s devig renormalised selection %d to %.17g, which has no finite price; "+
					"the market spans a wider range of prices than float64 can hold apart: %w",
				method, i, scaled, ErrProbabilityNotPriceable)
		}
		out[i] = Probability(scaled)
	}

	return DevigResult{
		Method:        method,
		Probabilities: out,
		Overround:     overround,
		Parameter:     parameter,
		Iterations:    iterations,
	}, nil
}
