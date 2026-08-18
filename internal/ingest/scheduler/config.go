package scheduler

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidConfig is returned by [Config.Validate] and by [New]. Every
// validation failure wraps it, so a caller matches with errors.Is and an
// operator gets the specific field in the message (CLAUDE.md §12: "fail fast
// and loudly on a bad config").
var ErrInvalidConfig = errors.New("scheduler: invalid configuration")

// Package defaults. Every one is derived in doc.go's quota arithmetic from
// ADR 0003; none of them is a round number chosen because it looked tidy.
const (
	// DefaultCreditsPerSweep is markets × regions for the board CLAUDE.md §6
	// specifies: h2h, spreads and totals over one US region. ADR 0003's cost
	// model makes this exactly the credit cost of one /odds request.
	DefaultCreditsPerSweep = 3

	// DefaultQuotaBudget is ADR 0003's recommended 100K tier ($59/month), the
	// cheapest that carries a four-league live board.
	DefaultQuotaBudget = 100_000

	// DefaultQuotaPeriod is the 30-day month every figure in ADR 0003 is
	// computed against.
	DefaultQuotaPeriod = 30 * 24 * time.Hour

	// DefaultMaxConcurrentPolls bounds in-flight provider requests. Four is
	// ADR 0003's league count: enough that a synchronised live tick across the
	// whole slate does not serialise, low enough that the provider sees a
	// handful of connections rather than a stampede. ADR 0003 records that a
	// per-second rate limit is NOT VERIFIED to be absent, so this is also the
	// only thing standing between a large slate and an unannounced 429.
	DefaultMaxConcurrentPolls = 4

	// DefaultCatalogueRefresh is how often the event list is re-read. ADR 0003
	// "Consequent implementation requirements" #2: /events and /sports are
	// FREE, so this can be aggressive. The cost of refreshing is one HTTP
	// round trip; the cost of NOT refreshing is polling a league at the
	// pregame cadence for a minute after its first game went live.
	DefaultCatalogueRefresh = time.Minute

	// DefaultPollTimeout bounds one provider sweep. CLAUDE.md §12: "every
	// external call has a timeout".
	DefaultPollTimeout = 20 * time.Second

	// DefaultCatalogueTimeout bounds one catalogue refresh.
	DefaultCatalogueTimeout = 15 * time.Second

	// DefaultShutdownGrace is how long an in-flight poll may continue after
	// the run context is cancelled. Below httpx.DefaultShutdownTimeout (15s)
	// so the whole process still drains inside its own budget.
	DefaultShutdownGrace = 5 * time.Second

	// DefaultDiscoveryWindow is the cadence for a league whose fixtures are not
	// yet known. See Config.DiscoveryWindow.
	DefaultDiscoveryWindow = WindowDistant

	// DefaultJitterFraction spreads sweeps so four leagues sharing one cadence
	// do not fire in the same millisecond for ever. ADR 0003 flags the
	// provider's per-second rate limit as unverified and says the scheduler
	// "should jitter its sweeps […] regardless".
	DefaultJitterFraction = 0.10

	// maxBackoffDoublings caps the shift used to compute a backed-off
	// interval. Purely an overflow guard: 2^20 × 90s is already 3 years, and
	// the tier ceiling clamps long before that. It exists so an absurd
	// unchanged-count cannot shift a time.Duration into a negative number.
	maxBackoffDoublings = 20
)

// Tier is one window's cadence: the interval it polls at when the payload is
// moving, and the ceiling that adaptive backoff may not exceed when it is not.
//
// MaxInterval == Interval DISABLES backoff for that tier, which is the correct
// setting for live play — see doc.go.
type Tier struct {
	Interval    time.Duration
	MaxInterval time.Duration
}

// Validate checks one tier.
func (t Tier) Validate(w Window) error {
	switch {
	case t.Interval <= 0:
		return fmt.Errorf("%w: tier %s Interval must be positive, got %s", ErrInvalidConfig, w, t.Interval)
	case t.MaxInterval < t.Interval:
		return fmt.Errorf("%w: tier %s MaxInterval (%s) is below Interval (%s)",
			ErrInvalidConfig, w, t.MaxInterval, t.Interval)
	}
	return nil
}

// intervalAfter returns the effective interval after n consecutive unchanged
// polls: the base interval doubled n times, clamped to the ceiling.
//
// n <= 0 is the "something moved" case and returns the base interval, which is
// what makes recovery from backoff immediate rather than gradual (§5's
// requirement that a backed-off market return to cadence as soon as it moves).
func (t Tier) intervalAfter(n int) time.Duration {
	if n <= 0 {
		return t.Interval
	}
	if n > maxBackoffDoublings {
		n = maxBackoffDoublings
	}
	d := t.Interval << uint(n) //nolint:gosec // bounded by maxBackoffDoublings above.
	if d <= 0 || d > t.MaxInterval {
		return t.MaxInterval
	}
	return d
}

// Tiers is the whole cadence ladder. It is a struct rather than a
// map[Window]Tier so that a missing window is a compile-time impossibility
// instead of a zero-valued Tier that would poll every 0 seconds.
type Tiers struct {
	Live    Tier
	NearTip Tier
	Today   Tier
	Distant Tier
	Futures Tier
}

// DefaultTiers is doc.go's table. Changing a number here changes the monthly
// bill; re-run the arithmetic in doc.go before doing so.
func DefaultTiers() Tiers {
	return Tiers{
		Live:    Tier{Interval: 90 * time.Second, MaxInterval: 90 * time.Second},
		NearTip: Tier{Interval: 5 * time.Minute, MaxInterval: 10 * time.Minute},
		Today:   Tier{Interval: 15 * time.Minute, MaxInterval: time.Hour},
		Distant: Tier{Interval: time.Hour, MaxInterval: 4 * time.Hour},
		Futures: Tier{Interval: 6 * time.Hour, MaxInterval: 24 * time.Hour},
	}
}

// For returns the tier for w. An invalid window returns the slowest tier and
// false, so a caller that ignores the boolean degrades to under-polling rather
// than to a zero interval and a hot loop.
func (t Tiers) For(w Window) (Tier, bool) {
	switch w {
	case WindowLive:
		return t.Live, true
	case WindowNearTip:
		return t.NearTip, true
	case WindowToday:
		return t.Today, true
	case WindowDistant:
		return t.Distant, true
	case WindowFutures:
		return t.Futures, true
	default:
		return t.Futures, false
	}
}

// Validate checks every tier and the ordering between them.
func (t Tiers) Validate() error {
	prev := time.Duration(0)
	for _, w := range Windows() {
		tier, ok := t.For(w)
		if !ok {
			return fmt.Errorf("%w: no tier for window %s", ErrInvalidConfig, w)
		}
		if err := tier.Validate(w); err != nil {
			return err
		}
		// Urgency order must match cadence order, or the whole "high frequency
		// for live, low for futures" contract is a lie the type system cannot
		// see. An equal interval between adjacent tiers is allowed (two windows
		// may legitimately share a cadence); an inversion is not.
		if tier.Interval < prev {
			return fmt.Errorf("%w: tier %s polls every %s, slower-tier ordering violated (previous tier was %s)",
				ErrInvalidConfig, w, tier.Interval, prev)
		}
		prev = tier.Interval
	}
	return nil
}

// Quota is the provider credit budget. CLAUDE.md §5 requires the budget to be
// a config value; ADR 0003 requires it to be retunable for a different tier
// without a code change.
type Quota struct {
	// Budget is credits available per Period. Zero or negative is rejected —
	// "unlimited" is expressed by a provider whose CreditsPerSweep is 0 (the
	// synthetic generator), not by an unbounded budget, so that the limiter is
	// always live and never has a disabled code path that only production
	// exercises.
	Budget int

	// Period is the window Budget applies to. ADR 0003's figures are all
	// per-30-day-month.
	Period time.Duration

	// Burst is the token bucket's capacity in credits. Zero means one day's
	// allocation, which is exactly the "credits / day" column of ADR 0003's
	// tier table and is what lets a five-hour live window burn at 3.4× the
	// average rate and still average out across the day.
	Burst int
}

// DefaultQuota is ADR 0003's recommended tier.
func DefaultQuota() Quota {
	return Quota{Budget: DefaultQuotaBudget, Period: DefaultQuotaPeriod}
}

// burst resolves Burst, defaulting to one day's allocation.
func (q Quota) burst() int {
	if q.Burst > 0 {
		return q.Burst
	}
	days := q.Period.Hours() / 24
	if days <= 1 {
		return q.Budget
	}
	b := int(float64(q.Budget) / days)
	if b < 1 {
		b = 1
	}
	return b
}

// Validate checks the quota.
func (q Quota) Validate() error {
	switch {
	case q.Budget <= 0:
		return fmt.Errorf("%w: Quota.Budget must be positive, got %d", ErrInvalidConfig, q.Budget)
	case q.Period <= 0:
		return fmt.Errorf("%w: Quota.Period must be positive, got %s", ErrInvalidConfig, q.Period)
	case q.Burst < 0:
		return fmt.Errorf("%w: Quota.Burst must not be negative, got %d", ErrInvalidConfig, q.Burst)
	case q.Burst > q.Budget:
		return fmt.Errorf("%w: Quota.Burst (%d) exceeds Quota.Budget (%d)", ErrInvalidConfig, q.Burst, q.Budget)
	}
	return nil
}

// Config is the scheduler's whole configuration surface.
//
// It is a plain value with a Validate method rather than something that reads
// the environment itself: CLAUDE.md §12 puts configuration loading in one place
// and injects the result. internal/ingest.LoadConfig is the loader.
type Config struct {
	// Provider is the adapter's name — "the-odds-api" or "synthetic". It is
	// the `provider` label on every series this package exports, and it is the
	// value the Grafana dashboard's $provider variable is populated from.
	// Required.
	Provider string

	// Tiers is the cadence ladder. Zero value means DefaultTiers.
	Tiers Tiers

	// Boundaries are the window cutoffs. Zero value means DefaultBoundaries.
	Boundaries Boundaries

	// DiscoveryWindow is the cadence a league is polled at while nothing is
	// known about its fixtures — see [Catalogue] for why that state exists at
	// all. WindowUnknown means DefaultDiscoveryWindow.
	//
	// It is deliberately a slow tier. A league's FIRST sweep fires immediately
	// whatever its window, so discovery of a real slate is instant; this only
	// governs a league that keeps coming back with nothing, which is an
	// out-of-season competition. At the default (distant: 1h, backing off to
	// 4h) four dormant leagues cost about 2,000 credits a month — 2% of ADR
	// 0003's recommended tier — for the guarantee that a new slate is picked
	// up within the hour without anyone restarting the service.
	DiscoveryWindow Window

	// Quota is the credit budget. Zero value means DefaultQuota.
	Quota Quota

	// CreditsPerSweep is what one league sweep costs against Quota. Zero is
	// legitimate and is what the synthetic generator uses: it costs nothing,
	// so the limiter never engages, without the limiter having to be switched
	// off. Negative is rejected.
	//
	// It is a signed int with a deliberate zero default rather than defaulting
	// to DefaultCreditsPerSweep, because a caller that forgets to set it for
	// the REAL provider would then silently poll for free against a budget
	// that never drains. internal/ingest.LoadConfig sets it from the selected
	// adapter, and [Config.Validate] refuses a real provider that left it at 0.
	CreditsPerSweep int

	// MaxConcurrentPolls bounds in-flight sweeps. Zero means
	// DefaultMaxConcurrentPolls.
	MaxConcurrentPolls int

	// CatalogueRefresh is how often the event list is re-read. Zero means
	// DefaultCatalogueRefresh.
	CatalogueRefresh time.Duration

	// CatalogueTimeout bounds one catalogue call. Zero means
	// DefaultCatalogueTimeout.
	CatalogueTimeout time.Duration

	// PollTimeout bounds one sweep. Zero means DefaultPollTimeout.
	PollTimeout time.Duration

	// ShutdownGrace is how long an in-flight sweep may run past cancellation.
	// Zero means DefaultShutdownGrace.
	ShutdownGrace time.Duration

	// JitterFraction spreads sweeps by ±fraction of the interval. Zero means
	// DefaultJitterFraction; a negative value is rejected; explicitly setting
	// it to a tiny positive value is how a test makes timing deterministic.
	JitterFraction float64

	// Seed seeds the per-league jitter RNG. Zero seeds from the clock. It
	// exists so a test gets the same schedule twice, for the same reason
	// CLAUDE.md §5 wants the synthetic provider seeded.
	Seed int64

	// Now is the clock. Zero means time.Now. Injected rather than read
	// globally so window classification is testable without sleeping through
	// a real 90-second cadence (CLAUDE.md §12: no global mutable state).
	Now func() time.Time
}

// DefaultConfig returns the configuration for provider with every default
// applied. CreditsPerSweep is left for the caller, because only the caller
// knows which adapter it selected.
func DefaultConfig(provider string) Config {
	return Config{
		Provider:           provider,
		Tiers:              DefaultTiers(),
		Boundaries:         DefaultBoundaries(),
		DiscoveryWindow:    DefaultDiscoveryWindow,
		Quota:              DefaultQuota(),
		MaxConcurrentPolls: DefaultMaxConcurrentPolls,
		CatalogueRefresh:   DefaultCatalogueRefresh,
		CatalogueTimeout:   DefaultCatalogueTimeout,
		PollTimeout:        DefaultPollTimeout,
		ShutdownGrace:      DefaultShutdownGrace,
		JitterFraction:     DefaultJitterFraction,
	}
}

// withDefaults returns a copy of c with every zero-valued field resolved. It is
// called once by [New] so nothing downstream has to re-resolve a default.
func (c Config) withDefaults() Config {
	if c.Tiers == (Tiers{}) {
		c.Tiers = DefaultTiers()
	}
	if c.Boundaries == (Boundaries{}) {
		c.Boundaries = DefaultBoundaries()
	}
	if c.Quota == (Quota{}) {
		c.Quota = DefaultQuota()
	}
	if c.DiscoveryWindow == WindowUnknown {
		c.DiscoveryWindow = DefaultDiscoveryWindow
	}
	if c.MaxConcurrentPolls == 0 {
		c.MaxConcurrentPolls = DefaultMaxConcurrentPolls
	}
	if c.CatalogueRefresh == 0 {
		c.CatalogueRefresh = DefaultCatalogueRefresh
	}
	if c.CatalogueTimeout == 0 {
		c.CatalogueTimeout = DefaultCatalogueTimeout
	}
	if c.PollTimeout == 0 {
		c.PollTimeout = DefaultPollTimeout
	}
	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = DefaultShutdownGrace
	}
	if c.JitterFraction == 0 {
		c.JitterFraction = DefaultJitterFraction
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Validate reports every problem it can find, wrapping [ErrInvalidConfig].
//
// It is called on the DEFAULTED copy by [New], so a zero-valued Config is
// valid; calling it directly on a partially-filled Config will reject fields a
// default would have supplied. That asymmetry is intentional: Validate answers
// "is this exactly what the scheduler will run with", not "could this be made
// to work".
func (c Config) Validate() error {
	if c.Provider == "" {
		return fmt.Errorf("%w: Provider is empty (it is the `provider` metric label)", ErrInvalidConfig)
	}
	if err := c.Tiers.Validate(); err != nil {
		return err
	}
	if err := c.Boundaries.Validate(); err != nil {
		return err
	}
	if !c.DiscoveryWindow.Valid() {
		return fmt.Errorf("%w: DiscoveryWindow is %s", ErrInvalidConfig, c.DiscoveryWindow)
	}
	if err := c.Quota.Validate(); err != nil {
		return err
	}
	switch {
	case c.CreditsPerSweep < 0:
		return fmt.Errorf("%w: CreditsPerSweep must not be negative, got %d", ErrInvalidConfig, c.CreditsPerSweep)
	case c.CreditsPerSweep > c.Quota.burst():
		// One sweep that cannot fit in the bucket would block for ever, which
		// presents as a silently frozen board.
		return fmt.Errorf("%w: CreditsPerSweep (%d) exceeds the token bucket capacity (%d credits); "+
			"no sweep could ever be admitted", ErrInvalidConfig, c.CreditsPerSweep, c.Quota.burst())
	case c.MaxConcurrentPolls <= 0:
		return fmt.Errorf("%w: MaxConcurrentPolls must be positive, got %d", ErrInvalidConfig, c.MaxConcurrentPolls)
	case c.CatalogueRefresh <= 0:
		return fmt.Errorf("%w: CatalogueRefresh must be positive, got %s", ErrInvalidConfig, c.CatalogueRefresh)
	case c.CatalogueTimeout <= 0:
		return fmt.Errorf("%w: CatalogueTimeout must be positive, got %s", ErrInvalidConfig, c.CatalogueTimeout)
	case c.PollTimeout <= 0:
		return fmt.Errorf("%w: PollTimeout must be positive, got %s", ErrInvalidConfig, c.PollTimeout)
	case c.ShutdownGrace < 0:
		return fmt.Errorf("%w: ShutdownGrace must not be negative, got %s", ErrInvalidConfig, c.ShutdownGrace)
	case c.JitterFraction < 0 || c.JitterFraction >= 1:
		return fmt.Errorf("%w: JitterFraction must be in [0,1), got %v", ErrInvalidConfig, c.JitterFraction)
	case c.Now == nil:
		return fmt.Errorf("%w: Now is nil", ErrInvalidConfig)
	}
	return nil
}

// MonthlyCredits reports what this configuration would spend in credits per
// 30-day month if every league in leagues sat in window for the whole month.
//
// It is the arithmetic in doc.go, executable — which is the point. A cadence
// change that blows the budget should fail a test rather than an invoice, and
// TestDefaultConfigFitsTheRecommendedTier is that test.
func (c Config) MonthlyCredits(leagues int, window Window, hoursPerDay float64) float64 {
	tier, ok := c.Tiers.For(window)
	if !ok || tier.Interval <= 0 || leagues <= 0 || hoursPerDay <= 0 {
		return 0
	}
	sweepsPerDay := (hoursPerDay * 3600) / tier.Interval.Seconds()
	return sweepsPerDay * float64(c.CreditsPerSweep) * float64(leagues) * 30
}
