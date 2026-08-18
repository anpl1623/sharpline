// Command pricer consumes the compacted odds.normalized topic, devigs every
// market that moved, computes no-vig fair value, EV%, Kelly sizing, arbitrage
// and middles, and publishes the result to the compacted price.computed topic
// (CLAUDE.md §3).
//
// This file is the wiring only. internal/pricing is the composition root and
// carries the argument for the engine seam, the warm start, the change-detection
// guards and the shutdown order; read its doc.go before changing anything here.
//
// It is deliberately the sibling of cmd/ingest/main.go rather than a second
// idiom: the same probe branch, the same registry-before-collectors ordering,
// the same bus client options shared by every client in the process, the same
// deferred-close-plus-explicit-flush discipline, and the same
// listener-and-pipeline-stop-together shutdown.
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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider/theoddsapi"
	"github.com/anpl1623/sharpline/internal/platform/buildinfo"
	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/platform/logging"
	"github.com/anpl1623/sharpline/internal/pricing"
)

const service = "pricer"

func main() {
	// `sharpline healthcheck` self-probes this service's own /readyz and exits 0
	// or 1. The runtime image is gcr.io/distroless/static:nonroot — no shell, no
	// wget, no curl — so this binary is the only executable a Docker healthcheck
	// or a Kubernetes exec probe can invoke. It must stay the FIRST statement in
	// main: everything below it opens sockets. See internal/platform/httpx/probe.go.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.Pricer.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		// Config may have failed to load, so this is the only place a logger is
		// built without it. Exit non-zero so the orchestrator notices.
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
	// The build identity goes on the FIRST line this process writes, beside the
	// config; see cmd/api/main.go for why. It matters for this service in
	// particular: pricer is the one with an HPA on CPU (CLAUDE.md §9), so its
	// pods come and go, and a `count by (version)` over them is how a rolling
	// deploy is watched.
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

	// One collector set per process, shared by the producer, the consumer and the
	// snapshot reader below. Registering it once here rather than letting each
	// client build its own is what keeps sharpline_kafka_* a single series per
	// topic instead of three.
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

	producer, err := kafka.NewOddsProducer(ctx, kafka.ProducerOptions{ClientOptions: bus})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	// Close FLUSHES. pricing.Service.Run flushes explicitly first so a failure is
	// reported rather than swallowed by a deferred close, but the defer stays: an
	// early return from any path below must not discard accepted records.
	defer func() {
		if err := producer.Close(); err != nil {
			log.Error("closing the price producer failed", slog.String("error", err.Error()))
		}
	}()

	// ErrorPolicySkip: internal/platform/kafka/consumer.go blesses it for the
	// odds path specifically, because the next provider poll republishes the same
	// market — halting the whole pipeline on one poison record would freeze every
	// other market to preserve one. The service returns an error only for a
	// TRANSIENT publish failure, which under this policy is still logged, counted
	// and skipped; the offset for it is not committed, so it is redelivered.
	//
	// StartAtEnd is deliberately left false. A fresh group therefore replays
	// odds.normalized from the beginning, and on a COMPACTED topic that replay is
	// the current slate — which is what makes price.computed complete on a first
	// deploy rather than holding only the markets that happened to move since.
	consumer, err := kafka.NewConsumer(ctx, kafka.ConsumerOptions{
		ClientOptions: bus,
		Group:         pricing.GroupPricer,
		Topics:        []string{kafka.TopicOddsNormalized},
		ErrorPolicy:   kafka.ErrorPolicySkip,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer func() {
		if err := consumer.Close(); err != nil {
			log.Error("closing the normalized consumer failed", slog.String("error", err.Error()))
		}
	}()

	// The warm start reads price.computed — this service's OWN output — with no
	// consumer group, from the beginning, to the end offsets listed when the read
	// began. It is deliberately not warmed from Redis or from Postgres: the thing
	// this reads is the thing `stream` reads, which is the cache-coherency bug
	// class CLAUDE.md §3 chose a compacted Kafka topic to remove.
	snapshotter, err := kafka.NewSnapshotter(ctx, kafka.SnapshotOptions{
		ClientOptions: bus,
		Topic:         kafka.TopicPriceComputed,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer func() {
		if err := snapshotter.Close(); err != nil {
			log.Error("closing the price snapshotter failed", slog.String("error", err.Error()))
		}
	}()

	price, revision, err := newEngine(cfg, registry)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	log.Info("pricing engine built",
		slog.Any("reference_books", referenceBookSlugs(cfg.PricerReferenceBooks)),
		slog.String("devig_method", pricing.DefaultDevigMethod.String()),
		slog.String("engine_revision", revision),
	)

	svc, err := pricing.New(pricing.ServiceOptions{
		Price:          price,
		Producer:       producer,
		Consumer:       consumer,
		Snapshotter:    snapshotter,
		EngineRevision: revision,
		Logger:         log,
		Registry:       registry,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	// Warm the priced-state tracker HERE, under the startup context, rather than
	// letting it happen lazily inside the first delivered record.
	//
	// Warm is idempotent and guarded, so the lazy path stays correct and this is
	// purely a question of WHERE the snapshot read is paid for. Inside a handler
	// it is paid for while holding up the consumer's poll loop, which blocks the
	// group's rebalance for as long as the read takes — bounded by
	// kafka.DefaultSnapshotTimeout, i.e. up to a minute, during which every other
	// member of the group sits in the join phase. Phase 3b hit exactly this with
	// the normalizer. Here it is paid for before the consumer exists, which is
	// also the point at which a failure is legible.
	//
	// A failed warm start is NOT fatal — the service prices cold and reprices the
	// slate exactly once, which is bounded and harmless on a compacted topic.
	// Refusing to start would trade a one-time duplicate publish for a priced
	// board that stays frozen for as long as the broker is unhappy.
	if err := svc.Warm(ctx); err != nil {
		log.Error("pricer warm start failed; proceeding cold, the slate will reprice once",
			slog.String("error", err.Error()))
	}

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service:  service,
		Addr:     cfg.HTTPAddr,
		Logger:   log,
		Registry: registry,
		// Readiness reflects what this process actually depends on: the bus it
		// consumes from and produces to, and its own consumer loop.
		// *kafka.OddsProducer executes a real round trip on every probe rather
		// than latching a boolean, and *pricing.Service reports whether it is
		// consuming — a replica whose consumer has exited but whose listener is
		// still up would otherwise look healthy while pricing nothing.
		//
		// The consumer and the snapshotter are deliberately NOT listed: every bus
		// client reports the same checker name (`kafka`), so adding them would run
		// three round trips to answer one question and collapse into one JSON key
		// anyway.
		//
		// There is no Postgres checker because this binary opens no pool, and no
		// Redis checker because it opens no client. config.Pricer declares
		// RequireRedis, which under the phase-2 rule would oblige one — but there
		// is no internal/platform/redis in the tree, so no service opens Redis and
		// inventing a client to satisfy the declaration would reproduce the
		// phase-2 defect rather than avoid it. Reported upward for reconciliation;
		// a probe that stays silent about a dependency is honest, one that returns
		// 200 for a dependency the binary never opened is not.
		Checkers: []httpx.Checker{producer, svc},
	})
	if err != nil {
		return fmt.Errorf("%s: build operational listener: %w", service, err)
	}

	// The listener and the pricing loop run together and stop together: both
	// observe ctx, so SIGTERM drains the loop while /readyz is still answering,
	// which is what lets an orchestrator take this replica out of rotation before
	// its consumer finishes committing.
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

	wg.Add(2)
	go func() {
		defer wg.Done()
		fail(srv.Run(ctx))
	}()
	go func() {
		defer wg.Done()
		fail(svc.Run(ctx))
	}()
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log.Info("stopped")
	return nil
}

// referenceBookSlugs is the sharp-reference preference list, most preferred
// first, read from configuration with a documented default.
//
// internal/pricing/reference.go is explicit that the judgement must be
// configurable: "A pricing service that could not be told which book to trust
// would be hard-coding a trading judgement into a binary." So
// SHARPLINE_PRICER_REFERENCE_BOOKS wins whenever it is set, and this function
// supplies a default only when it is not.
//
// # Why these two, in this order, as the default
//
//   - "pinnacle" is The Odds API's sharp book and is already
//     theoddsapi.DefaultReferenceBook, so the two halves of the system name the
//     same book rather than disagreeing about it.
//   - "sharpline" is domain.SyntheticBookSlug, the synthetic generator's in-house
//     book, which quotes with the tightest margin, no lag and no bias.
//
// A ranked list rather than one name is what makes ONE binary correct against
// both providers: `ingest` picks its adapter from ODDS_API_KEY at startup and
// nothing tells `pricer` which it chose, so the pricer tries the real sharp book
// and falls through to the synthetic one when the real book does not quote the
// market.
//
// Both providers now also DESIGNATE their reference book on the wire
// (normalizer.BookRef.Reference), and that designation outranks this list
// entirely. This is the fallback for a provider that designates nothing, and
// reference.go records which of the two chose the book on every computed record
// so the difference is legible in
// sharpline_pricer_reference_book_total{source} rather than assumed.
func referenceBookSlugs(configured []string) []domain.Slug {
	if len(configured) == 0 {
		return []domain.Slug{
			domain.Slug(theoddsapi.DefaultReferenceBook),
			domain.SyntheticBookSlug,
		}
	}
	out := make([]domain.Slug, 0, len(configured))
	for _, s := range configured {
		out = append(out, domain.Slug(s))
	}
	return out
}

// newEngine builds the odds mathematics and adapts it to pricing.PriceFunc.
//
// # Why the adapter exists at all
//
// pricing.PriceFunc returns `any` so that the service is independent of whatever
// the engine computes — internal/pricing/doc.go argues that at length. Go has no
// covariance, so an engine returning a concrete payload type does not satisfy a
// signature returning `any` on its own; but `return e.Price(...)` inside a
// function declared to return (any, error) is legal, because Go's rule for
// returning a multi-valued call is assignability and every type is assignable to
// `any`. That is the whole of the adapter, and it is why the seam is a function
// type rather than an interface: an interface would have needed this adapter
// anyway, and would have hidden the fact.
//
// Everything else about the engine — the devig method, the attribution
// convention, the Kelly multiplier, the staleness policy — is left at
// internal/pricing's documented defaults, which are argued there with their
// alternatives. The one thing that cannot be defaulted is the reference book:
// an engine with an empty preference list resolves no sharp book and therefore
// computes no fair value at all, so the pricer would run, look healthy, and
// publish nothing.
//
// The engine's collectors go on the SAME registry as everything else in this
// process, so its families land on the one /metrics endpoint Prometheus scrapes
// at pricer:8082.
func newEngine(cfg *config.Config, registry prometheus.Registerer) (pricing.PriceFunc, string, error) {
	engine, err := pricing.NewEngine(pricing.Options{
		ReferenceBooks: referenceBookSlugs(cfg.PricerReferenceBooks),
		Registry:       registry,
	})
	if err != nil {
		return nil, "", err
	}
	price := func(ctx context.Context, rec normalizer.NormalizedMarket) (any, error) {
		return engine.Price(ctx, rec)
	}
	// The revision is the ENGINE's own digest of its whole configuration, not a
	// digest of the two settings this file happens to name. Suppression compares
	// the source record's fingerprint, which identifies the INPUT and says
	// nothing about the function applied to it, so a digest covering half the
	// configuration would silently hold yesterday's numbers for every market
	// that had not moved. See pricing.Engine.ConfigDigest.
	return price, engine.ConfigDigest(), nil
}
