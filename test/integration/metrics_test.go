package integration

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Metric assertions.
//
// The series in internal/platform/postgres are not decoration: the contract ledger
// records that "metric names are a CONTRACT set by deploy/observability/", and two
// of the assertions in this package are about observability rather than about
// correctness — a rejected ledger commit must land on
// sharpline_db_transactions_total{outcome="commit_failed"} so an alert can fire on
// it, and a retried startup must land on
// sharpline_db_connect_attempts_total{outcome="retryable"} so a slow rollout is
// distinguishable from a broken one.
//
// Reading a counter is deliberately done through Gather, on the registry the pool
// was constructed with, rather than through any accessor on the pool. That is the
// same path Prometheus takes at scrape time, so a series that gathers here is a
// series a dashboard can query.

// counterValue returns the value of a counter or gauge with exactly the given
// labels, and whether the series exists at all. A missing series and a zero series
// are different facts and the caller usually cares which.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !labelsMatch(metric, labels) {
				continue
			}
			switch {
			case metric.GetCounter() != nil:
				return metric.GetCounter().GetValue(), true
			case metric.GetGauge() != nil:
				return metric.GetGauge().GetValue(), true
			default:
				t.Fatalf("%s is neither a counter nor a gauge", name)
			}
		}
		// The family exists but not with these labels.
		return 0, false
	}
	return 0, false
}

func labelsMatch(metric *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, pair := range metric.GetLabel() {
		got[pair.GetName()] = pair.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// assertCounter asserts a counter's exact value. A `want` of 0 accepts an absent
// series, because a counter that has never been incremented is legitimately not
// exported yet — but any other expectation requires the series to exist, and says
// so with the registry's actual contents when it does not.
func assertCounter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string, want float64) {
	t.Helper()

	got, found := counterValue(t, reg, name, labels)
	if !found {
		if want == 0 {
			return
		}
		t.Errorf("%s%s is not exported at all, want %g.\nSeries present:\n%s",
			name, formatLabels(labels), want, describeRegistry(t, reg))
		return
	}
	if got != want {
		t.Errorf("%s%s = %g, want %g", name, formatLabels(labels), got, want)
	}
}

// assertGaugeEventually is for a value that changes asynchronously — sharpline_db_up
// is written by the readiness check, so it only moves when something calls Check.
func assertGauge(t *testing.T, reg *prometheus.Registry, name string, want float64) {
	t.Helper()

	got, found := counterValue(t, reg, name, nil)
	if !found {
		t.Errorf("%s is not exported.\nSeries present:\n%s", name, describeRegistry(t, reg))
		return
	}
	if got != want {
		t.Errorf("%s = %g, want %g", name, got, want)
	}
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// describeRegistry renders every series on a registry, so a failed metric assertion
// says what IS there instead of only what is not.
func describeRegistry(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		return fmt.Sprintf("  (gather failed: %v)", err)
	}

	var lines []string
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			lines = append(lines, "  "+family.GetName()+formatLabels(labels))
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "  (none)"
	}
	return strings.Join(lines, "\n")
}
