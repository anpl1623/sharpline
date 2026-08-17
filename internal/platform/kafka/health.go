// Readiness for internal/platform/httpx, in the same shape as
// internal/platform/postgres/health.go.
//
// # Liveness is not here, for the same reason it is not there
//
// httpx.Server.handleHealthz answers /healthz without consulting any
// dependency. A liveness probe that fails because Kafka is down restarts a
// healthy pod for no reason, and during a broker rolling restart it would
// restart EVERY pod in the deployment at once — turning a recoverable
// dependency blip into a self-inflicted outage at precisely the moment the
// system is least able to absorb one. Liveness means the process is up and its
// scheduler is responsive. This file implements readiness, and only readiness.
//
// # Readiness is a real round trip, every time it is asked
//
// Phase 2's handoff records the bug this exists to avoid, in its own words:
// "RequirePostgres in config means the binary MUST open a pool. api and settle
// declared it without opening one, so /api/readyz returned 200 with Postgres
// stopped — a probe worse than none."
//
// The equivalent mistake here would be latching a boolean when awaitReady
// succeeded in the constructor. A probe that reports a value captured once at
// boot answers "did this process once manage to reach a broker", which is not
// the question a load balancer, a Kubernetes readinessProbe or an operator is
// asking. So Check issues an ApiVersions request to a broker on every call —
// which needs a live connection and no topic, so it proves reachability without
// depending on Terraform having applied anything.
//
// The round trip is measured in sharpline_kafka_probe_duration_seconds and
// sharpline_kafka_up, and it deliberately produces NO trace span: a readiness
// probe fires every few seconds per replica for ever, and a span per probe
// would swamp the traces that CLAUDE.md §9 actually wants — the ones following
// one odds update from ingest to the browser.
//
// Wiring is one line in a cmd/ entrypoint, and *OddsProducer, *AuditProducer,
// *Consumer and *Snapshotter all satisfy httpx.Checker without this package
// importing httpx — no import edge in either direction:
//
//	srv, err := httpx.NewServer(httpx.ServerOptions{
//	    Service:  cfg.Service,
//	    Addr:     cfg.HTTPAddr,
//	    Logger:   log,
//	    Checkers: []httpx.Checker{db, bus},
//	})
package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// healthChecker is the readiness half of every bus client.
//
// It is embedded rather than reimplemented three times so that a producer, a
// consumer and a snapshotter cannot drift on what "ready" means. All three
// report the same checker name — `kafka` — because they all answer the same
// question: this process can reach the cluster.
type healthChecker struct {
	cl      *kgo.Client
	m       *Metrics
	log     *slog.Logger
	timeout time.Duration
	closed  *closedFlag
}

// Name implements httpx.Checker. It is the key this dependency appears under in
// the /readyz JSON payload:
//
//	{"status":"not ready","service":"ingest","checks":{"kafka":{"status":"down","error":"..."}}}
func (h *healthChecker) Name() string { return checkerName }

// Check implements httpx.Checker: it reports whether this process can reach the
// Kafka cluster right now.
//
// # A closed client is not ready, and says so first
//
// Once Close has run there is nothing to probe and franz-go would answer with
// ErrClientClosed. Reporting ErrClosed directly names the actual state — this
// process is shutting down — which is what an operator reading a /readyz body
// during a rolling update needs to see.
//
// # What this does NOT check
//
// Not "the topics exist", not "the consumer group is stable", not "lag is
// acceptable". Readiness answers "send me traffic". A consumer that is behind is
// still doing useful work and removing it from rotation would only make it
// further behind; lag has its own alert (KafkaConsumerLagHigh) and its own
// dashboard panel, which is where a lag problem belongs. Topic existence is
// Terraform's business (CLAUDE.md §9) and a missing topic surfaces at produce
// time as UNKNOWN_TOPIC_OR_PARTITION with the name in it, which is a far better
// error than a readiness probe going red with no detail.
//
// # Timeout
//
// ctx carries httpx's probe deadline (httpx.DefaultReadinessTimeout, 3s).
// ProbeTimeout (2s) is applied on top, so the check returns inside the probe's
// budget and the payload names `kafka` as the failing dependency instead of the
// whole probe timing out with nothing to show. Whichever deadline is nearer
// wins, which is what context composition already guarantees.
func (h *healthChecker) Check(ctx context.Context) error {
	if err := h.closed.err(); err != nil {
		return err
	}

	if err := probe(ctx, h.cl, h.m, h.timeout); err != nil {
		h.log.Warn("kafka readiness check failed",
			slog.String("code", errorCode(err)),
			slog.Bool("transient", IsTransientClusterError(err)),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("kafka: readiness check: %w", err)
	}
	return nil
}

// Ping performs one readiness round trip and returns its result.
//
// Check is what httpx calls; this is the same probe exposed for a caller that
// wants to test connectivity directly — a startup gate in a cmd/ entrypoint, or
// a test. It is not a retry loop: startup retries live in awaitReady, and this
// package deliberately exposes no general-purpose retry helper (see the comment
// on awaitReady for why).
func (h *healthChecker) Ping(ctx context.Context) error {
	if err := h.closed.err(); err != nil {
		return err
	}
	if err := probe(ctx, h.cl, h.m, h.timeout); err != nil {
		return fmt.Errorf("kafka: ping: %w", err)
	}
	return nil
}

// checker is the shape internal/platform/httpx declares for a readiness
// dependency, restated here ONLY as a compile-time assertion.
//
// It is not exported and nothing consumes it: CLAUDE.md §12 puts the interface
// with the consumer, and httpx.Checker is that declaration. This exists so that
// a signature change in httpx breaks the build here instead of silently
// dropping every bus client out of a service's readiness list — services that
// would then report themselves ready while the bus is unreachable.
type checker interface {
	Name() string
	Check(ctx context.Context) error
}

var (
	_ checker = (*OddsProducer)(nil)
	_ checker = (*AuditProducer)(nil)
	_ checker = (*Consumer)(nil)
	_ checker = (*Snapshotter)(nil)
)
