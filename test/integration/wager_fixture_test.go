package integration

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// A valid wager is not a one-liner, and the reason is worth stating: migration
// 00006 pins nine cross-column agreements on `wagers` alone plus a DEFERRABLE
// constraint trigger that counts legs, checks the teaser magnitude AND its sign, and
// verifies a straight's accepted price against its leg's quote. Building one
// correctly here is what proves the schema is usable rather than merely strict — and
// it is the fixture phase 8 needs in order to test placement at all.
//
// Everything below is derived from the constraints, not guessed:
//
//	wagers_potential_profit_identity   profit = payout - stake, exactly
//	wagers_potential_payout_covers_stake
//	wagers_return_iff_terminal         returned_minor is set iff the status is terminal
//	wagers_return_matches_outcome      and its VALUE depends on which terminal status
//	wagers_return_pair_complete        returned and net_return are set together
//	wagers_net_return_identity         net = returned - stake, exactly
//	wagers_teaser_points_matches_kind  teaser_points is set iff kind = 'teaser'
//	wagers_round_robin_matches_kind    round_robin_id is set iff kind = 'round_robin'
//	wagers_round_robin_stake_fk        and the stake must equal the round robin's per-combination stake
//	wagers_transitioned_after_placed
//	legs_graded_at_iff_graded          graded_at is set iff the leg status is not 'pending'
//	wagers_shape_at_commit             leg count per kind, teaser sign and magnitude,
//	                                   straight price agreement, declared combination size

// wagerFixture is a placed wager and its legs, as written.
type wagerFixture struct {
	ID           domain.WagerID
	UserID       domain.UserID
	Kind         string
	Status       string
	Rounding     string
	Stake        domain.Money
	Payout       domain.Money
	Profit       domain.Money
	Accepted     float64
	RoundRobinID *domain.RoundRobinID
	TeaserPoints *float64
	Legs         []legFixture

	// returned is the terminal return, set only through withStatus.
	//
	// Unexported so a caller cannot set it without also setting a terminal
	// status: wagers_return_iff_terminal is an EQUIVALENCE — returned_minor is
	// non-NULL exactly when the status is one of won/lost/void/push/cashed_out —
	// so the two fields have to move together or the row is unwritable.
	returned *domain.Money
}

type legFixture struct {
	ID         domain.LegID
	MarketID   domain.MarketID
	MarketType string
	Selection  domain.SelectionID
	Role       string
	BookID     domain.BookID
	Decimal    float64
	Line       *float64
	TeasedLine *float64
	Status     string
	GradedAt   *time.Time
}

// wagerOption tweaks the wager before it is written, for the enum tests that need a
// particular status or rounding mode.
type wagerOption func(*wagerFixture)

func withStatus(status string, returned domain.Money) wagerOption {
	return func(w *wagerFixture) { w.Status = status; w.returned = &returned }
}

func withRounding(r string) wagerOption {
	return func(w *wagerFixture) { w.Rounding = r }
}

func withLegStatus(status string) wagerOption {
	return func(w *wagerFixture) {
		graded := time.Now().UTC().Truncate(time.Microsecond)
		for i := range w.Legs {
			w.Legs[i].Status = status
			// legs_graded_at_iff_graded: graded_at is set exactly when the status
			// is not 'pending'. Not "usually" — the constraint is an equivalence.
			if status == domain.LegStatusPending.String() {
				w.Legs[i].GradedAt = nil
			} else {
				w.Legs[i].GradedAt = &graded
			}
		}
	}
}

// newStraightWager writes a single-leg wager on a moneyline market, in one
// transaction so the deferred shape trigger sees the complete ticket.
//
// x must be a transaction. A straight wager and its leg CANNOT be written on
// autocommit: wagers_shape_at_commit fires at the end of the statement that inserted
// the wager and would find zero legs. That is the deferral working as designed, and
// it is why this signature takes an execer the caller has already put in a
// transaction rather than opening one itself.
func newStraightWager(t *testing.T, ctx context.Context, x execer, cat catalogue, mkt market, user domain.UserID, opts ...wagerOption) wagerFixture {
	t.Helper()

	const decimal = 2.1500000000000004
	stake := domain.Money(10_000)

	w := wagerFixture{
		ID:       wagerID(t, uniqueID("wager")),
		UserID:   user,
		Kind:     domain.WagerKindStraight.String(),
		Status:   domain.WagerStatusPlaced.String(),
		Rounding: domain.RoundHalfAwayFromZero.String(),
		Stake:    stake,
		Accepted: decimal,
		Legs: []legFixture{{
			ID:         legID(t, uniqueID("leg")),
			MarketID:   mkt.ID,
			MarketType: mkt.Type,
			Selection:  mkt.HomeSelection,
			Role:       mkt.HomeRole,
			BookID:     cat.BookID,
			Decimal:    decimal,
			Status:     domain.LegStatusPending.String(),
		}},
	}
	w.Payout, w.Profit = payoutFor(t, stake, decimal)

	for _, opt := range opts {
		opt(&w)
	}
	writeWager(t, ctx, x, w)
	return w
}

// newParlayWager writes a two-leg parlay across two independent moneyline markets.
//
// Two MARKETS, not two selections in one: legs_wager_market_key forbids two legs
// answering one market, because "home and away moneyline ... cannot both win, and a
// ticket that requires both to win is dead on arrival".
func newParlayWager(t *testing.T, ctx context.Context, x execer, cat catalogue, first, second market, user domain.UserID, opts ...wagerOption) wagerFixture {
	t.Helper()

	const d1, d2 = 1.9100000000000001, 2.4500000000000002
	stake := domain.Money(5_000)
	accepted := d1 * d2

	w := wagerFixture{
		ID:       wagerID(t, uniqueID("wager")),
		UserID:   user,
		Kind:     domain.WagerKindParlay.String(),
		Status:   domain.WagerStatusPlaced.String(),
		Rounding: domain.RoundHalfToEven.String(),
		Stake:    stake,
		Accepted: accepted,
		Legs: []legFixture{
			{
				ID: legID(t, uniqueID("leg")), MarketID: first.ID, MarketType: first.Type,
				Selection: first.HomeSelection, Role: first.HomeRole, BookID: cat.BookID,
				Decimal: d1, Status: domain.LegStatusPending.String(),
			},
			{
				ID: legID(t, uniqueID("leg")), MarketID: second.ID, MarketType: second.Type,
				Selection: second.AwaySelection, Role: second.AwayRole, BookID: cat.BookID,
				Decimal: d2, Status: domain.LegStatusPending.String(),
			},
		},
	}
	w.Payout, w.Profit = payoutFor(t, stake, accepted)

	for _, opt := range opts {
		opt(&w)
	}
	writeWager(t, ctx, x, w)
	return w
}

// newTeaserWager writes a two-leg teaser on two spread markets.
//
// The deferred trigger checks the tease twice — once for MAGNITUDE
// (|teased - price| == teaser_points) and once for DIRECTION, because "the magnitude
// test cannot see a sign error". A teaser moves every line in the bettor's favour: an
// over's threshold moves DOWN, a spread or an under moves UP. Both spread legs here
// are `home`, so both teased lines are price_line + points.
func newTeaserWager(t *testing.T, ctx context.Context, x execer, cat catalogue, first, second market, user domain.UserID, opts ...wagerOption) wagerFixture {
	t.Helper()

	const points = 6.0
	const d1, d2 = 1.7500000000000002, 1.8200000000000003
	stake := domain.Money(2_500)
	accepted := d1 * d2
	teaser := points

	legs := make([]legFixture, 0, 2)
	for i, mkt := range []market{first, second} {
		line, ok := mkt.Line.(float64)
		if !ok {
			t.Fatalf("teaser leg %d needs a lined market; market %s carries no line", i, mkt.ID)
		}
		priceLine := line
		teased := priceLine + points // role = home on a spread: the handicap moves up
		decimal := d1
		if i == 1 {
			decimal = d2
		}
		legs = append(legs, legFixture{
			ID: legID(t, uniqueID("leg")), MarketID: mkt.ID, MarketType: mkt.Type,
			Selection: mkt.HomeSelection, Role: mkt.HomeRole, BookID: cat.BookID,
			Decimal: decimal, Line: &priceLine, TeasedLine: &teased,
			Status: domain.LegStatusPending.String(),
		})
	}

	w := wagerFixture{
		ID:           wagerID(t, uniqueID("wager")),
		UserID:       user,
		Kind:         domain.WagerKindTeaser.String(),
		Status:       domain.WagerStatusPlaced.String(),
		Rounding:     domain.RoundTowardZero.String(),
		Stake:        stake,
		Accepted:     accepted,
		TeaserPoints: &teaser,
		Legs:         legs,
	}
	w.Payout, w.Profit = payoutFor(t, stake, accepted)

	for _, opt := range opts {
		opt(&w)
	}
	writeWager(t, ctx, x, w)
	return w
}

// newRoundRobinWager writes a round robin parent, its declared combination size, and
// one two-leg ticket from it.
//
// Three coupled facts, all enforced: round_robin_sizes.size must be between 2 and the
// parent's selection_count; wagers.stake_minor must equal the parent's
// stake_per_combination_minor (a composite foreign key, not a trigger); and the
// ticket's leg count must be one of the declared sizes (the deferred trigger).
func newRoundRobinWager(t *testing.T, ctx context.Context, x execer, cat catalogue, first, second market, user domain.UserID, opts ...wagerOption) wagerFixture {
	t.Helper()

	rrID := roundRobinID(t, uniqueID("rr"))
	stake := domain.Money(1_500)
	const selectionCount = 3

	mustExec(t, ctx, x, `
INSERT INTO round_robins (id, user_id, selection_count, stake_per_combination_minor, placed_at)
VALUES ($1, $2, $3, $4, $5)`,
		rrID, user, int32(selectionCount), stake.MinorUnits(), time.Now().UTC())

	mustExec(t, ctx, x, `
INSERT INTO round_robin_sizes (round_robin_id, selection_count, size)
VALUES ($1, $2, $3)`, rrID, int32(selectionCount), int32(2))

	const d1, d2 = 2.0500000000000003, 1.9500000000000002
	accepted := d1 * d2

	w := wagerFixture{
		ID:           wagerID(t, uniqueID("wager")),
		UserID:       user,
		Kind:         domain.WagerKindRoundRobin.String(),
		Status:       domain.WagerStatusPlaced.String(),
		Rounding:     domain.RoundHalfAwayFromZero.String(),
		Stake:        stake,
		Accepted:     accepted,
		RoundRobinID: &rrID,
		Legs: []legFixture{
			{
				ID: legID(t, uniqueID("leg")), MarketID: first.ID, MarketType: first.Type,
				Selection: first.HomeSelection, Role: first.HomeRole, BookID: cat.BookID,
				Decimal: d1, Status: domain.LegStatusPending.String(),
			},
			{
				ID: legID(t, uniqueID("leg")), MarketID: second.ID, MarketType: second.Type,
				Selection: second.AwaySelection, Role: second.AwayRole, BookID: cat.BookID,
				Decimal: d2, Status: domain.LegStatusPending.String(),
			},
		},
	}
	w.Payout, w.Profit = payoutFor(t, stake, accepted)

	for _, opt := range opts {
		opt(&w)
	}
	writeWager(t, ctx, x, w)
	return w
}

func writeWager(t *testing.T, ctx context.Context, x execer, w wagerFixture) {
	t.Helper()

	placed := time.Now().UTC().Truncate(time.Microsecond)

	var returned, netReturn *int64
	if w.returned != nil {
		r := w.returned.MinorUnits()
		// wagers_net_return_identity: net = returned - stake, exactly. Integer
		// arithmetic, because CLAUDE.md §12 puts money in minor units and
		// "floating point never touches a balance".
		n := r - w.Stake.MinorUnits()
		returned, netReturn = &r, &n
	}

	mustExec(t, ctx, x, `
INSERT INTO wagers (id, user_id, kind, status, stake_minor, accepted_decimal, rounding,
                    potential_payout_minor, potential_profit_minor, teaser_points,
                    round_robin_id, returned_minor, net_return_minor,
                    placed_at, transitioned_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)`,
		w.ID, w.UserID, w.Kind, w.Status, w.Stake.MinorUnits(), w.Accepted, w.Rounding,
		w.Payout.MinorUnits(), w.Profit.MinorUnits(), w.TeaserPoints,
		w.RoundRobinID, returned, netReturn, placed)

	for _, l := range w.Legs {
		mustExec(t, ctx, x, `
INSERT INTO legs (id, wager_id, event_id, market_id, market_type, selection_id, role,
                  price_book_id, price_decimal, price_line, price_observed_at,
                  teased_line, status, graded_at)
VALUES ($1, $2, (SELECT event_id FROM markets WHERE id = $3), $3, $4, $5, $6,
        $7, $8, $9, $10, $11, $12, $13)`,
			l.ID, w.ID, l.MarketID, l.MarketType, l.Selection, l.Role,
			l.BookID, l.Decimal, l.Line, placed, l.TeasedLine, l.Status, l.GradedAt)
	}
}

// payoutFor computes the payout and profit the way the constraints require them to
// relate: profit is exactly payout - stake, and payout covers the stake.
//
// The rounding here is deliberately the simplest thing that satisfies the
// constraints. It is NOT a reimplementation of domain.Money.MulFloat — a test
// fixture that reimplemented the pricing rule would be asserting its own arithmetic
// against itself. The wager tests care that the row is insertable and reads back
// unchanged; what the payout SHOULD be for a given price is phase 8's question and
// internal/domain's answer.
func payoutFor(t *testing.T, stake domain.Money, decimal float64) (payout, profit domain.Money) {
	t.Helper()

	raw := math.Round(float64(stake.MinorUnits()) * decimal)
	if raw < float64(stake.MinorUnits()) {
		raw = float64(stake.MinorUnits())
	}
	payout = domain.Money(int64(raw))

	p, err := payout.Sub(stake)
	if err != nil {
		t.Fatalf("payout %s - stake %s: %v", payout, stake, err)
	}
	return payout, p
}
