package scheduler

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// ErrAlreadyRunning is returned by a second call to [Scheduler.Run].
var ErrAlreadyRunning = errors.New("scheduler: already running")

// DefaultErrorBackoffMax caps the exponential backoff applied after consecutive
// failed sweeps. Five minutes, because a provider outage longer than that is an
// incident rather than a blip, and continuing to knock every five minutes is
// what makes recovery automatic without becoming a retry storm.
const DefaultErrorBackoffMax = 5 * time.Minute

// -----------------------------------------------------------------------------
// Consumer-declared seams (CLAUDE.md §12: "Interfaces are declared by the
// consumer, not the producer. Keep them small.")
// -----------------------------------------------------------------------------

// Poller performs one league sweep against the odds provider.
//
// It is the scheduler's half of CLAUDE.md §5's "each provider gets an adapter
// behind one interface": the real The Odds API adapter and the synthetic
// stochastic market-maker both satisfy it, and nothing here can tell which is
// running. That indistinguishability is the property that makes the offline
// path exercise the online path's code.
//
// # The contract an implementation must honour
//
//   - It fetches the current prices for req.League, normalizes them, and
//     publishes to odds.raw.{provider}. The scheduler does not touch the bus.
//   - It reports how many markets it saw and how many of those CHANGED. That
//     second number is the whole input to adaptive backoff, and CLAUDE.md §5's
//     "hash each normalized market to suppress no-op updates" is where it comes
//     from. Reporting every market as changed disables backoff; reporting none
//     as changed backs a live market off to its ceiling.
//   - It honours ctx, which carries [Config.PollTimeout]. It must NOT retry
//     internally past that budget: retries that outlive their deadline turn one
//     slow sweep into a permanently occupied concurrency slot.
//   - It is called concurrently for different leagues, up to
//     Config.MaxConcurrentPolls, and must be safe for that.
type Poller interface {
	Poll(ctx context.Context, req PollRequest) (PollResult, error)
}

// PollerFunc adapts a function to [Poller].
type PollerFunc func(ctx context.Context, req PollRequest) (PollResult, error)

// Poll implements Poller.
func (f PollerFunc) Poll(ctx context.Context, req PollRequest) (PollResult, error) {
	return f(ctx, req)
}

// Catalogue supplies what the schedule is computed from: the leagues worth
// polling and, for each, whatever is currently known about its events.
//
// It is separate from [Poller] because the two have completely different cost
// profiles at the chosen provider: ADR 0003 records that `/v4/sports` and
// `/v4/sports/{sport}/events` cost ZERO credits while `/odds` costs
// markets × regions, and its implementation requirement #2 is to "refresh the
// event and league catalogue aggressively — only price polling costs
// anything". Folding the catalogue into the sweep would make every schedule
// recomputation cost money.
//
// # A league with no known events is still scheduled
//
// internal/ingest/provider's Catalogue returns sports, leagues and books — not
// events, because The Odds API's league list and its event list are different
// endpoints and the adapter interface only wraps the first. So on a cold start
// the scheduler knows WHICH leagues exist and nothing about their fixtures.
//
// Returning a [LeaguePlan] with an empty Events slice is the supported way to
// say exactly that. Such a league is scheduled at [Config.DiscoveryWindow],
// and because a league's first sweep fires immediately regardless of window,
// the discovery cadence only ever governs a league that keeps coming back
// empty — an out-of-season competition, which is precisely the thing that
// should be polled slowly.
type Catalogue interface {
	Schedule(ctx context.Context) ([]LeaguePlan, error)
}

// CatalogueFunc adapts a function to [Catalogue].
type CatalogueFunc func(ctx context.Context) ([]LeaguePlan, error)

// Schedule implements Catalogue.
func (f CatalogueFunc) Schedule(ctx context.Context) ([]LeaguePlan, error) { return f(ctx) }

// LeaguePlan is one league and what is currently known about its fixtures.
type LeaguePlan struct {
	// League is the league to sweep. Required; a plan with a zero id is
	// dropped and logged.
	League domain.LeagueID

	// Events are the league's events as most recently observed. It may be
	// empty — see [Catalogue]. Events that no longer accept wagers are
	// filtered out here, so a caller may pass the raw list.
	Events []domain.Event
}

// PollRequest is one scheduled league sweep.
type PollRequest struct {
	// League is the league to sweep. One request covers every event in it —
	// see doc.go on why the unit of work is a league and not an event.
	League domain.LeagueID

	// Window is why this sweep is due now. An adapter may use it to widen or
	// narrow what it asks for (live markets only, say), and it is carried
	// mostly so a log line explains itself.
	Window Window

	// Events are the league's pollable events as of the most recent catalogue
	// refresh. It is a fresh slice per request; an adapter that sweeps by
	// league key can ignore it, one that must address events individually
	// needs it.
	Events []domain.Event

	// ScheduledAt is when the sweep became due, before any time spent waiting
	// on the quota limiter. The difference between it and the moment Poll is
	// entered is limiter-induced delay, and it is measured as
	// sharpline_ingest_quota_wait_seconds.
	ScheduledAt time.Time

	// Attempt counts the consecutive FAILED sweeps that preceded this one.
	// Zero on the first try after any success.
	Attempt int
}

// PollResult is what one sweep found.
type PollResult struct {
	// Markets is how many markets the payload carried. Zero is a real state —
	// a league with no fixtures — and is recorded as result="empty", not as an
	// error.
	Markets int

	// Changed is how many of those markets differed from the previous
	// observation, i.e. how many survived the change-detection hash CLAUDE.md
	// §5 mandates and actually generated bus traffic. Zero drives adaptive
	// backoff.
	Changed int

	// QuotaRemaining is the provider's own remaining-credit count, from The
	// Odds API's `x-requests-remaining` response header. ADR 0003 makes it
	// authoritative over any local estimate, so reporting it here is what
	// keeps sharpline_provider_quota_remaining from drifting.
	//
	// Negative means "the provider did not say", which is the correct value
	// for the synthetic generator and for any response that omitted the
	// header. Use [PollResult.WithoutQuota] or leave it negative rather than
	// reporting 0, which would read as "exhausted".
	QuotaRemaining int

	// QuotaLimit is the budget QuotaRemaining is measured against — The Odds
	// API's `x-requests-remaining` is a countdown from a plan size, and that
	// plan size is what the dashboard divides by.
	//
	// It travels WITH QuotaRemaining because the two are one ratio. Reporting
	// a provider remaining against a locally-configured limit is how
	// sharpline_provider_quota_remaining came to read 5,000% of its own limit
	// on the synthetic path.
	//
	// Zero or negative means "the provider did not say", and the configured
	// limit stands.
	QuotaLimit int
}

// WithoutQuota returns a PollResult that reports no provider quota, for
// adapters that never see one.
func (r PollResult) WithoutQuota() PollResult {
	r.QuotaRemaining = -1
	r.QuotaLimit = -1
	return r
}

// LeagueState is a snapshot of one league's scheduling state, for tests and for
// operational logging.
type LeagueState struct {
	League      domain.LeagueID
	Window      Window
	Events      int
	QuietSweeps int
	Failures    int
	Interval    time.Duration
	LastPollAt  time.Time
	NextDueAt   time.Time
}

// Options are [New]'s dependencies. Everything is constructor-injected;
// nothing is read from a global (CLAUDE.md §12).
type Options struct {
	// Config is the cadence ladder, the quota budget and the timeouts.
	Config Config
	// Poller performs the sweeps. Required.
	Poller Poller
	// Catalogue supplies the events. Required.
	Catalogue Catalogue
	// Logger receives lifecycle and failure events. Required — a scheduler
	// that cannot report a quota exhaustion is a scheduler whose failures are
	// invisible.
	Logger *slog.Logger
	// Metrics is the collector set. nil builds an unregistered set, which is
	// correct for a unit test.
	Metrics *Metrics
	// ErrorBackoffMax caps the post-failure backoff. Zero means
	// DefaultErrorBackoffMax.
	ErrorBackoffMax time.Duration
}

// Scheduler drives adaptive polling. Construct with [New]; run with
// [Scheduler.Run].
type Scheduler struct {
	cfg     Config
	poller  Poller
	cat     Catalogue
	log     *slog.Logger
	m       *Metrics
	budget  *Budget
	errCap  time.Duration
	credits int

	// sem bounds concurrent in-flight sweeps.
	sem chan struct{}

	running atomic.Bool

	mu      sync.Mutex
	runners map[domain.LeagueID]*leagueRunner
}

// New validates the configuration and builds a scheduler.
func New(opts Options) (*Scheduler, error) {
	cfg := opts.Config.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch {
	case opts.Poller == nil:
		return nil, fmt.Errorf("%w: Poller is nil", ErrInvalidConfig)
	case opts.Catalogue == nil:
		return nil, fmt.Errorf("%w: Catalogue is nil", ErrInvalidConfig)
	case opts.Logger == nil:
		return nil, fmt.Errorf("%w: Logger is nil", ErrInvalidConfig)
	case opts.ErrorBackoffMax < 0:
		return nil, fmt.Errorf("%w: ErrorBackoffMax must not be negative, got %s",
			ErrInvalidConfig, opts.ErrorBackoffMax)
	}

	m := opts.Metrics
	if m == nil {
		var err error
		if m, err = NewMetrics(nil); err != nil {
			return nil, err
		}
	}

	budget, err := NewBudget(cfg.Quota, cfg.Now)
	if err != nil {
		return nil, err
	}

	errCap := opts.ErrorBackoffMax
	if errCap == 0 {
		errCap = DefaultErrorBackoffMax
	}

	s := &Scheduler{
		cfg:     cfg,
		poller:  opts.Poller,
		cat:     opts.Catalogue,
		log:     opts.Logger.With(slog.String("component", "ingest.scheduler")),
		m:       m,
		budget:  budget,
		errCap:  errCap,
		credits: cfg.CreditsPerSweep,
		sem:     make(chan struct{}, cfg.MaxConcurrentPolls),
		runners: make(map[domain.LeagueID]*leagueRunner),
	}
	s.m.recordQuota(cfg.Provider, budget)
	s.m.recordSchedule(cfg.Provider, nil)
	return s, nil
}

// Budget exposes the shared quota limiter, so an operational endpoint or a test
// can read it without reaching into the scheduler.
func (s *Scheduler) Budget() *Budget { return s.budget }

// Run drives the schedule until ctx is cancelled.
//
// It returns nil on a clean shutdown. It returns an error only for a condition
// no retry can fix — a second concurrent Run. A catalogue failure or a provider
// failure is logged, counted and retried: ingest must survive a provider blip,
// because a crash-looping ingest turns a five-minute provider outage into a
// five-minute outage plus a cold start.
//
// # Shutdown
//
// On cancellation every league goroutine stops scheduling new sweeps, an
// in-flight sweep is given Config.ShutdownGrace to finish (its provider credit
// is already spent — abandoning the payload wastes it for nothing), and Run
// does not return until every goroutine it started has exited. Nothing here
// leaks; TestSchedulerLeaksNoGoroutines proves it.
func (s *Scheduler) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	defer s.running.Store(false)

	s.log.Info("adaptive polling started",
		slog.String("provider", s.cfg.Provider),
		slog.Int("credits_per_sweep", s.credits),
		slog.Int("max_concurrent_polls", s.cfg.MaxConcurrentPolls),
		slog.String("catalogue_refresh", s.cfg.CatalogueRefresh.String()),
		slog.Any("cadence", cadenceAttrs(s.cfg.Tiers)),
		slog.Any("quota", s.budget),
	)

	var wg sync.WaitGroup
	s.planner(ctx, &wg)

	// Every runner observes the same cancellation; wait for all of them.
	wg.Wait()
	s.retireAll()

	s.log.Info("adaptive polling stopped", slog.Any("quota", s.budget))
	return nil
}

// Snapshot reports the current state of every scheduled league, sorted by
// league id so the output is stable.
func (s *Scheduler) Snapshot() []LeagueState {
	s.mu.Lock()
	runners := make([]*leagueRunner, 0, len(s.runners))
	for _, r := range s.runners {
		runners = append(runners, r)
	}
	s.mu.Unlock()

	out := make([]LeagueState, 0, len(runners))
	for _, r := range runners {
		out = append(out, r.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].League < out[j].League })
	return out
}

// -----------------------------------------------------------------------------
// Planner
// -----------------------------------------------------------------------------

// planner refreshes the catalogue and reconciles the set of league runners.
//
// It runs on the caller's goroutine rather than a spawned one so that Run's
// lifetime is exactly the planner's lifetime, and there is no window where Run
// has returned while a planner tick is still in flight.
func (s *Scheduler) planner(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(s.cfg.CatalogueRefresh)
	defer ticker.Stop()

	for {
		s.refresh(ctx, wg)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refresh reads the catalogue once and applies it to the schedule.
func (s *Scheduler) refresh(ctx context.Context, wg *sync.WaitGroup) {
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CatalogueTimeout)
	plans, err := s.cat.Schedule(callCtx)
	cancel()

	if err != nil {
		s.m.recordCatalogue(s.cfg.Provider, 0, err)
		if ctx.Err() != nil {
			// Shutting down; the failure is the cancellation itself.
			return
		}
		s.log.Error("catalogue refresh failed; the schedule is running on a stale event list",
			slog.String("error", err.Error()))
		return
	}

	now := s.cfg.Now()
	present := make(map[domain.LeagueID]struct{}, len(plans))
	counts := make(map[Window]int, len(Windows()))
	pollable := 0

	for _, plan := range plans {
		if plan.League.IsZero() {
			s.log.Warn("catalogue returned a plan with no league id; dropped")
			continue
		}
		if _, dup := present[plan.League]; dup {
			s.log.Warn("catalogue returned the same league twice; the later plan is ignored",
				slog.String("league", plan.League.String()))
			continue
		}

		events := pollableEvents(plan.Events, now, s.cfg.Boundaries)
		pollable += len(events)

		w, ok := FoldWindows(events, now, s.cfg.Boundaries)
		if !ok {
			// Known league, nothing (yet) to price. Poll it slowly rather than
			// dropping it: dropping would mean the league is never swept again
			// and its fixtures are never discovered.
			w = s.cfg.DiscoveryWindow
		}

		present[plan.League] = struct{}{}
		counts[w]++
		s.apply(ctx, wg, plan.League, w, events)
	}

	s.m.recordCatalogue(s.cfg.Provider, pollable, nil)
	s.retireAbsent(present)
	s.m.recordSchedule(s.cfg.Provider, counts)
	s.m.recordQuota(s.cfg.Provider, s.budget)
}

// pollableEvents drops the events whose status no longer accepts wagers, so a
// caller may hand over a raw provider list.
func pollableEvents(events []domain.Event, now time.Time, b Boundaries) []domain.Event {
	out := make([]domain.Event, 0, len(events))
	for _, e := range events {
		if ClassifyEvent(e, now, b).Valid() {
			out = append(out, e)
		}
	}
	return out
}

// apply starts a runner for league, or hands an existing one its new window and
// event list.
func (s *Scheduler) apply(
	ctx context.Context, wg *sync.WaitGroup, league domain.LeagueID, w Window, events []domain.Event,
) {
	s.mu.Lock()
	r, ok := s.runners[league]
	if !ok {
		r = newLeagueRunner(league, w, events, s.cfg, s.errCap)
		s.runners[league] = r
	}
	s.mu.Unlock()

	if ok {
		r.update(w, events)
		return
	}

	s.m.publishCadence(s.cfg.Provider, league, s.cfg.Tiers)
	s.log.Info("league entered the schedule",
		slog.String("league", league.String()),
		slog.String("window", w.String()),
		slog.Int("events", len(events)),
		slog.String("interval", intervalOf(s.cfg.Tiers, w).String()),
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runLeague(ctx, r)
	}()
}

// retireAbsent stops runners for leagues the catalogue no longer lists.
func (s *Scheduler) retireAbsent(present map[domain.LeagueID]struct{}) {
	s.mu.Lock()
	var gone []*leagueRunner
	for league, r := range s.runners {
		if _, ok := present[league]; ok {
			continue
		}
		gone = append(gone, r)
		delete(s.runners, league)
	}
	s.mu.Unlock()

	for _, r := range gone {
		r.stop()
		s.m.retireCadence(s.cfg.Provider, r.league)
		s.log.Info("league left the schedule",
			slog.String("league", r.league.String()))
	}
}

// retireAll drops every cadence series on shutdown, so a restarted process does
// not inherit a stale scrape.
func (s *Scheduler) retireAll() {
	s.mu.Lock()
	runners := s.runners
	s.runners = make(map[domain.LeagueID]*leagueRunner)
	s.mu.Unlock()

	for _, r := range runners {
		s.m.retireCadence(s.cfg.Provider, r.league)
	}
	s.m.recordSchedule(s.cfg.Provider, nil)
}

// -----------------------------------------------------------------------------
// Per-league runner
// -----------------------------------------------------------------------------

// leagueRunner owns one league's schedule. Its state is mutated only by its own
// goroutine except through update/stop/snapshot, which take the mutex.
type leagueRunner struct {
	league domain.LeagueID
	cfg    Config
	errCap time.Duration
	rng    *rand.Rand

	// updates carries window/event changes from the planner. Capacity 1 with a
	// non-blocking send: the planner must never block on a league whose
	// goroutine is inside a slow provider call, and a superseded update is
	// worthless anyway — the next refresh is one CatalogueRefresh away.
	updates chan struct{}
	done    chan struct{}
	stopped sync.Once

	mu       sync.Mutex
	window   Window
	events   []domain.Event
	quiet    int
	failures int
	lastPoll time.Time
	nextDue  time.Time
}

func newLeagueRunner(
	league domain.LeagueID, w Window, events []domain.Event, cfg Config, errCap time.Duration,
) *leagueRunner {
	return &leagueRunner{
		league:  league,
		cfg:     cfg,
		errCap:  errCap,
		rng:     newLeagueRNG(cfg.Seed, league),
		updates: make(chan struct{}, 1),
		done:    make(chan struct{}),
		window:  w,
		events:  events,
	}
}

// newLeagueRNG gives every league its own generator, seeded deterministically
// from the configured seed and the league id.
//
// One shared *rand.Rand would need a mutex and would make the jitter sequence
// depend on goroutine scheduling, which is precisely the thing that makes a
// "deterministic" test flaky. Seeding from the league id means adding a fifth
// league does not change the first four's schedules.
func newLeagueRNG(seed int64, league domain.LeagueID) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(league))
	s := uint64(seed) //nolint:gosec // deliberate wraparound: this is a seed, not a quantity.
	if seed == 0 {
		s = rand.Uint64()
	}
	return rand.New(rand.NewPCG(s, h.Sum64()))
}

// update hands the runner a new window and event list and wakes it.
func (r *leagueRunner) update(w Window, events []domain.Event) {
	r.mu.Lock()
	promoted := w.MoreUrgentThan(r.window)
	r.window = w
	r.events = events
	if promoted {
		// A league that has just gone live is not quiet: it is about to be the
		// busiest thing on the board. Carrying a backoff multiplier across the
		// promotion would mean the first live poll came a multiple of the live
		// cadence late, which is exactly when staleness is most visible.
		r.quiet = 0
	}
	r.mu.Unlock()

	select {
	case r.updates <- struct{}{}:
	default:
	}
}

func (r *leagueRunner) stop() { r.stopped.Do(func() { close(r.done) }) }

func (r *leagueRunner) snapshot() LeagueState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return LeagueState{
		League:      r.league,
		Window:      r.window,
		Events:      len(r.events),
		QuietSweeps: r.quiet,
		Failures:    r.failures,
		Interval:    r.intervalLocked(),
		LastPollAt:  r.lastPoll,
		NextDueAt:   r.nextDue,
	}
}

// intervalLocked is the effective interval: the tier's backed-off interval, or
// the post-failure backoff if that is longer.
//
// Failure backoff is computed separately from quiet backoff and the two are
// maxed rather than added. They answer different questions — "nothing is
// moving" versus "the provider is not answering" — and a live tier deliberately
// disables the first (MaxInterval == Interval) while still needing the second,
// because hammering a provider that is returning 429s at the live cadence is
// how a rate limit becomes a ban.
func (r *leagueRunner) intervalLocked() time.Duration {
	tier, _ := r.cfg.Tiers.For(r.window)
	d := tier.intervalAfter(r.quiet)
	if r.failures > 0 {
		if f := backoffAfter(tier.Interval, r.failures, r.errCap); f > d {
			d = f
		}
	}
	return d
}

// backoffAfter doubles base n times, clamped to ceiling.
func backoffAfter(base time.Duration, n int, ceiling time.Duration) time.Duration {
	if n <= 0 {
		return base
	}
	if n > maxBackoffDoublings {
		n = maxBackoffDoublings
	}
	d := base << uint(n) //nolint:gosec // bounded by maxBackoffDoublings above.
	if d <= 0 || d > ceiling {
		return ceiling
	}
	return d
}

// runLeague is the goroutine per poller CLAUDE.md §2 chose Go for.
func (s *Scheduler) runLeague(ctx context.Context, r *leagueRunner) {
	for {
		delay := s.nextDelay(r)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.done:
			timer.Stop()
			return
		case <-r.updates:
			// The window or the event list changed. Recompute the delay: a
			// promotion to a faster tier must take effect now, not after the
			// slower tier's timer expires.
			timer.Stop()
			continue
		case <-timer.C:
		}

		s.sweep(ctx, r)
	}
}

// nextDelay computes how long this league waits before its next sweep, with
// jitter, and records the due instant for Snapshot.
func (s *Scheduler) nextDelay(r *leagueRunner) time.Duration {
	now := s.cfg.Now()

	r.mu.Lock()
	interval := r.intervalLocked()
	last := r.lastPoll
	jitter := r.jitter(interval, last.IsZero())
	r.mu.Unlock()

	var due time.Time
	if last.IsZero() {
		// First sweep of this league's life. Fire as soon as possible so the
		// board populates on startup, but spread the initial burst across the
		// slate so four leagues do not open four provider connections in the
		// same millisecond.
		due = now.Add(jitter)
	} else {
		due = last.Add(interval + jitter)
	}

	r.mu.Lock()
	r.nextDue = due
	r.mu.Unlock()

	if d := due.Sub(now); d > 0 {
		return d
	}
	return 0
}

// jitter returns the offset applied to one interval.
//
// On the first sweep it is one-sided and positive (0, +f·interval] so startup
// only ever spreads out, never fires early against a clock that has not run
// yet. Afterwards it is two-sided (−f·interval, +f·interval) so jitter does not
// systematically lengthen the average cadence — which would quietly make the
// real cadence slower than the number the SLO objective is computed from.
func (r *leagueRunner) jitter(interval time.Duration, first bool) time.Duration {
	if r.cfg.JitterFraction <= 0 {
		return 0
	}
	span := float64(interval) * r.cfg.JitterFraction
	if first {
		return time.Duration(r.rng.Float64() * span)
	}
	return time.Duration((r.rng.Float64()*2 - 1) * span)
}

// sweep runs one league poll: reserve credits, take a concurrency slot, call
// the adapter, record the outcome and update the backoff state.
func (s *Scheduler) sweep(ctx context.Context, r *leagueRunner) {
	scheduledAt := s.cfg.Now()

	req := PollRequest{
		League:      r.league,
		ScheduledAt: scheduledAt,
	}
	r.mu.Lock()
	req.Window = r.window
	req.Attempt = r.failures
	req.Events = append([]domain.Event(nil), r.events...)
	r.mu.Unlock()

	// ---- quota ----
	if err := s.budget.Acquire(ctx, s.credits); err != nil {
		s.m.recordQuota(s.cfg.Provider, s.budget)
		if errors.Is(err, ErrQuotaExhausted) {
			s.m.recordQuotaBlocked(s.cfg.Provider, r.league)
			// Loud, per ADR 0003's implementation requirement #5: the correct
			// behaviour at exhaustion is an alert and a visible degraded state,
			// never a board that silently serves hour-old prices as if they
			// were live. There is deliberately no failover to the synthetic
			// generator — substituting simulated prices for real ones in a
			// running deployment is indistinguishable from fabricating market
			// data.
			s.log.Error("provider quota exhausted; sweep refused and this league's board is now frozen",
				slog.String("league", r.league.String()),
				slog.String("window", req.Window.String()),
				slog.Any("quota", s.budget),
				slog.String("error", err.Error()),
			)
			// Advance the clock so the refusal is retried on the normal
			// cadence rather than in a hot loop.
			r.mu.Lock()
			r.lastPoll = s.cfg.Now()
			r.mu.Unlock()
		}
		return
	}
	waited := s.cfg.Now().Sub(scheduledAt)
	s.m.recordQuotaWait(s.cfg.Provider, waited)
	if waited > 0 {
		s.log.Debug("sweep waited on the provider quota limiter",
			slog.String("league", r.league.String()),
			slog.String("waited", waited.String()),
		)
	}

	// ---- concurrency slot ----
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		// Credits were reserved and the request was never issued, so this is
		// the one refundable case.
		s.budget.Refund(s.credits)
		s.m.recordQuota(s.cfg.Provider, s.budget)
		return
	}
	defer func() { <-s.sem }()

	// ---- the call ----
	pollCtx, cancel := s.pollContext(ctx)
	start := s.cfg.Now()
	res, err := s.poller.Poll(pollCtx, req)
	elapsed := s.cfg.Now().Sub(start)
	cancel()

	if res.QuotaRemaining >= 0 {
		s.budget.Reconcile(res.QuotaRemaining, res.QuotaLimit)
	}
	s.m.recordQuota(s.cfg.Provider, s.budget)
	result := s.m.recordPoll(s.cfg.Provider, r.league, req.Window, res, err, elapsed)

	s.applyOutcome(r, req.Window, result, res, err, elapsed)
}

// pollContext bounds one sweep.
//
// It is DETACHED from ctx and then re-attached with a grace period, which is
// deliberate. A sweep that has already issued its provider request has already
// spent the credit; severing it the instant SIGTERM lands throws that away and
// loses a payload the system could have published. So an in-flight sweep gets
// Config.ShutdownGrace to finish, bounded by Config.PollTimeout regardless, and
// then it is cancelled — because blocking shutdown for ever on an unresponsive
// provider converts a graceful drain into a SIGKILL with less to show for it.
func (s *Scheduler) pollContext(ctx context.Context) (context.Context, context.CancelFunc) {
	pollCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.PollTimeout)

	if s.cfg.ShutdownGrace <= 0 {
		// No grace configured: shutdown severs the call immediately.
		stop := context.AfterFunc(ctx, cancel)
		return pollCtx, func() { stop(); cancel() }
	}

	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }

	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		grace := time.NewTimer(s.cfg.ShutdownGrace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
			cancel()
		}
	}()

	return pollCtx, func() { finish(); cancel() }
}

// applyOutcome updates the league's backoff state from one sweep's result.
func (s *Scheduler) applyOutcome(
	r *leagueRunner, w Window, result string, res PollResult, err error, elapsed time.Duration,
) {
	now := s.cfg.Now()

	r.mu.Lock()
	r.lastPoll = now
	switch result {
	case resultError:
		r.failures++
	case resultChanged:
		// §5's reset condition: anything moved, so poll at full cadence again.
		r.quiet = 0
		r.failures = 0
	default: // unchanged, empty
		r.quiet++
		r.failures = 0
	}
	quiet, failures := r.quiet, r.failures
	next := r.intervalLocked()
	r.mu.Unlock()

	s.m.recordBackoff(s.cfg.Provider, r.league, quiet)

	attrs := []slog.Attr{
		slog.String("league", r.league.String()),
		slog.String("window", w.String()),
		slog.String("result", result),
		slog.Int("markets", res.Markets),
		slog.Int("changed", res.Changed),
		slog.String("took", elapsed.String()),
		slog.String("next_interval", next.String()),
		slog.Int("quiet_sweeps", quiet),
		slog.Int("consecutive_failures", failures),
	}
	if err != nil {
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "provider sweep failed",
			append(attrs, slog.String("error", err.Error()))...)
		return
	}
	s.log.LogAttrs(context.Background(), slog.LevelDebug, "provider sweep complete", attrs...)
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

func intervalOf(t Tiers, w Window) time.Duration {
	tier, _ := t.For(w)
	return tier.Interval
}

func cadenceAttrs(t Tiers) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(Windows()))
	for _, w := range Windows() {
		tier, ok := t.For(w)
		if !ok {
			continue
		}
		attrs = append(attrs, slog.String(w.String(), tier.Interval.String()))
	}
	return attrs
}
