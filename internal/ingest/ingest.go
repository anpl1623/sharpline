// The composition root itself. Read doc.go first: it carries the argument for
// the adapter announcement, the two-layer change detection and the shutdown
// order, and this file is the code those arguments describe.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/scheduler"
	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Wire and topology constants
// -----------------------------------------------------------------------------

// RawMessageType is the kafka.Message.Type of every record this package writes
// to odds.raw.{provider}.
//
// The envelope's data is the provider's OWN BYTES, passed through unchanged as a
// json.RawMessage — provider.RawPayload.Body, which the adapter is contractually
// forbidden from putting a credential in. The normalizer's Decoder is what gives
// those bytes meaning, and it is selected from the topic's {provider} suffix,
// which is why nothing about the provider's format is encoded in this type name.
//
// Bumping it to v2 means the FRAME changed (the data stopped being the provider's
// verbatim bytes). A provider changing its own payload shape does not touch this.
const RawMessageType = "odds.raw.v1"

// Consumer group identifiers. They are the unit of offset ownership, so they are
// frozen: changing one on a running deployment starts from no committed offsets
// and replays the whole retained log.
const (
	// GroupNormalizer consumes odds.raw.{provider} and publishes odds.normalized.
	GroupNormalizer = "ingest-normalizer"

	// GroupTimescaleWriter consumes odds.normalized and writes line history.
	//
	// It is a SEPARATE group from GroupNormalizer even though both run in this
	// process, because they consume different topics and must hold independent
	// offsets: a writer that fell behind must not be able to stall normalization,
	// and replaying one must not replay the other.
	GroupTimescaleWriter = "ingest-timescale-writer"
)

// Defaults. Each is overridable through Config; zero means the default.
const (
	// DefaultRawRefreshAfter is the ceiling on suppression: an event's payload is
	// republished once this long has passed since its last publication, whether
	// or not this package believes anything moved.
	//
	// It is the self-healing half of the change-detection argument in doc.go. Two
	// minutes is comfortably above the live cadence, so it costs a trickle of bus
	// traffic in the steady state, and comfortably below any window in which a
	// frozen board would go unnoticed.
	DefaultRawRefreshAfter = 2 * time.Minute

	// DefaultFlushTimeout bounds the final producer flush during shutdown.
	//
	// It is below httpx.DefaultShutdownTimeout's sibling budget on purpose: the
	// whole process has to drain inside the orchestrator's grace period, and a
	// flush that outlives it is a SIGKILL with the records still buffered, which
	// is precisely the loss the flush exists to prevent.
	DefaultFlushTimeout = 10 * time.Second
)

// Errors this package returns. Every configuration failure wraps ErrInvalidConfig
// so a caller matches with errors.Is (CLAUDE.md §12).
var (
	// ErrInvalidConfig means the service was asked to start with a configuration
	// or a dependency set it cannot run.
	ErrInvalidConfig = errors.New("ingest: invalid configuration")

	// ErrAdapterMismatch means the adapter that was constructed is not the one
	// the credentials select.
	//
	// It exists because the failure it catches is the dangerous one: a deployment
	// holding a real API key that is quietly serving a simulation. ADR 0003 has
	// no failover path for exactly this reason, so a mismatch is a startup
	// failure rather than a warning.
	ErrAdapterMismatch = errors.New("ingest: selected adapter contradicts the configured credentials")

	// ErrNotRunning is what the readiness checker reports before Run has started
	// the stages and after they have stopped.
	ErrNotRunning = errors.New("ingest: pipeline is not running")
)

// DefaultMarkets is the market set one league sweep asks for.
//
// h2h, spreads and totals — ADR 0003's M = 3, which is what its credit
// arithmetic and scheduler.DefaultCreditsPerSweep are both computed against.
// Player props are deliberately absent: The Odds API serves them only from the
// per-event endpoint, so adding them here would multiply the cost of every sweep
// by the size of the slate rather than by one.
func DefaultMarkets() []domain.MarketType {
	return []domain.MarketType{
		domain.MarketTypeMoneyline,
		domain.MarketTypeSpread,
		domain.MarketTypeTotal,
	}
}

// costProbeLeague is the league identifier handed to Adapter.Cost at startup to
// learn what one sweep costs.
//
// Adapter.Cost is documented to perform no I/O and read no clock, and both
// adapters bill on the market set — The Odds API on markets × regions, the
// synthetic generator on len(scope.Markets) — so which league is named does not
// change the answer. It still has to be a syntactically valid identifier,
// because Scope.Validate refuses an empty one and an adapter is entitled to
// validate what it is handed. Nothing is fetched with it and it never reaches a
// topic, a row or a metric label.
const costProbeLeague = domain.LeagueID("sweep-cost-probe")

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

// Config is the ingest service's typed configuration.
//
// It is built by [LoadConfig] from internal/platform/config's output plus the
// adapter that was selected. This package never reads the process environment
// itself: CLAUDE.md §12 puts configuration loading in one place and injects the
// result, and a second loader here would be a second place for a deployment to
// disagree with itself.
type Config struct {
	// Provider is the selected adapter's name. It is the {provider} in
	// odds.raw.{provider} and the `provider` label on every ingest series.
	Provider provider.Name

	// Simulated reports whether the prices this process will publish are
	// computed by the synthetic generator rather than observed from a real
	// bookmaker. It is on the service logger and in the info metric; see doc.go.
	Simulated bool

	// OddsAPIKeySet records whether ODDS_API_KEY was present, so that
	// [New] can refuse the one combination that must never run silently: a real
	// key configured and a simulated adapter selected.
	OddsAPIKeySet bool

	// Markets is the market set one sweep asks for. Empty means DefaultMarkets.
	Markets []domain.MarketType

	// Scheduler is the cadence ladder, the quota budget and the timeouts.
	Scheduler scheduler.Config

	// RawRefreshAfter is the suppression ceiling. Zero means
	// DefaultRawRefreshAfter; negative is rejected.
	RawRefreshAfter time.Duration

	// FlushTimeout bounds the shutdown flush. Zero means DefaultFlushTimeout.
	FlushTimeout time.Duration

	// Now is the clock. Zero means time.Now. Injected rather than read globally
	// so the refresh ceiling is testable without sleeping (CLAUDE.md §12).
	Now func() time.Time
}

// LoadConfig turns the process configuration and the selected adapter into this
// package's typed configuration.
//
// The adapter is a parameter rather than a name because two fields can only come
// from it: Provider, which must be the adapter's own answer rather than an
// inference from the key, and Scheduler.CreditsPerSweep, which scheduler.Config
// documents as "internal/ingest.LoadConfig sets it from the selected adapter"
// precisely so that a caller cannot leave the real provider polling for free
// against a budget that never drains.
func LoadConfig(base *config.Config, a provider.Adapter) (Config, error) {
	if base == nil {
		return Config{}, fmt.Errorf("%w: no process configuration", ErrInvalidConfig)
	}
	if a == nil {
		return Config{}, fmt.Errorf("%w: no provider adapter", ErrInvalidConfig)
	}

	name := a.Name()
	if _, err := provider.NewName(name.String()); err != nil {
		return Config{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	markets := DefaultMarkets()
	sched := scheduler.DefaultConfig(name.String())
	sched.Seed = base.SyntheticSeed
	sched.CreditsPerSweep = sweepCost(a, markets)

	// The live tier is the only cadence the environment can move, and it moves
	// only the INTERVAL, never the ceiling the backoff walks up to. Raising the
	// floor above MaxInterval would invert the pair and make "backed off" mean
	// "polled sooner", so MaxInterval is carried up with it when it would
	// otherwise be crossed.
	//
	// ADR 0003's arithmetic assumes 90s. A deployment that changes this is
	// changing its own bill, which is why config.Load refuses a non-positive
	// value rather than clamping one.
	if d := base.IngestLiveInterval; d > 0 {
		sched.Tiers.Live.Interval = d
		if sched.Tiers.Live.MaxInterval < d {
			sched.Tiers.Live.MaxInterval = d
		}
	}

	return Config{
		Provider:        name,
		Simulated:       name == provider.NameSynthetic,
		OddsAPIKeySet:   base.HasOddsAPIKey(),
		Markets:         markets,
		Scheduler:       sched,
		RawRefreshAfter: DefaultRawRefreshAfter,
		FlushTimeout:    DefaultFlushTimeout,
	}, nil
}

// sweepCost asks the adapter what one league sweep costs, in provider credits.
// See costProbeLeague for why the scope names a league that is never fetched.
func sweepCost(a provider.Adapter, markets []domain.MarketType) int {
	cost := a.Cost(provider.Scope{League: costProbeLeague, Markets: markets})
	if cost < 0 {
		return 0
	}
	return cost
}

// withDefaults resolves every zero-valued field.
func (c Config) withDefaults() Config {
	if len(c.Markets) == 0 {
		c.Markets = DefaultMarkets()
	}
	if c.RawRefreshAfter == 0 {
		c.RawRefreshAfter = DefaultRawRefreshAfter
	}
	if c.FlushTimeout == 0 {
		c.FlushTimeout = DefaultFlushTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Scheduler.Provider == "" {
		c.Scheduler.Provider = c.Provider.String()
	}
	return c
}

// Validate reports whether the configuration describes a service that can run.
//
// It deliberately does NOT validate Config.Scheduler: scheduler.New resolves its
// own defaults and then validates, and validating a half-resolved copy here
// would reject configurations the scheduler is perfectly willing to run.
func (c Config) Validate() error {
	if c.Provider.IsZero() {
		return fmt.Errorf("%w: Provider is empty (it is the odds.raw topic suffix and a metric label)",
			ErrInvalidConfig)
	}
	if _, err := provider.NewName(c.Provider.String()); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if len(c.Markets) == 0 {
		return fmt.Errorf("%w: Markets is empty; a sweep must ask for something", ErrInvalidConfig)
	}
	seen := make(map[domain.MarketType]bool, len(c.Markets))
	for _, m := range c.Markets {
		if !m.Valid() {
			return fmt.Errorf("%w: market type %d is not defined", ErrInvalidConfig, uint8(m))
		}
		if seen[m] {
			return fmt.Errorf("%w: market type %s requested twice", ErrInvalidConfig, m)
		}
		seen[m] = true
	}
	if c.RawRefreshAfter < 0 {
		return fmt.Errorf("%w: RawRefreshAfter must not be negative, got %s", ErrInvalidConfig, c.RawRefreshAfter)
	}
	if c.FlushTimeout <= 0 {
		return fmt.Errorf("%w: FlushTimeout must be positive, got %s", ErrInvalidConfig, c.FlushTimeout)
	}
	// The dangerous combination, refused rather than warned about: see
	// ErrAdapterMismatch and ADR 0003's ban on silent failover.
	if c.OddsAPIKeySet && c.Simulated {
		return fmt.Errorf("%w: %s is set but the %s adapter was selected; there is no failover "+
			"from a real feed to a simulation and a deployment must never be unable to tell which it is serving",
			ErrAdapterMismatch, config.EnvOddsAPIKey, provider.NameSynthetic)
	}
	return nil
}

// LogValue implements slog.LogValuer.
func (c Config) LogValue() slog.Value {
	types := make([]string, 0, len(c.Markets))
	for _, m := range c.Markets {
		types = append(types, m.String())
	}
	return slog.GroupValue(
		slog.String("provider", c.Provider.String()),
		slog.Bool("simulated", c.Simulated),
		slog.Bool("odds_api_key_set", c.OddsAPIKeySet),
		slog.Any("markets", types),
		slog.Int("credits_per_sweep", c.Scheduler.CreditsPerSweep),
		slog.String("raw_refresh_after", c.RawRefreshAfter.String()),
		slog.String("flush_timeout", c.FlushTimeout.String()),
	)
}

// -----------------------------------------------------------------------------
// Consumer-declared seams (CLAUDE.md §12: "Interfaces are declared by the
// consumer, not the producer. Keep them small.")
// -----------------------------------------------------------------------------

// RawPublisher is the slice of *kafka.OddsProducer this package uses.
//
// Two methods, because two is all the composition root needs: one to put a
// provider payload on odds.raw.{provider}, and one to make sure the buffer is
// empty before the process exits. Closing belongs to whoever constructed the
// producer — this package neither owns its lifetime nor should be able to end it
// while another stage is still publishing.
type RawPublisher interface {
	PublishRaw(ctx context.Context, p kafka.Provider, id domain.EventID, msg kafka.Message) error
	Flush(ctx context.Context) error
}

// Consumer is the part of *kafka.Consumer each stage loop drives.
//
// The Consumer owns the poll loop, the commit boundary and the group lifecycle,
// and this package reimplements none of them: it hands over a handler and waits
// for Run to return, which it does only after committing what it has handled.
type Consumer interface {
	Run(ctx context.Context, h kafka.Handler) error
}

// Compile-time proof that the shipped types satisfy the declarations above. They
// are here rather than at the call site because a mismatch should break THIS
// package's build, where the interfaces are declared.
var (
	_ RawPublisher  = (*kafka.OddsProducer)(nil)
	_ Consumer      = (*kafka.Consumer)(nil)
	_ httpx.Checker = (*Service)(nil)
)

// -----------------------------------------------------------------------------
// Metrics
// -----------------------------------------------------------------------------

// Metric namespace and subsystem, joining the `sharpline_ingest_` family
// internal/ingest/scheduler already exports.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "ingest"
)

// Raw publication outcomes. A closed set; every value is written by exactly one
// branch below and the label never carries error text.
const (
	rawPublished  = "published"
	rawRefreshed  = "refreshed"
	rawSuppressed = "suppressed"
	rawNoPayload  = "no_payload"
	rawFailed     = "failed"
)

// Metrics is this package's own instrumentation.
//
// It is deliberately small. The three families that matter here are already
// owned elsewhere and are NOT re-emitted: internal/ingest/provider owns
// sharpline_provider_* and the stage="received" slice of
// sharpline_odds_staleness_seconds, internal/ingest/scheduler owns
// sharpline_ingest_polls_total and sharpline_ingest_poll_interval_seconds, and
// internal/platform/kafka owns the produce and consumer-lag series. Registering
// any of them twice would fail the process at startup, which is the correct
// outcome and is why this package does not try.
type Metrics struct {
	providerInfo *prometheus.GaugeVec // provider, mode, simulated
	rawRecords   *prometheus.CounterVec
}

// NewMetrics builds the collectors and registers them with reg. A nil reg builds
// them unregistered, which is correct for a unit test and for a process with no
// /metrics endpoint — the observe calls stay live so no call site needs a nil
// check. This mirrors internal/platform/kafka's NewMetrics exactly.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		providerInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "provider_info",
			Help: "Which odds source this ingest process is running, as label values on a constant 1. " +
				"mode=\"simulated\" means every price it publishes is computed by the synthetic stochastic " +
				"generator rather than observed from a bookmaker, and any +EV, arbitrage or CLV signal " +
				"derived from it is a statement about a random number generator. " +
				"An info metric rather than a value: `... * on(provider) group_left(mode) " +
				"sharpline_ingest_provider_info` labels any other ingest series with what produced it.",
		}, []string{"provider", "mode", "simulated"}),

		rawRecords: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "raw_records_total",
			Help: "Provider event payloads seen by the raw publisher, by what happened to them. " +
				"outcome=\"suppressed\" is change detection working (CLAUDE.md §5: most polls return " +
				"identical data and must not generate bus traffic) — a healthy steady state is mostly " +
				"suppressed. outcome=\"refreshed\" is the suppression ceiling firing, which bounds how long " +
				"a change-detection defect can hide a real move. outcome=\"no_payload\" means the adapter " +
				"returned an event with no raw bytes, so nothing replayable was recorded for it.",
		}, []string{"provider", "outcome"}),
	}
	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{m.providerInfo, m.rawRecords} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("ingest metrics: %w", err)
		}
	}
	return m, nil
}

// publishProviderInfo sets the constant-1 identity series.
func (m *Metrics) publishProviderInfo(name provider.Name, simulated bool) {
	if m == nil {
		return
	}
	m.providerInfo.WithLabelValues(name.String(), modeOf(simulated), strconv.FormatBool(simulated)).Set(1)
}

func (m *Metrics) observeRaw(name provider.Name, outcome string) {
	if m == nil {
		return
	}
	m.rawRecords.WithLabelValues(name.String(), outcome).Inc()
}

// modeOf is the human-facing half of the provider identity.
func modeOf(simulated bool) string {
	if simulated {
		return "simulated"
	}
	return "live"
}

// -----------------------------------------------------------------------------
// Service
// -----------------------------------------------------------------------------

// Options are [New]'s dependencies. Everything is constructor-injected; nothing
// is read from a global (CLAUDE.md §12).
type Options struct {
	// Config is the typed configuration, normally from [LoadConfig].
	Config Config

	// Adapter is the selected odds source. Required.
	Adapter provider.Adapter

	// Producer publishes to odds.raw.{provider}. Required.
	Producer RawPublisher

	// RawConsumer subscribes to odds.raw.{provider} and drives Normalizer.
	// Required.
	RawConsumer Consumer

	// Normalizer maps a raw payload onto the domain, applies change detection
	// and publishes odds.normalized. Required.
	//
	// IT MUST PUBLISH SYNCHRONOUSLY. doc.go's shutdown argument depends on it:
	// the Consumer commits the last successfully handled record per partition, so
	// a handler that returned before its record was acknowledged would let an
	// offset be committed ahead of the record it produced, and the market would
	// be lost on the next restart. kafka.OddsProducer.PublishNormalized waits for
	// the acknowledgement; PublishNormalizedAsync does not.
	Normalizer kafka.Handler

	// NormalizedConsumer subscribes to odds.normalized and drives Writer.
	// Required.
	NormalizedConsumer Consumer

	// Writer turns one normalized market into one committed transaction against
	// the prices hypertable and the catalogue. Required.
	Writer kafka.Handler

	// Logger receives lifecycle and failure events. Required — a pipeline that
	// cannot report which adapter it selected is the failure mode doc.go exists
	// to prevent.
	Logger *slog.Logger

	// Registry receives this package's collectors and, when ProviderMetrics is
	// nil, the provider seam's. Nil builds them unregistered.
	Registry prometheus.Registerer

	// ProviderMetrics is an already-registered provider collector set. Nil builds
	// one against Registry.
	//
	// It is exposed because sharpline_provider_quota_remaining, _quota_limit and
	// sharpline_provider_requests_total are a contract with deploy/observability
	// and must be registered EXACTLY ONCE per process: an adapter that exports
	// its own copies has to be given a different registerer, or this one does.
	ProviderMetrics *provider.Metrics

	// SchedulerMetrics is an already-registered scheduler collector set. Nil
	// builds one against Registry.
	SchedulerMetrics *scheduler.Metrics
}

// Service is the running ingest pipeline. Construct with [New]; run with
// [Service.Run].
type Service struct {
	cfg      Config
	log      *slog.Logger
	producer RawPublisher
	sched    *scheduler.Scheduler
	metrics  *Metrics
	stages   []stage

	// running gates the readiness checker. It is false before Run has started
	// the stages and false again once they have stopped, so /readyz reports the
	// truth during both a cold start and a drain rather than only in between.
	running atomic.Bool
}

// stage is one long-running consumer loop.
type stage struct {
	name     string
	topic    string
	group    string
	consumer Consumer
	handler  kafka.Handler
}

// New validates the options and builds the pipeline. It performs no I/O and
// starts nothing; call [Service.Run] for that.
func New(opts Options) (*Service, error) {
	cfg := opts.Config.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch {
	case opts.Adapter == nil:
		return nil, fmt.Errorf("%w: Adapter is nil", ErrInvalidConfig)
	case opts.Producer == nil:
		return nil, fmt.Errorf("%w: Producer is nil", ErrInvalidConfig)
	case opts.RawConsumer == nil:
		return nil, fmt.Errorf("%w: RawConsumer is nil", ErrInvalidConfig)
	case opts.Normalizer == nil:
		return nil, fmt.Errorf("%w: Normalizer is nil", ErrInvalidConfig)
	case opts.NormalizedConsumer == nil:
		return nil, fmt.Errorf("%w: NormalizedConsumer is nil", ErrInvalidConfig)
	case opts.Writer == nil:
		return nil, fmt.Errorf("%w: Writer is nil", ErrInvalidConfig)
	case opts.Logger == nil:
		return nil, fmt.Errorf("%w: Logger is nil", ErrInvalidConfig)
	}

	// The adapter's own answer, not an inference: a mismatch between what was
	// constructed and what the credentials select is the one failure that must
	// never be survivable, and Validate above has already refused it.
	if got := opts.Adapter.Name(); got != cfg.Provider {
		return nil, fmt.Errorf("%w: adapter reports %q, configuration says %q",
			ErrAdapterMismatch, got, cfg.Provider)
	}

	busProvider, err := kafka.NewProvider(cfg.Provider.String())
	if err != nil {
		// provider.NewName and kafka.NewProvider are one contract with two
		// spellings; a name this package accepted and the bus rejected would
		// publish to a topic that cannot be created.
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	rawTopic, err := kafka.OddsRaw(busProvider)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	m, err := NewMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}
	pm := opts.ProviderMetrics
	if pm == nil {
		if pm, err = provider.NewMetrics(opts.Registry); err != nil {
			return nil, err
		}
	}
	sm := opts.SchedulerMetrics
	if sm == nil {
		if sm, err = scheduler.NewMetrics(opts.Registry); err != nil {
			return nil, err
		}
	}

	// Every line this process writes carries the source and whether it is
	// simulated. One startup banner scrolls out of a log window; a field on every
	// record does not. See doc.go.
	log := opts.Logger.With(
		slog.String("provider", cfg.Provider.String()),
		slog.Bool("simulated", cfg.Simulated),
	)

	p := &poller{
		adapter:      opts.Adapter,
		producer:     opts.Producer,
		busProvider:  busProvider,
		name:         cfg.Provider,
		markets:      cfg.Markets,
		refreshAfter: cfg.RawRefreshAfter,
		now:          cfg.Now,
		log:          log.With(slog.String("component", "ingest.poller")),
		pm:           pm,
		m:            m,
		digests:      make(map[domain.LeagueID]map[domain.MarketID]uint64),
		published:    make(map[domain.LeagueID]map[domain.EventID]time.Time),
		events:       make(map[domain.LeagueID][]domain.Event),
	}

	sched, err := scheduler.New(scheduler.Options{
		Config:    cfg.Scheduler,
		Poller:    p,
		Catalogue: &catalogueSource{adapter: opts.Adapter, poller: p, pm: pm, name: cfg.Provider},
		Logger:    log,
		Metrics:   sm,
	})
	if err != nil {
		return nil, fmt.Errorf("ingest: build scheduler: %w", err)
	}

	return &Service{
		cfg:      cfg,
		log:      log,
		producer: opts.Producer,
		sched:    sched,
		metrics:  m,
		stages: []stage{
			{
				name:     "normalizer",
				topic:    rawTopic.Name(),
				group:    GroupNormalizer,
				consumer: opts.RawConsumer,
				handler:  opts.Normalizer,
			},
			{
				name:     "timescale-writer",
				topic:    kafka.TopicOddsNormalized,
				group:    GroupTimescaleWriter,
				consumer: opts.NormalizedConsumer,
				handler:  opts.Writer,
			},
		},
	}, nil
}

// Scheduler exposes the polling scheduler so an operational endpoint or a test
// can read its state without reaching into the service.
func (s *Service) Scheduler() *scheduler.Scheduler { return s.sched }

// Name implements httpx.Checker. It is the key this dependency appears under in
// the /readyz payload.
func (s *Service) Name() string { return "ingest" }

// Check implements httpx.Checker: the pipeline is ready when its stages are
// running.
//
// It answers a question the Postgres and Kafka checkers do not: those report
// that the dependencies are reachable, this reports that this process is
// actually consuming and polling. A replica whose stages have exited but whose
// listener is still up would otherwise look healthy while producing nothing.
//
// It deliberately does NOT consult the provider. A provider outage must not take
// the pod out of rotation — the scheduler is built to survive one, and the
// visible symptom belongs on the quota and staleness alerts rather than on a
// restart loop.
func (s *Service) Check(context.Context) error {
	if !s.running.Load() {
		return ErrNotRunning
	}
	return nil
}

// Run drives the pipeline until ctx is cancelled.
//
// Shutdown order is doc.go's, and each step is load-bearing:
//
//  1. ctx cancellation stops every league goroutine scheduling a new sweep;
//  2. scheduler.Run waits for in-flight sweeps to drain, and each consumer's Run
//     waits for the record in its handler and then commits;
//  3. only once all three have returned is the producer flushed, because a flush
//     that races a live stage flushes a buffer that is still being filled.
func (s *Service) Run(ctx context.Context) error {
	s.announce()

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

	s.running.Store(true)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.sched.Run(ctx); err != nil {
			fail(fmt.Errorf("ingest: scheduler: %w", err))
		}
	}()

	for _, st := range s.stages {
		wg.Add(1)
		go func(st stage) {
			defer wg.Done()
			s.log.Info("stage running",
				slog.String("stage", st.name),
				slog.String("topic", st.topic),
				slog.String("group", st.group),
			)
			err := st.consumer.Run(ctx, st.handler)
			s.log.Info("stage stopped", slog.String("stage", st.name))
			if err != nil && !errors.Is(err, context.Canceled) {
				fail(fmt.Errorf("ingest: stage %s: %w", st.name, err))
			}
		}(st)
	}

	wg.Wait()
	s.running.Store(false)

	// The producer is flushed on a context DETACHED from ctx, because by the time
	// this runs ctx is the cancelled one that stopped the stages. Flushing on it
	// would return instantly with the buffer intact, which is exactly the
	// accepted-but-unwritten loss the flush exists to prevent.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.FlushTimeout)
	defer cancel()
	if err := s.producer.Flush(flushCtx); err != nil {
		s.log.Error("final producer flush failed; records accepted but not written are lost",
			slog.String("error", err.Error()))
		fail(fmt.Errorf("ingest: final flush: %w", err))
	} else {
		s.log.Info("producer flushed; every accepted record is written")
	}

	return errors.Join(errs...)
}

// announce states the odds source, unmissably. See doc.go.
func (s *Service) announce() {
	s.metrics.publishProviderInfo(s.cfg.Provider, s.cfg.Simulated)

	attrs := []slog.Attr{
		slog.String("adapter", s.cfg.Provider.String()),
		slog.String("mode", modeOf(s.cfg.Simulated)),
		slog.Bool("simulated", s.cfg.Simulated),
		slog.Bool("odds_api_key_set", s.cfg.OddsAPIKeySet),
		slog.Any("config", s.cfg),
	}

	if s.cfg.Simulated {
		// WARN, not INFO. Every price this process is about to publish is
		// computed by a random number generator, and a reader skimming levels
		// must not have to notice an INFO line to learn that.
		s.log.LogAttrs(context.Background(), slog.LevelWarn,
			"ODDS SOURCE IS SIMULATED: prices are computed by the synthetic stochastic generator, "+
				"not observed from any bookmaker; no output of this process is a claim about a real market",
			attrs...)
		return
	}
	s.log.LogAttrs(context.Background(), slog.LevelInfo,
		"odds source is the live provider feed", attrs...)
}

// -----------------------------------------------------------------------------
// Catalogue: adapter → scheduler.Catalogue
// -----------------------------------------------------------------------------

// catalogueSource answers "which leagues, and what is known about their events".
//
// The two halves come from different places on purpose. The LEAGUES come from
// provider.Adapter.Catalogue, which is free at the real provider (ADR 0003:
// "/events and /sports are free") and can therefore be re-read on the
// scheduler's aggressive refresh cadence without spending a credit. The EVENTS
// come from the last sweep the poller performed, because provider.Catalogue
// carries sports, leagues and books and deliberately not fixtures — the league
// list and the event list are different endpoints and the adapter interface only
// wraps the first.
//
// A league with no remembered events is therefore normal on a cold start, and
// scheduler.Catalogue documents that state as supported: it is scheduled at the
// discovery cadence, and a league's first sweep fires immediately whatever its
// window, so the fixtures are learned on the next tick rather than an hour later.
type catalogueSource struct {
	adapter provider.Adapter
	poller  *poller
	pm      *provider.Metrics
	name    provider.Name
}

// Schedule implements scheduler.Catalogue.
func (c *catalogueSource) Schedule(ctx context.Context) ([]scheduler.LeaguePlan, error) {
	cat, err := c.adapter.Catalogue(ctx)
	c.pm.ObserveRequest(c.name, err)
	if err != nil {
		return nil, fmt.Errorf("ingest: catalogue from %s: %w", c.name, err)
	}
	// Validated rather than trusted: this is the first point at which a whole
	// book set exists, and Catalogue.Validate is where the "at most one sharp
	// reference book" rule is checked. A malformed catalogue that reached the
	// schedule would present later as leagues that never poll.
	if err := cat.Validate(); err != nil {
		return nil, fmt.Errorf("ingest: catalogue from %s: %w", c.name, err)
	}

	plans := make([]scheduler.LeaguePlan, 0, len(cat.Leagues))
	for _, l := range cat.Leagues {
		plans = append(plans, scheduler.LeaguePlan{
			League: l.ID(),
			Events: c.poller.knownEvents(l.ID()),
		})
	}
	return plans, nil
}

// -----------------------------------------------------------------------------
// Poller: adapter → scheduler.Poller, and the raw publication
// -----------------------------------------------------------------------------

// poller performs one league sweep: fetch, instrument, detect change, publish.
//
// It is called concurrently for different leagues, up to
// scheduler.Config.MaxConcurrentPolls, and one league is only ever swept by one
// goroutine at a time. The mutex therefore guards cross-league access — the
// catalogue reading an event list, a snapshot for an operator — rather than a
// race between two sweeps of the same league.
type poller struct {
	adapter     provider.Adapter
	producer    RawPublisher
	busProvider kafka.Provider
	name        provider.Name

	markets      []domain.MarketType
	refreshAfter time.Duration
	now          func() time.Time

	log *slog.Logger
	pm  *provider.Metrics
	m   *Metrics

	mu sync.Mutex
	// digests is the in-memory, non-authoritative change-detection state: one
	// content hash per market, replaced wholesale per league per sweep so a
	// market that vanishes from the payload is forgotten rather than leaked.
	digests map[domain.LeagueID]map[domain.MarketID]uint64
	// published is when each event's raw payload last reached the bus, which is
	// what the suppression ceiling is measured against.
	published map[domain.LeagueID]map[domain.EventID]time.Time
	// events is the fixture list the catalogue hands back to the scheduler.
	events map[domain.LeagueID][]domain.Event
}

var _ scheduler.Poller = (*poller)(nil)

// knownEvents returns the events most recently observed for a league.
func (p *poller) knownEvents(league domain.LeagueID) []domain.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]domain.Event(nil), p.events[league]...)
}

// Poll implements scheduler.Poller.
func (p *poller) Poll(ctx context.Context, req scheduler.PollRequest) (scheduler.PollResult, error) {
	scope := provider.Scope{League: req.League, Markets: p.markets}
	if err := scope.Validate(); err != nil {
		// Not retryable and not the provider's fault: the schedule handed over a
		// league the adapter could never be asked about.
		return scheduler.PollResult{}.WithoutQuota(), fmt.Errorf("ingest: %w", err)
	}

	snap, err := p.adapter.Fetch(ctx, scope)

	// Instrumented before the error is handled, because a failed request is a
	// request: it counts against the outcome breakdown and it may well have
	// changed the quota reading (a 429 and a quota exhaustion both do).
	p.pm.ObserveRequest(p.name, err)
	quota := p.adapter.Quota()
	p.pm.ObserveQuota(p.name, quota)
	res := quotaResult(quota)

	if err != nil {
		return res, fmt.Errorf("ingest: fetch %s: %w", req.League, err)
	}
	// The adapter's own output is validated rather than trusted. Snapshot.Validate
	// catches the three failures that are otherwise silent and produce a plausible
	// wrong number: a selection role its market type does not admit, a price whose
	// line has drifted from its selection's, and a spread's away price quoted at
	// the home line.
	if err := snap.Validate(); err != nil {
		return res, fmt.Errorf("ingest: snapshot from %s: %w", p.name, err)
	}
	p.pm.ObserveSnapshot(snap)

	return p.consume(ctx, req, snap, res)
}

// consume applies change detection to one snapshot and publishes what moved.
func (p *poller) consume(
	ctx context.Context, req scheduler.PollRequest, snap provider.Snapshot, res scheduler.PollResult,
) (scheduler.PollResult, error) {
	now := p.now()

	p.mu.Lock()
	prev := p.digests[req.League]
	lastPub := p.published[req.League]
	p.mu.Unlock()

	next := make(map[domain.MarketID]uint64, snap.MarketCount())
	pub := make(map[domain.EventID]time.Time, len(snap.Events))
	events := make([]domain.Event, 0, len(snap.Events))

	var failure error
	for _, ev := range snap.Events {
		events = append(events, ev.Event)
		res.Markets += len(ev.Markets)

		id := ev.Event.ID()
		moved := false
		digests := make(map[domain.MarketID]uint64, len(ev.Markets))
		for _, mk := range ev.Markets {
			d := digestMarket(mk)
			digests[mk.Market.ID()] = d
			if old, seen := prev[mk.Market.ID()]; !seen || old != d {
				res.Changed++
				moved = true
			}
		}

		last, everPublished := lastPub[id]
		stale := p.refreshAfter > 0 && everPublished && now.Sub(last) >= p.refreshAfter
		switch {
		case moved:
			failure = p.publish(ctx, ev, rawPublished)
		case !everPublished:
			// Never published: a market whose digest happens to match a restart's
			// cold cache would otherwise never reach the bus at all.
			failure = p.publish(ctx, ev, rawPublished)
		case stale:
			failure = p.publish(ctx, ev, rawRefreshed)
		default:
			p.m.observeRaw(p.name, rawSuppressed)
		}
		if failure != nil {
			// Leave this event's markets OUT of next, so the following sweep sees
			// them as changed and republishes. Over-publishing after a bus failure
			// is the safe direction; recording a digest for a payload that never
			// reached the topic is not.
			break
		}

		for mid, d := range digests {
			next[mid] = d
		}
		if moved || !everPublished || stale {
			pub[id] = now
		} else {
			pub[id] = last
		}
	}

	p.mu.Lock()
	p.digests[req.League] = next
	p.published[req.League] = pub
	p.events[req.League] = events
	p.mu.Unlock()

	if failure != nil {
		return res, failure
	}
	return res, nil
}

// publish puts one event's provider payload on odds.raw.{provider}.
//
// The payload is the provider's own bytes, unmodified: the raw topic is the
// replayable record of what the provider actually said, which is what a golden
// file is recorded from and the only artefact that survives a parsing bug
// downstream.
func (p *poller) publish(ctx context.Context, ev provider.EventSnapshot, outcome string) error {
	raw := ev.Raw
	if raw.IsZero() {
		// Not an error: an adapter may legitimately have no bytes for an event
		// it synthesised the markets of. It is counted and logged because the
		// consequence is real — nothing replayable was recorded — and because for
		// the real provider it would be a defect.
		p.m.observeRaw(p.name, rawNoPayload)
		p.log.Warn("provider returned an event with no raw payload; nothing replayable was recorded",
			slog.String("event", ev.Event.ID().String()),
			slog.String("league", ev.Event.LeagueID().String()),
		)
		return nil
	}
	if ct := raw.ContentType; ct != "" && !strings.HasPrefix(ct, "application/json") {
		p.m.observeRaw(p.name, rawFailed)
		return fmt.Errorf("ingest: event %s: raw payload content type %q is not JSON; the envelope "+
			"carries it as the message data and cannot frame anything else", ev.Event.ID(), ct)
	}

	// ObservedAt is the PROVIDER's instant, never the moment we received the
	// bytes. The envelope propagates it, and it is the subtrahend in every
	// staleness measurement downstream.
	msg := kafka.Message{
		Type:       RawMessageType,
		ID:         ev.Event.ID().String(),
		ObservedAt: raw.ObservedAt,
		Payload:    json.RawMessage(raw.Body),
	}
	if err := p.producer.PublishRaw(ctx, p.busProvider, ev.Event.ID(), msg); err != nil {
		p.m.observeRaw(p.name, rawFailed)
		return fmt.Errorf("ingest: publish raw payload for event %s: %w", ev.Event.ID(), err)
	}
	p.m.observeRaw(p.name, outcome)
	return nil
}

// quotaResult seeds a PollResult with the provider's own remaining-credit count.
//
// A reading the adapter does not have is reported as -1, which scheduler's
// PollResult documents as "the provider did not say". Reporting 0 would read as
// exhausted and would freeze the board.
func quotaResult(q provider.Quota) scheduler.PollResult {
	if !q.Known {
		return scheduler.PollResult{}.WithoutQuota()
	}
	remaining := q.Remaining
	switch {
	case remaining < 0:
		remaining = 0
	case remaining > math.MaxInt32:
		remaining = math.MaxInt32
	}
	limit := q.Limit
	if limit > math.MaxInt32 {
		limit = math.MaxInt32
	}
	if limit < 0 {
		limit = 0
	}
	return scheduler.PollResult{QuotaRemaining: int(remaining), QuotaLimit: int(limit)}
}

// -----------------------------------------------------------------------------
// Content hashing
// -----------------------------------------------------------------------------

// fieldSep separates hashed fields so that concatenation cannot be ambiguous —
// ("ab", "c") and ("a", "bc") must not produce the same digest.
const fieldSep = 0x1f

// digestMarket hashes everything about one market that a consumer would call a
// change, and nothing that merely marks the passage of time.
//
// # What is excluded, and why the exclusions are the whole design
//
// Market.UpdatedAt and every Price.ObservedAt are deliberately absent. The Odds
// API's last_update advances on every refresh whether or not the price moved, so
// a digest that covered it would differ on every poll, suppression would never
// fire, and the bus would carry thousands of no-op records a minute — the exact
// failure CLAUDE.md §5's "most polls return identical data and must not generate
// bus traffic" names.
//
// Everything else IS covered: the market's status and line, each selection's
// role and name, and each quote's book, decimal price and line. Leaving any of
// them out would suppress a real move, and the board would go stale while the
// market ran.
//
// # This digest is not the fingerprint
//
// internal/ingest/normalizer computes the authoritative one, over the PUBLISHED
// RECORD, carried on the wire and warmed from the compacted topic across
// restarts. This one is in-memory only, is never persisted or transmitted, and
// exists to keep an unchanged payload off odds.raw.{provider}. The refresh
// ceiling bounds how long the two can disagree.
func digestMarket(m provider.MarketSnapshot) uint64 {
	h := fnv.New64a()
	w := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{fieldSep})
	}

	w(m.Market.ID().String())
	w(m.Market.EventID().String())
	w(m.Market.Type().String())
	w(m.Market.Status().String())
	w(lineToken(m.Market.Line()))
	w(m.Market.Subject())

	sels := append([]domain.Selection(nil), m.Selections...)
	sort.Slice(sels, func(i, j int) bool { return sels[i].ID() < sels[j].ID() })
	for _, s := range sels {
		w(s.ID().String())
		w(s.Role().String())
		w(s.Name())
	}

	// Sorted because a provider's ordering is not a contract, and a digest that
	// changed when two books swapped position would report a move that never
	// happened.
	prices := append([]domain.Price(nil), m.Prices...)
	sort.Slice(prices, func(i, j int) bool {
		if prices[i].SelectionID() != prices[j].SelectionID() {
			return prices[i].SelectionID() < prices[j].SelectionID()
		}
		return prices[i].BookID() < prices[j].BookID()
	})
	for _, pr := range prices {
		w(pr.SelectionID().String())
		w(pr.BookID().String())
		w(strconv.FormatFloat(pr.Decimal(), 'g', -1, 64))
		w(lineToken(pr.Line()))
	}

	return h.Sum64()
}

// lineToken renders a Line so that absent and 0.0 hash differently.
//
// A pick'em is a real, frequently traded line and "no line at all" is a
// different fact — the same distinction domain.Line exists to preserve and that
// JSON would otherwise flatten.
func lineToken(l domain.Line) string {
	v, ok := l.Value()
	if !ok {
		return "-"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
