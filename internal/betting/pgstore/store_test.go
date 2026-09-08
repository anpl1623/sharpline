package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/betting/pgstore"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// -----------------------------------------------------------------------------
// Round trips: what was booked is what comes back
// -----------------------------------------------------------------------------

// TestALinedTicketRoundTripsThroughTheDatabaseUnchanged is the assertion the
// whole adapter exists to satisfy, made on the ticket shape nothing else in the
// tree writes.
//
// Every leg test/integration books is a moneyline leg, whose price_line is NULL,
// so the write path for a leg that CARRIES a line has never been exercised
// against the real columns. A spread is the ordinary case in this product, and
// three separate things have to survive the trip for one to grade correctly: the
// line's VALUE, the ROUNDING MODE (which decides the payout and is frozen at
// placement by wagers_assert_transition), and the booked price's OBSERVATION
// INSTANT, which is what CLV is measured against.
//
// The comparison is field by field rather than reflect.DeepEqual because
// domain.Wager holds unexported time.Time values, where == compares
// monotonic-clock readings and location pointers rather than instants.
func TestALinedTicketRoundTripsThroughTheDatabaseUnchanged(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())

	line := mustLine(t, -3.5)
	mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeSpread, line)

	at := time.Now().UTC().Truncate(time.Microsecond)
	price := bookedQuote(t, ctx, db.Pool(), mkt.Home, cat.BookID, 1.9100000000000001, line, at)
	user := newUser(t, ctx, db.Pool())

	// RoundHalfToEven and not the placement path's usual half-away-from-zero:
	// the mode is a stored policy value, and a store that wrote a constant here
	// would still pass every test that only ever books one mode.
	want := straight(t, user, homeLeg(t, cat, mkt, price, domain.NoLine()),
		domain.Money(7_500), domain.RoundHalfToEven, at)
	place(t, ctx, store, want)

	got, err := store.WagerByID(ctx, want.ID())
	if err != nil {
		t.Fatalf("read the ticket back: %v", err)
	}
	assertSameWager(t, got, want)

	gotLine, ok := got.Legs()[0].Price().Line().Value()
	if !ok || gotLine != -3.5 {
		t.Errorf("the booked line came back as (%v, %t), want (-3.5, true): a leg that grades "+
			"at the wrong handicap pays the wrong customer", gotLine, ok)
	}
}

// TestAPickEmComesBackPresentAndAMoneylineComesBackAbsent is the distinction
// domain.Line is a presence-carrying value type FOR, asserted end to end.
//
// A stored 0.0 is a PICK'EM — a real traded handicap, the most common spread in
// several of the sports in scope — and a stored NULL is a market with no line at
// all. Both arrive from the driver as a pgtype.Float8 whose Float64 is zero, and
// only the Valid flag separates them. Collapsing them turns every pick'em spread
// into a moneyline at grading time: the margin test disappears and a home side
// that lost by three grades as a winner.
//
// The two legs are put on ONE parlay deliberately. That is the shape that makes
// the bug visible in a single read — the same []Leg carrying one line that must
// be present and one that must be absent — and it is a shape the slip really
// produces.
func TestAPickEmComesBackPresentAndAMoneylineComesBackAbsent(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	pickEm := mustLine(t, 0)
	spread := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeSpread, pickEm)
	moneyline := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())

	const d1, d2 = 1.9500000000000002, 2.0500000000000003
	spreadLeg := homeLeg(t, cat, spread,
		bookedQuote(t, ctx, db.Pool(), spread.Home, cat.BookID, d1, pickEm, at), domain.NoLine())
	moneylineLeg := homeLeg(t, cat, moneyline,
		bookedQuote(t, ctx, db.Pool(), moneyline.Home, cat.BookID, d2, domain.NoLine(), at), domain.NoLine())

	want, err := domain.NewWager(domain.WagerParams{
		ID:              mustWagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindParlay,
		Legs:            []domain.Leg{spreadLeg, moneylineLeg},
		Stake:           domain.Money(3_000),
		AcceptedDecimal: d1 * d2,
		Rounding:        domain.RoundHalfAwayFromZero,
		PlacedAt:        at,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}
	place(t, ctx, store, want)

	got, err := store.WagerByID(ctx, want.ID())
	if err != nil {
		t.Fatalf("read the parlay back: %v", err)
	}
	assertSameWager(t, got, want)

	for _, leg := range got.Legs() {
		value, present := leg.Price().Line().Value()
		switch leg.MarketType() {
		case domain.MarketTypeSpread:
			if !present || value != 0 {
				t.Errorf("the pick'em leg came back as (%v, %t), want (0, true): a pick'em read "+
					"as an absent line grades a spread as a moneyline", value, present)
			}
		case domain.MarketTypeMoneyline:
			if present {
				t.Errorf("the moneyline leg came back carrying line %v; NULL is not a line", value)
			}
		}
	}
}

// TestATeaserRoundTripsWithItsPointsAndItsMovedLines covers the one ticket shape
// that stores TWO lines per leg and a float on the wager itself.
//
// leg.go keeps the REAL market price and carries the teased line beside it
// rather than forging a price at the moved number, "which would corrupt the line
// history and destroy CLV, since the book never traded there". That means a
// teaser leg's grading line and its booked line are deliberately different
// values in two different columns, and an adapter that wrote either into the
// other would produce a ticket that looks entirely plausible and grades at a
// handicap nobody sold.
func TestATeaserRoundTripsWithItsPointsAndItsMovedLines(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	// Two MARKETS, not two selections in one: legs_wager_market_key forbids two
	// legs answering the same question.
	const points = 6.0
	const d1, d2 = 1.7500000000000002, 1.8200000000000003

	legs := make([]domain.Leg, 0, 2)
	for _, spec := range []struct {
		line    float64
		decimal float64
	}{{-7.5, d1}, {-3.0, d2}} {
		line := mustLine(t, spec.line)
		mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeSpread, line)
		price := bookedQuote(t, ctx, db.Pool(), mkt.Home, cat.BookID, spec.decimal, line, at)
		// A teaser moves every line in the bettor's favour, and wagers_assert_shape
		// checks the DIRECTION as well as the magnitude: a spread's handicap moves
		// UP, only an over's threshold moves down.
		teased := mustLine(t, spec.line+points)
		legs = append(legs, homeLeg(t, cat, mkt, price, teased))
	}

	want, err := domain.NewWager(domain.WagerParams{
		ID:              mustWagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindTeaser,
		Legs:            legs,
		Stake:           domain.Money(2_500),
		AcceptedDecimal: d1 * d2,
		Rounding:        domain.RoundTowardZero,
		TeaserPoints:    points,
		PlacedAt:        at,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}
	place(t, ctx, store, want)

	got, err := store.WagerByID(ctx, want.ID())
	if err != nil {
		t.Fatalf("read the teaser back: %v", err)
	}
	assertSameWager(t, got, want)

	gotPoints, hasPoints := got.TeaserPoints()
	if !hasPoints || gotPoints != points {
		t.Fatalf("teaser points came back as (%v, %t), want (%v, true)", gotPoints, hasPoints, points)
	}
	for _, leg := range got.Legs() {
		booked, _ := leg.Price().Line().Value()
		grading, ok := leg.GradingLine().Value()
		if !ok {
			t.Fatalf("teaser leg %s has no grading line", leg.ID())
		}
		if grading != booked+points {
			t.Errorf("leg %s grades at %v but was booked at %v with %v points: the teased line "+
				"and the booked line have been confused", leg.ID(), grading, booked, points)
		}
	}
}

// TestAnOpenTicketIsRehydratedByOpeningItBeforeGradingItsLegs is the asymmetry
// HydrateWager's doc comment calls the thing to understand before editing it,
// asserted against instants a real settlement produces.
//
// wagers.transitioned_at on an OPEN ticket is the instant its first event went
// live. settlement.sql's GradeLeg touches only `legs`, so a leg graded afterwards
// does NOT advance it — and therefore transitioned_at is EARLIER than any
// graded_at on the ticket. domain.Wager.stamp refuses a non-monotone update with
// ErrStaleUpdate, so a replay that graded the legs first and opened afterwards
// would fail to rehydrate a perfectly valid row, and every open parlay in the
// system would read as a database error.
//
// The two UPDATEs are written here as raw statements because they are what
// settlement's own OpenWager and GradeLeg statements do; the point of the test is
// the READ, and the writes only have to produce the row settlement produces.
func TestAnOpenTicketIsRehydratedByOpeningItBeforeGradingItsLegs(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	placedAt := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)

	const d1, d2 = 2.0500000000000003, 1.9500000000000002
	first := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	second := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	legs := []domain.Leg{
		homeLeg(t, cat, first,
			bookedQuote(t, ctx, db.Pool(), first.Home, cat.BookID, d1, domain.NoLine(), placedAt), domain.NoLine()),
		homeLeg(t, cat, second,
			bookedQuote(t, ctx, db.Pool(), second.Home, cat.BookID, d2, domain.NoLine(), placedAt), domain.NoLine()),
	}

	w, err := domain.NewWager(domain.WagerParams{
		ID:              mustWagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindParlay,
		Legs:            legs,
		Stake:           domain.Money(1_000),
		AcceptedDecimal: d1 * d2,
		Rounding:        domain.RoundHalfAwayFromZero,
		PlacedAt:        placedAt,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}
	place(t, ctx, store, w)

	openedAt := placedAt.Add(time.Hour)
	gradedAt := openedAt.Add(time.Hour)
	mustExec(t, ctx, db.Pool(),
		`UPDATE wagers SET status = 'open', transitioned_at = $2 WHERE id = $1`, w.ID(), openedAt)
	mustExec(t, ctx, db.Pool(),
		`UPDATE legs SET status = 'won', graded_at = $2 WHERE id = $1`, legs[0].ID(), gradedAt)

	got, err := store.WagerByID(ctx, w.ID())
	if err != nil {
		t.Fatalf("an open ticket whose leg was graded after it opened failed to rehydrate: %v", err)
	}
	if got.Status() != domain.WagerStatusOpen {
		t.Errorf("status: got %s, want open", got.Status())
	}

	// The STORED transition instant is untouched by the grading, which is the
	// fact that makes Open-before-grade the right replay order in the first
	// place. Asserted against the column rather than against the rehydrated
	// value, because they are deliberately different numbers: the replay's last
	// stamp is Wager.GradeLeg's, so a rehydrated open ticket's UpdatedAt() is the
	// most recent GRADING instant. That is a property of replaying transitions
	// through the domain rather than assigning fields, and it is harmless here
	// only because nothing writes wagers.transitioned_at from a rehydrated
	// ticket — settlement writes the instant its own Settle call stamped.
	var stored time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT transitioned_at FROM wagers WHERE id = $1`, w.ID()).Scan(&stored); err != nil {
		t.Fatalf("read the stored transition instant: %v", err)
	}
	if !stored.Equal(openedAt) {
		t.Errorf("wagers.transitioned_at is %s, want %s: grading a leg must not advance the "+
			"ticket's own transition instant", stored, openedAt)
	}
	if !got.UpdatedAt().Equal(gradedAt) {
		t.Errorf("the rehydrated ticket's last stamp is %s, want the grading instant %s",
			got.UpdatedAt(), gradedAt)
	}

	graded, ok := got.Leg(legs[0].ID())
	if !ok {
		t.Fatalf("the graded leg is missing from the rehydrated ticket")
	}
	if graded.Status() != domain.LegStatusWon {
		t.Errorf("graded leg status: got %s, want won", graded.Status())
	}
	pending, ok := got.Leg(legs[1].ID())
	if !ok {
		t.Fatalf("the ungraded leg is missing from the rehydrated ticket")
	}
	if pending.Status() != domain.LegStatusPending {
		t.Errorf("the ungraded leg came back %s, want pending", pending.Status())
	}
}

// TestASettledTicketReplaysItsGradingsInTheOrderTheyHappened is the other half
// of the transition replay, and it is the half a naive implementation gets wrong
// without ever failing.
//
// queries/betting.sql's ListWagerLegs is ORDER BY selection_id, which is a
// stable order and deliberately NOT the order the legs were graded in — a parlay
// across two games is settled leg by leg as each game finishes, hours apart, and
// selection ids say nothing about that. Wager.stamp refuses a non-monotone
// instant, so HydrateWager sorts the gradings by graded_at before replaying
// them; without the sort a ticket whose arrival order happens to be descending
// fails to rehydrate with ErrStaleUpdate.
//
// The test therefore reads the arrival order back from the database and grades
// the FIRST-arriving leg LAST, rather than assuming what the ordering will be.
// Assuming would make the test depend on the server's collation, and a test that
// silently stops exercising the sort is worse than no test — the sort would look
// covered.
func TestASettledTicketReplaysItsGradingsInTheOrderTheyHappened(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	placedAt := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Microsecond)

	const d1, d2 = 2.0500000000000003, 1.9500000000000002
	first := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	second := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	legs := []domain.Leg{
		homeLeg(t, cat, first,
			bookedQuote(t, ctx, db.Pool(), first.Home, cat.BookID, d1, domain.NoLine(), placedAt), domain.NoLine()),
		homeLeg(t, cat, second,
			bookedQuote(t, ctx, db.Pool(), second.Home, cat.BookID, d2, domain.NoLine(), placedAt), domain.NoLine()),
	}

	stake := domain.Money(4_000)
	w, err := domain.NewWager(domain.WagerParams{
		ID:              mustWagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindParlay,
		Legs:            legs,
		Stake:           stake,
		AcceptedDecimal: d1 * d2,
		Rounding:        domain.RoundHalfAwayFromZero,
		PlacedAt:        placedAt,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}
	place(t, ctx, store, w)

	// The order HydrateWager will actually receive them in.
	rows, err := db.Pool().Query(ctx, `SELECT id FROM legs WHERE wager_id = $1 ORDER BY selection_id`, w.ID())
	if err != nil {
		t.Fatalf("read leg arrival order: %v", err)
	}
	var arrival []domain.LegID
	for rows.Next() {
		var id domain.LegID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan leg id: %v", err)
		}
		arrival = append(arrival, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read leg arrival order: %v", err)
	}
	if len(arrival) != 2 {
		t.Fatalf("the parlay came back with %d legs, want 2", len(arrival))
	}

	// First to arrive, last to be graded: the arrival order is now the reverse of
	// the grading order, which is what the sort exists to correct.
	early := placedAt.Add(2 * time.Hour)
	late := placedAt.Add(4 * time.Hour)
	mustExec(t, ctx, db.Pool(),
		`UPDATE legs SET status = 'won', graded_at = $2 WHERE id = $1`, arrival[0], late)
	mustExec(t, ctx, db.Pool(),
		`UPDATE legs SET status = 'won', graded_at = $2 WHERE id = $1`, arrival[1], early)

	// wagers_net_return_identity: net = returned - stake, exactly. Integer
	// arithmetic through the domain's own Sub, because CLAUDE.md §12 keeps money
	// in minor units and floating point never touches a balance.
	payout := w.PotentialPayout()
	net, err := payout.Sub(stake)
	if err != nil {
		t.Fatalf("payout %s - stake %s: %v", payout, stake, err)
	}
	mustExec(t, ctx, db.Pool(), `
UPDATE wagers
   SET status = 'won', returned_minor = $2, net_return_minor = $3, transitioned_at = $4
 WHERE id = $1`, w.ID(), payout.MinorUnits(), net.MinorUnits(), late)

	got, err := store.WagerByID(ctx, w.ID())
	if err != nil {
		t.Fatalf("a settled parlay whose legs arrive in reverse grading order failed to "+
			"rehydrate: %v", err)
	}
	if got.Status() != domain.WagerStatusWon {
		t.Errorf("status: got %s, want won", got.Status())
	}
	returned, ok := got.Returned()
	if !ok || returned != payout {
		t.Errorf("returned: got (%s, %t), want (%s, true)", returned, ok, payout)
	}
	if gotNet, _ := got.NetReturn(); gotNet != net {
		t.Errorf("net return: got %s, want %s", gotNet, net)
	}
	if !got.AllLegsGraded() {
		t.Error("a settled ticket came back with an ungraded leg")
	}
	for _, leg := range got.Legs() {
		at, graded := leg.GradedAt()
		if !graded {
			t.Errorf("leg %s has no grading instant", leg.ID())
			continue
		}
		want := early
		if leg.ID() == arrival[0] {
			want = late
		}
		if !at.Equal(want) {
			t.Errorf("leg %s graded at %s, want %s: the replay stamped an instant that never "+
				"happened", leg.ID(), at, want)
		}
	}
}

// TestACashedOutTicketIsRebuiltThroughCashOutAndNotThroughSettle.
//
// A cash-out closes a ticket EARLY, which means its legs are still pending when
// it becomes terminal — the customer sold the position rather than waiting for
// the result. domain.Wager.Settle refuses that shape twice over: it requires a
// GRADED outcome (WagerStatus.IsGraded excludes cashed_out) and it applies
// checkReturn, whose won/lost/void/push arms none of them describe a negotiated
// price. CashOut applies its own rule instead — strictly positive, at most the
// potential payout — and the two are deliberately different checks on different
// amounts.
//
// So a rehydration that collapsed the two branches would fail on every
// cashed-out ticket in the system, and cash-out is a shipped feature: the
// customer's own wager history would 500 on precisely the bets they closed
// early.
func TestACashedOutTicketIsRebuiltThroughCashOutAndNotThroughSettle(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	placedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)

	mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	price := bookedQuote(t, ctx, db.Pool(), mkt.Home, cat.BookID, 1.9100000000000001, domain.NoLine(), placedAt)
	w := straight(t, user, homeLeg(t, cat, mkt, price, domain.NoLine()),
		domain.Money(10_000), domain.RoundHalfAwayFromZero, placedAt)
	place(t, ctx, store, w)

	// Below the potential payout and above zero: a price taken while the game is
	// still in play. wagers_return_matches_outcome's cashed_out arm is the same
	// rule stated in SQL.
	const returned = domain.Money(12_000)
	net, err := returned.Sub(w.Stake())
	if err != nil {
		t.Fatalf("returned %s - stake %s: %v", returned, w.Stake(), err)
	}
	closedAt := placedAt.Add(time.Hour)
	mustExec(t, ctx, db.Pool(), `
UPDATE wagers
   SET status = 'cashed_out', returned_minor = $2, net_return_minor = $3, transitioned_at = $4
 WHERE id = $1`, w.ID(), returned.MinorUnits(), net.MinorUnits(), closedAt)

	got, err := store.WagerByID(ctx, w.ID())
	if err != nil {
		t.Fatalf("a cashed-out ticket failed to rehydrate: %v. Settle refuses a non-graded "+
			"outcome, so this is what a collapsed CashOut branch looks like.", err)
	}
	if got.Status() != domain.WagerStatusCashedOut {
		t.Errorf("status: got %s, want cashed_out", got.Status())
	}
	amount, ok := got.Returned()
	if !ok || amount != returned {
		t.Errorf("returned: got (%s, %t), want (%s, true)", amount, ok, returned)
	}
	if got.Legs()[0].Status() != domain.LegStatusPending {
		t.Errorf("the leg came back %s; cashing out closes a ticket without grading it",
			got.Legs()[0].Status())
	}
}

// -----------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------

// TestAReplayedPlacementLeavesTheTransactionUsableForFurtherWrites is the
// savepoint's contract on the WAGER path, and it is a strictly stronger claim
// than "the read-back works".
//
// PostgreSQL aborts the ENTIRE transaction on any statement error: after the
// 23505 on wagers_pkey, every subsequent command fails with SQLSTATE 25P02
// (in_failed_sql_transaction) until the transaction is rolled back. txStore.
// insertOnce opens a SAVEPOINT around the INSERT for exactly this reason, and
// without it the idempotency guarantee reads correct in every doc comment and
// does not hold at runtime.
//
// test/integration proves the recovery on the ROUND ROBIN path, where the
// service writes after the collision. The wager path only ever reads afterwards,
// so a savepoint regression there would leave that test green — a read is a
// weaker probe than a write, because a failed transaction refuses both but a
// caller that never writes cannot tell the difference between "recovered" and
// "about to be rolled back". Here the collision is followed by a read AND by a
// second ticket that must COMMIT, so the assertion is that the whole transaction
// survived and not merely that one statement was answered.
func TestAReplayedPlacementLeavesTheTransactionUsableForFurtherWrites(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	first := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	replayed := straight(t, user, homeLeg(t, cat, first,
		bookedQuote(t, ctx, db.Pool(), first.Home, cat.BookID, 1.9100000000000001, domain.NoLine(), at),
		domain.NoLine()), domain.Money(2_500), domain.RoundHalfAwayFromZero, at)
	place(t, ctx, store, replayed)

	// A second, entirely different ticket, written AFTER the collision inside the
	// same transaction. This is the statement 25P02 would kill.
	second := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	fresh := straight(t, user, homeLeg(t, cat, second,
		bookedQuote(t, ctx, db.Pool(), second.Home, cat.BookID, 2.1500000000000004, domain.NoLine(), at),
		domain.NoLine()), domain.Money(1_500), domain.RoundHalfAwayFromZero, at)

	var collision error
	var existing domain.Wager
	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		collision = tx.InsertWager(ctx, replayed)
		if !errors.Is(collision, betting.ErrAlreadyPlaced) {
			return nil
		}
		var err error
		if existing, err = tx.WagerByID(ctx, replayed.ID()); err != nil {
			return err
		}
		return tx.InsertWager(ctx, fresh)
	})
	if err != nil {
		t.Fatalf("the transaction did not survive the primary-key collision: %v", err)
	}
	if !errors.Is(collision, betting.ErrAlreadyPlaced) {
		t.Fatalf("a replayed placement reported %v, want betting.ErrAlreadyPlaced", collision)
	}
	if existing.ID() != replayed.ID() {
		t.Errorf("the replay read back wager %s, want %s", existing.ID(), replayed.ID())
	}
	if _, err := store.WagerByID(ctx, fresh.ID()); err != nil {
		t.Errorf("the ticket written after the collision did not commit: %v", err)
	}
}

// TestALegCollisionIsAHardFailureThatKeepsItsSQLSTATE.
//
// Three unique constraints are reachable from InsertWager and only wagers_pkey
// means "replay". A leg collision means the slip put two legs on one selection
// or one market — a malformed ticket — and reporting it as ErrAlreadyPlaced
// would send the service to read a wager that was never written.
//
// The SQLSTATE assertion is the second half. A leg collision is a plumbing fault
// somebody has to diagnose from a log line, and the wrapped driver error is the
// only thing in the message that names WHICH constraint refused. classifyWagerInsert's
// own comment records that this fact was destroyed once already, on the
// self-exclusion path, by wrapping a sentinel INSTEAD OF the driver error rather
// than as well as.
func TestALegCollisionIsAHardFailureThatKeepsItsSQLSTATE(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	price := bookedQuote(t, ctx, db.Pool(), mkt.Home, cat.BookID, 2.0, domain.NoLine(), at)
	leg := homeLeg(t, cat, mkt, price, domain.NoLine())

	first := straight(t, user, leg, domain.Money(1_000), domain.RoundHalfAwayFromZero, at)
	place(t, ctx, store, first)

	// A DIFFERENT ticket reusing the SAME leg id. RoundRobin.Combinations()
	// returns subsets of the same []Leg values, so a service that passed them
	// through verbatim would do exactly this.
	second := straight(t, user, leg, domain.Money(1_000), domain.RoundHalfAwayFromZero, at)

	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertWager(ctx, second)
	})
	if err == nil {
		t.Fatal("two tickets sharing a leg id were both written; legs_pkey should have refused the second")
	}
	if errors.Is(err, betting.ErrAlreadyPlaced) {
		t.Errorf("a leg collision was reported as a replay: %v. The service would answer it by "+
			"reading back a wager that was never written.", err)
	}
	if state := postgres.SQLState(err); state != "23505" {
		t.Errorf("SQLSTATE: got %q, want 23505; the driver error was dropped and the one fact "+
			"that names the failing constraint went with it. Error was: %v", state, err)
	}
}

// TestALedgerTransactionReplayIsReportedRatherThanWrittenTwice covers
// classifyLedgerWrite, which nothing else in the tree reaches.
//
// internal/betting derives a grant's transaction identifier deterministically,
// so a retried grant — an HTTP retry, a redelivered request, a client that gave
// up on a slow response — arrives as the same primary key. The collision is what
// stops the customer being credited twice, and it has to be reported as
// betting.ErrAlreadyPlaced rather than as a raw unique violation, because the
// caller's branch for "this already happened" is written against the sentinel.
func TestALedgerTransactionReplayIsReportedRatherThanWrittenTwice(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	txn, err := domain.NewGrantTransaction(
		mustTransactionID(t, uniqueID("txn")), user, domain.Money(50_000), at)
	if err != nil {
		t.Fatalf("NewGrantTransaction: %v", err)
	}

	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertTransaction(ctx, txn)
	}); err != nil {
		t.Fatalf("first grant: %v", err)
	}

	replay := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertTransaction(ctx, txn)
	})
	if !errors.Is(replay, betting.ErrAlreadyPlaced) {
		t.Fatalf("a replayed ledger transaction reported %v, want betting.ErrAlreadyPlaced", replay)
	}

	// And the money moved exactly once. A second grant that had been written
	// would balance perfectly and be invisible in every check but this one.
	cash, err := domain.UserCashAccount(user)
	if err != nil {
		t.Fatalf("UserCashAccount: %v", err)
	}
	var balance domain.Money
	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		var err error
		balance, err = tx.Balance(ctx, cash)
		return err
	}); err != nil {
		t.Fatalf("fold the balance: %v", err)
	}
	if balance != domain.Money(50_000) {
		t.Errorf("balance after a replayed grant is %s, want 500.00", balance)
	}
}

// TestAFailedReadIsAnErrorAndNeverAZeroValue is the converse of the mapping
// [betting.Tx.Balance] exists for.
//
// "No rows" MUST fold to zero — account_balances has no row for a customer who
// has never been credited, and surfacing that would make a first-ever placement
// fail. But the same switch has a second arm, and if a real failure fell through
// it the customer's balance would read as 0 during an outage: every placement
// would be refused as unaffordable, and the same collapse on SumEntries would
// report "this customer has staked nothing", which permits every stake.
//
// The failure is produced with an already-cancelled context rather than by
// breaking the server, because it is the only way to fail ONE statement inside a
// live transaction and it is the failure mode that actually happens — a request
// deadline expiring mid-placement.
func TestAFailedReadIsAnErrorAndNeverAZeroValue(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := newUser(t, ctx, db.Pool())

	cash, err := domain.UserCashAccount(user)
	if err != nil {
		t.Fatalf("UserCashAccount: %v", err)
	}

	// The zero balance is real first, so the test is about the ERROR arm and not
	// about a user who happens to have no rows either way.
	var zero domain.Money
	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		var err error
		zero, err = tx.Balance(ctx, cash)
		return err
	}); err != nil {
		t.Fatalf("fold an untouched balance: %v", err)
	}
	if zero != 0 {
		t.Fatalf("an untouched account folded to %s, want zero", zero)
	}

	cases := []struct {
		name string
		call func(ctx context.Context, tx betting.Tx) error
	}{
		{"balance", func(ctx context.Context, tx betting.Tx) error {
			got, err := tx.Balance(ctx, cash)
			if err == nil {
				t.Errorf("a cancelled balance read returned %s and no error", got)
			}
			return err
		}},
		{"entry sum", func(ctx context.Context, tx betting.Tx) error {
			got, err := tx.SumEntries(ctx, cash, []domain.EntryKind{domain.EntryKindStake}, time.Now().Add(-time.Hour))
			if err == nil {
				t.Errorf("a cancelled entry sum returned %s and no error", got)
			}
			return err
		}},
		{"limits", func(ctx context.Context, tx betting.Tx) error {
			got, err := tx.LimitsInForce(ctx, user, time.Now())
			if err == nil {
				t.Errorf("a cancelled limits read returned %d limits and no error", len(got))
			}
			return err
		}},
		{"user status", func(ctx context.Context, tx betting.Tx) error {
			got, err := tx.UserStatus(ctx, user)
			if err == nil {
				t.Errorf("a cancelled status read returned %q and no error", got)
			}
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The transaction opens on a live context; only the statement inside
			// it is cancelled. The outer error is expected and is not the
			// assertion — the assertions are inside the callback.
			dead, cancel := context.WithCancel(ctx)
			cancel()
			_ = store.InTx(ctx, func(_ context.Context, tx betting.Tx) error {
				return tc.call(dead, tx)
			})
		})
	}
}

// -----------------------------------------------------------------------------
// Reads the placement path depends on
// -----------------------------------------------------------------------------

// TestGrantCreditAnswersForTheCustomerAndForNobodyElse covers the one method on
// betting.Tx that nothing else in the tree exercises, and it is a method whose
// every discriminating predicate lives in SQL.
//
// A grant is a TWO-SIDED movement: issuance is debited and the customer's cash
// is credited by the same magnitude with the opposite sign. GetGrantCreditForUser
// has to return the customer's side, for this customer, on a transaction that is
// a grant — and a predicate the wrong way round returns a NEGATIVE amount that a
// limit check would then treat as a credit of nothing, or returns somebody
// else's grant against this customer's daily cap.
//
// The three refusals are asserted alongside the answer because "returns the
// right number" and "refuses the wrong question" are different claims.
func TestGrantCreditAnswersForTheCustomerAndForNobodyElse(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := newUser(t, ctx, db.Pool())
	stranger := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	const amount = domain.Money(25_000)
	txn, err := domain.NewGrantTransaction(mustTransactionID(t, uniqueID("txn")), user, amount, at)
	if err != nil {
		t.Fatalf("NewGrantTransaction: %v", err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertTransaction(ctx, txn)
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	err = store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		credited, occurred, err := tx.GrantCredit(ctx, txn.ID(), user)
		if err != nil {
			return err
		}
		if credited != amount {
			t.Errorf("grant credit is %s, want %s. A negative here is the ISSUANCE half of the "+
				"same movement, which is the failure mode that looks like an answer.", credited, amount)
		}
		if !occurred.Equal(at) {
			t.Errorf("grant occurred at %s, want %s", occurred, at)
		}

		if _, _, err := tx.GrantCredit(ctx, txn.ID(), stranger); !errors.Is(err, betting.ErrGrantNotFound) {
			t.Errorf("another customer read this grant: %v, want betting.ErrGrantNotFound", err)
		}
		unknown := mustTransactionID(t, uniqueID("txn"))
		if _, _, err := tx.GrantCredit(ctx, unknown, user); !errors.Is(err, betting.ErrGrantNotFound) {
			t.Errorf("an unknown transaction returned %v, want betting.ErrGrantNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("grant credit transaction: %v", err)
	}
}

// TestAbsenceIsASentinelAndNotAZeroValue.
//
// Each of these mappings is what stands between a slip that says something
// usable and a 500. ErrQuoteUnavailable in particular is an ORDINARY outcome —
// a book that does not price a market is not a fault — and a store that reported
// it as an internal error would make a perfectly healthy multi-book board look
// broken every time one book was missing a line.
func TestAbsenceIsASentinelAndNotAZeroValue(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())

	if _, err := store.WagerByID(ctx, mustWagerID(t, uniqueID("wager"))); !errors.Is(err, betting.ErrWagerNotFound) {
		t.Errorf("an unknown wager returned %v, want betting.ErrWagerNotFound", err)
	}

	// A real book that has never quoted the real selection above.
	silent := mustBookID(t, uniqueID("book"))
	mustExec(t, ctx, db.Pool(),
		`INSERT INTO books (id, slug, name, kind, is_reference) VALUES ($1, $2, $3, 'external', FALSE)`,
		silent, mustSlug(t, "book-"+silent.String()), "Silent "+silent.String())

	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		if _, err := tx.QuoteFor(ctx, mkt.Home, silent); !errors.Is(err, betting.ErrQuoteUnavailable) {
			t.Errorf("a book with no quote on a selection returned %v, want betting.ErrQuoteUnavailable", err)
		}
		if _, err := tx.MarketState(ctx, mustMarketID(t, uniqueID("market"))); !errors.Is(err, betting.ErrMarketNotOpen) {
			t.Errorf("a market that does not exist returned %v, want betting.ErrMarketNotOpen. From "+
				"the slip's point of view a vanished market and a closed one are one refusal.", err)
		}
		if _, err := tx.WagerByID(ctx, mustWagerID(t, uniqueID("wager"))); !errors.Is(err, betting.ErrWagerNotFound) {
			t.Errorf("an unknown wager inside a transaction returned %v, want betting.ErrWagerNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("absence transaction: %v", err)
	}
}

// TestSumEntriesRefusesTheTwoQuestionsThatWouldReadAsPermission.
//
// Both refusals guard the same hazard: an argument that the SQL would answer
// with a perfectly well-formed ZERO. `kind = ANY('{}')` is false for every row,
// so an empty kind set sums to nothing — and "this customer has staked nothing"
// is the answer that lets any stake through a responsible-gaming limit. A system
// account is filtered by owner here, and `account_user_id = NULL` is NULL and
// never true, so the house singleton would report as zero too.
//
// A programming error that reads as permission is exactly the one to fail loudly
// on, which is why these are refusals in Go rather than queries that happen to
// return nothing.
func TestSumEntriesRefusesTheTwoQuestionsThatWouldReadAsPermission(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := newUser(t, ctx, db.Pool())
	cash, err := domain.UserCashAccount(user)
	if err != nil {
		t.Fatalf("UserCashAccount: %v", err)
	}
	since := time.Now().UTC().Add(-24 * time.Hour)

	err = store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		if _, err := tx.SumEntries(ctx, cash, nil, since); !errors.Is(err, betting.ErrInvalidOptions) {
			t.Errorf("an empty kind set returned %v, want betting.ErrInvalidOptions", err)
		}
		if _, err := tx.SumEntries(ctx, cash, []domain.EntryKind{domain.EntryKindUnknown}, since); err == nil {
			t.Error("an unrecognised entry kind was sent to the database rather than refused")
		} else if !errors.Is(err, domain.ErrUnknownEntryKind) {
			t.Errorf("an unrecognised entry kind returned %v, want domain.ErrUnknownEntryKind", err)
		}
		if _, err := tx.SumEntries(ctx, domain.HouseAccount(),
			[]domain.EntryKind{domain.EntryKindStake}, since); !errors.Is(err, betting.ErrInvalidOptions) {
			t.Errorf("the house singleton returned %v, want betting.ErrInvalidOptions", err)
		}
		if _, err := tx.Balance(ctx, domain.IssuanceAccount()); !errors.Is(err, betting.ErrInvalidOptions) {
			t.Errorf("the issuance singleton's balance returned %v, want betting.ErrInvalidOptions. "+
				"Reporting it as zero is wrong in the direction that looks plausible.", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("refusal transaction: %v", err)
	}
}

// TestLimitsInForceCarriesASessionLimitWithNoAmount.
//
// user_limits.amount_minor is NULL on the session row — user_limits_money_is_amount
// makes that a biconditional — so the session limit is the one row a bare scan
// into a non-pointer integer fails on, and it is the row a money-limit test never
// reaches. The query projects the amount as a value plus a presence flag for
// exactly this row.
//
// ports.go also requires the row to be RETURNED rather than filtered out here:
// "Amount is meaningful only when Kind.IsMoney()" means the evaluator expects to
// see the non-money kinds and skip them, so a store that dropped them would make
// that guard dead code and hide a session limit from anything that later wanted
// to report the full set.
func TestLimitsInForceCarriesASessionLimitWithNoAmount(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := newUser(t, ctx, db.Pool())

	now := time.Now().UTC().Truncate(time.Microsecond)
	mustExec(t, ctx, db.Pool(), `
INSERT INTO user_limits (id, user_id, kind, period, amount_minor, duration_seconds,
                         requested_at, effective_from)
VALUES ($1, $2, 'session', 'session', NULL, 5400, $3, $3)`,
		uniqueID("lim"), user, now.Add(-time.Hour))
	mustExec(t, ctx, db.Pool(), `
INSERT INTO user_limits (id, user_id, kind, period, amount_minor, duration_seconds,
                         requested_at, effective_from)
VALUES ($1, $2, 'stake', 'day', 100000, NULL, $3, $3)`,
		uniqueID("lim"), user, now.Add(-time.Hour))

	var limits []betting.Limit
	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		var err error
		limits, err = tx.LimitsInForce(ctx, user, now)
		return err
	}); err != nil {
		t.Fatalf("read limits in force: %v", err)
	}

	var session, stake int
	for _, l := range limits {
		switch l.Kind {
		case auth.LimitKindSession:
			session++
			if l.Period != auth.LimitPeriodSession {
				t.Errorf("session limit carries period %s, want session", l.Period)
			}
			// Zero is a safe sentinel for "no amount" only because it is
			// unstorable: user_limits_amount_range requires amount_minor > 0.
			if l.Amount != 0 {
				t.Errorf("session limit carries amount %s; the column is NULL on that row", l.Amount)
			}
		case auth.LimitKindStake:
			stake++
			if l.Amount != domain.Money(100_000) {
				t.Errorf("stake limit amount is %s, want 1000.00", l.Amount)
			}
		}
	}
	if session != 1 {
		t.Errorf("found %d session limits in force, want 1: a store that filtered the non-money "+
			"kinds would make the evaluator's own guard dead code", session)
	}
	if stake != 1 {
		t.Errorf("found %d stake limits in force, want 1", stake)
	}
}

// -----------------------------------------------------------------------------
// Constructor and transaction contracts
// -----------------------------------------------------------------------------

// TestTheAdapterRefusesToBeBuiltOrDrivenWithoutItsDependencies.
//
// Both refusals are about the same thing: this package's every statement runs
// through [pgstore.Store.InTx], so a nil pool or a nil callback is a wire-up
// mistake that would otherwise surface as a nil dereference in a goroutine
// serving a placement. CLAUDE.md §12 asks for constructor injection with
// fail-fast validation, and these are the two ways that can be got wrong.
func TestTheAdapterRefusesToBeBuiltOrDrivenWithoutItsDependencies(t *testing.T) {
	t.Parallel()

	if _, err := pgstore.New(nil); !errors.Is(err, betting.ErrInvalidOptions) {
		t.Errorf("New(nil) returned %v, want betting.ErrInvalidOptions", err)
	}

	store, _ := newStore(t)
	if err := store.InTx(t.Context(), nil); !errors.Is(err, betting.ErrInvalidOptions) {
		t.Errorf("InTx with a nil function returned %v, want betting.ErrInvalidOptions", err)
	}
}

// -----------------------------------------------------------------------------
// Assertions
// -----------------------------------------------------------------------------

// assertSameWager compares two tickets field by field.
//
// Field by field and not reflect.DeepEqual: domain.Wager holds unexported
// time.Time values, and DeepEqual on a time.Time compares the monotonic-clock
// reading and the *time.Location POINTER, so a value that made a round trip
// through the database would never be DeepEqual to the one that went in even
// when every instant is identical. domain.Leg.Equal exists for the same reason
// and says so.
func assertSameWager(t *testing.T, got, want domain.Wager) {
	t.Helper()

	if got.ID() != want.ID() {
		t.Fatalf("id: got %s, want %s", got.ID(), want.ID())
	}
	if got.UserID() != want.UserID() {
		t.Errorf("user: got %s, want %s", got.UserID(), want.UserID())
	}
	if got.Kind() != want.Kind() {
		t.Errorf("kind: got %s, want %s", got.Kind(), want.Kind())
	}
	if got.Status() != want.Status() {
		t.Errorf("status: got %s, want %s", got.Status(), want.Status())
	}
	if got.Stake() != want.Stake() {
		t.Errorf("stake: got %s, want %s", got.Stake(), want.Stake())
	}
	if got.AcceptedDecimal() != want.AcceptedDecimal() {
		t.Errorf("accepted price: got %v, want %v", got.AcceptedDecimal(), want.AcceptedDecimal())
	}
	// The rounding mode decides the payout and is frozen at placement by
	// wagers_assert_transition. A store that wrote a constant here would produce
	// a ticket that pays a different number on settlement than the one quoted.
	if got.Rounding() != want.Rounding() {
		t.Errorf("rounding: got %s, want %s", got.Rounding(), want.Rounding())
	}
	if got.PotentialPayout() != want.PotentialPayout() {
		t.Errorf("potential payout: got %s, want %s", got.PotentialPayout(), want.PotentialPayout())
	}
	if got.PotentialProfit() != want.PotentialProfit() {
		t.Errorf("potential profit: got %s, want %s", got.PotentialProfit(), want.PotentialProfit())
	}
	gotPoints, gotHasPoints := got.TeaserPoints()
	wantPoints, wantHasPoints := want.TeaserPoints()
	if gotHasPoints != wantHasPoints || gotPoints != wantPoints {
		t.Errorf("teaser points: got (%v, %t), want (%v, %t)",
			gotPoints, gotHasPoints, wantPoints, wantHasPoints)
	}
	gotParent, gotHasParent := got.RoundRobinID()
	wantParent, wantHasParent := want.RoundRobinID()
	if gotHasParent != wantHasParent || gotParent != wantParent {
		t.Errorf("round robin: got (%s, %t), want (%s, %t)",
			gotParent, gotHasParent, wantParent, wantHasParent)
	}
	if !got.PlacedAt().Equal(want.PlacedAt()) {
		t.Errorf("placed at: got %s, want %s", got.PlacedAt(), want.PlacedAt())
	}

	gotLegs, wantLegs := got.Legs(), want.Legs()
	if len(gotLegs) != len(wantLegs) {
		t.Fatalf("leg count: got %d, want %d", len(gotLegs), len(wantLegs))
	}
	byID := make(map[domain.LegID]domain.Leg, len(gotLegs))
	for _, l := range gotLegs {
		byID[l.ID()] = l
	}
	for _, w := range wantLegs {
		g, ok := byID[w.ID()]
		if !ok {
			t.Errorf("leg %s is missing from the ticket that came back", w.ID())
			continue
		}
		if !g.Equal(w) {
			t.Errorf("leg %s round-tripped as\n  got  %s\n  want %s", w.ID(), g, w)
		}
	}
}
