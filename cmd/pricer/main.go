// Command pricer devigs incoming prices and computes no-vig fair value, EV%,
// Kelly sizing, arbitrage and middles, and parlay correlation (CLAUDE.md §3).
//
// Phase 0 wires the process shape only — typed config, JSON logging, the
// operational listener on the frozen internal port, graceful shutdown on
// SIGTERM. The odds math lands in phase 1 and the pricing pipeline in phase 4.
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

const service = "pricer"

func main() {
	// Self-probe mode; see cmd/api/main.go and internal/platform/httpx/probe.go.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.Pricer.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		logging.Bootstrap(os.Stderr, service).Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.Pricer)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	log.Info("starting", slog.Any("config", cfg))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service: service,
		Addr:    cfg.HTTPAddr,
		Logger:  log,
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
