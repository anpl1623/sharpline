package theoddsapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Table-driven unit tests for the mapping, per CLAUDE.md §10.
//
// # These constructed inputs are not mock provider data
//
// The distinction matters and it is worth stating. What the no-mock-data rule
// forbids is fabricated data STANDING IN for the provider: a canned HTTP
// response, a seeded row, a literal in a component. Every payload this package
// serves over HTTP comes from testdata/docsamples and nothing else — see
// golden_test.go.
//
// What is below are inputs to PURE FUNCTIONS, chosen to reach branches the two
// published samples cannot reach: an event missing one competitor, a market key
// this build does not map, a quote with no observation instant. Those states are
// real and a provider will eventually produce them; the samples simply do not
// contain them, and waiting for production to supply the test case is not a
// plan. None of this reaches a board, a database or a bus.

func testMapper(t *testing.T) (*mapper, *testHarness) {
	t.Helper()
	h := newHarness(t, newProviderStub(t), nil)
	return h.Adapter.mapper, h
}

// TestRoleForIsTheDomainCompatibilityMatrix.
//
// domain.MarketType.AllowsRole is the matrix; this is the mapping from the
// provider's outcome LABELS onto it. A wrong answer here does not error — it
// produces a selection on the wrong side of a market, which prices, grades and
// settles against machinery that was never written for it.
func TestRoleForIsTheDomainCompatibilityMatrix(t *testing.T) {
	const (
		home = "Tampa Bay Buccaneers"
		away = "Dallas Cowboys"
	)
	cases := []struct {
		name  string
		typ   domain.MarketType
		label string
		want  domain.SelectionRole
		ok    bool
	}{
		{"moneyline home", domain.MarketTypeMoneyline, home, domain.SelectionRoleHome, true},
		{"moneyline away", domain.MarketTypeMoneyline, away, domain.SelectionRoleAway, true},
		{"moneyline draw", domain.MarketTypeMoneyline, "Draw", domain.SelectionRoleDraw, true},
		{"moneyline is case-insensitive", domain.MarketTypeMoneyline, "tampa bay BUCCANEERS",
			domain.SelectionRoleHome, true},
		{"moneyline tolerates surrounding whitespace", domain.MarketTypeMoneyline, "  " + away + " ",
			domain.SelectionRoleAway, true},
		{"moneyline rejects an unknown competitor", domain.MarketTypeMoneyline, "Green Bay Packers",
			domain.SelectionRoleUnknown, false},
		{"spread home", domain.MarketTypeSpread, home, domain.SelectionRoleHome, true},
		{"spread away", domain.MarketTypeSpread, away, domain.SelectionRoleAway, true},
		// A spread has no draw: the handicap is quoted in half points precisely
		// to eliminate the tie, and domain.MarketType.AllowsRole refuses it.
		{"spread has no draw", domain.MarketTypeSpread, "Draw", domain.SelectionRoleUnknown, false},
		{"total over", domain.MarketTypeTotal, "Over", domain.SelectionRoleOver, true},
		{"total under", domain.MarketTypeTotal, "under", domain.SelectionRoleUnder, true},
		{"total rejects a team name", domain.MarketTypeTotal, home, domain.SelectionRoleUnknown, false},
		{"prop over", domain.MarketTypePlayerProp, "Over", domain.SelectionRoleOver, true},
		{"prop under", domain.MarketTypePlayerProp, "Under", domain.SelectionRoleUnder, true},
		// A named prop — "first touchdown scorer" — is a set of runners, and
		// domain permits SelectionRoleOutright on a player prop.
		{"named prop is a runner", domain.MarketTypePlayerProp, "Ja'Marr Chase",
			domain.SelectionRoleOutright, true},
		{"futures runner", domain.MarketTypeFutures, "Kansas City Chiefs", domain.SelectionRoleOutright, true},
		{"an empty label maps to nothing", domain.MarketTypeMoneyline, "   ",
			domain.SelectionRoleUnknown, false},
		{"an unknown market type maps to nothing", domain.MarketTypeUnknown, "Over",
			domain.SelectionRoleUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := roleFor(tc.typ, tc.label, home, away)
			if got != tc.want || ok != tc.ok {
				t.Errorf("roleFor(%s, %q) = (%s, %v), want (%s, %v)",
					tc.typ, tc.label, got, ok, tc.want, tc.ok)
			}
			if ok && !tc.typ.AllowsRole(got) {
				t.Errorf("roleFor produced %s, which a %s market does not admit; "+
					"provider.Snapshot.Validate would reject it", got, tc.typ)
			}
		})
	}
}

// TestMarketTypeForKeyCoversTheKeysThisBuildServes.
func TestMarketTypeForKeyCoversTheKeysThisBuildServes(t *testing.T) {
	cases := []struct {
		key  string
		want domain.MarketType
		ok   bool
	}{
		{marketKeyH2H, domain.MarketTypeMoneyline, true},
		{marketKeySpreads, domain.MarketTypeSpread, true},
		{marketKeyTotals, domain.MarketTypeTotal, true},
		{marketKeyOutrights, domain.MarketTypeFutures, true},
		{"player_pass_tds", domain.MarketTypePlayerProp, true},
		{"player_anything_at_all", domain.MarketTypePlayerProp, true},
		// Real provider keys this build does not serve. They are skipped and
		// counted, never guessed at: mapping "alternate_spreads" onto a spread
		// would put a 7.5-point alternate line in the main market's price set.
		{"alternate_spreads", domain.MarketTypeUnknown, false},
		{"spreads_h1", domain.MarketTypeUnknown, false},
		{"btts", domain.MarketTypeUnknown, false},
		{"h2h_lay", domain.MarketTypeUnknown, false},
		{"", domain.MarketTypeUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			got, ok := marketTypeForKey(tc.key)
			if got != tc.want || ok != tc.ok {
				t.Errorf("marketTypeForKey(%q) = (%s, %v), want (%s, %v)", tc.key, got, ok, tc.want, tc.ok)
			}
		})
	}

	// Every featured key must round-trip, or Cost would price a market the
	// sweep does not actually request.
	for _, typ := range []domain.MarketType{
		domain.MarketTypeMoneyline, domain.MarketTypeSpread,
		domain.MarketTypeTotal, domain.MarketTypeFutures,
	} {
		key, ok := featuredMarketKeyFor(typ)
		if !ok {
			t.Errorf("featuredMarketKeyFor(%s) has no key", typ)
			continue
		}
		if back, ok := marketTypeForKey(key); !ok || back != typ {
			t.Errorf("%s -> %q -> %s: the mapping does not round-trip", typ, key, back)
		}
	}
	if _, ok := featuredMarketKeyFor(domain.MarketTypePlayerProp); ok {
		t.Errorf("player props have a featured key; the /odds endpoint only serves featured markets " +
			"and the two endpoints bill differently")
	}
}

// -----------------------------------------------------------------------------
// Mapping losses
// -----------------------------------------------------------------------------

// rawEventBuilder assembles a neutral-shape event for a table row.
func rawEvent(mutate func(*normalizer.RawEvent)) normalizer.RawEvent {
	observed := time.Date(2026, 8, 17, 11, 59, 0, 0, time.UTC)
	point := -3.5
	away := 3.5
	ev := normalizer.RawEvent{
		ID:           "0123456789abcdef0123456789abcdef",
		SportKey:     "americanfootball",
		LeagueKey:    "americanfootball_nfl",
		LeagueName:   "NFL",
		HomeTeam:     "Tampa Bay Buccaneers",
		AwayTeam:     "Dallas Cowboys",
		CommenceTime: time.Date(2026, 9, 10, 0, 20, 0, 0, time.UTC),
		Books: []normalizer.RawBook{{
			Key:        "draftkings",
			Name:       "DraftKings",
			LastUpdate: observed,
			Markets: []normalizer.RawMarket{{
				Key:        marketKeySpreads,
				LastUpdate: observed,
				Outcomes: []normalizer.RawOutcome{
					{Name: "Tampa Bay Buccaneers", Price: 1.91, Point: &point},
					{Name: "Dallas Cowboys", Price: 1.91, Point: &away},
				},
			}},
		}},
	}
	if mutate != nil {
		mutate(&ev)
	}
	return ev
}

func TestMappingLossesAreCountedNotSwallowed(t *testing.T) {
	league := mustLeagueID(t, "americanfootball_nfl")
	fetchedAt := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		event     normalizer.RawEvent
		reason    string
		count     float64
		wantEvent bool
		why       string
	}{
		{
			name: "an event with one competitor is refused rather than guessed at",
			event: rawEvent(func(e *normalizer.RawEvent) {
				e.AwayTeam = ""
			}),
			reason: DropReasonInvalidEvent, count: 1, wantEvent: false,
			why: "the home-perspective line convention depends on both sides being known",
		},
		{
			name: "a market key this build does not serve is skipped",
			event: rawEvent(func(e *normalizer.RawEvent) {
				e.Books[0].Markets[0].Key = "alternate_spreads"
			}),
			reason: DropReasonUnsupportedMarket, count: 2, wantEvent: true,
			why: "the provider serves more markets than the charter's board shows",
		},
		{
			name: "a quote with no observation instant is dropped, never stamped with our clock",
			event: rawEvent(func(e *normalizer.RawEvent) {
				e.Books[0].LastUpdate = time.Time{}
				e.Books[0].Markets[0].LastUpdate = time.Time{}
			}),
			reason: DropReasonNoObservationInstant, count: 2, wantEvent: true,
			why: "stamping time.Now() would report zero provider staleness for ever",
		},
		{
			name: "an outcome naming neither competitor is dropped",
			event: rawEvent(func(e *normalizer.RawEvent) {
				e.Books[0].Markets[0].Outcomes[1].Name = "Green Bay Packers"
			}),
			reason: DropReasonUnmappedOutcome, count: 1, wantEvent: true,
			why: "a near-match that picked the wrong side would invert a price silently",
		},
		{
			name: "a spread with no point at all cannot be constructed",
			event: rawEvent(func(e *normalizer.RawEvent) {
				e.Books[0].Markets[0].Outcomes[0].Point = nil
				e.Books[0].Markets[0].Outcomes[1].Point = nil
			}),
			reason: DropReasonMissingLine, count: 2, wantEvent: true,
			why: "domain.LineRuleRequired means a spread without a line is not a market",
		},
		{
			name: "a player prop with no subject cannot name its market",
			event: rawEvent(func(e *normalizer.RawEvent) {
				e.Books[0].Markets[0].Key = "player_pass_tds"
				e.Books[0].Markets[0].Outcomes = []normalizer.RawOutcome{
					{Name: "Over", Price: 1.91},
					{Name: "Under", Price: 1.91},
				}
			}),
			reason: DropReasonUnmappedOutcome, count: 2, wantEvent: true,
			why: "the description is the only thing that names the player, and domain requires a subject",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, h := testMapper(t)
			snap, ok := m.mapEvent(tc.event, league, fetchedAt, nil)
			if ok != tc.wantEvent {
				t.Fatalf("mapEvent ok = %v, want %v. %s", ok, tc.wantEvent, tc.why)
			}
			if got := h.dropped(tc.reason); got != tc.count {
				t.Errorf("%s drops = %v, want %v. %s", tc.reason, got, tc.count, tc.why)
			}
			if ok {
				probe := provider.Snapshot{
					Provider: provider.NameTheOddsAPI, FetchedAt: fetchedAt,
					Scope: provider.Scope{
						League:  league,
						Markets: []domain.MarketType{domain.MarketTypeSpread, domain.MarketTypePlayerProp},
					},
					Events: []provider.EventSnapshot{snap},
				}
				if err := probe.Validate(); err != nil {
					t.Errorf("what survived does not validate: %v", err)
				}
			}
		})
	}
}

// TestMapEventStatusUsesTheProvidersOwnInPlayTest.
func TestMapEventStatusUsesTheProvidersOwnInPlayTest(t *testing.T) {
	league := mustLeagueID(t, "americanfootball_nfl")
	start := time.Date(2026, 9, 10, 0, 20, 0, 0, time.UTC)

	for _, tc := range []struct {
		name      string
		fetchedAt time.Time
		want      domain.EventStatus
	}{
		{"before kickoff", start.Add(-time.Hour), domain.EventStatusScheduled},
		{"after kickoff", start.Add(time.Hour), domain.EventStatusLive},
		{"exactly at kickoff is not yet in play", start, domain.EventStatusScheduled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := testMapper(t)
			snap, ok := m.mapEvent(rawEvent(nil), league, tc.fetchedAt, nil)
			if !ok {
				t.Fatalf("mapEvent refused a well-formed event")
			}
			if got := snap.Event.Status(); got != tc.want {
				t.Errorf("status = %s, want %s", got, tc.want)
			}
			// UpdatedAt is the ingester's observation time and must be
			// monotonic across polls; it is never a display time.
			if !snap.Event.UpdatedAt().Equal(tc.fetchedAt) {
				t.Errorf("UpdatedAt = %s, want the fetch instant %s", snap.Event.UpdatedAt(), tc.fetchedAt)
			}
		})
	}
}

// TestMapEventOutrightHasNoCompetitors.
func TestMapEventOutrightHasNoCompetitors(t *testing.T) {
	m, _ := testMapper(t)
	league := mustLeagueID(t, "americanfootball_nfl_super_bowl_winner")
	observed := time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC)

	ev := normalizer.RawEvent{
		ID:           "fedcba9876543210fedcba9876543210",
		LeagueKey:    "americanfootball_nfl_super_bowl_winner",
		LeagueName:   "NFL Super Bowl Winner",
		Name:         "NFL Super Bowl Winner",
		CommenceTime: time.Date(2027, 2, 8, 23, 30, 0, 0, time.UTC),
		Books: []normalizer.RawBook{{
			Key: "draftkings", Name: "DraftKings", LastUpdate: observed,
			Markets: []normalizer.RawMarket{{
				Key: marketKeyOutrights, LastUpdate: observed,
				Outcomes: []normalizer.RawOutcome{
					{Name: "Kansas City Chiefs", Price: 7.0},
					{Name: "San Francisco 49ers", Price: 9.0},
				},
			}},
		}},
	}

	snap, ok := m.mapEvent(ev, league, observed, nil)
	if !ok {
		t.Fatalf("mapEvent refused an outright")
	}
	if got, want := snap.Event.Kind(), domain.EventKindOutright; got != want {
		t.Errorf("kind = %s, want %s", got, want)
	}
	if len(snap.Markets) != 1 {
		t.Fatalf("outright produced %d markets, want 1", len(snap.Markets))
	}
	mk := snap.Markets[0]
	if got, want := mk.Market.Type(), domain.MarketTypeFutures; got != want {
		t.Errorf("market type = %s, want %s", got, want)
	}
	if mk.Market.Line().Present() {
		t.Errorf("a futures market carries a line: %s", mk.Market.Line())
	}
	// Every runner is its own selection, and their identifiers must differ —
	// the role is the same on all of them, so the name is what separates them.
	if len(mk.Selections) != 2 {
		t.Fatalf("outright produced %d selections, want 2", len(mk.Selections))
	}
	if mk.Selections[0].ID() == mk.Selections[1].ID() {
		t.Errorf("two runners share the selection id %s; on a compacted topic they would overwrite "+
			"each other for ever", mk.Selections[0].ID())
	}

	probe := provider.Snapshot{
		Provider: provider.NameTheOddsAPI, FetchedAt: observed,
		Scope: provider.Scope{
			League:  league,
			Markets: []domain.MarketType{domain.MarketTypeFutures},
		},
		Events: []provider.EventSnapshot{snap},
	}
	if err := probe.Validate(); err != nil {
		t.Fatalf("outright snapshot does not validate: %v", err)
	}
}

// TestMarketLineTieBreakIsDeterministic.
func TestMarketLineTieBreakIsDeterministic(t *testing.T) {
	m, _ := testMapper(t)
	g := &marketGroup{
		key: marketKeyTotals, typ: domain.MarketTypeTotal,
		quotes: []quote{
			{role: domain.SelectionRoleOver, point: 47.5, hasPoint: true},
			{role: domain.SelectionRoleUnder, point: 44.5, hasPoint: true},
		},
	}
	for i := 0; i < 64; i++ {
		line, ok := m.marketLine(g)
		if !ok {
			t.Fatalf("marketLine refused a group with points")
		}
		if v, _ := line.Value(); v != 44.5 {
			t.Fatalf("tie broke to %v on iteration %d; the smaller line is the documented tie-break "+
				"and a line that moves between decodes of one payload reads downstream as a line "+
				"move that never happened", v, i)
		}
	}
}

// TestMarketLineCountsAwaySpreadsTowardsTheHomeLine.
//
// The Odds API states a spread as +6.5 on one side and -6.5 on the other. If the
// away quote were counted at face value the two sides of ONE agreed line would
// be tallied as two rival lines, and half the book's prices would be dropped as
// a disagreement with itself.
func TestMarketLineCountsAwaySpreadsTowardsTheHomeLine(t *testing.T) {
	m, _ := testMapper(t)
	g := &marketGroup{
		key: marketKeySpreads, typ: domain.MarketTypeSpread,
		quotes: []quote{
			{role: domain.SelectionRoleHome, point: -6.5, hasPoint: true},
			{role: domain.SelectionRoleAway, point: 6.5, hasPoint: true},
		},
	}
	line, ok := m.marketLine(g)
	if !ok {
		t.Fatalf("marketLine refused a symmetric spread")
	}
	if v, _ := line.Value(); v != -6.5 {
		t.Errorf("line = %v, want -6.5 (the home perspective)", v)
	}

	// A pick'em must not split its own vote on the sign of zero.
	if got := homePerspective(domain.MarketTypeSpread, domain.SelectionRoleAway, 0); got != 0 {
		t.Errorf("an away pick'em inverted to %v; -0.0 is a different map key from 0.0", got)
	}
	// Totals are absolute and shared by both sides, so they are never inverted.
	if got := homePerspective(domain.MarketTypeTotal, domain.SelectionRoleUnder, 47.5); got != 47.5 {
		t.Errorf("a total's under side was inverted to %v", got)
	}
}

// TestMarketLineOptionalRuleAcceptsAnAbsentLine.
func TestMarketLineOptionalRuleAcceptsAnAbsentLine(t *testing.T) {
	m, _ := testMapper(t)
	g := &marketGroup{
		key: "player_anytime_td", subject: "Ja'Marr Chase", typ: domain.MarketTypePlayerProp,
		quotes: []quote{{role: domain.SelectionRoleOutright, name: "Yes"}},
	}
	line, ok := m.marketLine(g)
	if !ok {
		t.Fatalf("marketLine refused a prop with no point; LineRuleOptional permits one")
	}
	if line.Present() {
		t.Errorf("an absent line became %s", line)
	}
}

// -----------------------------------------------------------------------------
// Identifier reversal
// -----------------------------------------------------------------------------

// TestIdentityHashShapeMatchesTheNormalizer.
//
// looksHashed hard-codes normalizer.identity's private hash encoding, because
// the constant is not exported. This test is what keeps the copy honest: it
// derives an identifier from a key the normalizer CANNOT embed and asserts the
// component that comes back has the shape looksHashed expects.
func TestIdentityHashShapeMatchesTheNormalizer(t *testing.T) {
	p := testProvider(t)
	// Long enough to blow the embedding budget, so identity.token hashes it.
	long := strings.Repeat("verylongleaguekey", 8)

	id, err := normalizer.LeagueIDFor(p, long)
	if err != nil {
		t.Fatalf("LeagueIDFor: %v", err)
	}
	component := strings.TrimPrefix(string(id), string(p)+".l.")
	if component == long {
		t.Fatalf("normalizer embedded a %d-byte key verbatim; the budget assumption in "+
			"trimIdentityPrefix no longer holds", len(long))
	}
	if !looksHashed(component) {
		t.Fatalf("normalizer's hashed component %q does not match looksHashed's expected shape "+
			"(h + %d hex); the two encodings have drifted and a hash would now round-trip as a "+
			"provider key", component, identityHashHexLen)
	}
}

// TestLeagueKeyReversalRefusesAHashedComponent.
//
// The round-trip check alone is not enough: re-deriving from a hash reproduces
// the identifier exactly, so the reversal would hand back the hash as though it
// were the provider's own sport key and the adapter would spend a credit asking
// for a sport that does not exist.
func TestLeagueKeyReversalRefusesAHashedComponent(t *testing.T) {
	p := testProvider(t)

	if got, ok := leagueKeyFromID(p, mustLeagueID(t, "americanfootball_nfl")); !ok || got != "americanfootball_nfl" {
		t.Errorf("a verbatim key did not reverse: got (%q, %v)", got, ok)
	}

	long := strings.Repeat("verylongleaguekey", 8)
	id, err := normalizer.LeagueIDFor(p, long)
	if err != nil {
		t.Fatalf("LeagueIDFor: %v", err)
	}
	if got, ok := leagueKeyFromID(p, id); ok {
		t.Errorf("a hashed component reversed to %q; a hash is not a sport key and the request would "+
			"cost a credit to learn that", got)
	}

	// A foreign identifier must not reverse at all.
	if _, ok := leagueKeyFromID(p, domain.LeagueID("some-other-provider.l.americanfootball_nfl")); ok {
		t.Errorf("an identifier from another provider reversed")
	}
	if _, ok := leagueKeyFromID(p, domain.LeagueID(string(p)+".l.")); ok {
		t.Errorf("an empty component reversed")
	}
}

func TestLooksHashed(t *testing.T) {
	cases := map[string]bool{
		"h0123456789ab":        true,
		"habcdef012345":        true,
		"h0123456789ab0":       false, // too long
		"h0123456789a":         false, // too short
		"x0123456789ab":        false, // wrong prefix
		"h0123456789aG":        false, // not lowercase hex
		"americanfootball_nfl": false,
		"":                     false,
		"hhhhhhhhhhhhh":        false, // 'h' is not a hex digit
	}
	for in, want := range cases {
		if got := looksHashed(in); got != want {
			t.Errorf("looksHashed(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestSplitPayloadPreservesExactBytesOrNothing.
//
// provider.RawPayload requires "the provider's bytes, unmodified". A misaligned
// split would attach one event's bytes to another event's snapshot, which is
// worse than attaching none — the raw topic is the artefact a golden file is
// recorded from and the only one that survives a parsing bug.
func TestSplitPayloadPreservesExactBytesOrNothing(t *testing.T) {
	array := json.RawMessage(`[{"id":"a"},  {"id":"b"}]`)
	object := json.RawMessage(`{"id":"a"}`)

	if got := splitPayload(array, 2); len(got) != 2 ||
		string(got[0]) != `{"id":"a"}` || string(got[1]) != `{"id":"b"}` {
		t.Errorf("array split = %q, want two exact elements", got)
	}
	if got := splitPayload(object, 1); len(got) != 1 || string(got[0]) != string(object) {
		t.Errorf("object split = %q, want the object itself", got)
	}
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want int
	}{
		{"count mismatch yields nothing rather than a misalignment", array, 3},
		{"an object where several events were decoded yields nothing", object, 2},
		{"garbage yields nothing", json.RawMessage(`[{"id":`), 1},
		{"empty yields nothing", json.RawMessage(``), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitPayload(tc.raw, tc.want); got != nil {
				t.Errorf("split = %q, want nil", got)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Wire-level parsing edges
// -----------------------------------------------------------------------------

// TestWireTimeAcceptsBothDocumentedShapes.
//
// The provider's `dateFormat` parameter changes the JSON TYPE of every
// timestamp. A decoder that handled only one would fail obscurely the moment the
// parameter changed.
func TestWireTimeAcceptsBothDocumentedShapes(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		want    time.Time
		wantErr bool
	}{
		{"iso", `"2021-06-10T13:33:18Z"`, time.Date(2021, 6, 10, 13, 33, 18, 0, time.UTC), false},
		{"iso with an offset is normalised to UTC", `"2021-06-10T09:33:18-04:00"`,
			time.Date(2021, 6, 10, 13, 33, 18, 0, time.UTC), false},
		{"unix", `1623331998`, time.Unix(1623331998, 0).UTC(), false},
		// null is a documented, normal state — /scores sends it for an event
		// that has not started — so it is absence, not an error.
		{"null is absence", `null`, time.Time{}, false},
		{"an empty string is absence", `""`, time.Time{}, false},
		{"a malformed string is an error", `"yesterday"`, time.Time{}, true},
		{"a boolean is an error", `true`, time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got wireTime
			err := json.Unmarshal([]byte(tc.json), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsed %s without error", tc.json)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal %s: %v", tc.json, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("= %s, want %s", got.Time, tc.want)
			}
		})
	}
}

// TestMarketObservedAtPrefersTheMarketLevelTimestamp is the provider's own
// recommendation: "To check recency of odds, we recommend using this field
// instead of the 'last_update' field at the bookmaker level."
func TestMarketObservedAtPrefersTheMarketLevelTimestamp(t *testing.T) {
	marketAt := wireTime{Time: time.Date(2023, 1, 1, 5, 31, 29, 0, time.UTC)}
	bookAt := wireTime{Time: time.Date(2023, 1, 1, 5, 0, 0, 0, time.UTC)}

	if got, ok := (Market{LastUpdate: &marketAt}).ObservedAt(Bookmaker{LastUpdate: &bookAt}); !ok ||
		!got.Equal(marketAt.Time) {
		t.Errorf("= (%s, %v), want the market-level instant %s", got, ok, marketAt.Time)
	}
	if got, ok := (Market{}).ObservedAt(Bookmaker{LastUpdate: &bookAt}); !ok || !got.Equal(bookAt.Time) {
		t.Errorf("= (%s, %v), want the bookmaker-level fallback %s", got, ok, bookAt.Time)
	}
	// Neither present must report ABSENCE, so the caller decides. Papering over
	// it with time.Now() would make the staleness SLO report perfect freshness
	// for data of unknown age.
	if _, ok := (Market{}).ObservedAt(Bookmaker{}); ok {
		t.Errorf("a market with no timestamp anywhere reported one")
	}
}

// TestInPlayIsTheProvidersDocumentedTest.
func TestInPlayIsTheProvidersDocumentedTest(t *testing.T) {
	start := wireTime{Time: time.Date(2026, 9, 10, 0, 20, 0, 0, time.UTC)}
	ev := EventOdds{CommenceTime: start}
	if ev.InPlay(start.Add(-time.Second)) {
		t.Errorf("an event before its commence_time reported in-play")
	}
	if !ev.InPlay(start.Add(time.Second)) {
		t.Errorf("an event after its commence_time reported not in-play")
	}
	if (EventOdds{}).InPlay(start.Time) {
		t.Errorf("an event with no commence_time reported in-play")
	}
}

// TestParseRetryAfterReadsBothDocumentedForms.
func TestParseRetryAfterReadsBothDocumentedForms(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"7", 7 * time.Second},
		{" 7 ", 7 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"", 0},
		{"soon", 0},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		// A date already in the past means "now", not a negative wait.
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := parseRetryAfter(tc.raw, now); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseQuotaHeaderToleratesADecimal.
//
// The provider declares these as integers but has been observed sending
// decimals on fractional plans. Discarding a "5000.0" entirely would fall the
// gauge back to the local estimate for no reason.
func TestParseQuotaHeaderToleratesADecimal(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{"99000", 99_000, true},
		{"5000.0", 5_000, true},
		{"-1", -1, true},
		{"", 0, false},
		{"lots", 0, false},
		{"NaN", 0, false},
		{"Inf", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			h := http.Header{}
			if tc.raw != "" {
				h.Set(HeaderRequestsRemaining, tc.raw)
			}
			got, ok := parseQuotaHeader(h, HeaderRequestsRemaining)
			if got != tc.want || ok != tc.ok {
				t.Errorf("= (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestLimiterReconcilesDownwardsOnly.
//
// If the provider says fewer credits remain than the bucket is willing to spend,
// the bucket is clamped — we must not spend credits the subscription does not
// have. If it says MORE remain, the bucket is left alone: the bucket is not
// tracking the balance, it is PACING spend across the month, and raising it to
// match would defeat the pacing entirely on the 1st.
func TestLimiterReconcilesDownwardsOnly(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(LimiterConfig{
		MonthlyCredits: 100_000,
		CreditBurst:    3_400,
		Now:            func() time.Time { return now },
	})

	// http.Header keys are canonicalised on Set and on Get, so the header must
	// be built the way net/http builds it rather than as a raw map literal.
	generous := http.Header{}
	generous.Set(HeaderRequestsRemaining, "90000")

	before := l.Quota().CreditTokens
	l.ObserveHeaders(generous)
	if got := l.Quota().CreditTokens; got != before {
		t.Errorf("a generous provider reading raised the pacing bucket from %v to %v", before, got)
	}
	if got := l.Quota().Remaining; got != 90_000 {
		t.Errorf("Remaining = %d, want the provider's 90000", got)
	}
	if !l.Quota().FromProvider {
		t.Errorf("the quota is not marked as coming from the provider")
	}

	tight := http.Header{}
	tight.Set(HeaderRequestsRemaining, "12")
	l.ObserveHeaders(tight)
	if got := l.Quota().CreditTokens; got != 12 {
		t.Errorf("credit tokens = %v, want 12 — the bucket must not be willing to spend credits the "+
			"subscription does not have", got)
	}
}

// TestLimiterRefundsAnUnbilledRequest.
//
// guides/v4: "If no events are returned, the request will not count against the
// usage quota." Without a refund path an out-of-season league would drain the
// month polling an empty slate — the exact failure adaptive backoff exists to
// avoid, reintroduced by the limiter.
func TestLimiterRefundsAnUnbilledRequest(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(LimiterConfig{
		MonthlyCredits: 100_000,
		CreditBurst:    3_400,
		Now:            func() time.Time { return now },
	})

	if err := l.Reserve(3); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got := l.Quota().LocalEstimate; got != 99_997 {
		t.Fatalf("local estimate after reserving 3 = %d, want 99997", got)
	}
	l.Refund(3)
	if got := l.Quota().LocalEstimate; got != 100_000 {
		t.Errorf("local estimate after a full refund = %d, want 100000", got)
	}
	if got := l.Quota().CreditTokens; got != 3_400 {
		t.Errorf("credit tokens after a full refund = %v, want the full burst 3400", got)
	}
	// A refund larger than the spend must not manufacture credits.
	l.Refund(1_000_000)
	if got := l.Quota().CreditTokens; got != 3_400 {
		t.Errorf("an over-refund raised the bucket above its capacity: %v", got)
	}
}

// TestLimiterRefusesARequestLargerThanItsBucket.
//
// A cost above capacity is unsatisfiable at ANY delay, which is a configuration
// error rather than a wait — so RetryAfter is zero and the error says the
// budget can never satisfy it, instead of telling the scheduler to come back in
// a moment for ever.
func TestLimiterRefusesARequestLargerThanItsBucket(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(LimiterConfig{
		MonthlyCredits: 100,
		CreditBurst:    2,
		Now:            func() time.Time { return now },
	})
	err := l.Reserve(5)
	if err == nil {
		t.Fatalf("a 5-credit request was reserved from a 2-credit bucket")
	}
	var budget *BudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("error is not a *BudgetError: %v", err)
	}
	if budget.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s, want 0 — no amount of waiting makes this satisfiable", budget.RetryAfter)
	}
	if !strings.Contains(err.Error(), "cannot ever satisfy") {
		t.Errorf("the error does not say the request is unsatisfiable: %v", err)
	}
}
