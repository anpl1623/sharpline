package steam

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrInvalidConfig reports a threshold set that cannot mean what it says.
// CLAUDE.md §12: "fail fast and loudly on a bad config".
//
// It is this package's only sentinel. There is no ErrNotFound and no
// ErrUnavailable because there is nothing here to look up and nothing to be
// unavailable: the detector is arithmetic over a bounded buffer.
var ErrInvalidConfig = errors.New("steam: invalid detector configuration")

// Defaults. Every one is a term of the phase-12 contract, so each carries the
// reasoning that produced it rather than only the number.
const (
	// DefaultWindow is the hopping window's length: three minutes.
	//
	// It is sized against TWO independent constraints and sits at the tightest
	// point that satisfies both.
	//
	// LOWER BOUND — the window must contain at least two observations per book,
	// or there is no delta to measure. ADR 0003 buys a 90-second live poll
	// cadence, so a window shorter than 180 seconds cannot guarantee two.
	//
	// UPPER BOUND — a steam move is INSTANTANEOUS (the generator lands the whole
	// amplitude inside one 10-second step) where ordinary drift accumulates with
	// the square root of elapsed time. So the longer the window, the more drift is
	// inside it and the worse the separation between the two. Every second past
	// the lower bound costs discrimination.
	//
	// Three minutes is therefore the lower bound and not a compromise between the
	// two. A faster provider cadence would justify a shorter window and a sharper
	// detector; that is a reason to shorten this when the cadence changes, and it
	// is why the window length is stored on every finding.
	DefaultWindow = 3 * time.Minute

	// DefaultHop is how far the window advances: one minute.
	//
	// Window/Hop = 3, so each observation is seen in three framings and a move
	// near a window boundary is not cut in half by it. A hop equal to the window
	// (tumbling) would do exactly that — a jump straddling a boundary would show
	// as two half-moves, neither of which clears the magnitude threshold — and is
	// the failure mode hopping exists to remove. A finer hop buys diminishing
	// coverage for linearly more windows to evaluate and suppress.
	DefaultHop = time.Minute

	// DefaultAllowedLateness is how far behind the newest observation the
	// watermark trails: three minutes.
	//
	// THIS IS THE PARAMETER THAT MAKES THE DETECTOR WORK OR MAKES IT BLIND, and
	// doc.go argues it at length. Books quote off LAGGED views and stamp each
	// quote with the instant of the view — the synthetic generator's deepest book
	// is 90 seconds behind — so a lagged book's observation covering event-time T
	// does not arrive until up to its lag plus one poll interval after the sharp
	// book's. 90 seconds of book lag plus 90 seconds of poll cadence (ADR 0003) is
	// 180.
	//
	// Too small and the lagged books are absent from every window, the correlation
	// is computed over one book, and nothing ever fires — a failure that looks
	// exactly like a quiet market. Too large and findings are correct but late,
	// which for an alerting surface is its own kind of useless.
	DefaultAllowedLateness = 3 * time.Minute

	// DefaultMaxFollowerLag bounds how long after the lead a book may move and
	// still count as following it: two minutes.
	//
	// It has to exceed the deepest book's view lag (90 seconds in the generator)
	// or the softest book — the one whose participation is the most informative —
	// could never qualify. It must stay well inside the window, because a lag
	// longer than the window cannot be observed at all: both instants have to lie
	// inside [start, end) for the pair to be measured.
	DefaultMaxFollowerLag = 2 * time.Minute

	// DefaultCooldown suppresses a repeat finding for the same market, selection
	// and direction: five minutes of EVENT time.
	//
	// One jump appears in Window/Hop = 3 consecutive windows, and propagation
	// through the lagged books stretches that to four or five. Five minutes covers
	// the run with margin. It is deliberately shorter than the generator's
	// steamFullBlocks (three hours, over which a steam move holds its full
	// amplitude), because a market that genuinely steams twice in ten minutes has
	// done something worth two alerts.
	DefaultCooldown = 5 * time.Minute

	// DefaultMinMagnitude is the floor on |Δ| for the window, in IMPLIED
	// PROBABILITY POINTS. Five points.
	//
	// # Where the number comes from
	//
	// It is CALIBRATED AGAINST THE GENERATOR, not guessed, and
	// TestFiresOnGeneratedSteamAndNotOnDrift is the measurement rather than a
	// check on one. The derivation and the measurement agree, which is why both
	// are recorded.
	//
	// The derivation. The synthetic generator is dimensionless by construction:
	// every latent process is in units of its own stationary standard deviation,
	// and a league's line dispersion converts it to points, goals or yards.
	// Running that through the normal model the generator prices with —
	// ∂P/∂latent ≈ φ(μ/σ)·lineSD/σ — gives a similar factor in every league in
	// universe.go, between roughly 0.033 and 0.063 probability points per latent
	// σ. A steam jump in noise.go is (steamMinAbsZ + |z|)·steamAmplitude, from
	// 0.4σ to about 1.5σ with a mean near 0.8σ, so a large steam move is five to
	// eight probability points and a small one is two.
	//
	// The measurement. Sweeping this threshold over six hours of model time
	// across a whole league's moneyline markets shows the candidate count
	// collapsing as a Gaussian tail from 0.030 to 0.050 — 647 candidates, then
	// 175, then 43 — and then flattening. That knee is the boundary between the
	// two populations: the steep part is ordinary drift, whose window change is
	// Gaussian, and the flat part is the steam amplitude distribution, which is
	// not. Five points sits past the knee. Below it the detector reports roughly
	// seven times as many findings as the generator plants steam moves; at it, the
	// counts are within a factor of two of the number of steam moves large enough
	// to clear it.
	//
	// # Two honest consequences of a threshold in probability points
	//
	// RECALL IS DELIBERATELY LOW. Only steam moves above about one latent σ clear
	// five points, which is roughly a fifth of the moves noise.go generates. That
	// is the right trade for an ALERT surface — the cost of a missed small move is
	// nothing and the cost of a false alert is that nobody reads the next one —
	// but it is a trade and it should be named rather than discovered.
	//
	// SENSITIVITY VARIES BY LEAGUE, because the conversion factor does. Five
	// points is 0.8 latent σ on the football league and 1.5σ on the gridiron
	// league, so the same threshold is stricter on the sport whose line moves
	// least in probability terms. That is a defensible place to land — the unit a
	// user reasons in is "the market moved five points", not "the market moved 1.2
	// sigma" — and a per-league threshold is the obvious refinement if the
	// gridiron surface ever looks too quiet.
	//
	// It is the parameter most worth re-deriving against a real provider, which is
	// why it is stored on every finding as threshold_magnitude rather than being
	// implicit in the code that produced it.
	DefaultMinMagnitude = 0.050

	// DefaultMinVelocity is the floor on |velocity| in probability points per
	// minute: five points over the three-minute window, so one and two-thirds
	// points per minute.
	//
	// At the default window it is DefaultMinMagnitude / 3 exactly, so it binds at
	// the same place and is redundant. That is deliberate and is not an
	// oversight — see doc.go. The two stop being redundant the instant the window
	// length changes, and both are stored on every finding so a population
	// spanning a re-tuning can still be separated.
	DefaultMinVelocity = DefaultMinMagnitude / 3

	// DefaultMinCorrelation is the floor on the mean signed agreement across
	// books, in [−1, 1]. One half.
	//
	// With five books quoting, 0.5 means at least three net books agreeing with
	// the lead: enough to refuse a move that one book made alone, not so much that
	// a single soft book disagreeing kills a real finding. doc.go is explicit
	// about what this statistic does and does not discriminate — it screens out a
	// lone book's tick rounding, and it does NOT separate steam from drift,
	// because books that view one latent process are correlated whatever it does.
	DefaultMinCorrelation = 0.5

	// DefaultMinFollowers is how many other books must corroborate the lead: one.
	//
	// A move nobody follows is a move by one book. It is not raised above one
	// because the deepest-lagged books may not have reported inside the window at
	// all, and requiring three corroborating books would make the detector's
	// sensitivity a function of which books happened to refresh — a property that
	// would not survive a change of provider.
	DefaultMinFollowers = 1

	// DefaultNoiseFloor is the |Δ| below which a book counts as NOT HAVING MOVED
	// for the purposes of the correlation statistic: half a probability point.
	//
	// Without a floor, a book that did not move at all still has a sign, because
	// the last unit in the last place of a float64 is never exactly zero after a
	// tick conversion — so a quiet book would contribute ±1 at random and the
	// correlation would be a coin flip weighted by rounding. Half a point is
	// comfortably above one tick on the softest book in the generator's set (a
	// 10-cent American grid) and comfortably below the magnitude threshold.
	DefaultNoiseFloor = 0.005

	// DefaultMaxSamplesPerSeries bounds the retained observations for one
	// (market, selection, book): 64.
	//
	// The pruner is what actually bounds memory — it drops everything older than
	// the oldest window still capable of being evaluated — and this is the
	// backstop for a market that produces observations far faster than the
	// watermark advances. At the default cadence one book contributes two or three
	// observations per window, so 64 is more than twenty windows of history and
	// will not be reached by anything except a misbehaving feed.
	DefaultMaxSamplesPerSeries = 64

	// DefaultMaxWindowsPerAdvance bounds how many windows one observation may
	// close: 32.
	//
	// A watermark that jumps hours forward — the first record after a long outage,
	// or a replay that reaches a gap — would otherwise evaluate every window in
	// between, over a buffer that has been pruned and holds nothing, producing
	// nothing but a stall inside a Kafka handler while the group's rebalance is
	// blocked. Past the bound the detector jumps the evaluation cursor to the
	// newest window and counts the skip under [Stats.SkippedWindows], because a
	// gap that is silently absorbed is a gap nobody investigates.
	DefaultMaxWindowsPerAdvance = 32
)

// Config is the detector's whole parameter set.
//
// It is a value type with a Validate method rather than a set of constants
// because every threshold is written onto every finding it produces: a
// deployment that re-tunes one leaves a stored population that can still be
// separated into the two regimes, and that only works if the value is data.
//
// A zero field takes the documented default, so the zero Config is a working
// detector. A field that cannot have a sensible zero — none currently — would be
// required instead.
type Config struct {
	// Window is the hopping window's length. Zero means [DefaultWindow].
	Window time.Duration

	// Hop is how far the window advances between evaluations. Zero means
	// [DefaultHop]. It must not exceed Window: a hop longer than the window would
	// leave gaps in the cover, so an observation could fall in no window at all.
	Hop time.Duration

	// AllowedLateness is how far the watermark trails the newest observation.
	// Zero means [DefaultAllowedLateness]. See the constant: this is the
	// parameter that decides whether lagged books are inside the window at all.
	//
	// A genuinely ZERO lateness — a feed with no book lag at all — is therefore
	// not expressible as a zero field, deliberately: a detector that closed its
	// windows the instant the newest observation arrived would drop every lagged
	// book, and that should be an explicit decision rather than an omitted field.
	// Spell it as a nanosecond if a feed ever warrants it.
	AllowedLateness time.Duration

	// MaxFollowerLag bounds a follower's delay behind the lead. Zero means
	// [DefaultMaxFollowerLag].
	MaxFollowerLag time.Duration

	// Cooldown suppresses a repeat for one (market, selection, direction),
	// measured on window end in EVENT TIME. Zero means [DefaultCooldown];
	// negative is rejected. Setting it to zero is not expressible, deliberately:
	// with Window/Hop = 3 a detector without a cooldown emits every move three
	// times, and that should be an explicit decision rather than an omitted field.
	Cooldown time.Duration

	// MinMagnitude is the floor on |Δ| for the window, in probability points.
	// Zero means [DefaultMinMagnitude].
	MinMagnitude float64

	// MinVelocity is the floor on |velocity| in probability points per minute.
	// Zero means [DefaultMinVelocity].
	MinVelocity float64

	// MinCorrelation is the floor on the mean signed agreement across books, in
	// [−1, 1]. Zero means [DefaultMinCorrelation]. A genuinely zero requirement
	// is expressible as a tiny positive number; a negative one is legal and means
	// "even net disagreement is acceptable", which is a defensible thing to want
	// while calibrating and a strange thing to deploy.
	MinCorrelation float64

	// MinFollowers is how many books must corroborate the lead. Zero means
	// [DefaultMinFollowers]. It cannot be less than 1: a finding with no follower
	// is one book's move, and migrations/00009 CHECKs follower_count ≥ 1 anyway,
	// so a zero here would produce rows the database refuses.
	MinFollowers int

	// NoiseFloor is the |Δ| below which a book counts as not having moved, for
	// the correlation statistic only. Zero means [DefaultNoiseFloor].
	NoiseFloor float64

	// MaxSamplesPerSeries bounds retained observations per (market, selection,
	// book). Zero means [DefaultMaxSamplesPerSeries].
	MaxSamplesPerSeries int

	// MaxWindowsPerAdvance bounds how many windows one observation may close.
	// Zero means [DefaultMaxWindowsPerAdvance].
	MaxWindowsPerAdvance int
}

// DefaultConfig returns the configuration described on each constant.
func DefaultConfig() Config {
	return Config{
		Window:               DefaultWindow,
		Hop:                  DefaultHop,
		AllowedLateness:      DefaultAllowedLateness,
		MaxFollowerLag:       DefaultMaxFollowerLag,
		Cooldown:             DefaultCooldown,
		MinMagnitude:         DefaultMinMagnitude,
		MinVelocity:          DefaultMinVelocity,
		MinCorrelation:       DefaultMinCorrelation,
		MinFollowers:         DefaultMinFollowers,
		NoiseFloor:           DefaultNoiseFloor,
		MaxSamplesPerSeries:  DefaultMaxSamplesPerSeries,
		MaxWindowsPerAdvance: DefaultMaxWindowsPerAdvance,
	}
}

// Validate reports a configuration that cannot mean what it says.
//
// Each case is a rule that would otherwise produce a detector that runs, looks
// healthy and reports nothing — which is the failure mode this whole package has
// to be defended against, because "no steam today" is also the correct output
// most of the time and the two are indistinguishable from outside.
func (c Config) Validate() error {
	r := c.resolved()
	switch {
	case r.Window <= 0:
		return fmt.Errorf("%w: Window %s must be positive", ErrInvalidConfig, r.Window)
	case r.Hop <= 0:
		return fmt.Errorf("%w: Hop %s must be positive", ErrInvalidConfig, r.Hop)
	case r.Hop > r.Window:
		return fmt.Errorf("%w: Hop %s exceeds Window %s, so consecutive windows would leave gaps "+
			"and an observation could fall in none of them", ErrInvalidConfig, r.Hop, r.Window)
	case r.AllowedLateness < 0:
		return fmt.Errorf("%w: AllowedLateness %s is negative", ErrInvalidConfig, r.AllowedLateness)
	case r.MaxFollowerLag <= 0:
		return fmt.Errorf("%w: MaxFollowerLag %s must be positive", ErrInvalidConfig, r.MaxFollowerLag)
	case r.MaxFollowerLag > r.Window:
		return fmt.Errorf("%w: MaxFollowerLag %s exceeds Window %s, so it can never bind — both "+
			"move instants must lie inside one window to be compared",
			ErrInvalidConfig, r.MaxFollowerLag, r.Window)
	case r.Cooldown < 0:
		return fmt.Errorf("%w: Cooldown %s is negative", ErrInvalidConfig, r.Cooldown)
	case !finite(r.MinMagnitude, r.MinVelocity, r.MinCorrelation, r.NoiseFloor):
		return fmt.Errorf("%w: a threshold is not finite", ErrInvalidConfig)
	case r.MinMagnitude <= 0 || r.MinMagnitude >= 1:
		return fmt.Errorf("%w: MinMagnitude %v is outside (0, 1); it is in probability points, "+
			"not decimal odds and not a percentage", ErrInvalidConfig, r.MinMagnitude)
	case r.MinVelocity <= 0:
		return fmt.Errorf("%w: MinVelocity %v must be positive", ErrInvalidConfig, r.MinVelocity)
	case r.MinCorrelation < -1 || r.MinCorrelation > 1:
		return fmt.Errorf("%w: MinCorrelation %v is outside [-1, 1]", ErrInvalidConfig, r.MinCorrelation)
	case r.MinFollowers < 1:
		return fmt.Errorf("%w: MinFollowers %d must be at least 1; migrations/00009 CHECKs "+
			"follower_count >= 1, so a lower bound here would produce rows the database refuses",
			ErrInvalidConfig, r.MinFollowers)
	case r.NoiseFloor < 0:
		return fmt.Errorf("%w: NoiseFloor %v is negative", ErrInvalidConfig, r.NoiseFloor)
	case r.NoiseFloor > r.MinMagnitude:
		return fmt.Errorf("%w: NoiseFloor %v exceeds MinMagnitude %v, so the lead book itself would "+
			"count as not having moved", ErrInvalidConfig, r.NoiseFloor, r.MinMagnitude)
	case r.MaxSamplesPerSeries < 2:
		return fmt.Errorf("%w: MaxSamplesPerSeries %d is below 2, so no delta could ever be measured",
			ErrInvalidConfig, r.MaxSamplesPerSeries)
	case r.MaxWindowsPerAdvance < 1:
		return fmt.Errorf("%w: MaxWindowsPerAdvance %d must be at least 1",
			ErrInvalidConfig, r.MaxWindowsPerAdvance)
	}
	return nil
}

// resolved returns the configuration with every zero field replaced by its
// documented default.
//
// It is applied once in [New], so the detector holds resolved values and every
// finding records what was actually applied rather than the zero a caller
// happened to leave.
func (c Config) resolved() Config {
	if c.Window == 0 {
		c.Window = DefaultWindow
	}
	if c.Hop == 0 {
		c.Hop = DefaultHop
	}
	if c.AllowedLateness == 0 {
		c.AllowedLateness = DefaultAllowedLateness
	}
	if c.MaxFollowerLag == 0 {
		c.MaxFollowerLag = DefaultMaxFollowerLag
	}
	if c.Cooldown == 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.MinMagnitude == 0 {
		c.MinMagnitude = DefaultMinMagnitude
	}
	// DERIVED FROM MinMagnitude AND Window, not from DefaultMinVelocity, and the
	// difference matters the moment a caller sets one of the two.
	//
	// The two thresholds express the same constraint at a fixed window length —
	// velocity is delta over the window in minutes — so a caller who raises
	// MinMagnitude and leaves this zero means "raise the bar", not "raise one bar
	// and leave the other where the old default put it". Taking the constant here
	// would silently leave the velocity gate binding at the OLD magnitude, which
	// is a threshold nobody chose.
	//
	// At the shipped defaults this produces exactly DefaultMinVelocity, so the
	// constant remains the honest name for the resolved value.
	if c.MinVelocity == 0 {
		c.MinVelocity = c.MinMagnitude / c.Window.Minutes()
	}
	if c.MinCorrelation == 0 {
		c.MinCorrelation = DefaultMinCorrelation
	}
	if c.MinFollowers == 0 {
		c.MinFollowers = DefaultMinFollowers
	}
	if c.NoiseFloor == 0 {
		c.NoiseFloor = DefaultNoiseFloor
	}
	if c.MaxSamplesPerSeries == 0 {
		c.MaxSamplesPerSeries = DefaultMaxSamplesPerSeries
	}
	if c.MaxWindowsPerAdvance == 0 {
		c.MaxWindowsPerAdvance = DefaultMaxWindowsPerAdvance
	}
	return c
}

// finite reports whether every value is a real number. NaN and ±Inf are the two
// ways a threshold can be silently unsatisfiable: every comparison against NaN
// is false, so a NaN floor rejects everything and reports nothing.
func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
