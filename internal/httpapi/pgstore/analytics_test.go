package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/analytics"
	"github.com/anpl1623/sharpline/internal/analytics/clv"
	"github.com/anpl1623/sharpline/internal/analytics/clv/pgclv"
	analyticspg "github.com/anpl1623/sharpline/internal/analytics/pgstore"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi"
	"github.com/anpl1623/sharpline/internal/httpapi/pgstore"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// The phase-9 analytics data plane, against a REAL TimescaleDB.
//
// # Why this tier, and why it writes through the WRITER rather than with INSERTs
//
// Every signal row here is produced by internal/analytics/pgstore — the adapter
// `pricer` actually runs — and read back through internal/httpapi/pgstore, the
// adapter `api` actually serves. That is the point of the file: a hand-written
// INSERT would prove the reader against a shape the writer might not produce,
// which is exactly the drift the two halves of migration 00009 exist to prevent.
// Writing through the real adapter also puts the schema's CHECK constraints, its
// line rules and its natural-key ON CONFLICT arbiters on the path — none of which
// sqlc can see, because sqlc never contacts a server, and none of which a unit
// test with a fake store can reach.
//
// What only a database can answer, and is answered here:
//
//   - the keyset cursors on ev_signals and steam_signals really paginate: no row
//     served twice, no row skipped, over an ALL-DESC ordering whose tie-breakers
//     are part of the phase-12 contract;
//   - the arbitrage read joins its legs back in one pass and surfaces the leg age
//     and the observed spread that the discipline of decision 5 requires on every
//     reported finding;
//   - the staleness bounds are applied as FILTERS rather than merely stored;
//   - the natural keys really are replay keys — a second write of the same finding
//     corrects the row rather than adding one;
//   - `wager_leg_clv`'s two aggregate exclusions (void, line-moved) are the same
//     two `odds.AggregateCLV` applies, and a user with only excluded rows is
//     ABSENT from the leaderboard rather than present with a zero;
//   - the leaderboard orders on ROI and not on profit.
//
// # No mock data
//
// Every row is written by this test for this test, in an id and time namespace it
// owns. Nothing is seeded and nothing stands in for the pipeline.

// analyticsSetup is one test's private slice of the catalogue plus both adapters.
type analyticsSetup struct {
	fixture

	read  *pgstore.Store
	write *analyticspg.Store
	db    *postgres.DB

	event   domain.EventID
	market  domain.MarketID
	homeSel domain.SelectionID
	awaySel domain.SelectionID

	// book2 is a second book, because half of what this file asserts —
	// cross-book arbitrage, a steam lead and its followers, a per-book +EV
	// filter — is meaningless with one.
	book2 domain.BookID

	// at is this test's own anchor. Every instant below is derived from it, so
	// two tests running in parallel cannot land rows inside each other's windows.
	at time.Time
}

func newAnalyticsSetup(t *testing.T) analyticsSetup {
	t.Helper()

	ctx := t.Context()
	read, db := newStore(t)
	f := seedCatalogue(t, ctx, db)

	write, err := analyticspg.New(db)
	if err != nil {
		t.Fatalf("analytics pgstore: %v", err)
	}

	s := analyticsSetup{fixture: f, read: read, write: write, db: db}
	s.at = f.window.Truncate(time.Second)

	s.event = insertEvent(t, ctx, db, f, "evt_"+f.prefix, s.at.Add(2*time.Hour), "scheduled")
	s.market = insertMarket(t, ctx, db, f, s.event, "moneyline", nil)
	s.homeSel = insertSelection(t, ctx, db, s.market, string(s.market)+".home", "home")
	s.awaySel = insertSelection(t, ctx, db, s.market, string(s.market)+".away", "away")

	s.book2 = mustBookID(t, "book2_"+f.prefix)
	exec(t, ctx, db,
		`INSERT INTO books (id, slug, name, kind, is_reference) VALUES ($1, $2, $3, 'external', FALSE)`,
		s.book2, mustSlug(t, "book2-"+f.prefix), "Book2 "+f.prefix)

	return s
}

// evSignal builds a schema-valid +EV finding. Every derived field is computed
// from the offered price and the fair probability rather than being chosen, so
// the row satisfies migration 00009's identities for the same reason a real
// finding does.
func (s analyticsSetup) evSignal(sel domain.SelectionID, book domain.BookID,
	offered float64, fair float64, observed time.Time,
) analytics.EVSignal {
	ev := fair*offered - 1
	return analytics.EVSignal{
		SchemaVersion:        analytics.SchemaVersion,
		SelectionID:          sel,
		MarketID:             s.market,
		MarketType:           "moneyline",
		LeagueID:             s.leagueID,
		BookID:               book,
		ReferenceBookID:      s.bookID,
		DevigMethod:          odds.MethodShin.String(),
		OfferedDecimal:       odds.Decimal(offered),
		OfferedImplied:       1 / offered,
		Line:                 domain.NoLine(),
		FairProbability:      fair,
		FairDecimal:          odds.Decimal(1 / fair),
		ExpectedValue:        ev,
		ExpectedValuePercent: ev * 100,
		Edge:                 fair - 1/offered,
		EdgePercent:          (fair - 1/offered) * 100,
		Kelly:                ev / (offered - 1),
		FractionalKelly:      ev / (offered - 1) * 0.25,
		KellyFraction:        0.25,
		QuoteObservedAt:      observed,
		QuoteAgeSeconds:      12,
		ThresholdEVPercent:   analytics.DefaultMinEVPercent,
		MaxQuoteAgeSeconds:   analytics.DefaultMaxEVQuoteAge.Seconds(),
		DetectedAt:           observed.Add(time.Second),
	}
}

// TestEVSignalsRankPaginateAndFilter proves the +EV read surface end to end.
//
// The ranking tuple `(ev%, quote_observed_at, selection_id, book_id)` is a
// cross-language contract, so the assertion is not "sorted by EV" but "this exact
// order", and the pagination assertion is the strict one: walking the cursor must
// yield every row exactly once. A cursor that repeats a row at a tie boundary
// passes a "sorted" check and fails this one.
func TestEVSignalsRankPaginateAndFilter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newAnalyticsSetup(t)

	// Six findings. Four of them tie on expected_value_percent AND on
	// quote_observed_at, which is what forces the last two components of the
	// ranking tuple — selection_id DESC, then book_id DESC — to decide. Without a
	// full tie a cursor short by a component still paginates correctly and the
	// contract goes untested.
	t1 := s.at.Add(time.Minute)
	type row struct {
		sel  domain.SelectionID
		book domain.BookID
		dec  float64
		at   time.Time
	}
	// Ranked order, which is what the reader must produce.
	want := []row{
		{s.homeSel, s.bookID, 2.50, s.at},                     // +25%
		{s.homeSel, s.bookID, 2.20, t1},                       // +10%, tie
		{s.homeSel, s.book2, 2.20, t1},                        // +10%, tie
		{s.awaySel, s.bookID, 2.20, t1},                       // +10%, tie
		{s.awaySel, s.book2, 2.20, t1},                        // +10%, tie
		{s.awaySel, s.book2, 2.04, s.at.Add(2 * time.Minute)}, // +2%
	}
	// Written in a DIFFERENT order on purpose: a reader serving insertion order
	// would otherwise pass this by accident.
	for _, i := range []int{5, 2, 0, 4, 1, 3} {
		r := want[i]
		if err := s.write.RecordEVSignal(ctx, s.evSignal(r.sel, r.book, r.dec, 0.50, r.at)); err != nil {
			t.Fatalf("RecordEVSignal: %v", err)
		}
	}

	base := httpapi.EVSignalQuery{
		LeagueID:      s.leagueID,
		ObservedAfter: s.at.Add(-time.Hour),
		MinEVPercent:  analytics.DefaultMinEVPercent,
		Limit:         2,
	}

	// Walk the cursor to exhaustion and collect what the reader served.
	var got []httpapi.EVSignal
	q := base
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("the cursor did not terminate; it is not advancing")
		}
		p, err := s.read.EVSignals(ctx, q)
		if err != nil {
			t.Fatalf("EVSignals page %d: %v", page, err)
		}
		got = append(got, p.Signals...)
		if !p.HasMore {
			break
		}
		last := p.Signals[len(p.Signals)-1]
		q = base
		q.After = &httpapi.EVSignalKey{
			ExpectedValuePercent: last.ExpectedValuePercent,
			QuoteObservedAt:      last.QuoteObservedAt,
			SelectionID:          last.SelectionID,
			BookID:               last.BookID,
		}
	}

	if len(got) != len(want) {
		t.Fatalf("the cursor served %d rows, want %d: a keyset that repeats or skips at a "+
			"tie boundary is exactly what this asserts", len(got), len(want))
	}
	for i := range got {
		if got[i].SelectionID != want[i].sel || got[i].BookID != want[i].book {
			t.Fatalf("row %d is (%s, %s), want (%s, %s); the ranking tuple is part of the "+
				"phase-12 contract, so this is an ordering defect and not a preference",
				i, got[i].SelectionID, got[i].BookID, want[i].sel, want[i].book)
		}
	}

	t.Run("the book filter is applied in SQL", func(t *testing.T) {
		q := base
		q.Limit = 50
		q.Books = []domain.BookID{s.book2}
		p, err := s.read.EVSignals(ctx, q)
		if err != nil {
			t.Fatalf("EVSignals: %v", err)
		}
		if len(p.Signals) != 3 {
			t.Fatalf("book filter served %d rows, want 3", len(p.Signals))
		}
		for _, sig := range p.Signals {
			if sig.BookID != s.book2 {
				t.Fatalf("book filter served a row from %s", sig.BookID)
			}
		}
	})

	t.Run("the EV floor is applied in SQL", func(t *testing.T) {
		q := base
		q.Limit = 50
		q.MinEVPercent = 9.9
		p, err := s.read.EVSignals(ctx, q)
		if err != nil {
			t.Fatalf("EVSignals: %v", err)
		}
		if len(p.Signals) != 5 {
			t.Fatalf("EV floor served %d rows, want 5 (one at 25%% and four at 10%%)", len(p.Signals))
		}
	})

	t.Run("the lower time bound is applied in SQL", func(t *testing.T) {
		q := base
		q.Limit = 50
		q.ObservedAfter = s.at.Add(90 * time.Second)
		p, err := s.read.EVSignals(ctx, q)
		if err != nil {
			t.Fatalf("EVSignals: %v", err)
		}
		if len(p.Signals) != 1 {
			t.Fatalf("time bound served %d rows, want 1", len(p.Signals))
		}
	})
}

// TestEVSignalNaturalKeyIsAReplayKey asserts that re-deriving a finding CORRECTS
// the row rather than adding one.
//
// It is the property the whole replay story rests on: `ON CONFLICT DO UPDATE` and
// not `DO NOTHING`, keyed on values derived from the input alone. A recomputation
// after a detector fix has to land, and a second copy of the same finding has to
// be impossible.
func TestEVSignalNaturalKeyIsAReplayKey(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newAnalyticsSetup(t)

	first := s.evSignal(s.homeSel, s.bookID, 2.50, 0.50, s.at)
	if err := s.write.RecordEVSignal(ctx, first); err != nil {
		t.Fatalf("RecordEVSignal: %v", err)
	}

	// Same selection, same book, same quote instant — the natural key — with a
	// corrected fair probability. This is what a detector fix replayed over the
	// same record produces.
	corrected := s.evSignal(s.homeSel, s.bookID, 2.50, 0.55, s.at)
	if err := s.write.RecordEVSignal(ctx, corrected); err != nil {
		t.Fatalf("RecordEVSignal (replay): %v", err)
	}

	p, err := s.read.EVSignals(ctx, httpapi.EVSignalQuery{
		LeagueID:      s.leagueID,
		ObservedAfter: s.at.Add(-time.Hour),
		MinEVPercent:  analytics.DefaultMinEVPercent,
		Limit:         50,
	})
	if err != nil {
		t.Fatalf("EVSignals: %v", err)
	}
	if len(p.Signals) != 1 {
		t.Fatalf("the replay produced %d rows, want 1: the natural key is not arbitrating",
			len(p.Signals))
	}
	if got := p.Signals[0].FairProbability; got != 0.55 {
		t.Fatalf("fair probability is %v, want the CORRECTED 0.55; DO NOTHING would have left "+
			"the original and a replay after a fix would be silently discarded", got)
	}
}

// TestArbitrageSignalsCarryTheirLegsAndTheirStalenessEvidence.
//
// Decision 5 of phase 9: the leg age and the observed spread ride on every
// reported finding, and the staleness bound is a declared threshold rather than a
// magic number. Both halves are asserted — that the evidence is served, and that
// the bound is a real filter rather than a stored decoration.
func TestArbitrageSignalsCarryTheirLegsAndTheirStalenessEvidence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newAnalyticsSetup(t)

	legs := []analytics.ArbitrageSignalLeg{
		{
			LegIndex:      0,
			SelectionID:   s.homeSel,
			Role:          "home",
			BookID:        s.bookID,
			DecimalOdds:   odds.Decimal(2.10),
			Line:          domain.NoLine(),
			StakeFraction: 0.5,
			ObservedAt:    s.at,
			AgeSeconds:    40,
		},
		{
			LegIndex:      1,
			SelectionID:   s.awaySel,
			Role:          "away",
			BookID:        s.book2,
			DecimalOdds:   odds.Decimal(2.15),
			Line:          domain.NoLine(),
			StakeFraction: 0.5,
			ObservedAt:    s.at.Add(10 * time.Second),
			AgeSeconds:    30,
		},
	}
	sum := 1/2.10 + 1/2.15
	sig := analytics.ArbitrageSignal{
		SchemaVersion:            analytics.SchemaVersion,
		MarketID:                 s.market,
		MarketType:               "moneyline",
		LeagueID:                 s.leagueID,
		Line:                     domain.NoLine(),
		SelectionCount:           2,
		ImpliedSum:               sum,
		ReturnFraction:           (1 - sum) / sum,
		DistinctBooks:            2,
		ObservedSpreadSeconds:    10,
		OldestLegAgeSeconds:      40,
		ObservedAt:               s.at,
		LegsFingerprint:          analytics.FingerprintArbitrageLegs(legs),
		MaxLegAgeSeconds:         analytics.DefaultMaxArbLegAge.Seconds(),
		MaxObservedSpreadSeconds: analytics.DefaultMaxArbSpread.Seconds(),
		DetectedAt:               s.at.Add(time.Minute),
		Legs:                     legs,
	}
	if err := s.write.RecordArbitrageSignal(ctx, sig); err != nil {
		t.Fatalf("RecordArbitrageSignal: %v", err)
	}

	base := httpapi.ArbitrageQuery{
		LeagueID:          s.leagueID,
		ObservedAfter:     s.at.Add(-time.Hour),
		MaxLegAge:         analytics.DefaultMaxArbLegAge,
		MaxObservedSpread: analytics.DefaultMaxArbSpread,
		MinReturnFraction: analytics.DefaultMinArbReturn,
		MinDistinctBooks:  1,
		Limit:             50,
	}

	found, err := s.read.ArbitrageSignals(ctx, base)
	if err != nil {
		t.Fatalf("ArbitrageSignals: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("read %d findings, want 1", len(found))
	}
	got := found[0]
	if len(got.Legs) != 2 {
		t.Fatalf("the finding carries %d legs, want 2; an arbitrage without its outcome set is "+
			"a number nobody can act on", len(got.Legs))
	}
	if got.Legs[0].Index != 0 || got.Legs[1].Index != 1 {
		t.Fatalf("legs came back out of index order: %d, %d", got.Legs[0].Index, got.Legs[1].Index)
	}
	if got.OldestLegAge != 40*time.Second {
		t.Fatalf("oldest leg age is %s, want 40s: decision 5 requires it on every reported arb",
			got.OldestLegAge)
	}
	if got.ObservedSpread != 10*time.Second {
		t.Fatalf("observed spread is %s, want 10s", got.ObservedSpread)
	}
	// The fingerprint is deliberately NOT on the wire — it is a replay key, not a
	// fact about the market — so what is asserted here is that it did its job: a
	// second write of the same leg set corrects one row rather than adding one.
	if err := s.write.RecordArbitrageSignal(ctx, sig); err != nil {
		t.Fatalf("RecordArbitrageSignal (replay): %v", err)
	}
	again, err := s.read.ArbitrageSignals(ctx, base)
	if err != nil {
		t.Fatalf("ArbitrageSignals: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("the replay produced %d findings, want 1: (market, observed_at, "+
			"legs_fingerprint) is the replay key and it is not arbitrating", len(again))
	}
	if again[0].ID != got.ID {
		t.Fatalf("the replay produced a NEW id (%s, was %s); the legs would then hang off a "+
			"dead parent", again[0].ID, got.ID)
	}
	if len(again[0].Legs) != 2 {
		t.Fatalf("the replay left %d legs, want 2: the leg set is replaced wholesale, so a "+
			"delete that outran its reinsert is visible here", len(again[0].Legs))
	}
	if got.MaxLegAge != analytics.DefaultMaxArbLegAge || got.MaxObservedSpread != analytics.DefaultMaxArbSpread {
		t.Fatalf("the finding reports bounds %s/%s, want the ones it was detected under (%s/%s)",
			got.MaxLegAge, got.MaxObservedSpread,
			analytics.DefaultMaxArbLegAge, analytics.DefaultMaxArbSpread)
	}
	if got.Legs[0].SelectionID != s.homeSel || got.Legs[1].SelectionID != s.awaySel {
		t.Fatalf("legs came back as (%s, %s)", got.Legs[0].SelectionID, got.Legs[1].SelectionID)
	}
	if got.Legs[0].Age != 40*time.Second || got.Legs[1].Age != 30*time.Second {
		t.Fatalf("per-leg ages are %s/%s, want 40s/30s", got.Legs[0].Age, got.Legs[1].Age)
	}

	t.Run("a tightened leg-age bound suppresses it", func(t *testing.T) {
		q := base
		q.MaxLegAge = 39 * time.Second
		out, err := s.read.ArbitrageSignals(ctx, q)
		if err != nil {
			t.Fatalf("ArbitrageSignals: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("a finding whose oldest leg is 40s old survived a 39s bound; the bound is " +
				"stored but not applied, which is the phantom-arbitrage failure decision 5 exists " +
				"to prevent")
		}
	})

	t.Run("a tightened spread bound suppresses it", func(t *testing.T) {
		q := base
		q.MaxObservedSpread = 9 * time.Second
		out, err := s.read.ArbitrageSignals(ctx, q)
		if err != nil {
			t.Fatalf("ArbitrageSignals: %v", err)
		}
		if len(out) != 0 {
			t.Fatal("a finding assembled across a 10s spread survived a 9s bound")
		}
	})

	t.Run("a raised return floor suppresses it", func(t *testing.T) {
		q := base
		q.MinReturnFraction = 0.5
		out, err := s.read.ArbitrageSignals(ctx, q)
		if err != nil {
			t.Fatalf("ArbitrageSignals: %v", err)
		}
		if len(out) != 0 {
			t.Fatal("a 5%-return finding survived a 50% floor")
		}
	})
}

// TestSteamSignalsReadBackByRecency.
//
// Steam is ranked by RECENCY and magnitude is a filter, which is the opposite of
// the +EV surface and is easy to get backwards. The followers array is asserted
// because a database cannot enforce JSON array order: ascending lag is a writer
// obligation, and the only place it can be checked is a round trip.
func TestSteamSignalsReadBackByRecency(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newAnalyticsSetup(t)

	mk := func(sel domain.SelectionID, end time.Time, delta float64) analytics.SteamSignal {
		dir := "shorten"
		if delta < 0 {
			dir = "drift"
		}
		mag := delta
		if mag < 0 {
			mag = -mag
		}
		return analytics.SteamSignal{
			SchemaVersion:                analytics.SchemaVersion,
			MarketID:                     s.market,
			MarketType:                   "moneyline",
			LeagueID:                     s.leagueID,
			SelectionID:                  sel,
			WindowStart:                  end.Add(-3 * time.Minute),
			WindowEnd:                    end,
			WindowSeconds:                180,
			HopSeconds:                   60,
			Direction:                    dir,
			DeltaProbability:             delta,
			MagnitudeProbabilityPoints:   mag,
			VelocityProbabilityPerMinute: delta / 3,
			DevigMethod:                  "none",
			LeadBookID:                   s.bookID,
			LeadMovedAt:                  end.Add(-90 * time.Second),
			Followers: []analytics.SteamFollower{
				{BookID: s.book2, MovedAt: end.Add(-60 * time.Second), LagSeconds: 30, DeltaProbability: delta * 0.9},
			},
			FollowerCount:         1,
			ParticipatingBooks:    2,
			CrossBookCorrelation:  1,
			ThresholdVelocity:     0.05 / 3,
			ThresholdMagnitude:    0.05,
			ThresholdCorrelation:  0.5,
			MinFollowers:          1,
			MaxFollowerLagSeconds: 120,
			DetectedAt:            end.Add(3 * time.Minute),
		}
	}

	// Written oldest-first, on purpose: if the reader were serving insertion order
	// rather than window_end DESC this would pass by accident, so it must not be
	// the order the assertion expects.
	written := []analytics.SteamSignal{
		mk(s.homeSel, s.at.Add(1*time.Minute), 0.08),
		mk(s.awaySel, s.at.Add(2*time.Minute), -0.09),
		mk(s.homeSel, s.at.Add(3*time.Minute), 0.12),
	}
	for _, sig := range written {
		if err := s.write.RecordSteamSignal(ctx, sig); err != nil {
			t.Fatalf("RecordSteamSignal: %v", err)
		}
	}

	base := httpapi.SteamQuery{
		WindowEndAfter:        s.at.Add(-time.Hour),
		MinMagnitude:          0,
		MinParticipatingBooks: 2,
		Limit:                 2,
	}

	var got []httpapi.SteamSignal
	q := base
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("the cursor did not terminate")
		}
		p, err := s.read.SteamSignals(ctx, q)
		if err != nil {
			t.Fatalf("SteamSignals page %d: %v", page, err)
		}
		for _, sig := range p.Signals {
			if sig.MarketID == s.market {
				got = append(got, sig)
			}
		}
		if !p.HasMore {
			break
		}
		last := p.Signals[len(p.Signals)-1]
		q = base
		q.After = &httpapi.SteamSignalKey{
			WindowEnd:   last.WindowEnd,
			MarketID:    last.MarketID,
			SelectionID: last.SelectionID,
		}
	}

	if len(got) != 3 {
		t.Fatalf("the cursor served %d of this test's rows, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].WindowEnd.Before(got[i-1].WindowEnd) {
			t.Fatalf("row %d closed at %s, not strictly before row %d's %s: steam ranks on "+
				"RECENCY, and magnitude is a filter",
				i, got[i].WindowEnd, i-1, got[i-1].WindowEnd)
		}
	}
	if got[0].Direction != httpapi.SteamShorten || got[1].Direction != httpapi.SteamDrift {
		t.Fatalf("directions round-tripped as %q, %q; the sign of the delta and the direction "+
			"must agree, and migration 00009 CHECKs that they do",
			got[0].Direction, got[1].Direction)
	}
	if len(got[0].Followers) != 1 || got[0].Followers[0].BookID != s.book2 {
		t.Fatalf("followers did not round-trip: %+v", got[0].Followers)
	}

	t.Run("the magnitude floor is applied in SQL", func(t *testing.T) {
		q := base
		q.Limit = 50
		q.MinMagnitude = 0.10
		p, err := s.read.SteamSignals(ctx, q)
		if err != nil {
			t.Fatalf("SteamSignals: %v", err)
		}
		n := 0
		for _, sig := range p.Signals {
			if sig.MarketID == s.market {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("magnitude floor served %d of this test's rows, want 1", n)
		}
	})
}

// -----------------------------------------------------------------------------
// CLV and the leaderboard
// -----------------------------------------------------------------------------

// spreadMarket adds a spread market to the setup, because the line-moved case
// cannot be expressed on a moneyline.
//
// `wager_leg_clv.line_moved` is a schema IDENTITY on
// `taken_line IS DISTINCT FROM closing_line`, not a flag a writer may set — which
// is the right design and which means a moneyline row can never carry it. The
// exclusion it drives is one of the two `odds.AggregateCLV` applies, so it has to
// be tested on a market type that has a line to move.
func (s analyticsSetup) spreadMarket(t *testing.T) (domain.MarketID, domain.SelectionID) {
	t.Helper()
	ctx := t.Context()

	line := 2.5
	m := insertMarket(t, ctx, s.db, s.fixture, s.event, "spread", &line)
	sel := insertSelection(t, ctx, s.db, m, string(m)+".home", "home")
	return m, sel
}

// settledWager writes one settled straight wager and its single graded leg, and
// returns the leg in the shape the CLV pass consumes.
//
// The two rows are written together because migration 00006 refuses either alone
// in a meaningful state: `wagers_return_matches_outcome` pins what
// `returned_minor` may be for each status, and `legs_graded_at_iff_graded` pins
// that a non-pending leg carries a grading instant. A fixture that got either
// wrong would fail as a constraint violation rather than as the assertion it was
// written for.
func (s analyticsSetup) settledWager(t *testing.T, user domain.UserID,
	market domain.MarketID, marketType domain.MarketType, sel domain.SelectionID, role string,
	line *float64, stakeMinor, returnedMinor int64, dec float64, legStatus string,
) clv.Leg {
	t.Helper()
	ctx := t.Context()

	tok := token()
	wid := "wgr_" + s.prefix + "_" + tok
	lid := "leg_" + s.prefix + "_" + tok
	placed := s.at
	graded := s.at.Add(time.Hour)

	status := legStatus
	if legStatus == "void" {
		status = "void"
	}
	// potential_payout must cover the stake AND whatever was actually returned,
	// because wagers_return_matches_outcome bounds a win by it.
	payout := int64(float64(stakeMinor) * dec)
	if payout < returnedMinor {
		payout = returnedMinor
	}
	if payout < stakeMinor {
		payout = stakeMinor
	}

	// ONE transaction for both rows. Migration 00006 declares a DEFERRABLE
	// constraint trigger that refuses a wager with no legs at COMMIT, so two
	// autocommitted statements fail on the first — which is the constraint doing
	// exactly its job, and is why the betting service writes the pair atomically
	// too.
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
INSERT INTO wagers (id, user_id, kind, status, stake_minor, accepted_decimal, rounding,
                    potential_payout_minor, potential_profit_minor,
                    returned_minor, net_return_minor, placed_at, transitioned_at)
VALUES ($1, $2, 'straight', $3, $4, $5, 'half_to_even', $6, $7, $8, $9, $10, $11)`,
		wid, user, status, stakeMinor, dec, payout, payout-stakeMinor,
		returnedMinor, returnedMinor-stakeMinor, placed, graded); err != nil {
		t.Fatalf("insert wager: %v", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO legs (id, wager_id, event_id, market_id, market_type, selection_id, role,
                  price_book_id, price_decimal, price_line, price_observed_at, status, graded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		lid, wid, s.event, market, marketType.String(), sel, role,
		s.bookID, dec, line, placed, legStatus, graded); err != nil {
		t.Fatalf("insert leg: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit wager %s: %v", wid, err)
	}

	return clv.Leg{
		LegID:       domain.LegID(lid),
		WagerID:     domain.WagerID(wid),
		UserID:      user,
		EventID:     s.event,
		MarketID:    market,
		MarketType:  marketType,
		SelectionID: sel,
		Book:        s.bookID,
		Decimal:     odds.Decimal(dec),
		ObservedAt:  placed,
		Status:      mustLegStatus(t, legStatus),
		GradedAt:    graded,
	}
}

// writeCLV records a measurement through the adapter `settle` runs, so migration
// 00009's identities are on the path rather than being assumed: `probability_clv`
// is a single subtraction the schema re-derives, `beat_close` is pinned at
// `odds.CLVTieBand`, `line_moved` is an identity on the two line columns, and
// `voided` is an identity on the leg status — a PUSH is not void.
func (s analyticsSetup) writeCLV(t *testing.T, store *pgclv.Store, leg clv.Leg,
	takenFair, closingFair float64, takenLine, closingLine domain.Line,
) {
	t.Helper()

	probCLV := closingFair - takenFair
	pctCLV := probCLV / takenFair * 100
	mag := pctCLV
	if mag < 0 {
		mag = -mag
	}
	_, takenHasLine := takenLine.Value()
	_, closingHasLine := closingLine.Value()
	moved := takenHasLine != closingHasLine
	if takenHasLine && closingHasLine {
		tv, _ := takenLine.Value()
		cv, _ := closingLine.Value()
		moved = tv != cv
	}

	m := clv.Measurement{
		Leg:         leg,
		LeagueID:    s.leagueID,
		ClosingBook: s.bookID,
		DevigMethod: odds.MethodShin,
		Result: odds.CLVResult{
			Market:         leg.MarketID,
			Selection:      leg.SelectionID,
			TakenBook:      s.bookID,
			ClosingBook:    s.bookID,
			Line:           takenLine,
			ClosingLine:    closingLine,
			TakenAt:        leg.ObservedAt,
			ClosedAt:       leg.ObservedAt.Add(30 * time.Minute),
			TakenFair:      odds.Probability(takenFair),
			ClosingFair:    odds.Probability(closingFair),
			TakenPrice:     odds.Decimal(1 / takenFair),
			ClosingPrice:   odds.Decimal(1 / closingFair),
			ProbabilityCLV: probCLV,
			PercentCLV:     pctCLV,
			Beat:           probCLV > odds.CLVTieBand,
			Magnitude:      mag,
			LineMoved:      moved,
		},
		ComputedAt: leg.GradedAt.Add(time.Minute),
	}
	if err := store.WriteLegCLV(ctx(t), m); err != nil {
		t.Fatalf("WriteLegCLV for %s: %v", leg.LegID, err)
	}
}

func ctx(t *testing.T) context.Context { return t.Context() }

// TestCLVAggregateAppliesBothExclusions.
//
// `odds.AggregateCLV` excludes a VOID leg (no action) and a LINE-MOVED leg (the
// two prices are not comparable), and the SQL aggregate has to apply exactly
// those two and no others. A PUSH is deliberately NOT excluded: it had action.
// The counts are reported separately rather than silently dropped, which is what
// makes "this customer's sample is small because most of their spreads moved"
// legible instead of looking like a thin bettor.
func TestCLVAggregateAppliesBothExclusions(t *testing.T) {
	t.Parallel()
	s := newAnalyticsSetup(t)
	c := ctx(t)

	store, err := pgclv.NewStore(s.db)
	if err != nil {
		t.Fatalf("pgclv: %v", err)
	}
	user := insertUser(t, c, s.db)
	spreadMarket, spreadSel := s.spreadMarket(t)

	// Countable: a win that beat the close, and a push that did not.
	won := s.settledWager(t, user, s.market, domain.MarketTypeMoneyline, s.homeSel, "home", nil, 1000, 2000, 2.0, "won")
	s.writeCLV(t, store, won, 0.50, 0.55, domain.NoLine(), domain.NoLine())

	push := s.settledWager(t, user, s.market, domain.MarketTypeMoneyline, s.awaySel, "away", nil, 1000, 1000, 2.0, "push")
	s.writeCLV(t, store, push, 0.50, 0.45, domain.NoLine(), domain.NoLine())

	// Excluded: a void leg.
	line := 2.5
	voided := s.settledWager(t, user, spreadMarket, domain.MarketTypeSpread, spreadSel, "home", &line, 1000, 1000, 2.0, "void")
	s.writeCLV(t, store, voided, 0.50, 0.60, mustLineValue(t, 2.5), mustLineValue(t, 2.5))

	// Excluded: a line move. Same market, a different selection is not available,
	// so a second spread market carries it.
	moveMarket, moveSel := s.spreadMarket(t)
	moved := s.settledWager(t, user, moveMarket, domain.MarketTypeSpread, moveSel, "home", &line, 1000, 0, 2.0, "lost")
	s.writeCLV(t, store, moved, 0.50, 0.60, mustLineValue(t, 2.5), mustLineValue(t, 3.5))

	agg, err := s.read.UserCLVAggregate(c, httpapi.CLVWindowQuery{
		UserID: user, From: s.at.Add(-time.Hour), To: s.at.Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UserCLVAggregate: %v", err)
	}
	if agg.Samples != 4 {
		t.Fatalf("samples = %d, want 4", agg.Samples)
	}
	if agg.Counted != 2 {
		t.Fatalf("counted = %d, want 2 (the win and the push)", agg.Counted)
	}
	if agg.VoidExcluded != 1 {
		t.Fatalf("void_excluded = %d, want 1", agg.VoidExcluded)
	}
	if agg.LineMovedExcluded != 1 {
		t.Fatalf("line_moved_excluded = %d, want 1", agg.LineMovedExcluded)
	}
	if agg.BeatCount != 1 {
		t.Fatalf("beat_count = %d, want 1: only the win closed shorter than it was taken",
			agg.BeatCount)
	}
	if agg.MeanProbabilityCLV == nil {
		t.Fatal("mean probability CLV is null with two countable rows")
	}
	// (+0.05 + −0.05) / 2 = 0, and it must arrive as a MEASURED zero rather than
	// as the null that "no countable sample" produces. The two are different facts
	// and the surface renders them differently.
	if got := *agg.MeanProbabilityCLV; got > 1e-12 || got < -1e-12 {
		t.Fatalf("mean probability CLV = %v, want 0", got)
	}

	t.Run("the display page returns the excluded rows too", func(t *testing.T) {
		p, err := s.read.UserCLV(c, httpapi.CLVQuery{
			UserID: user, GradedFrom: s.at.Add(-time.Hour), Limit: 50,
		})
		if err != nil {
			t.Fatalf("UserCLV: %v", err)
		}
		if len(p.Entries) != 4 {
			t.Fatalf("the display page served %d rows, want all 4; the exclusions belong to the "+
				"AGGREGATE, and a customer who cannot see the row cannot see why it did not count",
				len(p.Entries))
		}
	})

	t.Run("a user with no history reads as honest zeros, not ErrNoRows", func(t *testing.T) {
		empty := insertUser(t, c, s.db)
		agg, err := s.read.UserCLVAggregate(c, httpapi.CLVWindowQuery{
			UserID: empty, From: s.at.Add(-time.Hour), To: s.at.Add(48 * time.Hour),
		})
		if err != nil {
			t.Fatalf("UserCLVAggregate: %v", err)
		}
		if agg.Samples != 0 || agg.Counted != 0 {
			t.Fatalf("an empty history read as %+v", agg)
		}
		if agg.MeanProbabilityCLV != nil || agg.MeanPercentCLV != nil {
			t.Fatal("an empty history produced a mean; a mean over nothing is null, never 0.00%")
		}
	})

	t.Run("the per-league cut counts only countable rows", func(t *testing.T) {
		byLeague, err := s.read.UserCLVByLeague(c, httpapi.CLVWindowQuery{
			UserID: user, From: s.at.Add(-time.Hour), To: s.at.Add(48 * time.Hour),
		})
		if err != nil {
			t.Fatalf("UserCLVByLeague: %v", err)
		}
		if len(byLeague) != 1 {
			t.Fatalf("per-league cut returned %d rows, want 1", len(byLeague))
		}
		if byLeague[0].Counted != 2 {
			t.Fatalf("per-league counted = %d, want 2", byLeague[0].Counted)
		}
	})
}

// TestLeaderboardRanksOnROIAgainstRealRows is the gate item, at the database.
//
// A whale staking two orders of magnitude more and losing must not outrank a
// small, disciplined account — and the reason it must not is that the board ranks
// on RETURN PER UNIT STAKED, which is scale-free, rather than on profit, which is
// not. Asserted here against real `wagers`, real `legs` and real `wager_leg_clv`
// rows so that the ORDER BY, the two sample floors and the inner CLV join are all
// on the path. The unit-level version of this lives in internal/httpapi; this one
// proves the SQL agrees with it.
func TestLeaderboardRanksOnROIAgainstRealRows(t *testing.T) {
	t.Parallel()
	s := newAnalyticsSetup(t)
	c := ctx(t)

	store, err := pgclv.NewStore(s.db)
	if err != nil {
		t.Fatalf("pgclv: %v", err)
	}
	sharp := insertUser(t, c, s.db)
	whale := insertUser(t, c, s.db)
	absent := insertUser(t, c, s.db)

	// Three settled wagers each, at even money. A win returns twice the stake and
	// a loss returns nothing — migration 00006 pins both, so the fixture cannot
	// express a "loss" that returned something.
	//
	//   sharp: 3 x 1,000 staked, two won -> 4,000 returned, +1,000 net, ROI +33.3%
	//   whale: 3 x 500,000 staked, one won -> 1,000,000 returned,
	//                                         -500,000 net, ROI -33.3%
	//
	// The whale therefore stakes 500x and loses 500x what the sharp made, which is
	// exactly the shape a profit ranking orders differently from an ROI one.
	const n = 3
	for i := 0; i < n; i++ {
		sharpReturn := int64(2_000)
		if i == n-1 {
			sharpReturn = 0
		}
		leg := s.settledWager(t, sharp, s.market, domain.MarketTypeMoneyline, s.homeSel, "home",
			nil, 1_000, sharpReturn, 2.0, wonOrLost(sharpReturn))
		s.writeCLV(t, store, leg, 0.50, 0.54, domain.NoLine(), domain.NoLine())

		whaleReturn := int64(0)
		if i == 0 {
			whaleReturn = 1_000_000
		}
		wleg := s.settledWager(t, whale, s.market, domain.MarketTypeMoneyline, s.awaySel, "away",
			nil, 500_000, whaleReturn, 2.0, wonOrLost(whaleReturn))
		s.writeCLV(t, store, wleg, 0.50, 0.48, domain.NoLine(), domain.NoLine())

		// A user with settled wagers but NO countable CLV row: every one of theirs
		// is void, so the inner join must leave them off the board entirely.
		aleg := s.settledWager(t, absent, s.market, domain.MarketTypeMoneyline, s.homeSel, "home",
			nil, 1_000, 1_000, 2.0, "void")
		s.writeCLV(t, store, aleg, 0.50, 0.99, domain.NoLine(), domain.NoLine())
	}

	q := httpapi.LeaderboardQuery{
		Basis:            httpapi.LeaderboardByROI,
		MinSettledWagers: n,
		MinCLVSamples:    n,
		From:             s.at.Add(-time.Hour),
		To:               s.at.Add(48 * time.Hour),
		Limit:            50,
	}
	rows, err := s.read.Leaderboard(c, q)
	if err != nil {
		t.Fatalf("Leaderboard: %v", err)
	}

	var sharpRow, whaleRow *httpapi.LeaderboardEntry
	sharpAt, whaleAt := -1, -1
	for i := range rows {
		switch rows[i].UserID {
		case sharp:
			sharpRow, sharpAt = &rows[i], i
		case whale:
			whaleRow, whaleAt = &rows[i], i
		case absent:
			t.Fatal("a user whose every CLV row is void appeared on the board; the CLV join is " +
				"LEFT where it must be INNER, and a zero sample is being rendered as a zero score")
		}
	}
	if sharpRow == nil || whaleRow == nil {
		t.Fatalf("board is missing a competitor: sharp=%v whale=%v", sharpRow != nil, whaleRow != nil)
	}
	if sharpAt >= whaleAt {
		t.Fatalf("the whale ranked at %d and the sharp at %d", whaleAt, sharpAt)
	}
	if whaleRow.Staked <= sharpRow.Staked {
		t.Fatalf("the fixture is wrong: the whale staked %d and the sharp %d",
			whaleRow.Staked.MinorUnits(), sharpRow.Staked.MinorUnits())
	}
	if whaleRow.NetReturn >= sharpRow.NetReturn {
		t.Fatalf("the fixture is wrong: the whale is not the losing account")
	}
	if sharpRow.ROI <= whaleRow.ROI {
		t.Fatalf("ROI ordering is wrong: sharp %v, whale %v", sharpRow.ROI, whaleRow.ROI)
	}

	t.Run("the sample floor is a real filter", func(t *testing.T) {
		high := q
		high.MinSettledWagers = n + 1
		rows, err := s.read.Leaderboard(c, high)
		if err != nil {
			t.Fatalf("Leaderboard: %v", err)
		}
		for _, r := range rows {
			if r.UserID == sharp || r.UserID == whale {
				t.Fatalf("%s survived a floor above their sample count; one lucky maximum-stake "+
					"bet at the top of the board is the failure this exists to make "+
					"unrepresentable", r.UserID)
			}
		}
	})

	t.Run("the CLV basis reorders the same rows", func(t *testing.T) {
		byCLV := q
		byCLV.Basis = httpapi.LeaderboardByCLV
		rows, err := s.read.Leaderboard(c, byCLV)
		if err != nil {
			t.Fatalf("Leaderboard: %v", err)
		}
		si, wi := -1, -1
		for i := range rows {
			switch rows[i].UserID {
			case sharp:
				si = i
			case whale:
				wi = i
			}
		}
		if si < 0 || wi < 0 {
			t.Fatal("the CLV board dropped a competitor the ROI board carried")
		}
		if si >= wi {
			t.Fatalf("the whale beat the close less often and still ranked above the sharp "+
				"(%d vs %d)", wi, si)
		}
	})
}

// mustLegStatus and mustLineValue go through the domain's own parsers rather than
// converting, for the reason fixtures_test.go gives for the id constructors: the
// domain is the bound the schema's CHECKs mirror, so a bad fixture fails with the
// domain's message rather than as a constraint violation forty lines later.
func mustLegStatus(t *testing.T, s string) domain.LegStatus {
	t.Helper()
	v, err := domain.ParseLegStatus(s)
	if err != nil {
		t.Fatalf("leg status %q: %v", s, err)
	}
	return v
}

func mustLineValue(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("line %v: %v", v, err)
	}
	return l
}

// wonOrLost names the leg status that matches a return, so a fixture cannot
// silently express a status the ledger's own constraints refuse.
func wonOrLost(returned int64) string {
	if returned > 0 {
		return "won"
	}
	return "lost"
}

// TestTheStoreDistinguishesACatalogueRaceFromASchemaDrift.
//
// These are the two constraint failures the signals stage can see, they arrive as
// adjacent SQLSTATEs, and they have opposite meanings — which is why the adapter
// classifies them rather than reporting both as "rejected by the schema".
//
//   - 23503, a FOREIGN KEY: a catalogue parent has not committed yet. `ingest`
//     writes the catalogue and `pricer` only reads it, and nothing orders the two
//     consumers of `odds.normalized` against each other, so a market can be priced,
//     published and read here in the instant before its row lands. It is transient,
//     it is nobody's defect, and internal/analytics cannot pre-validate it because a
//     finding derived from one price.computed record carries no evidence about what
//     has been committed. The phase-9 gate found a cold start logging 109 of these
//     as database errors.
//   - 23514, a CHECK: internal/analytics validates every finding against these same
//     constraints before writing, so one arriving here IS a drift between the
//     detector and migration 00009.
//
// Only a real server produces either. This is the test that keeps the
// classification honest.
func TestTheStoreDistinguishesACatalogueRaceFromASchemaDrift(t *testing.T) {
	t.Parallel()
	s := newAnalyticsSetup(t)
	c := ctx(t)

	t.Run("a finding whose catalogue parent does not exist is a catalogue race", func(t *testing.T) {
		sig := s.evSignal(s.homeSel, s.bookID, 2.50, 0.50, s.at)
		// A league nobody has written. Every foreign key on ev_signals points at
		// the catalogue, so any of them would do; the league is the one furthest
		// from the finding's own identity.
		sig.LeagueID = domain.LeagueID("league_never_written_" + s.prefix)

		err := s.write.RecordEVSignal(c, sig)
		if err == nil {
			t.Fatal("a finding referencing a league that does not exist was accepted")
		}
		if !errorsIs(err, analytics.ErrCatalogueLag) {
			t.Fatalf("error = %v; want one wrapping ErrCatalogueLag. Reporting a referential "+
				"race as a detector bug sends an operator looking for a defect that is not "+
				"there, and it is what a cold start produces by the hundred", err)
		}
	})

	t.Run("a finding the CHECK constraints refuse is NOT a catalogue race", func(t *testing.T) {
		sig := s.evSignal(s.homeSel, s.bookID, 2.50, 0.50, s.at)
		// A moneyline with a line. migrations/00009's ev_signals_line_rule refuses
		// it, and internal/analytics refuses it one layer earlier — so this shape
		// reaching the database is exactly the drift the other message describes.
		sig.Line = mustLineValue(t, 2.5)

		err := s.write.RecordEVSignal(c, sig)
		if err == nil {
			t.Fatal("a moneyline finding carrying a line was accepted")
		}
		if errorsIs(err, analytics.ErrCatalogueLag) {
			t.Fatalf("a CHECK violation was classified as a catalogue race: %v; it would then "+
				"be retried for ever and never reported", err)
		}
	})
}

// errorsIs is errors.Is, named locally so the import list of this file stays the
// one the assertions need.
func errorsIs(err, target error) bool { return errors.Is(err, target) }
