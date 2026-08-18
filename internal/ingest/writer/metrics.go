// Prometheus instrumentation for the Timescale writer.
//
// # What this file deliberately does NOT emit
//
// BUS LAG. The Grafana dashboard's two "Bus lag" panels read
// sharpline_kafka_consumer_lag_records{group,topic,partition} and
// sharpline_kafka_consumer_lag_seconds{group,topic,partition}, and
// internal/platform/kafka's Consumer already exports both from its background
// lag refresher — filtered to the partitions THIS member owns, which is the
// property that makes the dashboard's `sum by (group, topic)` equal the true
// group lag instead of a multiple of it. Emitting a second series here would
// double-count the writer's partitions on a panel that already works.
//
// The obligation this creates on the caller is stated once, here, because it is
// invisible at the call site: the Consumer given to Run MUST be built with
// DisableLagExport left false (its default). Turning it off is documented as
// being for a one-shot job with no /metrics endpoint, and the writer is not one.
//
// STALENESS AND PIPELINE-LATENCY STAGES.
// sharpline_odds_staleness_seconds{stage,league,provider} and
// sharpline_pipeline_latency_seconds{stage,league} are a phase-0 contract with a
// CLOSED set of stage values — deploy/observability/rules/sharpline-alerts.yml
// enumerates them as "received | normalized | priced" plus the SLO's "fanout",
// where fanout is defined as the instant a price is written to the CLIENT
// SOCKET. The writer is not on that path: it is a storage sink running beside
// the pricer, and a row landing in the hypertable is not a user-visible event.
//
// Adding a fourth stage value would be a unilateral change to a contract this
// package does not own, and it would additionally require registering the shared
// histograms — which `ingest`, `pricer` and `stream` will each also register in
// the same process or a sibling one, where a disagreement about help text or
// bucket boundaries turns into a startup failure rather than a merge conflict.
//
// The two quantities are still measured, because they are genuinely the
// writer's business — migrations/00003 defines (created_at − ingested_at) as
// "bus lag plus write latency, made durable" and observed_at as "the subtrahend
// in every staleness measurement". They are emitted under
// sharpline_writer_* names that nothing else can collide with. If a later phase
// decides a stage="stored" series belongs on the shared histogram, that is an
// edit to the rules file and the dashboard first, and to this file second.
//
// # Labels deliberately not set
//
//   - `service`. deploy/observability/prometheus.yml attaches it as a TARGET
//     label on every scrape job; a metric label of the same name is renamed
//     `exported_service` on ingest and the two drift. internal/platform/kafka
//     makes the same choice for the same reason.
//   - `league` and `provider`. Both are provider-supplied strings whose value
//     set grows whenever the operator adds a league or an adapter, on series
//     that are written once per record. The per-league breakdown that would
//     justify the cardinality already exists on the staleness histogram the
//     fanout stage owns.
//   - the market, selection or book id. Tens of thousands of values.
//   - error text. Bounded classifications only.
package writer

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric namespace and subsystem: together, the `sharpline_writer_` prefix.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "writer"
)

// Message outcomes. A closed set; every value appears in a code path.
const (
	// msgWritten: the record's catalogue and quotes were committed.
	msgWritten = "written"
	// msgTombstone: a deletion on the compacted topic. History is retained by
	// design; see doc.go.
	msgTombstone = "tombstone"
	// msgInvalid: the record could not be turned into domain values. PERMANENT
	// — redelivering the same bytes cannot change it.
	msgInvalid = "invalid"
	// msgFailed: the transaction failed. May be transient; the offset is not
	// committed and the record is redelivered.
	msgFailed = "failed"
)

// Price-row outcomes.
const (
	// rowInserted: a new observation, stored.
	rowInserted = "inserted"
	// rowDuplicate: the natural key was already present, so ON CONFLICT DO
	// NOTHING skipped it. Normal on a redelivery; a SUSTAINED rate means the
	// consumer group is thrashing.
	rowDuplicate = "duplicate"
)

// Catalogue upsert outcomes, resolved exactly from `RETURNING (xmax = 0)`
// rather than inferred from a row count.
const (
	// upsertInserted: the row did not exist. A new event, market, book or
	// selection appearing is the interesting one of the three.
	upsertInserted = "inserted"
	// upsertUpdated: the row existed and something changed — a status, a line,
	// a score.
	upsertUpdated = "updated"
	// upsertUnchanged: the row existed and the upsert's WHERE clause declined
	// it, either because nothing differed or because the payload's observation
	// was older than the stored one. The steady state, and the reason the
	// set_updated_at triggers are not firing on every poll.
	upsertUnchanged = "unchanged"
)

// Flush outcomes.
const (
	outcomeOK    = "ok"
	outcomeError = "error"
)

// flushBuckets covers one transaction: catalogue upserts plus one multi-row
// insert plus a COMMIT. deploy/postgres/postgresql.conf keeps
// synchronous_commit ON — deliberately, because the ledger is the source of
// truth — so every commit is an fsync and the floor is milliseconds rather than
// microseconds. The tail runs to 10s because that is FlushTimeout's default and
// a sample in the top bucket is one that is about to be abandoned.
var flushBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// batchBuckets counts price rows per flush. migrations/00003 sizes a market at
// 6 selections × 10-20 books, so the interesting region is 50-150 and the
// boundaries either side of it are what distinguish a full slate sweep from a
// single-book correction.
var batchBuckets = []float64{1, 2, 5, 10, 25, 50, 100, 200, 500, 1000, 2500, 5000}

// lagBuckets is shared by the two age histograms.
//
// The boundaries are not arbitrary: 0.5 and 120 are the two the recording rules
// in deploy/observability/rules/sharpline-alerts.yml use as `le` selectors for
// the pipeline-latency and odds-freshness SLO ratios, and 0.25 is the
// PipelineLatencyP99Degraded threshold. Keeping them here means the writer's
// numbers can be read on the same scale as the SLOs even though they are not
// the SLO series.
var lagBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600,
}

// Metrics is the writer's collector set.
//
// Registration happens once, in NewMetrics, and the value is injected into the
// Writer — the pattern internal/platform/kafka uses, and for the same reason:
// one process may legitimately run more than one Writer (one per topic
// partition set, if a later phase splits them), and duplicate registration
// would fail its startup rather than its code review.
//
// A nil Registerer builds the collectors WITHOUT registering them. That is right
// for a unit test and for any process with no /metrics endpoint: the observe
// calls stay live and cost a few nanoseconds, so no call site needs a nil check.
type Metrics struct {
	messages   *prometheus.CounterVec
	priceRows  *prometheus.CounterVec
	catalogue  *prometheus.CounterVec
	batchRows  prometheus.Histogram
	flush      *prometheus.HistogramVec
	observeLag prometheus.Histogram
	busLag     prometheus.Histogram
}

// NewMetrics builds the collectors and registers them on reg.
//
// It returns an error rather than panicking: CLAUDE.md §12 forbids a panic
// outside main, and a duplicate registration is a wiring mistake the caller can
// report with the rest of its startup context.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "messages_total",
			Help: "odds.normalized records handled by the Timescale writer, by outcome " +
				"(written|tombstone|invalid|failed).",
		}, []string{"outcome"}),

		priceRows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "price_rows_total",
			Help: "Price rows offered to the prices hypertable, by outcome. " +
				"inserted=stored; duplicate=the natural key (selection, book, observed_at) " +
				"was already present and ON CONFLICT DO NOTHING skipped it, which is what " +
				"makes at-least-once redelivery harmless on an append-only table.",
		}, []string{"outcome"}),

		catalogue: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "catalogue_upserts_total",
			Help: "Catalogue rows upserted from the stream, by table and outcome " +
				"(inserted|updated|unchanged). unchanged is the steady state: the upserts " +
				"are guarded so an identical or older observation writes nothing and does " +
				"not fire the set_updated_at trigger.",
		}, []string{"table", "outcome"}),

		batchRows: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "batch_rows",
			Help: "Price rows written per flush. One flush is one odds.normalized record " +
				"— a whole market across every book — inserted as one multi-row statement.",
			Buckets: batchBuckets,
		}),

		flush: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "flush_duration_seconds",
			Help: "Wall-clock time for one write transaction: catalogue upserts, the " +
				"multi-row price insert, and COMMIT. The handler does not return until " +
				"this completes, so it is also the writer's contribution to consumer lag.",
			Buckets: flushBuckets,
		}, []string{"outcome"}),

		observeLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "observation_lag_seconds",
			Help: "Age of a quote when its row committed: (commit instant - observed_at), " +
				"taken from the OLDEST quote in the batch. The storage-side analogue of " +
				"the headline staleness SLO; it is NOT that SLO, which is measured at " +
				"fanout to the client socket.",
			Buckets: lagBuckets,
		}),

		busLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "bus_lag_seconds",
			Help: "(commit instant - ingested_at): Kafka lag plus write latency, i.e. the " +
				"quantity migrations/00003 says (created_at - ingested_at) makes durable " +
				"on every row. Rises on a replay, because ingested_at is the original " +
				"instant and is never re-stamped.",
			Buckets: lagBuckets,
		}),
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
		m.messages, m.priceRows, m.catalogue, m.batchRows, m.flush, m.observeLag, m.busLag,
	}
}

// observeMessage counts one handled record.
func (m *Metrics) observeMessage(outcome string) {
	m.messages.WithLabelValues(outcome).Inc()
}

// observePriceRows records how a batch of price rows landed.
//
// offered is what the statement was given; inserted is what Postgres reports it
// actually stored. The difference is the number of duplicates the natural-key
// index absorbed, and reporting it as a subtraction rather than as a separate
// count is what makes the two series sum to the offered total by construction.
func (m *Metrics) observePriceRows(offered, inserted int) {
	if inserted > 0 {
		m.priceRows.WithLabelValues(rowInserted).Add(float64(inserted))
	}
	if d := offered - inserted; d > 0 {
		m.priceRows.WithLabelValues(rowDuplicate).Add(float64(d))
	}
	m.batchRows.Observe(float64(offered))
}

// observeCatalogue counts one upsert outcome.
func (m *Metrics) observeCatalogue(table, outcome string, n int) {
	if n <= 0 {
		return
	}
	m.catalogue.WithLabelValues(table, outcome).Add(float64(n))
}

// observeFlush records one transaction.
func (m *Metrics) observeFlush(d time.Duration, err error) {
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
	}
	m.flush.WithLabelValues(outcome).Observe(d.Seconds())
}

// observeLags records the two ages of a committed batch.
//
// Negative values are clamped to zero, and the clamp is the honest choice here
// rather than a cover-up. observed_at comes from the PROVIDER's clock and
// ingested_at from the ingest container's, so skew between hosts legitimately
// produces a negative difference; migrations/00003 declines a
// CHECK (ingested_at >= observed_at) for exactly this reason and keeps
// domain.Price.Age() signed so a monitor can DETECT the skew. What detects it is
// sharpline_odds_clock_skew_total, which ingest owns and which the
// ProviderClockSkewDetected alert already watches. A negative sample on a
// histogram, by contrast, lands in the lowest bucket and is indistinguishable
// from a fast one, so clamping loses nothing that is not measured better
// elsewhere.
func (m *Metrics) observeLags(committedAt, oldestObservedAt, ingestedAt time.Time) {
	if !oldestObservedAt.IsZero() {
		m.observeLag.Observe(nonNegativeSeconds(committedAt.Sub(oldestObservedAt)))
	}
	if !ingestedAt.IsZero() {
		m.busLag.Observe(nonNegativeSeconds(committedAt.Sub(ingestedAt)))
	}
}

func nonNegativeSeconds(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Seconds()
}
