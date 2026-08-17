// Command ingest runs the provider adapters: adaptive polling, rate limiting
// against the provider quota, payload normalization, change detection, and
// publication of deltas to the bus (CLAUDE.md §3, §5).
//
// Phase 0 wires the process shape only — typed config, JSON logging, the
// operational listener on the frozen internal port, graceful shutdown on
// SIGTERM. No adapter is selected and no odds are produced yet; the
// ODDS_API_KEY-versus-synthetic decision is made here in phase 3.
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
)

const service = "ingest"

func main() {
	// Self-probe mode; see cmd/api/main.go and internal/platform/httpx/probe.go.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.Ingest.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		logging.Bootstrap(os.Stderr, service).Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.Ingest)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	// Build identity on the first line; see cmd/api/main.go for why.
	log.Info("starting",
		slog.Any("build", buildinfo.Read()),
		slog.Any("config", cfg),
	)

	// Recorded now because it is the single most consequential runtime switch
	// in this binary; the adapter it selects is built in phase 3.
	log.Info("provider adapter selection",
		slog.Bool("odds_api_key_set", cfg.HasOddsAPIKey()),
		slog.String("adapter", adapterName(cfg)),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The registry is built here rather than left to httpx.NewServer's nil
	// fallback so that a collector can be installed on it BEFORE the listener is
	// constructed. Registering after NewServer would work too, but only via
	// srv.Registry(), which reads as reaching back into the server for a
	// dependency it was never given (CLAUDE.md §12: constructor injection).
	registry := httpx.NewRegistry()

	// sharpline_build_info; see cmd/api/main.go.
	if err := buildinfo.Register(registry, service); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service:  service,
		Addr:     cfg.HTTPAddr,
		Logger:   log,
		Registry: registry,
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

// adapterName reports which ProviderAdapter this process will run: the real
// provider when a key is configured, otherwise the synthetic stochastic market
// maker, which is a live generator rather than fixture data.
func adapterName(cfg *config.Config) string {
	if cfg.HasOddsAPIKey() {
		return "the-odds-api"
	}
	return "synthetic"
}
