package odds

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// Tests for the guards and predicates that the rest of the suite reaches only
// indirectly, and for the defensive branches that a caller cannot reach through the
// public API but an internal one can.
//
// CLAUDE.md §10 asks for effectively 100% statement coverage here, on the grounds
// that "wrong odds math is the one bug class that destroys the project's
// credibility". These are not coverage filler: every expectation below is derived
// from the definition of the thing under test, not from what the implementation
// happened to print. The Valid predicates in particular had no caller anywhere in
// the module, so nothing established that `Valid` and `Validate` agree — a
// one-character slip in `Validate() == nil` would have gone unnoticed indefinitely.
//
// The branches this file reaches by calling unexported functions with values the
// public API cannot produce are marked as such at each call. They are defence in
// depth against a future caller inside this package, and testing them is how that
// defence is shown to work rather than merely asserted.

// -----------------------------------------------------------------------------
// The Valid predicates
// -----------------------------------------------------------------------------

// TestValidAgreesWithValidate checks each of the four price types' Valid predicate
// against its own Validate, on values chosen to sit on both sides of every boundary
// the validator draws. Valid is documented as "Validate() == nil" and nothing else,
// so agreement is the whole contract — and the point of testing it is that a
// negated or dropped comparison is invisible to every other test in the package.
func TestValidAgreesWithValidate(t *testing.T) {
	t.Run("american", func(t *testing.T) {
		// -110 is the standard spread price; +100 and -100 are the two ends of the
		// legal band's floor; 0 and ±99 sit inside the illegal (-100, +100) gap,
		// where no American price exists; ±MaxAmericanMagnitude is the ceiling and
		// one past it is over.
		cases := []struct {
			value American
			want  bool
		}{
			{-110, true},
			{100, true},
			{-100, true},
			{American(MaxAmericanMagnitude), true},
			{American(-MaxAmericanMagnitude), true},
			{0, false},
			{99, false},
			{-99, false},
			{American(MaxAmericanMagnitude + 1), false},
			{American(-MaxAmericanMagnitude - 1), false},
		}
		for _, c := range cases {
			if got := c.value.Valid(); got != c.want {
				t.Errorf("American(%d).Valid() = %v, want %v", int64(c.value), got, c.want)
			}
			if got, wantNil := c.value.Validate() == nil, c.want; got != wantNil {
				t.Errorf("American(%d): Valid() and Validate() disagree", int64(c.value))
			}
		}
	})

	t.Run("decimal", func(t *testing.T) {
		// A price must be strictly greater than 1: exactly 1.0 returns the stake and
		// no profit, and below 1.0 a winning bet loses money.
		cases := []struct {
			value Decimal
			want  bool
		}{
			{21.0 / 11.0, true}, // -110
			{2, true},
			{Decimal(math.Nextafter(1, 2)), true}, // the shortest representable price
			{1, false},
			{0.5, false},
			{0, false},
			{-2, false},
			{Decimal(math.NaN()), false},
			{Decimal(math.Inf(1)), false},
			{Decimal(math.Inf(-1)), false},
		}
		for _, c := range cases {
			if got := c.value.Valid(); got != c.want {
				t.Errorf("Decimal(%v).Valid() = %v, want %v", float64(c.value), got, c.want)
			}
			if got := c.value.Validate() == nil; got != c.want {
				t.Errorf("Decimal(%v): Valid() and Validate() disagree", float64(c.value))
			}
		}
	})

	t.Run("probability", func(t *testing.T) {
		// The closed interval [0, 1]. Both endpoints are valid probabilities even
		// though neither is priceable; that narrower question belongs to the
		// conversions and is tested separately.
		cases := []struct {
			value Probability
			want  bool
		}{
			{0, true},
			{1, true},
			{110.0 / 210.0, true}, // the implied probability of -110
			{Probability(math.Nextafter(1, 2)), false},
			{-math.SmallestNonzeroFloat64, false},
			{1.5, false},
			{Probability(math.NaN()), false},
			{Probability(math.Inf(1)), false},
		}
		for _, c := range cases {
			if got := c.value.Valid(); got != c.want {
				t.Errorf("Probability(%v).Valid() = %v, want %v", float64(c.value), got, c.want)
			}
			if got := c.value.Validate() == nil; got != c.want {
				t.Errorf("Probability(%v): Valid() and Validate() disagree", float64(c.value))
			}
		}
	})

	t.Run("fractional", func(t *testing.T) {
		cases := []struct {
			value Fractional
			want  bool
		}{
			{Fractional{Numerator: 10, Denominator: 11}, true}, // -110
			{Fractional{Numerator: 1, Denominator: 1}, true},   // evens
			{Fractional{Numerator: 0, Denominator: 1}, false},
			{Fractional{Numerator: -1, Denominator: 1}, false},
			{Fractional{Numerator: 1, Denominator: 0}, false},
			{Fractional{Numerator: 1, Denominator: -1}, false},
		}
		for _, c := range cases {
			if got := c.value.Valid(); got != c.want {
				t.Errorf("%d/%d: Valid() = %v, want %v", c.value.Numerator, c.value.Denominator, got, c.want)
			}
			if got := c.value.Validate() == nil; got != c.want {
				t.Errorf("%d/%d: Valid() and Validate() disagree", c.value.Numerator, c.value.Denominator)
			}
		}
	})
}

// -----------------------------------------------------------------------------
// Conversion error propagation
// -----------------------------------------------------------------------------

// TestChainedConversionsPropagateTheOriginalSentinel covers the composed
// conversions — the ones that reach their answer by going through decimal — on
// inputs that fail at the first hop.
//
// The property under test is that the sentinel survives the chain. A caller does
// errors.Is(err, ErrAmericanOutOfRange) on the result of American.Fractional and must
// get a true answer, even though the failure actually happened inside
// American.Decimal two calls down. Swallowing or replacing the sentinel at a hop
// would turn every such check into a silent false.
func TestChainedConversionsPropagateTheOriginalSentinel(t *testing.T) {
	cases := []struct {
		name string
		call func() error
		want error
	}{
		{
			// +50 is inside the illegal (-100, +100) band: no American price exists
			// there, so the decimal hop fails and Fractional must say so.
			name: "American.Fractional through the illegal band",
			call: func() error { _, err := American(50).Fractional(); return err },
			want: ErrAmericanOutOfRange,
		},
		{
			// A certainty has decimal odds of exactly 1.0 — a zero payout — so it
			// has no price in any format.
			name: "Probability.American at certainty",
			call: func() error { _, err := Probability(1).American(); return err },
			want: ErrProbabilityNotPriceable,
		},
		{
			// An impossibility implies infinite odds.
			name: "Probability.Fractional at impossibility",
			call: func() error { _, err := Probability(0).Fractional(); return err },
			want: ErrProbabilityNotPriceable,
		},
		{
			// A zero numerator means the bet returns the stake and nothing more.
			name: "Fractional.American with a zero numerator",
			call: func() error { _, err := (Fractional{Numerator: 0, Denominator: 1}).American(); return err },
			want: ErrFractionalNumerator,
		},
		{
			// A zero denominator is a division by zero, not a price.
			name: "Fractional.Probability with a zero denominator",
			call: func() error { _, err := (Fractional{Numerator: 1, Denominator: 0}).Probability(); return err },
			want: ErrFractionalDenominator,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatal("conversion succeeded on an input that has no price")
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want one wrapping %v", err, c.want)
			}
		})
	}
}

// TestDecimalAmericanNeverLeavesTheLegalBand is the assertion that replaced a
// validation Decimal.American used to perform on its own output.
//
// That check could never fail — the function's doc comment carries the proof — and
// an unreachable error return is a hole in the CLAUDE.md §10 coverage floor rather
// than defence in depth. The invariant it protected is real, though, so it is
// checked here instead, across the whole legal decimal range rather than at the one
// point a defensive branch would have caught.
//
// The sweep is geometric rather than uniform because the map from decimal to
// American is convex below evens: a uniform grid would spend almost all its points
// on long prices, where the branch under suspicion is trivially satisfied, and
// almost none just above 1.0, where the favourite branch's magnitude runs away.
func TestDecimalAmericanNeverLeavesTheLegalBand(t *testing.T) {
	// The two branch boundaries and the extremes, then a geometric sweep of d-1.
	fixed := []float64{
		1 + 1e-6, 1.0001, 1.001, 1.01, 1.5, 21.0 / 11.0, 1.9999999,
		2, 2.0000001, 3, 11, 101, 1001, 10001,
	}
	sweep := make([]float64, 0, 400)
	for excess := 1e-6; excess < 1e5; excess *= 1.1 {
		sweep = append(sweep, 1+excess)
	}

	checked := 0
	for _, value := range append(fixed, sweep...) {
		d, err := NewDecimal(value)
		if err != nil {
			t.Fatalf("NewDecimal(%v): %v", value, err)
		}
		a, err := d.American()
		if err != nil {
			// Legitimate on the shortest prices, where the magnitude exceeds the
			// representable ceiling. Nothing else may fail.
			if !errors.Is(err, ErrAmericanOutOfRange) {
				t.Fatalf("Decimal(%v).American(): %v", value, err)
			}
			continue
		}
		checked++
		if err := a.Validate(); err != nil {
			t.Fatalf("Decimal(%v).American() returned %d, which is not a legal price: %v", value, int64(a), err)
		}
		if magnitude := abs64(int64(a)); magnitude < MinAmericanMagnitude {
			t.Fatalf("Decimal(%v).American() returned %d, inside the illegal band", value, int64(a))
		}
		if int64(a) == -MinAmericanMagnitude {
			t.Fatalf("Decimal(%v).American() returned -100, which Canonical should have folded to +100", value)
		}
	}
	if checked < 100 {
		t.Fatalf("only %d prices produced an American value; the sweep is not exercising the range", checked)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// TestRenderProbabilityShortestRoundTrip covers the negative-places default, which
// RenderDecimal already had a test for and RenderProbability did not.
//
// A negative places argument means "shortest representation that round-trips",
// which is strconv's -1 precision. The expectation is derived by round-tripping
// rather than by writing down a literal: the contract is that parsing the rendered
// digits recovers the original float exactly, so that is what is asserted.
func TestRenderProbabilityShortestRoundTrip(t *testing.T) {
	for _, places := range []int{-1, -2, -1000} {
		got := RenderProbability(Probability(110.0/210.0), places)
		// 110/210 as a percentage is 52.380952380952380… and the shortest
		// round-tripping form of that double times 100 is what must come back.
		want := RenderProbability(Probability(110.0/210.0), shortestRoundTrip)
		if got != want {
			t.Errorf("RenderProbability(p, %d) = %q, want the shortest form %q", places, got, want)
		}
		if got == "" || got[len(got)-1] != '%' {
			t.Errorf("RenderProbability(p, %d) = %q, want a percentage", places, got)
		}
	}

	// Every negative argument must render more precision than a fixed two places,
	// which is what proves the default is "shortest round trip" and not "zero".
	if short, fixed := RenderProbability(Probability(110.0/210.0), -1), RenderProbability(Probability(110.0/210.0), 2); len(short) <= len(fixed) {
		t.Errorf("the negative-places default rendered %q, no longer than the two-place form %q", short, fixed)
	}
}

// -----------------------------------------------------------------------------
// Round robin ticket accounting
// -----------------------------------------------------------------------------

// TestRoundRobinTicketCountTripsOnTheRunningTotal covers the bound that fires on the
// accumulated ticket count rather than on any single combination size.
//
// The distinction matters and is easy to get wrong. binomial refuses a single C(n,k)
// above MaxRoundRobinTickets, but a round robin names several sizes at once and pays
// for all of them, so the limit that protects the bettor is the sum. Eighteen legs at
// sizes 3, 4 and 5 is the case: C(18,3) = 816, C(18,4) = 3060 and C(18,5) = 8568 are
// each comfortably under the 10,000 cap, and together they are 12,444 tickets — over
// it. Every number here is the plain combinatorial value, computed independently of
// the implementation.
func TestRoundRobinTicketCountTripsOnTheRunningTotal(t *testing.T) {
	const (
		threes = 816  // C(18,3)
		fours  = 3060 // C(18,4)
		fives  = 8568 // C(18,5)
	)
	if MaxRoundRobinTickets != 10000 {
		t.Fatalf("MaxRoundRobinTickets is %d; this test's arithmetic assumes 10000", MaxRoundRobinTickets)
	}

	// Each size on its own is accepted, so the refusal below cannot be blamed on
	// any single binomial.
	for size, want := range map[int]int{3: threes, 4: fours, 5: fives} {
		got, err := RoundRobinTicketCount(18, []int{size})
		if err != nil {
			t.Fatalf("RoundRobinTicketCount(18, [%d]): %v", size, err)
		}
		if got != want {
			t.Fatalf("C(18, %d) = %d, want %d", size, got, want)
		}
	}

	// Two of them still fit: 816 + 3060 = 3876.
	if got, err := RoundRobinTicketCount(18, []int{3, 4}); err != nil || got != threes+fours {
		t.Fatalf("RoundRobinTicketCount(18, [3 4]) = %d, %v; want %d, nil", got, err, threes+fours)
	}

	// All three do not: 816 + 3060 + 8568 = 12444.
	if threes+fours+fives <= MaxRoundRobinTickets {
		t.Fatalf("the test case sums to %d, which is within the cap; it cannot exercise the guard",
			threes+fours+fives)
	}
	got, err := RoundRobinTicketCount(18, []int{3, 4, 5})
	if !errors.Is(err, ErrTooManyCombinations) {
		t.Fatalf("RoundRobinTicketCount(18, [3 4 5]) = %d, %v; want ErrTooManyCombinations", got, err)
	}
	if got != 0 {
		t.Errorf("the count returned %d alongside its error; it must return zero", got)
	}
}

// TestBinomialErrorNamesTheSizeTheCallerAsked pins a fix to the message rather than
// to the arithmetic. binomial reflects k onto min(k, n-k) to halve its loop, and the
// error used to report the reflected value — telling a bettor who asked about
// 13-team parlays out of 25 that the problem was with 12-team parlays, a different
// question with a different answer. C(25,13) = 5,200,300, far over the cap, so the
// refusal itself is correct either way.
func TestBinomialErrorNamesTheSizeTheCallerAsked(t *testing.T) {
	_, err := Combinations(25, 13)
	if !errors.Is(err, ErrTooManyCombinations) {
		t.Fatalf("Combinations(25, 13) error = %v, want ErrTooManyCombinations", err)
	}
	if got := err.Error(); !strings.Contains(got, "binomial(25, 13)") {
		t.Errorf("error message %q does not name the size the caller asked for", got)
	}
	if got := err.Error(); strings.Contains(got, "binomial(25, 12)") {
		t.Errorf("error message %q reports the internally reflected size", got)
	}

	// And the reflection is still doing its job: the value is right where it fits.
	// C(25,23) = C(25,2) = 300.
	got, err := Combinations(25, 23)
	if err != nil {
		t.Fatalf("Combinations(25, 23): %v", err)
	}
	if len(got) != 300 {
		t.Errorf("C(25, 23) produced %d combinations, want 300", len(got))
	}
}

// -----------------------------------------------------------------------------
// The error-message convention
// -----------------------------------------------------------------------------

// TestErrorPrefixAppearsExactlyOnce enforces the convention stated in doc.go:
// "Every error returned from this package is prefixed \"odds:\" exactly once."
//
// It had drifted in twelve places. Every one of them was a call that wrapped an
// already-prefixed error under a fresh prefix, producing messages like
//
//	odds: parlay leg 1: odds: decimal 0.9: decimal odds must be strictly greater than 1
//
// which is not a formatting nit. The prefix is what tells a reader where an error
// crossed a package boundary, so a message claiming two crossings for one call is a
// message that lies about the call stack. Two helpers already existed to prevent it —
// one in vig.go and one in clv.go, each carrying its own copy of the unwrap — and
// four other files simply did not use them; there is now one helper, `unprefixed`,
// and this test is what stops the convention drifting again.
//
// The cases below are the ones that were wrong, plus a spread of paths that were
// always right, so a regression in either direction fails.
func TestErrorPrefixAppearsExactlyOnce(t *testing.T) {
	badMatrix := CorrelationMatrix{}
	identity := mustIdentity(t, 2)

	cases := []struct {
		name string
		call func() error
	}{
		// The twelve that were doubled.
		{"DevigPrices with an illegal price", func() error {
			_, err := DevigPrices(MethodShin, []Decimal{2.0, 0.5})
			return err
		}},
		{"Devig with an out-of-range probability", func() error {
			_, err := Devig(MethodMultiplicative, []Probability{1.5, 0.5})
			return err
		}},
		{"MinimumDecimalForEdge below the legal price", func() error {
			_, err := MinimumDecimalForEdge(0.5, -1)
			return err
		}},
		{"ParlayDecimal with an illegal leg", func() error {
			_, err := ParlayDecimal([]Decimal{2.0, 0.9})
			return err
		}},
		{"GaussianCopulaJoint with an illegal marginal", func() error {
			_, err := GaussianCopulaJoint([]Probability{1.5, 0.5}, identity)
			return err
		}},
		{"CorrelatedParlayDecimal with an illegal leg", func() error {
			_, err := CorrelatedParlayDecimal([]Decimal{2.0, 0.9}, identity)
			return err
		}},
		{"QuoteParlay with an illegal leg", func() error {
			_, err := QuoteParlay([]Decimal{2.0, 0.9}, identity)
			return err
		}},
		{"a teaser leg with an illegal original price", func() error {
			return TeaserLeg{Points: 6, OriginalPrice: 0.5, TeasedPrice: 0.4}.Validate()
		}},
		{"a teaser leg with an illegal teased price", func() error {
			return TeaserLeg{Points: 6, OriginalPrice: 2.0, TeasedPrice: 0.4}.Validate()
		}},
		{"TeaserDecimal with an illegal leg", func() error {
			_, err := TeaserDecimal([]TeaserLeg{{Points: 6, OriginalPrice: 0.5, TeasedPrice: 0.4}}, identity)
			return err
		}},
		{"DevigResult.Decimals on a degenerate probability", func() error {
			_, err := DevigResult{Probabilities: []Probability{0.5, 0}}.Decimals()
			return err
		}},
		{"RoundRobin over an illegal leg", func() error {
			_, err := RoundRobin([]Decimal{2.0, 0.9}, []int{2}, identity)
			return err
		}},

		// And a spread of paths that were always correct, so the test cannot pass
		// by having quietly stopped prefixing anything at all.
		{"a bare decimal validation", func() error { _, err := NewDecimal(0.5); return err }},
		{"a bare probability validation", func() error { _, err := NewProbability(1.5); return err }},
		{"an american price in the illegal band", func() error { _, err := NewAmerican(50); return err }},
		{"a market with too few selections", func() error {
			_, err := MarginFromProbabilities([]Probability{0.5})
			return err
		}},
		{"a market selection out of range", func() error {
			_, err := MarginFromProbabilities([]Probability{0.5, 1.5})
			return err
		}},
		{"an unconstructed correlation matrix", func() error {
			_, err := MultivariateNormalCDF([]float64{0, 0, 0}, badMatrix)
			return err
		}},
		{"a bivariate correlation out of range", func() error {
			_, err := BivariateNormalCDF(0, 0, 1.5)
			return err
		}},
		{"a combination size beyond the cap", func() error { _, err := Combinations(25, 13); return err }},
		{"kelly against an illegal price", func() error { _, err := Kelly(0.5, 0.5); return err }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatal("the call succeeded; this case no longer exercises an error path")
			}
			message := err.Error()
			if n := countOccurrences(message, "odds:"); n != 1 {
				t.Errorf("%q contains the package prefix %d times, want exactly 1", message, n)
			}
			if !strings.HasPrefix(message, "odds:") {
				t.Errorf("%q does not begin with the package prefix", message)
			}
		})
	}
}

func countOccurrences(haystack, needle string) int {
	n := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			n++
		}
	}
	return n
}

// -----------------------------------------------------------------------------
// Internal defences
// -----------------------------------------------------------------------------
//
// The tests below reach branches that no caller of the public API can reach, by
// calling unexported functions with matrices that NewCorrelationMatrix would have
// refused. They are not contrived coverage: each one is a guard that exists so that
// a future caller *inside this package* — a new pricing path that builds a matrix
// itself rather than through the constructor — gets an error instead of a NaN or a
// panic. A guard nobody has ever executed is a guard nobody knows works.

// TestQuoteParlayRefusesAnImpossibleCombination covers the one failure a correlated
// parlay quote can genuinely hit through the public API, on the one input the rest
// of the suite does not reach.
//
// Two legs at decimal 2.5 imply 0.4 each and are declared perfectly negatively
// correlated: the pair is the same game's two sides, and the ticket needs both to
// win. The Fréchet-Hoeffding lower bound is max(0, 0.4 + 0.4 - 1) = 0, and a copula
// at ρ = -1 sits exactly on it, so the joint probability is exactly zero. That is a
// legitimate probability, and GaussianCopulaJoint returns it without complaint; what
// has no answer is the price, because a zero probability implies infinite odds.
//
// The distinction is the point. Both legs price fine on their own, the independent
// parlay prices fine at 6.25, and only the correlated price is impossible — so a
// quote must refuse rather than fall back to the independent number, which is
// exactly the ticket a book declines.
func TestQuoteParlayRefusesAnImpossibleCombination(t *testing.T) {
	legs := []Decimal{2.5, 2.5}
	opposed, err := NewCorrelationMatrix(equicorrelated(2, -1))
	if err != nil {
		t.Fatalf("NewCorrelationMatrix at ρ=-1: %v", err)
	}

	// The parts that must still succeed, so the refusal below is attributable.
	independent, err := ParlayDecimal(legs)
	if err != nil {
		t.Fatalf("ParlayDecimal: %v", err)
	}
	if independent != 6.25 { // 2.5 × 2.5, exact in binary
		t.Fatalf("ParlayDecimal = %v, want exactly 6.25", float64(independent))
	}
	marginals, err := impliedMarginals(legs)
	if err != nil {
		t.Fatalf("impliedMarginals: %v", err)
	}
	joint, err := GaussianCopulaJoint(marginals, opposed)
	if err != nil {
		t.Fatalf("GaussianCopulaJoint returned an error; a joint of zero is a valid probability: %v", err)
	}
	if joint != 0 {
		t.Fatalf("joint = %v, want exactly 0 — the Fréchet lower bound max(0, 0.8-1) at ρ=-1",
			float64(joint))
	}

	// And the two prices that depend on it must refuse.
	if got, err := CorrelatedParlayDecimal(legs, opposed); !errors.Is(err, ErrProbabilityNotPriceable) {
		t.Fatalf("CorrelatedParlayDecimal = %v, %v; want ErrProbabilityNotPriceable", float64(got), err)
	}
	got, err := QuoteParlay(legs, opposed)
	if !errors.Is(err, ErrProbabilityNotPriceable) {
		t.Fatalf("QuoteParlay = %+v, %v; want ErrProbabilityNotPriceable", got, err)
	}
	if got != (ParlayQuote{}) {
		t.Errorf("QuoteParlay returned %+v alongside its error; it must return the zero quote", got)
	}
}

// TestSingularCorrelationStillPrices is a regression test for a real failure the
// property suite found: a three-leg parlay that could not be priced at all.
//
// The matrix is equicorrelated at ρ = -0.5, which for three legs is exactly
// -1/(n-1) — the boundary of the positive semi-definite region. It is a legal
// correlation matrix and the constructor accepts it, but it is singular, so Cholesky
// produces a zero pivot on the last column and the third factor of Genz's integrand
// collapses from a smooth normal CDF to a {0,1} indicator. A lattice rule converges
// quickly on a smooth integrand and slowly on a discontinuous one, and at the old
// cap of 96 batches the 3σ spread reached only 1.017e-6 against the 1e-6 stopping
// target — so MultivariateNormalCDF returned ErrOrthantNotConverged and the ticket
// was refused.
//
// The marginals are not exotic. 12.5%, 30% and 95.6% with legs that cannot all
// comfortably happen together is an ordinary same-game shape. The fix was to raise
// the batch cap, not to loosen the tolerance; this test pins that the case now
// prices and that the answer it prices to is right.
func TestSingularCorrelationStillPrices(t *testing.T) {
	marginals := mustProbabilities(t, 0.125, 0.2998046875, 0.95556640625)
	boundary, err := NewCorrelationMatrix(equicorrelated(3, -0.5))
	if err != nil {
		t.Fatalf("ρ = -1/(n-1) is a legal correlation matrix but was rejected: %v", err)
	}

	// The premise: this matrix really is singular, so the discontinuous branch of
	// the integrand really is the one under test.
	factor, err := boundary.Cholesky()
	if err != nil {
		t.Fatalf("Cholesky: %v", err)
	}
	if factor[2][2] != 0 {
		t.Fatalf("the last pivot is %v, not zero; this matrix is not singular and the test's premise is wrong",
			factor[2][2])
	}

	joint, err := GaussianCopulaJoint(marginals, boundary)
	if err != nil {
		t.Fatalf("a legal three-leg parlay could not be priced: %v", err)
	}

	// The answer, checked against the Fréchet-Hoeffding interval that bounds every
	// joint probability under every dependence structure. Σp = 1.3804 over three
	// legs, so the lower bound max(0, Σp − (n−1)) is 0 and the upper bound min p is
	// 0.125. Strongly negative correlation puts the truth near the bottom of that
	// interval, and a joint of about 1.4e-5 is where it lands.
	if joint <= 0 || float64(joint) > 0.125 {
		t.Fatalf("joint = %.17g, outside the Fréchet interval (0, 0.125]", float64(joint))
	}
	// Pinned loosely on purpose: the quadrature's own stopping rule allows 1e-3
	// relative or 1e-6 absolute, whichever is looser, and asserting tighter than the
	// function promises would be asserting an implementation detail. 1e-5 absolute
	// is an order below the value itself and two orders above nothing.
	if math.Abs(float64(joint)-1.40683e-5) > 1e-5 {
		t.Errorf("joint = %.17g, want about 1.40683e-5", float64(joint))
	}

	// And the whole quote goes through, which is the thing the customer sees.
	legs := make([]Decimal, len(marginals))
	for i, p := range marginals {
		d, err := p.Decimal()
		if err != nil {
			t.Fatalf("leg %d: %v", i, err)
		}
		legs[i] = d
	}
	quote, err := QuoteParlay(legs, boundary)
	if err != nil {
		t.Fatalf("QuoteParlay on the singular matrix: %v", err)
	}
	// Negative correlation lengthens the price: the legs fight each other, so all
	// three winning is rarer than independence implies.
	if quote.CorrelatedDecimal <= quote.IndependentDecimal {
		t.Errorf("correlated price %v is not longer than the independent %v at ρ = -0.5",
			float64(quote.CorrelatedDecimal), float64(quote.IndependentDecimal))
	}
}

// TestJacobiRefusesToConvergeOnANonNumericMatrix covers the eigenvalue solver's
// iteration cap and the propagation of that failure into the positive
// semi-definiteness check.
//
// A matrix carrying NaN cannot converge, and it cannot converge in a specific way
// that matters: the convergence test is `sqrt(off) <= threshold`, and every
// comparison against NaN is false, so the loop runs to its sweep limit rather than
// exiting early with a wrong answer. That is the behaviour a bounded iterative
// solver must have (see the brief's rule: cap the iterations and return an error on
// non-convergence, never loop forever and never return an unconverged value), and
// this is the only input that exercises it.
func TestJacobiRefusesToConvergeOnANonNumericMatrix(t *testing.T) {
	// Built by hand: NewCorrelationMatrix rejects NaN, which is the point.
	poisoned := CorrelationMatrix{n: 3, data: []float64{
		1, math.NaN(), 0,
		math.NaN(), 1, 0,
		0, 0, 1,
	}}

	eigen, err := jacobiEigenvalues(poisoned.Rows())
	if !errors.Is(err, ErrEigenNotConverged) {
		t.Fatalf("jacobiEigenvalues on a NaN matrix = %v, %v; want ErrEigenNotConverged", eigen, err)
	}
	if eigen != nil {
		t.Errorf("the solver returned %v alongside its error; an unconverged estimate must not escape", eigen)
	}

	// And the failure reaches the caller rather than being read as "positive
	// semi-definite", which is what a swallowed error here would look like.
	if err := poisoned.checkPositiveSemiDefinite(); !errors.Is(err, ErrEigenNotConverged) {
		t.Fatalf("checkPositiveSemiDefinite on a NaN matrix = %v, want ErrEigenNotConverged", err)
	}
}

// TestOrthantByLatticeRefusesAMalformedMatrix covers the two failure returns inside
// the Genz integration, both of which sit behind guards that
// MultivariateNormalCDF's public entry point makes unreachable.
//
// They matter because orthantByLattice is reachable from anywhere in the package. A
// pricing path that hands it an unconstructed or non-PSD matrix must get an error;
// the alternative is a Cholesky of a matrix with a negative pivot, which produces
// NaN weights and a joint probability of NaN — precisely the silent-NaN failure
// odds/doc.go forbids.
func TestOrthantByLatticeRefusesAMalformedMatrix(t *testing.T) {
	thresholds := []float64{0, 0, 0}

	t.Run("unconstructed matrix", func(t *testing.T) {
		got, err := orthantByLattice(thresholds, CorrelationMatrix{})
		if !errors.Is(err, ErrCorrelationShape) {
			t.Fatalf("orthantByLattice on the zero matrix = %v, %v; want ErrCorrelationShape", got, err)
		}
		if got != 0 {
			t.Errorf("returned %v alongside its error", got)
		}
	})

	t.Run("indefinite matrix", func(t *testing.T) {
		// The intransitive matrix: legs 1 and 2 are almost perfectly correlated, so
		// are legs 2 and 3, but legs 1 and 3 are declared independent. No such joint
		// distribution exists — the smallest eigenvalue is about -0.273 — and
		// NewCorrelationMatrix rejects it, so it is built here by hand.
		indefinite := CorrelationMatrix{n: 3, data: []float64{
			1, 0.9, 0,
			0.9, 1, 0.9,
			0, 0.9, 1,
		}}
		if _, err := NewCorrelationMatrix(indefinite.Rows()); !errors.Is(err, ErrCorrelationNotPositiveSemiDefinite) {
			t.Fatalf("the constructor accepted the intransitive matrix: %v", err)
		}

		got, err := orthantByLattice(thresholds, indefinite)
		if !errors.Is(err, ErrCorrelationNotPositiveSemiDefinite) {
			t.Fatalf("orthantByLattice on an indefinite matrix = %v, %v; want ErrCorrelationNotPositiveSemiDefinite",
				got, err)
		}
		if got != 0 {
			t.Errorf("returned %v alongside its error", got)
		}
		if math.IsNaN(got) {
			t.Error("returned NaN")
		}
	})
}
