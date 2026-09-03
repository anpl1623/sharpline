package pgstore_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/settlement"
	"github.com/anpl1623/sharpline/internal/settlement/pgstore"
)

// -----------------------------------------------------------------------------
// The grading inputs
// -----------------------------------------------------------------------------

// TestTheWorkQueueCarriesEveryShapeOfGradingLine is the gap the moneyline-only
// fixtures elsewhere leave open.
//
// ListPendingLegsForEvent projects COALESCE(teased_line, price_line) — that is
// domain.Leg.GradingLine() written in SQL — and the value it produces is the
// number every spread and total is graded against. Four shapes reach that
// column and each grades differently:
//
//	moneyline   NULL          no line; the winner is the winner
//	spread      a real value  the margin is compared against it
//	spread      0.0           A PICK'EM. A real traded handicap, and the case an
//	                          adapter that treats NULL and zero alike destroys —
//	                          the margin test disappears and a home side that lost
//	                          by three grades as a winner.
//	teaser leg  the TEASED    the booked line is deliberately NOT the grading
//	            number        line; leg.go keeps the real price and carries the
//	                          moved number beside it, so grading against the
//	                          booked one settles at a handicap nobody sold.
//
// All four are asserted in one read because they arrive from the same statement
// and the COALESCE has to get all four right at once.
func TestTheWorkQueueCarriesEveryShapeOfGradingLine(t *testing.T) {
	t.Parallel()

	settleStore, betStore, db := stores(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	moneyline := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	handicap := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeSpread, mustLine(t, -3.5))
	pickEm := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeSpread, mustLine(t, 0))

	straightOn := map[string]domain.Wager{
		"moneyline": placeStraight(t, ctx, db.Pool(), betStore, cat, moneyline, user, 1.91, domain.Money(1_000), at),
		"handicap":  placeStraight(t, ctx, db.Pool(), betStore, cat, handicap, user, 1.95, domain.Money(1_000), at),
		"pick'em":   placeStraight(t, ctx, db.Pool(), betStore, cat, pickEm, user, 1.90, domain.Money(1_000), at),
	}

	// A teaser: two spread legs, each moved six points in the bettor's favour.
	// wagers_assert_shape checks the DIRECTION as well as the magnitude — a
	// spread's handicap moves UP, only an over's threshold moves down.
	const points = 6.0
	const d1, d2 = 1.7500000000000002, 1.8200000000000003
	teaserLegs := make([]domain.Leg, 0, 2)
	teasedFor := make(map[domain.LegID]float64, 2)
	for _, spec := range []struct{ line, decimal float64 }{{-7.5, d1}, {-3.0, d2}} {
		line := mustLine(t, spec.line)
		mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeSpread, line)
		price := bookedQuote(t, ctx, db.Pool(), mkt.Home, cat.BookID, spec.decimal, line, at)
		leg := homeLeg(t, cat, mkt, price, mustLine(t, spec.line+points))
		teaserLegs = append(teaserLegs, leg)
		teasedFor[leg.ID()] = spec.line + points
	}
	teaser, err := domain.NewWager(domain.WagerParams{
		ID:              mustWagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindTeaser,
		Legs:            teaserLegs,
		Stake:           domain.Money(2_500),
		AcceptedDecimal: d1 * d2,
		Rounding:        domain.RoundTowardZero,
		TeaserPoints:    points,
		PlacedAt:        at,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}
	place(t, ctx, betStore, teaser)

	settleTx(t, ctx, settleStore, func(ctx context.Context, tx settlement.Tx) error {
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

		lineOf := func(name string) (float64, bool) {
			t.Helper()
			w := straightOn[name]
			ref, ok := byLeg[w.Legs()[0].ID()]
			if !ok {
				t.Fatalf("the %s ticket's leg is missing from the work queue", name)
			}
			return ref.GradingLine.Value()
		}

		if _, present := lineOf("moneyline"); present {
			t.Error("a moneyline leg arrived carrying a grading line; NULL is not a line")
		}
		if v, present := lineOf("handicap"); !present || v != -3.5 {
			t.Errorf("the spread leg grades at (%v, %t), want (-3.5, true)", v, present)
		}
		if v, present := lineOf("pick'em"); !present || v != 0 {
			t.Errorf("the pick'em leg grades at (%v, %t), want (0, true). A pick'em read as an "+
				"absent line grades a spread as a moneyline.", v, present)
		}

		for _, leg := range teaser.Legs() {
			ref, ok := byLeg[leg.ID()]
			if !ok {
				t.Fatalf("teaser leg %s is missing from the work queue", leg.ID())
			}
			v, present := ref.GradingLine.Value()
			if !present {
				t.Errorf("teaser leg %s arrived with no grading line", leg.ID())
				continue
			}
			if v != teasedFor[leg.ID()] {
				booked, _ := leg.Price().Line().Value()
				t.Errorf("teaser leg %s grades at %v, want the TEASED %v (it was booked at %v): "+
					"the COALESCE is returning the booked line, which settles at a handicap "+
					"nobody sold", leg.ID(), v, teasedFor[leg.ID()], booked)
			}
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// The rows-affected contract, under a CHANGED redelivery
// -----------------------------------------------------------------------------

// TestARedeliveredGradingIsRefusedAndLeavesTheRecordAlone.
//
// ports.go states the danger of the rows-affected contract in one direction —
// "returning nil from either on a zero-row update is the single most dangerous
// thing an implementation of this interface can do, because the ledger write
// that follows would balance perfectly and pay twice". This test asserts the
// other direction, which is what makes the record trustworthy rather than merely
// un-double-paid: a refusal must also CHANGE NOTHING.
//
// The redelivery deliberately carries DIFFERENT values — a different leg
// grading, a different instant, a smaller payout. That is not a contrived input:
// a provider correcting a result mid-slate produces exactly it, and it is the
// case a redelivery of the identical message cannot see. If the guard were
// dropped from either statement, the identical-message test would still pass and
// this one would find a graded leg silently re-graded and a settled ticket
// silently repriced.
//
// The immediate triggers in migration 00006 are a second line of defence and are
// deliberately not what is asserted here. legs_assert_transition and
// wagers_assert_transition would raise 23001, which the service reads as a
// transport failure and retries; the SENTINEL is what tells it the work is
// already done. A test that accepted either would not distinguish "handled" from
// "crashed usefully".
func TestARedeliveredGradingIsRefusedAndLeavesTheRecordAlone(t *testing.T) {
	t.Parallel()

	settleStore, betStore, db := stores(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)

	mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	w := placeStraight(t, ctx, db.Pool(), betStore, cat, mkt, user, 2.0, domain.Money(4_000), at)
	leg := w.Legs()[0]

	gradedAt := at.Add(2 * time.Hour)
	won, err := w.GradeLeg(leg.ID(), domain.LegStatusWon, gradedAt)
	if err != nil {
		t.Fatalf("GradeLeg: %v", err)
	}
	settled, err := won.Settle(domain.WagerStatusWon, w.PotentialPayout(), gradedAt)
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}

	settleTx(t, ctx, settleStore, func(ctx context.Context, tx settlement.Tx) error {
		if err := tx.GradeLeg(ctx, leg.ID(), domain.LegStatusWon, gradedAt); err != nil {
			return err
		}
		return tx.SettleWager(ctx, settled)
	})

	// The correction. A different outcome, a later instant, and a payout of the
	// stake alone — every one of which is a legal value in isolation.
	correctedAt := gradedAt.Add(time.Hour)
	lost, err := w.GradeLeg(leg.ID(), domain.LegStatusLost, correctedAt)
	if err != nil {
		t.Fatalf("GradeLeg (correction): %v", err)
	}
	repriced, err := lost.Settle(domain.WagerStatusLost, 0, correctedAt)
	if err != nil {
		t.Fatalf("Settle (correction): %v", err)
	}

	var gradeErr, settleErr error
	settleTx(t, ctx, settleStore, func(ctx context.Context, tx settlement.Tx) error {
		gradeErr = tx.GradeLeg(ctx, leg.ID(), domain.LegStatusLost, correctedAt)
		settleErr = tx.SettleWager(ctx, repriced)
		return nil
	})

	if !errors.Is(gradeErr, settlement.ErrLegAlreadyGraded) {
		t.Errorf("re-grading a graded leg returned %v, want settlement.ErrLegAlreadyGraded. "+
			"Reporting success here is what makes a consumer pay twice.", gradeErr)
	}
	if !errors.Is(settleErr, settlement.ErrWagerAlreadySettled) {
		t.Errorf("re-settling a settled wager returned %v, want settlement.ErrWagerAlreadySettled",
			settleErr)
	}

	// And the record itself.
	var (
		legStatus  string
		legGraded  pgtype.Timestamptz
		wagStatus  string
		wagReturn  *int64
		wagChanged time.Time
	)
	if err := db.Pool().QueryRow(ctx, `
SELECT l.status, l.graded_at, w.status, w.returned_minor, w.transitioned_at
  FROM legs l JOIN wagers w ON w.id = l.wager_id
 WHERE l.id = $1`, leg.ID()).Scan(&legStatus, &legGraded, &wagStatus, &wagReturn, &wagChanged); err != nil {
		t.Fatalf("read the settled record back: %v", err)
	}

	if legStatus != domain.LegStatusWon.String() {
		t.Errorf("the leg is now %s; a refused re-grading rewrote the result", legStatus)
	}
	if !legGraded.Valid || !legGraded.Time.Equal(gradedAt) {
		t.Errorf("the leg is graded at %v, want the original %s: graded_at is write-once so that "+
			"a replay cannot move the recorded grading time", legGraded.Time, gradedAt)
	}
	if wagStatus != domain.WagerStatusWon.String() {
		t.Errorf("the ticket is now %s; a refused re-settlement rewrote the outcome", wagStatus)
	}
	if wagReturn == nil || *wagReturn != w.PotentialPayout().MinorUnits() {
		t.Errorf("the ticket returned %v, want the original %s", wagReturn, w.PotentialPayout())
	}
	if !wagChanged.Equal(gradedAt) {
		t.Errorf("the ticket transitioned at %s, want the original %s", wagChanged, gradedAt)
	}
}

// TestGradeLegRefusesAStatusItCannotWriteBeforeReachingTheDatabase.
//
// [settlement.Tx.GradeLeg]'s job is to run the statement and report what
// happened, not to re-derive the state machine — but a status this build cannot
// spell is a different matter. Sent to the server it becomes a check violation
// naming legs_status_defined, which is a constraint name where the caller needs
// a sentence; worse, on a zero-row UPDATE it would be indistinguishable from
// ErrLegAlreadyGraded, and the consumer would move on believing a leg it never
// graded was already settled by somebody else.
func TestGradeLegRefusesAStatusItCannotWriteBeforeReachingTheDatabase(t *testing.T) {
	t.Parallel()

	settleStore, betStore, db := stores(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	w := placeStraight(t, ctx, db.Pool(), betStore, cat, mkt, user, 2.0, domain.Money(1_000), at)
	leg := w.Legs()[0]

	settleTx(t, ctx, settleStore, func(ctx context.Context, tx settlement.Tx) error {
		err := tx.GradeLeg(ctx, leg.ID(), domain.LegStatusUnknown, at.Add(time.Hour))
		if !errors.Is(err, settlement.ErrUnusableLeg) {
			t.Errorf("an unspellable status returned %v, want settlement.ErrUnusableLeg", err)
		}
		if !errors.Is(err, domain.ErrUnknownLegStatus) {
			t.Errorf("the refusal does not carry domain.ErrUnknownLegStatus: %v", err)
		}
		if errors.Is(err, settlement.ErrLegAlreadyGraded) {
			t.Error("a status this build cannot write was reported as 'somebody else graded it'")
		}
		return nil
	})

	// And the leg is untouched, so the transaction above really did refuse
	// rather than write and then complain.
	var status string
	if err := db.Pool().QueryRow(ctx, `SELECT status FROM legs WHERE id = $1`, leg.ID()).Scan(&status); err != nil {
		t.Fatalf("read the leg back: %v", err)
	}
	if status != domain.LegStatusPending.String() {
		t.Errorf("the leg is %s, want pending", status)
	}
}

// TestSettleWagerRefusesATicketThatIsNotTerminal.
//
// The row would be rejected by wagers_return_iff_terminal anyway, but as a check
// violation naming a constraint rather than as a sentence naming the mistake —
// and this is the one path where the caller is about to write a ledger movement,
// so the message it gets is the message somebody reads at three in the morning.
func TestSettleWagerRefusesATicketThatIsNotTerminal(t *testing.T) {
	t.Parallel()

	settleStore, betStore, db := stores(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	user := newUser(t, ctx, db.Pool())
	at := time.Now().UTC().Truncate(time.Microsecond)

	mkt := newMarket(t, ctx, db.Pool(), cat, domain.MarketTypeMoneyline, domain.NoLine())
	w := placeStraight(t, ctx, db.Pool(), betStore, cat, mkt, user, 2.0, domain.Money(1_000), at)

	settleTx(t, ctx, settleStore, func(ctx context.Context, tx settlement.Tx) error {
		if err := tx.SettleWager(ctx, w); !errors.Is(err, settlement.ErrInvalidOptions) {
			t.Errorf("settling a placed ticket returned %v, want settlement.ErrInvalidOptions", err)
		}
		return nil
	})
}

// TestAMissingTicketIsASentinelAndNotAZeroWager.
//
// It really is exceptional on the settlement path — the identifier comes from a
// leg row the same transaction just read, and legs.wager_id is a foreign key —
// which is precisely why it must not be quiet: a zero domain.Wager here would be
// settled for its zero stake and the customer's escrow would never be released.
func TestAMissingTicketIsASentinelAndNotAZeroWager(t *testing.T) {
	t.Parallel()

	settleStore, _, _ := stores(t)
	ctx := t.Context()

	settleTx(t, ctx, settleStore, func(ctx context.Context, tx settlement.Tx) error {
		got, err := tx.WagerWithLegs(ctx, mustWagerID(t, uniqueID("wager")))
		if !errors.Is(err, settlement.ErrWagerNotFound) {
			t.Errorf("an unknown wager returned %v, want settlement.ErrWagerNotFound", err)
		}
		if !got.IsZero() {
			t.Errorf("a missing wager still produced ticket %s", got.ID())
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// The results feed
// -----------------------------------------------------------------------------

// TestABatchLimitThatWouldReadAsNothingToSettleIsRefused.
//
// `LIMIT 0` returns no rows, and no rows on either of these calls reads as
// "there is nothing to settle" — a silent, permanent stall on a customer's
// escrow, with no error anywhere to notice. Both ports say so in the same words,
// and the upper bound is here because a caller's `int` is 64 bits while the
// column is INTEGER, so a large batch size would otherwise be TRUNCATED into a
// small or negative one rather than refused.
func TestABatchLimitThatWouldReadAsNothingToSettleIsRefused(t *testing.T) {
	t.Parallel()

	settleStore, _, db := stores(t)
	ctx := t.Context()
	cat := newCatalogue(t, ctx, db.Pool())
	feed := results(t, db)

	oversize := int(math.MaxInt32) + 1
	settleTx(t, ctx, settleStore, func(ctx context.Context, tx settlement.Tx) error {
		for _, limit := range []int{0, -1, oversize} {
			if _, err := tx.PendingLegsForEvent(ctx, cat.EventID, limit); !errors.Is(err, settlement.ErrInvalidOptions) {
				t.Errorf("a batch limit of %d returned %v, want settlement.ErrInvalidOptions", limit, err)
			}
		}
		return nil
	})

	for _, limit := range []int{0, -1, oversize} {
		if _, err := feed.Since(ctx, time.Now().UTC().Add(-time.Hour), limit); !errors.Is(err, settlement.ErrInvalidOptions) {
			t.Errorf("a results batch limit of %d returned %v, want settlement.ErrInvalidOptions", limit, err)
		}
	}
}

// TestAFailedResultsReadIsAnErrorAndNotAnEmptySlate.
//
// [pgstore.Results.Since] returns a slice, and the caller's next move on an
// empty one is to advance nothing and wait. That is the correct response to a
// quiet slate and the worst possible response to a failed query: settlement
// would report itself healthy while every customer's stake sat in escrow.
//
// The failure is produced with an already-cancelled context because that is the
// failure that actually happens — a poll's deadline expiring under load — and
// because it fails the statement without breaking the server for the rest of the
// package.
func TestAFailedResultsReadIsAnErrorAndNotAnEmptySlate(t *testing.T) {
	t.Parallel()

	_, _, db := stores(t)
	feed := results(t, db)

	dead, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := feed.Since(dead, time.Now().UTC().Add(-time.Hour), 100)
	if err == nil {
		t.Fatalf("a cancelled results read returned %d results and no error; an empty slate and "+
			"a broken query must not look alike", len(got))
	}
	if got != nil {
		t.Errorf("a failed read still returned %d results", len(got))
	}
}

// -----------------------------------------------------------------------------
// Constructor and transaction contracts
// -----------------------------------------------------------------------------

// TestTheAdaptersRefuseToBeBuiltOrDrivenWithoutTheirDependencies.
//
// CLAUDE.md §12 asks for constructor injection with fail-fast validation, and
// these are the ways it can be got wrong. The nil callback matters most: every
// statement this package runs reaches the server through InTx, so a nil there
// would be a nil dereference inside a settlement rather than at wire-up.
//
// The nil LOGGER is deliberately NOT a refusal, and the asymmetry is the point.
// ResultsOptions.Logger is optional in code and not in spirit — Since SKIPS a row
// that is not a usable result, and a skipped result is a customer's stake sitting
// in escrow with nothing to release it — so the default has to be a real logger
// rather than a nil that panics on the first skip.
func TestTheAdaptersRefuseToBeBuiltOrDrivenWithoutTheirDependencies(t *testing.T) {
	t.Parallel()

	if _, err := pgstore.NewStore(nil); !errors.Is(err, settlement.ErrInvalidOptions) {
		t.Errorf("NewStore(nil) returned %v, want settlement.ErrInvalidOptions", err)
	}
	if _, err := pgstore.NewResults(pgstore.ResultsOptions{}); !errors.Is(err, settlement.ErrInvalidOptions) {
		t.Errorf("NewResults with no database returned %v, want settlement.ErrInvalidOptions", err)
	}

	settleStore, _, db := stores(t)
	if err := settleStore.InTx(t.Context(), nil); !errors.Is(err, settlement.ErrInvalidOptions) {
		t.Errorf("InTx with a nil function returned %v, want settlement.ErrInvalidOptions", err)
	}

	feed, err := pgstore.NewResults(pgstore.ResultsOptions{DB: db})
	if err != nil {
		t.Fatalf("a results source with no logger was refused: %v", err)
	}
	if _, err := feed.Since(t.Context(), time.Now().UTC(), 10); err != nil {
		t.Errorf("the default-logger results source cannot read: %v", err)
	}
}
