package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Length bounds. IDs and slugs are ASCII by construction so their bounds are in
// bytes; display names are user- and provider-facing text so their bound is in
// runes, and a Cyrillic or accented team name is not penalised for it.
const (
	// MaxIDLen is generous on purpose: provider identifiers are opaque and we
	// do not get to choose their shape. The Odds API emits 32-character hex
	// event ids and short keys such as "americanfootball_nfl"; 128 bytes leaves
	// four times that headroom without letting a malformed payload become an
	// unbounded map key.
	MaxIDLen = 128

	// MaxSlugLen bounds the human-authored keys ("nba", "draftkings").
	MaxSlugLen = 64

	// MaxNameLen bounds display names ("Los Angeles Dodgers").
	MaxNameLen = 160

	// errSampleLen caps how much of a rejected value appears in an error
	// message. Provider payloads are untrusted; echoing an unbounded string
	// into a log line is how a log becomes an attack surface.
	errSampleLen = 32
)

// Identifier types.
//
// Each entity gets its own defined type over string rather than sharing a bare
// string. That makes
//
//	func priceKey(EventID) string
//	priceKey(someMarketID)   // does not compile
//
// a compile error rather than a silent mis-keyed Kafka record or a WebSocket
// subscription that routes to the wrong channel. Those two call sites — the
// compacted `odds.normalized` key (CLAUDE.md §3) and the `event:{id}` /
// `market:{id}` subscription routing (§5) — are the entire reason this file
// exists.
//
// The one hole Go leaves is untyped string constants: priceKey("abc") still
// compiles, because an untyped constant converts implicitly. That is acceptable
// — the bug class this prevents is passing the *wrong variable*, and literal
// identifiers do not appear outside tests.
//
// The zero value of every ID type is the empty string, which no constructor
// will ever produce, so a zero ID is always detectable via IsZero.
type (
	// SportID identifies a Sport.
	SportID string
	// LeagueID identifies a League.
	LeagueID string
	// EventID identifies an Event.
	EventID string
	// MarketID identifies a Market.
	MarketID string
	// SelectionID identifies a Selection.
	SelectionID string
	// BookID identifies a Book.
	BookID string
	// CompetitorID identifies a Competitor — a team or an individual. It is
	// optional: providers frequently supply only a name.
	CompetitorID string
)

// Slug is a stable, human-readable key: "basketball", "nba", "draftkings". It
// is the value that appears in URLs, in `league:{slug}` subscriptions, and in
// operator-facing configuration, so unlike an opaque provider ID it is
// constrained to lowercase.
type Slug string

// validID enforces the identifier charset.
//
// The charset excludes ':' for a load-bearing reason: CLAUDE.md §5 defines
// WebSocket channels as `event:{id}` and `market:{id}`. If an identifier could
// contain a colon, splitting a channel name back into (kind, id) would be
// ambiguous, and the ambiguity would surface as cross-subscription leakage
// rather than as a parse error. Whitespace and control bytes are excluded for
// the same reason applied to log lines and Kafka keys.
func validID(s string) error {
	if s == "" {
		return ErrEmptyID
	}
	if len(s) > MaxIDLen {
		return ErrIDTooLong
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
		default:
			return ErrIDCharset
		}
	}
	return nil
}

// sample truncates an untrusted value for inclusion in an error message.
func sample(s string) string {
	if len(s) <= errSampleLen {
		return s
	}
	return s[:errSampleLen] + "…"
}

// idErr builds the contextual wrapper every ID constructor returns.
func idErr(kind, raw string, err error) error {
	return fmt.Errorf("%s %q: %w", kind, sample(raw), err)
}

// NewSportID validates and returns a SportID.
func NewSportID(s string) (SportID, error) {
	if err := validID(s); err != nil {
		return "", idErr("sport id", s, err)
	}
	return SportID(s), nil
}

// String returns the identifier as a bare string.
func (id SportID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id SportID) IsZero() bool { return id == "" }

// NewLeagueID validates and returns a LeagueID.
func NewLeagueID(s string) (LeagueID, error) {
	if err := validID(s); err != nil {
		return "", idErr("league id", s, err)
	}
	return LeagueID(s), nil
}

// String returns the identifier as a bare string.
func (id LeagueID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id LeagueID) IsZero() bool { return id == "" }

// NewEventID validates and returns an EventID.
func NewEventID(s string) (EventID, error) {
	if err := validID(s); err != nil {
		return "", idErr("event id", s, err)
	}
	return EventID(s), nil
}

// String returns the identifier as a bare string.
func (id EventID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id EventID) IsZero() bool { return id == "" }

// NewMarketID validates and returns a MarketID.
func NewMarketID(s string) (MarketID, error) {
	if err := validID(s); err != nil {
		return "", idErr("market id", s, err)
	}
	return MarketID(s), nil
}

// String returns the identifier as a bare string.
func (id MarketID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id MarketID) IsZero() bool { return id == "" }

// NewSelectionID validates and returns a SelectionID.
func NewSelectionID(s string) (SelectionID, error) {
	if err := validID(s); err != nil {
		return "", idErr("selection id", s, err)
	}
	return SelectionID(s), nil
}

// String returns the identifier as a bare string.
func (id SelectionID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id SelectionID) IsZero() bool { return id == "" }

// NewBookID validates and returns a BookID.
func NewBookID(s string) (BookID, error) {
	if err := validID(s); err != nil {
		return "", idErr("book id", s, err)
	}
	return BookID(s), nil
}

// String returns the identifier as a bare string.
func (id BookID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id BookID) IsZero() bool { return id == "" }

// NewCompetitorID validates and returns a CompetitorID.
func NewCompetitorID(s string) (CompetitorID, error) {
	if err := validID(s); err != nil {
		return "", idErr("competitor id", s, err)
	}
	return CompetitorID(s), nil
}

// String returns the identifier as a bare string.
func (id CompetitorID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id CompetitorID) IsZero() bool { return id == "" }

// NewSlug validates and returns a Slug.
//
// A slug is lowercase [a-z0-9] followed by any of [a-z0-9_-]. Leading with a
// letter or digit keeps a slug from ever looking like a flag or a relative
// path when it is interpolated into a URL or a CLI argument. Uppercase is
// rejected rather than folded, because folding makes "NBA" and "nba" two
// spellings of one key and someone will eventually compare the unfolded forms.
func NewSlug(s string) (Slug, error) {
	if s == "" {
		return "", idErr("slug", s, ErrEmptySlug)
	}
	if len(s) > MaxSlugLen {
		return "", idErr("slug", s, ErrSlugTooLong)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 {
			if !alnum {
				return "", idErr("slug", s, ErrSlugCharset)
			}
			continue
		}
		if !alnum && c != '-' && c != '_' {
			return "", idErr("slug", s, ErrSlugCharset)
		}
	}
	return Slug(s), nil
}

// String returns the slug as a bare string.
func (s Slug) String() string { return string(s) }

// IsZero reports whether the slug is unset.
func (s Slug) IsZero() bool { return s == "" }

// validateName normalises and validates a display name.
//
// Surrounding whitespace is trimmed rather than rejected: provider payloads
// routinely carry it and failing an entire event over a trailing space would be
// a worse outcome than normalising it. Trimming is deterministic, so the
// function stays pure.
func validateName(kind, s string) (string, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", fmt.Errorf("%s %q: %w", kind, sample(s), ErrEmptyName)
	}
	if !utf8.ValidString(t) {
		return "", fmt.Errorf("%s %q: %w", kind, sample(s), ErrNameCharset)
	}
	if utf8.RuneCountInString(t) > MaxNameLen {
		return "", fmt.Errorf("%s %q: %w", kind, sample(s), ErrNameTooLong)
	}
	for _, r := range t {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%s %q: %w", kind, sample(s), ErrNameCharset)
		}
	}
	return t, nil
}
