package integration

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// Enum round-tripping: every domain enum value written from Go, read back, and
// parsed back into the domain type.
//
// # Why this is worth a whole file
//
// Phase 2a made a decision with a consequence: the enums are TEXT with a named
// CHECK, not native ENUM types, because a native enum has no DROP VALUE and
// CLAUDE.md §12 requires every migration to be reversible. So there are ZERO
// user-defined types in the database, sqlc generates `string` for every one of these
// columns, and the correspondence between a Go constant and a database value is
// maintained by nothing but agreement between two files.
//
// A mismatch in that agreement is the worst failure mode available here, because it
// is SILENT IN BOTH DIRECTIONS. Write a value the CHECK does not list and the INSERT
// fails loudly — fine. But rename a String() case and the write still succeeds
// against a CHECK that happens to list both spellings, or a Parse of a stored value
// returns the zero enum, and a settled wager becomes a wager with an unknown status.
//
// So this asserts the full cycle for every value of every enum:
//
//	domain constant -> String() -> INSERT -> SELECT -> Parse* -> domain constant
//
// and separately asserts that 'unknown' — the invalid zero value every one of these
// enums has — is NOT STORABLE. That second half matters as much as the first: phase
// 2a's rule is that a column meaning "not yet known" is NULL, never the string
// "unknown", and a CHECK that admitted 'unknown' would let a zero-valued Go enum
// reach disk looking like data.

// enumCase is one enum's complete value set plus the column it lives in.
type enumCase struct {
	// name is the domain type, for the subtest name.
	name string
	// values is every VALID value. The invalid zero value is deliberately excluded
	// here and covered by TestEveryEnumCheckConstraintListsExactlyTheDomainsValues
	// and TestTheDatabaseRejectsTheInvalidZeroValueOnAWrite instead — a writer
	// whose whole job is to succeed cannot also be the one that proves a rejection.
	values []string
	// parse turns a stored string back into the domain type and reports whether it
	// round-tripped to the same value. It returns the re-rendered string so a
	// failure can show what the parse produced.
	parse func(string) (string, error)
	// write stores one value and returns what the database gives back.
	write func(t *testing.T, ctx context.Context, db *database, value string) string
}

func TestEveryDomainEnumRoundTripsThroughItsColumn(t *testing.T) {
	t.Parallel()

	db := sharedDatabase(t)

	for _, ec := range enumCases() {
		t.Run(ec.name, func(t *testing.T) {
			t.Parallel()

			if len(ec.values) == 0 {
				t.Fatal("no values declared; an enum with no cases is a test that asserts nothing")
			}

			for _, value := range ec.values {
				t.Run(value, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()

					got := ec.write(t, ctx, db, value)
					if got != value {
						t.Fatalf("wrote %q, read back %q", value, got)
					}
					if ec.parse == nil {
						// domain.Rounding has String() and Valid() but NO
						// ParseRounding, so this direction cannot be closed for it
						// yet. See TestRoundingHasNoParseFunction, which pins the
						// gap rather than letting it pass unnoticed.
						return
					}
					reparsed, err := ec.parse(got)
					if err != nil {
						t.Fatalf("the database returned %q and the domain cannot parse it: %v\n"+
							"The CHECK constraint and the Go enum have diverged, which is silent data corruption: "+
							"a stored value the domain rejects becomes an unknown-valued entity at read time.", got, err)
					}
					if reparsed != value {
						t.Errorf("%q round-tripped to %q: Parse and String disagree", value, reparsed)
					}
				})
			}
		})
	}
}

// TestEveryEnumCheckConstraintListsExactlyTheDomainsValues is the strongest form of
// the agreement this file is about, and it runs against the LIVE constraint rather
// than against the migration file.
//
// Phase 2a's handoff says "every enum value was verified against the domain's
// String() switch. 12/12 match. Do not 'helpfully' normalise a value." That
// verification was done by eye, once. This is the same check, mechanical, and in both
// directions — which matters because the two directions fail differently:
//
//	a value the DATABASE admits and the DOMAIN does not know
//	    -> Parse* errors at read time, or worse, a caller that ignores the error
//	       gets the zero enum and a settled wager becomes an unknown-status wager.
//
//	a value the DOMAIN has and the DATABASE refuses
//	    -> every write of that value fails at runtime, in production, on the one
//	       code path nobody exercised.
//
// And 'unknown' must appear in neither: the invalid zero value has no spelling in the
// database, because "a column meaning 'not yet known' is NULL".
func TestEveryEnumCheckConstraintListsExactlyTheDomainsValues(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	conn := rawConn(t, sharedDatabase(t).dsn)

	// Every domain enum's zero value must render as "unknown". Asserted first,
	// because the rest of this test is meaningless if one of them renders as
	// something the CHECK lists.
	for name, rendered := range map[string]string{
		"BookKind":      domain.BookKindUnknown.String(),
		"EventKind":     domain.EventKindUnknown.String(),
		"EventStatus":   domain.EventStatusUnknown.String(),
		"MarketType":    domain.MarketTypeUnknown.String(),
		"MarketStatus":  domain.MarketStatusUnknown.String(),
		"SelectionRole": domain.SelectionRoleUnknown.String(),
		"WagerKind":     domain.WagerKindUnknown.String(),
		"WagerStatus":   domain.WagerStatusUnknown.String(),
		"LegStatus":     domain.LegStatusUnknown.String(),
		"AccountKind":   domain.AccountKindUnknown.String(),
		"EntryKind":     domain.EntryKindUnknown.String(),
		"Rounding":      domain.RoundingUnknown.String(),
	} {
		if rendered != "unknown" {
			t.Errorf("%s's zero value renders as %q, not \"unknown\"", name, rendered)
		}
	}

	// There must be no user-defined types at all. Phase 2a chose TEXT + CHECK over
	// native enums for reversibility (a native enum has no DROP VALUE), and the
	// whole value-set comparison below depends on that choice still holding.
	if n := scalarInt(t, ctx, conn, `
SELECT count(*)
  FROM pg_type t
  JOIN pg_namespace n ON n.oid = t.typnamespace
 WHERE n.nspname = 'public' AND t.typtype = 'e'`); n != 0 {
		t.Errorf("%d native enum types exist in public; phase 2a's decision was TEXT + named CHECK, "+
			"because a native enum cannot DROP VALUE and so cannot be reversed", n)
	}

	for _, ec := range enumConstraints() {
		t.Run(ec.constraint, func(t *testing.T) {
			def, err := constraintDefinition(ctx, conn, ec.constraint)
			if err != nil {
				t.Fatalf("read %s: %v\nIf the constraint was renamed, this test must be updated with it — "+
					"a silently absent CHECK is how a bad enum value gets in.", ec.constraint, err)
			}

			admitted := quotedLiterals(def)
			if len(admitted) == 0 {
				t.Fatalf("%s admits no literal values; its definition is %q", ec.constraint, def)
			}

			missing := setDifference(ec.domainValues, admitted)
			extra := setDifference(admitted, append(append([]string{}, ec.domainValues...), ec.alsoMentions...))

			if len(missing) != 0 {
				t.Errorf("%s.%s: the domain has %v, which %s does NOT admit.\n"+
					"Every write of those values fails at runtime.",
					ec.table, ec.column, missing, ec.constraint)
			}
			if len(extra) != 0 {
				t.Errorf("%s.%s: %s admits %v, which the domain's String() switch does not produce.\n"+
					"A stored value the domain cannot parse is silent data corruption at read time.",
					ec.table, ec.column, ec.constraint, extra)
			}
			for _, v := range admitted {
				if v == "unknown" {
					t.Errorf("%s.%s admits \"unknown\". That is the invalid zero value of every domain "+
						"enum; a column that stores it lets an unset Go field persist as data.",
						ec.table, ec.column)
				}
			}
		})
	}
}

// TestTheDatabaseRejectsTheInvalidZeroValueOnAWrite complements the catalogue check
// with live writes, on the four enum columns whose row shape needs no wager or
// ledger machinery. The catalogue proves the CHECK does not list "unknown"; this
// proves the CHECK is actually attached and enforced.
func TestTheDatabaseRejectsTheInvalidZeroValueOnAWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn := rawConn(t, sharedDatabase(t).dsn)
	cat := newCatalogue(t, ctx, conn)

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "books.kind",
			sql:  `INSERT INTO books (id, slug, name, kind) VALUES ($1, $2, $3, 'unknown')`,
			args: []any{uniqueID("book"), uniqueSlug("book"), "Rejected Book"},
		},
		{
			name: "events.status",
			sql: `INSERT INTO events (id, league_id, kind, name, home_competitor_name,
                                     away_competitor_name, scheduled_start, status, observed_at)
                  VALUES ($1, $2, 'match', 'Rejected Event', 'H', 'A', $3, 'unknown', $3)`,
			args: []any{uniqueID("event"), cat.LeagueID, time.Now().UTC().Add(time.Hour)},
		},
		{
			name: "markets.status",
			sql: `INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
                  VALUES ($1, $2, 'moneyline', NULL, NULL, 'unknown', $3)`,
			args: []any{uniqueID("market"), cat.EventID, time.Now().UTC()},
		},
		{
			name: "selections.role",
			sql: `INSERT INTO selections (id, market_id, market_type, role, name)
                  VALUES ($1, $2, 'moneyline', 'unknown', 'Rejected Selection')`,
			args: nil, // filled in below, it needs a market
		},
	}

	// The selection case needs a real market to reference.
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	cases[3].args = []any{uniqueID("sel"), mkt.ID}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := conn.Exec(ctx, tc.sql, tc.args...)
			if err == nil {
				t.Fatalf("%s accepted the value \"unknown\"", tc.name)
			}
			if got := postgres.SQLState(err); got != "23514" {
				t.Errorf("%s rejected \"unknown\" with SQLSTATE %q, want 23514 (check_violation): %v",
					tc.name, got, err)
			}
		})
	}
}

// enumConstraint pairs a live CHECK constraint with the domain values it must admit.
type enumConstraint struct {
	table        string
	column       string
	constraint   string
	domainValues []string

	// alsoMentions are literals the definition legitimately contains that are NOT
	// values of this enum.
	//
	// It exists for exactly one constraint. selections_role_allowed is not a flat
	// IN list — it is a CASE over market_type, because which roles are legal
	// depends on the market ("total" admits over/under, "futures" admits only
	// outright). Its rendered definition therefore contains the five market types
	// as well as the six roles, and without this field they would be reported as
	// roles the database admits and the domain does not.
	alsoMentions []string
}

// enumConstraints names all twelve, with the constraint names migration 00001's
// convention produces. The domain values come from the enum constants themselves, so
// adding a value to an enum and forgetting the migration fails here rather than in
// production.
func enumConstraints() []enumConstraint {
	byName := map[string][]string{}
	for _, ec := range enumCases() {
		byName[ec.name] = ec.values
	}

	return []enumConstraint{
		{table: "books", column: "kind", constraint: "books_kind_defined", domainValues: byName["BookKind"]},
		{table: "events", column: "kind", constraint: "events_kind_defined", domainValues: byName["EventKind"]},
		{table: "events", column: "status", constraint: "events_status_defined", domainValues: byName["EventStatus"]},
		{table: "markets", column: "type", constraint: "markets_type_defined", domainValues: byName["MarketType"]},
		{table: "markets", column: "status", constraint: "markets_status_defined", domainValues: byName["MarketStatus"]},
		{
			table: "selections", column: "role", constraint: "selections_role_allowed",
			domainValues: byName["SelectionRole"],
			alsoMentions: byName["MarketType"],
		},
		{table: "wagers", column: "kind", constraint: "wagers_kind_defined", domainValues: byName["WagerKind"]},
		{table: "wagers", column: "status", constraint: "wagers_status_defined", domainValues: byName["WagerStatus"]},
		{table: "wagers", column: "rounding", constraint: "wagers_rounding_defined", domainValues: byName["Rounding"]},
		{table: "legs", column: "status", constraint: "legs_status_defined", domainValues: byName["LegStatus"]},
		{
			table: "ledger_entries", column: "account_kind",
			constraint: "ledger_entries_account_kind_defined", domainValues: byName["AccountKind"],
		},
		{
			table: "ledger_entries", column: "kind",
			constraint: "ledger_entries_kind_defined", domainValues: byName["EntryKind"],
		},
	}
}

func constraintDefinition(ctx context.Context, conn *pgx.Conn, name string) (string, error) {
	var def string
	err := conn.QueryRow(ctx, `
SELECT pg_get_constraintdef(con.oid)
  FROM pg_constraint con
  JOIN pg_class c ON c.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND con.conname = $1`, name).Scan(&def)
	if err != nil {
		return "", err
	}
	return def, nil
}

// quotedLiterals pulls the string literals out of a rendered CHECK definition.
//
// Postgres renders an IN list as `col = ANY (ARRAY['a'::text, 'b'::text])`, so the
// admitted values are exactly the single-quoted literals. selections_role_allowed is
// a CASE over market_type whose branches each carry a role list, and the union of its
// literals is still the set of roles it admits — plus the market types it switches
// on, which is why marketTypeLiterals is subtracted below.
func quotedLiterals(def string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(def); i++ {
		if def[i] != '\'' {
			continue
		}
		end := i + 1
		for end < len(def) && def[end] != '\'' {
			end++
		}
		if end >= len(def) {
			break
		}
		v := def[i+1 : end]
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
		i = end
	}
	return out
}

func setDifference(a, b []string) []string {
	in := map[string]bool{}
	for _, v := range b {
		in[v] = true
	}
	var out []string
	for _, v := range a {
		if !in[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// TestRoundingHasNoParseFunction pins a real gap rather than working around it.
//
// wagers.rounding is stored as TEXT and every other enum in the domain has a Parse*
// counterpart to read it back — ParseMarketType, ParseWagerStatus, ParseEntryKind,
// eleven of them. domain.Rounding has String() and Valid() and no ParseRounding, so
// there is currently NO WAY to rehydrate a stored wager's rounding mode into the
// domain type. Phase 8 needs it.
//
// The test asserts the value survives the round trip as a string, which is all that
// can be asserted today, and exists so the missing function is visible in a test run
// instead of only in a handoff note.
func TestRoundingHasNoParseFunction(t *testing.T) {
	t.Parallel()

	for _, r := range []domain.Rounding{
		domain.RoundHalfAwayFromZero,
		domain.RoundHalfToEven,
		domain.RoundTowardZero,
	} {
		if !r.Valid() {
			t.Errorf("%s reports itself invalid", r)
		}
	}
	t.Log("domain.Rounding has no ParseRounding; wagers.rounding can be written and read as a " +
		"string but not rehydrated into the domain type. Every other enum has a Parse* function. " +
		"Additive fix, and the CHECK constraint already fixes the value set.")
}

// -----------------------------------------------------------------------------
// The cases
// -----------------------------------------------------------------------------

func enumCases() []enumCase {
	return []enumCase{
		{
			name: "BookKind",
			values: []string{
				domain.BookKindExternal.String(),
				domain.BookKindSynthetic.String(),
			},
			parse: reparse(domain.ParseBookKind),
			write: writeBookKind,
		},
		{
			name: "EventKind",
			values: []string{
				domain.EventKindMatch.String(),
				domain.EventKindOutright.String(),
			},
			parse: reparse(domain.ParseEventKind),
			write: writeEventKind,
		},
		{
			name: "EventStatus",
			values: []string{
				domain.EventStatusScheduled.String(),
				domain.EventStatusLive.String(),
				domain.EventStatusSuspended.String(),
				domain.EventStatusEnded.String(),
				domain.EventStatusSettled.String(),
				domain.EventStatusPostponed.String(),
				domain.EventStatusCancelled.String(),
			},
			parse: reparse(domain.ParseEventStatus),
			write: writeEventStatus,
		},
		{
			name: "MarketType",
			values: []string{
				domain.MarketTypeMoneyline.String(),
				domain.MarketTypeSpread.String(),
				domain.MarketTypeTotal.String(),
				domain.MarketTypePlayerProp.String(),
				domain.MarketTypeFutures.String(),
			},
			parse: reparse(domain.ParseMarketType),
			write: writeMarketType,
		},
		{
			name: "MarketStatus",
			values: []string{
				domain.MarketStatusOpen.String(),
				domain.MarketStatusSuspended.String(),
				domain.MarketStatusClosed.String(),
				domain.MarketStatusSettled.String(),
				domain.MarketStatusVoided.String(),
			},
			parse: reparse(domain.ParseMarketStatus),
			write: writeMarketStatus,
		},
		{
			name: "SelectionRole",
			values: []string{
				domain.SelectionRoleHome.String(),
				domain.SelectionRoleAway.String(),
				domain.SelectionRoleDraw.String(),
				domain.SelectionRoleOver.String(),
				domain.SelectionRoleUnder.String(),
				domain.SelectionRoleOutright.String(),
			},
			parse: reparse(domain.ParseSelectionRole),
			write: writeSelectionRole,
		},
		{
			name: "WagerKind",
			values: []string{
				domain.WagerKindStraight.String(),
				domain.WagerKindParlay.String(),
				domain.WagerKindRoundRobin.String(),
				domain.WagerKindTeaser.String(),
			},
			parse: reparse(domain.ParseWagerKind),
			write: writeWagerKind,
		},
		{
			name: "WagerStatus",
			values: []string{
				domain.WagerStatusPlaced.String(),
				domain.WagerStatusOpen.String(),
				domain.WagerStatusWon.String(),
				domain.WagerStatusLost.String(),
				domain.WagerStatusVoid.String(),
				domain.WagerStatusPush.String(),
				domain.WagerStatusCashedOut.String(),
			},
			parse: reparse(domain.ParseWagerStatus),
			write: writeWagerStatus,
		},
		{
			name: "LegStatus",
			values: []string{
				domain.LegStatusPending.String(),
				domain.LegStatusWon.String(),
				domain.LegStatusLost.String(),
				domain.LegStatusVoid.String(),
				domain.LegStatusPush.String(),
			},
			parse: reparse(domain.ParseLegStatus),
			write: writeLegStatus,
		},
		{
			name: "AccountKind",
			values: []string{
				domain.AccountKindUserCash.String(),
				domain.AccountKindUserEscrow.String(),
				domain.AccountKindHouse.String(),
				domain.AccountKindIssuance.String(),
			},
			parse: reparse(domain.ParseAccountKind),
			write: writeAccountKind,
		},
		{
			name: "EntryKind",
			values: []string{
				domain.EntryKindGrant.String(),
				domain.EntryKindStake.String(),
				domain.EntryKindPayout.String(),
				domain.EntryKindLoss.String(),
				domain.EntryKindRefund.String(),
				domain.EntryKindCashOut.String(),
				domain.EntryKindAdjustment.String(),
			},
			parse: reparse(domain.ParseEntryKind),
			write: writeEntryKind,
		},
		{
			name: "Rounding",
			values: []string{
				domain.RoundHalfAwayFromZero.String(),
				domain.RoundHalfToEven.String(),
				domain.RoundTowardZero.String(),
			},
			// No ParseRounding exists. See TestRoundingHasNoParseFunction.
			parse: nil,
			write: writeRounding,
		},
	}
}

// reparse adapts a domain Parse* function into the enumCase.parse shape. The
// generic constraint is the two methods every one of these enums has, so a Parse
// function with a drifted signature fails to compile here rather than being skipped.
func reparse[T fmt.Stringer](parse func(string) (T, error)) func(string) (string, error) {
	return func(stored string) (string, error) {
		v, err := parse(stored)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	}
}

// -----------------------------------------------------------------------------
// Writers
// -----------------------------------------------------------------------------
//
// One per enum, each writing a row this call owns and reading the value back out of
// the column it was stored in. They are separate rather than one parameterised
// writer because the surrounding row differs per value: a spread market needs a line
// and a moneyline must not have one; a total's line must be positive; a player prop
// needs a subject; a leg's role must be one the market type permits; a terminal
// wager status requires a returned amount whose value depends on which status it is.
// A single writer would have to encode all of that anyway, in a switch, less
// readably.

func writeBookKind(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()
	conn := rawConn(t, db.dsn)

	id := uniqueID("book")
	mustExec(t, ctx, conn, `
INSERT INTO books (id, slug, name, kind) VALUES ($1, $2, $3, $4)`,
		id, uniqueSlug("book"), "Enum Book "+nextToken(), value)

	return scalarString(t, ctx, conn, `SELECT kind FROM books WHERE id = $1`, id)
}

func writeEventKind(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()
	conn := rawConn(t, db.dsn)
	cat := newCatalogue(t, ctx, conn)

	id := uniqueID("event")
	// events_competitors_match_kind is an exhaustive CASE on kind: 'match' requires
	// both competitor names, 'outright' requires all four competitor columns NULL,
	// and anything else is `ELSE false`. So the competitor columns are chosen from
	// the value under test, which is also what makes 'unknown' unwritable here for
	// two independent reasons.
	if value == domain.EventKindOutright.String() {
		mustExec(t, ctx, conn, `
INSERT INTO events (id, league_id, kind, name, scheduled_start, status, observed_at)
VALUES ($1, $2, $3, $4, $5, 'scheduled', $6)`,
			id, cat.LeagueID, value, "Enum Outright "+nextToken(),
			time.Now().UTC().Add(72*time.Hour), time.Now().UTC())
	} else {
		mustExec(t, ctx, conn, `
INSERT INTO events (id, league_id, kind, name, home_competitor_name, away_competitor_name,
                    scheduled_start, status, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'scheduled', $8)`,
			id, cat.LeagueID, value, "Enum Match "+nextToken(),
			"Home "+nextToken(), "Away "+nextToken(),
			time.Now().UTC().Add(72*time.Hour), time.Now().UTC())
	}

	return scalarString(t, ctx, conn, `SELECT kind FROM events WHERE id = $1`, id)
}

func writeEventStatus(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()
	conn := rawConn(t, db.dsn)
	cat := newCatalogue(t, ctx, conn)

	id := uniqueID("event")
	// The clock and score columns stay NULL for every status. events_clock_only_in_play
	// permits a clock ONLY for live/suspended, and both clock and score are
	// all-or-nothing groups, so NULL is the one choice valid across all seven
	// statuses — which is what keeps this a test of the status column alone.
	mustExec(t, ctx, conn, `
INSERT INTO events (id, league_id, kind, name, home_competitor_name, away_competitor_name,
                    scheduled_start, status, observed_at)
VALUES ($1, $2, 'match', $3, $4, $5, $6, $7, $8)`,
		id, cat.LeagueID, "Enum Status "+nextToken(),
		"Home "+nextToken(), "Away "+nextToken(),
		time.Now().UTC().Add(72*time.Hour), value, time.Now().UTC())

	return scalarString(t, ctx, conn, `SELECT status FROM events WHERE id = $1`, id)
}

// marketShapeFor returns the line and subject a market of the given type requires.
//
// markets_line_rule and markets_subject_matches_type are both exhaustive CASEs on
// `type`, so these are not preferences — a moneyline WITH a line is unwritable and a
// spread WITHOUT one is too.
func marketShapeFor(marketType string) (line *float64, subject *string) {
	switch marketType {
	case domain.MarketTypeSpread.String():
		v := -3.5
		return &v, nil
	case domain.MarketTypeTotal.String():
		// markets_line_rule requires a total's line to be strictly positive.
		v := 47.5
		return &v, nil
	case domain.MarketTypePlayerProp.String():
		v := 27.5
		s := "A. Player"
		return &v, &s
	default:
		// moneyline and futures: line IS NULL, subject IS NULL.
		return nil, nil
	}
}

// marketTypeFor returns a market type that permits the given selection role.
//
// selections_role_allowed is an exhaustive CASE on market_type — a total admits only
// over/under, futures only outright, a moneyline home/away/draw — so the pairing is
// dictated by the schema rather than chosen here.
func marketTypeFor(role string) string {
	switch role {
	case domain.SelectionRoleOver.String(), domain.SelectionRoleUnder.String():
		return domain.MarketTypeTotal.String()
	case domain.SelectionRoleOutright.String():
		return domain.MarketTypeFutures.String()
	default:
		// home, away and draw are all permitted on a moneyline.
		return domain.MarketTypeMoneyline.String()
	}
}

func writeMarketType(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()
	conn := rawConn(t, db.dsn)
	cat := newCatalogue(t, ctx, conn)

	id := uniqueID("market")
	line, subject := marketShapeFor(value)
	mustExec(t, ctx, conn, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, $4, $5, 'open', $6)`,
		id, cat.EventID, value, line, subject, time.Now().UTC())

	return scalarString(t, ctx, conn, `SELECT type FROM markets WHERE id = $1`, id)
}

func writeMarketStatus(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()
	conn := rawConn(t, db.dsn)
	cat := newCatalogue(t, ctx, conn)

	id := uniqueID("market")
	mustExec(t, ctx, conn, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, 'moneyline', NULL, NULL, $3, $4)`,
		id, cat.EventID, value, time.Now().UTC())

	return scalarString(t, ctx, conn, `SELECT status FROM markets WHERE id = $1`, id)
}

func writeSelectionRole(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()
	conn := rawConn(t, db.dsn)
	cat := newCatalogue(t, ctx, conn)

	marketType := marketTypeFor(value)
	line, subject := marketShapeFor(marketType)

	mktID := uniqueID("market")
	mustExec(t, ctx, conn, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, $4, $5, 'open', $6)`,
		mktID, cat.EventID, marketType, line, subject, time.Now().UTC())

	selID := uniqueID("sel")
	mustExec(t, ctx, conn, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, $3, $4, $5)`, selID, mktID, marketType, value, "Enum Selection "+nextToken())

	return scalarString(t, ctx, conn, `SELECT role FROM selections WHERE id = $1`, selID)
}

func writeWagerKind(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()

	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	cat := newCatalogue(t, ctx, conn)
	first := newMoneylineMarket(t, ctx, conn, cat)
	second := newMoneylineMarket(t, ctx, conn, cat)
	spreadA := newSpreadMarket(t, ctx, conn, cat, -3.5)
	spreadB := newSpreadMarket(t, ctx, conn, cat, 6.5)
	user := newUser(t, ctx, conn)

	var id domain.WagerID
	// One transaction, because wagers_shape_at_commit is deferred to COMMIT
	// specifically so a wager and its legs can arrive as separate statements.
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		switch value {
		case domain.WagerKindStraight.String():
			id = newStraightWager(t, ctx, tx, cat, first, user).ID
		case domain.WagerKindParlay.String():
			id = newParlayWager(t, ctx, tx, cat, first, second, user).ID
		case domain.WagerKindTeaser.String():
			id = newTeaserWager(t, ctx, tx, cat, spreadA, spreadB, user).ID
		case domain.WagerKindRoundRobin.String():
			id = newRoundRobinWager(t, ctx, tx, cat, first, second, user).ID
		default:
			// The 'unknown' case: wagers_kind_defined refuses it, and so does
			// wagers_teaser_points_matches_kind's companion CHECK. Written as a
			// straight so only the kind column is at fault.
			w := newStraightWager(t, ctx, tx, cat, first, user)
			id = w.ID
			_, err := tx.Exec(ctx, `UPDATE wagers SET kind = $1 WHERE id = $2`, value, w.ID)
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("write a %q wager: %v", value, err)
	}

	return scalarString(t, ctx, conn, `SELECT kind FROM wagers WHERE id = $1`, id)
}

// returnedFor gives a terminal status a returned amount its own CHECK accepts.
//
// wagers_return_matches_outcome is an exhaustive CASE and every branch is different:
// lost must return exactly 0, void and push exactly the stake, won at least the stake
// and at most the payout, cashed_out strictly positive and at most the payout, and the
// two non-terminal statuses must return NULL.
func returnedFor(t *testing.T, status string, stake, payout domain.Money) (domain.Money, bool) {
	t.Helper()

	switch status {
	case domain.WagerStatusPlaced.String(), domain.WagerStatusOpen.String():
		return 0, false
	case domain.WagerStatusLost.String():
		return domain.Money(0), true
	case domain.WagerStatusVoid.String(), domain.WagerStatusPush.String():
		return stake, true
	case domain.WagerStatusWon.String():
		return payout, true
	case domain.WagerStatusCashedOut.String():
		// Strictly between 0 and the payout: half the stake is the simplest value
		// that is positive and below the cap for every stake this package uses.
		half, _, err := stake.DivMod(2)
		if err != nil {
			t.Fatalf("halve %s: %v", stake, err)
		}
		return half, true
	default:
		// 'unknown' — every branch of the CASE is exhausted, so ELSE false applies
		// and no value of returned_minor can make the row writable. Returning NULL
		// keeps the failure attributable to wagers_status_defined.
		return 0, false
	}
}

func writeWagerStatus(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()

	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)

	var id domain.WagerID
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The straight fixture's stake and payout are fixed, so the returned amount
		// is computed from them rather than guessed.
		probe := domain.Money(10_000)
		payout, _ := payoutFor(t, probe, 2.1500000000000004)

		opts := []wagerOption{}
		if returned, terminal := returnedFor(t, value, probe, payout); terminal {
			opts = append(opts, withStatus(value, returned))
			// A terminal wager's legs are graded. legs_graded_at_iff_graded does
			// not know about the wager's status, so this is consistency for its own
			// sake rather than a constraint — but a settled wager with pending legs
			// is not a state phase 8 should learn from a fixture.
			opts = append(opts, withLegStatus(domain.LegStatusWon.String()))
		} else {
			opts = append(opts, withStatus(value, 0))
			// Undo the returned amount withStatus set: a non-terminal status must
			// have returned_minor IS NULL.
			opts = append(opts, func(w *wagerFixture) { w.returned = nil })
		}

		id = newStraightWager(t, ctx, tx, cat, mkt, user, opts...).ID
		return nil
	}); err != nil {
		t.Fatalf("write a %q wager: %v", value, err)
	}

	return scalarString(t, ctx, conn, `SELECT status FROM wagers WHERE id = $1`, id)
}

func writeLegStatus(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()

	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)

	var id domain.LegID
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		w := newStraightWager(t, ctx, tx, cat, mkt, user, withLegStatus(value))
		id = w.Legs[0].ID
		return nil
	}); err != nil {
		t.Fatalf("write a leg with status %q: %v", value, err)
	}

	return scalarString(t, ctx, conn, `SELECT status FROM legs WHERE id = $1`, id)
}

// counterpartFor returns an account kind that can hold the other half of a movement
// involving the given kind, plus whether that counterpart needs an owner.
//
// ledger_entries_owner_matches_account_kind is an equivalence:
// account_kind IN ('user_cash','user_escrow') exactly when account_user_id IS NOT NULL.
func counterpartFor(kind string) string {
	if kind == domain.AccountKindIssuance.String() {
		return domain.AccountKindHouse.String()
	}
	return domain.AccountKindIssuance.String()
}

func needsOwner(kind string) bool {
	return kind == domain.AccountKindUserCash.String() ||
		kind == domain.AccountKindUserEscrow.String()
}

func writeAccountKind(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()

	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)
	user := newUser(t, ctx, conn)

	txnID := transactionID(t, uniqueID("txn"))
	// 'adjustment' is the only kind whose transaction may carry either a wager or
	// none (ledger_transactions_wager_matches_kind), which keeps this a test of the
	// account_kind column without dragging in a wager fixture.
	kind := domain.EntryKindAdjustment.String()
	occurred := time.Now().UTC().Truncate(time.Microsecond)

	counterpart := counterpartFor(value)
	var owner, counterpartOwner *domain.UserID
	if needsOwner(value) {
		owner = &user
	}
	if needsOwner(counterpart) {
		counterpartOwner = &user
	}

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO ledger_transactions (id, kind, wager_id, occurred_at) VALUES ($1, $2, NULL, $3)`,
			txnID, kind, occurred); err != nil {
			return err
		}
		for i, e := range []struct {
			accountKind string
			owner       *domain.UserID
			amount      int64
		}{
			{value, owner, 4_200},
			{counterpart, counterpartOwner, -4_200},
		} {
			if _, err := tx.Exec(ctx, `
INSERT INTO ledger_entries (transaction_id, entry_index, account_kind, account_user_id,
                            amount_minor, kind, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				txnID, int32(i), e.accountKind, e.owner, e.amount, kind, occurred); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write a movement touching account kind %q: %v", value, err)
	}

	return scalarString(t, ctx, conn,
		`SELECT account_kind FROM ledger_entries WHERE transaction_id = $1 AND entry_index = 0`, txnID)
}

func writeEntryKind(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()

	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)

	// ledger_transactions_wager_matches_kind: 'grant' must NOT carry a wager,
	// 'adjustment' may, and stake/payout/loss/refund/cash_out MUST. So five of the
	// seven kinds need a real wager to point at, and one wager serves them all —
	// nothing constrains wager_id to be unique across transactions.
	var wager *domain.WagerID
	switch value {
	case domain.EntryKindGrant.String(), domain.EntryKindAdjustment.String():
		// no wager
	default:
		if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			w := newStraightWager(t, ctx, tx, cat, mkt, user)
			id := w.ID
			wager = &id
			return nil
		}); err != nil {
			t.Fatalf("write the wager a %q transaction must reference: %v", value, err)
		}
	}

	txnID := transactionID(t, uniqueID("txn"))
	occurred := time.Now().UTC().Truncate(time.Microsecond)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO ledger_transactions (id, kind, wager_id, occurred_at) VALUES ($1, $2, $3, $4)`,
			txnID, value, wager, occurred); err != nil {
			return err
		}
		// kind and occurred_at are repeated on every entry and pinned there by the
		// composite FK (transaction_id, kind, occurred_at), so they cannot drift
		// from the header.
		for i, e := range []struct {
			accountKind string
			owner       *domain.UserID
			amount      int64
		}{
			{domain.AccountKindUserCash.String(), &user, 1_000},
			{domain.AccountKindIssuance.String(), nil, -1_000},
		} {
			if _, err := tx.Exec(ctx, `
INSERT INTO ledger_entries (transaction_id, entry_index, account_kind, account_user_id,
                            amount_minor, kind, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				txnID, int32(i), e.accountKind, e.owner, e.amount, value, occurred); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write a %q transaction: %v", value, err)
	}

	header := scalarString(t, ctx, conn, `SELECT kind FROM ledger_transactions WHERE id = $1`, txnID)
	entry := scalarString(t, ctx, conn,
		`SELECT kind FROM ledger_entries WHERE transaction_id = $1 AND entry_index = 0`, txnID)
	if header != entry {
		t.Fatalf("header kind %q and entry kind %q disagree; the composite FK is supposed to make that impossible",
			header, entry)
	}
	return entry
}

func writeRounding(t *testing.T, ctx context.Context, db *database, value string) string {
	t.Helper()

	pool, _ := connectPool(t, db.dsn)
	conn := rawConn(t, db.dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)

	var id domain.WagerID
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id = newStraightWager(t, ctx, tx, cat, mkt, user, withRounding(value)).ID
		return nil
	}); err != nil {
		t.Fatalf("write a wager with rounding %q: %v", value, err)
	}

	return scalarString(t, ctx, conn, `SELECT rounding FROM wagers WHERE id = $1`, id)
}
