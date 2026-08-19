// Command ingest runs the odds pipeline: adaptive polling of a provider
// adapter, rate limiting against the provider quota, publication of raw payloads
// to odds.raw.{provider}, normalization and change detection onto
// odds.normalized, and the Timescale line-history writer that consumes it
// (CLAUDE.md §3, §5).
//
// This file is the wiring only. internal/ingest is the composition root and
// carries the argument for the adapter announcement, the change-detection
// layering and the shutdown order; read its doc.go before changing anything
// here.
//
// The one decision made HERE rather than there is which adapter to construct,
// because that is the only decision that needs the concrete provider packages.
// internal/ingest deliberately names neither of them: it takes a
// provider.Adapter and cannot tell which one it was handed, which is what makes
// the offline path exercise the online path's code.
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

	"github.com/anpl1623/sharpline/internal/ingest"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/provider/synthetic"
	"github.com/anpl1623/sharpline/internal/ingest/provider/theoddsapi"
	"github.com/anpl1623/sharpline/internal/ingest/scheduler"
	"github.com/anpl1623/sharpline/internal/ingest/writer"
	"github.com/anpl1623/sharpline/internal/platform/buildinfo"
	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/platform/logging"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

const service = "ingest"

func main() {
	// `sharpline healthcheck` self-probes this service's own /readyz and exits 0
	// or 1. The runtime image is gcr.io/distroless/static:nonroot — no shell, no
	// wget, no curl — so this binary is the only executable a Docker healthcheck
	// or a Kubernetes exec probe can invoke. It must stay the FIRST statement in
	// main: everything below it opens sockets. See internal/platform/httpx/probe.go.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.Ingest.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		// Config may have failed to load, so this is the only place a logger is
		// built without it. Exit non-zero so the orchestrator notices.
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
	// The build identity goes on the FIRST line this process writes, beside the
	// config; see cmd/api/main.go for why.
	log.Info("starting",
		slog.Any("build", buildinfo.Read()),
		slog.Any("config", cfg),
	)

	// ingest hosts the Timescale line-history writer (CLAUDE.md §3), so it opens
	// a Postgres pool and its readiness must reflect that. The check is here
	// rather than left to a nil-pointer three layers down because config.Load
	// only READS a DSN for a Spec that declares RequirePostgres: without that
	// declaration the field is empty and the failure would otherwise surface as
	// an unhelpful connection error. Phase 2 shipped a probe that returned 200
	// with the database stopped by declaring a dependency it never opened; this
	// is the inverse of that mistake and gets the same refusal.
	if cfg.PostgresDSN == "" {
		return fmt.Errorf("%s: %s is not set. ingest hosts the Timescale line-history writer and cannot "+
			"start without a database: config.Ingest must declare config.RequirePostgres, and the compose "+
			"stack and Helm chart must extend their Postgres environment anchors to this service",
			service, config.EnvPostgresDSN)
	}

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

	// sharpline_provider_quota_remaining, _quota_limit and
	// sharpline_provider_requests_total are a contract with deploy/observability
	// and must be registered EXACTLY ONCE per process. This is that once — the
	// seam-level set works for either adapter and is the only source of the
	// stage="received" slice of sharpline_odds_staleness_seconds. An adapter that
	// exports its own copies is therefore built with an unregistered set; see
	// selectAdapter.
	providerMetrics, err := provider.NewMetrics(registry)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	schedulerMetrics, err := scheduler.NewMetrics(registry)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	busMetrics, err := kafka.NewMetrics(registry)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	adapter, decoder, err := selectAdapter(cfg, log)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	icfg, err := ingest.LoadConfig(cfg, adapter)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	busProvider, err := kafka.NewProvider(icfg.Provider.String())
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	rawTopic, err := kafka.OddsRaw(busProvider)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

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
	// Close FLUSHES. Service.Run flushes explicitly first so a failure is
	// reported rather than swallowed by a deferred close, but the defer stays:
	// an early return from any path below must not discard accepted records.
	defer func() {
		if err := producer.Close(); err != nil {
			log.Error("closing the odds producer failed", slog.String("error", err.Error()))
		}
	}()

	// ErrorPolicySkip on both loops: internal/platform/kafka/consumer.go blesses
	// it for the odds path specifically, because the next provider poll
	// republishes the same market — halting the whole pipeline on one poison
	// record would freeze every other market to preserve one.
	rawConsumer, err := kafka.NewConsumer(ctx, kafka.ConsumerOptions{
		ClientOptions: bus,
		Group:         ingest.GroupNormalizer,
		Topics:        []string{rawTopic.Name()},
		ErrorPolicy:   kafka.ErrorPolicySkip,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer func() {
		if err := rawConsumer.Close(); err != nil {
			log.Error("closing the raw consumer failed", slog.String("error", err.Error()))
		}
	}()

	normalizedConsumer, err := kafka.NewConsumer(ctx, kafka.ConsumerOptions{
		ClientOptions: bus,
		Group:         ingest.GroupTimescaleWriter,
		Topics:        []string{kafka.TopicOddsNormalized},
		ErrorPolicy:   kafka.ErrorPolicySkip,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer func() {
		if err := normalizedConsumer.Close(); err != nil {
			log.Error("closing the normalized consumer failed", slog.String("error", err.Error()))
		}
	}()

	norm, err := newNormalizer(ctx, busProvider, decoder, producer, bus, registry, log)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	lineWriter, err := writer.New(writer.Options{
		DB:       db,
		Logger:   log,
		Registry: registry,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	svc, err := ingest.New(ingest.Options{
		Config:             icfg,
		Adapter:            adapter,
		Producer:           producer,
		RawConsumer:        rawConsumer,
		Normalizer:         norm,
		NormalizedConsumer: normalizedConsumer,
		Writer:             lineWriter,
		Logger:             log,
		Registry:           registry,
		ProviderMetrics:    providerMetrics,
		SchedulerMetrics:   schedulerMetrics,
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
		// it writes line history to, the bus it produces to and consumes from, and
		// the pipeline itself. *postgres.DB and *kafka.OddsProducer each execute a
		// real round trip on every probe rather than latching a boolean, and
		// *ingest.Service reports whether its stages are running — a replica whose
		// consumers have exited but whose listener is still up would otherwise
		// look healthy while producing nothing.
		//
		// The two consumers are deliberately NOT listed: every bus client reports
		// the same checker name (`kafka`), so adding them would run three probes
		// to answer one question and collapse into one JSON key anyway.
		Checkers: []httpx.Checker{db, producer, svc},
	})
	if err != nil {
		return fmt.Errorf("%s: build operational listener: %w", service, err)
	}

	// The listener and the pipeline run together and stop together: both observe
	// ctx, so SIGTERM drains the pipeline while /readyz is still answering, which
	// is what lets an orchestrator take this replica out of rotation before its
	// consumers finish committing.
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

// selectAdapter constructs the odds source and the decoder that reads what it
// publishes.
//
// The rule is ADR 0003's and the contract ledger's, and it has no third branch:
// the real The Odds API adapter when ODDS_API_KEY is set, the synthetic
// stochastic market maker when it is not. A key that is set but whose adapter
// cannot be built is a startup failure — there is deliberately no failover to
// the simulation, because substituting simulated prices for real ones in a
// running deployment is indistinguishable from fabricating market data.
//
// The decoder comes back with the adapter because it is the same decision: the
// decoder is the ONLY per-provider stage after the adapter, and pairing them
// here is what makes it impossible to run one provider's payloads through the
// other's syntax layer. Everything downstream of the decoder — the mapping onto
// the domain, the fingerprint, the suppression — is shared code, which is what
// makes "two providers produce identical domain values for equivalent input" a
// property of the architecture rather than of a test.
//
// internal/ingest announces the outcome; this function only decides it.
func selectAdapter(cfg *config.Config, log *slog.Logger) (provider.Adapter, normalizer.Decoder, error) {
	if cfg.HasOddsAPIKey() {
		acfg, err := theoddsapi.ConfigFromEnv(os.LookupEnv)
		if err != nil {
			return nil, nil, err
		}
		// ConfigFromEnv returns what the environment SAID, with every unset
		// optional still zero. The adapter defaults them internally, so without
		// this line acfg is not the config the adapter runs on and the two
		// reads below pick up empty strings — which is a fatal startup error
		// for the format and a silent loss of the sharp-book flag for the
		// reference book. Defaulting here makes acfg the single description of
		// what this process asked the provider for. It is idempotent.
		acfg = acfg.WithDefaults()
		// The adapter's own collectors are built UNREGISTERED: it exports its own
		// copies of the three sharpline_provider_* contract series and run() has
		// already registered the seam-level set, which is the one that also
		// carries the staleness histogram. Two registrations of one contract
		// series with different label sets fail the process at startup.
		metrics, err := theoddsapi.NewMetrics(nil)
		if err != nil {
			return nil, nil, err
		}
		adapter, err := theoddsapi.NewAdapter(acfg,
			theoddsapi.WithLogger(log),
			theoddsapi.WithMetrics(metrics),
		)
		if err != nil {
			return nil, nil, err
		}
		// The format is the adapter's, not a second opinion: the decoder converts
		// whatever was ASKED FOR into decimal, and asking for one format while
		// decoding another silently reinterprets every price.
		// The reference book is the adapter's too, for the same reason: the
		// decoder is what stamps the sharp-book designation onto every record
		// replayed off odds.raw, and a decoder that disagreed with the adapter
		// would designate a different book on the replay path than on the live
		// one.
		decoder, err := theoddsapi.NewDecoder(acfg.OddsFormat, acfg.ReferenceBook, metrics)
		if err != nil {
			return nil, nil, err
		}
		return adapter, decoder, nil
	}

	// Seeded from SHARPLINE_SYNTHETIC_SEED so a demo replays.
	adapter, err := synthetic.New(synthetic.Options{Seed: cfg.SyntheticSeed})
	if err != nil {
		return nil, nil, err
	}
	// The synthetic generator has no wire format of its own — it marshals the
	// normalizer's neutral shape directly — so its decoder is a plain unmarshal.
	busProvider, err := kafka.NewProvider(provider.NameSynthetic.String())
	if err != nil {
		return nil, nil, err
	}
	decoder, err := normalizer.NewNeutralDecoder(busProvider)
	if err != nil {
		return nil, nil, err
	}
	return adapter, decoder, nil
}

// newNormalizer builds the odds.raw.{provider} → odds.normalized stage.
//
// The fingerprint store is warmed from odds.normalized itself — the compacted
// topic, read from the beginning with no consumer group, which is exactly what
// kafka.Snapshotter is for — so the first poll after a deploy does not
// republish the entire board. It is deliberately not warmed from Redis: the
// thing this reads is the thing clients read, which is the cache-coherency bug
// class CLAUDE.md §3 chose Kafka to remove.
//
// # The contract this call depends on
//
// internal/ingest/normalizer must export a constructor of this shape:
//
//	func New(Options) (*Normalizer, error)   // *Normalizer implements kafka.Handler
//
//	type Options struct {
//	    Provider    kafka.Provider   // selects the decoder and stamps the record
//	    Decoder     Decoder          // raw.go's seam; per provider
//	    Producer    …                // needs PublishNormalized (SYNCHRONOUS — see below)
//	    Snapshotter …                // warm start from the compacted topic
//	    Logger      *slog.Logger
//	    Registry    prometheus.Registerer
//	    RefreshAfter time.Duration   // suppression ceiling; zero means the package default
//	}
//
// The handler MUST publish synchronously. internal/ingest/doc.go's shutdown
// argument rests on it: the Consumer commits the last successfully handled
// record per partition, so a handler that returned before its record was
// acknowledged would let an offset be committed ahead of the record it produced
// and the market would be lost on the next restart.
// kafka.OddsProducer.PublishNormalized waits; PublishNormalizedAsync does not.
func newNormalizer(
	ctx context.Context,
	busProvider kafka.Provider,
	decoder normalizer.Decoder,
	producer *kafka.OddsProducer,
	bus kafka.ClientOptions,
	registry prometheus.Registerer,
	log *slog.Logger,
) (kafka.Handler, error) {
	snapshotter, err := kafka.NewSnapshotter(ctx, kafka.SnapshotOptions{
		ClientOptions: bus,
		Topic:         kafka.TopicOddsNormalized,
	})
	if err != nil {
		return nil, err
	}

	norm, err := normalizer.New(normalizer.Options{
		Provider:    busProvider,
		Decoder:     decoder,
		Producer:    producer,
		Snapshotter: snapshotter,
		Logger:      log,
		Registry:    registry,
	})
	if err != nil {
		return nil, err
	}

	// Warm the fingerprint store HERE, under the startup context, rather than
	// letting it happen lazily inside the first delivered record.
	//
	// Warm is idempotent and guarded, so the lazy path stays correct and this is
	// purely a question of WHERE the snapshot read is paid for. Inside a handler
	// it is paid for while holding up the consumer's poll loop, which blocks the
	// group's rebalance for as long as the read takes — bounded by
	// kafka.DefaultSnapshotTimeout, i.e. up to a minute. Here it is paid for
	// before the consumer exists, which is also the point at which a failure is
	// legible: the "warm start" line lands next to the adapter announcement
	// instead of buried in the first poll.
	//
	// A failed warm start is NOT fatal — the normalizer proceeds cold and
	// republishes the slate exactly once, which is bounded and harmless on a
	// compacted topic. Refusing to start would trade a one-time duplicate
	// publish for a board that stays frozen for as long as the broker is
	// unhappy, which is the worse failure.
	if err := norm.Warm(ctx); err != nil {
		log.Error("normalizer warm start failed; proceeding cold, the first sweep will republish the slate",
			"error", err)
	}

	return norm, nil
}
