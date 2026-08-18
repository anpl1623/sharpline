package synthetic

import (
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
)

// TestRawPayloadDecodesWithTheNeutralDecoder is the wire contract normalizer/
// raw.go asserts from the other side: "the synthetic adapter ... marshals this
// type directly ... and NeutralDecoder is a plain unmarshal".
//
// It is worth a real test rather than trusting the shared type, because the
// coupling is through JSON TAGS and a strict unmarshal — a field renamed on
// either side compiles cleanly here and fails at run time as an empty board.
func TestRawPayloadDecodesWithTheNeutralDecoder(t *testing.T) {
	dec, err := normalizer.NewNeutralDecoder(neutralProviderSlug)
	if err != nil {
		t.Fatalf("NewNeutralDecoder: %v", err)
	}
	if dec.Provider() != neutralProviderSlug {
		t.Fatalf("decoder provider = %q, want %q", dec.Provider(), neutralProviderSlug)
	}

	events, quoted := 0, 0
	for _, l := range leagues() {
		snap := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))
		for _, ev := range snap.Events {
			events++
			if ev.Raw.IsZero() {
				t.Fatalf("event %s carries no raw payload", ev.Event.ID())
			}
			if ev.Raw.ContentType != rawContentType {
				t.Fatalf("event %s content type = %q", ev.Event.ID(), ev.Raw.ContentType)
			}
			raw, err := dec.Decode(ev.Raw.Body)
			if err != nil {
				t.Fatalf("event %s: %v\n%s", ev.Event.ID(), err, ev.Raw.Body)
			}
			if raw.ID != string(ev.Event.ID()) {
				t.Errorf("raw id %q != parsed event id %q", raw.ID, ev.Event.ID())
			}
			if raw.LeagueKey != string(l.leagueID()) {
				t.Errorf("raw league key %q != %q", raw.LeagueKey, l.leagueID())
			}
			if !raw.CommenceTime.Equal(ev.Event.ScheduledStart()) {
				t.Errorf("raw commence time %s != %s", raw.CommenceTime, ev.Event.ScheduledStart())
			}
			switch ev.Event.Kind() {
			case domain.EventKindMatch:
				if raw.HomeTeam == "" || raw.AwayTeam == "" {
					t.Errorf("match %s has an empty side in the raw payload", raw.ID)
				}
			case domain.EventKindOutright:
				if raw.HomeTeam != "" || raw.AwayTeam != "" {
					t.Errorf("outright %s carries competitors in the raw payload", raw.ID)
				}
				if raw.Name == "" {
					t.Errorf("outright %s carries no name; there are no competitors to derive one from", raw.ID)
				}
			}

			// Every priced (market, book) pair in the parsed form must appear in
			// the bytes, and nothing else may. The raw topic is the replayable
			// record of what the provider said; a payload that disagreed with
			// the parsed snapshot would make it evidence of nothing.
			wantOutcomes := 0
			for _, m := range ev.Markets {
				wantOutcomes += len(m.Prices)
			}
			gotOutcomes := 0
			for _, b := range raw.Books {
				for _, m := range b.Markets {
					gotOutcomes += len(m.Outcomes)
					if !knownMarketKey(m.Key) {
						t.Errorf("event %s: market key %q is outside the vocabulary the normalizer maps", raw.ID, m.Key)
					}
					for _, o := range m.Outcomes {
						if o.Price <= 1 {
							t.Errorf("event %s: raw outcome %q priced at %v", raw.ID, o.Name, o.Price)
						}
					}
				}
			}
			if gotOutcomes != wantOutcomes {
				t.Errorf("event %s: raw payload carries %d outcomes, parsed snapshot carries %d prices",
					raw.ID, gotOutcomes, wantOutcomes)
			}
			quoted += gotOutcomes
		}
	}
	if events == 0 || quoted == 0 {
		t.Fatalf("nothing to check: %d events, %d outcomes", events, quoted)
	}
	t.Logf("%d events, %d raw outcomes", events, quoted)
}

// TestRawPointCarriesTheSelectionPerspective checks the convention
// normalizer.RawOutcome fixes: the point is stated "from THIS OUTCOME's own
// perspective", already inverted for an away spread, and no inversion is
// applied to it anywhere downstream. Getting this wrong produces a plausible
// wrong number rather than an error.
func TestRawPointCarriesTheSelectionPerspective(t *testing.T) {
	dec, err := normalizer.NewNeutralDecoder(neutralProviderSlug)
	if err != nil {
		t.Fatalf("NewNeutralDecoder: %v", err)
	}
	checked := 0
	for _, l := range leagues() {
		snap := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))
		for _, ev := range snap.Events {
			raw, err := dec.Decode(ev.Raw.Body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, m := range ev.Markets {
				if m.Market.Type() != domain.MarketTypeSpread || len(m.Prices) == 0 {
					continue
				}
				home, ok := m.Market.Line().Value()
				if !ok {
					t.Fatalf("spread %s has no line", m.Market.ID())
				}
				for _, b := range raw.Books {
					for _, rm := range b.Markets {
						if rm.Key != rawKeySpread {
							continue
						}
						if len(rm.Outcomes) != 2 {
							t.Fatalf("spread has %d outcomes", len(rm.Outcomes))
						}
						if rm.Outcomes[0].Point == nil || rm.Outcomes[1].Point == nil {
							t.Fatalf("spread outcome carries no point")
						}
						if *rm.Outcomes[0].Point != home {
							t.Errorf("home outcome point %v != market line %v", *rm.Outcomes[0].Point, home)
						}
						if *rm.Outcomes[1].Point != -home && !(home == 0 && *rm.Outcomes[1].Point == 0) {
							t.Errorf("away outcome point %v is not the inverse of %v", *rm.Outcomes[1].Point, home)
						}
						checked++
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no spread markets to check")
	}
}

// TestMoneylineAndFuturesCarryNoPoint checks the other side of the line rule:
// domain.MarketType.LineRule forbids a line on a moneyline and on a futures
// market, and an absent point is a different fact from a zero one.
func TestMoneylineAndFuturesCarryNoPoint(t *testing.T) {
	dec, err := normalizer.NewNeutralDecoder(neutralProviderSlug)
	if err != nil {
		t.Fatalf("NewNeutralDecoder: %v", err)
	}
	for _, l := range leagues() {
		snap := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))
		for _, ev := range snap.Events {
			raw, err := dec.Decode(ev.Raw.Body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, b := range raw.Books {
				for _, rm := range b.Markets {
					if rm.Key != rawKeyMoneyline && rm.Key != rawKeyOutright {
						continue
					}
					for _, o := range rm.Outcomes {
						if o.Point != nil {
							t.Errorf("%s outcome %q carries point %v", rm.Key, o.Name, *o.Point)
						}
					}
				}
			}
		}
	}
}

// TestPlayerPropKeysAreInTheProviderFamily checks that prop market keys match
// normalizer.MarketKeyPlayerPrefix, which the normalizer matches by prefix, and
// that the subject travels in Description where RawOutcome expects it.
func TestPlayerPropKeysAreInTheProviderFamily(t *testing.T) {
	dec, err := normalizer.NewNeutralDecoder(neutralProviderSlug)
	if err != nil {
		t.Fatalf("NewNeutralDecoder: %v", err)
	}
	props := 0
	for _, l := range leagues() {
		snap := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))
		for _, ev := range snap.Events {
			raw, err := dec.Decode(ev.Raw.Body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, b := range raw.Books {
				for _, rm := range b.Markets {
					if !strings.HasPrefix(rm.Key, normalizer.MarketKeyPlayerPrefix) {
						continue
					}
					props++
					for _, o := range rm.Outcomes {
						if o.Description == "" {
							t.Errorf("prop market %q has an outcome with no subject", rm.Key)
						}
						if o.Name != overSelectionName && o.Name != underSelectionName {
							t.Errorf("prop outcome name %q, want Over or Under", o.Name)
						}
					}
				}
			}
		}
	}
	if props == 0 {
		t.Fatal("no player-prop markets anywhere in the slate")
	}
	t.Logf("%d raw player-prop markets", props)
}

func TestPlayerMarketKey(t *testing.T) {
	cases := map[string]string{
		"Points":        "player_points",
		"Passing Yards": "player_passing_yards",
		"Shots on Goal": "player_shots_on_goal",
	}
	for in, want := range cases {
		if got := playerMarketKey(in); got != want {
			t.Errorf("playerMarketKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// knownMarketKey reports whether the normalizer maps a market key. Anything else
// would be counted as an unsupported market and skipped, which for the
// synthetic feed means silently dropped.
func knownMarketKey(k string) bool {
	switch k {
	case normalizer.MarketKeyH2H, normalizer.MarketKeySpreads,
		normalizer.MarketKeyTotals, normalizer.MarketKeyOutrights:
		return true
	}
	return strings.HasPrefix(k, normalizer.MarketKeyPlayerPrefix)
}
