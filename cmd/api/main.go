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

	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/logging"
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
	log.Info("starting", slog.Any("config", cfg))

	// SIGTERM is what Kubernetes and `docker stop` send; SIGINT is Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service: service,
		Addr:    cfg.HTTPAddr,
		Logger:  log,
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
