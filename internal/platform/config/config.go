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
	"net/netip"
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

	// EnvTrustedProxies is the comma-separated set of CIDRs (or bare
	// addresses) whose forwarding headers this service may believe.
	//
	// It exists because CLAUDE.md §6 requires per-IP rate limiting and a
	// per-IP limiter is only as good as its idea of the client's IP. Behind
	// the Caddy proxy, RemoteAddr is the PROXY, so with no trusted set every
	// request in the system buckets on one address and one client exhausts the
	// limit for everybody. Trust the header unconditionally instead and the
	// control evaporates the other way: any caller picks its own bucket by
	// sending a different X-Forwarded-For.
	//
	// So it is DEPLOYMENT CONFIGURATION, not a default this binary can guess.
	// Set it to exactly the hop in front of the service — the compose bridge
	// subnet, or the ingress controller's pod CIDR — and nothing else. Never a
	// blanket private-range list: on a shared bridge network "trust RFC1918"
	// means "trust every container", which is every container that could be
	// compromised.
	//
	// Empty is valid and means "believe nobody", which is the correct and safe
	// behaviour for a service addressed directly.
	EnvTrustedProxies = "SHARPLINE_TRUSTED_PROXIES"

	// EnvTOTPKeyring is the AEAD keyring that seals TOTP shared secrets at
	// rest, in auth.ParseKeyring's frozen `id:base64key[,id:base64key...]`
	// format, most recent key FIRST.
	//
	// It is separate from EnvJWTSigningKey and must stay separate. The JWT key
	// signs short-lived bearer tokens and can be rotated at will — every token
	// under the old key expires within minutes. This key DECRYPTS DATA AT REST,
	// so losing it means every enrolled second factor becomes permanently
	// unopenable and every one of those users is locked out of their account.
	// One variable serving both purposes guarantees that the safe rotation
	// cadence for one is the catastrophic one for the other.
	//
	// OPTIONAL. Unset means no keyring, which means TOTP enrolment is refused
	// rather than performed without encryption — migrations/00005 requires
	// user_totp to hold ciphertext, and a service that stored a bare shared
	// secret because a variable was missing would be a silent downgrade of the
	// one credential that must never be readable from a database dump.
	// Password login, refresh rotation and the whole read surface are
	// unaffected.
	EnvTOTPKeyring = "SHARPLINE_TOTP_KEYRING"

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

	// EnvIngestResultsInterval overrides how often the results poller reads its
	// work queue.
	//
	// It is exposed for the same reason EnvIngestLiveInterval is, applied to the
	// other arrow: the interval is the FLOOR of the settlement lag, so it is
	// literally what a customer waits between the final whistle and their ticket
	// becoming settleable, and a deployment that cannot change it cannot trade
	// that latency against the load it costs.
	//
	// It is cheap where the odds cadence is not — one indexed read against a
	// partial index per tick, plus one provider call — so the number is chosen
	// for the customer rather than for the bill. That asymmetry is exactly why
	// it is a SEPARATE knob and not derived from the live cadence.
	EnvIngestResultsInterval = "SHARPLINE_INGEST_RESULTS_INTERVAL"

	// EnvIngestResultsDelay overrides how long after its scheduled start a
	// contest is considered plausibly over, and therefore worth asking a results
	// provider about.
	//
	// This is the one number in the results path that HAS to be wrong for some
	// sport: a single horizon cannot be right for a 48-minute basketball game, a
	// three-hour NFL broadcast and a five-day Test match at once, and the
	// package's default errs deliberately WIDE. Erring wide costs a query the
	// provider answers with silence; erring narrow costs every customer on a
	// long fixture the difference. A deployment whose slate is all one sport
	// should be able to tighten it without a rebuild, which is what this is for.
	EnvIngestResultsDelay = "SHARPLINE_INGEST_RESULTS_DELAY"
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
	// RequireJWT means the binary CANNOT START without a signing key, because
	// every one of its useful paths mints or verifies an access token.
	//
	// It is not "this binary understands tokens". `stream` verifies one when a
	// client presents it and serves the public board when nobody does, so it
	// reads SHARPLINE_JWT_SIGNING_KEY without declaring this — see the read in
	// LoadFrom, which is unconditional. Declaring it here would turn an absent
	// key into a refusal to serve public data.
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
	//
	// Postgres was added in phase 9. The pricing pass itself still touches no
	// database — it is a fold of the compacted price.computed topic and must stay
	// one, because internal/pricing's change detection depends on the pass being a
	// pure function of the record. What needs the pool is the SIGNALS STAGE, a
	// second consumer group in the same binary (cmd/pricer's package comment
	// argues why it is a second consumer rather than a hook), which persists +EV,
	// arbitrage and steam findings to the tables migrations/00009 creates.
	//
	// It is REQUIRED rather than optional for the same reason ingest's is: a
	// pricer that started without a DSN would report itself ready, publish
	// findings to the bus, and leave every analytics table empty — the query
	// surface would answer 200 with an empty collection, which is
	// indistinguishable from "the market is quiet".
	Pricer = Spec{
		Service:         "pricer",
		DefaultHTTPAddr: ":8082",
		Requires:        RequireHTTP | RequirePostgres | RequireKafka,
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

	// TrustedProxies is the parsed EnvTrustedProxies set, in the order given.
	// Nil means no peer's forwarding header is believed. See EnvTrustedProxies
	// for why this has no default.
	TrustedProxies []string

	// TOTPKeyring is the raw EnvTOTPKeyring spec, unparsed. Empty means no
	// keyring; see EnvTOTPKeyring. It is a string rather than a parsed
	// *auth.Keyring because internal/platform must not import internal/auth,
	// and because parsing it here would put key material in a struct that is
	// logged (LogValue below reports only whether it is set).
	TOTPKeyring string

	// OddsAPIKey selects the ingest provider adapter: the real The Odds API
	// adapter when set, the synthetic stochastic market maker when not.
	OddsAPIKey string

	// SyntheticSeed seeds the synthetic provider's RNG so tests are
	// deterministic. Zero means "seed from the clock".
	SyntheticSeed int64

	// IngestLiveInterval overrides the live-window poll cadence. Zero means the
	// scheduler's own default (90s). See EnvIngestLiveInterval.
	IngestLiveInterval time.Duration

	// IngestResultsInterval overrides the results poller's cadence. Zero means
	// the poller's own default (1m). See EnvIngestResultsInterval.
	IngestResultsInterval time.Duration

	// IngestResultsDelay overrides the results poller's "plausibly over"
	// horizon. Zero means the poller's own default (2h).
	// See EnvIngestResultsDelay.
	IngestResultsDelay time.Duration

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
		// Both of the next two report PRESENCE only. The keyring is key
		// material and the trusted set is small enough to print, but printing
		// the latter and not the former would invite someone to "make it
		// consistent" in the wrong direction.
		slog.Bool("totp_keyring_set", c.TOTPKeyring != ""),
		slog.Int("trusted_proxies", len(c.TrustedProxies)),
		slog.Bool("odds_api_key_set", c.OddsAPIKey != ""),
		slog.Int64("synthetic_seed", c.SyntheticSeed),
		slog.String("ingest_live_interval", c.IngestLiveInterval.String()),
		slog.String("ingest_results_interval", c.IngestResultsInterval.String()),
		slog.String("ingest_results_delay", c.IngestResultsDelay.String()),
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

	// SHARPLINE_JWT_SIGNING_KEY — READ FOR EVERY BINARY, REQUIRED ONLY BY THOSE
	// THAT DECLARE RequireJWT.
	//
	// This is the one variable whose requirement and whose USE are not the same
	// question, and collapsing them was a real defect. `stream` verifies an
	// access token presented on the WebSocket handshake (ADR 0008), so it needs
	// the key whenever a deployment wants authenticated subscriptions — but a
	// gateway serving the PUBLIC odds board must still start without one,
	// because market data is public (CLAUDE.md §6) and an anonymous connection
	// is a first-class state rather than a degraded one. Under the old shape the
	// key was read only when it was mandatory, so `stream` could never see it at
	// all: every presented credential was refused, `newVerifier` logged its
	// warning on every boot, and there was no configuration that fixed it.
	//
	// Requiring it for `stream` is the other wrong answer. RequireJWT is a HARD
	// gate — the process refuses to start — so declaring it would make a missing
	// key take down a public board that does not need one.
	//
	// So: read it always, demand it only where it is mandatory, and validate its
	// LENGTH wherever it is present. That last clause matters more than it
	// looks. A key too short to be worth having is a misconfiguration in every
	// binary that holds one, and the failure it produces is silent — tokens
	// verify, and the signature they verify against is weak. CLAUDE.md §12
	// ("fail fast and loudly on a bad config") applies to a key that is optional
	// exactly as much as to one that is required.
	cfg.JWTSigningKey = get(lookup, EnvJWTSigningKey, "")
	switch {
	case cfg.JWTSigningKey == "":
		if spec.Requires.has(RequireJWT) {
			problems = append(problems, fmt.Errorf("%w: %s", ErrMissing, EnvJWTSigningKey))
		}
	case len(cfg.JWTSigningKey) < minJWTKeyLen:
		problems = append(problems, fmt.Errorf("%w: %s is %d bytes, want at least %d",
			ErrInvalid, EnvJWTSigningKey, len(cfg.JWTSigningKey), minJWTKeyLen))
	}

	// SHARPLINE_TRUSTED_PROXIES — always optional; empty means believe nobody.
	//
	// Validated HERE rather than only where it is consumed, so a typo in a CIDR
	// is a refusal to start (CLAUDE.md §12: "fail fast and loudly on a bad
	// config") rather than a limiter that silently falls back to bucketing the
	// whole internet under the proxy's address. A rate-limiting control that
	// degrades quietly is the one that is never noticed.
	if raw := strings.TrimSpace(get(lookup, EnvTrustedProxies, "")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			// A bare address is accepted and means that single host; anything
			// else must be a CIDR. Both forms are kept verbatim so the consumer
			// parses exactly the string the operator wrote.
			if _, err := netip.ParsePrefix(part); err != nil {
				if _, err := netip.ParseAddr(part); err != nil {
					problems = append(problems, fmt.Errorf(
						"%w: %s=%q: %q is neither a CIDR nor an IP address",
						ErrInvalid, EnvTrustedProxies, raw, part))
					continue
				}
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, part)
		}
	}

	// SHARPLINE_TOTP_KEYRING — always optional. Parsed by the consumer
	// (auth.ParseKeyring) rather than here, so key material never lands in this
	// struct in decoded form.
	cfg.TOTPKeyring = strings.TrimSpace(get(lookup, EnvTOTPKeyring, ""))

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

	// The three ingest cadence knobs. Each is always optional, but must parse
	// and be POSITIVE if present. Every one of them is a loop's period or a
	// horizon a loop subtracts, and a non-positive value is refused rather than
	// clamped for the reason the whole of this function fails fast: a silently
	// corrected cadence is a bill, or a settlement delay, that nobody predicted.
	//
	// Zero is therefore never a legal SET value, only an absent one — which is
	// what lets the field's zero value mean "the owning package's default"
	// downstream without a second flag to say which of the two happened.
	for _, k := range []struct {
		env   string
		why   string
		field *time.Duration
	}{
		{
			env:   EnvIngestLiveInterval,
			why:   "a non-positive poll interval is an unbounded loop against a metered provider",
			field: &cfg.IngestLiveInterval,
		},
		{
			env:   EnvIngestResultsInterval,
			why:   "a non-positive interval is an unbounded loop against the work-queue read",
			field: &cfg.IngestResultsInterval,
		},
		{
			env: EnvIngestResultsDelay,
			// Zero would be legal to the poller — a provider trusted to say when
			// a contest is over is entitled to be asked immediately — but it is
			// refused HERE because an env var set to "0s" is indistinguishable
			// downstream from one that was never set, and the two would mean
			// opposite things. Asking about everything that has started is
			// spelled as a small positive value, not as zero.
			why: "a non-positive horizon cannot be distinguished from an unset one, " +
				"which means the poller's own default instead",
			field: &cfg.IngestResultsDelay,
		},
	} {
		raw, ok := lookup(k.env)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("%w: %s=%q: not a Go duration (e.g. \"90s\", \"5m\")",
				ErrInvalid, k.env, raw))
		case d <= 0:
			problems = append(problems, fmt.Errorf("%w: %s=%q: must be positive; %s",
				ErrInvalid, k.env, raw, k.why))
		default:
			*k.field = d
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
