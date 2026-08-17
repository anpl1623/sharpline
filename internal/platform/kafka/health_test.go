package kafka

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Readiness, without a cluster.
//
// The half that needs one — a probe that SUCCEEDS, and a probe that keeps
// answering correctly while the process runs — is in
// test/integration/kafka_test.go. What is here is the shape every bus client
// reports and the two things Check does before it reaches the network.

// newTestHealthChecker builds a checker over a client that can reach nothing.
func newTestHealthChecker(t *testing.T) (*healthChecker, *closedFlag, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	cl, err := kgo.NewClient(
		kgo.SeedBrokers("127.0.0.1:1"),
		kgo.WithLogger(&slogAdapter{log: discardLogger(), level: kgo.LogLevelError}),
		kgo.DialTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("build the test client: %v", err)
	}
	t.Cleanup(cl.Close)

	closed := &closedFlag{}
	return &healthChecker{
		cl:      cl,
		m:       m,
		log:     discardLogger(),
		timeout: 250 * time.Millisecond,
		closed:  closed,
	}, closed, reg
}

// TestHealthCheckerNameIsTheSameForEveryClient pins the key this dependency
// appears under in the /readyz payload.
//
// All four bus clients report the same name deliberately: they all answer the
// same question, which is whether this process can reach the cluster. A per-type
// name would put `kafka-producer` and `kafka-consumer` in one service's payload
// and tell an operator nothing extra.
func TestHealthCheckerNameIsTheSameForEveryClient(t *testing.T) {
	t.Parallel()

	h, _, _ := newTestHealthChecker(t)
	if h.Name() != "kafka" {
		t.Errorf("Name() = %q, want %q", h.Name(), "kafka")
	}

	// The compile-time assertion in health.go, restated as a runtime one so a
	// failure names which type stopped satisfying the shape.
	for name, v := range map[string]any{
		"OddsProducer":  (*OddsProducer)(nil),
		"AuditProducer": (*AuditProducer)(nil),
		"Consumer":      (*Consumer)(nil),
		"Snapshotter":   (*Snapshotter)(nil),
	} {
		// A typed nil is enough: the assertion is about the METHOD SET, and
		// calling through it would dereference the nil rather than tell us
		// anything the shared healthChecker above has not already reported.
		if _, ok := v.(checker); !ok {
			t.Errorf("%s does not satisfy httpx.Checker's shape; it would silently drop out of a "+
				"service's readiness list and the service would report itself ready with the bus down", name)
		}
	}
}

// TestHealthCheckerReportsAClosedClientAsClosed covers the guard that runs
// before the probe.
//
// Once Close has run there is nothing to probe and franz-go would answer with
// ErrClientClosed. Reporting ErrClosed directly names the actual state — this
// process is shutting down — which is what an operator reading a /readyz body
// during a rolling update needs to see.
func TestHealthCheckerReportsAClosedClientAsClosed(t *testing.T) {
	t.Parallel()

	h, closed, _ := newTestHealthChecker(t)
	closed.set()

	if err := h.Check(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("Check() on a closed client = %v, want ErrClosed", err)
	}
	if err := h.Ping(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("Ping() on a closed client = %v, want ErrClosed", err)
	}
}

// TestHealthCheckerIsARealRoundTripEveryTime is the property phase 2's handoff
// paid for the hard way: "api and settle declared RequirePostgres without
// opening a pool, so /api/readyz returned 200 with Postgres stopped — a probe
// worse than none."
//
// The equivalent mistake here would be latching a boolean when awaitReady
// succeeded in the constructor. This checker never saw a successful constructor
// at all, so the only way it could return nil is if it were reporting something
// other than the answer to "can I reach a broker right now".
func TestHealthCheckerIsARealRoundTripEveryTime(t *testing.T) {
	t.Parallel()

	h, _, reg := newTestHealthChecker(t)

	err := h.Check(t.Context())
	if err == nil {
		t.Fatal("Check() returned nil against a cluster that does not exist")
	}
	if !strings.Contains(err.Error(), "readiness check") {
		t.Errorf("Check() = %q, want it to name itself as the readiness check", err)
	}

	pingErr := h.Ping(t.Context())
	if pingErr == nil {
		t.Fatal("Ping() returned nil against a cluster that does not exist")
	}
	if !strings.Contains(pingErr.Error(), "ping") {
		t.Errorf("Ping() = %q, want it to name itself as the ping", pingErr)
	}

	// Both round trips are measured, and both moved the up gauge to zero. That
	// gauge is what a dashboard reads when a service is up and the bus is not.
	families, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("gather: %v", gatherErr)
	}
	var sawUp bool
	for _, f := range families {
		switch f.GetName() {
		case "sharpline_kafka_up":
			sawUp = true
			for _, metric := range f.GetMetric() {
				if metric.GetGauge().GetValue() != 0 {
					t.Errorf("sharpline_kafka_up = %v after two failed probes, want 0",
						metric.GetGauge().GetValue())
				}
			}
		case "sharpline_kafka_probe_duration_seconds":
			for _, metric := range f.GetMetric() {
				if metric.GetHistogram().GetSampleCount() < 2 {
					t.Errorf("probe duration observed %d samples, want one per Check/Ping (2)",
						metric.GetHistogram().GetSampleCount())
				}
			}
		}
	}
	if !sawUp {
		t.Error("sharpline_kafka_up was never set; a failed probe must be visible on the dashboard")
	}
}
