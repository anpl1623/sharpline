// Health and readiness for internal/platform/httpx.
//
// # Liveness is not here, on purpose
//
// httpx.Server.handleHealthz answers /healthz without consulting any dependency,
// and says why in its own comment: "a liveness probe that fails because Postgres
// is down restarts a healthy pod for no reason". This file adds nothing to that
// path. A database outage must not become a rolling restart of every service
// that talks to it — that turns a recoverable dependency failure into a
// self-inflicted outage, and it does it at exactly the moment the system is
// least able to absorb one.
//
// So: liveness = the process is up and its scheduler is responsive. This file
// implements readiness, and only readiness.
//
// # Readiness is a real round trip
//
// httpx.Checker is a two-method interface declared by the consumer (CLAUDE.md
// §12), and *DB satisfies it without this package importing httpx — no import
// edge in either direction. Wiring is one line in a cmd/ entrypoint:
//
//	srv, err := httpx.NewServer(httpx.ServerOptions{
//	    Service:  cfg.Service,
//	    Addr:     cfg.HTTPAddr,
//	    Logger:   log,
//	    Checkers: []httpx.Checker{db},
//	})
//
// Check acquires a pooled connection and executes a statement on it. It is
// explicitly NOT a boolean latched at startup: phase 0's gate verified
// /api/readyz responds, and a readiness endpoint that reports a value captured
// once at boot answers "did this process once manage to connect", which is not
// the question a load balancer is asking.
//
// The round trip is measured in sharpline_db_ping_duration_seconds and
// sharpline_db_up, and deliberately does NOT produce a trace span — see the
// comment on DB.ping for why, and for the pgx call chain that makes it so.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
)

// Name implements httpx.Checker. It is the key this dependency appears under in
// the /readyz JSON payload:
//
//	{"status":"not ready","service":"api","checks":{"postgres":{"status":"down","error":"..."}}}
func (db *DB) Name() string { return checkerName }

// Check implements httpx.Checker: it reports whether this service can execute a
// database statement right now.
//
// # Pool saturation counts as not-ready, deliberately
//
// The round trip goes through the pool, so a pool with every connection checked
// out fails this check even though the server itself is healthy. That is the
// intended answer. Readiness means "send me traffic"; a replica that cannot
// obtain a connection cannot serve a request, and having it dropped from the
// Service's endpoints sheds load toward replicas that can. The alternative —
// bypassing the pool with a dedicated connection so the check always passes —
// would keep a replica in rotation precisely while it is unable to do any work,
// and would consume one of the server's 97 available connections per replica to
// achieve it.
//
// The two cases are distinguishable in the logs and in the metrics rather than
// being conflated: pool statistics accompany every failure, and
// sharpline_db_pool_empty_acquires_total rising with sharpline_db_up at 0 is
// saturation, while sharpline_db_up at 0 with a flat acquire count is an
// unreachable server.
//
// # Timeout
//
// ctx carries httpx's probe deadline (httpx.DefaultReadinessTimeout, 3s).
// PingTimeout (2s) is applied on top, so the check returns inside the probe's
// budget and the payload names `postgres` as the failing dependency instead of
// the whole probe timing out with nothing to show. Whichever deadline is nearer
// wins, which is what context composition already guarantees.
func (db *DB) Check(ctx context.Context) error {
	if db.closed.isSet() {
		return ErrClosed
	}

	if err := db.ping(ctx); err != nil {
		db.log.Warn("postgres readiness check failed",
			slog.String("sqlstate", SQLState(err)),
			slog.Bool("transient", IsTransientConnectError(err)),
			slog.String("pool", db.statSummary()),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("postgres: readiness check: %w", err)
	}
	return nil
}

// Ping performs one readiness round trip and returns its result. Check is what
// httpx calls; this is the same probe exposed for a caller that wants to test
// connectivity directly — a startup gate in a cmd/ entrypoint, or a test.
//
// It is not a retry loop. Startup retries live in Connect, and there is no
// general-purpose retry in this package by design (see awaitReady).
func (db *DB) Ping(ctx context.Context) error {
	if err := db.ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// statSummary renders pool statistics for a log line. Kept as one string
// attribute rather than eight so a failing-probe line stays readable; the
// machine-readable form is the Prometheus series.
func (db *DB) statSummary() string {
	s := db.Stat()
	if s == nil {
		return "closed"
	}
	return fmt.Sprintf("acquired=%d idle=%d constructing=%d max=%d empty_acquires=%d canceled_acquires=%d",
		s.AcquiredConns(), s.IdleConns(), s.ConstructingConns(), s.MaxConns(),
		s.EmptyAcquireCount(), s.CanceledAcquireCount())
}

// Checker is the shape internal/platform/httpx declares for a readiness
// dependency, restated here ONLY as a compile-time assertion.
//
// It is not exported and nothing consumes it: CLAUDE.md §12 puts the interface
// with the consumer, and httpx.Checker is that declaration. This exists so that
// a signature change in httpx breaks the build here instead of silently
// dropping *DB out of a service's readiness list — a service that then reports
// itself ready while its database is unreachable.
type checker interface {
	Name() string
	Check(ctx context.Context) error
}

var _ checker = (*DB)(nil)
