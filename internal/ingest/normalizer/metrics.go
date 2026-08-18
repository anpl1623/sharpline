// Prometheus instrumentation for the normalizer.
//
// # The one number this phase is judged on
//
// `sharpline_normalizer_markets_total{result}`. CLAUDE.md §5 says "most polls
// return identical data and must not generate bus traffic", and this counter is
// the only thing that says whether that is true in a running system. A healthy
// pipeline is overwhelmingly result="suppressed"; a pipeline where everything is
// result="published" means the fingerprint covers something that advances on
// every poll and the bus is carrying no-ops. Both failures are silent in every
// other signal — the board looks right either way — so the ratio is the check.
//
// The dashboard's existing "Poll outcome" panel reads
// sharpline_ingest_polls_total{result="unchanged"}, which is the SEPARATE,
// coarser, pre-bus suppression internal/ingest applies to whole payloads. The
// two are related and not the same: that one keeps an unchanged payload off
// odds.raw.*, this one keeps an unchanged MARKET off odds.normalized, and a poll
// that moved one market out of forty is "changed" there and 1/40 published here.
//
// # Two series here are a CONTRACT and are shared with other packages
//
// sharpline_odds_staleness_seconds{stage,league,provider} and
// sharpline_odds_clock_skew_total{provider,stage} are declared by
// internal/ingest/provider, which registers them for stage="received" in the
// same process. sharpline_pipeline_latency_seconds{stage,league} is declared
// here because nothing had defined it yet, and `pricer` and `stream` will emit
// their own stages onto it.
//
// One process therefore has two packages wanting the same collector. That is
// what prometheus.AlreadyRegisteredError exists for: shared attempts the
// registration and, when the identical descriptor is already present, adopts the
// existing collector instead. It is NOT a "duplicate registration" workaround —
// a descriptor that differs in help text or label names produces a plain error
// and fails startup loudly, which is the behaviour we want, and
// TestSharedContractSeriesRegisterAlongsideTheProviderSet pins it by registering
// both packages' sets on one registry.
//
// THE ORDER IS SYMMETRIC, and deliberately so. It was not: provider's
// NewMetrics registered directly and treated AlreadyRegisteredError as a
// failure, so it had to be constructed BEFORE this one and reversing the two
// lines in cmd/ingest failed the process at startup. provider now adopts the two
// shared series the same way, and the test above asserts BOTH orders succeed and
// land on one collector — so the construction order is no longer load-bearing.
//
// Bucket boundaries are part of the contract, not a tuning choice.
// deploy/observability/rules/sharpline-alerts.yml: "several rules select a
// single bucket by an exact `le` literal […] if the emitted histogram has no
// boundary at that value the selector matches NOTHING, the rule silently
// evaluates to empty, and the SLI reads as absent rather than as broken."
//
// # Labels deliberately not set
//
//   - `service`. deploy/observability/prometheus.yml attaches it as a TARGET
//     label; a metric label of the same name is renamed `exported_service` and
//     the two drift. internal/platform/kafka and internal/ingest/writer make the
//     same choice.
//   - a market, event, selection or book identifier. Tens of thousands of values.
//   - error text. Bounded classifications only — Reason exists precisely so that
//     an untrusted provider string never becomes a label value.
package normalizer

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Metric namespace and subsystem: together, the `sharpline_normalizer_` prefix.
// deploy/observability/prometheus.yml states the rule that every application
// series is prefixed `sharpline_`.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "normalizer"
)

// Record outcomes. A closed set; every value appears in a code path.
const (
	// recordMapped: the payload decoded and produced at least one market view.
	recordMapped = "mapped"
	// recordRejected: the payload could not be decoded or the EVENT could not
	// be represented, so nothing on it could be.
	recordRejected = "rejected"
	// recordTombstone: a deletion on odds.raw.{provider}. That topic is
	// retention-based and this package never writes one, so it means an operator
	// or another producer did.
	recordTombstone = "tombstone"
	// recordUnsupported: an envelope this build does not read.
	recordUnsupported = "unsupported"
)

// Market results. THE change-detection vocabulary. A closed set.
const (
	// resultPublished: the fingerprint differed from the stored one, so the
	// market moved and the record went on the bus.
	resultPublished = "published"
	// resultSuppressed: the fingerprint was identical and the refresh ceiling
	// had not elapsed. This is the outcome CLAUDE.md §5 exists to produce.
	resultSuppressed = "suppressed"
	// resultRefreshed: the fingerprint was identical but the last publication
	// was older than RefreshAfter, so it was republished anyway. The bounded
	// trickle that makes any fingerprint defect self-heal.
	resultRefreshed = "refreshed"
	// resultStale: the payload's observation instant was strictly older than
	// the state already published, so applying it would regress the compacted
	// snapshot. Skipped and counted.
	resultStale = "stale"
	// resultFailed: the produce returned an error. The offset is not committed
	// and the record is redelivered.
	resultFailed = "failed"
)

// Warm-start outcomes.
const (
	// warmStartOK: the compacted topic was read to its end offsets.
	warmStartOK = "ok"
	// warmStartFailed: the read failed. Normalization proceeds COLD, which
	// republishes the slate once; see Normalizer.Warm for why that is the right
	// trade against refusing to normalize at all.
	warmStartFailed = "failed"
)

// mapBuckets covers mapping one raw event onto the domain. It is pure CPU over a
// payload already in memory, so the interesting region is tens of microseconds
// and the tail exists to catch a pathological payload rather than to be
// populated.
var mapBuckets = []float64{
	0.00001, 0.000025, 0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.05,
}

// PipelineLatencyBuckets returns the boundaries for
// sharpline_pipeline_latency_seconds.
//
// THEY ARE PART OF THE CONTRACT. sharpline-alerts.yml requires the exact
// boundary 0.5 — it is SLO 2's compliance bucket — and fixes the rest of the
// list. Prometheus renders integral bounds without a decimal point, so le="0.5"
// and le="1" are the labels the rules match.
//
// Exported for the same reason provider.StalenessBuckets is: `pricer` and
// `stream` emit their own stages onto this histogram, and three copies of one
// slice eventually differ. It lives here only because this package is the first
// to need it; a shared observability package is the right long-term home.
func PipelineLatencyBuckets() []float64 {
	return []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
}

// Metrics is the normalizer's collector set.
//
// Registration happens once, in NewMetrics, and the value is injected — the
// pattern internal/platform/kafka, internal/ingest/provider and
// internal/ingest/writer all follow, for the same reason: one process may
// legitimately build more than one Normalizer, and duplicate registration should
// fail its startup rather than its code review.
//
// A nil Registerer builds the collectors WITHOUT registering them. That is right
// for a unit test and for any process with no /metrics endpoint: the observe
// calls stay live and cost a few nanoseconds, so no call site needs a nil check.
type Metrics struct {
	records    *prometheus.CounterVec
	markets    *prometheus.CounterVec
	rejects    *prometheus.CounterVec
	warmStart  *prometheus.CounterVec
	warmMillis prometheus.Gauge
	held       prometheus.Gauge
	mismatches prometheus.Counter
	mapping    prometheus.Histogram

	// Shared with internal/ingest/provider (staleness, skew) and with the
	// pricer and stream stages (pipeline). See the file comment.
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
		records: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "records_total",
			Help: "odds.raw.{provider} records handled, by outcome " +
				"(mapped|rejected|tombstone|unsupported).",
		}, []string{"outcome"}),

		markets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "markets_total",
			Help: "Normalized markets by change-detection result. suppressed is the " +
				"hash deciding a poll was a no-op (CLAUDE.md §5: most polls return " +
				"identical data and must not generate bus traffic), so a healthy " +
				"pipeline is mostly suppressed; all-published means the fingerprint " +
				"covers something that advances every poll. refreshed is the bounded " +
				"republication of an unchanged market past the suppression ceiling.",
		}, []string{"result"}),

		rejects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "rejects_total",
			Help: "Payload fragments that could not be normalized, by scope and bounded " +
				"reason. reason is NEVER derived from provider text. A steady " +
				"unsupported_market rate is expected: the provider serves more markets " +
				"than this board shows.",
		}, []string{"scope", "reason"}),

		warmStart: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "warm_start_total",
			Help: "Attempts to rebuild change-detection state from the compacted " +
				"odds.normalized topic. A failed warm start is not fatal — the process " +
				"normalizes cold and republishes the slate once — but it is the reason " +
				"a deploy would show a burst of published markets.",
		}, []string{"outcome"}),

		warmMillis: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "warm_start_duration_seconds",
			Help: "Wall-clock time of the last warm start. It bounds how long the first " +
				"record waits, so it is the number a Kubernetes startupProbe budget has " +
				"to be sized against.",
		}),

		held: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "fingerprints",
			Help: "Markets currently held in the change-detection store. Compare it " +
				"against the market count on the board: far below means suppression " +
				"cannot fire, far above means keys are accumulating and market " +
				"identifiers are not stable.",
		}),

		mismatches: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "fingerprint_mismatches_total",
			Help: "Warm-start records whose recomputed hash differed from the " +
				"fingerprint the producer stored on them. Non-zero means the hash " +
				"or the payload shape changed without the schema version being " +
				"bumped, and every affected market will republish once.",
		}),

		mapping: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "map_duration_seconds",
			Help: "Time to map one raw event onto domain values and hash its markets. " +
				"Pure CPU; it is this package's share of consumer lag.",
			Buckets: mapBuckets,
		}),

		staleness: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "odds_staleness_seconds",
			Help:      "Age of a price, measured from the provider's own observation instant. stage=received is the provider-attributable share.",
			Buckets:   provider.StalenessBuckets(),
		}, []string{"stage", "league", "provider"}),

		pipeline: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "pipeline_latency_seconds",
			Help:      "Age of a price measured from ingested_at: the share of staleness this system controls, as opposed to the provider's. SLO 2 reads stage=fanout.",
			Buckets:   PipelineLatencyBuckets(),
		}, []string{"stage", "league"}),

		clockSkew: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "odds_clock_skew_total",
			Help:      "Prices whose observation instant was in the future, so the staleness observation was clamped to zero.",
		}, []string{"provider", "stage"}),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.records, m.markets, m.rejects, m.warmStart, m.warmMillis, m.held, m.mismatches, m.mapping,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("normalizer metrics: %w", err)
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

// sharedHistogramVec registers a contract histogram that another package in this
// process may already own, and adopts the existing collector if so.
//
// AlreadyRegisteredError is returned only for an IDENTICAL descriptor. A
// disagreement about help text or label names is a different error and fails
// startup, which is the point: two packages emitting different stages of one SLO
// series must agree about the series, and the registry is the only place that
// check can be mechanical.
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
		return nil, fmt.Errorf("normalizer metrics: a collector of type %T is already registered "+
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
		return nil, fmt.Errorf("normalizer metrics: a collector of type %T is already registered "+
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
	return nil, fmt.Errorf("normalizer metrics: %w", err)
}

// observeRecord counts one consumed raw record.
func (m *Metrics) observeRecord(outcome string) {
	if m == nil {
		return
	}
	m.records.WithLabelValues(outcome).Inc()
}

// observeMarket counts one change-detection decision.
func (m *Metrics) observeMarket(result string) {
	if m == nil {
		return
	}
	m.markets.WithLabelValues(result).Inc()
}

// observeReject counts one rejection. It is the metric half of Reject.
func (m *Metrics) observeReject(r Reject) {
	if m == nil {
		return
	}
	m.rejects.WithLabelValues(string(r.Scope), string(r.Reason)).Inc()
}

// observeWarmStart records one warm-start attempt.
func (m *Metrics) observeWarmStart(outcome string, d time.Duration, mismatches int) {
	if m == nil {
		return
	}
	m.warmStart.WithLabelValues(outcome).Inc()
	m.warmMillis.Set(d.Seconds())
	if mismatches > 0 {
		m.mismatches.Add(float64(mismatches))
	}
}

// observeHeld publishes the store size.
func (m *Metrics) observeHeld(n int) {
	if m == nil {
		return
	}
	m.held.Set(float64(n))
}

// observeMapping records one mapping pass.
func (m *Metrics) observeMapping(d time.Duration) {
	if m == nil {
		return
	}
	m.mapping.Observe(d.Seconds())
}

// observePublished records the two staleness quantities for every price in a
// record that was just written to the bus.
//
// # It is observed ONCE PER PRICE, not once per record
//
// The dashboard defines freshness as "the instant the price is written to the
// client socket − observed_at carried on THAT PRICE". Observing once per record
// with the newest instant would report the freshest book's age for every book on
// the market, which is the number that flatters the pipeline most.
//
// # Negative staleness is clamped AND counted, never swallowed
//
// A provider may stamp an observation instant slightly in the future.
// domain.Price.Age returns the negative duration deliberately, so that "a
// monitor can detect the skew instead of silently reporting healthy staleness",
// and migrations/00003 declines a CHECK constraint to keep the skewed value
// storable. A histogram destroys that signal — a negative sample lands in the
// lowest bucket and reads as EXCELLENT — so sharpline-alerts.yml fixes the
// contract: clamp the observation at 0 and increment
// sharpline_odds_clock_skew_total. ProviderClockSkewDetected alerts on the
// counter, so the clamp is never silent.
func (m *Metrics) observePublished(rec NormalizedMarket, at time.Time) {
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
		m.staleness.WithLabelValues(provider.StageNormalized, league, rec.Provider).Observe(age)
	}
	if skew > 0 {
		m.clockSkew.WithLabelValues(rec.Provider, provider.StageNormalized).Add(float64(skew))
	}
	if !rec.IngestedAt.IsZero() {
		// One observation per record rather than per price: ingested_at is a
		// property of the payload, not of a quote, so N identical samples would
		// only weight the histogram by book count.
		lat := at.Sub(rec.IngestedAt).Seconds()
		if lat < 0 {
			lat = 0
		}
		m.pipeline.WithLabelValues(provider.StageNormalized, league).Observe(lat)
	}
}
