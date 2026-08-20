package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// Store is internal/betting's Postgres adapter.
//
// It holds the pool — for [Store.InTx], which is the only way anything in this
// package reaches a statement — and a *gen.Queries bound to that pool, for the
// one read that is deliberately outside a transaction ([Store.WagerByID], the
// cash-out quote).
//
// There is no other state, and in particular no cached anything: a balance is a
// fold over ledger_entries and a price is whatever the last poll wrote, so a
// value held here would be a value that can be stale in the one subsystem where
// stale means overdrawn.
type Store struct {
	db *postgres.DB
	q  *gen.Queries
}

// New builds the adapter. This is what the composition root wires.
func New(db *postgres.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: pgstore needs a database", betting.ErrInvalidOptions)
	}
	return &Store{db: db, q: gen.New(db.Pool())}, nil
}

// Compile-time proof that this adapter satisfies every port it claims. If a port
// grows a method, this file fails to build rather than failing at wire-up.
var (
	_ betting.Store  = (*Store)(nil)
	_ betting.Wagers = (*Store)(nil)
	_ betting.Tx     = (*txStore)(nil)
)

// InTx implements betting.Store.
//
// It is a DELEGATION TO postgres.DB.InTx AND NOTHING ELSE, and doc.go carries
// the full argument for why that is not negotiable. The short version:
// migrations/00006 installs the double-entry assertion as DEFERRABLE INITIALLY
// DEFERRED, so an unbalanced ledger write returns SUCCESS from every INSERT and
// fails at COMMIT. postgres.InTx is the only helper in this tree that propagates
// a failed commit as an error, rolls back on a context detached from the
// caller's, and closes the transaction before re-raising a panic. A hand-rolled
// Begin/Commit here would report a rejected money movement as written, and
// because balances are derived there would be no stored value anywhere that
// disagreed.
//
// The callback receives a [betting.Tx] bound to the transaction's own pgx.Tx via
// gen.Queries.WithTx, which is what makes ports.go's rule — "EVERY METHOD ON Tx
// RUNS INSIDE THE SAME DATABASE TRANSACTION" — structural rather than a
// convention somebody has to honour.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx betting.Tx) error) error {
	if fn == nil {
		return fmt.Errorf("%w: nil transaction function", betting.ErrInvalidOptions)
	}
	return s.db.InTx(ctx, func(ctx context.Context, pgtx pgx.Tx) error {
		return fn(ctx, &txStore{tx: pgtx, q: s.q.WithTx(pgtx)})
	})
}

// WagerByID implements betting.Wagers: the read-only lookup the cash-out QUOTE
// uses.
//
// Deliberately not in a transaction, and deliberately not locked. Quoting a
// cash-out writes nothing, so wrapping it in one would hold a connection — and,
// if it locked, a row that settlement wants — across a pricing read for no
// invariant. The transaction that ACCEPTS a cash-out is a different call and
// takes its own read.
func (s *Store) WagerByID(ctx context.Context, id domain.WagerID) (domain.Wager, error) {
	return wagerByID(ctx, s.q, id)
}

// txStore is [betting.Tx]: every statement a placement runs, bound to one
// transaction.
//
// It has no Commit or Rollback, and never will: the transaction's lifetime
// belongs to postgres.InTx, and a method here that ended it would make the
// outcome ambiguous — postgres/tx.go says so explicitly and logs loudly if a
// callback does it anyway.
//
// It DOES hold the pgx.Tx, for exactly one purpose: [txStore.insertOnce] opens a
// SAVEPOINT on it. That is not a licence to end the outer transaction, and the
// field is unexported and used by that one helper. The reason it is needed at
// all is the subject of insertOnce's own comment, and it is the difference
// between the idempotency guarantee working and not working.
type txStore struct {
	tx pgx.Tx
	q  *gen.Queries
}

// insertOnce runs a single INSERT inside a SAVEPOINT, so that a duplicate key
// leaves the surrounding transaction USABLE instead of aborted.
//
// # Why this exists
//
// PostgreSQL aborts the ENTIRE transaction on any statement error. After a
// unique violation, every subsequent command in that transaction fails with
// SQLSTATE 25P02 (in_failed_sql_transaction) until it is rolled back — the
// server does not care that the caller was expecting that particular error and
// has a plan for it.
//
// This package's idempotency design (doc.go, and decision 1 of the phase-8
// brief) is built on precisely such a plan: a replayed submit derives the same
// primary key, the INSERT collides, and the SERVICE ANSWERS THE COLLISION BY
// READING THE EXISTING TICKET BACK — in the same transaction. Without a
// savepoint that read-back is the first statement after the error, so it fails
// with 25P02, and the replay that was supposed to return "already placed, here
// is your bet" returns an internal database error instead. The guarantee reads
// correct in every doc comment and does not hold at runtime.
//
// A SAVEPOINT is the only mechanism PostgreSQL offers for this. ROLLBACK TO
// SAVEPOINT discards the failed statement and returns the transaction to a
// usable state, leaving everything written before the savepoint intact — which
// matters, because a round robin's parent row is written before the per-ticket
// inserts that may each collide independently.
//
// # Why it wraps ONE statement and not the method
//
// Only the statements whose duplicate the caller CONTINUES PAST need this: the
// wagers row and the round_robins parent. Wrapping a whole method would put the
// legs — whose duplicate is a malformed slip and a hard failure — inside a
// savepoint that nothing ever rolls back to, which buys nothing and hides the
// narrowness of the mechanism. The savepoint is a recovery point, so it belongs
// exactly where a recovery happens.
//
// The savepoint costs one extra round trip per insert. That is paid on the
// placement path only, it is not paid per leg, and the alternative is an
// idempotency guarantee that does not work.
func (t *txStore) insertOnce(ctx context.Context, exec func(context.Context, *gen.Queries) error) error {
	sp, err := t.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: open savepoint: %w", err)
	}

	if err := exec(ctx, t.q.WithTx(sp)); err != nil {
		// ROLLBACK TO SAVEPOINT is what makes the returned error RECOVERABLE.
		// If it fails the transaction is still aborted, so the caller must not
		// be told only about the original error and left to run a read-back
		// that cannot work — both errors are returned, never one instead of the
		// other, matching postgres.InTx's rule for the same situation.
		if rbErr := sp.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("pgstore: roll back savepoint: %w", rbErr))
		}
		return err
	}

	// RELEASE SAVEPOINT. Nothing is undone; this only discards the recovery
	// point, which stops a long round robin from accumulating one open
	// subtransaction per ticket.
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("pgstore: release savepoint: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// The wagering gate
// -----------------------------------------------------------------------------

// UserStatus implements betting.Tx.
//
// The row is LOCKED FOR UPDATE by the query itself (queries/betting.sql's
// GetUserStatus), which ports.go declares as part of this method's contract
// rather than as an implementation detail an adapter may drop. The lock is not
// for the self-exclusion race — that one is defensible in either order — it is
// what serialises one customer's concurrent placements so that the affordability
// fold and the limit sums, neither of which has a constraint behind it, cannot
// both pass on a balance that neither has spent yet.
//
// The RAW STRING is returned, not an auth.UserStatus. Parsing belongs in the
// service, where an unrecognised value fails closed with ErrAccountNotWagerable;
// a store that parsed would have to decide what to do with a status its build
// predates, and that decision is not a store's to make.
func (t *txStore) UserStatus(ctx context.Context, u domain.UserID) (string, error) {
	row, err := t.q.GetUserStatus(ctx, u)
	if err != nil {
		return "", fmt.Errorf("pgstore: lock and read user status: %w", err)
	}
	return row.Status, nil
}

// -----------------------------------------------------------------------------
// Money reads
// -----------------------------------------------------------------------------

// Balance implements betting.Tx: the derived balance of one account.
//
// NOT FOUND MEANS ZERO, and mapping it is this method's whole job beyond running
// the query. account_balances reports one row per account that has ever been
// touched, so a brand-new customer with no grant yet produces pgx.ErrNoRows —
// and migration 00006 keeps that distinction on purpose, because "touched and
// nets to nothing" is a different fact from "never touched". Surfacing the
// no-rows would make a first-ever placement fail with a database error.
//
// A SYSTEM ACCOUNT IS REFUSED rather than answered. The house and issuance
// singletons carry a NULL owner, and `account_user_id = NULL` is NULL and never
// true, so the query would return no row and this method would report their
// balance as ZERO — which is not merely wrong but wrong in the direction that
// looks plausible, since a fresh house account really is zero. Migration 00006
// notes that an operator report on those needs `account_user_id IS NULL` and is
// a separate query; until one exists, asking here is a programming error and is
// reported as one.
func (t *txStore) Balance(ctx context.Context, a domain.Account) (domain.Money, error) {
	owner, ok := a.Owner()
	if !ok {
		return 0, fmt.Errorf("%w: %s is a system account; account_balances is keyed by owner "+
			"and cannot answer for the house or issuance singleton", betting.ErrInvalidOptions, a)
	}

	row, err := t.q.GetAccountBalance(ctx, gen.GetAccountBalanceParams{
		AccountKind:   a.Kind().String(),
		AccountUserID: &owner,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("pgstore: fold balance for %s: %w", a, err)
	}
	return row.BalanceMinor, nil
}

// GrantCredit implements betting.Tx: what a grant credited to a user's cash.
//
// Every discriminating predicate — the transaction being a GRANT, the entry
// being the user_cash side, the owner being this user — is in the STATEMENT
// rather than here, so "no such grant" arrives as pgx.ErrNoRows and there is no
// branch in Go that could report the issuance half or somebody else's movement
// by getting a comparison the wrong way round. queries/betting.sql argues each
// predicate individually.
func (t *txStore) GrantCredit(
	ctx context.Context,
	id domain.TransactionID,
	u domain.UserID,
) (domain.Money, time.Time, error) {
	owner := u
	row, err := t.q.GetGrantCreditForUser(ctx, gen.GetGrantCreditForUserParams{
		TransactionID: id,
		Owner:         &owner,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, time.Time{}, fmt.Errorf("%w: transaction %s for user %s",
			betting.ErrGrantNotFound, id, u)
	case err != nil:
		return 0, time.Time{}, fmt.Errorf("pgstore: read grant credit for %s: %w", id, err)
	}
	return row.AmountMinor, row.OccurredAt, nil
}

// SumEntries implements betting.Tx: the SIGNED sum of ledger_entries against one
// account over a set of entry kinds, since an instant.
//
// The kinds are passed through as their String() spellings, which are the same
// strings ledger_entries.kind stores — migration 00005 chose the user_limits.kind
// spellings to match domain.EntryKind character for character precisely so no
// mapping table sits between a limit and the entries it bounds.
//
// THE SUM IS CONVERTED THROUGH domain.FromMinorUnits RATHER THAN CAST. sqlc
// returns total_minor as a bare int64 and sqlc.yaml records why that is right:
// every money OVERRIDE maps a stored column already bounded by a CHECK to
// domain.MaxSafeMoney, and a SUM has no such bound. A hundred thousand in-bounds
// entries can total something no Money may hold, and a cast would produce a
// silently invalid value in the one place that decides whether a customer has
// hit their own loss limit. FromMinorUnits is that bound check.
//
// An empty or nil kinds slice is refused rather than sent. `kind = ANY('{}')` is
// false for every row, so the query would return a perfectly well-formed zero —
// and a zero sum means "this customer has staked nothing", which is the answer
// that lets any stake through. A programming error that reads as permission is
// exactly the one to fail loudly on.
func (t *txStore) SumEntries(ctx context.Context, a domain.Account, kinds []domain.EntryKind, since time.Time) (domain.Money, error) {
	if len(kinds) == 0 {
		return 0, fmt.Errorf("%w: SumEntries needs at least one entry kind; an empty set sums "+
			"to zero, which reads as 'nothing staked' and permits everything", betting.ErrInvalidOptions)
	}
	owner, ok := a.Owner()
	if !ok {
		return 0, fmt.Errorf("%w: %s is a system account; ledger_entries is filtered by owner "+
			"here and cannot answer for the house or issuance singleton", betting.ErrInvalidOptions, a)
	}

	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if !k.Valid() {
			return 0, fmt.Errorf("%w: %w", betting.ErrInvalidOptions, domain.ErrUnknownEntryKind)
		}
		names = append(names, k.String())
	}

	row, err := t.q.SumLedgerEntriesSince(ctx, gen.SumLedgerEntriesSinceParams{
		AccountKind:   a.Kind().String(),
		AccountUserID: &owner,
		Kinds:         names,
		Since:         since.UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("pgstore: sum %v entries for %s: %w", names, a, err)
	}

	total, err := domain.FromMinorUnits(row.TotalMinor)
	if err != nil {
		return 0, fmt.Errorf("pgstore: %d entries for %s total %d minor units: %w",
			row.EntryCount, a, row.TotalMinor, err)
	}
	return total, nil
}

// LimitsInForce implements betting.Tx.
//
// The predicate — [effective_from, superseded_at) containing `at` — lives in the
// query, and queries/betting.sql's GetCurrentLimits carries the argument for why
// it is not `superseded_at IS NULL`: during a cooling-off period those are
// different rows, and the current one is the LOOSENING that has not yet bound.
//
// SESSION LIMITS ARE RETURNED, not filtered here. ports.go says Amount "is
// meaningful only when Kind.IsMoney()", which means the evaluator expects to see
// the non-money kinds and skip them; a store that dropped them would make that
// guard dead code and would hide a session limit from anything that later wanted
// to report the full set. The query projects the amount as a value plus a
// presence flag for exactly this row, because user_limits.amount_minor is NULL
// on it and a bare scan would fail at runtime on the session row specifically —
// the one a money-limit test never reaches.
//
// Both enum columns go through auth's own ParseX, so a value this build does not
// recognise is a wrapped error at the read rather than a zero LimitKind that
// silently matches nothing.
func (t *txStore) LimitsInForce(ctx context.Context, u domain.UserID, at time.Time) ([]betting.Limit, error) {
	rows, err := t.q.GetCurrentLimits(ctx, gen.GetCurrentLimitsParams{UserID: u, At: at.UTC()})
	if err != nil {
		return nil, fmt.Errorf("pgstore: read limits in force for %s: %w", u, err)
	}

	limits := make([]betting.Limit, 0, len(rows))
	for _, row := range rows {
		kind, err := auth.ParseLimitKind(row.Kind)
		if err != nil {
			return nil, fmt.Errorf("pgstore: stored limit %s: %w", row.ID, err)
		}
		period, err := auth.ParseLimitPeriod(row.Period)
		if err != nil {
			return nil, fmt.Errorf("pgstore: stored limit %s: %w", row.ID, err)
		}

		// Zero is a safe sentinel for "no amount" only because it is
		// unstorable: user_limits_amount_range requires amount_minor > 0, so a
		// zero here can only have come from the coalesce over a NULL.
		var amount domain.Money
		if row.HasAmount {
			amount, err = domain.FromMinorUnits(row.AmountMinor)
			if err != nil {
				return nil, fmt.Errorf("pgstore: stored limit %s amount: %w", row.ID, err)
			}
		}

		limits = append(limits, betting.Limit{
			Kind:          kind,
			Period:        period,
			Amount:        amount,
			EffectiveFrom: row.EffectiveFrom,
		})
	}
	return limits, nil
}

// -----------------------------------------------------------------------------
// Catalogue reads
// -----------------------------------------------------------------------------

// QuoteFor implements betting.Tx: the current price for one selection at one
// book, plus the catalogue context a leg needs.
//
// THE PRICE IS BUILT THROUGH domain.NewPrice, not assembled as a struct literal.
// That constructor is what refuses a NaN decimal, a price outside
// (MinDecimalOdds, MaxDecimalOdds], and a zero observation instant — every one
// of which the columns' CHECK constraints also refuse, which is exactly why
// running the constructor here costs nothing and catches a schema/domain
// divergence at the read.
//
// NO STALENESS TEST. ports.go is explicit that this method "does NOT apply a
// staleness filter — the service compares the returned Price.ObservedAt against
// its own configured MaxQuoteAge and raises ErrStaleQuote". How old is too old
// is a reviewable option, not a constant buried in an adapter.
//
// A book that has never quoted the selection produces no row, which becomes
// betting.ErrQuoteUnavailable. That is an ordinary outcome on a market one book
// does not price, not a fault.
func (t *txStore) QuoteFor(ctx context.Context, sel domain.SelectionID, book domain.BookID) (betting.Quote, error) {
	row, err := t.q.GetCurrentQuoteForSelection(ctx, gen.GetCurrentQuoteForSelectionParams{
		SelectionID: sel,
		BookID:      book,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return betting.Quote{}, fmt.Errorf("%w: book %s has no quote on selection %s",
			betting.ErrQuoteUnavailable, book, sel)
	case err != nil:
		return betting.Quote{}, fmt.Errorf("pgstore: read quote for selection %s at book %s: %w",
			sel, book, err)
	}

	line, err := lineFrom(row.Line)
	if err != nil {
		return betting.Quote{}, fmt.Errorf("pgstore: quote for selection %s: %w", sel, err)
	}
	price, err := domain.NewPrice(domain.PriceParams{
		SelectionID: row.SelectionID,
		BookID:      row.BookID,
		Decimal:     float64(row.DecimalOdds),
		Line:        line,
		ObservedAt:  row.ObservedAt,
	})
	if err != nil {
		return betting.Quote{}, fmt.Errorf("pgstore: stored quote for selection %s: %w", sel, err)
	}

	marketType, err := domain.ParseMarketType(row.MarketType)
	if err != nil {
		return betting.Quote{}, fmt.Errorf("pgstore: market %s: %w", row.MarketID, err)
	}
	role, err := domain.ParseSelectionRole(row.Role)
	if err != nil {
		return betting.Quote{}, fmt.Errorf("pgstore: selection %s: %w", sel, err)
	}

	return betting.Quote{
		Price:      price,
		EventID:    row.EventID,
		MarketID:   row.MarketID,
		MarketType: marketType,
		Role:       role,
	}, nil
}

// MarketState implements betting.Tx.
//
// A market that does not exist becomes betting.ErrMarketNotOpen rather than a
// not-found of its own, which is ports.go's instruction and the right customer
// story: from the slip's point of view a market that has vanished and one that
// is closed are the same refusal, and two sentences for one situation buy
// nothing.
func (t *txStore) MarketState(ctx context.Context, m domain.MarketID) (betting.MarketState, error) {
	row, err := t.q.GetMarketState(ctx, m)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return betting.MarketState{}, fmt.Errorf("%w: no market %s", betting.ErrMarketNotOpen, m)
	case err != nil:
		return betting.MarketState{}, fmt.Errorf("pgstore: read market state %s: %w", m, err)
	}

	status, err := domain.ParseMarketStatus(row.MarketStatus)
	if err != nil {
		return betting.MarketState{}, fmt.Errorf("pgstore: market %s: %w", m, err)
	}
	eventStatus, err := domain.ParseEventStatus(row.EventStatus)
	if err != nil {
		return betting.MarketState{}, fmt.Errorf("pgstore: event %s: %w", row.EventID, err)
	}

	return betting.MarketState{
		MarketID:       row.ID,
		EventID:        row.EventID,
		Status:         status,
		EventStatus:    eventStatus,
		ScheduledStart: row.ScheduledStart,
	}, nil
}

// -----------------------------------------------------------------------------
// Writes
// -----------------------------------------------------------------------------

// InsertRoundRobin implements betting.Tx: the round_robins parent row AND its
// round_robin_sizes children, in that order.
//
// The order is fixed by round_robin_sizes_parent_fk, a COMPOSITE foreign key on
// (round_robin_id, selection_count), and the denormalised selection_count on
// each size row is not a value this adapter chooses — it is whatever the parent
// says, and the database refuses any other. Passing len(rr.Legs()) to both is
// what makes "size <= len(legs)" a CHECK rather than a trigger.
//
// The sizes arrive already sorted and de-duplicated: domain.NewRoundRobin does
// that, which is what makes the composite primary key (round_robin_id, size) a
// statement about the domain rather than a coincidence.
//
// # The replay contract, which the service depends on
//
// The parent id is derived from the idempotency key like every other id here, so
// a replayed round robin collides on round_robins_pkey. That is reported as
// betting.ErrAlreadyPlaced, and internal/betting/placement.go relies on it: it
// logs the replay and FALLS THROUGH to the per-ticket inserts, each of which
// reports its own duplicate and reads back. The parent INSERT therefore goes
// through [txStore.insertOnce] — a duplicate here is a state the caller
// continues past, so the transaction has to survive it.
func (t *txStore) InsertRoundRobin(ctx context.Context, rr domain.RoundRobin) error {
	legs := rr.Legs()
	if len(legs) > maxInt32 {
		return fmt.Errorf("%w: round robin %s has %d selections", betting.ErrInvalidOptions, rr.ID(), len(legs))
	}
	count := int32(len(legs))

	err := t.insertOnce(ctx, func(ctx context.Context, q *gen.Queries) error {
		return q.InsertRoundRobin(ctx, gen.InsertRoundRobinParams{
			ID:                       rr.ID(),
			UserID:                   rr.UserID(),
			SelectionCount:           count,
			StakePerCombinationMinor: rr.StakePerCombination(),
			PlacedAt:                 rr.PlacedAt(),
		})
	})
	if err != nil {
		return classifyRoundRobinInsert(rr.ID(), err)
	}

	for _, size := range rr.Sizes() {
		if err := t.q.InsertRoundRobinSize(ctx, gen.InsertRoundRobinSizeParams{
			RoundRobinID:   rr.ID(),
			SelectionCount: count,
			Size:           int32(size),
		}); err != nil {
			return fmt.Errorf("pgstore: insert round robin %s size %d: %w", rr.ID(), size, err)
		}
	}
	return nil
}

// InsertWager implements betting.Tx: the wagers row AND every one of its legs.
//
// # The idempotency guard
//
// A replayed submit derives the SAME wager id — internal/betting computes it
// from (userID, idempotencyKey, combination index) — so the second INSERT hits
// wagers_pkey and PostgreSQL raises SQLSTATE 23505. That is mapped to
// betting.ErrAlreadyPlaced, and the service answers it by reading the existing
// ticket back. doc.go carries the full argument, including why this is what lets
// Redis be a pure fast path.
//
// THE CONSTRAINT NAME IS INSPECTED, NEVER ASSUMED. Three unique constraints are
// reachable from this method, and only one of them means "replay":
//
//	wagers_pkey                 the same placement, again        -> ErrAlreadyPlaced
//	legs_wager_selection_key    two legs on one selection        -> malformed slip
//	legs_wager_market_key       two legs on one market           -> malformed slip
//
// Reporting either of the last two as ErrAlreadyPlaced would hand the customer
// somebody else's answer: the service would go and read a wager that does not
// exist, and the failure would surface as an incoherent 500 far from its cause.
// internal/auth/pgstore separates a taken email from an id collision the same
// way and for the same reason.
//
// # Why the legs are inserted one at a time, after the wager
//
// Fixed by the schema, not chosen. legs.wager_id is a foreign key, so the wager
// row must exist first; and 00006 checks the ticket's SHAPE — leg-count arity,
// teaser correspondence and direction, a straight's price against its leg — in a
// DEFERRED constraint trigger at COMMIT, precisely so the wager may be written
// before the legs that would otherwise contradict it. A ticket with no legs is
// unstorable, but only at COMMIT, which is why this method and InsertWager are
// one method on the port rather than two.
func (t *txStore) InsertWager(ctx context.Context, w domain.Wager) error {
	returned, netReturn := returnedPair(w)
	roundRobin, _ := w.RoundRobinID()

	err := t.insertOnce(ctx, func(ctx context.Context, q *gen.Queries) error {
		return q.InsertWager(ctx, gen.InsertWagerParams{
			ID:                   w.ID(),
			UserID:               w.UserID(),
			Kind:                 w.Kind().String(),
			Status:               w.Status().String(),
			StakeMinor:           w.Stake(),
			AcceptedDecimal:      oddsDecimal(w.AcceptedDecimal()),
			Rounding:             w.Rounding().String(),
			PotentialPayoutMinor: w.PotentialPayout(),
			PotentialProfitMinor: w.PotentialProfit(),
			TeaserPoints:         float8From(w.TeaserPoints()),
			RoundRobinID:         optional(roundRobin, !roundRobin.IsZero()),
			ReturnedMinor:        returned,
			NetReturnMinor:       netReturn,
			PlacedAt:             w.PlacedAt(),
			TransitionedAt:       w.UpdatedAt(),
		})
	})
	if err != nil {
		return classifyWagerInsert(w.ID(), err)
	}

	for _, leg := range w.Legs() {
		gradedAt, hasGradedAt := leg.GradedAt()
		lineValue, hasLine := leg.Price().Line().Value()
		teasedValue, hasTeased := leg.TeasedLine().Value()

		if err := t.q.InsertWagerLeg(ctx, gen.InsertWagerLegParams{
			ID:              leg.ID(),
			WagerID:         w.ID(),
			EventID:         leg.EventID(),
			MarketID:        leg.MarketID(),
			MarketType:      leg.MarketType().String(),
			SelectionID:     leg.SelectionID(),
			Role:            leg.Role().String(),
			PriceBookID:     leg.Price().BookID(),
			PriceDecimal:    oddsDecimal(leg.QuotedDecimal()),
			PriceLine:       pgtype.Float8{Float64: lineValue, Valid: hasLine},
			PriceObservedAt: leg.Price().ObservedAt(),
			TeasedLine:      pgtype.Float8{Float64: teasedValue, Valid: hasTeased},
			Status:          leg.Status().String(),
			GradedAt:        pgtype.Timestamptz{Time: gradedAt, Valid: hasGradedAt},
		}); err != nil {
			return classifyWagerInsert(w.ID(), fmt.Errorf("leg %s: %w", leg.ID(), err))
		}
	}
	return nil
}

// InsertTransaction implements betting.Tx: one balanced money movement, header
// then entries.
//
// THE ENTRIES ARE NOT CHECKED FOR BALANCE HERE, AND MUST NOT BE. They are
// balanced by construction — domain.NewTransaction refuses at least two entries
// that do not sum to exactly zero — and they are verified again by
// ledger_entries_balanced_at_commit at COMMIT. A third check in this adapter
// would be a second implementation of the one invariant in this system that must
// have exactly one, and the version that drifts is always the one nobody is
// reading.
//
// entry_index is the LOOP INDEX and not a counter that restarts, because
// migration 00006 defines it as the ordinal within Transaction.entries so that
// ORDER BY entry_index rehydrates Transaction.Entries() in the order the domain
// built them.
//
// kind and occurred_at are repeated on every entry and pinned to the header by a
// composite foreign key, so they are taken from the transaction rather than from
// the entry — an entry's own Kind() is guaranteed equal by NewTransaction, and
// passing the header's value is what makes that guarantee visible here.
func (t *txStore) InsertTransaction(ctx context.Context, tr domain.Transaction) error {
	wagerID, hasWager := tr.WagerID()

	if err := t.q.InsertLedgerTransaction(ctx, gen.InsertLedgerTransactionParams{
		ID:         tr.ID(),
		Kind:       tr.Kind().String(),
		WagerID:    optional(wagerID, hasWager),
		OccurredAt: tr.OccurredAt(),
	}); err != nil {
		return classifyLedgerWrite(tr.ID(), err)
	}

	for i, entry := range tr.Entries() {
		account := entry.Account()
		owner, _ := account.Owner()

		if err := t.q.InsertLedgerEntry(ctx, gen.InsertLedgerEntryParams{
			TransactionID: tr.ID(),
			EntryIndex:    int32(i),
			AccountKind:   account.Kind().String(),
			AccountUserID: optional(owner, account.Kind().IsUserOwned()),
			AmountMinor:   entry.Amount(),
			Kind:          tr.Kind().String(),
			OccurredAt:    tr.OccurredAt(),
		}); err != nil {
			return classifyLedgerWrite(tr.ID(), fmt.Errorf("entry %d: %w", i, err))
		}
	}
	return nil
}

// WagerByID implements betting.Tx: the replay path's read-back.
//
// Reached only after InsertWager reported ErrAlreadyPlaced, where the existing
// ticket IS the answer to the request. A missing row on that path means the
// store contradicted itself — it refused an insert for a primary key it does not
// hold — so it is reported as betting.ErrWagerNotFound and not as a
// customer-facing failure.
func (t *txStore) WagerByID(ctx context.Context, id domain.WagerID) (domain.Wager, error) {
	return wagerByID(ctx, t.q, id)
}

// wagerByID is the body of both WagerByID methods. One function because the two
// differ only in which *gen.Queries they hold — pool or transaction — and a
// second copy would be a second place for the hydration call to drift.
func wagerByID(ctx context.Context, q *gen.Queries, id domain.WagerID) (domain.Wager, error) {
	row, err := q.GetWager(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Wager{}, fmt.Errorf("%w: %s", betting.ErrWagerNotFound, id)
	case err != nil:
		return domain.Wager{}, fmt.Errorf("pgstore: read wager %s: %w", id, err)
	}

	legs, err := q.ListWagerLegs(ctx, id)
	if err != nil {
		return domain.Wager{}, fmt.Errorf("pgstore: read legs of wager %s: %w", id, err)
	}

	w, err := HydrateWager(row, legs)
	if err != nil {
		return domain.Wager{}, fmt.Errorf("pgstore: %w", err)
	}
	return w, nil
}

// -----------------------------------------------------------------------------
// Audit
// -----------------------------------------------------------------------------

// The two audit_log columns that are constant for everything internal/betting
// writes, and are therefore supplied here rather than carried on
// betting.AuditEntry.
//
//	actor_kind  Always 'user'. Every action in internal/betting is a customer
//	            acting on their own account: the package has no operator entry
//	            point and no scheduled one, and internal/httpapi mounts no route
//	            that names a user other than the token's own. Migration 00007
//	            also admits 'admin' and 'system', and both would be wrong here —
//	            a customer's bet attributed to 'system' would look ordinary in a
//	            listing and would quietly exclude itself from every "what did
//	            this customer do" query.
//
//	outcome     Always 'success'. betting.Tx.RecordAudit is only ever reached
//	            after the change it records has been written, and any refusal
//	            rolls this transaction back and takes the row with it — so a
//	            'failure' row is not merely unused here, it is unwritable.
//
// Constants rather than parameters because a field would offer the caller a
// choice with exactly one correct answer.
const (
	auditActorKindUser  = "user"
	auditOutcomeSuccess = "success"
)

// RecordAudit implements betting.Tx.
//
// It runs on the transaction's own *gen.Queries, so the row is one more
// statement in the placement or grant transaction and shares its fate — which
// is the whole reason the method is on betting.Tx rather than on a port of its
// own. See that interface for the argument.
//
// # Why the JSONB is encoded here and not by the caller
//
// betting.AuditEntry.After is a map[string]any because that is the shape the
// SERVICE naturally produces; audit_log.state_after is JSONB with a CHECK that
// it is an object. Marshalling at the boundary keeps encoding/json out of
// internal/betting, and keeps the CHECK's requirement — an object, never an
// array and never a bare scalar — enforced by the one type that can only ever
// encode to one.
//
// An empty or nil map becomes SQL NULL rather than `{}`. "Nothing was recorded
// about the new state" and "the new state is the empty object" are different
// claims, and migration 00007 keeps both columns nullable precisely so the
// first is expressible.
func (t *txStore) RecordAudit(ctx context.Context, e betting.AuditEntry) error {
	after, err := marshalAuditState(e.After)
	if err != nil {
		return fmt.Errorf("pgstore: encode audit state for %s: %w", e.Action, err)
	}

	params := gen.InsertAuditEntryParams{
		// UTC explicitly: occurred_at is TIMESTAMPTZ and pgx would convert a
		// non-UTC value correctly anyway, but every instant this schema stores
		// is UTC by convention and normalising at the boundary keeps a stray
		// local-zone value from being the thing a reader has to notice.
		OccurredAt: e.OccurredAt.UTC(),
		ActorKind:  auditActorKindUser,
		ActorID:    e.ActorID.String(),
		Action:     e.Action,
		EntityType: e.EntityType,
		EntityID:   e.EntityID,
		Outcome:    auditOutcomeSuccess,
		StateAfter: after,
		TraceID:    auditText(e.Context.TraceID),
		SpanID:     auditText(e.Context.SpanID),
		RequestID:  auditText(e.Context.RequestID),
	}
	// StateBefore is left nil. Both actions this package audits CREATE the row
	// they describe, so there is no prior state; betting.AuditEntry has no
	// Before field for the same reason.
	if e.Context.ClientIP.IsValid() {
		ip := e.Context.ClientIP
		params.ClientIp = &ip
	}

	if err := t.q.InsertAuditEntry(ctx, params); err != nil {
		return fmt.Errorf("pgstore: insert audit entry %s for %s %s: %w",
			e.Action, e.EntityType, e.EntityID, err)
	}
	return nil
}

// marshalAuditState encodes a diff for state_after, or nil for nothing.
func marshalAuditState(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return json.Marshal(m)
}

// auditText maps an empty string onto SQL NULL.
//
// audit_log's CHECKs refuse a present-but-empty trace id, span id or request
// id — `request_id IS NULL OR request_id <> ”` — so passing "" through as a
// non-null value would fail the INSERT rather than record "there was no request
// id", which is what an empty AuditContext field actually means.
func auditText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// -----------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------

// classifyWagerInsert turns a driver error from the placement INSERTs into one
// of internal/betting's sentinels where there is one, and wraps it otherwise.
//
// Three SQLSTATEs are meaningful on this path and each says something different:
//
//	23505  unique_violation      wagers_pkey -> a replay. Anything else is a
//	                             malformed slip; see InsertWager.
//	23001  restrict_violation    migration 00008's wagers_refuse_excluded_user.
//	                             It is the ONLY 23001 source on an INSERT into
//	                             wagers -- wagers_assert_transition fires on
//	                             UPDATE and DELETE only -- so the code alone is a
//	                             sound discriminator at this call site.
//	23514  check_violation       a CHECK on the row: a payout below the stake, a
//	                             teaser without points, a return that does not
//	                             match the outcome. These are arithmetic or
//	                             plumbing faults and are wrapped, not sentinelled,
//	                             because there is nothing the customer can do
//	                             about one and nothing the service should retry.
//
// Everything else — a connection failure above all — passes through wrapped, so
// that a database outage stays a database outage. Collapsing it into
// ErrAlreadyPlaced would make the service go looking for a wager that was never
// written, which is how an outage gets reported as a successful placement.
func classifyWagerInsert(id domain.WagerID, err error) error {
	if constraintName(err) == "wagers_pkey" {
		return fmt.Errorf("%w: wager %s", betting.ErrAlreadyPlaced, id)
	}
	if postgres.SQLState(err) == sqlStateRestrictViolation {
		// BOTH the sentinel and the driver error are wrapped, matching
		// classifyLedgerWrite below. Wrapping only the sentinel — which this
		// branch used to do — makes errors.Is(err, ErrAccountNotWagerable) true
		// and postgres.SQLState(err) EMPTY, so the one fact that distinguishes
		// "migration 00008's trigger refused this" from "the service refused
		// this" is destroyed at the moment it is produced. The two refusals are
		// deliberately different layers (decision 2 of the phase-8 brief) and a
		// caller that cannot tell them apart cannot prove the backstop fired.
		return fmt.Errorf("%w: the database refused wager %s: %s: %w",
			betting.ErrAccountNotWagerable, id, serverMessage(err), err)
	}
	return fmt.Errorf("pgstore: insert wager %s: %w", id, err)
}

// classifyRoundRobinInsert is classifyWagerInsert for the round_robins parent.
//
// Only one unique constraint is reachable from that INSERT — round_robins_pkey —
// so unlike the wagers path there is no sibling constraint to be confused with,
// and no leg-collision hazard. The constraint NAME is still what is matched
// rather than the bare SQLSTATE, for the same reason it is on the wagers path:
// a future unique index on this table would otherwise start silently reporting
// itself as a replay, and a replay is a code path that skips writing money.
//
// Nothing here is self-exclusion: migration 00008's trigger is installed on
// wagers, not on round_robins, so a 23001 cannot arrive at this call site. The
// excluded customer is refused when the first TICKET is inserted, which is the
// row that represents a bet.
func classifyRoundRobinInsert(id domain.RoundRobinID, err error) error {
	if constraintName(err) == "round_robins_pkey" {
		return fmt.Errorf("%w: round robin %s", betting.ErrAlreadyPlaced, id)
	}
	return fmt.Errorf("pgstore: insert round robin %s: %w", id, err)
}

// classifyLedgerWrite names the ledger when the ledger is what refused.
//
// The deferred balance assertion raises 23514 AT COMMIT, not here, so the error
// this function sees on a 23514 is a per-row CHECK — a zero amount, an owner on
// a system account, an amount outside domain.MaxSafeMoney. The commit-time one
// surfaces from postgres.InTx instead, wrapped as "postgres: commit", and this
// message is what lets a reader tell the two apart when they land in the same
// log line.
//
// The distinction that matters to a caller is "the ledger rejected this" versus
// "the connection died", because the first is final and the second is not — and
// CLAUDE.md's rule for phase 8 is that a failed transaction is NEVER retried
// unless postgres.IsTransientConnectError says it was a connection failure, since
// a retried ledger write double-applies.
func classifyLedgerWrite(id domain.TransactionID, err error) error {
	switch postgres.SQLState(err) {
	case sqlStateUniqueViolation:
		return fmt.Errorf("%w: ledger transaction %s", betting.ErrAlreadyPlaced, id)
	case sqlStateCheckViolation:
		return fmt.Errorf("pgstore: the ledger rejected transaction %s: %s: %w",
			id, serverMessage(err), err)
	default:
		return fmt.Errorf("pgstore: write ledger transaction %s: %w", id, err)
	}
}

// The three SQLSTATEs this package reasons about, named rather than repeated as
// literals. postgres.IsUniqueViolation and friends exist for the two that have
// helpers; 23001 does not, because it is raised by this schema's own triggers
// rather than by a constraint the driver knows about.
const (
	sqlStateUniqueViolation   = "23505"
	sqlStateRestrictViolation = "23001"
	sqlStateCheckViolation    = "23514"
)

// maxInt32 bounds the two lengths this package narrows to int32 for a column
// that is INTEGER. Both are already far smaller — domain.MaxRoundRobinLegs is 10
// and domain.MaxWagerLegs is 25 — so the check can never fire; it is here so
// that a future widening of either constant is a compile-time-visible error
// rather than a silent truncation.
const maxInt32 = 1<<31 - 1

// constraintName returns the constraint a PostgreSQL error names, or "".
//
// Empty for anything that is not a *pgconn.PgError, and empty for a plpgsql
// RAISE — which is why the trigger-raised 23001 above is matched on its SQLSTATE
// instead.
func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// serverMessage returns the server's own sentence for an error, trimmed of the
// newlines a multi-line RAISE produces so it survives a single log line.
//
// It is included in the wrapped error on the two paths where the schema's
// message is the most informative thing available: migration 00006 and 00008
// write those messages deliberately, with HINTs, and discarding them in favour
// of a generic sentence would throw away the best diagnostic in the system.
func serverMessage(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err.Error()
	}
	msg := pgErr.Message
	if pgErr.Hint != "" {
		msg += " (" + pgErr.Hint + ")"
	}
	return strings.Join(strings.Fields(msg), " ")
}
