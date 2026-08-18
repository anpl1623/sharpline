package normalizer

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

const testProvider = kafka.Provider("the-odds-api")

var (
	testStart    = time.Date(2026, 8, 17, 20, 15, 0, 0, time.UTC)
	testObserved = time.Date(2026, 8, 17, 18, 30, 0, 0, time.UTC)
)

func point(v float64) *float64 { return &v }

func mapperFor(t *testing.T) *Mapper {
	t.Helper()
	m, err := NewMapper(MapperOptions{Provider: testProvider})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// baseRaw is one well-formed event carrying the three featured markets from one
// book. Every reject case below is this value with exactly one thing wrong,
// which is what keeps a case's name an accurate description of what it tests.
func baseRaw() RawEvent {
	return RawEvent{
		ID:           "evt-1",
		SportKey:     "americanfootball",
		SportName:    "American Football",
		LeagueKey:    "americanfootball_nfl",
		LeagueName:   "NFL",
		HomeTeam:     "Kansas City Chiefs",
		AwayTeam:     "Detroit Lions",
		CommenceTime: testStart,
		Books: []RawBook{{
			Key:        "draftkings",
			Name:       "DraftKings",
			LastUpdate: testObserved,
			Markets: []RawMarket{
				{Key: MarketKeyH2H, LastUpdate: testObserved, Outcomes: []RawOutcome{
					{Name: "Kansas City Chiefs", Price: 1.66},
					{Name: "Detroit Lions", Price: 2.30},
				}},
				{Key: MarketKeySpreads, LastUpdate: testObserved, Outcomes: []RawOutcome{
					{Name: "Kansas City Chiefs", Price: 1.91, Point: point(-3.5)},
					{Name: "Detroit Lions", Price: 1.91, Point: point(3.5)},
				}},
				{Key: MarketKeyTotals, LastUpdate: testObserved, Outcomes: []RawOutcome{
					{Name: "Over", Price: 1.87, Point: point(48.5)},
					{Name: "Under", Price: 1.95, Point: point(48.5)},
				}},
			},
		}},
	}
}

// TestMapProducesOneViewPerMarket is the happy path, and it pins the two
// conventions that are easiest to get plausibly wrong.
func TestMapProducesOneViewPerMarket(t *testing.T) {
	res, err := mapperFor(t).Map(baseRaw())
	if err != nil {
		t.Fatalf("Map: %v (rejects: %v)", err, res.Rejects)
	}
	if len(res.Rejects) != 0 {
		t.Fatalf("unexpected rejects: %v", res.Rejects)
	}
	if got, want := len(res.Views), 3; got != want {
		t.Fatalf("views = %d, want %d", got, want)
	}

	byType := map[domain.MarketType]MarketView{}
	for _, v := range res.Views {
		byType[v.Market.Type()] = v
	}

	ml, ok := byType[domain.MarketTypeMoneyline]
	if !ok {
		t.Fatal("no moneyline view")
	}
	if ml.Market.Line().Present() {
		t.Errorf("moneyline carries a line: %s", ml.Market.Line())
	}
	for _, p := range ml.Prices {
		if p.Line().Present() {
			t.Errorf("a moneyline quote carries a line: %s", p.Line())
		}
	}

	spread, ok := byType[domain.MarketTypeSpread]
	if !ok {
		t.Fatal("no spread view")
	}
	// The MARKET line is stated from the home side. The PRICE line is stated
	// from the selection's own side, exactly as the provider sent it — raw.go:
	// "No inversion is applied to it anywhere."
	if v, _ := spread.Market.Line().Value(); v != -3.5 {
		t.Errorf("spread market line = %v, want -3.5 (home perspective)", v)
	}
	for _, p := range spread.Prices {
		var role domain.SelectionRole
		for _, s := range spread.Selections {
			if s.ID() == p.SelectionID() {
				role = s.Role()
			}
		}
		v, present := p.Line().Value()
		if !present {
			t.Fatalf("spread quote for %s carries no line", role)
		}
		want := -3.5
		if role == domain.SelectionRoleAway {
			want = 3.5
		}
		if v != want {
			t.Errorf("%s quote line = %v, want %v", role, v, want)
		}
	}

	total, ok := byType[domain.MarketTypeTotal]
	if !ok {
		t.Fatal("no total view")
	}
	// A total is absolute: over and under share the identical threshold and
	// Invert is never applied.
	for _, p := range total.Prices {
		if v, _ := p.Line().Value(); v != 48.5 {
			t.Errorf("total quote line = %v, want 48.5 on both sides", v)
		}
	}

	// The event is derived once and is identical on every market.
	for _, v := range res.Views {
		if v.Event.ID() != ml.Event.ID() {
			t.Errorf("event identifier differs across markets: %s vs %s", v.Event.ID(), ml.Event.ID())
		}
		if v.Event.Name() != "Detroit Lions at Kansas City Chiefs" {
			t.Errorf("event name = %q, want the Away at Home form", v.Event.Name())
		}
		if v.Market.EventID() != v.Event.ID() {
			t.Errorf("market %s claims event %s", v.Market.ID(), v.Market.EventID())
		}
		// Every market's UpdatedAt is its own observation instant, which is what
		// makes the record round-trip: payload.go rebuilds the event from it.
		if !v.Market.UpdatedAt().Equal(testObserved) || !v.Event.UpdatedAt().Equal(testObserved) {
			t.Errorf("market/event UpdatedAt = %s/%s, want %s",
				v.Market.UpdatedAt(), v.Event.UpdatedAt(), testObserved)
		}
	}
}

// TestMapReadsTheClockFromThePayloadNotFromNow pins the purity claim.
//
// An event is live when its advertised start is before the NEWEST observation
// instant in its own payload, never before time.Now. That is what makes a replay
// of odds.raw.* six months from now reproduce the records it produced the first
// time.
func TestMapReadsTheClockFromThePayloadNotFromNow(t *testing.T) {
	m := mapperFor(t)

	scheduled, err := m.Map(baseRaw())
	if err != nil {
		t.Fatal(err)
	}
	if got := scheduled.Views[0].Event.Status(); got != domain.EventStatusScheduled {
		t.Errorf("status = %s, want scheduled (start is after the observation)", got)
	}

	live := baseRaw()
	live.CommenceTime = testObserved.Add(-time.Hour)
	res, err := m.Map(live)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Views[0].Event.Status(); got != domain.EventStatusLive {
		t.Errorf("status = %s, want live (start is before the observation)", got)
	}

	// The whole payload is 30 years old. A mapper that read a clock would call
	// this live-and-long-over; a pure one still answers from the payload.
	old := baseRaw()
	old.CommenceTime = testStart.AddDate(-30, 0, 0)
	old.Books[0].LastUpdate = testObserved.AddDate(-30, 0, 0)
	for i := range old.Books[0].Markets {
		old.Books[0].Markets[i].LastUpdate = testObserved.AddDate(-30, 0, 0)
	}
	replay, err := m.Map(old)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range replay.Views {
		if !v.Market.UpdatedAt().Equal(testObserved.AddDate(-30, 0, 0).UTC()) {
			t.Errorf("replayed market UpdatedAt = %s; a clock leaked into the mapping", v.Market.UpdatedAt())
		}
	}
}

// TestMapKeepsQuotesAtANonConsensusLine is the deliberate divergence from the
// adapter-side mapper, and it is worth being explicit about.
//
// internal/ingest/provider/theoddsapi DROPS a quote at a line other than the
// modal one, because provider.Snapshot.Validate enforces one line per market.
// Nothing enforces that here and nothing should: payload.go's Domain()
// deliberately declines ValidatePriceForSelection because "a book quoting -3
// against a -3.5 consensus is normal market disagreement rather than
// corruption", and multi-book comparison (CLAUDE.md §6) exists precisely to show
// that disagreement.
func TestMapKeepsQuotesAtANonConsensusLine(t *testing.T) {
	raw := baseRaw()
	raw.Books = []RawBook{
		spreadBook("draftkings", -3.5, 3.5),
		spreadBook("fanduel", -3.5, 3.5),
		spreadBook("betmgm", -3.5, 3.5),
		spreadBook("caesars", -3, 3),
	}

	res, err := mapperFor(t).Map(raw)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(res.Views) != 1 {
		t.Fatalf("views = %d, want 1", len(res.Views))
	}
	v := res.Views[0]

	if got, _ := v.Market.Line().Value(); got != -3.5 {
		t.Errorf("market line = %v, want the modal -3.5", got)
	}
	if got, want := len(v.Prices), 8; got != want {
		t.Fatalf("prices = %d, want %d — the odd book's quotes were dropped", got, want)
	}
	odd := 0
	for _, p := range v.Prices {
		if val, _ := p.Line().Value(); val == -3 || val == 3 {
			odd++
		}
	}
	if odd != 2 {
		t.Errorf("quotes at the non-consensus line = %d, want 2", odd)
	}
	if got, want := len(v.Books), 4; got != want {
		t.Errorf("books = %d, want %d", got, want)
	}
}

func spreadBook(key string, home, away float64) RawBook {
	return RawBook{
		Key: key, Name: strings.ToUpper(key[:1]) + key[1:], LastUpdate: testObserved,
		Markets: []RawMarket{{Key: MarketKeySpreads, LastUpdate: testObserved, Outcomes: []RawOutcome{
			{Name: "Kansas City Chiefs", Price: 1.91, Point: point(home)},
			{Name: "Detroit Lions", Price: 1.91, Point: point(away)},
		}}},
	}
}

// TestMapIsDeterministicAcrossRuns is what suppression depends on: the same
// payload must produce the same record, or every poll looks like a move. The
// modal-line vote and the collection ordering are the two places a map's
// randomised iteration order could leak in.
func TestMapIsDeterministicAcrossRuns(t *testing.T) {
	raw := baseRaw()
	raw.Books = append(raw.Books, spreadBook("fanduel", -3.5, 3.5), spreadBook("betmgm", -3, 3))
	m := mapperFor(t)

	first, err := m.Map(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := recordsOf(t, first)
	for i := 0; i < 24; i++ {
		res, err := m.Map(raw)
		if err != nil {
			t.Fatal(err)
		}
		got := recordsOf(t, res)
		if len(got) != len(want) {
			t.Fatalf("run %d produced %d records, want %d", i, len(got), len(want))
		}
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("run %d record %d differs:\n got %s\nwant %s", i, j, got[j], want[j])
			}
		}
	}
}

// recordsOf renders a map result as the exact JSON that would go on the bus, so
// a difference in field ORDER or in a float's representation is caught, not just
// a difference in the hash.
func recordsOf(t *testing.T, res MapResult) []string {
	t.Helper()
	out := make([]string, 0, len(res.Views))
	for _, v := range res.Views {
		rec := newRecord(testProvider.String(), v, testObserved)
		rec.Fingerprint = rec.Hash().String()
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// TestRecordRoundTripsThroughTheWire is the property every consumer of
// odds.normalized depends on: a record that has been on a compacted topic for a
// month must rebuild into the same validated domain values it was derived from.
func TestRecordRoundTripsThroughTheWire(t *testing.T) {
	res, err := mapperFor(t).Map(baseRaw())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range res.Views {
		rec := newRecord(testProvider.String(), v, testObserved)
		rec.Fingerprint = rec.Hash().String()

		wire, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		var back NormalizedMarket
		if err := json.Unmarshal(wire, &back); err != nil {
			t.Fatal(err)
		}
		if back.Hash() != rec.Hash() {
			t.Fatalf("market %s: fingerprint changed across the wire: %s -> %s",
				rec.Market.ID, rec.Hash(), back.Hash())
		}

		view, err := back.Domain()
		if err != nil {
			t.Fatalf("market %s: Domain(): %v", rec.Market.ID, err)
		}
		again := newRecord(testProvider.String(), view, rec.IngestedAt)
		again.Fingerprint = again.Hash().String()
		reWire, err := json.Marshal(again)
		if err != nil {
			t.Fatal(err)
		}
		if string(reWire) != string(wire) {
			t.Fatalf("market %s did not round trip:\n got %s\nwant %s", rec.Market.ID, reWire, wire)
		}
	}
}

// TestMapRejectsAreTableDriven covers every rejection reason the mapper can
// produce. CLAUDE.md's phase brief requires it: a payload that cannot be
// normalised "must be REJECTED AND COUNTED with a reason, never coerced into
// something the domain happens to accept".
func TestMapRejectsAreTableDriven(t *testing.T) {
	long := strings.Repeat("a", domain.MaxNameLen+40)
	nan := func() *float64 { v := 0.0; return point(v / v) }

	cases := []struct {
		name string
		// fatal marks a reason that kills the whole event, so Map returns the
		// Reject as an error rather than listing it.
		fatal  bool
		scope  Scope
		reason Reason
		mutate func(*RawEvent)
	}{{
		name: "no provider event id", fatal: true, scope: ScopeEvent, reason: ReasonMissingEventID,
		mutate: func(r *RawEvent) { r.ID = "  " },
	}, {
		name: "no league key", fatal: true, scope: ScopeEvent, reason: ReasonMissingLeague,
		mutate: func(r *RawEvent) { r.LeagueKey = "" },
	}, {
		name: "one competitor", fatal: true, scope: ScopeEvent, reason: ReasonMissingCompetitor,
		mutate: func(r *RawEvent) { r.AwayTeam = "" },
	}, {
		name: "no commence time", fatal: true, scope: ScopeEvent, reason: ReasonMissingStartTime,
		mutate: func(r *RawEvent) { r.CommenceTime = time.Time{} },
	}, {
		name: "no observation instant anywhere", fatal: true, scope: ScopeEvent, reason: ReasonNoObservationTime,
		mutate: func(r *RawEvent) {
			r.Books[0].LastUpdate = time.Time{}
			for i := range r.Books[0].Markets {
				r.Books[0].Markets[i].LastUpdate = time.Time{}
			}
		},
	}, {
		name: "the domain refuses the event", fatal: true, scope: ScopeEvent, reason: ReasonInvalidEvent,
		mutate: func(r *RawEvent) { r.Name = long },
	}, {
		name: "one market carries no observation instant", scope: ScopeMarket, reason: ReasonNoObservationTime,
		mutate: func(r *RawEvent) {
			// The bookmaker-level fallback is present for the others, so only this
			// market loses its instant.
			r.Books[0].LastUpdate = time.Time{}
			r.Books[0].Markets[0].LastUpdate = time.Time{}
		},
	}, {
		name: "a market key this build does not map", scope: ScopeMarket, reason: ReasonUnsupportedMarket,
		mutate: func(r *RawEvent) { r.Books[0].Markets[0].Key = "btts" },
	}, {
		name: "a player prop with no subject", scope: ScopeSelection, reason: ReasonMissingSubject,
		mutate: func(r *RawEvent) {
			r.Books[0].Markets[0] = RawMarket{
				Key: "player_pass_tds", LastUpdate: testObserved,
				Outcomes: []RawOutcome{{Name: "Over", Price: 1.9, Point: point(1.5)}},
			}
		},
	}, {
		name: "a spread with no point on any quote", scope: ScopeMarket, reason: ReasonMissingLine,
		mutate: func(r *RawEvent) {
			for i := range r.Books[0].Markets[1].Outcomes {
				r.Books[0].Markets[1].Outcomes[i].Point = nil
			}
		},
	}, {
		name: "one quote on a spread with no point", scope: ScopePrice, reason: ReasonMissingLine,
		mutate: func(r *RawEvent) { r.Books[0].Markets[1].Outcomes[1].Point = nil },
	}, {
		name: "one quote at a non-finite point", scope: ScopePrice, reason: ReasonInvalidLine,
		mutate: func(r *RawEvent) { r.Books[0].Markets[1].Outcomes[1].Point = nan() },
	}, {
		name: "the domain refuses the market", scope: ScopeMarket, reason: ReasonInvalidMarket,
		mutate: func(r *RawEvent) {
			// A total is a threshold on combined scoring, so a non-positive one is
			// a parse error rather than a tradeable market.
			for i := range r.Books[0].Markets[2].Outcomes {
				r.Books[0].Markets[2].Outcomes[i].Point = point(0)
			}
		},
	}, {
		name: "an outcome that names no known side", scope: ScopeSelection, reason: ReasonUnknownRole,
		mutate: func(r *RawEvent) { r.Books[0].Markets[0].Outcomes[0].Name = "Nobody" },
	}, {
		name: "the same book quotes one selection twice", scope: ScopePrice, reason: ReasonDuplicateSelection,
		mutate: func(r *RawEvent) {
			r.Books[0].Markets[2].Outcomes = append(r.Books[0].Markets[2].Outcomes,
				RawOutcome{Name: "Over", Price: 1.80, Point: point(48.5)})
		},
	}, {
		name: "the domain refuses the selection", scope: ScopeSelection, reason: ReasonInvalidSelection,
		mutate: func(r *RawEvent) {
			r.Books[0].Markets[0] = RawMarket{
				Key: MarketKeyOutrights, LastUpdate: testObserved,
				Outcomes: []RawOutcome{{Name: long, Price: 6.5}},
			}
		},
	}, {
		name: "odds outside the legal band", scope: ScopePrice, reason: ReasonInvalidOdds,
		mutate: func(r *RawEvent) { r.Books[0].Markets[0].Outcomes[0].Price = 1.0 },
	}, {
		name: "a bookmaker with no key", scope: ScopePrice, reason: ReasonInvalidBook,
		mutate: func(r *RawEvent) {
			nameless := spreadBook("draftkings", -3.5, 3.5)
			nameless.Key = "  "
			r.Books = append(r.Books, nameless)
		},
	}, {
		name: "a market whose every quote was rejected", scope: ScopeMarket, reason: ReasonNoPrices,
		mutate: func(r *RawEvent) {
			for i := range r.Books[0].Markets[0].Outcomes {
				r.Books[0].Markets[0].Outcomes[i].Price = 1.0
			}
		},
	}}

	m := mapperFor(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := baseRaw()
			tc.mutate(&raw)

			res, err := m.Map(raw)
			if tc.fatal {
				if err == nil {
					t.Fatalf("Map succeeded; want a %s/%s rejection", tc.scope, tc.reason)
				}
				var r Reject
				if !errors.As(err, &r) {
					t.Fatalf("Map returned %v, which is not a Reject", err)
				}
				if r.Scope != tc.scope || r.Reason != tc.reason {
					t.Fatalf("rejected as %s/%s, want %s/%s", r.Scope, r.Reason, tc.scope, tc.reason)
				}
				if len(res.Views) != 0 {
					t.Fatalf("a fatal rejection still produced %d views", len(res.Views))
				}
				return
			}

			if err != nil {
				t.Fatalf("Map: %v", err)
			}
			if !hasReject(res.Rejects, tc.scope, tc.reason) {
				t.Fatalf("no %s/%s rejection; got %v", tc.scope, tc.reason, res.Rejects)
			}
			// The narrowest loss possible: one bad market or quote must never cost
			// the whole event.
			if len(res.Views) == 0 {
				t.Fatalf("a %s-scoped rejection cost the entire event", tc.scope)
			}
		})
	}

	t.Run("every reason is covered", func(t *testing.T) {
		covered := map[Reason]string{}
		for _, tc := range cases {
			covered[tc.reason] = tc.name
		}
		// Reasons this package produces outside the mapper. Each names the test
		// that exercises it, so an unexercised reason cannot hide behind this map.
		for reason, where := range map[Reason]string{
			ReasonDecode:             "TestHandleMessageRejectsAnUndecodablePayload",
			ReasonUnsupportedMessage: "TestHandleMessageSkipsAnUnknownEnvelope",
			ReasonInvalidIdentifier:  "TestWarmStartCountsAnUnusableKey",
			ReasonInvalidPrice:       "TestPriceRejectReasonClassifiesOnSentinels",
		} {
			covered[reason] = where
		}
		for _, r := range Reasons() {
			if _, ok := covered[r]; !ok {
				t.Errorf("reason %q is declared and never exercised; either test it or delete it", r)
			}
		}
		if got, want := len(covered), len(Reasons()); got != want {
			t.Errorf("covered %d reasons, Reasons() declares %d — a case names a reason that is not declared",
				got, want)
		}
	})
}

func hasReject(rs []Reject, scope Scope, reason Reason) bool {
	for _, r := range rs {
		if r.Scope == scope && r.Reason == reason {
			return true
		}
	}
	return false
}

// TestPriceRejectReasonClassifiesOnSentinels pins the split between "the
// provider sent an unusable number" and "something else about the price was
// wrong". They are different operational problems and the counter that reports
// provider data quality must not absorb our own bugs.
func TestPriceRejectReasonClassifiesOnSentinels(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want Reason
	}{
		{"not finite", domain.ErrOddsNotFinite, ReasonInvalidOdds},
		{"out of range", domain.ErrOddsOutOfRange, ReasonInvalidOdds},
		{"wrapped", errors.Join(errors.New("price: "), domain.ErrOddsOutOfRange), ReasonInvalidOdds},
		{"zero observation instant", domain.ErrZeroTime, ReasonInvalidPrice},
		{"anything else", errors.New("boom"), ReasonInvalidPrice},
	} {
		if got := priceRejectReason(tc.err); got != tc.want {
			t.Errorf("%s: priceRejectReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestMapDerivesTheSportFromTheLeagueWhenAbsent pins the documented fallback.
func TestMapDerivesTheSportFromTheLeagueWhenAbsent(t *testing.T) {
	raw := baseRaw()
	raw.SportKey = ""
	raw.SportName = ""

	res, err := mapperFor(t).Map(raw)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Views[0]
	if got, want := v.Sport.Slug().String(), "americanfootball"; got != want {
		t.Errorf("sport slug = %q, want %q", got, want)
	}
	if !v.League.BelongsTo(v.Sport) {
		t.Errorf("league %s does not belong to sport %s", v.League.ID(), v.Sport.ID())
	}
	// The provider's own key, verbatim. Never a prettified guess.
	if got, want := v.Sport.Name(), "americanfootball"; got != want {
		t.Errorf("sport name = %q, want the provider's own key %q", got, want)
	}
}

// TestBookKindComesFromTheProvider is ADR 0003's requirement made mechanical:
// the synthetic fallback "must not silently substitute for real data in a
// running deployment — that would be indistinguishable from fabricating market
// data", and BookRef.Kind is how a consumer can tell.
func TestBookKindComesFromTheProvider(t *testing.T) {
	real, err := NewMapper(MapperOptions{Provider: "the-odds-api"})
	if err != nil {
		t.Fatal(err)
	}
	fake, err := NewMapper(MapperOptions{Provider: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		m    *Mapper
		want domain.BookKind
	}{{real, domain.BookKindExternal}, {fake, domain.BookKindSynthetic}} {
		res, err := tc.m.Map(baseRaw())
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range res.Views[0].Books {
			if b.Kind() != tc.want {
				t.Errorf("provider %s: book kind = %s, want %s", tc.m.Provider(), b.Kind(), tc.want)
			}
		}
	}
}

// TestNewMapperRejectsAProviderThatCannotBeAnIdentifier keeps the failure at
// construction, per CLAUDE.md §12's "fail fast and loudly on a bad config".
func TestNewMapperRejectsAProviderThatCannotBeAnIdentifier(t *testing.T) {
	for _, name := range []string{"", "UPPER", strings.Repeat("a", MaxProviderLen+1)} {
		if _, err := NewMapper(MapperOptions{Provider: kafka.Provider(name)}); err == nil {
			t.Errorf("provider %q was accepted", name)
		} else if !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("provider %q: error %v does not wrap ErrInvalidOptions", name, err)
		}
	}
}
