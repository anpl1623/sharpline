// Play-money issuance: the one path by which money enters this system.
//
// # Why this file exists at all
//
// Everything else in this package MOVES money that is already there. A stake
// debits cash and credits escrow; a settlement moves escrow to cash or to the
// house. Every one of those movements is zero-sum over accounts that already
// hold a balance, so without an issuance path every balance in the system is
// permanently zero and the entire betting surface is unreachable — the ledger
// is correct, complete, and about nothing.
//
// migrations/00006 says where money is supposed to come from, and names the
// gap: "An empty wagers / ledger after `make up` is CORRECT. Money enters
// through EntryKindGrant, written by the application." domain.ledger.go supplies
// [domain.NewGrantTransaction] for it. This file is the application writing it.
//
// # Why a customer may grant to themselves, and why that is not a hole
//
// CLAUDE.md §0 is explicit that this is a simulation and that "no real money
// moves": there is no deposit, no payment processor, and no custody. A grant is
// therefore not a deposit and must not be modelled as one — the OpenAPI
// document says so in as many words at the `grant` limit kind. What a grant
// actually is, in a play-money book, is the customer topping their own chips
// back up.
//
// That is also the reading the rest of the codebase already committed to. The
// responsible-gaming machinery carries a 'grant' LIMIT KIND end to end —
// auth.LimitKindGrant, user_limits' CHECK, limits.go's grantEntryKinds, and the
// OpenAPI enum — and a limit exists to cap something the customer can do to
// themselves. A grant that only an operator could issue would make that limit
// meaningless, because no customer decision would ever be bounded by it. The
// limit was designed for this endpoint; this endpoint is what makes the limit
// do anything.
//
// So the control on issuance is not authorisation, it is the customer's own
// self-imposed cap plus a hard per-request ceiling, and a self-excluded account
// cannot top up at all.
//
// # The replay guarantee, and why it is stricter here than anywhere else
//
// A grant is idempotent on (userID, Idempotency-Key) through a derived
// transaction id, exactly like a placement — see [DeriveGrantTransactionID],
// which carries the argument for why a doubled grant is the worst replay in the
// system: it mints money into a ledger that still balances afterwards, so
// nothing downstream detects it.
package betting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
)

// MaxGrantAmount is the hard ceiling on a single top-up, in minor units.
//
// It is a constant rather than an option because it is not a tuning knob: it
// bounds the blast radius of a bug or an abusive client in the one operation
// that creates money, and an operator who could raise it could mint an
// arbitrary balance. A customer who wants more chips submits again, under a
// fresh idempotency key, and is bounded by their own 'grant' limit while doing
// so.
//
// 1,000,000 minor units is 10,000.00 in major units — comfortably more than any
// slip this book prices, and far below domain.MaxSafeMoney, so a sequence of
// grants cannot approach the 2^53-1 bound that keeps a balance exact in the
// browser.
const MaxGrantAmount domain.Money = 1_000_000

// GrantRequest is a customer's play-money top-up.
type GrantRequest struct {
	UserID domain.UserID

	// Amount is minor units and must be positive. Zero is refused rather than
	// treated as a no-op, because a zero-amount movement is unstorable
	// (ledger_entries' CHECK refuses a zero amount) and answering "fine" to a
	// request that wrote nothing would be a lie the customer cannot detect.
	Amount domain.Money

	// IdempotencyKey identifies the SUBMIT. Required, for the reason in
	// [DeriveGrantTransactionID].
	IdempotencyKey string

	// Audit is the provenance stamped onto the audit row written in the same
	// transaction as the movement. The zero value is legal and means "no HTTP
	// request produced this"; the row is still written, with null correlation
	// columns. See [PlaceRequest.Audit] and [AuditContext].
	Audit AuditContext
}

// Grant is the outcome of a top-up.
type Grant struct {
	// TransactionID is the derived ledger transaction. Returned so a client
	// that timed out can look the movement up rather than guess.
	TransactionID domain.TransactionID

	// Amount is what was credited by the transaction this request names — on a
	// replay, what the ORIGINAL request credited, which is not necessarily what
	// this request asked for. A client presenting a used key with a different
	// amount gets the original movement back, unchanged, exactly as a replayed
	// placement gets the original ticket back.
	Amount domain.Money

	// Balance is the customer's cash balance AFTER the movement, folded over
	// ledger_entries inside the same transaction that wrote it. It is a fold
	// and never a stored figure (CLAUDE.md §4).
	Balance domain.Money

	// OccurredAt is the movement's instant.
	OccurredAt time.Time

	// Replayed reports that this key had already issued this grant and nothing
	// was written. Not an error — see [ErrAlreadyPlaced].
	Replayed bool
}

// Grant credits a customer's cash account with play money, idempotently.
//
// The order inside the transaction is the same one [Service.Place] uses and for
// the same reason: the users row is LOCKED FIRST, and the limit sums that follow
// are read-then-write over a derived quantity with no constraint behind them, so
// without the lock two concurrent top-ups could each observe the same "used" and
// both pass a limit that only one of them fits under.
//
// ON FAILURE NOTHING IS RETRIED, per CLAUDE.md's phase-8 rule: a retried ledger
// write double-applies. A caller that wants a retry resubmits with the SAME
// idempotency key, which is safe by construction.
func (s *Service) Grant(ctx context.Context, req GrantRequest) (Grant, error) {
	if req.UserID.IsZero() {
		return Grant{}, fmt.Errorf("betting: grant: %w", domain.ErrEmptyID)
	}
	key, err := normaliseIdempotencyKey(req.IdempotencyKey)
	if err != nil {
		return Grant{}, err
	}
	if req.Amount <= 0 {
		return Grant{}, fmt.Errorf("betting: grant of %d minor units is not positive: %w",
			req.Amount.MinorUnits(), ErrInvalidGrantAmount)
	}
	if req.Amount > MaxGrantAmount {
		return Grant{}, fmt.Errorf("betting: grant of %d minor units exceeds the %d maximum: %w",
			req.Amount.MinorUnits(), MaxGrantAmount.MinorUnits(), ErrInvalidGrantAmount)
	}

	// ONE instant for the whole grant: the transaction's occurred_at and the
	// limit windows are the same value, read once. See [NewService].
	now := s.now().UTC()

	txnID, err := DeriveGrantTransactionID(req.UserID, key)
	if err != nil {
		return Grant{}, err
	}

	var out Grant
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		var txErr error
		out, txErr = s.grant(ctx, tx, req, txnID, now)
		return txErr
	})
	if err == nil {
		return out, nil
	}

	if !errors.Is(err, ErrAlreadyPlaced) {
		return Grant{}, err
	}

	// A replay. The transaction that discovered it was ROLLED BACK — a
	// duplicate primary key aborts it, and unlike the placement path there is
	// no savepoint here, deliberately: placement recovers in-transaction
	// because it has more to write afterwards, whereas a grant has nothing left
	// to do but report. So the read-back is a SEPARATE transaction, which is
	// correct precisely because the movement it reports was committed by an
	// earlier request and is not in flight.
	return s.replayedGrant(ctx, req.UserID, txnID)
}

// grant is the body of the transaction.
func (s *Service) grant(
	ctx context.Context,
	tx Tx,
	req GrantRequest,
	txnID domain.TransactionID,
	now time.Time,
) (Grant, error) {
	// The lock, first. Everything below is read-then-write over a derived
	// quantity.
	if err := s.checkStatus(ctx, tx, req.UserID); err != nil {
		return Grant{}, err
	}

	// A grant adds to the 'grant' limit and to nothing else. It is not a stake,
	// and although it does raise the customer's net position it is deliberately
	// not netted against the LOSS limit: a loss limit caps what the customer
	// may lose, and letting a top-up reduce the measured loss would let someone
	// who set a £100 loss limit spend past it by topping up in between, which
	// inverts the control. limits.go's usedForLimit already excludes grants
	// from the loss sum for the same reason.
	if err := evaluateLimitsWith(ctx, tx, req.UserID, now, func(kind auth.LimitKind) domain.Money {
		if kind == auth.LimitKindGrant {
			return req.Amount
		}
		return 0
	}); err != nil {
		return Grant{}, err
	}

	movement, err := domain.NewGrantTransaction(txnID, req.UserID, req.Amount, now)
	if err != nil {
		return Grant{}, fmt.Errorf("betting: build grant transaction %s: %w", txnID, err)
	}
	if err := tx.InsertTransaction(ctx, movement); err != nil {
		// ErrAlreadyPlaced included: [Service.Grant] reads it back outside this
		// transaction, because a duplicate key has aborted this one.
		return Grant{}, fmt.Errorf("betting: insert grant transaction %s: %w", txnID, err)
	}

	// The audit row, in this transaction, so the record of who minted the money
	// commits with the money (CLAUDE.md §6, and [Tx.RecordAudit]). It matters
	// more here than anywhere else in this package: a grant is the ONLY
	// operation that creates value, and a doubled or unattributed one leaves a
	// ledger that still balances, so the trail is the only place the anomaly
	// would ever show up.
	//
	// [Service.replayedGrant] deliberately writes none. It reaches its
	// transaction only after this one aborted on a duplicate key, meaning the
	// movement it reports was committed — and audited — by an earlier request.
	if err := auditGrant(ctx, tx, movement, req.UserID, req.Audit); err != nil {
		return Grant{}, err
	}

	balance, err := s.cashBalance(ctx, tx, req.UserID)
	if err != nil {
		return Grant{}, err
	}

	return Grant{
		TransactionID: txnID,
		Amount:        req.Amount,
		Balance:       balance,
		OccurredAt:    now,
	}, nil
}

// replayedGrant answers a duplicate key with the movement that already exists.
//
// The AMOUNT IS READ BACK rather than echoed from the request. Echoing would
// make a client that resubmitted a used key with a larger figure believe the
// larger figure had been credited, when nothing was written at all — the same
// trap the OpenAPI document describes for a replayed placement presenting a
// different slip.
func (s *Service) replayedGrant(ctx context.Context, user domain.UserID, txnID domain.TransactionID) (Grant, error) {
	var out Grant
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		credited, occurredAt, err := tx.GrantCredit(ctx, txnID, user)
		if err != nil {
			return fmt.Errorf("betting: read back grant transaction %s: %w", txnID, err)
		}
		balance, err := s.cashBalance(ctx, tx, user)
		if err != nil {
			return err
		}

		out = Grant{
			TransactionID: txnID,
			Amount:        credited,
			Balance:       balance,
			OccurredAt:    occurredAt,
			Replayed:      true,
		}
		return nil
	})
	if err != nil {
		return Grant{}, err
	}
	return out, nil
}

// cashBalance folds the customer's derived cash balance.
func (s *Service) cashBalance(ctx context.Context, tx Tx, user domain.UserID) (domain.Money, error) {
	cash, err := domain.UserCashAccount(user)
	if err != nil {
		return 0, fmt.Errorf("betting: cash account for %s: %w", user, err)
	}
	balance, err := tx.Balance(ctx, cash)
	if err != nil {
		return 0, fmt.Errorf("betting: read balance for %s: %w", user, err)
	}
	return balance, nil
}
