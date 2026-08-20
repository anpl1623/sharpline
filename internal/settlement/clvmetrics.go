// Prometheus instrumentation for the closing-line-value pass.
//
// # Why this is a separate collector set from [Metrics]
//
// It is a separate LOOP with a separate failure mode, and the whole design
// premise of the CLV pass is that its failures must not be confusable with
// settlement's. Folding these series into [Metrics] would put "a customer was not
// paid" and "a report was not written" behind the same struct, and the next
// person to write an alert rule would have to read this comment to know which
// `outcome="failed"` they were selecting. Two structs, two registrations, one
// process — the pattern internal/platform/kafka, internal/ingest/writer and
// internal/pricing already use for the same reason.
//
// The names still share the `sharpline_settlement_` prefix, because they are
// still the settle binary's series and deploy/observability/prometheus.yml
// requires every application series to be prefixed `sharpline_`. The `clv_`
// infix is what separates them.
//
// # The number an operator should look at first
//
// `sharpline_settlement_clv_unmeasurable_total`, BY REASON — and the important
// thing about it is that a non-zero rate is NOT an alarm. clv/doc.go §5 states
// plainly that an in-play wager has no closing line value under this definition,
// so `reason="close_before_take"` climbing steadily on a system with live betting
// is the design working. What is worth alerting on is a reason that SHOULD be
// rare climbing: `taken_quote_mismatch` means a leg was booked off a quote that
// is not in the prices hypertable, and `closing_incomplete` climbing on markets
// that were never suspended means the closing lookback is too short.
//
// This is exactly why the reason is a label rather than a single counter: an
// undifferentiated "unmeasurable" total cannot distinguish a deliberate exclusion
// from a broken one, and a metric that cannot make that distinction gets muted.
//
// # Labels deliberately not set
//
//   - `service`. Attached as a TARGET label by prometheus.yml; a metric label of
//     the same name is renamed `exported_service` and the two drift. Every
//     package in this repo makes the same choice.
//   - a leg, wager, user or market identifier. Unbounded cardinality, and
//     per-leg attribution belongs on the signals.clv topic, which is a whole
//     record rather than a label.
package settlement

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/analytics/clv"
)

// Leg dispositions for the CLV pass. A closed set: every value is written by
// exactly one branch of [CLVPass.measure].
const (
	// clvLegMeasured: a row was written and it counts toward the leaderboard.
	clvLegMeasured = "measured"

	// clvLegLineMoved: a row was written and it does NOT count. The market moved
	// its line between the take and the close, so the number is indicative only —
	// odds.AggregateCLV excludes it and the leaderboard query filters it in SQL.
	// Counted separately because a bettor whose legs are mostly line-moved has a
	// leaderboard position computed from a fraction of their record, and that is a
	// different claim from one computed from all of it.
	clvLegLineMoved = "line_moved"

	// clvLegUnmeasurable: the data cannot answer for this leg, for one of
	// clv.Reasons()' enumerated reasons. NOT a failure. See the reason counter.
	clvLegUnmeasurable = "unmeasurable"

	// clvLegUnusable: the work-queue row is not a graded leg — a malformed
	// identifier, an unknown market type, a status that is not terminal. A defect
	// in the query or the schema rather than in the market data, and the one value
	// here that should always be zero.
	clvLegUnusable = "unusable"

	// clvLegFailed: the store or the bus refused. Transient; the leg stays on the
	// queue and the next pass retries it until it ages out of the retry window.
	clvLegFailed = "failed"
)

// Pass outcomes.
const (
	// clvPassOK: the queue was read and drained as far as it went.
	clvPassOK = "ok"

	// clvPassFailed: a read of the queue failed. The window is recomputed from
	// scratch on the next tick, so nothing is skipped.
	clvPassFailed = "failed"
)

// CLVDurationBuckets returns the boundaries for
// sharpline_settlement_clv_duration_seconds.
//
// One measurement is two indexed reads of the prices hypertable, a devig, and —
// on the success path only — one synchronous acknowledged publish and one upsert.
// So the shape is the same shape [SettlementBuckets] describes and for the same
// reason: this is round trips, not arithmetic.
//
// The list is identical to [SettlementBuckets] on purpose. Two histograms over
// two loops in one binary that share their boundaries can be compared panel to
// panel, and an operator asking "is the CLV pass slower than settlement" gets an
// answer by overlaying them rather than by reading two bucket lists.
//
// Exported so a test asserting the contract reads the same list the emitter uses.
func CLVDurationBuckets() []float64 { return SettlementBuckets() }

// CLVMetrics is the CLV pass's collector set.
//
// A nil Registerer builds the collectors WITHOUT registering them, which is right
// for a unit test and for any process with no /metrics endpoint. Every observe
// method tolerates a nil *CLVMetrics, so a pass built without one is not a
// special case either.
type CLVMetrics struct {
	legs         *prometheus.CounterVec
	unmeasurable *prometheus.CounterVec
	passes       *prometheus.CounterVec

	duration prometheus.Histogram

	publishFailures prometheus.Counter
	queueDepth      prometheus.Gauge
}

// NewCLVMetrics builds the collectors and registers them on reg.
//
// It PRE-CREATES every reason label. clv.Reasons() is a closed set, and a reason
// that has never fired should read as an honest zero rather than as an absent
// series a dashboard renders as a gap — which matters more here than usual,
// because several of these reasons firing at zero is the evidence that the
// closing-price rules are behaving.
func NewCLVMetrics(reg prometheus.Registerer) (*CLVMetrics, error) {
	m := &CLVMetrics{
		legs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "clv_legs_total",
			Help: "Graded legs the CLV pass acted on, by what it decided. measured and " +
				"line_moved are the two shapes that write a row, and only measured " +
				"counts toward a leaderboard — a line-moved sample is indicative only " +
				"and every aggregate excludes it. unmeasurable is the data declining to " +
				"answer and is NOT a failure; see clv_unmeasurable_total for why. " +
				"unusable should always be zero: it is a work-queue row that is not a " +
				"graded leg. failed is a store or bus fault and WILL be retried.",
		}, []string{"outcome"}),

		unmeasurable: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "clv_unmeasurable_total",
			Help: "Graded legs with no closing line value, by reason. A NON-ZERO RATE IS " +
				"NOT AN ALARM: close_before_take is every in-play wager and is a " +
				"deliberate exclusion. The reasons worth alerting on are the ones that " +
				"should be rare — taken_quote_mismatch means a leg was booked off a " +
				"quote that is not in the prices hypertable, and closing_incomplete " +
				"climbing on markets that were never suspended means the closing " +
				"lookback is too short for how often they are repriced.",
		}, []string{"reason"}),

		passes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "clv_passes_total",
			Help: "Passes over the CLV work queue. A rate near zero with the process up " +
				"means the loop has stopped — and unlike the settlement loop, nothing " +
				"else in the system will notice, because this pass is deliberately not " +
				"a readiness dependency.",
		}, []string{"outcome"}),

		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "clv_duration_seconds",
			Help: "Time to measure one graded leg: two reads of the prices hypertable, a " +
				"devig, and on the success path one acknowledged publish and one " +
				"upsert. Shares its boundaries with " +
				"sharpline_settlement_duration_seconds so the two loops can be " +
				"overlaid on one panel.",
			Buckets: CLVDurationBuckets(),
		}),

		publishFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "clv_publish_failures_total",
			Help: "Publishes to signals.clv that failed, and therefore left the leg " +
				"unmeasured rather than storing a row whose signal was never sent. " +
				"Sustained non-zero means the bus is refusing and CLV is not being " +
				"recorded — it does NOT mean settlements are affected, which is the " +
				"whole point of running this pass outside the settlement transaction.",
		}),

		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "clv_queue_depth",
			Help: "Graded legs seen awaiting measurement on the last pass. It does not " +
				"drain to zero: a leg that is permanently unmeasurable stays on the " +
				"queue until it ages out of the retry window, so the floor is the " +
				"steady-state count of in-play and unpriceable legs inside that window. " +
				"A step change in the floor is the signal, not the floor itself.",
		}),
	}

	// Pre-create the closed label sets so an unfired value reads as zero.
	for _, outcome := range []string{
		clvLegMeasured, clvLegLineMoved, clvLegUnmeasurable, clvLegUnusable, clvLegFailed,
	} {
		m.legs.WithLabelValues(outcome)
	}
	for _, reason := range clv.Reasons() {
		m.unmeasurable.WithLabelValues(reason.String())
	}
	for _, outcome := range []string{clvPassOK, clvPassFailed} {
		m.passes.WithLabelValues(outcome)
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.legs, m.unmeasurable, m.passes, m.duration, m.publishFailures, m.queueDepth,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("settlement clv metrics: %w", err)
		}
	}
	return m, nil
}

// observeLeg counts one leg's disposition and how long reaching it took.
//
// The duration covers the WHOLE measurement including the reads that decided a
// leg was unmeasurable, because that is the work the pass actually did. Timing
// only the success path would report the loop as fast precisely when most of its
// time was going into legs it could not measure.
func (m *CLVMetrics) observeLeg(outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.legs.WithLabelValues(outcome).Inc()
	m.duration.Observe(d.Seconds())
}

// observeUnmeasurable records why one leg had no closing line value.
//
// It is called IN ADDITION to observeLeg(clvLegUnmeasurable), not instead of it,
// so the two counters agree by construction: sum(clv_unmeasurable_total) equals
// clv_legs_total{outcome="unmeasurable"}, and a dashboard that disagrees is
// reporting a bug in this file rather than in the data.
func (m *CLVMetrics) observeUnmeasurable(reason clv.Reason) {
	if m == nil {
		return
	}
	m.unmeasurable.WithLabelValues(reason.String()).Inc()
}

// observePass counts one walk of the work queue.
func (m *CLVMetrics) observePass(outcome string) {
	if m == nil {
		return
	}
	m.passes.WithLabelValues(outcome).Inc()
}

// observePublishFailure counts one refused publish, which is one leg left
// unmeasured.
func (m *CLVMetrics) observePublishFailure() {
	if m == nil {
		return
	}
	m.publishFailures.Inc()
}

// observeQueueDepth publishes how many legs the last pass saw awaiting
// measurement.
func (m *CLVMetrics) observeQueueDepth(n int) {
	if m == nil {
		return
	}
	m.queueDepth.Set(float64(n))
}
