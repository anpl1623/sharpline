package domain

import (
	"errors"
	"strings"
	"testing"
)

// TestValidIDCharset covers the shared identifier rule every ID constructor
// delegates to.
func TestValidIDCharset(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		// Shapes real providers actually emit.
		{"the odds api event id", "e912304de2b2ce35b473ce2ecd3d1502", nil},
		{"the odds api sport key", "americanfootball_nfl", nil},
		{"bookmaker key", "draftkings", nil},
		{"uuid with hyphens", "9f8b7c6d-1234-4a5b-8c9d-0e1f2a3b4c5d", nil},
		{"dotted key", "nba.2026.finals", nil},
		{"single character", "a", nil},
		{"digits only", "1234567890", nil},
		{"mixed case", "AbC123", nil},
		{"max length", strings.Repeat("a", MaxIDLen), nil},

		{"empty", "", ErrEmptyID},
		{"one over max length", strings.Repeat("a", MaxIDLen+1), ErrIDTooLong},

		// The colon rejection is load-bearing: WebSocket channels are
		// `event:{id}`, so an id containing a colon would make the channel name
		// ambiguous to parse.
		{"colon", "event:1", ErrIDCharset},
		{"space", "los angeles", ErrIDCharset},
		{"tab", "a\tb", ErrIDCharset},
		{"newline", "a\nb", ErrIDCharset},
		{"null byte", "a\x00b", ErrIDCharset},
		{"slash", "a/b", ErrIDCharset},
		{"percent", "a%20b", ErrIDCharset},
		{"non-ascii", "córdoba", ErrIDCharset},
		{"emoji", "a🏀b", ErrIDCharset},
		{"sql-ish", "1'; drop table prices--", ErrIDCharset},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validID(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("validID(%q) = %v, want %v", tc.in, err, tc.want)
			}
			if tc.want != nil && !errors.Is(err, ErrInvalid) {
				t.Errorf("validID(%q) error does not reach ErrInvalid", tc.in)
			}
		})
	}
}

// TestIDConstructorsAgreeOnTheRule runs one accepted and one rejected value
// through every ID constructor, so a constructor added later without wiring in
// validID is caught.
func TestIDConstructorsAgreeOnTheRule(t *testing.T) {
	const good = "abc-123"
	const bad = "abc:123"

	ctors := []struct {
		name string
		fn   func(string) (string, error)
	}{
		{"SportID", func(s string) (string, error) { v, err := NewSportID(s); return v.String(), err }},
		{"LeagueID", func(s string) (string, error) { v, err := NewLeagueID(s); return v.String(), err }},
		{"EventID", func(s string) (string, error) { v, err := NewEventID(s); return v.String(), err }},
		{"MarketID", func(s string) (string, error) { v, err := NewMarketID(s); return v.String(), err }},
		{"SelectionID", func(s string) (string, error) { v, err := NewSelectionID(s); return v.String(), err }},
		{"BookID", func(s string) (string, error) { v, err := NewBookID(s); return v.String(), err }},
		{"CompetitorID", func(s string) (string, error) { v, err := NewCompetitorID(s); return v.String(), err }},
	}

	for _, c := range ctors {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.fn(good)
			if err != nil {
				t.Fatalf("New%s(%q) = %v", c.name, good, err)
			}
			if got != good {
				t.Errorf("New%s(%q).String() = %q", c.name, good, got)
			}

			got, err = c.fn(bad)
			if !errors.Is(err, ErrIDCharset) {
				t.Fatalf("New%s(%q) error = %v, want ErrIDCharset", c.name, bad, err)
			}
			if got != "" {
				t.Errorf("New%s rejected the input but returned %q, not the zero value", c.name, got)
			}
			if !strings.Contains(err.Error(), strings.ToLower(strings.TrimSuffix(c.name, "ID"))) {
				t.Errorf("New%s error %q does not name the entity", c.name, err)
			}
		})
	}
}

// TestIDZeroValues confirms IsZero is a reliable "unset" test: no constructor
// can produce a zero ID, so the zero value is unambiguous.
func TestIDZeroValues(t *testing.T) {
	if !EventID("").IsZero() || !MarketID("").IsZero() || !SelectionID("").IsZero() ||
		!BookID("").IsZero() || !SportID("").IsZero() || !LeagueID("").IsZero() ||
		!CompetitorID("").IsZero() {
		t.Error("the empty identifier does not report IsZero")
	}
	id, err := NewEventID("x")
	if err != nil {
		t.Fatalf("NewEventID: %v", err)
	}
	if id.IsZero() {
		t.Error("a constructed identifier reports IsZero")
	}
}

// TestIDErrorsTruncateUntrustedInput checks the error message does not echo an
// unbounded provider string into a log line.
func TestIDErrorsTruncateUntrustedInput(t *testing.T) {
	long := strings.Repeat("!", 4096)
	_, err := NewEventID(long)
	if err == nil {
		t.Fatal("NewEventID accepted 4096 exclamation marks")
	}
	if len(err.Error()) > 200 {
		t.Errorf("error message is %d bytes; untrusted input is not being truncated", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "…") {
		t.Errorf("error %q does not show the truncation marker", err)
	}
}

func TestNewSlug(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"league slug", "nba", nil},
		{"sport slug", "americanfootball", nil},
		{"book slug", "draftkings", nil},
		{"hyphenated", "premier-league", nil},
		{"underscored", "americanfootball_nfl", nil},
		{"leading digit", "2026-world-cup", nil},
		{"single char", "a", nil},
		{"max length", strings.Repeat("a", MaxSlugLen), nil},

		{"empty", "", ErrEmptySlug},
		{"too long", strings.Repeat("a", MaxSlugLen+1), ErrSlugTooLong},

		// Uppercase is rejected rather than folded so that "NBA" and "nba"
		// never become two spellings of one key.
		{"uppercase", "NBA", ErrSlugCharset},
		{"mixed case", "premierLeague", ErrSlugCharset},
		{"leading hyphen", "-nba", ErrSlugCharset},
		{"leading underscore", "_nba", ErrSlugCharset},
		{"dot", "nba.com", ErrSlugCharset},
		{"space", "premier league", ErrSlugCharset},
		{"colon", "league:nba", ErrSlugCharset},
		{"slash", "sport/nba", ErrSlugCharset},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewSlug(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewSlug(%q) = %v, want %v", tc.in, err, tc.want)
			}
			if tc.want == nil {
				if got.String() != tc.in {
					t.Errorf("NewSlug(%q).String() = %q", tc.in, got)
				}
				if got.IsZero() {
					t.Error("a constructed slug reports IsZero")
				}
				return
			}
			if !got.IsZero() {
				t.Errorf("NewSlug rejected the input but returned %q", got)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{"plain", "Boston Celtics", "Boston Celtics", nil},
		{"accented", "Nikola Jokić", "Nikola Jokić", nil},
		{"cyrillic", "Динамо Москва", "Динамо Москва", nil},
		{"punctuation", "St. Louis Blues", "St. Louis Blues", nil},
		{"ampersand", "Brighton & Hove Albion", "Brighton & Hove Albion", nil},

		// Providers ship padded strings routinely; failing an entire event over
		// a trailing space would be a worse outcome than normalising it.
		{"trailing space trimmed", "Boston Celtics  ", "Boston Celtics", nil},
		{"leading space trimmed", "\tBoston Celtics", "Boston Celtics", nil},
		{"surrounding newline trimmed", "\nBoston Celtics\n", "Boston Celtics", nil},

		{"empty", "", "", ErrEmptyName},
		{"whitespace only", "   \t\n ", "", ErrEmptyName},
		{"interior control char", "Boston\x07Celtics", "", ErrNameCharset},
		{"interior null", "Boston\x00Celtics", "", ErrNameCharset},
		{"delete char", "Boston\x7fCeltics", "", ErrNameCharset},
		{"invalid utf-8", "Boston\xffCeltics", "", ErrNameCharset},
		{"too long", strings.Repeat("a", MaxNameLen+1), "", ErrNameTooLong},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateName("test name", tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validateName(%q) error = %v, want %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("validateName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNameLengthIsCountedInRunes confirms a multi-byte name is not penalised
// for its encoding: MaxNameLen runes of Cyrillic is accepted even though it is
// twice that many bytes.
func TestNameLengthIsCountedInRunes(t *testing.T) {
	atLimit := strings.Repeat("я", MaxNameLen)
	if _, err := validateName("test name", atLimit); err != nil {
		t.Errorf("a %d-rune name was rejected: %v", MaxNameLen, err)
	}
	overLimit := strings.Repeat("я", MaxNameLen+1)
	if _, err := validateName("test name", overLimit); !errors.Is(err, ErrNameTooLong) {
		t.Errorf("a %d-rune name error = %v, want ErrNameTooLong", MaxNameLen+1, err)
	}
}

// TestSampleTruncates covers the untrusted-input truncation helper directly.
func TestSampleTruncates(t *testing.T) {
	short := strings.Repeat("a", errSampleLen)
	if got := sample(short); got != short {
		t.Errorf("sample truncated a value at the limit: %q", got)
	}
	long := strings.Repeat("a", errSampleLen+1)
	got := sample(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("sample(%d chars) = %q, want a truncation marker", len(long), got)
	}
	if len([]rune(got)) != errSampleLen+1 {
		t.Errorf("sample produced %d runes, want %d", len([]rune(got)), errSampleLen+1)
	}
}
