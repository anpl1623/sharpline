package integration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// The sqlc-generated queries, against the real schema.
//
// Compilation already proves the generated code type-checks. What it does not prove
// is that the SQL runs, that the 44 type overrides in sqlc.yaml actually resolve at
// the WIRE level, or that the values survive the trip. All three are runtime
// questions:
//
//	the SQL runs           — sqlc parses migrations/ to type its queries, so a query
//	                         can be internally consistent with the migration text and
//	                         still fail against the built database (a partial index
//	                         predicate that does not match, a view column sqlc typed
//	                         optimistically).
//
//	the overrides resolve  — pgx maps a named type like domain.SelectionID through its
//	                         UNDERLYING kind. That works, and it works silently: if an
//	                         override were dropped, the field would become `string`,
//	                         everything would still compile at every call site that
//	                         only prints it, and the distinct-ID protection phase 1
//	                         built would be gone. So the STATIC types are asserted by
//	                         reflection, not just used.
//
//	money stays integral   — domain.Money is int64 minor units. A column that came
//	                         back as a float would compare equal for small values and
//	                         diverge later, silently.

// TestGeneratedRowTypesAreTheDomainTypes is a reflection assertion, not a value
// assertion, and it is here because a value assertion cannot catch this.
//
// If sqlc.yaml lost an override, `row.ID` would become a `string` holding the same
// characters. Every comparison in every other test in this file would still pass. The
// only thing that would change is that a MarketID could be passed where a SelectionID
// was wanted — the exact substitution phase 1's distinct ID types exist to refuse, and
// the reason the ledger records that the array adapters must be per-type rather than
// one generic `[T ~string]` helper.
func TestGeneratedRowTypesAreTheDomainTypes(t *testing.T) {
	t.Parallel()

	// Field name -> the type it MUST have. Written as strings so the assertion
	// names the drift rather than merely failing to compile somewhere else.
	cases := []struct {
		what   string
		value  any
		fields map[string]string
	}{
		{
			what:  "ListSportsRow",
			value: gen.ListSportsRow{},
			fields: map[string]string{
				"ID":   "domain.SportID",
				"Slug": "domain.Slug",
				"Name": "string",
			},
		},
		{
			what:  "FindLeagueBySlugRow",
			value: gen.FindLeagueBySlugRow{},
			fields: map[string]string{
				"ID":      "domain.LeagueID",
				"SportID": "domain.SportID",
				"Slug":    "domain.Slug",
			},
		},
		{
			what:  "ListSelectionsForMarketsRow",
			value: gen.ListSelectionsForMarketsRow{},
			fields: map[string]string{
				"ID":       "domain.SelectionID",
				"MarketID": "domain.MarketID",
				// Enums stay `string`: the schema stores them as TEXT + CHECK, and
				// the domain enums are uint8, so pgx cannot bridge them. Conversion
				// is the domain's Parse* functions at the boundary. Pinned here so
				// that if someone "improves" this to a domain type, the change is
				// deliberate.
				"MarketType": "string",
				"Role":       "string",
			},
		},
		{
			what:  "LatestPriceForEachBookOnSelectionsRow",
			value: gen.LatestPriceForEachBookOnSelectionsRow{},
			fields: map[string]string{
				"SelectionID": "domain.SelectionID",
				"BookID":      "domain.BookID",
				"DecimalOdds": "odds.Decimal",
				// A nullable line stays pgtype.Float8 rather than becoming a
				// pointer, because pgtype.Float8{Float64, Valid} mirrors
				// domain.Line{value, present} exactly — Valid IS Line.Present().
				"Line":       "pgtype.Float8",
				"ObservedAt": "time.Time",
				"IngestedAt": "time.Time",
			},
		},
		{
			what:  "GetAccountBalanceRow",
			value: gen.GetAccountBalanceRow{},
			fields: map[string]string{
				"AccountKind": "string",
				// A pointer, because the domain type is the point: the house and
				// issuance singletons have no owner, and NULL is how that is said.
				"AccountUserID": "*domain.UserID",
				// int64 minor units. THE assertion in this test.
				"BalanceMinor": "domain.Money",
				"EntryCount":   "int64",
			},
		},
		{
			what:  "InsertPriceParams",
			value: gen.InsertPriceParams{},
			fields: map[string]string{
				"SelectionID": "domain.SelectionID",
				"BookID":      "domain.BookID",
				"DecimalOdds": "odds.Decimal",
			},
		},
		{
			what:  "InsertWagerParams",
			value: gen.InsertWagerParams{},
			fields: map[string]string{
				"ID":                   "domain.WagerID",
				"UserID":               "domain.UserID",
				"StakeMinor":           "domain.Money",
				"PotentialPayoutMinor": "domain.Money",
				"PotentialProfitMinor": "domain.Money",
				"AcceptedDecimal":      "odds.Decimal",
			},
		},
		{
			what:  "InsertLedgerEntryParams",
			value: gen.InsertLedgerEntryParams{},
			fields: map[string]string{
				"TransactionID": "domain.TransactionID",
				"AmountMinor":   "domain.Money",
				"AccountUserID": "*domain.UserID",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			rt := reflect.TypeOf(tc.value)
			for name, wantType := range tc.fields {
				field, ok := rt.FieldByName(name)
				if !ok {
					t.Errorf("%s has no field %s; the generated shape changed", tc.what, name)
					continue
				}
				if got := field.Type.String(); got != wantType {
					t.Errorf("%s.%s is %s, want %s.\n"+
						"A dropped override in sqlc.yaml compiles everywhere and silently removes the "+
						"distinct-type protection: a raw string accepts a MarketID where a SelectionID is wanted.",
						tc.what, name, got, wantType)
				}
			}
		})
	}
}

// TestEveryGeneratedQueryRunsAgainstTheRealSchema drives all twenty, on rows this
// test wrote.
func TestEveryGeneratedQueryRunsAgainstTheRealSchema(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)
	q := gen.New(pool.Pool())

	cat := newCatalogue(t, ctx, conn)
	moneyline := newMoneylineMarket(t, ctx, conn, cat)
	total := newTotalMarket(t, ctx, conn, cat, 47.5)

	// ---- catalogue reads ---------------------------------------------------

	t.Run("ListSports", func(t *testing.T) {
		rows, err := q.ListSports(ctx)
		if err != nil {
			t.Fatalf("ListSports: %v", err)
		}
		found := false
		for _, r := range rows {
			if r.ID == cat.SportID {
				found = true
				if r.Slug != cat.SportSlug {
					t.Errorf("slug is %q, want %q", r.Slug, cat.SportSlug)
				}
				if r.Name != cat.SportName {
					t.Errorf("name is %q, want %q", r.Name, cat.SportName)
				}
			}
		}
		if !found {
			t.Errorf("this test's sport %s is not in ListSports (%d rows)", cat.SportID, len(rows))
		}
	})

	t.Run("ListLeaguesInSport", func(t *testing.T) {
		rows, err := q.ListLeaguesInSport(ctx, cat.SportID)
		if err != nil {
			t.Fatalf("ListLeaguesInSport: %v", err)
		}
		// The sport is this test's own, so it has exactly one league.
		if len(rows) != 1 {
			t.Fatalf("%d leagues in this test's sport, want 1", len(rows))
		}
		if rows[0].ID != cat.LeagueID || rows[0].SportID != cat.SportID {
			t.Errorf("got league %s in sport %s, want %s in %s",
				rows[0].ID, rows[0].SportID, cat.LeagueID, cat.SportID)
		}
	})

	t.Run("FindLeagueBySlug", func(t *testing.T) {
		row, err := q.FindLeagueBySlug(ctx, cat.LeagueSlug)
		if err != nil {
			t.Fatalf("FindLeagueBySlug(%q): %v", cat.LeagueSlug, err)
		}
		if row.ID != cat.LeagueID {
			t.Errorf("got league %s, want %s", row.ID, cat.LeagueID)
		}

		// A slug that exists nowhere must be ErrNoRows, not an empty row: it is a
		// `:one` query and the caller has to distinguish.
		_, err = q.FindLeagueBySlug(ctx, slug(t, uniqueSlug("nope")))
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("FindLeagueBySlug on an unknown slug returned %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("ListBooks", func(t *testing.T) {
		rows, err := q.ListBooks(ctx)
		if err != nil {
			t.Fatalf("ListBooks: %v", err)
		}
		for _, r := range rows {
			if r.ID != cat.BookID {
				continue
			}
			if r.Kind != domain.BookKindExternal.String() {
				t.Errorf("book kind is %q, want %q", r.Kind, domain.BookKindExternal.String())
			}
			if _, err := domain.ParseBookKind(r.Kind); err != nil {
				t.Errorf("the domain cannot parse the stored book kind %q: %v", r.Kind, err)
			}
			if r.IsReference {
				t.Error("book is flagged as the reference book; the fixture inserts FALSE")
			}
			return
		}
		t.Errorf("this test's book %s is not in ListBooks (%d rows)", cat.BookID, len(rows))
	})

	t.Run("ListOpenEventsInLeague", func(t *testing.T) {
		rows, err := q.ListOpenEventsInLeague(ctx, gen.ListOpenEventsInLeagueParams{
			LeagueID: cat.LeagueID,
			RowLimit: 50,
		})
		if err != nil {
			t.Fatalf("ListOpenEventsInLeague: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("%d open events in this test's league, want 1", len(rows))
		}
		got := rows[0]
		if got.ID != cat.EventID {
			t.Errorf("got event %s, want %s", got.ID, cat.EventID)
		}
		if got.Status != domain.EventStatusScheduled.String() {
			t.Errorf("status is %q, want %q", got.Status, domain.EventStatusScheduled.String())
		}
		if !got.ScheduledStart.Equal(cat.Start) {
			t.Errorf("scheduled_start is %s, want %s", got.ScheduledStart, cat.Start)
		}
		// A competitor id was never written, so the pointer must be nil rather
		// than a zero-valued CompetitorID — the distinction between "absent" and
		// "empty" is what the pointer override is for.
		if got.HomeCompetitorID != nil {
			t.Errorf("home_competitor_id is %v, want nil: the fixture writes only names", *got.HomeCompetitorID)
		}
		if !got.HomeCompetitorName.Valid || got.HomeCompetitorName.String != cat.HomeName {
			t.Errorf("home_competitor_name is %+v, want %q", got.HomeCompetitorName, cat.HomeName)
		}
		// The clock and score groups are all-or-nothing and were never set.
		for name, valid := range map[string]bool{
			"clock_period":     got.ClockPeriod.Valid,
			"clock_elapsed_ns": got.ClockElapsedNs.Valid,
			"clock_running":    got.ClockRunning.Valid,
			"score_home":       got.ScoreHome.Valid,
			"score_away":       got.ScoreAway.Valid,
		} {
			if valid {
				t.Errorf("%s came back non-NULL for a scheduled event", name)
			}
		}
	})

	t.Run("ListOpenEventsStartingBefore", func(t *testing.T) {
		rows, err := q.ListOpenEventsStartingBefore(ctx, gen.ListOpenEventsStartingBeforeParams{
			StartingBefore: cat.Start.Add(time.Second),
			RowLimit:       500,
		})
		if err != nil {
			t.Fatalf("ListOpenEventsStartingBefore: %v", err)
		}
		if !containsEvent(rows, cat.EventID) {
			t.Errorf("this test's event %s starting at %s is not in the result (%d rows)",
				cat.EventID, cat.Start, len(rows))
		}

		// And it is excluded by a bound before its start, which is the property the
		// partial index is there to serve.
		before, err := q.ListOpenEventsStartingBefore(ctx, gen.ListOpenEventsStartingBeforeParams{
			StartingBefore: cat.Start.Add(-time.Hour),
			RowLimit:       500,
		})
		if err != nil {
			t.Fatalf("ListOpenEventsStartingBefore (earlier bound): %v", err)
		}
		if containsEvent(before, cat.EventID) {
			t.Error("the event appeared under a bound an hour before its scheduled start")
		}
	})

	t.Run("GetEventWithLeague", func(t *testing.T) {
		row, err := q.GetEventWithLeague(ctx, cat.EventID)
		if err != nil {
			t.Fatalf("GetEventWithLeague: %v", err)
		}
		// The join reaches all the way up to the sport, which is the whole reason
		// the query exists rather than three round trips.
		if row.LeagueID != cat.LeagueID || row.LeagueSlug != cat.LeagueSlug || row.LeagueName != cat.LeagueName {
			t.Errorf("league columns are (%s, %s, %s), want (%s, %s, %s)",
				row.LeagueID, row.LeagueSlug, row.LeagueName,
				cat.LeagueID, cat.LeagueSlug, cat.LeagueName)
		}
		if row.SportID != cat.SportID || row.SportSlug != cat.SportSlug || row.SportName != cat.SportName {
			t.Errorf("sport columns are (%s, %s, %s), want (%s, %s, %s)",
				row.SportID, row.SportSlug, row.SportName,
				cat.SportID, cat.SportSlug, cat.SportName)
		}
		if _, err := domain.ParseEventKind(row.Kind); err != nil {
			t.Errorf("the domain cannot parse the stored event kind %q: %v", row.Kind, err)
		}
		if _, err := domain.ParseEventStatus(row.Status); err != nil {
			t.Errorf("the domain cannot parse the stored event status %q: %v", row.Status, err)
		}
	})

	t.Run("ListMarketsForEvent", func(t *testing.T) {
		rows, err := q.ListMarketsForEvent(ctx, cat.EventID)
		if err != nil {
			t.Fatalf("ListMarketsForEvent: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("%d markets on this test's event, want 2 (moneyline + total)", len(rows))
		}
		byID := map[domain.MarketID]gen.ListMarketsForEventRow{}
		for _, r := range rows {
			byID[r.ID] = r
			if _, err := domain.ParseMarketType(r.Type); err != nil {
				t.Errorf("the domain cannot parse the stored market type %q: %v", r.Type, err)
			}
			if _, err := domain.ParseMarketStatus(r.Status); err != nil {
				t.Errorf("the domain cannot parse the stored market status %q: %v", r.Status, err)
			}
		}

		// The moneyline carries no line, and pgtype.Float8.Valid is exactly
		// domain.Line.Present().
		ml, ok := byID[moneyline.ID]
		if !ok {
			t.Fatalf("the moneyline market %s is missing", moneyline.ID)
		}
		if ml.Line.Valid {
			t.Errorf("the moneyline market came back with line %v; markets_line_rule requires NULL", ml.Line.Float64)
		}
		if line := lineFrom(t, ml.Line); line.Present() {
			t.Errorf("domain.Line from a NULL column reports Present(); got %s", line)
		}

		// The total carries one, and it survives as a float64 exactly.
		tot, ok := byID[total.ID]
		if !ok {
			t.Fatalf("the total market %s is missing", total.ID)
		}
		if !tot.Line.Valid {
			t.Fatal("the total market came back with a NULL line; markets_line_rule requires a positive value")
		}
		if math.Float64bits(tot.Line.Float64) != math.Float64bits(47.5) {
			t.Errorf("total line is %v, want 47.5", tot.Line.Float64)
		}
		line := lineFrom(t, tot.Line)
		if v, present := line.Value(); !present || v != 47.5 {
			t.Errorf("domain.Line from the column is (%v, %v), want (47.5, true)", v, present)
		}
	})

	t.Run("ListSelectionsForMarkets", func(t *testing.T) {
		// The parameter is []string, not []domain.MarketID: sqlc applies a column
		// override to a scalar parameter but an explicitly cast array parameter
		// takes its type from the cast. The ledger asks for per-type adapters in
		// internal/platform/postgres for exactly this; until they exist a caller
		// converts at the call site, which is what this does.
		ids := []string{string(moneyline.ID), string(total.ID)}

		rows, err := q.ListSelectionsForMarkets(ctx, ids)
		if err != nil {
			t.Fatalf("ListSelectionsForMarkets: %v", err)
		}
		if len(rows) != 4 {
			t.Fatalf("%d selections across two two-way markets, want 4", len(rows))
		}
		for _, r := range rows {
			if r.MarketID != moneyline.ID && r.MarketID != total.ID {
				t.Errorf("selection %s belongs to market %s, which was not asked for", r.ID, r.MarketID)
			}
			if _, err := domain.ParseSelectionRole(r.Role); err != nil {
				t.Errorf("the domain cannot parse the stored role %q: %v", r.Role, err)
			}
		}

		// An empty set must return no rows rather than every row. `= ANY(ARRAY[])`
		// is false for everything, which is the correct behaviour and the opposite
		// of what a naive `IN ()` string-built query would do.
		empty, err := q.ListSelectionsForMarkets(ctx, []string{})
		if err != nil {
			t.Fatalf("ListSelectionsForMarkets on an empty set: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("an empty market set returned %d selections", len(empty))
		}
	})

	t.Run("SearchOpenEventsByCompetitorPrefix", func(t *testing.T) {
		// The FULL home name, not a truncation of it.
		//
		// A truncated prefix is not unique even though the names are: tokens are
		// fixed-width, so "Home 000031" is nobody else's name — but "Home 00003"
		// is a prefix of "Home 000030" through "Home 000039", which other tests
		// own. Using the whole name is what makes the count below an exact
		// assertion; the shorter-prefix case is covered separately, by containment.
		prefix := cat.HomeName

		rows, err := q.SearchOpenEventsByCompetitorPrefix(ctx, gen.SearchOpenEventsByCompetitorPrefixParams{
			Prefix:   prefix,
			RowLimit: 50,
		})
		if err != nil {
			t.Fatalf("SearchOpenEventsByCompetitorPrefix: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != cat.EventID {
			t.Fatalf("prefix %q matched %d events, want this test's one (%s)", prefix, len(rows), cat.EventID)
		}

		// A genuinely PARTIAL prefix — the name minus its last character — must
		// still find this event. It may find other tests' events too, so this is a
		// containment assertion: what is being tested is that the match is a
		// prefix match and not an equality match.
		partial := cat.HomeName[:len(cat.HomeName)-1]
		partialRows, err := q.SearchOpenEventsByCompetitorPrefix(ctx, gen.SearchOpenEventsByCompetitorPrefixParams{
			Prefix:   partial,
			RowLimit: 200,
		})
		if err != nil {
			t.Fatalf("SearchOpenEventsByCompetitorPrefix (partial): %v", err)
		}
		if !containsSearchResult(partialRows, cat.EventID) {
			t.Errorf("the partial prefix %q did not match this event; the query is matching on equality "+
				"rather than on a prefix", partial)
		}

		// Case-insensitive: the query lowercases both sides so the LIKE stays
		// indexable regardless of collation.
		upper, err := q.SearchOpenEventsByCompetitorPrefix(ctx, gen.SearchOpenEventsByCompetitorPrefixParams{
			Prefix:   strings.ToUpper(prefix),
			RowLimit: 50,
		})
		if err != nil {
			t.Fatalf("SearchOpenEventsByCompetitorPrefix (upper): %v", err)
		}
		if len(upper) != 1 {
			t.Errorf("an upper-cased prefix matched %d events, want 1", len(upper))
		}

		// The away name matches too — that is what the UNION is for.
		away, err := q.SearchOpenEventsByCompetitorPrefix(ctx, gen.SearchOpenEventsByCompetitorPrefixParams{
			Prefix:   cat.AwayName,
			RowLimit: 50,
		})
		if err != nil {
			t.Fatalf("SearchOpenEventsByCompetitorPrefix (away): %v", err)
		}
		if len(away) != 1 {
			t.Errorf("the away competitor's name matched %d events, want 1", len(away))
		}

		// STATUS PREDICATE — the phase-2 tripwire, now inverted.
		//
		// Phase 2 recorded that this query carried NO status predicate despite its
		// name, and left an assertion saying "If that was a deliberate fix, delete
		// this assertion." Phase 3 made the deliberate fix: queries/catalogue.sql
		// now spells out `status IN ('scheduled', 'live')` on both arms of the
		// UNION, and its own comment states that this assertion inverts — a
		// cancelled event must return ZERO rows.
		//
		// The set is deliberately NARROWER than ListOpenEventsInLeague's
		// ('scheduled','live','suspended'). Those queries populate the board, which
		// shows a suspended market greyed out; this one answers "what can I bet on",
		// and Market.AcceptsWagers refuses a suspended market at the slip. Both
		// halves of that decision are asserted below so a later widening of either
		// literal has to come here and say so.
		for _, tc := range []struct {
			status string
			want   int
			why    string
		}{
			{"cancelled", 0, "a cancelled event can never be bet on"},
			{"suspended", 0, "a suspended market is refused at the slip by Market.AcceptsWagers"},
			{"settled", 0, "a settled event is over"},
			{"live", 1, "a live event is tradeable and must stay searchable"},
		} {
			if _, err := conn.Exec(ctx,
				`UPDATE events SET status = $2 WHERE id = $1`, cat.EventID, tc.status); err != nil {
				t.Fatalf("set event status to %q: %v", tc.status, err)
			}

			got, err := q.SearchOpenEventsByCompetitorPrefix(ctx, gen.SearchOpenEventsByCompetitorPrefixParams{
				Prefix:   prefix,
				RowLimit: 50,
			})
			if err != nil {
				t.Fatalf("SearchOpenEventsByCompetitorPrefix (%s): %v", tc.status, err)
			}
			if len(got) != tc.want {
				t.Errorf("a %q event matched %d rows, want %d — %s. The status predicate in "+
					"queries/catalogue.sql and this table disagree; change both together.",
					tc.status, len(got), tc.want, tc.why)
			}
		}

		if _, err := conn.Exec(ctx,
			`UPDATE events SET status = 'scheduled' WHERE id = $1`, cat.EventID); err != nil {
			t.Fatalf("restore event status: %v", err)
		}
	})

	// ---- prices ------------------------------------------------------------

	t.Run("InsertPrice and the two price reads", func(t *testing.T) {
		base := uniqueTimeWindow()

		// odds.Decimal carries a float64. A value with a full mantissa is used so
		// the comparison below is bit-exactness rather than two short decimals
		// happening to agree.
		want := []odds.Decimal{}
		for i := range 5 {
			d, err := odds.NewDecimal(1.8300000000000003 + float64(i)/256)
			if err != nil {
				t.Fatalf("NewDecimal: %v", err)
			}
			want = append(want, d)

			if err := q.InsertPrice(ctx, gen.InsertPriceParams{
				SelectionID: moneyline.HomeSelection,
				BookID:      cat.BookID,
				DecimalOdds: d,
				ObservedAt:  base.Add(time.Duration(i) * time.Minute),
				IngestedAt:  base.Add(time.Duration(i)*time.Minute + 90*time.Millisecond),
			}); err != nil {
				t.Fatalf("InsertPrice %d: %v", i, err)
			}
		}

		// ON CONFLICT DO NOTHING is the at-least-once idempotency guard, not
		// defensive padding: `ingest` consumes from Kafka and a rebalance or a
		// deliberate replay WILL redeliver a record whose natural key is already
		// stored. A replay must be a no-op, and it must not overwrite.
		replay, err := odds.NewDecimal(9.5)
		if err != nil {
			t.Fatalf("NewDecimal: %v", err)
		}
		if err := q.InsertPrice(ctx, gen.InsertPriceParams{
			SelectionID: moneyline.HomeSelection,
			BookID:      cat.BookID,
			DecimalOdds: replay,
			ObservedAt:  base, // the same natural key as the first insert
			IngestedAt:  base,
		}); err != nil {
			t.Fatalf("a replayed InsertPrice returned an error instead of doing nothing: %v", err)
		}

		history, err := q.ListPriceHistoryForSelectionAtBook(ctx, gen.ListPriceHistoryForSelectionAtBookParams{
			SelectionID:   moneyline.HomeSelection,
			BookID:        cat.BookID,
			FromInclusive: base,
			ToExclusive:   base.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("ListPriceHistoryForSelectionAtBook: %v", err)
		}
		if len(history) != len(want) {
			t.Fatalf("%d rows of history, want %d: the replayed insert created a row instead of doing nothing",
				len(history), len(want))
		}
		for i, row := range history {
			if math.Float64bits(float64(row.DecimalOdds)) != math.Float64bits(float64(want[i])) {
				t.Errorf("history[%d] decimal_odds is %v, want %v (bit-exact)", i, row.DecimalOdds, want[i])
			}
			if row.SelectionID != moneyline.HomeSelection {
				t.Errorf("history[%d] selection is %s, want %s", i, row.SelectionID, moneyline.HomeSelection)
			}
			if row.Line.Valid {
				t.Errorf("history[%d] came back with a line; the fixture writes NULL", i)
			}
			if !row.IngestedAt.After(row.ObservedAt) {
				t.Errorf("history[%d] ingested_at %s is not after observed_at %s; the two columns were swapped",
					i, row.IngestedAt, row.ObservedAt)
			}
		}
		// The FIRST row must still hold the ORIGINAL odds, not the replay's 9.5.
		if math.Float64bits(float64(history[0].DecimalOdds)) != math.Float64bits(float64(want[0])) {
			t.Errorf("the replayed insert overwrote the stored price: %v, want %v",
				history[0].DecimalOdds, want[0])
		}

		latest, err := q.LatestPriceForEachBookOnSelections(ctx, gen.LatestPriceForEachBookOnSelectionsParams{
			SelectionIDs:  []string{string(moneyline.HomeSelection)},
			ObservedAfter: base.Add(-time.Second),
		})
		if err != nil {
			t.Fatalf("LatestPriceForEachBookOnSelections: %v", err)
		}
		if len(latest) != 1 {
			t.Fatalf("%d current lines for one selection at one book, want 1", len(latest))
		}
		// DISTINCT ON ... ORDER BY observed_at DESC: the newest quote wins.
		if math.Float64bits(float64(latest[0].DecimalOdds)) != math.Float64bits(float64(want[len(want)-1])) {
			t.Errorf("the current line is %v, want the most recent quote %v",
				latest[0].DecimalOdds, want[len(want)-1])
		}

		// The staleness horizon is a real filter, not decoration: a bound after
		// every quote returns nothing.
		stale, err := q.LatestPriceForEachBookOnSelections(ctx, gen.LatestPriceForEachBookOnSelectionsParams{
			SelectionIDs:  []string{string(moneyline.HomeSelection)},
			ObservedAfter: base.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("LatestPriceForEachBookOnSelections (stale horizon): %v", err)
		}
		if len(stale) != 0 {
			t.Errorf("%d rows came back past the staleness horizon; observed_after is not being applied", len(stale))
		}
	})

	// ---- betting writes ----------------------------------------------------

	t.Run("the seven betting inserts", func(t *testing.T) {
		user := newUser(t, ctx, conn)
		stake := domain.Money(2_000)
		acceptedFirst, err := odds.NewDecimal(2.0500000000000003)
		if err != nil {
			t.Fatalf("NewDecimal: %v", err)
		}
		acceptedSecond, err := odds.NewDecimal(1.9500000000000002)
		if err != nil {
			t.Fatalf("NewDecimal: %v", err)
		}
		combined, err := odds.NewDecimal(float64(acceptedFirst) * float64(acceptedSecond))
		if err != nil {
			t.Fatalf("NewDecimal: %v", err)
		}
		payout, profit := payoutFor(t, stake, float64(combined))

		rr := roundRobinID(t, uniqueID("rr"))
		wager := wagerID(t, uniqueID("wager"))
		placed := time.Now().UTC().Truncate(time.Microsecond)

		// A round robin, its declared combination size, a wager drawn from it and
		// its two legs — all in ONE transaction, because wagers_shape_at_commit is
		// deferred to COMMIT precisely so a ticket can be assembled statement by
		// statement.
		if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			qtx := q.WithTx(tx)

			if err := qtx.InsertRoundRobin(ctx, gen.InsertRoundRobinParams{
				ID:                       rr,
				UserID:                   user,
				SelectionCount:           3,
				StakePerCombinationMinor: stake,
				PlacedAt:                 placed,
			}); err != nil {
				return err
			}
			if err := qtx.InsertRoundRobinSize(ctx, gen.InsertRoundRobinSizeParams{
				RoundRobinID:   rr,
				SelectionCount: 3,
				Size:           2,
			}); err != nil {
				return err
			}
			if err := qtx.InsertWager(ctx, gen.InsertWagerParams{
				ID:                   wager,
				UserID:               user,
				Kind:                 domain.WagerKindRoundRobin.String(),
				Status:               domain.WagerStatusPlaced.String(),
				StakeMinor:           stake,
				AcceptedDecimal:      combined,
				Rounding:             domain.RoundHalfAwayFromZero.String(),
				PotentialPayoutMinor: payout,
				PotentialProfitMinor: profit,
				RoundRobinID:         &rr,
				PlacedAt:             placed,
				TransitionedAt:       placed,
			}); err != nil {
				return err
			}
			for i, leg := range []struct {
				market    market
				selection domain.SelectionID
				role      string
				price     odds.Decimal
				line      *float64
			}{
				{moneyline, moneyline.HomeSelection, moneyline.HomeRole, acceptedFirst, nil},
				{total, total.HomeSelection, total.HomeRole, acceptedSecond, float64Ptr(47.5)},
			} {
				if err := qtx.InsertWagerLeg(ctx, gen.InsertWagerLegParams{
					ID:              legID(t, uniqueID("leg")),
					WagerID:         wager,
					EventID:         cat.EventID,
					MarketID:        leg.market.ID,
					MarketType:      leg.market.Type,
					SelectionID:     leg.selection,
					Role:            leg.role,
					PriceBookID:     cat.BookID,
					PriceDecimal:    leg.price,
					PriceLine:       float8From(leg.line),
					PriceObservedAt: placed,
					Status:          domain.LegStatusPending.String(),
				}); err != nil {
					return fmt.Errorf("leg %d: %w", i, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("write the round robin ticket: %v", err)
		}

		// A stake movement against that wager: cash is debited, escrow credited.
		// 'stake' is one of the kinds ledger_transactions_wager_matches_kind
		// REQUIRES a wager for, which is why it comes after the wager exists.
		txn := transactionID(t, uniqueID("txn"))
		debit, err := stake.Neg()
		if err != nil {
			t.Fatalf("negate %s: %v", stake, err)
		}
		if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			qtx := q.WithTx(tx)
			if err := qtx.InsertLedgerTransaction(ctx, gen.InsertLedgerTransactionParams{
				ID:         txn,
				Kind:       domain.EntryKindStake.String(),
				WagerID:    &wager,
				OccurredAt: placed,
			}); err != nil {
				return err
			}
			for i, e := range []gen.InsertLedgerEntryParams{
				{
					TransactionID: txn, EntryIndex: 0,
					AccountKind: domain.AccountKindUserCash.String(), AccountUserID: &user,
					AmountMinor: debit, Kind: domain.EntryKindStake.String(), OccurredAt: placed,
				},
				{
					TransactionID: txn, EntryIndex: 1,
					AccountKind: domain.AccountKindUserEscrow.String(), AccountUserID: &user,
					AmountMinor: stake, Kind: domain.EntryKindStake.String(), OccurredAt: placed,
				},
			} {
				if err := qtx.InsertLedgerEntry(ctx, e); err != nil {
					return fmt.Errorf("entry %d: %w", i, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("write the stake movement: %v", err)
		}

		// GetAccountBalance, and the assertion that money is integral minor units.
		cash, err := q.GetAccountBalance(ctx, gen.GetAccountBalanceParams{
			AccountKind:   domain.AccountKindUserCash.String(),
			AccountUserID: &user,
		})
		if err != nil {
			t.Fatalf("GetAccountBalance(user_cash): %v", err)
		}
		if cash.BalanceMinor != debit {
			t.Errorf("cash balance is %s, want %s", cash.BalanceMinor, debit)
		}
		if cash.BalanceMinor.MinorUnits() != -stake.MinorUnits() {
			t.Errorf("cash balance in minor units is %d, want %d",
				cash.BalanceMinor.MinorUnits(), -stake.MinorUnits())
		}
		if cash.AccountUserID == nil || *cash.AccountUserID != user {
			t.Errorf("the balance row's owner is %v, want %s", cash.AccountUserID, user)
		}

		escrow, err := q.GetAccountBalance(ctx, gen.GetAccountBalanceParams{
			AccountKind:   domain.AccountKindUserEscrow.String(),
			AccountUserID: &user,
		})
		if err != nil {
			t.Fatalf("GetAccountBalance(user_escrow): %v", err)
		}
		if escrow.BalanceMinor != stake {
			t.Errorf("escrow balance is %s, want %s", escrow.BalanceMinor, stake)
		}

		// The two halves sum to zero, which is the invariant the whole design rests
		// on and is asserted here through the generated read rather than raw SQL.
		total, err := cash.BalanceMinor.Add(escrow.BalanceMinor)
		if err != nil {
			t.Fatalf("sum the two balances: %v", err)
		}
		if !total.IsZero() {
			t.Errorf("cash %s + escrow %s = %s, want zero", cash.BalanceMinor, escrow.BalanceMinor, total)
		}
	})
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// newTotalMarket inserts a total market with over and under selections. A total's
// line must be strictly positive (markets_line_rule) and its selections must be
// over/under (selections_role_allowed).
func newTotalMarket(t *testing.T, ctx context.Context, x execer, c catalogue, line float64) market {
	t.Helper()

	m := market{
		ID:            marketID(t, uniqueID("market")),
		Type:          domain.MarketTypeTotal.String(),
		Line:          line,
		HomeSelection: selectionID(t, uniqueID("sel")),
		AwaySelection: selectionID(t, uniqueID("sel")),
		HomeRole:      domain.SelectionRoleOver.String(),
		AwayRole:      domain.SelectionRoleUnder.String(),
	}

	mustExec(t, ctx, x, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, $4, NULL, 'open', $5)`,
		m.ID, c.EventID, m.Type, line, time.Now().UTC())

	for _, s := range []struct {
		id   domain.SelectionID
		role string
	}{
		{m.HomeSelection, m.HomeRole},
		{m.AwaySelection, m.AwayRole},
	} {
		mustExec(t, ctx, x, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, $3, $4, $5)`, s.id, m.ID, m.Type, s.role, s.role+" "+nextToken())
	}
	return m
}

// lineFrom converts a nullable line column into domain.Line, which is the conversion
// every caller of these queries has to perform: domain.Line has unexported fields, so
// pgx cannot construct one, and pgtype.Float8.Valid is exactly Line.Present().
func lineFrom(t *testing.T, v pgtype.Float8) domain.Line {
	t.Helper()
	if !v.Valid {
		return domain.NoLine()
	}
	line, err := domain.NewLine(v.Float64)
	if err != nil {
		t.Fatalf("NewLine(%v): %v", v.Float64, err)
	}
	return line
}

func float8From(v *float64) pgtype.Float8 {
	if v == nil {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: *v, Valid: true}
}

func float64Ptr(v float64) *float64 { return &v }

func containsEvent(rows []gen.ListOpenEventsStartingBeforeRow, id domain.EventID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

func containsSearchResult(rows []gen.SearchOpenEventsByCompetitorPrefixRow, id domain.EventID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
