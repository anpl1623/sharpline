// Command migrate applies the schema migrations. It is the odd one out among
// the six binaries: a run-to-completion job, not a server (CLAUDE.md §9 —
// "goose; compose dependency + k8s Job"). It starts no listener, exposes no
// port, and must terminate so that `depends_on: condition: service_completed`
// and a Helm pre-install/pre-upgrade hook Job both work.
//
// Exit codes are the contract:
//
//	0  migrations are applied and the schema is at the expected version
//	1  configuration was invalid, or a migration failed
//
// Phase 0 has no migrations and no goose wiring — that is phase 2. Until then
// this binary validates its configuration and exits 0, which keeps
// `docker compose up` working end to end rather than blocking on a job that
// cannot yet succeed.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/logging"
)

const service = "migrate"

func main() {
	if err := run(); err != nil {
		logging.Bootstrap(os.Stderr, service).Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.Migrate)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	log.Info("starting", slog.Any("config", cfg))

	// Phase 2 replaces this block with: open a pgx pool against
	// cfg.PostgresDSN under a context with a timeout, run goose Up against the
	// embedded migrations/ directory, and report the resulting schema version.
	log.Info("no migrations to apply",
		slog.String("reason", "migrations are introduced in phase 2"),
		slog.Int("applied", 0),
	)

	log.Info("completed")
	return nil
}
