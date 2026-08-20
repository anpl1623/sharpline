// Responsible-gaming limit evaluation: the period arithmetic, the sums over
// ledger_entries, and the one kind that is deliberately not evaluated here.
//
// CLAUDE.md §6 asks for "responsible-gaming-style self-imposed limits (a nod to
// how the real domain works)". migrations/00005 built the storage as an
// append-only history and delegated the POLICY — what a period means, when a
// loosening binds, how a limit is measured — to this package. This file is that
// policy, written down.
//
// # Why enforcement is a sum and not a counter
//
// A running total per (user, kind, period) would be one indexed read instead of
// three aggregates. It is refused for the reason CLAUDE.md §4 refuses a balance
// column: a counter is a second copy of a fact the ledger already holds, it is
// independently writable, and when it drifts nobody notices — because the
// counter, not the ledger, is the number the control consults.
//
// So a limit is a fold over ledger_entries in the window, taken inside the
// placement transaction under the same users-row lock as the balance. It cannot
// be stale, it cannot drift, and it agrees with the wager history by
// construction rather than by a reconciliation job.
//
// The three money kinds spell exactly [domain.EntryKind]'s strings — 'grant',
// 'stake', 'loss' — and migrations/00005 says why: "so that enforcing a limit is
// a sum over ledger_entries filtered by the same string, with no translation
// table in between." Keep them equal.
//
// # Why 'session' is not evaluated here, stated as a biconditional
//
// A limit is a money sum EXACTLY WHEN auth.LimitKind.IsMoney() is true, and
// 'session' is the complement. migrations/00005 makes that a database invariant
// with three CHECKs (user_limits_session_period, user_limits_session_is_duration,
// user_limits_money_is_amount) so a money-denominated session limit is
// unstorable.
//
// A session limit bounds the WALL-CLOCK DURATION of one authenticated session,
// which is internal/auth's question — it is enforced by capping a refresh
// token's successor rather than by refusing a bet, and a customer who has hit it
// is signed out, not declined. Evaluating it here would mean treating a seconds
// value as minor units, which is the exact class of units error CLAUDE.md §12
// puts money in integers to avoid. So it is skipped, loudly, in one place.
package betting

import (
	"context"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
)

// lossEntryKinds are the entry kinds that net to a customer's betting result
// against their cash account.
//
// EVERY KIND EXCEPT 'grant'. The reasoning, per kind:
//
//	stake       −stake against cash at placement. Money going out.
//	payout      +returned on a win.
//	refund      +stake on a void or push.
//	cash_out    +the agreed price on an early close.
//	adjustment  an operator correction, signed. Included, because after a
//	            correction the customer's real position has changed, and a loss
//	            limit that ignored corrections would keep declining a customer
//	            whose loss was already put right.
//	grant       EXCLUDED. It is play money entering the system, not a loss
//	            recovered. Including it would let a fresh grant silently raise
//	            the customer's loss headroom, which is exactly backwards for a
//	            control whose whole purpose is to hold when the customer is
//	            tempted to top up.
//
// So: loss = −(net of every non-grant cash entry in the window). A positive
// result is a net loss; a negative one means the customer is up, and the limit
// cannot be breached.
var lossEntryKinds = []domain.EntryKind{
	domain.EntryKindStake,
	domain.EntryKindPayout,
	domain.EntryKindRefund,
	domain.EntryKindCashOut,
	domain.EntryKindAdjustment,
}

// stakeEntryKinds and grantEntryKinds are single-element sets, named so the
// call sites read as the question being asked rather than as a slice literal.
var (
	stakeEntryKinds = []domain.EntryKind{domain.EntryKindStake}
	grantEntryKinds = []domain.EntryKind{domain.EntryKindGrant}
)

// windowStart returns the beginning of the period ending at `at`.
//
// THE WINDOWS ARE ROLLING, NOT CALENDAR, and auth.LimitPeriodDay explicitly
// leaves the choice to this package ("a rolling or calendar day — which of the
// two is policy and lives in internal/betting"). The reason:
//
// A calendar window has to reset at midnight in SOME timezone. Every instant in
// this schema is UTC and migration 00005 has no per-user timezone column —
// CLAUDE.md §0 forbids adding one, since jurisdiction and location fields are
// the licensed-book surface this project deliberately does not have. So a
// calendar day would reset at UTC midnight, which is 1am or 2am across Europe:
// mid-session, for a customer who set a daily limit precisely so that a bad
// evening would end. A rolling window has no reset a customer can sit and wait
// for, and needs no timezone at all.
//
// Week is seven days. Month is AddDate(0, -1, 0), a real calendar month rather
// than "thirty days": AddDate normalises an overflowing day, so a month before
// 31 March is 3 March. The window is then up to three days LONGER than a strict
// month at the year's two short boundaries — which is the safe direction, since
// a longer lookback sums more spending and is therefore more restrictive.
func windowStart(p auth.LimitPeriod, at time.Time) (time.Time, bool) {
	switch p {
	case auth.LimitPeriodDay:
		return at.Add(-24 * time.Hour), true
	case auth.LimitPeriodWeek:
		return at.AddDate(0, 0, -7), true
	case auth.LimitPeriodMonth:
		return at.AddDate(0, -1, 0), true
	default:
		// LimitPeriodSession and LimitPeriodUnknown. Neither is a money window;
		// see the file header. The bool is the caller's signal to skip rather
		// than to guess.
		return time.Time{}, false
	}
}

// evaluateLimits refuses the placement if any self-imposed money limit in force
// would be breached by staking `stake` at `at`.
//
// It is called inside the placement transaction, after [Tx.UserStatus] has
// locked the customer's row, so the sums it takes cannot be raced by a
// concurrent placement of the same customer's.
//
// The request is checked against `used + stake > limit`, in minor units, so the
// comparison is exact. A slip that takes the customer exactly TO the limit is
// accepted; one that takes them past it is refused. That boundary is stated
// because it is the one a customer will test, and "$200 limit, $200 staked" is
// the reading that matches how the number is described to them.
func evaluateLimits(
	ctx context.Context,
	tx Tx,
	user domain.UserID,
	stake domain.Money,
	at time.Time,
) error {
	return evaluateLimitsWith(ctx, tx, user, at, func(kind auth.LimitKind) domain.Money {
		return requestedForLimit(kind, stake)
	})
}

// evaluateLimitsWith is the limit loop, parameterised by WHAT THE MOVEMENT ADDS
// to each limit kind.
//
// Two movements in this package are limit-checked and they contribute to
// different kinds: a stake adds to the stake and loss limits and nothing to the
// grant limit, while a grant adds to the grant limit and nothing to the other
// two. Everything else about the check — the window arithmetic, the
// effective-from assertion, the exact minor-unit comparison, the breach value —
// is identical, and a second copy of it would be a second place for the
// boundary rule ("exactly AT the limit is accepted") to drift.
//
// The callback returns minor units and is total over the money kinds: a kind
// the movement does not touch returns zero, which can never breach a
// non-negative limit. It is NOT allowed to skip a kind, because a limit that is
// silently not evaluated is indistinguishable from a limit that passed.
func evaluateLimitsWith(
	ctx context.Context,
	tx Tx,
	user domain.UserID,
	at time.Time,
	requestedFor func(auth.LimitKind) domain.Money,
) error {
	limits, err := tx.LimitsInForce(ctx, user, at)
	if err != nil {
		return fmt.Errorf("betting: read limits in force for %s: %w", user, err)
	}
	if len(limits) == 0 {
		return nil
	}

	cash, err := domain.UserCashAccount(user)
	if err != nil {
		return fmt.Errorf("betting: cash account for %s: %w", user, err)
	}

	for _, lim := range limits {
		if !lim.Kind.IsMoney() {
			// The biconditional in the file header. Skipped, not evaluated as
			// zero: a session limit's Amount is meaningless, and treating a
			// meaningless zero as "limit of zero" would refuse every bet.
			continue
		}
		since, windowed := windowStart(lim.Period, at)
		if !windowed {
			// A money kind on a session period is unstorable
			// (user_limits_session_period) and auth.LimitPairValid says so in
			// Go. Reaching here means the row violates a database CHECK, which
			// is a corrupt row rather than a customer decision — refusing the
			// bet is the only safe reading, since the alternative is ignoring a
			// responsible-gaming control because it looked malformed.
			return fmt.Errorf("betting: %s limit on a %s period is not a money window: %w",
				lim.Kind, lim.Period, ErrLimitExceeded)
		}
		if lim.EffectiveFrom.After(at) {
			// [Tx.LimitsInForce] promises not to return these. Asserting the
			// promise rather than trusting it costs one comparison and turns a
			// store bug — a pending loosening served as though it were in force
			// — into a refusal instead of into an unlimited customer.
			return fmt.Errorf("betting: %s limit per %s is not effective until %s: %w",
				lim.Kind, lim.Period, lim.EffectiveFrom.UTC().Format(time.RFC3339), ErrLimitExceeded)
		}

		used, err := usedForLimit(ctx, tx, cash, lim.Kind, since)
		if err != nil {
			return err
		}
		requested := requestedFor(lim.Kind)

		projected, err := used.Add(requested)
		if err != nil {
			return fmt.Errorf("betting: project %s limit usage: %w", lim.Kind, err)
		}
		if projected.Compare(lim.Amount) > 0 {
			return &LimitBreach{
				Kind:        lim.Kind.String(),
				Period:      lim.Period.String(),
				Limit:       lim.Amount.MinorUnits(),
				Used:        used.MinorUnits(),
				Requested:   requested.MinorUnits(),
				WindowStart: since.UTC().Format(time.RFC3339),
			}
		}
	}
	return nil
}

// usedForLimit is how much of one limit the customer has already consumed in
// the window.
//
// The sign handling is the interesting part, and it is why [Tx.SumEntries]
// returns a SIGNED sum rather than an absolute one: the ledger's convention is
// "positive credits the account, negative debits it" (ledger.go), and a second
// spelling of that convention living inside a store adapter is how the two
// eventually disagree. So the sum comes back signed and the negation happens
// here, once, at the place where the question makes the direction obvious.
//
//	grant  entries CREDIT cash, so the sum is already positive: used as is.
//	stake  entries DEBIT cash, so the sum is negative: negated.
//	loss   is the net of every non-grant entry, negated — a customer who is up
//	       on the period produces a negative "used", which can never breach a
//	       positive limit and is deliberately not clamped to zero, because
//	       clamping would hide a sign error behind a plausible zero.
func usedForLimit(
	ctx context.Context,
	tx Tx,
	cash domain.Account,
	kind auth.LimitKind,
	since time.Time,
) (domain.Money, error) {
	var (
		kinds  []domain.EntryKind
		negate bool
	)
	switch kind {
	case auth.LimitKindGrant:
		kinds, negate = grantEntryKinds, false
	case auth.LimitKindStake:
		kinds, negate = stakeEntryKinds, true
	case auth.LimitKindLoss:
		kinds, negate = lossEntryKinds, true
	default:
		return 0, fmt.Errorf("betting: %s is not a money limit kind: %w", kind, ErrLimitExceeded)
	}

	sum, err := tx.SumEntries(ctx, cash, kinds, since)
	if err != nil {
		return 0, fmt.Errorf("betting: sum %s entries since %s: %w",
			kind, since.UTC().Format(time.RFC3339), err)
	}
	if !negate {
		return sum, nil
	}
	negated, err := sum.Neg()
	if err != nil {
		return 0, fmt.Errorf("betting: negate %s entry sum: %w", kind, err)
	}
	return negated, nil
}

// requestedForLimit is what THIS slip adds to one limit's usage.
//
// A stake adds to the stake limit and, since the money leaves the customer's
// cash the moment the ticket is booked, to the loss limit as well — a bet that
// has not settled is a loss until it is not, which is the conservative reading
// and the one a customer setting a loss limit means. It adds nothing to a grant
// limit: placing a bet does not issue play money, and counting it as though it
// did would refuse a bet for breaching a limit on a movement that never
// happened.
func requestedForLimit(kind auth.LimitKind, stake domain.Money) domain.Money {
	switch kind {
	case auth.LimitKindStake, auth.LimitKindLoss:
		return stake
	default:
		return domain.ZeroMoney
	}
}
