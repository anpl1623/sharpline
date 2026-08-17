package odds

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// This file holds the correlation model that same-game parlay pricing rests on
// (CLAUDE.md §4, "Parlay pricing with correlation adjustment for same-game legs")
// and the three numerical primitives it needs: the standard normal CDF, its
// inverse, and the bivariate normal CDF. The Go standard library has none of the
// three, and internal/domain is forbidden external dependencies (CLAUDE.md §8),
// so each is implemented here from a published, citable algorithm and tested
// against exact closed forms.
//
// # Why a correlation model exists at all
//
// The decimal price of a parlay is the product of its leg prices only when the
// legs are independent. Legs inside one event are not: a quarterback throwing for
// 300 yards moves with his team going over the total, and a team covering a large
// spread moves against the opponent winning outright. Multiplying independent
// probabilities therefore misprices a same-game parlay — for positively
// correlated legs it understates the joint probability and so overstates the
// price, which is exactly the error that would make this system advertise edges
// that do not exist. That is a correctness defect with product consequences, not
// an academic nicety, and it is why books haircut same-game parlays.
//
// # Correlation is an input, never a constant
//
// Nothing in this file invents a correlation coefficient. Every function takes a
// validated CorrelationMatrix supplied by the caller. The values arrive later
// from observed line and result history (CLAUDE.md phase 9); inventing them here
// would be fabricated data.
//
// # Sentinel errors
//
// The package convention (errors.go) is that sentinels live in errors.go. The
// ones below are declared here because this file was added by a different author
// than errors.go and file ownership is exclusive during the build; they are part
// of the same stable contract and are matched with errors.Is exactly like the
// rest. Folding them into errors.go is a pure move with no behaviour change.

var (
	// ErrCorrelationShape reports a correlation matrix that is not square, is
	// ragged, is empty, or exceeds MaxCorrelationDimension.
	ErrCorrelationShape = errors.New("correlation matrix must be square, non-empty, and within the dimension bound")

	// ErrCorrelationNotSymmetric reports a matrix whose (i,j) and (j,i) entries
	// disagree by more than correlationSymmetryTolerance. Correlation is a
	// symmetric relation; an asymmetric matrix is a transposition bug upstream,
	// not a modelling choice.
	ErrCorrelationNotSymmetric = errors.New("correlation matrix must be symmetric")

	// ErrCorrelationDiagonal reports a diagonal entry that is not 1. The
	// correlation of a random variable with itself is 1 by definition, so a
	// diagonal that is anything else means the caller passed a covariance matrix
	// where a correlation matrix was required.
	ErrCorrelationDiagonal = errors.New("correlation matrix diagonal must be 1")

	// ErrCorrelationOutOfRange reports an off-diagonal entry that is not a finite
	// number in [-1, 1].
	ErrCorrelationOutOfRange = errors.New("correlation coefficient must be a finite number in [-1, 1]")

	// ErrCorrelationNotPositiveSemiDefinite reports a matrix that is symmetric
	// with a unit diagonal and entries in range, and is still not a correlation
	// matrix because no set of random variables can exhibit that correlation
	// structure.
	//
	// This is the failure mode that is easy to miss and expensive to ship. The
	// pairwise-plausible matrix
	//
	//	[ 1.0  0.9  0.0 ]
	//	[ 0.9  1.0  0.9 ]
	//	[ 0.0  0.9  1.0 ]
	//
	// passes every entrywise check and has a smallest eigenvalue of about -0.27:
	// A cannot be 0.9-correlated with B, B 0.9-correlated with C, and A
	// uncorrelated with C. Accepting it silently would produce a confident,
	// meaningless price.
	ErrCorrelationNotPositiveSemiDefinite = errors.New("correlation matrix is not positive semi-definite")

	// ErrEigenNotConverged reports that the cyclic Jacobi eigenvalue iteration
	// used by the positive-semi-definiteness test did not reach its convergence
	// criterion within jacobiMaxSweeps. It cannot happen for a matrix of the size
	// this package admits — Jacobi converges quadratically and needs under ten
	// sweeps in practice — but the iteration reports non-convergence rather than
	// returning an unconverged answer.
	ErrEigenNotConverged = errors.New("symmetric eigenvalue iteration did not converge")

	// ErrCorrelationIndex reports a submatrix request naming an index that is out
	// of range or repeated.
	ErrCorrelationIndex = errors.New("correlation matrix index is out of range or repeated")
)

// -----------------------------------------------------------------------------
// Bounds and tolerances
// -----------------------------------------------------------------------------

// MaxCorrelationDimension is the largest correlation matrix this package accepts,
// and therefore the largest number of legs a correlated parlay may carry (see
// MaxParlayLegs). It is a product bound rather than a numerical one — real books
// cap same-game parlays well below it — and it keeps the O(n²) pairwise loop and
// the O(n³) eigenvalue check trivially cheap.
const MaxCorrelationDimension = 25

const (
	// correlationSymmetryTolerance is the largest |r_ij - r_ji| accepted before
	// the matrix is rejected as asymmetric. A matrix that round-tripped through
	// JSON, or was assembled from a covariance estimate by dividing through by
	// standard deviations, can disagree across the diagonal in the last bit or
	// two; 1e-12 is roughly 4,500 ULPs at unit scale, which absorbs that while
	// being nine orders of magnitude tighter than any correlation difference a
	// caller could care about. Accepted matrices are symmetrised to the mean of
	// the two entries, so nothing downstream sees the discrepancy.
	correlationSymmetryTolerance = 1e-12

	// correlationDiagonalTolerance is the largest |r_ii - 1| accepted, with the
	// same reasoning and the same follow-up: an accepted diagonal is snapped to
	// exactly 1.
	correlationDiagonalTolerance = 1e-12

	// correlationRangeTolerance is how far past ±1 an off-diagonal entry may sit
	// before it is rejected rather than snapped.
	//
	// A correlation computed the way every real estimator computes it — a
	// covariance divided by the product of two standard deviations — can land an
	// ULP or two past 1 for a pair that is perfectly correlated. Rejecting that
	// would reject a legitimate estimate over float rounding, so entries within
	// this distance of ±1 are snapped onto it and anything further out is a real
	// error. 1e-12 is thousands of ULPs at unit scale and still twelve orders
	// below the smallest correlation difference that changes a price.
	correlationRangeTolerance = 1e-12

	// psdTolerance is the most negative eigenvalue tolerated before a matrix is
	// rejected as not positive semi-definite.
	//
	// A matrix that is exactly positive semi-definite in real arithmetic can have
	// its smallest eigenvalue computed as a small negative number: the error in a
	// Jacobi eigenvalue is bounded by roughly n·ε·‖R‖₂, which at n = 25 and
	// ‖R‖₂ ≤ 25 is about 25 · 2.2e-16 · 25 ≈ 1.4e-13. 1e-10 sits three orders
	// above that.
	//
	// It cannot admit a genuinely indefinite matrix: the intransitive example in
	// the ErrCorrelationNotPositiveSemiDefinite documentation has a smallest
	// eigenvalue of -0.2728, nine orders of magnitude past this bound. There is
	// no useful correlation structure in the gap.
	psdTolerance = 1e-10

	// jacobiMaxSweeps caps the cyclic Jacobi iteration. Cyclic Jacobi converges
	// quadratically once the off-diagonal norm is small, and a symmetric matrix of
	// order 25 is solved in six to ten sweeps. 64 is a backstop that guarantees
	// termination; reaching it returns ErrEigenNotConverged rather than the
	// unconverged diagonal.
	jacobiMaxSweeps = 64

	// jacobiTolerance is the convergence criterion: the iteration stops when the
	// Frobenius norm of the strictly upper triangle has fallen to this fraction of
	// the Frobenius norm of the whole matrix. At 1e-14 relative — about 45 ULPs —
	// the diagonal has converged to the eigenvalues to within the psdTolerance
	// budget above with three orders to spare.
	jacobiTolerance = 1e-14

	// choleskyZeroPivot is the pivot at or below which a Cholesky column is
	// treated as rank-deficient and zeroed. See CorrelationMatrix.Cholesky.
	choleskyZeroPivot = 1e-12
)

// -----------------------------------------------------------------------------
// CorrelationMatrix
// -----------------------------------------------------------------------------

// CorrelationMatrix is a correlation matrix that has been validated as symmetric,
// unit-diagonal, entrywise in [-1, 1], and positive semi-definite. Those four
// conditions together are exactly the definition of a correlation matrix, and the
// type exists so that a downstream pricing function can take one as a parameter
// and know it is looking at a realisable dependence structure rather than a bag
// of plausible-looking numbers.
//
// The value is immutable. Entries are stored in a private row-major slice and
// every accessor returns a copy, so a caller cannot reach in and invalidate a
// matrix after it has been checked.
//
// The zero value is deliberately invalid: IsZero reports true and N returns 0, so
// a CorrelationMatrix that was never constructed cannot be mistaken for the
// identity. Use IdentityCorrelation for the no-correlation case.
type CorrelationMatrix struct {
	n    int
	data []float64 // row-major, len == n*n; nil for the zero value
}

// NewCorrelationMatrix validates rows and returns it as a CorrelationMatrix.
//
// rows must be square, non-empty, and no larger than MaxCorrelationDimension. The
// checks run in the order symmetry → range → diagonal → positive
// semi-definiteness, cheapest and most-specific first, so the error a caller sees
// names the most actionable problem. Entries that are symmetric and diagonal
// entries that are 1 only to within the documented tolerances are snapped exactly,
// so the stored matrix is exactly symmetric with an exactly unit diagonal.
//
// The input is copied; later mutation of rows does not affect the result.
func NewCorrelationMatrix(rows [][]float64) (CorrelationMatrix, error) {
	n := len(rows)
	if n == 0 {
		return CorrelationMatrix{}, fmt.Errorf("odds: correlation matrix is empty: %w", ErrCorrelationShape)
	}
	if n > MaxCorrelationDimension {
		return CorrelationMatrix{}, fmt.Errorf("odds: correlation matrix has dimension %d, beyond the bound %d: %w",
			n, MaxCorrelationDimension, ErrCorrelationShape)
	}
	for i, row := range rows {
		if len(row) != n {
			return CorrelationMatrix{}, fmt.Errorf("odds: correlation matrix row %d has %d entries, want %d: %w",
				i, len(row), n, ErrCorrelationShape)
		}
	}

	data := make([]float64, n*n)
	for i := range n {
		for j := range n {
			v := rows[i][j]
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return CorrelationMatrix{}, fmt.Errorf("odds: correlation (%d,%d) = %v: %w", i, j, v, ErrNotFinite)
			}
			data[i*n+j] = v
		}
	}

	// The diagonal is checked before the range, not after. A diagonal entry that
	// round-tripped through arithmetic can land a bit above 1, and rejecting that
	// as "out of range" would name the wrong problem for a matrix that is in fact
	// fine; conversely a diagonal of 4 is a covariance matrix, and saying so is
	// more useful than saying 4 is not a correlation.
	for i := range n {
		if d := data[i*n+i]; math.Abs(d-1) > correlationDiagonalTolerance {
			return CorrelationMatrix{}, fmt.Errorf("odds: correlation (%d,%d) = %g, want 1: %w",
				i, i, d, ErrCorrelationDiagonal)
		}
		data[i*n+i] = 1
	}

	for i := range n {
		for j := i + 1; j < n; j++ {
			up, lo := data[i*n+j], data[j*n+i]
			for _, v := range [2]float64{up, lo} {
				if v < -1-correlationRangeTolerance || v > 1+correlationRangeTolerance {
					return CorrelationMatrix{}, fmt.Errorf("odds: correlation (%d,%d) = %g: %w",
						i, j, v, ErrCorrelationOutOfRange)
				}
			}
			up, lo = snapToUnitRange(up), snapToUnitRange(lo)
			if math.Abs(up-lo) > correlationSymmetryTolerance {
				return CorrelationMatrix{}, fmt.Errorf(
					"odds: correlation (%d,%d) = %g but (%d,%d) = %g: %w", i, j, up, j, i, lo, ErrCorrelationNotSymmetric)
			}
			mid := (up + lo) / 2
			data[i*n+j], data[j*n+i] = mid, mid
		}
	}

	c := CorrelationMatrix{n: n, data: data}
	if err := c.checkPositiveSemiDefinite(); err != nil {
		return CorrelationMatrix{}, err
	}
	return c, nil
}

// PairCorrelation names one off-diagonal entry of a correlation matrix. It is the
// pairwise-lookup form of the same information: the correlation between the leg at
// index I and the leg at index J.
type PairCorrelation struct {
	I, J int
	Rho  float64
}

// NewCorrelationMatrixFromPairs builds an n×n correlation matrix from a list of
// pairwise coefficients. The diagonal is 1 and any pair not named is 0, which is
// the right default: an unmeasured pair is one this system has no evidence about,
// and assuming independence is the only assumption that does not invent data.
//
// Naming the same unordered pair twice, or naming a diagonal entry, is an error
// rather than a silent last-write-wins. The result is validated exactly as
// NewCorrelationMatrix validates a dense matrix, so a set of individually
// plausible pairwise numbers that cannot coexist is still rejected.
func NewCorrelationMatrixFromPairs(n int, pairs []PairCorrelation) (CorrelationMatrix, error) {
	if n <= 0 || n > MaxCorrelationDimension {
		return CorrelationMatrix{}, fmt.Errorf("odds: correlation dimension %d is outside [1, %d]: %w",
			n, MaxCorrelationDimension, ErrCorrelationShape)
	}

	rows := make([][]float64, n)
	for i := range rows {
		rows[i] = make([]float64, n)
		rows[i][i] = 1
	}

	seen := make(map[[2]int]struct{}, len(pairs))
	for _, p := range pairs {
		if p.I < 0 || p.I >= n || p.J < 0 || p.J >= n || p.I == p.J {
			return CorrelationMatrix{}, fmt.Errorf("odds: pair (%d,%d) is not an off-diagonal entry of an %d×%d matrix: %w",
				p.I, p.J, n, n, ErrCorrelationIndex)
		}
		key := [2]int{min(p.I, p.J), max(p.I, p.J)}
		if _, dup := seen[key]; dup {
			return CorrelationMatrix{}, fmt.Errorf("odds: pair (%d,%d) is specified more than once: %w",
				p.I, p.J, ErrCorrelationIndex)
		}
		seen[key] = struct{}{}
		rows[p.I][p.J] = p.Rho
		rows[p.J][p.I] = p.Rho
	}
	return NewCorrelationMatrix(rows)
}

// IdentityCorrelation returns the n×n identity: every leg independent of every
// other. It is the matrix to pass for a parlay whose legs are in different games,
// and it is the only matrix for which correlated pricing is guaranteed to
// reproduce the independent product exactly.
func IdentityCorrelation(n int) (CorrelationMatrix, error) {
	if n <= 0 || n > MaxCorrelationDimension {
		return CorrelationMatrix{}, fmt.Errorf("odds: correlation dimension %d is outside [1, %d]: %w",
			n, MaxCorrelationDimension, ErrCorrelationShape)
	}
	data := make([]float64, n*n)
	for i := range n {
		data[i*n+i] = 1
	}
	return CorrelationMatrix{n: n, data: data}, nil
}

// N returns the dimension of the matrix, which is the number of legs it describes.
// The zero value returns 0.
func (c CorrelationMatrix) N() int { return c.n }

// IsZero reports whether c is the zero value, i.e. was never constructed.
func (c CorrelationMatrix) IsZero() bool { return c.n == 0 || c.data == nil }

// At returns the correlation between legs i and j and reports whether both
// indices were in range. Out-of-range indices return (0, false) rather than
// panicking, because nothing in this package panics.
func (c CorrelationMatrix) At(i, j int) (float64, bool) {
	if c.IsZero() || i < 0 || j < 0 || i >= c.n || j >= c.n {
		return 0, false
	}
	return c.at(i, j), true
}

// at reads an entry without bounds checking. Every call site has already
// established that i and j are in range.
func (c CorrelationMatrix) at(i, j int) float64 { return c.data[i*c.n+j] }

// Rows returns the matrix as a freshly allocated slice of rows. Mutating the
// result cannot affect c.
func (c CorrelationMatrix) Rows() [][]float64 {
	if c.IsZero() {
		return nil
	}
	rows := make([][]float64, c.n)
	for i := range c.n {
		rows[i] = make([]float64, c.n)
		copy(rows[i], c.data[i*c.n:(i+1)*c.n])
	}
	return rows
}

// IsIdentity reports whether every off-diagonal entry is exactly zero, i.e. every
// leg is independent of every other.
//
// The test is exact equality against zero, which is the one place in this package
// where that is the correct comparison: it is not asking whether two computed
// quantities are close, it is asking whether the caller supplied any correlation
// at all. A matrix with an off-diagonal of 1e-300 is a matrix with correlation in
// it, and it takes the correlated code path.
func (c CorrelationMatrix) IsIdentity() bool {
	if c.IsZero() {
		return false
	}
	for i := range c.n {
		for j := i + 1; j < c.n; j++ {
			if c.at(i, j) != 0 {
				return false
			}
		}
	}
	return true
}

// CopulaIsExact reports whether GaussianCopulaJoint evaluates this matrix exactly
// rather than through the numerical integration in MultivariateNormalCDF.
//
// It is exact in two cases: at most two legs, where the joint orthant probability
// has the closed form implemented by BivariateNormalCDF, and the identity, where
// the joint probability is the product of the marginals. Everything else goes
// through the lattice quadrature documented on MultivariateNormalCDF, which is
// accurate but numerical, and a UI quoting such a price should say so.
func (c CorrelationMatrix) CopulaIsExact() bool {
	return !c.IsZero() && (c.n <= 2 || c.IsIdentity())
}

// Submatrix returns the correlation matrix restricted to the given leg indices,
// in the order given. It is what prices one parlay of a round robin: the k-leg
// combination carries the k×k block of the full matrix.
//
// A principal submatrix of a positive semi-definite matrix is itself positive
// semi-definite (take the quadratic form over vectors supported on the chosen
// indices), and symmetry and the unit diagonal are inherited entry by entry. The
// result is therefore valid by construction and is not re-validated.
func (c CorrelationMatrix) Submatrix(indices []int) (CorrelationMatrix, error) {
	if c.IsZero() {
		return CorrelationMatrix{}, fmt.Errorf("odds: submatrix of an unconstructed correlation matrix: %w", ErrCorrelationShape)
	}
	k := len(indices)
	if k == 0 || k > c.n {
		return CorrelationMatrix{}, fmt.Errorf("odds: submatrix of %d indices from a %d×%d matrix: %w",
			k, c.n, c.n, ErrCorrelationShape)
	}
	seen := make(map[int]struct{}, k)
	for _, idx := range indices {
		if idx < 0 || idx >= c.n {
			return CorrelationMatrix{}, fmt.Errorf("odds: submatrix index %d is outside [0, %d): %w",
				idx, c.n, ErrCorrelationIndex)
		}
		if _, dup := seen[idx]; dup {
			return CorrelationMatrix{}, fmt.Errorf("odds: submatrix index %d appears twice: %w", idx, ErrCorrelationIndex)
		}
		seen[idx] = struct{}{}
	}

	return c.permute(indices), nil
}

// permute returns the principal submatrix on the given indices without validating
// them. Submatrix is the checked entry point and every external caller goes through
// it; this exists for the internal callers that generate their own index list — a
// permutation, or the positions surviving a filter — where the check can only ever
// pass and its error return would be dead code.
//
// The caller guarantees that every index is in [0, c.N()) and that no index repeats.
func (c CorrelationMatrix) permute(indices []int) CorrelationMatrix {
	k := len(indices)
	data := make([]float64, k*k)
	for i, ii := range indices {
		for j, jj := range indices {
			data[i*k+j] = c.at(ii, jj)
		}
	}
	return CorrelationMatrix{n: k, data: data}
}

// Eigenvalues returns the eigenvalues of the matrix in ascending order, computed
// by the cyclic Jacobi method (see jacobiEigenvalues). They sum to N, since the
// diagonal is all ones and the trace is the sum of the eigenvalues — a cheap
// invariant the tests assert.
func (c CorrelationMatrix) Eigenvalues() ([]float64, error) {
	if c.IsZero() {
		return nil, fmt.Errorf("odds: eigenvalues of an unconstructed correlation matrix: %w", ErrCorrelationShape)
	}
	return jacobiEigenvalues(c.Rows())
}

// Cholesky returns the lower-triangular L with L·Lᵀ = c. It is what a simulation
// needs to draw correlated normal deviates: if z is a vector of independent
// standard normals then L·z has covariance c.
//
// The factorisation is unpivoted. For a positive definite matrix — which is every
// correlation estimate that is not exactly rank-deficient — that is
// unconditionally correct and accurate to machine precision. A positive
// semi-definite matrix can be singular, for example the 2×2 matrix of all ones,
// and there the pivot for a dependent column is zero; that column is set to zero,
// which is exact in real arithmetic because a zero Schur-complement diagonal in a
// positive semi-definite matrix forces the rest of its column to zero by
// Cauchy-Schwarz. In floating point the column is not exactly zero but is of order
// 1e-16, so zeroing it costs nothing measurable. Symmetric pivoting (Higham,
// "Analysis of the Cholesky decomposition of a semi-definite matrix", 1990) would
// improve the numerical behaviour of the rank-deficient case; it is not needed for
// correctness at this size and is not implemented.
func (c CorrelationMatrix) Cholesky() ([][]float64, error) {
	if c.IsZero() {
		return nil, fmt.Errorf("odds: cholesky of an unconstructed correlation matrix: %w", ErrCorrelationShape)
	}
	n := c.n
	l := make([][]float64, n)
	for i := range l {
		l[i] = make([]float64, n)
	}

	for j := range n {
		sum := c.at(j, j)
		for k := range j {
			sum -= l[j][k] * l[j][k]
		}
		if sum < -psdTolerance {
			return nil, fmt.Errorf("odds: cholesky pivot %d is %g: %w", j, sum, ErrCorrelationNotPositiveSemiDefinite)
		}
		if sum <= choleskyZeroPivot {
			// Rank-deficient column: the pivot and the whole column below it are
			// zero for a positive semi-definite matrix.
			continue
		}
		pivot := math.Sqrt(sum)
		l[j][j] = pivot
		for i := j + 1; i < n; i++ {
			s := c.at(i, j)
			for k := range j {
				s -= l[i][k] * l[j][k]
			}
			l[i][j] = s / pivot
		}
	}
	return l, nil
}

// checkPositiveSemiDefinite rejects c if its smallest eigenvalue is below
// -psdTolerance.
func (c CorrelationMatrix) checkPositiveSemiDefinite() error {
	eigen, err := jacobiEigenvalues(c.Rows())
	if err != nil {
		return err
	}
	if smallest := eigen[0]; smallest < -psdTolerance {
		return fmt.Errorf("odds: correlation matrix has smallest eigenvalue %g, below the tolerance -%g: %w",
			smallest, psdTolerance, ErrCorrelationNotPositiveSemiDefinite)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Symmetric eigenvalues: the cyclic Jacobi method
// -----------------------------------------------------------------------------

// jacobiEigenvalues returns the eigenvalues of a real symmetric matrix in
// ascending order, by the cyclic Jacobi method.
//
// # Algorithm
//
// Jacobi (1846) repeatedly applies plane rotations J(p,q,θ) that annihilate one
// off-diagonal pair, A ← Jᵀ A J. Each rotation is orthogonal, so the eigenvalues
// are preserved exactly and the Frobenius norm of the off-diagonal strictly
// decreases; sweeping over every (p,q) with p < q drives the off-diagonal to zero
// and leaves the eigenvalues on the diagonal. The rotation angle is chosen by the
// standard numerically stable formula, taking the smaller root so that |t| ≤ 1 and
// the rotation never mixes a large diagonal entry into a small one. See Golub &
// Van Loan, Matrix Computations, 4th ed., §8.5.
//
// Jacobi is chosen over the faster tridiagonal-QR path for two reasons that matter
// here and would not matter in a linear algebra library. It is short enough to
// audit, and it is unconditionally stable and known to compute small eigenvalues
// with high relative accuracy (Demmel & Veselić, 1992) — which is the entire
// question being asked, since the caller is being told whether the smallest
// eigenvalue is negative. At n ≤ 25 its cubic cost per sweep is irrelevant.
//
// # Convergence criterion and iteration cap
//
// The iteration stops when off(A) — the Frobenius norm of the strictly upper
// triangle — has fallen below jacobiTolerance times the Frobenius norm of A, and
// gives up after jacobiMaxSweeps sweeps with ErrEigenNotConverged rather than
// returning an unconverged diagonal. Cyclic Jacobi converges quadratically once
// off(A) is small, so the cap is a backstop that is not reached at this size.
//
// The input is assumed square and is mutated in place; every call site passes a
// fresh copy from CorrelationMatrix.Rows.
func jacobiEigenvalues(a [][]float64) ([]float64, error) {
	n := len(a)
	if n == 0 {
		return nil, fmt.Errorf("odds: eigenvalues of an empty matrix: %w", ErrCorrelationShape)
	}
	if n == 1 {
		return []float64{a[0][0]}, nil
	}

	// The Frobenius norm is invariant under the orthogonal rotations, so the
	// convergence threshold is computed once from the input.
	var frobenius float64
	for i := range n {
		for j := range n {
			frobenius += a[i][j] * a[i][j]
		}
	}
	threshold := jacobiTolerance * math.Sqrt(frobenius)

	converged := false
	for sweep := 0; sweep < jacobiMaxSweeps && !converged; sweep++ {
		var off float64
		for p := range n {
			for q := p + 1; q < n; q++ {
				off += a[p][q] * a[p][q]
			}
		}
		if math.Sqrt(off) <= threshold {
			converged = true
			break
		}

		for p := range n {
			for q := p + 1; q < n; q++ {
				apq := a[p][q]
				if apq == 0 {
					continue
				}
				// theta = cot(2θ); t = tan θ is the root of t² + 2·theta·t - 1 = 0
				// with the smaller magnitude, written so that no cancellation occurs.
				theta := (a[q][q] - a[p][p]) / (2 * apq)
				var t float64
				if theta >= 0 {
					t = 1 / (theta + math.Sqrt(theta*theta+1))
				} else {
					t = -1 / (-theta + math.Sqrt(theta*theta+1))
				}
				cs := 1 / math.Sqrt(t*t+1)
				sn := t * cs

				for k := range n { // A ← A·J
					akp, akq := a[k][p], a[k][q]
					a[k][p] = cs*akp - sn*akq
					a[k][q] = sn*akp + cs*akq
				}
				for k := range n { // A ← Jᵀ·A
					apk, aqk := a[p][k], a[q][k]
					a[p][k] = cs*apk - sn*aqk
					a[q][k] = sn*apk + cs*aqk
				}
			}
		}
	}
	if !converged {
		return nil, fmt.Errorf("odds: jacobi iteration did not converge in %d sweeps: %w",
			jacobiMaxSweeps, ErrEigenNotConverged)
	}

	eigen := make([]float64, n)
	for i := range n {
		eigen[i] = a[i][i]
	}
	sort.Float64s(eigen)
	return eigen, nil
}

// -----------------------------------------------------------------------------
// Numerical primitives: Φ, Φ⁻¹, Φ₂
// -----------------------------------------------------------------------------

// NormalCDF returns Φ(x), the cumulative distribution function of the standard
// normal, using the identity Φ(x) = ½·erfc(−x/√2) over the standard library's
// math.Erfc. That routine is the FDLIBM implementation, accurate to under one ULP,
// and the identity is exact rather than an approximation — which is why this
// package implements Φ⁻¹ and Φ₂ from published algorithms but does not implement Φ.
//
// ±Inf are accepted and map to 1 and 0, which are the correct limits. NaN is
// rejected with ErrNotFinite rather than propagated.
func NormalCDF(x float64) (float64, error) {
	if math.IsNaN(x) {
		return 0, fmt.Errorf("odds: normal cdf argument %v: %w", x, ErrNotFinite)
	}
	return normalCDF(x), nil
}

// normalCDF is NormalCDF without the NaN check, for the inner loops of the
// quadrature routines where the argument is already known to be a real number.
func normalCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// Coefficients for NormalQuantile, taken verbatim from Algorithm AS 241.
//
// Go has no const arrays, so these are package-level vars. Nothing in this package
// writes to them; treat them as constants.
var (
	as241A = [8]float64{
		3.3871328727963666080, 1.3314166789178437745e+2,
		1.9715909503065514427e+3, 1.3731693765509461125e+4,
		4.5921953931549871457e+4, 6.7265770927008700853e+4,
		3.3430575583588128105e+4, 2.5090809287301226727e+3,
	}
	as241B = [8]float64{
		1.0, 4.2313330701600911252e+1,
		6.8718700749205790830e+2, 5.3941960214247511077e+3,
		2.1213794301586595867e+4, 3.9307895800092710610e+4,
		2.8729085735721942674e+4, 5.2264952788528545610e+3,
	}
	as241C = [8]float64{
		1.42343711074968357734, 4.63033784615654529590,
		5.76949722146069140550, 3.64784832476320460504,
		1.27045825245236838258, 2.41780725177450611770e-1,
		2.27238449892691845833e-2, 7.74545014278341407640e-4,
	}
	as241D = [8]float64{
		1.0, 2.05319162663775882187,
		1.67638483018380384940, 6.89767334985100004550e-1,
		1.48103976427480074590e-1, 1.51986665636164571966e-2,
		5.47593808499534494600e-4, 1.05075007164441684324e-9,
	}
	as241E = [8]float64{
		6.65790464350110377720, 5.46378491116411436990,
		1.78482653991729133580, 2.96560571828504891230e-1,
		2.65321895265761230930e-2, 1.24266094738807843860e-3,
		2.71155556874348757815e-5, 2.01033439929228813265e-7,
	}
	as241F = [8]float64{
		1.0, 5.99832206555887937690e-1,
		1.36929880922735805310e-1, 1.48753612908506148525e-2,
		7.86869131145613259100e-4, 1.84631831751005468180e-5,
		1.42151175831644588870e-7, 2.04426310338993978564e-15,
	}
)

const (
	// as241Split1 and as241Split2 are the two branch points of AS 241: the central
	// region is |p - ½| ≤ 0.425, and the tail region is split again at
	// √(−log r) = 5, i.e. at r ≈ 1.4e-11.
	as241Split1 = 0.425
	as241Split2 = 5.0

	// as241Const1 and as241Const2 are the argument shifts the rational
	// approximations are centred on.
	as241Const1 = 0.180625
	as241Const2 = 1.6
)

// NormalQuantile returns Φ⁻¹(p), the standard normal deviate whose lower-tail area
// is p.
//
// # Algorithm and accuracy
//
// This is Algorithm AS 241's double-precision routine PPND16: Michael J. Wichura,
// "Algorithm AS 241: The Percentage Points of the Normal Distribution", Journal of
// the Royal Statistical Society, Series C (Applied Statistics), Vol. 37, No. 3
// (1988), pp. 477-484. It is three rational approximations of degree 7 over 7 —
// one for the central region |p - ½| ≤ 0.425, one for the moderate tails, one for
// the extreme tails — and is accurate to about 1e-16 relative over the whole
// range, which is at or below one ULP of the result. Coefficients are transcribed
// from the published algorithm; the tests check them against widely published
// critical values.
//
// A rational approximation rather than an iterative inversion of Φ is deliberate:
// it is a fixed, bounded number of arithmetic operations with no convergence
// criterion to state and no way to return an unconverged answer.
//
// # Range
//
// p must lie in the open interval (0, 1). Exactly 0 and exactly 1 are valid
// probabilities but their quantiles are −∞ and +∞, so they return ErrNotFinite
// rather than an infinity; a caller that wants the limiting behaviour should
// special-case them, which is what GaussianCopulaJoint does. Anything outside
// [0, 1], or NaN, fails Probability.Validate first.
func NormalQuantile(p Probability) (float64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	v := float64(p)
	if v <= 0 || v >= 1 {
		return 0, fmt.Errorf("odds: normal quantile of probability %g is infinite: %w", v, ErrNotFinite)
	}

	return normalQuantile(v), nil
}

// normalQuantile is the AS 241 rational approximation itself. The argument must
// already be known to lie strictly inside (0, 1); NormalQuantile establishes that,
// and the quadrature inner loops establish it by clamping.
func normalQuantile(v float64) float64 {
	q := v - 0.5
	if math.Abs(q) <= as241Split1 {
		r := as241Const1 - q*q
		return q * as241Poly(as241A, r) / as241Poly(as241B, r)
	}

	r := v
	if q > 0 {
		r = 1 - v
	}
	// r is now the smaller tail area and lies in (0, 0.075), so the logarithm is
	// of a strictly positive number bounded away from 1 and the square root is of
	// a strictly positive number.
	r = math.Sqrt(-math.Log(r))

	var value float64
	if r <= as241Split2 {
		r -= as241Const2
		value = as241Poly(as241C, r) / as241Poly(as241D, r)
	} else {
		r -= as241Split2
		value = as241Poly(as241E, r) / as241Poly(as241F, r)
	}
	if q < 0 {
		return -value
	}
	return value
}

// as241Poly evaluates c[7]·x⁷ + … + c[1]·x + c[0] by Horner's rule.
func as241Poly(c [8]float64, x float64) float64 {
	v := c[7]
	for i := 6; i >= 0; i-- {
		v = v*x + c[i]
	}
	return v
}

// Gauss-Legendre nodes and weights for the bivariate normal quadrature, from
// Genz's BVND. Column 0 is the positive half of the 6-point rule, column 1 the
// 12-point rule, column 2 the 20-point rule; the negative half is generated by
// symmetry at evaluation time. Unused trailing slots are never read, because
// bvnRuleLength bounds the loop.
//
// As with the AS 241 coefficients, these are constants that Go's type system
// cannot express as such.
var (
	bvnNodes = [3][10]float64{
		{-0.9324695142031522, -0.6612093864662647, -0.2386191860831970},
		{
			-0.9815606342467191, -0.9041172563704750, -0.7699026741943050,
			-0.5873179542866171, -0.3678314989981802, -0.1252334085114692,
		},
		{
			-0.9931285991850949, -0.9639719272779138, -0.9122344282513259,
			-0.8391169718222188, -0.7463319064601508, -0.6360536807265150,
			-0.5108670019508271, -0.3737060887154196, -0.2277858511416451,
			-0.07652652113349733,
		},
	}
	bvnWeights = [3][10]float64{
		{0.1713244923791705, 0.3607615730481384, 0.4679139345726904},
		{
			0.04717533638651177, 0.1069393259953183, 0.1600783285433464,
			0.2031674267230659, 0.2334925365383547, 0.2491470458134029,
		},
		{
			0.01761400713915212, 0.04060142980038694, 0.06267204833410906,
			0.08327674157670475, 0.1019301198172404, 0.1181945319615184,
			0.1316886384491766, 0.1420961093183821, 0.1491729864726037,
			0.1527533871307259,
		},
	}
	bvnRuleLength = [3]int{3, 6, 10}
)

// BivariateNormalCDF returns Φ₂(h, k; r) = P(X ≤ h, Y ≤ k) for a standard
// bivariate normal pair with correlation r.
//
// # Algorithm and accuracy
//
// The implementation is a transcription of Alan Genz's BVND, which computes the
// upper orthant P(X > h, Y > k); the lower orthant this function returns is
// BVND(−h, −k, r), since (−X, −Y) is standard bivariate normal with the same
// correlation. BVND is the method of Z. Drezner and G. O. Wesolowsky, "On the
// computation of the bivariate normal integral", Journal of Statistical
// Computation and Simulation, Vol. 35 (1990), pp. 101-107, with Genz's
// modifications for double precision and for |r| near 1 (A. Genz, "Numerical
// computation of rectangular bivariate and trivariate normal and t
// probabilities", Statistics and Computing, Vol. 14 (2004), pp. 251-260).
//
// Two formulations are used. For |r| < 0.925 the integral is taken over the
// correlation via the tetrachoric-style substitution, integrand exp((sin θ·hk −
// (h²+k²)/2)/(1 − sin²θ)), by Gauss-Legendre quadrature; the order steps up with
// |r| (6, 12, or 20 points) because the integrand sharpens. For |r| ≥ 0.925 that
// integrand becomes singular, so a different substitution with an asymptotic
// correction term is used. Genz reports about 1e-15 accuracy across the whole
// parameter space; the tests here check it against exact closed forms and against
// an independently derived quadrature.
//
// # Range and edge cases
//
// r must be a finite number in [-1, 1]; anything else is ErrCorrelationOutOfRange
// or ErrNotFinite. Both r = 1 and r = -1 are exact special cases and return
// min(Φ(h), Φ(k)) and max(0, Φ(h) + Φ(k) − 1) respectively, the Fréchet-Hoeffding
// bounds. Infinite h or k are handled before the quadrature — Φ₂(−∞, k; r) = 0 and
// Φ₂(+∞, k; r) = Φ(k) — because feeding an infinity into the integrand produces
// NaN rather than the limit. NaN in either argument is rejected.
//
// # Symmetry in h and k
//
// Φ₂(h, k; r) = Φ₂(k, h; r) — the two variables are interchangeable in the
// definition — and this function honours that bit for bit, by ordering the pair
// before evaluating. BVND does not do it on its own: it is a quadrature in which h
// and k enter asymmetrically (dh drives the branch tests and the asymptotic
// correction), so evaluating it on a swapped pair can differ in the last place. That
// is the difference between two prices for one two-leg parlay depending on which leg
// the customer tapped first, so the ordering is imposed here rather than left to the
// caller.
func BivariateNormalCDF(h, k, r float64) (float64, error) {
	if math.IsNaN(h) || math.IsNaN(k) {
		return 0, fmt.Errorf("odds: bivariate normal cdf arguments (%v, %v): %w", h, k, ErrNotFinite)
	}
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0, fmt.Errorf("odds: bivariate normal correlation %v: %w", r, ErrNotFinite)
	}
	if r < -1 || r > 1 {
		return 0, fmt.Errorf("odds: bivariate normal correlation %g: %w", r, ErrCorrelationOutOfRange)
	}

	// Infinite limits, resolved as limits rather than fed to the quadrature.
	if math.IsInf(h, -1) || math.IsInf(k, -1) {
		return 0, nil
	}
	if math.IsInf(h, 1) && math.IsInf(k, 1) {
		return 1, nil
	}
	if math.IsInf(h, 1) {
		return normalCDF(k), nil
	}
	if math.IsInf(k, 1) {
		return normalCDF(h), nil
	}

	// Canonical argument order, so that Φ₂(h, k; r) and Φ₂(k, h; r) are the same
	// bits and not merely the same number to within the quadrature. Both are finite
	// here, so the comparison is a total order.
	if h > k {
		h, k = k, h
	}
	return bvnUpperOrthant(-h, -k, r), nil
}

// bvnUpperOrthant returns P(X > dh, Y > dk), the quantity Genz's BVND computes.
// Both limits are finite and r is in [-1, 1]; BivariateNormalCDF establishes that,
// so this is a total function of three real numbers and returns no error.
func bvnUpperOrthant(dh, dk, r float64) float64 {
	const twoPi = 2 * math.Pi

	var rule int
	switch {
	case math.Abs(r) < 0.3:
		rule = 0
	case math.Abs(r) < 0.75:
		rule = 1
	default:
		rule = 2
	}
	nodes, weights, lg := bvnNodes[rule], bvnWeights[rule], bvnRuleLength[rule]

	h, k := dh, dk
	hk := h * k
	bvn := 0.0

	if math.Abs(r) < 0.925 {
		// The sin substitution. asr·(±x + 1)/2 stays strictly inside (−π/2, π/2)
		// because |asin(r)| < π/2 here and |x| < 1, so 1 − sin² is bounded away
		// from zero and the division is safe.
		hs := (h*h + k*k) / 2
		asr := math.Asin(r)
		for i := range lg {
			for _, sign := range [2]float64{-1, 1} {
				sn := math.Sin(asr * (sign*nodes[i] + 1) / 2)
				bvn += weights[i] * math.Exp((sn*hk-hs)/(1-sn*sn))
			}
		}
		return bvn*asr/(2*twoPi) + normalCDF(-h)*normalCDF(-k)
	}

	// |r| ≥ 0.925. Reflect a negative correlation onto a positive one, then use
	// the asymptotic expansion plus a correction integral.
	if r < 0 {
		k = -k
		hk = -hk
	}

	if math.Abs(r) < 1 {
		as := (1 - r) * (1 + r)
		a := math.Sqrt(as)
		bs := (h - k) * (h - k)
		c := (4 - hk) / 8
		d := (12 - hk) / 16
		asr := -(bs/as + hk) / 2
		if asr > -100 {
			bvn = a * math.Exp(asr) * (1 - c*(bs-as)*(1-d*bs/5)/3 + c*d*as*as/5)
		}
		if -hk < 100 {
			b := math.Sqrt(bs)
			bvn -= math.Exp(-hk/2) * math.Sqrt(twoPi) * normalCDF(-b/a) * b * (1 - c*bs*(1-d*bs/5)/3)
		}
		a /= 2
		for i := range lg {
			for _, sign := range [2]float64{-1, 1} {
				t := a * (sign*nodes[i] + 1)
				xs := t * t
				rs := math.Sqrt(1 - xs)
				asr := -(bs/xs + hk) / 2
				if asr > -100 {
					bvn += a * weights[i] * math.Exp(asr) *
						(math.Exp(-hk*(1-rs)/(2*(1+rs)))/rs - (1 + c*xs*(1+d*xs)))
				}
			}
		}
		bvn = -bvn / twoPi
	}

	if r > 0 {
		return bvn + normalCDF(-math.Max(h, k))
	}
	return -bvn + math.Max(0, normalCDF(-h)-normalCDF(-k))
}

// -----------------------------------------------------------------------------
// The multivariate normal orthant probability
// -----------------------------------------------------------------------------

// ErrOrthantNotConverged reports that the lattice quadrature behind
// MultivariateNormalCDF did not reach its accuracy target within the iteration
// cap. It returns the error rather than the unconverged estimate, because a price
// computed from a quadrature that quietly stopped early is worse than no price.
var ErrOrthantNotConverged = errors.New("multivariate normal quadrature did not reach its accuracy target")

const (
	// orthantRelTol and orthantAbsTol together set the stopping rule. The
	// quadrature stops when three times the largest movement of the running
	// estimate across its last two doublings of work falls to
	// max(orthantAbsTol, orthantRelTol·estimate) — a refinement test, so a result
	// is called converged only when doubling the effort twice in a row would move
	// the answer by comfortably less than the target. latticeEstimate documents why
	// the criterion measures movement rather than a standard error across shifts.
	//
	// The target is RELATIVE because the quantity spans orders of magnitude: a
	// three-leg joint sits near 0.2 and a twenty-five-leg one near 1e-5, and an
	// absolute target tight enough for the second is unreachable for a whole class
	// of inputs while being pointlessly tight for the first.
	//
	// 1e-3 relative is the STOPPING RULE, not the delivered accuracy. Because the
	// joint probability is inverted to get the price, a relative error in one is a
	// relative error in the other, and one American odds unit at a parlay price of
	// 6.50 is about 1.5e-3 relative — so even a result that only just meets the
	// stopping rule is inside the smallest increment anything downstream displays.
	//
	// The delivered accuracy is far better than that almost everywhere, because
	// orthantMinBatches puts a floor of 65,536 points under every evaluation
	// regardless of how quickly the rule is met. Two measurements, both produced by
	// the tests rather than asserted here:
	//
	//   - Against the closed form that exists when the correlation matrix is the
	//     identity — which this routine integrates rather than special-cases — the
	//     relative error is around 1e-13 at three to six constraints.
	//   - Against a two-million-sample simulation of correlated draws at three to
	//     six legs and correlations from -0.25 to +0.3, the largest difference is
	//     4e-4 in absolute probability, itself only a couple of Monte Carlo standard
	//     errors, so most of that gap is the reference rather than the quadrature.
	//
	// The stopping rule binds only on long, heavily correlated tickets and on the
	// singular edge of the positive semi-definite region, where the integrand
	// collapses to a discontinuous indicator. Those cases now cost more batches
	// rather than being refused; see orthantMaxBatches.
	//
	// orthantAbsTol is a floor, not a target. Relative accuracy on a vanishingly
	// small orthant probability is both unattainable and pointless: a joint
	// probability of 1e-6 is a decimal price of a million, four orders past the
	// longest price this package can even express in American odds
	// (MaxAmericanMagnitude bounds the decimal price near 10,001, i.e. a probability
	// near 1e-4). The floor stops the loop chasing precision no price could carry.
	orthantRelTol = 1e-3
	orthantAbsTol = 1e-6

	// orthantLatticePoints is the number of points in one shifted lattice, and
	// orthantMinBatches the smallest number of lattices evaluated. Powers of two
	// are not required by the Richtmyer construction.
	//
	// The split between the two is a real trade rather than an arbitrary
	// factorisation. Accuracy for a fixed budget favours few large lattices, since a
	// single lattice's error falls as 1/N; the error ESTIMATE favours many small
	// ones, because it is measured across doublings of the batch count and a budget
	// of B batches offers only log₂B of them. Eight lattices of 8,192 is the
	// smallest budget that both leaves three checkpoints under the floor — which is
	// what the two-quiet-doublings rule needs before it may stop at all — and lands
	// inside the target across the whole accuracy profile.
	//
	// The floor is a real cost: 65,536 integrand evaluations, each a normal CDF and
	// a normal quantile per dimension, so a six-leg parlay is on the order of ten
	// milliseconds. That is fine for pricing a bet slip and is not fine in a
	// per-tick loop; a caller pricing hundreds of combinations — a large round
	// robin — should cache. Cutting the budget to make it fast would trade a
	// measurable accuracy claim for an unmeasurable one, which is the wrong trade
	// in the package CLAUDE.md §10 singles out as the one that must not be wrong.
	orthantLatticePoints = 8192
	orthantMinBatches    = 8

	// orthantMaxBatches caps the batch loop at 256, i.e. 2,097,152 points. Reaching
	// it returns ErrOrthantNotConverged.
	//
	// # History, because the first answer here was the wrong one
	//
	// It was 96, and was raised to 256 to make one input class stop being refused:
	// a correlation matrix sitting exactly on the boundary of the positive
	// semi-definite region. Equicorrelation at ρ = -1/(n-1) is singular, so Cholesky
	// produces a zero pivot, so the last variable is a deterministic combination of
	// the earlier ones and its factor in the integrand collapses from a normal CDF
	// to a {0,1} indicator. A lattice rule is fast on a smooth integrand and slow on
	// a discontinuous one.
	//
	// That raise treated a symptom. The property suite went on to find a second
	// input on the same edge — three legs at 0.25, 0.15929 and 0.96405 at ρ = -0.5 —
	// where the old rule's spread was still 1.395e-6 against a 1e-6 target after all
	// 256 batches, and the ticket was refused again. Raising the cap a second time
	// would have bought one more counterexample's silence at 4M evaluations, and a
	// third would have cost 8M.
	//
	// The reason it was a treadmill is recorded on latticeEstimate: the quantity
	// being driven below the target was not the error. On this input the true error
	// was 1.9e-7 at the eight-batch FLOOR — already an order inside the target —
	// while the reported statistic, which assumes independent batches this estimator
	// does not have, stood at 7.5e-6 and fell only as B^-0.5. The cap was never the
	// binding constraint; the error estimate was. Correcting it prices that input at
	// 64 batches, a quarter of the cap, with a measured error of 1.4e-7.
	//
	// So the cap stays at 256 as headroom rather than as the fix, and it still binds
	// on integrands that genuinely cannot be reached — a 24-dimensional product of
	// Gaussians is pinned as one in TestLatticeAccuracyProfile — where reaching it
	// still refuses rather than returning the estimate it happens to hold.
	//
	// The cost is bounded and paid only by the hard cases: a well-conditioned parlay
	// still stops at the orthantMinBatches floor of 8. The worst case is about 2.1M
	// integrand evaluations, a few hundred milliseconds; a caller pricing hundreds of
	// combinations should already be caching, as the note above says.
	orthantMaxBatches = 256

	// orthantMaxDimension is the largest integration dimension, one below the
	// largest correlation matrix. It bounds the slice of generating constants.
	orthantMaxDimension = MaxCorrelationDimension - 1
)

// firstPrimes seeds the two irrational generating vectors below. The Richtmyer
// construction needs one irrational per dimension whose square roots are linearly
// independent over the rationals, and the square roots of distinct primes are.
// Twice orthantMaxDimension of them are needed: the first block generates the
// lattice, the second block generates the shifts.
var firstPrimes = [2 * orthantMaxDimension]int{
	2, 3, 5, 7, 11, 13, 17, 19, 23, 29,
	31, 37, 41, 43, 47, 53, 59, 61, 67, 71,
	73, 79, 83, 89, 97, 101, 103, 107, 109, 113,
	127, 131, 137, 139, 149, 151, 157, 163, 167, 173,
	179, 181, 191, 193, 197, 199, 211, 223,
}

// latticeGenerator[i] is the fractional part of √p_i, the Richtmyer generating
// constant for dimension i; latticeShiftBase[i] is the same for a disjoint block
// of primes and drives the batch shifts. Both are computed once at package
// initialisation and never written again.
var latticeGenerator, latticeShiftBase = func() ([orthantMaxDimension]float64, [orthantMaxDimension]float64) {
	var generator, shift [orthantMaxDimension]float64
	for i := range orthantMaxDimension {
		generator[i] = fractionalPart(math.Sqrt(float64(firstPrimes[i])))
		shift[i] = fractionalPart(math.Sqrt(float64(firstPrimes[i+orthantMaxDimension])))
	}
	return generator, shift
}()

// fractionalPart returns x − ⌊x⌋ for x ≥ 0.
func fractionalPart(x float64) float64 { return x - math.Floor(x) }

// MultivariateNormalCDF returns P(Z₁ ≤ t₁, …, Z_n ≤ t_n) for a standard
// multivariate normal vector Z with the given correlation matrix. It is the
// orthant probability that a correlated parlay's joint win probability reduces to.
//
// # Algorithm
//
// One and two dimensions have closed forms and are dispatched to NormalCDF and
// BivariateNormalCDF. Three or more have none, so the probability is computed by
// numerical integration using Alan Genz's separation-of-variables transformation
// (A. Genz, "Numerical Computation of Multivariate Normal Probabilities", Journal
// of Computational and Graphical Statistics, Vol. 1 (1992), pp. 141-149), which is
// the method behind every serious implementation of this function.
//
// The transformation: factor R = L·Lᵀ and substitute Z = L·w. The constraint on
// w_i then depends only on w₁ … w_{i−1}, so the region becomes a product of
// intervals and the n-dimensional integral over an unbounded region collapses to
// an (n−1)-dimensional integral over the unit cube with integrand
//
//	∏ᵢ Φ((tᵢ − Σ_{j<i} L_ij·y_j) / L_ii),   y_j = Φ⁻¹(u_j·e_j)
//
// which is smooth and bounded — exactly the shape a lattice rule handles well.
//
// The cube integral is evaluated by a randomised lattice rule: a rank-1 Richtmyer
// lattice with generating vector (√2, √3, √5, …) taken fractionally, wrapped in the
// baker (tent) transform x ↦ |2x − 1| that periodises the integrand and lifts the
// convergence rate, then shifted (Cranley & Patterson, "Randomization of Number
// Theoretic Methods for Multiple Integration", SIAM Journal on Numerical Analysis,
// Vol. 13 (1976), pp. 904-914) and averaged over several shifts. The shifts run
// along their own Kronecker sequence rather than a random source, so the average is
// over a stratified set of displacements and not over independent draws — which is
// what makes the result reproducible, and what dictates how its error is estimated
// (see latticeEstimate).
//
// Variables are reordered most-constrained-first — ascending threshold — before
// factoring, which is Genz's recommended ordering heuristic and sharply reduces the
// variance. It has a second benefit here: the ordering is derived from the
// thresholds and the correlation structure alone, never from the caller's argument
// order, so permuting the legs of a parlay returns a bit-identical probability. Ties
// between equal thresholds are broken by colour refinement over the correlation
// matrix rather than by argument position; canonicalOrder states precisely what that
// guarantees and the one class of matrix on which it falls short.
//
// # Convergence criterion, iteration cap, and determinism
//
// Batches of orthantLatticePoints are accumulated until three times the largest
// movement of the running estimate across its last two doublings of work falls to
// max(orthantAbsTol, orthantRelTol·estimate), with at least orthantMinBatches
// evaluated and at most orthantMaxBatches. Exhausting the cap returns
// ErrOrthantNotConverged; the unconverged estimate is never returned to a caller.
//
// The shifts are generated from a fixed irrational sequence rather than from a
// random source, so the function is deterministic: the same inputs give bit-identical
// output on every run and every platform, which is what lets it be tested at all. It
// also means the batch means are not independent samples, so a standard error across
// them says nothing about this estimator — latticeEstimate records the measurement
// that established that and the refinement test used in its place. What remains true
// either way is that the stopping quantity is an error ESTIMATE and not a bound, so
// the tests measure achieved accuracy against independent references — the
// identity-matrix closed form, a polar-coordinate integral for the singular
// trivariate orthant, and a seeded simulation — instead of trusting it.
//
// # Range and edge cases
//
// len(thresholds) must equal the matrix dimension. A threshold of −∞ makes the
// probability exactly 0; a threshold of +∞ removes that constraint, and the
// remaining variables are integrated over the corresponding principal submatrix.
// NaN is rejected with ErrNotFinite.
func MultivariateNormalCDF(thresholds []float64, c CorrelationMatrix) (float64, error) {
	if c.IsZero() {
		return 0, fmt.Errorf("odds: multivariate normal cdf against an unconstructed correlation matrix: %w",
			ErrCorrelationShape)
	}
	if len(thresholds) != c.N() {
		return 0, fmt.Errorf("odds: %d thresholds against a %d×%d correlation matrix: %w",
			len(thresholds), c.N(), c.N(), ErrCorrelationShape)
	}
	for i, t := range thresholds {
		if math.IsNaN(t) {
			return 0, fmt.Errorf("odds: multivariate normal threshold %d is %v: %w", i, t, ErrNotFinite)
		}
		if math.IsInf(t, -1) {
			return 0, nil // an impossible constraint makes the whole orthant empty
		}
	}

	// Drop the inactive constraints. Everything after this point works on the
	// principal submatrix of the constraints that actually bind.
	active := make([]int, 0, len(thresholds))
	for i, t := range thresholds {
		if !math.IsInf(t, 1) {
			active = append(active, i)
		}
	}
	if len(active) == 0 {
		return 1, nil
	}
	if len(active) < len(thresholds) {
		bound := make([]float64, len(active))
		for i, idx := range active {
			bound[i] = thresholds[idx]
		}
		// permute, not Submatrix: active was just built by filtering the caller's
		// own index range, so it is in bounds and strictly increasing by
		// construction and Submatrix's validation could not fail.
		return MultivariateNormalCDF(bound, c.permute(active))
	}

	switch c.N() {
	case 1:
		return normalCDF(thresholds[0]), nil
	case 2:
		return BivariateNormalCDF(thresholds[0], thresholds[1], c.at(0, 1))
	default:
		return orthantByLattice(thresholds, c)
	}
}

// orthantByLattice is the Genz separation-of-variables integration described on
// MultivariateNormalCDF. It is only called with three or more binding constraints.
func orthantByLattice(thresholds []float64, c CorrelationMatrix) (float64, error) {
	n := len(thresholds)

	// Shape first, because everything below indexes into c. MultivariateNormalCDF
	// has already established this for its own call, but this function is reachable
	// from anywhere in the package and reading past the end of an unconstructed
	// matrix is not a failure mode worth leaving open.
	if c.IsZero() || c.N() != n {
		return 0, fmt.Errorf("odds: lattice orthant over %d thresholds against a %d×%d correlation matrix: %w",
			n, c.N(), c.N(), ErrCorrelationShape)
	}

	// Most-constrained-first ordering: ascending threshold, ties broken by the
	// correlation structure so that the ordering depends on nothing but the
	// mathematical content of (thresholds, R). See canonicalOrder.
	order := canonicalOrder(thresholds, c)

	bound := make([]float64, n)
	for i, idx := range order {
		bound[i] = thresholds[idx]
	}
	// permute rather than Submatrix: order is a permutation this function just
	// generated, so the public constructor's index validation has nothing to check
	// and its error return would be unreachable — an unreachable error return being
	// exactly what keeps this package off the CLAUDE.md §10 coverage floor.
	factor, err := c.permute(order).Cholesky()
	if err != nil {
		return 0, err
	}

	dimension := n - 1
	y := make([]float64, dimension)
	integrand := func(u []float64) float64 {
		weight := 1.0
		for i := range n {
			offset := 0.0
			for j := range i {
				offset += factor[i][j] * y[j]
			}

			var interval float64
			switch {
			case factor[i][i] > 0:
				interval = normalCDF((bound[i] - offset) / factor[i][i])
			case offset <= bound[i]:
				// A zero pivot means this variable is a deterministic combination
				// of the earlier ones, so the constraint is either satisfied or not.
				interval = 1
			default:
				interval = 0
			}

			weight *= interval
			if weight <= 0 {
				return 0
			}
			if i < dimension {
				y[i] = normalQuantile(clampToOpenUnit(u[i] * interval))
			}
		}
		return weight
	}

	value, _, err := latticeEstimate(dimension, integrand, orthantMinBatches, orthantMaxBatches, orthantRelTol, orthantAbsTol)
	if err != nil {
		return 0, err
	}
	return value, nil
}

// canonicalOrder returns the variable ordering orthantByLattice integrates in.
//
// Genz's heuristic is most-constrained-first — ascending threshold — and that is the
// primary key. The tie-break needs care because of a promise MultivariateNormalCDF
// makes in its own documentation: the answer must not depend on the order the caller
// happened to list the legs in. Sorting on the threshold alone does not deliver
// that. A stable sort falls back to the caller's order whenever two thresholds are
// bit-equal, so permuting the arguments yields a different Cholesky factor, a
// different integrand, and — the lattice rule being an approximation rather than an
// exact evaluation — a different answer. The size of the discrepancy is small (about
// 3e-8 on a three-leg parlay with equal marginals, well inside the 1e-3 stopping
// rule) but a joint probability that moves when the bet slip is reordered is
// indefensible at any magnitude, so it is fixed here rather than tolerated.
//
// The tie-break is colour refinement — the one-dimensional Weisfeiler-Leman
// algorithm — over the correlation matrix read as a weighted complete graph:
//
//	colour⁰(i)   = rank of tᵢ among the thresholds
//	colourᵏ⁺¹(i) = rank of ( colourᵏ(i), sorted{ (colourᵏ(j), ρᵢⱼ) : j ≠ i } )
//
// iterated until the partition stops splitting, which takes at most n rounds. Every
// key is built from thresholds and correlations alone — never from an index — so the
// partition is identical under any relabelling of the variables. Each key carries the
// previous colour as its leading component, so refinement can only split cells and
// never disturbs the coarser ascending-threshold arrangement.
//
// # What this guarantees, and what it does not
//
// Variables still sharing a colour at the fixed point are WL-indistinguishable, and
// the final sort leaves them in the caller's relative order. That residual freedom is
// harmless whenever the cell is genuinely exchangeable — whenever the transposition
// is an automorphism of (thresholds, R), which covers every tie reachable from equal
// marginals over a symmetric correlation structure. Writing σ for that automorphism,
// the two candidate orders satisfy order′[k] = σ(order[k]), so the reordered matrix
// R[order′[p]][order′[q]] = R[order[p]][order[q]] and the reordered thresholds are
// equal term by term: the integrand is bit-identical, not merely close.
//
// The gap is the classical limit of 1-WL. Non-isomorphic weighted graphs exist that
// it cannot separate — the strongly-regular pairs, smallest on 16 vertices, and they
// need a large set of exactly-equal correlations as well as exactly-equal thresholds.
// On such a matrix the caller's order could still leak into the result, bounded as
// before by the quadrature tolerance. Closing that gap means full canonicalisation,
// which is graph isomorphism; it is not worth it here and the limitation is recorded
// rather than papered over.
func canonicalOrder(thresholds []float64, c CorrelationMatrix) []int {
	n := len(thresholds)

	colour := rankByKey(n, func(i int) []float64 { return []float64{thresholds[i]} })
	distinct := countDistinct(colour)
	for range n {
		next := rankByKey(n, func(i int) []float64 {
			neighbours := make([][2]float64, 0, n-1)
			for j := range n {
				if j != i {
					neighbours = append(neighbours, [2]float64{float64(colour[j]), c.at(i, j)})
				}
			}
			sort.Slice(neighbours, func(a, b int) bool {
				if neighbours[a][0] != neighbours[b][0] {
					return neighbours[a][0] < neighbours[b][0]
				}
				return neighbours[a][1] < neighbours[b][1]
			})
			key := make([]float64, 0, 1+2*(n-1))
			key = append(key, float64(colour[i]))
			for _, pair := range neighbours {
				key = append(key, pair[0], pair[1])
			}
			return key
		})
		grown := countDistinct(next)
		colour = next
		if grown == distinct {
			break // the partition is stable; further rounds cannot split it
		}
		distinct = grown
	}

	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return colour[order[a]] < colour[order[b]] })
	return order
}

// rankByKey assigns each of n items the rank of its key in the sorted order of all
// keys, with equal keys sharing a rank. Keys are compared lexicographically and must
// all be the same length. Ranks rather than the keys themselves are carried forward
// so that a refinement round's key stays a fixed-width vector of small numbers
// instead of growing by a factor of n each round.
func rankByKey(n int, key func(int) []float64) []int {
	keys := make([][]float64, n)
	for i := range keys {
		keys[i] = key(i)
	}
	byKey := make([]int, n)
	for i := range byKey {
		byKey[i] = i
	}
	sort.SliceStable(byKey, func(a, b int) bool { return compareKeys(keys[byKey[a]], keys[byKey[b]]) < 0 })

	rank := make([]int, n)
	next := 0
	for position, idx := range byKey {
		if position > 0 && compareKeys(keys[byKey[position-1]], keys[idx]) != 0 {
			next++
		}
		rank[idx] = next
	}
	return rank
}

// compareKeys orders two equal-length float vectors lexicographically. Every value
// reaching it is finite: thresholds are checked for NaN by MultivariateNormalCDF and
// ±∞ is stripped before orthantByLattice runs, correlations are range-checked by
// NewCorrelationMatrix, and colours are small non-negative integers.
func compareKeys(a, b []float64) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// countDistinct returns the number of distinct values in a rank vector, which is
// dense in [0, n) by construction, so the largest rank plus one is the count.
func countDistinct(rank []int) int {
	highest := -1
	for _, r := range rank {
		if r > highest {
			highest = r
		}
	}
	return highest + 1
}

// clampToOpenUnit pulls a probability strictly inside (0, 1) so that
// normalQuantile cannot be handed an endpoint. The bounds are the smallest normal
// double and the largest double below 1, so the clamp only ever fires on a value
// that has already underflowed or rounded to an endpoint, and the quantile it
// produces (about ∓38) is far outside any region carrying probability mass.
func clampToOpenUnit(p float64) float64 {
	const (
		lowest  = 1e-300
		highest = 1 - 1e-16
	)
	switch {
	case !(p > lowest): // also catches NaN
		return lowest
	case p > highest:
		return highest
	default:
		return p
	}
}

// latticeEstimate integrates f over the unit cube [0,1]^dimension by averaging
// shifted Richtmyer lattice rules, stopping when the running estimate stops moving:
// three times the largest movement across the last two doublings of the batch count
// must fall to max(absTol, relTol·|estimate|).
//
// # Why the stopping rule is a refinement test and not a standard error
//
// This routine used to stop on 3·√(V̂/B), where V̂ is the sample variance of the B
// batch means. That statistic is the standard error of a mean of B INDEPENDENT
// samples, and the batches here are not independent samples of anything. Every
// batch is the same Richtmyer lattice displaced by shift_b = frac(b·√q), a Kronecker
// sequence, so the shifts are a deterministic low-discrepancy set rather than random
// draws (Cranley & Patterson randomisation with the randomness removed, which is
// what makes this function reproducible bit for bit). Averaging over stratified
// shifts converges far faster than averaging over independent ones, so 3·√(V̂/B) is
// not an estimate of this estimator's error — it is an estimate of a different,
// worse estimator's error, and it exceeds the truth by one to three orders of
// magnitude.
//
// That is measurable rather than arguable, and TestLatticeErrorEstimateTracksTruth
// measures it. On the singular three-leg orthant that motivated this change the old
// statistic fell as B^-0.5 (7.53e-6 at 8 batches to 3.46e-7 at 4096, a factor of 21.8
// over 512× the work, against √512 = 22.6) while the true error fell roughly as B^-1
// and was already 1.9e-7 at the eight-batch floor. The rule therefore demanded two
// million points to certify an answer that sixty-five thousand had already delivered,
// exhausted the cap, and refused to price a perfectly ordinary ticket.
//
// The replacement measures the thing the caller actually cares about: how much the
// answer would still move if the work kept doubling. Because the running mean over 2B
// batches is (m_B + m'_B)/2, the movement |m_2B − m_B| is half the disagreement
// between the two halves and so is naturally on the scale of the error of m_2B — a
// self-calibrating quantity, no distributional assumption attached. Two guards keep
// it conservative: the movement is multiplied by three, preserving the old rule's
// safety factor, and the largest of the LAST TWO movements is used, so a single
// checkpoint that happens to land on a stationary point cannot trigger a stop.
//
// It remains an error ESTIMATE and not a proven bound — a deterministic sequence can
// plateau — which is why the tests measure achieved accuracy against independent
// references (the identity-matrix closed form, and a polar-coordinate reference for
// the singular trivariate orthant) rather than against this number.
//
// Checkpoints are at powers of two and at maxBatches, so the work is at most twice
// what an every-batch test would use, and a doubling is what the movement is
// measured across.
//
// On non-convergence it returns the last estimate together with its spread AND a
// non-nil error. The estimate is returned only so that a diagnostic can report how
// far off it was; it must not be used, and the single production caller discards
// it. Every other function in this package treats a non-nil error as fatal. The
// spread is +Inf when the cap was too small to reach two checkpoint movements at all,
// which is the honest report: nothing was measured.
//
// It is factored out of orthantByLattice so that the stopping rule can be tested
// directly, including the non-convergence path that a converged integrand never
// reaches.
func latticeEstimate(dimension int, f func([]float64) float64, minBatches, maxBatches int, relTol, absTol float64) (estimate, spread float64, err error) {
	if dimension < 1 || dimension > orthantMaxDimension {
		return 0, 0, fmt.Errorf("odds: lattice integration in %d dimensions is outside [1, %d]: %w",
			dimension, orthantMaxDimension, ErrCorrelationShape)
	}

	point := make([]float64, dimension)
	shift := make([]float64, dimension)

	var (
		total       float64
		checkpoint  float64 // running estimate at the previous checkpoint
		movement    float64 // |estimate − checkpoint| at the previous checkpoint
		checkpoints int
	)
	spread = math.Inf(1)

	for batch := 1; batch <= maxBatches; batch++ {
		for i := range dimension {
			shift[i] = fractionalPart(float64(batch) * latticeShiftBase[i])
		}

		batchTotal := 0.0
		for k := 1; k <= orthantLatticePoints; k++ {
			for i := range dimension {
				x := fractionalPart(float64(k)*latticeGenerator[i] + shift[i])
				// The baker transform: periodising the integrand is what lifts a
				// lattice rule above plain Monte Carlo on a smooth function.
				point[i] = math.Abs(2*x - 1)
			}
			batchTotal += f(point)
		}

		total += batchTotal / orthantLatticePoints
		estimate = total / float64(batch)

		if batch&(batch-1) != 0 && batch != maxBatches {
			continue // not a doubling and not the cap
		}

		previous := movement
		movement = math.Abs(estimate - checkpoint)
		checkpoint = estimate
		checkpoints++

		// The first checkpoint has nothing to move away from, and the second has
		// measured only one movement where the rule wants two.
		if checkpoints < 3 {
			continue
		}

		// The larger of the last two movements, so a single checkpoint that lands
		// on a stationary point cannot certify a result on its own.
		spread = 3 * math.Max(movement, previous)
		if batch >= minBatches && spread <= math.Max(absTol, relTol*math.Abs(estimate)) {
			return estimate, spread, nil
		}
	}

	return estimate, spread, fmt.Errorf(
		"odds: lattice integration in %d dimensions moved by %g across its last doublings on an estimate of %g, "+
			"against a target of max(%g, %g·estimate), in %d batches of %d points: %w",
		dimension, spread, estimate, absTol, relTol, maxBatches, orthantLatticePoints, ErrOrthantNotConverged)
}

// snapToUnitRange pulls a value that is within correlationRangeTolerance of ±1
// exactly onto it. See correlationRangeTolerance.
func snapToUnitRange(v float64) float64 {
	switch {
	case v > 1:
		return 1
	case v < -1:
		return -1
	default:
		return v
	}
}
