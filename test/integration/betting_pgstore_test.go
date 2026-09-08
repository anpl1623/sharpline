package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/betting"
	bettingpg "github.com/anpl1623/sharpline/internal/betting/pgstore"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// internal/betting/pgstore against a real Postgres.
//
// Everything asserted here is a property of the ADAPTER plus the SCHEMA
// together, which is why none of it can be tested with a fake. The four that
// matter most, and which the phase-8 brief calls out by name:
//
//	a balanced movement written through the store COMMITS
//	a deferred violation returns SUCCESS from every statement and fails at COMMIT,
//	  and the error reaches the caller through Store.InTx
//	a replayed placement collides on wagers_pkey and becomes ErrAlreadyPlaced
//	migration 00008's trigger refuses a wager for a self-excluded user
//
// The fixtures are test-owned rows built by these tests for these tests, per the
// rule stated at the top of fixture_test.go. Nothing here is seeded into a
// running system, and no number below stands in for something the pipeline would
// have produced — the prices these wagers are booked at are written by the test
// precisely so the test can assert what was booked.

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// bettingStore wires the adapter over the shared database.
func bettingStore(t *testing.T) (*bettingpg.Store, *postgres.DB) {
	t.Helper()

	db, _ := sharedPool(t)
	store, err := bettingpg.New(db)
	if err != nil {
		t.Fatalf("build betting pgstore: %v", err)
	}
	return store, db
}

// bookedPrice writes a quote into the hypertable and returns it as the
// domain.Price a leg would be booked at.
//
// The row is written as well as returned because [betting.Tx.QuoteFor] reads it
// back — the whole point of that method is that the customer never supplies a
// price — and because a leg's booked price is a COPY of a quote that really
// existed, not a number invented at placement.
func bookedPrice(t *testing.T, ctx context.Context, x execer, sel domain.SelectionID, book domain.BookID, decimal float64, at time.Time) domain.Price {
	t.Helper()

	mustExec(t, ctx, x, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, $3, NULL, $4, $4)`, sel, book, decimal, at)

	price, err := domain.NewPrice(domain.PriceParams{
		SelectionID: sel,
		BookID:      book,
		Decimal:     decimal,
		Line:        domain.NoLine(),
		ObservedAt:  at,
	})
	if err != nil {
		t.Fatalf("NewPrice: %v", err)
	}
	return price
}

// moneylineLeg builds one pending leg on a moneyline market's home side.
func moneylineLeg(t *testing.T, cat catalogue, mkt market, price domain.Price) domain.Leg {
	t.Helper()

	leg, err := domain.NewLeg(domain.LegParams{
		ID:          legID(t, uniqueID("leg")),
		EventID:     cat.EventID,
		MarketID:    mkt.ID,
		MarketType:  domain.MarketTypeMoneyline,
		Role:        domain.SelectionRoleHome,
		SelectionID: mkt.HomeSelection,
		Price:       price,
		TeasedLine:  domain.NoLine(),
	})
	if err != nil {
		t.Fatalf("NewLeg: %v", err)
	}
	return leg
}

// straightWager builds a placeable single-leg ticket.
//
// The accepted price EQUALS the leg's quote, because validateTicketPrice and the
// deferred wagers_shape_at_commit trigger both require it of a straight — the
// two numbers are one value travelling by two routes.
func straightWager(t *testing.T, user domain.UserID, leg domain.Leg, stake domain.Money, at time.Time) domain.Wager {
	t.Helper()

	w, err := domain.NewWager(domain.WagerParams{
		ID:              wagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindStraight,
		Legs:            []domain.Leg{leg},
		Stake:           stake,
		AcceptedDecimal: leg.QuotedDecimal(),
		Rounding:        domain.RoundHalfAwayFromZero,
		PlacedAt:        at,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}
	return w
}

// grant credits a customer's cash account from issuance, through the store.
//
// Deliberately written through [betting.Tx.InsertTransaction] rather than raw
// SQL: the balance the placement tests then fold has to have arrived by the same
// route a real grant does, or the fold is proving something about the fixture.
func grant(t *testing.T, ctx context.Context, store *bettingpg.Store, user domain.UserID, amount domain.Money, at time.Time) {
	t.Helper()

	txn, err := domain.NewGrantTransaction(transactionID(t, uniqueID("txn")), user, amount, at)
	if err != nil {
		t.Fatalf("NewGrantTransaction: %v", err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertTransaction(ctx, txn)
	}); err != nil {
		t.Fatalf("grant %s to %s: %v", amount, user, err)
	}
}

// -----------------------------------------------------------------------------
// The happy path, end to end
// -----------------------------------------------------------------------------

// TestABalancedPlacementCommitsThroughTheStoreAndReadsBack is the positive case
// the other tests are variations of.
//
// It drives one placement the way internal/betting does — one Store.InTx, a
// wager and its leg, a stake movement, one COMMIT — and then reads the ticket
// back through the store and checks that what comes out is what went in.
//
// The read-back is the half that earns its place. Writing is easy to get right
// by accident; REHYDRATING is not, because the domain has no rehydration
// constructor and HydrateWager has to replay the ticket's transitions to
// reconstruct it. A round trip is the only way to assert that replay actually
// reproduces the value.
func TestABalancedPlacementCommitsThroughTheStoreAndReadsBack(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, db := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)

	at := time.Now().UTC().Truncate(time.Microsecond)
	price := bookedPrice(t, ctx, conn, mkt.HomeSelection, cat.BookID, 2.5, at)
	leg := moneylineLeg(t, cat, mkt, price)
	w := straightWager(t, user, leg, domain.Money(5_000), at)

	grant(t, ctx, store, user, domain.Money(50_000), at)

	stake, err := domain.NewStakeTransaction(transactionID(t, uniqueID("txn")), w, at)
	if err != nil {
		t.Fatalf("NewStakeTransaction: %v", err)
	}

	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		if err := tx.InsertWager(ctx, w); err != nil {
			return err
		}
		return tx.InsertTransaction(ctx, stake)
	}); err != nil {
		t.Fatalf("placement transaction: %v", err)
	}

	got, err := store.WagerByID(ctx, w.ID())
	if err != nil {
		t.Fatalf("read the wager back: %v", err)
	}

	if got.ID() != w.ID() {
		t.Errorf("id: got %s, want %s", got.ID(), w.ID())
	}
	if got.Status() != w.Status() {
		t.Errorf("status: got %s, want %s", got.Status(), w.Status())
	}
	if got.Stake() != w.Stake() {
		t.Errorf("stake: got %s, want %s", got.Stake(), w.Stake())
	}
	if got.PotentialPayout() != w.PotentialPayout() {
		t.Errorf("payout: got %s, want %s", got.PotentialPayout(), w.PotentialPayout())
	}
	if got.AcceptedDecimal() != w.AcceptedDecimal() {
		t.Errorf("accepted price: got %v, want %v", got.AcceptedDecimal(), w.AcceptedDecimal())
	}
	if got.Rounding() != w.Rounding() {
		t.Errorf("rounding: got %s, want %s", got.Rounding(), w.Rounding())
	}
	if !got.PlacedAt().Equal(w.PlacedAt()) {
		t.Errorf("placed at: got %s, want %s", got.PlacedAt(), w.PlacedAt())
	}
	if got.LegCount() != 1 {
		t.Fatalf("leg count: got %d, want 1", got.LegCount())
	}

	// The booked price must survive the round trip EXACTLY. CLAUDE.md §4: a leg
	// holds the price at placement time and nothing may re-resolve it.
	back, ok := got.Leg(leg.ID())
	if !ok {
		t.Fatalf("leg %s is missing from the rehydrated wager", leg.ID())
	}
	if !back.Price().Equal(price) {
		t.Errorf("booked price: got %s, want %s", back.Price(), price)
	}

	// The stake left cash and arrived in escrow, and both are folds over
	// ledger_entries rather than stored columns.
	assertBalances(t, ctx, store, user, domain.Money(45_000), domain.Money(5_000))

	// Nothing above should have disturbed the pool's own health.
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping after placement: %v", err)
	}
}

// assertBalances folds both of a customer's accounts through the store.
func assertBalances(t *testing.T, ctx context.Context, store *bettingpg.Store, user domain.UserID, wantCash, wantEscrow domain.Money) {
	t.Helper()

	cash, err := domain.UserCashAccount(user)
	if err != nil {
		t.Fatalf("UserCashAccount: %v", err)
	}
	escrow, err := domain.UserEscrowAccount(user)
	if err != nil {
		t.Fatalf("UserEscrowAccount: %v", err)
	}

	err = store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		gotCash, err := tx.Balance(ctx, cash)
		if err != nil {
			return err
		}
		gotEscrow, err := tx.Balance(ctx, escrow)
		if err != nil {
			return err
		}
		if gotCash != wantCash {
			t.Errorf("cash balance: got %s, want %s", gotCash, wantCash)
		}
		if gotEscrow != wantEscrow {
			t.Errorf("escrow balance: got %s, want %s", gotEscrow, wantEscrow)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fold balances: %v", err)
	}
}

// TestAnUntouchedAccountFoldsToZeroRatherThanNoRows is the mapping the phase-8
// brief singles out, and it is one line of adapter code standing between a
// brand-new customer and a placement that fails with "no rows in result set".
//
// account_balances reports one row per account that has ever been touched, so an
// account with no entries produces pgx.ErrNoRows. Migration 00006 keeps that
// distinction on purpose — "touched and nets to nothing" is a different fact
// from "never touched" — which means the adapter, not the view, is where zero
// has to come from.
func TestAnUntouchedAccountFoldsToZeroRatherThanNoRows(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)
	user := newUser(t, ctx, conn)

	assertBalances(t, ctx, store, user, 0, 0)
}

// -----------------------------------------------------------------------------
// The deferred constraint, through Store.InTx
// -----------------------------------------------------------------------------

// TestADeferredShapeViolationFailsAtCommitAndSurfacesThroughStoreInTx is the
// test this file exists for, and it is the phase-8 brief's "an unbalanced write
// is rejected AT COMMIT and the error reaches the caller through InTx" asserted
// against the store rather than against the transaction helper.
//
// # Why a SHAPE violation and not an unbalanced LEDGER
//
// An unbalanced ledger movement is UNREACHABLE through this store, and that is a
// result rather than a gap: [betting.Tx.InsertTransaction] takes a
// domain.Transaction, and domain.NewTransaction refuses anything that is not at
// least two non-zero entries summing to exactly zero. There is no expression in
// this package that can produce one. The raw-SQL proof that the database and
// postgres.InTx do the right thing with an unbalanced movement lives in
// ledger_test.go, which drives it three ways.
//
// What IS reachable through the store, using nothing but valid domain values, is
// migration 00006's OTHER deferred trigger: a round robin declared "by 2s" and a
// ticket carrying three legs. domain.NewRoundRobin accepts sizes {2} on three
// selections, domain.NewWager accepts a three-leg round-robin ticket, and
// wagers_shape_at_commit rejects the PAIR — at COMMIT, because a wager and its
// legs are separate statements and the rule is about the set of them.
//
// Three things are asserted, and all three matter:
//
//	every statement inside the callback SUCCEEDS
//	the callback returns nil, and Store.InTx STILL returns an error
//	nothing was written
//
// The second is the one that would be silently wrong in a hand-rolled
// Begin/Commit that dropped the commit error, and it is exactly the failure mode
// postgres/tx.go was written to prevent.
func TestADeferredShapeViolationFailsAtCommitAndSurfacesThroughStoreInTx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	user := newUser(t, ctx, conn)
	at := time.Now().UTC().Truncate(time.Microsecond)

	legs := make([]domain.Leg, 0, 3)
	for range 3 {
		mkt := newMoneylineMarket(t, ctx, conn, cat)
		price := bookedPrice(t, ctx, conn, mkt.HomeSelection, cat.BookID, 2.0, at)
		legs = append(legs, moneylineLeg(t, cat, mkt, price))
	}

	stake := domain.Money(1_000)
	rr, err := domain.NewRoundRobin(domain.RoundRobinParams{
		ID:                  roundRobinID(t, uniqueID("rr")),
		UserID:              user,
		Legs:                legs,
		Sizes:               []int{2}, // "by 2s" -- a three-leg ticket is not a combination
		StakePerCombination: stake,
		PlacedAt:            at,
	})
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	ticket, err := domain.NewWager(domain.WagerParams{
		ID:              wagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindRoundRobin,
		Legs:            legs, // three legs, against sizes {2}
		Stake:           stake,
		AcceptedDecimal: 8.0, // 2.0 ^ 3; a parlay price, and irrelevant to the rule under test
		Rounding:        domain.RoundHalfAwayFromZero,
		RoundRobinID:    rr.ID(),
		PlacedAt:        at,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}

	callbackReturnedNil := false
	err = store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		if err := tx.InsertRoundRobin(ctx, rr); err != nil {
			t.Errorf("InsertRoundRobin failed INSIDE the transaction; the deferred trigger "+
				"should not have fired until COMMIT: %v", err)
			return err
		}
		if err := tx.InsertWager(ctx, ticket); err != nil {
			t.Errorf("InsertWager failed INSIDE the transaction; the deferred trigger should "+
				"not have fired until COMMIT: %v", err)
			return err
		}
		callbackReturnedNil = true
		return nil
	})

	if !callbackReturnedNil {
		t.Fatal("the callback did not reach its end; the statements were expected to succeed " +
			"and the COMMIT was expected to fail")
	}
	if err == nil {
		t.Fatal("Store.InTx returned nil for a transaction the database REFUSED at COMMIT. " +
			"A wager that violates wagers_shape_at_commit would have been reported as placed.")
	}
	if state := postgres.SQLState(err); state != "23514" {
		t.Errorf("SQLSTATE: got %q, want 23514 (check_violation from wagers_assert_shape); "+
			"error was: %v", state, err)
	}

	// Nothing was written. Postgres rolls back a transaction whose COMMIT fails,
	// so the ticket must not be readable.
	if _, err := store.WagerByID(ctx, ticket.ID()); !errors.Is(err, betting.ErrWagerNotFound) {
		t.Errorf("after a failed COMMIT the wager is still readable: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Idempotency
// -----------------------------------------------------------------------------

// TestAReplayedPlacementBecomesErrAlreadyPlaced is decision 1 of the phase-8
// brief, asserted against the mechanism rather than against a mock.
//
// The wager id is derived from the idempotency key, so a replayed submit inserts
// the SAME primary key. That is a 23505 on wagers_pkey, and the adapter's job is
// to turn it into betting.ErrAlreadyPlaced so the service can answer by reading
// the existing ticket back rather than by returning an error.
//
// The read-back is asserted too, because "report a replay" and "report the right
// ticket" are different claims and only the second one is useful.
func TestAReplayedPlacementBecomesErrAlreadyPlaced(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)

	at := time.Now().UTC().Truncate(time.Microsecond)
	price := bookedPrice(t, ctx, conn, mkt.HomeSelection, cat.BookID, 1.91, at)
	w := straightWager(t, user, moneylineLeg(t, cat, mkt, price), domain.Money(2_500), at)

	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertWager(ctx, w)
	}); err != nil {
		t.Fatalf("first placement: %v", err)
	}

	var replay error
	var existing domain.Wager
	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		replay = tx.InsertWager(ctx, w)
		if !errors.Is(replay, betting.ErrAlreadyPlaced) {
			return nil
		}
		// The service's own answer to a replay: read the ticket back and return
		// it. It has to work in the SAME transaction the collision happened in,
		// which is why it is asserted here rather than after the rollback.
		var err error
		existing, err = tx.WagerByID(ctx, w.ID())
		return err
	})
	if err != nil {
		t.Fatalf("replay transaction: %v", err)
	}
	if !errors.Is(replay, betting.ErrAlreadyPlaced) {
		t.Fatalf("a replayed placement reported %v, want betting.ErrAlreadyPlaced", replay)
	}
	if existing.ID() != w.ID() {
		t.Errorf("the replay read back wager %s, want %s", existing.ID(), w.ID())
	}
}

// TestAReplayedRoundRobinParentIsReportedAsAReplayAndLeavesTheTxUsable covers
// the OTHER insert a replay collides on, and it is here because both halves of
// it were broken and neither was caught.
//
// A round robin is placed as a parent row plus one ticket per combination.
// internal/betting/placement.go handles a replayed round robin by logging the
// parent collision and FALLING THROUGH to the per-ticket inserts, each of which
// reports its own duplicate and reads back. Two things have to be true for that
// to work, and this test asserts them separately because they failed separately:
//
//	the parent's 23505 is reported as betting.ErrAlreadyPlaced
//	    -- InsertRoundRobin used to wrap the driver error verbatim, so the
//	       service's errors.Is(err, ErrAlreadyPlaced) branch was DEAD CODE and a
//	       replayed round robin surfaced a raw unique violation.
//
//	the transaction SURVIVES that collision
//	    -- PostgreSQL aborts a transaction on any statement error, so without a
//	       SAVEPOINT the fall-through inserts all fail with 25P02 and the replay
//	       returns a database error rather than the customer's existing tickets.
//
// The second assertion is the load-bearing one: it is written as a real
// subsequent INSERT that must succeed and COMMIT, rather than as a second read,
// because a read is what the wager path does and this path does something the
// wager path never does — it keeps WRITING after the collision.
func TestAReplayedRoundRobinParentIsReportedAsAReplayAndLeavesTheTxUsable(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	user := newUser(t, ctx, conn)
	at := time.Now().UTC().Truncate(time.Microsecond)

	legs := make([]domain.Leg, 0, 3)
	for range 3 {
		mkt := newMoneylineMarket(t, ctx, conn, cat)
		price := bookedPrice(t, ctx, conn, mkt.HomeSelection, cat.BookID, 2.0, at)
		legs = append(legs, moneylineLeg(t, cat, mkt, price))
	}

	stake := domain.Money(1_000)
	rr, err := domain.NewRoundRobin(domain.RoundRobinParams{
		ID:                  roundRobinID(t, uniqueID("rr")),
		UserID:              user,
		Legs:                legs,
		Sizes:               []int{2},
		StakePerCombination: stake,
		PlacedAt:            at,
	})
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertRoundRobin(ctx, rr)
	}); err != nil {
		t.Fatalf("first round robin placement: %v", err)
	}

	// One combination, booked as a ticket. This is the write the service does
	// AFTER the parent collision, and it is the thing 25P02 would have killed.
	combination := rr.Combinations()[0]
	ticket, err := domain.NewWager(domain.WagerParams{
		ID:              wagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindRoundRobin,
		Legs:            combination,
		Stake:           stake,
		AcceptedDecimal: 4.0, // 2.0 ^ 2, the two legs of this combination
		Rounding:        domain.RoundHalfAwayFromZero,
		RoundRobinID:    rr.ID(),
		PlacedAt:        at,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}

	var replay error
	err = store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		replay = tx.InsertRoundRobin(ctx, rr)
		if !errors.Is(replay, betting.ErrAlreadyPlaced) {
			return nil
		}
		return tx.InsertWager(ctx, ticket)
	})
	if err != nil {
		t.Fatalf("the transaction did not survive the parent collision: %v", err)
	}
	if !errors.Is(replay, betting.ErrAlreadyPlaced) {
		t.Fatalf("a replayed round robin parent reported %v, want betting.ErrAlreadyPlaced", replay)
	}

	// The fall-through write COMMITTED. Reading it back proves the savepoint
	// released rather than silently discarding everything after it.
	if _, err := store.WagerByID(ctx, ticket.ID()); err != nil {
		t.Errorf("the ticket written after the parent collision is not readable: %v", err)
	}
}

// TestALegIDCollisionIsNotReportedAsAReplay is the negative half, and it guards
// a real hazard migration 00006 names by hand: RoundRobin.Combinations() returns
// subsets of the SAME []Leg values, so a betting service that passed them
// through verbatim would insert one LegID twice and violate legs' primary key.
//
// That is a 23505, like a replay, and it means something completely different.
// Reporting it as ErrAlreadyPlaced would send the service to read a wager that
// was never written, and the failure would surface as an incoherent error far
// from its cause. The adapter inspects the CONSTRAINT NAME rather than the
// SQLSTATE for exactly this reason.
func TestALegIDCollisionIsNotReportedAsAReplay(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	first := newMoneylineMarket(t, ctx, conn, cat)
	second := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)
	at := time.Now().UTC().Truncate(time.Microsecond)

	priceA := bookedPrice(t, ctx, conn, first.HomeSelection, cat.BookID, 2.0, at)
	legA := moneylineLeg(t, cat, first, priceA)
	wagerA := straightWager(t, user, legA, domain.Money(1_000), at)

	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertWager(ctx, wagerA)
	}); err != nil {
		t.Fatalf("first placement: %v", err)
	}

	// A DIFFERENT ticket that re-uses the first ticket's leg id — the mistake the
	// round-robin expansion would make.
	priceB := bookedPrice(t, ctx, conn, second.HomeSelection, cat.BookID, 2.0, at)
	legB, err := domain.NewLeg(domain.LegParams{
		ID:          legA.ID(), // the collision
		EventID:     cat.EventID,
		MarketID:    second.ID,
		MarketType:  domain.MarketTypeMoneyline,
		Role:        domain.SelectionRoleHome,
		SelectionID: second.HomeSelection,
		Price:       priceB,
		TeasedLine:  domain.NoLine(),
	})
	if err != nil {
		t.Fatalf("NewLeg: %v", err)
	}
	wagerB := straightWager(t, user, legB, domain.Money(1_000), at)

	var insertErr error
	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		insertErr = tx.InsertWager(ctx, wagerB)
		return errors.New("roll back; the assertion is on insertErr")
	}); err == nil {
		t.Fatal("expected the deliberate rollback error")
	}

	if insertErr == nil {
		t.Fatal("re-using a LegID across two wagers was accepted; legs_pkey should have refused it")
	}
	if errors.Is(insertErr, betting.ErrAlreadyPlaced) {
		t.Errorf("a legs_pkey collision was reported as a replayed placement, which would send "+
			"the service to read a wager that does not exist: %v", insertErr)
	}
	if state := postgres.SQLState(insertErr); state != "23505" {
		t.Errorf("SQLSTATE: got %q, want 23505; error was: %v", state, insertErr)
	}
}

// -----------------------------------------------------------------------------
// Self-exclusion: migration 00008
// -----------------------------------------------------------------------------

// TestTheSelfExclusionTriggerRefusesAWagerAtTheDatabase is decision 2c: no
// route at all, including raw SQL, can book a bet for an excluded customer.
//
// The service layer is the authoritative check and is not exercised here; this
// asserts the BACKSTOP, by inserting through the adapter with no status read in
// front of it. That is precisely the caller migration 00008 exists to stop.
//
// The three statuses are asserted TOGETHER, because the interesting part of the
// migration is what it does NOT refuse. 'suspended' is deliberately permitted at
// the database so an operator correction during a review does not require
// lifting the suspension — the customer-facing route is closed at the service,
// where auth.UserStatus.CanWager() is false for it.
func TestTheSelfExclusionTriggerRefusesAWagerAtTheDatabase(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)
	cat := newCatalogue(t, ctx, conn)

	cases := []struct {
		status  auth.UserStatus
		refused bool
		why     string
	}{
		{auth.UserStatusActive, false, "an active account is the ordinary case"},
		{auth.UserStatusSelfExcluded, true, "the customer's own responsible-gaming control"},
		{auth.UserStatusClosed, true, "a closed account cannot authenticate, so nothing may bet for it"},
		{auth.UserStatusSuspended, false,
			"deliberately permitted at the database: the operator correction path must not " +
				"require lifting the suspension. The service still refuses it."},
	}

	for _, tc := range cases {
		t.Run(tc.status.String(), func(t *testing.T) {
			mkt := newMoneylineMarket(t, ctx, conn, cat)
			user := newUser(t, ctx, conn)
			mustExec(t, ctx, conn, `UPDATE users SET status = $1 WHERE id = $2`, tc.status.String(), user)

			at := time.Now().UTC().Truncate(time.Microsecond)
			price := bookedPrice(t, ctx, conn, mkt.HomeSelection, cat.BookID, 2.0, at)
			w := straightWager(t, user, moneylineLeg(t, cat, mkt, price), domain.Money(1_000), at)

			err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
				return tx.InsertWager(ctx, w)
			})

			if tc.refused {
				if err == nil {
					t.Fatalf("a %s account was booked a wager; migration 00008's "+
						"wagers_refuse_excluded_user should have refused it (%s)", tc.status, tc.why)
				}
				if !errors.Is(err, betting.ErrAccountNotWagerable) {
					t.Errorf("refusal did not carry betting.ErrAccountNotWagerable: %v", err)
				}
				if state := postgres.SQLState(err); state != "23001" {
					t.Errorf("SQLSTATE: got %q, want 23001 (restrict_violation); error was: %v", state, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("a %s account was refused a wager, and should not have been (%s): %v",
					tc.status, tc.why, err)
			}
		})
	}
}

// TestUserStatusIsReadableInsideThePlacementTransaction covers the gate the
// service actually uses, and the fact that it takes FOR UPDATE.
//
// The lock is what serialises one customer's concurrent placements, which is the
// only thing standing between two slips and an overdraft — neither the balance
// fold nor the limit sums has a constraint behind it. There is no non-flaky way
// to assert the serialisation itself in a single-process test, so what is
// asserted here is that the locking read WORKS and returns the stored value; the
// lock's presence is a property of the query text, which `sqlc diff` pins.
func TestUserStatusIsReadableInsideThePlacementTransaction(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)
	user := newUser(t, ctx, conn)
	mustExec(t, ctx, conn, `UPDATE users SET status = 'self_excluded' WHERE id = $1`, user)

	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		status, err := tx.UserStatus(ctx, user)
		if err != nil {
			return err
		}
		if status != auth.UserStatusSelfExcluded.String() {
			t.Errorf("status: got %q, want %q", status, auth.UserStatusSelfExcluded)
		}
		parsed, err := auth.ParseUserStatus(status)
		if err != nil {
			t.Errorf("the stored status does not parse: %v", err)
		}
		if parsed.CanWager() {
			t.Error("a self-excluded account reported CanWager() == true")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read user status: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Responsible gaming
// -----------------------------------------------------------------------------

// TestLimitsInForceReturnsTheTighterRowDuringACoolingOffPeriod is the whole
// reason GetCurrentLimits is not `superseded_at IS NULL`.
//
// Migration 00005 makes the limit history append-only and ASYMMETRIC: a
// tightening binds immediately, a loosening only after a cooling-off period. So
// between the request and the expiry, the row with superseded_at IS NULL is the
// LOOSER limit that is not yet in force, and the row that actually binds is the
// superseded, tighter one. A store that read the current row would leave the
// customer effectively unlimited for the whole cooling-off window — which is the
// one window the control exists to cover.
func TestLimitsInForceReturnsTheTighterRowDuringACoolingOffPeriod(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)
	user := newUser(t, ctx, conn)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tightenedAt := now.Add(-48 * time.Hour)
	loosenedFrom := now.Add(24 * time.Hour)

	// The tightening: in force since two days ago, closed at the instant the
	// loosening will bind. That stamping rule is the write side's obligation and
	// ports.go states it — closing the old row at now() instead would open a gap
	// with NO limit in force, which is worse than either row.
	mustExec(t, ctx, conn, `
INSERT INTO user_limits (id, user_id, kind, period, amount_minor, requested_at, effective_from, superseded_at)
VALUES ($1, $2, 'stake', 'day', 10000, $3, $3, $4)`,
		uniqueID("lim"), user, tightenedAt, loosenedFrom)

	// The loosening: requested now, binding tomorrow. It is the CURRENT row.
	mustExec(t, ctx, conn, `
INSERT INTO user_limits (id, user_id, kind, period, amount_minor, requested_at, effective_from)
VALUES ($1, $2, 'stake', 'day', 500000, $3, $4)`,
		uniqueID("lim"), user, now, loosenedFrom)

	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		limits, err := tx.LimitsInForce(ctx, user, now)
		if err != nil {
			return err
		}
		if len(limits) != 1 {
			t.Fatalf("limits in force: got %d rows, want 1: %+v", len(limits), limits)
		}
		got := limits[0]
		if got.Kind != auth.LimitKindStake || got.Period != auth.LimitPeriodDay {
			t.Errorf("limit: got %s/%s, want stake/day", got.Kind, got.Period)
		}
		if got.Amount != domain.Money(10_000) {
			t.Errorf("amount: got %s, want the TIGHTER limit of 100.00; the pending loosening "+
				"is not in force until %s", got.Amount, loosenedFrom)
		}

		// After the cooling-off expires the looser row binds, with no gap.
		later, err := tx.LimitsInForce(ctx, user, loosenedFrom)
		if err != nil {
			return err
		}
		if len(later) != 1 || later[0].Amount != domain.Money(500_000) {
			t.Errorf("after the cooling-off: got %+v, want a single 5000.00 stake limit", later)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read limits: %v", err)
	}
}

// TestSumEntriesIsTheSignedFoldTheLimitCheckCompares covers the enforcement
// query and the two properties the adapter adds to it.
//
// The sum is SIGNED — a stake sum over user_cash is negative, because the cash
// account was debited — and the adapter returns it unchanged rather than
// wrapping it in abs(). The sign convention is the ledger's own and a second
// spelling of it is how two parts of a codebase come to disagree about which way
// money moves. And the total goes through domain.FromMinorUnits, which is the
// only bound check a SUM gets: every stored amount is bounded by a CHECK, their
// total is not.
func TestSumEntriesIsTheSignedFoldTheLimitCheckCompares(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)
	user := newUser(t, ctx, conn)
	at := time.Now().UTC().Truncate(time.Microsecond)

	grant(t, ctx, store, user, domain.Money(20_000), at)

	price := bookedPrice(t, ctx, conn, mkt.HomeSelection, cat.BookID, 2.0, at)
	w := straightWager(t, user, moneylineLeg(t, cat, mkt, price), domain.Money(3_000), at)
	stake, err := domain.NewStakeTransaction(transactionID(t, uniqueID("txn")), w, at)
	if err != nil {
		t.Fatalf("NewStakeTransaction: %v", err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		if err := tx.InsertWager(ctx, w); err != nil {
			return err
		}
		return tx.InsertTransaction(ctx, stake)
	}); err != nil {
		t.Fatalf("placement: %v", err)
	}

	cash, err := domain.UserCashAccount(user)
	if err != nil {
		t.Fatalf("UserCashAccount: %v", err)
	}
	since := at.Add(-time.Hour)

	err = store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		staked, err := tx.SumEntries(ctx, cash, []domain.EntryKind{domain.EntryKindStake}, since)
		if err != nil {
			return err
		}
		if staked != domain.Money(-3_000) {
			t.Errorf("stake sum over cash: got %s, want -30.00 (SIGNED: the cash account was "+
				"debited)", staked)
		}

		granted, err := tx.SumEntries(ctx, cash, []domain.EntryKind{domain.EntryKindGrant}, since)
		if err != nil {
			return err
		}
		if granted != domain.Money(20_000) {
			t.Errorf("grant sum over cash: got %s, want 200.00", granted)
		}

		// The loss limit is a NET over several kinds at once, which is why the
		// port takes a set rather than one value.
		net, err := tx.SumEntries(ctx, cash, []domain.EntryKind{
			domain.EntryKindStake, domain.EntryKindPayout,
			domain.EntryKindRefund, domain.EntryKindCashOut,
		}, since)
		if err != nil {
			return err
		}
		if net != domain.Money(-3_000) {
			t.Errorf("net over the loss kinds: got %s, want -30.00", net)
		}

		// An empty kind set sums to zero, which reads as "nothing staked" and
		// permits everything. It is refused rather than answered.
		if _, err := tx.SumEntries(ctx, cash, nil, since); !errors.Is(err, betting.ErrInvalidOptions) {
			t.Errorf("an empty kind set returned %v, want betting.ErrInvalidOptions", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sum entries: %v", err)
	}
}

// TestASystemAccountIsRefusedRatherThanFoldedToZero guards the one mapping that
// would be wrong in the direction that looks right.
//
// The house and issuance singletons carry a NULL owner, and `account_user_id =
// NULL` is NULL and never true — so the query returns no row and the ErrNoRows
// mapping would report their balance as ZERO. A fresh house account really is
// zero, so the wrong answer is indistinguishable from the right one until it is
// not.
func TestASystemAccountIsRefusedRatherThanFoldedToZero(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)

	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		if _, err := tx.Balance(ctx, domain.HouseAccount()); !errors.Is(err, betting.ErrInvalidOptions) {
			t.Errorf("house balance returned %v, want betting.ErrInvalidOptions", err)
		}
		if _, err := tx.Balance(ctx, domain.IssuanceAccount()); !errors.Is(err, betting.ErrInvalidOptions) {
			t.Errorf("issuance balance returned %v, want betting.ErrInvalidOptions", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("system account probe: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Catalogue reads
// -----------------------------------------------------------------------------

// TestQuoteForReturnsTheNewestQuoteWithEverythingALegNeeds covers the one route
// by which a price reaches a leg.
//
// The customer names a selection and a book; the number they are booked at is
// whatever this read returns. Asserting that it returns the NEWEST quote is the
// substance — a slip validated against a stale row is a customer booked at a
// price the market has left — and asserting that the market, event, type and
// role travel with it is what lets domain.NewLeg be called without four more
// round trips.
func TestQuoteForReturnsTheNewestQuoteWithEverythingALegNeeds(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)

	at := time.Now().UTC().Truncate(time.Microsecond)
	bookedPrice(t, ctx, conn, mkt.HomeSelection, cat.BookID, 2.10, at.Add(-10*time.Minute))
	newest := bookedPrice(t, ctx, conn, mkt.HomeSelection, cat.BookID, 1.87, at)

	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		quote, err := tx.QuoteFor(ctx, mkt.HomeSelection, cat.BookID)
		if err != nil {
			return err
		}
		if !quote.Price.Equal(newest) {
			t.Errorf("quote: got %s, want the newest %s", quote.Price, newest)
		}
		if quote.MarketID != mkt.ID || quote.EventID != cat.EventID {
			t.Errorf("catalogue context: got market %s / event %s, want %s / %s",
				quote.MarketID, quote.EventID, mkt.ID, cat.EventID)
		}
		if quote.MarketType != domain.MarketTypeMoneyline || quote.Role != domain.SelectionRoleHome {
			t.Errorf("shape: got %s/%s, want moneyline/home", quote.MarketType, quote.Role)
		}

		// A book that has never quoted the selection is an ordinary refusal, not
		// a fault: not every book prices every market.
		absent := bookID(t, uniqueID("book"))
		if _, err := tx.QuoteFor(ctx, mkt.HomeSelection, absent); !errors.Is(err, betting.ErrQuoteUnavailable) {
			t.Errorf("an unquoted book returned %v, want betting.ErrQuoteUnavailable", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("quote read: %v", err)
	}
}

// TestMarketStateReadsBothStatusesIndependently is the slip's other validation
// read.
//
// Two statuses rather than one because they are independent: an admin may
// suspend a single market while the event trades on, so reading either alone
// answers the wrong question. A market that does not exist is reported as
// ErrMarketNotOpen and not as a not-found of its own, because from the slip's
// point of view a vanished market and a closed one are the same refusal.
func TestMarketStateReadsBothStatusesIndependently(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, _ := bettingStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)

	// The ordinary in-play case: the market is suspended for a scoring play
	// while its event is live.
	mustExec(t, ctx, conn, `UPDATE markets SET status = 'suspended' WHERE id = $1`, mkt.ID)
	mustExec(t, ctx, conn, `UPDATE events SET status = 'live' WHERE id = $1`, cat.EventID)

	err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		state, err := tx.MarketState(ctx, mkt.ID)
		if err != nil {
			return err
		}
		if state.Status != domain.MarketStatusSuspended {
			t.Errorf("market status: got %s, want suspended", state.Status)
		}
		if state.EventStatus != domain.EventStatusLive {
			t.Errorf("event status: got %s, want live", state.EventStatus)
		}
		if state.EventID != cat.EventID {
			t.Errorf("event: got %s, want %s", state.EventID, cat.EventID)
		}
		if !state.ScheduledStart.Equal(cat.Start) {
			t.Errorf("scheduled start: got %s, want %s", state.ScheduledStart, cat.Start)
		}

		absent := marketID(t, uniqueID("market"))
		if _, err := tx.MarketState(ctx, absent); !errors.Is(err, betting.ErrMarketNotOpen) {
			t.Errorf("a missing market returned %v, want betting.ErrMarketNotOpen", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("market state read: %v", err)
	}
}
