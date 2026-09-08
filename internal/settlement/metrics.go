// Prometheus instrumentation for the settle service.
//
// # No series here is a pre-existing contract, and that is worth saying
//
// internal/pricing/metrics.go opens by naming the three series it did not get to
// choose — the dashboard panel and the two alert rules that phase 0 wrote before
// any code existed. Settlement has no such inheritance:
// deploy/observability/rules/sharpline-alerts.yml mentions `settle` only through
// the generic Postgres family (`sharpline_db_*`, which internal/platform/postgres
// already exports for this binary because it opens a pool), and no dashboard
// panel reads a `sharpline_settlement_*` series today.
//
// So this file DEFINES a contract rather than filling one, and the names below
// should be treated as frozen from here on for the reason the alerts file gives
// about bucket boundaries: a rule that selects a bucket by an exact `le` literal
// matches nothing if the boundary moves, "the rule silently evaluates to empty,
// and the SLI reads as absent rather than as broken".
//
// # The one number an operator should look at first
//
// `sharpline_settlement_results_lag_seconds` — how long a finished game waited
// before its tickets were graded. It is the settlement equivalent of the odds
// staleness SLO: a customer whose parlay came in an hour ago and whose balance
// has not moved does not care that the poll loop is running, and every other
// series here can look healthy while that one climbs (a results source returning
// nothing produces zero errors and zero settlements, which is indistinguishable
// from a quiet Sunday unless the lag is measured).
//
// # Labels deliberately not set
//
//   - `service`. deploy/observability/prometheus.yml attaches it as a TARGET
//     label; a metric label of the same name is renamed `exported_service` and
//     the two drift. Every package in this repo makes the same choice.
//   - a wager, user, event or leg identifier. Unbounded cardinality, and
//     per-wager attribution belongs on a span and in the audit trail — which for
//     this service is a whole Kafka topic, not a log line.
//   - error text. Bounded classifications only.
package settlement

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Metric namespace and subsystem: together, the `sharpline_settlement_` prefix
// that deploy/observability/prometheus.yml's "every application series is
// prefixed sharpline_" rule requires.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "settlement"
)

// Wager outcomes. THE settle service's decision vocabulary, and a closed set:
// every value is written by exactly one branch of Service.settleWager.
const (
	// outcomeWon, outcomeLost, outcomeVoid, outcomePush are the four graded
	// terminal states. Their sum is the number of tickets this system has
	// closed, and the ratio between them is the book's own hit rate.
	outcomeWon  = "won"
	outcomeLost = "lost"
	outcomeVoid = "void"
	outcomePush = "push"

	// outcomeDeferred: legs were graded, but the ticket still has legs on games
	// that have not finished, so it stays open. The ordinary state of a parlay
	// mid-slate, and expected to dominate on a Sunday afternoon.
	outcomeDeferred = "deferred"

	// outcomeAlreadyDone: another settle replica, or an earlier redelivery of
	// this result, graded or settled the ticket first. The transaction rolled
	// back and nothing was lost. Expected at a low rate; a HIGH rate means two
	// replicas are contending on the same events and the poll cadence is too
	// aggressive for the batch size.
	outcomeAlreadyDone = "already_settled"

	// outcomeFailed: the transaction did not commit for any other reason — a
	// database error, a refused publish, an arithmetic fault. The result is NOT
	// consumed: the cursor does not advance past a batch containing one of
	// these, so the next poll retries from a fresh read.
	outcomeFailed = "failed"

	// outcomeUnusable: a pending-leg row the grader could not read at all. It is
	// permanent, it is skipped, and it is the one value here that should always
	// be zero — a non-zero count is a customer's stake in escrow with nothing
	// that will ever release it.
	outcomeUnusable = "unusable"
)

// Poll outcomes.
const (
	// pollOK: the results source answered.
	pollOK = "ok"

	// pollFailed: it did not. The cursor holds and the next tick retries.
	pollFailed = "failed"
)

// Result dispositions, counted per result rather than per wager.
const (
	// resultSettled: the result was read and every open leg on it was acted on.
	resultSettled = "settled"

	// resultIdle: nobody had a bet on the game. The overwhelmingly common case
	// on a real slate, and cheap — one indexed query matching zero rows.
	resultIdle = "idle"

	// resultUnusable: the source returned a row that is not a result. Permanent,
	// skipped, and a defect in the source rather than in the data.
	resultUnusable = "unusable"
)

// SettlementBuckets returns the boundaries for
// sharpline_settlement_duration_seconds.
//
// The shape of the list is the shape of the work, and the work here is NOT
// arithmetic — it is one database transaction plus one synchronous, acknowledged
// Kafka publish. So the interesting region starts an order of magnitude above
// the pricer's: a few milliseconds for the round trips on a warm connection,
// tens of milliseconds when the broker has to fsync to every in-sync replica,
// and everything past a second exists to catch a stalled broker rather than to
// be populated.
//
// The top boundary is 30s deliberately. kafka.AuditProducer retries without
// bound and is limited only by the caller's deadline, so a broker outage shows
// up here as mass at the ceiling rather than as errors — and a histogram whose
// ceiling is below the caller's timeout cannot show that at all.
//
// Exported so a test asserting the contract reads the same list the emitter
// uses, rather than asserting its own copy.
func SettlementBuckets() []float64 {
	return []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
}

// ResultsLagBuckets returns the boundaries for
// sharpline_settlement_results_lag_seconds.
//
// This measures a DIFFERENT kind of delay from every other latency histogram in
// the system, and its boundaries say so. The quantity is (now − the provider's
// observation instant for the final result), so it accumulates the provider's
// own reporting delay, ingest's poll cadence, the normalizer, the writer, and
// this service's poll interval — a chain whose floor is already tens of seconds
// on the live tier (ADR 0003's cadence ladder). Millisecond boundaries here
// would all be empty and would say nothing.
//
// 60s is the boundary to watch: it is roughly one ingest live-tier poll plus one
// settle poll, so mass above it means something in the chain has stopped rather
// than that the chain is slow. The 3600 ceiling catches a settle service that
// has been down and is working through a backlog, which is exactly when an
// operator most wants to see the shape of the queue drain.
func ResultsLagBuckets() []float64 {
	return []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}
}

// Metrics is the settle service's collector set.
//
// Registration happens once, in [NewMetrics], and the value is injected — the
// pattern internal/platform/kafka, internal/ingest/writer and internal/pricing
// all follow, for the same reason: one process may legitimately build more than
// one Service, and a duplicate registration should fail its startup rather than
// its code review.
//
// A nil Registerer builds the collectors WITHOUT registering them. That is right
// for a unit test and for any process with no /metrics endpoint: the observe
// calls stay live and cost a few nanoseconds, so no call site needs a nil check.
// Every observe method additionally tolerates a nil *Metrics, so a Service built
// without one is not a special case either.
type Metrics struct {
	wagers  *prometheus.CounterVec
	legs    *prometheus.CounterVec
	results *prometheus.CounterVec
	polls   *prometheus.CounterVec

	duration prometheus.Histogram
	lag      prometheus.Histogram

	publishFailures prometheus.Counter
	cursor          prometheus.Gauge
}

// NewMetrics builds the collectors and registers them on reg.
//
// It returns an error rather than panicking: CLAUDE.md §12 forbids a panic
// outside main, and a registration conflict is a wiring mistake the caller
// reports with the rest of its startup context.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		wagers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "wagers_total",
			Help: "Tickets this service acted on, by what it decided. won/lost/void/push " +
				"are the four graded terminal states and their sum is the number of " +
				"tickets closed. deferred means legs were graded but the ticket still " +
				"has games to run, which is the ordinary state of a parlay mid-slate. " +
				"already_settled is a redelivery or a second replica arriving first and " +
				"is harmless at a low rate. failed is a transaction that did not commit " +
				"and WILL be retried from a fresh read on the next poll. unusable should " +
				"always be zero: it is a stake in escrow that nothing will release.",
		}, []string{"outcome"}),

		legs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "legs_total",
			Help: "Legs graded, by the status they graded to. void is the value to watch: " +
				"the grader voids a leg it cannot honestly decide — a player prop, " +
				"whose statistic this results feed does not carry, or a futures market, " +
				"which no single event resolves — so a rising void rate is a report " +
				"about the feed's coverage rather than about the games.",
		}, []string{"status"}),

		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "results_total",
			Help: "Results read from the feed, by disposition. idle means nobody had a bet " +
				"on the game and dominates on a real slate. unusable means the source " +
				"returned a row that is not a result — an ended event with no final " +
				"score, or a status that is not terminal — and is a defect in the " +
				"source, not in the data.",
		}, []string{"disposition"}),

		polls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "polls_total",
			Help: "Polls of the results feed. A rate near zero with the process up means " +
				"the loop has stopped; pair it with the readiness probe, which reports " +
				"the same fact from the other side.",
		}, []string{"outcome"}),

		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "duration_seconds",
			Help: "Time to settle one ticket: the whole database transaction including " +
				"the synchronous, acknowledged publish to wager.events that happens " +
				"inside it. Not arithmetic — this is round trips, so the interesting " +
				"region is milliseconds to tens of milliseconds and mass at the 30s " +
				"ceiling means the broker is refusing to acknowledge and the audit " +
				"producer is retrying, which by design blocks the commit.",
			Buckets: SettlementBuckets(),
		}),

		lag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "results_lag_seconds",
			Help: "Age of a result when settlement picked it up, measured from the " +
				"provider's own observation instant. THE headline number for this " +
				"service: a customer whose bet came in an hour ago and whose balance " +
				"has not moved is not reassured that the poll loop is running. It " +
				"accumulates the provider's reporting delay, ingest's poll cadence and " +
				"this service's, so its floor is tens of seconds by construction; mass " +
				"above 60s means a link in that chain has stopped rather than slowed.",
			Buckets: ResultsLagBuckets(),
		}),

		publishFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "publish_failures_total",
			Help: "Publishes to wager.events that failed and therefore ABORTED the " +
				"settlement transaction. Every increment here is a ticket that was " +
				"deliberately not settled because its audit record could not be " +
				"written, which is the correct failure and not a lost settlement: the " +
				"next poll retries it. Sustained non-zero means the bus is down and " +
				"nothing is being graded.",
		}),

		cursor: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "cursor_timestamp_seconds",
			Help: "Unix time of the results cursor: settlement has consumed every result " +
				"finalised before this instant. time() minus this gauge is the backlog, " +
				"and it is the series to alert on for a service that has stalled " +
				"without erroring — a results source returning nothing produces no " +
				"errors at all.",
		}),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.wagers, m.legs, m.results, m.polls,
		m.duration, m.lag, m.publishFailures, m.cursor,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("settlement metrics: %w", err)
		}
	}
	return m, nil
}

// observeWager counts one settlement decision and how long the transaction that
// reached it took.
//
// The duration covers the WHOLE transaction — the reads, the grading writes, the
// wager update, the ledger insert and the publish — because that is the unit of
// work that either happens or does not. Timing only the publish, or only the
// ledger insert, would produce a number that looks healthy while the thing an
// operator cares about (did this customer get paid, and when) is not measured
// anywhere.
func (m *Metrics) observeWager(outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.wagers.WithLabelValues(outcome).Inc()
	m.duration.Observe(d.Seconds())
}

// observeLeg counts one graded leg.
func (m *Metrics) observeLeg(status domain.LegStatus) {
	if m == nil {
		return
	}
	m.legs.WithLabelValues(status.String()).Inc()
}

// observeResult counts one result and, where the result was usable, how stale it
// was when settlement reached it.
//
// The lag is observed ONCE PER RESULT rather than once per wager. A popular game
// carries hundreds of tickets and an obscure one carries none, so per-wager
// observation would weight the histogram by betting volume and report the
// pipeline as fast whenever the fast results happened to be the popular ones.
// The question is "how long did this GAME wait", and a game waits once.
func (m *Metrics) observeResult(disposition string, lag time.Duration) {
	if m == nil {
		return
	}
	m.results.WithLabelValues(disposition).Inc()
	if disposition == resultUnusable {
		// An unusable row has no trustworthy finalisation instant — a missing
		// one is among the reasons it is unusable — so there is no lag to
		// record. Observing zero would drag the p50 down and report the feed as
		// fresher than it is, which is the opposite of what this series is for.
		return
	}
	if lag < 0 {
		// The provider's clock is ahead of ours. Clamped rather than dropped:
		// internal/ingest/provider treats the same condition the same way, and a
		// dropped observation would silently shrink the denominator.
		lag = 0
	}
	m.lag.Observe(lag.Seconds())
}

// observePoll counts one poll of the results feed.
func (m *Metrics) observePoll(outcome string) {
	if m == nil {
		return
	}
	m.polls.WithLabelValues(outcome).Inc()
}

// observePublishFailure counts one refused publish, which is one aborted
// settlement.
func (m *Metrics) observePublishFailure() {
	if m == nil {
		return
	}
	m.publishFailures.Inc()
}

// observeCursor publishes where the results cursor has reached.
func (m *Metrics) observeCursor(at time.Time) {
	if m == nil {
		return
	}
	m.cursor.Set(float64(at.Unix()))
}

// wagerOutcomeLabel maps a settled ticket's terminal status onto its metric
// label.
//
// It is a switch rather than domain.WagerStatus.String() so that the label set
// stays CLOSED: the constants above are the vocabulary, and a status added to
// the domain would appear here as a compile-visible gap rather than as a new
// label value silently entering a dashboard's legend. cashed_out is deliberately
// absent — a cash-out is priced and written by internal/betting, never by this
// service, and a settle replica emitting one would mean the two services had
// crossed.
func wagerOutcomeLabel(s domain.WagerStatus) (string, error) {
	switch s {
	case domain.WagerStatusWon:
		return outcomeWon, nil
	case domain.WagerStatusLost:
		return outcomeLost, nil
	case domain.WagerStatusVoid:
		return outcomeVoid, nil
	case domain.WagerStatusPush:
		return outcomePush, nil
	default:
		return "", fmt.Errorf("settlement: %s is not an outcome this service produces: %w",
			s, domain.ErrIllegalTransition)
	}
}
