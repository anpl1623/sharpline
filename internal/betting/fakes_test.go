// Fakes and fixtures for the betting tests.
//
// This package must be unit-testable with no database, which is what the
// consumer-declared ports in ports.go buy. The fakes here are deliberately
// dumb: maps and slices, one injectable error per method, and no behaviour that
// is not stated in the port's own doc comment. A fake that is clever enough to
// have its own bugs stops being evidence about the code under test.
//
// The one place a fake DOES model real behaviour precisely is
// [fakeTx.InsertWager]: it returns [ErrAlreadyPlaced] on a repeated id, exactly
// as the port contract requires of a real store on a wagers_pkey collision.
// That contract is the whole idempotency mechanism, so a fake that did not
// honour it would make every replay test vacuous.
package betting

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
)

// discardLogger keeps the fakes' absorbed-failure warnings out of test output.
// Those warnings are asserted through behaviour — the placement still succeeds
// — rather than by scraping the log.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Fixture instants. Fixed rather than time.Now() so that every staleness and
// window assertion is deterministic.
var (
	testNow      = time.Date(2026, 8, 20, 18, 30, 0, 0, time.UTC)
	testObserved = testNow.Add(-2 * time.Second)
)

func testClock() time.Time { return testNow }

// -----------------------------------------------------------------------------
// Domain fixture builders
// -----------------------------------------------------------------------------

func mustPrice(t *testing.T, sel domain.SelectionID, book domain.BookID, decimal float64, line domain.Line, at time.Time) domain.Price {
	t.Helper()
	p, err := domain.NewPrice(domain.PriceParams{
		SelectionID: sel,
		BookID:      book,
		Decimal:     decimal,
		Line:        line,
		ObservedAt:  at,
	})
	if err != nil {
		t.Fatalf("NewPrice(%s, %g): %v", sel, decimal, err)
	}
	return p
}

func mustLine(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("NewLine(%g): %v", v, err)
	}
	return l
}

// moneylineQuote is the simplest usable quote: no line, one selection, one
// market, one event.
func moneylineQuote(t *testing.T, n int, decimal float64) Quote {
	t.Helper()
	sel := domain.SelectionID(fmt.Sprintf("sel-%d", n))
	return Quote{
		Price:      mustPrice(t, sel, testBook, decimal, domain.NoLine(), testObserved),
		EventID:    domain.EventID(fmt.Sprintf("evt-%d", n)),
		MarketID:   domain.MarketID(fmt.Sprintf("mkt-%d", n)),
		MarketType: domain.MarketTypeMoneyline,
		Role:       domain.SelectionRoleHome,
	}
}

// spreadQuote carries a line, so it can be teased and so a line-move test has
// something to move.
func spreadQuote(t *testing.T, n int, decimal, line float64) Quote {
	t.Helper()
	sel := domain.SelectionID(fmt.Sprintf("sel-%d", n))
	return Quote{
		Price:      mustPrice(t, sel, testBook, decimal, mustLine(t, line), testObserved),
		EventID:    domain.EventID(fmt.Sprintf("evt-%d", n)),
		MarketID:   domain.MarketID(fmt.Sprintf("mkt-%d", n)),
		MarketType: domain.MarketTypeSpread,
		Role:       domain.SelectionRoleHome,
	}
}

const (
	testUser = domain.UserID("user-1")
	testBook = domain.BookID("book-1")
	testKey  = "idem-key-1"
)

// -----------------------------------------------------------------------------
// Fake store
// -----------------------------------------------------------------------------

// fakeStore hands out one shared [fakeTx], so state written inside a
// transaction is visible to the next one. That is what makes the replay tests
// meaningful: the second Place must see the wager the first one wrote.
type fakeStore struct {
	tx *fakeTx

	// inTxErr, when set, fails the transaction as a whole — the stand-in for a
	// COMMIT rejected by the deferred zero-sum trigger, which is the failure
	// postgres.InTx exists to surface.
	inTxErr error

	calls int
}

func (s *fakeStore) InTx(ctx context.Context, fn func(context.Context, Tx) error) error {
	s.calls++
	if err := fn(ctx, s.tx); err != nil {
		return err
	}
	return s.inTxErr
}

// fakeTx implements [Tx] over maps.
type fakeTx struct {
	status  string
	balance domain.Money
	limits  []Limit
	sums    map[domain.EntryKind]domain.Money

	quotes  map[domain.SelectionID]Quote
	markets map[domain.MarketID]MarketState

	wagers       map[domain.WagerID]domain.Wager
	roundRobins  map[domain.RoundRobinID]domain.RoundRobin
	transactions []domain.Transaction
	audits       []AuditEntry

	// Injectable failures, one per method that has an interesting error path.
	statusErr  error
	balanceErr error
	limitsErr  error
	sumErr     error
	quoteErr   error
	auditErr   error

	// Call counters, so a test can assert the fast path skipped the work rather
	// than merely producing the same answer.
	statusCalls  int
	balanceCalls int
	quoteCalls   int
	auditCalls   int
}

func newFakeTx() *fakeTx {
	return &fakeTx{
		status:      auth.UserStatusActive.String(),
		balance:     domain.Money(1_000_00),
		sums:        map[domain.EntryKind]domain.Money{},
		quotes:      map[domain.SelectionID]Quote{},
		markets:     map[domain.MarketID]MarketState{},
		wagers:      map[domain.WagerID]domain.Wager{},
		roundRobins: map[domain.RoundRobinID]domain.RoundRobin{},
	}
}

// withQuote registers a quote and an open market for it, which is the state
// nearly every test wants.
func (tx *fakeTx) withQuote(q Quote) *fakeTx {
	tx.quotes[q.Price.SelectionID()] = q
	tx.markets[q.MarketID] = MarketState{
		MarketID:       q.MarketID,
		EventID:        q.EventID,
		Status:         domain.MarketStatusOpen,
		EventStatus:    domain.EventStatusScheduled,
		ScheduledStart: testNow.Add(time.Hour),
	}
	return tx
}

func (tx *fakeTx) UserStatus(_ context.Context, _ domain.UserID) (string, error) {
	tx.statusCalls++
	if tx.statusErr != nil {
		return "", tx.statusErr
	}
	return tx.status, nil
}

func (tx *fakeTx) Balance(_ context.Context, _ domain.Account) (domain.Money, error) {
	tx.balanceCalls++
	if tx.balanceErr != nil {
		return 0, tx.balanceErr
	}
	return tx.balance, nil
}

func (tx *fakeTx) LimitsInForce(_ context.Context, _ domain.UserID, _ time.Time) ([]Limit, error) {
	if tx.limitsErr != nil {
		return nil, tx.limitsErr
	}
	return tx.limits, nil
}

func (tx *fakeTx) SumEntries(_ context.Context, _ domain.Account, kinds []domain.EntryKind, _ time.Time) (domain.Money, error) {
	if tx.sumErr != nil {
		return 0, tx.sumErr
	}
	total := domain.ZeroMoney
	for _, k := range kinds {
		total += tx.sums[k]
	}
	return total, nil
}

func (tx *fakeTx) QuoteFor(_ context.Context, sel domain.SelectionID, book domain.BookID) (Quote, error) {
	tx.quoteCalls++
	if tx.quoteErr != nil {
		return Quote{}, tx.quoteErr
	}
	q, ok := tx.quotes[sel]
	if !ok || q.Price.BookID() != book {
		return Quote{}, fmt.Errorf("no quote for %s at %s: %w", sel, book, ErrQuoteUnavailable)
	}
	return q, nil
}

func (tx *fakeTx) MarketState(_ context.Context, m domain.MarketID) (MarketState, error) {
	state, ok := tx.markets[m]
	if !ok {
		return MarketState{}, fmt.Errorf("no market %s: %w", m, ErrMarketNotOpen)
	}
	return state, nil
}

func (tx *fakeTx) InsertRoundRobin(_ context.Context, rr domain.RoundRobin) error {
	if _, dup := tx.roundRobins[rr.ID()]; dup {
		return fmt.Errorf("round robin %s: %w", rr.ID(), ErrAlreadyPlaced)
	}
	tx.roundRobins[rr.ID()] = rr
	return nil
}

// InsertWager honours the port's idempotency contract: a repeated primary key
// is [ErrAlreadyPlaced], not a generic failure. See the file header.
func (tx *fakeTx) InsertWager(_ context.Context, w domain.Wager) error {
	if _, dup := tx.wagers[w.ID()]; dup {
		return fmt.Errorf("wager %s: %w", w.ID(), ErrAlreadyPlaced)
	}
	tx.wagers[w.ID()] = w
	return nil
}

// InsertTransaction mirrors ledger_transactions' PRIMARY KEY, exactly as
// InsertWager mirrors wagers_pkey.
//
// The duplicate check is not decoration: a grant is idempotent through its
// derived transaction id, so a fake that silently accepted the same id twice
// would let a replayed top-up look like it credited the customer again and the
// unit test for the one movement that CREATES money would be asserting nothing.
func (tx *fakeTx) InsertTransaction(_ context.Context, t domain.Transaction) error {
	for _, existing := range tx.transactions {
		if existing.ID() == t.ID() {
			return fmt.Errorf("ledger transaction %s: %w", t.ID(), ErrAlreadyPlaced)
		}
	}
	tx.transactions = append(tx.transactions, t)
	return nil
}

func (tx *fakeTx) GrantCredit(
	_ context.Context,
	id domain.TransactionID,
	u domain.UserID,
) (domain.Money, time.Time, error) {
	cash, err := domain.UserCashAccount(u)
	if err != nil {
		return 0, time.Time{}, err
	}
	for _, t := range tx.transactions {
		if t.ID() != id || t.Kind() != domain.EntryKindGrant {
			continue
		}
		for _, entry := range t.Entries() {
			if entry.Account() == cash {
				return entry.Amount(), t.OccurredAt(), nil
			}
		}
	}
	return 0, time.Time{}, fmt.Errorf("transaction %s: %w", id, ErrGrantNotFound)
}

func (tx *fakeTx) WagerByID(_ context.Context, id domain.WagerID) (domain.Wager, error) {
	w, ok := tx.wagers[id]
	if !ok {
		return domain.Wager{}, fmt.Errorf("wager %s: %w", id, ErrWagerNotFound)
	}
	return w, nil
}

// RecordAudit appends to a slice, so a test can assert what the transaction
// recorded rather than only that it succeeded.
//
// The entries are NOT rolled back when the fake transaction fails, and no test
// should read them as though they were: [fakeStore.InTx] has no rollback to
// model. What the real adapter guarantees — the row shares the placement's fate
// — is a property of the pgx transaction and is asserted in the integration
// tier, not here. Here the assertion is that the service ASKED, with the right
// action, entity and diff.
func (tx *fakeTx) RecordAudit(_ context.Context, e AuditEntry) error {
	tx.auditCalls++
	if tx.auditErr != nil {
		return tx.auditErr
	}
	tx.audits = append(tx.audits, e)
	return nil
}

// -----------------------------------------------------------------------------
// Fake cache, wagers, fair prices
// -----------------------------------------------------------------------------

type fakeCache struct {
	entries map[string][]domain.WagerID

	lookupErr error
	recordErr error

	lookups int
	records int
}

func newFakeCache() *fakeCache { return &fakeCache{entries: map[string][]domain.WagerID{}} }

func cacheKey(u domain.UserID, key string) string { return u.String() + "\x00" + key }

func (c *fakeCache) Lookup(_ context.Context, u domain.UserID, key string) ([]domain.WagerID, bool, error) {
	c.lookups++
	if c.lookupErr != nil {
		return nil, false, c.lookupErr
	}
	ids, ok := c.entries[cacheKey(u, key)]
	return ids, ok, nil
}

func (c *fakeCache) Record(_ context.Context, u domain.UserID, key string, ids []domain.WagerID, _ time.Duration) error {
	c.records++
	if c.recordErr != nil {
		return c.recordErr
	}
	c.entries[cacheKey(u, key)] = ids
	return nil
}

type fakeWagers struct {
	wagers map[domain.WagerID]domain.Wager
	err    error
}

func (f *fakeWagers) WagerByID(_ context.Context, id domain.WagerID) (domain.Wager, error) {
	if f.err != nil {
		return domain.Wager{}, f.err
	}
	w, ok := f.wagers[id]
	if !ok {
		return domain.Wager{}, fmt.Errorf("wager %s: %w", id, ErrWagerNotFound)
	}
	return w, nil
}

type fakeFairPrices struct {
	prices map[domain.SelectionID]FairPrice
	err    error
}

func (f *fakeFairPrices) FairPricesFor(_ context.Context, sels []domain.SelectionID) ([]FairPrice, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]FairPrice, 0, len(sels))
	for _, s := range sels {
		if p, ok := f.prices[s]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// fixedTeaserPricer stands in for a book that has a posted teaser ladder, so
// the teaser path can be tested end to end without this package inventing one.
// See [ErrTeaserUnsupported] and doc.go for why the shipped pricer refuses.
type fixedTeaserPricer struct {
	fallback IndependentPricer
	decimal  float64
}

func (p fixedTeaserPricer) TicketDecimal(ctx context.Context, t Ticket) (float64, error) {
	if t.Kind == domain.WagerKindTeaser {
		return p.decimal, nil
	}
	return p.fallback.TicketDecimal(ctx, t)
}

// -----------------------------------------------------------------------------
// Service construction
// -----------------------------------------------------------------------------

// newTestService wires a service over the given transaction state, with the
// cache disabled by default — a test that wants the fast path opts in.
func newTestService(t *testing.T, tx *fakeTx, mutate ...func(*Options)) (*Service, *fakeStore) {
	t.Helper()
	store := &fakeStore{tx: tx}
	opts := Options{Logger: discardLogger()}
	for _, m := range mutate {
		m(&opts)
	}
	svc, err := NewService(store, IndependentPricer{}, testClock, opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store
}

// straightSlip is the canonical single-leg slip against [moneylineQuote].
func straightSlip(q Quote, stake domain.Money) Slip {
	return Slip{
		Kind: domain.WagerKindStraight,
		Legs: []SlipLeg{{
			SelectionID: q.Price.SelectionID(),
			BookID:      q.Price.BookID(),
			SeenDecimal: q.Price.Decimal(),
			SeenLine:    q.Price.Line(),
		}},
		Stake:    stake,
		Rounding: domain.RoundHalfAwayFromZero,
	}
}
