// Tests for the settlement service: the money that moves for each of the four
// outcomes, the deferral rule for a ticket whose games have not all finished,
// the repricing of a partially-voided parlay, and the interlock that makes a
// refused audit publish abort the settlement.
//
// # The fakes model a TRANSACTION, not a map
//
// fakeStore.InTx stages every write and applies it only on a nil return from the
// body. That is more machinery than a recording double needs, and it is the
// point: the single most important assertion in this file is that a publish
// failure leaves NOTHING behind, and a fake that applied writes as they were
// made could not tell the difference between "rolled back" and "never written".
//
// Nothing here touches a database. The same transaction body runs against a real
// Postgres in the integration tier, where the deferred zero-sum constraint and
// the wagers_assert_transition trigger assert the same rules from the other side.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Domain fixtures
// -----------------------------------------------------------------------------

// placedAt is two hours before every fixture result, so a settlement stamped at
// the result's own instant satisfies both domain.Wager.stamp's monotonicity and
// migrations/00006's wagers_transitioned_after_placed.
var placedAt = finalisedAt.Add(-2 * time.Hour)

func mustPrice(t *testing.T, selection string, decimal float64, line domain.Line) domain.Price {
	t.Helper()
	p, err := domain.NewPrice(domain.PriceParams{
		SelectionID: domain.SelectionID(selection),
		BookID:      "book-sharpline",
		Decimal:     decimal,
		Line:        line,
		ObservedAt:  placedAt,
	})
	if err != nil {
		t.Fatalf("domain.NewPrice(%s): %v", selection, err)
	}
	return p
}

// legSpec is the shape of one fixture leg.
type legSpec struct {
	id      string
	event   string
	market  string
	sel     string
	typ     domain.MarketType
	role    domain.SelectionRole
	decimal float64
	line    domain.Line
}

func (s legSpec) build(t *testing.T) domain.Leg {
	t.Helper()
	leg, err := domain.NewLeg(domain.LegParams{
		ID:          domain.LegID(s.id),
		EventID:     domain.EventID(s.event),
		MarketID:    domain.MarketID(s.market),
		MarketType:  s.typ,
		Role:        s.role,
		SelectionID: domain.SelectionID(s.sel),
		Price:       mustPrice(t, s.sel, s.decimal, s.line),
	})
	if err != nil {
		t.Fatalf("domain.NewLeg(%s): %v", s.id, err)
	}
	return leg
}

// ref turns a fixture leg into the grading input the store would hand over.
func (s legSpec) ref(wager string) LegRef {
	return LegRef{
		LegID:       domain.LegID(s.id),
		WagerID:     domain.WagerID(wager),
		EventID:     domain.EventID(s.event),
		MarketType:  s.typ,
		Role:        s.role,
		GradingLine: s.line,
	}
}

// moneylineHome is the simplest possible leg: a two-way home moneyline.
func moneylineHome(id, event, market, sel string, decimal float64) legSpec {
	return legSpec{
		id: id, event: event, market: market, sel: sel,
		typ: domain.MarketTypeMoneyline, role: domain.SelectionRoleHome,
		decimal: decimal, line: domain.NoLine(),
	}
}

func mustWager(
	t *testing.T,
	id string,
	kind domain.WagerKind,
	stake int64,
	accepted float64,
	legs ...domain.Leg,
) domain.Wager {
	t.Helper()
	w, err := domain.NewWager(domain.WagerParams{
		ID:              domain.WagerID(id),
		UserID:          "user-1",
		Kind:            kind,
		Legs:            legs,
		Stake:           domain.Money(stake),
		AcceptedDecimal: accepted,
		Rounding:        domain.RoundHalfAwayFromZero,
		PlacedAt:        placedAt,
	})
	if err != nil {
		t.Fatalf("domain.NewWager(%s): %v", id, err)
	}
	return w
}

// -----------------------------------------------------------------------------
// Fakes
// -----------------------------------------------------------------------------

type fakeResults struct {
	batches   [][]Result
	watermark []time.Time
	calls     int
	err       error
}

func (f *fakeResults) Since(_ context.Context, watermark time.Time, _ int) ([]Result, error) {
	f.watermark = append(f.watermark, watermark)
	if f.err != nil {
		return nil, f.err
	}
	defer func() { f.calls++ }()
	if f.calls >= len(f.batches) {
		return nil, nil
	}
	return f.batches[f.calls], nil
}

// gradedLeg is one staged leg grading.
type gradedLeg struct {
	legID  domain.LegID
	status domain.LegStatus
	at     time.Time
}

// fakeTx models one transaction over an in-memory ticket book.
type fakeTx struct {
	// wagers is the committed state. It is shared with the store, so a second
	// transaction sees what the first one committed.
	wagers   map[domain.WagerID]domain.Wager
	pending  map[domain.EventID][]LegRef
	legOwner map[domain.LegID]domain.WagerID

	// Staged in this transaction, applied only on commit.
	stagedLegs   []gradedLeg
	stagedWagers []domain.Wager
	stagedTxns   []domain.Transaction

	// Committed movements, in order.
	movements []domain.Transaction

	// Programmable failures, each returned once.
	loadErr   error
	gradeErr  error
	settleErr error
	insertErr error

	// Call counts, for the assertions that are about what was NOT attempted.
	loads, grades, settles, inserts int
}

func newFakeTx() *fakeTx {
	return &fakeTx{
		wagers:   make(map[domain.WagerID]domain.Wager),
		pending:  make(map[domain.EventID][]LegRef),
		legOwner: make(map[domain.LegID]domain.WagerID),
	}
}

// add registers a ticket and the pending legs a result would find for it.
func (tx *fakeTx) add(w domain.Wager, refs ...LegRef) {
	tx.wagers[w.ID()] = w
	for _, leg := range w.Legs() {
		tx.legOwner[leg.ID()] = w.ID()
	}
	for _, ref := range refs {
		tx.pending[ref.EventID] = append(tx.pending[ref.EventID], ref)
	}
}

func (tx *fakeTx) PendingLegsForEvent(_ context.Context, id domain.EventID, _ int) ([]LegRef, error) {
	// Only legs that are STILL pending, which is what the real query's WHERE
	// clause says and what makes a redelivery a no-op rather than a re-grade.
	var out []LegRef
	for _, ref := range tx.pending[id] {
		w, ok := tx.wagers[ref.WagerID]
		if !ok {
			continue
		}
		leg, ok := w.Leg(ref.LegID)
		if !ok || leg.Status() != domain.LegStatusPending {
			continue
		}
		out = append(out, ref)
	}
	return out, nil
}

func (tx *fakeTx) WagerWithLegs(_ context.Context, id domain.WagerID) (domain.Wager, error) {
	tx.loads++
	if err := take(&tx.loadErr); err != nil {
		return domain.Wager{}, err
	}
	w, ok := tx.wagers[id]
	if !ok {
		return domain.Wager{}, fmt.Errorf("%w: %s", ErrWagerNotFound, id)
	}
	return w, nil
}

func (tx *fakeTx) GradeLeg(_ context.Context, id domain.LegID, status domain.LegStatus, at time.Time) error {
	tx.grades++
	if err := take(&tx.gradeErr); err != nil {
		return err
	}
	owner, ok := tx.legOwner[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrLegAlreadyGraded, id)
	}
	if leg, ok := tx.wagers[owner].Leg(id); !ok || leg.Status() != domain.LegStatusPending {
		return fmt.Errorf("%w: %s", ErrLegAlreadyGraded, id)
	}
	tx.stagedLegs = append(tx.stagedLegs, gradedLeg{legID: id, status: status, at: at})
	return nil
}

func (tx *fakeTx) SettleWager(_ context.Context, w domain.Wager) error {
	tx.settles++
	if err := take(&tx.settleErr); err != nil {
		return err
	}
	if tx.wagers[w.ID()].IsTerminal() {
		return fmt.Errorf("%w: %s", ErrWagerAlreadySettled, w.ID())
	}
	tx.stagedWagers = append(tx.stagedWagers, w)
	return nil
}

func (tx *fakeTx) InsertTransaction(_ context.Context, t domain.Transaction) error {
	tx.inserts++
	if err := take(&tx.insertErr); err != nil {
		return err
	}
	for _, existing := range tx.movements {
		if existing.ID() == t.ID() {
			return fmt.Errorf("%w: %s", ErrTransactionExists, t.ID())
		}
	}
	tx.stagedTxns = append(tx.stagedTxns, t)
	return nil
}

// begin clears anything a previous transaction staged.
func (tx *fakeTx) begin() {
	tx.stagedLegs, tx.stagedWagers, tx.stagedTxns = nil, nil, nil
}

// commit applies the staged writes.
//
// Leg gradings are replayed through domain.Wager.GradeLeg rather than poked into
// a struct, so the in-memory book can only ever hold states the domain itself
// admits — which is what stops a test from asserting against a ticket the real
// system could not produce.
func (tx *fakeTx) commit(t *testing.T) {
	t.Helper()
	for _, g := range tx.stagedLegs {
		owner := tx.legOwner[g.legID]
		next, err := tx.wagers[owner].GradeLeg(g.legID, g.status, g.at)
		if err != nil {
			t.Fatalf("committing leg %s: %v", g.legID, err)
		}
		tx.wagers[owner] = next
	}
	for _, w := range tx.stagedWagers {
		tx.wagers[w.ID()] = w
	}
	tx.movements = append(tx.movements, tx.stagedTxns...)
	tx.begin()
}

func (tx *fakeTx) rollback() { tx.begin() }

type fakeStore struct {
	tx *fakeTx
	t  *testing.T

	oldest    time.Time
	oldestOK  bool
	oldestErr error

	// commits counts every transaction that committed; writeCommits counts only
	// those that actually staged a write. The pending-leg read is a transaction
	// too, so an assertion about "did the settlement commit" has to be able to
	// tell the two apart.
	commits, writeCommits, rollbacks int
}

func (s *fakeStore) OldestUnsettledAt(context.Context) (time.Time, bool, error) {
	if s.oldestErr != nil {
		return time.Time{}, false, s.oldestErr
	}
	return s.oldest, s.oldestOK, nil
}

func (s *fakeStore) InTx(ctx context.Context, fn func(context.Context, Tx) error) error {
	s.tx.begin()
	if err := fn(ctx, s.tx); err != nil {
		s.rollbacks++
		s.tx.rollback()
		return err
	}
	s.commits++
	if len(s.tx.stagedLegs)+len(s.tx.stagedWagers)+len(s.tx.stagedTxns) > 0 {
		s.writeCommits++
	}
	s.tx.commit(s.t)
	return nil
}

type published struct {
	wagerID domain.WagerID
	msg     kafka.Message
}

type fakePublisher struct {
	sent []published
	err  error
}

func (p *fakePublisher) PublishWagerEvent(_ context.Context, id domain.WagerID, msg kafka.Message) error {
	if p.err != nil {
		return p.err
	}
	p.sent = append(p.sent, published{wagerID: id, msg: msg})
	return nil
}

// take returns a programmed error once and clears it, so a fake can fail a
// single call rather than every call.
func take(slot *error) error {
	if *slot == nil {
		return nil
	}
	err := *slot
	*slot = nil
	return err
}

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

type harness struct {
	svc     *Service
	store   *fakeStore
	tx      *fakeTx
	results *fakeResults
	pub     *fakePublisher
	reg     *prometheus.Registry
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	tx := newFakeTx()
	h := &harness{
		tx:      tx,
		store:   &fakeStore{tx: tx, t: t},
		results: &fakeResults{},
		pub:     &fakePublisher{},
		reg:     prometheus.NewRegistry(),
	}

	svc, err := New(ServiceOptions{
		Results:   h.results,
		Store:     h.store,
		Publisher: h.pub,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry:  h.reg,
		// A frozen clock. Nothing STORED reads it — every instant written comes
		// from the result — so freezing it only pins the lag histogram and the
		// cursor gauge, which is exactly what a test wants to assert exactly.
		Clock: func() time.Time { return finalisedAt.Add(30 * time.Second) },
	})
	if err != nil {
		t.Fatalf("settlement.New: %v", err)
	}
	h.svc = svc
	return h
}

// settled returns the committed state of one ticket.
func (h *harness) settled(t *testing.T, id string) domain.Wager {
	t.Helper()
	w, ok := h.tx.wagers[domain.WagerID(id)]
	if !ok {
		t.Fatalf("wager %s is not in the book", id)
	}
	return w
}

// onlyMovement returns the single committed ledger movement, failing if there is
// not exactly one.
func (h *harness) onlyMovement(t *testing.T) domain.Transaction {
	t.Helper()
	if len(h.tx.movements) != 1 {
		t.Fatalf("committed %d ledger movements, want exactly 1", len(h.tx.movements))
	}
	return h.tx.movements[0]
}

// assertBalanced checks the invariant the whole ledger exists for, and reports
// the per-account net effect so a caller can assert where the money went.
func assertBalanced(t *testing.T, txn domain.Transaction) {
	t.Helper()
	if err := domain.LedgerIsBalanced(txn); err != nil {
		t.Errorf("the settlement movement does not balance: %v", err)
	}
}

func netFor(t *testing.T, txn domain.Transaction, kind domain.AccountKind, owner domain.UserID) domain.Money {
	t.Helper()
	account, err := domain.NewAccount(kind, owner)
	if err != nil {
		t.Fatalf("domain.NewAccount(%s): %v", kind, err)
	}
	got, err := txn.NetFor(account)
	if err != nil {
		t.Fatalf("NetFor(%s): %v", account, err)
	}
	return got
}

// -----------------------------------------------------------------------------
// The four outcomes
// -----------------------------------------------------------------------------

// TestSettleStraightWinPaysTheFrozenPayout is the happy path, and it asserts the
// money three ways: what the ticket says it returned, where the ledger moved it,
// and that the entries sum to zero.
func TestSettleStraightWinPaysTheFrozenPayout(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20)); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	got := h.settled(t, "wager-1")
	if got.Status() != domain.WagerStatusWon {
		t.Fatalf("ticket settled %s, want won", got.Status())
	}
	returned, _ := got.Returned()
	if returned != domain.Money(2500) {
		t.Errorf("returned %s, want the frozen potential payout of 2500 minor units", returned)
	}
	net, _ := got.NetReturn()
	if net != domain.Money(1500) {
		t.Errorf("net return %s, want 1500 minor units", net)
	}

	txn := h.onlyMovement(t)
	assertBalanced(t, txn)
	if txn.Kind() != domain.EntryKindPayout {
		t.Errorf("movement kind %s, want payout", txn.Kind())
	}
	if got := netFor(t, txn, domain.AccountKindUserEscrow, "user-1"); got != domain.Money(-1000) {
		t.Errorf("escrow moved %s, want -1000 (the stake released)", got)
	}
	if got := netFor(t, txn, domain.AccountKindUserCash, "user-1"); got != domain.Money(2500) {
		t.Errorf("cash moved %s, want +2500 (the return)", got)
	}
	if got := netFor(t, txn, domain.AccountKindHouse, ""); got != domain.Money(-1500) {
		t.Errorf("house moved %s, want -1500 (it funded the profit)", got)
	}

	if len(h.pub.sent) != 1 {
		t.Fatalf("published %d audit records, want exactly 1", len(h.pub.sent))
	}
	if h.pub.sent[0].msg.Type != MessageType {
		t.Errorf("published message type %q, want %q", h.pub.sent[0].msg.Type, MessageType)
	}
	if !h.pub.sent[0].msg.ObservedAt.Equal(finalisedAt) {
		t.Errorf("published ObservedAt %s, want the result's own instant %s",
			h.pub.sent[0].msg.ObservedAt, finalisedAt)
	}
}

// TestSettleStraightLossTakesTheStake asserts the outcome with no cash entry at
// all: the escrow goes straight to the house and the customer's cash is
// untouched, because a zero row is not a movement.
func TestSettleStraightLossTakesTheStake(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 20, 24)); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	got := h.settled(t, "wager-1")
	if got.Status() != domain.WagerStatusLost {
		t.Fatalf("ticket settled %s, want lost", got.Status())
	}
	returned, _ := got.Returned()
	if !returned.IsZero() {
		t.Errorf("a losing ticket returned %s, want nothing", returned)
	}

	txn := h.onlyMovement(t)
	assertBalanced(t, txn)
	if txn.Kind() != domain.EntryKindLoss {
		t.Errorf("movement kind %s, want loss", txn.Kind())
	}
	if got := netFor(t, txn, domain.AccountKindUserCash, "user-1"); !got.IsZero() {
		t.Errorf("cash moved %s on a loser; there must be no cash entry at all", got)
	}
	if got := netFor(t, txn, domain.AccountKindHouse, ""); got != domain.Money(1000) {
		t.Errorf("house moved %s, want +1000 (it kept the stake)", got)
	}
	if txn.EntryCount() != 2 {
		t.Errorf("a losing settlement wrote %d entries, want 2", txn.EntryCount())
	}
}

// TestSettleSpreadPushRefunds covers landing exactly on the number: the stake
// comes back, the house takes no part, and the ticket is recorded as a PUSH
// rather than as a void — the distinction between a result and a cancellation
// that domain.LegStatusPush labours over.
func TestSettleSpreadPushRefunds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := legSpec{
		id: "leg-a", event: "evt-1", market: "mkt-1", sel: "sel-home",
		typ: domain.MarketTypeSpread, role: domain.SelectionRoleHome,
		decimal: 1.91, line: mustLine(t, -3),
	}
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 1.91, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))

	// Home wins by exactly three.
	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 21)); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	got := h.settled(t, "wager-1")
	if got.Status() != domain.WagerStatusPush {
		t.Fatalf("ticket settled %s, want push", got.Status())
	}
	returned, _ := got.Returned()
	if returned != domain.Money(1000) {
		t.Errorf("returned %s, want the stake back", returned)
	}

	txn := h.onlyMovement(t)
	assertBalanced(t, txn)
	if txn.Kind() != domain.EntryKindRefund {
		t.Errorf("movement kind %s, want refund", txn.Kind())
	}
	if got := netFor(t, txn, domain.AccountKindHouse, ""); !got.IsZero() {
		t.Errorf("house moved %s on a push; it takes no part in a refund", got)
	}
}

// TestSettleCancelledEventVoidsAndRefunds is the same money as a push and a
// different status, which is the whole reason the domain keeps the two apart.
func TestSettleCancelledEventVoidsAndRefunds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))

	if err := h.svc.handleResult(context.Background(), cancelledResult("evt-1")); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	got := h.settled(t, "wager-1")
	if got.Status() != domain.WagerStatusVoid {
		t.Fatalf("ticket settled %s, want void", got.Status())
	}
	returned, _ := got.Returned()
	if returned != domain.Money(1000) {
		t.Errorf("returned %s, want the stake back", returned)
	}
	if leg := got.Legs()[0]; leg.Status() != domain.LegStatusVoid {
		t.Errorf("leg graded %s, want void", leg.Status())
	}
}

// -----------------------------------------------------------------------------
// Multi-leg tickets
// -----------------------------------------------------------------------------

// TestSettleParlayDefersUntilEveryLegIsGraded is the rule wager.go states —
// AllLegsGraded is "the precondition for grading the ticket itself" — asserted
// end to end. The first result grades one leg and moves no money; the second
// closes the ticket.
func TestSettleParlayDefersUntilEveryLegIsGraded(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-a", 2.0)
	second := moneylineHome("leg-b", "evt-2", "mkt-2", "sel-b", 1.5)
	w := mustWager(t, "wager-1", domain.WagerKindParlay, 1000, 3.0,
		first.build(t), second.build(t))
	h.tx.add(w, first.ref("wager-1"), second.ref("wager-1"))

	ctx := context.Background()
	if err := h.svc.handleResult(ctx, finalResult(t, "evt-1", 24, 20)); err != nil {
		t.Fatalf("handleResult(first): %v", err)
	}

	mid := h.settled(t, "wager-1")
	if mid.IsTerminal() {
		t.Fatalf("the ticket settled %s after one of two games; it must stay open", mid.Status())
	}
	if leg, _ := mid.Leg("leg-a"); leg.Status() != domain.LegStatusWon {
		t.Errorf("the finished leg graded %s, want won", leg.Status())
	}
	if len(h.tx.movements) != 0 {
		t.Errorf("wrote %d ledger movements for a ticket that is still running", len(h.tx.movements))
	}
	if len(h.pub.sent) != 0 {
		t.Errorf("published %d audit records for a ticket that is still running", len(h.pub.sent))
	}

	if err := h.svc.handleResult(ctx, finalResult(t, "evt-2", 30, 27)); err != nil {
		t.Fatalf("handleResult(second): %v", err)
	}

	got := h.settled(t, "wager-1")
	if got.Status() != domain.WagerStatusWon {
		t.Fatalf("ticket settled %s, want won", got.Status())
	}
	returned, _ := got.Returned()
	if returned != domain.Money(3000) {
		t.Errorf("returned %s, want the frozen 3000 payout", returned)
	}
	if len(h.pub.sent) != 1 {
		t.Errorf("published %d audit records, want exactly 1 — the settlement, not the grading",
			len(h.pub.sent))
	}
}

// TestSettleParlayWithAVoidedLegReprices asserts the removal rule: a voided leg
// contributes a multiplier of 1, so the ticket reprices as though it had never
// been added — the accepted price with the removed leg's own price divided out.
//
// The player prop is the natural fixture here, because this results feed voids
// one by design and the reasoning is recorded in grader.go.
func TestSettleParlayWithAVoidedLegReprices(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	kept := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-a", 2.0)
	prop := legSpec{
		id: "leg-b", event: "evt-1", market: "mkt-2", sel: "sel-b",
		typ: domain.MarketTypePlayerProp, role: domain.SelectionRoleOver,
		decimal: 1.5, line: mustLine(t, 62.5),
	}
	// 2.0 × 1.5 = 3.0, so the ticket was written at 3.0 and pays 3000 on 1000.
	w := mustWager(t, "wager-1", domain.WagerKindParlay, 1000, 3.0,
		kept.build(t), prop.build(t))
	h.tx.add(w, kept.ref("wager-1"), prop.ref("wager-1"))

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20)); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	got := h.settled(t, "wager-1")
	if got.Status() != domain.WagerStatusWon {
		t.Fatalf("ticket settled %s, want won", got.Status())
	}
	returned, _ := got.Returned()
	if returned != domain.Money(2000) {
		t.Errorf("returned %s, want 2000 — 3.0 with the voided 1.5 divided out, on a 1000 stake",
			returned)
	}
	if returned.Compare(got.PotentialPayout()) >= 0 {
		t.Errorf("a partially-voided parlay returned %s against a headline payout of %s; "+
			"a removal can only ever reduce the ticket", returned, got.PotentialPayout())
	}
}

// TestSettleParlayWithALosingLegIsLost asserts that a loss is absorbing: no
// arithmetic over the surviving legs can recover it.
func TestSettleParlayWithALosingLegIsLost(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	// Both legs sit on one event, so one final grades both. They back opposite
	// sides of two different markets, which is what lets a single 24-20 produce
	// one winner and one loser on one ticket.
	winner := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-a", 2.0)
	loser := legSpec{
		id: "leg-b", event: "evt-1", market: "mkt-2", sel: "sel-b",
		typ: domain.MarketTypeMoneyline, role: domain.SelectionRoleAway,
		decimal: 1.5, line: domain.NoLine(),
	}
	w := mustWager(t, "wager-1", domain.WagerKindParlay, 1000, 3.0,
		winner.build(t), loser.build(t))
	h.tx.add(w, winner.ref("wager-1"), loser.ref("wager-1"))

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20)); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	got := h.settled(t, "wager-1")
	if got.Status() != domain.WagerStatusLost {
		t.Fatalf("ticket settled %s, want lost", got.Status())
	}
	returned, _ := got.Returned()
	if !returned.IsZero() {
		t.Errorf("a parlay with a losing leg returned %s, want nothing", returned)
	}
}

// -----------------------------------------------------------------------------
// The publish interlock
// -----------------------------------------------------------------------------

// TestPublishFailureAbortsTheSettlement is the most important assertion in this
// file. doc.go's ordering rule says a refused audit publish must abort the
// transaction, and "abort" has to mean that NOTHING survives: not the leg
// grading, not the ticket's terminal status, not the ledger movement.
//
// A settlement that committed with no audit record would be undetectable — the
// customer is paid, the ledger balances, and the only evidence that it happened
// is missing from the one topic whose job is to hold it.
func TestPublishFailureAbortsTheSettlement(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.pub.err = errors.New("broker unavailable")

	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))

	err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20))
	if err == nil {
		t.Fatal("handleResult returned nil after a refused publish; the failure must reach the " +
			"caller so the cursor holds and the next poll retries")
	}

	if h.store.writeCommits != 0 {
		t.Errorf("committed %d writing transactions after a refused publish, want 0",
			h.store.writeCommits)
	}
	if h.store.rollbacks != 1 {
		t.Errorf("rolled back %d transactions, want exactly 1", h.store.rollbacks)
	}

	got := h.settled(t, "wager-1")
	if got.IsTerminal() {
		t.Errorf("the ticket is %s after a rolled-back settlement; it must still be open",
			got.Status())
	}
	if leg := got.Legs()[0]; leg.Status() != domain.LegStatusPending {
		t.Errorf("the leg is %s after a rolled-back settlement; it must still be pending",
			leg.Status())
	}
	if len(h.tx.movements) != 0 {
		t.Errorf("%d ledger movements survived a rolled-back settlement", len(h.tx.movements))
	}

	if got := testutil.ToFloat64(h.svc.metrics.publishFailures); got != 1 {
		t.Errorf("sharpline_settlement_publish_failures_total = %v, want 1", got)
	}
}

// TestPublishHappensAfterEveryWrite pins the ordering inside the transaction.
// The publish is last so that the window in which an event exists for a
// settlement that was rolled back is as narrow as it can be made without an
// outbox; if it ever moves earlier, this fails.
func TestPublishHappensAfterEveryWrite(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.pub.err = errors.New("broker unavailable")

	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20)); err == nil {
		t.Fatal("expected the refused publish to fail the transaction")
	}

	if h.tx.grades != 1 {
		t.Errorf("graded %d legs before the publish, want 1", h.tx.grades)
	}
	if h.tx.settles != 1 {
		t.Errorf("wrote %d wager settlements before the publish, want 1", h.tx.settles)
	}
	if h.tx.inserts != 1 {
		t.Errorf("wrote %d ledger movements before the publish, want 1", h.tx.inserts)
	}
}

// -----------------------------------------------------------------------------
// Idempotency
// -----------------------------------------------------------------------------

// TestRedeliveredResultSettlesOnce covers the routine case the inclusive cursor
// boundary guarantees will happen: the same result arriving twice.
func TestRedeliveredResultSettlesOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))

	ctx := context.Background()
	res := finalResult(t, "evt-1", 24, 20)
	for i := range 3 {
		if err := h.svc.handleResult(ctx, res); err != nil {
			t.Fatalf("handleResult (delivery %d): %v", i+1, err)
		}
	}

	if len(h.tx.movements) != 1 {
		t.Errorf("three deliveries wrote %d ledger movements, want exactly 1", len(h.tx.movements))
	}
	if len(h.pub.sent) != 1 {
		t.Errorf("three deliveries published %d audit records, want exactly 1", len(h.pub.sent))
	}
	returned, _ := h.settled(t, "wager-1").Returned()
	if returned != domain.Money(2500) {
		t.Errorf("returned %s after three deliveries, want 2500 — paid once", returned)
	}
}

// TestSettlementTransactionIDIsDeterministic is the third idempotency guard on
// its own: the identifier is a pure function of the wager, so a replay collides
// on the primary key instead of writing a second balanced payout.
func TestSettlementTransactionIDIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := SettlementTransactionID("wager-abc")
	if err != nil {
		t.Fatalf("SettlementTransactionID: %v", err)
	}
	second, err := SettlementTransactionID("wager-abc")
	if err != nil {
		t.Fatalf("SettlementTransactionID: %v", err)
	}
	if first != second {
		t.Errorf("the same wager produced %q and %q", first, second)
	}

	other, err := SettlementTransactionID("wager-abd")
	if err != nil {
		t.Fatalf("SettlementTransactionID: %v", err)
	}
	if other == first {
		t.Errorf("two different wagers produced the same transaction id %q", first)
	}
}

// TestSettlementTransactionIDFallsBackToADigest covers the branch that keeps a
// long wager identifier settleable. domain.NewTransactionID refuses anything past
// MaxIDLen, so without the fallback such a ticket could not name its own
// settlement and would never close.
func TestSettlementTransactionIDFallsBackToADigest(t *testing.T) {
	t.Parallel()

	long := domain.WagerID("")
	for len(long) < domain.MaxIDLen {
		long += "w"
	}

	id, err := SettlementTransactionID(long)
	if err != nil {
		t.Fatalf("SettlementTransactionID on a maximum-length wager id: %v", err)
	}
	if len(id) > domain.MaxIDLen {
		t.Errorf("the digest form is %d bytes, over the %d limit", len(id), domain.MaxIDLen)
	}
	again, err := SettlementTransactionID(long)
	if err != nil {
		t.Fatalf("SettlementTransactionID: %v", err)
	}
	if id != again {
		t.Errorf("the digest form is not deterministic: %q then %q", id, again)
	}
}

// -----------------------------------------------------------------------------
// The money rules, in isolation
// -----------------------------------------------------------------------------

// TestDecideTicket exercises the outcome rules directly, so that each one is
// pinned by a case that names it rather than only by an end-to-end path.
func TestDecideTicket(t *testing.T) {
	t.Parallel()

	// Two legs at 2.0 and 1.5, a parlay written at 3.0 on a 1000 stake.
	build := func(t *testing.T, a, b domain.LegStatus) domain.Wager {
		t.Helper()
		first := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-a", 2.0).build(t)
		second := moneylineHome("leg-b", "evt-2", "mkt-2", "sel-b", 1.5).build(t)
		w := mustWager(t, "wager-1", domain.WagerKindParlay, 1000, 3.0, first, second)

		at := placedAt.Add(time.Hour)
		w, err := w.GradeLeg("leg-a", a, at)
		if err != nil {
			t.Fatalf("GradeLeg(leg-a, %s): %v", a, err)
		}
		w, err = w.GradeLeg("leg-b", b, at)
		if err != nil {
			t.Fatalf("GradeLeg(leg-b, %s): %v", b, err)
		}
		return w
	}

	cases := []struct {
		name     string
		a, b     domain.LegStatus
		want     domain.WagerStatus
		returned int64
	}{
		{"both won", domain.LegStatusWon, domain.LegStatusWon, domain.WagerStatusWon, 3000},
		{"one lost is lost", domain.LegStatusWon, domain.LegStatusLost, domain.WagerStatusLost, 0},
		{"a loss beats a void", domain.LegStatusVoid, domain.LegStatusLost, domain.WagerStatusLost, 0},
		{"both pushed refunds as a push", domain.LegStatusPush, domain.LegStatusPush, domain.WagerStatusPush, 1000},
		{"both voided refunds as a void", domain.LegStatusVoid, domain.LegStatusVoid, domain.WagerStatusVoid, 1000},
		{"a void among pushes makes the ticket void", domain.LegStatusPush, domain.LegStatusVoid, domain.WagerStatusVoid, 1000},
		{"a pushed leg is divided out", domain.LegStatusWon, domain.LegStatusPush, domain.WagerStatusWon, 2000},
		{"a voided leg is divided out", domain.LegStatusVoid, domain.LegStatusWon, domain.WagerStatusWon, 1500},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := decideTicket(build(t, c.a, c.b))
			if err != nil {
				t.Fatalf("decideTicket: %v", err)
			}
			if got.status != c.want {
				t.Errorf("status %s, want %s", got.status, c.want)
			}
			if got.returned != domain.Money(c.returned) {
				t.Errorf("returned %s, want %d minor units", got.returned, c.returned)
			}
		})
	}
}

// TestDecideTicketRefundsATeaserWithARemovedLeg pins the house policy stated in
// decideTicket and in errors.go: this system holds no teaser price schedule, so
// a teaser that loses a leg to a void or a push cannot be repriced honestly.
//
// The two real-world alternatives are named there — "reduce to the next-shorter
// teaser", which needs a schedule that does not exist, and "a push loses the
// teaser", which is a harsher rule than a play-money book should adopt by
// default — and refunding is the only rule that cannot pay a number nobody
// quoted. Changing it means changing this test, which is where the reasoning
// gets re-read.
//
// Note the leg that WON: without the teaser branch the ticket would reprice off
// the accepted price with the pushed leg's UNTEASED market price divided out,
// which is a number bearing no relation to what the customer bought.
func TestDecideTicketRefundsATeaserWithARemovedLeg(t *testing.T) {
	t.Parallel()

	// A six-point teaser: each leg's line is moved exactly six points in the
	// bettor's favour, which is what domain.NewWager's validateTeaser checks.
	teased := func(t *testing.T, id, market, sel string, booked, moved float64) domain.Leg {
		t.Helper()
		leg, err := domain.NewLeg(domain.LegParams{
			ID:          domain.LegID(id),
			EventID:     "evt-1",
			MarketID:    domain.MarketID(market),
			MarketType:  domain.MarketTypeSpread,
			Role:        domain.SelectionRoleHome,
			SelectionID: domain.SelectionID(sel),
			Price:       mustPrice(t, sel, 1.91, mustLine(t, booked)),
			TeasedLine:  mustLine(t, moved),
		})
		if err != nil {
			t.Fatalf("domain.NewLeg(%s): %v", id, err)
		}
		return leg
	}

	w, err := domain.NewWager(domain.WagerParams{
		ID:     "wager-1",
		UserID: "user-1",
		Kind:   domain.WagerKindTeaser,
		Legs: []domain.Leg{
			teased(t, "leg-a", "mkt-1", "sel-a", -3.5, 2.5),
			teased(t, "leg-b", "mkt-2", "sel-b", -7, -1),
		},
		Stake:           domain.Money(1000),
		AcceptedDecimal: 1.9,
		Rounding:        domain.RoundHalfAwayFromZero,
		TeaserPoints:    6,
		PlacedAt:        placedAt,
	})
	if err != nil {
		t.Fatalf("domain.NewWager(teaser): %v", err)
	}

	at := placedAt.Add(time.Hour)
	w, err = w.GradeLeg("leg-a", domain.LegStatusWon, at)
	if err != nil {
		t.Fatalf("GradeLeg(leg-a): %v", err)
	}
	w, err = w.GradeLeg("leg-b", domain.LegStatusPush, at)
	if err != nil {
		t.Fatalf("GradeLeg(leg-b): %v", err)
	}

	got, err := decideTicket(w)
	if err != nil {
		t.Fatalf("decideTicket: %v", err)
	}
	if got.status != domain.WagerStatusVoid {
		t.Errorf("status %s, want void", got.status)
	}
	if got.returned != w.Stake() {
		t.Errorf("returned %s, want the stake %s", got.returned, w.Stake())
	}
	if got.note != ErrNoTeaserSchedule.Error() {
		t.Errorf("the teaser refund carried the note %q, want the policy named by "+
			"ErrNoTeaserSchedule", got.note)
	}
	if _, err := w.Settle(got.status, got.returned, at); err != nil {
		t.Errorf("domain.Wager.Settle refused the computed outcome: %v", err)
	}
}

// TestDecideTicketNeverPaysLessThanTheStakeOnAWin pins the floor. A correlated
// same-game parlay is priced BELOW the naive product of its legs, so dividing a
// removed leg out of it can drop the effective price under 1 — at which point
// the ticket must return the stake, because domain.Wager.Settle refuses a win
// that costs the customer money and is right to.
func TestDecideTicketNeverPaysLessThanTheStakeOnAWin(t *testing.T) {
	t.Parallel()

	first := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-a", 2.0).build(t)
	second := moneylineHome("leg-b", "evt-1", "mkt-2", "sel-b", 3.0).build(t)
	// A heavily correlation-adjusted same-game parlay: quoted at 2.4 against a
	// naive product of 6.0. Removing the 3.0 leg leaves 0.8.
	w := mustWager(t, "wager-1", domain.WagerKindParlay, 1000, 2.4, first, second)

	at := placedAt.Add(time.Hour)
	w, err := w.GradeLeg("leg-a", domain.LegStatusWon, at)
	if err != nil {
		t.Fatalf("GradeLeg: %v", err)
	}
	w, err = w.GradeLeg("leg-b", domain.LegStatusVoid, at)
	if err != nil {
		t.Fatalf("GradeLeg: %v", err)
	}

	got, err := decideTicket(w)
	if err != nil {
		t.Fatalf("decideTicket: %v", err)
	}
	if got.status != domain.WagerStatusWon {
		t.Fatalf("status %s, want won", got.status)
	}
	if got.returned != w.Stake() {
		t.Errorf("returned %s, want the stake %s", got.returned, w.Stake())
	}
	if got.note == "" {
		t.Error("the floor was applied with no note; a settlement that did not follow from the " +
			"legs must say so")
	}
	// The domain must accept what this computed. If it does not, the rule and
	// the guard disagree and the ticket is unsettleable.
	if _, err := w.Settle(got.status, got.returned, at); err != nil {
		t.Errorf("domain.Wager.Settle refused the computed outcome: %v", err)
	}
}

// -----------------------------------------------------------------------------
// The loop
// -----------------------------------------------------------------------------

// TestResumeStartsFromTheOldestOpenWager asserts the cursor seed, including the
// lookback that absorbs skew between this system's placement clock and the
// provider's observation clock.
func TestResumeStartsFromTheOldestOpenWager(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.store.oldest, h.store.oldestOK = placedAt, true

	if err := h.svc.resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	want := placedAt.Add(-DefaultResumeLookback)
	if !h.svc.Cursor().Equal(want) {
		t.Errorf("cursor %s, want the oldest open placement less the lookback (%s)",
			h.svc.Cursor(), want)
	}
}

// TestResumeOnAnEmptyBookStartsFromNow covers the fresh database: nothing is
// open, so no historical result can pay anybody and re-reading history would be
// work with no possible outcome.
func TestResumeOnAnEmptyBookStartsFromNow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.svc.resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if h.svc.Cursor().Before(finalisedAt) {
		t.Errorf("cursor %s is in the past on an empty book", h.svc.Cursor())
	}
}

// TestResumeFailureIsFatal covers the deliberate choice in resume: a service
// that could not read its cursor must refuse to start rather than run from the
// wrong one and silently under-settle.
func TestResumeFailureIsFatal(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.store.oldestErr = errors.New("pool exhausted")

	if err := h.svc.Run(context.Background()); err == nil {
		t.Fatal("Run returned nil when the cursor could not be seeded")
	}
	if h.svc.Check(context.Background()) == nil {
		t.Error("the readiness check passed for a service that never started")
	}
}

// TestUnusableResultIsSkippedNotRetried covers the permanent-failure path: a row
// that is not a result cannot become one, so it is counted and stepped over
// rather than left to block every later result behind it.
func TestUnusableResultIsSkippedNotRetried(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	bad := Result{
		EventID:     "evt-1",
		Status:      domain.EventStatusEnded,
		FinalisedAt: finalisedAt,
		// No score, which is what makes it unusable.
	}
	if err := h.svc.handleResult(context.Background(), bad); err != nil {
		t.Fatalf("handleResult returned %v; an unusable row must be skipped, not retried", err)
	}
	if h.store.commits != 0 || h.store.rollbacks != 0 {
		t.Errorf("an unusable row opened %d committed and %d rolled-back transactions; "+
			"it must not reach the store at all", h.store.commits, h.store.rollbacks)
	}
}

// TestGroupByWagerPreservesOrder pins the property the comment on groupByWager
// claims: two replicas walking one result take the same tickets in the same
// order, which turns contention into a short row-lock wait rather than a
// deadlock.
func TestGroupByWagerPreservesOrder(t *testing.T) {
	t.Parallel()

	refs := []LegRef{
		{LegID: "l1", WagerID: "w1", EventID: "e1"},
		{LegID: "l2", WagerID: "w2", EventID: "e1"},
		{LegID: "l3", WagerID: "w1", EventID: "e1"},
		{LegID: "l4", WagerID: "w3", EventID: "e1"},
		{LegID: "l5", WagerID: "w2", EventID: "e1"},
	}

	got := groupByWager(refs)
	want := []struct {
		wager string
		legs  int
	}{{"w1", 2}, {"w2", 2}, {"w3", 1}}

	if len(got) != len(want) {
		t.Fatalf("grouped into %d tickets, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].wagerID != domain.WagerID(w.wager) {
			t.Errorf("group %d is %s, want %s", i, got[i].wagerID, w.wager)
		}
		if len(got[i].legs) != w.legs {
			t.Errorf("group %s holds %d legs, want %d", w.wager, len(got[i].legs), w.legs)
		}
	}
}

// -----------------------------------------------------------------------------
// Construction
// -----------------------------------------------------------------------------

// TestNewRefusesIncompleteOptions covers each required dependency. The publisher
// case is the one that matters: it is not merely a missing collaborator, it is
// the interlock that makes "a settlement always has an audit record" true.
func TestNewRefusesIncompleteOptions(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	full := ServiceOptions{
		Results:   &fakeResults{},
		Store:     &fakeStore{tx: newFakeTx(), t: t},
		Publisher: &fakePublisher{},
		Logger:    log,
	}
	if _, err := New(full); err != nil {
		t.Fatalf("the complete options do not build: %v", err)
	}

	cases := map[string]func(ServiceOptions) ServiceOptions{
		"no results source":   func(o ServiceOptions) ServiceOptions { o.Results = nil; return o },
		"no store":            func(o ServiceOptions) ServiceOptions { o.Store = nil; return o },
		"no publisher":        func(o ServiceOptions) ServiceOptions { o.Publisher = nil; return o },
		"no logger":           func(o ServiceOptions) ServiceOptions { o.Logger = nil; return o },
		"negative poll":       func(o ServiceOptions) ServiceOptions { o.PollInterval = -1; return o },
		"negative batch":      func(o ServiceOptions) ServiceOptions { o.ResultBatch = -1; return o },
		"negative tx timeout": func(o ServiceOptions) ServiceOptions { o.TxTimeout = -1; return o },
		"negative lookback":   func(o ServiceOptions) ServiceOptions { o.ResumeLookback = -1; return o },
		"negative leg batch":  func(o ServiceOptions) ServiceOptions { o.PendingLegBatch = -1; return o },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(mutate(full)); !errors.Is(err, ErrInvalidOptions) {
				t.Errorf("New with %s = %v, want ErrInvalidOptions", name, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Contention and failure classification
// -----------------------------------------------------------------------------

// TestConcurrentSettlementIsAConflictNotAFailure covers the race two settle
// replicas make routine: this transaction loaded a live ticket and found it
// terminal by the time it tried to write. Nothing is lost and nothing is worth
// retrying, so the result is consumed and the cursor is free to advance.
func TestConcurrentSettlementIsAConflictNotAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))
	h.tx.settleErr = fmt.Errorf("%w: wager-1", ErrWagerAlreadySettled)

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20)); err != nil {
		t.Fatalf("handleResult returned %v; losing a settlement race is not a failure", err)
	}
	if h.store.writeCommits != 0 {
		t.Errorf("committed %d writing transactions after losing the race, want 0",
			h.store.writeCommits)
	}
	if h.store.rollbacks != 1 {
		t.Errorf("rolled back %d transactions, want exactly 1", h.store.rollbacks)
	}
	if len(h.pub.sent) != 0 {
		t.Errorf("published %d audit records for a settlement that did not happen", len(h.pub.sent))
	}
}

// TestDuplicateLedgerTransactionIsAConflict covers the third idempotency guard
// firing: the deterministic transaction identifier collided, which means this
// ticket has already been paid. It is the guard doing its job, not an error.
func TestDuplicateLedgerTransactionIsAConflict(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))
	h.tx.insertErr = fmt.Errorf("%w: stl.wager-1", ErrTransactionExists)

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20)); err != nil {
		t.Fatalf("handleResult returned %v; a colliding transaction id means already paid", err)
	}
	if len(h.tx.movements) != 0 {
		t.Errorf("%d ledger movements survived the collision", len(h.tx.movements))
	}
}

// TestTransientStoreFailureIsReturned is the other half of the classification.
// A database error says nothing about this ticket, so it must NOT be written off
// as permanent: it is returned, the cursor holds, and the next poll retries from
// a fresh read.
func TestTransientStoreFailureIsReturned(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))
	h.tx.loadErr = errors.New("connection reset by peer")

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20)); err == nil {
		t.Fatal("a database failure was swallowed; it must reach the caller so the cursor holds")
	}
	if h.settled(t, "wager-1").IsTerminal() {
		t.Error("the ticket settled despite the failure")
	}
}

// TestLegGradedConcurrentlyIsAConflict covers the same race one level down: the
// ticket was live when it was loaded but one of its legs was graded by another
// replica before this transaction reached the write.
func TestLegGradedConcurrentlyIsAConflict(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))
	h.tx.gradeErr = fmt.Errorf("%w: leg-a", ErrLegAlreadyGraded)

	if err := h.svc.handleResult(context.Background(), finalResult(t, "evt-1", 24, 20)); err != nil {
		t.Fatalf("handleResult returned %v; losing a grading race is not a failure", err)
	}
	if h.store.writeCommits != 0 {
		t.Errorf("committed %d writing transactions, want 0", h.store.writeCommits)
	}
}

// -----------------------------------------------------------------------------
// The cursor
// -----------------------------------------------------------------------------

// TestPollAdvancesTheCursorPerResult asserts the cursor moves as each result is
// finished with, not once at the end of a batch. A batch abandoned halfway must
// not re-read the results it already settled.
func TestPollAdvancesTheCursorPerResult(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := finalResult(t, "evt-1", 24, 20)
	second := finalResult(t, "evt-2", 30, 27)
	second.FinalisedAt = finalisedAt.Add(time.Minute)
	h.results.batches = [][]Result{{first, second}}

	read, err := h.svc.poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if read != 2 {
		t.Errorf("poll read %d results, want 2", read)
	}
	if !h.svc.Cursor().Equal(second.FinalisedAt) {
		t.Errorf("cursor %s, want the last result's instant %s",
			h.svc.Cursor(), second.FinalisedAt)
	}
	if len(h.results.watermark) != 1 || !h.results.watermark[0].IsZero() {
		t.Errorf("the feed was asked from %v, want the seeded zero cursor", h.results.watermark)
	}
}

// TestPollParksTheCursorOnTheFailingResult asserts the retry boundary. The feed
// is inclusive, so parking ON the failure re-reads exactly it and everything
// after it — and skips the results before it, which are already done.
func TestPollParksTheCursorOnTheFailingResult(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	spec := moneylineHome("leg-a", "evt-2", "mkt-1", "sel-home", 2.5)
	w := mustWager(t, "wager-1", domain.WagerKindStraight, 1000, 2.5, spec.build(t))
	h.tx.add(w, spec.ref("wager-1"))
	h.tx.loadErr = errors.New("connection reset by peer")

	// evt-1 has no open legs and completes; evt-2 fails.
	first := finalResult(t, "evt-1", 24, 20)
	second := finalResult(t, "evt-2", 30, 27)
	second.FinalisedAt = finalisedAt.Add(time.Minute)
	h.results.batches = [][]Result{{first, second}}

	if _, err := h.svc.poll(context.Background()); err == nil {
		t.Fatal("poll returned nil despite a failed settlement")
	}
	if !h.svc.Cursor().Equal(second.FinalisedAt) {
		t.Errorf("cursor %s, want it parked on the failing result at %s",
			h.svc.Cursor(), second.FinalisedAt)
	}
}

// TestPollFailureLeavesTheCursorAlone covers the read itself failing: there is
// nothing to advance past, so the next tick asks the same question again.
func TestPollFailureLeavesTheCursorAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.svc.setCursor(finalisedAt)
	h.results.err = errors.New("statement timeout")

	if _, err := h.svc.poll(context.Background()); err == nil {
		t.Fatal("poll returned nil despite a failed read")
	}
	if !h.svc.Cursor().Equal(finalisedAt) {
		t.Errorf("cursor moved to %s after a failed read", h.svc.Cursor())
	}
}

// TestCursorNeverMovesBackwards pins the guard in setCursor. A cursor that could
// regress would re-settle history, and the cost of the comparison is nothing
// against the cost of finding that out the hard way.
func TestCursorNeverMovesBackwards(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.svc.setCursor(finalisedAt)
	h.svc.setCursor(finalisedAt.Add(-time.Hour))

	if !h.svc.Cursor().Equal(finalisedAt) {
		t.Errorf("cursor %s, want it held at %s", h.svc.Cursor(), finalisedAt)
	}
}

// -----------------------------------------------------------------------------
// The published record
// -----------------------------------------------------------------------------

// TestPublishedRecordReconstructsTheSettlement asserts what the audit trail is
// for: everything needed to check the payout WITHOUT the database. The entries
// must sum to zero on the decoded integers, the legs must all be there — not
// only the one this result decided — and the money must be in minor units.
func TestPublishedRecordReconstructsTheSettlement(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := moneylineHome("leg-a", "evt-1", "mkt-1", "sel-a", 2.0)
	second := moneylineHome("leg-b", "evt-2", "mkt-2", "sel-b", 1.5)
	w := mustWager(t, "wager-1", domain.WagerKindParlay, 1000, 3.0,
		first.build(t), second.build(t))
	h.tx.add(w, first.ref("wager-1"), second.ref("wager-1"))

	ctx := context.Background()
	if err := h.svc.handleResult(ctx, finalResult(t, "evt-1", 24, 20)); err != nil {
		t.Fatalf("handleResult(first): %v", err)
	}
	if err := h.svc.handleResult(ctx, finalResult(t, "evt-2", 30, 27)); err != nil {
		t.Fatalf("handleResult(second): %v", err)
	}

	if len(h.pub.sent) != 1 {
		t.Fatalf("published %d records, want 1", len(h.pub.sent))
	}
	rec, ok := h.pub.sent[0].msg.Payload.(SettledWager)
	if !ok {
		t.Fatalf("payload is %T, want a SettledWager", h.pub.sent[0].msg.Payload)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("the published record does not validate: %v", err)
	}

	if rec.Wager.ReturnedMinor != 3000 {
		t.Errorf("returned_minor is %d, want 3000", rec.Wager.ReturnedMinor)
	}
	if rec.Wager.NetReturnMinor != 2000 {
		t.Errorf("net_return_minor is %d, want 2000", rec.Wager.NetReturnMinor)
	}
	if len(rec.Legs) != 2 {
		t.Errorf("the record carries %d legs, want both of them — a settlement has to show "+
			"why it paid what it paid, not only the leg that finished last", len(rec.Legs))
	}
	for _, leg := range rec.Legs {
		if leg.Status != domain.LegStatusWon {
			t.Errorf("leg %s published as %s, want won", leg.ID, leg.Status)
		}
		if leg.GradedAt == nil {
			t.Errorf("leg %s published with no graded_at", leg.ID)
		}
	}

	wantID, err := SettlementTransactionID("wager-1")
	if err != nil {
		t.Fatalf("SettlementTransactionID: %v", err)
	}
	if rec.Settlement.ID != wantID {
		t.Errorf("settlement id %q, want the derived %q", rec.Settlement.ID, wantID)
	}
	if h.pub.sent[0].msg.ID != wantID.String() {
		t.Errorf("message id %q, want the derived transaction id %q",
			h.pub.sent[0].msg.ID, wantID)
	}
	if rec.Result.EventID != "evt-2" {
		t.Errorf("the record names event %s, want the one that decided the last leg", rec.Result.EventID)
	}
	if rec.Result.HomeScore == nil || *rec.Result.HomeScore != 30 {
		t.Errorf("the record's home score is %v, want 30", rec.Result.HomeScore)
	}
}

// TestValidateRejectsAnUnbalancedRecord is the consumer-side check, asserted
// from this side: a record whose entries do not sum to zero is self-
// contradicting and must never leave the process.
func TestValidateRejectsAnUnbalancedRecord(t *testing.T) {
	t.Parallel()

	rec := SettledWager{
		SchemaVersion: SchemaVersion,
		Wager:         WagerRecord{ID: "wager-1", Status: domain.WagerStatusWon},
		Legs:          []LegRecord{{ID: "leg-a"}},
		Settlement: TransactionRecord{
			Entries: []EntryRecord{
				{AccountKind: domain.AccountKindUserEscrow, AmountMinor: -1000},
				{AccountKind: domain.AccountKindUserCash, AmountMinor: 2500},
			},
		},
	}
	if err := rec.Validate(); !errors.Is(err, domain.ErrUnbalancedTransaction) {
		t.Errorf("Validate on an unbalanced record = %v, want ErrUnbalancedTransaction", err)
	}
}
