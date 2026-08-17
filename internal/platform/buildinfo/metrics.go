package buildinfo

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric namespace. `sharpline_build_info` joins the `sharpline_db_*` family
// internal/platform/postgres already exports, and follows the same convention
// upstream uses for this exact shape of series: go_build_info,
// prometheus_build_info, node_exporter_build_info.
const (
	metricNamespace = "sharpline"
	metricName      = "build_info"
)

// NewCollector returns the `sharpline_build_info` collector for one service.
//
// # Why the value is a constant 1
//
// This is an INFO METRIC: the numbers are in the labels, and the sample value
// carries no information. That is a deliberate, conventional shape rather than a
// shrug — a version is not a quantity, and encoding it as one (2026081701 as a
// gauge) produces a series that `rate()` and `histogram_quantile()` will happily
// compute nonsense from.
//
// What the shape buys, concretely: `count by (version) (sharpline_build_info)`
// answers "how many replicas are on which build" during a rolling deploy, and
// joining it onto any other series with
// `... * on(service) group_left(version) sharpline_build_info` labels a latency
// graph with the build that produced it. Neither is expressible if the version
// is a value.
//
// # Cardinality
//
// One series per (service, version, commit, build_date, platform) tuple, so the
// series count grows by one per service per build that has ever been scraped —
// and Prometheus retains the old series for the retention window after a deploy.
// Six services and a handful of builds a day is nothing; this would be a real
// cost on a metric emitted per request, which is why the labels are frozen at
// process start and cannot be extended per-scrape.
//
// service is a parameter rather than another linker stamp because it cannot be
// one: each cmd/*/main.go declares `const service = "…"`, and `-X` cannot write
// to a const. The Dockerfile says the same thing and travels the name as the
// io.sharpline.service OCI label instead.
func NewCollector(service string) prometheus.Collector {
	i := Read()

	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      metricName,
		Help: "Identity of the running binary, as label values on a constant 1. " +
			"Panels: count by (version) (sharpline_build_info) during a rolling deploy; " +
			"`* on(service) group_left(version) sharpline_build_info` to annotate any other series with its build. " +
			"stamped=\"false\" means the binary was not produced by deploy/docker/go.Dockerfile, so version and commit are \"unknown\".",
	}, []string{"service", "version", "commit", "build_date", "go_version", "platform", "stamped"})

	g.WithLabelValues(
		service,
		i.Version,
		i.Commit,
		i.BuildDate,
		i.GoVersion,
		i.Platform(),
		fmt.Sprintf("%t", i.Stamped),
	).Set(1)

	return g
}

// Register installs the collector on reg and logs nothing — the caller decides
// how loud a failure is. A nil registry registers nothing and returns nil, which
// is the correct behaviour for `migrate`: it is a run-to-completion Job with no
// listener and therefore no /metrics endpoint to scrape (see cmd/migrate), so its
// build identity travels in its startup log line only.
//
// A duplicate registration is returned as an error rather than swallowed. Two
// collectors for one service on one registry means the process built two, which
// is a programming error that should surface at startup — the same reasoning
// internal/platform/postgres applies to its own collectors.
func Register(reg prometheus.Registerer, service string) error {
	if reg == nil {
		return nil
	}
	if err := reg.Register(NewCollector(service)); err != nil {
		return fmt.Errorf("buildinfo: register %s_%s collector: %w", metricNamespace, metricName, err)
	}
	return nil
}
