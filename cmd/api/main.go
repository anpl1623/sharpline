// Command api serves the Sharpline REST API: auth, catalog, bet slip, wagers,
// account and history (CLAUDE.md §3).
//
// Phase 0 wires the process shape only — typed config, JSON logging, the
// operational listener on the frozen internal port, graceful shutdown on
// SIGTERM. No routes beyond /healthz, /readyz and /metrics exist yet, and no
// data is served: an empty surface is correct, a fabricated one is a defect.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/anpl1623/sharpline/internal/platform/buildinfo"
	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/logging"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

const service = "api"

// publicPrefix is the path prefix the reverse proxy forwards to this service
// verbatim (deploy/proxy/Caddyfile, Route 2). Every public route this binary
// ever mounts lives beneath it.
const publicPrefix = "/api"

func main() {
	// `sharpline healthcheck` self-probes this service's own /readyz and
	// exits 0 or 1. The runtime image is gcr.io/distroless/static:nonroot —
	// no shell, no wget, no curl — so this binary is the only executable a
	// Docker healthcheck or a Kubernetes exec probe can invoke. See
	// internal/platform/httpx/probe.go.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.API.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		// Config may have failed to load, so this is the only place a logger
		// is built without it. Exit non-zero so the orchestrator notices.
		logging.Bootstrap(os.Stderr, service).Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.API)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	// The build identity goes on the FIRST line this process writes, beside the
	// config, because it answers the question every other line depends on:
	// which code produced them. The runtime image is distroless — no shell —
	// so there is no `sharpline --version` an operator can run against a
	// container to find out afterwards.
	log.Info("starting",
		slog.Any("build", buildinfo.Read()),
		slog.Any("config", cfg),
	)

	// SIGTERM is what Kubernetes and `docker stop` send; SIGINT is Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The registry is built BEFORE the pool and handed to both, which is the
	// only ordering that gets all three properties at once: the pool's
	// collectors land on the same /metrics as the Go runtime collectors
	// httpx.NewRegistry installs, the pool exists before NewServer so it can be
	// passed as a Checker by value rather than through a mutable indirection,
	// and no global registry is touched (CLAUDE.md §12: no global mutable
	// state).
	registry := httpx.NewRegistry()

	// sharpline_build_info: the same three facts as the log line above, as
	// labels on a constant 1, so "which build is this replica on" is answerable
	// from Prometheus during a rolling deploy rather than only from whichever
	// log lines are still in the retention window.
	if err := buildinfo.Register(registry, service); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	// config.API declares RequirePostgres, which config.go defines as "the
	// binary opens a Postgres connection pool" — so the DSN is already
	// validated at startup and this is the code that keeps that promise.
	// Connect blocks until the server answers or the retry budget is spent; it
	// does NOT latch a boolean, and db.Check re-executes a statement on every
	// /readyz.
	db, err := postgres.Connect(ctx, postgres.Options{
		DSN:      cfg.PostgresDSN,
		Service:  service,
		Logger:   log,
		Registry: registry,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer db.Close()

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service:  service,
		Addr:     cfg.HTTPAddr,
		Logger:   log,
		Registry: registry,
		// *postgres.DB satisfies httpx.Checker (Name/Check), so /readyz makes a
		// real round trip through the pool on every probe. A readiness endpoint
		// that stays green while the database is unreachable is worse than
		// none: the orchestrator keeps routing to a replica that cannot serve.
		// Liveness deliberately does NOT consult it — a database outage must
		// not become a rolling restart (see internal/platform/postgres/health.go).
		Checkers: []httpx.Checker{db},
		// deploy/proxy/Caddyfile forwards /api/* here WITHOUT stripping the
		// prefix, so this is what makes `curl https://<host>/api/healthz`
		// answer. It mirrors /healthz and /readyz only — never /metrics, which
		// the proxy hard-denies at the root and which has no business being
		// public. Operational truth, not product surface: no route below
		// serves data, because no data exists yet in phase 0.
		PublicPrefix: publicPrefix,
	})
	if err != nil {
		return fmt.Errorf("%s: build operational listener: %w", service, err)
	}

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log.Info("stopped")
	return nil
}
