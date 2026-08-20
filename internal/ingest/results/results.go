// The poller: the loop itself. Read doc.go first — it carries the argument for
// why results are their own source, why the work queue lives in the database,
// and why there is no memo of what has already been recorded.
package results

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// Defaults. Every one is overridable through [Config]; a zero field takes the
// default rather than failing, so a zero Config is a working poller.
const (
	// DefaultInterval is how often the work queue is read.
	//
	// A minute, which is the same order as ADR 0003's 90-second live odds
	// cadence and for a related reason: the interval is the FLOOR of the
	// settlement lag, so it is what a customer waits between the final whistle
	// and their ticket becoming settleable, and a five-minute loop would be
	// visible to them. It costs one indexed read per tick against a partial
	// index — cheap enough that the number is chosen for the customer rather
	// than for the database.
	DefaultInterval = time.Minute

	// DefaultSettleDelay is how long after its scheduled start a contest is
	// considered plausibly over, and therefore worth asking about.
	//
	// Two hours. It is deliberately SHORTER than the longest contest in scope,
	// and that is the right direction to be wrong in: the horizon decides what
	// to ASK about and the provider decides what has actually finished, so a
	// query issued too early costs one entry in a batch the provider answers
	// with silence, while a horizon set too late costs every customer on that
	// fixture the difference. See doc.go.
	DefaultSettleDelay = 2 * time.Hour

	// DefaultBatchSize bounds one work-queue read.
	//
	// One tick must not be able to pull an unbounded slate into memory. Two
	// hundred is an order of magnitude above any steady-state queue — the
	// shipped generator stages 32 contests a day across four leagues — which is
	// what keeps the oldest-first ordering from letting a handful of
	// permanently-stuck rows crowd out fresh ones. doc.go names that starvation
	// shape and the gauge that makes it visible.
	DefaultBatchSize = 200

	// DefaultPollTimeout bounds one whole tick: the queue read, the provider
	// call, and every write it produces.
	//
	// CLAUDE.md §12: every external call has a timeout. It is one budget for the
	// tick rather than one per statement because the tick is the unit that must
	// finish before the next one starts; a per-statement timeout would let a
	// slow batch of 200 writes run for 200 times as long as anybody intended.
	DefaultPollTimeout = 30 * time.Second

	// DefaultErrorBackoffMax caps the exponential backoff after consecutive
	// failed ticks. Five minutes, matching scheduler.DefaultErrorBackoffMax and
	// for the same reason: an outage longer than that is an incident rather than
	// a blip, and continuing to knock every five minutes is what makes recovery
	// automatic without becoming a retry storm.
	DefaultErrorBackoffMax = 5 * time.Minute
)

// Errors this package returns.
var (
	// ErrInvalidOptions means the poller was constructed with a configuration or
	// a dependency set it cannot run. CLAUDE.md §12: "typed struct and startup
	// validation — fail fast and loudly on a bad config".
	ErrInvalidOptions = errors.New("results: invalid options")

	// ErrAlreadyRunning is returned by a second concurrent call to [Poller.Run].
	// One poller is one loop; two would double every write's contention for no
	// extra throughput, since the writes are guarded and idempotent.
	ErrAlreadyRunning = errors.New("results: already running")
)

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

// Config is the results poller's cadence and horizons.
//
// It is a plain struct injected by the composition root rather than something
// this package reads from the environment. CLAUDE.md §12 puts configuration
// loading in one place — internal/platform/config — and a second reader here
// would be a second place for a deployment to disagree with itself.
type Config struct {
	// Interval is how often the work queue is read. Zero means
	// [DefaultInterval]; negative is rejected.
	Interval time.Duration

	// SettleDelay is how long after its scheduled start a contest is worth
	// asking about. Zero means [DefaultSettleDelay]; negative is rejected.
	SettleDelay time.Duration

	// BatchSize bounds one work-queue read. Zero means [DefaultBatchSize].
	BatchSize int

	// PollTimeout bounds one whole tick. Zero means [DefaultPollTimeout].
	PollTimeout time.Duration

	// ErrorBackoffMax caps the backoff after consecutive failures. Zero means
	// [DefaultErrorBackoffMax].
	ErrorBackoffMax time.Duration

	// Now is the clock. Nil means time.Now.
	//
	// Injected rather than read globally so the horizon and the settlement lag
	// are testable without sleeping (CLAUDE.md §12: no global mutable state). It
	// is NEVER used to stamp a stored value: events.observed_at is the
	// provider's instant, carried on provider.FinalResult.FinalisedAt.
	Now func() time.Time
}

// withDefaults returns a copy with every zero field replaced by its default.
func (c Config) withDefaults() Config {
	if c.Interval == 0 {
		c.Interval = DefaultInterval
	}
	if c.SettleDelay == 0 {
		c.SettleDelay = DefaultSettleDelay
	}
	if c.BatchSize == 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.PollTimeout == 0 {
		c.PollTimeout = DefaultPollTimeout
	}
	if c.ErrorBackoffMax == 0 {
		// The default ceiling is [DefaultErrorBackoffMax], EXCEPT where the
		// interval is already longer than it — in which case the ceiling is the
		// interval, because Validate refuses a ceiling below the floor and an
		// operator who set only SHARPLINE_INGEST_RESULTS_INTERVAL would
		// otherwise get a refused startup for a knob they never touched, with no
		// knob available to fix it. Taking the larger of the two keeps "zero
		// means the default" true without letting a default contradict an
		// explicit setting.
		c.ErrorBackoffMax = max(DefaultErrorBackoffMax, c.Interval)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Validate reports whether the configuration describes a poller that can run. It
// is called by [New] after defaults are applied, so it never sees a zero field.
func (c Config) Validate() error {
	switch {
	case c.Interval <= 0:
		return fmt.Errorf("%w: Interval must be positive, got %s", ErrInvalidOptions, c.Interval)
	case c.SettleDelay < 0:
		// Zero is legal and means "ask about anything that has started". It is
		// not the default, but a provider that can be trusted to say when a
		// contest is over is entitled to be asked immediately, and refusing it
		// would put a policy in the validator rather than in the default.
		return fmt.Errorf("%w: SettleDelay must not be negative, got %s", ErrInvalidOptions, c.SettleDelay)
	case c.BatchSize <= 0:
		return fmt.Errorf("%w: BatchSize must be positive, got %d; a zero limit returns no rows, "+
			"which reads as 'there is nothing to settle'", ErrInvalidOptions, c.BatchSize)
	case c.PollTimeout <= 0:
		return fmt.Errorf("%w: PollTimeout must be positive, got %s", ErrInvalidOptions, c.PollTimeout)
	case c.ErrorBackoffMax < c.Interval:
		// A ceiling below the floor would make "backed off" mean "polled
		// sooner". internal/ingest.LoadConfig refuses the same inversion on the
		// scheduler's live tier.
		return fmt.Errorf("%w: ErrorBackoffMax %s is below Interval %s, which would make a backoff "+
			"poll sooner than a healthy tick", ErrInvalidOptions, c.ErrorBackoffMax, c.Interval)
	case c.Now == nil:
		return fmt.Errorf("%w: Now must not be nil", ErrInvalidOptions)
	}
	return nil
}

// LogValue implements slog.LogValuer.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("interval", c.Interval.String()),
		slog.String("settle_delay", c.SettleDelay.String()),
		slog.Int("batch_size", c.BatchSize),
		slog.String("poll_timeout", c.PollTimeout.String()),
		slog.String("error_backoff_max", c.ErrorBackoffMax.String()),
	)
}

// -----------------------------------------------------------------------------
// Poller
// -----------------------------------------------------------------------------

// Options are [New]'s dependencies. Everything is constructor-injected; nothing
// is read from a global (CLAUDE.md §12).
type Options struct {
	// Config is the cadence and horizons. The zero value is valid.
	Config Config

	// Provider is the results source. Required.
	Provider provider.ResultsProvider

	// Store is the `events` table. Required.
	Store Store

	// Logger receives lifecycle and failure events. Required — there is no
	// silent fallback, because the two things this loop has to be able to say
	// ("a contest became settleable", "the results path is disabled") are
	// invisible anywhere else.
	Logger *slog.Logger

	// Registry receives the collectors. Nil builds them unregistered, which is
	// right for a unit test and for a process with no /metrics endpoint.
	// Ignored when Metrics is set.
	Registry prometheus.Registerer

	// Metrics is an already-registered collector set. Takes precedence over
	// Registry.
	Metrics *Metrics
}

// Poller is the running results loop. Construct with [New]; run with
// [Poller.Run].
type Poller struct {
	cfg   Config
	src   provider.ResultsProvider
	store Store
	log   *slog.Logger
	m     *Metrics

	// prov is the results source's name in the form internal/ingest/normalizer
	// takes, so that [Poller.resolve] can derive the identifier the database
	// holds from the key the provider states. It is resolved once, in [New], and
	// never afterwards: the derivation runs once per reported contest per tick,
	// and re-validating a constant on each one would be work in the hot path to
	// re-learn something startup already knows.
	prov kafka.Provider

	// running rejects a second concurrent Run. It is the only mutable state on
	// the type: everything else the loop needs lives on the stack of one tick or
	// in the database, which is what makes the poller restartable without
	// losing anything.
	running atomic.Bool
}

// New validates the options and builds the poller. It performs no I/O and starts
// nothing; call [Poller.Run] for that.
func New(opts Options) (*Poller, error) {
	cfg := opts.Config.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch {
	case opts.Provider == nil:
		return nil, fmt.Errorf("%w: Provider is nil; without a results source nothing ever "+
			"settles and every stake stays in escrow", ErrInvalidOptions)
	case opts.Store == nil:
		return nil, fmt.Errorf("%w: Store is nil", ErrInvalidOptions)
	case opts.Logger == nil:
		return nil, fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	}

	name := opts.Provider.Name()
	if _, err := provider.NewName(name.String()); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	// The same name, in the two further forms the identifier derivation demands.
	// Both checks belong at startup rather than at the first reported contest:
	// a provider whose slug cannot appear inside an identifier can never resolve
	// anything, and CLAUDE.md §12 wants that failure loud and immediate rather
	// than discovered as a settlement feed that silently records nothing.
	prov, err := kafka.NewProvider(name.String())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	if err := normalizer.ValidateProviderForIdentity(prov); err != nil {
		return nil, fmt.Errorf("%w: results source %s: %w", ErrInvalidOptions, name, err)
	}

	m := opts.Metrics
	if m == nil {
		if m, err = NewMetrics(opts.Registry); err != nil {
			return nil, err
		}
	}

	return &Poller{
		cfg:   cfg,
		src:   opts.Provider,
		store: opts.Store,
		log:   opts.Logger.With(slog.String("component", "ingest.results")),
		m:     m,
		prov:  prov,
	}, nil
}

// Provider reports the results source's name, so the composition root can assert
// that a deployment's outcomes and its prices come from the same place.
func (p *Poller) Provider() provider.Name { return p.src.Name() }

// Run drives the loop until ctx is cancelled.
//
// # It returns nil for every ordinary ending, and that is deliberate
//
// The caller runs this beside the scheduler and the two consumer loops and joins
// their errors, so anything returned here fails the whole ingest process. Three
// endings are therefore reported as nil and logged instead:
//
//   - CONTEXT CANCELLATION is a normal shutdown.
//   - A FAILED TICK is not fatal. The work queue is in the database and nothing
//     is lost by trying again, so the loop backs off exponentially to
//     [Config.ErrorBackoffMax] and keeps going. Killing the process would take
//     the odds board down to fix a settlement outage, which is strictly worse
//     than a settlement outage.
//   - A PROVIDER THAT DOES NOT SERVE RESULTS (provider.ErrNotSupported) stops
//     the loop, once, at WARN. That is the state a deployment with
//     ODDS_API_KEY set is in today, and it is not a reason to refuse to serve an
//     odds board — but it must be said out loud rather than looking like a
//     healthy loop that never finds anything.
//
// The first tick fires IMMEDIATELY rather than after one interval. A deploy
// otherwise leaves every contest that finished during the restart waiting an
// extra interval for no reason, and the immediate tick is what makes a restart
// the fix for a stalled feed rather than a further delay.
func (p *Poller) Run(ctx context.Context) error {
	if !p.running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	defer p.running.Store(false)

	p.log.Info("results poller running",
		slog.String("provider", p.src.Name().String()),
		slog.Any("config", p.cfg),
	)

	timer := time.NewTimer(0)
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			p.log.Info("results poller stopped")
			return nil
		case <-timer.C:
		}

		err := p.tick(ctx)
		switch {
		case err == nil:
			failures = 0

		case ctx.Err() != nil:
			// The tick was cut short by shutdown, not by a fault. Loop round so
			// the ctx.Done arm reports the stop; counting it as a failure would
			// put an error line in every graceful shutdown.
			continue

		case errors.Is(err, provider.ErrNotSupported):
			// WARN and not ERROR: the process is healthy and the odds board is
			// serving. What is missing is a capability, and the message says
			// which and why rather than leaving somebody to infer it from an
			// empty settlement feed.
			p.log.Warn("results path disabled: the odds provider serves no results endpoint, so "+
				"no contest will be recorded as finished and no wager on one will settle",
				slog.String("provider", p.src.Name().String()),
				slog.String("error", err.Error()),
			)
			return nil

		default:
			failures++
			p.log.Error("results poll failed",
				slog.Int("consecutive_failures", failures),
				slog.String("error", err.Error()),
			)
		}
		timer.Reset(p.delay(failures))
	}
}

// delay is how long to wait before the next tick: the interval when the last one
// succeeded, and an exponential backoff capped at [Config.ErrorBackoffMax] after
// consecutive failures.
//
// The doubling is computed by shifting rather than by accumulating, so it cannot
// drift and cannot overflow into a negative duration: the shift is bounded
// before it is applied, which matters because a loop that has been failing for a
// day would otherwise shift past the width of an int64.
func (p *Poller) delay(failures int) time.Duration {
	if failures <= 0 {
		return p.cfg.Interval
	}
	const maxShift = 16
	shift := failures - 1
	if shift > maxShift {
		shift = maxShift
	}
	d := p.cfg.Interval << shift
	if d <= 0 || d > p.cfg.ErrorBackoffMax {
		return p.cfg.ErrorBackoffMax
	}
	return d
}

// -----------------------------------------------------------------------------
// One tick
// -----------------------------------------------------------------------------

// tick performs one whole cycle: read the work queue, ask the provider, write
// what came back.
//
// The three stages fail differently and are counted differently on purpose. A
// queue read that fails is the shared database being unhappy; a provider call
// that fails is the half of this loop that can be down while everything else is
// fine; a write that fails is a stake that has not been released. Folding them
// into one error counter would make the dashboard unable to say which.
func (p *Poller) tick(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.PollTimeout)
	defer cancel()

	started := p.cfg.Now()

	// The horizon is computed HERE, from the injected clock, and passed down.
	// queries/results.sql refuses to read now() for the reason every instant in
	// that schema is a parameter: a query that reads the database's clock cannot
	// be tested at a fixed time and cannot be replayed.
	horizon := started.UTC().Add(-p.cfg.SettleDelay)

	pending, err := p.store.EventsAwaitingResult(ctx, horizon, p.cfg.BatchSize)
	if err != nil {
		p.m.observePoll(pollStoreError, p.cfg.Now().Sub(started))
		return fmt.Errorf("results: read work queue before %s: %w",
			horizon.Format(time.RFC3339), err)
	}
	p.m.observeQueueDepth(len(pending))
	if len(pending) == 0 {
		p.m.observePoll(pollOK, p.cfg.Now().Sub(started))
		return nil
	}

	byID := make(map[domain.EventID]PendingEvent, len(pending))
	for _, e := range pending {
		byID[e.EventID] = e
	}
	window := resultWindow(pending, started)

	final, err := p.src.Results(ctx, window)
	if err != nil {
		p.m.observeProviderError(err)
		p.m.observePoll(pollProviderError, p.cfg.Now().Sub(started))
		return fmt.Errorf("results: ask %s what finished %s: %w", p.src.Name(), window, err)
	}

	outcome, writeErr := p.record(ctx, final, byID)
	p.m.observePoll(outcome, p.cfg.Now().Sub(started))
	return writeErr
}

// resultWindow is the span of finishing instants one tick asks about.
//
// SINCE is the oldest queued contest's scheduled start, which is a sound lower
// bound on any of their finishing instants because no contest finishes before it
// begins. Deriving it from the queue rather than from a constant is what keeps
// the routine tick asking about the last couple of hours instead of about the
// whole history of the sport, and it is also what makes the window widen exactly
// as far as it has to when a stranded row is the reason there is anything to ask
// about at all.
//
// The minimum is taken rather than assumed from the ordering. The queue is read
// oldest-first, but that is queries/results.sql's property to keep, and a window
// that silently depended on it would narrow itself the moment somebody changed
// an ORDER BY — stranding, without a symptom, exactly the contests that had been
// waiting longest.
//
// UNTIL is the tick's own clock reading. It is a hint like the horizon is: the
// provider clamps it to its own clock, because two clocks are two clocks and a
// window reaching past the adapter's would invite a result for a contest still
// being played.
func resultWindow(pending []PendingEvent, now time.Time) provider.ResultWindow {
	since := now
	for _, e := range pending {
		if e.ScheduledStart.Before(since) {
			since = e.ScheduledStart
		}
	}
	return provider.ResultWindow{Since: since.UTC(), Until: now.UTC()}
}

// record writes every result the provider stated that this deployment is waiting
// on, and accounts for every contest that was waited on.
//
// # The identifier crossing happens here and nowhere else
//
// The provider states an outcome in ITS OWN identifier space; the work queue is
// in the domain's. [Poller.resolve] maps one to the other in the forward
// direction — the same derivation internal/ingest/normalizer applied when the
// row was written — and the intersection against byID is what decides whether
// there is a row to write at all. Doing it in this one place is the point: the
// defect this shape replaced was a comparison of the two spaces that lived in an
// adapter, could never match, and reported "unresolved" for every contest for
// ever without erroring once.
//
// A write failure does NOT abandon the batch. Each contest is independent — a
// row that cannot be written says nothing about the next one — and stopping at
// the first failure would let one bad event hold up every other customer's
// settlement behind it. The failures are counted, the first is returned so the
// tick backs off, and the rest of the batch still lands.
func (p *Poller) record(
	ctx context.Context, final []provider.FinalResult, byID map[domain.EventID]PendingEvent,
) (outcome string, err error) {
	var (
		recorded    int
		unchanged   int
		failed      int
		unsolicited int
		seen        = make(map[domain.EventID]bool, len(final))
		firstErr    error
		// worst is the poll outcome the failures so far justify. A write that
		// failed is the database's; a result the domain refused is the
		// provider's; and they are not the same alert, so the label is chosen
		// from what actually went wrong rather than defaulted.
		worst = pollOK
	)
	fail := func(outcome string, err error) {
		failed++
		if firstErr == nil {
			firstErr, worst = err, outcome
		}
	}

	for _, r := range final {
		id, ok := p.resolve(r)
		if !ok {
			// Unattributable: the provider stated an outcome with no key to
			// derive an identifier from. Counted with the other results that
			// reached no row, and dropped rather than guessed at.
			unsolicited++
			continue
		}
		pending, wanted := byID[id]
		if !wanted {
			// ORDINARY, and not a fault. Results is a window query — "what
			// finished", not "what happened to these rows" — so a provider that
			// covers more contests than this deployment ingested will report
			// them, every tick, for as long as they stay inside its window.
			// There is no row to write and nothing to say about it beyond the
			// count.
			unsolicited++
			continue
		}
		if seen[id] {
			// A second statement about one contest in one answer. Unlike the
			// case above this IS an adapter bug: the two copies cannot both be
			// attributed, and picking one would be picking arbitrarily.
			unsolicited++
			p.log.Error("results provider stated the same contest twice in one answer; "+
				"dropping the second",
				slog.String("event", id.String()),
				slog.String("event_key", r.EventKey),
				slog.String("provider", p.src.Name().String()),
			)
			continue
		}
		seen[id] = true

		// The provider's own output is checked before it is written. The failure
		// this catches is an `ended` result with no score, which the events
		// table would store happily — migrations/00002 constrains the score PAIR,
		// not its presence — and which would then grade every spread on the
		// contest against a 0-0 zero value.
		if err := r.Validate(); err != nil {
			fail(pollProviderError, fmt.Errorf("results: %s stated an unusable result: %w", p.src.Name(), err))
			p.log.Error("results provider stated an unusable result; not writing it",
				slog.String("event", id.String()),
				slog.String("error", err.Error()),
			)
			continue
		}

		wrote, err := p.store.RecordResult(ctx, id, r)
		if err != nil {
			fail(pollStoreError, fmt.Errorf("results: record %s: %w", id, err))
			p.log.Error("recording a contest's result failed; every stake on it stays in escrow",
				slog.String("event", id.String()),
				slog.String("error", err.Error()),
			)
			continue
		}
		if !wrote {
			// Already recorded, superseded by a newer observation, or never
			// ingested. All three are steady states; see [Store].
			unchanged++
			continue
		}

		recorded++
		lag := p.cfg.Now().Sub(r.FinalisedAt)
		p.m.observeLag(lag)
		// INFO and not DEBUG. This is the moment a contest becomes settleable and
		// every stake riding on it becomes payable, and it happens at most once
		// per event in the lifetime of a deployment. It is the line somebody
		// reads when asked why a customer has not been paid.
		p.log.Info("contest result recorded; wagers on it are now settleable",
			slog.String("event", id.String()),
			slog.String("name", pending.Name),
			slog.String("status", r.Status.String()),
			slog.String("score", scoreText(r)),
			slog.Time("finalised_at", r.FinalisedAt),
			slog.String("settlement_lag", lag.String()),
		)
	}

	p.m.observeEvents(eventRecorded, recorded)
	p.m.observeEvents(eventUnchanged, unchanged)
	p.m.observeEvents(eventFailed, failed)
	p.m.observeEvents(eventUnsolicited, unsolicited)
	// Everything waited on that the provider had nothing to say about. It is
	// derived by subtraction rather than counted, so recorded + unchanged +
	// failed + unresolved sums to the work queue's size by construction and a
	// contest that quietly went unanswered cannot hide in the arithmetic.
	// `unsolicited` is deliberately outside that sum: it counts statements about
	// contests the queue never held, which is a different denominator.
	p.m.observeEvents(eventUnresolved, len(byID)-len(seen))
	if unsolicited > 0 {
		p.log.Debug("results the work queue was not waiting on",
			slog.Int("count", unsolicited),
			slog.Int("queried", len(byID)),
			slog.String("provider", p.src.Name().String()),
		)
	}

	return worst, firstErr
}

// resolve maps a provider's own event key onto the identifier the database
// holds, and reports whether it could.
//
// # Only the forward direction exists
//
// internal/ingest/normalizer.EventIDFor is what wrote the identifier on the
// catalogue row in the first place, from the same key the results adapter is
// stating now, so applying it here reproduces that row's identifier exactly. It
// is a total function: a key it cannot embed verbatim — too long, or carrying a
// byte the scheme reserves — it hashes, and either way it answers.
//
// The inverse does not exist, which is why the seam is shaped the way it is. A
// derivation that hashes some of its inputs cannot be run backwards, and an
// "inverse" that worked only for the keys that happened to be embedded would
// fail silently and precisely where nobody could tell that it had. The one
// alternative that would also work — carrying the provider key in its own column
// on `events` — is a schema change to store what the forward direction already
// determines.
//
// A failure here is unreachable for any adapter that satisfies
// provider.FinalResult's contract, because the only thing EventIDFor refuses is
// an empty key. It is handled rather than ignored because the alternative is
// writing a terminal status onto whatever identifier an empty key happens to
// derive, which would be the same identifier for every such result.
func (p *Poller) resolve(r provider.FinalResult) (domain.EventID, bool) {
	id, err := normalizer.EventIDFor(p.prov, r.EventKey)
	if err != nil {
		p.log.Error("results provider stated an outcome this system cannot attribute to an event; "+
			"dropping it",
			slog.String("provider", p.src.Name().String()),
			slog.String("event_key", r.EventKey),
			slog.String("error", err.Error()),
		)
		return "", false
	}
	return id, true
}

// scoreText renders a result's final score for the log, or "-" for a cancelled
// contest that has none. It exists so the log line has one shape rather than
// two.
func scoreText(r provider.FinalResult) string {
	if !r.HasScore {
		return "-"
	}
	return r.Score.String()
}
