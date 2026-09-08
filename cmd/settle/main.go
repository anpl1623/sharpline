// Command settle consumes the results feed, grades open wagers, writes the
// double-entry ledger rows and emits settlement events (CLAUDE.md §3).
//
// This file is the wiring only. internal/settlement is the composition root and
// carries the argument for the results-feed decision, the publish-before-commit
// ordering, the three independent idempotency guards and the never-retry-a-
// failed-transaction rule; read its doc.go before changing anything here.
//
// It is deliberately the sibling of cmd/pricer/main.go and cmd/ingest/main.go
// rather than a third idiom: the same probe branch, the same
// registry-before-collectors ordering, the same bus client options shared by
// every client in the process, the same deferred-close-plus-explicit-flush
// discipline, and the same listener-and-worker-stop-together shutdown.
//
// The one shape that is NOT shared, and is different on purpose: this binary
// consumes no Kafka topic. Its input is the `events` table, which the ingest
// pipeline writes — internal/settlement/doc.go argues at length why that is
// reading the pipeline's own output rather than reading fixture data. So there
// is no consumer here, no consumer group, and no warm start; there is a poll
// loop and a Postgres pool.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/anpl1623/sharpline/internal/analytics/clv"
	"github.com/anpl1623/sharpline/internal/analytics/clv/pgclv"
	"github.com/anpl1623/sharpline/internal/platform/buildinfo"
	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/platform/logging"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/settlement"
	"github.com/anpl1623/sharpline/internal/settlement/pgstore"
)

const service = "settle"

func main() {
	// Self-probe mode; see cmd/api/main.go and internal/platform/httpx/probe.go.
	// It must stay the FIRST statement in main: everything below it opens
	// sockets, and the runtime image is gcr.io/distroless/static:nonroot — no
	// shell, no wget, no curl — so this binary is the only executable a Docker
	// healthcheck or a Kubernetes exec probe can invoke.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.Settle.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		logging.Bootstrap(os.Stderr, service).Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.Settle)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	// Build identity on the first line; see cmd/api/main.go for why. It matters
	// more here than anywhere else in the system: this binary writes the ledger,
	// and "which build graded this wager" is a question a settlement dispute
	// starts with.
	log.Info("starting",
		slog.Any("build", buildinfo.Read()),
		slog.Any("config", cfg),
	)

	// SIGTERM is what Kubernetes and `docker stop` send; SIGINT is Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The registry is built before anything that exports a series, so every
	// collector lands on the same /metrics as the Go runtime collectors
	// httpx.NewRegistry installs, and no global registry is touched (CLAUDE.md
	// §12: no global mutable state).
	registry := httpx.NewRegistry()
	if err := buildinfo.Register(registry, service); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	// config.Settle declares RequirePostgres, and the ledger this service
	// writes is the one place in the system where a wrong balance is permanent
	// (balances are derived, CLAUDE.md §4). The pool is opened here for the same
	// reasons cmd/api opens one, and the comments there apply verbatim: one
	// registry shared with the listener, and *postgres.DB as a real readiness
	// Checker rather than a boolean latched at startup.
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

	// One collector set per process. Registering it once here rather than
	// letting the client build its own is what keeps sharpline_kafka_* a single
	// series per topic — and it is the pattern every other entrypoint follows,
	// so a second bus client added later costs nothing.
	busMetrics, err := kafka.NewMetrics(registry)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	bus := kafka.ClientOptions{
		Brokers: cfg.KafkaBrokers,
		Service: service,
		Logger:  log,
		Metrics: busMetrics,
	}

	// THE AUDIT PRODUCER, and not the odds producer.
	//
	// kafka.NewAuditProducer is the never-give-up half of the bus layer's
	// posture table: PublishWagerEvent is synchronous, has no asynchronous
	// sibling, and retries without bound so that "the caller stays blocked and
	// can refuse to commit the surrounding database transaction, which is the
	// correct failure". internal/settlement depends on exactly that — its
	// publish happens inside the settlement transaction, so a broker that will
	// not acknowledge means a ticket that does not settle rather than a
	// settlement with no audit record. Substituting the odds producer here, with
	// its bounded retries and its fire-and-forget path, would silently remove
	// that interlock.
	producer, err := kafka.NewAuditProducer(ctx, kafka.ProducerOptions{ClientOptions: bus})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	// Close FLUSHES. Every publish on this path is already synchronous and
	// acknowledged before its transaction commits, so in principle there is
	// nothing buffered to lose — the defer stays because "in principle" is not
	// the standard this binary is held to, and an early return from any path
	// below must not discard an accepted record.
	defer func() {
		if err := producer.Close(); err != nil {
			log.Error("closing the audit producer failed", slog.String("error", err.Error()))
		}
	}()

	// Two adapters over one pool, because they are two responsibilities.
	//
	// settlement.ResultsSource reads the `events` table — a pure read that never
	// opens a transaction — and settlement.Store reads and writes wagers, legs
	// and the ledger, always inside one. internal/settlement declares them
	// separately for that reason and never learns that they share a pool.
	//
	// The results source takes the logger because it SKIPS a row that is not a
	// usable result, and a skipped result is a customer's stake sitting in
	// escrow with nothing to release it. That has to be visible.
	results, err := pgstore.NewResults(pgstore.ResultsOptions{DB: db, Logger: log})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	store, err := pgstore.NewStore(db)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	svc, err := settlement.New(settlement.ServiceOptions{
		Results:   results,
		Store:     store,
		Publisher: producer,
		Logger:    log,
		Registry:  registry,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	// THE CLOSING-LINE-VALUE PASS — a third adapter over the same pool, and a
	// SECOND LOOP that this binary runs alongside the settlement one.
	//
	// internal/settlement/clv.go carries the argument in full, and the one
	// sentence of it that governs this wiring is: CLV must not be able to fail a
	// settlement. That is why the pass is constructed separately, run on its own
	// goroutine, and — see the Checkers list below — deliberately absent from the
	// readiness set. A wedged measurement must cost a missing report and nothing
	// else.
	//
	// The measurer is built first because it OWNS the two parameters of the
	// closing-price definition, and the pass reads them back off it rather than
	// restating them. They are stamped onto every published record, so a value
	// chosen in two places would eventually be two values and the phase-12
	// validation would be comparing answers to two different questions.
	clvStore, err := pgclv.NewStore(db)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	measurer, err := clv.New(clv.Options{Store: clvStore})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	clvPass, err := settlement.NewCLVPass(settlement.CLVOptions{
		Measurer:  measurer,
		Store:     clvStore,
		Publisher: producer,
		Logger:    log,
		Registry:  registry,
		// Read back off the measurer, never restated. See above.
		ClosingLookback: measurer.ClosingLookback(),
		TakenLookback:   measurer.TakenLookback(),
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service:  service,
		Addr:     cfg.HTTPAddr,
		Logger:   log,
		Registry: registry,
		// Readiness reflects what this process actually depends on: the database
		// it grades and pays from, the bus it writes the audit trail to, and its
		// own poll loop.
		//
		// *postgres.DB and *kafka.AuditProducer each execute a real round trip on
		// every probe rather than latching a boolean. *settlement.Service reports
		// whether the loop is running, which is a question neither of the other
		// two can answer: a replica whose loop has exited but whose listener is
		// still up would otherwise look healthy while every finished game sat
		// ungraded and every customer's stake sat in escrow.
		//
		// There is no Redis checker because this binary opens no client.
		// config.Settle does not declare RequireRedis, so nothing is being
		// silently skipped — unlike the pricer, whose declaration and whose
		// checkers disagree and which says so.
		//
		// THE CLV PASS IS DELIBERATELY ABSENT from this list, and its absence is a
		// design decision rather than an omission. It is the second loop this
		// binary runs, and internal/settlement/clv.go's premise is that a wedged
		// measurement must not be able to stop a settlement. Listing it here would
		// reintroduce exactly that coupling through the orchestrator: a failing
		// CLV checker takes the replica out of rotation, and a replica out of
		// rotation is a replica whose finished games sit ungraded. The pass
		// reports itself through sharpline_settlement_clv_* instead, which is
		// where a report's health belongs.
		Checkers: []httpx.Checker{db, producer, svc},
	})
	if err != nil {
		return fmt.Errorf("%s: build operational listener: %w", service, err)
	}

	// The listener and the settlement loop run together and stop together: both
	// observe ctx, so SIGTERM drains the loop while /readyz is still answering,
	// which is what lets an orchestrator take this replica out of rotation
	// before it finishes.
	//
	// "Drains" is literal here and is the reason this binary's shutdown is worth
	// a paragraph rather than a line. settlement.Service stops at a POLL
	// boundary, and a settlement already in flight runs to completion under a
	// context detached from this one — because a context cancelled in the middle
	// of a ledger transaction is precisely the failure
	// internal/platform/postgres/tx.go's detached rollback exists to survive, and
	// inflicting it deliberately once per deploy would be a poor use of that
	// helper. So this WaitGroup can legitimately take up to one transaction
	// timeout to complete after the signal, and that is correct rather than slow.
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	fail := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	wg.Add(3)
	go func() {
		defer wg.Done()
		fail(srv.Run(ctx))
	}()
	go func() {
		defer wg.Done()
		fail(svc.Run(ctx))
	}()
	go func() {
		defer wg.Done()
		// CLVPass.Run always returns nil, by design: a CLV pass has no failure
		// that should take this process down with it, and this join is exactly the
		// mechanism by which one otherwise could. The call is still wrapped in
		// fail for symmetry with its two siblings, so that a future change to that
		// signature is surfaced here rather than dropped on the floor.
		fail(clvPass.Run(ctx))
	}()
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log.Info("stopped")
	return nil
}
