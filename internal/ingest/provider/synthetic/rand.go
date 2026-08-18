package synthetic

import (
	"math"

	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// Counter-based randomness.
//
// Every draw in this package is a pure function of (seed, a string key, a
// counter) rather than a value pulled from a sequential generator. That choice
// is what makes the model path-independent: advancing a process ten steps one at
// a time and advancing it ten steps in a loop consume the same draws in the same
// order, so the state depends on how many steps have elapsed and never on how
// the caller chunked them.
//
// A math/rand/v2 source seeded once could not offer that. Two goroutines pulling
// from one stream, or one goroutine advancing two events in a different order,
// would produce different numbers for the same instant — and the bug would look
// like nondeterministic pricing rather than like a misuse of a generator.
//
// SplitMix64 is the mixing function. It is the finaliser Go's own
// math/rand/v2.PCG and Java's SplittableRandom use, it passes BigCrush, it is
// four multiplies and four shifts, and it needs no state beyond its input. Good
// statistical quality is genuinely required here and not just tidy: the noise
// drives a mean-reverting process whose stationary variance is the width of the
// simulated market, so a generator with structure in its low bits would show up
// as a market that does not disperse.

// splitmix64 mixes a 64-bit value. Constants are the published SplitMix64
// finaliser.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// fnv1a hashes a string into 64 bits. It exists only to fold a variable-length
// key into splitmix64's fixed-width input; splitmix64 supplies the statistical
// quality, so FNV's weaknesses as a general hash do not reach the output.
func fnv1a(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// draw returns the 64-bit value for (seed, key, counter).
//
// The three inputs are folded through splitmix64 in sequence rather than XORed
// together, so that swapping two of them — the same event at step 7 and a
// different event at a step that happens to hash alike — cannot collide.
func draw(seed int64, key string, counter int64) uint64 {
	h := splitmix64(uint64(seed) + 0x243F6A8885A308D3)
	h = splitmix64(h ^ fnv1a(key))
	return splitmix64(h ^ uint64(counter))
}

// uniform maps a 64-bit draw onto [0, 1).
//
// The top 53 bits are used because that is float64's significand width: taking
// the low bits of a generator and dividing would concentrate the results on a
// coarse lattice for large values. 1/2^53 is exact in binary floating point, so
// the multiplication introduces no rounding of its own.
func uniform(h uint64) float64 {
	return float64(h>>11) / (1 << 53)
}

// uniformAt is the [0,1) draw for a key and counter.
func uniformAt(seed int64, key string, counter int64) float64 {
	return uniform(draw(seed, key, counter))
}

// normalClamp bounds the uniform fed to the inverse normal CDF.
//
// The quantile of 0 is −∞ and of 1 is +∞, so the endpoints must be excluded. The
// bound is 2^-53-ish rather than something tighter because a draw that lands
// there is already a 7-sigma event; clamping it costs nothing observable and
// removes the one input that could put an infinity into a price.
const normalClamp = 1e-15

// normalAt returns a standard normal draw for (seed, key, counter).
//
// It uses odds.NormalQuantile — the AS241 inverse normal already in the domain —
// rather than Box–Muller or a ziggurat. Inverse-CDF is the only method that
// consumes exactly one uniform per draw, and consuming exactly one is what keeps
// the counter-based scheme path-independent: a rejection or pair-based method
// would make the number of draws depend on their values.
//
// The error return of odds.NormalQuantile cannot fire here, because the argument
// is clamped strictly inside (0, 1) on the line above. It is still checked,
// because "cannot fire" arguments are how silent NaNs get into price series; on
// the impossible path the draw is zero, which is the distribution's mean and
// therefore the least distorting answer available.
func normalAt(seed int64, key string, counter int64) float64 {
	u := uniformAt(seed, key, counter)
	if u < normalClamp {
		u = normalClamp
	} else if u > 1-normalClamp {
		u = 1 - normalClamp
	}
	z, err := odds.NormalQuantile(odds.Probability(u))
	if err != nil || math.IsNaN(z) || math.IsInf(z, 0) {
		return 0
	}
	return z
}

// pickIndex returns a deterministic index in [0, n) for (seed, key, counter).
func pickIndex(seed int64, key string, counter int64, n int) int {
	if n <= 0 {
		return 0
	}
	return int(draw(seed, key, counter) % uint64(n))
}

// pickPair returns two distinct indices in [0, n), deterministically.
//
// The second index is drawn from [0, n-1) and shifted past the first, which is
// the standard way to sample without replacement in constant time and without a
// rejection loop — the loop would reintroduce the value-dependent draw count
// that normalAt goes out of its way to avoid.
func pickPair(seed int64, key string, counter int64, n int) (int, int) {
	if n < 2 {
		return 0, 0
	}
	a := pickIndex(seed, key+":a", counter, n)
	b := pickIndex(seed, key+":b", counter, n-1)
	if b >= a {
		b++
	}
	return a, b
}
