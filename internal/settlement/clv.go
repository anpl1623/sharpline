// The closing-line-value pass: the second loop this binary runs, and the one
// that is deliberately not allowed to matter to the first.
//
// # WHY THIS IS A SEPARATE LOOP AND NOT A STEP INSIDE settleWager
//
// This is the load-bearing decision in the file and every other choice here
// follows from it.
//
// CLV MUST NOT BE ABLE TO FAIL A SETTLEMENT. Money movement is the thing this
// service exists for; a missing CLV row is a missing report. Those two are not
// comparable, and any design in which they share a failure path has made them
// comparable by construction — a broker that will not acknowledge a signals.clv
// record would roll back a ledger movement, and a customer would go unpaid
// because an analytics number could not be published.
//
// So the measurement is not a step inside settleInTx, not a deferred call after
// it, and not an error the settlement transaction can observe. It is a separate
// goroutine reading a separate work queue, and the ONLY thing the two loops share
// is the `legs` table: settlement writes a leg's status and graded_at, and this
// pass later notices that a graded leg has no measurement. There is no code path
// by which this file can return an error into settle.go, and that is checked by
// the compiler rather than by discipline — settle.go does not import anything
// declared here and holds no reference to a [CLVPass].
//
// The consequences of that separation, stated so nobody re-couples them:
//
//   - THE PASS IS NOT A READINESS DEPENDENCY. *CLVPass deliberately does not
//     implement httpx.Checker. If it did, a wedged measurement would take the
//     replica out of rotation and stop it settling — reintroducing exactly the
//     coupling this design removes, through the orchestrator instead of through
//     the call stack.
//   - A FAILURE IS VISIBLE IN METRICS, NEVER SWALLOWED. Every leg the pass cannot
//     measure increments a counter, and clvmetrics.go labels the reason. Silence
//     is what would be dangerous here: a measurement that fails quietly is
//     indistinguishable from a market nobody bet, and the leaderboard would
//     simply be short a few hundred samples with nothing to say so.
//   - THE PASS OWNS NO CURSOR AND NO STATE. Its window is recomputed from the
//     clock on every tick and its work queue is an anti-join, so a crash costs
//     nothing and a restart re-derives everything.
//
// # RESTARTABILITY, AND WHY THE WINDOW SLIDES
//
// The queue is "graded legs with no measurement, graded inside [now − retry
// window, now)". A leg leaves it by being measured. A leg that CANNOT be measured
// — an in-play wager, a market that shut an hour before kickoff — never leaves
// it, so without the lower bound it would be retried on every pass until the end
// of time, and queries/analytics.sql says so explicitly.
//
// The sliding window is what bounds that: an unmeasurable leg is retried for one
// retry window and then ages out. Retrying at all is not pointless — a
// measurement can start succeeding without anything about the leg changing, when
// a late-arriving price is backfilled or a suspension episode is recorded after
// the fact — and giving up on the first attempt would drop those permanently.
//
// # PUBLISH FIRST, THEN PERSIST
//
// The reverse order loses records silently. A row written before a failed publish
// takes the leg off the queue for ever, and the signal that was never sent is not
// recoverable by anything — the same argument doc.go makes about wager.events,
// applied to a topic with no ledger behind it.
//
// Publishing first inverts which way it fails: the worst case is a record on
// signals.clv for a measurement whose row did not land, and the next pass sees
// the leg still queued, measures it again and republishes. Both the row and the
// topic absorb that — the row is an upsert on leg_id, and a consumer
// deduplicating on leg_id sees a recomputation of one measurement rather than two
// measurements. A duplicate that both sides are built to absorb is a better
// failure than a hole neither side can detect.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/analytics/clv"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Defaults
// -----------------------------------------------------------------------------

const (
	// DefaultCLVPollInterval is how often the work queue is read.
	//
	// Thirty seconds, and the number is chosen against what feeds the queue rather
	// than against how fast a database can be polled. A leg becomes measurable
	// only when settlement grades it, settlement polls its own feed every
	// DefaultPollInterval, and a result cannot reach `events` faster than ingest's
	// live-tier cadence. So the queue fills in bursts separated by tens of
	// seconds, and polling faster than that would mostly issue queries that match
	// nothing.
	//
	// It is six times the settlement interval on purpose: CLV is a report, and a
	// report that is thirty seconds behind the ledger is not behind at all,
	// whereas a customer's balance is.
	DefaultCLVPollInterval = 30 * time.Second

	// DefaultCLVBatch bounds how many legs one read of the work queue returns.
	//
	// Five hundred, and the number is a FAIRNESS bound rather than a memory one.
	// The queue is ordered oldest-first and every leg graded from one result
	// carries that result's finalisation instant, so a popular game's legs all
	// share one graded_at; a batch smaller than the number of legs on a single
	// game could be filled entirely by legs at one instant, and [CLVPass.pass]
	// would be unable to step past them. Five hundred is comfortably more than any
	// single event in this system carries and small enough that one read is one
	// modest result set.
	//
	// The degenerate case is still handled rather than assumed away; see
	// [CLVPass.pass].
	DefaultCLVBatch = 500

	// DefaultCLVRetryWindow is how far back a graded leg stays on the queue.
	//
	// Twenty-four hours. It is the retry budget for a leg whose measurement failed
	// for a reason that might be fixed — a backfilled price, a suspension recorded
	// late — and it is simultaneously the point at which the system stops asking a
	// question the data has already refused to answer.
	//
	// Erring long here is cheap and erring short is not, which is why it is a day
	// rather than an hour: a leg that ages out unmeasured is unmeasured for ever,
	// because nothing ever puts it back. The cost of the length is that every
	// permanently unmeasurable leg inside the window is re-attempted once per
	// pass, which is two indexed reads — visible, bounded, and reported as the
	// floor of sharpline_settlement_clv_queue_depth.
	DefaultCLVRetryWindow = 24 * time.Hour

	// DefaultCLVTimeout bounds one leg's measurement.
	//
	// CLAUDE.md §12: every external call has a timeout. This one also bounds the
	// SYNCHRONOUS PUBLISH, which is the part that can genuinely take a long time —
	// kafka.AuditProducer retries without bound by design and "the bound is the
	// caller's context deadline, which is where it belongs". Twenty seconds
	// matches [DefaultTxTimeout] so that a wedged broker looks the same on both of
	// this binary's loops.
	DefaultCLVTimeout = 20 * time.Second
)

// -----------------------------------------------------------------------------
// The seams
// -----------------------------------------------------------------------------

// CLVMeasurer measures one graded leg against its market's close.
//
// Declared here rather than in internal/analytics/clv because this package is the
// CONSUMER (CLAUDE.md §12), and declared over that package's values because
// restating a dozen fields of a measurement in a second shape would be two
// definitions of one record — which is the failure this whole phase is meant to
// avoid, one layer down.
//
// The three error kinds it returns are the contract and they are handled
// differently; see [CLVPass.measure].
type CLVMeasurer interface {
	Measure(ctx context.Context, leg clv.Leg) (clv.Measurement, error)
}

// CLVStore is the CLV pass's persistence seam: a work queue and a writer.
//
// Note what is NOT here. There is no transaction, because there is no
// multi-statement invariant to hold — one upsert on one primary key — and because
// a transaction is the mechanism by which this pass could acquire the ability to
// fail something else. internal/settlement's [Store] is declared over a [Tx] for
// precisely the opposite reason, and the difference between the two seams is the
// difference between money and a report.
type CLVStore interface {
	// GradedLegsAwaitingCLV returns graded legs with no measurement yet, graded
	// inside [from, to), oldest first, at most limit of them.
	//
	// Both bounds are required. The lower one is what stops a permanently
	// unmeasurable leg from being retried for ever; the upper one is what makes
	// one pass a bounded walk rather than a moving target.
	GradedLegsAwaitingCLV(ctx context.Context, from, to time.Time, limit int) ([]clv.Leg, error)

	// WriteLegCLV persists one measurement, upserting on the leg.
	//
	// It is called ONLY for a measurement odds.EvaluateCLV actually produced.
	// migrations/00009: absence is meaningful, and a row of nulls would put "we
	// could not measure it" and "it measured zero" in the same shape.
	WriteLegCLV(ctx context.Context, m clv.Measurement) error
}

// CLVPublisher writes closing line value to signals.clv.
//
// One method, and it is the SYNCHRONOUS one — but for a different reason from
// [AuditPublisher]'s. There, a blocked publish exists so the caller can refuse to
// commit a ledger transaction. Here there is no transaction to refuse: the
// blocking is what lets the pass know whether the record landed, so that it can
// decline to write the row and leave the leg on the queue.
//
// The consequence is the one this file's header describes: a bus outage stops
// CLV from being recorded and stops NOTHING ELSE.
type CLVPublisher interface {
	PublishCLVSignal(ctx context.Context, id domain.WagerID, msg kafka.Message) error
}

// Compile-time proof that the shipped type satisfies the declaration, here rather
// than at the composition root because a mismatch should break THIS package's
// build. There is deliberately no equivalent for [CLVStore] or [CLVMeasurer]:
// their implementations live in packages that import this one.
var _ CLVPublisher = (*kafka.AuditProducer)(nil)

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// CLVOptions are [NewCLVPass]'s dependencies. Everything is constructor-injected;
// nothing is read from a global (CLAUDE.md §12).
type CLVOptions struct {
	// Measurer computes one leg's closing line value. Required.
	Measurer CLVMeasurer

	// Store is the work queue and the writer. Required.
	Store CLVStore

	// Publisher writes signals.clv. Required, and deliberately not optional: a
	// pass built without one would write rows nothing was told about, and the
	// phase-12 validation reads the topic rather than the table.
	Publisher CLVPublisher

	// Logger is required. A pass that logged nowhere would make an unusable
	// work-queue row — the one condition here that should never happen —
	// invisible.
	Logger *slog.Logger

	// Registry receives this pass's collectors. Nil builds them unregistered,
	// which is right for a unit test. Ignored when Metrics is set.
	Registry prometheus.Registerer

	// Metrics is an already-registered collector set, for a process running more
	// than one pass. Takes precedence over Registry.
	Metrics *CLVMetrics

	// ClosingLookback and TakenLookback are the two declared parameters of the
	// closing-price definition, carried here ONLY so they can be stamped onto the
	// published record. They must be the values the [CLVMeasurer] was built with;
	// the wiring in cmd/settle reads them back off the measurer rather than
	// restating them, so there is one place they are chosen.
	//
	// Zero means the measurer's own defaults, which is only correct when the
	// measurer was also built with defaults.
	ClosingLookback time.Duration
	TakenLookback   time.Duration

	// PollInterval is the cadence. Zero means DefaultCLVPollInterval.
	PollInterval time.Duration

	// Batch bounds one read of the work queue. Zero means DefaultCLVBatch.
	Batch int

	// RetryWindow is how far back the queue looks. Zero means
	// DefaultCLVRetryWindow.
	RetryWindow time.Duration

	// Timeout bounds one leg's measurement, publish included. Zero means
	// DefaultCLVTimeout.
	Timeout time.Duration

	// Clock is the source of "now" for the queue window. Nil means time.Now.
	//
	// Injected because a test that asserts which legs a pass picked up needs it.
	// NOTHING STORED READS IT: every instant on a measurement comes from the
	// provider, and the one that does not (ComputedAt) comes from the measurer's
	// own clock rather than this one.
	Clock func() time.Time
}

func (o CLVOptions) validate() error {
	switch {
	case o.Measurer == nil:
		return fmt.Errorf("%w: CLV Measurer is nil", ErrInvalidOptions)
	case o.Store == nil:
		return fmt.Errorf("%w: CLV Store is nil", ErrInvalidOptions)
	case o.Publisher == nil:
		return fmt.Errorf("%w: CLV Publisher is nil; a pass that stored measurements nobody was "+
			"told about would be invisible to the phase-12 validation, which reads the topic",
			ErrInvalidOptions)
	case o.Logger == nil:
		return fmt.Errorf("%w: CLV Logger is nil", ErrInvalidOptions)
	case o.ClosingLookback < 0:
		return fmt.Errorf("%w: CLV ClosingLookback is negative", ErrInvalidOptions)
	case o.TakenLookback < 0:
		return fmt.Errorf("%w: CLV TakenLookback is negative", ErrInvalidOptions)
	case o.PollInterval < 0:
		return fmt.Errorf("%w: CLV PollInterval is negative", ErrInvalidOptions)
	case o.Batch < 0:
		return fmt.Errorf("%w: CLV Batch is negative", ErrInvalidOptions)
	case o.RetryWindow < 0:
		return fmt.Errorf("%w: CLV RetryWindow is negative", ErrInvalidOptions)
	case o.Timeout < 0:
		return fmt.Errorf("%w: CLV Timeout is negative", ErrInvalidOptions)
	}
	return nil
}

// -----------------------------------------------------------------------------
// CLVPass
// -----------------------------------------------------------------------------

// CLVPass measures graded legs against their markets' closes, publishes each
// measurement to signals.clv, and stores it.
//
// It is the settle binary's SECOND loop. Read this file's header before changing
// anything: the separation from the settlement loop is the design, not an
// implementation detail, and most of the choices below only make sense in the
// light of it.
type CLVPass struct {
	measurer  CLVMeasurer
	store     CLVStore
	publisher CLVPublisher

	log     *slog.Logger
	metrics *CLVMetrics
	clock   func() time.Time

	closingLookback time.Duration
	takenLookback   time.Duration

	pollInterval time.Duration
	batch        int
	retryWindow  time.Duration
	timeout      time.Duration

	// mu guards lastPassAt, which exists so an operator and a test can ask when
	// the pass last completed without racing the loop.
	mu         sync.Mutex
	lastPassAt time.Time

	// running reports whether the loop is up. It is NOT wired to /readyz — see
	// the file header — and exists for logging and for tests.
	running atomic.Bool
}

// There is deliberately NO `var _ httpx.Checker = (*CLVPass)(nil)` here.
//
// Making the CLV pass a readiness dependency would let a wedged measurement take
// the replica out of rotation, which would stop it settling — reintroducing
// through the orchestrator exactly the coupling this design removes from the call
// stack. If a future change makes this pass load-bearing enough to gate
// readiness, that is a change to the premise in the file header and belongs in an
// ADR, not in a one-line assertion added here.

// NewCLVPass validates the options and builds the pass. It performs no I/O and
// starts nothing; call Run.
func NewCLVPass(opts CLVOptions) (*CLVPass, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	m := opts.Metrics
	if m == nil {
		var err error
		if m, err = NewCLVMetrics(opts.Registry); err != nil {
			return nil, fmt.Errorf("settlement: register CLV metrics: %w", err)
		}
	}

	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	return &CLVPass{
		measurer:        opts.Measurer,
		store:           opts.Store,
		publisher:       opts.Publisher,
		log:             opts.Logger.With(slog.String("component", "settlement.clv")),
		metrics:         m,
		clock:           clock,
		closingLookback: positiveOr(opts.ClosingLookback, clv.DefaultClosingLookback),
		takenLookback:   positiveOr(opts.TakenLookback, clv.DefaultTakenLookback),
		pollInterval:    positiveOr(opts.PollInterval, DefaultCLVPollInterval),
		batch:           positiveIntOr(opts.Batch, DefaultCLVBatch),
		retryWindow:     positiveOr(opts.RetryWindow, DefaultCLVRetryWindow),
		timeout:         positiveOr(opts.Timeout, DefaultCLVTimeout),
	}, nil
}

// Metrics returns the collector set, so a process running several passes can
// share one registration.
func (p *CLVPass) Metrics() *CLVMetrics { return p.metrics }

// Running reports whether the loop is up. It is not a readiness check; see the
// note beside [NewCLVPass].
func (p *CLVPass) Running() bool { return p.running.Load() }

// LastPassAt reports when the pass last completed a walk of the work queue, or
// the zero time before the first one.
func (p *CLVPass) LastPassAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastPassAt
}

// Run walks the work queue until ctx is cancelled.
//
// It seeds nothing and holds no cursor: the window is recomputed from the clock
// on every tick, so a restart re-derives its own work and a crash costs a pass
// rather than a position.
//
// A failed pass does not stop the loop, for the reason [Service.Run] gives about
// its own: a failed read is transient by nature, and stopping would convert a
// hiccup into an outage that lasts until somebody notices a pod is CrashLooping.
// It costs even less here — nothing downstream is waiting on a measurement — so
// the failure is counted and the next tick starts over.
//
// It always returns nil. A CLV pass has no failure that should take a process
// down with it, and returning an error would let one do so through
// cmd/settle's errors.Join.
func (p *CLVPass) Run(ctx context.Context) error {
	p.log.Info("clv pass running",
		slog.String("publishes", kafka.TopicSignalsCLV),
		slog.String("message_type", CLVMessageType),
		slog.String("poll_interval", p.pollInterval.String()),
		slog.String("retry_window", p.retryWindow.String()),
		slog.Int("batch", p.batch),
		slog.String("closing_lookback", p.closingLookback.String()),
		slog.String("taken_lookback", p.takenLookback.String()),
	)

	p.running.Store(true)
	defer func() {
		p.running.Store(false)
		p.log.Info("clv pass stopped")
	}()

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	for {
		// Walk first, tick second, matching Service.Run: a pass that waited a full
		// interval before its first read would leave a backlog untouched for the
		// whole of it, which is precisely the moment somebody is watching a
		// restart.
		p.pass(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// pass walks the work queue once.
//
// # The walk, and the one degenerate case it has to handle
//
// The queue is read in batches ordered oldest-first, and the lower bound is
// advanced to the last leg's graded_at to step forward. That boundary is
// INCLUSIVE — legs.graded_at is what the query compares — so a batch can and
// routinely does re-read legs the previous batch already covered, and the `seen`
// set is what absorbs that.
//
// Stepping forward at all is the point. A naive "read the oldest N, measure them,
// wait for the next tick" starves: a leg that can never be measured stays at the
// front of the queue for its whole retry window, and if there are N of them
// nothing behind them is ever reached. Walking the window means unmeasurable legs
// cost one attempt each per pass instead of blocking every leg graded after them.
//
// The degenerate case is a batch entirely filled by legs sharing ONE graded_at,
// which cannot be stepped past because the next lower bound would be the bound we
// already used. It is possible in principle — every leg graded from one result
// carries that result's finalisation instant, so it takes more than a batch's
// worth of legs on a SINGLE event — and [DefaultCLVBatch] is sized to make it
// implausible in practice.
//
// When it does happen the walk stops and says so, and the bound on the damage is
// worth stating rather than leaving to inference: the legs beyond the batch are
// reached once the ones inside it stop occupying it, which happens either because
// they were measured or because they aged out of the retry window. The second
// path is what makes even a batch of permanently unmeasurable legs self-clearing
// rather than a permanent block, and it is why the retry window is a day rather
// than a week.
func (p *CLVPass) pass(ctx context.Context) {
	to := p.clock().UTC()
	from := to.Add(-p.retryWindow)
	seen := make(map[domain.LegID]struct{})

	for {
		if ctx.Err() != nil {
			return
		}

		legs, err := p.read(ctx, from, to)
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown, reported through the same path as a failure because a
				// stopped read and a broken one look identical from inside read. It
				// is neither counted nor logged: a deploy is not an incident.
				return
			}
			p.metrics.observePass(clvPassFailed)
			p.log.Error("reading the CLV work queue failed; the next tick starts over",
				slog.String("from", from.Format(time.RFC3339Nano)),
				slog.String("to", to.Format(time.RFC3339Nano)),
				slog.String("error", err.Error()),
			)
			return
		}
		if len(legs) == 0 {
			break
		}

		fresh := 0
		for _, leg := range legs {
			if ctx.Err() != nil {
				return
			}
			if _, dup := seen[leg.LegID]; dup {
				continue
			}
			seen[leg.LegID] = struct{}{}
			fresh++
			p.measure(ctx, leg)
		}

		if len(legs) < p.batch {
			break
		}
		if fresh == 0 {
			// Every leg in a full batch was one we had already seen, which means
			// the batch is saturated by legs sharing one graded_at and the lower
			// bound cannot advance past them. See the doc comment.
			p.log.Warn("the CLV work queue could not be advanced: a full batch shares one "+
				"grading instant; the rest of that instant's legs will be picked up once "+
				"these have measurements",
				slog.String("graded_at", legs[len(legs)-1].GradedAt.Format(time.RFC3339Nano)),
				slog.Int("batch", p.batch),
			)
			break
		}
		from = legs[len(legs)-1].GradedAt
	}

	p.metrics.observePass(clvPassOK)
	p.metrics.observeQueueDepth(len(seen))

	p.mu.Lock()
	p.lastPassAt = to
	p.mu.Unlock()
}

// read fetches one batch of the work queue under its own deadline.
func (p *CLVPass) read(ctx context.Context, from, to time.Time) ([]clv.Leg, error) {
	readCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	legs, err := p.store.GradedLegsAwaitingCLV(readCtx, from, to, p.batch)
	if err != nil {
		return nil, fmt.Errorf("settlement: read the CLV work queue [%s, %s): %w",
			from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), err)
	}
	return legs, nil
}

// measure computes, publishes and stores one leg's closing line value.
//
// # Error classification
//
// Three kinds come out of the measurer and they are treated differently on
// purpose. The classification is the whole reason [clv.Measure] returns errors
// rather than an (ok bool):
//
//	UNMEASURABLE  the data cannot answer for this leg, for one of the reasons
//	              internal/analytics/clv/doc.go enumerates. NOT A FAILURE. It is
//	              the documented outcome for an in-play wager, for a market that
//	              shut before kickoff, and for a field that lost a runner. Counted
//	              BY REASON, logged at DEBUG, no row written.
//	UNUSABLE      the work-queue row is not a graded leg. Permanent, and the one
//	              condition here that should never happen: it means the query or
//	              the schema produced something incoherent. Logged at ERROR.
//	FAILED        the store or the bus refused. Transient. Logged at WARN, the leg
//	              stays queued, the next pass retries it.
//
// Conflating the first with the third is the failure this signature exists to
// prevent: a store outage counted as "the market had no close" is an outage
// nobody can see, and a leaderboard silently computed from half a population.
//
// # Nothing here can fail anything else
//
// Every path returns void. There is no error to propagate, because there is no
// caller that should change its behaviour: [CLVPass.pass] moves to the next leg
// and the settlement loop never learns any of this happened.
func (p *CLVPass) measure(ctx context.Context, leg clv.Leg) {
	start := p.clock()

	legCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// NOT detached from the caller's context, which is the opposite of
	// Service.settleWager and is the right choice for the opposite reason. There,
	// a context cancelled mid-transaction would abandon a ledger movement, so the
	// transaction is deliberately allowed to outlive SIGTERM. Here nothing is
	// committed until the very last statement and every step is idempotent, so
	// being interrupted costs one repeated measurement on the next pass — which is
	// strictly better than holding a shutdown open for work that nobody is waiting
	// for.
	m, err := p.measurer.Measure(legCtx, leg)
	if err != nil {
		p.classify(leg, err, p.clock().Sub(start))
		return
	}

	if err := p.publishCLV(legCtx, m); err != nil {
		p.metrics.observePublishFailure()
		p.metrics.observeLeg(clvLegFailed, p.clock().Sub(start))
		p.log.Warn("publishing a closing line value failed; no row was written and the leg "+
			"stays on the queue",
			slog.String("leg_id", leg.LegID.String()),
			slog.String("wager_id", leg.WagerID.String()),
			slog.String("topic", kafka.TopicSignalsCLV),
			slog.String("error", err.Error()),
		)
		return
	}

	if err := p.store.WriteLegCLV(legCtx, m); err != nil {
		p.metrics.observeLeg(clvLegFailed, p.clock().Sub(start))
		p.log.Warn("storing a closing line value failed after it was published; the leg stays "+
			"on the queue and the next pass will republish and rewrite it",
			slog.String("leg_id", leg.LegID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	outcome := clvLegMeasured
	if m.Result.LineMoved {
		outcome = clvLegLineMoved
	}
	p.metrics.observeLeg(outcome, p.clock().Sub(start))
}

// classify records why a leg produced no measurement.
func (p *CLVPass) classify(leg clv.Leg, err error, took time.Duration) {
	if reason, ok := clv.ReasonFor(err); ok {
		p.metrics.observeUnmeasurable(reason)
		p.metrics.observeLeg(clvLegUnmeasurable, took)
		// DEBUG, not WARN. An unmeasurable leg is an expected outcome with a
		// documented cause, and every in-play wager on the system produces one; a
		// WARN per leg would train somebody to ignore the level. The counter,
		// labelled by reason, is where this is meant to be read.
		p.log.Debug("leg has no closing line value",
			slog.String("leg_id", leg.LegID.String()),
			slog.String("market_id", leg.MarketID.String()),
			slog.String("reason", reason.String()),
			slog.String("detail", err.Error()),
		)
		return
	}

	if errors.Is(err, clv.ErrUnusableLeg) {
		p.metrics.observeLeg(clvLegUnusable, took)
		p.log.Error("the CLV work queue returned a row that is not a graded leg; skipping it",
			slog.String("leg_id", leg.LegID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	p.metrics.observeLeg(clvLegFailed, took)
	p.log.Warn("measuring a leg's closing line value failed; it stays on the queue",
		slog.String("leg_id", leg.LegID.String()),
		slog.String("market_id", leg.MarketID.String()),
		slog.String("error", err.Error()),
	)
}

// publishCLV writes one measurement to signals.clv and blocks until the broker
// has acknowledged it.
//
// The record is validated before it is sent, for the reason [Service.publish]
// gives about its own: [LegCLV.Validate] re-checks the four identities on the
// values as they will be decoded rather than on the domain values they came from,
// and publishing a record that fails them would put a self-contradicting row on a
// topic whose whole purpose is to be reproducible by a second implementation.
func (p *CLVPass) publishCLV(ctx context.Context, m clv.Measurement) error {
	rec := newLegCLV(m, p.closingLookback, p.takenLookback)
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("settlement: refusing to publish an incoherent CLV record: %w", err)
	}

	msg := kafka.Message{
		Type: CLVMessageType,
		// The leg, which is wager_leg_clv's whole primary key — so a consumer
		// deduplicating across recomputations has a stable identity without
		// decoding the payload. It is deliberately not the wager: a parlay
		// produces one record per leg and they must not collide.
		ID: m.Leg.LegID.String(),
		// The leg's grading instant, which is the RESULT's own finalisation
		// instant and therefore the provider's clock.
		//
		// NOT ClosedAt, and the difference matters. ClosedAt is the market's close,
		// which is hours older than this record by construction, so using it would
		// report a staleness of hours on a record that was produced seconds after
		// the fact and would poison the headline SLO with a number that describes
		// the sport's schedule rather than the pipeline's health. NOT time.Now
		// either, for the reason kafka.Message states: a placeholder there
		// "produces a staleness measurement of zero and makes the headline SLO
		// report perfect health for data that has none".
		ObservedAt: m.Leg.GradedAt,
		Payload:    rec,
	}

	if err := p.publisher.PublishCLVSignal(ctx, m.Leg.WagerID, msg); err != nil {
		return fmt.Errorf("settlement: publish leg %s CLV to %s: %w",
			m.Leg.LegID, kafka.TopicSignalsCLV, err)
	}
	return nil
}
