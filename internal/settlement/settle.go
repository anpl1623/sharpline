// The service: the poll loop, the per-wager transaction, and the money rules
// for what a graded ticket returns.
//
// Read doc.go first. It carries the argument for why the results feed is
// pipeline output rather than fixture data, why the audit record is published
// before the transaction commits, why settlement is idempotent in three
// independent places, and why a failed transaction is never retried in place.
// This file is the code those arguments describe.
package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Defaults. Each is overridable through ServiceOptions; zero means the default.
// -----------------------------------------------------------------------------

const (
	// DefaultPollInterval is how often the results feed is read when there is
	// nothing to drain.
	//
	// Five seconds, and the number is chosen against what is UPSTREAM of it
	// rather than against how fast a database can be polled. A final score
	// cannot reach the `events` table faster than ingest's live-tier cadence
	// (ADR 0003's ladder, 90s by default), so polling every second would issue
	// ninety queries for every one that could possibly find something new.
	// Five seconds is small enough to be invisible in
	// sharpline_settlement_results_lag_seconds — whose floor is already tens of
	// seconds for exactly that reason — and large enough that an idle slate
	// costs one indexed query every five seconds.
	//
	// A full batch does NOT wait for the next tick; see Service.poll. The
	// interval governs the idle case, not the drain.
	DefaultPollInterval = 5 * time.Second

	// DefaultResultBatch bounds how many results one poll reads.
	//
	// It is a memory bound and a fairness bound rather than a throughput knob.
	// Each result costs one pending-leg query and then one transaction per
	// affected ticket, so a batch is a unit of work whose cost is not known in
	// advance; 128 finished games is far more than any real slate produces at
	// one instant and small enough that a backlog drains in visible, restartable
	// steps rather than as one enormous poll that a SIGTERM would abandon.
	DefaultResultBatch = 128

	// DefaultPendingLegBatch bounds how many ungraded legs one result's query
	// returns.
	//
	// It is a safety ceiling, not an expectation: a popular game carries
	// hundreds of tickets, not thousands, and a value this high is only ever
	// reached by a load test. It exists because an unbounded read is how one
	// event with a pathological number of open legs turns a poll into an
	// out-of-memory kill, and because a bounded read that fills its bound is a
	// legible signal — the leftover legs are picked up on the next poll, since
	// the cursor cannot advance past a result whose tickets did not all settle.
	DefaultPendingLegBatch = 2000

	// DefaultTxTimeout bounds one database transaction.
	//
	// CLAUDE.md §12: every external call has a timeout. This one also bounds the
	// SYNCHRONOUS PUBLISH that happens inside the transaction, which is the part
	// that can genuinely take a long time: kafka.AuditProducer retries without
	// bound by design and "the bound is the caller's context deadline, which is
	// where it belongs". This is that deadline. Twenty seconds is long enough to
	// ride out a leader election and short enough that a wedged broker does not
	// hold a Postgres backend open past
	// idle_in_transaction_session_timeout (60s, deploy/postgres/postgresql.conf).
	DefaultTxTimeout = 20 * time.Second

	// DefaultResumeLookback is subtracted from the resume cursor at startup.
	//
	// [Store.OldestUnsettledAt] gives a sound lower bound in principle: a ticket
	// cannot be waiting on a result that was already final when the ticket was
	// written. In practice the two instants come off different clocks — the
	// placement instant is this system's, the result instant is the provider's —
	// so an exactly-tight bound would be wrong by whatever skew exists between
	// them, and being wrong in that direction means silently never settling a
	// ticket.
	//
	// An hour is far beyond any plausible skew and costs almost nothing: the
	// extra results it re-reads are ones whose legs are already graded, so each
	// one is a single indexed query that matches zero pending legs. Erring long
	// here is cheap; erring short is a customer's stake stuck in escrow.
	DefaultResumeLookback = time.Hour
)

// The drain budget at shutdown is DefaultTxTimeout and nothing else, and there
// is deliberately no ShutdownGrace knob.
//
// Run's loop is synchronous: it calls drain, drain calls settleWager, and
// settleWager blocks on one transaction under a DETACHED context with its own
// deadline. So "wait for the in-flight settlement to finish" is not something
// Run has to arrange — it is what the call stack already does, and the bound is
// the transaction's own timeout. A separate grace period would be a second
// number that had to stay consistent with the first, and the failure mode of
// getting it wrong (a grace shorter than the timeout) is a context cancelled
// mid-transaction, which is exactly what the detachment exists to prevent.
//
// The consequence for an orchestrator: a settle replica can take up to
// DefaultTxTimeout to exit after SIGTERM, which fits inside Kubernetes' default
// terminationGracePeriodSeconds (30s). If it does not fit — if the pod is
// SIGKILLed mid-transaction — nothing is lost anyway: Postgres rolls back a
// transaction whose connection dies, and the next poll re-reads the result.

// settlementIDPrefix marks a ledger transaction as a settlement.
//
// The dot is in domain.Slug's forbidden set and in nothing else here: it is a
// legal identifier byte ([A-Za-z0-9._-]) but is not produced by any hash
// encoding, so "does this transaction id begin with stl." is an unambiguous
// question about provenance rather than a prefix that a wager identifier could
// coincidentally share.
const settlementIDPrefix = "stl."

// settlementIDDigestLen is how many hex characters of the wager digest are kept
// when the readable form would not fit. Forty hex characters is 160 bits, which
// is not a cryptographic claim — nothing here is adversarial — but is far past
// the point where a collision among the wagers one book will ever write is a
// consideration.
const settlementIDDigestLen = 40

// -----------------------------------------------------------------------------
// The deterministic ledger identifier
// -----------------------------------------------------------------------------

// SettlementTransactionID returns the ledger transaction identifier for settling
// one wager.
//
// It is a PURE FUNCTION OF THE WAGER IDENTIFIER, and that is the entire point.
// doc.go lists it as the third of three independent idempotency guards and
// explains why it is the one that matters most: a duplicate payout written under
// a fresh random identifier is a perfectly balanced, entirely legal-looking pair
// of ledger rows that no constraint in migrations/00006 would object to. Derived
// from the wager, a replay attempts to insert the same primary key, hits
// ledger_transactions_pkey, and is reported as [ErrTransactionExists] — which
// settle.go treats as "already paid", not as a failure.
//
// It is exported so that a test can re-derive it, and so that the API can answer
// "show me the ledger movement that settled this ticket" with a point lookup
// rather than a scan over ledger_transactions.wager_id.
//
// # Two forms, one of which will almost never be seen
//
// The readable form is the prefix followed by the wager identifier, so a human
// reading a ledger row can see which ticket it belongs to without a join. When
// that would exceed domain.MaxIDLen the digest form is used instead. The
// fallback is not decoration: domain.NewTransactionID refuses an over-long
// identifier, and a settlement that could not name itself would be an
// unsettleable ticket rather than an ugly one. Both forms are deterministic, so
// the idempotency guarantee is identical either way — and the ledger row carries
// wager_id as a column regardless, so nothing is lost but legibility.
func SettlementTransactionID(w domain.WagerID) (domain.TransactionID, error) {
	if _, err := domain.NewWagerID(string(w)); err != nil {
		return "", fmt.Errorf("settlement: transaction id for %q: %w", w, err)
	}

	raw := settlementIDPrefix + string(w)
	if len(raw) > domain.MaxIDLen {
		sum := sha256.Sum256([]byte(w))
		raw = settlementIDPrefix + hex.EncodeToString(sum[:])[:settlementIDDigestLen]
	}

	id, err := domain.NewTransactionID(raw)
	if err != nil {
		return "", fmt.Errorf("settlement: transaction id for %q: %w", w, err)
	}
	return id, nil
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// ServiceOptions are New's dependencies. Everything is constructor-injected;
// nothing is read from a global (CLAUDE.md §12).
type ServiceOptions struct {
	// Results is the results feed. Required — there is no default source,
	// because a settle service that read nothing would look exactly like a
	// settle service on a quiet Sunday.
	Results ResultsSource

	// Store is the persistence seam. Required.
	Store Store

	// Publisher writes the audit trail to wager.events. REQUIRED, and
	// deliberately not optional: doc.go's ordering rule makes a refused publish
	// abort the settlement, so a nil publisher would not merely lose the audit
	// trail — it would remove the interlock that guarantees there is one.
	Publisher AuditPublisher

	// Logger is required. A settle service that logs nowhere makes an ungradable
	// leg — a customer's stake stuck in escrow with nothing to release it —
	// invisible.
	Logger *slog.Logger

	// Registry receives this package's collectors. Nil builds them unregistered,
	// which is right for a unit test and for a process with no /metrics
	// endpoint. Ignored when Metrics is set.
	Registry prometheus.Registerer

	// Metrics is an already-registered collector set, for a process running more
	// than one Service. Takes precedence over Registry.
	Metrics *Metrics

	// PollInterval is the idle cadence. Zero means DefaultPollInterval.
	PollInterval time.Duration

	// ResultBatch bounds one poll's read. Zero means DefaultResultBatch.
	ResultBatch int

	// PendingLegBatch bounds one result's pending-leg read. Zero means
	// DefaultPendingLegBatch.
	PendingLegBatch int

	// TxTimeout bounds one transaction, publish included. Zero means
	// DefaultTxTimeout.
	TxTimeout time.Duration

	// ResumeLookback is subtracted from the startup cursor. Zero means
	// DefaultResumeLookback.
	ResumeLookback time.Duration

	// Clock is the source of "now" for the results-lag histogram, the cursor
	// gauge and the empty-database resume. Nil means time.Now.
	//
	// Injected because a test that asserts an exact lag rather than a bound
	// needs it, and because CLAUDE.md §12 forbids reaching for a global.
	// NOTHING STORED READS IT: every instant written to a leg, a wager or a
	// ledger row comes from the result's own finalisation instant, which came
	// from the provider.
	Clock func() time.Time
}

func (o ServiceOptions) validate() error {
	switch {
	case o.Results == nil:
		return fmt.Errorf("%w: Results is nil; a settle service that read no results would be "+
			"indistinguishable from one with nothing to settle", ErrInvalidOptions)
	case o.Store == nil:
		return fmt.Errorf("%w: Store is nil", ErrInvalidOptions)
	case o.Publisher == nil:
		return fmt.Errorf("%w: Publisher is nil; the audit publish is what interlocks the "+
			"settlement transaction, so there is no correct way to run without one", ErrInvalidOptions)
	case o.Logger == nil:
		return fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	case o.PollInterval < 0:
		return fmt.Errorf("%w: PollInterval is negative", ErrInvalidOptions)
	case o.ResultBatch < 0:
		return fmt.Errorf("%w: ResultBatch is negative", ErrInvalidOptions)
	case o.PendingLegBatch < 0:
		return fmt.Errorf("%w: PendingLegBatch is negative", ErrInvalidOptions)
	case o.TxTimeout < 0:
		return fmt.Errorf("%w: TxTimeout is negative", ErrInvalidOptions)
	case o.ResumeLookback < 0:
		return fmt.Errorf("%w: ResumeLookback is negative; it is SUBTRACTED from the resume "+
			"cursor, so a negative value would move the cursor forward and skip results",
			ErrInvalidOptions)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Service
// -----------------------------------------------------------------------------

// Service reads the results feed, grades every ungraded leg on a finished event,
// and settles the tickets that are thereby decided.
type Service struct {
	results   ResultsSource
	store     Store
	publisher AuditPublisher

	log     *slog.Logger
	metrics *Metrics
	clock   func() time.Time

	pollInterval   time.Duration
	resultBatch    int
	legBatch       int
	txTimeout      time.Duration
	resumeLookback time.Duration

	// mu guards cursor. Polls are sequential on Run's goroutine, so the
	// contention that matters is between the loop and a reader of Cursor — which
	// exists so an operator, and a test, can ask where settlement has reached
	// without racing the loop.
	mu     sync.Mutex
	cursor time.Time

	// running gates the readiness checker. It is false before Run has started
	// the loop and false again once it has stopped, so /readyz reports the truth
	// during both a cold start and a drain rather than only in between.
	running atomic.Bool
}

// Compile-time proof that Service satisfies the readiness contract. It is here
// rather than at the composition root because a mismatch should break THIS
// package's build.
var _ httpx.Checker = (*Service)(nil)

// New validates the options and builds the service. It performs no I/O and
// starts nothing; call Run.
func New(opts ServiceOptions) (*Service, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	m := opts.Metrics
	if m == nil {
		var err error
		if m, err = NewMetrics(opts.Registry); err != nil {
			return nil, fmt.Errorf("settlement: register metrics: %w", err)
		}
	}

	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	return &Service{
		results:        opts.Results,
		store:          opts.Store,
		publisher:      opts.Publisher,
		log:            opts.Logger.With(slog.String("component", "settlement.service")),
		metrics:        m,
		clock:          clock,
		pollInterval:   positiveOr(opts.PollInterval, DefaultPollInterval),
		resultBatch:    positiveIntOr(opts.ResultBatch, DefaultResultBatch),
		legBatch:       positiveIntOr(opts.PendingLegBatch, DefaultPendingLegBatch),
		txTimeout:      positiveOr(opts.TxTimeout, DefaultTxTimeout),
		resumeLookback: positiveOr(opts.ResumeLookback, DefaultResumeLookback),
	}, nil
}

// Metrics returns the collector set, so a process running several Services can
// share one registration.
func (s *Service) Metrics() *Metrics { return s.metrics }

// Name implements httpx.Checker. It is the key this dependency appears under in
// the /readyz payload.
func (s *Service) Name() string { return "settle" }

// Check implements httpx.Checker: the service is ready when its poll loop is
// running. See [ErrNotRunning] for why that is a different question from
// "Postgres is reachable".
func (s *Service) Check(context.Context) error {
	if !s.running.Load() {
		return ErrNotRunning
	}
	return nil
}

// Cursor reports the instant settlement has consumed the results feed up to.
// Every result finalised strictly before it has been acted on.
func (s *Service) Cursor() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *Service) setCursor(at time.Time) {
	s.mu.Lock()
	// Monotonic by construction: the feed is ordered ascending and the cursor
	// only ever advances to a result already read. The guard is here anyway
	// because a cursor that could move backwards would re-settle history, and
	// the cost of the comparison is nothing against the cost of finding out the
	// hard way.
	if at.After(s.cursor) {
		s.cursor = at
	}
	current := s.cursor
	s.mu.Unlock()
	s.metrics.observeCursor(current)
}

// -----------------------------------------------------------------------------
// The loop
// -----------------------------------------------------------------------------

// Run seeds the results cursor, then polls the feed until ctx is cancelled.
//
// # Why a poll failure does not stop the loop
//
// The results feed is a database read. A failed read is transient by nature —
// a connection recycled, a deadline that was a little tight — and stopping the
// service would convert a five-second hiccup into a settlement outage that lasts
// until somebody notices a pod is CrashLooping. The failure is counted
// (sharpline_settlement_polls_total{outcome="failed"}) and logged, the cursor
// holds, and the next tick tries again from exactly where the last one left off.
//
// # Why a failed SETTLEMENT holds the cursor
//
// A result whose tickets did not all settle does not advance the cursor past
// itself, so the next poll re-reads it. That is the retry, and doc.go explains
// why it is the only acceptable form of one: it re-derives the wager, the legs
// and the arithmetic from the current database state instead of re-issuing a
// computation made against a state that has moved on.
//
// # Shutdown
//
// The loop stops at a poll boundary. A settlement already in flight runs to
// completion under a detached context, because a context cancelled in the middle
// of a ledger transaction is the exact failure postgres/tx.go's rollback helper
// was written to survive and there is no reason to inflict it deliberately once
// per deploy. Run waits for it because the call stack is synchronous, and the
// bound is that transaction's own TxTimeout — see the note beside the defaults
// for why there is no second grace knob.
func (s *Service) Run(ctx context.Context) error {
	if err := s.resume(ctx); err != nil {
		return err
	}

	s.log.Info("settle running",
		slog.String("publishes", kafka.TopicWagerEvents),
		slog.String("message_type", MessageType),
		slog.String("poll_interval", s.pollInterval.String()),
		slog.Int("result_batch", s.resultBatch),
		slog.String("cursor", s.Cursor().Format(time.RFC3339Nano)),
	)

	s.running.Store(true)
	defer func() {
		s.running.Store(false)
		s.log.Info("settle loop stopped", slog.String("cursor", s.Cursor().Format(time.RFC3339Nano)))
	}()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		// Poll first, tick second. A service that waited a full interval before
		// its first read would leave a backlog untouched for the whole of it,
		// which is precisely the moment somebody is watching a restart.
		s.drain(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// resume seeds the cursor from the oldest ticket still holding escrow.
//
// A failure here is FATAL, deliberately. The alternative — start from the
// current instant and carry on — produces a service that runs, reports itself
// healthy, and silently never settles the tickets whose games finished while it
// was starting. Refusing to start is loud, is caught by the orchestrator, and is
// recoverable in seconds; under-settling quietly is discovered by a customer.
func (s *Service) resume(ctx context.Context) error {
	readCtx, cancel := context.WithTimeout(ctx, s.txTimeout)
	defer cancel()

	at, found, err := s.store.OldestUnsettledAt(readCtx)
	if err != nil {
		return fmt.Errorf("settlement: seed the results cursor: %w", err)
	}
	if !found {
		// Nothing is open, so no historical result can pay anybody. Starting
		// from now is not a shortcut here — it is the correct answer, and it is
		// what makes a fresh database cheap to start against.
		now := s.clock()
		s.setCursor(now)
		s.log.Info("no open wagers; settling results from now",
			slog.String("cursor", now.Format(time.RFC3339Nano)))
		return nil
	}

	from := at.Add(-s.resumeLookback)
	s.setCursor(from)
	s.log.Info("resuming from the oldest open wager",
		slog.String("oldest_open_placed_at", at.Format(time.RFC3339Nano)),
		slog.String("lookback", s.resumeLookback.String()),
		slog.String("cursor", from.Format(time.RFC3339Nano)),
	)
	return nil
}

// drain reads and processes results until the feed is caught up, the context is
// cancelled, or something fails.
//
// A FULL batch means there is more behind it, so the loop goes straight round
// again instead of waiting for the next tick. That is what lets a service that
// has been down work through a backlog at the speed of the database rather than
// at the speed of the poll interval — and the shape of that drain is visible in
// sharpline_settlement_cursor_timestamp_seconds climbing toward now.
func (s *Service) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		read, err := s.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown, reported through the same return path as a failure
				// because a stopped batch and a broken one look identical from
				// inside poll. It is neither counted nor logged as a failure:
				// paging somebody because a deploy happened is how a metric
				// stops being read.
				return
			}
			s.metrics.observePoll(pollFailed)
			s.log.Error("polling the results feed failed; the cursor holds and the next tick retries",
				slog.String("cursor", s.Cursor().Format(time.RFC3339Nano)),
				slog.String("error", err.Error()),
			)
			return
		}
		s.metrics.observePoll(pollOK)

		if read < s.resultBatch {
			return
		}
	}
}

// poll reads one batch of results and processes it in order.
//
// It returns how many results were READ, not how many settled: a caller deciding
// whether to go round again wants to know whether the feed had more to give.
func (s *Service) poll(ctx context.Context) (int, error) {
	readCtx, cancel := context.WithTimeout(ctx, s.txTimeout)
	defer cancel()

	from := s.Cursor()
	results, err := s.results.Since(readCtx, from, s.resultBatch)
	if err != nil {
		return 0, fmt.Errorf("settlement: read results since %s: %w",
			from.Format(time.RFC3339Nano), err)
	}

	for _, res := range results {
		if ctx.Err() != nil {
			// A clean stop at a result boundary. The cursor stays where the last
			// completed result put it, so nothing is skipped.
			return len(results), nil
		}
		if err := s.handleResult(ctx, res); err != nil {
			// The cursor is parked ON this result rather than before it. The
			// feed's boundary is inclusive, so the next poll re-reads exactly
			// this result and everything after it, and skips the ones already
			// done — which is cheap, because a result whose legs are all graded
			// matches zero pending legs.
			s.setCursor(res.FinalisedAt)
			return len(results), err
		}
		s.setCursor(res.FinalisedAt)
	}
	return len(results), nil
}

// handleResult acts on one finished event.
func (s *Service) handleResult(ctx context.Context, res Result) error {
	lag := s.clock().Sub(res.FinalisedAt)

	if err := res.Validate(); err != nil {
		// PERMANENT. Re-reading the same row cannot change it, so it is counted,
		// logged once, and stepped over rather than left to block every later
		// result behind it. A source producing these is producing a bug, and
		// sharpline_settlement_results_total{disposition="unusable"} is where
		// that becomes visible.
		s.metrics.observeResult(resultUnusable, 0)
		s.log.Error("results feed returned a row that is not a result; skipping it",
			slog.String("event_id", res.EventID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	refs, err := s.pendingLegs(ctx, res.EventID)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		// Nobody had a bet on this game. The overwhelmingly common case on a
		// real slate, and not a condition worth logging at anything but debug.
		s.metrics.observeResult(resultIdle, lag)
		return nil
	}
	s.metrics.observeResult(resultSettled, lag)

	for _, group := range groupByWager(refs) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.settleWager(ctx, res, group.wagerID, group.legs); err != nil {
			return err
		}
	}
	return nil
}

// pendingLegs reads every ungraded leg riding on one event.
//
// It runs in its own transaction, separate from the per-wager settlements that
// follow, because it spans wagers and they must not: a parlay's outcome is a
// function of all its legs at once, so the unit of atomicity is one TICKET.
// Holding one transaction open across every ticket on a popular game would
// serialise the whole slate behind the slowest publish on it.
//
// The cost of the split is a window in which a leg is placed after this read and
// before that ticket's transaction. It is closed on the other side: settleWager
// refuses to settle a ticket that still has an ungraded leg on this event which
// this read did not see, so the result is retried rather than settled short.
func (s *Service) pendingLegs(ctx context.Context, id domain.EventID) ([]LegRef, error) {
	readCtx, cancel := context.WithTimeout(ctx, s.txTimeout)
	defer cancel()

	var refs []LegRef
	err := s.store.InTx(readCtx, func(ctx context.Context, tx Tx) error {
		got, err := tx.PendingLegsForEvent(ctx, id, s.legBatch)
		if err != nil {
			return err
		}
		refs = got
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("settlement: pending legs for event %s: %w", id, err)
	}
	return refs, nil
}

// wagerGroup is one ticket's share of a result's pending legs.
type wagerGroup struct {
	wagerID domain.WagerID
	legs    []LegRef
}

// groupByWager collects a result's pending legs by ticket, preserving the order
// the store returned them in so that two replicas walking the same result take
// the same wagers in the same order — which turns a contended settlement into a
// short row-lock wait rather than into a deadlock.
func groupByWager(refs []LegRef) []wagerGroup {
	index := make(map[domain.WagerID]int, len(refs))
	out := make([]wagerGroup, 0, len(refs))
	for _, ref := range refs {
		at, ok := index[ref.WagerID]
		if !ok {
			index[ref.WagerID] = len(out)
			out = append(out, wagerGroup{wagerID: ref.WagerID, legs: []LegRef{ref}})
			continue
		}
		out[at].legs = append(out[at].legs, ref)
	}
	return out
}

// -----------------------------------------------------------------------------
// Settling one ticket
// -----------------------------------------------------------------------------

// settleWager grades one ticket's legs on this event and, if that decides the
// ticket, pays it — all in one transaction.
//
// # The order inside the transaction, and why the publish is last
//
//	load the ticket        (locked, so a concurrent replica queues rather than races)
//	grade its legs on this event
//	if legs remain ungraded → commit; the ticket stays open
//	decide the outcome and the amount
//	write the ticket's terminal row
//	write the balanced ledger movement
//	PUBLISH to wager.events
//	COMMIT
//
// doc.go argues the load-bearing part: the publish happens INSIDE the
// transaction so that a refused publish aborts the settlement. It is placed LAST
// inside it rather than earlier because every statement before it can still
// fail, and each one that runs after a successful publish widens the window in
// which an event exists on the audit trail for a settlement that was rolled
// back. Last-inside-the-transaction is the narrowest that window can be made
// without a transactional outbox.
//
// # Errors are classified, not merely returned
//
// Three kinds come out of this, and they are treated differently on purpose:
//
//	CONFLICT   another replica or an earlier redelivery got there first. Counted,
//	           not logged as a failure, and reported as success — there is
//	           nothing to fix and nothing to retry.
//	PERMANENT  this ticket cannot be settled by this build: an unreadable leg, a
//	           market with no grading rule, an arithmetic fault. Counted, logged
//	           at ERROR, and reported as success so that one wedged ticket does
//	           not hold every later result hostage. The stake stays in escrow and
//	           the metric is the alarm.
//	TRANSIENT  everything else. Returned, so the cursor holds and the next poll
//	           retries from a fresh read.
func (s *Service) settleWager(ctx context.Context, res Result, id domain.WagerID, refs []LegRef) error {
	start := s.clock()

	// DETACHED from the caller's context, with its own deadline. A settlement
	// already in flight when SIGTERM arrives finishes; see doc.go's shutdown
	// rule. The deadline is what bounds it, and it is also what bounds the
	// unbounded retries inside the audit publish.
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.txTimeout)
	defer cancel()

	var (
		outcome = outcomeFailed
		graded  []domain.LegStatus
	)

	err := s.store.InTx(txCtx, func(ctx context.Context, tx Tx) error {
		decided, statuses, err := s.settleInTx(ctx, tx, res, id, refs)
		outcome, graded = decided, statuses
		return err
	})

	took := s.clock().Sub(start)

	switch {
	case err == nil:
		s.metrics.observeWager(outcome, took)
		for _, st := range graded {
			s.metrics.observeLeg(st)
		}
		return nil

	case isConflict(err):
		// Somebody else settled it. Not a failure and not worth an ERROR line:
		// with two replicas and an inclusive cursor boundary this is the
		// expected outcome of a race the design already made safe.
		s.metrics.observeWager(outcomeAlreadyDone, took)
		s.log.Debug("wager was settled concurrently; nothing to do",
			slog.String("wager_id", id.String()),
			slog.String("event_id", res.EventID.String()),
			slog.String("reason", err.Error()),
		)
		return nil

	case isPermanent(err):
		s.metrics.observeWager(outcomeUnusable, took)
		s.log.Error("wager cannot be settled by this build; it will stay open and its stake "+
			"will stay in escrow until the cause is fixed",
			slog.String("wager_id", id.String()),
			slog.String("event_id", res.EventID.String()),
			slog.String("error", err.Error()),
		)
		return nil

	default:
		s.metrics.observeWager(outcomeFailed, took)
		return fmt.Errorf("settlement: settle wager %s on event %s: %w", id, res.EventID, err)
	}
}

// settleInTx is the transaction body. It returns the metric label for what it
// decided and the statuses of the legs it graded, so the caller can record both
// only if the transaction actually commits.
func (s *Service) settleInTx(
	ctx context.Context,
	tx Tx,
	res Result,
	id domain.WagerID,
	refs []LegRef,
) (string, []domain.LegStatus, error) {
	w, err := tx.WagerWithLegs(ctx, id)
	if err != nil {
		return outcomeFailed, nil, err
	}
	if w.IsTerminal() {
		// Already closed. Committing an empty transaction is the cheapest
		// correct answer: there is nothing to roll back, and treating it as a
		// conflict error would make a routine redelivery look like contention.
		return outcomeAlreadyDone, nil, nil
	}

	// The instant every leg is stamped with, and the instant the ticket
	// transitions at.
	//
	// It is the RESULT's own finalisation instant, so that a replayed or
	// redelivered result re-applies the original grading time rather than the
	// wall clock — the guarantee domain.Leg.WithStatus makes and the
	// legs_assert_transition trigger enforces.
	//
	// Floored at the ticket's last transition because domain.Wager.stamp refuses
	// a transition that precedes the one before it (ErrStaleUpdate) and
	// migrations/00006's wagers_transitioned_after_placed says the same thing
	// about placement. A result older than the ticket means a bet was accepted
	// on a game that had already finished, which the betting service refuses —
	// but if one ever exists, the ledger must still be able to close it, and
	// refusing to settle would hold the customer's stake for ever over a
	// timestamp. The floor is a function of stored state, so it is as
	// deterministic as the instant it replaces.
	at := res.FinalisedAt
	if at.Before(w.UpdatedAt()) {
		at = w.UpdatedAt()
	}

	graded := make([]domain.LegStatus, 0, len(refs))
	for _, ref := range refs {
		leg, ok := w.Leg(ref.LegID)
		if !ok {
			return outcomeFailed, nil, fmt.Errorf("%w: leg %s was listed under wager %s but is "+
				"not on it: %w", ErrUnusableLeg, ref.LegID, id, domain.ErrMismatchedParent)
		}
		if leg.Status() != domain.LegStatusPending {
			// Graded by an earlier redelivery. The domain would accept a
			// same-status re-grade as idempotent, but the write is skipped
			// anyway: Tx.GradeLeg is conditional on the leg being pending and
			// would report zero rows, and there is no reason to ask the database
			// a question whose answer is already in hand.
			continue
		}

		status, err := Grade(ref, res)
		if err != nil {
			return outcomeFailed, nil, err
		}
		next, err := w.GradeLeg(ref.LegID, status, at)
		if err != nil {
			return outcomeFailed, nil, err
		}
		if err := tx.GradeLeg(ctx, ref.LegID, status, at); err != nil {
			return outcomeFailed, nil, err
		}
		w = next
		graded = append(graded, status)
	}

	// Close the window pendingLegs opened. A leg on this event that is still
	// pending, and that the pending-leg read did not hand us, means a bet landed
	// between the two transactions. Settling now would grade the ticket short —
	// so the transaction is abandoned and the next poll re-reads the result with
	// the new leg in it. TRANSIENT on purpose: the cursor holds.
	if missed := ungradedLegsOn(w, res.EventID); missed > 0 {
		return outcomeFailed, nil, fmt.Errorf("wager %s has %d ungraded leg(s) on event %s that "+
			"the pending-leg read did not list; retrying on the next poll", id, missed, res.EventID)
	}

	if !w.AllLegsGraded() {
		// The ticket has legs on games that have not finished. wager.go names
		// AllLegsGraded "the precondition for grading the ticket itself", and
		// this is that precondition failing: the legs are written, the ticket
		// stays open, and its remaining events will decide it later.
		return outcomeDeferred, graded, nil
	}

	decided, err := decideTicket(w)
	if err != nil {
		return outcomeFailed, nil, err
	}
	if decided.note != "" {
		s.log.Info("ticket settled under a house policy rather than by its legs alone",
			slog.String("wager_id", id.String()),
			slog.String("kind", w.Kind().String()),
			slog.String("outcome", decided.status.String()),
			slog.String("policy", decided.note),
		)
	}

	settled, err := w.Settle(decided.status, decided.returned, at)
	if err != nil {
		return outcomeFailed, nil, err
	}

	txID, err := SettlementTransactionID(settled.ID())
	if err != nil {
		return outcomeFailed, nil, err
	}
	movement, err := domain.NewSettlementTransaction(txID, settled, at)
	if err != nil {
		return outcomeFailed, nil, err
	}

	if err := tx.SettleWager(ctx, settled); err != nil {
		return outcomeFailed, nil, err
	}
	if err := tx.InsertTransaction(ctx, movement); err != nil {
		return outcomeFailed, nil, err
	}
	if err := s.publish(ctx, settled, movement, res); err != nil {
		return outcomeFailed, nil, err
	}

	label, err := wagerOutcomeLabel(settled.Status())
	if err != nil {
		return outcomeFailed, nil, err
	}
	return label, graded, nil
}

// ungradedLegsOn counts the legs of w that ride on one event and are still
// pending.
func ungradedLegsOn(w domain.Wager, event domain.EventID) int {
	n := 0
	for _, leg := range w.Legs() {
		if leg.EventID() == event && leg.Status() == domain.LegStatusPending {
			n++
		}
	}
	return n
}

// publish writes the settlement to wager.events and blocks until the broker has
// acknowledged it.
//
// The record is validated before it is sent. That is not defensiveness for its
// own sake: [SettledWager.Validate] re-checks that the ledger entries sum to
// zero on the DECODED integers rather than on the domain values, which is the
// same check a consumer of the audit trail will make, and publishing a record
// that fails it would put a self-contradicting row on a topic whose entire value
// is that it can be trusted without the database.
func (s *Service) publish(ctx context.Context, w domain.Wager, t domain.Transaction, res Result) error {
	rec := newSettledWager(w, t, res)
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("settlement: refusing to publish an incoherent audit record: %w", err)
	}

	msg := kafka.Message{
		Type: MessageType,
		// The ledger transaction identifier, which is a pure function of the
		// wager — so a consumer deduplicating across redeliveries has a stable
		// key without decoding the payload.
		ID: t.ID().String(),
		// The provider's own instant for the result that decided the ticket.
		// NOT time.Now: kafka.Message is explicit that a placeholder here
		// "produces a staleness measurement of zero and makes the headline SLO
		// report perfect health for data that has none".
		ObservedAt: res.FinalisedAt,
		Payload:    rec,
	}

	if err := s.publisher.PublishWagerEvent(ctx, w.ID(), msg); err != nil {
		s.metrics.observePublishFailure()
		return fmt.Errorf("settlement: publish wager %s to %s (the transaction will be rolled "+
			"back, deliberately: a settlement with no audit record must not commit): %w",
			w.ID(), kafka.TopicWagerEvents, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// The money rules
// -----------------------------------------------------------------------------

// ticketOutcome is what a fully-graded ticket resolves to.
type ticketOutcome struct {
	status   domain.WagerStatus
	returned domain.Money

	// note names a HOUSE POLICY that decided this outcome, where one did, so
	// that a settlement which does not follow obviously from the legs is
	// legible in a log line rather than only in this file. Empty when the legs
	// alone determine the answer, which is the overwhelming majority of tickets.
	note string
}

// decideTicket computes what a fully-graded ticket returns.
//
// Every rule below is stated in terms of the legs' GRADED statuses and the
// ticket's own frozen terms. Nothing here re-derives a price from the market,
// and nothing consults a clock. The caller has already established
// [domain.Wager.AllLegsGraded].
//
// The four outcomes, in the order they are decided:
//
//	any leg LOST                → lost, returns nothing
//	every leg VOID or PUSH      → refunded at the stake; push if every leg
//	                              pushed, void if any was cancelled
//	no leg removed              → won at the FROZEN potential payout
//	some legs removed           → won at the ticket price with the removed legs
//	                              divided out (a teaser is refunded instead)
//
// # Why "lost" is decided first
//
// Because it is absorbing. domain.Leg.GradedMultiplier gives a lost leg a factor
// of zero, so a parlay containing one is worth nothing whatever the others did,
// and no arithmetic below can recover it. Checking it first also means the
// repricing path never has to reason about a dead ticket.
//
// # Why a win with no removals uses the STORED payout
//
// domain.Wager.PotentialPayout was computed once, at placement, from the
// accepted price under the ticket's own rounding mode, and frozen. Recomputing
// it as the product of the leg prices would produce a different number for a
// same-game parlay (correlation-adjusted), for a teaser (priced off a schedule
// that has nothing to do with the leg prices), and for a boosted ticket. wager.go
// is blunt about the stakes: "'To win $X' is a promise, and a promise recomputed
// later is not one."
func decideTicket(w domain.Wager) (ticketOutcome, error) {
	legs := w.Legs()

	var won, lost, void, push int
	for _, leg := range legs {
		switch leg.Status() {
		case domain.LegStatusWon:
			won++
		case domain.LegStatusLost:
			lost++
		case domain.LegStatusVoid:
			void++
		case domain.LegStatusPush:
			push++
		default:
			return ticketOutcome{}, fmt.Errorf("wager %s leg %s is %s: %w",
				w.ID(), leg.ID(), leg.Status(), domain.ErrLegNotGraded)
		}
	}

	if lost > 0 {
		return ticketOutcome{status: domain.WagerStatusLost, returned: domain.ZeroMoney}, nil
	}

	if won == 0 {
		// Every leg was removed. Money-identically a refund either way; the
		// distinction between the two statuses is a RESULT versus a
		// CANCELLATION, which domain.LegStatusPush labours over and which the
		// leaderboard's "how often does this user land on the number" needs.
		// A ticket carrying any cancellation is recorded as cancelled.
		status := domain.WagerStatusPush
		if void > 0 {
			status = domain.WagerStatusVoid
		}
		return ticketOutcome{status: status, returned: w.Stake()}, nil
	}

	removed := void + push
	if removed == 0 {
		return ticketOutcome{status: domain.WagerStatusWon, returned: w.PotentialPayout()}, nil
	}

	// From here the ticket won, but not at the price it was written at.
	if w.Kind() == domain.WagerKindTeaser {
		return ticketOutcome{
			status:   domain.WagerStatusVoid,
			returned: w.Stake(),
			note:     ErrNoTeaserSchedule.Error(),
		}, nil
	}

	return repriceWithRemovals(w, legs)
}

// repriceWithRemovals computes what a winning ticket returns when some of its
// legs were voided or pushed.
//
// # The rule
//
// A removed leg contributes a multiplier of exactly 1 (domain.Leg.
// GradedMultiplier: "the parlay reprices as though the leg had never been
// added"), so the ticket's effective price is its accepted price with the
// removed legs' booked prices divided out:
//
//	effective = accepted / Π(decimal of each removed leg)
//
// and the return is stake × effective, collapsed under THE TICKET'S OWN rounding
// mode. migrations/00006 stores that mode for exactly this path, and money.go
// says why a default would be wrong: "a silent default is how a house edge
// appears in a ledger that nobody meant to put one in".
//
// # Why divide out rather than multiply up
//
// The alternative is to multiply the surviving legs' prices together. For a
// plain parlay the two agree exactly, because the accepted price IS that
// product. They diverge for a same-game parlay, whose accepted price is
// correlation-adjusted and therefore below the naive product — and there,
// multiplying up would pay MORE than the customer's own ticket promised for a
// SHORTER version of it. Dividing out keeps the accepted price as the anchor, so
// the removal can only ever reduce what the ticket is worth.
//
// # The clamp
//
// The result is floored at the stake. domain.Wager.Settle refuses a win that
// returns less than the stake and it is right to: "winning must never cost
// money". The floor can only bind on a correlated ticket whose adjustment was
// larger than the removed leg's own price, which is a real if unusual shape, and
// returning the stake there is the conservative reading — the customer keeps
// what they risked on a ticket whose surviving legs all came in.
func repriceWithRemovals(w domain.Wager, legs []domain.Leg) (ticketOutcome, error) {
	effective := w.AcceptedDecimal()
	for _, leg := range legs {
		if leg.Status() != domain.LegStatusVoid && leg.Status() != domain.LegStatusPush {
			continue
		}
		quoted := leg.QuotedDecimal()
		if quoted <= domain.MinDecimalOdds {
			// Unreachable through domain.NewPrice, which bounds decimal odds
			// strictly above MinDecimalOdds. Guarded because the alternative is
			// a division that inflates the payout without bound, and an
			// unreachable branch that pays out is not a branch to leave to
			// chance.
			return ticketOutcome{}, fmt.Errorf("wager %s leg %s was booked at %g: %w",
				w.ID(), leg.ID(), quoted, domain.ErrOddsOutOfRange)
		}
		effective /= quoted
	}

	note := ""
	if effective <= 1 {
		effective = 1
		note = "the surviving legs repriced at or below the stake; returned the stake"
	}

	returned, err := w.Stake().MulFloat(effective, w.Rounding())
	if err != nil {
		return ticketOutcome{}, fmt.Errorf("wager %s repriced return: %w", w.ID(), err)
	}
	if returned.Compare(w.Stake()) < 0 {
		// Rounding toward zero can land a hair under the stake even when the
		// effective price is above 1. Same floor, same reason.
		returned = w.Stake()
	}

	return ticketOutcome{status: domain.WagerStatusWon, returned: returned, note: note}, nil
}

// -----------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------

// isConflict reports whether the error means somebody else got there first.
//
// All four sentinels describe the same situation from different tables: the row
// this transaction intended to change had already moved. None of them is a
// failure, and none of them is retryable — retrying would find the same answer.
func isConflict(err error) bool {
	return errors.Is(err, ErrWagerAlreadySettled) ||
		errors.Is(err, ErrLegAlreadyGraded) ||
		errors.Is(err, ErrTransactionExists) ||
		// domain.ErrIllegalTransition arrives here when a wager or leg loaded at
		// the start of the transaction was graded by another replica before this
		// one reached the state machine. It is the same race, caught one layer
		// higher, and it is emphatically NOT lumped in with domain.ErrInvalid
		// below — an illegal transition is a conflict, which is why the domain
		// files it under ErrConflict.
		errors.Is(err, domain.ErrIllegalTransition)
}

// isPermanent reports whether the error will still be true on the next poll.
//
// The membership is deliberately narrow. Everything here is a statement about
// THIS TICKET that no amount of retrying changes: a leg the grader cannot read,
// a market type with no rule, a value the domain rejects as impossible. Anything
// broader — a database error, a refused publish — must NOT be in this set, or a
// broker outage would be silently written off one ticket at a time.
//
// domain.ErrStaleUpdate is excluded even though it wraps ErrConflict: it is
// handled by the floored instant in settleInTx and would mean the floor did not
// work, which is a bug worth surfacing as a transient failure rather than
// absorbing.
func isPermanent(err error) bool {
	return errors.Is(err, ErrUnusableLeg) ||
		errors.Is(err, ErrUnusableResult) ||
		errors.Is(err, ErrUngradableMarket) ||
		errors.Is(err, domain.ErrInvalid) ||
		errors.Is(err, domain.ErrLegNotGraded)
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// positiveOr returns d when it is positive and fallback otherwise.
func positiveOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// positiveIntOr returns n when it is positive and fallback otherwise.
func positiveIntOr(n, fallback int) int {
	if n > 0 {
		return n
	}
	return fallback
}
