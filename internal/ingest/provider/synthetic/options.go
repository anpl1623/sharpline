package synthetic

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidOptions means the adapter was constructed with a configuration it
// cannot run. CLAUDE.md §12: "typed struct and startup validation — fail fast
// and loudly on a bad config".
var ErrInvalidOptions = errors.New("synthetic: invalid options")

// EnvSeed is the environment variable that fixes the generator's seed.
//
// The name is FROZEN. Two things depend on the exact string: a reproducible
// demo ("run it with SHARPLINE_SYNTHETIC_SEED=7 and you will see the same board
// I did") and the compose/Helm environment, which passes it to `ingest` and to
// nothing else. Renaming it silently re-randomises every recorded walkthrough.
//
// THIS PACKAGE DOES NOT READ IT. internal/platform/config owns the parse, as it
// owns every other environment variable, and hands the value to New through
// Options.Seed; the constant is here so the variable is discoverable from the
// package it configures, and TestEnvSeedMatchesPlatformConfig asserts the two
// spellings agree. A second parser would be a second place for the default and
// the error message to drift.
const EnvSeed = "SHARPLINE_SYNTHETIC_SEED"

// Defaults. Every one is overridable through [Options]; a zero field takes the
// default rather than failing, so an empty Options is a working adapter.
const (
	// DefaultSeed is the seed used when EnvSeed is unset.
	//
	// It is a fixed constant rather than a clock or a random draw, and that is
	// the whole point: an operator who sets nothing still gets the SAME board on
	// every start, so "the line moved" always means the model moved and never
	// means the process restarted.
	DefaultSeed int64 = 20260817

	// DefaultStep is the model's time quantum.
	//
	// Two constraints fix it. It must be well below the live polling cadence
	// (ADR 0003 buys 90 seconds) or the generator would return an identical
	// board on consecutive live polls and the pipeline would look frozen. And
	// maxBookLag steps must stay under the le="120" staleness bucket that
	// deploy/observability/rules/sharpline-alerts.yml treats as the SLO
	// boundary, because a book's quote is stamped with the instant of the view
	// it is quoting. 10s × 9 steps = 90s satisfies both.
	DefaultStep = 10 * time.Second

	// DefaultSlateDays is how many days forward the slate reaches, counting
	// today. Five puts events on both sides of the scheduler's 12-hour "today"
	// and 7-day "distant" boundaries, so every tier of
	// scheduler.ClassifyEvent has something in it.
	DefaultSlateDays = 5

	// DefaultEventsPerLeaguePerDay is the fixture grid's density.
	//
	// Eight is not arbitrary. It makes the spacing 3h, and with liveDuration at
	// 2h45m each league's between-fixture gap is 15 minutes while leagueOffset
	// staggers the four leagues by 45 minutes — so the four gaps tile the
	// 3-hour cycle without overlapping and at least three leagues are in play
	// at every instant. Changing this number breaks that property; see
	// TestLeagueGapsDoNotOverlap.
	DefaultEventsPerLeaguePerDay = 8

	// DefaultQuotaBudget is the simulated credit budget, per DefaultQuotaPeriod.
	//
	// It is far larger than any demo consumes — a live league sweep costs a
	// handful of credits — because the budget exists to keep the quota code path
	// warm, not to ration anything. See [Options.QuotaBudget].
	DefaultQuotaBudget int64 = 5_000_000

	// DefaultQuotaPeriod is the window DefaultQuotaBudget refills over. It
	// matches ADR 0003's 30-day month so the two adapters' gauges are the same
	// unit on the same dashboard.
	DefaultQuotaPeriod = 30 * 24 * time.Hour

	// DefaultTimeout bounds one Fetch or Catalogue.
	//
	// Nothing here can block — there is no socket — so this is a backstop
	// against a pathological configuration (a million-day slate) rather than
	// against a slow peer. provider.Adapter requires it regardless: "an adapter
	// must also apply its own default so that a caller passing
	// context.Background() cannot hang the poller for ever."
	DefaultTimeout = 5 * time.Second
)

// Bounds. They exist to turn a fat-fingered configuration into a startup error
// rather than into an adapter that allocates for a minute per fetch.
const (
	maxSlateDays             = 60
	maxEventsPerLeaguePerDay = 48
	maxStep                  = time.Hour
	minStep                  = time.Second
)

// Options configures the generator.
//
// The zero value is valid and yields the documented defaults. Every field is
// read once, in [New], and copied into the adapter; nothing here is consulted
// again at fetch time, so an Options value cannot be mutated out from under a
// running adapter.
type Options struct {
	// Seed fixes the entire simulated universe. The same seed produces the same
	// fixtures, the same latent paths, the same steam moves and the same prices
	// at the same instants, in every process and after every restart.
	//
	// Zero means DefaultSeed. A caller reading EnvSeed should use [SeedFromEnv],
	// which applies that rule in one place.
	Seed int64

	// Clock is the adapter's only source of time. nil means time.Now.
	//
	// It is injected rather than read directly because the determinism contract
	// is stated over clock readings: two adapters given the same seed and the
	// same instants must produce byte-identical snapshots, and a test cannot
	// assert that against the wall clock.
	Clock func() time.Time

	// Step is the model's time quantum. Zero means DefaultStep.
	//
	// The model advances with TIME, not with polls: the state at an instant is
	// the same whether the scheduler asked once or a thousand times, and two
	// polls inside one step return identical prices. That is what makes change
	// detection (CLAUDE.md §5) measurable rather than trivially 100%.
	Step time.Duration

	// SlateDays is how many days forward the fixture grid reaches. Zero means
	// DefaultSlateDays. Yesterday is always included on top of it, so that a
	// contest still in play at midnight does not vanish when the date rolls.
	SlateDays int

	// EventsPerLeaguePerDay is how many contests each league stages per day.
	// Zero means DefaultEventsPerLeaguePerDay.
	EventsPerLeaguePerDay int

	// QuotaBudget is the simulated credit budget. Zero means
	// DefaultQuotaBudget; negative is rejected.
	//
	// The credits are this adapter's own, not a claim about a remote service —
	// generating a price costs nothing and no money moves. They exist so the
	// quota surface (the sharpline_provider_quota_remaining gauge, the
	// ProviderQuotaLow alert, the scheduler's exhaustion backoff) is exercised
	// on the offline path instead of only by whoever pays for an API key. Set it
	// small to demonstrate exhaustion.
	QuotaBudget int64

	// QuotaPeriod is the window QuotaBudget refills over. Zero means
	// DefaultQuotaPeriod.
	QuotaPeriod time.Duration

	// Timeout bounds one Fetch or Catalogue. Zero means DefaultTimeout.
	Timeout time.Duration
}

// withDefaults returns a copy with every zero field replaced by its default.
func (o Options) withDefaults() Options {
	if o.Seed == 0 {
		o.Seed = DefaultSeed
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Step == 0 {
		o.Step = DefaultStep
	}
	if o.SlateDays == 0 {
		o.SlateDays = DefaultSlateDays
	}
	if o.EventsPerLeaguePerDay == 0 {
		o.EventsPerLeaguePerDay = DefaultEventsPerLeaguePerDay
	}
	if o.QuotaBudget == 0 {
		o.QuotaBudget = DefaultQuotaBudget
	}
	if o.QuotaPeriod == 0 {
		o.QuotaPeriod = DefaultQuotaPeriod
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	return o
}

// Validate reports whether the options describe a runnable generator. It is
// called by New after defaults are applied, so it never sees a zero field.
func (o Options) Validate() error {
	switch {
	case o.Clock == nil:
		return fmt.Errorf("%w: Clock must not be nil", ErrInvalidOptions)
	case o.Step < minStep || o.Step > maxStep:
		return fmt.Errorf("%w: Step %s is outside [%s, %s]", ErrInvalidOptions, o.Step, minStep, maxStep)
	case o.SlateDays < 1 || o.SlateDays > maxSlateDays:
		return fmt.Errorf("%w: SlateDays %d is outside [1, %d]", ErrInvalidOptions, o.SlateDays, maxSlateDays)
	case o.EventsPerLeaguePerDay < 1 || o.EventsPerLeaguePerDay > maxEventsPerLeaguePerDay:
		return fmt.Errorf("%w: EventsPerLeaguePerDay %d is outside [1, %d]",
			ErrInvalidOptions, o.EventsPerLeaguePerDay, maxEventsPerLeaguePerDay)
	case o.QuotaBudget < 0:
		return fmt.Errorf("%w: QuotaBudget %d must not be negative", ErrInvalidOptions, o.QuotaBudget)
	case o.QuotaPeriod <= 0:
		return fmt.Errorf("%w: QuotaPeriod %s must be positive", ErrInvalidOptions, o.QuotaPeriod)
	case o.Timeout <= 0:
		return fmt.Errorf("%w: Timeout %s must be positive", ErrInvalidOptions, o.Timeout)
	}
	return nil
}

// -----------------------------------------------------------------------------
// The simulated calendar
// -----------------------------------------------------------------------------

// Calendar constants. universe.go's buildSlate and buildMatch read these; they
// are here rather than there because they are the knobs that decide which
// scheduler windows the slate exercises, which is a configuration question
// rather than a fact about the invented leagues.
const (
	// liveDuration is how long a contest is in play.
	//
	// It is 15 minutes short of the default fixture spacing on purpose: see
	// DefaultEventsPerLeaguePerDay for the tiling argument that keeps at least
	// three leagues live at every instant.
	liveDuration = 165 * time.Minute

	// endedGrace is how long a finished contest stays on the board after the
	// final whistle. It exists so the normalizer sees the transition to closed
	// rather than having the event simply disappear, which is the difference
	// between a market that settles and one that is orphaned on a compacted
	// topic.
	endedGrace = 45 * time.Minute

	// propPostingWindow is how long before kickoff player props are offered. A
	// book does not price a player prop three days out, and a scheduler tier
	// that spends credits on markets nobody has posted is spending them on
	// nothing.
	propPostingWindow = 3 * time.Hour

	// futuresHorizonDays is how far out the season-title event's notional start
	// is. Nothing prices against it — scheduler.ClassifyEvent sends an outright
	// to WindowFutures by KIND, whatever the clock says — but domain.NewEvent
	// requires a scheduled start, and a date inside the slate would be a lie
	// about when the season ends.
	futuresHorizonDays = 120
)
