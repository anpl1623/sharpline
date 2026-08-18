package normalizer

import (
	"errors"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// TestRejectVocabularyIsClosedAndUnique guards the property that makes Reason
// safe as a Prometheus label value.
//
// errors.go is explicit: a Reason "is NEVER the error text. A provider payload
// is untrusted input and an error string built from it can carry arbitrary
// bytes; using one as a Prometheus label value is a cardinality bomb whose
// author is a third party."
func TestRejectVocabularyIsClosedAndUnique(t *testing.T) {
	seen := make(map[Reason]bool, len(Reasons()))
	for _, r := range Reasons() {
		if r == "" {
			t.Error("an empty reason is declared")
		}
		if seen[r] {
			t.Errorf("reason %q is declared twice", r)
		}
		seen[r] = true
		for _, c := range r {
			if (c < 'a' || c > 'z') && c != '_' {
				t.Errorf("reason %q contains %q; a label value must stay in [a-z_]", r, string(c))
			}
		}
	}

	scopes := make(map[Scope]bool, len(Scopes()))
	for _, s := range Scopes() {
		if scopes[s] {
			t.Errorf("scope %q is declared twice", s)
		}
		scopes[s] = true
	}
	if got, want := len(Scopes()), 4; got != want {
		t.Errorf("Scopes() has %d entries, want %d — one per nesting level of a payload", got, want)
	}
}

// TestRejectIsAnError lets a caller errors.As a rejection out of a wrapped error
// rather than parsing its text, which is what Map's fatal path depends on.
func TestRejectIsAnError(t *testing.T) {
	inner := errors.New("boom")
	r := reject(ScopeMarket, ReasonInvalidMarket, "spreads", inner)

	if got, want := r.Error(), "market invalid_market: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(r, inner) {
		t.Error("Unwrap does not expose the underlying error")
	}

	bare := reject(ScopeEvent, ReasonMissingLeague, "nfl", nil)
	if got, want := bare.Error(), "event missing_league"; got != want {
		t.Errorf("Error() with no cause = %q, want %q", got, want)
	}
	if errors.Unwrap(bare) != nil {
		t.Error("a causeless Reject unwrapped to something")
	}

	var out Reject
	if !errors.As(errors.Join(errors.New("context: "), r), &out) {
		t.Fatal("a wrapped Reject is not recoverable with errors.As")
	}
	if out.Scope != ScopeMarket || out.Reason != ReasonInvalidMarket {
		t.Errorf("recovered %s/%s, want market/invalid_market", out.Scope, out.Reason)
	}
}

// TestRejectTruncatesUntrustedText. internal/domain/ids.go states the rule for
// the same reason: "provider payloads are untrusted; echoing an unbounded string
// into a log line is how a log becomes an attack surface."
func TestRejectTruncatesUntrustedText(t *testing.T) {
	r := reject(ScopePrice, ReasonInvalidOdds, strings.Repeat("x", 4096), nil)
	if len(r.Key) > maxKeySample+len("…") {
		t.Fatalf("Reject.Key is %d bytes; the sample is unbounded", len(r.Key))
	}
	if !strings.HasSuffix(r.Key, "…") {
		t.Errorf("a truncated key %q does not say so", r.Key)
	}
	short := reject(ScopePrice, ReasonInvalidOdds, "over", nil)
	if short.Key != "over" {
		t.Errorf("a short key was altered: %q", short.Key)
	}
}

// TestRoleForRefusesAFuzzyMatch is the decision worth pinning: a near-match that
// picks the wrong side produces a plausible, silently inverted price, which is
// far worse than a counted rejection.
func TestRoleForRefusesAFuzzyMatch(t *testing.T) {
	const (
		home = "Kansas City Chiefs"
		away = "Detroit Lions"
	)
	for _, tc := range []struct {
		name  string
		typ   domain.MarketType
		label string
		want  domain.SelectionRole
		ok    bool
	}{
		{"home on a moneyline", domain.MarketTypeMoneyline, home, domain.SelectionRoleHome, true},
		{"away on a moneyline", domain.MarketTypeMoneyline, away, domain.SelectionRoleAway, true},
		{"case and space insensitive", domain.MarketTypeMoneyline, "  kansas city chiefs ", domain.SelectionRoleHome, true},
		{"a soccer draw", domain.MarketTypeMoneyline, "Draw", domain.SelectionRoleDraw, true},
		{"a spread has no draw", domain.MarketTypeSpread, "Draw", domain.SelectionRoleUnknown, false},
		{"home on a spread", domain.MarketTypeSpread, home, domain.SelectionRoleHome, true},
		{"a near miss", domain.MarketTypeMoneyline, "Kansas City", domain.SelectionRoleUnknown, false},
		{"over on a total", domain.MarketTypeTotal, "Over", domain.SelectionRoleOver, true},
		{"under on a total", domain.MarketTypeTotal, "UNDER", domain.SelectionRoleUnder, true},
		{"a team name on a total", domain.MarketTypeTotal, home, domain.SelectionRoleUnknown, false},
		{"over on a prop", domain.MarketTypePlayerProp, "Over", domain.SelectionRoleOver, true},
		{"a named prop outcome", domain.MarketTypePlayerProp, "Travis Kelce", domain.SelectionRoleOutright, true},
		{"a futures runner", domain.MarketTypeFutures, "Detroit Lions", domain.SelectionRoleOutright, true},
		{"an empty label", domain.MarketTypeFutures, "   ", domain.SelectionRoleUnknown, false},
		{"an unknown market type", domain.MarketTypeUnknown, home, domain.SelectionRoleUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := roleFor(tc.typ, tc.label, home, away)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("roleFor(%s, %q) = (%s, %v), want (%s, %v)", tc.typ, tc.label, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestMarketTypeForKey pins the provider vocabulary raw.go fixes.
func TestMarketTypeForKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want domain.MarketType
		ok   bool
	}{
		{MarketKeyH2H, domain.MarketTypeMoneyline, true},
		{MarketKeySpreads, domain.MarketTypeSpread, true},
		{MarketKeyTotals, domain.MarketTypeTotal, true},
		{MarketKeyOutrights, domain.MarketTypeFutures, true},
		{MarketKeyPlayerPrefix + "pass_tds", domain.MarketTypePlayerProp, true},
		{"alternate_spreads", domain.MarketTypeUnknown, false},
		{"btts", domain.MarketTypeUnknown, false},
		{"", domain.MarketTypeUnknown, false},
	} {
		got, ok := marketTypeForKey(tc.key)
		if got != tc.want || ok != tc.ok {
			t.Errorf("marketTypeForKey(%q) = (%s, %v), want (%s, %v)", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

// TestDecodeRejectsThePayloadsThatUnmarshalIntoNothing. Both an empty value and
// the four bytes `null` leave the target at its zero value, and a zero RawEvent
// fails several layers later with an error naming a missing event id rather than
// a missing payload.
func TestDecodeRejectsThePayloadsThatUnmarshalIntoNothing(t *testing.T) {
	dec, err := NewNeutralDecoder(testProvider)
	if err != nil {
		t.Fatal(err)
	}
	if got := dec.Provider(); got != testProvider {
		t.Errorf("Provider() = %q, want %q", got, testProvider)
	}

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"empty", ""},
		{"whitespace", "   \n"},
		{"null", "null"},
		{"truncated", `{"id":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := dec.Decode([]byte(tc.payload)); err == nil {
				t.Fatal("accepted")
			} else if !errors.Is(err, ErrDecode) {
				t.Errorf("error %v does not wrap ErrDecode", err)
			}
		})
	}

	// A payload carrying fields this build does not know must NOT be an error: a
	// provider adds fields without warning, and a decoder that treats an addition
	// as a failure turns a routine provider change into a total ingestion outage.
	ev, err := dec.Decode([]byte(`{"id":"e1","league_key":"nfl","commence_time":"2026-08-17T20:15:00Z","brand_new":42}`))
	if err != nil {
		t.Fatalf("an unknown field was rejected: %v", err)
	}
	if ev.ID != "e1" {
		t.Errorf("id = %q, want e1", ev.ID)
	}
}

// TestNewNeutralDecoderValidatesItsProvider keeps a misconfigured slug a
// construction failure.
func TestNewNeutralDecoderValidatesItsProvider(t *testing.T) {
	for _, name := range []string{"", "Not Valid", "-leading"} {
		if _, err := NewNeutralDecoder(kafka.Provider(name)); err == nil {
			t.Errorf("provider %q was accepted", name)
		} else if !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("provider %q: error %v does not wrap ErrInvalidOptions", name, err)
		}
	}
}

// TestAccessorsReportTheWiring covers the small readers a diagnostic reaches for.
func TestAccessorsReportTheWiring(t *testing.T) {
	m := mapperFor(t)
	if got := m.Provider(); got != testProvider {
		t.Errorf("Mapper.Provider() = %q, want %q", got, testProvider)
	}

	h := newHarness(t)
	want, err := kafka.OddsRaw(testProvider)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.n.RawTopic(); got != want.Name() {
		t.Errorf("RawTopic() = %q, want %q", got, want.Name())
	}
}

// TestNilMetricsAreSafe: NewMetrics(nil) builds the collectors unregistered, so
// no observe call site needs a nil check. This checks the other half — that a nil
// *Metrics, which is what a partially-built value would be, does not panic.
// CLAUDE.md §12 forbids a panic outside main.
func TestNilMetricsAreSafe(t *testing.T) {
	var m *Metrics
	m.observeRecord(recordMapped)
	m.observeMarket(resultPublished)
	m.observeReject(reject(ScopeEvent, ReasonDecode, "k", nil))
	m.observeWarmStart(warmStartOK, 0, 1)
	m.observeHeld(3)
	m.observeMapping(0)
	m.observePublished(sampleRecord(), sampleRecord().ObservedAt)

	unregistered, err := NewMetrics(nil)
	if err != nil {
		t.Fatalf("NewMetrics(nil): %v", err)
	}
	unregistered.observePublished(sampleRecord(), sampleRecord().ObservedAt)
}
