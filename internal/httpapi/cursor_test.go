package httpapi

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

func TestCursorRoundTrips(t *testing.T) {
	t.Parallel()

	// A nanosecond-precision instant, on purpose. The database orders by the
	// full-precision timestamptz, so a cursor that rounded to microseconds could
	// re-emit or skip a row quoted inside the rounded interval.
	start := time.Date(2026, 8, 19, 18, 30, 0, 123456789, time.UTC)
	id, err := domain.NewEventID("evt_nfl_2026_w1_kc_buf")
	if err != nil {
		t.Fatalf("event id: %v", err)
	}
	scope := cursorScope("board", "", "horizon")

	encoded := encodeCursor(cursor{key: EventKey{ScheduledStart: start, ID: id}, scope: scope})
	got, err := decodeCursor(encoded, scope)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.key.ScheduledStart.Equal(start) {
		t.Errorf("instant = %v (%d ns), want %v (%d ns)",
			got.key.ScheduledStart, got.key.ScheduledStart.Nanosecond(), start, start.Nanosecond())
	}
	if got.key.ID != id {
		t.Errorf("id = %q, want %q", got.key.ID, id)
	}
}

func TestCursorIsURLSafe(t *testing.T) {
	t.Parallel()

	id, err := domain.NewEventID("evt_with-dots.and_underscores")
	if err != nil {
		t.Fatalf("event id: %v", err)
	}
	encoded := encodeCursor(cursor{key: EventKey{ScheduledStart: testNow, ID: id}, scope: 42})

	// base64url has no `+`, `/` or `=`, so a cursor survives a query string
	// unescaped. A standard-base64 cursor would silently corrupt the moment a
	// client failed to escape it.
	if strings.ContainsAny(encoded, "+/=") {
		t.Errorf("cursor %q contains characters that need escaping in a query string", encoded)
	}
	if len(encoded) > maxCursorLen {
		t.Errorf("cursor is %d bytes, over the spec's %d maximum", len(encoded), maxCursorLen)
	}
}

func TestCursorRejectsAMismatchedScope(t *testing.T) {
	t.Parallel()

	id, err := domain.NewEventID("evt_1")
	if err != nil {
		t.Fatalf("event id: %v", err)
	}
	encoded := encodeCursor(cursor{key: EventKey{ScheduledStart: testNow, ID: id}, scope: cursorScope("board", "nfl")})

	// The whole reason the fingerprint exists: without it, a client that changed
	// a filter mid-listing would receive a page from a different set, ordered
	// consistently, with nothing reporting a problem.
	if _, err := decodeCursor(encoded, cursorScope("board", "nba")); !errors.Is(err, ErrBadCursor) {
		t.Errorf("decode under a different scope returned %v, want ErrBadCursor", err)
	}
}

func TestCursorScopeIsUnambiguousAcrossFieldBoundaries(t *testing.T) {
	t.Parallel()

	// Without a length prefix, ("ab","c") and ("a","bc") hash identically — and
	// then a cursor from one query silently validates against another.
	if cursorScope("ab", "c") == cursorScope("a", "bc") {
		t.Error("cursorScope concatenates without a length prefix: two different queries share a fingerprint")
	}
}

func TestCursorRejectsGarbage(t *testing.T) {
	t.Parallel()

	for name, input := range map[string]string{
		"empty":             "",
		"not base64":        "!!!!not base64!!!!",
		"wrong field count": base64.RawURLEncoding.EncodeToString([]byte("1\x1f2")),
		"bad version":       base64.RawURLEncoding.EncodeToString([]byte("9\x1f1\x1f1\x1fevt_1")),
		"bad instant":       base64.RawURLEncoding.EncodeToString([]byte("1\x1fnotanumber\x1f1\x1fevt_1")),
		"too long":          strings.Repeat("A", maxCursorLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCursor(input, 1); !errors.Is(err, ErrBadCursor) {
				t.Errorf("decode(%q) = %v, want ErrBadCursor", input, err)
			}
		})
	}
}

// TestCursorRejectsAnInvalidIdentifier: the identifier goes through the domain
// constructor rather than being cast, so a cursor cannot smuggle a value the
// rest of the system considers impossible into a query parameter.
func TestCursorRejectsAnInvalidIdentifier(t *testing.T) {
	t.Parallel()

	bad := base64.RawURLEncoding.EncodeToString(
		[]byte("1\x1f0\x1f1\x1f" + strings.Repeat("x", 200)))
	if _, err := decodeCursor(bad, 1); !errors.Is(err, ErrBadCursor) {
		t.Errorf("decode of an over-long identifier = %v, want ErrBadCursor", err)
	}

	colon := base64.RawURLEncoding.EncodeToString([]byte("1\x1f0\x1f1\x1fevt:1"))
	if _, err := decodeCursor(colon, 1); !errors.Is(err, ErrBadCursor) {
		// `:` is excluded from every identifier because WebSocket channels are
		// `event:{id}` and a colon inside an id makes splitting a channel name
		// ambiguous.
		t.Errorf("decode of an identifier containing ':' = %v, want ErrBadCursor", err)
	}
}

// -----------------------------------------------------------------------------
// Parameter bounds
// -----------------------------------------------------------------------------

// TestParameterBoundsMatchTheSpec.
//
// oapi-codegen emits no validation in models-only mode, so the bounds in
// params.go are hand-written from openapi.yaml. This asserts each one against
// the SPEC TEXT, so the duplication cannot drift silently — which is the only
// thing that makes duplicating them acceptable.
func TestParameterBoundsMatchTheSpec(t *testing.T) {
	t.Parallel()

	raw, err := SpecBytes()
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec := string(raw)

	for _, tc := range []struct {
		what     string
		fragment string
	}{
		{"page limit maximum", "maximum: 200, default: 50"},
		{"search prefix bounds", "minLength: 2, maxLength: 64"},
		{"max_points bounds", "maximum: 5000, default: 1000"},
		{"book filter cap", "maxItems: 32"},
		{"cursor length", "maxLength: 512"},
		{"money bounds", "maximum: 9007199254740991"},
	} {
		if !strings.Contains(spec, tc.fragment) {
			t.Errorf("%s: openapi.yaml no longer contains %q; params.go's constants may now be wrong",
				tc.what, tc.fragment)
		}
	}

	if maxPageLimit != 200 || defaultPageLimit != 50 {
		t.Errorf("page limit constants (%d/%d) do not match the spec (200/50)", maxPageLimit, defaultPageLimit)
	}
	if minSearchPrefix != 2 || maxSearchPrefix != 64 {
		t.Errorf("search prefix constants (%d/%d) do not match the spec (2/64)", minSearchPrefix, maxSearchPrefix)
	}
	if maxHistoryPoints != 5000 || defaultMaxHistoryPoints != 1000 {
		t.Errorf("history point constants (%d/%d) do not match the spec (5000/1000)", maxHistoryPoints, defaultMaxHistoryPoints)
	}
	if maxCursorLen != 512 {
		t.Errorf("maxCursorLen = %d, spec says 512", maxCursorLen)
	}
	if int64(domain.MaxSafeMoney) != 9007199254740991 {
		t.Errorf("domain.MaxSafeMoney = %d, spec bounds money at 9007199254740991", int64(domain.MaxSafeMoney))
	}
}

// TestEscapeLikeNeutralisesEveryMetacharacter.
//
// Unescaped, a search for `%` turns a prefix lookup into a leading-wildcard scan
// of the whole events table — a denial of service one character long.
func TestEscapeLikeNeutralisesEveryMetacharacter(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"celtics": "celtics",
		"%":       `\%`,
		"_":       `\_`,
		`\`:       `\\`,
		"50%":     `50\%`,
		// The backslash must be escaped FIRST, or escaping the others would be
		// undone by a user-supplied backslash.
		`a\%b`: `a\\\%b`,
	} {
		if got := escapeLike(input); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestSuggestResolutionNeverSuggestsRaw.
//
// A raw series' point count is the number of stored quotes, which cannot be
// known without running the query — so it can never be PROVED to fit inside
// max_points and must never be offered as a remedy.
func TestSuggestResolutionNeverSuggestsRaw(t *testing.T) {
	t.Parallel()

	for _, window := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour, 365 * 24 * time.Hour} {
		res, ok := suggestResolution(window, 100)
		if ok && res == gen.Raw {
			t.Errorf("window %v: suggested raw, which cannot be proved to fit", window)
		}
	}
}

func TestRenderOddsKeepsDecimalCanonical(t *testing.T) {
	t.Parallel()

	d, err := odds.NewDecimal(1.91)
	if err != nil {
		t.Fatalf("decimal: %v", err)
	}
	if got := renderOdds(d, odds.FormatDecimal); got != nil {
		t.Errorf("renderOdds(decimal) = %q, want nil — the canonical value is already on the wire", *got)
	}
	for _, f := range []odds.Format{odds.FormatAmerican, odds.FormatFractional} {
		if got := renderOdds(d, f); got == nil || *got == "" {
			t.Errorf("renderOdds(%v) produced no display string", f)
		}
	}
}
