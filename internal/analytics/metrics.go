// Prometheus instrumentation for the signals stage.
//
// # The problem this instrumentation exists to solve
//
// AN EMPTY SIGNALS BOARD IS THE CORRECT OUTPUT MOST OF THE TIME. A well-priced
// market has no positive EV against the sharp book, a feed with a permanent
// arbitrage on it is a feed with a bug, and a slate steams a handful of times an
// hour at most. So "no findings" and "the detector is broken" produce IDENTICAL
// output, and no amount of staring at the board separates them.
//
// That is exactly the phase-3a failure the contract ledger records — "a whole bus
// package nothing instantiated", healthy containers, valid records, an empty
// surface for a reason no metric showed. Every series below exists to make one of
// those two states distinguishable from the other:
//
//	records_total{result}          is the stage consuming at all?
//	signals_total{kind}            is anything being found?
//	suppressed_total{kind,reason}  if not, WHICH gate is refusing everything?
//	writes_total{kind,sink,...}    do the findings reach the two sinks?
//	detector_duration_seconds      is a detector pathologically slow?
//	window_lag_seconds             how late is a steam finding by the time it lands?
//	steam_markets                  is the detector's state bounded?
//
// The suppression counter is the one that earns its keep. A magnitude threshold
// set two decimal places wrong produces a stage that consumes every record,
// evaluates every window, reports nothing, and looks perfectly healthy — and the
// only observable difference is that `suppressed_total{reason="below_threshold"}`
// is large where it should be small, or zero where it should be large.
//
// # Labels deliberately not set
//
//   - `service`. deploy/observability/prometheus.yml attaches it as a TARGET
//     label; a metric label of the same name is renamed `exported_service` and
//     the two drift. Every other package in this repository makes the same
//     choice.
//   - a market, event, selection or book identifier. Tens of thousands of values.
//     Per-finding attribution is a row in Postgres and a record on the bus, which
//     is precisely why phase 9 has both; it is not a Prometheus label.
//   - a league. Defensible — there are four — and still declined, because the
//     cardinality would multiply through the reason label on the suppression
//     counter for a breakdown no alert would read.
//   - error text. Bounded classifications only.
//
// # Subsystem
//
// `sharpline_analytics_`. internal/pricing already owns `pricing_` (the frozen
// contract series the dashboard and PricingLatencyHigh read) and `pricer_` (the
// engine's own arithmetic). This stage runs in the same process and takes a third
// subsystem rather than extending either, because it is a different stage with a
// different failure surface: a signals detector that stops finding things has
// nothing to do with a devig that has become slow, and a panel that mixed them
// would answer neither question.
package analytics

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric namespace and subsystem: together the `sharpline_analytics_` prefix that
// deploy/observability/prometheus.yml's "every application series is prefixed
// sharpline_" rule requires.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "analytics"
)

// Steam window stages. `evaluated` is the denominator the magnitude threshold was
// calibrated against and `candidate` is the population just above the noise, so
// the pair separates "the slate is quiet" from "the detector is not running".
const (
	steamStageEvaluated = "evaluated"
	steamStageCandidate = "candidate"
)

// Signal kinds. A closed set and a label value, so it is written out rather than
// derived from a type name that a refactor could change under the dashboard.
const (
	kindEV    = "ev"
	kindArb   = "arb"
	kindSteam = "steam"
)

// Record results. THE stage's decision vocabulary, and a closed set: every
// record delivered leaves [Service.HandleMessage] under exactly one of these.
const (
	// resultProcessed: the record was decoded and every detector ran.
	resultProcessed = "processed"

	// resultTombstoned: the market was deleted upstream. The detectors' state for
	// it is released and no finding is retracted — a signal is an event that
	// happened, and a market ceasing to exist does not un-happen it.
	resultTombstoned = "tombstoned"

	// resultInvalid: the record could not be turned into a market — wrong message
	// type, unusable key, undecodable payload. PERMANENT: redelivery cannot
	// change the bytes on the topic.
	resultInvalid = "invalid"

	// resultFailed: a sink refused. TRANSIENT: the record is reported to the
	// Consumer as failed, and what happens next is the consumer's ErrorPolicy —
	// under Stop the offset stays uncommitted and the record is redelivered,
	// under Skip (which is what `pricer` wires) it is advanced over. Either way
	// the finding is re-derived when the market next reprices, because the write
	// path is an idempotent upsert on an input-derived replay key.
	resultFailed = "failed"

	// resultDeferred: every finding on the record was refused because a CATALOGUE
	// PARENT is not in the database yet, and nothing else went wrong. See
	// [analytics.ErrCatalogueLag].
	//
	// It is its own outcome and not a flavour of resultFailed because the two ask
	// different questions of whoever is looking. A sustained resultFailed rate is
	// an outage. A resultDeferred rate is `ingest`'s catalogue writer running
	// behind this stage — expected in the seconds after a cold start, when a fresh
	// group replays the whole compacted topic against an empty catalogue, and a
	// real problem only if it persists. Folding them together is how the cold-start
	// burst that phase 9's gate found came to look like 109 database errors.
	resultDeferred = "deferred"
)

// Write sinks and outcomes.
const (
	sinkStore = "store"
	sinkBus   = "bus"

	writeOK = "ok"

	// writeFailed: the sink returned an error. The record is redelivered.
	writeFailed = "failed"

	// writeNoSink: the dependency is not wired at all. It is a DISTINCT outcome
	// from a failure because it is a different problem with a different fix — a
	// deployment gap rather than an outage — and because it would otherwise be
	// invisible: a stage with no publisher succeeds at everything it attempts.
	writeNoSink = "no_sink"

	// writeContended: the sink lost a lock-ordering race and was rolled back
	// without writing anything, and the write was run again. Counted once PER
	// RETRIED ATTEMPT, so the outcome that eventually landed is counted under ok
	// or failed on top of it.
	//
	// It is a distinct outcome and not folded into either of those because it
	// answers a question neither can: a stage that is succeeding only after two
	// retries is one poll cycle away from failing, and the only visible symptom
	// otherwise would be latency. See [analytics.ErrContended] for what produces
	// it and why re-running is safe.
	writeContended = "contended"

	// writeCatalogueLag: the sink refused the write because a catalogue parent —
	// the finding's league, book, market or selection — had not committed yet, and
	// the write was run again. Counted once PER RETRIED ATTEMPT, exactly like
	// writeContended, so the outcome that eventually landed is counted on top of
	// it.
	//
	// Separate from writeContended because the two have different causes and
	// different fixes: contention is two writers holding the same catalogue rows,
	// and this is one writer not having produced the row at all. See
	// [analytics.ErrCatalogueLag].
	writeCatalogueLag = "catalogue_lag"
)

// DetectorBuckets returns the boundaries for
// sharpline_analytics_detector_duration_seconds.
//
// The shape of the list is the shape of the work. All three detectors are pure
// CPU over a payload already in memory: the +EV finder is one pass over
// books × selections, the arbitrage surface is one pass over findings
// internal/pricing already made, and the steam detector is a handful of binary
// searches over a bounded ring. The interesting region is tens of microseconds
// to a few milliseconds, and everything above 0.1 exists to catch a pathological
// market rather than to be populated.
//
// If the bulk of the mass ever lands above 0.01, something in this package has
// started doing I/O inside a detector — which is the one thing the design
// forbids, because these run inside a Kafka handler and the group's rebalance is
// blocked for the whole poll.
//
// Exported so a test asserting the contract reads the same list the emitter
// uses, matching [pricing.PricingBuckets] and provider.StalenessBuckets.
func DetectorBuckets() []float64 {
	return []float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.5}
}

// WindowLagBuckets returns the boundaries for
// sharpline_analytics_window_lag_seconds.
//
// The lag of a steam finding is structurally at least
// [steam.Config.AllowedLateness] — the watermark deliberately trails the newest
// observation by that much — plus the hop's own granularity, plus however long
// the record took to reach this stage. With the defaults that floor is 180
// seconds, so the boundaries are laid out to make the interesting question
// legible: not "is there lag" (there is, by design) but "how much of it is ours".
//
// 180 and 240 bracket the designed floor. Mass above 600 means the stage is
// falling behind the priced board rather than waiting for late books, which is a
// capacity problem and a different fix.
func WindowLagBuckets() []float64 {
	return []float64{60, 120, 180, 240, 300, 420, 600, 900, 1800, 3600}
}

// Metrics is the signals stage's collector set.
//
// Registration happens once, in [NewMetrics], and the value is injected — the
// pattern internal/pricing, internal/platform/kafka and internal/ingest all
// follow, for the same reason: one process may legitimately build more than one
// Service, and a duplicate registration should fail its startup rather than its
// code review.
//
// A nil Registerer builds the collectors WITHOUT registering them. That is right
// for a unit test and for any process with no /metrics endpoint: the observe
// calls stay live and cost a few nanoseconds, so no call site needs a nil check.
type Metrics struct {
	records      *prometheus.CounterVec
	signals      *prometheus.CounterVec
	suppressed   *prometheus.CounterVec
	writes       *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	windowLag    prometheus.Histogram
	steamState   prometheus.Gauge
	steamWindows *prometheus.CounterVec
}

// NewMetrics builds the collectors and registers them on reg.
//
// It returns an error rather than panicking: CLAUDE.md §12 forbids a panic
// outside main, and a registration conflict is a wiring mistake the caller
// reports with the rest of its startup context.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		records: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "records_total",
			Help: "Priced markets seen on price.computed by what the signals stage did " +
				"with them. A flat `processed` while the pricer is publishing means " +
				"this stage's consumer has stopped; `invalid` is permanent and no " +
				"reprice can help; `failed` is a sink that refused; `deferred` is a " +
				"catalogue row `ingest` has not committed yet, which is expected in " +
				"the seconds after a cold start and is a problem only if it persists. " +
				"`failed` and `deferred` are both recovered by the market's next " +
				"reprice, not by a redelivery -- the pricer wires this stage with " +
				"ErrorPolicySkip.",
		}, []string{"result"}),

		signals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "signals_total",
			Help: "Findings emitted, by kind. ZERO IS A LEGITIMATE VALUE for all " +
				"three — a well-priced board has no +EV, a feed with a permanent " +
				"arbitrage has a bug, and steam is rare — which is exactly why this " +
				"counter is useless on its own and must be read beside " +
				"sharpline_analytics_suppressed_total.",
		}, []string{"kind"}),

		suppressed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "suppressed_total",
			Help: "Candidate findings refused, by kind and by which rule refused them. " +
				"THE SERIES THAT SEPARATES a quiet market from a broken detector: a " +
				"threshold set one decimal place wrong produces a stage that consumes " +
				"every record, evaluates every window and reports nothing, and the only " +
				"observable difference is here. For arbitrage, `stale_leg` dominating is " +
				"the EXPECTED and healthy state.",
		}, []string{"kind", "reason"}),

		writes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "writes_total",
			Help: "Findings written, by kind, by sink (store or bus) and by outcome. " +
				"`no_sink` means the dependency is not wired at all, which is a " +
				"deployment gap rather than an outage and is counted separately " +
				"because a stage with no publisher otherwise succeeds at everything " +
				"it attempts.",
		}, []string{"kind", "sink", "outcome"}),

		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "detector_duration_seconds",
			Help: "Time to run one detector over one priced market. It measures the " +
				"detector alone — not the decode before it and not the writes after " +
				"it — so a rise here is arithmetic getting slower and nothing else. " +
				"Mass above 0.01 means a detector has started doing I/O, which is " +
				"forbidden: these run inside a Kafka handler and block the group's " +
				"rebalance for the whole poll.",
			Buckets: DetectorBuckets(),
		}, []string{"kind"}),

		windowLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "window_lag_seconds",
			Help: "Age of a steam window's END when the finding was emitted. It is " +
				"structurally at least the detector's allowed lateness (180s by " +
				"default), because the watermark deliberately waits for lagged books " +
				"before closing a window — so the question this answers is not " +
				"whether there is lag but how much of it is ours. Mass above 600s " +
				"means the stage is falling behind the priced board.",
			Buckets: WindowLagBuckets(),
		}),

		steamWindows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "steam_windows_total",
			Help: "Hopping windows the steam detector CLOSED, by stage. `evaluated` " +
				"is every (market, selection, window) the watermark decided; " +
				"`candidate` is the subset where some book moved far enough to be " +
				"considered a lead. It is the denominator the magnitude threshold " +
				"was calibrated against -- findings over evaluated is the firing " +
				"rate, and a rate near a percent means the threshold has been " +
				"loosened into a firehose. Without it, `signals_total{kind=\"steam\"} " +
				"= 0` is indistinguishable between a quiet slate, a detector whose " +
				"watermark never advances, and one whose bar is set past every real " +
				"move -- three different problems with three different fixes.",
		}, []string{"stage"}),

		steamState: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "steam_markets",
			Help: "Markets the steam detector holds windowed state for. Compare it " +
				"against the pricer's markets_tracked gauge: a number that only ever " +
				"grows means tombstones are not reaching the detector and the " +
				"per-market state is a leak on a slate that rolls over daily.",
		}),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.records, m.signals, m.suppressed, m.writes, m.duration, m.windowLag,
		m.steamState, m.steamWindows,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("analytics metrics: %w", err)
		}
	}
	return m, nil
}

// observeRecord counts one record's outcome.
func (m *Metrics) observeRecord(result string) {
	if m == nil {
		return
	}
	m.records.WithLabelValues(result).Inc()
}

// observeSteamWindows counts the windows one steam pass closed, and how many of
// them held a move big enough to be a candidate lead.
//
// Both are counted even when they are zero-valued increments, so that the series
// exist from the first record rather than appearing the first time the detector
// finds something — a series that only exists once it is non-zero cannot be used
// to tell "nothing yet" from "nothing wired".
func (m *Metrics) observeSteamWindows(windows, candidates int) {
	if m == nil {
		return
	}
	m.steamWindows.WithLabelValues(steamStageEvaluated).Add(float64(windows))
	m.steamWindows.WithLabelValues(steamStageCandidate).Add(float64(candidates))
}

// observeDetector records one detector's pass over one record: how long it took,
// how many findings it produced, and why the rest were refused.
//
// The reasons are taken as a map keyed by a bounded vocabulary rather than as
// separate calls, so a detector that grows a new refusal reason cannot forget to
// count it — the counter follows the stats struct, which the detector fills in
// the same switch that makes the decision.
func (m *Metrics) observeDetector(kind string, d time.Duration, signals int, reasons map[string]int) {
	if m == nil {
		return
	}
	m.duration.WithLabelValues(kind).Observe(d.Seconds())
	if signals > 0 {
		m.signals.WithLabelValues(kind).Add(float64(signals))
	}
	for reason, n := range reasons {
		if n > 0 {
			m.suppressed.WithLabelValues(kind, reason).Add(float64(n))
		}
	}
}

// observeWrite counts one attempted write to one sink.
func (m *Metrics) observeWrite(kind, sink, outcome string) {
	if m == nil {
		return
	}
	m.writes.WithLabelValues(kind, sink, outcome).Inc()
}

// observeWindowLag records how old a steam window's end was when its finding was
// emitted.
//
// A NEGATIVE lag is clamped to zero and is not silently swallowed elsewhere: it
// means the window ended in the future relative to our clock, which is provider
// clock skew, and internal/ingest/provider already owns
// sharpline_odds_clock_skew_total for that condition at the ingest stage. This
// package does not open a second series for the same fact; a histogram would
// simply put the negative sample in the lowest bucket, where it reads as
// excellent.
func (m *Metrics) observeWindowLag(d time.Duration) {
	if m == nil {
		return
	}
	s := d.Seconds()
	if s < 0 {
		s = 0
	}
	m.windowLag.Observe(s)
}

// observeSteamState publishes the detector's per-market state size.
func (m *Metrics) observeSteamState(n int) {
	if m == nil {
		return
	}
	m.steamState.Set(float64(n))
}
