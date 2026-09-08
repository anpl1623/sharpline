// Prometheus instrumentation for the results poller.
//
// # The number an operator actually wants is the lag
//
// Counting results is easy and nearly useless: `recorded` fires at most once per
// contest, so it tells you the feed moved at some point and nothing about
// whether it is moving now. The question somebody asks at 22:00 is "the game
// finished forty minutes ago, why has my ticket not settled", and the series
// that answers it is [Metrics.observeLag] — the wall-clock gap between the final
// whistle and the instant the result landed in `events`.
//
// It is measured from provider.FinalResult.FinalisedAt, the PROVIDER's own
// instant for the outcome, and never from the row's created_at or the poller's
// tick. That is what makes the number attributable: a lag of an hour is either
// the provider's window opening late or this loop being stopped, and both are
// visible as the same quantity rising rather than as a metric that resets
// whenever a process restarts.
//
// # Labels deliberately not set
//
//   - `service`. deploy/observability/prometheus.yml attaches it as a TARGET
//     label on every scrape job; a metric label of the same name is renamed
//     `exported_service` on ingest and the two drift. internal/platform/kafka
//     and internal/ingest/writer make the same choice for the same reason.
//   - `league` and `event`. The event is unbounded and the league grows whenever
//     an operator adds one, on series written once per contest. The per-league
//     breakdown that would justify the cardinality already exists on the
//     staleness histogram the fanout stage owns.
//   - `provider`. It is a single value for the lifetime of a process — cmd/ingest
//     takes the odds adapter and the results source from one call and refuses to
//     start if their names disagree — and
//     sharpline_ingest_provider_info{provider,mode,simulated} already carries it
//     for a dashboard to join on.
//   - error text. Bounded classifications only: the provider-error series is
//     labelled by provider.Disposition, which is a closed set of three.
//
// # What this file deliberately does NOT emit
//
// A stage on sharpline_odds_staleness_seconds. That histogram's stage values are
// a phase-0 contract with a CLOSED set — deploy/observability/rules names
// received | normalized | priced | fanout — and a result is not an odds
// observation travelling that pipeline. Adding a fifth value would be a
// unilateral change to a contract this package does not own, and would
// additionally require registering a shared histogram that ingest already
// registers in the same process.
package results

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Metric namespace and subsystem: together, the `sharpline_ingest_results_`
// prefix. The subsystem names the SERVICE as well as the concern, because
// `settle` also handles results and a bare `sharpline_results_` prefix would
// leave a dashboard unable to say which side of the pipe a number came from.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "ingest_results"
)

// Poll outcomes. A closed set; every value appears in a code path.
const (
	// pollOK: the tick completed. It says nothing about whether any result was
	// found, because the ordinary tick finds none.
	pollOK = "ok"
	// pollStoreError: the work queue could not be read, or a write failed. The
	// database is the shared dependency, so a sustained rate here is an
	// incident rather than a provider problem.
	pollStoreError = "store_error"
	// pollProviderError: the provider refused or failed. Broken out separately
	// because the remedy is completely different and because the provider is
	// the half of this loop that can be down without anything else being wrong.
	pollProviderError = "provider_error"
)

// Per-contest outcomes. A closed set; see [Poller.record] for what each means.
const (
	// eventRecorded: this tick wrote the terminal status and final score, so
	// every wager on the contest became settleable at this instant. At most
	// once per event in the lifetime of a deployment.
	eventRecorded = "recorded"
	// eventUnchanged: a guard declined the write — already recorded, a newer
	// observation is stored, or the contest was never ingested. Normal on any
	// tick that races another replica or re-reads a row before settle has moved
	// it on.
	eventUnchanged = "unchanged"
	// eventFailed: the write failed. A SUSTAINED rate means stakes are sitting
	// in escrow with nothing to release them, which is why it is its own label
	// value and not folded into the poll counter.
	eventFailed = "failed"
	// eventUnresolved: the contest was on the work queue and the provider's
	// answer said nothing about it. THE COMMON CASE, and not a fault: most of
	// the queue is contests that started recently enough to still be in play. A
	// queue where this is the only value for hours, with the depth gauge flat,
	// is the shape of a stranded backlog.
	eventUnresolved = "unresolved"
	// eventUnsolicited: the provider reported an outcome the work queue was not
	// waiting on. It is DROPPED rather than written, because there is no row the
	// poller is confident it belongs to.
	//
	// IT IS ORDINARY AND NOT AN ADAPTER BUG, which is a change of meaning from
	// when this label was introduced. Results is a window query — "what finished
	// in this span", not "what happened to these rows" — so a provider covering
	// more contests than this deployment ingested reports them on every tick for
	// as long as they stay inside the window. A steady non-zero rate here is the
	// normal shape; what is worth looking at is this rate rising while
	// `recorded` stays flat, which is the shape of identifiers that no longer
	// resolve onto the catalogue.
	//
	// It also covers the two genuine adapter bugs — one contest stated twice in
	// one answer, and an outcome with no event key on it — because both are
	// unattributable in the same way and both are dropped. Those two log at
	// ERROR; the ordinary case does not log per contest.
	eventUnsolicited = "unsolicited"
)

// pollBuckets covers one tick: one indexed read, one provider call, and up to
// BatchSize guarded UPDATEs. The floor is a millisecond because the read is a
// primary-key-ordered index scan; the tail runs to DefaultPollTimeout, because a
// sample in the top bucket is one that is about to be abandoned.
var pollBuckets = []float64{
	0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// lagBuckets spans the whole range the settlement lag can legitimately take.
//
// The FLOOR is the poll interval — a result cannot land sooner than the next
// tick after the contest ends — so the interesting region starts around a minute
// and the boundaries below it exist only to make a suspiciously fast sample
// visible. The CEILING is the provider's own lookback window, three days for the
// shipped generator, because a contest that finished longer ago than that can
// still be recorded the moment somebody widens the horizon, and a histogram that
// topped out at an hour would render every one of those as the same +Inf.
//
// These are not the SLO boundaries in deploy/observability/rules — this is not
// an SLO series — so they are chosen for the quantity's own shape rather than
// borrowed.
var lagBuckets = []float64{
	1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 10800, 21600, 86400, 259200,
}

// Metrics is the results poller's collector set.
//
// Registration happens once, in [NewMetrics], and the value is injected — the
// pattern internal/ingest/writer and internal/platform/kafka both use. A nil
// Registerer builds the collectors WITHOUT registering them, which is right for
// a unit test and for a process with no /metrics endpoint: the observe calls
// stay live and cost a few nanoseconds, so no call site needs a nil check.
type Metrics struct {
	polls        *prometheus.CounterVec
	pollDuration prometheus.Histogram
	queueDepth   prometheus.Gauge
	events       *prometheus.CounterVec
	lag          prometheus.Histogram
	providerErrs *prometheus.CounterVec
}

// NewMetrics builds the collectors and registers them on reg.
//
// It returns an error rather than panicking: CLAUDE.md §12 forbids a panic
// outside main, and a duplicate registration is a wiring mistake the caller can
// report with the rest of its startup context.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		polls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "polls_total",
			Help: "Results polls, by outcome (ok|store_error|provider_error). One poll is one " +
				"work-queue read plus one provider call plus the writes it produced.",
		}, []string{"outcome"}),

		pollDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "poll_duration_seconds",
			Help: "Wall-clock time for one results poll, including the provider call and every " +
				"guarded UPDATE it issued.",
			Buckets: pollBuckets,
		}),

		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "queue_depth",
			Help: "Contests on the last work-queue read: started long enough ago to be plausibly " +
				"over and not yet recorded as finished. It is BOUNDED BY the batch size, so a " +
				"reading pinned at that bound means the queue is not draining.",
		}),

		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "events_total",
			Help: "Contests by outcome (recorded|unchanged|failed|unresolved|unsolicited). " +
				"recorded fires at most once per contest and is the instant every wager on it " +
				"became settleable; unresolved is the ordinary answer for a contest still being " +
				"played; unsolicited counts outcomes the work queue was not waiting on, which a " +
				"window query produces routinely; a sustained failed rate means stakes are stuck " +
				"in escrow.",
		}, []string{"outcome"}),

		lag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "settlement_lag_seconds",
			Help: "(write instant - the provider's finalisation instant): how long a contest sat " +
				"finished before its result reached the events table and its wagers became " +
				"settleable. Observed only on a result this process recorded.",
			Buckets: lagBuckets,
		}),

		providerErrs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "provider_errors_total",
			Help: "Failed results requests by provider.Disposition (retryable|fatal|" +
				"quota_exhausted). A fatal one is what disables this loop, so a single " +
				"increment there is more interesting than a thousand retryable ones.",
		}, []string{"disposition"}),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range m.collectors() {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.polls, m.pollDuration, m.queueDepth, m.events, m.lag, m.providerErrs,
	}
}

// observePoll records one completed tick.
func (m *Metrics) observePoll(outcome string, d time.Duration) {
	m.polls.WithLabelValues(outcome).Inc()
	m.pollDuration.Observe(d.Seconds())
}

// observeQueueDepth publishes the size of the last work-queue read.
func (m *Metrics) observeQueueDepth(n int) {
	m.queueDepth.Set(float64(n))
}

// observeEvents counts n contests that shared one outcome.
func (m *Metrics) observeEvents(outcome string, n int) {
	if n <= 0 {
		return
	}
	m.events.WithLabelValues(outcome).Add(float64(n))
}

// observeLag records how long one recorded contest waited to become settleable.
//
// A negative value is clamped to zero and the clamp is honest rather than a
// cover-up: FinalisedAt is the PROVIDER's clock and the write instant is this
// container's, so skew between hosts legitimately produces a negative
// difference, and a negative sample on a histogram lands in the lowest bucket
// where it is indistinguishable from a fast one. What DETECTS provider clock
// skew is sharpline_odds_clock_skew_total, which the provider layer owns and
// which the ProviderClockSkewDetected alert already watches, so nothing is lost
// here that is not measured better elsewhere. internal/ingest/writer's
// observeLags makes the same call for the same reason.
func (m *Metrics) observeLag(d time.Duration) {
	if d < 0 {
		d = 0
	}
	m.lag.Observe(d.Seconds())
}

// observeProviderError counts one failed results request by its classification.
// The error text never becomes a label; provider.Classify's closed set of three
// is what is counted.
func (m *Metrics) observeProviderError(err error) {
	if err == nil {
		return
	}
	m.providerErrs.WithLabelValues(provider.Classify(err).String()).Inc()
}
