package theoddsapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the provider's documented host (guides/v4 §Host; the
// OpenAPI document's single `servers` entry).
//
// The provider also publishes https://ipv6-api.the-odds-api.com for callers
// that require IPv6. It is not the default because it is not the documented
// primary, but BaseURL is configuration precisely so a deployment on an
// IPv6-only network can point at it without a code change.
const DefaultBaseURL = "https://api.the-odds-api.com"

// Endpoint path TEMPLATES. These are the metric label values and the span
// names, so they must be bounded — never a concrete path with a sport key or an
// event id substituted in.
const (
	EndpointSports    = "/v4/sports"
	EndpointOdds      = "/v4/sports/{sport}/odds"
	EndpointEventOdds = "/v4/sports/{sport}/events/{eventId}/odds"
)

// Defaults for the HTTP layer.
const (
	// DefaultRequestTimeout bounds ONE attempt. CLAUDE.md §12: "every external
	// call has a timeout".
	DefaultRequestTimeout = 15 * time.Second

	// DefaultMaxAttempts includes the first try, so 3 means at most two
	// retries. Retries are only ever issued for 429, 5xx and transport
	// failures — never for a quota exhaustion or a bad key.
	DefaultMaxAttempts = 3

	// DefaultMaxResponseBytes caps the body this package will read. A full
	// multi-league sweep with a dozen books is a few megabytes; 32 MiB leaves
	// an order of magnitude of headroom while still refusing to buffer an
	// unbounded response into the heap of a container with a memory limit.
	DefaultMaxResponseBytes = 32 << 20

	// errorBodyExcerpt is how much of an error response is kept for the error
	// message. Enough to carry a documented error code and a sentence of
	// explanation, short enough not to dump a page of HTML from a proxy into
	// the logs.
	errorBodyExcerpt = 512
)

// FeaturedMarkets are the market keys the /odds endpoint accepts, and they are
// exactly the three the charter's board shows (CLAUDE.md §6: moneyline, spread,
// total) plus outrights for futures sports.
//
// The provider's INVALID_MARKET documentation is explicit that non-featured
// markets — player props, period markets, alternate lines — are rejected by
// this endpoint and must be requested one event at a time from the event-odds
// endpoint. That restriction is enforced here at config-validation time rather
// than discovered as a 422 in production.
var FeaturedMarkets = []string{"h2h", "spreads", "totals", "outrights"}

// DefaultReferenceBook is the bookmaker key marked as the sharp reference when
// none is configured. See Config.ReferenceBook.
const DefaultReferenceBook = "pinnacle"

// Regions are the documented values of the `regions` parameter (OpenAPI:
// regions enum).
var Regions = []string{"uk", "us", "us2", "us_dfs", "us_ex", "eu", "au"}

// Config is the adapter's typed configuration, validated at construction.
//
// CLAUDE.md §12: "Config via environment variables with a typed struct and
// startup validation — fail fast and loudly on a bad config." Every problem is
// reported in one error rather than one per construction attempt.
type Config struct {
	// APIKey is the provider credential, read from ODDS_API_KEY.
	//
	// It is never logged, never placed in an error message, never put in a
	// span attribute and never used as a metric label. It appears in exactly
	// one place: the apiKey query parameter of an outbound request. See doc.go.
	APIKey string

	// BaseURL defaults to DefaultBaseURL. Overridable so tests can point at an
	// httptest server and so an IPv6-only deployment can use the provider's
	// alternate host.
	BaseURL string

	// Regions selects which bookmakers appear. Ignored by the provider when
	// Bookmakers is set — "if both bookmakers and regions are specified,
	// bookmakers takes precedence".
	Regions []string

	// Bookmakers names specific books. ADR 0003 requirement 1 prefers this to
	// Regions for cross-region coverage: "Every group of 10 bookmakers counts
	// as 1 request", so ten books spanning us/us2/eu cost one region-equivalent
	// where regions=us,us2,eu costs three.
	Bookmakers []string

	// Markets is the featured market set for a sweep. Defaults to
	// h2h,spreads,totals — ADR 0003's M = 3.
	Markets []string

	// PlayerPropMarkets names the non-featured market keys the per-event
	// endpoint may be asked for ("player_pass_tds", "player_rush_yds").
	//
	// It is EMPTY by default and that is a cost decision, not an oversight. ADR
	// 0003 scenario E: props are billed per event, so "one afternoon of NFL
	// player props costs 6,144 credits — 6.1% of the entire 100K monthly tier,
	// spent in four hours", and its verdict is that "player props are a 5M-tier
	// feature". A scope asking for domain.MarketTypePlayerProp with this unset
	// is refused with ErrNotSupported rather than quietly draining the budget.
	//
	// Members must NOT be featured markets: the two endpoints have different
	// cost models and mixing them would price the request wrongly.
	PlayerPropMarkets []string

	// ReferenceBook is the provider bookmaker key treated as the sharp
	// reference that CLAUDE.md §6's positive-EV finder prices against.
	//
	// It is configuration rather than a constant because which book is sharp is
	// a judgement that changes, and because a deployment whose plan does not
	// carry that book must be able to name another one without a code change.
	// At most one book may claim it — provider.Catalogue.Validate enforces that
	// — so this is a single key and not a list.
	ReferenceBook string

	// IncludeInactiveSports passes `all=true` to /v4/sports, which returns
	// out-of-season leagues as well as in-season ones.
	//
	// Off by default: an out-of-season league is a league whose sweeps return an
	// empty slate, and while the provider does not bill an empty response, the
	// scheduler would still be spending frequency slots and wall time on it.
	// The endpoint is free either way, so this is a scheduling decision rather
	// than a budget one.
	IncludeInactiveSports bool

	// OddsFormat defaults to decimal, which is the domain's canonical price
	// representation, so no conversion happens at the edge.
	OddsFormat OddsFormat

	// DateFormat defaults to iso.
	DateFormat DateFormat

	// RequestTimeout bounds one attempt. Defaults to DefaultRequestTimeout.
	RequestTimeout time.Duration

	// MaxAttempts is the total number of tries for a retryable failure.
	// Defaults to DefaultMaxAttempts.
	MaxAttempts int

	// MaxResponseBytes caps a response body. Defaults to
	// DefaultMaxResponseBytes.
	MaxResponseBytes int64

	// UserAgent identifies this client to the provider. A blank one gets Go's
	// default, which tells an operator on their side nothing.
	UserAgent string

	// Limiter configures the two token buckets. MonthlyCredits is the budget
	// CLAUDE.md §5 requires be a config value.
	Limiter LimiterConfig
}

// Validation bounds.
const (
	// minAPIKeyLen is a sanity floor, not the provider's format. The OpenAPI
	// document describes the key as 40 characters; anything under 16 is a
	// placeholder like "changeme" that would otherwise fail as a confusing 401
	// at the first poll instead of loudly at startup.
	minAPIKeyLen = 16

	maxRequestTimeout = 60 * time.Second
	maxAttemptsCap    = 10
)

// ErrInvalidConfig is returned by Validate. It is separate from
// ErrInvalidRequest because the remedies differ: this one is fixed before the
// process starts, that one after the provider has already refused something.
var ErrInvalidConfig = errors.New("theoddsapi: invalid configuration")

// withDefaults returns a copy of cfg with every unset optional field filled in.
func (c Config) withDefaults() Config {
	out := c
	if strings.TrimSpace(out.BaseURL) == "" {
		out.BaseURL = DefaultBaseURL
	}
	if len(out.Markets) == 0 {
		// ADR 0003's M = 3: the charter's core board is moneyline, spread and
		// total (CLAUDE.md §6).
		out.Markets = []string{"h2h", "spreads", "totals"}
	}
	if len(out.Regions) == 0 && len(out.Bookmakers) == 0 {
		// The provider requires one or the other; `us` is ADR 0003's R = 1.
		out.Regions = []string{"us"}
	}
	if out.OddsFormat == "" {
		out.OddsFormat = OddsFormatDecimal
	}
	if strings.TrimSpace(out.ReferenceBook) == "" {
		// Pinnacle is the book the +EV literature treats as the sharp
		// reference. It is a DEFAULT, not an assumption that the deployment's
		// plan carries it: a book that never appears in a payload simply never
		// gets the flag, and the catalogue then has no reference book, which
		// provider.Catalogue.ReferenceBook reports honestly as absent.
		out.ReferenceBook = DefaultReferenceBook
	}
	if out.DateFormat == "" {
		out.DateFormat = DateFormatISO
	}
	if out.RequestTimeout <= 0 {
		out.RequestTimeout = DefaultRequestTimeout
	}
	if out.MaxAttempts <= 0 {
		out.MaxAttempts = DefaultMaxAttempts
	}
	if out.MaxResponseBytes <= 0 {
		out.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if strings.TrimSpace(out.UserAgent) == "" {
		out.UserAgent = "sharpline-ingest/1 (+https://github.com/anpl1623/sharpline)"
	}
	return out
}

// Validate reports every problem with the configuration at once.
func (c Config) Validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if strings.TrimSpace(c.APIKey) == "" {
		add("APIKey is empty — set %s, or run the synthetic adapter instead", envAPIKey)
	} else if len(c.APIKey) < minAPIKeyLen {
		// The LENGTH is reported, never the value.
		add("APIKey is %d bytes, want at least %d (the provider documents a 40-character key)",
			len(c.APIKey), minAPIKeyLen)
	}

	if u, err := url.Parse(c.BaseURL); err != nil {
		add("BaseURL is not a URL: %v", err)
	} else if u.Scheme != "http" && u.Scheme != "https" {
		add("BaseURL scheme %q is neither http nor https", u.Scheme)
	} else if u.Host == "" {
		add("BaseURL has no host")
	}

	if len(c.Regions) == 0 && len(c.Bookmakers) == 0 {
		add("one of Regions or Bookmakers must be set — the provider's MISSING_REGION error is otherwise certain")
	}
	for _, r := range c.Regions {
		if !contains(Regions, r) {
			add("region %q is not one of the documented regions %v", r, Regions)
		}
	}

	if len(c.Markets) == 0 {
		add("Markets is empty")
	}
	for _, m := range c.Markets {
		if !contains(FeaturedMarkets, m) {
			// The provider's own INVALID_MARKET page: the odds endpoint
			// "only support[s] featured markets".
			add("market %q is not a featured market %v — non-featured markets such as player props "+
				"must be requested per event from %s", m, FeaturedMarkets, EndpointEventOdds)
		}
	}

	for _, m := range c.PlayerPropMarkets {
		switch {
		case strings.TrimSpace(m) == "":
			add("PlayerPropMarkets contains an empty entry")
		case contains(FeaturedMarkets, m):
			// The two endpoints bill differently — a sweep is markets × regions
			// regardless of slate size, a per-event call is markets × regions
			// PER EVENT — so a key in both lists makes the predicted cost
			// depend on which path happened to run.
			add("PlayerPropMarkets contains the featured market %q — featured markets belong in Markets, "+
				"and the two endpoints have different cost models", m)
		case m != strings.ToLower(m):
			add("PlayerPropMarkets entry %q is not lowercase; the provider's market keys are", m)
		}
	}

	if rb := strings.TrimSpace(c.ReferenceBook); rb != "" && rb != strings.ToLower(rb) {
		add("ReferenceBook %q is not lowercase; the provider's bookmaker keys are", rb)
	}

	if !c.OddsFormat.Valid() {
		add("OddsFormat %q is neither %q nor %q", string(c.OddsFormat), OddsFormatDecimal, OddsFormatAmerican)
	}
	if !c.DateFormat.Valid() {
		add("DateFormat %q is neither %q nor %q", string(c.DateFormat), DateFormatISO, DateFormatUnix)
	}

	if c.RequestTimeout <= 0 {
		add("RequestTimeout must be positive")
	} else if c.RequestTimeout > maxRequestTimeout {
		add("RequestTimeout %s exceeds %s — a poll that slow is a poll that has already missed its cadence",
			c.RequestTimeout, maxRequestTimeout)
	}
	if c.MaxAttempts < 1 {
		add("MaxAttempts must be at least 1")
	} else if c.MaxAttempts > maxAttemptsCap {
		add("MaxAttempts %d exceeds %d — each attempt can cost credits", c.MaxAttempts, maxAttemptsCap)
	}
	if c.MaxResponseBytes <= 0 {
		add("MaxResponseBytes must be positive")
	}

	if c.Limiter.MonthlyCredits <= 0 {
		add("Limiter.MonthlyCredits must be positive — CLAUDE.md §5 requires the quota budget be a config value, "+
			"and a zero budget refuses every priced request. ADR 0003's recommended tier is 100000 (%s)",
			envMonthlyCredits)
	}
	if c.Limiter.RequestsPerSecond > DocumentedRequestsPerSecond {
		add("Limiter.RequestsPerSecond %.2f exceeds the provider's documented ceiling of %.0f/s",
			c.Limiter.RequestsPerSecond, DocumentedRequestsPerSecond)
	}
	if c.Limiter.CreditBurst < 0 {
		add("Limiter.CreditBurst must not be negative")
	}
	if c.Limiter.CreditBurst > c.Limiter.MonthlyCredits && c.Limiter.MonthlyCredits > 0 {
		add("Limiter.CreditBurst %d exceeds Limiter.MonthlyCredits %d — the bucket could spend the whole "+
			"month's budget in one burst, which is the failure the bucket exists to prevent",
			c.Limiter.CreditBurst, c.Limiter.MonthlyCredits)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalidConfig, errors.Join(problems...))
}

// SweepCost returns the credit cost of one sweep under this configuration.
func (c Config) SweepCost() int {
	return SweepCost(len(c.Markets), len(c.Regions), len(c.Bookmakers))
}

// LogValue implements slog.LogValuer so that logging a whole Config cannot leak
// the credential. The key is reported as present/absent and by length, never by
// value — exactly the shape internal/platform/config uses for every other
// secret in this repository. TestConfigLogValueNeverLeaksKey asserts it.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", c.BaseURL),
		slog.Bool("api_key_set", c.APIKey != ""),
		slog.Int("api_key_len", len(c.APIKey)),
		slog.Any("regions", c.Regions),
		slog.Any("bookmakers", c.Bookmakers),
		slog.Any("markets", c.Markets),
		slog.Any("player_prop_markets", c.PlayerPropMarkets),
		slog.String("reference_book", c.ReferenceBook),
		slog.Bool("include_inactive_sports", c.IncludeInactiveSports),
		slog.String("odds_format", string(c.OddsFormat)),
		slog.String("date_format", string(c.DateFormat)),
		slog.Duration("request_timeout", c.RequestTimeout),
		slog.Int("max_attempts", c.MaxAttempts),
		slog.Int64("monthly_credits", c.Limiter.MonthlyCredits),
		slog.Int64("credit_burst", c.Limiter.CreditBurst),
		slog.Float64("requests_per_second", c.Limiter.RequestsPerSecond),
		slog.Int("sweep_cost_credits", c.SweepCost()),
	)
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
