// Package config loads and validates Sharpline service configuration from the
// process environment.
//
// CLAUDE.md §12: "Config via environment variables with a typed struct and
// startup validation — fail fast and loudly on a bad config." Loading therefore
// reports *every* problem it finds in one error rather than stopping at the
// first, so an operator fixes a broken deployment in one pass instead of six.
//
// There is exactly one Config type. Which fields a given binary actually
// requires differs (migrate needs a database and no HTTP listener; stream needs
// Kafka and Redis and no database), so each cmd/ entrypoint passes the Spec
// that describes its own requirements. Everything else is optional and carries
// a documented default.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/platform/logging"
)

// Sentinel errors for configuration failures. CLAUDE.md §12 places domain
// sentinels in the domain package; these are platform-level, not domain-level,
// and are matched with errors.Is by callers and tests.
var (
	// ErrMissing means a variable this binary requires was unset or empty.
	ErrMissing = errors.New("config: required environment variable is not set")
	// ErrInvalid means a variable was set but could not be parsed or failed a
	// validity check.
	ErrInvalid = errors.New("config: invalid environment variable value")
)

// Frozen environment variable names. These are contract, shared with the
// compose stack and the Helm chart; renaming one is a breaking change.
const (
	EnvEnv           = "SHARPLINE_ENV"
	EnvLogLevel      = "SHARPLINE_LOG_LEVEL"
	EnvHTTPAddr      = "SHARPLINE_HTTP_ADDR"
	EnvPostgresDSN   = "SHARPLINE_POSTGRES_DSN"
	EnvRedisAddr     = "SHARPLINE_REDIS_ADDR"
	EnvRedisPassword = "SHARPLINE_REDIS_PASSWORD"
	EnvKafkaBrokers  = "SHARPLINE_KAFKA_BROKERS"
	EnvOTELEndpoint  = "SHARPLINE_OTEL_ENDPOINT"
	EnvJWTSigningKey = "SHARPLINE_JWT_SIGNING_KEY"
	EnvOddsAPIKey    = "ODDS_API_KEY"
	EnvSyntheticSeed = "SHARPLINE_SYNTHETIC_SEED"

	// EnvPricerReferenceBooks is the ORDERED, comma-separated list of book slugs
	// `pricer` treats as the sharp reference, most preferred first.
	//
	// It exists because sharpness is an OPINION, not a fact any provider
	// reports, and internal/pricing/reference.go is explicit that a binary with
	// the judgement compiled into it is the wrong shape. A ranked list rather
	// than one name is what makes ONE binary correct against both providers:
	// `ingest` picks its adapter from ODDS_API_KEY at startup and nothing tells
	// `pricer` which it chose, so the pricer tries the real sharp book and falls
	// through to the synthetic one when the real book does not quote a market.
	//
	// It is OPTIONAL and empty is legal. The catalogue's own designation
	// (normalizer.BookRef.Reference) outranks this list wherever it exists, so
	// an unset value is not a broken pricer — it is one that relies on the
	// provider's designation alone, and every computed record says which of the
	// two chose its reference book.
	EnvPricerReferenceBooks = "SHARPLINE_PRICER_REFERENCE_BOOKS"

	// EnvIngestLiveInterval overrides the LIVE-window poll cadence.
	//
	// ADR 0003 promises the cadence ladder is "retunable for a different tier
	// without a code change", and the live tier is the one that dominates the
	// bill -- it is ~77% of the default monthly credit spend. Until this
	// existed the promise was unmet: scheduler.DefaultTiers was reachable only
	// by editing Go.
	//
	// It is also the knob that makes change detection MEASURABLE. The
	// suppression a hash buys depends entirely on the ratio between the poll
	// interval and the rate the market actually moves, so a deployment that
	// cannot change the interval cannot observe the trade-off it is making.
	//
	// Only the live tier is exposed. The other four are derived from it in the
	// ADR's arithmetic and exposing all five invites an inconsistent ladder.
	EnvIngestLiveInterval = "SHARPLINE_INGEST_LIVE_INTERVAL"
)

// Deployment environments. SHARPLINE_ENV must be one of these; an unrecognised
// value is a hard failure rather than a silent fallback, because "prod" vs
// "prd" deciding whether debug endpoints are exposed is exactly the class of
// bug fail-fast validation exists to prevent.
const (
	EnvDev     = "dev"
	EnvTest    = "test"
	EnvStaging = "staging"
	EnvProd    = "prod"
)

// defaults applied when the corresponding variable is unset.
const (
	defaultEnv      = EnvDev
	defaultLogLevel = "info"

	// minJWTKeyLen is 32 bytes — the output width of SHA-256, and the floor
	// below which an HMAC signing key is not worth having.
	minJWTKeyLen = 32
)

// Requirement is a bit set naming the dependencies a binary cannot start
// without.
type Requirement uint16

const (
	// RequireHTTP means the binary serves /healthz, /readyz and /metrics and
	// therefore needs a listen address.
	RequireHTTP Requirement = 1 << iota
	// RequirePostgres means the binary opens a Postgres connection pool.
	RequirePostgres
	// RequireRedis means the binary uses Redis for cache, presence, rate
	// limiting or idempotency keys.
	RequireRedis
	// RequireKafka means the binary produces to or consumes from the bus.
	RequireKafka
	// RequireJWT means the binary mints or verifies access tokens.
	RequireJWT
)

// has reports whether every bit in want is set in r.
func (r Requirement) has(want Requirement) bool { return r&want != 0 }

// Spec describes one binary's configuration contract: its name, the port it
// listens on when SHARPLINE_HTTP_ADDR is unset, and what it requires.
type Spec struct {
	// Service is the binary name, stamped on every log line.
	Service string
	// DefaultHTTPAddr is the frozen internal listen address for this service.
	// Empty for run-to-completion jobs.
	DefaultHTTPAddr string
	// Requires names the dependencies whose configuration must be present.
	Requires Requirement
}

// The six binaries. Ports are the frozen internal topology; nothing but the
// proxy publishes a host port (CLAUDE.md §9).
var (
	// API serves REST + OpenAPI: auth, catalog, bet slip, wagers, account.
	API = Spec{
		Service:         "api",
		DefaultHTTPAddr: ":8080",
		Requires:        RequireHTTP | RequirePostgres | RequireRedis | RequireKafka | RequireJWT,
	}
	// Stream is the WebSocket gateway. Subscription state lives in Redis, not
	// in the pod, so the deployment can scale without session affinity.
	Stream = Spec{
		Service:         "stream",
		DefaultHTTPAddr: ":8081",
		Requires:        RequireHTTP | RequireRedis | RequireKafka,
	}
	// Pricer devigs, computes fair value, EV and Kelly, and finds arbitrage and
	// middles.
	//
	// It does NOT declare RequireRedis, and that is a correction rather than an
	// omission. Phase 2's rule is that a declared dependency must be OPENED by
	// the binary ("RequirePostgres in config means the binary MUST open a
	// pool") — api and settle once declared Postgres without opening one and
	// /api/readyz returned 200 with the database stopped, a probe worse than
	// none. The pricer opens no Redis client: its whole state is a fold of a
	// compacted topic and rebuilds from one snapshot read, which is exactly the
	// argument internal/pricing/doc.go makes for not putting it in a cache. The
	// legitimate future use is a SHARED store once replicas exist and a
	// rebalance moves partitions; the declaration comes back with the client,
	// not before it.
	Pricer = Spec{
		Service:         "pricer",
		DefaultHTTPAddr: ":8082",
		Requires:        RequireHTTP | RequireKafka,
	}
	// Ingest polls provider adapters and publishes normalized deltas. Redis
	// backs the distributed rate limiter that protects the provider quota.
	//
	// Postgres is required because ingest also HOSTS THE TIMESCALE LINE-HISTORY
	// WRITER (CLAUDE.md §3's event flow: odds.normalized → timescale writer).
	// That consumer opens a pool and writes the prices hypertable, so a binary
	// that started without a DSN would report itself healthy while silently
	// persisting nothing — the board would look live and the line history it is
	// the whole point of would be empty. Phase 2's handoff flagged this as an
	// open question "resolve when phase 3 builds that writer"; this is the
	// resolution.
	Ingest = Spec{
		Service:         "ingest",
		DefaultHTTPAddr: ":8083",
		Requires:        RequireHTTP | RequirePostgres | RequireRedis | RequireKafka,
	}
	// Settle grades open wagers and writes ledger entries.
	Settle = Spec{
		Service:         "settle",
		DefaultHTTPAddr: ":8084",
		Requires:        RequireHTTP | RequirePostgres | RequireKafka,
	}
	// Migrate is a run-to-completion job: no listener, database only.
	Migrate = Spec{
		Service:  "migrate",
		Requires: RequirePostgres,
	}
)

// Config is the single typed configuration struct shared by all six binaries.
//
// Secrets are held as plain strings but never logged as such: Config implements
// slog.LogValuer and redacts them.
type Config struct {
	// Service is copied from the Spec, not read from the environment.
	Service string

	Env      string
	LogLevel slog.Level

	// HTTPAddr is the address the health/ready/metrics listener binds. Empty
	// for run-to-completion jobs.
	HTTPAddr string

	PostgresDSN   string
	RedisAddr     string
	RedisPassword string
	KafkaBrokers  []string

	// OTELEndpoint is the OTLP collector address. Empty disables export, which
	// is the correct behaviour for unit tests and for a bare `docker run`.
	OTELEndpoint string

	JWTSigningKey string

	// OddsAPIKey selects the ingest provider adapter: the real The Odds API
	// adapter when set, the synthetic stochastic market maker when not.
	OddsAPIKey string

	// SyntheticSeed seeds the synthetic provider's RNG so tests are
	// deterministic. Zero means "seed from the clock".
	SyntheticSeed int64

	// IngestLiveInterval overrides the live-window poll cadence. Zero means the
	// scheduler's own default (90s). See EnvIngestLiveInterval.
	IngestLiveInterval time.Duration

	// PricerReferenceBooks is the ordered sharp-book preference list, already
	// split and trimmed. Nil means "catalogue designation only".
	// See EnvPricerReferenceBooks.
	PricerReferenceBooks []string
}

// IsProd reports whether this process is running in the production environment.
func (c Config) IsProd() bool { return c.Env == EnvProd }

// HasOddsAPIKey reports whether the real provider adapter is configured.
func (c Config) HasOddsAPIKey() bool { return c.OddsAPIKey != "" }

// LogValue implements slog.LogValuer so that logging a whole Config cannot leak
// a credential. Secrets are reported as present/absent, never by value.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("service", c.Service),
		slog.String("env", c.Env),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("http_addr", c.HTTPAddr),
		slog.String("postgres_dsn", redactDSN(c.PostgresDSN)),
		slog.String("redis_addr", c.RedisAddr),
		slog.Bool("redis_password_set", c.RedisPassword != ""),
		slog.Any("kafka_brokers", c.KafkaBrokers),
		slog.String("otel_endpoint", c.OTELEndpoint),
		slog.Bool("jwt_signing_key_set", c.JWTSigningKey != ""),
		slog.Bool("odds_api_key_set", c.OddsAPIKey != ""),
		slog.Int64("synthetic_seed", c.SyntheticSeed),
		slog.String("ingest_live_interval", c.IngestLiveInterval.String()),
		slog.Any("pricer_reference_books", c.PricerReferenceBooks),
	)
}

// Lookup is the consumer-declared seam over the process environment. It has the
// shape of os.LookupEnv, which lets tests supply a map without touching global
// process state.
type Lookup func(key string) (value string, ok bool)

// MapLookup adapts a map to Lookup, for tests and for callers that assemble an
// environment programmatically.
func MapLookup(env map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// Load reads and validates configuration for spec from the process environment.
func Load(spec Spec) (*Config, error) {
	return LoadFrom(spec, os.LookupEnv)
}

// LoadFrom reads and validates configuration for spec from lookup.
//
// Every validation failure is collected and returned together via errors.Join,
// so a misconfigured deployment surfaces all of its problems on the first boot
// attempt. Each joined error wraps ErrMissing or ErrInvalid.
func LoadFrom(spec Spec, lookup Lookup) (*Config, error) {
	if lookup == nil {
		return nil, fmt.Errorf("%w: nil environment lookup", ErrInvalid)
	}
	if spec.Service == "" {
		return nil, fmt.Errorf("%w: spec has no service name", ErrInvalid)
	}

	cfg := &Config{Service: spec.Service}
	var problems []error

	// SHARPLINE_ENV — defaulted, but a value outside the known set is fatal.
	cfg.Env = strings.ToLower(strings.TrimSpace(get(lookup, EnvEnv, defaultEnv)))
	switch cfg.Env {
	case EnvDev, EnvTest, EnvStaging, EnvProd:
	default:
		problems = append(problems, fmt.Errorf("%w: %s=%q (want one of %s, %s, %s, %s)",
			ErrInvalid, EnvEnv, cfg.Env, EnvDev, EnvTest, EnvStaging, EnvProd))
	}

	// SHARPLINE_LOG_LEVEL — defaulted to info.
	level, err := logging.ParseLevel(get(lookup, EnvLogLevel, defaultLogLevel))
	if err != nil {
		problems = append(problems, fmt.Errorf("%w: %s: %s", ErrInvalid, EnvLogLevel, err))
	}
	cfg.LogLevel = level

	// SHARPLINE_HTTP_ADDR — defaulted to this service's frozen internal port.
	if spec.Requires.has(RequireHTTP) {
		cfg.HTTPAddr = get(lookup, EnvHTTPAddr, spec.DefaultHTTPAddr)
		if cfg.HTTPAddr == "" {
			problems = append(problems, fmt.Errorf("%w: %s", ErrMissing, EnvHTTPAddr))
		} else if err := validateListenAddr(cfg.HTTPAddr); err != nil {
			problems = append(problems, fmt.Errorf("%w: %s=%q: %s", ErrInvalid, EnvHTTPAddr, cfg.HTTPAddr, err))
		}
	}

	// SHARPLINE_POSTGRES_DSN
	if spec.Requires.has(RequirePostgres) {
		cfg.PostgresDSN = strings.TrimSpace(get(lookup, EnvPostgresDSN, ""))
		if cfg.PostgresDSN == "" {
			problems = append(problems, fmt.Errorf("%w: %s", ErrMissing, EnvPostgresDSN))
		} else if err := validatePostgresDSN(cfg.PostgresDSN); err != nil {
			problems = append(problems, fmt.Errorf("%w: %s: %s", ErrInvalid, EnvPostgresDSN, err))
		}
	}

	// SHARPLINE_REDIS_ADDR / SHARPLINE_REDIS_PASSWORD. The password is optional
	// even when Redis is required: a passwordless Redis is valid in a test
	// container even though the shipped compose and Helm configs set one.
	if spec.Requires.has(RequireRedis) {
		cfg.RedisAddr = strings.TrimSpace(get(lookup, EnvRedisAddr, ""))
		if cfg.RedisAddr == "" {
			problems = append(problems, fmt.Errorf("%w: %s", ErrMissing, EnvRedisAddr))
		} else if err := validateHostPort(cfg.RedisAddr); err != nil {
			problems = append(problems, fmt.Errorf("%w: %s=%q: %s", ErrInvalid, EnvRedisAddr, cfg.RedisAddr, err))
		}
		cfg.RedisPassword = get(lookup, EnvRedisPassword, "")
	}

	// SHARPLINE_KAFKA_BROKERS — comma separated host:port list.
	if spec.Requires.has(RequireKafka) {
		raw := strings.TrimSpace(get(lookup, EnvKafkaBrokers, ""))
		if raw == "" {
			problems = append(problems, fmt.Errorf("%w: %s", ErrMissing, EnvKafkaBrokers))
		} else {
			brokers, err := parseBrokers(raw)
			if err != nil {
				problems = append(problems, fmt.Errorf("%w: %s=%q: %s", ErrInvalid, EnvKafkaBrokers, raw, err))
			}
			cfg.KafkaBrokers = brokers
		}
	}

	// SHARPLINE_JWT_SIGNING_KEY
	if spec.Requires.has(RequireJWT) {
		cfg.JWTSigningKey = get(lookup, EnvJWTSigningKey, "")
		switch {
		case cfg.JWTSigningKey == "":
			problems = append(problems, fmt.Errorf("%w: %s", ErrMissing, EnvJWTSigningKey))
		case len(cfg.JWTSigningKey) < minJWTKeyLen:
			problems = append(problems, fmt.Errorf("%w: %s is %d bytes, want at least %d",
				ErrInvalid, EnvJWTSigningKey, len(cfg.JWTSigningKey), minJWTKeyLen))
		}
	}

	// SHARPLINE_OTEL_ENDPOINT — always optional; empty disables export.
	cfg.OTELEndpoint = strings.TrimSpace(get(lookup, EnvOTELEndpoint, ""))
	if cfg.OTELEndpoint != "" {
		if err := validateOTELEndpoint(cfg.OTELEndpoint); err != nil {
			problems = append(problems, fmt.Errorf("%w: %s=%q: %s", ErrInvalid, EnvOTELEndpoint, cfg.OTELEndpoint, err))
		}
	}

	// ODDS_API_KEY — always optional. Absent selects the synthetic provider.
	cfg.OddsAPIKey = strings.TrimSpace(get(lookup, EnvOddsAPIKey, ""))

	// SHARPLINE_SYNTHETIC_SEED — always optional, but must parse if present.
	if raw, ok := lookup(EnvSyntheticSeed); ok && strings.TrimSpace(raw) != "" {
		seed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			problems = append(problems, fmt.Errorf("%w: %s=%q: not a base-10 int64", ErrInvalid, EnvSyntheticSeed, raw))
		}
		cfg.SyntheticSeed = seed
	}

	// SHARPLINE_PRICER_REFERENCE_BOOKS — always optional. Empty entries are
	// dropped rather than rejected so a trailing comma is not a startup failure;
	// the slugs themselves are validated by pricing.NewEngine, which owns the
	// charset rule, rather than being validated twice in two places.
	if raw, ok := lookup(EnvPricerReferenceBooks); ok {
		for _, part := range strings.Split(raw, ",") {
			if slug := strings.TrimSpace(part); slug != "" {
				cfg.PricerReferenceBooks = append(cfg.PricerReferenceBooks, slug)
			}
		}
	}

	// SHARPLINE_INGEST_LIVE_INTERVAL — always optional, but must parse and be
	// positive if present. A zero or negative interval is a hot loop against a
	// metered API, so it is refused rather than clamped: a silently-corrected
	// cadence is a bill nobody predicted.
	if raw, ok := lookup(EnvIngestLiveInterval); ok && strings.TrimSpace(raw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("%w: %s=%q: not a Go duration (e.g. \"90s\", \"5m\")",
				ErrInvalid, EnvIngestLiveInterval, raw))
		case d <= 0:
			problems = append(problems, fmt.Errorf("%w: %s=%q: must be positive; a non-positive poll "+
				"interval is an unbounded loop against a metered provider",
				ErrInvalid, EnvIngestLiveInterval, raw))
		default:
			cfg.IngestLiveInterval = d
		}
	}

	if len(problems) > 0 {
		// No service prefix here: the caller adds one, and the logger stamps
		// the service on the line regardless.
		return nil, fmt.Errorf("invalid configuration: %w", errors.Join(problems...))
	}
	return cfg, nil
}

// get returns the trimmed-of-nothing raw value of key, or fallback when the key
// is unset or set to the empty string. An explicitly empty variable is treated
// as unset: compose and Helm both render unset values as "".
func get(lookup Lookup, key, fallback string) string {
	if v, ok := lookup(key); ok && v != "" {
		return v
	}
	return fallback
}

// validateListenAddr accepts the forms net.Listen accepts for TCP: ":8080",
// "0.0.0.0:8080", "[::1]:8080". The host half may be empty — that means "all
// interfaces", which is what every service inside the compose network uses.
func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("not a host:port listen address: %w", err)
	}
	return validatePort(port)
}

// validateHostPort requires both a host and a numeric port.
func validateHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("not a host:port address: %w", err)
	}
	if host == "" {
		return errors.New("missing host")
	}
	return validatePort(port)
}

func validatePort(port string) error {
	if port == "" {
		return errors.New("missing port")
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port %q is not numeric: %w", port, err)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", n)
	}
	return nil
}

// parseBrokers splits and validates a comma-separated Kafka bootstrap list.
func parseBrokers(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	brokers := make([]string, 0, len(parts))
	for _, part := range parts {
		broker := strings.TrimSpace(part)
		if broker == "" {
			continue
		}
		if err := validateHostPort(broker); err != nil {
			return nil, fmt.Errorf("broker %q: %w", broker, err)
		}
		brokers = append(brokers, broker)
	}
	if len(brokers) == 0 {
		return nil, errors.New("no brokers after parsing")
	}
	return brokers, nil
}

// validatePostgresDSN accepts both DSN forms pgx and goose understand: the URL
// form ("postgres://user:pass@host:5432/db") and the libpq keyword/value form
// ("host=... user=... dbname=...").
func validatePostgresDSN(dsn string) error {
	if !strings.Contains(dsn, "://") {
		if !strings.Contains(dsn, "host=") && !strings.Contains(dsn, "dbname=") {
			return errors.New(`neither a postgres:// URL nor a libpq keyword/value DSN (expected "host=" or "dbname=")`)
		}
		return nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("not a parseable URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("scheme is %q, want postgres or postgresql", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("URL has no host")
	}
	return nil
}

// validateOTELEndpoint accepts either a bare host:port (OTLP/gRPC) or an
// http/https URL (OTLP/HTTP).
func validateOTELEndpoint(endpoint string) error {
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return fmt.Errorf("not a parseable URL: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("scheme is %q, want http or https", u.Scheme)
		}
		if u.Host == "" {
			return errors.New("URL has no host")
		}
		return nil
	}
	return validateHostPort(endpoint)
}

// redactDSN strips the password from a postgres URL so a Config can be logged
// whole. Anything unparseable is reported as set/unset rather than echoed.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		// Keyword/value DSNs can carry `password=` inline; never echo one.
		return "[redacted]"
	}
	// url.URL.Redacted replaces any userinfo password with "xxxxx".
	return u.Redacted()
}
