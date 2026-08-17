package odds

import (
	"errors"
	"math"
	"testing"
)

// The root finder is tested here entirely against functions whose roots are known
// in closed form and can be checked by hand. That separation is the point: when a
// devigged probability looks wrong, these tests have already established whether
// the solver is capable of finding a root at all, so the investigation starts at
// the objective function rather than in the iteration.
//
// Nothing in this file calls anything from devig.go.

// -----------------------------------------------------------------------------
// Tolerances
// -----------------------------------------------------------------------------

const (
	// solverRootTolerance is the absolute error allowed in a returned root for a
	// function with a simple (multiplicity-one) root.
	//
	// Justification. DefaultRootConfig stops on either |f(x)| ≤ 1e-14 or a bracket
	// narrower than 1e-14 relative. Under the second criterion the root is located
	// to 1e-14·max(1,|x|), which for every test function below (roots in [0, 4]) is
	// at most 4e-14. Under the first, the distance to the true root is |f|/|f'|, and
	// every simple root here has |f'| ≥ 0.5 in its neighbourhood, giving at most
	// 2e-14. 1e-12 is 25× the worse of the two: comfortably reachable, and still far
	// too tight to admit a genuinely broken step — a solver that stopped one
	// bisection early would be out by half a bracket width, which starts at O(1).
	solverRootTolerance = 1e-12

	// solverMultipleRootTolerance applies to x⁵, whose root at 0 has multiplicity 5.
	// There |f'| vanishes at the root, so the residual criterion is met while x is
	// still far away: |x| ≤ (1e-14)^(1/5) ≈ 6.3e-3. The bracket criterion does
	// better and is what actually fires, but the bound that can be *proved* is the
	// weaker one, so that is the bound asserted. Stating 1e-12 here would be
	// asserting luck.
	solverMultipleRootTolerance = 1e-2
)

// -----------------------------------------------------------------------------
// The test functions
// -----------------------------------------------------------------------------

// rootCase is one objective with a root known in closed form.
type rootCase struct {
	name string
	f    RootFunc
	lo   float64
	hi   float64
	want float64
	// tol overrides solverRootTolerance where the multiplicity of the root makes
	// the tighter bound unprovable.
	tol float64
	// linear marks a function Illinois solves exactly on its first secant step,
	// because a secant through two points of a straight line *is* the line.
	linear bool
}

func plainRootFunc(f func(float64) float64) RootFunc {
	return func(x float64) (float64, error) { return f(x), nil }
}

func rootCases() []rootCase {
	return []rootCase{
		{
			// 2x - 7 = 0 at x = 7/2. Exactly representable, and the one case where
			// the secant step is not an approximation.
			name: "linear 2x-7", lo: 0, hi: 10, want: 3.5, linear: true,
			f: plainRootFunc(func(x float64) float64 { return 2*x - 7 }),
		},
		{
			// x² - 2x - 3 = (x-3)(x+1). The root at 3 is the one inside [0, 10];
			// the root at -1 is outside and must not be found.
			name: "quadratic (x-3)(x+1)", lo: 0, hi: 10, want: 3,
			f: plainRootFunc(func(x float64) float64 { return x*x - 2*x - 3 }),
		},
		{
			// x² - 2 = 0 at √2, the classic irrational root.
			name: "x^2-2 gives sqrt2", lo: 0, hi: 2, want: math.Sqrt2,
			f: plainRootFunc(func(x float64) float64 { return x*x - 2 }),
		},
		{
			// x³ - 8 = 0 at exactly 2, and strongly convex, which is the shape that
			// makes unmodified false position stall.
			name: "cubic x^3-8", lo: 0, hi: 30, want: 2,
			f: plainRootFunc(func(x float64) float64 { return x*x*x - 8 }),
		},
		{
			// e^x - e = 0 at exactly 1.
			name: "exp(x)-e", lo: -5, hi: 5, want: 1,
			f: plainRootFunc(func(x float64) float64 { return math.Exp(x) - math.E }),
		},
		{
			// ln(x) = 0 at exactly 1, approached from a domain that excludes 0.
			name: "log(x)", lo: 0.01, hi: 100, want: 1,
			f: plainRootFunc(math.Log),
		},
		{
			// cos(x) - x = 0 at the Dottie number, 0.739085133215160641…, the unique
			// real fixed point of cosine. Checked against the published constant.
			name: "cos(x)-x gives the Dottie number", lo: 0, hi: 1, want: 0.7390851332151607,
			f: plainRootFunc(func(x float64) float64 { return math.Cos(x) - x }),
		},
		{
			// A root of multiplicity five at the origin: flat, ill-conditioned, and
			// the case a residual-only stopping rule handles badly.
			name: "x^5 multiple root at 0", lo: -1, hi: 2, want: 0, tol: solverMultipleRootTolerance,
			f: plainRootFunc(func(x float64) float64 { return x * x * x * x * x }),
		},
		{
			// Reversed sign convention: f decreasing rather than increasing through
			// the root, which exercises the other branch of the side test.
			name: "decreasing 3-x", lo: -10, hi: 10, want: 3,
			f: plainRootFunc(func(x float64) float64 { return 3 - x }),
		},
	}
}

func (c rootCase) tolerance() float64 {
	if c.tol > 0 {
		return c.tol
	}
	return solverRootTolerance
}

// -----------------------------------------------------------------------------
// Core behaviour
// -----------------------------------------------------------------------------

// TestSolversFindKnownRoots is the base assertion: both solvers land on the
// closed-form root of every test function, and report a root inside the bracket
// they were given.
func TestSolversFindKnownRoots(t *testing.T) {
	solvers := []struct {
		name  string
		solve func(RootFunc, RootBracket, RootConfig) (RootResult, error)
	}{
		{"Bisect", Bisect},
		{"Illinois", Illinois},
	}

	for _, s := range solvers {
		for _, c := range rootCases() {
			t.Run(s.name+"/"+c.name, func(t *testing.T) {
				b, err := NewRootBracket(c.f, c.lo, c.hi)
				if err != nil {
					t.Fatalf("NewRootBracket: %v", err)
				}
				got, err := s.solve(c.f, b, DefaultRootConfig())
				if err != nil {
					t.Fatalf("solve: %v", err)
				}
				if math.Abs(got.Root-c.want) > c.tolerance() {
					t.Errorf("root = %.17g, want %.17g (|diff| = %.3g, tolerance %.3g)",
						got.Root, c.want, math.Abs(got.Root-c.want), c.tolerance())
				}
				if got.Root < c.lo || got.Root > c.hi {
					t.Errorf("root %g escaped the bracket [%g, %g]", got.Root, c.lo, c.hi)
				}
				if got.Iterations < 0 || got.Iterations > DefaultRootConfig().MaxIterations {
					t.Errorf("iterations = %d, outside [0, %d]", got.Iterations, DefaultRootConfig().MaxIterations)
				}
				if math.IsNaN(got.Value) || math.IsInf(got.Value, 0) {
					t.Errorf("reported residual is %v", got.Value)
				}
				t.Logf("root %.17g after %d iterations, residual %.3g, final width %.3g",
					got.Root, got.Iterations, got.Value, got.Width)
			})
		}
	}
}

// TestBisectAndIllinoisAgree asserts the two independent iterations converge to the
// same place. Bisection's correctness argument is one line — the bracket halves
// every step — so agreement is evidence that Illinois's much less obvious step rule
// has not introduced a bias.
func TestBisectAndIllinoisAgree(t *testing.T) {
	for _, c := range rootCases() {
		t.Run(c.name, func(t *testing.T) {
			b, err := NewRootBracket(c.f, c.lo, c.hi)
			if err != nil {
				t.Fatalf("NewRootBracket: %v", err)
			}
			slow, err := Bisect(c.f, b, DefaultRootConfig())
			if err != nil {
				t.Fatalf("Bisect: %v", err)
			}
			fast, err := Illinois(c.f, b, DefaultRootConfig())
			if err != nil {
				t.Fatalf("Illinois: %v", err)
			}
			// Twice the tolerance, because each solver is separately allowed to be
			// one tolerance away from the true root and they may err in opposite
			// directions.
			if math.Abs(slow.Root-fast.Root) > 2*c.tolerance() {
				t.Errorf("Bisect = %.17g, Illinois = %.17g, differ by %.3g",
					slow.Root, fast.Root, math.Abs(slow.Root-fast.Root))
			}
		})
	}
}

// TestIllinoisConvergesFasterThanBisection asserts the acceleration is actually
// engaged rather than silently degenerating to bisection.
//
// It is a real regression test, not a performance nicety. The first version of
// Illinois in this package halved the retained endpoint's weight on *every*
// retention instead of only on consecutive ones. That over-damps the secant so
// severely that the method collapses back to bisection speed — 43 to 52 iterations
// on these functions against 4 to 15 for the correct rule — while still converging
// to the right answer, so every accuracy assertion in this file passed and the bug
// was invisible. Only an iteration-count comparison catches it.
//
// The threshold is a factor of two, comfortably inside the observed 3-10× margin
// and well clear of the noise of a few iterations either way.
func TestIllinoisConvergesFasterThanBisection(t *testing.T) {
	const minimumSpeedup = 2

	for _, c := range rootCases() {
		// The multiple root is excluded: with f' vanishing at the root, no secant
		// method has an advantage there, and the case exists to test robustness
		// rather than speed.
		if c.tol > 0 {
			continue
		}
		// A linear function is solved exactly in one step by construction, which is
		// covered by its own test.
		if c.linear {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			b, err := NewRootBracket(c.f, c.lo, c.hi)
			if err != nil {
				t.Fatalf("NewRootBracket: %v", err)
			}
			slow, err := Bisect(c.f, b, DefaultRootConfig())
			if err != nil {
				t.Fatalf("Bisect: %v", err)
			}
			fast, err := Illinois(c.f, b, DefaultRootConfig())
			if err != nil {
				t.Fatalf("Illinois: %v", err)
			}
			t.Logf("bisection %d iterations (|err| %.2g), illinois %d iterations (|err| %.2g)",
				slow.Iterations, math.Abs(slow.Root-c.want), fast.Iterations, math.Abs(fast.Root-c.want))
			if fast.Iterations*minimumSpeedup > slow.Iterations {
				t.Errorf("illinois took %d iterations against bisection's %d, less than the %d× speedup that "+
					"proves the endpoint-halving rule is engaged",
					fast.Iterations, slow.Iterations, minimumSpeedup)
			}
		})
	}
}

// TestIllinoisSolvesALinearFunctionInOneStep pins the property that makes the
// secant step worth having: on a straight line the secant *is* the line, so the
// first step is exact.
func TestIllinoisSolvesALinearFunctionInOneStep(t *testing.T) {
	var c rootCase
	for _, candidate := range rootCases() {
		if candidate.linear {
			c = candidate
			break
		}
	}
	if c.f == nil {
		t.Fatal("no linear case in the table")
	}

	b, err := NewRootBracket(c.f, c.lo, c.hi)
	if err != nil {
		t.Fatalf("NewRootBracket: %v", err)
	}
	got, err := Illinois(c.f, b, DefaultRootConfig())
	if err != nil {
		t.Fatalf("Illinois: %v", err)
	}
	if got.Iterations != 1 {
		t.Errorf("iterations = %d, want 1 on a linear function", got.Iterations)
	}
	// Exact equality is the property under test: 7/2 is representable, and the
	// secant arithmetic reaches it with no rounding at all.
	if got.Root != c.want {
		t.Errorf("root = %.17g, want exactly %.17g", got.Root, c.want)
	}
	if got.Value != 0 {
		t.Errorf("residual = %.17g, want exactly 0", got.Value)
	}
}

// TestRootAtBracketEndpointIsReturnedImmediately covers the degenerate bracket
// where the caller happened to hand in the answer. Iterating from there would be
// harmless but wasteful, and reporting a nonzero iteration count would misrepresent
// what happened.
func TestRootAtBracketEndpointIsReturnedImmediately(t *testing.T) {
	f := plainRootFunc(func(x float64) float64 { return x - 2 })

	for _, tc := range []struct {
		name   string
		lo, hi float64
	}{
		{"root at lo", 2, 5},
		{"root at hi", -5, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewRootBracket(f, tc.lo, tc.hi)
			if err != nil {
				t.Fatalf("NewRootBracket: %v", err)
			}
			for _, s := range []struct {
				name  string
				solve func(RootFunc, RootBracket, RootConfig) (RootResult, error)
			}{{"Bisect", Bisect}, {"Illinois", Illinois}} {
				got, err := s.solve(f, b, DefaultRootConfig())
				if err != nil {
					t.Fatalf("%s: %v", s.name, err)
				}
				if got.Root != 2 {
					t.Errorf("%s root = %v, want exactly 2", s.name, got.Root)
				}
				if got.Iterations != 0 {
					t.Errorf("%s iterations = %d, want 0", s.name, got.Iterations)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Bracketing
// -----------------------------------------------------------------------------

// TestNewRootBracketRejectsAnIntervalWithNoSignChange asserts the distinction the
// error taxonomy rests on: "there is no root here" is reported as ErrRootNoBracket,
// not as a failure to converge. The two point at completely different bugs.
func TestNewRootBracketRejectsAnIntervalWithNoSignChange(t *testing.T) {
	f := plainRootFunc(func(x float64) float64 { return x*x + 1 }) // never zero
	_, err := NewRootBracket(f, -5, 5)
	if !errors.Is(err, ErrRootNoBracket) {
		t.Errorf("err = %v, want ErrRootNoBracket", err)
	}
}

// TestNewRootBracketRejectsMalformedIntervals covers the endpoint validation.
func TestNewRootBracketRejectsMalformedIntervals(t *testing.T) {
	f := plainRootFunc(func(x float64) float64 { return x })

	cases := []struct {
		name   string
		lo, hi float64
		want   error
	}{
		{"reversed", 5, -5, ErrRootInvalidBracket},
		{"empty", 3, 3, ErrRootInvalidBracket},
		{"NaN low", math.NaN(), 1, ErrRootInvalidBracket},
		{"NaN high", -1, math.NaN(), ErrRootInvalidBracket},
		{"infinite low", math.Inf(-1), 1, ErrRootInvalidBracket},
		{"infinite high", -1, math.Inf(1), ErrRootInvalidBracket},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewRootBracket(f, c.lo, c.hi); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// TestFindRootBracketReturnsTheLeftmostSignChange uses sine over [1, 10], which has
// roots at π and 2π. Leftmost matters for Shin, whose equation always has a second,
// degenerate root at z = 1 that is not an answer.
func TestFindRootBracketReturnsTheLeftmostSignChange(t *testing.T) {
	f := plainRootFunc(math.Sin)

	b, err := FindRootBracket(f, 1, 10, 64)
	if err != nil {
		t.Fatalf("FindRootBracket: %v", err)
	}
	if !(b.Lo < math.Pi && math.Pi < b.Hi) {
		t.Fatalf("bracket [%g, %g] does not contain π", b.Lo, b.Hi)
	}
	if b.Hi >= 2*math.Pi {
		t.Errorf("bracket [%g, %g] reaches past the second root at 2π", b.Lo, b.Hi)
	}

	got, err := Illinois(f, b, DefaultRootConfig())
	if err != nil {
		t.Fatalf("Illinois: %v", err)
	}
	if math.Abs(got.Root-math.Pi) > solverRootTolerance {
		t.Errorf("root = %.17g, want π = %.17g", got.Root, math.Pi)
	}
}

// TestFindRootBracketReportsNoSignChange asserts the scan fails loudly rather than
// returning an arbitrary subinterval.
func TestFindRootBracketReportsNoSignChange(t *testing.T) {
	f := plainRootFunc(func(x float64) float64 { return x*x + 1 })
	if _, err := FindRootBracket(f, -5, 5, 128); !errors.Is(err, ErrRootNoBracket) {
		t.Errorf("err = %v, want ErrRootNoBracket", err)
	}
}

// TestFindRootBracketRejectsBadArguments covers the argument validation, including
// the sample count, which is the one place a caller can silently ask for a scan
// that inspects nothing.
func TestFindRootBracketRejectsBadArguments(t *testing.T) {
	f := plainRootFunc(func(x float64) float64 { return x })

	if _, err := FindRootBracket(f, 0, 1, 0); !errors.Is(err, ErrRootConfig) {
		t.Errorf("zero samples: err = %v, want ErrRootConfig", err)
	}
	if _, err := FindRootBracket(f, 1, 0, 10); !errors.Is(err, ErrRootInvalidBracket) {
		t.Errorf("reversed range: err = %v, want ErrRootInvalidBracket", err)
	}
	if _, err := FindRootBracket(f, 0, 0, 10); !errors.Is(err, ErrRootInvalidBracket) {
		t.Errorf("empty range: err = %v, want ErrRootInvalidBracket", err)
	}
	if _, err := FindRootBracket(nil, 0, 1, 10); !errors.Is(err, ErrRootFuncFailed) {
		t.Errorf("nil function: err = %v, want ErrRootFuncFailed", err)
	}
	for _, bad := range []struct {
		name   string
		lo, hi float64
	}{
		{"NaN low", math.NaN(), 1},
		{"NaN high", 0, math.NaN()},
		{"infinite low", math.Inf(-1), 1},
		{"infinite high", 0, math.Inf(1)},
	} {
		if _, err := FindRootBracket(f, bad.lo, bad.hi, 10); !errors.Is(err, ErrRootInvalidBracket) {
			t.Errorf("%s: err = %v, want ErrRootInvalidBracket", bad.name, err)
		}
	}
}

// TestFindRootBracketEndsExactlyOnTheUpperBound asserts the grid is computed from
// the endpoints rather than accumulated, so the last sample is exactly hi. An
// accumulated grid drifts, and the drift is invisible until a root sits within a
// rounding error of the interval's end.
func TestFindRootBracketEndsExactlyOnTheUpperBound(t *testing.T) {
	const hi = 0.1 // deliberately not representable in binary
	var last float64
	f := func(x float64) (float64, error) {
		last = x
		return x - 1, nil // no sign change on [0, 0.1], so every sample is visited
	}
	if _, err := FindRootBracket(f, 0, hi, 37); !errors.Is(err, ErrRootNoBracket) {
		t.Fatalf("err = %v, want ErrRootNoBracket", err)
	}
	if last != hi {
		t.Errorf("last sample = %.17g, want exactly %.17g", last, hi)
	}
}

// -----------------------------------------------------------------------------
// Failure modes
// -----------------------------------------------------------------------------

// TestSolversReportNonConvergence asserts the contract that matters most: an
// exhausted iteration budget returns an error and no value. A solver that returned
// its best unconverged guess would put a wrong price on the board with no signal.
func TestSolversReportNonConvergence(t *testing.T) {
	// √2 is irrational, so no finite bisection ever hits it exactly; with one
	// iteration allowed and tolerances that cannot be met in one step, both solvers
	// must give up.
	f := plainRootFunc(func(x float64) float64 { return x*x - 2 })
	b, err := NewRootBracket(f, 0, 2)
	if err != nil {
		t.Fatalf("NewRootBracket: %v", err)
	}
	cfg := RootConfig{MaxIterations: 1, XTolerance: 1e-16, FTolerance: 1e-16}

	for _, s := range []struct {
		name  string
		solve func(RootFunc, RootBracket, RootConfig) (RootResult, error)
	}{{"Bisect", Bisect}, {"Illinois", Illinois}} {
		got, err := s.solve(f, b, cfg)
		if !errors.Is(err, ErrRootNoConvergence) {
			t.Errorf("%s: err = %v, want ErrRootNoConvergence", s.name, err)
		}
		if got != (RootResult{}) {
			t.Errorf("%s: returned %+v alongside the error, want the zero value", s.name, got)
		}
	}
}

// TestSolversPropagateObjectiveFailures asserts that a function that cannot
// evaluate aborts the solve, and that its own error survives to the caller
// alongside the sentinel.
func TestSolversPropagateObjectiveFailures(t *testing.T) {
	sentinel := errors.New("domain error from the objective")

	// Fails only away from the endpoints, so the bracket builds successfully and the
	// failure surfaces mid-iteration rather than during setup.
	f := func(x float64) (float64, error) {
		if x > 0.001 && x < 1.999 {
			return 0, sentinel
		}
		return x*x - 2, nil
	}
	b, err := NewRootBracket(f, 0, 2)
	if err != nil {
		t.Fatalf("NewRootBracket: %v", err)
	}
	if _, err := Illinois(f, b, DefaultRootConfig()); !errors.Is(err, ErrRootFuncFailed) || !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to wrap both ErrRootFuncFailed and the objective's own error", err)
	}
}

// TestSolversRejectNonFiniteObjectiveValues asserts NaN and ±Inf are treated as
// failures. A solver that accepts NaN converges on nothing and reports success,
// because every comparison against NaN is false.
func TestSolversRejectNonFiniteObjectiveValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  float64
	}{{"NaN", math.NaN()}, {"+Inf", math.Inf(1)}, {"-Inf", math.Inf(-1)}} {
		t.Run(tc.name, func(t *testing.T) {
			f := func(x float64) (float64, error) {
				if x > 0.001 && x < 1.999 {
					return tc.bad, nil
				}
				return x*x - 2, nil
			}
			b, err := NewRootBracket(f, 0, 2)
			if err != nil {
				t.Fatalf("NewRootBracket: %v", err)
			}
			if _, err := Illinois(f, b, DefaultRootConfig()); !errors.Is(err, ErrRootFuncFailed) {
				t.Errorf("err = %v, want ErrRootFuncFailed", err)
			}
		})
	}
}

// TestBracketConstructionPropagatesObjectiveFailures covers the evaluation guards
// in both bracket constructors, at each endpoint separately — a constructor that
// checked only its low end would let a NaN at the high end through and the sign
// test would then silently accept it, since every comparison against NaN is false.
func TestBracketConstructionPropagatesObjectiveFailures(t *testing.T) {
	sentinel := errors.New("objective refuses this argument")
	// x+1 rather than x, so the function is strictly positive over the scan range
	// [0, 1] and FindRootBracket has no sign change to stop at before reaching the
	// point that fails. With f(x) = x the very first sample closes a bracket and the
	// mid-scan guard is never exercised, which is how the first version of this test
	// passed without testing anything.
	failAt := func(bad float64) RootFunc {
		return func(x float64) (float64, error) {
			if x == bad {
				return 0, sentinel
			}
			return x + 1, nil
		}
	}

	t.Run("NewRootBracket at the low end", func(t *testing.T) {
		if _, err := NewRootBracket(failAt(-1), -1, 1); !errors.Is(err, ErrRootFuncFailed) || !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want both ErrRootFuncFailed and the objective's error", err)
		}
	})
	t.Run("NewRootBracket at the high end", func(t *testing.T) {
		if _, err := NewRootBracket(failAt(1), -1, 1); !errors.Is(err, ErrRootFuncFailed) {
			t.Errorf("err = %v, want ErrRootFuncFailed", err)
		}
	})
	t.Run("NewRootBracket on a non-finite value", func(t *testing.T) {
		// log(-1) is NaN, so the low endpoint evaluates to something unusable.
		if _, err := NewRootBracket(plainRootFunc(math.Log), -1, 5); !errors.Is(err, ErrRootFuncFailed) {
			t.Errorf("err = %v, want ErrRootFuncFailed", err)
		}
	})
	t.Run("FindRootBracket at the first sample", func(t *testing.T) {
		if _, err := FindRootBracket(failAt(0), 0, 1, 8); !errors.Is(err, ErrRootFuncFailed) {
			t.Errorf("err = %v, want ErrRootFuncFailed", err)
		}
	})
	t.Run("FindRootBracket mid-scan", func(t *testing.T) {
		if _, err := FindRootBracket(failAt(0.5), 0, 1, 8); !errors.Is(err, ErrRootFuncFailed) {
			t.Errorf("err = %v, want ErrRootFuncFailed", err)
		}
	})
}

// TestFindRootBracketAcceptsARootOnTheLowerBound covers the case where the scan's
// very first evaluation is already zero. It needs no special handling — an exact
// zero counts as a straddle — and the solvers must then return that endpoint
// without iterating.
func TestFindRootBracketAcceptsARootOnTheLowerBound(t *testing.T) {
	f := plainRootFunc(func(x float64) float64 { return x })

	b, err := FindRootBracket(f, 0, 5, 16)
	if err != nil {
		t.Fatalf("FindRootBracket: %v", err)
	}
	if b.Lo != 0 || b.FLo != 0 {
		t.Fatalf("bracket = %+v, want the root at the low endpoint", b)
	}
	got, err := Illinois(f, b, DefaultRootConfig())
	if err != nil {
		t.Fatalf("Illinois: %v", err)
	}
	if got.Root != 0 || got.Iterations != 0 {
		t.Errorf("root = %v after %d iterations, want 0 after 0", got.Root, got.Iterations)
	}
}

// TestSolversRejectNilObjective covers the nil function guard on every entry point.
func TestSolversRejectNilObjective(t *testing.T) {
	b := RootBracket{Lo: 0, Hi: 1, FLo: -1, FHi: 1}
	if _, err := Bisect(nil, b, DefaultRootConfig()); !errors.Is(err, ErrRootFuncFailed) {
		t.Errorf("Bisect: err = %v, want ErrRootFuncFailed", err)
	}
	if _, err := Illinois(nil, b, DefaultRootConfig()); !errors.Is(err, ErrRootFuncFailed) {
		t.Errorf("Illinois: err = %v, want ErrRootFuncFailed", err)
	}
	if _, err := NewRootBracket(nil, 0, 1); !errors.Is(err, ErrRootFuncFailed) {
		t.Errorf("NewRootBracket: err = %v, want ErrRootFuncFailed", err)
	}
}

// TestSolversRejectAHandBuiltBracketThatDoesNotStraddle asserts the solvers
// re-validate a RootBracket a caller assembled by hand rather than trusting the
// struct's field values.
func TestSolversRejectAHandBuiltBracketThatDoesNotStraddle(t *testing.T) {
	f := plainRootFunc(func(x float64) float64 { return x })

	cases := []struct {
		name string
		b    RootBracket
		want error
	}{
		{"same sign", RootBracket{Lo: 1, Hi: 2, FLo: 1, FHi: 2}, ErrRootNoBracket},
		{"reversed", RootBracket{Lo: 2, Hi: 1, FLo: -1, FHi: 1}, ErrRootInvalidBracket},
		{"NaN value", RootBracket{Lo: 0, Hi: 1, FLo: math.NaN(), FHi: 1}, ErrRootInvalidBracket},
		{"infinite endpoint", RootBracket{Lo: 0, Hi: math.Inf(1), FLo: -1, FHi: 1}, ErrRootInvalidBracket},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Illinois(f, c.b, DefaultRootConfig()); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// TestRootConfigValidation covers every rejection in the stopping rule, including
// the one that is easy to miss: two zero tolerances describe a criterion that can
// never be met, so the solve would always burn its full budget and then fail.
func TestRootConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  RootConfig
		ok   bool
	}{
		{"default", DefaultRootConfig(), true},
		{"x tolerance only", RootConfig{MaxIterations: 10, XTolerance: 1e-9, FTolerance: 0}, true},
		{"f tolerance only", RootConfig{MaxIterations: 10, XTolerance: 0, FTolerance: 1e-9}, true},
		{"zero iterations", RootConfig{MaxIterations: 0, XTolerance: 1e-9, FTolerance: 1e-9}, false},
		{"negative iterations", RootConfig{MaxIterations: -1, XTolerance: 1e-9, FTolerance: 1e-9}, false},
		{"negative x tolerance", RootConfig{MaxIterations: 10, XTolerance: -1, FTolerance: 1e-9}, false},
		{"negative f tolerance", RootConfig{MaxIterations: 10, XTolerance: 1e-9, FTolerance: -1}, false},
		{"NaN tolerance", RootConfig{MaxIterations: 10, XTolerance: math.NaN(), FTolerance: 1e-9}, false},
		{"infinite tolerance", RootConfig{MaxIterations: 10, XTolerance: 1e-9, FTolerance: math.Inf(1)}, false},
		{"no criterion at all", RootConfig{MaxIterations: 10, XTolerance: 0, FTolerance: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatal("Validate() = nil, want an error")
				}
				if !errors.Is(err, ErrRootConfig) {
					t.Errorf("err = %v, want ErrRootConfig", err)
				}
			}
			// The solvers must apply the same rule, not a laxer one of their own.
			f := plainRootFunc(func(x float64) float64 { return x - 0.5 })
			b := RootBracket{Lo: 0, Hi: 1, FLo: -0.5, FHi: 0.5}
			_, solveErr := Illinois(f, b, c.cfg)
			if !c.ok && !errors.Is(solveErr, ErrRootConfig) {
				t.Errorf("Illinois with an invalid config: err = %v, want ErrRootConfig", solveErr)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Internals
// -----------------------------------------------------------------------------

// TestMidpointCannotOverflow asserts the lo + (hi-lo)/2 form. The naive (lo+hi)/2
// overflows to +Inf for two large same-signed endpoints, and the resulting Inf
// escapes the bracket on the very first step.
func TestMidpointCannotOverflow(t *testing.T) {
	lo, hi := math.MaxFloat64/2, math.MaxFloat64
	if naive := (lo + hi) / 2; !math.IsInf(naive, 1) {
		t.Skip("the naive form did not overflow on this platform; the test has nothing to prove")
	}
	got := midpoint(lo, hi)
	if math.IsInf(got, 0) || math.IsNaN(got) {
		t.Fatalf("midpoint(%g, %g) = %v", lo, hi, got)
	}
	if got < lo || got > hi {
		t.Errorf("midpoint(%g, %g) = %g, outside the interval", lo, hi, got)
	}
}

// TestSecantStepRejectsUnusableSteps pins the safeguard that keeps every iteration
// a real reduction. A step landing on an endpoint leaves the bracket unchanged, and
// the iteration would stall with no criterion noticing.
func TestSecantStepRejectsUnusableSteps(t *testing.T) {
	cases := []struct {
		name           string
		lo, hi         float64
		flo, fhi       float64
		wantOK         bool
		wantX          float64
		checkXExactly  bool
		wantXInBracket bool
	}{
		{
			name: "ordinary secant", lo: 0, hi: 2, flo: -1, fhi: 1,
			wantOK: true, wantX: 1, checkXExactly: true,
		},
		{
			name: "equal values give a zero denominator", lo: 0, hi: 2, flo: 1, fhi: 1,
			wantOK: false,
		},
		{
			name: "intercept lands on the low endpoint", lo: 0, hi: 2, flo: 0, fhi: 1,
			wantOK: false,
		},
		{
			name: "intercept lands on the high endpoint", lo: 0, hi: 2, flo: -1, fhi: 0,
			wantOK: false,
		},
		{
			name: "asymmetric weights stay inside", lo: 0, hi: 1, flo: -1e-8, fhi: 4,
			wantOK: true, wantXInBracket: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x, ok := secantStep(c.lo, c.hi, c.flo, c.fhi)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (x = %v)", ok, c.wantOK, x)
			}
			if !ok {
				return
			}
			if c.checkXExactly && x != c.wantX {
				t.Errorf("x = %.17g, want exactly %.17g", x, c.wantX)
			}
			if c.wantXInBracket && (x <= c.lo || x >= c.hi) {
				t.Errorf("x = %g escaped (%g, %g)", x, c.lo, c.hi)
			}
		})
	}
}

// TestStraddlesZero covers the sign test, including the exact-zero cases that make
// an endpoint-root work and the large/small magnitudes where writing the test as a
// product would overflow or underflow to the wrong answer.
func TestStraddlesZero(t *testing.T) {
	cases := []struct {
		name string
		a, b float64
		want bool
	}{
		{"opposite signs", -1, 1, true},
		{"same sign positive", 1, 2, false},
		{"same sign negative", -1, -2, false},
		{"zero on the left", 0, 5, true},
		{"zero on the right", -5, 0, true},
		{"both zero", 0, 0, true},
		{"product would underflow to zero", -1e-200, 1e-200, true},
		{"product would underflow, same sign", 1e-200, 1e-200, false},
		{"product would overflow to +Inf", -1e200, 1e200, true},
		{"product would overflow, same sign", 1e200, 1e200, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := straddlesZero(c.a, c.b); got != c.want {
				t.Errorf("straddlesZero(%g, %g) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestRootScaleFloorsAtOne pins the max(1, …) floor. Without it the relative
// x-tolerance would demand a bracket of width zero for a root at the origin, which
// no finite iteration can deliver.
func TestRootScaleFloorsAtOne(t *testing.T) {
	cases := []struct {
		lo, hi float64
		want   float64
	}{
		{0, 0, 1},
		{-0.1, 0.1, 1},
		{0, 1e6, 1e6},
		{-1e6, 0, 1e6},
	}
	for _, c := range cases {
		if got := rootScale(c.lo, c.hi); got != c.want {
			t.Errorf("rootScale(%g, %g) = %g, want %g", c.lo, c.hi, got, c.want)
		}
	}
}

// TestRootBracketWidth covers the accessor.
func TestRootBracketWidth(t *testing.T) {
	if got := (RootBracket{Lo: -1.5, Hi: 2.5}).Width(); got != 4 {
		t.Errorf("Width() = %g, want 4", got)
	}
}

// TestSolversNeverPanic sweeps a grid of hostile inputs through both solvers. The
// contract is total: every combination returns either a usable result or an error,
// and none of them panics or produces a non-finite root.
func TestSolversNeverPanic(t *testing.T) {
	nasty := []float64{
		math.NaN(), math.Inf(1), math.Inf(-1),
		0, -0, 1, -1, math.SmallestNonzeroFloat64, math.MaxFloat64, -math.MaxFloat64,
	}
	objectives := []RootFunc{
		plainRootFunc(func(x float64) float64 { return x }),
		plainRootFunc(func(x float64) float64 { return math.NaN() }),
		plainRootFunc(func(x float64) float64 { return math.Inf(1) }),
		plainRootFunc(math.Log),
		func(float64) (float64, error) { return 0, errors.New("always fails") },
	}

	for fi, f := range objectives {
		for _, lo := range nasty {
			for _, hi := range nasty {
				b, err := NewRootBracket(f, lo, hi)
				if err != nil {
					continue
				}
				for _, solve := range []func(RootFunc, RootBracket, RootConfig) (RootResult, error){Bisect, Illinois} {
					got, err := solve(f, b, DefaultRootConfig())
					if err != nil {
						continue
					}
					if math.IsNaN(got.Root) || math.IsInf(got.Root, 0) {
						t.Errorf("objective %d on [%v, %v]: root = %v", fi, lo, hi, got.Root)
					}
					if got.Root < b.Lo || got.Root > b.Hi {
						t.Errorf("objective %d: root %g escaped [%g, %g]", fi, got.Root, b.Lo, b.Hi)
					}
				}
			}
		}
	}
}
