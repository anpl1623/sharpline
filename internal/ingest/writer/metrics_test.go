package writer_test

import (
	"math"
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/anpl1623/sharpline/internal/ingest/writer"
)

// -----------------------------------------------------------------------------
// Metric readers
// -----------------------------------------------------------------------------
//
// These read the REGISTRY rather than any field on the Writer, so what they
// assert is what a Prometheus scrape would actually see — the metric name
// included. A test that checked an internal counter would still pass after a
// rename that broke the dashboard.

// counterValue returns the value of a counter series, or 0 when the series does
// not exist yet.
//
// Absence and zero are deliberately conflated for a CounterVec: a label
// combination that has never been observed has no child, and treating that as
// zero is what makes "no duplicates were suppressed" and "the duplicate counter
// is at zero" the same assertion, which is what a caller means.
func counterValue(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, g, name, labels)
	if m == nil {
		return 0
	}
	if m.GetCounter() == nil {
		t.Fatalf("%s is not a counter", name)
	}
	return m.GetCounter().GetValue()
}

// histogramCount returns a histogram's sample count.
func histogramCount(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) uint64 {
	t.Helper()
	m := findMetric(t, g, name, labels)
	if m == nil {
		return 0
	}
	if m.GetHistogram() == nil {
		t.Fatalf("%s is not a histogram", name)
	}
	return m.GetHistogram().GetSampleCount()
}

// histogramSum returns a histogram's sample sum, and whether the series exists.
func histogramSum(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	m := findMetric(t, g, name, labels)
	if m == nil {
		return 0, false
	}
	if m.GetHistogram() == nil {
		t.Fatalf("%s is not a histogram", name)
	}
	return m.GetHistogram().GetSampleSum(), true
}

func findMetric(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) *dto.Metric {
	t.Helper()

	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matchesLabels(m, labels) {
				return m
			}
		}
	}
	return nil
}

func matchesLabels(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// metricNames lists every family the registry holds.
//
// It reads a GATHER, so it only sees series that have at least one sample. That
// is the right lens for [TestWriterDoesNotEmitSeriesItDoesNotOwn], which asks
// what a scrape would show, and the WRONG lens for the name contract — see
// declaredNames.
func metricNames(t *testing.T, g prometheus.Gatherer) []string {
	t.Helper()

	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	return names
}

// declaredNames returns the fully-qualified name of every series NewMetrics
// registers, whether or not it has been observed yet.
//
// Gathering cannot answer this question. prometheus.Registry.Gather emits
// nothing for a CounterVec or a HistogramVec that has no child yet, so a freshly
// registered collector set gathers as three plain histograms and four absent
// families — which is how the earlier spelling of the name-contract test came to
// assert that four of its own seven series did not exist.
//
// Registering into a capturing Registerer and reading the descriptors instead
// makes the assertion independent of whether anything has been measured, which
// is what a NAME contract should be.
func declaredNames(t *testing.T) []string {
	t.Helper()

	r := &describingRegisterer{}
	if _, err := writer.NewMetrics(r); err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	if len(r.names) == 0 {
		t.Fatal("no collectors were registered")
	}
	return r.names
}

// describingRegisterer records what it is asked to register and stores nothing.
type describingRegisterer struct{ names []string }

func (r *describingRegisterer) Register(c prometheus.Collector) error {
	descs := make(chan *prometheus.Desc, 8)
	go func() {
		c.Describe(descs)
		close(descs)
	}()
	for d := range descs {
		r.names = append(r.names, fqName(d))
	}
	return nil
}

func (r *describingRegisterer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		_ = r.Register(c)
	}
}

func (r *describingRegisterer) Unregister(prometheus.Collector) bool { return false }

// descFQName matches prometheus.Desc's stable String() form:
//
//	Desc{fqName: "sharpline_writer_messages_total", help: "...", ...}
var descFQName = regexp.MustCompile(`fqName: "([^"]+)"`)

// fqName extracts a descriptor's metric name. An empty result is returned rather
// than being papered over, so a change in the upstream format fails the name
// contract loudly instead of quietly asserting nothing.
func fqName(d *prometheus.Desc) string {
	m := descFQName.FindStringSubmatch(d.String())
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// The exported metric names are read from outside this package — by
// deploy/observability and by anyone reading the dashboard — so this test exists
// to make a rename break the build rather than a panel.
//
// Renaming any of these is a decision, not a refactor: Prometheus answers a query
// for a series that no longer exists with NO DATA rather than with an error, so
// the breakage is silent.
func TestMetricNamesAreTheOnesDeclared(t *testing.T) {
	t.Parallel()

	want := []string{
		"sharpline_writer_batch_rows",
		"sharpline_writer_bus_lag_seconds",
		"sharpline_writer_catalogue_upserts_total",
		"sharpline_writer_flush_duration_seconds",
		"sharpline_writer_messages_total",
		"sharpline_writer_observation_lag_seconds",
		"sharpline_writer_price_rows_total",
	}

	got := make(map[string]bool, len(want))
	for _, n := range declaredNames(t) {
		if n == "" {
			t.Fatal("a registered collector's descriptor did not yield a metric name; " +
				"prometheus.Desc's String() format changed and descFQName must be updated")
		}
		got[n] = true
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("metric %s is not registered; the dashboard and this package must agree", n)
		}
		delete(got, n)
	}
	for n := range got {
		t.Errorf("unexpected metric %s: every series this package exports is part of its contract "+
			"and must be declared in the test that guards the names", n)
	}
}

// The bus-lag series belong to internal/platform/kafka's Consumer, and the
// staleness/pipeline series belong to phase 0's closed stage set. Emitting
// either here would double-count a working dashboard panel or unilaterally widen
// a contract this package does not own; metrics.go has the argument. This test
// is what stops a future edit from doing it by accident.
func TestWriterDoesNotEmitSeriesItDoesNotOwn(t *testing.T) {
	t.Parallel()

	forbidden := map[string]string{
		"sharpline_kafka_consumer_lag_records": "owned by internal/platform/kafka's Consumer, which filters to the partitions this member owns",
		"sharpline_kafka_consumer_lag_seconds": "owned by internal/platform/kafka's Consumer",
		"sharpline_odds_staleness_seconds":     "phase-0 contract with a closed stage set; the writer is not on the fanout path",
		"sharpline_pipeline_latency_seconds":   "phase-0 contract with a closed stage set",
	}

	for _, n := range declaredNames(t) {
		if why, bad := forbidden[n]; bad {
			t.Errorf("the writer registered %s, which it must not: %s", n, why)
		}
	}
}

// A nil Registerer must still produce a usable Metrics: the observe calls run on
// every record, and making them conditional would put a nil check on the hot
// path of every call site.
func TestNewMetricsWithoutARegistry(t *testing.T) {
	t.Parallel()

	m, err := writer.NewMetrics(nil)
	if err != nil {
		t.Fatalf("NewMetrics(nil): %v", err)
	}
	if m == nil {
		t.Fatal("NewMetrics(nil) returned a nil Metrics; every observe call would panic")
	}
}

// Duplicate registration is a wiring mistake, and it is returned as an error
// rather than raised as a panic — CLAUDE.md §12 forbids a panic outside main, so
// the caller reports it with the rest of its startup context.
func TestNewMetricsRefusesDuplicateRegistration(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	if _, err := writer.NewMetrics(reg); err != nil {
		t.Fatalf("first NewMetrics: %v", err)
	}
	if _, err := writer.NewMetrics(reg); err == nil {
		t.Fatal("a second NewMetrics on the same registry succeeded; two Writers sharing a " +
			"process must share one Metrics, and silently accepting the duplicate would " +
			"double-count every series")
	}
}

// Buckets that the recording rules select on by `le` must exist, exactly.
//
// deploy/observability/rules/sharpline-alerts.yml computes its SLO ratios with
// `..._bucket{le="0.5"}` and `..._bucket{le="120"}`. Those selectors match a
// bucket boundary literally; a histogram without the boundary answers with no
// data rather than with an error. The writer's two lag histograms are not the
// SLO series, but they are deliberately on the same scale so the numbers can be
// read together, and that is only true if the boundaries line up.
func TestLagHistogramsCarryTheSLOBucketBoundaries(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	if _, err := writer.NewMetrics(reg); err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// A histogram with no observations reports its buckets with zero counts, so
	// the boundaries are readable without writing a sample.
	for _, name := range []string{
		"sharpline_writer_observation_lag_seconds",
		"sharpline_writer_bus_lag_seconds",
	} {
		m := findMetric(t, reg, name, map[string]string{})
		if m == nil {
			t.Fatalf("%s is not registered", name)
		}
		bounds := make(map[float64]bool)
		for _, b := range m.GetHistogram().GetBucket() {
			bounds[b.GetUpperBound()] = true
		}
		for _, want := range []float64{0.25, 0.5, 120} {
			if !bounds[want] {
				t.Errorf("%s has no le=%v bucket; the SLO recording rules select on that boundary literally",
					name, want)
			}
		}
	}
}

// Bucket boundaries must be finite and strictly increasing, which Prometheus
// requires and which a hand-maintained slice can break in a way that only shows
// up as a malformed scrape.
func TestBucketBoundariesAreStrictlyIncreasing(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	if _, err := writer.NewMetrics(reg); err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			h := m.GetHistogram()
			if h == nil {
				continue
			}
			prev := math.Inf(-1)
			for _, b := range h.GetBucket() {
				ub := b.GetUpperBound()
				if math.IsInf(ub, 0) || math.IsNaN(ub) {
					t.Errorf("%s has a non-finite explicit bucket boundary %v", f.GetName(), ub)
				}
				if ub <= prev {
					t.Errorf("%s bucket boundaries are not strictly increasing: %v after %v",
						f.GetName(), ub, prev)
				}
				prev = ub
			}
		}
	}
}
