package betting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
)

// TestGrantCreditsAndBalances is the happy path, and it asserts the SHAPE of the
// movement rather than only that no error came back.
//
// A grant is the one operation in the package that CREATES money, so "it did not
// fail" is far too weak a claim: the interesting properties are that exactly one
// transaction was written, that it is a grant, that its two halves sum to zero,
// and that the customer's cash side is the credit rather than the debit. A bug
// that reversed the entries would leave the ledger balanced and the customer
// poorer, and every assertion short of these would pass.
func TestGrantCreditsAndBalances(t *testing.T) {
	t.Parallel()

	tx := newFakeTx()
	svc, _ := newTestService(t, tx)

	grant, err := svc.Grant(context.Background(), GrantRequest{
		UserID:         testUser,
		Amount:         domain.Money(25_000),
		IdempotencyKey: testKey,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if grant.Replayed {
		t.Error("a first grant reported itself as a replay")
	}
	if grant.Amount != domain.Money(25_000) {
		t.Errorf("credited %d, want 25000", grant.Amount.MinorUnits())
	}
	if grant.TransactionID.IsZero() {
		t.Error("the grant reported no transaction id")
	}

	if len(tx.transactions) != 1 {
		t.Fatalf("wrote %d ledger transactions, want exactly 1", len(tx.transactions))
	}
	movement := tx.transactions[0]
	if movement.Kind() != domain.EntryKindGrant {
		t.Errorf("wrote a %s transaction, want a grant", movement.Kind())
	}

	cash, err := domain.UserCashAccount(testUser)
	if err != nil {
		t.Fatalf("UserCashAccount: %v", err)
	}

	var sum domain.Money
	var credited domain.Money
	for _, entry := range movement.Entries() {
		sum += entry.Amount()
		if entry.Account() == cash {
			credited = entry.Amount()
		}
	}
	if sum != 0 {
		t.Errorf("the grant's entries sum to %d, want exactly 0 -- a grant is double-entry "+
			"like every other movement and an unbalanced one is rejected at COMMIT", sum.MinorUnits())
	}
	if credited != domain.Money(25_000) {
		t.Errorf("the customer's cash side is %d, want +25000; a negative here means the "+
			"entries are reversed and the top-up debited the customer", credited.MinorUnits())
	}
}

// TestGrantIsIdempotentOnTheKey is the property that matters most in the
// package: a replayed top-up must not mint money a second time.
//
// The fake mirrors ledger_transactions' primary key, so the second submit
// collides exactly as the database would, and the assertion is on the number of
// TRANSACTIONS WRITTEN rather than on the response — a service that answered
// correctly while writing twice would pass any response-only check and would
// have doubled the customer's balance.
func TestGrantIsIdempotentOnTheKey(t *testing.T) {
	t.Parallel()

	tx := newFakeTx()
	svc, _ := newTestService(t, tx)
	req := GrantRequest{UserID: testUser, Amount: domain.Money(10_000), IdempotencyKey: testKey}

	first, err := svc.Grant(context.Background(), req)
	if err != nil {
		t.Fatalf("first grant: %v", err)
	}
	second, err := svc.Grant(context.Background(), req)
	if err != nil {
		t.Fatalf("replayed grant: %v", err)
	}

	if len(tx.transactions) != 1 {
		t.Fatalf("a replayed grant wrote %d transactions, want 1 -- the customer was "+
			"credited twice", len(tx.transactions))
	}
	if !second.Replayed {
		t.Error("the replay did not report itself as one, so a client cannot tell a " +
			"fresh credit from a repeated one")
	}
	if first.Replayed {
		t.Error("the first grant reported itself as a replay")
	}
	if second.TransactionID != first.TransactionID {
		t.Errorf("the replay reported transaction %s, want %s", second.TransactionID, first.TransactionID)
	}
	if second.Amount != first.Amount {
		t.Errorf("the replay reported %d credited, want %d", second.Amount.MinorUnits(), first.Amount.MinorUnits())
	}
}

// TestAReplayedGrantReportsTheOriginalAmount is the trap the OpenAPI document
// describes for a replayed placement, applied to money.
//
// The key identifies the SUBMIT, not the body. Resubmitting a used key with a
// LARGER figure must return the movement that actually exists — echoing the
// request back would tell a customer they had been credited 900 when nothing at
// all was written.
func TestAReplayedGrantReportsTheOriginalAmount(t *testing.T) {
	t.Parallel()

	tx := newFakeTx()
	svc, _ := newTestService(t, tx)

	if _, err := svc.Grant(context.Background(), GrantRequest{
		UserID: testUser, Amount: domain.Money(100), IdempotencyKey: testKey,
	}); err != nil {
		t.Fatalf("first grant: %v", err)
	}

	replay, err := svc.Grant(context.Background(), GrantRequest{
		UserID: testUser, Amount: domain.Money(900), IdempotencyKey: testKey,
	})
	if err != nil {
		t.Fatalf("replayed grant: %v", err)
	}

	if replay.Amount != domain.Money(100) {
		t.Errorf("the replay reported %d credited, want 100 -- the amount must be read "+
			"back, never echoed from a request that wrote nothing", replay.Amount.MinorUnits())
	}
	if len(tx.transactions) != 1 {
		t.Errorf("wrote %d transactions, want 1", len(tx.transactions))
	}
}

// TestGrantRefusesBadInput covers the amounts and keys that must never reach a
// ledger write.
func TestGrantRefusesBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  GrantRequest
		want error
	}{
		{
			name: "no user",
			req:  GrantRequest{Amount: 100, IdempotencyKey: testKey},
			want: domain.ErrEmptyID,
		},
		{
			name: "no idempotency key",
			req:  GrantRequest{UserID: testUser, Amount: 100},
			want: ErrIdempotencyKeyRequired,
		},
		{
			// Not a harmless no-op: ledger_entries refuses a zero amount by
			// CHECK, so answering "fine" would report a transaction the
			// database would have rejected.
			name: "a zero amount",
			req:  GrantRequest{UserID: testUser, Amount: 0, IdempotencyKey: testKey},
			want: ErrInvalidGrantAmount,
		},
		{
			name: "a negative amount",
			req:  GrantRequest{UserID: testUser, Amount: -100, IdempotencyKey: testKey},
			want: ErrInvalidGrantAmount,
		},
		{
			name: "past the per-request ceiling",
			req:  GrantRequest{UserID: testUser, Amount: MaxGrantAmount + 1, IdempotencyKey: testKey},
			want: ErrInvalidGrantAmount,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := newFakeTx()
			svc, _ := newTestService(t, tx)

			if _, err := svc.Grant(context.Background(), tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("Grant() = %v, want %v", err, tc.want)
			}
			if len(tx.transactions) != 0 {
				t.Errorf("a refused grant still wrote %d ledger transactions", len(tx.transactions))
			}
		})
	}
}

// TestGrantIsRefusedForAnAccountThatMayNotWager proves issuance goes through the
// SAME gate a placement does.
//
// A self-excluded customer topping their balance up is the exclusion being
// worked around one step earlier, so the control has to sit on the money coming
// in and not only on the bet going out. The status is read inside the
// transaction against the locked row, exactly as [Service.Place] reads it.
func TestGrantIsRefusedForAnAccountThatMayNotWager(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		want   error
	}{
		{name: "self excluded", status: auth.UserStatusSelfExcluded.String(), want: ErrSelfExcluded},
		{name: "suspended", status: auth.UserStatusSuspended.String(), want: ErrAccountNotWagerable},
		{name: "closed", status: auth.UserStatusClosed.String(), want: ErrAccountNotWagerable},
		{name: "an unrecognised status", status: "pending_review", want: ErrAccountNotWagerable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := newFakeTx()
			tx.status = tc.status
			svc, _ := newTestService(t, tx)

			_, err := svc.Grant(context.Background(), GrantRequest{
				UserID: testUser, Amount: domain.Money(500), IdempotencyKey: testKey,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Grant() = %v, want %v", err, tc.want)
			}
			if len(tx.transactions) != 0 {
				t.Errorf("a blocked account was still credited: %d transactions written",
					len(tx.transactions))
			}
		})
	}
}

// TestGrantIsBoundedByTheCustomersOwnGrantLimit is what makes the 'grant' limit
// kind mean something.
//
// auth.LimitKindGrant, user_limits' CHECK and limits.go's grantEntryKinds all
// carry the kind end to end, and before this path existed nothing could ever
// breach it — the limit was unreachable by construction. The boundary asserted
// here is limits.go's stated one: a request that lands EXACTLY on the limit is
// accepted, and one that goes past it is refused.
func TestGrantIsBoundedByTheCustomersOwnGrantLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		used    domain.Money
		request domain.Money
		refused bool
	}{
		{name: "well under", used: 0, request: 400, refused: false},
		{name: "exactly at the limit", used: 600, request: 400, refused: false},
		{name: "one minor unit past", used: 600, request: 401, refused: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tx := newFakeTx()
			tx.limits = []Limit{{
				Kind:          auth.LimitKindGrant,
				Period:        auth.LimitPeriodDay,
				Amount:        domain.Money(1_000),
				EffectiveFrom: testNow.Add(-time.Hour),
			}}
			tx.sums[domain.EntryKindGrant] = tc.used

			svc, _ := newTestService(t, tx)
			_, err := svc.Grant(context.Background(), GrantRequest{
				UserID: testUser, Amount: tc.request, IdempotencyKey: testKey,
			})

			if tc.refused {
				if !errors.Is(err, ErrLimitExceeded) {
					t.Fatalf("Grant() = %v, want ErrLimitExceeded", err)
				}
				if len(tx.transactions) != 0 {
					t.Errorf("a grant over the customer's own limit was still written")
				}
				return
			}
			if err != nil {
				t.Fatalf("Grant() = %v, want the grant to be accepted", err)
			}
		})
	}
}

// TestAStakeLimitDoesNotBlockAGrant is the other half of the limit rule, and it
// is a real hazard rather than a hypothetical one.
//
// evaluateLimitsWith is shared by placement and issuance, and the ONLY thing
// separating them is which kinds the movement contributes to. If a grant were
// fed through requestedForLimit — the placement's mapping — it would count
// against the STAKE and LOSS limits, and a customer who had set a modest stake
// limit would find they could no longer top up at all.
func TestAStakeLimitDoesNotBlockAGrant(t *testing.T) {
	t.Parallel()

	tx := newFakeTx()
	tx.limits = []Limit{
		{
			Kind:          auth.LimitKindStake,
			Period:        auth.LimitPeriodDay,
			Amount:        domain.Money(100),
			EffectiveFrom: testNow.Add(-time.Hour),
		},
		{
			Kind:          auth.LimitKindLoss,
			Period:        auth.LimitPeriodDay,
			Amount:        domain.Money(100),
			EffectiveFrom: testNow.Add(-time.Hour),
		},
	}

	svc, _ := newTestService(t, tx)
	if _, err := svc.Grant(context.Background(), GrantRequest{
		UserID: testUser, Amount: domain.Money(50_000), IdempotencyKey: testKey,
	}); err != nil {
		t.Fatalf("a stake or loss limit refused a grant: %v -- topping up is neither "+
			"staking nor losing, and counting it as either locks the customer out", err)
	}
	if len(tx.transactions) != 1 {
		t.Errorf("wrote %d transactions, want 1", len(tx.transactions))
	}
}

// TestGrantIDsAreDistinctPerKeyAndUser guards the derivation the whole guarantee
// rests on.
//
// Two different keys must not collide (a customer's second top-up would be
// swallowed as a replay) and two different customers sharing a key must not
// collide (one would be handed the other's movement).
func TestGrantIDsAreDistinctPerKeyAndUser(t *testing.T) {
	t.Parallel()

	base, err := DeriveGrantTransactionID(testUser, testKey)
	if err != nil {
		t.Fatalf("DeriveGrantTransactionID: %v", err)
	}
	again, err := DeriveGrantTransactionID(testUser, testKey)
	if err != nil {
		t.Fatalf("DeriveGrantTransactionID: %v", err)
	}
	if base != again {
		t.Fatal("the derivation is not deterministic, so a replay would never collide")
	}

	otherKey, err := DeriveGrantTransactionID(testUser, testKey+"-2")
	if err != nil {
		t.Fatalf("DeriveGrantTransactionID: %v", err)
	}
	if otherKey == base {
		t.Error("two different keys derived one id: a customer's second top-up would be " +
			"swallowed as a replay")
	}

	otherUser, err := DeriveGrantTransactionID(domain.UserID("user-2"), testKey)
	if err != nil {
		t.Fatalf("DeriveGrantTransactionID: %v", err)
	}
	if otherUser == base {
		t.Error("two customers sharing a key derived one id: one would be handed the " +
			"other's movement")
	}

	// A grant and a wager movement under the same key must not collide either;
	// the field tag is what separates them.
	wager, err := DeriveWagerID(testUser, testKey, 0)
	if err != nil {
		t.Fatalf("DeriveWagerID: %v", err)
	}
	stake, err := DeriveTransactionID(wager, domain.EntryKindStake)
	if err != nil {
		t.Fatalf("DeriveTransactionID: %v", err)
	}
	if stake == base {
		t.Error("a grant and a stake movement derived the same transaction id")
	}
}

// TestDeriveGrantTransactionIDRefusesBadInput mirrors the other derivations:
// an empty user or key must fail rather than produce an id every caller with an
// empty key also gets.
func TestDeriveGrantTransactionIDRefusesBadInput(t *testing.T) {
	t.Parallel()

	if _, err := DeriveGrantTransactionID("", testKey); !errors.Is(err, domain.ErrEmptyID) {
		t.Errorf("empty user = %v, want ErrEmptyID", err)
	}
	if _, err := DeriveGrantTransactionID(testUser, "  "); !errors.Is(err, ErrIdempotencyKeyRequired) {
		t.Errorf("blank key = %v, want ErrIdempotencyKeyRequired", err)
	}
}
