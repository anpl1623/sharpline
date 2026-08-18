// Deterministic identifier derivation.
//
// # Why this file is the most dangerous one in the package
//
// odds.normalized is compacted and keyed by market_id. The broker keeps the
// latest record per key, which is what makes the topic a replayable snapshot of
// every current line (CLAUDE.md §3). That property is worth exactly as much as
// the stability of the key: if one market's identifier differs between two polls,
// or between two runs of the same binary, compaction stops collapsing and starts
// accumulating, and it does so silently — the topic still serves records, there
// are just now two of them for one market and one of them is frozen for ever.
//
// So every identifier here is a pure function of provider-stable attributes.
// Nothing in this file reads a clock, a random source, a map in iteration order,
// an environment variable, or a counter. identity_test.go checks the pure-function
// property directly and, for the cross-restart claim, by re-deriving in a
// subprocess.
//
// # The shape, and what it buys
//
//	sport      the-odds-api.s.americanfootball
//	league     the-odds-api.l.americanfootball_nfl
//	book       the-odds-api.b.draftkings
//	event      the-odds-api.e.42db668449664943833b5c04a583328a
//	market     the-odds-api.e.42db66…328a.m.spreads
//	selection  the-odds-api.e.42db66…328a.m.spreads.x.home
//
// Readable, hierarchical, and greppable: an operator looking at a WebSocket
// channel name `market:the-odds-api.e.42db….m.spreads` knows the provider, the
// event and the market type without a lookup. That is worth real effort, because
// the alternative — an opaque hash — makes every production question start with a
// join.
//
// Readability is best-effort and correctness is not. Any component that would not
// fit its budget, or that contains a byte outside [A-Za-z0-9_-], is replaced by
// `h` + 48 bits of SHA-256. `.` is excluded from the readable charset even though
// domain.validID permits it, because `.` is this scheme's separator and letting a
// provider inject one would make the structure ambiguous to the human reading it.
//
// # Budgets
//
// domain.MaxIDLen is 128 bytes and identifiers nest, so the budgets are chosen so
// that the longest possible selection identifier still fits:
//
//	provider  ≤ 16                                              16
//	event     = provider + ".e." + ≤35                          54
//	market    = event    + ".m." + ≤28 [+ "." + 13]             99
//	selection = market   + ".x." + ≤8  [+ "." + 13]            123
//
// The provider budget is the only one enforced by REJECTION rather than by
// hashing: the provider slug is the suffix of the odds.raw.{provider} topic name
// and is declared in Terraform, so a provider whose slug does not fit is a
// misconfiguration to fail on at startup, not a value to quietly mangle.
package normalizer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// Component budgets. See the file comment for the arithmetic that fixes them.
const (
	// MaxProviderLen bounds the provider slug inside an identifier.
	MaxProviderLen = 16

	eventKeyBudget  = 35
	marketKeyBudget = 28
	shortKeyBudget  = 40
	nameBudget      = 24
	subjectBudget   = 24

	// hashHexLen is how many hex characters of SHA-256 a hashed component keeps.
	// 48 bits, and every hash here is scoped to a population of at most a few
	// hundred (the subjects within one event's one market key; the runner names
	// within one market), so the birthday bound is not close to being a concern.
	hashHexLen = 12
)

// Component tags. One letter each, so they cost almost nothing in the budget and
// are still unambiguous when read.
const (
	tagSport     = "s"
	tagLeague    = "l"
	tagBook      = "b"
	tagEvent     = "e"
	tagMarket    = "m"
	tagSelection = "x"
)

// ValidateProviderForIdentity checks that a provider slug can appear inside a
// derived identifier.
//
// Called by NewMapper so a misconfigured provider fails at construction rather
// than at the first record, per CLAUDE.md §12's "fail fast and loudly on a bad
// config".
func ValidateProviderForIdentity(p kafka.Provider) error {
	v, err := kafka.NewProvider(string(p))
	if err != nil {
		return err
	}
	if len(v) > MaxProviderLen {
		return fmt.Errorf("provider %q is %d bytes, identifier budget is %d",
			string(v), len(v), MaxProviderLen)
	}
	return nil
}

// SportIDFor derives the identifier for a sport from the provider's sport key.
func SportIDFor(p kafka.Provider, sportKey string) (domain.SportID, error) {
	return domain.NewSportID(join(string(p), tagSport, token(sportKey, shortKeyBudget)))
}

// LeagueIDFor derives the identifier for a league from the provider's league key
// ("americanfootball_nfl").
func LeagueIDFor(p kafka.Provider, leagueKey string) (domain.LeagueID, error) {
	return domain.NewLeagueID(join(string(p), tagLeague, token(leagueKey, shortKeyBudget)))
}

// BookIDFor derives the identifier for a bookmaker from the provider's bookmaker
// key ("draftkings").
func BookIDFor(p kafka.Provider, bookKey string) (domain.BookID, error) {
	return domain.NewBookID(join(string(p), tagBook, token(bookKey, shortKeyBudget)))
}

// EventIDFor derives the identifier for an event from the provider's own event
// identifier.
//
// It is exported because `ingest` needs it too: odds.raw.{provider} is keyed by
// EventID (kafka/topics.go), so the raw producer and this package must agree on
// the derivation exactly. Two implementations of one identifier is the failure
// this export exists to prevent.
func EventIDFor(p kafka.Provider, providerEventID string) (domain.EventID, error) {
	if strings.TrimSpace(providerEventID) == "" {
		return "", fmt.Errorf("event key: %w", domain.ErrEmptyID)
	}
	return domain.NewEventID(join(string(p), tagEvent, token(providerEventID, eventKeyBudget)))
}

// MarketIDFor derives the identifier for a market.
//
// The inputs are the event, the PROVIDER's market key, and the subject. The
// market key rather than the mapped domain.MarketType, because two provider keys
// can map to one domain type ("player_pass_tds" and "player_rush_yds" are both
// domain.MarketTypePlayerProp about the same player) and collapsing them would
// give two genuinely different markets one identifier — which on a compacted
// topic means they overwrite each other for ever.
//
// THE LINE IS NOT AN INPUT, and that is a decision rather than an omission. A
// market whose line moves from -3.5 to -4 is the same market —
// domain.Market.WithLine exists for that transition — and folding the line into
// the key would issue a new identifier on every move, shattering the line-movement
// series that CLAUDE.md §6 charts and §9's CLV is computed from, and leaving one
// orphaned compacted key behind per move.
func MarketIDFor(eventID domain.EventID, marketKey, subject string) (domain.MarketID, error) {
	if eventID.IsZero() {
		return "", fmt.Errorf("market id: event id: %w", domain.ErrEmptyID)
	}
	if strings.TrimSpace(marketKey) == "" {
		return "", fmt.Errorf("market key: %w", domain.ErrEmptyID)
	}
	s := join(eventID.String(), tagMarket, token(marketKey, marketKeyBudget))
	if subject != "" {
		s += "." + hashOf(subject)
	}
	return domain.NewMarketID(s)
}

// SelectionIDFor derives the identifier for a selection.
//
// For the fixed roles — home, away, draw, over, under — the role alone
// distinguishes the selections of a market, so the identifier ends there and
// stays readable. For an outright the role is the same on every runner, so the
// runner's name is hashed in; the name is the only thing that identifies a runner
// (domain.SelectionParams.Name says so), and it is provider text, so it is hashed
// rather than embedded.
func SelectionIDFor(marketID domain.MarketID, role domain.SelectionRole, name string) (domain.SelectionID, error) {
	if marketID.IsZero() {
		return "", fmt.Errorf("selection id: market id: %w", domain.ErrEmptyID)
	}
	if !role.Valid() {
		return "", fmt.Errorf("selection id: %w", domain.ErrUnknownSelectionRole)
	}
	s := join(marketID.String(), tagSelection, role.String())
	if role == domain.SelectionRoleOutright {
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("outright selection name: %w", domain.ErrEmptyName)
		}
		s += "." + hashOf(name)
	}
	return domain.NewSelectionID(s)
}

// SportKeyFromLeagueKey derives a sport key from a league key.
//
// The Odds API composes its league keys as {sport}_{league}:
// "americanfootball_nfl", "basketball_nba", "icehockey_nhl". Taking the prefix is
// therefore the provider's own grouping rather than a guess, and a key with no
// separator is its own sport, which is the right answer for a single-league sport.
//
// It is only a fallback: RawEvent.SportKey is preferred when the adapter supplies
// one, because /v4/sports carries an explicit `group` and costs zero credits
// (ADR 0003).
func SportKeyFromLeagueKey(leagueKey string) string {
	if i := strings.IndexByte(leagueKey, '_'); i > 0 {
		return leagueKey[:i]
	}
	return leagueKey
}

// SlugFor derives a human-facing slug from a provider key.
//
// Slugs are what appear in URLs and in `league:{slug}` subscriptions (CLAUDE.md
// §5), so unlike an identifier they are lowercased and hyphen-normalised rather
// than kept verbatim.
//
// # The namespace, and the collision it exists to prevent
//
// leagues.slug and books.slug are UNIQUE GLOBALLY in the schema (migrations/00002:
// "UNIQUE is what makes domain.SyntheticBookSlug usable as a stable handle"). Two
// providers that use the same key for the same real-world league — very likely,
// since a synthetic generator naturally imitates "americanfootball_nfl" — derive
// the same slug but DIFFERENT identifiers, and the second one to be written
// violates the unique constraint.
//
// The namespace is how a deployment keeps them apart: leave it empty for the real
// provider so the slugs stay clean, and set it for the synthetic one. It is
// explicit rather than derived from the provider slug because prefixing
// unconditionally would make every URL carry "the-odds-api-" for no benefit in
// the overwhelmingly common single-provider case.
func SlugFor(namespace, key string) (domain.Slug, error) {
	s := slugify(key)
	if namespace != "" {
		s = slugify(namespace) + "-" + s
	}
	if len(s) > domain.MaxSlugLen {
		s = s[:domain.MaxSlugLen]
		s = strings.TrimRight(s, "-_")
	}
	return domain.NewSlug(s)
}

// slugify lowercases, folds every byte outside [a-z0-9] to '-', collapses runs,
// trims the ends, and guarantees a leading alphanumeric.
//
// A key that folds away to nothing — one made entirely of punctuation, or of
// non-ASCII — becomes its hash rather than an error, because a slug is a display
// and routing concern and failing an entire league over an unfortunate name would
// trade a cosmetic problem for a data-loss one.
func slugify(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	prevDash := false
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
			prevDash = false
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
			prevDash = false
		case c == '_':
			// Underscores survive: domain.NewSlug permits them and the provider's
			// own "americanfootball_nfl" is more recognisable kept than folded.
			b.WriteByte(c)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-_")
	if s == "" {
		return hashOf(key)
	}
	// domain.NewSlug requires the first byte to be alphanumeric.
	if c := s[0]; (c < 'a' || c > 'z') && (c < '0' || c > '9') {
		s = hashOf(key)
	}
	return s
}

// join concatenates identifier components with the scheme's separator.
func join(parts ...string) string { return strings.Join(parts, ".") }

// token returns raw when it is safe and short enough to embed verbatim, and its
// hash otherwise.
//
// "Safe" excludes '.', which domain.validID permits but this scheme reserves as
// its separator.
func token(raw string, budget int) string {
	if raw == "" || len(raw) > budget || !embeddable(raw) {
		return hashOf(raw)
	}
	return raw
}

// embeddable reports whether s may appear verbatim in an identifier.
func embeddable(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// hashOf returns the `h`-prefixed truncated SHA-256 of s.
//
// The prefix is not decoration: without it a hashed component could begin with a
// digit and be mistaken for a provider's own numeric key, and `h` makes "this was
// too long or too strange to embed" visible at a glance in a log line.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "h" + hex.EncodeToString(sum[:])[:hashHexLen]
}
