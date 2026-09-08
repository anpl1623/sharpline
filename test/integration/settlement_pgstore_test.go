package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/betting"
	bettingpg "github.com/anpl1623/sharpline/internal/betting/pgstore"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/settlement"
	settlementpg "github.com/anpl1623/sharpline/internal/settlement/pgstore"
)

// internal/settlement/pgstore against a real Postgres.
//
// The property under test throughout is the ROWS-AFFECTED CONTRACT. Every write
// in settlement.sql is an UPDATE guarded by the status it expects to find, and
// internal/settlement/ports.go states what happens if an adapter gets it wrong:
// "Returning nil from either on a zero-row update is the single most dangerous
// thing an implementation of this interface can do, because the ledger write
// that follows would balance perfectly and pay twice."
//
// A fake cannot prove that. The guard's behaviour under a concurrent update is a
// property of PostgreSQL's EvalPlanQual re-check, and the immediate row triggers
// that back it up are in migration 00006. Both need the real engine.

// settlementStore wires the settlement adapter over the shared database.
func settlementStore(t *testing.T) *settlementpg.Store {
	t.Helper()

	db, _ := sharedPool(t)
	store, err := settlementpg.NewStore(db)
	if err != nil {
		t.Fatalf("build settlement pgstore: %v", err)
	}
	return store
}

// placeStraight writes one single-leg ticket through the betting adapter and
// returns the domain value that was written.
//
// It goes through the placement path rather than raw SQL on purpose: a grading
// test that built its own rows would be grading a fixture, and the interesting
// question is whether a ticket the placement path produced can be settled by the
// settlement path.
func placeStraight(t *testing.T, ctx context.Context, x execer, store *bettingpg.Store, cat catalogue, decimal float64, stake domain.Money, at time.Time) domain.Wager {
	t.Helper()

	mkt := newMoneylineMarket(t, ctx, x, cat)
	user := newUser(t, ctx, x)
	price := bookedPrice(t, ctx, x, mkt.HomeSelection, cat.BookID, decimal, at)
	w := straightWager(t, user, moneylineLeg(t, cat, mkt, price), stake, at)

	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertWager(ctx, w)
	}); err != nil {
		t.Fatalf("place straight wager: %v", err)
	}
	return w
}

// -----------------------------------------------------------------------------
// The rows-affected contract
// -----------------------------------------------------------------------------

// TestGradingAndSettlementAreIdempotentUnderRedelivery is the test this file
// exists for.
//
// CLAUDE.md §3 puts Kafka at at-least-once and the results feed's boundary is
// deliberately inclusive, so `settle` WILL be handed the same result twice as a
// matter of course. The second pass must be a NO-OP that says so — not an error
// the consumer retries, and above all not a success the consumer pays out on.
//
// Both guarded UPDATEs are driven twice and the second call of each is checked
// for its sentinel. The ledger movement is written once, in the same transaction
// as the first settlement, so the balances afterwards are the assertion that
// nothing paid twice.
func TestGradingAndSettlementAreIdempotentUnderRedelivery(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	betStore, _ := bettingStore(t)
	setStore := settlementStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	at := time.Now().UTC().Truncate(time.Microsecond)
	w := placeStraight(t, ctx, conn, betStore, cat, 2.0, domain.Money(4_000), at)
	user := w.UserID()

	grant(t, ctx, betStore, user, domain.Money(40_000), at)

	// Move the stake into escrow, so the settlement below has something to
	// release. This is the placement's own movement, written after the fact here
	// only because this test placed the wager before it granted.
	stakeTxn, err := domain.NewStakeTransaction(transactionID(t, uniqueID("txn")), w, at)
	if err != nil {
		t.Fatalf("NewStakeTransaction: %v", err)
	}
	if err := betStore.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertTransaction(ctx, stakeTxn)
	}); err != nil {
		t.Fatalf("stake movement: %v", err)
	}

	gradedAt := at.Add(2 * time.Hour)
	leg := w.Legs()[0]

	settled, err := w.GradeLeg(leg.ID(), domain.LegStatusWon, gradedAt)
	if err != nil {
		t.Fatalf("GradeLeg: %v", err)
	}
	settled, err = settled.Settle(domain.WagerStatusWon, w.PotentialPayout(), gradedAt)
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	payout, err := domain.NewSettlementTransaction(transactionID(t, uniqueID("txn")), settled, gradedAt)
	if err != nil {
		t.Fatalf("NewSettlementTransaction: %v", err)
	}

	// First pass: everything applies.
	if err := setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
		locked, err := tx.WagerWithLegs(ctx, w.ID())
		if err != nil {
			return err
		}
		if locked.Status() != domain.WagerStatusPlaced {
			t.Errorf("locked wager status: got %s, want placed", locked.Status())
		}
		if err := tx.GradeLeg(ctx, leg.ID(), domain.LegStatusWon, gradedAt); err != nil {
			return err
		}
		if err := tx.SettleWager(ctx, settled); err != nil {
			return err
		}
		return tx.InsertTransaction(ctx, payout)
	}); err != nil {
		t.Fatalf("first settlement: %v", err)
	}

	// Second pass: the redelivery. Every guarded write must report that somebody
	// else already did the work, and the ledger movement must collide on its
	// derived primary key rather than pay again.
	var gradeErr, settleErr, ledgerErr error
	err = setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
		gradeErr = tx.GradeLeg(ctx, leg.ID(), domain.LegStatusWon, gradedAt)
		settleErr = tx.SettleWager(ctx, settled)
		ledgerErr = tx.InsertTransaction(ctx, payout)
		return errors.New("roll back; the assertions are on the three sentinels")
	})
	if err == nil {
		t.Fatal("expected the deliberate rollback error")
	}

	if !errors.Is(gradeErr, settlement.ErrLegAlreadyGraded) {
		t.Errorf("re-grading a graded leg returned %v, want settlement.ErrLegAlreadyGraded. "+
			"Reporting success here is what makes a consumer pay twice.", gradeErr)
	}
	if !errors.Is(settleErr, settlement.ErrWagerAlreadySettled) {
		t.Errorf("re-settling a settled wager returned %v, want settlement.ErrWagerAlreadySettled",
			settleErr)
	}
	if !errors.Is(ledgerErr, settlement.ErrTransactionExists) {
		t.Errorf("re-writing the settlement movement returned %v, want settlement.ErrTransactionExists",
			ledgerErr)
	}

	// The customer was paid exactly once: 400.00 granted, 40.00 staked into
	// escrow, 80.00 returned, escrow emptied.
	assertBalances(t, ctx, betStore, user, domain.Money(44_000), 0)
}

// TestWagerWithLegsRehydratesASettledTicket closes the loop on the transition
// replay in HydrateWager, which is shared between the two adapters.
//
// A settled wager is the hard case: the reconstruction has to grade the legs and
// then apply the terminal status, in that order, at the stored instants, without
// tripping the domain's monotonicity guard. Reading one back and finding the
// same status, the same returned amount and the same graded leg is the only way
// to assert that the replay reproduces the value rather than merely completing.
func TestWagerWithLegsRehydratesASettledTicket(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	betStore, _ := bettingStore(t)
	setStore := settlementStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	at := time.Now().UTC().Truncate(time.Microsecond)
	w := placeStraight(t, ctx, conn, betStore, cat, 3.0, domain.Money(2_000), at)
	leg := w.Legs()[0]

	gradedAt := at.Add(90 * time.Minute)
	lost, err := w.GradeLeg(leg.ID(), domain.LegStatusLost, gradedAt)
	if err != nil {
		t.Fatalf("GradeLeg: %v", err)
	}
	lost, err = lost.Settle(domain.WagerStatusLost, 0, gradedAt)
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if err := setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
		if err := tx.GradeLeg(ctx, leg.ID(), domain.LegStatusLost, gradedAt); err != nil {
			return err
		}
		return tx.SettleWager(ctx, lost)
	}); err != nil {
		t.Fatalf("settle as lost: %v", err)
	}

	err = setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
		got, err := tx.WagerWithLegs(ctx, w.ID())
		if err != nil {
			return err
		}
		if got.Status() != domain.WagerStatusLost {
			t.Errorf("status: got %s, want lost", got.Status())
		}
		returned, ok := got.Returned()
		if !ok || returned != 0 {
			t.Errorf("returned: got %s (present=%t), want exactly 0", returned, ok)
		}
		net, _ := got.NetReturn()
		if net != domain.Money(-2_000) {
			t.Errorf("net return: got %s, want -20.00", net)
		}
		if !got.UpdatedAt().Equal(gradedAt) {
			t.Errorf("transition instant: got %s, want %s", got.UpdatedAt(), gradedAt)
		}
		back, ok := got.Leg(leg.ID())
		if !ok {
			t.Fatalf("leg %s missing from the rehydrated ticket", leg.ID())
		}
		if back.Status() != domain.LegStatusLost {
			t.Errorf("leg status: got %s, want lost", back.Status())
		}
		gradedBack, ok := back.GradedAt()
		if !ok || !gradedBack.Equal(gradedAt) {
			t.Errorf("leg graded_at: got %s (present=%t), want %s", gradedBack, ok, gradedAt)
		}
		if !got.AllLegsGraded() {
			t.Error("AllLegsGraded() is false on a fully graded ticket")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
}

// TestSettleWagerRefusesANonTerminalWagerBeforeTouchingTheDatabase is a small
// guard with a specific payoff.
//
// wagers_return_iff_terminal would refuse the row anyway, but as a check
// violation naming a constraint. Refusing in the adapter produces a sentence
// that names the mistake, and it does so without spending a round trip on a
// statement that cannot succeed.
func TestSettleWagerRefusesANonTerminalWagerBeforeTouchingTheDatabase(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	betStore, _ := bettingStore(t)
	setStore := settlementStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	at := time.Now().UTC().Truncate(time.Microsecond)
	w := placeStraight(t, ctx, conn, betStore, cat, 2.0, domain.Money(1_000), at)

	err := setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
		if err := tx.SettleWager(ctx, w); !errors.Is(err, settlement.ErrInvalidOptions) {
			t.Errorf("settling a `placed` wager returned %v, want settlement.ErrInvalidOptions", err)
		}
		// A missing ticket is exceptional on this path, because the identifier
		// normally comes from a leg row read in the same transaction.
		absent := wagerID(t, uniqueID("wager"))
		if _, err := tx.WagerWithLegs(ctx, absent); !errors.Is(err, settlement.ErrWagerNotFound) {
			t.Errorf("an unknown wager returned %v, want settlement.ErrWagerNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("guard probe: %v", err)
	}
}

// -----------------------------------------------------------------------------
// The grader's work queue
// -----------------------------------------------------------------------------

// TestPendingLegsForEventCarriesTheGradingInputs covers the query's three
// derived columns, and DrawQuoted is the one that matters.
//
// Whether a moneyline market also quotes a draw decides what a TIE means: a PUSH
// on a two-way market with the stake returned, a LOSS on a three-way one. It is
// a property of the market that domain.Leg deliberately does not copy, so it has
// to arrive with the pending-leg list or the grader needs a catalogue query per
// leg. Both shapes are live in this system — the synthetic provider quotes
// three-way moneylines for the sports that admit a draw — so neither may be
// assumed.
func TestPendingLegsForEventCarriesTheGradingInputs(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	betStore, _ := bettingStore(t)
	setStore := settlementStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)

	cat := newCatalogue(t, ctx, conn)
	at := time.Now().UTC().Truncate(time.Microsecond)

	// A two-way moneyline: a tie is a push.
	twoWay := placeStraight(t, ctx, conn, betStore, cat, 2.0, domain.Money(1_000), at)

	// A three-way moneyline: the same market with a draw selection added, so a
	// tie is a loss for both sides.
	threeWayMarket := newMoneylineMarket(t, ctx, conn, cat)
	mustExec(t, ctx, conn, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, 'moneyline', 'draw', 'Draw')`,
		selectionID(t, uniqueID("sel")), threeWayMarket.ID)

	user := newUser(t, ctx, conn)
	price := bookedPrice(t, ctx, conn, threeWayMarket.HomeSelection, cat.BookID, 2.4, at)
	threeWay := straightWager(t, user, moneylineLeg(t, cat, threeWayMarket, price), domain.Money(1_000), at)
	if err := betStore.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertWager(ctx, threeWay)
	}); err != nil {
		t.Fatalf("place the three-way ticket: %v", err)
	}

	err := setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
		refs, err := tx.PendingLegsForEvent(ctx, cat.EventID, 50)
		if err != nil {
			return err
		}

		byLeg := make(map[domain.LegID]settlement.LegRef, len(refs))
		for _, ref := range refs {
			if err := ref.Validate(); err != nil {
				t.Errorf("the adapter produced a ref the consumer's own boundary refuses: %v", err)
			}
			byLeg[ref.LegID] = ref
		}

		two, ok := byLeg[twoWay.Legs()[0].ID()]
		if !ok {
			t.Fatalf("the two-way ticket's leg is missing from the work queue")
		}
		if two.DrawQuoted {
			t.Error("a market with no draw selection reported DrawQuoted; a tie would grade " +
				"as a loss instead of a push")
		}
		if two.WagerID != twoWay.ID() {
			t.Errorf("wager: got %s, want %s", two.WagerID, twoWay.ID())
		}
		if two.MarketType != domain.MarketTypeMoneyline || two.Role != domain.SelectionRoleHome {
			t.Errorf("shape: got %s/%s, want moneyline/home", two.MarketType, two.Role)
		}
		if two.GradingLine.Present() {
			t.Errorf("a moneyline leg carries a grading line: %s", two.GradingLine)
		}

		three, ok := byLeg[threeWay.Legs()[0].ID()]
		if !ok {
			t.Fatalf("the three-way ticket's leg is missing from the work queue")
		}
		if !three.DrawQuoted {
			t.Error("a market WITH a draw selection reported DrawQuoted false; a tie would " +
				"grade as a push and return two stakes the book never lost")
		}

		// A leg that has been graded leaves the queue. That is the whole point of
		// the `status = 'pending'` predicate.
		if err := tx.GradeLeg(ctx, twoWay.Legs()[0].ID(), domain.LegStatusWon, at.Add(time.Hour)); err != nil {
			return err
		}
		after, err := tx.PendingLegsForEvent(ctx, cat.EventID, 50)
		if err != nil {
			return err
		}
		for _, ref := range after {
			if ref.LegID == twoWay.Legs()[0].ID() {
				t.Error("a graded leg is still in the pending queue")
			}
		}

		// A limit of zero would return no rows, which reads as "nothing to
		// settle" — a silent, permanent stall on a customer's escrow.
		if _, err := tx.PendingLegsForEvent(ctx, cat.EventID, 0); !errors.Is(err, settlement.ErrInvalidOptions) {
			t.Errorf("a zero limit returned %v, want settlement.ErrInvalidOptions", err)
		}
		return errors.New("roll back; this test must not leave a graded leg behind")
	})
	if err == nil {
		t.Fatal("expected the deliberate rollback error")
	}
}

// TestAnEventWithNoExposureReturnsAnEmptyQueueRatherThanAnError is the ordinary
// case for a result nobody bet on, and there is deliberately no sentinel for it.
// "Nobody has money on this game" is not an error, and reporting it as one would
// make a quiet Sunday look like an outage.
func TestAnEventWithNoExposureReturnsAnEmptyQueueRatherThanAnError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	setStore := settlementStore(t)
	conn := rawConn(t, sharedDatabase(t).dsn)
	cat := newCatalogue(t, ctx, conn)

	err := setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
		refs, err := tx.PendingLegsForEvent(ctx, cat.EventID, 10)
		if err != nil {
			return err
		}
		if len(refs) != 0 {
			t.Errorf("got %d pending legs on an event nobody bet on, want 0", len(refs))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("empty queue: %v", err)
	}
}

// -----------------------------------------------------------------------------
// The results feed
// -----------------------------------------------------------------------------

// TestTheResultsSourceReadsTerminalEventsAndSkipsUnusableRows covers decision 5:
// the results feed is the `events` table, which the ingest writer fills from the
// provider's own output.
//
// Three shapes are written and each is there for a reason:
//
//	ended + score      the ordinary result
//	cancelled          a result with no score: every leg voids, every stake back
//	ended + no score   STORABLE, per migration 00002's note that
//	                   events_score_all_or_nothing constrains the PAIR and does
//	                   not require one. It must be SKIPPED, not handed on, or the
//	                   grader would settle every spread on the event against a
//	                   0-0 zero value.
//
// The boundary is asserted as INCLUSIVE, because a provider poll finalises a
// whole slate at one observation instant and an exclusive bound would drop every
// result sharing the instant the cursor names.
func TestTheResultsSourceReadsTerminalEventsAndSkipsUnusableRows(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, _ := sharedPool(t)
	results, err := settlementpg.NewResults(settlementpg.ResultsOptions{DB: db, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("build results source: %v", err)
	}

	conn := rawConn(t, sharedDatabase(t).dsn)

	// A window of this test's own, far enough from every other test's rows that
	// the watermark selects only these three. uniqueTimeWindow is the helper the
	// hypertable tests use for the same reason.
	base := uniqueTimeWindow()

	scored := newCatalogue(t, ctx, conn)
	mustExec(t, ctx, conn, `
UPDATE events SET status = 'ended', score_home = 24, score_away = 17, observed_at = $1
 WHERE id = $2`, base, scored.EventID)

	cancelled := newCatalogue(t, ctx, conn)
	mustExec(t, ctx, conn, `
UPDATE events SET status = 'cancelled', observed_at = $1 WHERE id = $2`,
		base.Add(time.Second), cancelled.EventID)

	unusable := newCatalogue(t, ctx, conn)
	mustExec(t, ctx, conn, `
UPDATE events SET status = 'ended', observed_at = $1 WHERE id = $2`,
		base.Add(2*time.Second), unusable.EventID)

	// The boundary is inclusive: asking from `base` must return the row observed
	// exactly at `base`.
	got, err := results.Since(ctx, base, 100)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}

	byEvent := make(map[domain.EventID]settlement.Result, len(got))
	var previous time.Time
	for _, r := range got {
		if !previous.IsZero() && r.FinalisedAt.Before(previous) {
			t.Errorf("results are not ordered oldest-first: %s follows %s", r.FinalisedAt, previous)
		}
		previous = r.FinalisedAt
		byEvent[r.EventID] = r
	}

	final, ok := byEvent[scored.EventID]
	if !ok {
		t.Fatalf("the event finalised exactly at the watermark was not returned; the boundary " +
			"must be INCLUSIVE or a whole slate finalised at one instant is lost")
	}
	if !final.IsScored() {
		t.Errorf("an ended event with a score reported IsScored() false: %+v", final)
	}
	if final.Score.Home() != 24 || final.Score.Away() != 17 {
		t.Errorf("score: got %s, want 24-17", final.Score)
	}
	if !final.FinalisedAt.Equal(base) {
		t.Errorf("finalisation instant: got %s, want the provider's observed_at %s",
			final.FinalisedAt, base)
	}

	void, ok := byEvent[cancelled.EventID]
	if !ok {
		t.Fatal("a cancelled event was not returned; it is a result with no score, and every " +
			"leg riding on it voids")
	}
	if !void.IsCancelled() || void.HasScore {
		t.Errorf("cancelled result: got %+v, want IsCancelled() with no score", void)
	}

	if _, present := byEvent[unusable.EventID]; present {
		t.Error("an ended event with NO score was handed on; grading it would settle every " +
			"spread against a 0-0 zero value")
	}

	// The limit is honoured and a non-positive one is refused.
	one, err := results.Since(ctx, base, 1)
	if err != nil {
		t.Fatalf("Since with a limit of 1: %v", err)
	}
	if len(one) > 1 {
		t.Errorf("limit 1 returned %d results", len(one))
	}
	if _, err := results.Since(ctx, base, 0); !errors.Is(err, settlement.ErrInvalidOptions) {
		t.Errorf("a zero limit returned %v, want settlement.ErrInvalidOptions", err)
	}
}

// TestOldestUnsettledAtSeedsTheResultsCursor runs on a database of its own.
//
// The query is GLOBAL — "the earliest wager anywhere that still holds escrow" —
// so it cannot be asserted on the shared database, where every other test in
// this package is writing open wagers in parallel. A dedicated container is the
// price of testing a global aggregate at all, and the alternative (asserting a
// bound rather than a value) would pass with a broken query.
//
// Both answers are asserted, and the first is the one a fresh deployment gets:
// found=false on an empty database, which settlement reads as "there is no
// ticket a historical result could pay" and turns into a cursor at the current
// instant.
func TestOldestUnsettledAtSeedsTheResultsCursor(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	own := freshMigratedDatabase(t)
	db, _ := connectPool(t, own.dsn)

	setStore, err := settlementpg.NewStore(db)
	if err != nil {
		t.Fatalf("build settlement pgstore: %v", err)
	}
	betStore, err := bettingpg.New(db)
	if err != nil {
		t.Fatalf("build betting pgstore: %v", err)
	}

	if _, found, err := setStore.OldestUnsettledAt(ctx); err != nil {
		t.Fatalf("OldestUnsettledAt on an empty database: %v", err)
	} else if found {
		t.Fatal("an empty database reported an oldest unsettled wager")
	}

	conn := rawConn(t, own.dsn)
	cat := newCatalogue(t, ctx, conn)

	older := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Microsecond)
	newer := older.Add(3 * time.Hour)
	placeStraight(t, ctx, conn, betStore, cat, 2.0, domain.Money(1_000), newer)
	oldest := placeStraight(t, ctx, conn, betStore, cat, 2.0, domain.Money(1_000), older)

	at, found, err := setStore.OldestUnsettledAt(ctx)
	if err != nil {
		t.Fatalf("OldestUnsettledAt: %v", err)
	}
	if !found {
		t.Fatal("two open wagers exist and OldestUnsettledAt found none")
	}
	if !at.Equal(oldest.PlacedAt()) {
		t.Errorf("cursor seed: got %s, want the earlier placement %s", at, oldest.PlacedAt())
	}

	// Settling the older ticket moves the seed forward: a settled ticket is not
	// waiting on anything.
	leg := oldest.Legs()[0]
	settledAt := older.Add(time.Hour)
	done, err := oldest.GradeLeg(leg.ID(), domain.LegStatusLost, settledAt)
	if err != nil {
		t.Fatalf("GradeLeg: %v", err)
	}
	done, err = done.Settle(domain.WagerStatusLost, 0, settledAt)
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if err := setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
		if err := tx.GradeLeg(ctx, leg.ID(), domain.LegStatusLost, settledAt); err != nil {
			return err
		}
		return tx.SettleWager(ctx, done)
	}); err != nil {
		t.Fatalf("settle the older ticket: %v", err)
	}

	at, found, err = setStore.OldestUnsettledAt(ctx)
	if err != nil {
		t.Fatalf("OldestUnsettledAt after settlement: %v", err)
	}
	if !found || !at.Equal(newer) {
		t.Errorf("after settling the older ticket the seed is %s (found=%t), want %s",
			at, found, newer)
	}
}

// TestEachSettlementOutcomeMovesTheRightMoney is the money table, driven end to
// end against a real PostgreSQL rather than against a fake ledger.
//
// The four terminal outcomes are the whole point of the settle service and they
// differ ONLY in what comes back to the customer, so they are asserted as one
// table with one shared shape: grant, stake into escrow, grade, settle, and then
// read the derived balances back. Every case ends with escrow EMPTY — an outcome
// that settled a ticket but left its stake escrowed would show a customer money
// they can never spend, and it is the failure mode a per-outcome test written in
// isolation is most likely to miss.
//
// The amounts are stated as arithmetic rather than as constants so the intent is
// legible:
//
//	won   stake 40.00 at 2.0  -> 80.00 returned. Cash 400 - 40 + 80 = 440.00
//	lost  stake 40.00         ->  0.00 returned. Cash 400 - 40      = 360.00
//	void  stake 40.00         -> 40.00 refunded. Cash back to        400.00
//	push  stake 40.00         -> 40.00 refunded. Cash back to        400.00
//
// void and push return the same money and are still separate cases, because they
// are different FACTS about the contest — a cancelled event versus a spread that
// landed exactly on the number — and domain.Wager.Settle reaches them by
// different routes. Collapsing them because the arithmetic agrees would stop the
// test noticing if one of those routes broke.
//
// The zero-sum assertion at the end is the one that cannot be satisfied by
// accident: every entry the settlement wrote, summed across ALL accounts
// including the house, must be exactly 0.
func TestEachSettlementOutcomeMovesTheRightMoney(t *testing.T) {
	t.Parallel()

	const (
		granted = domain.Money(40_000) // 400.00
		staked  = domain.Money(4_000)  // 40.00
	)

	tests := []struct {
		name      string
		leg       domain.LegStatus
		wager     domain.WagerStatus
		returned  domain.Money
		wantCash  domain.Money
		rationale string
	}{
		{
			name: "won", leg: domain.LegStatusWon, wager: domain.WagerStatusWon,
			returned: domain.Money(8_000), wantCash: domain.Money(44_000),
			rationale: "stake and profit both come back: the frozen payout, not a re-priced one",
		},
		{
			name: "lost", leg: domain.LegStatusLost, wager: domain.WagerStatusLost,
			returned: 0, wantCash: domain.Money(36_000),
			rationale: "nothing comes back; the escrowed stake goes to the house",
		},
		{
			name: "void", leg: domain.LegStatusVoid, wager: domain.WagerStatusVoid,
			returned: staked, wantCash: granted,
			rationale: "the contest did not happen, so the customer is made whole",
		},
		{
			name: "push", leg: domain.LegStatusPush, wager: domain.WagerStatusPush,
			returned: staked, wantCash: granted,
			rationale: "the result landed exactly on the number, so the stake is returned",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			betStore, _ := bettingStore(t)
			setStore := settlementStore(t)
			conn := rawConn(t, sharedDatabase(t).dsn)

			cat := newCatalogue(t, ctx, conn)
			at := time.Now().UTC().Truncate(time.Microsecond)

			w := placeStraight(t, ctx, conn, betStore, cat, 2.0, staked, at)
			user := w.UserID()
			grant(t, ctx, betStore, user, granted, at)

			stakeTxn, err := domain.NewStakeTransaction(transactionID(t, uniqueID("txn")), w, at)
			if err != nil {
				t.Fatalf("NewStakeTransaction: %v", err)
			}
			if err := betStore.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
				return tx.InsertTransaction(ctx, stakeTxn)
			}); err != nil {
				t.Fatalf("stake movement: %v", err)
			}

			// The stake has left cash and is sitting in escrow. Asserted before
			// settling, so a later balance failure cannot be blamed on the
			// placement half.
			assertBalances(t, ctx, betStore, user, granted-staked, staked)

			gradedAt := at.Add(2 * time.Hour)
			leg := w.Legs()[0]

			settled, err := w.GradeLeg(leg.ID(), tc.leg, gradedAt)
			if err != nil {
				t.Fatalf("GradeLeg(%s): %v", tc.leg, err)
			}
			settled, err = settled.Settle(tc.wager, tc.returned, gradedAt)
			if err != nil {
				t.Fatalf("Settle(%s): %v", tc.wager, err)
			}

			payoutID := transactionID(t, uniqueID("txn"))
			payout, err := domain.NewSettlementTransaction(payoutID, settled, gradedAt)
			if err != nil {
				t.Fatalf("NewSettlementTransaction: %v", err)
			}

			if err := setStore.InTx(ctx, func(ctx context.Context, tx settlement.Tx) error {
				if err := tx.GradeLeg(ctx, leg.ID(), tc.leg, gradedAt); err != nil {
					return err
				}
				if err := tx.SettleWager(ctx, settled); err != nil {
					return err
				}
				return tx.InsertTransaction(ctx, payout)
			}); err != nil {
				t.Fatalf("settlement (%s): %v", tc.name, err)
			}

			// The money, derived from the ledger and never read from a column.
			// Escrow is zero in every case: a settled ticket holds nothing.
			assertBalances(t, ctx, betStore, user, tc.wantCash, 0)

			// The settlement movement itself balances exactly. Integer
			// equality, not a tolerance — that is the reason money is minor
			// units in the first place.
			var sum, entries int64
			if err := conn.QueryRow(ctx,
				`SELECT coalesce(sum(amount_minor), 0)::BIGINT, count(*)::BIGINT
				   FROM ledger_entries WHERE transaction_id = $1`,
				payoutID,
			).Scan(&sum, &entries); err != nil {
				t.Fatalf("sum settlement entries: %v", err)
			}
			if sum != 0 {
				t.Errorf("the %s settlement's entries sum to %d, want exactly 0 (%s)",
					tc.name, sum, tc.rationale)
			}
			if entries < 2 {
				t.Errorf("the %s settlement wrote %d entries; a movement is at least two",
					tc.name, entries)
			}

			// The entries themselves, logged so the money movement is legible
			// in the test output rather than only asserted about.
			rows, err := conn.Query(ctx,
				`SELECT entry_index, account_kind, coalesce(account_user_id, '-'), amount_minor, kind
				   FROM ledger_entries WHERE transaction_id = $1 ORDER BY entry_index`, payoutID)
			if err != nil {
				t.Fatalf("read settlement entries: %v", err)
			}
			defer rows.Close()
			for rows.Next() {
				var idx int32
				var accountKind, owner, entryKind string
				var amount int64
				if err := rows.Scan(&idx, &accountKind, &owner, &amount, &entryKind); err != nil {
					t.Fatalf("scan entry: %v", err)
				}
				t.Logf("%-5s entry %d: %-12s %-28s %+8d %s",
					tc.name, idx, accountKind, owner, amount, entryKind)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate entries: %v", err)
			}

			// And the whole ledger for this customer nets to what was granted
			// minus what the house kept — asserted as a fold over every entry
			// they have, across every account.
			var net int64
			if err := conn.QueryRow(ctx,
				`SELECT coalesce(sum(amount_minor), 0)::BIGINT
				   FROM ledger_entries WHERE account_user_id = $1`,
				user,
			).Scan(&net); err != nil {
				t.Fatalf("fold customer entries: %v", err)
			}
			if net != tc.wantCash.MinorUnits() {
				t.Errorf("the customer's entries fold to %d, want %d", net, tc.wantCash.MinorUnits())
			}
		})
	}
}
