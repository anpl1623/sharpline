package theoddsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Golden-file tests, per CLAUDE.md §10: "Provider normalization uses golden
// files against recorded payloads."
//
// Every payload replayed here is one The Odds API published in its own
// documentation. What is under test is THIS package's reading of THEIR format,
// so a provider wire-format change — or a regression in the decoder — shows up
// as a failing assertion rather than as a subtly wrong price on the board.

func TestGoldenCatalogueDecodesPublishedSports(t *testing.T) {
	stub := newProviderStub(t)
	stub.route("/v4/sports/", json200(readGolden(t, goldenSports)))
	h := newHarness(t, stub, nil)

	cat, err := h.Adapter.Catalogue(context.Background())
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("catalogue built from the provider's own sample does not validate: %v", err)
	}

	// The provider's league key is what identifies a league; its title "can
	// change, for example if a league undergoes a name change", which is why
	// the identifier is derived from the key and the title is only the display
	// name.
	nfl, ok := cat.League(mustLeagueID(t, "americanfootball_nfl"))
	if !ok {
		t.Fatalf("catalogue has no americanfootball_nfl league")
	}
	if got, want := nfl.Name(), "NFL"; got != want {
		t.Errorf("league name = %q, want %q (the sample's `title`)", got, want)
	}
	if got, want := string(nfl.Slug()), "americanfootball_nfl"; got != want {
		t.Errorf("league slug = %q, want %q", got, want)
	}

	// "americanfootball_nfl" is a LEAGUE; its sport is the key's prefix, and
	// the display name is the sample's `group`.
	var sport domain.Sport
	for _, s := range cat.Sports {
		if s.Slug() == "americanfootball" {
			sport = s
		}
	}
	if sport.IsZero() {
		t.Fatalf("catalogue has no americanfootball sport")
	}
	if got, want := sport.Name(), "American Football"; got != want {
		t.Errorf("sport name = %q, want %q (the sample's `group`)", got, want)
	}
	if nfl.SportID() != sport.ID() {
		t.Errorf("league %s names sport %s, want %s", nfl.ID(), nfl.SportID(), sport.ID())
	}

	// Several leagues share the americanfootball prefix in this sample
	// (NCAAF, NFL, NFL Super Bowl Winner). They must collapse onto ONE sport,
	// not three — provider.Catalogue.Validate rejects a duplicate sport, so a
	// regression here is a hard failure rather than a cosmetic one.
	seen := map[domain.SportID]int{}
	for _, s := range cat.Sports {
		seen[s.ID()]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("sport %s appears %d times", id, n)
		}
	}

	// Books are discovered from odds payloads, never invented from a key. The
	// catalogue is honestly empty before the first Fetch.
	if len(cat.Books) != 0 {
		t.Errorf("catalogue reports %d books before any odds payload; The Odds API publishes no "+
			"bookmaker endpoint, so a non-empty list here means a title was invented", len(cat.Books))
	}
}

// TestGoldenCatalogueIsFree asserts the catalogue costs no credits.
//
// ADR 0003 requirement 2 is built on it: "Refresh the event and league
// catalogue aggressively — only price polling costs anything." A regression
// that started charging for /v4/sports would silently convert a free
// aggressive refresh into a budget leak.
func TestGoldenCatalogueIsFree(t *testing.T) {
	stub := newProviderStub(t)
	stub.route("/v4/sports/", json200(readGolden(t, goldenSports)))
	h := newHarness(t, stub, nil)

	before := h.Adapter.client.Quota().LocalEstimate
	if _, err := h.Adapter.Catalogue(context.Background()); err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	if after := h.Adapter.client.Quota().LocalEstimate; after != before {
		t.Errorf("catalogue spent %d credits from the local estimate; /v4/sports is documented free",
			before-after)
	}
}

// TestGoldenOddsSweep is the central regression test: the provider's own
// published /odds sample, in American format, all the way through to domain
// values.
func TestGoldenOddsSweep(t *testing.T) {
	body := stripDocsElision(t, readGolden(t, goldenOdds))

	stub := newProviderStub(t)
	stub.route("/v4/sports/americanfootball_nfl/odds/", json200(body))
	h := newHarness(t, stub, nil)

	league := mustLeagueID(t, "americanfootball_nfl")
	scope := provider.Scope{
		League:  league,
		Markets: []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread},
	}

	snap, err := h.Adapter.Fetch(context.Background(), scope)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := snap.Validate(); err != nil {
		t.Fatalf("snapshot from the provider's own sample does not validate: %v", err)
	}

	// The sweep endpoint is reached with the scope's markets, not the process
	// configuration's, and with the region parameter the cost model prices.
	reqs := h.Stub.seen()
	if len(reqs) != 1 {
		t.Fatalf("issued %d requests, want exactly 1 sweep", len(reqs))
	}
	if got, want := reqs[0].Query.Get("markets"), "h2h,spreads"; got != want {
		t.Errorf("markets parameter = %q, want %q", got, want)
	}
	if got, want := reqs[0].Query.Get("oddsFormat"), "american"; got != want {
		t.Errorf("oddsFormat parameter = %q, want %q", got, want)
	}

	if len(snap.Events) != 1 {
		t.Fatalf("snapshot carries %d events, want 1", len(snap.Events))
	}
	e := snap.Events[0]

	if got, want := e.Event.ID(), mustEventID(t, "bda33adca828c09dc3cac3a856aef176"); got != want {
		t.Errorf("event id = %s, want %s", got, want)
	}
	if got, want := e.Event.Kind(), domain.EventKindMatch; got != want {
		t.Errorf("event kind = %s, want %s", got, want)
	}
	// The Odds API publishes no event name, so it is derived in the form
	// domain.EventParams.Name documents as typical.
	if got, want := e.Event.Name(), "Dallas Cowboys at Tampa Bay Buccaneers"; got != want {
		t.Errorf("event name = %q, want %q", got, want)
	}
	if got, want := e.Event.ScheduledStart(), time.Date(2021, 9, 10, 0, 20, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("scheduled start = %s, want %s", got, want)
	}
	// commence_time is in 2021 and the harness clock is 2026, so the provider's
	// own documented in-play test says this event has started.
	if got, want := e.Event.Status(), domain.EventStatusLive; got != want {
		t.Errorf("event status = %s, want %s", got, want)
	}

	// -------------------------------------------------------------- moneyline
	h2h := findMarket(t, e, domain.MarketTypeMoneyline, "")
	if h2h.Market.Line().Present() {
		t.Errorf("moneyline carries a line %s; domain.LineRuleForbidden says it must not", h2h.Market.Line())
	}
	if got, want := len(h2h.Prices), 24; got != want {
		t.Errorf("moneyline has %d prices, want %d (12 books × 2 sides)", got, want)
	}
	// -303 American is 1 + 100/303. The conversion is internal/domain/odds', not
	// a second implementation in this package.
	if p, ok := priceFor(t, h2h, domain.SelectionRoleHome, "unibet"); !ok {
		t.Errorf("no unibet price for the home moneyline")
	} else if !nearly(p.Decimal(), 1+100.0/303.0) {
		t.Errorf("unibet home moneyline = %v, want %v (american -303)", p.Decimal(), 1+100.0/303.0)
	}
	if p, ok := priceFor(t, h2h, domain.SelectionRoleAway, "unibet"); !ok {
		t.Errorf("no unibet price for the away moneyline")
	} else {
		if !nearly(p.Decimal(), 3.4) {
			t.Errorf("unibet away moneyline = %v, want 3.4 (american +240)", p.Decimal())
		}
		// This sample carries NO market-level last_update — it predates the
		// field — so the observation instant must come from the bookmaker-level
		// fallback. Stamping our own clock here is the failure wire.go warns
		// about: it would report zero provider staleness for ever.
		want := time.Date(2021, 6, 10, 13, 33, 18, 0, time.UTC)
		if !p.ObservedAt().Equal(want) {
			t.Errorf("observed_at = %s, want %s (unibet's bookmaker-level last_update)", p.ObservedAt(), want)
		}
		if p.ObservedAt().Equal(h.Now) {
			t.Errorf("observed_at is the fetch instant; it must be the PROVIDER's instant")
		}
	}

	// ----------------------------------------------------------------- spread
	spread := findMarket(t, e, domain.MarketTypeSpread, "")
	line, ok := spread.Market.Line().Value()
	if !ok {
		t.Fatalf("spread market carries no line")
	}
	// Eleven books quote ±6.5 and one quotes ±6. The market takes the modal
	// line, stated from the HOME perspective.
	if line != -6.5 {
		t.Errorf("spread line = %v, want -6.5 (the line eleven of twelve books quote, home perspective)", line)
	}
	if got, want := len(spread.Prices), 22; got != want {
		t.Errorf("spread has %d prices, want %d (11 books × 2 sides; betonlineag is off the consensus line)",
			got, want)
	}
	if _, ok := priceFor(t, spread, domain.SelectionRoleHome, "betonlineag"); ok {
		t.Errorf("betonlineag's spread survived at ±6 while the market trades at ±6.5; " +
			"domain.ValidatePriceForSelection would reject that pairing")
	}
	if got := h.dropped(DropReasonLineDisagreement); got != 2 {
		t.Errorf("line_disagreement drops = %v, want 2 (betonlineag's two sides). "+
			"The loss must be COUNTED, never silent", got)
	}
	// The away side trades at the inverse of the market's line. Reading
	// Market.Line() and forgetting to invert is the exact bug the
	// home-perspective convention exists to prevent.
	if p, ok := priceFor(t, spread, domain.SelectionRoleAway, "unibet"); !ok {
		t.Errorf("no unibet away spread price")
	} else {
		if v, _ := p.Line().Value(); v != 6.5 {
			t.Errorf("away spread price line = %v, want +6.5", v)
		}
		if !nearly(p.Decimal(), 1+100.0/109.0) {
			t.Errorf("unibet away spread = %v, want %v (american -109)", p.Decimal(), 1+100.0/109.0)
		}
	}

	// ------------------------------------------------------------ raw payload
	// provider.RawPayload requires "the provider's bytes, unmodified": the raw
	// topic is what a golden file is recorded from and the only artefact that
	// survives a parsing bug, so a re-marshalled struct would defeat it.
	if e.Raw.IsZero() {
		t.Fatalf("event carries no raw payload")
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(body, &elements); err != nil {
		t.Fatalf("re-parsing the repaired sample: %v", err)
	}
	if !bytes.Equal(e.Raw.Body, elements[0]) {
		t.Errorf("raw payload is not byte-identical to the provider's own array element")
	}
	// The odds format is not discoverable from the bytes, so it travels with
	// them as a media-type parameter.
	if got, want := e.Raw.ContentType, "application/json; odds-format=american"; got != want {
		t.Errorf("raw content type = %q, want %q", got, want)
	}

	// ------------------------------------------------------------------ books
	cat := provider.Catalogue{Books: h.Adapter.mapper.books.books()}
	if err := cat.Validate(); err != nil {
		t.Fatalf("book registry does not validate: %v", err)
	}
	if len(cat.Books) != 12 {
		t.Errorf("registry learned %d books, want 12", len(cat.Books))
	}
	dk, ok := cat.Book(mustBookID(t, "draftkings"))
	if !ok {
		t.Fatalf("registry has no draftkings book")
	}
	// The provider's own `title`, not a prettified key.
	if got, want := dk.Name(), "DraftKings"; got != want {
		t.Errorf("book name = %q, want %q", got, want)
	}
	if got, want := dk.Kind(), domain.BookKindExternal; got != want {
		t.Errorf("book kind = %s, want %s", got, want)
	}
	// The sample carries no pinnacle, so nothing claims to be the sharp
	// reference. An absent reference is the correct answer, not a promoted
	// stand-in.
	if _, ok := cat.ReferenceBook(); ok {
		t.Errorf("a book claims to be the sharp reference; the sample contains no %q", DefaultReferenceBook)
	}
}

// TestGoldenOddsSweepIsDeterministic guards the modal-line choice against Go's
// randomised map iteration.
//
// A line chosen by ranging a map would differ between two decodes of the SAME
// payload, and downstream that reads as a line move that never happened — which
// corrupts CLV and the line-movement chart rather than failing.
func TestGoldenOddsSweepIsDeterministic(t *testing.T) {
	body := stripDocsElision(t, readGolden(t, goldenOdds))
	league := mustLeagueID(t, "americanfootball_nfl")
	scope := provider.Scope{
		League:  league,
		Markets: []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread},
	}

	var first string
	for i := 0; i < 8; i++ {
		stub := newProviderStub(t)
		stub.route("/v4/sports/americanfootball_nfl/odds/", json200(body))
		h := newHarness(t, stub, nil)

		snap, err := h.Adapter.Fetch(context.Background(), scope)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		got := summarise(snap)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("decode %d differs from decode 0:\n got %s\nwant %s", i, got, first)
		}
	}
}

// summarise renders the identifier-bearing shape of a snapshot, which is
// exactly what must not move between two decodes of one payload.
func summarise(s provider.Snapshot) string {
	var b bytes.Buffer
	for _, e := range s.Events {
		b.WriteString(e.Event.ID().String())
		for _, m := range e.Markets {
			b.WriteString("|" + m.Market.ID().String() + "@" + m.Market.Line().String())
			for _, p := range m.Prices {
				b.WriteString("," + p.SelectionID().String() + "/" + p.BookID().String())
			}
		}
	}
	return b.String()
}

// TestGoldenEventOddsPlayerProps exercises the complementary sample: market-level
// last_update present, bookmaker-level absent, and a `description` naming the
// player.
func TestGoldenEventOddsPlayerProps(t *testing.T) {
	const providerEventID = "a512a48a58c4329048174217b2cc7ce0"

	stub := newProviderStub(t)
	stub.route("/v4/sports/americanfootball_nfl/events/"+providerEventID+"/odds",
		json200(readGolden(t, goldenEventOdds)))
	h := newHarness(t, stub, func(c *Config) {
		// Props are opt-in and priced per event; ADR 0003 scenario E is why.
		c.PlayerPropMarkets = []string{"player_pass_tds"}
	})

	scope := provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypePlayerProp},
		Events:  []domain.EventID{mustEventID(t, providerEventID)},
	}

	snap, err := h.Adapter.Fetch(context.Background(), scope)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := snap.Validate(); err != nil {
		t.Fatalf("snapshot does not validate: %v", err)
	}
	if len(snap.Events) != 1 {
		t.Fatalf("snapshot carries %d events, want 1", len(snap.Events))
	}
	e := snap.Events[0]

	// One wire market carrying two players is TWO domain markets, not two
	// selections: "David Blough over 0.5 passing TDs" and "Desmond Ridder over
	// 0.5 passing TDs" are different questions, and normalizer.MarketIDFor folds
	// the subject into the identifier for exactly that reason.
	if got, want := len(e.Markets), 2; got != want {
		t.Fatalf("event has %d markets, want %d (one per player): %s", got, want, marketSummary(e))
	}

	blough := findMarket(t, e, domain.MarketTypePlayerProp, "David Blough")
	if got, want := blough.Market.Subject(), "David Blough"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
	if v, ok := blough.Market.Line().Value(); !ok || v != 0.5 {
		t.Errorf("Blough line = %s, want 0.5", blough.Market.Line())
	}
	if got, want := len(blough.Prices), 4; got != want {
		t.Errorf("Blough prices = %d, want %d (2 books × over/under)", got, want)
	}
	if p, ok := priceFor(t, blough, domain.SelectionRoleOver, "draftkings"); !ok {
		t.Errorf("no draftkings over price for Blough")
	} else {
		if !nearly(p.Decimal(), 1+100.0/205.0) {
			t.Errorf("Blough over = %v, want %v (american -205)", p.Decimal(), 1+100.0/205.0)
		}
		// This sample carries market-level last_update and NO bookmaker-level
		// one, which is the branch the /odds sample cannot reach. The provider's
		// own schema recommends the market-level field.
		want := time.Date(2023, 1, 1, 5, 31, 29, 0, time.UTC)
		if !p.ObservedAt().Equal(want) {
			t.Errorf("observed_at = %s, want %s (the market-level last_update)", p.ObservedAt(), want)
		}
	}

	// The two books disagree about Ridder's line: DraftKings 0.5, FanDuel 1.5.
	// One vote each, so the tie is broken deterministically by the smaller line
	// and FanDuel's two quotes are dropped and counted.
	ridder := findMarket(t, e, domain.MarketTypePlayerProp, "Desmond Ridder")
	if v, ok := ridder.Market.Line().Value(); !ok || v != 0.5 {
		t.Errorf("Ridder line = %s, want 0.5 (deterministic tie-break)", ridder.Market.Line())
	}
	if got, want := len(ridder.Prices), 2; got != want {
		t.Errorf("Ridder prices = %d, want %d (draftkings only; fanduel is at 1.5)", got, want)
	}
	if got := h.dropped(DropReasonLineDisagreement); got != 2 {
		t.Errorf("line_disagreement drops = %v, want 2 (fanduel's two Ridder quotes)", got)
	}

	// A player-prop scope reaches the per-event endpoint, which is the only
	// place the provider serves non-featured markets.
	reqs := h.Stub.seen()
	if len(reqs) != 1 {
		t.Fatalf("issued %d requests, want 1", len(reqs))
	}
	if got, want := reqs[0].Query.Get("markets"), "player_pass_tds"; got != want {
		t.Errorf("markets parameter = %q, want %q", got, want)
	}
}

// TestGoldenNeutralDecoderRoundTrip proves the Decoder implements the
// normalizer's seam over the same published bytes the adapter maps.
//
// It matters because odds.raw.the-odds-api is replayed through THIS decoder, not
// through Fetch: a divergence between the two paths would show up as the
// normalizer disagreeing with the adapter about a price it had already seen.
func TestGoldenNeutralDecoderRoundTrip(t *testing.T) {
	body := stripDocsElision(t, readGolden(t, goldenOdds))
	var elements []json.RawMessage
	if err := json.Unmarshal(body, &elements); err != nil {
		t.Fatalf("parse sample: %v", err)
	}

	d, err := NewDecoder(OddsFormatAmerican, DefaultReferenceBook, nil)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got, want := string(d.Provider()), ProviderSlug; got != want {
		t.Errorf("decoder provider = %q, want %q", got, want)
	}

	raw, err := d.Decode(elements[0])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got, want := raw.ID, "bda33adca828c09dc3cac3a856aef176"; got != want {
		t.Errorf("raw id = %q, want %q", got, want)
	}
	// The Odds API's `sport_key` is a LEAGUE key; the sport is its prefix.
	if got, want := raw.LeagueKey, "americanfootball_nfl"; got != want {
		t.Errorf("league key = %q, want %q", got, want)
	}
	if got, want := raw.SportKey, "americanfootball"; got != want {
		t.Errorf("sport key = %q, want %q", got, want)
	}
	if len(raw.Books) != 12 {
		t.Fatalf("raw carries %d books, want 12", len(raw.Books))
	}

	// The sharp-reference designation travels with the decoder, not with the
	// payload, because The Odds API publishes no sharpness label. The sample
	// carries no "pinnacle" bookmaker (see TestGoldenCatalogue), so a decoder
	// configured with the default key must flag NOTHING rather than guessing —
	// and a decoder configured with a key the sample does carry must flag
	// exactly that one book. Both halves are asserted, because a flag that is
	// always false and a flag that is always true look identical from one test.
	for _, b := range raw.Books {
		if b.Reference {
			t.Errorf("book %s is flagged sharp, but the sample carries no %q bookmaker",
				b.Key, DefaultReferenceBook)
		}
	}
	present, err := NewDecoder(OddsFormatAmerican, "draftkings", nil)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	flagged, err := present.Decode(elements[0])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	designated := 0
	for _, b := range flagged.Books {
		if b.Reference {
			designated++
			if b.Key != "draftkings" {
				t.Errorf("flagged %s, want draftkings", b.Key)
			}
		}
	}
	if designated != 1 {
		t.Errorf("%d books flagged sharp, want exactly 1", designated)
	}

	// RawOutcome.Price is ALWAYS decimal — the neutral shape's own contract.
	// American -303 must never reach the neutral shape.
	for _, b := range raw.Books {
		for _, m := range b.Markets {
			for _, o := range m.Outcomes {
				if o.Price <= 1 {
					t.Fatalf("book %s market %s outcome %q price %v is not decimal odds; the neutral "+
						"shape requires decimal and an American number read as decimal is a silent "+
						"catastrophe", b.Key, m.Key, o.Name, o.Price)
				}
			}
		}
	}

	// A decoder constructed for the WRONG format is the one failure this
	// package cannot detect, and the test says so out loud rather than
	// pretending otherwise.
	//
	// Reading this American payload as decimal refuses every NEGATIVE price
	// (-303 is not a legal decimal) but SILENTLY ACCEPTS every positive one:
	// +240 becomes decimal odds of 240.0, a 23,900% return, which is arithmetic
	// nonsense and entirely valid. wire.go's warning is exactly this — "a raw
	// -110 read as decimal odds is a catastrophic and entirely silent error".
	//
	// So the mitigation is not detection, it is that the format never has to be
	// guessed: it travels with the request into NewDecoder, and it travels with
	// the archived bytes as RawContentType's media-type parameter.
	wrong, err := NewDecoder(OddsFormatDecimal, DefaultReferenceBook, nil)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	got, err := wrong.Decode(elements[0])
	if err != nil {
		t.Fatalf("Decode with the wrong format returned an error; it drops outcomes instead: %v", err)
	}
	survivors, absurd := 0, 0
	for _, b := range got.Books {
		for _, m := range b.Markets {
			for _, o := range m.Outcomes {
				survivors++
				if o.Price > 100 {
					absurd++
				}
			}
		}
	}
	if survivors == 0 {
		t.Fatalf("reading the American sample as decimal dropped everything; the point of this test is " +
			"that it does NOT, and that the format therefore cannot be inferred from the payload")
	}
	if absurd != survivors {
		t.Errorf("%d of %d surviving prices are implausibly large; a misread American payload should "+
			"produce nothing BUT implausible prices", absurd, survivors)
	}
	if got, want := RawContentType(OddsFormatAmerican), "application/json; odds-format=american"; got != want {
		t.Errorf("RawContentType = %q, want %q — this parameter is the only thing that tells a replayer "+
			"how to read `price`", got, want)
	}
}

func mustBookID(t *testing.T, key string) domain.BookID {
	t.Helper()
	id, err := normalizer.BookIDFor(testProvider(t), key)
	if err != nil {
		t.Fatalf("BookIDFor(%q): %v", key, err)
	}
	return id
}
