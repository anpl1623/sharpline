package theoddsapi

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Environment variable names.
//
// ODDS_API_KEY is FROZEN — internal/platform/config already declares it as
// config.EnvOddsAPIKey, the compose stack routes it to `ingest` alone through
// the x-env-base anchor, and its presence is what selects this adapter over the
// synthetic one. Everything else here is adapter-specific tuning, namespaced
// under SHARPLINE_ODDS_ like the rest of the project's variables.
//
// .env is gitignored and .env.example carries an EMPTY value for the key. There
// is no default key and there never will be one.
const (
	envAPIKey         = "ODDS_API_KEY"
	envBaseURL        = "SHARPLINE_ODDS_BASE_URL"
	envRegions        = "SHARPLINE_ODDS_REGIONS"
	envBookmakers     = "SHARPLINE_ODDS_BOOKMAKERS"
	envMarkets        = "SHARPLINE_ODDS_MARKETS"
	envOddsFormat     = "SHARPLINE_ODDS_FORMAT"
	envRequestTimeout = "SHARPLINE_ODDS_REQUEST_TIMEOUT"
	envMaxAttempts    = "SHARPLINE_ODDS_MAX_ATTEMPTS"

	// envPlayerPropMarkets opts in to the per-event endpoint. Empty by
	// default; ADR 0003 scenario E prices out why.
	envPlayerPropMarkets = "SHARPLINE_ODDS_PLAYER_PROP_MARKETS"

	envReferenceBook         = "SHARPLINE_ODDS_REFERENCE_BOOK"
	envIncludeInactiveSports = "SHARPLINE_ODDS_INCLUDE_INACTIVE_SPORTS"

	// envMonthlyCredits is the budget CLAUDE.md §5 requires be a config value:
	// "Respect provider quotas via a token-bucket limiter with the budget as a
	// config value". ADR 0003's recommended tier is 100000.
	envMonthlyCredits = "SHARPLINE_ODDS_MONTHLY_CREDITS"

	envCreditBurst       = "SHARPLINE_ODDS_CREDIT_BURST"
	envRequestsPerSecond = "SHARPLINE_ODDS_REQUESTS_PER_SECOND"
)

// Lookup is the consumer-declared seam over the process environment. It has the
// shape of os.LookupEnv, which lets a test supply a map without touching global
// process state — the same seam internal/platform/config uses.
type Lookup func(key string) (value string, ok bool)

// MapLookup adapts a map to Lookup.
func MapLookup(env map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// ConfigFromEnv builds a Config from the environment.
//
// It reports every malformed variable in one error rather than stopping at the
// first, so a broken deployment is fixed in one pass. It does NOT call
// Validate: the caller does that, because New does it too and reporting the
// same problem twice from two places produces confusing startup output.
//
// A MISSING key is not an error here — its absence is how `ingest` chooses the
// synthetic adapter (ADR 0003). Callers test HasKey before constructing.
func ConfigFromEnv(lookup Lookup) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("theoddsapi: nil environment lookup")
	}

	var problems []error
	get := func(key string) string {
		v, ok := lookup(key)
		if !ok {
			return ""
		}
		return strings.TrimSpace(v)
	}
	list := func(key string) []string {
		raw := get(key)
		if raw == "" {
			return nil
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	intVal := func(key string, fallback int64) int64 {
		raw := get(key)
		if raw == "" {
			return fallback
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s=%q: not a base-10 integer", key, raw))
			return fallback
		}
		return n
	}

	cfg := Config{
		// Not trimmed of anything but surrounding whitespace, and never
		// inspected further. A key is opaque.
		APIKey:            get(envAPIKey),
		BaseURL:           get(envBaseURL),
		Regions:           list(envRegions),
		Bookmakers:        list(envBookmakers),
		Markets:           list(envMarkets),
		PlayerPropMarkets: list(envPlayerPropMarkets),
		ReferenceBook:     get(envReferenceBook),
	}

	if raw := get(envIncludeInactiveSports); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			problems = append(problems, fmt.Errorf(
				"%s=%q: not a boolean such as \"true\" or \"false\"", envIncludeInactiveSports, raw))
		} else {
			cfg.IncludeInactiveSports = b
		}
	}

	if raw := get(envOddsFormat); raw != "" {
		cfg.OddsFormat = OddsFormat(raw)
	}
	if raw := get(envRequestTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s=%q: not a Go duration such as \"15s\"", envRequestTimeout, raw))
		} else {
			cfg.RequestTimeout = d
		}
	}
	cfg.MaxAttempts = int(intVal(envMaxAttempts, 0))
	cfg.Limiter.MonthlyCredits = intVal(envMonthlyCredits, 0)
	cfg.Limiter.CreditBurst = intVal(envCreditBurst, 0)

	if raw := get(envRequestsPerSecond); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s=%q: not a number", envRequestsPerSecond, raw))
		} else {
			cfg.Limiter.RequestsPerSecond = f
		}
	}

	if len(problems) > 0 {
		return cfg, fmt.Errorf("%w: %w", ErrInvalidConfig, errors.Join(problems...))
	}
	return cfg, nil
}

// HasKey reports whether the environment selects the real adapter.
//
// ADR 0003: "ingest selects its adapter at startup — real adapter when
// ODDS_API_KEY is set, synthetic stochastic market-maker when it is empty."
// This is the whole of that decision, in one place, so `ingest` does not
// reimplement it.
func HasKey(lookup Lookup) bool {
	if lookup == nil {
		return false
	}
	v, ok := lookup(envAPIKey)
	return ok && strings.TrimSpace(v) != ""
}
