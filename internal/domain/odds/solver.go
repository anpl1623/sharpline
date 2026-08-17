package odds

import (
	"errors"
	"fmt"
	"math"
)

// This file is the one-dimensional root finder that the power and Shin devigging
// methods share. It is written once, here, and tested in solver_test.go against
// functions whose roots are known in closed form — so when a devig number looks
// wrong, the solver has already been cleared of suspicion independently.
//
// Design rules, all of them consequences of the phase brief:
//
//   - Every iteration keeps a bracket that provably contains a root. A step that
//     would leave the bracket is discarded and replaced by a bisection step, so the
//     method cannot diverge, cycle, or wander off to a different root.
//   - The convergence criterion is stated, not implied (see RootConfig).
//   - The iteration count is capped. Exhausting it returns ErrRootNoConvergence and
//     no value. An unconverged estimate is never returned as if it had converged.
//   - Nothing here panics. A caller-supplied function that fails, returns NaN, or
//     returns ±Inf aborts the solve with an error.

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

// The root-finder and devig sentinels live in their own files rather than in
// errors.go. errors.go is the frozen conversion vocabulary; these are new errors
// belonging to new code, and keeping them next to the code that returns them is
// what stops the sentinel list and the implementation from drifting apart. They
// obey the same contract as the ones in errors.go: bare messages, wrapped with %w,
// matched with errors.Is and never on message text.
var (
	// ErrRootConfig reports a RootConfig that cannot be honoured: a non-positive
	// iteration cap, or a tolerance that is negative or not finite.
	ErrRootConfig = errors.New("root solver configuration is not usable")

	// ErrRootInvalidBracket reports bracket endpoints that are not a usable
	// interval: not finite, out of order, or equal.
	ErrRootInvalidBracket = errors.New("root bracket endpoints are not a valid interval")

	// ErrRootNoBracket reports that the requested interval contains no sign change,
	// so no root is known to exist inside it. This is deliberately distinct from
	// non-convergence: the solver never ran, because there was nothing to solve.
	ErrRootNoBracket = errors.New("no sign change found in the search interval")

	// ErrRootNoConvergence reports that the iteration cap was reached before either
	// convergence criterion was met. The unconverged estimate is discarded.
	ErrRootNoConvergence = errors.New("root solver did not converge within the iteration cap")

	// ErrRootFuncFailed reports that the objective function returned an error, or
	// returned NaN or ±Inf. The underlying error, when there is one, is wrapped
	// alongside this sentinel.
	ErrRootFuncFailed = errors.New("objective function failed to evaluate")
)

// -----------------------------------------------------------------------------
// Types
// -----------------------------------------------------------------------------

// RootFunc is a scalar function of one real variable that may fail to evaluate.
//
// Returning an error is the way to signal "x is outside my domain"; returning NaN
// or ±Inf is treated as a failure too, because a solver that accepts either
// silently converges on nonsense.
type RootFunc func(x float64) (float64, error)

// RootBracket is a closed interval whose endpoint values straddle zero, together
// with those values. By the intermediate value theorem a continuous f has at least
// one root inside it.
//
// The zero value is not a usable bracket. Construct one with NewRootBracket or
// FindRootBracket, both of which evaluate f and verify the sign change rather than
// taking the caller's word for it.
type RootBracket struct {
	Lo  float64 // left endpoint
	Hi  float64 // right endpoint, strictly greater than Lo
	FLo float64 // f(Lo)
	FHi float64 // f(Hi), with the opposite sign to FLo (or one of the two is exactly 0)
}

// Width returns Hi - Lo.
func (b RootBracket) Width() float64 { return b.Hi - b.Lo }

// RootConfig is the stopping rule. Every field is required; there is no zero-value
// default, because a silently-defaulted tolerance is exactly how an unconverged
// answer gets shipped. Use DefaultRootConfig and adjust it.
type RootConfig struct {
	// MaxIterations caps the number of function evaluations the iteration performs
	// after the two bracket endpoints. Reaching it is an error, never a result.
	MaxIterations int

	// XTolerance is the *relative* half-width the bracket must shrink to. The
	// iteration stops when Hi-Lo ≤ XTolerance · max(1, |Lo|, |Hi|).
	//
	// Relative rather than absolute because the two callers work on wildly
	// different scales: Shin's z lives in [0, 1), while the power exponent k can
	// legitimately run to 1e15 when a market contains a price of decimal 1.000001.
	// An absolute tolerance that is meaningful for z is unreachable for k, and one
	// that is reachable for k is meaningless for z. The max(1, ...) floor stops the
	// criterion degenerating to "exactly zero" for roots near the origin.
	XTolerance float64

	// FTolerance is the absolute residual at which the iteration stops: |f(x)| ≤
	// FTolerance is accepted as a root.
	//
	// Both criteria are checked, and either one alone is sufficient. They cover
	// different failure modes: on a steep function the bracket collapses long
	// before the residual is small, and on a flat one the residual is small long
	// before the bracket collapses.
	FTolerance float64
}

// DefaultRootConfig returns the configuration both devigging solvers use.
//
// The numbers, and why they are these numbers:
//
//   - MaxIterations 200. Plain bisection halves the bracket every step, so 200
//     steps shrink any bracket by 2^-200 — far past the point where consecutive
//     float64 are indistinguishable, which takes at most 64. The cap is a backstop
//     against a pathological objective, not a working limit; a solve that reaches
//     it has gone wrong in a way that must surface as an error.
//   - XTolerance 1e-14, which is about 45 ULPs of a value near 1. Tight enough
//     that the recovered parameter is exact to within accumulated float noise,
//     loose enough that the bracket can actually reach it — two adjacent float64
//     near 1 are 2.2e-16 apart, so the criterion is reachable with 6 ULPs to
//     spare rather than being a criterion that can only be met by exhausting the
//     iteration cap.
//   - FTolerance 1e-14. The residual is a sum of n terms each computed by a few
//     correctly-rounded operations, so its own noise floor is a few times n·1e-16.
//     For the market sizes this system prices (n ≤ 64 selections) that floor is
//     under 1e-14, and demanding less would again make the criterion unreachable.
//
// Both tolerances are ten orders of magnitude tighter than any difference the
// domain cares about: the four devig methods disagree by ~1e-2 in probability on a
// longshot, so a solver accurate to 1e-14 cannot be the reason two methods differ.
func DefaultRootConfig() RootConfig {
	return RootConfig{
		MaxIterations: 200,
		XTolerance:    1e-14,
		FTolerance:    1e-14,
	}
}

// Validate reports whether c is a usable stopping rule.
func (c RootConfig) Validate() error {
	if c.MaxIterations <= 0 {
		return fmt.Errorf("odds: max iterations %d must be strictly positive: %w", c.MaxIterations, ErrRootConfig)
	}
	if math.IsNaN(c.XTolerance) || math.IsInf(c.XTolerance, 0) || c.XTolerance < 0 {
		return fmt.Errorf("odds: x tolerance %v must be finite and non-negative: %w", c.XTolerance, ErrRootConfig)
	}
	if math.IsNaN(c.FTolerance) || math.IsInf(c.FTolerance, 0) || c.FTolerance < 0 {
		return fmt.Errorf("odds: f tolerance %v must be finite and non-negative: %w", c.FTolerance, ErrRootConfig)
	}
	if c.XTolerance == 0 && c.FTolerance == 0 {
		return fmt.Errorf("odds: both tolerances are zero, so no stopping criterion can ever be met: %w", ErrRootConfig)
	}
	return nil
}

// RootResult is a converged root together with the evidence that it converged.
type RootResult struct {
	// Root is the estimate. It always lies inside the original bracket.
	Root float64

	// Value is f(Root) as last evaluated. It is reported rather than assumed zero
	// so a caller can assert on the residual instead of trusting the solver.
	Value float64

	// Iterations is the number of function evaluations performed after the two
	// bracket endpoints.
	Iterations int

	// Width is the final bracket width, which bounds the distance from Root to a
	// true root of a continuous f.
	Width float64
}

// -----------------------------------------------------------------------------
// Bracket construction
// -----------------------------------------------------------------------------

// NewRootBracket evaluates f at lo and hi and returns the bracket if the two values
// straddle zero. An endpoint whose value is exactly zero is accepted: it is a root,
// and the solvers return it immediately.
//
// The sign test is f(lo)·f(hi) ≤ 0 written as a pair of comparisons rather than as a
// product, because the product of two small values underflows to zero and the
// product of two large ones overflows to ±Inf — both of which corrupt the test at
// exactly the magnitudes where it matters.
func NewRootBracket(f RootFunc, lo, hi float64) (RootBracket, error) {
	if f == nil {
		return RootBracket{}, fmt.Errorf("odds: objective function is nil: %w", ErrRootFuncFailed)
	}
	if math.IsNaN(lo) || math.IsInf(lo, 0) || math.IsNaN(hi) || math.IsInf(hi, 0) {
		return RootBracket{}, fmt.Errorf("odds: bracket [%v, %v] has a non-finite endpoint: %w", lo, hi, ErrRootInvalidBracket)
	}
	if !(lo < hi) {
		return RootBracket{}, fmt.Errorf("odds: bracket [%g, %g] is empty or reversed: %w", lo, hi, ErrRootInvalidBracket)
	}

	flo, err := evalRootFunc(f, lo)
	if err != nil {
		return RootBracket{}, err
	}
	fhi, err := evalRootFunc(f, hi)
	if err != nil {
		return RootBracket{}, err
	}
	if !straddlesZero(flo, fhi) {
		return RootBracket{}, fmt.Errorf("odds: f(%g) = %g and f(%g) = %g have the same sign: %w",
			lo, flo, hi, fhi, ErrRootNoBracket)
	}
	return RootBracket{Lo: lo, Hi: hi, FLo: flo, FHi: fhi}, nil
}

// FindRootBracket scans [lo, hi] on a uniform grid of samples+1 points and returns
// the *leftmost* subinterval across which f changes sign.
//
// It exists because neither devigging solver knows its bracket up front. Shin's z
// is only known to lie somewhere in [0, 1); the power exponent k is bracketed
// analytically, but the scan is the cheap insurance that the analytic bound really
// did straddle the root rather than being trusted on the strength of an argument.
//
// Leftmost, not any: if f has several roots in the interval the smallest is the one
// the devigging models mean — Shin's z is the *incidence* of insider money, and the
// second root of that equation is the degenerate z = 1 "everyone is an insider"
// solution, which is not an answer.
//
// Honest limit: a uniform scan cannot see a pair of roots that fall inside a single
// grid cell, and it reports the leftmost cell containing an odd number of roots. It
// is a bracket finder, not a root counter. The caller chooses samples with the shape
// of its own objective in mind.
func FindRootBracket(f RootFunc, lo, hi float64, samples int) (RootBracket, error) {
	if f == nil {
		return RootBracket{}, fmt.Errorf("odds: objective function is nil: %w", ErrRootFuncFailed)
	}
	if samples < 1 {
		return RootBracket{}, fmt.Errorf("odds: sample count %d must be strictly positive: %w", samples, ErrRootConfig)
	}
	if math.IsNaN(lo) || math.IsInf(lo, 0) || math.IsNaN(hi) || math.IsInf(hi, 0) {
		return RootBracket{}, fmt.Errorf("odds: scan range [%v, %v] has a non-finite endpoint: %w", lo, hi, ErrRootInvalidBracket)
	}
	if !(lo < hi) {
		return RootBracket{}, fmt.Errorf("odds: scan range [%g, %g] is empty or reversed: %w", lo, hi, ErrRootInvalidBracket)
	}

	prevX := lo
	prevF, err := evalRootFunc(f, lo)
	if err != nil {
		return RootBracket{}, err
	}
	// prevF == 0 needs no special case: straddlesZero treats an exact zero as a
	// straddle, so the first grid point closes a bracket whose FLo is already the
	// root, and the solvers return it without iterating.

	for i := 1; i <= samples; i++ {
		// Computed from the endpoints rather than accumulated by repeated addition,
		// so rounding cannot drift and the final point is exactly hi.
		x := hi
		if i < samples {
			x = lo + (hi-lo)*float64(i)/float64(samples)
		}
		fx, err := evalRootFunc(f, x)
		if err != nil {
			return RootBracket{}, err
		}
		if straddlesZero(prevF, fx) {
			return RootBracket{Lo: prevX, Hi: x, FLo: prevF, FHi: fx}, nil
		}
		prevX, prevF = x, fx
	}

	return RootBracket{}, fmt.Errorf("odds: f does not change sign across %d samples of [%g, %g]: %w",
		samples, lo, hi, ErrRootNoBracket)
}

// straddlesZero reports whether a and b lie on opposite sides of zero, counting an
// exact zero at either end as a straddle.
func straddlesZero(a, b float64) bool {
	if a == 0 || b == 0 {
		return true
	}
	return (a < 0) != (b < 0)
}

// evalRootFunc calls f and rejects a failure, a NaN, or an infinity.
func evalRootFunc(f RootFunc, x float64) (float64, error) {
	v, err := f(x)
	if err != nil {
		return 0, fmt.Errorf("odds: objective function at x = %g: %w: %w", x, err, ErrRootFuncFailed)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("odds: objective function at x = %g returned %v: %w", x, v, ErrRootFuncFailed)
	}
	return v, nil
}

// -----------------------------------------------------------------------------
// Solvers
// -----------------------------------------------------------------------------

// Bisect finds a root by repeated interval halving.
//
// It is the reference implementation: slow, but with a convergence proof that fits
// in one line — the bracket width is exactly halved every step, so after i steps it
// is 2^-i of the original and the root is located to that precision. Nothing about
// the shape of f can make it fail. Illinois is the solver the devigging code
// actually calls; Bisect exists so that solver_test.go can assert the two agree,
// which is what distinguishes "Illinois has a bug" from "the objective is wrong".
//
// Stops on either criterion in RootConfig; returns ErrRootNoConvergence if neither
// is met within MaxIterations.
func Bisect(f RootFunc, b RootBracket, cfg RootConfig) (RootResult, error) {
	return solve(f, b, cfg, false)
}

// Illinois finds a root by the Illinois variant of the false-position method.
//
// # The method
//
// Plain false position draws a secant through the two bracket endpoints and takes
// the x-intercept as the next point. It always keeps the bracket, which is the
// property worth having, but on a convex function one endpoint sticks: the same end
// is retained every step, the interval stops shrinking, and convergence degrades to
// linear with a constant close to 1.
//
// The Illinois modification (Dowell & Jarratt, 1971) fixes it in one line: when an
// endpoint is retained on two *consecutive* steps, its stored function value is
// halved. The halved value is a *weight*, no longer f at that point, and it drags
// the next secant toward the stale end until that end finally moves. The result is
// superlinear convergence with the bracket never lost.
//
// The "consecutive" qualifier is the whole algorithm. Halving on every retention
// instead — an easy simplification to reach for — damps the secant so hard that the
// method degenerates to roughly bisection speed: measured on the test functions in
// solver_test.go it took 43-52 iterations where the correct rule takes 4-15, and on
// this package's own devigging objectives it took 30-44 where the correct rule takes
// 4-8. TestIllinoisConvergesFasterThanBisection exists to keep that regression
// caught rather than merely commented on.
//
// # Why not Newton
//
// Newton is faster still, but needs a derivative and has no bracket: on the Shin
// residual, whose second root at z = 1 is a fixed point of the model rather than an
// answer, an unbracketed method can converge to the wrong root and report success.
// Trading a few evaluations for the guarantee that the returned root lies in the
// interval that was asked for is the right trade for pricing code.
//
// # Safeguard
//
// A secant step that lands outside the open bracket, or that is not finite, is
// discarded and replaced by a bisection step. That is what makes the bracket
// invariant unconditional rather than conditional on f being well behaved.
func Illinois(f RootFunc, b RootBracket, cfg RootConfig) (RootResult, error) {
	return solve(f, b, cfg, true)
}

// solve is the shared iteration. accelerate selects the Illinois secant step;
// with it false the step is always a bisection.
func solve(f RootFunc, b RootBracket, cfg RootConfig, accelerate bool) (RootResult, error) {
	if f == nil {
		return RootResult{}, fmt.Errorf("odds: objective function is nil: %w", ErrRootFuncFailed)
	}
	if err := cfg.Validate(); err != nil {
		return RootResult{}, err
	}
	if err := b.validate(); err != nil {
		return RootResult{}, err
	}

	// An endpoint that is already a root is the answer; there is nothing to iterate.
	if b.FLo == 0 {
		return RootResult{Root: b.Lo, Value: 0, Iterations: 0, Width: b.Width()}, nil
	}
	if b.FHi == 0 {
		return RootResult{Root: b.Hi, Value: 0, Iterations: 0, Width: b.Width()}, nil
	}

	lo, hi := b.Lo, b.Hi
	// flo and fhi start as true function values. Under the Illinois rule a retained
	// endpoint's entry is halved and becomes a *weight* rather than f at that point.
	flo, fhi := b.FLo, b.FHi

	// loIsNegative is the true sign of f at the low end. It is captured once and
	// never recomputed, because it cannot change: an endpoint is only ever replaced
	// by a point on its own side of the root. The side decision below tests fx
	// against this rather than against flo, so that Illinois halving — which shrinks
	// a weight towards zero and could in principle underflow to it — can never
	// corrupt the bracket invariant.
	loIsNegative := b.FLo < 0

	// retained records which endpoint survived the previous step: -1 for the high
	// end, +1 for the low end, 0 before the first step. The Illinois halving fires
	// only when the same end survives twice running, which is what distinguishes
	// the method from a heavily over-damped false position.
	retained := 0

	for i := 1; i <= cfg.MaxIterations; i++ {
		x := midpoint(lo, hi)
		if accelerate {
			if s, ok := secantStep(lo, hi, flo, fhi); ok {
				x = s
			}
		}

		fx, err := evalRootFunc(f, x)
		if err != nil {
			return RootResult{}, err
		}
		if fx == 0 {
			return RootResult{Root: x, Value: 0, Iterations: i, Width: hi - lo}, nil
		}

		// Narrow the bracket first, so the width reported alongside a residual-based
		// stop reflects the step that was just taken.
		if (fx < 0) != loIsNegative {
			// fx sits on the far side from lo, so the root is in [lo, x]: the high end
			// moves and the low end is retained.
			hi, fhi = x, fx
			if accelerate && retained == +1 {
				flo /= 2
			}
			retained = +1
		} else {
			lo, flo = x, fx
			if accelerate && retained == -1 {
				fhi /= 2
			}
			retained = -1
		}

		width := hi - lo
		if math.Abs(fx) <= cfg.FTolerance || width <= cfg.XTolerance*rootScale(lo, hi) {
			return RootResult{Root: x, Value: fx, Iterations: i, Width: width}, nil
		}
	}

	return RootResult{}, fmt.Errorf(
		"odds: no root to within |f| ≤ %g or bracket width ≤ %g·scale after %d iterations on [%g, %g]: %w",
		cfg.FTolerance, cfg.XTolerance, cfg.MaxIterations, b.Lo, b.Hi, ErrRootNoConvergence)
}

// validate re-checks a bracket that a caller may have constructed by hand.
func (b RootBracket) validate() error {
	for _, v := range [...]float64{b.Lo, b.Hi, b.FLo, b.FHi} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("odds: bracket {lo: %v, hi: %v, f(lo): %v, f(hi): %v} is not finite: %w",
				b.Lo, b.Hi, b.FLo, b.FHi, ErrRootInvalidBracket)
		}
	}
	if !(b.Lo < b.Hi) {
		return fmt.Errorf("odds: bracket [%g, %g] is empty or reversed: %w", b.Lo, b.Hi, ErrRootInvalidBracket)
	}
	if !straddlesZero(b.FLo, b.FHi) {
		return fmt.Errorf("odds: f(%g) = %g and f(%g) = %g have the same sign: %w",
			b.Lo, b.FLo, b.Hi, b.FHi, ErrRootNoBracket)
	}
	return nil
}

// midpoint returns the arithmetic mean of lo and hi, computed as lo + (hi-lo)/2
// rather than (lo+hi)/2 so that it cannot overflow for large same-signed endpoints
// and is guaranteed to land inside [lo, hi].
func midpoint(lo, hi float64) float64 { return lo + (hi-lo)/2 }

// secantStep returns the x-intercept of the line through (lo, flo) and (hi, fhi),
// and reports whether that point is usable: finite and strictly inside (lo, hi).
//
// Strictly inside matters. A secant landing exactly on an endpoint would leave the
// bracket unchanged and the iteration would stall without any criterion detecting
// it; falling back to a bisection step in that case keeps every iteration a real
// reduction.
func secantStep(lo, hi, flo, fhi float64) (float64, bool) {
	den := fhi - flo
	if den == 0 {
		return 0, false
	}
	x := hi - fhi*(hi-lo)/den
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0, false
	}
	if x <= lo || x >= hi {
		return 0, false
	}
	return x, true
}

// rootScale is the max(1, |lo|, |hi|) factor that turns RootConfig.XTolerance from
// an absolute width into a relative one. The floor of 1 keeps the criterion
// attainable for a root at or near the origin, where a purely relative test would
// demand a bracket of width zero.
func rootScale(lo, hi float64) float64 {
	return math.Max(1, math.Max(math.Abs(lo), math.Abs(hi)))
}
