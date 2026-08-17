package kafka

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Metric names are a CONTRACT, and this file is what enforces it.
//
// The contract ledger says it twice, once per phase: "Metric names are a
// CONTRACT set by deploy/observability/. Later phases must emit exactly the names
// the dashboard scrapes — read the dashboard JSON before inventing one."
//
// The failure mode a renamed series produces is the quietest one in the whole
// system: the service still starts, /metrics still serves, Prometheus still
// scrapes, and a Grafana panel just says "No data" — which is exactly what it
// says before the feature is built, so nobody notices. Nothing else in the
// repository connects a Go identifier to a PromQL string, so nothing else can
// catch it.

// exerciseEveryCollector calls every observation helper at least once, so that a
// Gather() returns every family.
//
// It is needed because a *Vec with no children contributes NOTHING to Gather —
// an unexercised CounterVec is indistinguishable from a deleted one. Anything
// missing here is a series that this test cannot see, so a new collector must be
// added to this function as well as to collectors().
func exerciseEveryCollector(m *Metrics) {
	const (
		topic = "odds.normalized"
		group = "pricer"
	)
	someErr := errors.New("boom")

	// ---- producer ----
	m.observeProduce(topic, 512, 3*time.Millisecond, nil)
	m.observeProduce(topic, 512, 3*time.Millisecond, someErr) // populates produceErrors
	m.observeProduceBatch(topic, 8)
	m.setBufferedRecords(3)
	m.observeDataLoss(topic)
	m.observeTombstone(topic)

	// ---- consumer ----
	m.observeConsumed(group, topic, 512)
	m.observeHandler(group, topic, 2*time.Millisecond, nil)
	m.observeHandler(group, topic, 2*time.Millisecond, someErr) // populates handlerErrors
	m.observeDecodeError(group, topic, ErrMalformedEnvelope)
	m.observeCommit(group, time.Millisecond, nil)
	m.observeFetchError(group, topic, someErr)
	m.setAssigned(group, topic, 2)
	m.observeRebalance(group)
	m.observeRevoked(group, topic, 1)
	m.observeLost(group, topic, 1)
	m.observeLagRefreshError(group)
	m.setLagRecords(group, topic, "0", 12)
	m.setLagSeconds(group, topic, "0", 1.5)

	// ---- snapshot ----
	m.observeSnapshot(topic, 250*time.Millisecond, 10, 1)

	// ---- cluster ----
	m.observeProbe(2*time.Millisecond, nil)
	m.observeConnectAttempt(connectOK)
}

// gatherFamilies registers a fresh Metrics, exercises it, and returns the metric
// families by name.
func gatherFamilies(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	exerciseEveryCollector(m)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		byName[f.GetName()] = f
	}
	return byName
}

// TestMetricNamesAndLabelsAreFrozen pins every series this package emits.
//
// The label sets are pinned as well as the names, because a PromQL expression
// aggregating `by (group, topic)` breaks just as completely when a label is
// renamed as when the metric is. The dashboard's own description of panel 14 is
// quoted in doc.go and it is a specification, not prose.
func TestMetricNamesAndLabelsAreFrozen(t *testing.T) {
	t.Parallel()

	// name -> sorted label names. Change a line here only together with the
	// PromQL in deploy/observability that reads it.
	want := map[string][]string{
		// ---- the contract series that predate this package -----------------
		"sharpline_kafka_consumer_lag_records":            {"group", "partition", "topic"},
		"sharpline_kafka_consumer_lag_seconds":            {"group", "partition", "topic"},
		"sharpline_kafka_consumer_group_rebalances_total": {"group"},
		"sharpline_kafka_produce_records_total":           {"outcome", "topic"},
		"sharpline_kafka_offset_commits_total":            {"group", "outcome"},

		// ---- producer ------------------------------------------------------
		"sharpline_kafka_produce_duration_seconds":  {"outcome", "topic"},
		"sharpline_kafka_produce_bytes_total":       {"topic"},
		"sharpline_kafka_produce_errors_total":      {"code", "topic"},
		"sharpline_kafka_produce_batch_records":     {"topic"},
		"sharpline_kafka_producer_buffered_records": {},
		"sharpline_kafka_producer_data_loss_total":  {"topic"},
		"sharpline_kafka_tombstones_produced_total": {"topic"},

		// ---- consumer ------------------------------------------------------
		"sharpline_kafka_consume_records_total":          {"group", "topic"},
		"sharpline_kafka_consume_bytes_total":            {"group", "topic"},
		"sharpline_kafka_handler_duration_seconds":       {"group", "outcome", "topic"},
		"sharpline_kafka_handler_errors_total":           {"group", "topic"},
		"sharpline_kafka_decode_errors_total":            {"group", "reason", "topic"},
		"sharpline_kafka_offset_commit_duration_seconds": {"group", "outcome"},
		"sharpline_kafka_fetch_errors_total":             {"code", "group", "topic"},
		"sharpline_kafka_partitions_assigned":            {"group", "topic"},
		"sharpline_kafka_partitions_revoked_total":       {"group", "topic"},
		"sharpline_kafka_partitions_lost_total":          {"group", "topic"},
		"sharpline_kafka_lag_refresh_errors_total":       {"group"},

		// ---- snapshot ------------------------------------------------------
		"sharpline_kafka_snapshot_duration_seconds": {"topic"},
		"sharpline_kafka_snapshot_records_total":    {"kind", "topic"},

		// ---- cluster -------------------------------------------------------
		"sharpline_kafka_up":                     {},
		"sharpline_kafka_probe_duration_seconds": {},
		"sharpline_kafka_connect_attempts_total": {"outcome"},
	}

	got := gatherFamilies(t)

	for name, wantLabels := range want {
		family, ok := got[name]
		if !ok {
			t.Errorf("series %q is not emitted; either it was renamed or exerciseEveryCollector no longer touches it", name)
			continue
		}
		if len(family.GetMetric()) == 0 {
			t.Errorf("series %q has no samples", name)
			continue
		}
		gotLabels := make([]string, 0, 4)
		for _, pair := range family.GetMetric()[0].GetLabel() {
			gotLabels = append(gotLabels, pair.GetName())
		}
		sort.Strings(gotLabels)
		if strings.Join(gotLabels, ",") != strings.Join(wantLabels, ",") {
			t.Errorf("series %q has labels %v, want %v", name, gotLabels, wantLabels)
		}
	}

	for name := range got {
		if _, expected := want[name]; !expected {
			t.Errorf("series %q is emitted but not declared here; a new metric must be pinned, "+
				"and its PromQL documented at its definition in metrics.go", name)
		}
	}

	if len(got) != len(want) {
		t.Errorf("gathered %d series, pinned %d", len(got), len(want))
	}
}

// TestEveryMetricIsPrefixedAndCarriesHelp enforces the two rules
// deploy/observability/prometheus.yml states about application series.
func TestEveryMetricIsPrefixedAndCarriesHelp(t *testing.T) {
	t.Parallel()

	for name, family := range gatherFamilies(t) {
		if !strings.HasPrefix(name, "sharpline_kafka_") {
			t.Errorf("series %q is not prefixed sharpline_kafka_", name)
		}
		if strings.TrimSpace(family.GetHelp()) == "" {
			t.Errorf("series %q has no HELP text", name)
		}

		// prometheus.yml attaches `service` as a TARGET label on every scrape
		// job. A metric label of the same name is renamed `exported_service` on
		// ingest, and the two then drift — one saying which binary was scraped,
		// the other saying whatever the code passed.
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == "service" || pair.GetName() == "instance" || pair.GetName() == "job" {
					t.Errorf("series %q carries a %q label, which collides with a target label",
						name, pair.GetName())
				}
			}
		}
	}
}

// observabilityFiles are the two files that READ these series. Their paths are
// relative to this package.
var observabilityFiles = []string{
	"../../../deploy/observability/rules/sharpline-alerts.yml",
	"../../../deploy/observability/grafana/dashboards/sharpline-overview.json",
}

// TestObservabilityConfigReferencesOnlyEmittedSeries closes the loop in the
// direction that actually breaks.
//
// A dashboard panel or an alert rule naming a series this package does not emit
// produces "No data" and a rule that never fires — silently, and
// indistinguishably from a healthy system. Reading the real files means the test
// fails when either side moves, which is the only moment the mismatch is cheap to
// fix.
func TestObservabilityConfigReferencesOnlyEmittedSeries(t *testing.T) {
	t.Parallel()

	emitted := gatherFamilies(t)
	// A histogram is scraped as _bucket / _sum / _count; PromQL names those
	// directly, so they are resolved back to their family.
	suffixes := []string{"_bucket", "_sum", "_count"}

	referenced := map[string][]string{}
	for _, path := range observabilityFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, match := range regexp.MustCompile(`sharpline_kafka_[a-z0-9_]+`).FindAllString(string(raw), -1) {
			referenced[match] = append(referenced[match], path)
		}
	}

	if len(referenced) == 0 {
		t.Fatal("no sharpline_kafka_ series are referenced by deploy/observability; either the dashboard " +
			"lost its Kafka panels or these paths are wrong")
	}

	for name, files := range referenced {
		family := name
		for _, suffix := range suffixes {
			if base := strings.TrimSuffix(family, suffix); base != family {
				if _, ok := emitted[base]; ok {
					family = base
					break
				}
			}
		}
		if _, ok := emitted[family]; !ok {
			t.Errorf("deploy/observability references %q (in %v) but this package emits no such series; "+
				"the panel or alert will silently report no data", name, files)
		}
	}
	t.Logf("deploy/observability references %d kafka series", len(referenced))
}

// TestNewMetricsRegistrationSemantics covers the two registry cases the package
// depends on.
func TestNewMetricsRegistrationSemantics(t *testing.T) {
	t.Parallel()

	t.Run("a nil registry builds live but unregistered collectors", func(t *testing.T) {
		t.Parallel()
		// This is the unit-test and one-shot-job path. The observe calls must
		// stay live so that no call site in the package needs a nil check.
		m, err := NewMetrics(nil)
		if err != nil {
			t.Fatalf("NewMetrics(nil) = %v", err)
		}
		if m == nil {
			t.Fatal("NewMetrics(nil) returned nil")
		}
		exerciseEveryCollector(m)
	})

	t.Run("two Metrics on one registry is an error, not a silent merge", func(t *testing.T) {
		t.Parallel()
		// A process with two halves reporting under one series produces
		// plausible nonsense. It must fail at startup instead.
		reg := prometheus.NewRegistry()
		if _, err := NewMetrics(reg); err != nil {
			t.Fatalf("first NewMetrics: %v", err)
		}
		if _, err := NewMetrics(reg); err == nil {
			t.Error("a second NewMetrics on the same registry succeeded")
		}
	})

	t.Run("one Metrics serves a producer and a consumer in the same process", func(t *testing.T) {
		t.Parallel()
		// The reason registration is in NewMetrics rather than in each
		// constructor: pricer legitimately consumes odds.normalized AND produces
		// price.computed, and duplicate registration would fail its startup.
		reg := prometheus.NewRegistry()
		m, err := NewMetrics(reg)
		if err != nil {
			t.Fatalf("NewMetrics: %v", err)
		}
		m.observeProduce("price.computed", 128, time.Millisecond, nil)
		m.observeConsumed("pricer", "odds.normalized", 128)

		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		if len(families) == 0 {
			t.Fatal("gathered nothing after both a produce and a consume")
		}
	})
}

// TestForgetPartitionLagRemovesBothSeries checks the cleanup that keeps the
// dashboard's `sum by (group, topic)` honest.
//
// Without it a revoked partition's gauge keeps its last value for ever and the
// panel double-counts the partition — once from the member that lost it and once
// from the member that gained it. A stale gauge is worse than a missing one: it
// is indistinguishable from a real measurement.
func TestForgetPartitionLagRemovesBothSeries(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	m.setLagRecords("pricer", "odds.normalized", "0", 12)
	m.setLagSeconds("pricer", "odds.normalized", "0", 1.5)
	m.setLagRecords("pricer", "odds.normalized", "1", 7)
	m.setLagSeconds("pricer", "odds.normalized", "1", 0.5)

	count := func(name string) int {
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, f := range families {
			if f.GetName() == name {
				return len(f.GetMetric())
			}
		}
		return 0
	}

	if got := count("sharpline_kafka_consumer_lag_records"); got != 2 {
		t.Fatalf("lag_records has %d series, want 2", got)
	}

	m.forgetPartitionLag("pricer", "odds.normalized", "0")

	if got := count("sharpline_kafka_consumer_lag_records"); got != 1 {
		t.Errorf("lag_records has %d series after forgetting partition 0, want 1", got)
	}
	if got := count("sharpline_kafka_consumer_lag_seconds"); got != 1 {
		t.Errorf("lag_seconds has %d series after forgetting partition 0, want 1; both gauges must be "+
			"deleted together or the two panels disagree about which partitions exist", got)
	}
}

// TestErrorCodeIsBounded checks the metric-label classification.
//
// The rule: Kafka's own error names are a fixed enumerated set and are safe label
// values, everything else collapses onto a small closed set, and the error TEXT
// is never a label — a broker or a malformed payload can put arbitrary bytes in
// it, and an unbounded label value with an untrusted author is a cardinality bomb.
func TestErrorCodeIsBounded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, ""},
		{
			"a Kafka protocol error keeps its own name",
			&kerr.Error{Code: 3, Message: "UNKNOWN_TOPIC_OR_PARTITION", Retriable: true},
			"UNKNOWN_TOPIC_OR_PARTITION",
		},
		{
			"a wrapped Kafka protocol error still keeps it",
			errors.Join(errors.New("produce"), &kerr.Error{Code: 5, Message: "LEADER_NOT_AVAILABLE"}),
			"LEADER_NOT_AVAILABLE",
		},
		{"cancelled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"record timeout", kgo.ErrRecordTimeout, "record_timeout"},
		{"retries exhausted", kgo.ErrRecordRetries, "record_retries_exhausted"},
		{"producer buffer full", kgo.ErrMaxBuffered, "producer_buffer_full"},
		{"franz-go client closed", kgo.ErrClientClosed, "client_closed"},
		{"this package's closed sentinel", ErrClosed, "client_closed"},
		{"aborting", kgo.ErrAborting, "aborting"},
		{"unsupported envelope", ErrUnsupportedEnvelope, "unsupported_envelope"},
		{"malformed envelope", ErrMalformedEnvelope, "malformed_envelope"},
		{"anything else", errors.New("some broker said something"), "unknown"},
		{
			"an error whose text is untrusted and unbounded",
			errors.New(strings.Repeat("payload bytes ", 512)),
			"unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := errorCode(tc.err)
			if got != tc.want {
				t.Errorf("errorCode() = %q, want %q", got, tc.want)
			}
			if len(got) > 64 {
				t.Errorf("errorCode() returned a %d-byte label value; label values must be bounded", len(got))
			}
			if strings.ContainsAny(got, " \n\t\"") {
				t.Errorf("errorCode() = %q, which contains whitespace or a quote; that is error text, not a code", got)
			}
		})
	}
}

// TestOutcomeLabelValuesAreClosed pins the small enumerated label values, which
// alert rules match on literally (`outcome="error"`).
func TestOutcomeLabelValuesAreClosed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ got, want string }{
		{outcomeOK, "ok"},
		{outcomeError, "error"},
		{connectOK, "ok"},
		{connectRetryable, "retryable"},
		{connectFatal, "fatal"},
		{snapshotKindValue, "value"},
		{snapshotKindTombstone, "tombstone"},
	} {
		if tc.got != tc.want {
			t.Errorf("label value = %q, want %q; alert rules match these literally", tc.got, tc.want)
		}
	}
}

// TestProbeSetsTheUpGauge checks the one gauge that distinguishes "the service is
// down" from "the service is up and the bus is not".
func TestProbeSetsTheUpGauge(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	up := func() float64 {
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, f := range families {
			if f.GetName() == "sharpline_kafka_up" {
				return f.GetMetric()[0].GetGauge().GetValue()
			}
		}
		t.Fatal("sharpline_kafka_up is not emitted")
		return -1
	}

	m.observeProbe(time.Millisecond, nil)
	if got := up(); got != 1 {
		t.Errorf("up = %v after a successful probe, want 1", got)
	}

	m.observeProbe(time.Millisecond, errors.New("unreachable"))
	if got := up(); got != 0 {
		t.Errorf("up = %v after a failed probe, want 0", got)
	}

	m.observeProbe(time.Millisecond, nil)
	if got := up(); got != 1 {
		t.Errorf("up = %v after recovery, want 1; the gauge must be able to come back", got)
	}
}

// TestHistogramBucketsCoverTheirBudgets checks that each histogram's top bucket
// is at least as large as the timeout it measures against.
//
// A histogram whose largest finite bucket is below the operation's own deadline
// reports every slow case in +Inf, so a p99 computed from it is a lower bound
// wearing a percentile's name.
func TestHistogramBucketsCoverTheirBudgets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		buckets []float64
		atLeast float64
		why     string
	}{
		{"produce", produceBuckets, 30, "the odds producer's RecordDeliveryTimeout is 30s"},
		{"handler", handlerBuckets, 1, "a handler slower than a second is already pathological but must still be measurable"},
		{"commit", commitBuckets, 1, "an offset commit blocks the consume loop"},
		{"probe", probeBuckets, float64(DefaultProbeTimeout) / float64(time.Second), "measuring past the probe timeout is measuring a timeout"},
		{"snapshot", snapshotBuckets, 30, "a snapshot read is a startup-path cost and can be slow"},
		{"batch", batchBuckets, 128, "batch sizes are read by order of magnitude"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if len(tc.buckets) == 0 {
				t.Fatal("no buckets")
			}
			for i := 1; i < len(tc.buckets); i++ {
				if tc.buckets[i] <= tc.buckets[i-1] {
					t.Fatalf("buckets are not strictly increasing at index %d: %v", i, tc.buckets)
				}
			}
			if top := tc.buckets[len(tc.buckets)-1]; top < tc.atLeast {
				t.Errorf("top bucket is %v, want at least %v (%s)", top, tc.atLeast, tc.why)
			}
		})
	}

	// handlerBuckets shares its 0.25 boundary with sharpline_pricing_duration_
	// seconds' alert threshold (PricingLatencyHigh fires above 250ms), so the two
	// graphs agree on where "slow" starts.
	var has250ms bool
	for _, b := range handlerBuckets {
		if b == 0.25 {
			has250ms = true
		}
	}
	if !has250ms {
		t.Error("handlerBuckets has no 0.25 boundary; it must line up with PricingLatencyHigh's 250ms threshold")
	}
}
