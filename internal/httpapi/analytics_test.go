package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// Tests for the phase 9 analytics surface.
//
// The assertions cluster around two things, and neither is "the handler returned
// 200". The first is WHAT THE HANDLER ASKED THE STORE FOR — every window,
// threshold and unit conversion on this surface is invisible in a response body
// and decisive in a query plan, so the fakes record the query and the tests read
// it. The second is the set of promises the spec makes that a renderer will
// otherwise quietly break: null means stay null, an empty list is a real answer,
// staleness travels on every arbitrage leg, and no account identifier reaches
// the leaderboard.

// -----------------------------------------------------------------------------
// Empty is correct
// -----------------------------------------------------------------------------

// TestAnalyticsWithNoFindingsAnswers200WithEmptyCollections is the "no mock
// data" rule for phase 9.
//
// `pricer` may have detected nothing and `settle` may have graded nothing — that
// is the state of every fresh deployment, and it is not an error. Each endpoint
// must answer 200 with `[]`, never a 404, never a 500, and emphatically never an
// example finding. An empty +EV list is then diagnosable as "the detector has
// not fired" rather than mistaken for a broken API, and a demo can never show a
// fabricated arbitrage.
func TestAnalyticsWithNoFindingsAnswers200WithEmptyCollections(t *testing.T) {
	t.Parallel()

	d := newDeps()
	api := d.api(t)

	t.Run("ev", func(t *testing.T) {
		rec, req := newRequest(http.MethodGet, "/v1/signals/ev", nil)
		api.handleEVSignals(rec, req)
		requireStatus(t, rec, http.StatusOK)
		assertJSONArrayNotNull(t, rec, "data")
	})

	t.Run("arbitrage", func(t *testing.T) {
		rec, req := newRequest(http.MethodGet, "/v1/signals/arbitrage", nil)
		api.handleArbitrageSignals(rec, req)
		requireStatus(t, rec, http.StatusOK)
		assertJSONArrayNotNull(t, rec, "data")
	})

	t.Run("steam", func(t *testing.T) {
		rec, req := newRequest(http.MethodGet, "/v1/signals/steam", nil)
		api.handleSteamSignals(rec, req)
		requireStatus(t, rec, http.StatusOK)
		assertJSONArrayNotNull(t, rec, "data")
	})

	t.Run("leaderboard", func(t *testing.T) {
		rec, req := newRequest(http.MethodGet, "/v1/leaderboard", nil)
		api.handleLeaderboard(rec, req)
		requireStatus(t, rec, http.StatusOK)
		assertJSONArrayNotNull(t, rec, "data")
	})

	t.Run("account clv", func(t *testing.T) {
		rec := serveAuthed(t, d.api(t).handleAccountCLV, "usr_1", http.MethodGet, "/v1/account/clv", nil)
		requireStatus(t, rec, http.StatusOK)
		assertJSONArrayNotNull(t, rec, "data")
		assertJSONArrayNotNull(t, rec, "by_league")
	})
}

// -----------------------------------------------------------------------------
// The bounds the handlers push down
// -----------------------------------------------------------------------------

// TestSignalWindowsDefaultAndAreCeilinged checks the bound that keeps a
// hypertable read off every chunk ever created.
//
// `ev_signals` and `steam_signals` have no retention policy, so an unbounded
// lower bound is a scan of the whole history — history.go states the rule for
// `prices` and these feeds inherit it. The default must be present, and a
// caller must not be able to reach past the matching Kafka topic's retention,
// because the window this API serves and the window phase 12 could replay are
// meant to be the same window.
func TestSignalWindowsDefaultAndAreCeilinged(t *testing.T) {
	t.Parallel()

	t.Run("ev defaults to six hours", func(t *testing.T) {
		d := newDeps()
		rec, req := newRequest(http.MethodGet, "/v1/signals/ev", nil)
		d.api(t).handleEVSignals(rec, req)
		requireStatus(t, rec, http.StatusOK)

		if got, want := d.signals.evQuery.ObservedAfter, testNow.Add(-defaultEVSignalWindow); !got.Equal(want) {
			t.Errorf("observed_after = %s, want %s", got, want)
		}
	})

	t.Run("ev refuses a lookback past the topic retention", func(t *testing.T) {
		d := newDeps()
		tooOld := testNow.Add(-maxEVSignalLookback - time.Hour).Format(time.RFC3339)
		rec, req := newRequest(http.MethodGet, "/v1/signals/ev?observed_after="+tooOld, nil)
		d.api(t).handleEVSignals(rec, req)
		requireStatus(t, rec, http.StatusBadRequest)
		assertInvalidParam(t, rec, "observed_after")
	})

	t.Run("steam defaults to two hours", func(t *testing.T) {
		d := newDeps()
		rec, req := newRequest(http.MethodGet, "/v1/signals/steam", nil)
		d.api(t).handleSteamSignals(rec, req)
		requireStatus(t, rec, http.StatusOK)

		if got, want := d.signals.steamQuery.WindowEndAfter, testNow.Add(-defaultSteamSignalWindow); !got.Equal(want) {
			t.Errorf("window_end_after = %s, want %s", got, want)
		}
	})
}

// TestArbitrageDefaultsAreTheDetectorsOwnBounds is decision 5 of the phase 9
// brief as an assertion.
//
// The phase 4 gate measured the leg-age bound binding almost constantly: most
// cross-book "arbitrage" is one book that has not moved yet. The reader's
// default must therefore be the DETECTOR's own bound — a narrower default would
// hide findings silently, and the response must echo whatever was applied so a
// reader can see why the list is short.
func TestArbitrageDefaultsAreTheDetectorsOwnBounds(t *testing.T) {
	t.Parallel()

	d := newDeps()
	rec, req := newRequest(http.MethodGet, "/v1/signals/arbitrage", nil)
	d.api(t).handleArbitrageSignals(rec, req)
	requireStatus(t, rec, http.StatusOK)

	q := d.signals.arbQuery
	if q.MaxLegAge != defaultArbMaxLegAge {
		t.Errorf("max leg age = %s, want %s (pricing.DefaultArbitrageConfig().MaxLegAge)", q.MaxLegAge, defaultArbMaxLegAge)
	}
	if q.MaxObservedSpread != defaultArbMaxSpread {
		t.Errorf("max spread = %s, want %s (pricing.DefaultArbitrageConfig().MaxLegSpread)", q.MaxObservedSpread, defaultArbMaxSpread)
	}
	if q.MinDistinctBooks != 1 {
		// 1 is legal and is the STRONGER finding: a single under-round market
		// has no execution risk from a second book moving between bets.
		t.Errorf("min distinct books = %d, want 1", q.MinDistinctBooks)
	}

	body := decodeJSONBody[gen.ArbitrageSignalList](t, rec)
	if body.Bounds.MaxLegAgeSeconds != defaultArbMaxLegAge.Seconds() {
		t.Errorf("echoed max_leg_age_seconds = %v, want %v", body.Bounds.MaxLegAgeSeconds, defaultArbMaxLegAge.Seconds())
	}
	if body.Bounds.MaxSpreadSeconds != defaultArbMaxSpread.Seconds() {
		t.Errorf("echoed max_spread_seconds = %v, want %v", body.Bounds.MaxSpreadSeconds, defaultArbMaxSpread.Seconds())
	}
	if body.Page.NextCursor != nil || body.Page.HasMore {
		t.Error("the arbitrage list must not advertise a next page; the live set turns over faster than a cursor survives")
	}
}

// TestArbitrageReturnIsSentAsAFractionNotAPercent guards the one unit conversion
// on this surface that is silent when it is wrong.
//
// The wire speaks percent because that is what a reader reads; the column holds
// the fraction. A handler that forwarded 2 (meaning 2%) as a fraction would ask
// the database for findings returning 200% and every list would be empty — with
// no error anywhere.
func TestArbitrageReturnIsSentAsAFractionNotAPercent(t *testing.T) {
	t.Parallel()

	d := newDeps()
	rec, req := newRequest(http.MethodGet, "/v1/signals/arbitrage?min_return_percent=2.5", nil)
	d.api(t).handleArbitrageSignals(rec, req)
	requireStatus(t, rec, http.StatusOK)

	if got, want := d.signals.arbQuery.MinReturnFraction, 0.025; got != want {
		t.Errorf("min return fraction = %v, want %v", got, want)
	}
}

// TestArbitrageSurfacesLegAgeOnEveryLeg is the other half of decision 5.
//
// A finding without its per-leg ages is exactly the misleading artefact the
// discipline exists to prevent: a two-leg arbitrage with one fresh side and one
// stale side is not an opportunity, and the only place that is visible is the
// leg.
func TestArbitrageSurfacesLegAgeOnEveryLeg(t *testing.T) {
	t.Parallel()

	d := newDeps()
	d.signals.arb = []ArbitrageSignal{{
		ID:                "0192f3a1-4b2c-7d8e-9f01-23456789abcd",
		MarketID:          "mkt_1",
		MarketType:        domain.MarketTypeMoneyline,
		LeagueID:          "lg_nfl",
		SelectionCount:    2,
		ImpliedSum:        0.97,
		ReturnFraction:    0.030927835051546393,
		DistinctBooks:     2,
		ObservedSpread:    4 * time.Second,
		OldestLegAge:      12 * time.Second,
		ObservedAt:        testNow.Add(-12 * time.Second),
		MaxLegAge:         defaultArbMaxLegAge,
		MaxObservedSpread: defaultArbMaxSpread,
		DetectedAt:        testNow,
		Legs: []ArbitrageLeg{
			{Index: 0, SelectionID: "sel_h", Role: domain.SelectionRoleHome, BookID: "bk_a", DecimalOdds: 2.10, StakeFraction: 0.49, ObservedAt: testNow.Add(-12 * time.Second), Age: 12 * time.Second},
			{Index: 1, SelectionID: "sel_a", Role: domain.SelectionRoleAway, BookID: "bk_b", DecimalOdds: 2.05, StakeFraction: 0.51, ObservedAt: testNow.Add(-8 * time.Second), Age: 8 * time.Second},
		},
	}}

	rec, req := newRequest(http.MethodGet, "/v1/signals/arbitrage", nil)
	d.api(t).handleArbitrageSignals(rec, req)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.ArbitrageSignalList](t, rec)
	if len(body.Data) != 1 {
		t.Fatalf("data = %d findings, want 1", len(body.Data))
	}
	got := body.Data[0]
	if got.OldestLegAgeSeconds != 12 || got.ObservedSpreadSeconds != 4 {
		t.Errorf("staleness = (oldest %v, spread %v), want (12, 4)", got.OldestLegAgeSeconds, got.ObservedSpreadSeconds)
	}
	if len(got.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(got.Legs))
	}
	for _, leg := range got.Legs {
		if leg.AgeSeconds <= 0 {
			t.Errorf("leg %d has no age; a reader cannot tell a fresh arb from a stale one without it", leg.LegIndex)
		}
	}
	// return_percent is derived rather than stored, so it must be exactly 100x
	// the fraction the detector recorded.
	if want := got.ReturnFraction * 100; got.ReturnPercent != want {
		t.Errorf("return_percent = %v, want %v", got.ReturnPercent, want)
	}
}

// -----------------------------------------------------------------------------
// Cursors
// -----------------------------------------------------------------------------

// TestEVCursorRoundTripsExactly checks the float component of the +EV key.
//
// The list is ranked by expected value, so TIES AT THE PAGE BOUNDARY ARE THE
// NORMAL CASE — two books quoting the same price on the same selection produce
// the same number. A cursor that rounded the boundary would re-emit or skip
// every row inside the rounding interval, silently.
func TestEVCursorRoundTripsExactly(t *testing.T) {
	t.Parallel()

	scope := cursorScope("signals.ev", "test")
	want := EVSignalKey{
		ExpectedValuePercent: 3.1415926535897932,
		QuoteObservedAt:      testNow.Add(-97 * time.Millisecond),
		SelectionID:          "sel_1",
		BookID:               "bk_a",
	}
	encoded := encodeSignalCursor(scope,
		strconv.FormatFloat(want.ExpectedValuePercent, 'g', -1, 64),
		strconv.FormatInt(want.QuoteObservedAt.UnixNano(), 10),
		want.SelectionID.String(),
		want.BookID.String(),
	)
	got, err := decodeEVCursor(encoded, scope)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ExpectedValuePercent != want.ExpectedValuePercent {
		t.Errorf("expected value = %v, want %v (the cursor must name the same float64 the page ended on)",
			got.ExpectedValuePercent, want.ExpectedValuePercent)
	}
	if !got.QuoteObservedAt.Equal(want.QuoteObservedAt) {
		t.Errorf("instant = %s, want %s", got.QuoteObservedAt, want.QuoteObservedAt)
	}
}

// TestSignalCursorRefusesADifferentQuery is the scope fingerprint doing its job.
//
// Unlike the board, where `book` only changes how a page is rendered, every
// filter on the +EV feed changes WHICH ROWS are in the set. A client that
// changed one mid-listing and was served a page anyway would receive rows from a
// different set, consistently ordered, with nothing reporting it.
func TestSignalCursorRefusesADifferentQuery(t *testing.T) {
	t.Parallel()

	minted := cursorScope("signals.ev", "a")
	presented := cursorScope("signals.ev", "b")

	encoded := encodeSignalCursor(minted, "1", "0", "sel_1", "bk_a")
	if _, err := decodeEVCursor(encoded, presented); err == nil {
		t.Fatal("a cursor minted under one filter set decoded under another")
	}
}

// TestSignalCursorAndEventCursorCannotBeConfused checks the two codecs.
//
// They share a version byte and a separator on purpose — one thing to version —
// and they differ in layout on purpose, so neither can silently accept the
// other's payload and produce a plausible wrong page.
func TestSignalCursorAndEventCursorCannotBeConfused(t *testing.T) {
	t.Parallel()

	scope := cursorScope("board", "lg_nfl")
	event := encodeCursor(cursor{
		key:   EventKey{ScheduledStart: testNow, ID: "evt_1"},
		scope: scope,
	})
	if _, err := decodeSignalCursor(event, scope, 4); err == nil {
		t.Error("an event cursor decoded as a four-field signal cursor")
	}

	signal := encodeSignalCursor(scope, "1", "0", "sel_1", "bk_a")
	if _, err := decodeCursor(signal, scope); err == nil {
		t.Error("a signal cursor decoded as an event cursor")
	}
}

// -----------------------------------------------------------------------------
// CLV
// -----------------------------------------------------------------------------

// TestCLVShowsExcludedRowsAndDoesNotCountThem is the rule odds/clv.go states and
// this surface has to keep on both sides at once.
//
// "Show it next to the two lines in a user interface; never rank anyone by it."
// A line-moved leg is in `data` with both lines on it; the aggregate's counts
// say it was dropped. If the two ever disagree a customer computes a mean from
// their own rows that does not match the one the leaderboard ranked them on.
func TestCLVShowsExcludedRowsAndDoesNotCountThem(t *testing.T) {
	t.Parallel()

	takenLine, closingLine := -3.0, -3.5
	d := newDeps()
	d.clv.page = CLVPage{Entries: []CLVEntry{
		{
			LegID: "leg_moved", WagerID: "wgr_1", MarketID: "mkt_1",
			MarketType: domain.MarketTypeSpread, SelectionID: "sel_h", LeagueID: "lg_nfl",
			TakenBookID: "bk_a", ClosingBookID: "bk_ref", DevigMethod: odds.MethodMultiplicative,
			TakenLine: &takenLine, ClosingLine: &closingLine,
			TakenAt: testNow.Add(-2 * time.Hour), ClosedAt: testNow.Add(-time.Hour),
			TakenFair: 0.50, ClosingFair: 0.53, TakenPrice: 2.0, ClosingPrice: 1.8868,
			ProbabilityCLV: 0.03, PercentCLV: 6.0, Magnitude: 6.0,
			BeatClose: true, LineMoved: true,
			Status: domain.LegStatusWon, GradedAt: testNow.Add(-30 * time.Minute),
		},
		{
			LegID: "leg_void", WagerID: "wgr_2", MarketID: "mkt_2",
			MarketType: domain.MarketTypeMoneyline, SelectionID: "sel_a", LeagueID: "lg_nfl",
			TakenBookID: "bk_a", ClosingBookID: "bk_ref", DevigMethod: odds.MethodMultiplicative,
			TakenAt: testNow.Add(-3 * time.Hour), ClosedAt: testNow.Add(-2 * time.Hour),
			TakenFair: 0.40, ClosingFair: 0.40, TakenPrice: 2.5, ClosingPrice: 2.5,
			Status: domain.LegStatusVoid, Voided: true, GradedAt: testNow.Add(-40 * time.Minute),
		},
	}}
	d.clv.aggregate = CLVAggregate{
		Samples: 2, Counted: 0, VoidExcluded: 1, LineMovedExcluded: 1,
	}

	rec := serveAuthed(t, d.api(t).handleAccountCLV, "usr_1", http.MethodGet, "/v1/account/clv", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.CLVResponse](t, rec)

	if len(body.Data) != 2 {
		t.Fatalf("data = %d rows, want 2; the display path shows what the aggregate drops", len(body.Data))
	}
	if !body.Data[0].LineMoved || body.Data[0].TakenLine == nil || body.Data[0].ClosingLine == nil {
		t.Error("the line-moved row must carry both lines, or a customer cannot see what happened")
	}
	if !body.Data[1].Voided {
		t.Error("the voided row must be flagged as such")
	}
	if body.Aggregate.Counted != 0 || body.Aggregate.VoidExcluded != 1 || body.Aggregate.LineMovedExcluded != 1 {
		t.Errorf("aggregate counts = %+v, want counted 0 with one exclusion of each kind", body.Aggregate)
	}
}

// TestCLVMeanStaysNullWithNothingCountable is the null that must not become a
// zero.
//
// odds.AggregateCLV reports ErrCLVNoSamples rather than returning zero for the
// same reason: a mean over no numbers does not exist. A surface that rendered it
// as 0.00% would tell a customer with three line-moved legs that they are
// exactly break-even, which is a claim nobody made.
func TestCLVMeanStaysNullWithNothingCountable(t *testing.T) {
	t.Parallel()

	d := newDeps()
	d.clv.aggregate = CLVAggregate{Samples: 3, Counted: 0, LineMovedExcluded: 3}

	rec := serveAuthed(t, d.api(t).handleAccountCLV, "usr_1", http.MethodGet, "/v1/account/clv", nil)
	requireStatus(t, rec, http.StatusOK)

	var raw struct {
		Aggregate map[string]json.RawMessage `json:"aggregate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"mean_probability_clv", "mean_percent_clv", "beat_rate"} {
		if v, present := raw.Aggregate[field]; present && string(v) != "null" {
			t.Errorf("%s = %s, want absent or null with nothing countable", field, string(v))
		}
	}
}

// TestCLVRefusesAnInvertedWindow keeps the 400/422 distinction respond.go draws.
//
// A `from` after its `to` is syntactically valid and semantically impossible.
// Serving it as an empty page would be worse than either status, because an
// empty page is indistinguishable from a customer who has never had a leg
// graded.
func TestCLVRefusesAnInvertedWindow(t *testing.T) {
	t.Parallel()

	d := newDeps()
	target := "/v1/account/clv?graded_from=" + testNow.Format(time.RFC3339) +
		"&graded_to=" + testNow.Add(-time.Hour).Format(time.RFC3339)
	rec := serveAuthed(t, d.api(t).handleAccountCLV, "usr_1", http.MethodGet, target, nil)
	requireStatus(t, rec, http.StatusUnprocessableEntity)
}

// -----------------------------------------------------------------------------
// The leaderboard
// -----------------------------------------------------------------------------

// TestLeaderboardRanksOnROINotProfit is CLAUDE.md §6's rule as an assertion, and
// it is the gate item for this phase.
//
// A customer who staked a fortune and lost must not outrank one who staked
// little and won. That property is structural rather than incidental: ROI is
// net return divided by turnover, so the comparison the board makes cannot see
// stake size at all. This test states it with numbers where a profit ranking
// would give the opposite answer — the loser's turnover is 200x the winner's,
// and their net return is the larger absolute figure.
func TestLeaderboardRanksOnROINotProfit(t *testing.T) {
	t.Parallel()

	// Staked 1,000,000 minor units and lost 50,000 of it: ROI -5%.
	whale := LeaderboardEntry{
		UserID: "usr_whale", SettledWagers: 400,
		Staked: 1_000_000, NetReturn: -50_000, ROI: -0.05,
		CLVSamples: 400, BeatCount: 100, BeatRate: 0.25,
		MeanProbabilityCLV: -0.004, MeanPercentCLV: -0.9,
	}
	// Staked 5,000 and made 600: ROI +12%, on a far smaller absolute profit than
	// the whale's absolute loss.
	sharp := LeaderboardEntry{
		UserID: "usr_sharp", SettledWagers: 60,
		Staked: 5_000, NetReturn: 600, ROI: 0.12,
		CLVSamples: 60, BeatCount: 41, BeatRate: 0.6833333333333333,
		MeanProbabilityCLV: 0.012, MeanPercentCLV: 2.4,
	}

	if sharp.ROI <= whale.ROI {
		t.Fatal("the fixture does not exercise the property it claims to")
	}
	if whale.Staked <= sharp.Staked {
		t.Fatal("the fixture does not exercise the property it claims to")
	}

	d := newDeps()
	// The store returns them already ranked; the handler must not re-sort, and
	// must not promote the larger money.
	d.leaderboard.entries = []LeaderboardEntry{sharp, whale}

	rec, req := newRequest(http.MethodGet, "/v1/leaderboard", nil)
	d.api(t).handleLeaderboard(rec, req)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.LeaderboardPage](t, rec)
	if len(body.Data) != 2 {
		t.Fatalf("data = %d rows, want 2", len(body.Data))
	}
	if body.Data[0].Rank != 1 || body.Data[1].Rank != 2 {
		t.Errorf("ranks = %d, %d; want 1, 2 dense over the returned order", body.Data[0].Rank, body.Data[1].Rank)
	}
	if body.Data[0].Roi <= body.Data[1].Roi {
		t.Errorf("the top row's ROI (%v) is not above the second's (%v)", body.Data[0].Roi, body.Data[1].Roi)
	}
	if body.Data[0].StakedMinor >= body.Data[1].StakedMinor {
		t.Error("the top row has the larger turnover; this board must not reward stake size")
	}
	if body.Basis != gen.Roi {
		t.Errorf("basis = %q, want roi by default", body.Basis)
	}
}

// TestLeaderboardHasNoProfitBasis makes the refusal structural rather than
// documented. `basis=profit` must be a parameter error, not a third ranking.
func TestLeaderboardHasNoProfitBasis(t *testing.T) {
	t.Parallel()

	d := newDeps()
	rec, req := newRequest(http.MethodGet, "/v1/leaderboard?basis=profit", nil)
	d.api(t).handleLeaderboard(rec, req)
	requireStatus(t, rec, http.StatusBadRequest)
	assertInvalidParam(t, rec, "basis")
}

// TestLeaderboardEchoesItsSampleFloors checks the fact that makes the ranking
// readable.
//
// A board without its minimum sample size cannot be audited: a reader has no way
// to know whether the name at the top got there on one lucky maximum-stake bet.
// The floors are parameters with a default, and the default must be visible.
func TestLeaderboardEchoesItsSampleFloors(t *testing.T) {
	t.Parallel()

	d := newDeps()
	rec, req := newRequest(http.MethodGet, "/v1/leaderboard", nil)
	d.api(t).handleLeaderboard(rec, req)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.LeaderboardPage](t, rec)
	if body.MinimumSamples.SettledWagers != defaultMinSettledWagers {
		t.Errorf("settled_wagers floor = %d, want %d", body.MinimumSamples.SettledWagers, defaultMinSettledWagers)
	}
	if body.MinimumSamples.ClvSamples != defaultMinCLVSamples {
		t.Errorf("clv_samples floor = %d, want %d", body.MinimumSamples.ClvSamples, defaultMinCLVSamples)
	}
	if q := d.leaderboard.query; q.MinSettledWagers != defaultMinSettledWagers || q.MinCLVSamples != defaultMinCLVSamples {
		t.Errorf("floors pushed down = %+v, want the same numbers the response echoed", q)
	}
	if body.Window.From.After(body.Window.To) {
		t.Error("the echoed window is inverted")
	}
}

// TestLeaderboardPublishesNoAccountIdentity is the privacy property.
//
// `users` holds an email address and nothing else, so there is no name to
// publish; the account identifier must not stand in for one on a public,
// unauthenticated page. The handle is derived, stable across renders, and
// carries none of the input.
func TestLeaderboardPublishesNoAccountIdentity(t *testing.T) {
	t.Parallel()

	const user = domain.UserID("usr_01J8Z9QK3M4N5P6R7S8T9V0W1X")

	d := newDeps()
	d.leaderboard.entries = []LeaderboardEntry{{
		UserID: user, SettledWagers: 50, Staked: 10_000, NetReturn: 500, ROI: 0.05,
		CLVSamples: 50, BeatCount: 30, BeatRate: 0.6,
	}}

	rec, req := newRequest(http.MethodGet, "/v1/leaderboard", nil)
	d.api(t).handleLeaderboard(rec, req)
	requireStatus(t, rec, http.StatusOK)

	if strings.Contains(rec.Body.String(), user.String()) {
		t.Fatal("the account identifier reached a public response body")
	}

	body := decodeJSONBody[gen.LeaderboardPage](t, rec)
	if len(body.Data) != 1 {
		t.Fatalf("data = %d rows, want 1", len(body.Data))
	}
	if got := body.Data[0].User; got != publicHandle(user) {
		t.Errorf("user = %q, want the derived handle %q", got, publicHandle(user))
	}
	// Two different accounts must not collapse onto one row, and the same
	// account must render identically on every request — a customer has to be
	// able to find themselves between refreshes.
	if publicHandle(user) == publicHandle("usr_someone_else") {
		t.Error("two accounts derived the same handle")
	}
}

// -----------------------------------------------------------------------------
// Steam
// -----------------------------------------------------------------------------

// TestSteamRendersNoneAsADevigMethodAndKeepsRecencyOrder covers the two things a
// renderer most easily gets wrong on this feed.
//
// `none` is a legal margin treatment and the expected one — a book's margin is
// very nearly constant across a window of seconds — so it must render as the
// enum member rather than as an empty string. And the feed is ordered by
// RECENCY: the handler must not re-sort by magnitude, however tempting a
// "biggest moves" list looks.
func TestSteamRendersNoneAsADevigMethodAndKeepsRecencyOrder(t *testing.T) {
	t.Parallel()

	shin := odds.MethodShin
	d := newDeps()
	d.signals.steam = SteamSignalPage{Signals: []SteamSignal{
		{
			MarketID: "mkt_recent", MarketType: domain.MarketTypeSpread, LeagueID: "lg_nfl",
			SelectionID: "sel_h",
			WindowStart: testNow.Add(-90 * time.Second), WindowEnd: testNow.Add(-30 * time.Second),
			Window: time.Minute, Hop: 15 * time.Second,
			Direction: SteamShorten, DeltaProbability: 0.012, Magnitude: 0.012, Velocity: 0.012,
			DevigMethod: nil,
			LeadBookID:  "bk_ref", LeadMovedAt: testNow.Add(-80 * time.Second),
			Followers: []SteamFollower{
				{BookID: "bk_a", MovedAt: testNow.Add(-50 * time.Second), Lag: 30 * time.Second, DeltaProbability: 0.010},
			},
			FollowerCount: 1, ParticipatingBooks: 2, CrossBookCorrelation: 0.94,
			ThresholdVelocity: 0.005, ThresholdMagnitude: 0.008, ThresholdCorrelation: 0.7,
			MinFollowers: 1, MaxFollowerLag: 90 * time.Second, DetectedAt: testNow,
		},
		{
			MarketID: "mkt_bigger", MarketType: domain.MarketTypeTotal, LeagueID: "lg_nfl",
			SelectionID: "sel_o",
			WindowStart: testNow.Add(-2 * time.Hour), WindowEnd: testNow.Add(-119 * time.Minute),
			Window: time.Minute, Hop: 15 * time.Second,
			Direction: SteamDrift, DeltaProbability: -0.045, Magnitude: 0.045, Velocity: -0.045,
			DevigMethod: &shin,
			LeadBookID:  "bk_ref", LeadMovedAt: testNow.Add(-2 * time.Hour),
			Followers: []SteamFollower{
				{BookID: "bk_b", MovedAt: testNow.Add(-119 * time.Minute), Lag: 60 * time.Second, DeltaProbability: -0.040},
			},
			FollowerCount: 1, ParticipatingBooks: 2, CrossBookCorrelation: 0.91,
			ThresholdVelocity: 0.005, ThresholdMagnitude: 0.008, ThresholdCorrelation: 0.7,
			MinFollowers: 1, MaxFollowerLag: 90 * time.Second, DetectedAt: testNow,
		},
	}}

	rec, req := newRequest(http.MethodGet, "/v1/signals/steam", nil)
	d.api(t).handleSteamSignals(rec, req)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.SteamSignalPage](t, rec)
	if len(body.Data) != 2 {
		t.Fatalf("data = %d rows, want 2", len(body.Data))
	}
	if body.Data[0].MarketId != "mkt_recent" {
		t.Error("the feed was re-sorted; steam is ranked by recency because the follower lag is the opportunity")
	}
	if body.Data[0].DevigMethod != gen.SteamBasisNone {
		t.Errorf("devig_method = %q, want %q", body.Data[0].DevigMethod, gen.SteamBasisNone)
	}
	if body.Data[1].DevigMethod != gen.SteamBasis(shin.String()) {
		t.Errorf("devig_method = %q, want %q", body.Data[1].DevigMethod, shin.String())
	}
	if body.Data[0].Direction != gen.Shorten || body.Data[1].Direction != gen.Drift {
		t.Error("direction must travel as a word so colour is never its only carrier")
	}
}

// -----------------------------------------------------------------------------
// Filters
// -----------------------------------------------------------------------------

// TestUnknownFilterValuesAre400 keeps the rule parseBookFilter states, on the
// two filters phase 9 adds.
//
// A typo that quietly returns nothing is indistinguishable from "there is
// nothing to report" — and unlike the board, "nothing to report" is the common
// answer here, so an analytics surface that can fake it is one nobody can trust.
func TestUnknownFilterValuesAre400(t *testing.T) {
	t.Parallel()

	t.Run("league", func(t *testing.T) {
		d := newDeps()
		rec, req := newRequest(http.MethodGet, "/v1/signals/ev?league=not-a-league", nil)
		d.api(t).handleEVSignals(rec, req)
		requireStatus(t, rec, http.StatusBadRequest)
		assertInvalidParam(t, rec, "league")
	})

	t.Run("market type", func(t *testing.T) {
		d := newDeps()
		rec, req := newRequest(http.MethodGet, "/v1/signals/ev?market_type=parlay", nil)
		d.api(t).handleEVSignals(rec, req)
		requireStatus(t, rec, http.StatusBadRequest)
		assertInvalidParam(t, rec, "market_type")
	})
}

// TestMarketTypeFilterIsOrderIndependent guards the cursor fingerprint.
//
// The filter feeds a hash, so `?market_type=total&market_type=spread` and the
// reverse must produce the same scope. Without the sort, a client that reordered
// its own parameters between pages would be told its cursor belongs to a
// different query.
func TestMarketTypeFilterIsOrderIndependent(t *testing.T) {
	t.Parallel()

	bad := &badParams{}
	a := parseMarketTypes(map[string][]string{"market_type": {"total", "spread"}}, bad)
	b := parseMarketTypes(map[string][]string{"market_type": {"spread", "total"}}, bad)
	if bad.any() {
		t.Fatalf("unexpected parameter errors: %+v", bad.items)
	}
	if strings.Join(stringsOfMarketTypes(a), ",") != strings.Join(stringsOfMarketTypes(b), ",") {
		t.Errorf("%v and %v are not the same filter", a, b)
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// assertInvalidParam checks that a 400 or 422 NAMES the offending parameter.
//
// The name is the actionable half of the envelope: a client that is told only
// "one or more parameters are invalid" has to bisect its own query string.
func assertInvalidParam(t *testing.T, rec *httptest.ResponseRecorder, name string) {
	t.Helper()
	body := decodeJSONBody[gen.Error](t, rec)
	if body.InvalidParams == nil {
		t.Fatalf("no invalid_params in %s", rec.Body.String())
	}
	for _, p := range *body.InvalidParams {
		if p.Name == name {
			return
		}
	}
	t.Errorf("invalid_params does not name %q: %s", name, rec.Body.String())
}
