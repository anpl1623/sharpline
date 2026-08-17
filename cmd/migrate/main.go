// Command migrate applies the schema migrations. It is the odd one out among
// the six binaries: a run-to-completion job, not a server (CLAUDE.md §9 —
// "goose; compose dependency + k8s Job"). It starts no listener, exposes no
// port, and must terminate so that `depends_on: condition:
// service_completed_successfully` and a Helm pre-install/pre-upgrade hook Job
// both work.
//
// It deliberately exposes no /metrics endpoint. deploy/observability/prometheus.yml
// leaves migrate out of the scrape config with the same reasoning, and the
// Grafana overview panel documents its absence: a run-to-completion workload
// with no frozen port cannot be scraped, and adding a listener to make it
// scrapable would stop it terminating. Its observable surface is its structured
// log, which is why the log reports how many migrations applied and which.
//
// Exit codes are the contract:
//
//	0  the requested command succeeded
//	1  configuration was invalid, the database was unreachable, or a
//	   migration/validation failed
//	2  argv did not name a command this binary implements
//
// Exit code 1 is the important one. `api` starts only once this container exits
// 0, so exiting 0 after a silent failure would put api in front of a schema it
// does not match. Every failure path here ends in a non-zero exit and a logged
// error.
//
// The SQL is compiled in: see internal/platform/migrate and the migrations
// package for the embed. The runtime image is distroless/static with no shell
// and no filesystem to mount SQL from, so this binary is the only thing in it —
// which is also why every operation is a subcommand of the binary rather than a
// separate tool.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/logging"
	"github.com/anpl1623/sharpline/internal/platform/migrate"
)

const service = "migrate"

// Exit codes. Named so the contract in the package comment is enforced by the
// code rather than restated by it.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	// argv is parsed before configuration is loaded: a typo in a compose
	// `command:` or a Helm hook argument should be reported as a usage error,
	// not misdiagnosed as a missing DSN.
	inv, err := migrate.ParseArgs(os.Args[1:])
	if err != nil {
		log := logging.Bootstrap(os.Stderr, service)
		log.Error("usage", slog.String("error", err.Error()))
		// The help text goes to stderr unstructured on purpose. A JSON-escaped
		// multi-line string is unreadable, and this is the one message whose
		// audience is a human reading `docker logs` rather than a collector.
		fmt.Fprintln(os.Stderr, migrate.Usage())
		os.Exit(exitUsage)
	}

	if err := run(inv); err != nil {
		logging.Bootstrap(os.Stderr, service).Error("fatal",
			slog.String("command", string(inv.Command)),
			slog.String("error", err.Error()),
		)
		os.Exit(exitFailure)
	}
	os.Exit(exitOK)
}

func run(inv migrate.Invocation) error {
	cfg, err := config.Load(config.Migrate)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	log.Info("starting",
		slog.Any("config", cfg),
		slog.String("command", string(inv.Command)),
	)

	// SIGTERM must abort cleanly. A Kubernetes Job can be deleted mid-run and a
	// `docker compose down` sends SIGTERM: cancelling the context rolls back the
	// in-flight migration's transaction and this process exits non-zero, which
	// is the correct report. Without this, the container is killed and the
	// exit status is a lie about what state the schema is in.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	runner, err := migrate.New(migrate.Options{
		DSN:    cfg.PostgresDSN,
		Logger: log,
		// goose's verbose mode is gated behind SHARPLINE_LOG_LEVEL=debug, and
		// the reason is measured rather than assumed: with it on, a single `up`
		// over the current seven migrations emitted 208 "executing statement"
		// lines carrying the full SQL of every statement — 220KB of log for one
		// run, which buries the summary line and would dominate log storage for
		// a Job that runs on every deploy. The per-migration reporting the
		// operator actually needs (version, file, direction, duration) is
		// emitted by internal/platform/migrate itself at info level, so nothing
		// is lost by default and the full statement trace is one variable away
		// when a migration is being debugged.
		Verbose: cfg.LogLevel <= slog.LevelDebug,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer func() {
		if err := runner.Close(); err != nil {
			log.Warn("closing database pool", slog.String("error", err.Error()))
		}
	}()

	summary, runErr := runner.Run(ctx, inv)
	if runErr != nil {
		// The summary is logged even on failure: a partially applied `up` needs
		// to report which migrations committed before it broke.
		if summary != nil {
			log.Error("failed", slog.Any("summary", summary))
		}
		// Returned unwrapped, unlike the config error above. Every error out of
		// internal/platform/migrate already begins "migrate: ", and the log line
		// carries service=migrate, so re-prefixing produced the doubled
		// "migrate: migrate: no applied migration to roll back" this comment
		// exists to stop coming back.
		return runErr
	}

	log.Info("completed", slog.Any("summary", summary))
	return nil
}
