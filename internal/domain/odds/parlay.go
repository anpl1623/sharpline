package odds

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// This file prices parlays: straight parlays, correlated same-game parlays, round
// robins, and teasers (CLAUDE.md §4 and §6). It builds on correlation.go, which
// holds the correlation matrix type and the three numerical primitives the copula
// needs.
//
// # The independent case
//
// For legs that are genuinely independent — different games, no shared personnel,
// no shared weather — the decimal price of a parlay is the product of the leg
// prices, and the joint probability is the product of the leg probabilities. That
// is why decimal is this package's canonical representation: it is the only one of
// the four that composes multiplicatively.
//
// Independence is an assumption about the world, not a property of the arithmetic.
// ParlayDecimal will multiply any legs it is handed, including two legs of the same
// game, and produce a confidently wrong number. Use it only when the legs are in
// different events; otherwise use CorrelatedParlayDecimal with a real correlation
// matrix.
//
// # The correlated case
//
// Two legs of the same event move together. Multiplying their probabilities
// understates the joint probability of a positively correlated pair, which
// overstates the fair price, which is precisely the arithmetic that would make a
// +EV finder advertise edges that do not exist. Real books solve this by
// haircutting same-game parlays; this package solves it with a Gaussian copula,
// which is the standard industry construction.
//
// The construction: leg i wins iff a latent standard normal Z_i falls below the
// threshold t_i = Φ⁻¹(p_i), which reproduces the marginal probability p_i exactly
// by definition of the quantile. Correlation between legs is imposed by giving the
// vector Z the caller's correlation matrix R, so the joint probability that every
// leg wins is the orthant probability Φ_n(t_1, …, t_n; R). The copula changes the
// dependence structure and leaves every marginal untouched, which is exactly the
// property wanted: the individual leg prices are still the individual leg prices.
//
// # Fair marginals or implied marginals — the caller decides
//
// Every function here that takes probabilities is agnostic about whether they are
// fair (devigged) or implied (1/d, margin included). The distinction determines
// what the answer means, and the caller must make it deliberately:
//
//   - Implied marginals in, and the result is a quotable price whose margin is the
//     compounded margin of the legs. That is what a book posts.
//   - Fair marginals in, and the result is a fair no-vig price. That is what an EV
//     calculation must compare a posted parlay price against.
//
// Passing implied marginals and calling the answer "fair" is the single most
// expensive mistake available in this file.
//
// # Sentinel errors
//
// As with correlation.go, these sentinels are declared beside the code that
// returns them rather than in errors.go, for file-ownership reasons during the
// build. They are part of the same stable contract and are matched with errors.Is.

var (
	// ErrNoLegs reports a parlay with no legs. A parlay of nothing has no price;
	// it is not a price of 1.0.
	ErrNoLegs = errors.New("a parlay must have at least one leg")

	// ErrTooManyLegs reports a parlay with more than MaxParlayLegs legs.
	ErrTooManyLegs = errors.New("parlay leg count exceeds the supported maximum")

	// ErrLegCountMismatch reports a correlation matrix whose dimension is not the
	// number of legs. Silently truncating or padding would price a different
	// parlay than the one that was asked for.
	ErrLegCountMismatch = errors.New("correlation matrix dimension does not match the leg count")

	// ErrParlayNotPriceable reports a computed joint probability that is not a
	// probability: it left the Fréchet-Hoeffding interval that bounds the joint
	// probability of any set of events under any dependence structure, by more than
	// the numerical slack the quadrature is allowed.
	//
	// It is the backstop on the correlated path. The quadrature behind
	// MultivariateNormalCDF reports its own non-convergence separately, with
	// ErrOrthantNotConverged; this error catches the different failure where the
	// integration converged to something that cannot be a probability. Returning an
	// error is the same call a real book makes when it declares a combination
	// unpriceable and refuses the ticket, and it is strictly better than quoting a
	// fabricated number.
	ErrParlayNotPriceable = errors.New("correlated parlay probability is outside the bounds every joint probability must satisfy")

	// ErrCombinationSize reports a round-robin combination size that is not
	// between 1 and the number of legs.
	ErrCombinationSize = errors.New("round robin combination size must be between 1 and the leg count")

	// ErrTooManyCombinations reports a round robin that would generate more than
	// MaxRoundRobinTickets tickets. The count is binomial in the leg set, so it
	// grows past anything a bettor would place long before it grows past anything
	// a computer would notice.
	ErrTooManyCombinations = errors.New("round robin would generate more tickets than the supported maximum")

	// ErrTeaserPoints reports a teaser leg whose point adjustment is not a finite
	// number in (0, MaxTeaserPoints].
	ErrTeaserPoints = errors.New("teaser point adjustment must be a finite number greater than zero")

	// ErrTeaserNotFavourable reports a teaser leg whose teased price is longer
	// than its original price. Moving a line in the bettor's favour can only make
	// the selection more likely to win, so it can only shorten the price. A longer
	// teased price means the caller supplied the wrong number — most often the
	// untensed price, or the price of the other side.
	ErrTeaserNotFavourable = errors.New("a teased price cannot be longer than the price at the original line")
)

// -----------------------------------------------------------------------------
// Bounds and tolerances
// -----------------------------------------------------------------------------

const (
	// MaxParlayLegs is the largest number of legs a parlay may carry. It equals
	// MaxCorrelationDimension because a correlated parlay needs one correlation
	// matrix row per leg, and the two limits must not be able to drift apart.
	MaxParlayLegs = MaxCorrelationDimension

	// MaxRoundRobinTickets caps the number of parlays a single round-robin request
	// may generate. C(25, 12) is 5,200,300 tickets, which is not a bet slip; 10,000
	// is already an order of magnitude past any real ticket and keeps the response
	// bounded.
	MaxRoundRobinTickets = 10_000

	// MaxTeaserPoints bounds a teaser's point adjustment. The published ladder runs
	// from four points (basketball) through six, six and a half and seven
	// (football) to the ten-point "sweetheart" teaser; 30 is a sanity bound far
	// above all of them, not a market limit.
	MaxTeaserPoints = 30.0

	// frechetSlack is the tolerance on the Fréchet-Hoeffding bounds check in
	// boundedJoint, and the distance by which a result may be clamped onto a bound
	// rather than rejected.
	//
	// It is set from the accuracy of the machinery underneath it. At one and two
	// legs that is BivariateNormalCDF at about 1e-15, so the slack there is pure
	// headroom — the two-leg clamp fires only within an ULP or so of the bound. At
	// three or more legs it is the lattice quadrature, whose measured error against
	// an independent simulation tops out around 4e-4 in absolute probability but
	// whose excursions past a Fréchet bound are far smaller than that, because a
	// bound is only in reach when the true answer is already sitting on it.
	//
	// 1e-5 lets a converged quadrature that lands a hair past the mathematical
	// ceiling be repaired by projecting it onto the ceiling — which is itself a
	// valid probability — instead of failing a price over numerical noise, while
	// staying two orders below the size of a genuine breakdown, which misses by
	// whole percentage points.
	frechetSlack = 1e-5
)

// -----------------------------------------------------------------------------
// Joint probability
// -----------------------------------------------------------------------------

// JointProbabilityIndependent returns the probability that every leg wins,
// assuming the legs are independent: the product of the marginals.
//
// It is a product of doubles, nothing more, and it is correct only for legs that
// really are independent. For legs inside one event, use GaussianCopulaJoint.
//
// The factors are multiplied in ascending order rather than in the caller's order.
// See orderedProduct: a parlay is a set of legs, so reordering the bet slip must not
// move the price, and float64 multiplication is not associative.
//
// The product can underflow to zero for a long parlay of long shots. Zero is a
// valid Probability, so it is returned rather than treated as an error; converting
// it to a price is what fails, with ErrProbabilityNotPriceable.
func JointProbabilityIndependent(marginals []Probability) (Probability, error) {
	if err := validateMarginals(marginals); err != nil {
		return 0, err
	}
	factors := make([]float64, len(marginals))
	for i, p := range marginals {
		factors[i] = float64(p)
	}
	return NewProbability(orderedProduct(factors))
}

// orderedProduct multiplies its arguments in ascending order, leaving the caller's
// slice untouched.
//
// The sort is not an optimisation, it is the correctness fix for a real defect.
// float64 multiplication is commutative but not associative, so (a·b)·c and (a·c)·b
// can differ in the last place — three marginals of 0.75, 0.75 and 0.6015625
// multiply to 0.3384375 in one order and 0.33843749999999995 in another. A parlay is
// a set of legs, so a joint probability that depends on the order the slip was built
// in is wrong twice over: it is not a function of its argument, and it means the
// same ticket can be quoted two prices.
//
// Sorting by value makes the sequence a canonical representation of the multiset:
// factors that compare equal are bit-equal, so any tie-break among them yields the
// identical product. Ascending order is the conventional choice and costs nothing —
// the relative error of a product is bounded by (n−1)·ε whatever the order, since
// each multiplication rounds once and there is no cancellation to arrange.
func orderedProduct(factors []float64) float64 {
	ordered := make([]float64, len(factors))
	copy(ordered, factors)
	sort.Float64s(ordered)

	product := 1.0
	for _, f := range ordered {
		product *= f
	}
	return product
}

// GaussianCopulaJoint returns the probability that every leg wins, under a
// Gaussian copula with the given correlation matrix.
//
// # Model
//
// Leg i wins iff Z_i ≤ t_i where t_i = Φ⁻¹(p_i) and Z ~ N(0, R). The marginal
// probability of leg i is Φ(t_i) = p_i by construction, so the copula alters only
// the dependence between legs. The answer is the orthant probability
// Φ_n(t_1, …, t_n; R).
//
// # What is exact and what is not — stated plainly
//
//   - One leg: exact. The answer is that leg's marginal.
//   - Two legs: exact to about 1e-15. Φ₂ has a closed form, evaluated by
//     BivariateNormalCDF.
//   - R = I, any number of legs: exact, and bit-identical to
//     JointProbabilityIndependent, because the function short-circuits to it
//     rather than computing a product of ones.
//   - Three or more correlated legs: numerical. Φ_n has no closed form for n ≥ 3,
//     so it is integrated by MultivariateNormalCDF, which uses Genz's
//     separation-of-variables transformation with a shifted lattice rule and
//     iterates until its own error estimate reaches its target. That is a quadrature
//     result, not a closed form, but it is a controlled one: the routine reports
//     ErrOrthantNotConverged rather than returning an unconverged value, and the
//     tests measure the achieved accuracy against an independent seeded simulation
//     rather than assuming it.
//
// The first implementation of the n-leg case here was the pairwise (Kirkwood
// superposition) product Π p_i · Π_{i<j} Φ₂(t_i,t_j;ρ_ij)/(p_i p_j), which is
// cheaper and reproduces every structural invariant exactly. It was replaced
// because measuring it against simulation showed errors of six to twenty per cent
// of the joint probability at four to six legs and correlations around 0.3 — the
// exact regime a same-game parlay lives in. An error that size is a mispriced
// ticket, which is the thing this whole file exists to prevent.
//
// Use CorrelationMatrix.CopulaIsExact to decide whether a UI should present the
// resulting price as exact or as a numerical estimate.
//
// # Residual limits
//
// The quadrature's ordering heuristic is Genz's; its stopping rule is a refinement
// test on the running estimate, which is an error estimate rather than a proven
// bound (latticeEstimate states what it measures and what it does not). The
// result is checked against the Fréchet-Hoeffding bounds that hold under any
// dependence structure — max(0, Σp_i − (n−1)) ≤ P ≤ min p_i — and a violation
// beyond the numerical slack returns ErrParlayNotPriceable. Passing that check
// does not prove the estimate is accurate; it only proves the estimate is a
// possible probability.
//
// # Degenerate marginals
//
// A marginal of exactly 0 makes the whole parlay impossible and returns 0 without
// touching the copula. A marginal of exactly 1 is a certainty: that leg drops out
// of the model entirely, which is exactly right, because Φ₂(+∞, t_j; ρ) = p_j
// makes its every pairwise bracket 1. Both comparisons are exact equality against
// a literal, which is the correct test here — the question is whether the caller
// passed the degenerate value, not whether two computed quantities are close.
func GaussianCopulaJoint(marginals []Probability, c CorrelationMatrix) (Probability, error) {
	if err := validateMarginals(marginals); err != nil {
		return 0, err
	}
	if err := validateMatrixAgainstLegs(c, len(marginals)); err != nil {
		return 0, err
	}

	for _, p := range marginals {
		if p == 0 {
			return 0, nil // an impossible leg makes the parlay impossible
		}
	}
	if c.IsIdentity() {
		return JointProbabilityIndependent(marginals)
	}

	// Certain legs contribute a factor of 1 and a bracket of 1; drop them.
	active := make([]int, 0, len(marginals))
	for i, p := range marginals {
		if p < 1 {
			active = append(active, i)
		}
	}

	// thresholds is indexed by leg, and only the active entries are meaningful.
	thresholds := make([]float64, len(marginals))
	for _, i := range active {
		t, err := NormalQuantile(marginals[i])
		if err != nil {
			return 0, fmt.Errorf("odds: parlay leg %d at marginal %g: %w", i, float64(marginals[i]), unprefixed(err))
		}
		thresholds[i] = t
	}

	var (
		joint float64
		err   error
	)
	switch len(active) {
	case 0:
		joint = 1
	case 1:
		joint = float64(marginals[active[0]])
	case 2:
		a, b := active[0], active[1]
		rho := c.at(a, b)
		if rho == 0 {
			joint = float64(marginals[a]) * float64(marginals[b])
			break
		}
		joint, err = BivariateNormalCDF(thresholds[a], thresholds[b], rho)
		if err != nil {
			return 0, err
		}
	default:
		bound := make([]float64, len(active))
		for i, idx := range active {
			bound[i] = thresholds[idx]
		}
		// permute, not Submatrix: active is this function's own filter of
		// 0..len(marginals)-1, so it is in bounds and free of repeats and the
		// checked constructor would have nothing to reject.
		joint, err = MultivariateNormalCDF(bound, c.permute(active))
		if err != nil {
			return 0, err
		}
	}

	return boundedJoint(joint, marginals)
}

// boundedJoint checks a computed joint probability against the Fréchet-Hoeffding
// bounds and returns it as a Probability.
//
// The bounds hold for every dependence structure: the joint probability of a set
// of events can be no larger than the smallest of their individual probabilities,
// and no smaller than what the union bound forces. A result outside them by more
// than frechetSlack is not a probability and returns ErrParlayNotPriceable. A
// result outside them by less than frechetSlack is quadrature noise and is clamped
// onto the boundary, which is itself a valid probability.
func boundedJoint(joint float64, marginals []Probability) (Probability, error) {
	if math.IsNaN(joint) || math.IsInf(joint, 0) {
		return 0, fmt.Errorf("odds: correlated parlay probability over %d legs evaluated to %v: %w",
			len(marginals), joint, ErrParlayNotPriceable)
	}

	upper := 1.0
	sum := 0.0
	for _, p := range marginals {
		upper = math.Min(upper, float64(p))
		sum += float64(p)
	}
	lower := math.Max(0, sum-float64(len(marginals)-1))

	if joint > upper+frechetSlack || joint < lower-frechetSlack {
		return 0, fmt.Errorf(
			"odds: correlated parlay probability over %d legs evaluated to %g, outside the Fréchet-Hoeffding interval [%g, %g]: %w",
			len(marginals), joint, lower, upper, ErrParlayNotPriceable)
	}
	return NewProbability(math.Min(math.Max(joint, lower), upper))
}

// -----------------------------------------------------------------------------
// Parlay pricing
// -----------------------------------------------------------------------------

// ParlayDecimal returns the decimal price of a parlay of independent legs: the
// product of the leg prices.
//
// Derivation: staking one unit on leg 1 returns d_1 if it wins, and rolling that
// whole return onto leg 2 returns d_1·d_2 if both win. A parlay is exactly that
// rollover, which is why the composition is multiplicative and why the package
// keeps decimal as its canonical form.
//
// Correct only for legs in different events. See CorrelatedParlayDecimal for legs
// that move together. The product is validated on the way out, so a parlay long
// enough to overflow to +Inf returns ErrNotFinite rather than an infinity.
//
// The legs are multiplied in ascending price order, not in the caller's order, so
// that reordering the bet slip cannot move the quote by an ULP. See orderedProduct.
func ParlayDecimal(legs []Decimal) (Decimal, error) {
	if err := validateLegPrices(legs); err != nil {
		return 0, err
	}
	factors := make([]float64, len(legs))
	for i, d := range legs {
		factors[i] = float64(d)
	}
	return NewDecimal(orderedProduct(factors))
}

// CorrelatedParlayDecimal returns the decimal price of a parlay whose legs move
// together, given a correlation matrix over the legs.
//
// The price is the reciprocal of the joint probability that every leg wins,
// evaluated through the Gaussian copula. Because implied probability is exactly
// the reciprocal of decimal odds, this reduces to the plain product of the leg
// prices when the matrix is the identity — and the identity case short-circuits to
// ParlayDecimal so that it is bit-identical, not merely equal to within rounding.
//
// The margin question in the package documentation applies: this function derives
// its marginals from the leg prices as p = 1/d, so they carry whatever margin the
// legs carried and the result is a quotable price rather than a fair one. To
// price a fair parlay, devig the legs first and go through GaussianCopulaJoint
// with the fair marginals.
//
// For positively correlated legs the joint probability rises above the independent
// product, so the price falls: that shortening is the same haircut a book applies
// to a same-game parlay, arrived at from the correlation rather than from a fudge
// factor.
func CorrelatedParlayDecimal(legs []Decimal, c CorrelationMatrix) (Decimal, error) {
	if err := validateLegPrices(legs); err != nil {
		return 0, err
	}
	if err := validateMatrixAgainstLegs(c, len(legs)); err != nil {
		return 0, err
	}
	if c.IsIdentity() {
		return ParlayDecimal(legs)
	}

	marginals, err := impliedMarginals(legs)
	if err != nil {
		return 0, err
	}
	joint, err := GaussianCopulaJoint(marginals, c)
	if err != nil {
		return 0, err
	}
	return joint.Decimal()
}

// ParlayQuote is a parlay priced both ways: as if the legs were independent, and
// with the correlation applied. It exists because the difference is the number a
// user needs to see. A same-game parlay quoted at 5.20 when the naive product says
// 6.50 looks like the book stealing until the two numbers are shown side by side.
type ParlayQuote struct {
	// Legs is the number of legs in the parlay.
	Legs int

	// IndependentProbability and IndependentDecimal price the parlay as a product,
	// ignoring correlation. They are reciprocals of each other to within rounding.
	IndependentProbability Probability
	IndependentDecimal     Decimal

	// CorrelatedProbability and CorrelatedDecimal price the parlay through the
	// Gaussian copula with the supplied correlation matrix.
	CorrelatedProbability Probability
	CorrelatedDecimal     Decimal

	// Exact reports whether the correlated figures are exact rather than the
	// pairwise approximation. See CorrelationMatrix.CopulaIsExact. A user interface
	// quoting an inexact price should say so.
	Exact bool
}

// QuoteParlay prices a parlay both independently and with correlation.
func QuoteParlay(legs []Decimal, c CorrelationMatrix) (ParlayQuote, error) {
	independentDecimal, err := ParlayDecimal(legs)
	if err != nil {
		return ParlayQuote{}, err
	}
	if err := validateMatrixAgainstLegs(c, len(legs)); err != nil {
		return ParlayQuote{}, err
	}
	marginals, err := impliedMarginals(legs)
	if err != nil {
		return ParlayQuote{}, err
	}
	independentProbability, err := JointProbabilityIndependent(marginals)
	if err != nil {
		return ParlayQuote{}, err
	}
	correlatedProbability, err := GaussianCopulaJoint(marginals, c)
	if err != nil {
		return ParlayQuote{}, err
	}
	correlatedDecimal, err := CorrelatedParlayDecimal(legs, c)
	if err != nil {
		return ParlayQuote{}, err
	}

	return ParlayQuote{
		Legs:                   len(legs),
		IndependentProbability: independentProbability,
		IndependentDecimal:     independentDecimal,
		CorrelatedProbability:  correlatedProbability,
		CorrelatedDecimal:      correlatedDecimal,
		Exact:                  c.CopulaIsExact(),
	}, nil
}

// CorrelationHaircut returns the fraction of the naive independent price that
// correlation removes: (independent − correlated) / independent.
//
// It is positive when the legs are net positively correlated, which is the usual
// case for a same-game parlay and the reason such a parlay pays less than the
// product of its legs. It is negative for net negatively correlated legs, where
// the correlation lengthens the price instead.
//
// A zero-valued or unpriced quote returns 0 rather than dividing by zero.
func (q ParlayQuote) CorrelationHaircut() float64 {
	if q.IndependentDecimal <= 0 {
		return 0
	}
	return float64(q.IndependentDecimal-q.CorrelatedDecimal) / float64(q.IndependentDecimal)
}

// -----------------------------------------------------------------------------
// Round robin
// -----------------------------------------------------------------------------

// Combinations returns every k-element subset of {0, …, n−1} as a slice of
// ascending index slices, in lexicographic order: Combinations(4, 2) yields
// [0 1], [0 2], [0 3], [1 2], [1 3], [2 3].
//
// The generator is the standard odometer over an ascending index vector: advance
// the rightmost index that has not reached its ceiling, then reset everything to
// its right to the smallest legal values. It allocates one fresh slice per
// combination, so a caller may keep or mutate the results without aliasing.
//
// The count is checked against MaxRoundRobinTickets before any generation starts,
// so an oversized request fails immediately instead of allocating its way there.
func Combinations(n, k int) ([][]int, error) {
	if n <= 0 {
		return nil, fmt.Errorf("odds: combinations of %d items: %w", n, ErrNoLegs)
	}
	if n > MaxParlayLegs {
		return nil, fmt.Errorf("odds: combinations of %d items, beyond the bound %d: %w", n, MaxParlayLegs, ErrTooManyLegs)
	}
	if k < 1 || k > n {
		return nil, fmt.Errorf("odds: combination size %d is outside [1, %d]: %w", k, n, ErrCombinationSize)
	}
	count, err := binomial(n, k)
	if err != nil {
		return nil, err
	}

	out := make([][]int, 0, count)
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i
	}
	for {
		combination := make([]int, k)
		copy(combination, idx)
		out = append(out, combination)

		i := k - 1
		for i >= 0 && idx[i] == n-k+i {
			i--
		}
		if i < 0 {
			return out, nil
		}
		idx[i]++
		for j := i + 1; j < k; j++ {
			idx[j] = idx[j-1] + 1
		}
	}
}

// RoundRobinTicket is one parlay of a round robin: which legs it covers, and what
// that combination prices at.
type RoundRobinTicket struct {
	// Legs holds the indices of the covered legs, ascending, into the slice passed
	// to RoundRobin.
	Legs []int

	// Decimal is the price of this combination, correlation applied.
	Decimal Decimal
}

// RoundRobin prices every parlay of every requested size over the given legs.
//
// A round robin is the combinatorial expansion of a leg set: "five teams by
// threes" is the ten distinct 3-leg parlays over five selections, each staked
// separately. Tickets come back grouped by size in the order the sizes were
// given, and lexicographically by leg index within each size.
//
// Each ticket is priced with the principal submatrix of c restricted to its own
// legs, which is the correct correlation structure for that combination and is
// itself a valid correlation matrix (see CorrelationMatrix.Submatrix).
//
// sizes must be non-empty, each entry in [1, len(legs)], with no repeats — a
// repeated size would silently double every ticket it names. Stake accounting is
// not done here: a round robin's total stake is the ticket count times the unit
// stake, and money is integer minor units handled outside this package
// (CLAUDE.md §12). Use RoundRobinTicketCount to size it.
func RoundRobin(legs []Decimal, sizes []int, c CorrelationMatrix) ([]RoundRobinTicket, error) {
	if err := validateLegPrices(legs); err != nil {
		return nil, err
	}
	if err := validateMatrixAgainstLegs(c, len(legs)); err != nil {
		return nil, err
	}
	total, err := RoundRobinTicketCount(len(legs), sizes)
	if err != nil {
		return nil, err
	}

	tickets := make([]RoundRobinTicket, 0, total)
	for _, size := range sizes {
		combinations, err := Combinations(len(legs), size)
		if err != nil {
			return nil, err
		}
		for _, combination := range combinations {
			subLegs := make([]Decimal, len(combination))
			for i, idx := range combination {
				subLegs[i] = legs[idx]
			}
			// permute, not Submatrix: Combinations returns strictly increasing
			// index tuples drawn from 0..len(legs)-1, so validation has nothing
			// to catch.
			price, err := CorrelatedParlayDecimal(subLegs, c.permute(combination))
			if err != nil {
				return nil, fmt.Errorf("odds: round robin ticket %v: %w", combination, unprefixed(err))
			}
			tickets = append(tickets, RoundRobinTicket{Legs: combination, Decimal: price})
		}
	}
	return tickets, nil
}

// RoundRobinTicketCount reports how many parlays a round robin over n legs at the
// requested sizes produces, without generating any of them. It is the sum of the
// binomial coefficients, and it is what a bet slip multiplies the unit stake by.
func RoundRobinTicketCount(n int, sizes []int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("odds: round robin over %d legs: %w", n, ErrNoLegs)
	}
	if n > MaxParlayLegs {
		return 0, fmt.Errorf("odds: round robin over %d legs, beyond the bound %d: %w", n, MaxParlayLegs, ErrTooManyLegs)
	}
	if len(sizes) == 0 {
		return 0, fmt.Errorf("odds: round robin with no combination sizes: %w", ErrCombinationSize)
	}

	seen := make(map[int]struct{}, len(sizes))
	total := 0
	for _, size := range sizes {
		if size < 1 || size > n {
			return 0, fmt.Errorf("odds: round robin combination size %d is outside [1, %d]: %w", size, n, ErrCombinationSize)
		}
		if _, dup := seen[size]; dup {
			return 0, fmt.Errorf("odds: round robin combination size %d is repeated: %w", size, ErrCombinationSize)
		}
		seen[size] = struct{}{}

		count, err := binomial(n, size)
		if err != nil {
			return 0, err
		}
		total += count
		if total > MaxRoundRobinTickets {
			return 0, fmt.Errorf("odds: round robin over %d legs at sizes %v generates more than %d tickets: %w",
				n, sizes, MaxRoundRobinTickets, ErrTooManyCombinations)
		}
	}
	return total, nil
}

// binomial returns C(n, k), or ErrTooManyCombinations if it exceeds
// MaxRoundRobinTickets.
//
// The multiplicative recurrence C(n, i+1) = C(n, i)·(n−i)/(i+1) is used, which is
// exact in integers because C(n, i)·(n−i) is always divisible by i+1. Reflecting k
// onto min(k, n−k) halves the iteration count and keeps the intermediate as small
// as the recurrence allows. Accumulation is in int64 and the running value is
// bounded every step, so no intermediate can overflow: the largest reachable
// product before the bound trips is under 10,000 · 25.
func binomial(n, k int) (int, error) {
	if k < 0 || k > n {
		return 0, fmt.Errorf("odds: binomial(%d, %d) is undefined: %w", n, k, ErrCombinationSize)
	}
	// The reflection is an internal optimisation; the caller asked about k, so the
	// error below must name k and not the reflected value. Reporting the reflection
	// tells a user who asked for 13-team parlays out of 25 that the problem is with
	// 12-team parlays, which is a different question with a different answer.
	steps := k
	if steps > n-steps {
		steps = n - steps
	}
	result := int64(1)
	for i := 0; i < steps; i++ {
		result = result * int64(n-i) / int64(i+1)
		if result > int64(MaxRoundRobinTickets) {
			return 0, fmt.Errorf("odds: binomial(%d, %d) exceeds %d: %w", n, k, MaxRoundRobinTickets, ErrTooManyCombinations)
		}
	}
	return int(result), nil
}

// -----------------------------------------------------------------------------
// Teasers
// -----------------------------------------------------------------------------

// TeaserLeg is one leg of a teaser: a selection whose line has been moved in the
// bettor's favour, at the price that move costs.
//
// # The caller supplies the teased price, and must
//
// This package cannot derive TeasedPrice from OriginalPrice. Doing so requires a
// model of how the sport's margins are distributed — for football, the mass
// concentrated on the key numbers 3 and 7, which is why a six-point teaser through
// both is worth so much more than six points elsewhere on the ladder. That
// distribution is empirical. Estimating it belongs to the analytics phase, and
// inventing one here would be fabricated data of exactly the kind the project
// forbids. So the interface takes the moved-line price as an input, and this file
// does arithmetic on it and nothing else.
//
// Points is carried for auditing and display: it records how far the line moved,
// and validation uses it only to confirm a move was actually requested.
type TeaserLeg struct {
	// Points is how far the line moved in the bettor's favour, in points. Must be
	// finite and in (0, MaxTeaserPoints].
	Points float64

	// OriginalPrice is the price of the selection at the posted line.
	OriginalPrice Decimal

	// TeasedPrice is the price of the selection at the moved line, supplied by the
	// caller. It must be no longer than OriginalPrice.
	TeasedPrice Decimal
}

// Validate reports whether l is a usable teaser leg.
//
// Beyond validating the two prices, it enforces the one consistency check
// available without a scoring model: a favourable line move cannot make a
// selection less likely to win, so the teased price cannot be longer than the
// original. That check catches the common caller error of passing the untensed
// price, or the other side's price, in the teased slot.
func (l TeaserLeg) Validate() error {
	if math.IsNaN(l.Points) || math.IsInf(l.Points, 0) {
		return fmt.Errorf("odds: teaser points %v: %w", l.Points, ErrNotFinite)
	}
	if l.Points <= 0 || l.Points > MaxTeaserPoints {
		return fmt.Errorf("odds: teaser points %g is outside (0, %g]: %w", l.Points, MaxTeaserPoints, ErrTeaserPoints)
	}
	if err := l.OriginalPrice.Validate(); err != nil {
		return fmt.Errorf("odds: teaser original price %g: %w", float64(l.OriginalPrice), unprefixed(err))
	}
	if err := l.TeasedPrice.Validate(); err != nil {
		return fmt.Errorf("odds: teaser teased price %g: %w", float64(l.TeasedPrice), unprefixed(err))
	}
	if l.TeasedPrice > l.OriginalPrice {
		return fmt.Errorf("odds: teased price %g is longer than the original price %g after a %g point move: %w",
			float64(l.TeasedPrice), float64(l.OriginalPrice), l.Points, ErrTeaserNotFavourable)
	}
	return nil
}

// TeaserDecimal returns the true decimal price of a teaser: the correlated parlay
// price of its legs at their moved lines.
//
// A teaser is a parlay whose legs have been bought at better lines, so it is
// priced exactly as a parlay of the teased prices — and correlation matters at
// least as much, because the classic teaser is two legs of one game.
//
// This is the true price, not the posted one. Books post a fixed payout for a
// teaser by team count and point count — a two-team six-point football teaser is
// commonly −120 — and the interesting question is whether that fixed payout beats
// this true price. That comparison is an expected-value calculation and lives with
// the rest of the EV code, not here.
func TeaserDecimal(legs []TeaserLeg, c CorrelationMatrix) (Decimal, error) {
	prices, err := teasedPrices(legs)
	if err != nil {
		return 0, err
	}
	return CorrelatedParlayDecimal(prices, c)
}

// TeaserProbability returns the probability that every teased leg wins, from the
// implied probabilities of the caller-supplied teased prices.
//
// The implied/fair distinction in the package documentation applies with force
// here: teased prices as posted carry margin, so this is an implied joint
// probability. Feed it devigged teased prices to get a fair one.
func TeaserProbability(legs []TeaserLeg, c CorrelationMatrix) (Probability, error) {
	prices, err := teasedPrices(legs)
	if err != nil {
		return 0, err
	}
	marginals, err := impliedMarginals(prices)
	if err != nil {
		return 0, err
	}
	return GaussianCopulaJoint(marginals, c)
}

// teasedPrices validates every leg and extracts the moved-line prices.
func teasedPrices(legs []TeaserLeg) ([]Decimal, error) {
	if len(legs) == 0 {
		return nil, fmt.Errorf("odds: teaser with no legs: %w", ErrNoLegs)
	}
	if len(legs) > MaxParlayLegs {
		return nil, fmt.Errorf("odds: teaser with %d legs, beyond the bound %d: %w",
			len(legs), MaxParlayLegs, ErrTooManyLegs)
	}
	prices := make([]Decimal, len(legs))
	for i, l := range legs {
		if err := l.Validate(); err != nil {
			return nil, fmt.Errorf("odds: teaser leg %d: %w", i, unprefixed(err))
		}
		prices[i] = l.TeasedPrice
	}
	return prices, nil
}

// -----------------------------------------------------------------------------
// Shared validation
// -----------------------------------------------------------------------------

// validateMarginals checks the leg count and every marginal probability.
func validateMarginals(marginals []Probability) error {
	if len(marginals) == 0 {
		return fmt.Errorf("odds: parlay with no legs: %w", ErrNoLegs)
	}
	if len(marginals) > MaxParlayLegs {
		return fmt.Errorf("odds: parlay with %d legs, beyond the bound %d: %w",
			len(marginals), MaxParlayLegs, ErrTooManyLegs)
	}
	for i, p := range marginals {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("odds: parlay leg %d has marginal %g: %w", i, float64(p), unprefixed(err))
		}
	}
	return nil
}

// validateLegPrices checks the leg count and every leg price.
func validateLegPrices(legs []Decimal) error {
	if len(legs) == 0 {
		return fmt.Errorf("odds: parlay with no legs: %w", ErrNoLegs)
	}
	if len(legs) > MaxParlayLegs {
		return fmt.Errorf("odds: parlay with %d legs, beyond the bound %d: %w",
			len(legs), MaxParlayLegs, ErrTooManyLegs)
	}
	for i, d := range legs {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("odds: parlay leg %d is priced %g: %w", i, float64(d), unprefixed(err))
		}
	}
	return nil
}

// validateMatrixAgainstLegs checks that c was constructed and describes exactly n
// legs.
func validateMatrixAgainstLegs(c CorrelationMatrix, n int) error {
	if c.IsZero() {
		return fmt.Errorf("odds: correlation matrix was never constructed; use IdentityCorrelation for independent legs: %w",
			ErrCorrelationShape)
	}
	if c.N() != n {
		return fmt.Errorf("odds: %d legs against a %d×%d correlation matrix: %w", n, c.N(), c.N(), ErrLegCountMismatch)
	}
	return nil
}

// impliedMarginals converts leg prices to their implied probabilities, p = 1/d.
// The result carries whatever margin the prices carried; see the package
// documentation on the implied/fair distinction.
func impliedMarginals(legs []Decimal) ([]Probability, error) {
	marginals := make([]Probability, len(legs))
	for i, d := range legs {
		p, err := d.Probability()
		if err != nil {
			return nil, fmt.Errorf("odds: parlay leg %d priced %g: %w", i, float64(d), unprefixed(err))
		}
		marginals[i] = p
	}
	return marginals, nil
}
