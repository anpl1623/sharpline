package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// Tests for the catalogue entities — Sport, League, Book, Event, Market,
// Selection, Price — and the enums and state machines they carry.
//
// These files previously had no test of their own. That mattered beyond the
// coverage number: EventStatus and MarketStatus both expose CanTransitionTo state
// machines with no transition table pinned anywhere, and Price.PayoutFor /
// Price.ProfitFor are the functions CLAUDE.md §12's "floating point never touches a
// balance" rule actually rests on, since they are the only place in the catalogue
// where a float64 price meets integer minor units.
//
// Conventions used throughout:
//
//   - Expected values are derived from the definition, not from what the code
//     printed. Payouts are computed as an exact rational in the comment and written
//     as an integer of minor units; the state-machine tables are transcribed from
//     the prose in the type comments, which is the specification, and a mismatch
//     means one of the two is wrong rather than that the table needs updating.
//   - Every enum is checked round-trip (String → Parse → String) and every
//     transition table is checked exhaustively over all pairs, including the
//     invalid zero value, rather than at a handful of interesting points.
//   - Errors are matched with errors.Is against the sentinel, never on message
//     text.

// -----------------------------------------------------------------------------
// Shared fixtures
// -----------------------------------------------------------------------------

// A fixed instant, so nothing here reads a clock. Tip-off for a notional evening
// game; the value is arbitrary and only its ordering against the other constants
// matters.
var (
	testTip     = time.Date(2026, 3, 14, 23, 30, 0, 0, time.UTC)
	testObserve = testTip.Add(-90 * time.Minute)
)

func mustCompetitor(t *testing.T, id, name string) Competitor {
	t.Helper()
	c, err := NewCompetitor(CompetitorID(id), name)
	if err != nil {
		t.Fatalf("NewCompetitor(%q, %q): %v", id, name, err)
	}
	return c
}

// matchEvent is a scheduled two-sided event, the shape most of the file needs.
func matchEvent(t *testing.T) Event {
	t.Helper()
	e, err := NewEvent(EventParams{
		ID:             "evt-1",
		LeagueID:       "lg-nba",
		Kind:           EventKindMatch,
		Name:           "Celtics at Lakers",
		Home:           mustCompetitor(t, "cmp-lal", "Los Angeles Lakers"),
		Away:           mustCompetitor(t, "cmp-bos", "Boston Celtics"),
		ScheduledStart: testTip,
		Status:         EventStatusScheduled,
		UpdatedAt:      testObserve,
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	return e
}

func mustMarket(t *testing.T, p MarketParams) Market {
	t.Helper()
	m, err := NewMarket(p)
	if err != nil {
		t.Fatalf("NewMarket(%+v): %v", p, err)
	}
	return m
}

// spreadMarket is a home-favourite spread at -3.5, the canonical case for the
// home-perspective line convention.
func spreadMarket(t *testing.T) Market {
	t.Helper()
	return mustMarket(t, MarketParams{
		ID:        "mkt-spread",
		EventID:   "evt-1",
		Type:      MarketTypeSpread,
		Line:      mustLine(t, -3.5),
		Status:    MarketStatusOpen,
		UpdatedAt: testObserve,
	})
}

func mustSelection(t *testing.T, p SelectionParams) Selection {
	t.Helper()
	s, err := NewSelection(p)
	if err != nil {
		t.Fatalf("NewSelection(%+v): %v", p, err)
	}
	return s
}

func mustPrice(t *testing.T, p PriceParams) Price {
	t.Helper()
	price, err := NewPrice(p)
	if err != nil {
		t.Fatalf("NewPrice(%+v): %v", p, err)
	}
	return price
}

// -----------------------------------------------------------------------------
// Sport
// -----------------------------------------------------------------------------

func TestNewSport(t *testing.T) {
	good := SportParams{ID: "spt-1", Slug: "basketball", Name: "Basketball"}

	s, err := NewSport(good)
	if err != nil {
		t.Fatalf("NewSport: %v", err)
	}
	if s.ID() != "spt-1" || s.Slug() != "basketball" || s.Name() != "Basketball" {
		t.Errorf("accessors returned (%s, %s, %q)", s.ID(), s.Slug(), s.Name())
	}
	if s.IsZero() {
		t.Error("a constructed sport reports IsZero")
	}
	if got, want := s.String(), `sport(basketball "Basketball")`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (Sport{}).String(), "sport(<zero>)"; got != want {
		t.Errorf("zero String() = %q, want %q", got, want)
	}
	if !(Sport{}).IsZero() {
		t.Error("the zero Sport does not report IsZero")
	}

	// The name is trimmed, not merely accepted: display names arrive from
	// providers with incidental whitespace and two entries differing only by a
	// trailing space would render as duplicates.
	padded, err := NewSport(SportParams{ID: "spt-2", Slug: "soccer", Name: "  Soccer\t"})
	if err != nil {
		t.Fatalf("NewSport with padding: %v", err)
	}
	if padded.Name() != "Soccer" {
		t.Errorf("name %q was not trimmed", padded.Name())
	}

	cases := []struct {
		name  string
		mutot func(*SportParams)
		want  error
	}{
		{"empty id", func(p *SportParams) { p.ID = "" }, ErrEmptyID},
		{"oversized id", func(p *SportParams) { p.ID = SportID(strings.Repeat("a", MaxIDLen+1)) }, ErrIDTooLong},
		{"empty slug", func(p *SportParams) { p.Slug = "" }, ErrEmptySlug},
		{"slug charset", func(p *SportParams) { p.Slug = "Basketball" }, ErrSlugCharset},
		{"empty name", func(p *SportParams) { p.Name = "   " }, ErrEmptyName},
		{"control character in name", func(p *SportParams) { p.Name = "Basket\x07ball" }, ErrNameCharset},
		{"oversized name", func(p *SportParams) { p.Name = strings.Repeat("x", MaxNameLen+1) }, ErrNameTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := good
			c.mutot(&p)
			got, err := NewSport(p)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewSport error = %v, want %v", err, c.want)
			}
			if !got.IsZero() {
				t.Errorf("a rejected sport came back non-zero: %s", got)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error %v does not reach the ErrInvalid root", err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// League
// -----------------------------------------------------------------------------

func TestNewLeague(t *testing.T) {
	good := LeagueParams{ID: "lg-nba", SportID: "spt-1", Slug: "nba", Name: "NBA"}

	l, err := NewLeague(good)
	if err != nil {
		t.Fatalf("NewLeague: %v", err)
	}
	if l.ID() != "lg-nba" || l.SportID() != "spt-1" || l.Slug() != "nba" || l.Name() != "NBA" {
		t.Errorf("accessors returned (%s, %s, %s, %q)", l.ID(), l.SportID(), l.Slug(), l.Name())
	}
	if got, want := l.String(), `league(nba "NBA")`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (League{}).String(), "league(<zero>)"; got != want {
		t.Errorf("zero String() = %q, want %q", got, want)
	}
	if !(League{}).IsZero() || l.IsZero() {
		t.Error("IsZero is wrong for one of the two leagues")
	}

	cases := []struct {
		name  string
		mutot func(*LeagueParams)
		want  error
	}{
		{"empty id", func(p *LeagueParams) { p.ID = "" }, ErrEmptyID},
		{"empty sport id", func(p *LeagueParams) { p.SportID = "" }, ErrEmptyID},
		{"slug charset", func(p *LeagueParams) { p.Slug = "-nba" }, ErrSlugCharset},
		{"empty name", func(p *LeagueParams) { p.Name = "" }, ErrEmptyName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := good
			c.mutot(&p)
			if _, err := NewLeague(p); !errors.Is(err, c.want) {
				t.Fatalf("NewLeague error = %v, want %v", err, c.want)
			}
		})
	}
}

// TestLeagueBelongsTo covers the parent check, including the case that makes the
// guard worth having: a zero SportID must not match a zero Sport. Without the
// IsZero test, two unset values would compare equal and every orphaned league would
// claim to belong to every unconstructed sport.
func TestLeagueBelongsTo(t *testing.T) {
	basketball, err := NewSport(SportParams{ID: "spt-1", Slug: "basketball", Name: "Basketball"})
	if err != nil {
		t.Fatalf("NewSport: %v", err)
	}
	soccer, err := NewSport(SportParams{ID: "spt-2", Slug: "soccer", Name: "Soccer"})
	if err != nil {
		t.Fatalf("NewSport: %v", err)
	}
	nba, err := NewLeague(LeagueParams{ID: "lg-nba", SportID: "spt-1", Slug: "nba", Name: "NBA"})
	if err != nil {
		t.Fatalf("NewLeague: %v", err)
	}

	if !nba.BelongsTo(basketball) {
		t.Error("the NBA does not belong to basketball")
	}
	if nba.BelongsTo(soccer) {
		t.Error("the NBA belongs to soccer")
	}
	if (League{}).BelongsTo(Sport{}) {
		t.Error("a zero league belongs to a zero sport; two unset ids must not match")
	}
	if nba.BelongsTo(Sport{}) {
		t.Error("the NBA belongs to the zero sport")
	}
}

// -----------------------------------------------------------------------------
// Book
// -----------------------------------------------------------------------------

func TestBookKind(t *testing.T) {
	// String and Parse must be exact inverses over the defined kinds, because
	// those strings are the values that go into Postgres, Kafka and the API — a
	// drift between the two directions is a data-migration problem, not a bug
	// that shows up in a request.
	for _, k := range []BookKind{BookKindExternal, BookKindSynthetic} {
		text := k.String()
		back, err := ParseBookKind(text)
		if err != nil {
			t.Fatalf("ParseBookKind(%q): %v", text, err)
		}
		if back != k {
			t.Errorf("round trip of %v via %q gave %v", k, text, back)
		}
		if !k.Valid() {
			t.Errorf("%v is not Valid", k)
		}

		marshalled, err := k.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", k, err)
		}
		if string(marshalled) != text {
			t.Errorf("MarshalText gave %q, String gave %q", marshalled, text)
		}
		var target BookKind
		if err := target.UnmarshalText(marshalled); err != nil {
			t.Fatalf("UnmarshalText(%q): %v", marshalled, err)
		}
		if target != k {
			t.Errorf("UnmarshalText(%q) gave %v, want %v", marshalled, target, k)
		}
	}

	if BookKindUnknown.Valid() {
		t.Error("the zero BookKind reports Valid")
	}
	if got := BookKindUnknown.String(); got != "unknown" {
		t.Errorf("BookKindUnknown.String() = %q, want %q", got, "unknown")
	}
	if got := BookKind(200).String(); got != "unknown" {
		t.Errorf("an undefined kind renders as %q", got)
	}
	if _, err := BookKindUnknown.MarshalText(); !errors.Is(err, ErrUnknownBookKind) {
		t.Errorf("marshalling the zero kind gave %v, want ErrUnknownBookKind", err)
	}
	if _, err := ParseBookKind("pinnacle"); !errors.Is(err, ErrUnknownBookKind) {
		t.Errorf("ParseBookKind(\"pinnacle\") gave %v, want ErrUnknownBookKind", err)
	}
	var target BookKind
	if err := target.UnmarshalText([]byte("nonsense")); !errors.Is(err, ErrUnknownBookKind) {
		t.Errorf("UnmarshalText on nonsense gave %v, want ErrUnknownBookKind", err)
	}
	if target != BookKindUnknown {
		t.Errorf("a failed UnmarshalText wrote %v into the receiver", target)
	}

	// A long unparseable value is truncated in the message rather than echoed
	// whole, so a hostile or corrupt payload cannot flood a log line.
	long := strings.Repeat("z", 500)
	_, err := ParseBookKind(long)
	if err == nil {
		t.Fatal("a 500-byte kind parsed successfully")
	}
	if len(err.Error()) > 200 {
		t.Errorf("the error echoed %d bytes of a 500-byte input", len(err.Error()))
	}
}

func TestNewBook(t *testing.T) {
	good := BookParams{ID: "bk-1", Slug: "pinnacle", Name: "Pinnacle", Kind: BookKindExternal, Reference: true}

	b, err := NewBook(good)
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	if b.ID() != "bk-1" || b.Slug() != "pinnacle" || b.Name() != "Pinnacle" || b.Kind() != BookKindExternal {
		t.Errorf("accessors returned (%s, %s, %q, %v)", b.ID(), b.Slug(), b.Name(), b.Kind())
	}
	if !b.IsReference() {
		t.Error("the reference flag was dropped")
	}
	if b.IsSynthetic() {
		t.Error("an external book reports IsSynthetic")
	}
	if got, want := b.String(), "book(pinnacle external reference)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	plain, err := NewBook(BookParams{ID: "bk-2", Slug: "draftkings", Name: "DraftKings", Kind: BookKindExternal})
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	if got, want := plain.String(), "book(draftkings external)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if plain.IsReference() {
		t.Error("a book with Reference unset reports IsReference")
	}
	if got, want := (Book{}).String(), "book(<zero>)"; got != want {
		t.Errorf("zero String() = %q, want %q", got, want)
	}
	if !(Book{}).IsZero() || plain.IsZero() {
		t.Error("IsZero is wrong for one of the two books")
	}

	cases := []struct {
		name  string
		mutot func(*BookParams)
		want  error
	}{
		{"empty id", func(p *BookParams) { p.ID = "" }, ErrEmptyID},
		{"slug charset", func(p *BookParams) { p.Slug = "Pinnacle" }, ErrSlugCharset},
		{"unknown kind", func(p *BookParams) { p.Kind = BookKindUnknown }, ErrUnknownBookKind},
		{"empty name", func(p *BookParams) { p.Name = "" }, ErrEmptyName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := good
			c.mutot(&p)
			if _, err := NewBook(p); !errors.Is(err, c.want) {
				t.Fatalf("NewBook error = %v, want %v", err, c.want)
			}
		})
	}
}

// TestNewSyntheticBook checks the in-house book, and specifically that marking it
// as the reference is permitted. That permission is load-bearing: it is what lets
// the no-API-key demo path have a +EV surface at all, and rejecting it would leave
// the offline system with no reference book and an empty analytics screen.
func TestNewSyntheticBook(t *testing.T) {
	for _, reference := range []bool{false, true} {
		b, err := NewSyntheticBook("bk-synth", reference)
		if err != nil {
			t.Fatalf("NewSyntheticBook(reference=%v): %v", reference, err)
		}
		if b.Slug() != SyntheticBookSlug {
			t.Errorf("slug = %q, want the canonical %q", b.Slug(), SyntheticBookSlug)
		}
		if !b.IsSynthetic() || b.Kind() != BookKindSynthetic {
			t.Error("the synthetic book does not report itself as synthetic")
		}
		if b.IsReference() != reference {
			t.Errorf("IsReference() = %v, want %v", b.IsReference(), reference)
		}
	}
	if _, err := NewSyntheticBook("", false); !errors.Is(err, ErrEmptyID) {
		t.Errorf("NewSyntheticBook with no id gave %v, want ErrEmptyID", err)
	}
}

func TestBookQuoted(t *testing.T) {
	b, err := NewBook(BookParams{ID: "bk-1", Slug: "pinnacle", Name: "Pinnacle", Kind: BookKindExternal})
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	mine := mustPrice(t, PriceParams{SelectionID: "sel-1", BookID: "bk-1", Decimal: 1.91, ObservedAt: testObserve})
	theirs := mustPrice(t, PriceParams{SelectionID: "sel-1", BookID: "bk-2", Decimal: 1.91, ObservedAt: testObserve})

	if !b.Quoted(mine) {
		t.Error("the book does not claim its own price")
	}
	if b.Quoted(theirs) {
		t.Error("the book claims another book's price")
	}
	// The zero-id guard again: an unconstructed book must not claim an
	// unconstructed price.
	if (Book{}).Quoted(Price{}) {
		t.Error("a zero book claims a zero price")
	}
}

// -----------------------------------------------------------------------------
// Line
// -----------------------------------------------------------------------------

// TestLineModelsAbsenceSeparatelyFromZero is the property the type exists for: a
// pick'em spread IS 0.0 and is present, while a moneyline has no line at all, and
// collapsing the two would make every lineless market look like a pick'em.
func TestLineModelsAbsenceSeparatelyFromZero(t *testing.T) {
	absent := NoLine()
	pickEm := mustLine(t, 0)

	if absent.Present() {
		t.Error("NoLine reports Present")
	}
	if !pickEm.Present() {
		t.Error("a 0.0 line reports absent")
	}
	if absent.Equal(pickEm) {
		t.Error("an absent line equals a pick'em")
	}
	if absent != (Line{}) {
		t.Error("NoLine is not the zero value")
	}

	if v, ok := absent.Value(); ok || v != 0 {
		t.Errorf("absent.Value() = (%v, %v), want (0, false)", v, ok)
	}
	if v, ok := pickEm.Value(); !ok || v != 0 {
		t.Errorf("pickEm.Value() = (%v, %v), want (0, true)", v, ok)
	}
	if got := absent.ValueOr(-7); got != -7 {
		t.Errorf("absent.ValueOr(-7) = %v, want -7", got)
	}
	if got := pickEm.ValueOr(-7); got != 0 {
		t.Errorf("pickEm.ValueOr(-7) = %v, want 0", got)
	}

	// Two absent lines are equal to each other, and a line is equal to itself.
	if !absent.Equal(NoLine()) {
		t.Error("two absent lines are unequal")
	}
	if !pickEm.Equal(mustLine(t, 0)) {
		t.Error("two pick'em lines are unequal")
	}
}

// TestLineInvertKeepsAPickEmRenderingAsZero covers the negative-zero case
// explicitly. Negating 0.0 in IEEE-754 gives -0.0, which compares equal to 0.0 but
// formats as "-0"; a pick'em whose away side rendered as "-0" on the board would be
// a visible defect that no equality assertion would catch.
func TestLineInvertKeepsAPickEmRenderingAsZero(t *testing.T) {
	inverted := mustLine(t, 0).Invert()

	v, ok := inverted.Value()
	if !ok {
		t.Fatal("inverting a pick'em lost its presence")
	}
	if math.Signbit(v) {
		t.Errorf("inverting 0.0 produced negative zero, which renders as %q", inverted.String())
	}
	if got := inverted.String(); got != "0" {
		t.Errorf("inverted pick'em renders as %q, want %q", got, "0")
	}
	if got := inverted.SignedString(); got != "0" {
		t.Errorf("inverted pick'em signs as %q, want %q", got, "0")
	}

	// The ordinary case, and the involution: inverting twice is the identity.
	home := mustLine(t, -3.5)
	away := home.Invert()
	if v, _ := away.Value(); v != 3.5 {
		t.Errorf("inverting -3.5 gave %v, want 3.5", v)
	}
	if !away.Invert().Equal(home) {
		t.Error("inverting twice is not the identity")
	}
	if !NoLine().Invert().Equal(NoLine()) {
		t.Error("inverting an absent line produced a present one")
	}
}

func TestLineRendering(t *testing.T) {
	cases := []struct {
		line   Line
		plain  string
		signed string
	}{
		{NoLine(), "none", "none"},
		{mustLine(t, 0), "0", "0"},
		{mustLine(t, -3.5), "-3.5", "-3.5"},
		{mustLine(t, 3.5), "3.5", "+3.5"},
		{mustLine(t, 224.5), "224.5", "+224.5"},
		{mustLine(t, -7), "-7", "-7"},
	}
	for _, c := range cases {
		if got := c.line.String(); got != c.plain {
			t.Errorf("String() = %q, want %q", got, c.plain)
		}
		if got := c.line.SignedString(); got != c.signed {
			t.Errorf("SignedString() = %q, want %q", got, c.signed)
		}
	}
}

func TestNewLineRejectsNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		got, err := NewLine(v)
		if !errors.Is(err, ErrLineNotFinite) {
			t.Errorf("NewLine(%v) error = %v, want ErrLineNotFinite", v, err)
		}
		if got.Present() {
			t.Errorf("NewLine(%v) returned a present line", v)
		}
	}
	// The reason it matters: Line is compared with ==, and a NaN inside one would
	// make a market unequal to itself.
	l := mustLine(t, -3.5)
	if l != l { //nolint:staticcheck // the point is that this self-comparison holds
		t.Error("a Line is not equal to itself")
	}
}

// TestLineJSONPreservesAbsence covers the wire format. The distinction between
// "no line" and "a line of zero" is exactly what would be lost if absence
// serialised as 0 rather than null, and it crosses the bus on every market update.
func TestLineJSONPreservesAbsence(t *testing.T) {
	cases := []struct {
		line Line
		want string
	}{
		{NoLine(), "null"},
		{mustLine(t, 0), "0"},
		{mustLine(t, -3.5), "-3.5"},
		{mustLine(t, 224.5), "224.5"},
	}
	for _, c := range cases {
		encoded, err := json.Marshal(c.line)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", c.line, err)
		}
		if string(encoded) != c.want {
			t.Errorf("Marshal(%s) = %s, want %s", c.line, encoded, c.want)
		}
		var back Line
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatalf("Unmarshal(%s): %v", encoded, err)
		}
		if !back.Equal(c.line) {
			t.Errorf("round trip of %s through %s gave %s", c.line, encoded, back)
		}
	}

	// A struct field round-trips too, which is the shape the bus actually sends.
	type envelope struct {
		Line Line `json:"line"`
	}
	encoded, err := json.Marshal(envelope{Line: NoLine()})
	if err != nil {
		t.Fatalf("Marshal envelope: %v", err)
	}
	if string(encoded) != `{"line":null}` {
		t.Errorf("absent line in a struct encoded as %s", encoded)
	}

	var target Line
	if err := target.UnmarshalJSON([]byte(`"-3.5"`)); !errors.Is(err, ErrLineSyntax) {
		t.Errorf("a quoted number gave %v, want ErrLineSyntax", err)
	}
	if err := target.UnmarshalJSON([]byte(`NaN`)); !errors.Is(err, ErrLineSyntax) {
		// Go's ParseFloat accepts "NaN", so this must be caught by NewLine
		// instead; either sentinel is a refusal, but a NaN must never land.
		if !errors.Is(err, ErrLineNotFinite) {
			t.Errorf("NaN gave %v, want a refusal", err)
		}
	}
	if target.Present() {
		t.Error("a failed UnmarshalJSON left a present line in the receiver")
	}
}

func TestLineRuleString(t *testing.T) {
	cases := map[LineRule]string{
		LineRuleForbidden: "forbidden",
		LineRuleRequired:  "required",
		LineRuleOptional:  "optional",
		LineRuleUnknown:   "unknown",
		LineRule(200):     "unknown",
	}
	for rule, want := range cases {
		if got := rule.String(); got != want {
			t.Errorf("LineRule(%d).String() = %q, want %q", uint8(rule), got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// MarketType
// -----------------------------------------------------------------------------

func TestMarketType(t *testing.T) {
	defined := []MarketType{
		MarketTypeMoneyline, MarketTypeSpread, MarketTypeTotal,
		MarketTypePlayerProp, MarketTypeFutures,
	}
	wantText := map[MarketType]string{
		MarketTypeMoneyline:  "moneyline",
		MarketTypeSpread:     "spread",
		MarketTypeTotal:      "total",
		MarketTypePlayerProp: "player_prop",
		MarketTypeFutures:    "futures",
	}

	for _, ty := range defined {
		if got := ty.String(); got != wantText[ty] {
			t.Errorf("String() = %q, want %q", got, wantText[ty])
		}
		if !ty.Valid() {
			t.Errorf("%v is not Valid", ty)
		}
		back, err := ParseMarketType(ty.String())
		if err != nil || back != ty {
			t.Errorf("round trip of %v gave (%v, %v)", ty, back, err)
		}
		text, err := ty.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", ty, err)
		}
		var target MarketType
		if err := target.UnmarshalText(text); err != nil || target != ty {
			t.Errorf("UnmarshalText(%q) gave (%v, %v)", text, target, err)
		}
	}

	if MarketTypeUnknown.Valid() || MarketType(200).Valid() {
		t.Error("an undefined market type reports Valid")
	}
	if got := MarketTypeUnknown.String(); got != "unknown" {
		t.Errorf("MarketTypeUnknown renders as %q", got)
	}
	if _, err := MarketTypeUnknown.MarshalText(); !errors.Is(err, ErrUnknownMarketType) {
		t.Errorf("marshalling the zero type gave %v", err)
	}
	if _, err := ParseMarketType("team_total"); !errors.Is(err, ErrUnknownMarketType) {
		t.Errorf("ParseMarketType(\"team_total\") gave %v", err)
	}
	var target MarketType
	if err := target.UnmarshalText([]byte("team_total")); !errors.Is(err, ErrUnknownMarketType) {
		t.Errorf("UnmarshalText on an undefined type gave %v", err)
	}
}

// TestMarketTypeLineRule pins the line matrix from the type comments. It is a
// specification, not an observation: moneyline and futures ask a question with no
// handicap, spread and total are defined by one, and a player prop may or may not
// carry one ("over 24.5 points" does, "first to score" does not).
func TestMarketTypeLineRule(t *testing.T) {
	want := map[MarketType]LineRule{
		MarketTypeMoneyline:  LineRuleForbidden,
		MarketTypeFutures:    LineRuleForbidden,
		MarketTypeSpread:     LineRuleRequired,
		MarketTypeTotal:      LineRuleRequired,
		MarketTypePlayerProp: LineRuleOptional,
		MarketTypeUnknown:    LineRuleUnknown,
	}
	for ty, rule := range want {
		if got := ty.LineRule(); got != rule {
			t.Errorf("%v.LineRule() = %v, want %v", ty, got, rule)
		}
	}
	if got := MarketType(200).LineRule(); got != LineRuleUnknown {
		t.Errorf("an undefined type's LineRule = %v", got)
	}

	// Only a player prop names its subject.
	for _, ty := range []MarketType{MarketTypeMoneyline, MarketTypeSpread, MarketTypeTotal, MarketTypeFutures} {
		if ty.NeedsSubject() {
			t.Errorf("%v claims to need a subject", ty)
		}
	}
	if !MarketTypePlayerProp.NeedsSubject() {
		t.Error("a player prop does not need a subject")
	}
}

// TestMarketTypeAllowsRole pins the full type-by-role compatibility matrix
// exhaustively, including the invalid zero values on both axes. The matrix is
// transcribed from the prose on AllowsRole; a disagreement means the code and the
// documentation have drifted, which is the failure this test exists to catch.
func TestMarketTypeAllowsRole(t *testing.T) {
	types := []MarketType{
		MarketTypeUnknown, MarketTypeMoneyline, MarketTypeSpread,
		MarketTypeTotal, MarketTypePlayerProp, MarketTypeFutures,
	}
	roles := []SelectionRole{
		SelectionRoleUnknown, SelectionRoleHome, SelectionRoleAway, SelectionRoleDraw,
		SelectionRoleOver, SelectionRoleUnder, SelectionRoleOutright,
	}
	allowed := map[MarketType]map[SelectionRole]bool{
		MarketTypeMoneyline: {
			SelectionRoleHome: true, SelectionRoleAway: true, SelectionRoleDraw: true,
		},
		MarketTypeSpread: {
			SelectionRoleHome: true, SelectionRoleAway: true,
		},
		MarketTypeTotal: {
			SelectionRoleOver: true, SelectionRoleUnder: true,
		},
		MarketTypePlayerProp: {
			SelectionRoleOver: true, SelectionRoleUnder: true, SelectionRoleOutright: true,
		},
		MarketTypeFutures: {
			SelectionRoleOutright: true,
		},
	}

	for _, ty := range types {
		for _, role := range roles {
			want := allowed[ty][role]
			if got := ty.AllowsRole(role); got != want {
				t.Errorf("%v.AllowsRole(%v) = %v, want %v", ty, role, got, want)
			}
		}
	}

	// A spread admits no draw. The handicap is quoted in half points precisely to
	// eliminate the tie, so this is a rule of the domain rather than an omission.
	if MarketTypeSpread.AllowsRole(SelectionRoleDraw) {
		t.Error("a spread admits a draw selection")
	}
	// And undefined values on either axis are refused rather than defaulting.
	if MarketType(200).AllowsRole(SelectionRoleHome) || MarketTypeSpread.AllowsRole(SelectionRole(200)) {
		t.Error("an undefined enum value was admitted")
	}
}

// -----------------------------------------------------------------------------
// SelectionRole
// -----------------------------------------------------------------------------

func TestSelectionRole(t *testing.T) {
	wantText := map[SelectionRole]string{
		SelectionRoleHome:     "home",
		SelectionRoleAway:     "away",
		SelectionRoleDraw:     "draw",
		SelectionRoleOver:     "over",
		SelectionRoleUnder:    "under",
		SelectionRoleOutright: "outright",
	}
	for role, text := range wantText {
		if got := role.String(); got != text {
			t.Errorf("String() = %q, want %q", got, text)
		}
		if !role.Valid() {
			t.Errorf("%v is not Valid", role)
		}
		back, err := ParseSelectionRole(text)
		if err != nil || back != role {
			t.Errorf("ParseSelectionRole(%q) gave (%v, %v)", text, back, err)
		}
		marshalled, err := role.MarshalText()
		if err != nil || string(marshalled) != text {
			t.Errorf("MarshalText(%v) gave (%q, %v)", role, marshalled, err)
		}
		var target SelectionRole
		if err := target.UnmarshalText([]byte(text)); err != nil || target != role {
			t.Errorf("UnmarshalText(%q) gave (%v, %v)", text, target, err)
		}
	}

	if SelectionRoleUnknown.Valid() || SelectionRole(200).Valid() {
		t.Error("an undefined role reports Valid")
	}
	if got := SelectionRoleUnknown.String(); got != "unknown" {
		t.Errorf("the zero role renders as %q", got)
	}
	if _, err := SelectionRoleUnknown.MarshalText(); !errors.Is(err, ErrUnknownSelectionRole) {
		t.Errorf("marshalling the zero role gave %v", err)
	}

	// There is deliberately no yes/no pair: a "will X score" market is quoted as
	// over/under 0.5 so that it prices and grades through the identical machinery.
	for _, absent := range []string{"yes", "no", "Home", ""} {
		if _, err := ParseSelectionRole(absent); !errors.Is(err, ErrUnknownSelectionRole) {
			t.Errorf("ParseSelectionRole(%q) gave %v, want ErrUnknownSelectionRole", absent, err)
		}
	}
	var target SelectionRole
	if err := target.UnmarshalText([]byte("yes")); !errors.Is(err, ErrUnknownSelectionRole) {
		t.Errorf("UnmarshalText(\"yes\") gave %v", err)
	}
}

// TestSelectionRoleOpposite pins the pairing that devigging and arbitrage
// detection both depend on. Draw and outright deliberately have none: a three-way
// market's complement is the other two selections together, and a futures field can
// hold forty runners.
func TestSelectionRoleOpposite(t *testing.T) {
	pairs := map[SelectionRole]SelectionRole{
		SelectionRoleHome:  SelectionRoleAway,
		SelectionRoleAway:  SelectionRoleHome,
		SelectionRoleOver:  SelectionRoleUnder,
		SelectionRoleUnder: SelectionRoleOver,
	}
	for role, want := range pairs {
		got, ok := role.Opposite()
		if !ok {
			t.Fatalf("%v has no opposite", role)
		}
		if got != want {
			t.Errorf("%v.Opposite() = %v, want %v", role, got, want)
		}
		// The relation is an involution: the opposite of the opposite is the
		// original. Anything else would make a devigged pair disagree with itself.
		back, ok := got.Opposite()
		if !ok || back != role {
			t.Errorf("%v.Opposite().Opposite() = (%v, %v), want (%v, true)", role, back, ok, role)
		}
	}

	for _, role := range []SelectionRole{
		SelectionRoleDraw, SelectionRoleOutright, SelectionRoleUnknown, SelectionRole(200),
	} {
		got, ok := role.Opposite()
		if ok {
			t.Errorf("%v reported an opposite of %v", role, got)
		}
		if got != SelectionRoleUnknown {
			t.Errorf("%v.Opposite() returned %v alongside false", role, got)
		}
	}
}

// TestSelectionRoleDisplayOrder pins the board ordering. Home above draw above
// away is how a three-way soccer market is rendered everywhere, and over above
// under is the US convention for a total; the point of centralising it is that no
// surface invents its own comparator.
func TestSelectionRoleDisplayOrder(t *testing.T) {
	want := []SelectionRole{
		SelectionRoleHome,
		SelectionRoleDraw,
		SelectionRoleAway,
		SelectionRoleOver,
		SelectionRoleUnder,
		SelectionRoleOutright,
	}
	for i, role := range want {
		if got := role.DisplayOrder(); got != i {
			t.Errorf("%v.DisplayOrder() = %d, want %d", role, got, i)
		}
	}
	// Undefined roles sort last, so a value the system does not understand cannot
	// displace a real selection at the top of a market.
	for _, role := range []SelectionRole{SelectionRoleUnknown, SelectionRole(200)} {
		if got := role.DisplayOrder(); got != len(want) {
			t.Errorf("%v.DisplayOrder() = %d, want %d (last)", role, got, len(want))
		}
	}
}

// -----------------------------------------------------------------------------
// Status enums and their state machines
// -----------------------------------------------------------------------------

func TestEventStatusEnum(t *testing.T) {
	wantText := map[EventStatus]string{
		EventStatusScheduled: "scheduled",
		EventStatusLive:      "live",
		EventStatusSuspended: "suspended",
		EventStatusEnded:     "ended",
		EventStatusSettled:   "settled",
		EventStatusPostponed: "postponed",
		EventStatusCancelled: "cancelled",
	}
	for status, text := range wantText {
		if got := status.String(); got != text {
			t.Errorf("String() = %q, want %q", got, text)
		}
		if !status.Valid() {
			t.Errorf("%v is not Valid", status)
		}
		back, err := ParseEventStatus(text)
		if err != nil || back != status {
			t.Errorf("ParseEventStatus(%q) gave (%v, %v)", text, back, err)
		}
		marshalled, err := status.MarshalText()
		if err != nil || string(marshalled) != text {
			t.Errorf("MarshalText(%v) gave (%q, %v)", status, marshalled, err)
		}
		var target EventStatus
		if err := target.UnmarshalText([]byte(text)); err != nil || target != status {
			t.Errorf("UnmarshalText(%q) gave (%v, %v)", text, target, err)
		}
	}
	if EventStatusUnknown.Valid() || EventStatus(200).Valid() {
		t.Error("an undefined event status reports Valid")
	}
	if got := EventStatusUnknown.String(); got != "unknown" {
		t.Errorf("the zero status renders as %q", got)
	}
	if _, err := EventStatusUnknown.MarshalText(); !errors.Is(err, ErrUnknownEventStatus) {
		t.Errorf("marshalling the zero status gave %v", err)
	}
	if _, err := ParseEventStatus("abandoned"); !errors.Is(err, ErrUnknownEventStatus) {
		t.Errorf("ParseEventStatus(\"abandoned\") gave %v", err)
	}
	var target EventStatus
	if err := target.UnmarshalText([]byte("abandoned")); !errors.Is(err, ErrUnknownEventStatus) {
		t.Errorf("UnmarshalText on an undefined status gave %v", err)
	}
}

// TestEventStatusPredicates pins the three questions the rest of the system asks
// of a status. AcceptsWagers is the one that matters most: a status that wrongly
// answered true would let a bet be placed on a contest already under way.
func TestEventStatusPredicates(t *testing.T) {
	type expect struct{ inPlay, started, terminal, accepts bool }
	want := map[EventStatus]expect{
		EventStatusUnknown:   {false, false, false, false},
		EventStatusScheduled: {false, false, false, true},
		EventStatusLive:      {true, true, false, true},
		EventStatusSuspended: {true, true, false, false},
		EventStatusEnded:     {false, true, false, false},
		EventStatusSettled:   {false, true, true, false},
		EventStatusPostponed: {false, false, false, false},
		// Cancelled counts as started. A contest abandoned mid-play reaches
		// cancelled from live or suspended and still carries a partial score that
		// grading needs, so HasStarted must admit it or Event.WithScore would
		// refuse the very update settlement depends on. See the open item noted
		// below the table.
		EventStatusCancelled: {false, true, false, false},
	}
	for status, e := range want {
		if got := status.IsInPlay(); got != e.inPlay {
			t.Errorf("%v.IsInPlay() = %v, want %v", status, got, e.inPlay)
		}
		if got := status.HasStarted(); got != e.started {
			t.Errorf("%v.HasStarted() = %v, want %v", status, got, e.started)
		}
		if got := status.IsTerminal(); got != e.terminal {
			t.Errorf("%v.IsTerminal() = %v, want %v", status, got, e.terminal)
		}
		if got := status.AcceptsWagers(); got != e.accepts {
			t.Errorf("%v.AcceptsWagers() = %v, want %v", status, got, e.accepts)
		}
	}

	// The looseness this admits, recorded rather than hidden: an event cancelled
	// straight out of scheduled — a postponement that became an abandonment
	// without a ball being thrown — also answers true here, so WithScore would
	// accept a score for a contest that never started. The status alone cannot
	// distinguish the two cancellations; separating them needs a record of the
	// status the event was cancelled FROM, which is a settlement-phase concern and
	// not a change to make inside phase 1.
	if !EventStatusCancelled.HasStarted() {
		t.Error("the note above is stale: cancelled no longer counts as started")
	}

	// Every in-play status has started, which is the invariant WithScore and
	// WithClock both lean on.
	for status := range want {
		if status.IsInPlay() && !status.HasStarted() {
			t.Errorf("%v is in play but has not started", status)
		}
	}
	// Settled is the only terminal status.
	terminal := 0
	for status := range want {
		if status.IsTerminal() {
			terminal++
			if status != EventStatusSettled {
				t.Errorf("%v is terminal; only settled should be", status)
			}
		}
	}
	if terminal != 1 {
		t.Errorf("%d terminal statuses, want exactly 1", terminal)
	}
}

// TestEventStatusTransitionTable checks every ordered pair of statuses against the
// table written in the type's own documentation, including the invalid zero value
// on both sides. An exhaustive table is the only honest way to test a state
// machine: spot checks pass just as happily when an edge has been added by
// accident as when it has not.
func TestEventStatusTransitionTable(t *testing.T) {
	all := []EventStatus{
		EventStatusUnknown, EventStatusScheduled, EventStatusLive, EventStatusSuspended,
		EventStatusEnded, EventStatusSettled, EventStatusPostponed, EventStatusCancelled,
	}
	legal := map[EventStatus][]EventStatus{
		EventStatusScheduled: {EventStatusLive, EventStatusPostponed, EventStatusCancelled},
		EventStatusLive:      {EventStatusSuspended, EventStatusEnded, EventStatusCancelled},
		EventStatusSuspended: {EventStatusLive, EventStatusEnded, EventStatusPostponed, EventStatusCancelled},
		EventStatusEnded:     {EventStatusSettled},
		EventStatusPostponed: {EventStatusScheduled, EventStatusCancelled},
		EventStatusCancelled: {EventStatusSettled},
		EventStatusSettled:   {}, // terminal
	}

	for _, from := range all {
		permitted := map[EventStatus]bool{}
		for _, to := range legal[from] {
			permitted[to] = true
		}
		for _, to := range all {
			// s → s is legal for every valid status, so that at-least-once
			// redelivery of an update is a no-op rather than an error.
			want := from.Valid() && to.Valid() && (from == to || permitted[to])
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%v → %v: CanTransitionTo = %v, want %v", from, to, got, want)
			}
		}
	}

	// The two rules worth naming separately, since they are the ones a future
	// edit is most likely to relax by accident.
	if EventStatusSettled.CanTransitionTo(EventStatusLive) {
		t.Error("a settled event can go back to live")
	}
	if EventStatusEnded.CanTransitionTo(EventStatusLive) {
		t.Error("an ended event can go back to live")
	}
}

func TestMarketStatusEnum(t *testing.T) {
	wantText := map[MarketStatus]string{
		MarketStatusOpen:      "open",
		MarketStatusSuspended: "suspended",
		MarketStatusClosed:    "closed",
		MarketStatusSettled:   "settled",
		MarketStatusVoided:    "voided",
	}
	for status, text := range wantText {
		if got := status.String(); got != text {
			t.Errorf("String() = %q, want %q", got, text)
		}
		if !status.Valid() {
			t.Errorf("%v is not Valid", status)
		}
		back, err := ParseMarketStatus(text)
		if err != nil || back != status {
			t.Errorf("ParseMarketStatus(%q) gave (%v, %v)", text, back, err)
		}
		marshalled, err := status.MarshalText()
		if err != nil || string(marshalled) != text {
			t.Errorf("MarshalText(%v) gave (%q, %v)", status, marshalled, err)
		}
		var target MarketStatus
		if err := target.UnmarshalText([]byte(text)); err != nil || target != status {
			t.Errorf("UnmarshalText(%q) gave (%v, %v)", text, target, err)
		}
	}
	if MarketStatusUnknown.Valid() || MarketStatus(200).Valid() {
		t.Error("an undefined market status reports Valid")
	}
	if got := MarketStatusUnknown.String(); got != "unknown" {
		t.Errorf("the zero status renders as %q", got)
	}
	if _, err := MarketStatusUnknown.MarshalText(); !errors.Is(err, ErrUnknownMarketStatus) {
		t.Errorf("marshalling the zero status gave %v", err)
	}
	if _, err := ParseMarketStatus("halted"); !errors.Is(err, ErrUnknownMarketStatus) {
		t.Errorf("ParseMarketStatus(\"halted\") gave %v", err)
	}
	var target MarketStatus
	if err := target.UnmarshalText([]byte("halted")); !errors.Is(err, ErrUnknownMarketStatus) {
		t.Errorf("UnmarshalText on an undefined status gave %v", err)
	}

	for status, accepts := range map[MarketStatus]bool{
		MarketStatusOpen:      true,
		MarketStatusSuspended: false,
		MarketStatusClosed:    false,
		MarketStatusSettled:   false,
		MarketStatusVoided:    false,
		MarketStatusUnknown:   false,
	} {
		if got := status.AcceptsWagers(); got != accepts {
			t.Errorf("%v.AcceptsWagers() = %v, want %v", status, got, accepts)
		}
	}
	for status, terminal := range map[MarketStatus]bool{
		MarketStatusOpen:      false,
		MarketStatusSuspended: false,
		MarketStatusClosed:    false,
		MarketStatusSettled:   true,
		MarketStatusVoided:    true,
		MarketStatusUnknown:   false,
	} {
		if got := status.IsTerminal(); got != terminal {
			t.Errorf("%v.IsTerminal() = %v, want %v", status, got, terminal)
		}
	}
}

// TestMarketStatusTransitionTable is the market's exhaustive state machine, and it
// exists mainly to pin one absent edge: there is no path from closed back to open.
// Closing is what a market does when its outcome is being determined, and reopening
// it would let a wager be placed on a known result — the single worst bug this state
// machine can have.
func TestMarketStatusTransitionTable(t *testing.T) {
	all := []MarketStatus{
		MarketStatusUnknown, MarketStatusOpen, MarketStatusSuspended,
		MarketStatusClosed, MarketStatusSettled, MarketStatusVoided,
	}
	legal := map[MarketStatus][]MarketStatus{
		MarketStatusOpen:      {MarketStatusSuspended, MarketStatusClosed, MarketStatusVoided},
		MarketStatusSuspended: {MarketStatusOpen, MarketStatusClosed, MarketStatusVoided},
		MarketStatusClosed:    {MarketStatusSettled, MarketStatusVoided},
		MarketStatusSettled:   {},
		MarketStatusVoided:    {},
	}

	for _, from := range all {
		permitted := map[MarketStatus]bool{}
		for _, to := range legal[from] {
			permitted[to] = true
		}
		for _, to := range all {
			want := from.Valid() && to.Valid() && (from == to || permitted[to])
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%v → %v: CanTransitionTo = %v, want %v", from, to, got, want)
			}
		}
	}

	if MarketStatusClosed.CanTransitionTo(MarketStatusOpen) {
		t.Error("a closed market can be reopened; a wager could then be placed on a known result")
	}
	if MarketStatusSettled.CanTransitionTo(MarketStatusVoided) {
		t.Error("a settled market can be voided")
	}
}

func TestEventKindEnum(t *testing.T) {
	for kind, text := range map[EventKind]string{
		EventKindMatch:    "match",
		EventKindOutright: "outright",
	} {
		if got := kind.String(); got != text {
			t.Errorf("String() = %q, want %q", got, text)
		}
		if !kind.Valid() {
			t.Errorf("%v is not Valid", kind)
		}
		back, err := ParseEventKind(text)
		if err != nil || back != kind {
			t.Errorf("ParseEventKind(%q) gave (%v, %v)", text, back, err)
		}
		marshalled, err := kind.MarshalText()
		if err != nil || string(marshalled) != text {
			t.Errorf("MarshalText(%v) gave (%q, %v)", kind, marshalled, err)
		}
		var target EventKind
		if err := target.UnmarshalText([]byte(text)); err != nil || target != kind {
			t.Errorf("UnmarshalText(%q) gave (%v, %v)", text, target, err)
		}
	}
	if EventKindUnknown.Valid() || EventKind(200).Valid() {
		t.Error("an undefined event kind reports Valid")
	}
	if got := EventKindUnknown.String(); got != "unknown" {
		t.Errorf("the zero kind renders as %q", got)
	}
	if _, err := EventKindUnknown.MarshalText(); !errors.Is(err, ErrUnknownEventKind) {
		t.Errorf("marshalling the zero kind gave %v", err)
	}
	if _, err := ParseEventKind("tournament"); !errors.Is(err, ErrUnknownEventKind) {
		t.Errorf("ParseEventKind(\"tournament\") gave %v", err)
	}
	var target EventKind
	if err := target.UnmarshalText([]byte("tournament")); !errors.Is(err, ErrUnknownEventKind) {
		t.Errorf("UnmarshalText on an undefined kind gave %v", err)
	}
}

// -----------------------------------------------------------------------------
// Competitor, Score, GameClock
// -----------------------------------------------------------------------------

func TestNewCompetitor(t *testing.T) {
	// The identifier is optional, because providers frequently supply a display
	// name and nothing else and refusing the event over a missing surrogate key
	// would drop real markets.
	anonymous, err := NewCompetitor("", "Boston Celtics")
	if err != nil {
		t.Fatalf("NewCompetitor with no id: %v", err)
	}
	if !anonymous.ID().IsZero() {
		t.Errorf("id = %q, want the zero id", anonymous.ID())
	}
	if anonymous.Name() != "Boston Celtics" || anonymous.String() != "Boston Celtics" {
		t.Errorf("name = %q, String = %q", anonymous.Name(), anonymous.String())
	}
	if anonymous.IsZero() {
		t.Error("a named competitor reports IsZero")
	}

	identified := mustCompetitor(t, "cmp-bos", "Boston Celtics")
	if identified.ID() != "cmp-bos" {
		t.Errorf("id = %q", identified.ID())
	}
	if !(Competitor{}).IsZero() {
		t.Error("the zero Competitor does not report IsZero")
	}
	if got := (Competitor{}).String(); got != "<none>" {
		t.Errorf("zero String() = %q, want %q", got, "<none>")
	}

	// A present but malformed id is still rejected — optional means absent or
	// valid, not absent or anything.
	if _, err := NewCompetitor("has space", "Boston Celtics"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a malformed id gave %v, want a refusal", err)
	}
	if _, err := NewCompetitor("cmp-bos", "  "); !errors.Is(err, ErrEmptyName) {
		t.Errorf("an empty name gave %v, want ErrEmptyName", err)
	}
}

// TestNewScore covers the two derived quantities that settlement grades against:
// Margin for a spread and Total for a total. The expected values are the plain
// arithmetic, and the signs matter — a spread graded against a margin of the wrong
// sign pays the losing side.
func TestNewScore(t *testing.T) {
	cases := []struct {
		home, away     int
		margin, total  int
		representation string
	}{
		{0, 0, 0, 0, "0-0"},
		{110, 104, 6, 214, "110-104"},
		{104, 110, -6, 214, "104-110"},
		{3, 0, 3, 3, "3-0"},
	}
	for _, c := range cases {
		s, err := NewScore(c.home, c.away)
		if err != nil {
			t.Fatalf("NewScore(%d, %d): %v", c.home, c.away, err)
		}
		if s.Home() != c.home || s.Away() != c.away {
			t.Errorf("accessors gave %d-%d", s.Home(), s.Away())
		}
		if got := s.Margin(); got != c.margin {
			t.Errorf("%s.Margin() = %d, want %d", c.representation, got, c.margin)
		}
		if got := s.Total(); got != c.total {
			t.Errorf("%s.Total() = %d, want %d", c.representation, got, c.total)
		}
		if got := s.String(); got != c.representation {
			t.Errorf("String() = %q, want %q", got, c.representation)
		}
	}

	for _, c := range [][2]int{{-1, 0}, {0, -1}, {-1, -1}} {
		if _, err := NewScore(c[0], c[1]); !errors.Is(err, ErrNegativeScore) {
			t.Errorf("NewScore(%d, %d) gave %v, want ErrNegativeScore", c[0], c[1], err)
		}
	}
}

func TestNewGameClock(t *testing.T) {
	c, err := NewGameClock(3, 7*time.Minute+34*time.Second, true)
	if err != nil {
		t.Fatalf("NewGameClock: %v", err)
	}
	if c.Period() != 3 || c.Elapsed() != 7*time.Minute+34*time.Second || !c.Running() {
		t.Errorf("accessors gave (%d, %s, %v)", c.Period(), c.Elapsed(), c.Running())
	}
	if c.IsZero() {
		t.Error("a constructed clock reports IsZero")
	}
	if got, want := c.String(), "P3 7m34s (running)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	stopped, err := NewGameClock(1, 0, false)
	if err != nil {
		t.Fatalf("NewGameClock at zero elapsed: %v", err)
	}
	if got, want := stopped.String(), "P1 0s (stopped)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if !(GameClock{}).IsZero() {
		t.Error("the zero GameClock does not report IsZero")
	}

	// Period is 1-based — baseball carries the inning here with zero elapsed, so
	// period 0 is a parse error rather than "before the game".
	for _, period := range []int{0, -1, MaxClockPeriod + 1} {
		if _, err := NewGameClock(period, time.Minute, true); !errors.Is(err, ErrInvalidPeriod) {
			t.Errorf("NewGameClock(period=%d) gave %v, want ErrInvalidPeriod", period, err)
		}
	}
	if _, err := NewGameClock(MaxClockPeriod, time.Minute, true); err != nil {
		t.Errorf("the boundary period %d was rejected: %v", MaxClockPeriod, err)
	}
	for _, elapsed := range []time.Duration{-time.Second, MaxClockElapsed + time.Second} {
		if _, err := NewGameClock(1, elapsed, true); !errors.Is(err, ErrInvalidElapsed) {
			t.Errorf("NewGameClock(elapsed=%s) gave %v, want ErrInvalidElapsed", elapsed, err)
		}
	}
	if _, err := NewGameClock(1, MaxClockElapsed, true); err != nil {
		t.Errorf("the boundary elapsed %s was rejected: %v", MaxClockElapsed, err)
	}
}

// -----------------------------------------------------------------------------
// Event
// -----------------------------------------------------------------------------

func TestNewEvent(t *testing.T) {
	e := matchEvent(t)

	if e.ID() != "evt-1" || e.LeagueID() != "lg-nba" || e.Kind() != EventKindMatch {
		t.Errorf("accessors gave (%s, %s, %v)", e.ID(), e.LeagueID(), e.Kind())
	}
	if e.Name() != "Celtics at Lakers" || e.Status() != EventStatusScheduled {
		t.Errorf("name = %q, status = %v", e.Name(), e.Status())
	}
	if e.Home().Name() != "Los Angeles Lakers" || e.Away().Name() != "Boston Celtics" {
		t.Errorf("competitors are %s / %s", e.Home(), e.Away())
	}
	if !e.ScheduledStart().Equal(testTip) || !e.UpdatedAt().Equal(testObserve) {
		t.Errorf("times are %s / %s", e.ScheduledStart(), e.UpdatedAt())
	}
	if e.IsZero() {
		t.Error("a constructed event reports IsZero")
	}
	if _, ok := e.Clock(); ok {
		t.Error("a scheduled event carries a clock")
	}
	if _, ok := e.Score(); ok {
		t.Error("a scheduled event carries a score")
	}
	if got, want := e.String(), `event(evt-1 "Celtics at Lakers" scheduled)`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (Event{}).String(), "event(<zero>)"; got != want {
		t.Errorf("zero String() = %q, want %q", got, want)
	}

	// Times are normalised to UTC rather than rejected for carrying an offset:
	// providers emit RFC 3339 with zone offsets, and normalising once here means
	// every later comparison is between two UTC instants.
	tokyo := time.FixedZone("JST", 9*3600)
	offset, err := NewEvent(EventParams{
		ID: "evt-2", LeagueID: "lg-nba", Kind: EventKindMatch, Name: "Away at Home",
		Home:           mustCompetitor(t, "cmp-a", "Home"),
		Away:           mustCompetitor(t, "cmp-b", "Away"),
		ScheduledStart: testTip.In(tokyo),
		Status:         EventStatusScheduled,
		UpdatedAt:      testObserve.In(tokyo),
	})
	if err != nil {
		t.Fatalf("NewEvent with an offset: %v", err)
	}
	if loc := offset.ScheduledStart().Location(); loc != time.UTC {
		t.Errorf("scheduled start kept location %v", loc)
	}
	if loc := offset.UpdatedAt().Location(); loc != time.UTC {
		t.Errorf("updated at kept location %v", loc)
	}
	if !offset.ScheduledStart().Equal(testTip) {
		t.Error("normalisation to UTC changed the instant")
	}
}

// TestNewEventCompetitorRules covers the shape rule that makes the two event kinds
// worth distinguishing: a match needs both sides, and an outright must have
// neither. The alternative — optional competitors on every event — is how "the home
// team is empty" becomes a runtime surprise in the middle of a board render.
func TestNewEventCompetitorRules(t *testing.T) {
	home := mustCompetitor(t, "cmp-lal", "Los Angeles Lakers")
	away := mustCompetitor(t, "cmp-bos", "Boston Celtics")
	base := EventParams{
		ID: "evt-1", LeagueID: "lg-nba", Name: "Celtics at Lakers",
		ScheduledStart: testTip, Status: EventStatusScheduled, UpdatedAt: testObserve,
	}

	cases := []struct {
		name  string
		mutot func(*EventParams)
		want  error
	}{
		{"match with no competitors", func(p *EventParams) { p.Kind = EventKindMatch }, ErrCompetitorsRequired},
		{"match with only a home side", func(p *EventParams) { p.Kind = EventKindMatch; p.Home = home }, ErrCompetitorsRequired},
		{"match with only an away side", func(p *EventParams) { p.Kind = EventKindMatch; p.Away = away }, ErrCompetitorsRequired},
		{"outright with a home side", func(p *EventParams) { p.Kind = EventKindOutright; p.Home = home }, ErrCompetitorsNotApplicable},
		{"outright with an away side", func(p *EventParams) { p.Kind = EventKindOutright; p.Away = away }, ErrCompetitorsNotApplicable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mutot(&p)
			if _, err := NewEvent(p); !errors.Is(err, c.want) {
				t.Fatalf("NewEvent error = %v, want %v", err, c.want)
			}
		})
	}

	// And both well-formed shapes are accepted.
	match := base
	match.Kind, match.Home, match.Away = EventKindMatch, home, away
	if _, err := NewEvent(match); err != nil {
		t.Errorf("a well-formed match was rejected: %v", err)
	}
	outright := base
	outright.Kind, outright.Name = EventKindOutright, "2027 NBA Champion"
	got, err := NewEvent(outright)
	if err != nil {
		t.Fatalf("a well-formed outright was rejected: %v", err)
	}
	if !got.Home().IsZero() || !got.Away().IsZero() {
		t.Error("an outright came back with competitors")
	}
}

func TestNewEventRejectsBadInput(t *testing.T) {
	base := EventParams{
		ID: "evt-1", LeagueID: "lg-nba", Kind: EventKindMatch, Name: "Celtics at Lakers",
		Home:           mustCompetitor(t, "cmp-lal", "Los Angeles Lakers"),
		Away:           mustCompetitor(t, "cmp-bos", "Boston Celtics"),
		ScheduledStart: testTip, Status: EventStatusScheduled, UpdatedAt: testObserve,
	}
	cases := []struct {
		name  string
		mutot func(*EventParams)
		want  error
	}{
		{"empty id", func(p *EventParams) { p.ID = "" }, ErrEmptyID},
		{"empty league id", func(p *EventParams) { p.LeagueID = "" }, ErrEmptyID},
		{"unknown kind", func(p *EventParams) { p.Kind = EventKindUnknown }, ErrUnknownEventKind},
		{"unknown status", func(p *EventParams) { p.Status = EventStatusUnknown }, ErrUnknownEventStatus},
		{"empty name", func(p *EventParams) { p.Name = "" }, ErrEmptyName},
		{"zero scheduled start", func(p *EventParams) { p.ScheduledStart = time.Time{} }, ErrZeroTime},
		{"zero updated at", func(p *EventParams) { p.UpdatedAt = time.Time{} }, ErrZeroTime},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mutot(&p)
			got, err := NewEvent(p)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewEvent error = %v, want %v", err, c.want)
			}
			if !got.IsZero() {
				t.Errorf("a rejected event came back non-zero: %s", got)
			}
		})
	}
}

func TestEventTimingHelpers(t *testing.T) {
	e := matchEvent(t)

	if e.HasStartedBy(testTip.Add(-time.Nanosecond)) {
		t.Error("the event had started a nanosecond before tip-off")
	}
	// The boundary is inclusive: at exactly the scheduled instant, it has started.
	if !e.HasStartedBy(testTip) {
		t.Error("the event had not started at exactly its scheduled instant")
	}
	if !e.HasStartedBy(testTip.Add(time.Hour)) {
		t.Error("the event had not started an hour after tip-off")
	}

	if got, want := e.TimeToStart(testObserve), 90*time.Minute; got != want {
		t.Errorf("TimeToStart = %s, want %s", got, want)
	}
	if got := e.TimeToStart(testTip); got != 0 {
		t.Errorf("TimeToStart at tip-off = %s, want 0", got)
	}
	// Negative after the start, which is what an adaptive poller keys off to tell
	// a near-tip event from one already under way.
	if got, want := e.TimeToStart(testTip.Add(20*time.Minute)), -20*time.Minute; got != want {
		t.Errorf("TimeToStart after tip = %s, want %s", got, want)
	}

	if !e.AcceptsWagers() || e.IsInPlay() || e.IsTerminal() {
		t.Error("a scheduled event's delegated predicates are wrong")
	}
}

// TestEventWithStatusClearsTheClockOnLeavingPlay covers the rule that an ended
// event must not still report "Q3 7:34". The clock is dropped by WithStatus rather
// than by the caller, because a stale clock is a lie the UI would render happily.
func TestEventWithStatusClearsTheClockOnLeavingPlay(t *testing.T) {
	e := matchEvent(t)

	live, err := e.WithStatus(EventStatusLive, testTip)
	if err != nil {
		t.Fatalf("scheduled → live: %v", err)
	}
	clock, err := NewGameClock(3, 7*time.Minute+34*time.Second, true)
	if err != nil {
		t.Fatalf("NewGameClock: %v", err)
	}
	ticking, err := live.WithClock(clock, testTip.Add(time.Hour))
	if err != nil {
		t.Fatalf("WithClock: %v", err)
	}
	if got, ok := ticking.Clock(); !ok || got != clock {
		t.Fatalf("Clock() = (%v, %v), want the clock we set", got, ok)
	}

	// Suspended is still in play, so the clock survives.
	suspended, err := ticking.WithStatus(EventStatusSuspended, testTip.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("live → suspended: %v", err)
	}
	if _, ok := suspended.Clock(); !ok {
		t.Error("suspending the event dropped the clock; suspended is still in play")
	}

	// Ended is not, so it must be dropped.
	ended, err := suspended.WithStatus(EventStatusEnded, testTip.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("suspended → ended: %v", err)
	}
	if got, ok := ended.Clock(); ok {
		t.Errorf("an ended event still reports the clock %s", got)
	}

	// The original is untouched: every state change returns a new value.
	if _, ok := ticking.Clock(); !ok {
		t.Error("the source event was mutated")
	}
	if ticking.Status() != EventStatusLive {
		t.Errorf("the source event's status became %v", ticking.Status())
	}
}

func TestEventWithStatusRejections(t *testing.T) {
	e := matchEvent(t)

	if _, err := e.WithStatus(EventStatusUnknown, testTip); !errors.Is(err, ErrUnknownEventStatus) {
		t.Errorf("→ unknown gave %v, want ErrUnknownEventStatus", err)
	}
	if _, err := e.WithStatus(EventStatus(200), testTip); !errors.Is(err, ErrUnknownEventStatus) {
		t.Errorf("→ an undefined status gave %v", err)
	}
	// scheduled → ended skips the whole contest.
	if _, err := e.WithStatus(EventStatusEnded, testTip); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("scheduled → ended gave %v, want ErrIllegalTransition", err)
	}
	if _, err := e.WithStatus(EventStatusLive, time.Time{}); !errors.Is(err, ErrZeroTime) {
		t.Errorf("a zero stamp gave %v, want ErrZeroTime", err)
	}

	// Out-of-order redelivery must not resurrect an earlier state. An update
	// stamped at exactly UpdatedAt is accepted, since two observations can share
	// an instant; one stamped before it is refused.
	if _, err := e.WithStatus(EventStatusLive, testObserve.Add(-time.Nanosecond)); !errors.Is(err, ErrStaleUpdate) {
		t.Errorf("a stale stamp gave %v, want ErrStaleUpdate", err)
	}
	same, err := e.WithStatus(EventStatusLive, testObserve)
	if err != nil {
		t.Fatalf("an update at exactly UpdatedAt was refused: %v", err)
	}
	if !same.UpdatedAt().Equal(testObserve) {
		t.Errorf("UpdatedAt became %s", same.UpdatedAt())
	}
	// Redelivery of the same status is a no-op rather than an error.
	if _, err := e.WithStatus(EventStatusScheduled, testObserve); err != nil {
		t.Errorf("re-applying the current status gave %v", err)
	}
}

func TestEventClockAndScoreRules(t *testing.T) {
	e := matchEvent(t)
	clock, err := NewGameClock(1, time.Minute, true)
	if err != nil {
		t.Fatalf("NewGameClock: %v", err)
	}
	score, err := NewScore(110, 104)
	if err != nil {
		t.Fatalf("NewScore: %v", err)
	}

	// A clock is only meaningful in play. This is a conflict, not bad input — the
	// same clock a minute later would have been correct.
	if _, err := e.WithClock(clock, testTip); !errors.Is(err, ErrClockNotInPlay) {
		t.Errorf("clocking a scheduled event gave %v, want ErrClockNotInPlay", err)
	}
	if !errors.Is(errOfDomain(e.WithClock(clock, testTip)), ErrConflict) {
		t.Error("ErrClockNotInPlay does not reach the ErrConflict root")
	}

	live, err := e.WithStatus(EventStatusLive, testTip)
	if err != nil {
		t.Fatalf("scheduled → live: %v", err)
	}
	if _, err := live.WithClock(GameClock{}, testTip); !errors.Is(err, ErrInvalidPeriod) {
		t.Errorf("clocking with the zero clock gave %v, want ErrInvalidPeriod", err)
	}
	if _, err := live.WithClock(clock, time.Time{}); !errors.Is(err, ErrZeroTime) {
		t.Errorf("clocking with a zero stamp gave %v", err)
	}

	// A score requires the event to have started. A scheduled event reporting a
	// score is a data error, and accepting it would let a settled-looking result
	// reach the grading path.
	if _, err := e.WithScore(score, testTip); !errors.Is(err, ErrScoreNotApplicable) {
		t.Errorf("scoring a scheduled event gave %v, want ErrScoreNotApplicable", err)
	}
	scored, err := live.WithScore(score, testTip.Add(time.Hour))
	if err != nil {
		t.Fatalf("WithScore: %v", err)
	}
	if got, ok := scored.Score(); !ok || got != score {
		t.Errorf("Score() = (%v, %v), want the score we set", got, ok)
	}
	if _, err := live.WithScore(score, time.Time{}); !errors.Is(err, ErrZeroTime) {
		t.Errorf("scoring with a zero stamp gave %v", err)
	}
	// An ended event has started, so it can still take a final score.
	ended, err := live.WithStatus(EventStatusEnded, testTip.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("live → ended: %v", err)
	}
	if _, err := ended.WithScore(score, testTip.Add(3*time.Hour)); err != nil {
		t.Errorf("scoring an ended event gave %v", err)
	}

	// WithoutClock is the stoppage case, where the provider simply stops
	// reporting one. It is allowed from any status, including out of play.
	ticking, err := live.WithClock(clock, testTip.Add(time.Hour))
	if err != nil {
		t.Fatalf("WithClock: %v", err)
	}
	cleared, err := ticking.WithoutClock(testTip.Add(2 * time.Hour))
	if err != nil {
		t.Fatalf("WithoutClock: %v", err)
	}
	if _, ok := cleared.Clock(); ok {
		t.Error("WithoutClock left a clock behind")
	}
	if _, err := ticking.WithoutClock(time.Time{}); !errors.Is(err, ErrZeroTime) {
		t.Errorf("WithoutClock with a zero stamp gave %v", err)
	}
	if _, err := ticking.WithoutClock(testObserve.Add(-time.Hour)); !errors.Is(err, ErrStaleUpdate) {
		t.Errorf("WithoutClock with a stale stamp gave %v", err)
	}
}

// errOfDomain discards a constructor's value so an error can be matched inline.
func errOfDomain[T any](_ T, err error) error { return err }

// -----------------------------------------------------------------------------
// Market
// -----------------------------------------------------------------------------

func TestNewMarketLineRules(t *testing.T) {
	base := MarketParams{ID: "mkt-1", EventID: "evt-1", Status: MarketStatusOpen, UpdatedAt: testObserve}

	t.Run("forbidden types reject a line", func(t *testing.T) {
		for _, ty := range []MarketType{MarketTypeMoneyline, MarketTypeFutures} {
			p := base
			p.Type, p.Line = ty, mustLine(t, -3.5)
			if _, err := NewMarket(p); !errors.Is(err, ErrLineNotApplicable) {
				t.Errorf("%v with a line gave %v, want ErrLineNotApplicable", ty, err)
			}
			p.Line = NoLine()
			if _, err := NewMarket(p); err != nil {
				t.Errorf("%v without a line was rejected: %v", ty, err)
			}
		}
	})

	t.Run("required types reject absence", func(t *testing.T) {
		for _, ty := range []MarketType{MarketTypeSpread, MarketTypeTotal} {
			p := base
			p.Type, p.Line = ty, NoLine()
			if _, err := NewMarket(p); !errors.Is(err, ErrLineRequired) {
				t.Errorf("%v without a line gave %v, want ErrLineRequired", ty, err)
			}
		}
	})

	t.Run("a spread may be zero or negative", func(t *testing.T) {
		// Zero is a pick'em and negative is the favourite's handicap; both are
		// ordinary markets, which is exactly why a total's rule cannot be reused.
		for _, v := range []float64{-14, -3.5, 0, 3.5} {
			p := base
			p.Type, p.Line = MarketTypeSpread, mustLine(t, v)
			m, err := NewMarket(p)
			if err != nil {
				t.Errorf("a spread at %v was rejected: %v", v, err)
				continue
			}
			if got, _ := m.Line().Value(); got != v {
				t.Errorf("the spread line came back as %v, want %v", got, v)
			}
		}
	})

	t.Run("a total must be positive", func(t *testing.T) {
		// Combined scoring is non-negative by construction, so a zero or negative
		// total is a parse error rather than a tradeable market.
		for _, v := range []float64{-1, 0} {
			p := base
			p.Type, p.Line = MarketTypeTotal, mustLine(t, v)
			if _, err := NewMarket(p); !errors.Is(err, ErrLineNotPositive) {
				t.Errorf("a total at %v gave %v, want ErrLineNotPositive", v, err)
			}
		}
		p := base
		p.Type, p.Line = MarketTypeTotal, mustLine(t, 224.5)
		if _, err := NewMarket(p); err != nil {
			t.Errorf("a total at 224.5 was rejected: %v", err)
		}
	})

	t.Run("a player prop takes either", func(t *testing.T) {
		for _, line := range []Line{NoLine(), mustLine(t, 24.5)} {
			p := base
			p.Type, p.Line, p.Subject = MarketTypePlayerProp, line, "Nikola Jokić"
			if _, err := NewMarket(p); err != nil {
				t.Errorf("a player prop with line %s was rejected: %v", line, err)
			}
		}
	})
}

func TestNewMarketSubjectRules(t *testing.T) {
	base := MarketParams{ID: "mkt-1", EventID: "evt-1", Status: MarketStatusOpen, UpdatedAt: testObserve}

	prop := base
	prop.Type, prop.Line, prop.Subject = MarketTypePlayerProp, mustLine(t, 24.5), "  Nikola Jokić "
	m, err := NewMarket(prop)
	if err != nil {
		t.Fatalf("NewMarket: %v", err)
	}
	if m.Subject() != "Nikola Jokić" {
		t.Errorf("subject %q was not trimmed", m.Subject())
	}

	missing := prop
	missing.Subject = "   "
	if _, err := NewMarket(missing); !errors.Is(err, ErrSubjectRequired) {
		t.Errorf("a player prop with no subject gave %v, want ErrSubjectRequired", err)
	}

	// Every other type must leave it empty: the subject is implied by the event
	// or by the selection, and a second copy is a second thing to keep in sync.
	for _, ty := range []MarketType{MarketTypeMoneyline, MarketTypeSpread, MarketTypeTotal, MarketTypeFutures} {
		p := base
		p.Type, p.Subject = ty, "Nikola Jokić"
		if ty == MarketTypeSpread || ty == MarketTypeTotal {
			p.Line = mustLine(t, 3.5)
		}
		if _, err := NewMarket(p); !errors.Is(err, ErrSubjectNotApplicable) {
			t.Errorf("%v with a subject gave %v, want ErrSubjectNotApplicable", ty, err)
		}
	}

	// And a non-prop market comes back with an empty subject rather than a
	// silently retained one.
	moneyline := base
	moneyline.Type = MarketTypeMoneyline
	got, err := NewMarket(moneyline)
	if err != nil {
		t.Fatalf("NewMarket: %v", err)
	}
	if got.Subject() != "" {
		t.Errorf("a moneyline came back with subject %q", got.Subject())
	}
}

func TestNewMarketRejectsBadInput(t *testing.T) {
	base := MarketParams{
		ID: "mkt-1", EventID: "evt-1", Type: MarketTypeSpread,
		Line: mustLine(t, -3.5), Status: MarketStatusOpen, UpdatedAt: testObserve,
	}
	cases := []struct {
		name  string
		mutot func(*MarketParams)
		want  error
	}{
		{"empty id", func(p *MarketParams) { p.ID = "" }, ErrEmptyID},
		{"empty event id", func(p *MarketParams) { p.EventID = "" }, ErrEmptyID},
		{"unknown type", func(p *MarketParams) { p.Type = MarketTypeUnknown }, ErrUnknownMarketType},
		{"unknown status", func(p *MarketParams) { p.Status = MarketStatusUnknown }, ErrUnknownMarketStatus},
		{"zero updated at", func(p *MarketParams) { p.UpdatedAt = time.Time{} }, ErrZeroTime},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mutot(&p)
			got, err := NewMarket(p)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewMarket error = %v, want %v", err, c.want)
			}
			if !got.IsZero() {
				t.Errorf("a rejected market came back non-zero: %s", got)
			}
		})
	}
}

func TestMarketAccessorsAndBelongsTo(t *testing.T) {
	m := spreadMarket(t)
	e := matchEvent(t)

	if m.ID() != "mkt-spread" || m.EventID() != "evt-1" || m.Type() != MarketTypeSpread {
		t.Errorf("accessors gave (%s, %s, %v)", m.ID(), m.EventID(), m.Type())
	}
	if v, ok := m.Line().Value(); !ok || v != -3.5 {
		t.Errorf("Line() = (%v, %v)", v, ok)
	}
	if m.Status() != MarketStatusOpen || !m.AcceptsWagers() {
		t.Errorf("status = %v, AcceptsWagers = %v", m.Status(), m.AcceptsWagers())
	}
	if !m.UpdatedAt().Equal(testObserve) {
		t.Errorf("UpdatedAt = %s", m.UpdatedAt())
	}
	if !m.BelongsTo(e) {
		t.Error("the market does not belong to its event")
	}
	if m.BelongsTo(Event{}) || (Market{}).BelongsTo(Event{}) {
		t.Error("a market belongs to the zero event")
	}
	if m.IsZero() || !(Market{}).IsZero() {
		t.Error("IsZero is wrong for one of the two markets")
	}
	if got, want := m.String(), "market(mkt-spread spread line=-3.5 open)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (Market{}).String(), "market(<zero>)"; got != want {
		t.Errorf("zero String() = %q, want %q", got, want)
	}
}

func TestMarketWithStatusAndWithLine(t *testing.T) {
	m := spreadMarket(t)
	later := testObserve.Add(time.Minute)

	suspended, err := m.WithStatus(MarketStatusSuspended, later)
	if err != nil {
		t.Fatalf("open → suspended: %v", err)
	}
	if suspended.Status() != MarketStatusSuspended || suspended.AcceptsWagers() {
		t.Error("suspension did not take effect")
	}
	if m.Status() != MarketStatusOpen {
		t.Error("the source market was mutated")
	}
	if !suspended.UpdatedAt().Equal(later) {
		t.Errorf("UpdatedAt = %s, want %s", suspended.UpdatedAt(), later)
	}

	if _, err := m.WithStatus(MarketStatusUnknown, later); !errors.Is(err, ErrUnknownMarketStatus) {
		t.Errorf("→ unknown gave %v", err)
	}
	if _, err := m.WithStatus(MarketStatusSettled, later); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("open → settled gave %v, want ErrIllegalTransition", err)
	}
	if _, err := m.WithStatus(MarketStatusClosed, testObserve.Add(-time.Nanosecond)); !errors.Is(err, ErrStaleUpdate) {
		t.Errorf("a stale stamp gave %v, want ErrStaleUpdate", err)
	}
	if _, err := m.WithStatus(MarketStatusClosed, time.Time{}); !errors.Is(err, ErrZeroTime) {
		t.Errorf("a zero stamp gave %v", err)
	}

	// A line move is routine — it is the dataset the whole project is built on.
	moved, err := m.WithLine(mustLine(t, -4), later)
	if err != nil {
		t.Fatalf("WithLine: %v", err)
	}
	if v, _ := moved.Line().Value(); v != -4 {
		t.Errorf("the line moved to %v, want -4", v)
	}
	if v, _ := m.Line().Value(); v != -3.5 {
		t.Errorf("the source market's line became %v", v)
	}

	// But the type's rule still applies, so a spread can never become lineless
	// by way of an update.
	if _, err := m.WithLine(NoLine(), later); !errors.Is(err, ErrLineRequired) {
		t.Errorf("dropping a spread's line gave %v, want ErrLineRequired", err)
	}
	if _, err := m.WithLine(mustLine(t, -4), time.Time{}); !errors.Is(err, ErrZeroTime) {
		t.Errorf("a zero stamp gave %v", err)
	}
	if _, err := m.WithLine(mustLine(t, -4), testObserve.Add(-time.Hour)); !errors.Is(err, ErrStaleUpdate) {
		t.Errorf("a stale stamp gave %v", err)
	}

	total := mustMarket(t, MarketParams{
		ID: "mkt-total", EventID: "evt-1", Type: MarketTypeTotal,
		Line: mustLine(t, 224.5), Status: MarketStatusOpen, UpdatedAt: testObserve,
	})
	if _, err := total.WithLine(mustLine(t, 0), later); !errors.Is(err, ErrLineNotPositive) {
		t.Errorf("moving a total to zero gave %v, want ErrLineNotPositive", err)
	}
}

// -----------------------------------------------------------------------------
// Selection
// -----------------------------------------------------------------------------

func TestNewSelection(t *testing.T) {
	s := mustSelection(t, SelectionParams{
		ID: "sel-home", MarketID: "mkt-spread", Role: SelectionRoleHome, Name: "  Los Angeles Lakers ",
	})
	if s.ID() != "sel-home" || s.MarketID() != "mkt-spread" || s.Role() != SelectionRoleHome {
		t.Errorf("accessors gave (%s, %s, %v)", s.ID(), s.MarketID(), s.Role())
	}
	if s.Name() != "Los Angeles Lakers" {
		t.Errorf("name %q was not trimmed", s.Name())
	}
	if s.IsZero() || !(Selection{}).IsZero() {
		t.Error("IsZero is wrong for one of the two selections")
	}
	if got, want := s.String(), `selection(sel-home home "Los Angeles Lakers")`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (Selection{}).String(), "selection(<zero>)"; got != want {
		t.Errorf("zero String() = %q, want %q", got, want)
	}

	m := spreadMarket(t)
	if !s.BelongsTo(m) {
		t.Error("the selection does not belong to its market")
	}
	if s.BelongsTo(Market{}) || (Selection{}).BelongsTo(Market{}) {
		t.Error("a selection belongs to the zero market")
	}

	base := SelectionParams{ID: "sel-1", MarketID: "mkt-1", Role: SelectionRoleHome, Name: "Home"}
	cases := []struct {
		name  string
		mutot func(*SelectionParams)
		want  error
	}{
		{"empty id", func(p *SelectionParams) { p.ID = "" }, ErrEmptyID},
		{"empty market id", func(p *SelectionParams) { p.MarketID = "" }, ErrEmptyID},
		{"unknown role", func(p *SelectionParams) { p.Role = SelectionRoleUnknown }, ErrUnknownSelectionRole},
		{"empty name", func(p *SelectionParams) { p.Name = " " }, ErrEmptyName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mutot(&p)
			got, err := NewSelection(p)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewSelection error = %v, want %v", err, c.want)
			}
			if !got.IsZero() {
				t.Errorf("a rejected selection came back non-zero: %s", got)
			}
		})
	}
}

func TestValidateSelectionForMarket(t *testing.T) {
	m := spreadMarket(t)

	ok := mustSelection(t, SelectionParams{
		ID: "sel-home", MarketID: "mkt-spread", Role: SelectionRoleHome, Name: "Lakers",
	})
	if err := ValidateSelectionForMarket(m, ok); err != nil {
		t.Fatalf("a well-formed pair was rejected: %v", err)
	}

	orphan := mustSelection(t, SelectionParams{
		ID: "sel-x", MarketID: "mkt-other", Role: SelectionRoleHome, Name: "Lakers",
	})
	if err := ValidateSelectionForMarket(m, orphan); !errors.Is(err, ErrMismatchedParent) {
		t.Errorf("a selection naming another market gave %v, want ErrMismatchedParent", err)
	}

	// A draw on a spread: the role is valid and the parent is right, and the pair
	// is still nonsense, which is precisely the check this function exists for.
	draw := mustSelection(t, SelectionParams{
		ID: "sel-draw", MarketID: "mkt-spread", Role: SelectionRoleDraw, Name: "Draw",
	})
	if err := ValidateSelectionForMarket(m, draw); !errors.Is(err, ErrRoleNotApplicable) {
		t.Errorf("a draw on a spread gave %v, want ErrRoleNotApplicable", err)
	}
}

// TestEffectiveLineInvertsOnlyTheAwaySpread is the home-perspective convention
// stated as executable code. Reading Market.Line() directly and forgetting to
// invert for the away side is the exact bug the convention exists to prevent, and
// it produces a plausible-looking wrong number rather than an error.
func TestEffectiveLineInvertsOnlyTheAwaySpread(t *testing.T) {
	spread := spreadMarket(t)
	total := mustMarket(t, MarketParams{
		ID: "mkt-total", EventID: "evt-1", Type: MarketTypeTotal,
		Line: mustLine(t, 224.5), Status: MarketStatusOpen, UpdatedAt: testObserve,
	})
	moneyline := mustMarket(t, MarketParams{
		ID: "mkt-ml", EventID: "evt-1", Type: MarketTypeMoneyline,
		Status: MarketStatusOpen, UpdatedAt: testObserve,
	})

	cases := []struct {
		name   string
		market Market
		sel    Selection
		want   Line
	}{
		{
			name:   "home spread trades at the market line",
			market: spread,
			sel:    mustSelection(t, SelectionParams{ID: "s1", MarketID: "mkt-spread", Role: SelectionRoleHome, Name: "Lakers"}),
			want:   mustLine(t, -3.5),
		},
		{
			name:   "away spread trades at the inverse",
			market: spread,
			sel:    mustSelection(t, SelectionParams{ID: "s2", MarketID: "mkt-spread", Role: SelectionRoleAway, Name: "Celtics"}),
			want:   mustLine(t, 3.5),
		},
		{
			name:   "over shares the absolute total",
			market: total,
			sel:    mustSelection(t, SelectionParams{ID: "s3", MarketID: "mkt-total", Role: SelectionRoleOver, Name: "Over"}),
			want:   mustLine(t, 224.5),
		},
		{
			name:   "under shares the identical total, uninverted",
			market: total,
			sel:    mustSelection(t, SelectionParams{ID: "s4", MarketID: "mkt-total", Role: SelectionRoleUnder, Name: "Under"}),
			want:   mustLine(t, 224.5),
		},
		{
			name:   "a moneyline has no line on either side",
			market: moneyline,
			sel:    mustSelection(t, SelectionParams{ID: "s5", MarketID: "mkt-ml", Role: SelectionRoleAway, Name: "Celtics"}),
			want:   NoLine(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := EffectiveLine(c.market, c.sel)
			if err != nil {
				t.Fatalf("EffectiveLine: %v", err)
			}
			if !got.Equal(c.want) {
				t.Errorf("EffectiveLine = %s, want %s", got, c.want)
			}
		})
	}

	// A pick'em's away side is a pick'em that renders as "0", not "-0".
	pick := mustMarket(t, MarketParams{
		ID: "mkt-pick", EventID: "evt-1", Type: MarketTypeSpread,
		Line: mustLine(t, 0), Status: MarketStatusOpen, UpdatedAt: testObserve,
	})
	away := mustSelection(t, SelectionParams{ID: "s6", MarketID: "mkt-pick", Role: SelectionRoleAway, Name: "Celtics"})
	got, err := EffectiveLine(pick, away)
	if err != nil {
		t.Fatalf("EffectiveLine on a pick'em: %v", err)
	}
	if got.String() != "0" {
		t.Errorf("the away side of a pick'em renders as %q", got.String())
	}

	// The validation runs first, so a mismatched pair errors rather than
	// returning a line.
	if _, err := EffectiveLine(spread, away); !errors.Is(err, ErrMismatchedParent) {
		t.Errorf("EffectiveLine on a mismatched pair gave %v", err)
	}
}

// -----------------------------------------------------------------------------
// Price
// -----------------------------------------------------------------------------

func TestNewPrice(t *testing.T) {
	p := mustPrice(t, PriceParams{
		SelectionID: "sel-home", BookID: "bk-1", Decimal: 1.91,
		Line: mustLine(t, -3.5), ObservedAt: testObserve,
	})
	if p.SelectionID() != "sel-home" || p.BookID() != "bk-1" || p.Decimal() != 1.91 {
		t.Errorf("accessors gave (%s, %s, %v)", p.SelectionID(), p.BookID(), p.Decimal())
	}
	if v, ok := p.Line().Value(); !ok || v != -3.5 {
		t.Errorf("Line() = (%v, %v)", v, ok)
	}
	if !p.ObservedAt().Equal(testObserve) {
		t.Errorf("ObservedAt = %s", p.ObservedAt())
	}
	if p.IsZero() || !(Price{}).IsZero() {
		t.Error("IsZero is wrong for one of the two prices")
	}
	if got := (Price{}).String(); got != "price(<zero>)" {
		t.Errorf("zero String() = %q", got)
	}
	if got := p.String(); !strings.Contains(got, "sel-home@bk-1") || !strings.Contains(got, "line=-3.5") {
		t.Errorf("String() = %q", got)
	}

	// Observation instants are normalised to UTC, since ObservedAt is the
	// hypertable's time dimension and every later comparison must be between two
	// UTC instants.
	tokyo := time.FixedZone("JST", 9*3600)
	shifted := mustPrice(t, PriceParams{
		SelectionID: "sel-home", BookID: "bk-1", Decimal: 1.91, ObservedAt: testObserve.In(tokyo),
	})
	if loc := shifted.ObservedAt().Location(); loc != time.UTC {
		t.Errorf("ObservedAt kept location %v", loc)
	}
	if !shifted.ObservedAt().Equal(testObserve) {
		t.Error("normalisation changed the instant")
	}
}

// TestNewPriceOddsBounds covers the guard band. MinDecimalOdds is EXCLUSIVE
// because 1.0 returns the stake and nothing else — an implied probability of
// exactly 1, which divides by zero downstream — and MaxDecimalOdds is an inclusive
// sanity bound whose purpose is catching an adapter that read an American price or
// a cents field as a decimal one.
func TestNewPriceOddsBounds(t *testing.T) {
	base := PriceParams{SelectionID: "sel-1", BookID: "bk-1", ObservedAt: testObserve}

	accepted := []float64{
		math.Nextafter(MinDecimalOdds, 2), // the shortest representable price
		1.01, 1.91, 2.0, 1001, MaxDecimalOdds,
	}
	for _, d := range accepted {
		p := base
		p.Decimal = d
		if _, err := NewPrice(p); err != nil {
			t.Errorf("a decimal of %v was rejected: %v", d, err)
		}
	}

	outOfRange := []float64{MinDecimalOdds, 1.0, 0.5, 0, -1.91, MaxDecimalOdds + 1, 1e9}
	for _, d := range outOfRange {
		p := base
		p.Decimal = d
		got, err := NewPrice(p)
		if !errors.Is(err, ErrOddsOutOfRange) {
			t.Errorf("a decimal of %v gave %v, want ErrOddsOutOfRange", d, err)
		}
		if !got.IsZero() {
			t.Errorf("a rejected price came back non-zero: %s", got)
		}
	}

	// Non-finite values are caught before the range test, because NaN compares
	// false against every bound and the range message alone would be useless.
	for _, d := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := base
		p.Decimal = d
		if _, err := NewPrice(p); !errors.Is(err, ErrOddsNotFinite) {
			t.Errorf("a decimal of %v gave %v, want ErrOddsNotFinite", d, err)
		}
	}

	for _, c := range []struct {
		name  string
		mutot func(*PriceParams)
		want  error
	}{
		{"empty selection id", func(p *PriceParams) { p.SelectionID = "" }, ErrEmptyID},
		{"empty book id", func(p *PriceParams) { p.BookID = "" }, ErrEmptyID},
		{"zero observation time", func(p *PriceParams) { p.ObservedAt = time.Time{} }, ErrZeroTime},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := base
			p.Decimal = 1.91
			c.mutot(&p)
			if _, err := NewPrice(p); !errors.Is(err, c.want) {
				t.Fatalf("NewPrice error = %v, want %v", err, c.want)
			}
		})
	}
}

// TestPriceAgeAndStaleness covers the function the project's headline SLO is built
// on. The negative case is deliberate: a quote stamped in the future is clock skew
// between the ingester and the caller, and returning the negative duration rather
// than clamping to zero is what lets a monitor see the skew instead of reporting
// healthy staleness.
func TestPriceAgeAndStaleness(t *testing.T) {
	p := mustPrice(t, PriceParams{
		SelectionID: "sel-1", BookID: "bk-1", Decimal: 1.91, ObservedAt: testObserve,
	})

	cases := []struct {
		now  time.Time
		want time.Duration
	}{
		{testObserve, 0},
		{testObserve.Add(2 * time.Second), 2 * time.Second},
		{testObserve.Add(-5 * time.Second), -5 * time.Second},
	}
	for _, c := range cases {
		if got := p.Age(c.now); got != c.want {
			t.Errorf("Age at %s = %s, want %s", c.now, got, c.want)
		}
	}

	ttl := time.Second
	if p.IsStale(testObserve, ttl) {
		t.Error("a quote is stale at the instant it was observed")
	}
	if p.IsStale(testObserve.Add(ttl), ttl) {
		t.Error("a quote is stale at exactly its TTL; the comparison must be strict")
	}
	if !p.IsStale(testObserve.Add(ttl+time.Nanosecond), ttl) {
		t.Error("a quote one nanosecond past its TTL is not stale")
	}
	// A skewed quote is never stale, however long ago the TTL was.
	if p.IsStale(testObserve.Add(-time.Hour), ttl) {
		t.Error("a quote stamped in the future is stale")
	}
	// A non-positive TTL makes every quote with a positive age stale.
	if !p.IsStale(testObserve.Add(time.Nanosecond), 0) {
		t.Error("a zero TTL did not make a one-nanosecond-old quote stale")
	}
	if p.IsStale(testObserve, 0) {
		t.Error("a zero-age quote is stale under a zero TTL")
	}
}

// TestPriceSameQuoteAs covers the change-detection primitive CLAUDE.md §5 requires:
// "most polls return identical data and must not generate bus traffic".
//
// The exact float64 comparison inside is correct and is not a float-comparison
// mistake — the question is whether the provider sent a different number, which is
// about bytes rather than numeric closeness. A tolerance would suppress genuine
// one-tick moves, which are exactly what steam detection looks for.
func TestPriceSameQuoteAs(t *testing.T) {
	base := PriceParams{
		SelectionID: "sel-1", BookID: "bk-1", Decimal: 1.91,
		Line: mustLine(t, -3.5), ObservedAt: testObserve,
	}
	original := mustPrice(t, base)

	later := base
	later.ObservedAt = testObserve.Add(time.Minute)
	repoll := mustPrice(t, later)

	if !original.SameQuoteAs(repoll) {
		t.Error("an unchanged re-poll is not recognised as the same quote")
	}
	if original.Equal(repoll) {
		t.Error("two prices at different instants compare fully equal")
	}
	if !original.Equal(mustPrice(t, base)) {
		t.Error("two identical prices do not compare fully equal")
	}

	// One tick of odds movement at the same line is a change.
	tick := base
	tick.Decimal = 1.92
	if original.SameQuoteAs(mustPrice(t, tick)) {
		t.Error("a one-tick odds move was suppressed as unchanged")
	}

	// A line move at the same odds is also a change, and this is the case that
	// makes carrying the line on the price worth it: without it, "-3.5 at 1.91"
	// followed by "-4 at 1.91" would look like no update at all.
	moved := base
	moved.Line = mustLine(t, -4)
	if original.SameQuoteAs(mustPrice(t, moved)) {
		t.Error("a line move at unchanged odds was suppressed as unchanged")
	}

	// A different book quoting identically is a different quote.
	other := base
	other.BookID = "bk-2"
	if original.SameQuoteAs(mustPrice(t, other)) {
		t.Error("another book's identical quote was treated as the same quote")
	}
	otherSel := base
	otherSel.SelectionID = "sel-2"
	if original.SameQuoteAs(mustPrice(t, otherSel)) {
		t.Error("another selection's identical quote was treated as the same quote")
	}
}

func TestPriceIsNewerThan(t *testing.T) {
	base := PriceParams{SelectionID: "sel-1", BookID: "bk-1", Decimal: 1.91, ObservedAt: testObserve}
	earlier := mustPrice(t, base)
	laterParams := base
	laterParams.ObservedAt = testObserve.Add(time.Second)
	later := mustPrice(t, laterParams)

	if !later.IsNewerThan(earlier) {
		t.Error("the later price is not newer")
	}
	if earlier.IsNewerThan(later) {
		t.Error("the earlier price is newer")
	}
	// Strictly after: two observations sharing an instant order neither way, so a
	// tie cannot silently reorder a time series.
	if earlier.IsNewerThan(mustPrice(t, base)) {
		t.Error("a price is newer than another at the identical instant")
	}
}

// TestPricePayoutAndProfit is the money-integrality guarantee of CLAUDE.md §12
// stated as executable code: a float64 price multiplies an integer stake and the
// answer is still an exact integer of minor units.
//
// Every expected value below is the exact rational, written out in the comment and
// asserted as an integer of minor units, so a change in the rounding implementation
// cannot quietly redefine what is correct. The separation of PayoutFor (total
// return, stake included) from ProfitFor (net winnings) is checked in both
// directions because conflating them is the most common arithmetic error in this
// domain and both answers are plausible numbers of the right magnitude.
func TestPricePayoutAndProfit(t *testing.T) {
	stake := Money(10_000) // 100.00

	cases := []struct {
		name           string
		decimal        float64
		rounding       Rounding
		payout, profit Money
	}{
		{
			// 100.00 × 2.50 = 250.00 exactly; profit 150.00.
			name: "an exact multiple", decimal: 2.5, rounding: RoundHalfAwayFromZero,
			payout: 25_000, profit: 15_000,
		},
		{
			// -110 is decimal 21/11 = 1.909090…; 10000 × 21/11 = 19090.909…
			// minor units, which rounds to 19091 (190.91). Profit 90.91.
			name: "the standard spread price", decimal: 21.0 / 11.0, rounding: RoundHalfAwayFromZero,
			payout: 19_091, profit: 9_091,
		},
		{
			// The same stake truncated instead: 19090 (190.90), profit 90.90.
			// Truncation is the mode that can never overpay.
			name: "the same price truncated", decimal: 21.0 / 11.0, rounding: RoundTowardZero,
			payout: 19_090, profit: 9_090,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := mustPrice(t, PriceParams{
				SelectionID: "sel-1", BookID: "bk-1", Decimal: c.decimal, ObservedAt: testObserve,
			})
			payout, err := p.PayoutFor(stake, c.rounding)
			if err != nil {
				t.Fatalf("PayoutFor: %v", err)
			}
			if payout != c.payout {
				t.Errorf("PayoutFor(%s) = %d minor units, want %d", stake, int64(payout), int64(c.payout))
			}
			profit, err := p.ProfitFor(stake, c.rounding)
			if err != nil {
				t.Fatalf("ProfitFor: %v", err)
			}
			if profit != c.profit {
				t.Errorf("ProfitFor(%s) = %d minor units, want %d", stake, int64(profit), int64(c.profit))
			}
			// The two are related by definition, and the relation is exact
			// integer arithmetic rather than a rounding of the same float twice.
			if payout-stake != profit {
				t.Errorf("payout %d − stake %d = %d, but ProfitFor gave %d",
					int64(payout), int64(stake), int64(payout-stake), int64(profit))
			}
			// A winning bet at odds above 1 never profits less than nothing.
			if c.decimal > 1 && profit < 0 {
				t.Errorf("profit %d is negative at odds of %v", int64(profit), c.decimal)
			}
		})
	}
}

// TestPricePayoutRoundingAtAnExactTie separates the three rounding modes at a
// genuine tie.
//
// Constructing one takes care, and a first draft of this test got it wrong: 100.00
// at a decimal of 1.00005 looks like it lands on 10000.5 minor units, but neither
// 1.00005 nor the product is exactly representable in binary and the real product is
// a hair above the half, so all three modes agreed and the test proved nothing about
// tie-breaking. The tie has to be built from dyadic rationals to exist at all.
//
// 1.0625 is 17/16, exact in binary. A stake of 8 minor units pays 8 × 17/16 = 8.5
// exactly — a true tie, with 8 and 9 equidistant. Half-away-from-zero takes 9;
// half-to-even takes 8, because 8 is the even neighbour; truncation takes 8. That
// half-to-even lands lower here and higher on the next tie up is the whole point of
// the mode: over many settlements it has no directional bias, which
// half-away-from-zero does, and a book that always rounded the customer's way is a
// book that leaks money on every graded ticket.
func TestPricePayoutRoundingAtAnExactTie(t *testing.T) {
	p := mustPrice(t, PriceParams{
		SelectionID: "sel-1", BookID: "bk-1", Decimal: 1.0625, ObservedAt: testObserve,
	})

	// The tie is real before anything is asserted about how it breaks.
	if product := float64(8) * 1.0625; product != 8.5 {
		t.Fatalf("8 × 1.0625 = %v in float64, not 8.5; this test's premise is wrong", product)
	}

	cases := []struct {
		stake    Money
		rounding Rounding
		want     Money
	}{
		{8, RoundHalfAwayFromZero, 9},   // 8.5 → away from zero → 9
		{8, RoundHalfToEven, 8},         // 8.5 → the even neighbour → 8
		{8, RoundTowardZero, 8},         // 8.5 → truncated → 8
		{24, RoundHalfAwayFromZero, 26}, // 24 × 17/16 = 25.5 → 26
		{24, RoundHalfToEven, 26},       // 25.5 → the even neighbour → 26
		{24, RoundTowardZero, 25},       // 25.5 → truncated → 25
	}
	for _, c := range cases {
		got, err := p.PayoutFor(c.stake, c.rounding)
		if err != nil {
			t.Fatalf("PayoutFor(%d, %v): %v", int64(c.stake), c.rounding, err)
		}
		if got != c.want {
			t.Errorf("PayoutFor(%d minor units, %v) = %d, want %d",
				int64(c.stake), c.rounding, int64(got), int64(c.want))
		}
	}

	// Truncation is the only mode that can never pay more than the exact amount,
	// which is why it is the safe default for a stake calculation.
	for _, stake := range []Money{8, 24, 40, 56} {
		truncated, err := p.PayoutFor(stake, RoundTowardZero)
		if err != nil {
			t.Fatalf("PayoutFor: %v", err)
		}
		if exact := float64(stake) * 1.0625; float64(truncated) > exact {
			t.Errorf("truncated payout %d exceeds the exact %v", int64(truncated), exact)
		}
	}
}

// TestPricePayoutRejectsBadInput covers the two ways the money multiplication can
// refuse: an undefined rounding mode, and a stake large enough that the product
// leaves the exactly-representable range. Both must be errors rather than silently
// wrong balances.
func TestPricePayoutRejectsBadInput(t *testing.T) {
	p := mustPrice(t, PriceParams{
		SelectionID: "sel-1", BookID: "bk-1", Decimal: 2.5, ObservedAt: testObserve,
	})

	if _, err := p.PayoutFor(Money(10_000), RoundingUnknown); !errors.Is(err, ErrUnknownRounding) {
		t.Errorf("an undefined rounding mode gave %v, want ErrUnknownRounding", err)
	}
	if _, err := p.ProfitFor(Money(10_000), RoundingUnknown); !errors.Is(err, ErrUnknownRounding) {
		t.Errorf("ProfitFor did not propagate the rounding failure: %v", err)
	}

	// A stake at the top of the exactly-representable range multiplied by a price
	// above 1 cannot land back inside it.
	huge := Money(math.MaxInt64)
	if _, err := p.PayoutFor(huge, RoundHalfAwayFromZero); err == nil {
		t.Error("multiplying the largest Money by 2.5 succeeded")
	}
	if _, err := p.ProfitFor(huge, RoundHalfAwayFromZero); err == nil {
		t.Error("ProfitFor on the largest Money succeeded")
	}
}

// TestValidatePriceForSelection covers the cross-entity check whose third clause is
// the one worth having: a price whose line has drifted from its market's is not
// merely stale, it is wrong, and settling a wager against it grades the bet at a
// handicap the customer never took. Both values are individually valid, so nothing
// else in the system would notice.
func TestValidatePriceForSelection(t *testing.T) {
	market := spreadMarket(t)
	home := mustSelection(t, SelectionParams{
		ID: "sel-home", MarketID: "mkt-spread", Role: SelectionRoleHome, Name: "Lakers",
	})
	away := mustSelection(t, SelectionParams{
		ID: "sel-away", MarketID: "mkt-spread", Role: SelectionRoleAway, Name: "Celtics",
	})

	// The home side trades at the market's own line.
	good := mustPrice(t, PriceParams{
		SelectionID: "sel-home", BookID: "bk-1", Decimal: 1.91,
		Line: mustLine(t, -3.5), ObservedAt: testObserve,
	})
	if err := ValidatePriceForSelection(market, home, good); err != nil {
		t.Fatalf("a well-formed price was rejected: %v", err)
	}

	// The away side trades at the inverse, so a price carrying the market's raw
	// line is wrong even though every field in it is valid. This is the exact bug
	// the home-perspective convention exists to prevent.
	awayCorrect := mustPrice(t, PriceParams{
		SelectionID: "sel-away", BookID: "bk-1", Decimal: 1.91,
		Line: mustLine(t, 3.5), ObservedAt: testObserve,
	})
	if err := ValidatePriceForSelection(market, away, awayCorrect); err != nil {
		t.Fatalf("a correctly inverted away price was rejected: %v", err)
	}
	awayUninverted := mustPrice(t, PriceParams{
		SelectionID: "sel-away", BookID: "bk-1", Decimal: 1.91,
		Line: mustLine(t, -3.5), ObservedAt: testObserve,
	})
	if err := ValidatePriceForSelection(market, away, awayUninverted); !errors.Is(err, ErrLineMismatch) {
		t.Errorf("an uninverted away price gave %v, want ErrLineMismatch", err)
	}

	// A price naming a different selection.
	wrongSelection := mustPrice(t, PriceParams{
		SelectionID: "sel-away", BookID: "bk-1", Decimal: 1.91,
		Line: mustLine(t, -3.5), ObservedAt: testObserve,
	})
	if err := ValidatePriceForSelection(market, home, wrongSelection); !errors.Is(err, ErrMismatchedParent) {
		t.Errorf("a price quoting the wrong selection gave %v, want ErrMismatchedParent", err)
	}

	// And the selection/market check runs first, so an incompatible pair is
	// reported as such rather than as a line mismatch.
	draw := mustSelection(t, SelectionParams{
		ID: "sel-draw", MarketID: "mkt-spread", Role: SelectionRoleDraw, Name: "Draw",
	})
	drawPrice := mustPrice(t, PriceParams{
		SelectionID: "sel-draw", BookID: "bk-1", Decimal: 20, ObservedAt: testObserve,
	})
	if err := ValidatePriceForSelection(market, draw, drawPrice); !errors.Is(err, ErrRoleNotApplicable) {
		t.Errorf("a draw on a spread gave %v, want ErrRoleNotApplicable", err)
	}

	// A moneyline price carries no line on either side, and a price that invented
	// one is rejected.
	moneyline := mustMarket(t, MarketParams{
		ID: "mkt-ml", EventID: "evt-1", Type: MarketTypeMoneyline,
		Status: MarketStatusOpen, UpdatedAt: testObserve,
	})
	mlSelection := mustSelection(t, SelectionParams{
		ID: "sel-ml", MarketID: "mkt-ml", Role: SelectionRoleHome, Name: "Lakers",
	})
	mlPrice := mustPrice(t, PriceParams{
		SelectionID: "sel-ml", BookID: "bk-1", Decimal: 1.65, ObservedAt: testObserve,
	})
	if err := ValidatePriceForSelection(moneyline, mlSelection, mlPrice); err != nil {
		t.Fatalf("a lineless moneyline price was rejected: %v", err)
	}
	invented := mustPrice(t, PriceParams{
		SelectionID: "sel-ml", BookID: "bk-1", Decimal: 1.65,
		Line: mustLine(t, 0), ObservedAt: testObserve,
	})
	if err := ValidatePriceForSelection(moneyline, mlSelection, invented); !errors.Is(err, ErrLineMismatch) {
		t.Errorf("a moneyline price carrying a pick'em line gave %v, want ErrLineMismatch", err)
	}
}

// TestCatalogueErrorsReachTheirRoot checks that every sentinel this file exercises
// classifies as either invalid input or a state conflict, which is the distinction
// the API layer will map onto 4xx-versus-409 and the ingester onto drop-versus-retry.
func TestCatalogueErrorsReachTheirRoot(t *testing.T) {
	invalid := []error{
		ErrEmptyID, ErrIDTooLong, ErrEmptySlug, ErrSlugCharset, ErrEmptyName,
		ErrNameTooLong, ErrNameCharset, ErrZeroTime,
		ErrUnknownEventKind, ErrUnknownEventStatus, ErrUnknownMarketType,
		ErrUnknownMarketStatus, ErrUnknownSelectionRole, ErrUnknownBookKind,
		ErrCompetitorsRequired, ErrCompetitorsNotApplicable, ErrNegativeScore,
		ErrInvalidPeriod, ErrInvalidElapsed,
		ErrLineRequired, ErrLineNotApplicable, ErrLineNotFinite, ErrLineNotPositive,
		ErrLineSyntax, ErrSubjectRequired, ErrSubjectNotApplicable,
		ErrRoleNotApplicable, ErrMismatchedParent,
		ErrOddsNotFinite, ErrOddsOutOfRange,
	}
	conflict := []error{
		ErrStaleUpdate, ErrIllegalTransition, ErrClockNotInPlay,
		ErrScoreNotApplicable, ErrLineMismatch,
	}

	for _, err := range invalid {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%v does not reach ErrInvalid", err)
		}
		if errors.Is(err, ErrConflict) {
			t.Errorf("%v classifies as both invalid and conflicting", err)
		}
	}
	for _, err := range conflict {
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%v does not reach ErrConflict", err)
		}
		if errors.Is(err, ErrInvalid) {
			t.Errorf("%v classifies as both conflicting and invalid", err)
		}
	}

	// And the two roots are distinct, so the mapping above is not vacuous.
	if errors.Is(ErrInvalid, ErrConflict) || errors.Is(ErrConflict, ErrInvalid) {
		t.Fatal("the two error roots are not distinct")
	}
	_ = fmt.Sprint(ErrInvalid) // keep fmt used if the assertions above change
}
