package pgstore_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/betting/pgstore"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// [pgstore.HydrateWager]'s refusals, driven directly rather than through the
// database.
//
// # Why these rows are built in Go and not written to a table
//
// Every case below is a row the SCHEMA MAKES UNSTORABLE — an enum value no CHECK
// admits, a terminal status with a NULL returned amount that
// wagers_return_iff_terminal forbids, a leg whose status and graded_at disagree
// where legs_graded_at_iff_graded is a biconditional. That is exactly why the
// read-side guards exist and exactly why they cannot be reached through an
// INSERT: the database refuses the row before the reader ever sees it.
//
// The guards are not therefore dead code. They are the assertion that the schema
// and the domain still agree, and the day they stop agreeing — a migration that
// adds a status Go does not know, a constraint dropped in a hurry — is the day
// one of these branches is the only thing standing between a divergence and a
// silent zero value in a money record. HydrateWager is exported precisely so
// internal/settlement/pgstore shares one copy of this reconstruction; a test that
// could only reach it through a table could not reach it at all.
//
// This is a test fixture, not mock data. Nothing here is seeded into a database,
// read by application code, or standing in for something the pipeline produced;
// each row is constructed by a case, handed to one pure function, and asserted on
// by the case that built it. The round trips that DO go through Postgres — every
// storable shape — are in store_test.go.

// hydrateAt is a fixed instant, so a failing case reports the same values on
// every run. Truncated to microseconds because that is the precision
// TIMESTAMPTZ stores and the round-trip tests share the constraint.
var hydrateAt = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// storableRow returns a wager row and its single leg row in the shape the
// database would really hand back: a placed straight, one pending moneyline leg,
// the accepted price equal to the leg's quote.
//
// Every refusal case starts from this and breaks exactly one thing, so a case
// that fails is unambiguous about which guard it is asserting.
func storableRow() (gen.GetWagerRow, []gen.ListWagerLegsRow) {
	const decimal = 1.9100000000000001

	wager := gen.GetWagerRow{
		ID:                   "wager_hydrate",
		UserID:               "usr_hydrate",
		Kind:                 domain.WagerKindStraight.String(),
		Status:               domain.WagerStatusPlaced.String(),
		StakeMinor:           domain.Money(10_000),
		AcceptedDecimal:      odds.Decimal(decimal),
		Rounding:             domain.RoundHalfAwayFromZero.String(),
		PotentialPayoutMinor: domain.Money(19_100),
		PotentialProfitMinor: domain.Money(9_100),
		PlacedAt:             hydrateAt,
		TransitionedAt:       hydrateAt,
	}
	legs := []gen.ListWagerLegsRow{{
		ID:              "leg_hydrate",
		WagerID:         wager.ID,
		EventID:         "event_hydrate",
		MarketID:        "market_hydrate",
		MarketType:      domain.MarketTypeMoneyline.String(),
		SelectionID:     "sel_hydrate",
		Role:            domain.SelectionRoleHome.String(),
		PriceBookID:     "book_hydrate",
		PriceDecimal:    odds.Decimal(decimal),
		PriceObservedAt: hydrateAt,
		Status:          domain.LegStatusPending.String(),
	}}
	return wager, legs
}

// TestTheStorableRowHydrates is the control.
//
// Without it every refusal below could pass for the wrong reason — a fixture
// that was malformed in some second way would produce an error whatever the case
// broke, and the table would assert nothing at all.
func TestTheStorableRowHydrates(t *testing.T) {
	t.Parallel()

	wager, legs := storableRow()
	w, err := pgstore.HydrateWager(wager, legs)
	if err != nil {
		t.Fatalf("the baseline row this file's cases mutate does not hydrate: %v", err)
	}
	if w.Status() != domain.WagerStatusPlaced || w.LegCount() != 1 {
		t.Fatalf("baseline hydrated as %s with %d legs, want placed with 1", w.Status(), w.LegCount())
	}
}

// TestHydrateWagerRefusesARowTheSchemaShouldHaveMadeUnstorable.
//
// Each case names the constraint that should have refused the row on the way in,
// because that is the diagnostic: an error from one of these branches means a
// migration and this build no longer agree, and the constraint name is where to
// look.
//
// The alternative — a zero value where the enum did not parse, or a
// dereferenced nil where the schema promised a value — would produce a
// domain.Wager that looks entirely ordinary and is wrong about money.
func TestHydrateWagerRefusesARowTheSchemaShouldHaveMadeUnstorable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		corrupt func(*gen.GetWagerRow, []gen.ListWagerLegsRow)
		wantErr error
		why     string
	}{
		{
			name:    "a wager kind this build does not know",
			corrupt: func(w *gen.GetWagerRow, _ []gen.ListWagerLegsRow) { w.Kind = "system_bet" },
			wantErr: domain.ErrUnknownWagerKind,
			why:     "wagers_kind_defined admits five spellings and this is not one of them",
		},
		{
			name:    "a wager status this build does not know",
			corrupt: func(w *gen.GetWagerRow, _ []gen.ListWagerLegsRow) { w.Status = "settled" },
			wantErr: domain.ErrUnknownWagerStatus,
			why:     "wagers_status_defined; 'settled' is an EVENT status, not a wager one",
		},
		{
			name:    "a rounding mode this build does not know",
			corrupt: func(w *gen.GetWagerRow, _ []gen.ListWagerLegsRow) { w.Rounding = "banker" },
			wantErr: domain.ErrUnknownRounding,
			why:     "wagers_rounding_defined; a zero mode here would silently change the payout",
		},
		{
			name:    "a market type this build does not know",
			corrupt: func(_ *gen.GetWagerRow, l []gen.ListWagerLegsRow) { l[0].MarketType = "correct_score" },
			wantErr: domain.ErrUnknownMarketType,
			why:     "legs_market_fk pins market_type to markets.type",
		},
		{
			name:    "a selection role this build does not know",
			corrupt: func(_ *gen.GetWagerRow, l []gen.ListWagerLegsRow) { l[0].Role = "neither" },
			wantErr: domain.ErrUnknownSelectionRole,
			why:     "legs_role_allowed, whose ELSE FALSE arm refuses any type it does not name",
		},
		{
			name:    "a leg status this build does not know",
			corrupt: func(_ *gen.GetWagerRow, l []gen.ListWagerLegsRow) { l[0].Status = "cashed_out" },
			wantErr: domain.ErrUnknownLegStatus,
			why:     "legs_status_defined; a ticket cashes out, a leg does not",
		},
		{
			name: "a price line that is not a finite number",
			corrupt: func(_ *gen.GetWagerRow, l []gen.ListWagerLegsRow) {
				l[0].MarketType = domain.MarketTypeSpread.String()
				l[0].PriceLine = pgtype.Float8{Float64: math.NaN(), Valid: true}
			},
			wantErr: domain.ErrLineNotFinite,
			why: "legs_price_line_finite; NaN compares false against everything including " +
				"itself, so a spread graded against one always loses",
		},
		{
			name: "a booked price outside the range a price may hold",
			corrupt: func(w *gen.GetWagerRow, l []gen.ListWagerLegsRow) {
				w.AcceptedDecimal = 0.5
				l[0].PriceDecimal = 0.5
			},
			wantErr: domain.ErrOddsOutOfRange,
			why:     "legs_price_decimal_range; a price at or below 1.0 returns less than the stake",
		},
		{
			name: "a terminal ticket with no returned amount",
			corrupt: func(w *gen.GetWagerRow, _ []gen.ListWagerLegsRow) {
				w.Status = domain.WagerStatusWon.String()
				w.ReturnedMinor = nil
			},
			// No domain sentinel: this one is HydrateWager's own refusal, because
			// the alternative is dereferencing a nil pointer on a settled ticket.
			why: "wagers_return_iff_terminal makes returned_minor non-NULL exactly on a terminal row",
		},
		{
			name: "a graded leg with no grading instant",
			corrupt: func(_ *gen.GetWagerRow, l []gen.ListWagerLegsRow) {
				l[0].Status = domain.LegStatusWon.String()
			},
			why: "legs_graded_at_iff_graded is a biconditional, so this row is a schema fault " +
				"rather than a case to paper over",
		},
		{
			name: "a pending leg carrying a grading instant",
			corrupt: func(_ *gen.GetWagerRow, l []gen.ListWagerLegsRow) {
				l[0].GradedAt = pgtype.Timestamptz{Time: hydrateAt, Valid: true}
			},
			why: "the other half of legs_graded_at_iff_graded; a pending leg has not been graded",
		},
		{
			name: "an open ticket whose transition precedes its placement",
			corrupt: func(w *gen.GetWagerRow, _ []gen.ListWagerLegsRow) {
				w.Status = domain.WagerStatusOpen.String()
				w.TransitionedAt = hydrateAt.Add(-time.Hour)
			},
			wantErr: domain.ErrStaleUpdate,
			why: "wagers_transitioned_after_placed. The instant is REPORTED and not clamped: " +
				"clamping would rewrite a recorded transition time to one that never happened, " +
				"in the one subsystem whose purpose is being auditable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wager, legs := storableRow()
			tc.corrupt(&wager, legs)

			w, err := pgstore.HydrateWager(wager, legs)
			if err == nil {
				t.Fatalf("hydrated %s into a usable ticket. %s.", w, tc.why)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error is %v, want one wrapping %v (%s)", err, tc.wantErr, tc.why)
			}
			if !w.IsZero() {
				t.Errorf("a refused row still produced wager %s; a partially built money record "+
					"is worse than none", w.ID())
			}
		})
	}
}
