package theoddsapi

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestHasKeyIsTheAdapterSelectionRule.
//
// ADR 0003 puts the whole of the startup choice in one place: "ingest selects
// its adapter at startup — real adapter when ODDS_API_KEY is set, synthetic
// stochastic market-maker when it is empty." Two implementations of that rule
// would eventually disagree, and the failure would be a deployment silently
// serving generated prices while believing it was serving real ones — which the
// no-mock-data rule exists to prevent.
func TestHasKeyIsTheAdapterSelectionRule(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"unset selects synthetic", map[string]string{}, false},
		{"empty selects synthetic", map[string]string{envAPIKey: ""}, false},
		{"whitespace selects synthetic", map[string]string{envAPIKey: "   "}, false},
		{"set selects the real adapter", map[string]string{envAPIKey: testAPIKey}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasKey(MapLookup(tc.env)); got != tc.want {
				t.Errorf("HasKey = %v, want %v", got, tc.want)
			}
		})
	}
	if HasKey(nil) {
		t.Errorf("HasKey(nil) selected the real adapter; a missing environment must fall back to synthetic")
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		envAPIKey:                testAPIKey,
		envBaseURL:               "https://ipv6-api.the-odds-api.com",
		envRegions:               "us, us2",
		envMarkets:               "h2h,totals",
		envPlayerPropMarkets:     "player_pass_tds, player_rush_yds",
		envReferenceBook:         "pinnacle",
		envIncludeInactiveSports: "true",
		envOddsFormat:            "decimal",
		envRequestTimeout:        "9s",
		envMaxAttempts:           "2",
		envMonthlyCredits:        "100000",
		envCreditBurst:           "3400",
		envRequestsPerSecond:     "4.5",
	}

	cfg, err := ConfigFromEnv(MapLookup(env))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.APIKey != testAPIKey {
		t.Errorf("APIKey was altered by parsing; a key is opaque")
	}
	if got, want := strings.Join(cfg.Regions, "|"), "us|us2"; got != want {
		t.Errorf("Regions = %q, want %q (whitespace around a comma is trimmed)", got, want)
	}
	if got, want := strings.Join(cfg.PlayerPropMarkets, "|"), "player_pass_tds|player_rush_yds"; got != want {
		t.Errorf("PlayerPropMarkets = %q, want %q", got, want)
	}
	if !cfg.IncludeInactiveSports {
		t.Errorf("IncludeInactiveSports = false, want true")
	}
	if cfg.RequestTimeout != 9*time.Second {
		t.Errorf("RequestTimeout = %s, want 9s", cfg.RequestTimeout)
	}
	if cfg.Limiter.MonthlyCredits != 100_000 || cfg.Limiter.CreditBurst != 3_400 {
		t.Errorf("limiter budget = %d/%d, want 100000/3400", cfg.Limiter.MonthlyCredits, cfg.Limiter.CreditBurst)
	}
	if cfg.Limiter.RequestsPerSecond != 4.5 {
		t.Errorf("RequestsPerSecond = %v, want 4.5", cfg.Limiter.RequestsPerSecond)
	}
	if err := cfg.withDefaults().Validate(); err != nil {
		t.Errorf("a fully specified environment does not validate: %v", err)
	}
}

// TestConfigFromEnvReportsEveryProblemAtOnce.
//
// CLAUDE.md §12: "fail fast and loudly on a bad config". One problem per restart
// turns a five-minute fix into an afternoon.
func TestConfigFromEnvReportsEveryProblemAtOnce(t *testing.T) {
	_, err := ConfigFromEnv(MapLookup(map[string]string{
		envAPIKey:                testAPIKey,
		envMaxAttempts:           "many",
		envMonthlyCredits:        "lots",
		envRequestsPerSecond:     "fast",
		envRequestTimeout:        "soon",
		envIncludeInactiveSports: "maybe",
	}))
	if err == nil {
		t.Fatalf("five malformed variables were accepted")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("error does not unwrap to ErrInvalidConfig: %v", err)
	}
	for _, key := range []string{
		envMaxAttempts, envMonthlyCredits, envRequestsPerSecond,
		envRequestTimeout, envIncludeInactiveSports,
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the error does not name %s, so it would take another restart to find: %v", key, err)
		}
	}
	assertNoKey(t, "a config parse error", err.Error())
}

// TestConfigRejectsAFeaturedMarketInThePropList.
//
// The two endpoints bill differently — a sweep is markets × regions regardless
// of slate size, a per-event call is markets × regions PER EVENT — so a key in
// both lists makes the predicted cost depend on which path happened to run,
// and Cost is the number the token bucket charges before it spends.
func TestConfigRejectsAFeaturedMarketInThePropList(t *testing.T) {
	cfg := baseConfig("https://api.the-odds-api.com")
	cfg.PlayerPropMarkets = []string{"h2h"}
	err := cfg.withDefaults().Validate()
	if err == nil {
		t.Fatalf("a featured market was accepted in PlayerPropMarkets")
	}
	if !strings.Contains(err.Error(), "cost model") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

// TestConfigRejectsANonFeaturedMarketInTheSweepList mirrors it: the provider's
// own INVALID_MARKET page says /odds "only support[s] featured markets", so this
// is caught at startup rather than as a 422 in production.
func TestConfigRejectsANonFeaturedMarketInTheSweepList(t *testing.T) {
	cfg := baseConfig("https://api.the-odds-api.com")
	cfg.Markets = []string{"h2h", "player_pass_tds"}
	err := cfg.withDefaults().Validate()
	if err == nil {
		t.Fatalf("a player prop was accepted in Markets")
	}
	if !strings.Contains(err.Error(), EndpointEventOdds) {
		t.Errorf("the error does not name the endpoint that does serve it: %v", err)
	}
}

// TestConfigRejectsABurstThatCouldSpendTheMonth.
func TestConfigRejectsABurstThatCouldSpendTheMonth(t *testing.T) {
	cfg := baseConfig("https://api.the-odds-api.com")
	cfg.Limiter.CreditBurst = cfg.Limiter.MonthlyCredits + 1
	if err := cfg.withDefaults().Validate(); err == nil {
		t.Fatalf("a burst larger than the whole monthly budget was accepted; that is the exact failure " +
			"the bucket exists to prevent")
	}
}

// TestDefaultsMatchADR0003 pins the defaults the quota arithmetic assumes.
func TestDefaultsMatchADR0003(t *testing.T) {
	cfg := Config{APIKey: testAPIKey, Limiter: LimiterConfig{MonthlyCredits: 100_000}}.withDefaults()

	if got, want := strings.Join(cfg.Markets, ","), "h2h,spreads,totals"; got != want {
		t.Errorf("default markets = %q, want %q (ADR 0003's M = 3)", got, want)
	}
	if got, want := strings.Join(cfg.Regions, ","), "us"; got != want {
		t.Errorf("default regions = %q, want %q (ADR 0003's R = 1)", got, want)
	}
	if got, want := cfg.SweepCost(), 3; got != want {
		t.Errorf("default sweep cost = %d credits, want %d — every scenario in ADR 0003 is computed "+
			"from this number", got, want)
	}
	if got, want := cfg.OddsFormat, OddsFormatDecimal; got != want {
		t.Errorf("default odds format = %q, want %q — Decimal is the domain's canonical price type, "+
			"so no conversion happens at the edge", got, want)
	}
	if got, want := cfg.ReferenceBook, DefaultReferenceBook; got != want {
		t.Errorf("default reference book = %q, want %q", got, want)
	}
	if cfg.IncludeInactiveSports {
		t.Errorf("out-of-season sports are included by default; a league that is not playing is a " +
			"league whose sweeps return nothing")
	}
	if len(cfg.PlayerPropMarkets) != 0 {
		t.Errorf("player props are enabled by default; ADR 0003 scenario E prices them at 6,144 credits "+
			"for one NFL Sunday: %v", cfg.PlayerPropMarkets)
	}
	if got, want := cfg.BaseURL, DefaultBaseURL; got != want {
		t.Errorf("default base URL = %q, want %q", got, want)
	}
}
