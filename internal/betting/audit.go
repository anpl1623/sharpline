// The audit vocabulary this package writes, and the two entries it builds.
//
// CLAUDE.md §6 asks for "an audit log on every state-changing action". This
// package performs exactly two — booking a ticket and issuing play money — and
// both are written through [Tx.RecordAudit], inside the transaction that
// performs them. Read that method's comment first; it carries the argument for
// why an after-the-fact write on another connection is not an acceptable
// substitute.
//
// # Why the action strings are constants here rather than literals at the call
// # site
//
// They are a VOCABULARY, not decoration. migration 00007 bounds their shape
// with a CHECK — dotted lowercase, at least one dot, ≤96 characters — but not
// their spelling, and the queries that will read this table ("everything that
// happened to this wager", "every top-up this customer made last night") filter
// on the exact string. Two spellings of one action is a query that silently
// misses half its rows, which is the failure mode an audit trail can least
// afford. Declaring them once makes a rename a compile-time event.
//
// The spellings follow the ones migration 00007 names as examples —
// 'market.suspend', 'wager.place', 'feature_flag.update', 'auth.login' — and
// the ones internal/httpapi already writes ('user_limit.set', 'totp.remove').
package betting

import (
	"context"
	"fmt"

	"github.com/anpl1623/sharpline/internal/domain"
)

// The actions this package records. `domain.verb`, present tense, matching the
// vocabulary in migration 00007 and internal/httpapi.
const (
	// ActionWagerPlace is one BOOKED TICKET. A round robin writes one entry per
	// combination, not one for the request: three tickets is three state
	// changes, each with its own id, and collapsing them would make "what
	// happened to ticket AC" unanswerable — which is the same reason wager.go
	// refuses to model a round robin as a single wager.
	ActionWagerPlace = "wager.place"

	// ActionLedgerGrant is one play-money issuance. The entity is the LEDGER
	// TRANSACTION rather than the user, because the transaction is the thing
	// that came into existence and the thing a reviewer would go and read.
	ActionLedgerGrant = "ledger.grant"
)

// The entity types this package names. migration 00007 constrains them to
// lowercase snake_case with no dots, and they are singular table names so that
// "which table is entity_id in" needs no lookup table.
const (
	entityWager             = "wager"
	entityLedgerTransaction = "ledger_transaction"
)

// audit_log's `actor_kind` and `outcome` columns are NOT on [AuditEntry] and
// are supplied by the adapter, which is the layer that knows the table has
// them. Both are constant for everything this package writes — every action
// here is a customer acting on their own account, and only committed changes
// reach [Tx.RecordAudit] — so carrying them as fields would offer a caller a
// choice that has exactly one correct answer, and 'system' on a customer's bet
// would look ordinary in a listing. internal/betting/pgstore states the same
// reasoning where the values are actually written.

// auditPlacement records one booked ticket.
//
// The after-state is the ticket's IDENTIFYING AND MONETARY facts and nothing
// else — kind, stake, leg count, the accepted price, the payout at stake, and
// the round-robin parent where there is one. Not the legs: a five-leg parlay's
// legs are five rows in `legs` joined to this wager id, and copying them into a
// JSONB column would be a second, un-updatable copy of data the entity_id
// already points at. What the diff is for is answering "what did this action
// commit" without a join, and for a placement that is the money.
//
// Money crosses as MINOR UNITS (int64), never as a float and never as a
// formatted string, per CLAUDE.md §12. A JSONB number that went through a
// float64 would round a large payout, in the one table whose purpose is being
// exact about what happened.
func auditPlacement(ctx context.Context, tx Tx, w domain.Wager, ac AuditContext) error {
	after := map[string]any{
		"kind":                   w.Kind().String(),
		"status":                 w.Status().String(),
		"leg_count":              w.LegCount(),
		"stake_minor":            w.Stake().MinorUnits(),
		"accepted_decimal":       w.AcceptedDecimal(),
		"potential_payout_minor": w.PotentialPayout().MinorUnits(),
	}
	if parent, ok := w.RoundRobinID(); ok {
		after["round_robin_id"] = parent.String()
	}
	if points, ok := w.TeaserPoints(); ok {
		after["teaser_points"] = points
	}

	entry := AuditEntry{
		Context:    ac,
		OccurredAt: w.PlacedAt(),
		ActorID:    w.UserID(),
		Action:     ActionWagerPlace,
		EntityType: entityWager,
		EntityID:   w.ID().String(),
		After:      after,
	}
	if err := tx.RecordAudit(ctx, entry); err != nil {
		return fmt.Errorf("betting: audit placement of wager %s: %w", w.ID(), err)
	}
	return nil
}

// auditGrant records one play-money issuance.
//
// The user id is on the entry as the ACTOR and is deliberately not repeated in
// the diff: a grant credits exactly one customer's cash account and it is the
// customer's own request, so actor and beneficiary are the same party by
// construction. Repeating it would invite a later reader to believe the two
// could differ.
func auditGrant(ctx context.Context, tx Tx, t domain.Transaction, user domain.UserID, ac AuditContext) error {
	credited, err := creditedTo(t, user)
	if err != nil {
		return err
	}

	entry := AuditEntry{
		Context:    ac,
		OccurredAt: t.OccurredAt(),
		ActorID:    user,
		Action:     ActionLedgerGrant,
		EntityType: entityLedgerTransaction,
		EntityID:   t.ID().String(),
		After: map[string]any{
			"kind":         t.Kind().String(),
			"amount_minor": credited.MinorUnits(),
		},
	}
	if err := tx.RecordAudit(ctx, entry); err != nil {
		return fmt.Errorf("betting: audit grant transaction %s: %w", t.ID(), err)
	}
	return nil
}

// creditedTo is what the transaction moved INTO the customer's cash account.
//
// Read off the movement rather than taken from the request, for the reason
// [Service.replayedGrant] reads the amount back rather than echoing it: the
// number in the trail must be the number in the ledger, and the only way to
// guarantee that is to compute both from the same value. domain.NewGrantTransaction
// guarantees the entry exists, so its absence is a domain bug and is reported
// as one rather than silently audited as zero.
func creditedTo(t domain.Transaction, user domain.UserID) (domain.Money, error) {
	cash, err := domain.UserCashAccount(user)
	if err != nil {
		return 0, fmt.Errorf("betting: cash account for %s: %w", user, err)
	}
	net, err := t.NetFor(cash)
	if err != nil {
		return 0, fmt.Errorf("betting: net of transaction %s against %s: %w", t.ID(), cash, err)
	}
	return net, nil
}
