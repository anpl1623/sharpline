// The running scheduler: adaptive cadence, quota refusal, bounded concurrency
// and a clean shutdown.
//
// # Real timers, tiny intervals
//
// Config.Now is injectable and the classification tests use a frozen clock, but
// the RUN loop sleeps on real time.Timers. A frozen clock here would mean
// asserting that the scheduler hangs. So these tests compress the cadence ladder
// into milliseconds and assert on observable state — Snapshot(), the recorded
// requests, the registered collectors — rather than on wall-clock durations,
// which is what keeps them from being flaky on a loaded CI runner.
//
// JitterFraction is set to a tiny positive value rather than zero: zero would be
// replaced by the package default in withDefaults, and setting it to 1e-9 keeps
// the jitter CODE PATH live while making its contribution unobservable. That is
// the mode Config.JitterFraction's doc comment describes.
package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/scheduler"
)

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

func testLogger() *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runConfig is the compressed cadence ladder every running test uses. The
// ordering constraint Tiers.Validate enforces is preserved, so this is a
// configuration the shipped validator accepts rather than a special case.
func runConfig() scheduler.Config {
	cfg := scheduler.DefaultConfig("synthetic")
	cfg.CreditsPerSweep = 0 // the synthetic adapter is free; the limiter stays in the path
	cfg.Tiers = scheduler.Tiers{
		Live:    scheduler.Tier{Interval: 10 * time.Millisecond, MaxInterval: 10 * time.Millisecond},
		NearTip: scheduler.Tier{Interval: 20 * time.Millisecond, MaxInterval: 160 * time.Millisecond},
		Today:   scheduler.Tier{Interval: 30 * time.Millisecond, MaxInterval: 240 * time.Millisecond},
		Distant: scheduler.Tier{Interval: 40 * time.Millisecond, MaxInterval: 320 * time.Millisecond},
		Futures: scheduler.Tier{Interval: 50 * time.Millisecond, MaxInterval: 400 * time.Millisecond},
	}
	cfg.CatalogueRefresh = time.Hour // one refresh at startup unless a test asks for more
	cfg.CatalogueTimeout = 5 * time.Second
	cfg.PollTimeout = 5 * time.Second
	cfg.ShutdownGrace = 50 * time.Millisecond
	cfg.JitterFraction = 1e-9
	cfg.Seed = 1
	return cfg
}

// answerFunc decides what one sweep returns. n is the 1-based call ordinal
// across the whole recorder, so a test can change its mind on a later sweep
// without a second synchronisation primitive.
type answerFunc func(ctx context.Context, n int, req scheduler.PollRequest) (scheduler.PollResult, error)

// recorder is the test's Poller. It records every request, tracks in-flight
// concurrency for the semaphore assertion, and delegates the answer.
type recorder struct {
	mu          sync.Mutex
	reqs        []scheduler.PollRequest
	inFlight    int
	maxInFlight int
	answer      answerFunc
}

func newRecorder(answer answerFunc) *recorder { return &recorder{answer: answer} }

// unchanged is the steady state CLAUDE.md §5 says a healthy pipeline is mostly
// in: markets seen, none of them moved.
func unchanged(context.Context, int, scheduler.PollRequest) (scheduler.PollResult, error) {
	return scheduler.PollResult{Markets: 12, Changed: 0}.WithoutQuota(), nil
}

// changed is a sweep that found movement, which is §5's backoff reset condition.
func changed(context.Context, int, scheduler.PollRequest) (scheduler.PollResult, error) {
	return scheduler.PollResult{Markets: 12, Changed: 4}.WithoutQuota(), nil
}

func (r *recorder) Poll(ctx context.Context, req scheduler.PollRequest) (scheduler.PollResult, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	n := len(r.reqs)
	r.inFlight++
	if r.inFlight > r.maxInFlight {
		r.maxInFlight = r.inFlight
	}
	answer := r.answer
	r.mu.Unlock()

	res, err := answer(ctx, n, req)

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return res, err
}

func (r *recorder) setAnswer(a answerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer = a
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *recorder) requests() []scheduler.PollRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]scheduler.PollRequest(nil), r.reqs...)
}

func (r *recorder) peakConcurrency() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxInFlight
}

// plans is a mutable catalogue: a test swaps the schedule to exercise a
// promotion or a retirement.
type plans struct {
	mu   sync.Mutex
	next func(n int) ([]scheduler.LeaguePlan, error)
	runs int
}

func (p *plans) Schedule(context.Context) ([]scheduler.LeaguePlan, error) {
	p.mu.Lock()
	p.runs++
	n := p.runs
	next := p.next
	p.mu.Unlock()
	return next(n)
}

func (p *plans) set(next func(n int) ([]scheduler.LeaguePlan, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next = next
}

func staticPlans(ps ...scheduler.LeaguePlan) *plans {
	return &plans{next: func(int) ([]scheduler.LeaguePlan, error) { return ps, nil }}
}

// running is a started scheduler plus the means to stop it exactly once.
type running struct {
	*scheduler.Scheduler
	reg    *prometheus.Registry
	cancel context.CancelFunc
	errCh  chan error
	once   sync.Once
	err    error
}

// stop cancels the run context and waits for Run to return, which is the
// assertion that shutdown terminates rather than merely that it was requested.
func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.once.Do(func() {
		r.cancel()
		select {
		case r.err = <-r.errCh:
		case <-time.After(15 * time.Second):
			r.err = errors.New("Scheduler.Run did not return within 15s of cancellation")
		}
	})
	return r.err
}

// start builds and runs a scheduler, registering a cleanup that stops it.
func start(t *testing.T, cfg scheduler.Config, p scheduler.Poller, c scheduler.Catalogue,
	mutate ...func(*scheduler.Options),
) *running {
	t.Helper()

	reg := prometheus.NewRegistry()
	m, err := scheduler.NewMetrics(reg)
	if err != nil {
		t.Fatalf("build scheduler metrics: %v", err)
	}

	opts := scheduler.Options{
		Config:    cfg,
		Poller:    p,
		Catalogue: c,
		Logger:    testLogger(),
		Metrics:   m,
	}
	for _, fn := range mutate {
		fn(&opts)
	}

	s, err := scheduler.New(opts)
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &running{Scheduler: s, reg: reg, cancel: cancel, errCh: make(chan error, 1)}
	go func() { r.errCh <- s.Run(ctx) }()

	t.Cleanup(func() {
		if err := r.stop(t); err != nil {
			t.Errorf("Scheduler.Run returned %v on shutdown, want nil", err)
		}
	})
	return r
}

// waitFor polls cond until it holds or the deadline passes. Polling rather than
// signalling because the state under test is updated by the scheduler's own
// goroutines after a sweep returns, so there is no edge the test can subscribe
// to without reaching inside the package.
func waitFor(t *testing.T, timeout time.Duration, describe string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, describe)
		}
		time.Sleep(time.Millisecond)
	}
}

// leagueState returns one league's scheduling state, or false if it is not
// scheduled.
func leagueState(s *scheduler.Scheduler, league domain.LeagueID) (scheduler.LeagueState, bool) {
	for _, st := range s.Snapshot() {
		if st.League == league {
			return st, true
		}
	}
	return scheduler.LeagueState{}, false
}

// -----------------------------------------------------------------------------
// Metric readers. Small and local: the assertions here are about a handful of
// series, and importing a matcher library for them would be more machinery than
// the thing being measured.
// -----------------------------------------------------------------------------

func sampleValue(m *dto.Metric) float64 {
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	default:
		return 0
	}
}

// seriesValue sums every sample of `name` whose labels are a superset of want.
func seriesValue(t *testing.T, g prometheus.Gatherer, name string, want map[string]string) float64 {
	t.Helper()
	total := 0.0
	for _, m := range samples(t, g, name, want) {
		total += sampleValue(m)
	}
	return total
}

// samples returns every sample of `name` whose labels are a superset of want.
func samples(t *testing.T, g prometheus.Gatherer, name string, want map[string]string) []*dto.Metric {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []*dto.Metric
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			match := true
			for k, v := range want {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				out = append(out, m)
			}
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// Construction
// -----------------------------------------------------------------------------

func TestNewRejectsMissingDependencies(t *testing.T) {
	t.Parallel()

	base := func() scheduler.Options {
		return scheduler.Options{
			Config:    runConfig(),
			Poller:    newRecorder(unchanged),
			Catalogue: staticPlans(),
			Logger:    testLogger(),
		}
	}

	cases := map[string]func(*scheduler.Options){
		"no poller":               func(o *scheduler.Options) { o.Poller = nil },
		"no catalogue":            func(o *scheduler.Options) { o.Catalogue = nil },
		"no logger":               func(o *scheduler.Options) { o.Logger = nil },
		"negative error backoff":  func(o *scheduler.Options) { o.ErrorBackoffMax = -time.Second },
		"invalid config":          func(o *scheduler.Options) { o.Config.MaxConcurrentPolls = -1 },
		"invalid quota":           func(o *scheduler.Options) { o.Config.Quota = scheduler.Quota{Budget: -1} },
		"unaffordable sweep cost": func(o *scheduler.Options) { o.Config.CreditsPerSweep = 1 << 30 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := base()
			mutate(&opts)
			s, err := scheduler.New(opts)
			if !errors.Is(err, scheduler.ErrInvalidConfig) {
				t.Fatalf("New = (%v, %v), want an error wrapping ErrInvalidConfig", s, err)
			}
			if s != nil {
				t.Error("New returned a scheduler alongside an error")
			}
		})
	}
}

// TestNewResolvesEveryDefault: a Config carrying only the provider name must be
// buildable, because that is what withDefaults is for and what Config.Validate's
// doc comment promises.
func TestNewResolvesEveryDefault(t *testing.T) {
	t.Parallel()

	s, err := scheduler.New(scheduler.Options{
		Config:    scheduler.Config{Provider: "synthetic"},
		Poller:    newRecorder(unchanged),
		Catalogue: staticPlans(),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("New with a bare Config: %v", err)
	}
	if got := s.Budget().Limit(); got != scheduler.DefaultQuotaBudget {
		t.Errorf("Budget().Limit() = %d, want the default budget %d", got, scheduler.DefaultQuotaBudget)
	}
	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot() on a scheduler that has never run = %v, want empty", got)
	}
}

// -----------------------------------------------------------------------------
// Scheduling
// -----------------------------------------------------------------------------

// TestEveryLeagueInTheCatalogueIsPolledImmediately covers the startup property
// nextDelay implements: a league's FIRST sweep fires as soon as possible
// whatever its window, so the board populates on startup instead of an hour
// later.
func TestEveryLeagueInTheCatalogueIsPolledImmediately(t *testing.T) {
	t.Parallel()

	// The slowest tier in the ladder — 50ms here, six hours in production. If
	// the first sweep waited for the interval this test would time out.
	futures := newOutright(t, "nba-fut", domain.EventStatusScheduled, testNow.Add(90*24*time.Hour))

	rec := newRecorder(unchanged)
	r := start(t, runConfig(), rec, staticPlans(
		scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{futures}},
		scheduler.LeaguePlan{League: "league-b", Events: []domain.Event{futures}},
	))

	waitFor(t, 5*time.Second, "both leagues to be swept", func() bool {
		seen := map[domain.LeagueID]bool{}
		for _, req := range rec.requests() {
			seen[req.League] = true
		}
		return seen["league-a"] && seen["league-b"]
	})

	states := r.Snapshot()
	if len(states) != 2 {
		t.Fatalf("Snapshot() has %d leagues, want 2", len(states))
	}
	// Snapshot is documented as sorted by league id so its output is stable.
	if !sort.SliceIsSorted(states, func(i, j int) bool { return states[i].League < states[j].League }) {
		t.Errorf("Snapshot() is not sorted by league id: %v", states)
	}
}

// TestSweepCarriesTheFoldedWindowAndTheEventList. The Window on the request is
// how a log line explains itself and how an adapter may narrow what it asks for,
// and it must be the fold over the league's events, not the first one's.
func TestSweepCarriesTheFoldedWindowAndTheEventList(t *testing.T) {
	t.Parallel()

	events := []domain.Event{
		newMatch(t, "distant", domain.EventStatusScheduled, time.Now().Add(5*24*time.Hour)),
		newMatch(t, "inplay", domain.EventStatusLive, time.Now().Add(-time.Hour)),
		// Settled: pollableEvents must drop it before the request is built.
		newMatch(t, "done", domain.EventStatusSettled, time.Now().Add(-5*time.Hour)),
	}

	rec := newRecorder(unchanged)
	start(t, runConfig(), rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: events}))

	waitFor(t, 5*time.Second, "the first sweep", func() bool { return rec.count() > 0 })

	req := rec.requests()[0]
	if req.Window != scheduler.WindowLive {
		t.Errorf("PollRequest.Window = %s, want live — the fold is a minimum over urgency", req.Window)
	}
	if len(req.Events) != 2 {
		t.Errorf("PollRequest carried %d events, want 2 (the settled fixture must be filtered out)",
			len(req.Events))
	}
	for _, e := range req.Events {
		if !e.AcceptsWagers() {
			t.Errorf("PollRequest carried %s, whose status does not accept wagers", e.ID())
		}
	}
	if req.ScheduledAt.IsZero() {
		t.Error("PollRequest.ScheduledAt is zero; it is the subtrahend for the quota-wait histogram")
	}
	if req.Attempt != 0 {
		t.Errorf("PollRequest.Attempt = %d on the first sweep, want 0", req.Attempt)
	}
}

// TestLeagueWithNoKnownEventsIsScheduledAtTheDiscoveryCadence. A cold start
// knows WHICH leagues exist and nothing about their fixtures — Catalogue's doc
// comment calls that state supported, and dropping such a league would mean its
// fixtures were never discovered.
func TestLeagueWithNoKnownEventsIsScheduledAtTheDiscoveryCadence(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-cold"}))

	waitFor(t, 5*time.Second, "the cold league to be swept", func() bool { return rec.count() > 0 })

	st, ok := leagueState(r.Scheduler, "league-cold")
	if !ok {
		t.Fatal("a league with no known events is not in the schedule; it must be polled slowly, not dropped")
	}
	if st.Window != scheduler.DefaultDiscoveryWindow {
		t.Errorf("window = %s, want the discovery window %s", st.Window, scheduler.DefaultDiscoveryWindow)
	}
	if st.Events != 0 {
		t.Errorf("Events = %d, want 0", st.Events)
	}
}

// TestMalformedPlansAreDropped: a plan with no league id, and a duplicate
// league, are both survivable and neither may produce a runner.
func TestMalformedPlansAreDropped(t *testing.T) {
	t.Parallel()

	rec := newRecorder(unchanged)
	r := start(t, runConfig(), rec, staticPlans(
		scheduler.LeaguePlan{League: ""},
		scheduler.LeaguePlan{League: "league-a"},
		scheduler.LeaguePlan{League: "league-a"},
	))

	waitFor(t, 5*time.Second, "the valid league to be swept", func() bool { return rec.count() > 0 })

	if got := r.Snapshot(); len(got) != 1 {
		t.Fatalf("Snapshot() has %d leagues, want 1: %v", len(got), got)
	}
}

// -----------------------------------------------------------------------------
// Adaptive backoff — CLAUDE.md §5's "backing off on unchanged payloads"
// -----------------------------------------------------------------------------

// TestBackoffDoublesOnUnchangedAndIsCappedByTheTierCeiling.
//
// The ceiling is the half that matters operationally: without it a market that
// goes quiet overnight would be polled at an interval that doubles for ever and
// would take hours to notice that it moved.
func TestBackoffDoublesOnUnchangedAndIsCappedByTheTierCeiling(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	// Near-tip: base 20ms, ceiling 160ms — exactly three doublings.
	nearTip := newMatch(t, "tip", domain.EventStatusScheduled, time.Now().Add(5*time.Minute))

	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{nearTip}}))

	want := map[int]time.Duration{
		1: 40 * time.Millisecond,
		2: 80 * time.Millisecond,
		3: 160 * time.Millisecond,
		4: 160 * time.Millisecond, // clamped
	}
	for _, quiet := range []int{1, 2, 3, 4} {
		waitFor(t, 10*time.Second, fmt.Sprintf("%d quiet sweeps", quiet), func() bool {
			st, ok := leagueState(r.Scheduler, "league-a")
			return ok && st.QuietSweeps >= quiet
		})
		st, _ := leagueState(r.Scheduler, "league-a")
		if st.QuietSweeps != quiet {
			// The scheduler ran ahead of the assertion; the clamp case below is
			// still meaningful, so only the exact-step checks are skipped.
			continue
		}
		if st.Interval != want[quiet] {
			t.Errorf("after %d unchanged sweeps the interval is %s, want %s",
				quiet, st.Interval, want[quiet])
		}
	}

	// Whatever the sampling caught above, the ceiling must hold.
	waitFor(t, 10*time.Second, "the backoff ceiling", func() bool {
		st, ok := leagueState(r.Scheduler, "league-a")
		return ok && st.QuietSweeps >= 5
	})
	st, _ := leagueState(r.Scheduler, "league-a")
	if st.Interval != cfg.Tiers.NearTip.MaxInterval {
		t.Errorf("interval = %s after %d quiet sweeps, want the tier ceiling %s",
			st.Interval, st.QuietSweeps, cfg.Tiers.NearTip.MaxInterval)
	}
}

// TestAnyChangedMarketResetsBackoffImmediately is §5's reset condition. Recovery
// is immediate rather than gradual: a market that has just started moving must
// be back at full cadence on the very next sweep.
func TestAnyChangedMarketResetsBackoffImmediately(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	nearTip := newMatch(t, "tip", domain.EventStatusScheduled, time.Now().Add(5*time.Minute))

	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{nearTip}}))

	waitFor(t, 10*time.Second, "the league to back off", func() bool {
		st, ok := leagueState(r.Scheduler, "league-a")
		return ok && st.QuietSweeps >= 3
	})

	rec.setAnswer(changed)

	waitFor(t, 10*time.Second, "backoff to reset", func() bool {
		st, ok := leagueState(r.Scheduler, "league-a")
		return ok && st.QuietSweeps == 0
	})
	st, _ := leagueState(r.Scheduler, "league-a")
	if st.Interval != cfg.Tiers.NearTip.Interval {
		t.Errorf("interval = %s after a changed sweep, want the base cadence %s",
			st.Interval, cfg.Tiers.NearTip.Interval)
	}
}

// TestLiveTierNeverBacksOff pins the property doc.go argues for: the live
// tier's ceiling equals its interval, so the only thing live backoff could do is
// be wrong at the worst possible moment — when a line sits still for three polls
// and then steams.
func TestLiveTierNeverBacksOff(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))

	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{live}}))

	waitFor(t, 10*time.Second, "several quiet live sweeps", func() bool {
		st, ok := leagueState(r.Scheduler, "league-a")
		return ok && st.QuietSweeps >= 5
	})

	st, _ := leagueState(r.Scheduler, "league-a")
	if st.Window != scheduler.WindowLive {
		t.Fatalf("window = %s, want live", st.Window)
	}
	if st.Interval != cfg.Tiers.Live.Interval {
		t.Errorf("interval = %s after %d quiet live sweeps, want the unchanged live cadence %s",
			st.Interval, st.QuietSweeps, cfg.Tiers.Live.Interval)
	}
}

// TestPromotionToAMoreUrgentWindowClearsBackoff.
//
// A league that has just gone near-tip is not quiet — it is about to be the
// busiest thing on the board. Carrying a backoff multiplier across the promotion
// would mean the first urgent poll came a multiple of the cadence late, which is
// exactly when staleness is most visible.
//
// The assertion is a MINIMUM over samples rather than a single reading, because
// the reset is transient: the very next unchanged sweep starts the multiplier
// climbing again. Without the reset the observed interval could never fall below
// the near-tip ceiling (160ms); with it, the base (20ms) and one doubling (40ms)
// are both observable.
func TestPromotionToAMoreUrgentWindowClearsBackoff(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	cfg.CatalogueRefresh = 5 * time.Millisecond

	today := newMatch(t, "today", domain.EventStatusScheduled, time.Now().Add(4*time.Hour))
	soon := newMatch(t, "soon", domain.EventStatusScheduled, time.Now().Add(5*time.Minute))

	cat := &plans{}
	cat.set(func(int) ([]scheduler.LeaguePlan, error) {
		return []scheduler.LeaguePlan{{League: "league-a", Events: []domain.Event{today}}}, nil
	})

	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, cat)

	waitFor(t, 10*time.Second, "the today-window league to back off to its ceiling", func() bool {
		st, ok := leagueState(r.Scheduler, "league-a")
		return ok && st.Interval == cfg.Tiers.Today.MaxInterval
	})

	// Kickoff approaches.
	cat.set(func(int) ([]scheduler.LeaguePlan, error) {
		return []scheduler.LeaguePlan{{League: "league-a", Events: []domain.Event{soon}}}, nil
	})

	waitFor(t, 10*time.Second, "promotion to the near-tip window", func() bool {
		st, ok := leagueState(r.Scheduler, "league-a")
		return ok && st.Window == scheduler.WindowNearTip
	})

	minInterval := time.Duration(1<<62 - 1)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := leagueState(r.Scheduler, "league-a"); ok && st.Interval < minInterval {
			minInterval = st.Interval
		}
		if minInterval <= cfg.Tiers.NearTip.Interval {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Had the multiplier survived the promotion, the interval could only ever
	// have been the near-tip CEILING: quiet was already past three doublings, and
	// nothing in this test ever reports a changed market to bring it back down.
	if minInterval >= cfg.Tiers.NearTip.MaxInterval {
		t.Errorf("the shortest interval observed after promotion was %s, never below the near-tip "+
			"ceiling %s; the backoff multiplier was carried across the promotion instead of being cleared",
			minInterval, cfg.Tiers.NearTip.MaxInterval)
	}
}

// TestFailureBackoffIsSeparateFromQuietBackoff.
//
// The two answer different questions — "nothing is moving" versus "the provider
// is not answering" — and they are maxed rather than added, so a live tier can
// disable the first while still needing the second. Hammering a provider that is
// returning 429s at the live cadence is how a rate limit becomes a ban.
func TestFailureBackoffIsSeparateFromQuietBackoff(t *testing.T) {
	t.Parallel()

	const errCap = 100 * time.Millisecond

	cfg := runConfig()
	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))

	boom := errors.New("provider is down")
	rec := newRecorder(func(context.Context, int, scheduler.PollRequest) (scheduler.PollResult, error) {
		return scheduler.PollResult{}.WithoutQuota(), boom
	})

	r := start(t, cfg, rec,
		staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{live}}),
		func(o *scheduler.Options) { o.ErrorBackoffMax = errCap })

	waitFor(t, 10*time.Second, "four consecutive failures", func() bool {
		st, ok := leagueState(r.Scheduler, "league-a")
		return ok && st.Failures >= 4
	})

	st, _ := leagueState(r.Scheduler, "league-a")
	// The live tier's own ceiling is 10ms, so an interval above it can only come
	// from the failure path.
	if st.Interval != errCap {
		t.Errorf("interval = %s after %d failures, want the error backoff ceiling %s "+
			"(the live tier's own ceiling is %s, so quiet backoff cannot produce this)",
			st.Interval, st.Failures, errCap, cfg.Tiers.Live.MaxInterval)
	}
	if st.QuietSweeps != 0 {
		t.Errorf("QuietSweeps = %d after only failures; a failed sweep is not a quiet one", st.QuietSweeps)
	}

	// Attempt counts the consecutive failed sweeps that preceded this one.
	reqs := rec.requests()
	for i, req := range reqs {
		if i >= 4 {
			break
		}
		if req.Attempt != i {
			t.Errorf("request %d carried Attempt = %d, want %d", i, req.Attempt, i)
		}
	}

	// A single success clears it.
	rec.setAnswer(changed)
	waitFor(t, 10*time.Second, "the failure count to clear", func() bool {
		st, ok := leagueState(r.Scheduler, "league-a")
		return ok && st.Failures == 0
	})
}

// -----------------------------------------------------------------------------
// Quota
// -----------------------------------------------------------------------------

// TestQuotaExhaustionRefusesTheSweep is ADR 0003's degraded state: a frozen
// board plus a firing alert, never a silent failover to synthetic prices.
func TestQuotaExhaustionRefusesTheSweep(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	cfg.CreditsPerSweep = 3
	cfg.Quota = scheduler.Quota{Budget: 3, Period: time.Hour, Burst: 3} // exactly one sweep
	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))

	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{live}}))

	waitFor(t, 5*time.Second, "the one affordable sweep", func() bool { return rec.count() >= 1 })
	waitFor(t, 5*time.Second, "a refused sweep", func() bool {
		return seriesValue(t, r.reg, "sharpline_ingest_quota_blocked_total",
			map[string]string{"provider": "synthetic", "league": "league-a"}) > 0
	})

	// The live cadence is 10ms, so several sweeps' worth of time passes here.
	time.Sleep(200 * time.Millisecond)
	if got := rec.count(); got != 1 {
		t.Errorf("the poller was called %d times against a one-sweep budget, want 1", got)
	}
	if got := r.Budget().Remaining(); got != 0 {
		t.Errorf("Budget().Remaining() = %d, want 0", got)
	}
	if got := seriesValue(t, r.reg, "sharpline_ingest_budget_remaining",
		map[string]string{"provider": "synthetic"}); got != 0 {
		t.Errorf("sharpline_ingest_budget_remaining = %v, want 0", got)
	}
}

// TestProviderReportedQuotaIsReconciled is ADR 0003 implementation requirement
// #3 reaching the shared limiter: the local count drifts, the provider's header
// does not.
func TestProviderReportedQuotaIsReconciled(t *testing.T) {
	t.Parallel()

	const reported = 4242

	cfg := runConfig()
	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))

	rec := newRecorder(func(context.Context, int, scheduler.PollRequest) (scheduler.PollResult, error) {
		return scheduler.PollResult{Markets: 3, Changed: 1, QuotaRemaining: reported}, nil
	})
	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{live}}))

	waitFor(t, 5*time.Second, "the provider's own remaining count to be adopted", func() bool {
		return r.Budget().Remaining() == reported
	})
	waitFor(t, 5*time.Second, "the gauge to follow it", func() bool {
		return seriesValue(t, r.reg, "sharpline_ingest_budget_remaining",
			map[string]string{"provider": "synthetic"}) == reported
	})
}

// TestNegativeQuotaRemainingIsNotAdopted: -1 is how an adapter says "the
// provider did not say", and the synthetic generator says it on every sweep.
// Adopting it as zero would freeze the offline board.
func TestNegativeQuotaRemainingIsNotAdopted(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	cfg.CreditsPerSweep = 0
	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))

	rec := newRecorder(unchanged) // WithoutQuota sets QuotaRemaining to -1
	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{live}}))

	waitFor(t, 5*time.Second, "several sweeps", func() bool { return rec.count() >= 3 })

	if got, want := r.Budget().Remaining(), cfg.Quota.Budget; got != want {
		t.Errorf("Budget().Remaining() = %d after sweeps reporting no quota, want the untouched budget %d",
			got, want)
	}
}

// -----------------------------------------------------------------------------
// Concurrency and shutdown
// -----------------------------------------------------------------------------

// TestConcurrentPollsAreBounded: a fifty-league slate must not open fifty
// simultaneous provider connections. ADR 0003 records that a per-second rate
// limit is not verified to be absent, so this bound is also the only thing
// standing between a large slate and an unannounced 429.
func TestConcurrentPollsAreBounded(t *testing.T) {
	t.Parallel()

	const (
		limit   = 2
		leagues = 8
	)

	cfg := runConfig()
	cfg.MaxConcurrentPolls = limit

	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))
	ps := make([]scheduler.LeaguePlan, 0, leagues)
	for i := 0; i < leagues; i++ {
		ps = append(ps, scheduler.LeaguePlan{
			League: domain.LeagueID(fmt.Sprintf("league-%02d", i)),
			Events: []domain.Event{live},
		})
	}

	rec := newRecorder(func(ctx context.Context, _ int, _ scheduler.PollRequest) (scheduler.PollResult, error) {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return scheduler.PollResult{}.WithoutQuota(), ctx.Err()
		}
		return scheduler.PollResult{Markets: 1, Changed: 1}.WithoutQuota(), nil
	})
	start(t, cfg, rec, staticPlans(ps...))

	waitFor(t, 10*time.Second, "every league to be swept at least once", func() bool {
		seen := map[domain.LeagueID]bool{}
		for _, req := range rec.requests() {
			seen[req.League] = true
		}
		return len(seen) == leagues
	})

	if peak := rec.peakConcurrency(); peak > limit {
		t.Errorf("peak in-flight sweeps = %d, MaxConcurrentPolls = %d", peak, limit)
	}
	if peak := rec.peakConcurrency(); peak < 2 {
		t.Errorf("peak in-flight sweeps = %d across %d leagues; the sweeps serialised, so the bound "+
			"was not what limited them and this test proves nothing", peak, leagues)
	}
}

// TestPollTimeoutBoundsOneSweep. CLAUDE.md §12: every external call has a
// timeout. A sweep that could block for ever would permanently occupy a
// concurrency slot.
func TestPollTimeoutBoundsOneSweep(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	cfg.PollTimeout = 30 * time.Millisecond

	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))

	deadlines := make(chan error, 4)
	rec := newRecorder(func(ctx context.Context, _ int, _ scheduler.PollRequest) (scheduler.PollResult, error) {
		<-ctx.Done()
		select {
		case deadlines <- ctx.Err():
		default:
		}
		return scheduler.PollResult{}.WithoutQuota(), ctx.Err()
	})
	start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{live}}))

	select {
	case err := <-deadlines:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the sweep context ended with %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a sweep that never returns was not bounded by PollTimeout")
	}
}

// TestShutdownGraceLetsAnInFlightSweepFinish.
//
// A sweep that has already issued its provider request has already spent the
// credit. Severing it the instant SIGTERM lands throws that away and loses a
// payload the system could have published.
func TestShutdownGraceLetsAnInFlightSweepFinish(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	cfg.ShutdownGrace = 2 * time.Second

	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))

	entered := make(chan struct{}, 1)
	completed := make(chan bool, 1)
	rec := newRecorder(func(ctx context.Context, n int, _ scheduler.PollRequest) (scheduler.PollResult, error) {
		if n != 1 {
			return scheduler.PollResult{Markets: 1, Changed: 1}.WithoutQuota(), nil
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-time.After(150 * time.Millisecond):
			select {
			case completed <- true:
			default:
			}
			return scheduler.PollResult{Markets: 1, Changed: 1}.WithoutQuota(), nil
		case <-ctx.Done():
			select {
			case completed <- false:
			default:
			}
			return scheduler.PollResult{}.WithoutQuota(), ctx.Err()
		}
	})

	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{live}}))

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first sweep never started")
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("Run returned %v on shutdown, want nil", err)
	}

	select {
	case ok := <-completed:
		if !ok {
			t.Error("the in-flight sweep was severed by cancellation; ShutdownGrace must let it finish, " +
				"because its provider credit is already spent")
		}
	case <-time.After(time.Second):
		t.Fatal("the in-flight sweep neither completed nor was cancelled")
	}
}

// TestRunRefusesASecondConcurrentRun. Two Runs would double every league's
// cadence and double the credit burn.
func TestRunRefusesASecondConcurrentRun(t *testing.T) {
	t.Parallel()

	rec := newRecorder(unchanged)
	r := start(t, runConfig(), rec, staticPlans(scheduler.LeaguePlan{League: "league-a"}))

	waitFor(t, 5*time.Second, "the first Run to start", func() bool { return len(r.Snapshot()) == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Run(ctx); !errors.Is(err, scheduler.ErrAlreadyRunning) {
		t.Fatalf("a second Run returned %v, want ErrAlreadyRunning", err)
	}
}

// TestRunIsRestartableAfterAStop: ErrAlreadyRunning must be about CONCURRENCY,
// not a one-shot latch, or a supervisor could never restart the schedule.
func TestRunIsRestartableAfterAStop(t *testing.T) {
	t.Parallel()

	rec := newRecorder(unchanged)
	r := start(t, runConfig(), rec, staticPlans(scheduler.LeaguePlan{League: "league-a"}))

	waitFor(t, 5*time.Second, "the first sweep", func() bool { return rec.count() > 0 })
	if err := r.stop(t); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	before := rec.count()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitFor(t, 5*time.Second, "the restarted schedule to sweep", func() bool { return rec.count() > before })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("the restarted Run returned %v, want nil", err)
	}
}

// TestSchedulerLeaksNoGoroutines is the test doc.go names.
//
// It is the assertion that matters most in this package: one goroutine per
// league is the shape CLAUDE.md §2 chose Go for, and a scheduler that leaked one
// per catalogue refresh would look perfectly healthy until a long-running
// process ran out of memory. `make test-race` runs it with the detector on.
func TestSchedulerLeaksNoGoroutines(t *testing.T) {
	// Deliberately NOT parallel: the measurement is a process-wide goroutine
	// count, and a sibling test starting one would be indistinguishable from a
	// leak here.
	cfg := runConfig()
	cfg.CatalogueRefresh = 5 * time.Millisecond
	cfg.ShutdownGrace = 20 * time.Millisecond

	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))
	ps := make([]scheduler.LeaguePlan, 0, 6)
	for i := 0; i < 6; i++ {
		ps = append(ps, scheduler.LeaguePlan{
			League: domain.LeagueID(fmt.Sprintf("league-%02d", i)),
			Events: []domain.Event{live},
		})
	}

	// Let anything a previous test left mid-teardown settle before the baseline
	// is taken, so the baseline is not inflated.
	settle(t)
	before := runtime.NumGoroutine()

	rec := newRecorder(func(ctx context.Context, _ int, _ scheduler.PollRequest) (scheduler.PollResult, error) {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return scheduler.PollResult{}.WithoutQuota(), ctx.Err()
		}
		return scheduler.PollResult{Markets: 2, Changed: 1}.WithoutQuota(), nil
	})

	s, err := scheduler.New(scheduler.Options{
		Config:    cfg,
		Poller:    rec,
		Catalogue: staticPlans(ps...),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, 10*time.Second, "every league to be swept", func() bool {
		seen := map[domain.LeagueID]bool{}
		for _, req := range rec.requests() {
			seen[req.League] = true
		}
		return len(seen) == len(ps)
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on shutdown, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s of cancellation")
	}

	settle(t)
	after := runtime.NumGoroutine()
	if after > before {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutines: %d before, %d after a full run of %d leagues. Every league runner, the "+
			"planner and every shutdown-grace watcher must have exited.\n%s", before, after, len(ps), buf[:n])
	}
}

// settle waits for the goroutine count to stop falling, so a count taken
// immediately after a shutdown is not measuring goroutines that are already on
// their way out.
func settle(t *testing.T) {
	t.Helper()
	prev := runtime.NumGoroutine()
	deadline := time.Now().Add(5 * time.Second)
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		runtime.Gosched()
		n := runtime.NumGoroutine()
		if n == prev {
			if stable++; stable >= 3 {
				return
			}
			continue
		}
		prev, stable = n, 0
	}
}

// -----------------------------------------------------------------------------
// Catalogue lifecycle and the metric contract
// -----------------------------------------------------------------------------

// TestCatalogueFailureIsSurvived: ingest must survive a provider blip, because a
// crash-looping ingest turns a five-minute provider outage into a five-minute
// outage plus a cold start.
func TestCatalogueFailureIsSurvived(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	cfg.CatalogueRefresh = 5 * time.Millisecond

	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))
	cat := &plans{next: func(n int) ([]scheduler.LeaguePlan, error) {
		if n <= 2 {
			return nil, errors.New("catalogue endpoint is down")
		}
		return []scheduler.LeaguePlan{{League: "league-a", Events: []domain.Event{live}}}, nil
	}}

	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, cat)

	waitFor(t, 10*time.Second, "the schedule to recover after a catalogue outage", func() bool {
		return rec.count() > 0
	})
	if got := seriesValue(t, r.reg, "sharpline_ingest_catalogue_refreshes_total",
		map[string]string{"provider": "synthetic", "outcome": "error"}); got < 2 {
		t.Errorf("sharpline_ingest_catalogue_refreshes_total{outcome=\"error\"} = %v, want at least 2", got)
	}
}

// TestLeagueLeavingTheCatalogueIsRetired.
//
// Leaving the cadence series behind would keep a settled league contributing to
// the max() that sets the headline SLO threshold for ever — a league that
// stopped being polled would go on defining how fresh the board is expected to
// be.
func TestLeagueLeavingTheCatalogueIsRetired(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	cfg.CatalogueRefresh = 5 * time.Millisecond

	live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))
	cat := &plans{}
	cat.set(func(int) ([]scheduler.LeaguePlan, error) {
		return []scheduler.LeaguePlan{{League: "league-a", Events: []domain.Event{live}}}, nil
	})

	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, cat)

	waitFor(t, 10*time.Second, "the league to be scheduled", func() bool {
		_, ok := leagueState(r.Scheduler, "league-a")
		return ok
	})
	if n := len(samples(t, r.reg, "sharpline_ingest_poll_interval_seconds",
		map[string]string{"league": "league-a"})); n != len(scheduler.Windows()) {
		t.Errorf("poll_interval_seconds has %d samples for the league, want one per window (%d)",
			n, len(scheduler.Windows()))
	}

	cat.set(func(int) ([]scheduler.LeaguePlan, error) { return nil, nil })

	waitFor(t, 10*time.Second, "the league to leave the schedule", func() bool {
		return len(r.Snapshot()) == 0
	})
	waitFor(t, 10*time.Second, "its cadence series to be retired", func() bool {
		return len(samples(t, r.reg, "sharpline_ingest_poll_interval_seconds",
			map[string]string{"league": "league-a"})) == 0
	})
}

// TestCadenceIsPublishedForEveryWindow is the SLO-threshold contract.
//
// The objective rule selects {window="live"} with max(), so publishing only the
// league's CURRENT window would make the headline threshold blink out of
// existence the moment no league happened to be in play — and
// OddsPollCadenceUnknown would then fire on a perfectly healthy pregame system.
func TestCadenceIsPublishedForEveryWindow(t *testing.T) {
	t.Parallel()

	cfg := runConfig()
	// A pregame-only slate: nothing is live, and the live series must exist
	// anyway.
	today := newMatch(t, "today", domain.EventStatusScheduled, time.Now().Add(4*time.Hour))

	rec := newRecorder(unchanged)
	r := start(t, cfg, rec, staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{today}}))

	waitFor(t, 5*time.Second, "the league to be scheduled", func() bool {
		_, ok := leagueState(r.Scheduler, "league-a")
		return ok
	})

	for _, w := range scheduler.Windows() {
		got := samples(t, r.reg, "sharpline_ingest_poll_interval_seconds",
			map[string]string{"provider": "synthetic", "league": "league-a", "window": w.String()})
		if len(got) != 1 {
			t.Errorf("sharpline_ingest_poll_interval_seconds{window=%q}: %d samples, want 1", w.String(), len(got))
			continue
		}
		tier, _ := cfg.Tiers.For(w)
		if v := sampleValue(got[0]); v != tier.Interval.Seconds() {
			t.Errorf("sharpline_ingest_poll_interval_seconds{window=%q} = %v, want the CONFIGURED "+
				"interval %v (not the backed-off one — the SLO threshold reads this series)",
				w.String(), v, tier.Interval.Seconds())
		}
	}
}

// TestPollOutcomesAreClassified pins the closed `result` label set the dashboard
// panel aggregates by. result="unchanged" is the one it singles out: a healthy
// pipeline is mostly unchanged, and a system where everything is "changed" means
// the change-detection hash is broken and the bus is carrying junk.
func TestPollOutcomesAreClassified(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer answerFunc
		want   string
	}{
		{"changed", changed, "changed"},
		{"unchanged", unchanged, "unchanged"},
		{
			"empty", func(context.Context, int, scheduler.PollRequest) (scheduler.PollResult, error) {
				return scheduler.PollResult{Markets: 0}.WithoutQuota(), nil
			}, "empty",
		},
		{
			"error", func(context.Context, int, scheduler.PollRequest) (scheduler.PollResult, error) {
				return scheduler.PollResult{}.WithoutQuota(), errors.New("boom")
			}, "error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			live := newMatch(t, "live", domain.EventStatusLive, time.Now().Add(-time.Hour))
			rec := newRecorder(tc.answer)
			r := start(t, runConfig(), rec,
				staticPlans(scheduler.LeaguePlan{League: "league-a", Events: []domain.Event{live}}))

			waitFor(t, 5*time.Second, "a classified sweep", func() bool {
				return seriesValue(t, r.reg, "sharpline_ingest_polls_total",
					map[string]string{"provider": "synthetic", "league": "league-a",
						"window": "live", "result": tc.want}) > 0
			})
		})
	}
}
