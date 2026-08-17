// Prometheus instrumentation for the bus layer.
//
// # Metric names are a contract, and six of them are read from outside this package
//
// deploy/observability/prometheus.yml states the rule — "every application
// series is prefixed `sharpline_`" — and the Grafana dashboard plus
// deploy/observability/rules/sharpline-alerts.yml are written against specific
// names. Six `sharpline_kafka_` series are named there, and each is matched
// EXACTLY, character for character, with the labels their PromQL aggregates by.
//
// Three PREDATE this package. They were written into the dashboard in phase 0
// against a bus that did not exist yet, so their names were a specification this
// code had to satisfy rather than a description of what it emits:
//
//	sharpline_kafka_consumer_lag_records{group,topic}       dashboard panel 14; alert KafkaConsumerLagHigh
//	sharpline_kafka_consumer_lag_seconds{group,topic}       dashboard panel 15; alert KafkaConsumerLagSecondsHigh
//	sharpline_kafka_consumer_group_rebalances_total{group}  dashboard panel 15; alert KafkaRebalanceStorm (the alert is new; the panel is not)
//
// The braces above list the labels the PromQL GROUPS BY, not the full label set —
// both lag gauges also carry `partition`, which aggregates away under the
// dashboard's sum and max. doc.go explains why it is there.
//
// Three more were added to deploy/observability in the same phase as this package
// — so they were named here first and adopted there — and they are exactly as
// binding now that a panel and an alert read them:
//
//	sharpline_kafka_produce_records_total{topic,outcome}    dashboard panel 23; alert KafkaProduceErrors (outcome="error")
//	sharpline_kafka_produce_duration_seconds{topic,outcome} dashboard panel 23, via its _bucket series and histogram_quantile
//	sharpline_kafka_offset_commits_total{group,outcome}     dashboard panel 24; alert KafkaOffsetCommitErrors (outcome="error")
//
// The direction a name travelled does not change the obligation: renaming any of
// the six, or dropping a label its PromQL groups by, breaks a panel or an alert
// silently — Prometheus answers a query for a series that no longer exists with
// no data rather than with an error. Read the dashboard JSON and the rules file
// before touching one, not this comment.
//
// Every other series here is internal to the package for now. Each definition
// below states the PromQL a panel would use, so a dashboard author does not have
// to guess.
//
// # One Metrics value per process, shared
//
// internal/platform/postgres registers its collectors inside Connect and treats
// two pools on one registry as a programming error. That cannot work here: a
// service legitimately has both a producer and a consumer — pricer consumes
// odds.normalized and produces price.computed — and duplicate registration would
// fail its startup.
//
// So registration happens exactly once, in NewMetrics, and the resulting value is
// injected into every producer, consumer and snapshotter in the process. Passing
// nil builds the collectors WITHOUT registering them, which is right for a unit
// test and for a one-shot job that serves no /metrics endpoint: the observe calls
// stay live and cost a few nanoseconds, so no call site needs a nil check.
//
// # Labels deliberately NOT set
//
//   - `service`. prometheus.yml attaches it as a TARGET label on every scrape
//     job. A metric label of the same name would be renamed `exported_service`
//     on ingest and the two would drift.
//   - the record key, the message id, or any payload field. Keys are market and
//     wager identifiers: tens of thousands of them, and on wager.events they are
//     user-linked. Unbounded cardinality and a privacy leak in one label.
//   - the error text. Bounded classifications only — see errorCode.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Metric namespace and subsystem. Together they produce the `sharpline_kafka_`
// prefix the dashboard and the alert rules already reference.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "kafka"
)

// Outcome label values. A closed set: every one appears in a code path below and
// nothing else is ever written to these labels.
const (
	outcomeOK    = "ok"
	outcomeError = "error"

	connectOK        = "ok"
	connectRetryable = "retryable"
	connectFatal     = "fatal"

	snapshotKindValue     = "value"
	snapshotKindTombstone = "tombstone"
)

// Histogram buckets.
//
// produceBuckets runs from 1ms because a local single-broker produce with
// linger=0 lands there, and out to 30s because that is the odds producer's
// RecordDeliveryTimeout — a batch in the top bucket is one that is about to be
// abandoned.
var produceBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// handlerBuckets is the per-message processing time. The boundary at 0.25 is
// shared with sharpline_pricing_duration_seconds' alert threshold
// (PricingLatencyHigh fires above 250ms), so the two graphs agree on where slow
// starts.
var handlerBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// commitBuckets is tighter: an offset commit is one round trip to the group
// coordinator, and it blocks the consume loop, so anything past a second is
// pathological rather than merely slow.
var commitBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// probeBuckets bounds the readiness round trip. The whole budget is
// httpx.DefaultReadinessTimeout (3s), so measuring past that is measuring a
// timeout.
var probeBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3,
}

// batchBuckets counts records per produce batch. Powers of two from 1, because
// the question it answers — "is anything actually batching?" — is answered by
// the order of magnitude, not by the exact count.
var batchBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024}

// snapshotBuckets runs to 60s: a snapshot read of a compacted topic is a
// startup-path operation, and one that takes a minute is one that will time out
// a Kubernetes startupProbe.
var snapshotBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Metrics holds every collector this package owns.
//
// It is a value, never a package-level variable (CLAUDE.md §12 forbids global
// mutable state), and it is safe for concurrent use — every field is a
// Prometheus collector, all of which are.
type Metrics struct {
	// ---- contract series (dashboard + alerts already reference these) ----
	consumerLagRecords *prometheus.GaugeVec   // group, topic, partition
	consumerLagSeconds *prometheus.GaugeVec   // group, topic, partition
	rebalances         *prometheus.CounterVec // group
	produceRecords     *prometheus.CounterVec // topic, outcome
	offsetCommits      *prometheus.CounterVec // group, outcome

	// ---- producer ----
	produceDuration  *prometheus.HistogramVec // topic, outcome
	produceBytes     *prometheus.CounterVec   // topic
	produceErrors    *prometheus.CounterVec   // topic, code
	produceBatchSize *prometheus.HistogramVec // topic
	bufferedRecords  prometheus.Gauge
	dataLoss         *prometheus.CounterVec // topic
	tombstones       *prometheus.CounterVec // topic

	// ---- consumer ----
	consumeRecords  *prometheus.CounterVec   // group, topic
	consumeBytes    *prometheus.CounterVec   // group, topic
	handlerDuration *prometheus.HistogramVec // group, topic, outcome
	handlerErrors   *prometheus.CounterVec   // group, topic
	decodeErrors    *prometheus.CounterVec   // group, topic, reason
	commitDuration  *prometheus.HistogramVec // group, outcome
	fetchErrors     *prometheus.CounterVec   // group, topic, code
	assignedGauge   *prometheus.GaugeVec     // group, topic
	revoked         *prometheus.CounterVec   // group, topic
	lost            *prometheus.CounterVec   // group, topic
	lagErrors       *prometheus.CounterVec   // group

	// ---- snapshot ----
	snapshotDuration *prometheus.HistogramVec // topic
	snapshotRecords  *prometheus.CounterVec   // topic, kind

	// ---- cluster ----
	up              prometheus.Gauge
	probeDuration   prometheus.Histogram
	connectAttempts *prometheus.CounterVec // outcome
}

// NewMetrics builds the collectors and registers them on reg.
//
// Call it ONCE per process and pass the result to every producer, consumer and
// snapshotter. reg may be nil, which builds the collectors but registers
// nothing.
//
// Registration failure is returned, not swallowed: two Metrics values on one
// registry means two halves of a process reporting under one series, and it
// fails at startup rather than producing plausible nonsense.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
		}, labels)
	}
	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
		}, labels)
	}
	histogram := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
			Buckets: buckets,
		}, labels)
	}

	m := &Metrics{
		// -------------------------------------------------------------- contract
		consumerLagRecords: gauge("consumer_lag_records",
			"Records between this member's committed offset and the partition's end offset, for the partitions "+
				"IT CURRENTLY OWNS. Group lag is readable cluster-wide from any member, so exporting all of it from "+
				"every member would multiply the graph by the member count; filtering to the owned partitions is what "+
				"makes the dashboard's sum by (group, topic) equal the true group lag. "+
				"Panel: sum by (group, topic) (sharpline_kafka_consumer_lag_records).",
			"group", "topic", "partition"),

		consumerLagSeconds: gauge("consumer_lag_seconds",
			"Wall-clock age of the newest record this member has committed on a partition, or 0 when it is caught up. "+
				"This is the lag number that maps onto the staleness SLO. Absent until the member has committed at "+
				"least one record on the partition — a gap in the graph, never a fabricated zero. "+
				"Panel: max by (group, topic) (sharpline_kafka_consumer_lag_seconds).",
			"group", "topic", "partition"),

		rebalances: counter("consumer_group_rebalances_total",
			"Completed group rebalances observed by this member, counted once per rebalance when the assignment "+
				"callback fires. Revocations and losses are counted separately (partitions_revoked_total, "+
				"partitions_lost_total) so that this series is not inflated three-fold by the dashboard's sum. "+
				"Panel: sum by (group) (rate(sharpline_kafka_consumer_group_rebalances_total[$__rate_interval])).",
			"group"),

		produceRecords: counter("produce_records_total",
			"Records whose produce completed, by topic and outcome. outcome=\"error\" means the record was NOT "+
				"written: on a compacted topic that is a market whose snapshot stays stale until the next change to "+
				"the same key; on wager.events it is a missing audit entry and the caller is expected to have "+
				"failed the surrounding transaction.",
			"topic", "outcome"),

		offsetCommits: counter("offset_commits_total",
			"Explicit offset commits, by group and outcome. Auto-commit is disabled everywhere, so every increment "+
				"here corresponds to a batch this process finished processing. outcome=\"error\" means a restart or "+
				"rebalance will redeliver from the last successful commit.",
			"group", "outcome"),

		// -------------------------------------------------------------- producer
		produceDuration: histogram("produce_duration_seconds",
			"Time from the Produce call to the broker's acknowledgement, including linger, batching and retries. "+
				"This is the batch latency: with linger=0 on the odds path it is dominated by the round trip, and a "+
				"rise with a flat produce rate means the broker, not the client. "+
				"Panel: histogram_quantile(0.99, sum by (le, topic) (rate(sharpline_kafka_produce_duration_seconds_bucket[$__rate_interval]))).",
			produceBuckets, "topic", "outcome"),

		produceBytes: counter("produce_bytes_total",
			"Uncompressed record bytes (key + value + headers) handed to the producer, by topic. Divide by "+
				"produce_records_total for mean record size — the number that decides whether the JSON wire format "+
				"is still affordable.",
			"topic"),

		produceErrors: counter("produce_errors_total",
			"Produce failures by topic and bounded error code. code=\"UNKNOWN_TOPIC_OR_PARTITION\" means Terraform "+
				"has not applied the topic: the broker runs with auto-creation disabled on purpose (CLAUDE.md §9).",
			"topic", "code"),

		produceBatchSize: histogram("produce_batch_records",
			"Records per batch actually written to a partition. Exists to check rather than assume the claim in "+
				"doc.go that linger=0 still batches: a high produce rate should show batches well above 1.",
			batchBuckets, "topic"),

		bufferedRecords: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "producer_buffered_records",
			Help: "Records buffered in the producer and not yet acknowledged, across every producer in this " +
				"process. Rising against a flat produce rate is backpressure: the broker is slower than the " +
				"publisher, and MaxBufferedRecords is what stops that becoming unbounded heap growth.",
		}),

		dataLoss: counter("producer_data_loss_total",
			"Times franz-go detected that produced data was lost — a broker's log truncated beneath the producer's "+
				"sequence, which is what a leader failover with acks<all looks like. This must be zero: acks=all and "+
				"idempotent production are both non-negotiable here precisely so it stays zero.",
			"topic"),

		tombstones: counter("tombstones_produced_total",
			"Tombstones produced, by topic. Every increment permanently deletes a key from a compacted snapshot, "+
				"so this should be a rare, explicable series. A sudden rise on odds.normalized means markets are "+
				"disappearing from every client that resyncs afterwards.",
			"topic"),

		// -------------------------------------------------------------- consumer
		consumeRecords: counter("consume_records_total",
			"Records handed to a handler, by group and topic. Compare with produce_records_total on the same topic: "+
				"a persistent shortfall with zero lag means records are being dropped somewhere between the two.",
			"group", "topic"),

		consumeBytes: counter("consume_bytes_total",
			"Record bytes consumed, by group and topic.",
			"group", "topic"),

		handlerDuration: histogram("handler_duration_seconds",
			"Time a message handler spent on one record. The consume loop is sequential, so this histogram's mean "+
				"multiplied by the record rate is the loop's utilisation — and when that approaches 1, the fix is "+
				"more partitions and more members, not more goroutines here. "+
				"Panel: histogram_quantile(0.99, sum by (le, group, topic) (rate(sharpline_kafka_handler_duration_seconds_bucket[$__rate_interval]))).",
			handlerBuckets, "group", "topic", "outcome"),

		handlerErrors: counter("handler_errors_total",
			"Handler failures by group and topic. Under the default ErrorPolicyStop each one terminates the "+
				"consume loop with the offsets uncommitted, so the record is redelivered — at-least-once means a "+
				"poison record retries for ever rather than being silently skipped, and that is the intended, "+
				"visible failure.",
			"group", "topic"),

		decodeErrors: counter("decode_errors_total",
			"Records that could not be decoded, by bounded reason. reason=\"unsupported_version\" means a producer "+
				"is writing a newer envelope than this build reads, which is the one failure mode the envelope "+
				"version exists to make loud instead of silent.",
			"group", "topic", "reason"),

		commitDuration: histogram("offset_commit_duration_seconds",
			"Time for one synchronous offset commit. It blocks the consume loop, so it is part of the pipeline's "+
				"latency budget and not merely a diagnostic.",
			commitBuckets, "group", "outcome"),

		fetchErrors: counter("fetch_errors_total",
			"Per-partition fetch errors by bounded error code. franz-go retries most of these internally; a "+
				"sustained rate means it is retrying and not succeeding.",
			"group", "topic", "code"),

		assignedGauge: gauge("partitions_assigned",
			"Partitions currently assigned to this member, by group and topic. Summed across members it equals the "+
				"topic's partition count whenever the group is stable — which makes it the honest way to see a "+
				"rebalance settle, and the series the HPA demo watches while Locust scales the consumer pool.",
			"group", "topic"),

		revoked: counter("partitions_revoked_total",
			"Partitions revoked from this member in a cooperative rebalance. Progress is committed synchronously "+
				"in the revoke callback, which franz-go blocks the rebalance on.",
			"group", "topic"),

		lost: counter("partitions_lost_total",
			"Partitions LOST — the session expired or the generation was fenced, so they were reassigned without "+
				"this member being asked. Nothing is committed on this path: the generation is stale and a commit "+
				"would either fail or clobber the progress of the member that now owns them. Non-zero means work "+
				"was redone, and usually means a handler exceeded the rebalance timeout.",
			"group", "topic"),

		lagErrors: counter("lag_refresh_errors_total",
			"Failures to refresh consumer-group lag from the coordinator. While non-zero, the lag gauges are stale "+
				"rather than wrong — they keep their last value, and this counter is how that is detectable.",
			"group"),

		// -------------------------------------------------------------- snapshot
		snapshotDuration: histogram("snapshot_duration_seconds",
			"Time to read a compacted topic from the beginning to the high watermark observed at the start. This is "+
				"a startup-path cost: it is how long a service waits before it has the current-line snapshot.",
			snapshotBuckets, "topic"),

		snapshotRecords: counter("snapshot_records_total",
			"Records read during snapshot reads, by kind. kind=\"tombstone\" entries DELETE a key from the folded "+
				"snapshot; a snapshot read that reports only values on a compacted topic has not been running long "+
				"enough for anything to have been deleted.",
			"topic", "kind"),

		// -------------------------------------------------------------- cluster
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "up",
			Help: "1 if the most recent readiness probe reached the Kafka cluster, 0 if it did not. Distinguishes " +
				"\"the service is down\" (up{component=\"backend\"} == 0) from \"the service is up and the bus is not\".",
		}),

		probeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem, Name: "probe_duration_seconds",
			Help: "Duration of the readiness round trip to a broker (an ApiVersions request, which requires a live " +
				"connection and no topic).",
			Buckets: probeBuckets,
		}),

		connectAttempts: counter("connect_attempts_total",
			"Startup connectivity attempts by outcome: ok, retryable (a transient failure that was retried), fatal "+
				"(a failure that was not retried).",
			"outcome"),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range m.collectors() {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("kafka: register metrics collector: %w", err)
		}
	}
	return m, nil
}

// collectors lists every collector, for registration. Kept as one method so a
// new field cannot be added and silently left unregistered.
func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.consumerLagRecords, m.consumerLagSeconds, m.rebalances,
		m.produceRecords, m.offsetCommits,
		m.produceDuration, m.produceBytes, m.produceErrors, m.produceBatchSize,
		m.bufferedRecords, m.dataLoss, m.tombstones,
		m.consumeRecords, m.consumeBytes, m.handlerDuration, m.handlerErrors,
		m.decodeErrors, m.commitDuration, m.fetchErrors,
		m.assignedGauge, m.revoked, m.lost, m.lagErrors,
		m.snapshotDuration, m.snapshotRecords,
		m.up, m.probeDuration, m.connectAttempts,
	}
}

// -----------------------------------------------------------------------------
// Observation helpers
//
// Every metric mutation in this package goes through one of these, so that a
// label ordering can never be wrong at a call site and the closed label-value
// sets above stay closed.
// -----------------------------------------------------------------------------

func (m *Metrics) observeProduce(topic string, bytes int, d time.Duration, err error) {
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
		m.produceErrors.WithLabelValues(topic, errorCode(err)).Inc()
	}
	m.produceRecords.WithLabelValues(topic, outcome).Inc()
	m.produceDuration.WithLabelValues(topic, outcome).Observe(d.Seconds())
	if bytes > 0 {
		m.produceBytes.WithLabelValues(topic).Add(float64(bytes))
	}
}

func (m *Metrics) observeProduceBatch(topic string, records int) {
	m.produceBatchSize.WithLabelValues(topic).Observe(float64(records))
}

func (m *Metrics) setBufferedRecords(n int64) { m.bufferedRecords.Set(float64(n)) }

func (m *Metrics) observeDataLoss(topic string) { m.dataLoss.WithLabelValues(topic).Inc() }

func (m *Metrics) observeTombstone(topic string) { m.tombstones.WithLabelValues(topic).Inc() }

func (m *Metrics) observeConsumed(group, topic string, bytes int) {
	m.consumeRecords.WithLabelValues(group, topic).Inc()
	if bytes > 0 {
		m.consumeBytes.WithLabelValues(group, topic).Add(float64(bytes))
	}
}

func (m *Metrics) observeHandler(group, topic string, d time.Duration, err error) {
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
		m.handlerErrors.WithLabelValues(group, topic).Inc()
	}
	m.handlerDuration.WithLabelValues(group, topic, outcome).Observe(d.Seconds())
}

func (m *Metrics) observeDecodeError(group, topic string, err error) {
	m.decodeErrors.WithLabelValues(group, topic, decodeFailureReason(err)).Inc()
}

func (m *Metrics) observeCommit(group string, d time.Duration, err error) {
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
	}
	m.offsetCommits.WithLabelValues(group, outcome).Inc()
	m.commitDuration.WithLabelValues(group, outcome).Observe(d.Seconds())
}

func (m *Metrics) observeFetchError(group, topic string, err error) {
	m.fetchErrors.WithLabelValues(group, topic, errorCode(err)).Inc()
}

func (m *Metrics) observeRebalance(group string) { m.rebalances.WithLabelValues(group).Inc() }

func (m *Metrics) observeRevoked(group, topic string, n int) {
	m.revoked.WithLabelValues(group, topic).Add(float64(n))
}

func (m *Metrics) observeLost(group, topic string, n int) {
	m.lost.WithLabelValues(group, topic).Add(float64(n))
}

func (m *Metrics) setAssigned(group, topic string, n int) {
	m.assignedGauge.WithLabelValues(group, topic).Set(float64(n))
}

func (m *Metrics) observeLagRefreshError(group string) { m.lagErrors.WithLabelValues(group).Inc() }

func (m *Metrics) setLagRecords(group, topic, partition string, lag float64) {
	m.consumerLagRecords.WithLabelValues(group, topic, partition).Set(lag)
}

func (m *Metrics) setLagSeconds(group, topic, partition string, seconds float64) {
	m.consumerLagSeconds.WithLabelValues(group, topic, partition).Set(seconds)
}

// forgetPartitionLag removes the lag series for a partition this member no
// longer owns.
//
// Without it a revoked partition's gauge keeps its last value for ever, and the
// dashboard's `sum by (group, topic)` double-counts the partition — once from the
// member that lost it and once from the member that gained it. A stale gauge is
// worse than a missing one: it is indistinguishable from a real measurement.
func (m *Metrics) forgetPartitionLag(group, topic, partition string) {
	labels := prometheus.Labels{"group": group, "topic": topic, "partition": partition}
	m.consumerLagRecords.Delete(labels)
	m.consumerLagSeconds.Delete(labels)
}

func (m *Metrics) observeSnapshot(topic string, d time.Duration, values, tombstones int) {
	m.snapshotDuration.WithLabelValues(topic).Observe(d.Seconds())
	if values > 0 {
		m.snapshotRecords.WithLabelValues(topic, snapshotKindValue).Add(float64(values))
	}
	if tombstones > 0 {
		m.snapshotRecords.WithLabelValues(topic, snapshotKindTombstone).Add(float64(tombstones))
	}
}

func (m *Metrics) observeProbe(d time.Duration, err error) {
	m.probeDuration.Observe(d.Seconds())
	if err != nil {
		m.up.Set(0)
		return
	}
	m.up.Set(1)
}

func (m *Metrics) observeConnectAttempt(outcome string) {
	m.connectAttempts.WithLabelValues(outcome).Inc()
}

// -----------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------

// errorCode maps an error onto a BOUNDED metric label.
//
// Kafka's own error names (UNKNOWN_TOPIC_OR_PARTITION, NOT_LEADER_OR_FOLLOWER,
// …) are a fixed, enumerated set, so they are safe label values and they are the
// names an operator will search for. Everything else collapses onto a small
// closed set of client-side causes. The error TEXT is never a label: a broker or
// a malformed payload can put arbitrary bytes in it.
func errorCode(err error) string {
	if err == nil {
		return ""
	}

	// A Kafka-protocol error decides on its own name, whatever it is wrapped in.
	var kerrErr *kerr.Error
	if errors.As(err, &kerrErr) {
		return kerrErr.Message
	}

	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, kgo.ErrRecordTimeout):
		return "record_timeout"
	case errors.Is(err, kgo.ErrRecordRetries):
		return "record_retries_exhausted"
	case errors.Is(err, kgo.ErrMaxBuffered):
		return "producer_buffer_full"
	case errors.Is(err, kgo.ErrClientClosed), errors.Is(err, ErrClosed):
		return "client_closed"
	case errors.Is(err, kgo.ErrAborting):
		return "aborting"
	case errors.Is(err, ErrUnsupportedEnvelope):
		return "unsupported_envelope"
	case errors.Is(err, ErrMalformedEnvelope):
		return "malformed_envelope"
	default:
		return "unknown"
	}
}
