package pgstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	bettingpg "github.com/anpl1623/sharpline/internal/betting/pgstore"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
	"github.com/anpl1623/sharpline/internal/settlement"
)

// Store is internal/settlement's Postgres adapter: settlement.Store, and through
// [Store.InTx], settlement.Tx.
type Store struct {
	db *postgres.DB
	q  *gen.Queries
}

// NewStore builds the adapter. This is what cmd/settle wires.
func NewStore(db *postgres.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: pgstore needs a database", settlement.ErrInvalidOptions)
	}
	return &Store{db: db, q: gen.New(db.Pool())}, nil
}

// Compile-time proof that this package satisfies the interfaces
// internal/settlement declares. They are asserted HERE and not there, because
// this package imports that one and the reverse assertion would be an import
// cycle — ports.go says so where it declines to write them.
var (
	_ settlement.Store         = (*Store)(nil)
	_ settlement.Tx            = (*txStore)(nil)
	_ settlement.ResultsSource = (*Results)(nil)
)

// OldestUnsettledAt implements settlement.Store: the placement instant of the
// earliest ticket that still holds escrow, and whether there is one.
//
// It seeds the results cursor at startup and is why settlement needs no cursor
// table: a ticket cannot be waiting on a result that was already final when the
// ticket was written, so the earliest open placement bounds "the oldest result
// settlement could still care about" from below.
//
// NO ROWS MEANS "nothing is open", not an error. That is the state of every
// fresh database and of any deployment that has settled everything, and the
// query is written as ORDER BY ... LIMIT 1 rather than min(placed_at) precisely
// so it can say so — an aggregate over an empty set returns one row containing
// NULL, which would scan into a non-pointer time.Time and fail.
//
// Deliberately not in a transaction: it runs once, at startup, before the
// service has anything to be consistent with.
func (s *Store) OldestUnsettledAt(ctx context.Context) (time.Time, bool, error) {
	row, err := s.q.GetOldestUnsettledPlacedAt(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("pgstore: read oldest unsettled placement: %w", err)
	}
	return row.PlacedAt, true, nil
}

// InTx implements settlement.Store.
//
// A DELEGATION TO postgres.DB.InTx AND NOTHING ELSE. doc.go carries the
// argument; the short version is that the ledger's zero-sum assertion is
// deferred to COMMIT, so a helper that does not propagate the commit error
// reports a movement the database refused as one that was written — and because
// balances are derived there is no stored value anywhere that would disagree.
//
// The callback receives a [settlement.Tx] bound to the transaction's own pgx.Tx.
// That is the only way this package produces one, which is what makes
// ports.go's claim — "there is no expression in settle.go that can write a
// ledger row outside a transaction" — structural.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx settlement.Tx) error) error {
	if fn == nil {
		return fmt.Errorf("%w: nil transaction function", settlement.ErrInvalidOptions)
	}
	return s.db.InTx(ctx, func(ctx context.Context, pgtx pgx.Tx) error {
		return fn(ctx, &txStore{q: s.q.WithTx(pgtx)})
	})
}

// txStore is settlement.Tx: the five statements one settlement runs, bound to
// one transaction.
//
// It holds no pgx.Tx and has no Commit or Rollback. The transaction's lifetime
// belongs to postgres.InTx, and a method here that ended it would make the
// outcome ambiguous — postgres/tx.go logs loudly if a callback does it anyway.
type txStore struct {
	q *gen.Queries
}

// PendingLegsForEvent implements settlement.Tx.
//
// Returns an EMPTY SLICE AND A NIL ERROR for an event nobody bet on. That is the
// ordinary case for a result on a game with no exposure, and there is
// deliberately no sentinel for it: "nobody has money on this game" is not an
// error, and reporting it as one would make a quiet Sunday look like an outage.
//
// The three grading inputs on each ref are computed by the query rather than
// here — queries/settlement.sql's ListPendingLegsForEvent explains each — and
// the one worth repeating is DrawQuoted. Whether a moneyline market also quotes
// a draw decides what a tie means (a PUSH on a two-way market, a LOSS on a
// three-way one), it is a property of the MARKET that domain.Leg deliberately
// does not copy, and both shapes are live in this system because the synthetic
// provider quotes three-way moneylines for the sports that admit a draw.
//
// THE REFS ARE NOT VALIDATED HERE, and that is not an omission. Every enum goes
// through the domain's own ParseX, so a mis-mapped value is already a wrapped
// error at the read; the remaining rule LegRef.Validate applies — that the market
// type admits the role — is enforced by legs_role_allowed on the way in and is
// re-checked by internal/settlement at its own boundary, which is where ports.go
// puts it. Validating in both places would mean a future divergence is caught
// twice and explained by neither.
func (t *txStore) PendingLegsForEvent(ctx context.Context, id domain.EventID, limit int) ([]settlement.LegRef, error) {
	rowLimit, err := int32Limit(limit)
	if err != nil {
		return nil, err
	}

	rows, err := t.q.ListPendingLegsForEvent(ctx, gen.ListPendingLegsForEventParams{
		EventID:  id,
		RowLimit: rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: read pending legs on event %s: %w", id, err)
	}

	refs := make([]settlement.LegRef, 0, len(rows))
	for _, row := range rows {
		marketType, err := domain.ParseMarketType(row.MarketType)
		if err != nil {
			return nil, fmt.Errorf("pgstore: leg %s: %w", row.ID, err)
		}
		role, err := domain.ParseSelectionRole(row.Role)
		if err != nil {
			return nil, fmt.Errorf("pgstore: leg %s: %w", row.ID, err)
		}
		line, err := lineFrom(row.GradingLine)
		if err != nil {
			return nil, fmt.Errorf("pgstore: leg %s grading line: %w", row.ID, err)
		}

		refs = append(refs, settlement.LegRef{
			LegID:       row.ID,
			WagerID:     row.WagerID,
			EventID:     row.EventID,
			MarketType:  marketType,
			Role:        role,
			GradingLine: line,
			DrawQuoted:  row.DrawQuoted,
		})
	}
	return refs, nil
}

// WagerWithLegs implements settlement.Tx: one ticket, LOCKED, with every leg on
// it, rehydrated through the domain constructors.
//
// The lock is taken by the query (queries/settlement.sql's
// GetWagerForSettlement) and doc.go says what it buys and what it does not: it
// is not what prevents a double payout, it is what turns a race between two
// settle replicas into a wait instead of a commit-time trigger exception.
//
// THE REHYDRATION IS internal/betting/pgstore's HydrateWager, deliberately not a
// copy. Rebuilding a domain.Wager means REPLAYING its transitions — the domain
// has no rehydration constructor, on purpose, and Wager.stamp enforces monotone
// instants — so the order the replay visits Open, GradeLeg and Settle in is
// subtle and asymmetric between an open ticket and a terminal one. Two copies of
// that would drift, and the drift would be silent: a wager rehydrated at the
// wrong instant still looks like a wager. domain.Wager is the betting
// aggregate; the one function that reconstructs it lives with the package that
// writes it.
//
// The row conversion below is a Go STRUCT CONVERSION and not a field-by-field
// copy, which makes it a compile-time assertion that the two projections are
// still identical: gen.GetWagerForSettlementRow and gen.GetWagerRow select the
// same fifteen columns in the same order, and the day one of them changes this
// line stops compiling. A hand-written copy would keep compiling and quietly
// drop the new column.
//
// A missing wager is settlement.ErrWagerNotFound, and it really is exceptional
// on this path: the identifier came from a leg row this same transaction just
// read, and legs.wager_id is a foreign key.
func (t *txStore) WagerWithLegs(ctx context.Context, id domain.WagerID) (domain.Wager, error) {
	row, err := t.q.GetWagerForSettlement(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Wager{}, fmt.Errorf("%w: %s", settlement.ErrWagerNotFound, id)
	case err != nil:
		return domain.Wager{}, fmt.Errorf("pgstore: lock and read wager %s: %w", id, err)
	}

	legs, err := t.q.ListWagerLegs(ctx, id)
	if err != nil {
		return domain.Wager{}, fmt.Errorf("pgstore: read legs of wager %s: %w", id, err)
	}

	w, err := bettingpg.HydrateWager(gen.GetWagerRow(row), legs)
	if err != nil {
		return domain.Wager{}, fmt.Errorf("pgstore: %w", err)
	}
	return w, nil
}

// GradeLeg implements settlement.Tx: one leg's terminal grading, conditional on
// the leg still being pending.
//
// ZERO ROWS IS settlement.ErrLegAlreadyGraded AND NOT SUCCESS. Redelivery is
// routine — Kafka is at-least-once and the results feed's boundary is
// deliberately inclusive — so this is an expected outcome and not a fault; what
// it must never be is silent, because the caller decides whether to write a
// ledger movement based on whether its own grading was applied.
//
// The instant is the RESULT's finalisation instant, propagated from the provider
// rather than read from a clock here, so two runs over the same result stamp the
// same time. legs_assert_transition makes graded_at write-once, so a second
// attempt at a different instant is refused rather than silently moving the
// record.
//
// A terminal status is required by the schema's legs_graded_at_iff_graded
// biconditional and by domain.Leg.WithStatus. Passing pending here would be
// refused by the database, which is the right place for it: this method's job is
// to run the statement and report what happened, not to re-derive the state
// machine.
func (t *txStore) GradeLeg(ctx context.Context, id domain.LegID, status domain.LegStatus, at time.Time) error {
	if !status.Valid() {
		return fmt.Errorf("%w: leg %s: %w", settlement.ErrUnusableLeg, id, domain.ErrUnknownLegStatus)
	}

	n, err := t.q.GradeLeg(ctx, gen.GradeLegParams{
		ID:       id,
		Status:   status.String(),
		GradedAt: pgtype.Timestamptz{Time: at.UTC(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("pgstore: grade leg %s %s: %w", id, status, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", settlement.ErrLegAlreadyGraded, id)
	}
	return nil
}

// SettleWager implements settlement.Tx: the ticket's terminal status, returned
// amount, net return and transition instant, conditional on the ticket not
// already being terminal.
//
// ZERO ROWS IS settlement.ErrWagerAlreadySettled AND NOT SUCCESS. This is the
// half of the rows-affected contract that decides money: the caller writes a
// ledger movement immediately after this returns, and a nil here on a zero-row
// update would produce a second, perfectly balanced payout for a wager somebody
// else already paid.
//
// IT TAKES THE SETTLED domain.Wager, NOT LOOSE COLUMNS, which is ports.go's
// choice and the right one. Wager.Settle computes the net return from the
// returned amount and the stake and refuses an amount that contradicts the
// outcome, so the four values written here cannot be assembled inconsistently at
// a call site; wagers_net_return_identity and wagers_return_matches_outcome then
// re-check the same arithmetic on arrival. The CHECK is the reconciliation
// between the domain and the schema, not a substitute for either.
//
// A wager that is not terminal is refused here rather than sent. The row would
// be rejected by wagers_return_iff_terminal anyway, but as a check violation
// naming a constraint rather than as a sentence naming the mistake.
func (t *txStore) SettleWager(ctx context.Context, w domain.Wager) error {
	if !w.IsTerminal() {
		return fmt.Errorf("%w: wager %s is %s; SettleWager writes a terminal outcome",
			settlement.ErrInvalidOptions, w.ID(), w.Status())
	}
	returned, ok := w.Returned()
	if !ok {
		return fmt.Errorf("%w: wager %s is %s with no returned amount",
			settlement.ErrInvalidOptions, w.ID(), w.Status())
	}
	netReturn, _ := w.NetReturn()

	n, err := t.q.SettleWager(ctx, gen.SettleWagerParams{
		ID:             w.ID(),
		Status:         w.Status().String(),
		ReturnedMinor:  &returned,
		NetReturnMinor: &netReturn,
		TransitionedAt: w.UpdatedAt(),
	})
	if err != nil {
		return fmt.Errorf("pgstore: settle wager %s as %s returning %s: %w",
			w.ID(), w.Status(), returned, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", settlement.ErrWagerAlreadySettled, w.ID())
	}
	return nil
}

// InsertTransaction implements settlement.Tx: one balanced ledger movement,
// header then entries, in the same transaction as the wager update.
//
// A PRIMARY-KEY COLLISION IS THE IDEMPOTENCY GUARD, NOT A FAILURE.
// internal/settlement derives the transaction identifier deterministically from
// the wager, so a replayed settlement collides here instead of paying twice, and
// the 23505 becomes settlement.ErrTransactionExists.
//
// THE ENTRIES ARE NOT CHECKED FOR BALANCE, AND MUST NOT BE. They are balanced by
// construction — domain.NewTransaction refuses at least two entries that do not
// sum to exactly zero — and verified again by ledger_entries_balanced_at_commit
// at COMMIT. A third check here would be a second implementation of the one
// invariant in this system that must have exactly one.
//
// entry_index is the LOOP INDEX, because migration 00006 defines it as the
// ordinal within Transaction.entries so that ORDER BY entry_index rehydrates
// Transaction.Entries() in the order the domain built them. kind and occurred_at
// are taken from the HEADER on every entry, which is what the composite foreign
// key (transaction_id, kind, occurred_at) pins them to.
func (t *txStore) InsertTransaction(ctx context.Context, tr domain.Transaction) error {
	wagerID, hasWager := tr.WagerID()

	err := t.q.InsertLedgerTransaction(ctx, gen.InsertLedgerTransactionParams{
		ID:         tr.ID(),
		Kind:       tr.Kind().String(),
		WagerID:    optional(wagerID, hasWager),
		OccurredAt: tr.OccurredAt(),
	})
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return fmt.Errorf("%w: %s", settlement.ErrTransactionExists, tr.ID())
		}
		return fmt.Errorf("pgstore: insert ledger transaction %s: %w", tr.ID(), err)
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
			return fmt.Errorf("pgstore: insert ledger transaction %s entry %d: %w", tr.ID(), i, err)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// int32Limit narrows a batch size to the INTEGER a LIMIT parameter is.
//
// A non-positive limit is refused rather than sent: `LIMIT 0` returns nothing,
// which a caller would read as "this event has no pending legs" and act on by
// settling nothing — a silent, permanent stall on a customer's escrow.
// ports.go's ResultsSource makes the same demand of its own limit.
func int32Limit(n int) (int32, error) {
	if n <= 0 {
		return 0, fmt.Errorf("%w: batch limit %d must be positive; LIMIT 0 returns no rows, "+
			"which reads as 'nothing to settle'", settlement.ErrInvalidOptions, n)
	}
	if n > math.MaxInt32 {
		return 0, fmt.Errorf("%w: batch limit %d exceeds the column's range",
			settlement.ErrInvalidOptions, n)
	}
	return int32(n), nil
}

// lineFrom turns a nullable DOUBLE PRECISION into a [domain.Line].
//
// NULL is domain.NoLine(); 0.0 is a stored PICK'EM, which is a real traded value
// and not an absent one. Collapsing the two would turn every pick'em spread into
// something the grader treats as unlined, which is how a spread grades as a
// moneyline.
//
// The column here is COALESCE(teased_line, price_line) — Leg.GradingLine() in
// SQL — so it is already the teased number where one exists and already inverted
// for an away spread.
func lineFrom(v pgtype.Float8) (domain.Line, error) {
	if !v.Valid {
		return domain.NoLine(), nil
	}
	return domain.NewLine(v.Float64)
}

// optional turns a (value, present) pair into the pointer sqlc uses for a
// nullable column carrying a domain type. See the twin in
// internal/betting/pgstore for why those columns are pointers and not pgtypes.
func optional[T any](v T, present bool) *T {
	if !present {
		return nil
	}
	return &v
}
