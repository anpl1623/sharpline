// The cadence ladder and the quota budget, including the arithmetic that fixes
// them.
//
// doc.go's credit table is prose. TestDefaultConfigFitsTheRecommendedTier is
// that prose executed: a cadence change that blows the budget should fail a test
// rather than an invoice, and Config.MonthlyCredits exists precisely so the
// assertion can be written.
package scheduler_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/ingest/scheduler"
)

// ADR 0003's recommended tier and the assumptions its Scenario C is computed
// against. Every number here is quoted from the ADR or from doc.go; none is
// invented here.
const (
	// adrLeagues is ADR 0003 Scenario C's league count.
	adrLeagues = 4

	// Hours per day each window is occupied, from doc.go's table. They sum to
	// 24 for the match windows; futures runs across the whole day because an
	// outright has no kickoff to be near.
	adrLiveHours    = 5.0
	adrNearTipHours = 1.0
	adrTodayHours   = 7.0
	adrDistantHours = 11.0
	adrFuturesHours = 24.0

	// adrScoresCreditsPerMonth is the settlement scores feed: ADR 0003's
	// "8/day @ 2 credits" per league. It is NOT scheduled by this package —
	// `settle` polls it — so it does not appear in MonthlyCredits and is added
	// here to reconcile with doc.go's per-league-per-day total.
	adrScoresCreditsPerMonth = 8 * 2 * adrLeagues * 30

	// adrTotalCreditsPerMonth is doc.go's bottom line.
	adrTotalCreditsPerMonth = 93_720

	// adrCreditsPerLeaguePerDay is doc.go's 781.
	adrCreditsPerLeaguePerDay = 781
)

// recommendedConfig is DefaultConfig with the credit cost of one sweep filled
// in, which is what internal/ingest.LoadConfig does from the selected adapter.
func recommendedConfig() scheduler.Config {
	cfg := scheduler.DefaultConfig("the-odds-api")
	cfg.CreditsPerSweep = scheduler.DefaultCreditsPerSweep
	return cfg
}

// monthlyTotal is doc.go's table, summed.
func monthlyTotal(cfg scheduler.Config) float64 {
	return cfg.MonthlyCredits(adrLeagues, scheduler.WindowLive, adrLiveHours) +
		cfg.MonthlyCredits(adrLeagues, scheduler.WindowNearTip, adrNearTipHours) +
		cfg.MonthlyCredits(adrLeagues, scheduler.WindowToday, adrTodayHours) +
		cfg.MonthlyCredits(adrLeagues, scheduler.WindowDistant, adrDistantHours) +
		cfg.MonthlyCredits(adrLeagues, scheduler.WindowFutures, adrFuturesHours) +
		adrScoresCreditsPerMonth
}

// TestDefaultConfigFitsTheRecommendedTier is the test doc.go names.
//
// It asserts the exact figure rather than merely "under budget", because the
// interesting failure is not "we went over" — it is "someone changed a cadence
// and nobody recomputed what it costs". An exact number forces the recomputation
// into the same commit.
func TestDefaultConfigFitsTheRecommendedTier(t *testing.T) {
	t.Parallel()

	cfg := recommendedConfig()

	perWindow := map[scheduler.Window]struct {
		hours float64
		want  float64
	}{
		scheduler.WindowLive:    {adrLiveHours, 72_000},
		scheduler.WindowNearTip: {adrNearTipHours, 4_320},
		scheduler.WindowToday:   {adrTodayHours, 10_080},
		scheduler.WindowDistant: {adrDistantHours, 3_960},
		scheduler.WindowFutures: {adrFuturesHours, 1_440},
	}
	for w, exp := range perWindow {
		got := cfg.MonthlyCredits(adrLeagues, w, exp.hours)
		if got != exp.want {
			t.Errorf("MonthlyCredits(%d leagues, %s, %.0fh) = %.0f, want %.0f — doc.go's credit table "+
				"and DefaultTiers have diverged", adrLeagues, w, exp.hours, got, exp.want)
		}
	}

	total := monthlyTotal(cfg)
	if total != adrTotalCreditsPerMonth {
		t.Fatalf("total = %.0f credits/month, doc.go says %d. Re-run the arithmetic in doc.go "+
			"in the same commit as the cadence change.", total, adrTotalCreditsPerMonth)
	}

	// The same number stated the other way round, because doc.go states it both
	// ways and the two must agree.
	perLeaguePerDay := total / adrLeagues / 30
	if perLeaguePerDay != adrCreditsPerLeaguePerDay {
		t.Errorf("per league per day = %.0f, doc.go says %d", perLeaguePerDay, adrCreditsPerLeaguePerDay)
	}

	budget := float64(cfg.Quota.Budget)
	if budget != scheduler.DefaultQuotaBudget {
		t.Fatalf("DefaultQuota().Budget = %.0f, want %d", budget, scheduler.DefaultQuotaBudget)
	}
	if total >= budget {
		t.Fatalf("the default cadence spends %.0f credits/month against a %.0f budget", total, budget)
	}

	headroom := (budget - total) / budget * 100
	if math.Abs(headroom-6.3) > 0.1 {
		t.Errorf("headroom = %.1f%%, doc.go says 6.3%%", headroom)
	}
}

// TestLiveAt60sDoesNotFitTheRecommendedTier executes doc.go's claim that "60s
// does not fit that tier at all", which is the reason the live cadence is 90s.
func TestLiveAt60sDoesNotFitTheRecommendedTier(t *testing.T) {
	t.Parallel()

	cfg := recommendedConfig()
	cfg.Tiers.Live = scheduler.Tier{Interval: 60 * time.Second, MaxInterval: 60 * time.Second}

	total := monthlyTotal(cfg)
	if total <= float64(cfg.Quota.Budget) {
		t.Fatalf("a 60s live cadence costs %.0f credits/month, which fits the %d budget; "+
			"doc.go's justification for 90s no longer holds", total, cfg.Quota.Budget)
	}
}

// TestNearTipAt120sDoesNotFitTheRecommendedTier executes doc.go's justification
// for a 300s near-tip cadence.
//
// NOTE: doc.go used to state the 120s alternative as "a further ~9,400/month,
// leaving 1.2% headroom". Both figures were wrong and they disagreed with each
// other — 100,200 credits is OVER the 100,000 tier, so the correct conclusion is
// stronger than the one the prose drew. The prose was corrected to match this
// test rather than the other way round.
func TestNearTipAt120sDoesNotFitTheRecommendedTier(t *testing.T) {
	t.Parallel()

	base := recommendedConfig()
	faster := recommendedConfig()
	faster.Tiers.NearTip = scheduler.Tier{Interval: 120 * time.Second, MaxInterval: 10 * time.Minute}

	extra := monthlyTotal(faster) - monthlyTotal(base)
	if extra != 6_480 {
		t.Errorf("a 120s near-tip cadence costs a further %.0f credits/month; doc.go says 6,480", extra)
	}
	if total := monthlyTotal(faster); total <= float64(base.Quota.Budget) {
		t.Errorf("a 120s near-tip cadence totals %.0f credits/month, which still fits the %d budget",
			total, base.Quota.Budget)
	}
}

// TestMonthlyCreditsRefusesNonsense: the helper answers 0 rather than a
// plausible-looking number for an input that cannot be costed.
func TestMonthlyCreditsRefusesNonsense(t *testing.T) {
	t.Parallel()

	cfg := recommendedConfig()
	cases := []struct {
		name    string
		leagues int
		window  scheduler.Window
		hours   float64
	}{
		{"no leagues", 0, scheduler.WindowLive, 5},
		{"negative leagues", -1, scheduler.WindowLive, 5},
		{"no hours", adrLeagues, scheduler.WindowLive, 0},
		{"invalid window", adrLeagues, scheduler.WindowUnknown, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cfg.MonthlyCredits(tc.leagues, tc.window, tc.hours); got != 0 {
				t.Errorf("MonthlyCredits = %v, want 0", got)
			}
		})
	}
}

// TestDefaultTiersDisableBackoffDuringLivePlay pins the property doc.go argues
// for: the live tier's ceiling EQUALS its interval, so the only thing live
// backoff could do is be wrong at the worst possible moment.
func TestDefaultTiersDisableBackoffDuringLivePlay(t *testing.T) {
	t.Parallel()

	tiers := scheduler.DefaultTiers()
	if tiers.Live.MaxInterval != tiers.Live.Interval {
		t.Errorf("live tier MaxInterval %s != Interval %s; backoff must be disabled during live play",
			tiers.Live.MaxInterval, tiers.Live.Interval)
	}
	// Every other tier must actually be able to back off, or the whole
	// "backing off on unchanged payloads" clause of §5 is inert.
	for name, tier := range map[string]scheduler.Tier{
		"near_tip": tiers.NearTip,
		"today":    tiers.Today,
		"distant":  tiers.Distant,
		"futures":  tiers.Futures,
	} {
		if tier.MaxInterval <= tier.Interval {
			t.Errorf("%s tier has ceiling %s at interval %s; it can never back off",
				name, tier.MaxInterval, tier.Interval)
		}
	}
}

func TestDefaultTiersValidate(t *testing.T) {
	t.Parallel()

	if err := scheduler.DefaultTiers().Validate(); err != nil {
		t.Fatalf("DefaultTiers().Validate(): %v", err)
	}
}

func TestTiersForUnknownWindowDegradesToTheSlowestTier(t *testing.T) {
	t.Parallel()

	tiers := scheduler.DefaultTiers()
	got, ok := tiers.For(scheduler.WindowUnknown)
	if ok {
		t.Error("Tiers.For(WindowUnknown) reported ok; an undefined window has no tier")
	}
	if got != tiers.Futures {
		t.Errorf("Tiers.For(WindowUnknown) = %+v, want the futures tier — a caller that ignores the "+
			"boolean must under-poll, not spin on a zero interval", got)
	}
}

func TestTiersValidateRejects(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*scheduler.Tiers){
		"zero live interval":     func(ts *scheduler.Tiers) { ts.Live.Interval = 0 },
		"negative live interval": func(ts *scheduler.Tiers) { ts.Live.Interval = -time.Second },
		"ceiling below interval": func(ts *scheduler.Tiers) { ts.Today.MaxInterval = time.Second },
		"urgency inversion": func(ts *scheduler.Tiers) {
			// Futures polled faster than distant: the "high frequency for live,
			// low for futures" contract is a lie the type system cannot see.
			ts.Futures = scheduler.Tier{Interval: time.Second, MaxInterval: time.Minute}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tiers := scheduler.DefaultTiers()
			mutate(&tiers)
			if err := tiers.Validate(); !errors.Is(err, scheduler.ErrInvalidConfig) {
				t.Errorf("Validate() = %v, want an error wrapping ErrInvalidConfig", err)
			}
		})
	}
}

// TestTiersValidateAllowsAdjacentEqualIntervals: two windows may legitimately
// share a cadence; only an inversion is a defect.
func TestTiersValidateAllowsAdjacentEqualIntervals(t *testing.T) {
	t.Parallel()

	tiers := scheduler.DefaultTiers()
	tiers.NearTip = tiers.Live
	if err := tiers.Validate(); err != nil {
		t.Errorf("Validate() rejected two adjacent tiers sharing a cadence: %v", err)
	}
}

func TestQuotaValidate(t *testing.T) {
	t.Parallel()

	if err := scheduler.DefaultQuota().Validate(); err != nil {
		t.Fatalf("DefaultQuota().Validate(): %v", err)
	}

	cases := map[string]scheduler.Quota{
		"zero budget":            {Budget: 0, Period: time.Hour},
		"negative budget":        {Budget: -1, Period: time.Hour},
		"zero period":            {Budget: 10, Period: 0},
		"negative period":        {Budget: 10, Period: -time.Hour},
		"negative burst":         {Budget: 10, Period: time.Hour, Burst: -1},
		"burst exceeds a period": {Budget: 10, Period: time.Hour, Burst: 11},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := q.Validate(); !errors.Is(err, scheduler.ErrInvalidConfig) {
				t.Errorf("Validate() = %v, want an error wrapping ErrInvalidConfig", err)
			}
		})
	}
}

// TestDefaultQuotaBurstIsOneDaysAllocation pins the "credits / day" column of
// ADR 0003's tier table: 100,000 over 30 days is 3,333/day, which is what lets a
// five-hour live window burn at 3.4× the average rate and still average out.
func TestDefaultQuotaBurstIsOneDaysAllocation(t *testing.T) {
	t.Parallel()

	// The bucket is observed through a Budget, because burst() is unexported and
	// a full bucket is exactly its capacity.
	b, err := scheduler.NewBudget(scheduler.DefaultQuota(), func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}
	const wantBurst = scheduler.DefaultQuotaBudget / 30
	if got := b.Tokens(); math.Abs(got-wantBurst) > 1 {
		t.Errorf("a fresh default Budget holds %.0f tokens, want one day's allocation (%d)", got, wantBurst)
	}
}

func TestConfigValidateRejects(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*scheduler.Config){
		"no provider":      func(c *scheduler.Config) { c.Provider = "" },
		"negative credits": func(c *scheduler.Config) { c.CreditsPerSweep = -1 },
		"credits exceed the bucket": func(c *scheduler.Config) {
			// One sweep that cannot fit in the bucket would block for ever,
			// which presents as a silently frozen board.
			c.CreditsPerSweep = c.Quota.Budget
		},
		"zero concurrency":           func(c *scheduler.Config) { c.MaxConcurrentPolls = 0 },
		"negative concurrency":       func(c *scheduler.Config) { c.MaxConcurrentPolls = -1 },
		"negative catalogue refresh": func(c *scheduler.Config) { c.CatalogueRefresh = -time.Second },
		"negative catalogue timeout": func(c *scheduler.Config) { c.CatalogueTimeout = -time.Second },
		"negative poll timeout":      func(c *scheduler.Config) { c.PollTimeout = -time.Second },
		"negative shutdown grace":    func(c *scheduler.Config) { c.ShutdownGrace = -time.Second },
		"negative jitter":            func(c *scheduler.Config) { c.JitterFraction = -0.1 },
		"jitter at one":              func(c *scheduler.Config) { c.JitterFraction = 1 },
		"invalid discovery window":   func(c *scheduler.Config) { c.DiscoveryWindow = scheduler.Window(200) },
		"nil clock":                  func(c *scheduler.Config) { c.Now = nil },
		"bad tiers":                  func(c *scheduler.Config) { c.Tiers.Live.Interval = 0 },
		"bad boundaries":             func(c *scheduler.Config) { c.Boundaries.NearTip = 0 },
		"bad quota":                  func(c *scheduler.Config) { c.Quota.Budget = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := recommendedConfig()
			cfg.Now = time.Now
			mutate(&cfg)
			if err := cfg.Validate(); !errors.Is(err, scheduler.ErrInvalidConfig) {
				t.Errorf("Validate() = %v, want an error wrapping ErrInvalidConfig", err)
			}
		})
	}
}

// TestRecommendedConfigValidates is the positive control for the table above: if
// the untouched configuration were already invalid, every case would "pass" for
// the wrong reason.
func TestRecommendedConfigValidates(t *testing.T) {
	t.Parallel()

	cfg := recommendedConfig()
	cfg.Now = time.Now
	if err := cfg.Validate(); err != nil {
		t.Fatalf("recommendedConfig().Validate(): %v", err)
	}
}

// TestZeroCreditsPerSweepIsLegitimate: the synthetic generator costs nothing, so
// the limiter stays unconditionally in the code path for both adapters rather
// than having an off switch only the offline build exercises.
func TestZeroCreditsPerSweepIsLegitimate(t *testing.T) {
	t.Parallel()

	cfg := scheduler.DefaultConfig("synthetic")
	cfg.Now = time.Now
	cfg.CreditsPerSweep = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a zero-cost adapter must be a valid configuration: %v", err)
	}
}
