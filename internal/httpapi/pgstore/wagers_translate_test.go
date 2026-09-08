package pgstore

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Row -> read model, tested without a database.
//
// The rest of this package's tests are integration tests against a real
// Postgres, and they have to be: a keyset page, an index-driven scan and a
// SQLSTATE classification are only meaningfully exercised against the engine
// that produces them. These four functions are the exception. They are pure —
// a row struct in, a read-model value out — and the defects they can carry are
// exactly the ones an integration test is worst at catching, because a fixture
// tends to be written with the same assumption the mapping was.
//
// Two of those defects are worth naming, because they are silent:
//
//   - A NULL line and a stored 0.0 collapsing into one value. Every seeded
//     spread in an integration fixture has a non-zero line, so a mapping that
//     turned a pick'em into "no line" would pass every one of them.
//
//   - The returned/net-return PAIR being read field by field. The database
//     refuses "one set, one null" (wagers_return_pair_complete), so no fixture
//     can produce the half-set row that would expose a mapping which handled
//     them separately.
//
// A test that has to construct the impossible row is a unit test.

func TestWagerFromRowParsesEnumsAndRefusesUnknownOnes(t *testing.T) {
	t.Parallel()

	base := wagerRow{
		ID:                   domain.WagerID("wgr_1"),
		UserID:               domain.UserID("usr_1"),
		Kind:                 "parlay",
		Status:               "open",
		StakeMinor:           2500,
		AcceptedDecimal:      3.72,
		Rounding:             "half_to_even",
		PotentialPayoutMinor: 9300,
		PotentialProfitMinor: 6800,
		PlacedAt:             time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
		TransitionedAt:       time.Date(2026, 8, 19, 12, 5, 0, 0, time.UTC),
	}

	t.Run("a well-formed row", func(t *testing.T) {
		t.Parallel()
		got, err := wagerFromRow(base)
		if err != nil {
			t.Fatalf("wagerFromRow: %v", err)
		}
		if got.Kind != domain.WagerKindParlay {
			t.Errorf("kind = %v, want parlay", got.Kind)
		}
		if got.Status != domain.WagerStatusOpen {
			t.Errorf("status = %v, want open", got.Status)
		}
		if got.Rounding != domain.RoundHalfToEven {
			t.Errorf("rounding = %v, want half_to_even", got.Rounding)
		}
		// UpdatedAt is transitioned_at, the DOMAIN instant, never the row's
		// updated_at bookkeeping column — which this query does not even select.
		if !got.UpdatedAt.Equal(base.TransitionedAt) {
			t.Errorf("updated_at = %v, want the transition instant %v", got.UpdatedAt, base.TransitionedAt)
		}
		if _, ok := got.SettledAt(); ok {
			t.Error("a running ticket reported a settlement instant")
		}
	})

	// An unrecognised enum is a real failure and must surface as one.
	// sqlc.yaml keeps these columns as `string` precisely so a schema/Go
	// divergence "surfaces as a wrapped error at the read, not as a silent zero
	// value", and a zero-valued WagerStatus would render as an invalid status on
	// a page nobody would think to distrust.
	unknown := []struct {
		name   string
		mutate func(r *wagerRow)
	}{
		{"kind", func(r *wagerRow) { r.Kind = "accumulator" }},
		{"status", func(r *wagerRow) { r.Status = "half_won" }},
		{"rounding", func(r *wagerRow) { r.Rounding = "bankers" }},
	}
	for _, tc := range unknown {
		t.Run("unknown "+tc.name, func(t *testing.T) {
			t.Parallel()
			row := base
			tc.mutate(&row)
			if _, err := wagerFromRow(row); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("an unrecognised %s produced %v, want a domain.ErrInvalid", tc.name, err)
			}
		})
	}
}

// TestWagerFromRowKeepsTheReturnPairTogether.
//
// Wager.Returned and Wager.NetReturn share ONE presence flag in the domain and
// the database refuses any row where only one is set. The mapping must preserve
// that: a read model able to express "returned but no net return" would let a
// settled ticket render a payout with no P&L beside it.
func TestWagerFromRowKeepsTheReturnPairTogether(t *testing.T) {
	t.Parallel()

	returned := domain.Money(4775)
	net := domain.Money(2275)

	cases := []struct {
		name        string
		returned    *domain.Money
		net         *domain.Money
		wantPresent bool
	}{
		{"still running", nil, nil, false},
		{"settled", &returned, &net, true},
		// Neither of these can come out of Postgres. They are constructed here
		// because the mapping must not be the thing that would let one through
		// if the constraint were ever dropped.
		{"returned without net", &returned, nil, false},
		{"net without returned", nil, &net, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := wagerFromRow(wagerRow{
				ID: "wgr_1", UserID: "usr_1", Kind: "straight", Status: "won",
				StakeMinor: 2500, AcceptedDecimal: 1.91, Rounding: "half_to_even",
				PotentialPayoutMinor: 4775, PotentialProfitMinor: 2275,
				ReturnedMinor: tc.returned, NetReturnMinor: tc.net,
			})
			if err != nil {
				t.Fatalf("wagerFromRow: %v", err)
			}
			present := got.Returned != nil
			if present != tc.wantPresent {
				t.Errorf("returned present = %v, want %v", present, tc.wantPresent)
			}
			if (got.Returned == nil) != (got.NetReturn == nil) {
				t.Error("the pair came apart; one is set and the other is not")
			}
		})
	}
}

// TestLegFromRowDistinguishesNoLineFromAPickEm.
//
// This is the reason domain.Line is a struct rather than a *float64, and this
// mapping is the boundary where a column that cannot express the difference
// becomes a type that can. A NULL is a moneyline; a stored 0.0 is a traded
// pick'em, and a bet on it grades differently from a bet with no handicap at
// all.
func TestLegFromRowDistinguishesNoLineFromAPickEm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		line        pgtype.Float8
		wantPresent bool
		wantValue   float64
	}{
		{"no line", pgtype.Float8{}, false, 0},
		{"pick'em", pgtype.Float8{Float64: 0, Valid: true}, true, 0},
		{"a real handicap", pgtype.Float8{Float64: -3.5, Valid: true}, true, -3.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := legFromRow(legRow{
				ID: "leg_1", EventID: "evt_1", MarketID: "mkt_1", MarketType: "spread",
				SelectionID: "sel_1", Role: "home", PriceBookID: "bk_1",
				PriceDecimal: 1.91, PriceLine: tc.line, Status: "pending",
				PriceObservedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
			})
			if err != nil {
				t.Fatalf("legFromRow: %v", err)
			}
			value, present := got.Line.Value()
			if present != tc.wantPresent || (present && value != tc.wantValue) {
				t.Errorf("line = %v present=%v, want %v present=%v",
					value, present, tc.wantValue, tc.wantPresent)
			}
			// With no teased line, the grading line IS the booked line.
			if !got.GradingLine().Equal(got.Line) {
				t.Errorf("grading line %v differs from the booked line %v with no tease",
					got.GradingLine(), got.Line)
			}
		})
	}
}

// TestLegFromRowGradesAtTheTeasedLine.
//
// A teaser leg keeps BOTH lines: the real one it was booked against, so line
// history and CLV are not corrupted by a number the book never traded, and the
// moved one it actually grades at.
func TestLegFromRowGradesAtTheTeasedLine(t *testing.T) {
	t.Parallel()

	graded := time.Date(2026, 8, 19, 15, 0, 0, 0, time.UTC)
	got, err := legFromRow(legRow{
		ID: "leg_1", EventID: "evt_1", MarketID: "mkt_1", MarketType: "spread",
		SelectionID: "sel_1", Role: "home", PriceBookID: "bk_1",
		PriceDecimal: 1.91,
		PriceLine:    pgtype.Float8{Float64: -3.5, Valid: true},
		TeasedLine:   pgtype.Float8{Float64: 2.5, Valid: true},
		Status:       "won",
		GradedAt:     pgtype.Timestamptz{Time: graded, Valid: true},
	})
	if err != nil {
		t.Fatalf("legFromRow: %v", err)
	}

	if v, _ := got.Line.Value(); v != -3.5 {
		t.Errorf("booked line = %v, want the untouched -3.5", v)
	}
	if v, _ := got.GradingLine().Value(); v != 2.5 {
		t.Errorf("grading line = %v, want the teased 2.5", v)
	}
	if got.GradedAt == nil || !got.GradedAt.Equal(graded) {
		t.Errorf("graded_at = %v, want %v", got.GradedAt, graded)
	}
}

// TestLegFromRowRefusesAnUnknownEnum, for the same reason a wager's does.
func TestLegFromRowRefusesAnUnknownEnum(t *testing.T) {
	t.Parallel()

	base := legRow{
		ID: "leg_1", EventID: "evt_1", MarketID: "mkt_1", MarketType: "spread",
		SelectionID: "sel_1", Role: "home", PriceBookID: "bk_1",
		PriceDecimal: 1.91, Status: "pending",
	}
	cases := []struct {
		name   string
		mutate func(r *legRow)
	}{
		{"market type", func(r *legRow) { r.MarketType = "handicap" }},
		{"role", func(r *legRow) { r.Role = "neither" }},
		{"status", func(r *legRow) { r.Status = "half_won" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := base
			tc.mutate(&row)
			if _, err := legFromRow(row); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("an unrecognised %s produced %v, want a domain.ErrInvalid", tc.name, err)
			}
		})
	}
}
