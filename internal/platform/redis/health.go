// Readiness for internal/platform/httpx, in the same shape as
// internal/platform/postgres/health.go and internal/platform/kafka/health.go.
//
// # Liveness is not here, for the same reason it is not there
//
// httpx.Server.handleHealthz answers /healthz without consulting any
// dependency. A liveness probe that fails because Redis is down restarts a
// healthy pod for no reason, and during a Redis rolling restart it would
// restart EVERY api and stream pod at once — turning a recoverable dependency
// blip into a self-inflicted outage at exactly the moment the system is least
// able to absorb one. Liveness means the process is up and its scheduler is
// responsive. This file implements readiness, and only readiness.
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
// boot answers "did this process once manage to reach Redis", which is not the
// question a load balancer, a Kubernetes readinessProbe or an operator is
// asking. So Check issues a real PING on every call.
//
// # WHICH SERVICES SHOULD REGISTER THIS CHECKER — read before wiring it
//
// This is a per-service decision and getting it wrong is the difference between
// a degradation and an outage. The checker is honest; what it should gate is
// not the same answer for every binary.
//
//	stream  — YES. CLAUDE.md §9: "stream must be horizontally scalable, so no
//	          session affinity, which means subscription state lives in Redis
//	          rather than in a pod." A stream replica that cannot reach Redis
//	          cannot route a subscription, so it genuinely cannot serve traffic
//	          and should be pulled from the Service's endpoints.
//
//	api     — NO, deliberately. `api` uses Redis for rate limiting and
//	          idempotency, and internal/httpapi/middleware fails OPEN when the
//	          limiter errors (see its own documentation for why). An api replica
//	          with Redis down still serves every request correctly, just
//	          unthrottled. Registering this checker would take a fully
//	          functional API out of rotation — and, because every replica shares
//	          the one Redis, it would take ALL of them out at once, converting a
//	          degradation into a total outage.
//
//	          The outage is not thereby invisible: sharpline_redis_up goes to 0
//	          and sharpline_http_rate_limit_fail_open_total starts climbing.
//	          Alert on those. That is the correct place for "Redis is down and
//	          we are unprotected" to show up — a page, not a 503 for every user.
//
//	ingest  — NO. It declares RequireRedis for idempotency keys only; the same
//	          fail-open reasoning applies.
//
// If a future consumer of Redis genuinely cannot function without it, that
// consumer registers the checker. Nothing else should.
package redis

import (
	"context"
	"fmt"
	"log/slog"
)

// Name implements httpx.Checker. It is the key this dependency appears under in
// the /readyz JSON payload:
//
//	{"status":"not ready","service":"stream","checks":{"redis":{"status":"down","error":"..."}}}
func (c *Client) Name() string { return checkerName }

// Check implements httpx.Checker: it reports whether this service can execute a
// Redis command right now.
//
// # Pool saturation counts as not-ready, deliberately
//
// The PING goes through the pool, so a pool with every connection busy fails
// this check even though the server itself is healthy. That is the intended
// answer for a service that registers this checker at all (see the file
// comment): readiness means "send me traffic", and a replica that cannot obtain
// a connection cannot serve a request.
//
// The two cases stay distinguishable rather than being conflated:
// sharpline_redis_pool_timeouts_total rising with sharpline_redis_up at 0 is
// saturation, while sharpline_redis_up at 0 with a flat timeout count is an
// unreachable server.
//
// # Timeout
//
// ctx carries httpx's probe deadline (httpx.DefaultReadinessTimeout, 3s).
// PingTimeout (1s) is applied on top, so the check returns inside the probe's
// budget and the payload names `redis` as the failing dependency instead of the
// whole probe timing out with nothing to show. Whichever deadline is nearer
// wins, which is what context composition already guarantees.
func (c *Client) Check(ctx context.Context) error {
	if c.closed.isSet() {
		return ErrClosed
	}

	if err := c.ping(ctx); err != nil {
		c.log.Warn("redis readiness check failed",
			slog.String("kind", errorKind(err)),
			slog.Bool("transient", IsTransientConnectError(err)),
			slog.String("pool", c.statSummary()),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("redis: readiness check: %w", err)
	}
	return nil
}

// Ping performs one readiness round trip and returns its result.
//
// Check is what httpx calls; this is the same probe exposed for a caller that
// wants to test connectivity directly — a startup gate in a cmd/ entrypoint, a
// health handler that reports degradation without failing readiness, or a test.
//
// It is not a retry loop. Startup retries live in awaitReady, and this package
// deliberately exposes no general-purpose retry helper.
func (c *Client) Ping(ctx context.Context) error {
	if c.closed.isSet() {
		return ErrClosed
	}
	if err := c.ping(ctx); err != nil {
		return fmt.Errorf("redis: ping: %w", err)
	}
	return nil
}

// statSummary renders pool statistics for a log line. Kept as one string
// attribute rather than five so a failing-probe line stays readable; the
// machine-readable form is the Prometheus series.
func (c *Client) statSummary() string {
	s := c.poolStats()
	if s == nil {
		return "closed"
	}
	return fmt.Sprintf("total=%d idle=%d hits=%d misses=%d timeouts=%d stale=%d",
		s.TotalConns, s.IdleConns, s.Hits, s.Misses, s.Timeouts, s.StaleConns)
}

// checker is the shape internal/platform/httpx declares for a readiness
// dependency, restated here ONLY as a compile-time assertion.
//
// It is not exported and nothing consumes it: CLAUDE.md §12 puts the interface
// with the consumer, and httpx.Checker is that declaration. This exists so that
// a signature change in httpx breaks the build here instead of silently
// dropping *Client out of a service's readiness list — a service that would
// then report itself ready while Redis is unreachable.
type checker interface {
	Name() string
	Check(ctx context.Context) error
}

var _ checker = (*Client)(nil)
