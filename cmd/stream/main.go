// Command stream is the WebSocket gateway: subscription routing, the
// snapshot-then-delta protocol and per-client backpressure (CLAUDE.md §3, §5).
//
// Phase 0 wires the process shape only — typed config, JSON logging, the
// operational listener on the frozen internal port, graceful shutdown on
// SIGTERM. The hub, the /ws upgrade endpoint and the Redis-backed subscription
// state arrive in phase 6.
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

const service = "stream"

func main() {
	// Self-probe mode; see cmd/api/main.go and internal/platform/httpx/probe.go.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.Stream.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		logging.Bootstrap(os.Stderr, service).Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.Stream)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	// Build identity on the first line; see cmd/api/main.go for why.
	log.Info("starting",
		slog.Any("build", buildinfo.Read()),
		slog.Any("config", cfg),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Built here so a collector lands on it before the listener exists; see
	// cmd/ingest/main.go.
	registry := httpx.NewRegistry()

	// sharpline_build_info; see cmd/api/main.go. This service is the one scaled
	// by a custom-metric HPA on active WebSocket connections (CLAUDE.md §9), so
	// its replica set is the most volatile in the cluster and identifying which
	// build a given pod is serving is correspondingly harder without this.
	if err := buildinfo.Register(registry, service); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service:  service,
		Addr:     cfg.HTTPAddr,
		Logger:   log,
		Registry: registry,
		// Read and write deadlines would sever a long-lived WebSocket
		// connection, so this listener runs without them. ReadHeaderTimeout
		// keeps its default, which still bounds a slowloris handshake. Idle
		// connections are reaped by the hub's own heartbeat ping/pong
		// instead (CLAUDE.md §5).
		ReadTimeout:  -1,
		WriteTimeout: -1,
		IdleTimeout:  -1,
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
