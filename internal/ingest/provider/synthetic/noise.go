package synthetic

import (
	"math"

	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// The latent process: how a market's true probability moves.
//
// rand.go supplies counter-based draws — a pure function of (seed, key, n). This
// file turns them into a stationary, mean-reverting, O(1)-to-evaluate path, plus
// the rare correlated jumps that are steam moves.
//
// # Why not iterate an OU recursion
//
// The obvious model is an Ornstein–Uhlenbeck recursion, x(n+1) = φ·x(n) + σ·z(n).
// It is the right process and it is unusable here, because evaluating it at step
// n means iterating from an origin. Either the adapter retains mutable state
// between polls — which reintroduces exactly the "restart changes the answer"
// property the seed exists to remove, and makes the model advance with POLLS
// instead of with time — or every fetch replays millions of steps.
//
// So the same process is written in its moving-average form instead. An AR(1) is
// identically the convolution of white noise with an exponential kernel:
//
//	x(n) = Σ_{k≥0} φ^k · z(n−k)     (normalised to unit variance)
//
// Truncating that sum at K terms is not an approximation of a different process;
// it is an exactly stationary Gaussian process whose autocorrelation is φ^lag for
// every lag well inside K. The kernels below are truncated where φ^K ≈ 1/16, so
// the retained kernel carries over 99.5% of the variance and the normalisation is
// computed from the finite sum, making the stationary variance exact rather than
// nearly right.
//
// What the form buys is the entire determinism contract: x(n) is a pure function
// of (seed, key, n), so it is identical across restarts, across processes, and
// across any interleaving of concurrent callers, and advancing ten steps one at a
// time is by construction the same as advancing ten at once.
//
// # Two timescales, both on their own grid, both interpolated
//
// One exponential kernel cannot be both a line that drifts over an afternoon and
// a price that reacts within a minute: a half-life long enough for the first
// makes K enormous, and a half-life short enough for the second makes the line
// jitter instead of trend. So the path is a variance-preserving mix of two — a
// slow component on a five-minute grid with an hour's half-life, and a fast one
// on a one-minute grid with a twelve-minute half-life. The weights are 0.88 and
// √(1−0.88²), so the mixture has unit variance exactly and most of the movement
// a human sees is trend rather than noise.
//
// Neither component is evaluated on the base step. That is the single most
// important choice in this file, and it is what makes change detection real.
//
// A process evaluated at every 10-second step moves by roughly σ·√(2/half-life)
// per step whatever its half-life is; make the half-life long enough for the
// per-step move to be invisible and the kernel needs thousands of terms. Putting
// the process on a coarser grid and interpolating LINEARLY between grid points
// divides the per-step move by the grid factor at no cost in kernel length, and
// it says something true about markets: a line is repriced on the order of
// minutes, not on the order of the polling interval. The measured effect is that
// four fifths of prices are byte-identical between two consecutive model steps,
// which is what CLAUDE.md §5's "most polls return identical data" is supposed to
// mean.
//
// The interpolation reads one grid point AHEAD of the current one. That is not a
// prediction: the whole path is a deterministic function of the seed, so "the
// next grid point" is as well-defined as the last one. It is the standard
// construction for smooth value noise.

// Kernel and steam parameters.
//
// The comments give each number's meaning in wall-clock terms AT THE DEFAULT
// STEP. Options.Step scales all of them, which is intended: a smaller step is a
// finer-grained simulation of the same market, not a different one.
const (
	// fastGrid is how many base steps make one fast-component grid step: one
	// minute at the default step.
	fastGrid = 6

	// fastKernelLen and fastHalfLifeGrid describe the fast component, in fast
	// grid steps. A 12-step half-life is twelve minutes and φ^48 = 1/16.
	fastKernelLen    = 48
	fastHalfLifeGrid = 12.0

	// slowGrid is how many base steps make one slow-component grid step: five
	// minutes at the default step.
	slowGrid = 30

	// slowKernelLen and slowHalfLifeGrid describe the slow component, in slow
	// grid steps. A 12-step half-life is one hour; the 40-step kernel remembers
	// 3h20m.
	slowKernelLen    = 40
	slowHalfLifeGrid = 12.0

	// weightSlow is the slow component's share of the mixture's standard
	// deviation. weightFast is √(1−weightSlow²), computed in New.
	weightSlow = 0.88

	// steamBlockSteps is the granularity at which a steam move can start: 10
	// minutes at the default step. Within a block the jump lands on a
	// hash-chosen step, so steam is not aligned to a visible grid.
	steamBlockSteps = 60

	// steamBlocks is how many blocks back the scan reaches — four hours, which
	// is longer than any contest in this universe trades for.
	steamBlocks = 24

	// steamFullBlocks is how long a steam move holds its full amplitude before
	// it starts to fade. Three hours: within one event's trading life a steam
	// move is PERMANENT, which is what distinguishes it from the mean-reverting
	// component. The fade over the remaining blocks exists only so the scan can
	// be truncated without a discontinuity at the window's edge.
	steamFullBlocks = 18

	// steamProbability is the chance any given block contains a steam move.
	// 2% of a 10-minute block is one steam move per eight hours per process, so
	// a contest's margin sees roughly one over its trading life while the slate
	// as a whole always has several in flight.
	//
	// It and steamAmplitude are the two knobs that decide how much of the
	// model's variance is steam rather than drift. Together they put steam at
	// about a quarter of the mean-reverting mixture's variance: enough that a
	// steam move is the largest single thing that happens to a line, small
	// enough that the line is not simply a sequence of jumps.
	steamProbability = 0.02

	// steamMinAbsZ and steamAmplitude size the jump, in units of the process's
	// own stationary standard deviation. |z| is floored so that a "steam move"
	// is never a move too small to see; the product runs from about 0.4σ to
	// 1.5σ, which on a basketball spread is roughly half a point to two and a
	// half points.
	steamMinAbsZ   = 0.8
	steamAmplitude = 0.5

	// suspendSteps is how long a market stays suspended after a steam move: 30
	// seconds at the default step.
	//
	// It is deliberately SHORTER than the shallowest book lag (2 steps) plus the
	// deepest (9 steps), so the staggered reconvergence that phase 9's steam
	// detector keys on happens while prices are flowing. A suspension long
	// enough to hide the whole convergence would turn the model's steam
	// signature into an invisible one, which is the opposite of the point.
	suspendSteps = 3
)

// noSteam is the "no jump in the window" sentinel for a step index. It is far
// below any real index rather than zero, because step 0 is a legitimate index
// (the Unix epoch) and using it would make an epoch-adjacent test look suspended.
const noSteam = math.MinInt64

// -----------------------------------------------------------------------------
// Draw streams
// -----------------------------------------------------------------------------

// stream is a counter-indexed sequence of draws for one key.
//
// It is exactly rand.go's draw(seed, key, counter) with the (seed, key) half of
// the mix hoisted out. That matters at the volume this package works at: a fetch
// evaluates order 10^4 draws, and fnv1a over a forty-byte key on every one of
// them would dominate the cost of the entire model. TestStreamMatchesDraw
// asserts the two agree value for value, so the optimisation cannot drift into a
// second, differently-seeded generator.
type stream struct{ h uint64 }

// newStream binds a seed and a key.
func newStream(seed int64, key string) stream {
	h := splitmix64(uint64(seed) + 0x243F6A8885A308D3)
	return stream{h: splitmix64(h ^ fnv1a(key))}
}

// bits returns the raw 64-bit draw at counter.
func (s stream) bits(counter int64) uint64 { return splitmix64(s.h ^ uint64(counter)) }

// unit returns the [0, 1) draw at counter.
func (s stream) unit(counter int64) float64 { return uniform(s.bits(counter)) }

// normal returns the standard normal draw at counter.
//
// It repeats normalAt's clamp-then-invert rather than calling it, because
// normalAt re-derives the key hash on every call. The clamp bound and the
// fallback are the same, so the two functions agree by construction.
func (s stream) normal(counter int64) float64 {
	u := s.unit(counter)
	if u < normalClamp {
		u = normalClamp
	} else if u > 1-normalClamp {
		u = 1 - normalClamp
	}
	return quantile(u)
}

// quantile is Φ⁻¹ with the same never-NaN contract normalAt documents: the
// argument is already clamped strictly inside (0, 1) so the error cannot fire,
// and on the impossible path the answer is the distribution's mean, which is the
// least distorting value available. A NaN escaping here would become a NaN
// price, and NewPrice would reject the whole snapshot rather than the one
// market.
func quantile(u float64) float64 {
	z, err := odds.NormalQuantile(odds.Probability(u))
	if err != nil || math.IsNaN(z) || math.IsInf(z, 0) {
		return 0
	}
	return z
}

// index returns a deterministic index in [0, n) at counter.
func (s stream) index(counter int64, n int) int {
	if n <= 0 {
		return 0
	}
	return int(s.bits(counter) % uint64(n))
}

// -----------------------------------------------------------------------------
// Kernels
// -----------------------------------------------------------------------------

// expKernel returns a normalised exponential kernel of length n whose weights
// decay with the given half-life, measured in the kernel's own step unit.
//
// The weights are scaled so that Σ w² = 1, which is what makes the convolution
// of unit-variance noise have unit variance. Normalising against the FINITE sum
// rather than against the infinite series' 1/(1−φ²) is the difference between a
// process whose stationary standard deviation is exactly the configured one and
// one that is a couple of percent short — and the configured value is what the
// league table calls "how far the line wanders over a day", so being a couple of
// percent short would be a quiet, permanent modelling error.
func expKernel(halfLife float64, n int) []float64 {
	phi := math.Exp(-math.Ln2 / halfLife)
	w := make([]float64, n)
	sumSq := 0.0
	v := 1.0
	for i := 0; i < n; i++ {
		w[i] = v
		sumSq += v * v
		v *= phi
	}
	scale := 1 / math.Sqrt(sumSq)
	for i := range w {
		w[i] *= scale
	}
	return w
}

// -----------------------------------------------------------------------------
// Paths
// -----------------------------------------------------------------------------

// path is one latent process evaluated at every book's lagged view.
//
// views is indexed by BOOK index, not by lag: books() fixes the order and the
// adapter's lags slice is built from it, so views[i] is the value book i is
// looking at. Keeping the two in the same order is what stops a price being
// quoted against the wrong book's view, which would be invisible — every value
// in the slice is a plausible number.
type path struct {
	// views[i] is the unit-variance latent value as book i sees it.
	views []float64

	// lastSteam is the step index of the most recent steam jump in the TRUE
	// (unlagged) process, or noSteam. It drives market suspension.
	lastSteam int64
}

// steamed reports whether the true process jumped within window steps of n.
func (p path) steamed(n int64, window int64) bool {
	return p.lastSteam != noSteam && n-p.lastSteam >= 0 && n-p.lastSteam <= window
}

// jump is one steam move: when it landed and how large it was.
type jump struct {
	at  int64
	amp float64
}

// scratch holds the buffers one Fetch reuses across every process it evaluates.
//
// It is created per call and never shared, so Fetch stays safe for the
// concurrent use provider.Adapter requires while still not allocating a fresh
// noise window for each of the ~130 processes a league sweep touches.
type scratch struct {
	fast  []float64
	slow  []float64
	jumps []jump
	probs []float64
	quo   []float64
}

func (s *scratch) fastBuf(n int) []float64 {
	if cap(s.fast) < n {
		s.fast = make([]float64, n)
	}
	return s.fast[:n]
}

func (s *scratch) slowBuf(n int) []float64 {
	if cap(s.slow) < n {
		s.slow = make([]float64, n)
	}
	return s.slow[:n]
}

// evolve evaluates one latent process at step n, once per book view.
//
// The result is in units of the process's stationary standard deviation: the
// caller multiplies by the league's line dispersion to get points, goals or
// passing yards. Keeping the process dimensionless is what lets one steam
// amplitude and one book-bias scale apply to every quantity in the universe
// without a per-quantity fudge factor.
func (a *Adapter) evolve(sc *scratch, key string, n int64) path {
	// One window of draws per component covers every book's view, because book
	// i's view is the same convolution shifted by lags[i] base steps. The
	// buffers are sized from maxLag rather than from the observation that the
	// deepest lag happens to be under one grid step, so a larger configured lag
	// cannot silently read past the window.
	fastTop := floorDiv(n, fastGrid) + 1
	fbuf := fillNoise(sc.fastBuf(gridBufLen(fastKernelLen, a.maxLag, fastGrid)),
		newStream(a.opts.Seed, "fast:"+key), fastTop)

	slowTop := floorDiv(n, slowGrid) + 1
	sbuf := fillNoise(sc.slowBuf(gridBufLen(slowKernelLen, a.maxLag, slowGrid)),
		newStream(a.opts.Seed, "slow:"+key), slowTop)

	jumps := a.steamJumps(sc, key, n)

	p := path{views: make([]float64, len(a.lags)), lastSteam: noSteam}
	for i, lag := range a.lags {
		at := n - int64(lag)

		fast := gridValue(fbuf, a.fastW, fastTop, at, fastGrid)
		slow := gridValue(sbuf, a.slowW, slowTop, at, slowGrid)

		steam := 0.0
		for _, j := range jumps {
			if j.at > at {
				continue
			}
			steam += j.amp * steamRamp(at-j.at)
		}

		p.views[i] = weightSlow*slow + a.weightFast*fast + steam
	}

	// Suspension is decided on the TRUE process, not on any book's view: a
	// market is pulled because the market moved, not because one book noticed.
	for _, j := range jumps {
		if j.at <= n && j.at > p.lastSteam {
			p.lastSteam = j.at
		}
	}
	return p
}

// gridBufLen sizes a component's noise window: the kernel, one point ahead for
// the interpolation, the grid points the deepest lag can reach back over, and
// one spare so an off-by-one is a wasted draw rather than an index panic.
func gridBufLen(kernel, maxLag int, grid int64) int {
	return kernel + 2 + maxLag/int(grid) + 1
}

// fillNoise writes buf[d] = z(top − d) and returns it.
func fillNoise(buf []float64, s stream, top int64) []float64 {
	for d := range buf {
		buf[d] = s.normal(top - int64(d))
	}
	return buf
}

// gridValue is one component's value at base step `at`: the truncated-kernel
// convolution evaluated at the two grid points bracketing `at`, interpolated
// linearly between them.
func gridValue(buf, w []float64, top, at, grid int64) float64 {
	m := floorDiv(at, grid)
	frac := float64(at-m*grid) / float64(grid)
	lo := convolve(buf, w, int(top-m))
	hi := convolve(buf, w, int(top-m-1))
	return (1-frac)*lo + frac*hi
}

// convolve sums w against the window starting at offset off.
func convolve(buf, w []float64, off int) float64 {
	if off < 0 {
		off = 0
	}
	s := 0.0
	for k := range w {
		s += w[k] * buf[off+k]
	}
	return s
}

// steamJumps returns every steam move inside the scan window ending at n.
//
// The occurrence test is drawn first and the offset and amplitude only when it
// fires, so the common case — no steam in this block — costs one draw. The scan
// reaches one block PAST the window so that a jump near the boundary is seen by
// the deepest-lagged book too; without it a book would miss a move that the
// unlagged process had already faded out of the window.
func (a *Adapter) steamJumps(sc *scratch, key string, n int64) []jump {
	occ := newStream(a.opts.Seed, "steam-occ:"+key)
	off := newStream(a.opts.Seed, "steam-off:"+key)
	amp := newStream(a.opts.Seed, "steam-amp:"+key)

	out := sc.jumps[:0]
	b0 := floorDiv(n, steamBlockSteps)
	for i := 0; i <= steamBlocks; i++ {
		b := b0 - int64(i)
		if occ.unit(b) >= steamProbability {
			continue
		}
		at := b*steamBlockSteps + int64(off.index(b, steamBlockSteps))
		if at > n {
			continue
		}
		z := amp.normal(b)
		mag := (steamMinAbsZ + math.Abs(z)) * steamAmplitude
		if z < 0 {
			mag = -mag
		}
		out = append(out, jump{at: at, amp: mag})
	}
	sc.jumps = out
	return out
}

// steamRamp is a steam move's weight at the given age in steps: full for
// steamFullBlocks blocks, then linear to zero at the window's edge.
func steamRamp(age int64) float64 {
	const (
		full   = int64(steamFullBlocks * steamBlockSteps)
		window = int64(steamBlocks * steamBlockSteps)
	)
	switch {
	case age < 0:
		return 0
	case age <= full:
		return 1
	case age >= window:
		return 0
	default:
		return 1 - float64(age-full)/float64(window-full)
	}
}

// -----------------------------------------------------------------------------
// Small numeric helpers
// -----------------------------------------------------------------------------

// floorDiv divides rounding toward negative infinity.
//
// Go's / truncates toward zero, which for a step index derived from a clock
// would put every instant in the second before an epoch boundary into the block
// AFTER it. The bug only shows up on the wrong side of the epoch, which is
// exactly the kind of thing that survives every test written against today's
// date.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// roundHalf rounds to the nearest half point, which is the granularity books
// quote handicaps and thresholds at.
func roundHalf(v float64) float64 { return math.Round(v*2) / 2 }

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
