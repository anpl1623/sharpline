// Prometheus instrumentation for the pricer.
//
// # The one series that is already a contract
//
// `sharpline_pricing_duration_seconds` is not a name this package chose. It is
// read by the dashboard's "Pricing latency" panel
// (deploy/observability/grafana/dashboards/sharpline-overview.json, panel id 13,
// p50 / p95 / p99) and by two rules in
// deploy/observability/rules/sharpline-alerts.yml — the recording rule
// `sharpline:pricing_duration_seconds:p99` and the PricingLatencyHigh alert that
// fires on it above 0.25. Phase 0 wrote all three before any code existed and
// the panel has read "No data" ever since. This file is what fills it.
//
// Its BUCKET BOUNDARIES are part of the same contract, and the alert file says
// why in general terms that apply here exactly: "several rules select a single
// bucket by an exact `le` literal […] if the emitted histogram has no boundary
// at that value the selector matches NOTHING, the rule silently evaluates to
// empty, and the SLI reads as absent rather than as broken." PricingLatencyHigh
// thresholds at 0.25, so 0.25 must be a boundary or the p99 is interpolated
// across the very threshold it is compared against.
//
// # Three series here belong to other packages and are joined, not duplicated
//
// `sharpline_odds_staleness_seconds{stage,league,provider}` and
// `sharpline_odds_clock_skew_total{provider,stage}` are declared by
// internal/ingest/provider; `sharpline_pipeline_latency_seconds{stage,league}`
// is declared by internal/ingest/normalizer. The dashboard's staleness-by-stage
// panel charts received → normalized → priced → fanout so that a regression is
// attributable to one segment, and until now nothing emitted the `priced` slice.
// This package emits it — the SAME series with a different stage label, never a
// parallel `sharpline_pricing_staleness_seconds` that no panel reads and no rule
// can compare against the others.
//
// That means the descriptors must AGREE: identical help text, identical label
// names, identical buckets. They are taken from the declaring packages
// (provider.StalenessBuckets, normalizer.PipelineLatencyBuckets, and the stage
// constants) rather than retyped, because three copies of one boundary list
// eventually differ and the difference is invisible until a quantile is wrong.
// The registration goes through shared(), which adopts an identical
// already-registered collector and FAILS on a descriptor that merely resembles
// one — so a disagreement about help text or label names stops the process at
// startup instead of producing two half-populated series.
//
// # Labels deliberately not set
//
//   - `service`. deploy/observability/prometheus.yml attaches it as a TARGET
//     label; a metric label of the same name would be renamed `exported_service`
//     and the two would drift. Every other package in this repo makes the same
//     choice.
//   - a market, event, selection or book identifier. Tens of thousands of values,
//     and the pricing histogram is aggregated `sum by (le)` with no other
//     grouping anyway. Per-market attribution is a span attribute, not a label.
//   - error text. Bounded classifications only.
package pricing

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Metric namespace and subsystem: together, the `sharpline_pricing_` prefix that
// deploy/observability/prometheus.yml's "every application series is prefixed
// sharpline_" rule requires and that the dashboard already reads.
//
// The identifiers are qualified because enginemetrics.go owns the unqualified
// pair for the `sharpline_pricer_` family it exports. Two subsystems in one
// process is deliberate rather than an accident of two authors: `pricing_` is
// the SERVICE — the frozen contract series the dashboard and PricingLatencyHigh
// read, plus the consume/publish decisions around it — and `pricer_` is the
// ENGINE's own arithmetic instrumentation, which no dashboard panel reads yet
// and which is free to change shape as the engine does.
const (
	serviceNamespace = "sharpline"
	serviceSubsystem = "pricing"
)

// Market results. THE pricer's decision vocabulary, and a closed set: every
// value is written by exactly one branch of Service.HandleMessage.
const (
	// resultPublished: the market was priced and the result reached the bus.
	resultPublished = "published"

	// resultSuppressed: the source record's fingerprint was identical to the one
	// this system last priced, so the INPUT did not move. Pricing is a pure
	// function of the record (see doc.go), so neither did the output.
	resultSuppressed = "suppressed"

	// resultStale: the record's observation instant was strictly older than the
	// state already published, so applying it would regress the compacted
	// snapshot. Skipped and counted.
	resultStale = "stale"

	// resultTombstoned: the market was deleted from odds.normalized, so its
	// entry in price.computed was deleted too. Not doing this leaves a priced
	// market in the snapshot for ever.
	resultTombstoned = "tombstoned"

	// resultInvalid: the record could not be turned into a priced market —
	// wrong message type, unusable key, undecodable payload, or an engine that
	// refused it. PERMANENT: redelivery cannot change the outcome.
	resultInvalid = "invalid"

	// resultFailed: the publish or the tombstone returned an error. TRANSIENT:
	// the offset is not committed and the record is redelivered.
	resultFailed = "failed"
)

// Warm-start outcomes.
const (
	// warmStartOK: price.computed was read to its end offsets and the tracker
	// holds what this system has already priced.
	warmStartOK = "ok"

	// warmStartFailed: the read failed. Pricing proceeds COLD, which reprices
	// and republishes the slate once; see Service.Warm for why that beats
	// refusing to price at all.
	warmStartFailed = "failed"
)

// PricingBuckets returns the boundaries for sharpline_pricing_duration_seconds.
//
// THEY ARE PART OF THE CONTRACT. deploy/observability/rules/sharpline-alerts.yml
// fixes this exact list and requires the boundary 0.25, because PricingLatencyHigh
// compares the p99 against 0.25 and a threshold that falls between two boundaries
// is answered by interpolation rather than by measurement.
//
// The shape of the list is the shape of the work: devigging one market is pure
// CPU over a payload already in memory — a handful of selections through a
// bounded root-finder — so the interesting region is hundreds of microseconds to
// a few milliseconds, and everything above 0.25 exists to catch a pathological
// market rather than to be populated. If the bulk of the mass ever lands above
// 0.05, the engine has started doing I/O.
//
// Exported for the same reason provider.StalenessBuckets and
// normalizer.PipelineLatencyBuckets are: a test that asserts the contract must
// read the same list the emitter uses, or it is asserting its own copy.
func PricingBuckets() []float64 {
	return []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}
}

// Metrics is the pricer's collector set.
//
// Registration happens once, in NewMetrics, and the value is injected — the
// pattern internal/platform/kafka, internal/ingest/provider,
// internal/ingest/normalizer and internal/ingest/writer all follow, for the same
// reason: one process may legitimately build more than one Service, and a
// duplicate registration should fail its startup rather than its code review.
//
// A nil Registerer builds the collectors WITHOUT registering them. That is right
// for a unit test and for any process with no /metrics endpoint: the observe
// calls stay live and cost a few nanoseconds, so no call site needs a nil check.
type Metrics struct {
	duration prometheus.Histogram
	markets  *prometheus.CounterVec

	warmStart  *prometheus.CounterVec
	warmMillis prometheus.Gauge
	tracked    prometheus.Gauge

	// Shared with internal/ingest/provider (staleness, skew) and
	// internal/ingest/normalizer (pipeline). See the file comment.
	staleness *prometheus.HistogramVec
	pipeline  *prometheus.HistogramVec
	clockSkew *prometheus.CounterVec
}

// NewMetrics builds the collectors and registers them on reg.
//
// It returns an error rather than panicking: CLAUDE.md §12 forbids a panic
// outside main, and a registration conflict is a wiring mistake the caller
// reports with the rest of its startup context.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: serviceNamespace,
			Subsystem: serviceSubsystem,
			Name:      "duration_seconds",
			Help: "Time to devig one market and compute fair value, EV and Kelly. " +
				"THE dashboard's pricing-latency panel and the PricingLatencyHigh alert " +
				"read this series; the 0.25 bucket boundary is required so the alert's " +
				"p99 threshold sits on a boundary rather than being interpolated. It " +
				"measures the engine call alone — not the decode, the publish or the " +
				"broker acknowledgement — so a rise here is arithmetic getting slower " +
				"and nothing else.",
			Buckets: PricingBuckets(),
		}),

		markets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: serviceNamespace,
			Subsystem: serviceSubsystem,
			Name:      "markets_total",
			Help: "Markets seen on odds.normalized by what the pricer did with them. " +
				"suppressed means the source fingerprint was unchanged, so the input " +
				"did not move and neither did the price; a healthy steady state has " +
				"some of it, but unlike the normalizer's counter this one is expected " +
				"to be mostly published, because upstream change detection has already " +
				"removed the no-ops. stale is the monotonicity guard refusing to " +
				"regress the compacted snapshot. invalid is permanent and redelivery " +
				"cannot help; failed is transient and the record is redelivered.",
		}, []string{"result"}),

		warmStart: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: serviceNamespace,
			Subsystem: serviceSubsystem,
			Name:      "warm_start_total",
			Help: "Attempts to rebuild the priced-state tracker from the compacted " +
				"price.computed topic. A failed warm start is not fatal — the service " +
				"prices cold and republishes the slate once — but it is the reason a " +
				"deploy would show a burst of published markets.",
		}, []string{"outcome"}),

		warmMillis: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: serviceNamespace,
			Subsystem: serviceSubsystem,
			Name:      "warm_start_duration_seconds",
			Help: "Wall-clock time of the last warm start. It is paid before the " +
				"consumer joins the group, so it is the number a Kubernetes " +
				"startupProbe budget has to be sized against.",
		}),

		tracked: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: serviceNamespace,
			Subsystem: serviceSubsystem,
			Name:      "markets_tracked",
			Help: "Markets this replica holds priced-state for. Compare it against " +
				"the normalizer's fingerprint gauge: far below means suppression " +
				"cannot fire, far above means keys are accumulating and market " +
				"identifiers are not stable.",
		}),

		staleness: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: serviceNamespace,
			Name:      "odds_staleness_seconds",
			Help:      "Age of a price, measured from the provider's own observation instant. stage=received is the provider-attributable share.",
			Buckets:   provider.StalenessBuckets(),
		}, []string{"stage", "league", "provider"}),

		pipeline: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: serviceNamespace,
			Name:      "pipeline_latency_seconds",
			Help:      "Age of a price measured from ingested_at: the share of staleness this system controls, as opposed to the provider's. SLO 2 reads stage=fanout.",
			Buckets:   normalizer.PipelineLatencyBuckets(),
		}, []string{"stage", "league"}),

		clockSkew: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: serviceNamespace,
			Name:      "odds_clock_skew_total",
			Help:      "Prices whose observation instant was in the future, so the staleness observation was clamped to zero.",
		}, []string{"provider", "stage"}),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.duration, m.markets, m.warmStart, m.warmMillis, m.tracked,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("pricing metrics: %w", err)
		}
	}

	var err error
	if m.staleness, err = sharedHistogramVec(reg, m.staleness); err != nil {
		return nil, err
	}
	if m.pipeline, err = sharedHistogramVec(reg, m.pipeline); err != nil {
		return nil, err
	}
	if m.clockSkew, err = sharedCounterVec(reg, m.clockSkew); err != nil {
		return nil, err
	}
	return m, nil
}

// sharedHistogramVec registers a contract histogram another package in this
// process may already own, and adopts the existing collector if so.
//
// AlreadyRegisteredError is returned only for an IDENTICAL descriptor. A
// disagreement about help text, label names or buckets is a different error and
// fails startup, which is the point: two packages emitting different stages of
// one SLO series must agree about the series, and the registry is the only place
// that check can be mechanical.
func sharedHistogramVec(reg prometheus.Registerer, c *prometheus.HistogramVec) (*prometheus.HistogramVec, error) {
	existing, err := shared(reg, c)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return c, nil
	}
	v, ok := existing.(*prometheus.HistogramVec)
	if !ok {
		return nil, fmt.Errorf("pricing metrics: a collector of type %T is already registered "+
			"where a *prometheus.HistogramVec was expected", existing)
	}
	return v, nil
}

// sharedCounterVec is sharedHistogramVec for a counter.
func sharedCounterVec(reg prometheus.Registerer, c *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	existing, err := shared(reg, c)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return c, nil
	}
	v, ok := existing.(*prometheus.CounterVec)
	if !ok {
		return nil, fmt.Errorf("pricing metrics: a collector of type %T is already registered "+
			"where a *prometheus.CounterVec was expected", existing)
	}
	return v, nil
}

// shared returns the already-registered collector, or nil when c was registered.
func shared(reg prometheus.Registerer, c prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(c)
	if err == nil {
		return nil, nil
	}
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		return already.ExistingCollector, nil
	}
	return nil, fmt.Errorf("pricing metrics: %w", err)
}

// observeDuration records one engine call.
//
// It is the ENGINE CALL and nothing else — not the JSON decode that precedes it
// and not the synchronous publish that follows. Folding the publish in would
// make a slow broker read as slow arithmetic, and PricingLatencyHigh would page
// the wrong component; the produce path already has
// sharpline_kafka_produce_duration_seconds.
func (m *Metrics) observeDuration(d time.Duration) {
	if m == nil {
		return
	}
	m.duration.Observe(d.Seconds())
}

// observeMarket counts one pricing decision.
func (m *Metrics) observeMarket(result string) {
	if m == nil {
		return
	}
	m.markets.WithLabelValues(result).Inc()
}

// observeWarmStart records one warm-start attempt.
func (m *Metrics) observeWarmStart(outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.warmStart.WithLabelValues(outcome).Inc()
	m.warmMillis.Set(d.Seconds())
}

// observeTracked publishes the tracker size.
func (m *Metrics) observeTracked(n int) {
	if m == nil {
		return
	}
	m.tracked.Set(float64(n))
}

// observePriced records the two staleness quantities for a market that has just
// been priced and published.
//
// # Staleness is observed ONCE PER PRICE, not once per record
//
// The dashboard defines freshness as "the instant the price is written to the
// client socket − observed_at carried on THAT PRICE". Observing once per record
// with the newest instant would report the freshest book's age for every book on
// the market, which is the number that flatters the pipeline most.
// internal/ingest/normalizer makes the same choice at stage=normalized, so the
// two stages on the panel are computed the same way and their difference is the
// cost of this segment rather than an artefact of how each was counted.
//
// # Negative staleness is clamped AND counted, never swallowed
//
// A provider may stamp an observation instant slightly in the future.
// domain.Price.Age returns the negative duration deliberately so "a monitor can
// detect the skew instead of silently reporting healthy staleness", and a
// histogram destroys that signal — a negative sample lands in the lowest bucket
// and reads as EXCELLENT. sharpline-alerts.yml fixes the contract: clamp the
// observation at 0 and increment sharpline_odds_clock_skew_total, which
// ProviderClockSkewDetected alerts on, so the clamp is never silent.
func (m *Metrics) observePriced(rec normalizer.NormalizedMarket, at time.Time) {
	if m == nil {
		return
	}
	league := rec.League.ID
	skew := 0
	for _, p := range rec.Prices {
		if p.ObservedAt.IsZero() {
			continue
		}
		age := at.Sub(p.ObservedAt).Seconds()
		if age < 0 {
			skew++
			age = 0
		}
		m.staleness.WithLabelValues(provider.StagePriced, league, rec.Provider).Observe(age)
	}
	if skew > 0 {
		m.clockSkew.WithLabelValues(rec.Provider, provider.StagePriced).Add(float64(skew))
	}
	if !rec.IngestedAt.IsZero() {
		// One observation per record rather than per price: ingested_at is a
		// property of the payload, not of a quote, so N identical samples would
		// only weight the histogram by book count.
		lat := at.Sub(rec.IngestedAt).Seconds()
		if lat < 0 {
			lat = 0
		}
		m.pipeline.WithLabelValues(provider.StagePriced, league).Observe(lat)
	}
}
