package wsgw

import (
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// gather returns the registry's metric families indexed by name.
func gather(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		if _, dup := out[f.GetName()]; dup {
			t.Fatalf("%s appears in more than one metric family; two collectors registered "+
				"under one name rather than sharing one", f.GetName())
		}
		out[f.GetName()] = f
	}
	return out
}

// labelSet flattens one metric's labels.
func labelSet(m *dto.Metric) map[string]string {
	out := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

// hasLabelValue reports whether a family carries a series with the given
// label/value pair.
func hasLabelValue(f *dto.MetricFamily, name, value string) bool {
	if f == nil {
		return false
	}
	for _, m := range f.GetMetric() {
		if labelSet(m)[name] == value {
			return true
		}
	}
	return false
}

// TestEveryDocumentedSeriesExistsAfterAnObservation.
//
// The names and label sets below are read by
// deploy/observability/grafana/dashboards/sharpline-overview.json and
// deploy/observability/rules/sharpline-alerts.yml. Prometheus answers a query
// for a series that does not exist with NO DATA rather than with an error, so a
// missing or misspelled series reads as "the SLI is absent" rather than as
// "the SLI is broken" — which is why this is asserted mechanically instead of
// being left to a dashboard nobody opens until an incident.
func TestEveryDocumentedSeriesExistsAfterAnObservation(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	m.observeConnectionOpened()
	m.observeConnection(ConnectionAccepted)
	m.observeConnection(ConnectionRejected)
	m.observeConnection(ConnectionUpgradeFailed)
	m.observeDrop(DropSlowConsumer)
	m.observeResync(ResyncSlowConsumer)
	m.observeSent(KindDelta)
	m.observeSentN(KindSnapshot, 3)
	m.observeWriteDelay(2 * time.Millisecond)
	m.observeQueueDepth(4)
	m.observeSubscriptions(11)
	m.observeMarketsTracked(7)
	m.observeBusRecord(recordStored)
	m.observeChannelReject(RejectMalformed)
	m.observePresenceError(PresenceOpHeartbeat)
	m.observeFanout(sampleComputed(t), sampleObservedAt.Add(3*time.Second))

	families := gather(t, reg)

	for _, name := range []string{
		"sharpline_ws_connections_active",
		"sharpline_ws_connections_total",
		"sharpline_ws_clients_dropped_total",
		"sharpline_ws_resyncs_total",
		"sharpline_ws_messages_sent_total",
		"sharpline_ws_write_delay_seconds",
		"sharpline_ws_send_queue_depth",
		"sharpline_ws_subscriptions_active",
		"sharpline_ws_markets_tracked",
		"sharpline_ws_bus_records_total",
		"sharpline_ws_channel_rejects_total",
		"sharpline_ws_presence_errors_total",
		"sharpline_odds_staleness_seconds",
		"sharpline_pipeline_latency_seconds",
	} {
		if families[name] == nil {
			t.Errorf("%s is absent after an observation", name)
		}
	}

	// The exact label VALUES the alert rules select on.
	if !hasLabelValue(families["sharpline_ws_clients_dropped_total"], "reason", "slow_consumer") {
		t.Error(`sharpline_ws_clients_dropped_total has no reason="slow_consumer"; ` +
			"WebSocketClientsDropping names it in its own description")
	}
	if !hasLabelValue(families["sharpline_ws_messages_sent_total"], "kind", "delta") {
		t.Error(`sharpline_ws_messages_sent_total has no kind="delta"; ` +
			"it is the denominator of WebSocketResyncStorm")
	}
	if !hasLabelValue(families["sharpline_ws_connections_total"], "result", "accepted") {
		t.Error(`sharpline_ws_connections_total has no result="accepted"`)
	}
	if !hasLabelValue(families["sharpline_odds_staleness_seconds"], "stage", provider.StageFanout) {
		t.Error(`sharpline_odds_staleness_seconds has no stage="fanout"; that stage IS the ` +
			"headline SLO and the recording rules read only it")
	}
	if !hasLabelValue(families["sharpline_pipeline_latency_seconds"], "stage", provider.StageFanout) {
		t.Error(`sharpline_pipeline_latency_seconds has no stage="fanout"; SLO 2 reads it`)
	}

	// The gauges carry the values that were set, not merely a series.
	if got := testutil.ToFloat64(m.marketsTracked); got != 7 {
		t.Errorf("sharpline_ws_markets_tracked = %v, want 7", got)
	}
	if got := testutil.ToFloat64(m.subscriptions); got != 11 {
		t.Errorf("sharpline_ws_subscriptions_active = %v, want 11", got)
	}
	if got := testutil.ToFloat64(m.connectionsActive); got != 1 {
		t.Errorf("sharpline_ws_connections_active = %v, want 1", got)
	}
	m.observeConnectionClosed()
	if got := testutil.ToFloat64(m.connectionsActive); got != 0 {
		t.Errorf("sharpline_ws_connections_active = %v after close, want 0 — a leaked increment "+
			"does not merely mis-report, it scales the deployment up and keeps it there", got)
	}
}

// TestAlertBucketLiteralsArePresent.
//
// sharpline-alerts.yml selects single buckets by an EXACT `le` literal, and says
// what happens otherwise: "if the emitted histogram has no boundary at that
// value the selector matches NOTHING, the rule silently evaluates to empty, and
// the SLI reads as absent rather than as broken."
//
// The literals are checked in the form Prometheus renders them —
// strconv.FormatFloat(bound, 'g', -1, 64) — rather than as float comparisons,
// because the rule matches the STRING.
func TestAlertBucketLiteralsArePresent(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	m.observeFanout(sampleComputed(t), sampleObservedAt.Add(time.Second))

	families := gather(t, reg)

	cases := []struct {
		series string
		want   string
		why    string
	}{
		{
			series: "sharpline_odds_staleness_seconds",
			want:   "120",
			why:    "SLO 1's compliance bucket",
		},
		{
			series: "sharpline_pipeline_latency_seconds",
			want:   "0.5",
			why:    "SLO 2's compliance bucket",
		},
	}

	for _, tc := range cases {
		t.Run(tc.series+" le="+tc.want, func(t *testing.T) {
			f := families[tc.series]
			if f == nil {
				t.Fatalf("%s is absent", tc.series)
			}
			seen := map[string]bool{}
			for _, metric := range f.GetMetric() {
				for _, b := range metric.GetHistogram().GetBucket() {
					seen[strconv.FormatFloat(b.GetUpperBound(), 'g', -1, 64)] = true
				}
			}
			if !seen[tc.want] {
				t.Fatalf("%s has no bucket boundary rendering as le=%q (%s); the alert rule "+
					"selecting it would match nothing and read as absent rather than broken",
					tc.series, tc.want, tc.why)
			}
		})
	}
}

// TestSharedContractSeriesRegisterAlongsideTheOtherStages is the mechanical half
// of the argument in metrics.go, and it is modelled on the normalizer's test of
// the same name.
//
// internal/ingest/provider declares the staleness and clock-skew series for
// stage="received", internal/ingest/normalizer declares the pipeline series and
// emits stage="normalized", and this package emits stage="fanout" onto all
// three. A process that ran more than one of them would build them on ONE
// registry, so a disagreement about help text, label names or bucket boundaries
// is a startup failure for the whole service. This is what turns that into a red
// build instead.
//
// BOTH ORDERS are asserted. internal/ingest learned this the hard way: one
// package registered directly and treated AlreadyRegisteredError as a failure,
// so reversing two lines in cmd/ingest killed the process at startup and nothing
// in the type system said so.
func TestSharedContractSeriesRegisterAlongsideTheOtherStages(t *testing.T) {
	// Each entry is a construction order. The wsgw set is returned so the test
	// can put a sample on the shared series afterwards: a HistogramVec with no
	// children gathers nothing at all, so "the series is present" is only
	// answerable once something has been observed onto it.
	orders := []struct {
		name  string
		build func(t *testing.T, reg prometheus.Registerer) *Metrics
	}{
		{
			name: "provider, normalizer, wsgw",
			build: func(t *testing.T, reg prometheus.Registerer) *Metrics {
				t.Helper()
				mustBuild(t, "provider", func() error { _, err := provider.NewMetrics(reg); return err })
				mustBuild(t, "normalizer", func() error { _, err := normalizer.NewMetrics(reg); return err })
				m, err := NewMetrics(reg)
				if err != nil {
					t.Fatalf("wsgw metrics last: %v", err)
				}
				return m
			},
		},
		{
			name: "wsgw, normalizer, provider",
			build: func(t *testing.T, reg prometheus.Registerer) *Metrics {
				t.Helper()
				m, err := NewMetrics(reg)
				if err != nil {
					t.Fatalf("wsgw metrics first: %v", err)
				}
				mustBuild(t, "normalizer", func() error { _, err := normalizer.NewMetrics(reg); return err })
				mustBuild(t, "provider", func() error { _, err := provider.NewMetrics(reg); return err })
				return m
			},
		},
		{
			name: "wsgw then provider",
			build: func(t *testing.T, reg prometheus.Registerer) *Metrics {
				t.Helper()
				m, err := NewMetrics(reg)
				if err != nil {
					t.Fatalf("wsgw metrics first: %v", err)
				}
				mustBuild(t, "provider", func() error { _, err := provider.NewMetrics(reg); return err })
				return m
			},
		},
	}

	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m := order.build(t, reg)
			m.observeFanout(sampleComputed(t), sampleObservedAt.Add(-time.Minute))

			// gather fails the test if any name appears in more than one family,
			// which is the "two collectors that happen to share a name" failure.
			families := gather(t, reg)
			for _, name := range []string{
				"sharpline_odds_staleness_seconds",
				"sharpline_pipeline_latency_seconds",
				"sharpline_odds_clock_skew_total",
			} {
				if families[name] == nil {
					t.Errorf("%s is absent after every set registered", name)
				}
			}
		})
	}
}

// mustBuild fails the test with the name of the collector set that could not be
// registered — which is the diagnostic that matters, because the failure means
// two packages disagree about a series they share.
func mustBuild(t *testing.T, name string, build func() error) {
	t.Helper()
	if err := build(); err != nil {
		t.Fatalf("%s collector set: %v — the packages disagree about a series they share, or "+
			"construction order is load-bearing again", name, err)
	}
}

// TestBothStagesLandOnOneStalenessSeries. Adopting the collector is only useful
// if the samples end up together: two collectors sharing a name would gather as
// two families and every dashboard query would see half the data.
func TestBothStagesLandOnOneStalenessSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := provider.NewMetrics(reg); err != nil {
		t.Fatalf("provider metrics: %v", err)
	}
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("wsgw metrics alongside the provider set: %v", err)
	}
	m.observeFanout(sampleComputed(t), sampleObservedAt.Add(time.Second))

	if got := testutil.CollectAndCount(m.staleness, "sharpline_odds_staleness_seconds"); got == 0 {
		t.Fatal("the shared staleness histogram recorded nothing")
	}
	gather(t, reg) // asserts exactly one family per name
}

// TestFanoutIsObservedOncePerPriceAndOncePerRecord pins the SLO's unit.
//
// Observing staleness once per RECORD with the newest instant would report the
// freshest book's age for every book on the market — the number that flatters
// the pipeline most. Observing pipeline latency once per PRICE would weight the
// histogram by book count, which is not what ingested_at is a property of.
// internal/ingest/normalizer makes exactly this split for exactly these reasons.
func TestFanoutIsObservedOncePerPriceAndOncePerRecord(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	c := sampleComputed(t)
	quotes := 0
	for _, b := range c.Books {
		quotes += len(b.Quotes)
	}
	if quotes < 2 {
		t.Fatalf("the fixture carries %d quotes; the test needs at least two to distinguish "+
			"per-price from per-record", quotes)
	}

	m.observeFanout(c, sampleObservedAt.Add(5*time.Second))

	families := gather(t, reg)
	if got := sampleCount(families["sharpline_odds_staleness_seconds"]); got != uint64(quotes) {
		t.Errorf("staleness samples = %d, want one per quote (%d)", got, quotes)
	}
	if got := sampleCount(families["sharpline_pipeline_latency_seconds"]); got != 1 {
		t.Errorf("pipeline latency samples = %d, want one per record", got)
	}
}

// TestFanoutClampsAndCountsClockSkew.
//
// domain.Price.Age returns a negative duration deliberately so "a monitor can
// detect the skew instead of silently reporting healthy staleness". A histogram
// destroys that: a negative sample lands in the lowest bucket and reads as
// EXCELLENT. So the contract is clamp AND count, and
// ProviderClockSkewDetected alerts on the counter — which only works if the
// counter moves.
func TestFanoutClampsAndCountsClockSkew(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	c := sampleComputed(t)
	// Observed one minute in the FUTURE relative to the fanout instant.
	m.observeFanout(c, c.Books[0].Quotes[0].ObservedAt.Add(-time.Minute))

	families := gather(t, reg)
	skew := families["sharpline_odds_clock_skew_total"]
	if skew == nil {
		t.Fatal("sharpline_odds_clock_skew_total is absent; the clamp was silent")
	}
	if !hasLabelValue(skew, "stage", provider.StageFanout) {
		t.Error(`sharpline_odds_clock_skew_total has no stage="fanout"`)
	}
	if !hasLabelValue(skew, "provider", sampleProvider) {
		t.Errorf("sharpline_odds_clock_skew_total has no provider=%q", sampleProvider)
	}

	// The clamp itself: nothing may be observed below zero.
	f := families["sharpline_odds_staleness_seconds"]
	if f == nil {
		t.Fatal("sharpline_odds_staleness_seconds is absent")
	}
	for _, metric := range f.GetMetric() {
		if sum := metric.GetHistogram().GetSampleSum(); sum < 0 {
			t.Errorf("staleness sum is %v; a negative observation reached the histogram and "+
				"would read as excellent freshness", sum)
		}
	}
}

// TestFanoutIgnoresARecordWithNoIngestedAt. A record written before the field
// existed still decodes, and observing a 55-year pipeline latency for it would
// destroy the histogram it landed in.
func TestFanoutIgnoresARecordWithNoIngestedAt(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	c := sampleComputed(t)
	c.IngestedAt = time.Time{}
	m.observeFanout(c, sampleObservedAt.Add(time.Second))

	families := gather(t, reg)
	if got := sampleCount(families["sharpline_pipeline_latency_seconds"]); got != 0 {
		t.Errorf("pipeline latency recorded %d samples for a record with no ingested_at", got)
	}
}

// TestNilMetricsIsSafe. Options.Metrics is optional so a unit test and a process
// with no /metrics endpoint need no registry, and no call site needs a nil
// check. The only way that is true is if every method tolerates a nil receiver.
func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.observeConnection(ConnectionAccepted)
	m.observeConnectionOpened()
	m.observeConnectionClosed()
	m.observeDrop(DropShutdown)
	m.observeResync(ResyncClientRequested)
	m.observeSent(KindHello)
	m.observeSentN(KindDelta, 5)
	m.observeWriteDelay(time.Millisecond)
	m.observeQueueDepth(1)
	m.observeSubscriptions(1)
	m.observeMarketsTracked(1)
	m.observeBusRecord(recordStored)
	m.observeChannelReject(RejectDuplicate)
	m.observePresenceError(PresenceOpForget)
	m.observeFanout(sampleComputed(t), time.Now())
}

// TestNilRegistererBuildsUnregisteredCollectors. Same contract as the
// normalizer's: the observe calls stay live and cost a few nanoseconds.
func TestNilRegistererBuildsUnregisteredCollectors(t *testing.T) {
	m, err := NewMetrics(nil)
	if err != nil {
		t.Fatalf("NewMetrics(nil): %v", err)
	}
	m.observeSent(KindDelta)
	if got := testutil.CollectAndCount(m.messagesSent, "sharpline_ws_messages_sent_total"); got != 1 {
		t.Errorf("an unregistered collector recorded %d series, want 1", got)
	}
}

// TestDuplicateRegistrationOfTheOwnedSeriesFails. One process may legitimately
// build more than one hub; duplicate registration should fail its startup rather
// than its code review.
func TestDuplicateRegistrationOfTheOwnedSeriesFails(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := NewMetrics(reg); err != nil {
		t.Fatalf("first NewMetrics: %v", err)
	}
	if _, err := NewMetrics(reg); err == nil {
		t.Fatal("a second collector set registered on the same registry without complaint")
	}
}

// sampleCount totals a histogram family's sample count across every label set.
func sampleCount(f *dto.MetricFamily) uint64 {
	if f == nil {
		return 0
	}
	var total uint64
	for _, m := range f.GetMetric() {
		total += m.GetHistogram().GetSampleCount()
	}
	return total
}

// assert the fixture keeps satisfying the document's own validator, so a change
// to pricing.ComputedMarket.Validate cannot leave these tests exercising a
// record the real pipeline would refuse.
func TestFixtureIsAValidComputedMarket(t *testing.T) {
	if err := sampleComputed(t).Validate(); err != nil {
		t.Fatalf("the fixture no longer validates: %v", err)
	}
}
