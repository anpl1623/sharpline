package config_test

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/platform/config"
)

// fullEnv returns an environment that satisfies every requirement of every
// spec. Individual cases mutate a copy of it, so each case states only the one
// thing it is actually testing.
func fullEnv() map[string]string {
	return map[string]string{
		config.EnvEnv:           "dev",
		config.EnvLogLevel:      "info",
		config.EnvHTTPAddr:      ":8080",
		config.EnvPostgresDSN:   "postgres://sharpline:secret@postgres:5432/sharpline?sslmode=disable",
		config.EnvRedisAddr:     "redis:6379",
		config.EnvRedisPassword: "redis-secret",
		config.EnvKafkaBrokers:  "kafka:9092",
		config.EnvOTELEndpoint:  "otel-collector:4317",
		config.EnvJWTSigningKey: strings.Repeat("k", 32),
		config.EnvOddsAPIKey:    "",
		config.EnvSyntheticSeed: "42",
	}
}

// with returns fullEnv with the given overrides applied. An override to the
// empty string is treated by the loader as "unset".
func with(overrides map[string]string) map[string]string {
	env := fullEnv()
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

// TestOptionalJWTSigningKeyIsActuallyCarried is the assertion the table above
// cannot make: a table case that only asserts "no error" would still pass if
// LoadFrom validated the key and then threw it away, which is exactly the shape
// of the defect this exists to prevent.
//
// `stream` does not declare RequireJWT, so nothing about its startup fails when
// the key is absent — and nothing about its startup failed when the key was
// present and unread either. The gateway simply refused every credential
// forever. So this reads the field.
func TestOptionalJWTSigningKeyIsActuallyCarried(t *testing.T) {
	t.Parallel()

	key := strings.Repeat("k", 32)

	for _, spec := range []config.Spec{config.Stream, config.API} {
		t.Run(spec.Service, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.LoadFrom(spec, config.MapLookup(
				with(map[string]string{config.EnvJWTSigningKey: key})))
			if err != nil {
				t.Fatalf("LoadFrom(%s) = %v, want no error", spec.Service, err)
			}
			if cfg.JWTSigningKey != key {
				t.Fatalf("%s: JWTSigningKey = %q, want the configured key; a service that "+
					"verifies tokens cannot do so with a key the loader discarded",
					spec.Service, cfg.JWTSigningKey)
			}
		})
	}
}

func TestLoadFromValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec config.Spec
		env  map[string]string
		// wantErr is the sentinel the failure must wrap; nil means the load
		// must succeed.
		wantErr error
		// wantMentions are substrings the error must contain, so a case cannot
		// pass by failing for an unrelated reason.
		wantMentions []string
	}{
		// ---- happy paths, one per binary -----------------------------------
		{name: "api with a complete environment", spec: config.API, env: fullEnv()},
		{name: "stream with a complete environment", spec: config.Stream, env: fullEnv()},
		{name: "pricer with a complete environment", spec: config.Pricer, env: fullEnv()},
		{name: "ingest with a complete environment", spec: config.Ingest, env: fullEnv()},
		{name: "settle with a complete environment", spec: config.Settle, env: fullEnv()},
		{name: "migrate with a complete environment", spec: config.Migrate, env: fullEnv()},

		// ---- defaults ------------------------------------------------------
		{
			name: "env and log level default when unset",
			spec: config.Pricer,
			env:  with(map[string]string{config.EnvEnv: "", config.EnvLogLevel: ""}),
		},
		{
			name: "http addr defaults to the frozen service port",
			spec: config.Pricer,
			env:  with(map[string]string{config.EnvHTTPAddr: ""}),
		},
		{
			name: "otel endpoint is optional",
			spec: config.Pricer,
			env:  with(map[string]string{config.EnvOTELEndpoint: ""}),
		},
		{
			name: "synthetic seed is optional",
			spec: config.Ingest,
			env:  with(map[string]string{config.EnvSyntheticSeed: ""}),
		},
		{
			name: "redis password is optional even when redis is required",
			spec: config.Stream,
			env:  with(map[string]string{config.EnvRedisPassword: ""}),
		},
		{
			name: "odds api key is optional",
			spec: config.Ingest,
			env:  with(map[string]string{config.EnvOddsAPIKey: ""}),
		},

		// ---- requirements are per-spec, not global -------------------------
		{
			name: "migrate does not require an http address",
			spec: config.Migrate,
			env:  with(map[string]string{config.EnvHTTPAddr: ""}),
		},
		{
			name: "migrate does not require kafka redis or a jwt key",
			spec: config.Migrate,
			env: with(map[string]string{
				config.EnvKafkaBrokers:  "",
				config.EnvRedisAddr:     "",
				config.EnvJWTSigningKey: "",
			}),
		},
		{
			name: "stream does not require postgres",
			spec: config.Stream,
			env:  with(map[string]string{config.EnvPostgresDSN: ""}),
		},
		{
			name: "settle does not require redis",
			spec: config.Settle,
			env:  with(map[string]string{config.EnvRedisAddr: ""}),
		},
		{
			// The pricer opens no Redis client. Phase 2's rule — a declared
			// dependency must be one the binary OPENS — is what makes a probe
			// mean something, and api/settle once declared Postgres without
			// opening a pool so /api/readyz returned 200 with the database
			// stopped. This asserts the declaration matches the binary.
			name: "pricer does not require redis",
			spec: config.Pricer,
			env:  with(map[string]string{config.EnvRedisAddr: ""}),
		},
		{
			name: "pricer reference books are optional",
			spec: config.Pricer,
			env:  with(map[string]string{config.EnvPricerReferenceBooks: ""}),
		},
		{
			name:         "api requires postgres",
			spec:         config.API,
			env:          with(map[string]string{config.EnvPostgresDSN: ""}),
			wantErr:      config.ErrMissing,
			wantMentions: []string{config.EnvPostgresDSN},
		},
		{
			name:         "api requires a jwt signing key",
			spec:         config.API,
			env:          with(map[string]string{config.EnvJWTSigningKey: ""}),
			wantErr:      config.ErrMissing,
			wantMentions: []string{config.EnvJWTSigningKey},
		},
		{
			name:         "migrate requires postgres",
			spec:         config.Migrate,
			env:          with(map[string]string{config.EnvPostgresDSN: ""}),
			wantErr:      config.ErrMissing,
			wantMentions: []string{config.EnvPostgresDSN},
		},
		{
			name:         "ingest requires kafka brokers",
			spec:         config.Ingest,
			env:          with(map[string]string{config.EnvKafkaBrokers: ""}),
			wantErr:      config.ErrMissing,
			wantMentions: []string{config.EnvKafkaBrokers},
		},
		{
			name:         "stream requires redis",
			spec:         config.Stream,
			env:          with(map[string]string{config.EnvRedisAddr: ""}),
			wantErr:      config.ErrMissing,
			wantMentions: []string{config.EnvRedisAddr},
		},

		// ---- invalid values ------------------------------------------------
		{
			name:         "unknown deployment environment is rejected",
			spec:         config.Pricer,
			env:          with(map[string]string{config.EnvEnv: "prd"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvEnv, "prd"},
		},
		{
			name:         "unknown log level is rejected",
			spec:         config.Pricer,
			env:          with(map[string]string{config.EnvLogLevel: "verbose"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvLogLevel, "verbose"},
		},
		{
			name:         "http address without a port is rejected",
			spec:         config.Pricer,
			env:          with(map[string]string{config.EnvHTTPAddr: "8082"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvHTTPAddr},
		},
		{
			name:         "http address with a non numeric port is rejected",
			spec:         config.Pricer,
			env:          with(map[string]string{config.EnvHTTPAddr: ":http"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvHTTPAddr},
		},
		{
			name:         "http address with an out of range port is rejected",
			spec:         config.Pricer,
			env:          with(map[string]string{config.EnvHTTPAddr: ":70000"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvHTTPAddr, "65535"},
		},
		{
			name:         "postgres dsn with the wrong scheme is rejected",
			spec:         config.Migrate,
			env:          with(map[string]string{config.EnvPostgresDSN: "mysql://user@host:3306/db"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvPostgresDSN, "mysql"},
		},
		{
			name:         "postgres dsn that is neither url nor keyword form is rejected",
			spec:         config.Migrate,
			env:          with(map[string]string{config.EnvPostgresDSN: "just-a-hostname"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvPostgresDSN},
		},
		{
			name: "libpq keyword value postgres dsn is accepted",
			spec: config.Migrate,
			env: with(map[string]string{
				config.EnvPostgresDSN: "host=postgres port=5432 user=sharpline dbname=sharpline",
			}),
		},
		{
			name:         "redis address without a host is rejected",
			spec:         config.Stream,
			env:          with(map[string]string{config.EnvRedisAddr: ":6379"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvRedisAddr},
		},
		{
			name:         "kafka broker without a port is rejected",
			spec:         config.Pricer,
			env:          with(map[string]string{config.EnvKafkaBrokers: "kafka"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvKafkaBrokers},
		},
		{
			name:         "kafka broker list where one entry is bad is rejected",
			spec:         config.Pricer,
			env:          with(map[string]string{config.EnvKafkaBrokers: "kafka-0:9092,kafka-1"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvKafkaBrokers, "kafka-1"},
		},
		{
			name: "kafka broker list of several valid entries is accepted",
			spec: config.Pricer,
			env:  with(map[string]string{config.EnvKafkaBrokers: "kafka-0:9092, kafka-1:9092 ,kafka-2:9092"}),
		},
		{
			name:         "short jwt signing key is rejected",
			spec:         config.API,
			env:          with(map[string]string{config.EnvJWTSigningKey: "too-short"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvJWTSigningKey, "32"},
		},
		{
			// The three cases below pin the split between "this binary REQUIRES
			// a key" and "this binary USES one if it is given". `stream`
			// verifies an access token presented on the WebSocket handshake
			// (ADR 0008) but serves the public odds board without one, so the
			// key must be readable there without being mandatory. Collapsing
			// the two questions is what made the gateway's auth path
			// unreachable in every deployment: the key was read only where it
			// was required, so `stream` never saw it.
			name: "stream loads without a jwt signing key",
			spec: config.Stream,
			env:  with(map[string]string{config.EnvJWTSigningKey: ""}),
		},
		{
			name:         "a short jwt signing key is rejected even where it is optional",
			spec:         config.Stream,
			env:          with(map[string]string{config.EnvJWTSigningKey: "too-short"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvJWTSigningKey, "32"},
		},
		{
			name:         "non numeric synthetic seed is rejected",
			spec:         config.Ingest,
			env:          with(map[string]string{config.EnvSyntheticSeed: "banana"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvSyntheticSeed, "banana"},
		},
		{
			name:         "otel endpoint with the wrong scheme is rejected",
			spec:         config.Pricer,
			env:          with(map[string]string{config.EnvOTELEndpoint: "grpc://otel-collector:4317"}),
			wantErr:      config.ErrInvalid,
			wantMentions: []string{config.EnvOTELEndpoint, "grpc"},
		},
		{
			name: "otel http endpoint is accepted",
			spec: config.Pricer,
			env:  with(map[string]string{config.EnvOTELEndpoint: "http://otel-collector:4318"}),
		},

		// ---- every problem is reported at once -----------------------------
		{
			name: "all problems are reported in one error",
			spec: config.API,
			env: with(map[string]string{
				config.EnvEnv:           "nowhere",
				config.EnvPostgresDSN:   "",
				config.EnvRedisAddr:     "",
				config.EnvKafkaBrokers:  "",
				config.EnvJWTSigningKey: "",
			}),
			wantErr: config.ErrMissing,
			wantMentions: []string{
				config.EnvEnv,
				config.EnvPostgresDSN,
				config.EnvRedisAddr,
				config.EnvKafkaBrokers,
				config.EnvJWTSigningKey,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.LoadFrom(tt.spec, config.MapLookup(tt.env))

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("LoadFrom(%s) = error %v, want success", tt.spec.Service, err)
				}
				if cfg == nil {
					t.Fatal("LoadFrom returned a nil config and a nil error")
				}
				if cfg.Service != tt.spec.Service {
					t.Errorf("Service = %q, want %q", cfg.Service, tt.spec.Service)
				}
				return
			}

			if err == nil {
				t.Fatalf("LoadFrom(%s) succeeded, want error wrapping %v", tt.spec.Service, tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error %v does not wrap %v", err, tt.wantErr)
			}
			if cfg != nil {
				t.Errorf("LoadFrom returned config %+v alongside an error, want nil", cfg)
			}
			for _, want := range tt.wantMentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

// TestPricerReferenceBooksParseIntoAnOrderedList.
//
// The list is ORDERED — it is a preference, and the order is what makes one
// binary correct against both providers — so this asserts the order survives,
// not merely the membership. Blank entries are dropped rather than rejected so a
// trailing comma is not a startup failure.
func TestPricerReferenceBooksParseIntoAnOrderedList(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadFrom(config.Pricer, config.MapLookup(map[string]string{
		config.EnvKafkaBrokers:         "kafka:9092",
		config.EnvPricerReferenceBooks: " pinnacle , sharpline , ,",
	}))
	if err != nil {
		t.Fatalf("LoadFrom(pricer): %v", err)
	}
	want := []string{"pinnacle", "sharpline"}
	if !slices.Equal(cfg.PricerReferenceBooks, want) {
		t.Errorf("PricerReferenceBooks = %v, want %v", cfg.PricerReferenceBooks, want)
	}

	empty, err := config.LoadFrom(config.Pricer, config.MapLookup(map[string]string{
		config.EnvKafkaBrokers: "kafka:9092",
	}))
	if err != nil {
		t.Fatalf("LoadFrom(pricer) with no preference list: %v", err)
	}
	if len(empty.PricerReferenceBooks) != 0 {
		t.Errorf("PricerReferenceBooks = %v with the variable unset, want empty — an unset "+
			"list means \"provider designation only\", not a guessed book",
			empty.PricerReferenceBooks)
	}
}

func TestLoadFromDefaults(t *testing.T) {
	t.Parallel()

	// Only the variables a pricer genuinely requires; everything else absent.
	// Redis is deliberately NOT here: the pricer opens no Redis client, its whole
	// state is a fold of a compacted topic, and phase 2's rule is that a declared
	// dependency must be one the binary actually opens.
	env := map[string]string{
		config.EnvKafkaBrokers: "kafka:9092",
	}

	cfg, err := config.LoadFrom(config.Pricer, config.MapLookup(env))
	if err != nil {
		t.Fatalf("LoadFrom(pricer) = error %v, want success", err)
	}

	if got, want := cfg.Env, config.EnvDev; got != want {
		t.Errorf("Env = %q, want %q", got, want)
	}
	if got, want := cfg.LogLevel, slog.LevelInfo; got != want {
		t.Errorf("LogLevel = %v, want %v", got, want)
	}
	if got, want := cfg.HTTPAddr, config.Pricer.DefaultHTTPAddr; got != want {
		t.Errorf("HTTPAddr = %q, want the frozen default %q", got, want)
	}
	if cfg.OTELEndpoint != "" {
		t.Errorf("OTELEndpoint = %q, want empty (export disabled)", cfg.OTELEndpoint)
	}
	if cfg.SyntheticSeed != 0 {
		t.Errorf("SyntheticSeed = %d, want 0", cfg.SyntheticSeed)
	}
	if cfg.HasOddsAPIKey() {
		t.Error("HasOddsAPIKey() = true with ODDS_API_KEY unset, want false")
	}
	if cfg.IsProd() {
		t.Error("IsProd() = true in the dev default, want false")
	}
}

func TestLoadFromParsesValues(t *testing.T) {
	t.Parallel()

	env := with(map[string]string{
		config.EnvEnv:           "PROD",
		config.EnvLogLevel:      " Debug ",
		config.EnvKafkaBrokers:  "kafka-0:9092, kafka-1:9092 ",
		config.EnvSyntheticSeed: "-7",
		config.EnvOddsAPIKey:    "  live-key  ",
	})

	cfg, err := config.LoadFrom(config.Ingest, config.MapLookup(env))
	if err != nil {
		t.Fatalf("LoadFrom(ingest) = error %v, want success", err)
	}

	if got, want := cfg.Env, config.EnvProd; got != want {
		t.Errorf("Env = %q, want %q (case-insensitive)", got, want)
	}
	if !cfg.IsProd() {
		t.Error("IsProd() = false, want true")
	}
	if got, want := cfg.LogLevel, slog.LevelDebug; got != want {
		t.Errorf("LogLevel = %v, want %v (trimmed, case-insensitive)", got, want)
	}
	if got, want := len(cfg.KafkaBrokers), 2; got != want {
		t.Fatalf("len(KafkaBrokers) = %d, want %d", got, want)
	}
	if got, want := cfg.KafkaBrokers[1], "kafka-1:9092"; got != want {
		t.Errorf("KafkaBrokers[1] = %q, want %q (whitespace trimmed)", got, want)
	}
	if got, want := cfg.SyntheticSeed, int64(-7); got != want {
		t.Errorf("SyntheticSeed = %d, want %d", got, want)
	}
	if got, want := cfg.OddsAPIKey, "live-key"; got != want {
		t.Errorf("OddsAPIKey = %q, want %q (whitespace trimmed)", got, want)
	}
	if !cfg.HasOddsAPIKey() {
		t.Error("HasOddsAPIKey() = false with ODDS_API_KEY set, want true")
	}
}

// TestLogValueRedactsSecrets guards the property that makes it safe for every
// cmd/ entrypoint to log its whole configuration at startup.
func TestLogValueRedactsSecrets(t *testing.T) {
	t.Parallel()

	const (
		pgPassword    = "pg-password-must-not-appear"
		redisPassword = "redis-password-must-not-appear"
		jwtKey        = "jwt-signing-key-must-not-appear-0123456789"
		oddsKey       = "odds-api-key-must-not-appear"
	)

	cfg, err := config.LoadFrom(config.API, config.MapLookup(with(map[string]string{
		config.EnvPostgresDSN:   "postgres://sharpline:" + pgPassword + "@postgres:5432/sharpline",
		config.EnvRedisPassword: redisPassword,
		config.EnvJWTSigningKey: jwtKey,
		config.EnvOddsAPIKey:    oddsKey,
	})))
	if err != nil {
		t.Fatalf("LoadFrom(api) = error %v, want success", err)
	}

	rendered := cfg.LogValue().String()
	for _, secret := range []string{pgPassword, redisPassword, jwtKey, oddsKey} {
		if strings.Contains(rendered, secret) {
			t.Errorf("LogValue() leaked a secret: %q appears in %q", secret, rendered)
		}
	}
	// The non-secret parts must still be there, or redaction has gone too far
	// and the log line is useless.
	for _, want := range []string{"api", "postgres:5432", "redis:6379"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("LogValue() = %q, want it to contain %q", rendered, want)
		}
	}
}

func TestLoadFromRejectsUnusableArguments(t *testing.T) {
	t.Parallel()

	t.Run("nil lookup", func(t *testing.T) {
		t.Parallel()
		if _, err := config.LoadFrom(config.API, nil); !errors.Is(err, config.ErrInvalid) {
			t.Errorf("LoadFrom(api, nil) = %v, want an error wrapping ErrInvalid", err)
		}
	})

	t.Run("spec without a service name", func(t *testing.T) {
		t.Parallel()
		_, err := config.LoadFrom(config.Spec{}, config.MapLookup(fullEnv()))
		if !errors.Is(err, config.ErrInvalid) {
			t.Errorf("LoadFrom(zero spec) = %v, want an error wrapping ErrInvalid", err)
		}
	})
}

// TestIngestLiveIntervalIsOptionalAndValidated.
//
// The live tier is ~77% of the monthly credit spend, so this is the one cadence
// the environment can move — ADR 0003 promises the ladder is "retunable for a
// different tier without a code change" and this variable is what makes that
// true. Everything about it that could quietly cost money is asserted here.
func TestIngestLiveIntervalIsOptionalAndValidated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "absent means the scheduler default", value: "", want: 0},
		{name: "a plain duration parses", value: "90s", want: 90 * time.Second},
		{name: "minutes parse", value: "5m", want: 5 * time.Minute},
		{name: "surrounding whitespace is tolerated", value: "  45s  ", want: 45 * time.Second},

		// A non-positive interval is an unbounded loop against a metered API.
		// It is REFUSED, not clamped: a cadence silently corrected to a default
		// is a bill nobody predicted.
		{name: "zero is refused", value: "0s", wantErr: true},
		{name: "negative is refused", value: "-30s", wantErr: true},

		// A bare number is the most likely typo — "90" meaning ninety seconds —
		// and Go's ParseDuration rejects it. Letting it through as 90ns would
		// poll eleven million times a second.
		{name: "a unitless number is refused", value: "90", wantErr: true},
		{name: "nonsense is refused", value: "soon", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := with(map[string]string{config.EnvIngestLiveInterval: tc.value})
			cfg, err := config.LoadFrom(config.Ingest, config.MapLookup(env))

			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s=%q was accepted; it must fail startup", config.EnvIngestLiveInterval, tc.value)
				}
				if !errors.Is(err, config.ErrInvalid) {
					t.Errorf("error does not wrap ErrInvalid: %v", err)
				}
				if !strings.Contains(err.Error(), config.EnvIngestLiveInterval) {
					t.Errorf("the error does not name the variable, so an operator cannot find it: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s=%q was rejected: %v", config.EnvIngestLiveInterval, tc.value, err)
			}
			if cfg.IngestLiveInterval != tc.want {
				t.Errorf("IngestLiveInterval = %s, want %s", cfg.IngestLiveInterval, tc.want)
			}
		})
	}
}

// TestIngestResultsKnobsAreOptionalAndValidated.
//
// The two results-path cadence knobs go through the same parse-and-refuse rule
// as the live interval, and they are asserted here for a different reason. The
// live interval's failure mode is a bill; theirs is a customer's stake sitting
// in escrow with nothing to release it, which is invisible until somebody asks
// why they have not been paid.
//
// The case that matters most is "0s". It parses, and a poller would happily take
// it as "use the default" — because the field's zero value is exactly what an
// UNSET variable produces — so the two would silently mean the same thing while
// an operator believed they had set something. It is refused at the boundary.
func TestIngestResultsKnobsAreOptionalAndValidated(t *testing.T) {
	t.Parallel()

	for _, knob := range []struct {
		env  string
		read func(*config.Config) time.Duration
	}{
		{config.EnvIngestResultsInterval, func(c *config.Config) time.Duration { return c.IngestResultsInterval }},
		{config.EnvIngestResultsDelay, func(c *config.Config) time.Duration { return c.IngestResultsDelay }},
	} {
		t.Run(knob.env, func(t *testing.T) {
			t.Parallel()

			for _, tc := range []struct {
				name    string
				value   string
				want    time.Duration
				wantErr bool
			}{
				{name: "absent means the poller's own default", value: "", want: 0},
				{name: "minutes parse", value: "30s", want: 30 * time.Second},
				{name: "hours parse", value: "6h", want: 6 * time.Hour},
				{name: "surrounding whitespace is tolerated", value: "  2m  ", want: 2 * time.Minute},

				{name: "zero is refused", value: "0s", wantErr: true},
				{name: "negative is refused", value: "-5m", wantErr: true},
				{name: "a unitless number is refused", value: "60", wantErr: true},
				{name: "nonsense is refused", value: "later", wantErr: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					env := with(map[string]string{knob.env: tc.value})
					cfg, err := config.LoadFrom(config.Ingest, config.MapLookup(env))

					if tc.wantErr {
						if err == nil {
							t.Fatalf("%s=%q was accepted; it must fail startup", knob.env, tc.value)
						}
						if !errors.Is(err, config.ErrInvalid) {
							t.Errorf("error does not wrap ErrInvalid: %v", err)
						}
						if !strings.Contains(err.Error(), knob.env) {
							t.Errorf("the error does not name the variable, so an operator cannot find it: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatalf("%s=%q was rejected: %v", knob.env, tc.value, err)
					}
					if got := knob.read(cfg); got != tc.want {
						t.Errorf("%s = %s, want %s", knob.env, got, tc.want)
					}
				})
			}
		})
	}
}
