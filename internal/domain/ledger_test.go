package domain

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// mustAccount builds an account or fails the test.
func mustAccount(t *testing.T, kind AccountKind, owner UserID) Account {
	t.Helper()
	a, err := NewAccount(kind, owner)
	if err != nil {
		t.Fatalf("NewAccount(%v, %q): %v", kind, owner, err)
	}
	return a
}

// mustEntry builds a ledger entry or fails the test.
func mustEntry(t *testing.T, account Account, amount Money, kind EntryKind) LedgerEntry {
	t.Helper()
	e, err := NewLedgerEntry(account, amount, kind)
	if err != nil {
		t.Fatalf("NewLedgerEntry(%v, %s, %v): %v", account, amount, kind, err)
	}
	return e
}

// mustNeg negates an amount or fails the test.
func mustNeg(t *testing.T, m Money) Money {
	t.Helper()
	n, err := m.Neg()
	if err != nil {
		t.Fatalf("Neg(%s): %v", m, err)
	}
	return n
}

// settledStraight builds a straight wager and drives it to a terminal state,
// which is the precondition NewSettlementTransaction requires.
func settledStraight(t *testing.T, id, user, stake string, decimal float64, outcome WagerStatus, returned Money) Wager {
	t.Helper()
	leg := mustLeg(t, betLegSpec{
		legID: id + "-leg", eventID: "evt-" + id, marketID: "mkt-" + id,
		typ: MarketTypeMoneyline, line: NoLine(), role: SelectionRoleHome, decimal: decimal,
	})
	w := mustWager(t, WagerParams{
		ID: WagerID(id), UserID: UserID(user), Kind: WagerKindStraight,
		Legs: []Leg{leg}, Stake: mustMoney(t, stake), AcceptedDecimal: decimal,
		Rounding: RoundHalfAwayFromZero, PlacedAt: ts(0),
	})
	if outcome == WagerStatusCashedOut {
		out, err := w.CashOut(returned, ts(time.Hour))
		if err != nil {
			t.Fatalf("CashOut: %v", err)
		}
		return out
	}
	settled, err := w.Settle(outcome, returned, ts(time.Hour))
	if err != nil {
		t.Fatalf("Settle(%v, %s): %v", outcome, returned, err)
	}
	return settled
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

func TestAccountKindTextRoundTrip(t *testing.T) {
	cases := []struct {
		kind AccountKind
		text string
		user bool
	}{
		{AccountKindUserCash, "user_cash", true},
		{AccountKindUserEscrow, "user_escrow", true},
		{AccountKindHouse, "house", false},
		{AccountKindIssuance, "issuance", false},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := c.kind.String(); got != c.text {
				t.Errorf("String() = %q, want %q", got, c.text)
			}
			parsed, err := ParseAccountKind(c.text)
			if err != nil {
				t.Fatalf("ParseAccountKind: %v", err)
			}
			if parsed != c.kind {
				t.Errorf("ParseAccountKind(%q) = %v", c.text, parsed)
			}
			b, err := c.kind.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			var back AccountKind
			if err := back.UnmarshalText(b); err != nil {
				t.Fatalf("UnmarshalText: %v", err)
			}
			if back != c.kind {
				t.Errorf("round trip = %v", back)
			}
			if c.kind.IsUserOwned() != c.user {
				t.Errorf("IsUserOwned() = %v, want %v", c.kind.IsUserOwned(), c.user)
			}
			if c.kind.IsSystem() == c.user {
				t.Errorf("IsSystem() = %v, want %v", c.kind.IsSystem(), !c.user)
			}
		})
	}
	if _, err := ParseAccountKind("cash"); !errors.Is(err, ErrUnknownAccountKind) {
		t.Errorf("ParseAccountKind of an undefined kind: %v", err)
	}
	if _, err := AccountKindUnknown.MarshalText(); !errors.Is(err, ErrUnknownAccountKind) {
		t.Errorf("MarshalText of the zero value: %v", err)
	}
}

func TestAccountConstruction(t *testing.T) {
	cases := []struct {
		name  string
		kind  AccountKind
		owner UserID
		want  error
	}{
		{"user cash needs an owner", AccountKindUserCash, "", ErrAccountOwnerRequired},
		{"user escrow needs an owner", AccountKindUserEscrow, "", ErrAccountOwnerRequired},
		{"the house takes no owner", AccountKindHouse, "usr-1", ErrAccountOwnerNotApplicable},
		{"issuance takes no owner", AccountKindIssuance, "usr-1", ErrAccountOwnerNotApplicable},
		{"the zero kind is refused", AccountKindUnknown, "usr-1", ErrUnknownAccountKind},
		{"a malformed owner is refused", AccountKindUserCash, "usr:1", ErrAccountOwnerRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewAccount(c.kind, c.owner)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewAccount: %v, want %v", err, c.want)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%v does not reach ErrInvalid", err)
			}
		})
	}

	t.Run("identity is the kind and owner pair", func(t *testing.T) {
		a := mustAccount(t, AccountKindUserCash, "usr-1")
		b := mustAccount(t, AccountKindUserCash, "usr-1")
		if a != b {
			t.Error("two accounts for the same user and kind are not equal")
		}
		other, err := UserCashAccount("usr-2")
		if err != nil {
			t.Fatalf("UserCashAccount: %v", err)
		}
		if a == other {
			t.Error("two users share one cash account")
		}
		escrow, err := UserEscrowAccount("usr-1")
		if err != nil {
			t.Fatalf("UserEscrowAccount: %v", err)
		}
		if a == escrow {
			t.Error("a user's cash and escrow are the same account")
		}
		if HouseAccount() == IssuanceAccount() {
			t.Error("the house and issuance are the same account")
		}

		owner, ok := a.Owner()
		if !ok || owner != "usr-1" {
			t.Errorf("Owner() = %q, %v", owner, ok)
		}
		if _, ok := HouseAccount().Owner(); ok {
			t.Error("the house account reports an owner")
		}
		if a.IsZero() || !(Account{}).IsZero() {
			t.Error("IsZero does not distinguish a constructed account from the zero value")
		}
		// Comparability is what makes Balances a one-pass fold.
		seen := map[Account]int{a: 1, escrow: 2, HouseAccount(): 3}
		if len(seen) != 3 {
			t.Errorf("accounts collide as map keys: %v", seen)
		}
		if s := (Account{}).String(); s != "account(<zero>)" {
			t.Errorf("zero Account String() = %q", s)
		}
		if s := a.String(); s == "account(<zero>)" {
			t.Errorf("constructed Account String() = %q", s)
		}
		if s := HouseAccount().String(); s != "account(house)" {
			t.Errorf("house String() = %q", s)
		}
	})
}

// ---------------------------------------------------------------------------
// Entries and transactions
// ---------------------------------------------------------------------------

func TestEntryKindTextRoundTrip(t *testing.T) {
	cases := []struct {
		kind EntryKind
		text string
	}{
		{EntryKindGrant, "grant"},
		{EntryKindStake, "stake"},
		{EntryKindPayout, "payout"},
		{EntryKindLoss, "loss"},
		{EntryKindRefund, "refund"},
		{EntryKindCashOut, "cash_out"},
		{EntryKindAdjustment, "adjustment"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := c.kind.String(); got != c.text {
				t.Errorf("String() = %q, want %q", got, c.text)
			}
			parsed, err := ParseEntryKind(c.text)
			if err != nil {
				t.Fatalf("ParseEntryKind: %v", err)
			}
			if parsed != c.kind {
				t.Errorf("ParseEntryKind(%q) = %v", c.text, parsed)
			}
			b, err := c.kind.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			var back EntryKind
			if err := back.UnmarshalText(b); err != nil {
				t.Fatalf("UnmarshalText: %v", err)
			}
			if back != c.kind {
				t.Errorf("round trip = %v", back)
			}
		})
	}
	if _, err := ParseEntryKind("void"); !errors.Is(err, ErrUnknownEntryKind) {
		t.Errorf("ParseEntryKind of a wager status: %v", err)
	}
	if _, err := EntryKindUnknown.MarshalText(); !errors.Is(err, ErrUnknownEntryKind) {
		t.Errorf("MarshalText of the zero value: %v", err)
	}
}

func TestNewLedgerEntry(t *testing.T) {
	cash := mustAccount(t, AccountKindUserCash, "usr-1")

	if _, err := NewLedgerEntry(Account{}, mustMoney(t, "1.00"), EntryKindGrant); !errors.Is(err, ErrUnknownAccountKind) {
		t.Errorf("entry on the zero account: %v", err)
	}
	if _, err := NewLedgerEntry(cash, mustMoney(t, "1.00"), EntryKindUnknown); !errors.Is(err, ErrUnknownEntryKind) {
		t.Errorf("entry with the zero kind: %v", err)
	}
	if _, err := NewLedgerEntry(cash, ZeroMoney, EntryKindGrant); !errors.Is(err, ErrZeroEntryAmount) {
		t.Errorf("entry moving nothing: %v", err)
	}
	if _, err := NewLedgerEntry(cash, MaxSafeMoney+1, EntryKindGrant); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("entry past the safe range: %v", err)
	}

	e := mustEntry(t, cash, mustMoney(t, "-12.34"), EntryKindStake)
	if e.Account() != cash {
		t.Errorf("Account() = %v", e.Account())
	}
	if e.Amount().Compare(mustMoney(t, "-12.34")) != 0 {
		t.Errorf("Amount() = %s", e.Amount())
	}
	if e.Kind() != EntryKindStake {
		t.Errorf("Kind() = %v", e.Kind())
	}
	if e.IsZero() || !(LedgerEntry{}).IsZero() {
		t.Error("IsZero does not distinguish a constructed entry from the zero value")
	}
	if s := (LedgerEntry{}).String(); s != "entry(<zero>)" {
		t.Errorf("zero entry String() = %q", s)
	}
	if s := e.String(); s == "entry(<zero>)" {
		t.Errorf("constructed entry String() = %q", s)
	}
}

// TestATransactionCannotBeConstructedUnbalanced is the structural claim the
// whole ledger rests on: there is no expression in the program that produces an
// unbalanced Transaction value.
func TestATransactionCannotBeConstructedUnbalanced(t *testing.T) {
	cash := mustAccount(t, AccountKindUserCash, "usr-1")
	escrow := mustAccount(t, AccountKindUserEscrow, "usr-1")
	ten := mustMoney(t, "10.00")

	cases := []struct {
		name   string
		params TransactionParams
		want   error
	}{
		{
			name: "entries that do not sum to zero",
			params: TransactionParams{
				ID: "txn-1", Kind: EntryKindStake, OccurredAt: ts(0),
				Entries: []LedgerEntry{
					mustEntry(t, cash, mustNeg(t, ten), EntryKindStake),
					mustEntry(t, escrow, mustMoney(t, "9.99"), EntryKindStake),
				},
			},
			want: ErrUnbalancedTransaction,
		},
		{
			name: "a single entry",
			params: TransactionParams{
				ID: "txn-2", Kind: EntryKindStake, OccurredAt: ts(0),
				Entries: []LedgerEntry{mustEntry(t, cash, ten, EntryKindStake)},
			},
			want: ErrTooFewEntries,
		},
		{
			name: "no entries at all",
			params: TransactionParams{
				ID: "txn-3", Kind: EntryKindStake, OccurredAt: ts(0),
			},
			want: ErrTooFewEntries,
		},
		{
			name: "entries of different kinds",
			params: TransactionParams{
				ID: "txn-4", Kind: EntryKindStake, OccurredAt: ts(0),
				Entries: []LedgerEntry{
					mustEntry(t, cash, mustNeg(t, ten), EntryKindStake),
					mustEntry(t, escrow, ten, EntryKindRefund),
				},
			},
			want: ErrMixedEntryKinds,
		},
		{
			name: "an unconstructed entry",
			params: TransactionParams{
				ID: "txn-5", Kind: EntryKindStake, OccurredAt: ts(0),
				Entries: []LedgerEntry{
					mustEntry(t, cash, mustNeg(t, ten), EntryKindStake),
					{},
				},
			},
			want: ErrUnknownAccountKind,
		},
		{
			name: "an empty identifier",
			params: TransactionParams{
				ID: "", Kind: EntryKindStake, OccurredAt: ts(0),
				Entries: []LedgerEntry{
					mustEntry(t, cash, mustNeg(t, ten), EntryKindStake),
					mustEntry(t, escrow, ten, EntryKindStake),
				},
			},
			want: ErrEmptyID,
		},
		{
			name: "the zero kind",
			params: TransactionParams{
				ID: "txn-6", Kind: EntryKindUnknown, OccurredAt: ts(0),
				Entries: []LedgerEntry{
					mustEntry(t, cash, mustNeg(t, ten), EntryKindStake),
					mustEntry(t, escrow, ten, EntryKindStake),
				},
			},
			want: ErrUnknownEntryKind,
		},
		{
			name: "the zero time",
			params: TransactionParams{
				ID: "txn-7", Kind: EntryKindStake,
				Entries: []LedgerEntry{
					mustEntry(t, cash, mustNeg(t, ten), EntryKindStake),
					mustEntry(t, escrow, ten, EntryKindStake),
				},
			},
			want: ErrZeroTime,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewTransaction(c.params)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewTransaction: %v, want %v", err, c.want)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%v does not reach ErrInvalid", err)
			}
		})
	}
}

func TestTransactionAccessors(t *testing.T) {
	cash := mustAccount(t, AccountKindUserCash, "usr-1")
	escrow := mustAccount(t, AccountKindUserEscrow, "usr-1")
	ten := mustMoney(t, "10.00")

	txn, err := NewTransaction(TransactionParams{
		ID:   "txn-1",
		Kind: EntryKindStake,
		Entries: []LedgerEntry{
			mustEntry(t, cash, mustNeg(t, ten), EntryKindStake),
			mustEntry(t, escrow, ten, EntryKindStake),
		},
		WagerID:    "wgr-1",
		OccurredAt: ts(time.Minute),
	})
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}

	if txn.ID() != "txn-1" || txn.Kind() != EntryKindStake || txn.EntryCount() != 2 {
		t.Errorf("accessors: %v %v %d", txn.ID(), txn.Kind(), txn.EntryCount())
	}
	if id, ok := txn.WagerID(); !ok || id != "wgr-1" {
		t.Errorf("WagerID() = %q, %v", id, ok)
	}
	if !txn.OccurredAt().Equal(ts(time.Minute)) {
		t.Errorf("OccurredAt() = %s", txn.OccurredAt())
	}
	if txn.IsZero() || !(Transaction{}).IsZero() {
		t.Error("IsZero does not distinguish a constructed transaction from the zero value")
	}
	if s := (Transaction{}).String(); s != "txn(<zero>)" {
		t.Errorf("zero Transaction String() = %q", s)
	}
	if s := txn.String(); s == "txn(<zero>)" {
		t.Errorf("constructed Transaction String() = %q", s)
	}

	// Entries are copied, so a caller cannot append an unbalanced row into a
	// value whose whole guarantee is that it balances.
	entries := txn.Entries()
	entries[0] = mustEntry(t, cash, ten, EntryKindStake)
	if txn.Entries()[0].Amount().Compare(mustNeg(t, ten)) != 0 {
		t.Error("mutating the slice returned by Entries() reached the transaction")
	}

	net, err := txn.NetFor(cash)
	if err != nil {
		t.Fatalf("NetFor: %v", err)
	}
	if net.Compare(mustNeg(t, ten)) != 0 {
		t.Errorf("NetFor(cash) = %s, want -10.00", net)
	}
	untouched, err := txn.NetFor(HouseAccount())
	if err != nil {
		t.Fatalf("NetFor: %v", err)
	}
	if !untouched.IsZero() {
		t.Errorf("NetFor(house) = %s, want 0.00", untouched)
	}

	// A transaction with no wager reference says so.
	grant, err := NewGrantTransaction("txn-grant", "usr-1", ten, ts(0))
	if err != nil {
		t.Fatalf("NewGrantTransaction: %v", err)
	}
	if _, ok := grant.WagerID(); ok {
		t.Error("a grant claims a wager reference")
	}
}

// ---------------------------------------------------------------------------
// Balances are derived
// ---------------------------------------------------------------------------

// TestBalanceIsAPureFold checks the property that "derived, never stored"
// actually buys: the answer depends only on the SET of transactions, not on the
// order they are folded in, and not on any state carried on the account.
func TestBalanceIsAPureFold(t *testing.T) {
	cash, err := UserCashAccount("usr-1")
	if err != nil {
		t.Fatalf("UserCashAccount: %v", err)
	}

	txns := buildNarrativeLedger(t)

	first, err := Balance(cash, txns...)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}

	shuffled := slices.Clone(txns)
	rng := rand.New(rand.NewPCG(7, 11))
	for round := range 20 {
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		got, err := Balance(cash, shuffled...)
		if err != nil {
			t.Fatalf("Balance on shuffle %d: %v", round, err)
		}
		if got.Compare(first) != 0 {
			t.Fatalf("shuffle %d gives %s, want %s — the fold is order dependent", round, got, first)
		}
	}

	// Reading the balance twice cannot change it, and reading it does not
	// depend on anything the Account carries.
	again, err := Balance(cash, txns...)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if again.Compare(first) != 0 {
		t.Errorf("a second read gives %s, want %s", again, first)
	}

	if _, err := Balance(Account{}, txns...); !errors.Is(err, ErrUnknownAccountKind) {
		t.Errorf("Balance on the zero account: %v", err)
	}
	empty, err := Balance(cash)
	if err != nil {
		t.Fatalf("Balance over no transactions: %v", err)
	}
	if !empty.IsZero() {
		t.Errorf("an empty ledger gives %s, want 0.00", empty)
	}
}

// buildNarrativeLedger walks one customer through a grant and four wagers with
// every terminal outcome, and returns the transactions in the order they
// happened. The amounts are the ones the wager tests already pinned: $100 at
// -110 returns $190.91, $50 at +150 returns $125.00.
func buildNarrativeLedger(t *testing.T) []Transaction {
	t.Helper()

	grant, err := NewGrantTransaction("txn-grant", "usr-1", mustMoney(t, "1000.00"), ts(0))
	if err != nil {
		t.Fatalf("NewGrantTransaction: %v", err)
	}
	txns := []Transaction{grant}

	type step struct {
		id       string
		stake    string
		decimal  float64
		outcome  WagerStatus
		returned string
	}
	steps := []step{
		{"wgr-won", "100.00", priceMinus110, WagerStatusWon, "190.91"},
		{"wgr-lost", "50.00", pricePlus150, WagerStatusLost, "0.00"},
		{"wgr-push", "20.00", priceMinus110, WagerStatusPush, "20.00"},
		{"wgr-cash", "40.00", pricePlus150, WagerStatusCashedOut, "55.00"},
	}
	for i, s := range steps {
		w := settledStraight(t, s.id, "usr-1", s.stake, s.decimal, s.outcome, mustMoney(t, s.returned))
		stake, err := NewStakeTransaction(TransactionID("txn-stake-"+strconv.Itoa(i)), w, ts(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("NewStakeTransaction: %v", err)
		}
		settle, err := NewSettlementTransaction(TransactionID("txn-settle-"+strconv.Itoa(i)), w, ts(time.Hour))
		if err != nil {
			t.Fatalf("NewSettlementTransaction: %v", err)
		}
		txns = append(txns, stake, settle)
	}

	// An operator correction: the house credits the customer $5.
	adjust, err := NewAdjustmentTransaction("txn-adjust", HouseAccount(),
		mustAccount(t, AccountKindUserCash, "usr-1"), mustMoney(t, "5.00"), "wgr-lost", ts(2*time.Hour))
	if err != nil {
		t.Fatalf("NewAdjustmentTransaction: %v", err)
	}
	return append(txns, adjust)
}

// TestNarrativeLedgerBalances checks every account against a hand-computed
// figure, so the movements are pinned by arithmetic done outside the code under
// test.
//
//	grant                        cash +1000.00   issuance -1000.00
//	won   stake 100 → ret 190.91 cash  -100.00 +190.91   house -90.91
//	lost  stake  50 → ret   0.00 cash   -50.00           house +50.00
//	push  stake  20 → ret  20.00 cash   -20.00  +20.00   house   0.00
//	cash  stake  40 → ret  55.00 cash   -40.00  +55.00   house -15.00
//	adjustment                   cash    +5.00           house  -5.00
//
//	cash     = 1000 - 100 + 190.91 - 50 - 20 + 20 - 40 + 55 + 5 = 1060.91
//	escrow   = 0 (every wager settled)
//	house    = -90.91 + 50 + 0 - 15 - 5 = -60.91
//	issuance = -1000.00
//	total    = 1060.91 + 0 - 60.91 - 1000 = 0
func TestNarrativeLedgerBalances(t *testing.T) {
	txns := buildNarrativeLedger(t)

	want := map[Account]string{
		mustAccount(t, AccountKindUserCash, "usr-1"):   "1060.91",
		mustAccount(t, AccountKindUserEscrow, "usr-1"): "0.00",
		HouseAccount():    "-60.91",
		IssuanceAccount(): "-1000.00",
	}

	balances, err := Balances(txns...)
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	for account, expected := range want {
		got, ok := balances[account]
		if !ok {
			t.Fatalf("%v is missing from Balances", account)
		}
		if got.Compare(mustMoney(t, expected)) != 0 {
			t.Errorf("%v = %s, want %s", account, got, expected)
		}
		single, err := Balance(account, txns...)
		if err != nil {
			t.Fatalf("Balance(%v): %v", account, err)
		}
		if single.Compare(got) != 0 {
			t.Errorf("Balance(%v) = %s but Balances says %s", account, single, got)
		}
	}

	if err := LedgerIsBalanced(txns...); err != nil {
		t.Fatalf("LedgerIsBalanced: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Settlement shapes
// ---------------------------------------------------------------------------

// TestSettlementTransactionShapes pins the entry set each outcome produces,
// including the zero-entry drop that makes a push two rows rather than three.
func TestSettlementTransactionShapes(t *testing.T) {
	cash := mustAccount(t, AccountKindUserCash, "usr-1")
	escrow := mustAccount(t, AccountKindUserEscrow, "usr-1")

	cases := []struct {
		name      string
		outcome   WagerStatus
		stake     string
		decimal   float64
		returned  string
		kind      EntryKind
		entries   int
		wantCash  string
		wantHold  string
		wantHouse string
	}{
		{"a winner", WagerStatusWon, "100.00", priceMinus110, "190.91", EntryKindPayout, 3, "190.91", "-100.00", "-90.91"},
		{"a loser", WagerStatusLost, "100.00", priceMinus110, "0.00", EntryKindLoss, 2, "0.00", "-100.00", "100.00"},
		{"a push", WagerStatusPush, "100.00", priceMinus110, "100.00", EntryKindRefund, 2, "100.00", "-100.00", "0.00"},
		{"a void", WagerStatusVoid, "100.00", priceMinus110, "100.00", EntryKindRefund, 2, "100.00", "-100.00", "0.00"},
		{"a cash out above the stake", WagerStatusCashedOut, "100.00", pricePlus150, "120.00", EntryKindCashOut, 3, "120.00", "-100.00", "-20.00"},
		{"a cash out below the stake", WagerStatusCashedOut, "100.00", pricePlus150, "60.00", EntryKindCashOut, 3, "60.00", "-100.00", "40.00"},
		{"a cash out at exactly the stake", WagerStatusCashedOut, "100.00", pricePlus150, "100.00", EntryKindCashOut, 2, "100.00", "-100.00", "0.00"},
		{"a partially voided winner", WagerStatusWon, "100.00", pricePlus150, "150.00", EntryKindPayout, 3, "150.00", "-100.00", "-50.00"},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := settledStraight(t, "wgr-"+strconv.Itoa(i), "usr-1", c.stake, c.decimal, c.outcome, mustMoney(t, c.returned))
			txn, err := NewSettlementTransaction(TransactionID("txn-"+strconv.Itoa(i)), w, ts(2*time.Hour))
			if err != nil {
				t.Fatalf("NewSettlementTransaction: %v", err)
			}
			if txn.Kind() != c.kind {
				t.Errorf("Kind() = %v, want %v", txn.Kind(), c.kind)
			}
			if txn.EntryCount() != c.entries {
				t.Errorf("EntryCount() = %d, want %d: %v", txn.EntryCount(), c.entries, txn.Entries())
			}
			for _, e := range txn.Entries() {
				if e.Amount().IsZero() {
					t.Errorf("a zero-amount entry survived: %v", e)
				}
				if e.Kind() != c.kind {
					t.Errorf("entry kind = %v, want %v", e.Kind(), c.kind)
				}
			}

			for _, pair := range []struct {
				account Account
				want    string
			}{{cash, c.wantCash}, {escrow, c.wantHold}, {HouseAccount(), c.wantHouse}} {
				got, err := txn.NetFor(pair.account)
				if err != nil {
					t.Fatalf("NetFor: %v", err)
				}
				if got.Compare(mustMoney(t, pair.want)) != 0 {
					t.Errorf("NetFor(%v) = %s, want %s", pair.account, got, pair.want)
				}
			}

			if err := LedgerIsBalanced(txn); err != nil {
				t.Errorf("LedgerIsBalanced: %v", err)
			}
		})
	}
}

func TestBuildersRejectBadInput(t *testing.T) {
	cash := mustAccount(t, AccountKindUserCash, "usr-1")

	t.Run("a grant must be positive", func(t *testing.T) {
		for _, amount := range []Money{ZeroMoney, mustMoney(t, "-1.00")} {
			if _, err := NewGrantTransaction("txn-1", "usr-1", amount, ts(0)); !errors.Is(err, ErrAmountNotPositive) {
				t.Errorf("NewGrantTransaction(%s): %v, want ErrAmountNotPositive", amount, err)
			}
		}
	})

	t.Run("a grant needs a real user", func(t *testing.T) {
		if _, err := NewGrantTransaction("txn-1", "", mustMoney(t, "10.00"), ts(0)); !errors.Is(err, ErrAccountOwnerRequired) {
			t.Errorf("NewGrantTransaction: %v", err)
		}
	})

	t.Run("an adjustment needs two distinct accounts", func(t *testing.T) {
		if _, err := NewAdjustmentTransaction("txn-1", cash, cash, mustMoney(t, "1.00"), "", ts(0)); !errors.Is(err, ErrSameAccountTransfer) {
			t.Errorf("NewAdjustmentTransaction to itself: %v", err)
		}
		if _, err := NewAdjustmentTransaction("txn-1", Account{}, cash, mustMoney(t, "1.00"), "", ts(0)); !errors.Is(err, ErrUnknownAccountKind) {
			t.Errorf("NewAdjustmentTransaction from the zero account: %v", err)
		}
		if _, err := NewAdjustmentTransaction("txn-1", HouseAccount(), cash, ZeroMoney, "", ts(0)); !errors.Is(err, ErrAmountNotPositive) {
			t.Errorf("NewAdjustmentTransaction of nothing: %v", err)
		}
	})

	t.Run("a stake needs a real wager", func(t *testing.T) {
		if _, err := NewStakeTransaction("txn-1", Wager{}, ts(0)); !errors.Is(err, ErrWagerRequired) {
			t.Errorf("NewStakeTransaction of the zero wager: %v", err)
		}
	})

	t.Run("a settlement needs a settled wager", func(t *testing.T) {
		w := mustWager(t, straightParams(t))
		if _, err := NewSettlementTransaction("txn-1", w, ts(time.Hour)); !errors.Is(err, ErrWagerNotSettled) {
			t.Fatalf("NewSettlementTransaction of a placed wager: %v, want ErrWagerNotSettled", err)
		}
		open, err := w.Open(ts(time.Hour))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := NewSettlementTransaction("txn-1", open, ts(2*time.Hour)); !errors.Is(err, ErrConflict) {
			t.Errorf("NewSettlementTransaction of an open wager: %v, want an ErrConflict", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The invariant double-entry exists for
// ---------------------------------------------------------------------------

// TestEveryEntryInTheSystemSumsToExactlyZero is the phase brief's headline
// assertion: for ANY sequence of valid transactions, the sum of every entry in
// the system is EXACTLY zero.
//
// The comparison is exact integer equality, and here that is the correct test
// rather than a float-comparison mistake — [Money] is an int64 count of minor
// units precisely so that this question has an exact answer (CLAUDE.md §12).
//
// The sequence is generated from a SEEDED PRNG, so the test is deterministic
// and reproducible while still covering combinations nobody wrote down: every
// terminal outcome, winners returning anywhere between their stake and their
// maximum, cash-outs above and below the stake, and adjustments in both
// directions.
func TestEveryEntryInTheSystemSumsToExactlyZero(t *testing.T) {
	const (
		seed    = 20260816
		users   = 5
		wagers  = 400
		adjusts = 40
	)
	rng := rand.New(rand.NewPCG(seed, 0x5f3759df))

	prices := []float64{priceMinus110, priceMinus105, pricePlus150, priceMinus200, 3.75, 12.0}
	roundings := []Rounding{RoundHalfAwayFromZero, RoundHalfToEven, RoundTowardZero}

	var txns []Transaction
	// Independent shadow accounting, kept with plain int64 arithmetic so it
	// cannot share a bug with the Money type under test.
	shadow := map[Account]int64{}
	record := func(txn Transaction) {
		txns = append(txns, txn)
		for _, e := range txn.Entries() {
			shadow[e.Account()] += e.Amount().MinorUnits()
		}
	}

	userIDs := make([]UserID, 0, users)
	for u := range users {
		id := UserID("usr-" + strconv.Itoa(u))
		userIDs = append(userIDs, id)
		grant, err := NewGrantTransaction(
			TransactionID("txn-grant-"+strconv.Itoa(u)), id,
			mustMoney(t, "1000.00"), ts(0))
		if err != nil {
			t.Fatalf("NewGrantTransaction: %v", err)
		}
		record(grant)
	}

	for i := range wagers {
		id := strconv.Itoa(i)
		user := userIDs[rng.IntN(users)]
		decimal := prices[rng.IntN(len(prices))]
		stakeMinor := int64(1 + rng.IntN(50_000)) // 0.01 to 500.00
		stake, err := FromMinorUnits(stakeMinor)
		if err != nil {
			t.Fatalf("FromMinorUnits: %v", err)
		}

		leg := mustLeg(t, betLegSpec{
			legID: "leg-" + id, eventID: "evt-" + id, marketID: "mkt-" + id,
			typ: MarketTypeMoneyline, line: NoLine(), role: SelectionRoleHome, decimal: decimal,
		})
		w, err := NewWager(WagerParams{
			ID: WagerID("wgr-" + id), UserID: user, Kind: WagerKindStraight,
			Legs: []Leg{leg}, Stake: stake, AcceptedDecimal: decimal,
			Rounding: roundings[rng.IntN(len(roundings))], PlacedAt: ts(0),
		})
		if err != nil {
			t.Fatalf("NewWager: %v", err)
		}

		stakeTxn, err := NewStakeTransaction(TransactionID("txn-stake-"+id), w, ts(time.Minute))
		if err != nil {
			t.Fatalf("NewStakeTransaction: %v", err)
		}
		record(stakeTxn)

		// Half the tickets pass through open first; the machine permits both.
		if rng.IntN(2) == 0 {
			w, err = w.Open(ts(30 * time.Minute))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
		}

		stakeUnits := w.Stake().MinorUnits()
		payoutUnits := w.PotentialPayout().MinorUnits()
		var settled Wager
		switch rng.IntN(5) {
		case 0: // Won, returning anywhere from the stake to the maximum.
			returned, ferr := FromMinorUnits(stakeUnits + rng.Int64N(payoutUnits-stakeUnits+1))
			if ferr != nil {
				t.Fatalf("FromMinorUnits: %v", ferr)
			}
			settled, err = w.Settle(WagerStatusWon, returned, ts(time.Hour))
			if err != nil {
				t.Fatalf("Settle(won, %s) on a %s stake with a %s maximum: %v",
					returned, w.Stake(), w.PotentialPayout(), err)
			}
		case 1:
			settled, err = w.Settle(WagerStatusLost, ZeroMoney, ts(time.Hour))
		case 2:
			settled, err = w.Settle(WagerStatusVoid, w.Stake(), ts(time.Hour))
		case 3:
			settled, err = w.Settle(WagerStatusPush, w.Stake(), ts(time.Hour))
		default: // Cashed out anywhere in (0, maximum].
			amount, ferr := FromMinorUnits(1 + rng.Int64N(payoutUnits))
			if ferr != nil {
				t.Fatalf("FromMinorUnits: %v", ferr)
			}
			settled, err = w.CashOut(amount, ts(time.Hour))
		}
		if err != nil {
			t.Fatalf("settling wager %s: %v", w.ID(), err)
		}

		settleTxn, err := NewSettlementTransaction(TransactionID("txn-settle-"+id), settled, ts(2*time.Hour))
		if err != nil {
			t.Fatalf("NewSettlementTransaction: %v", err)
		}
		record(settleTxn)
	}

	for i := range adjusts {
		id := strconv.Itoa(i)
		user := userIDs[rng.IntN(users)]
		cash, err := UserCashAccount(user)
		if err != nil {
			t.Fatalf("UserCashAccount: %v", err)
		}
		amount, err := FromMinorUnits(int64(1 + rng.IntN(10_000)))
		if err != nil {
			t.Fatalf("FromMinorUnits: %v", err)
		}
		from, to := HouseAccount(), cash
		if rng.IntN(2) == 0 {
			from, to = cash, HouseAccount()
		}
		txn, err := NewAdjustmentTransaction(TransactionID("txn-adjust-"+id), from, to, amount, "", ts(3*time.Hour))
		if err != nil {
			t.Fatalf("NewAdjustmentTransaction: %v", err)
		}
		record(txn)
	}

	if len(txns) != users+2*wagers+adjusts {
		t.Fatalf("built %d transactions, want %d", len(txns), users+2*wagers+adjusts)
	}

	// 1. The package's own audit hook.
	if err := LedgerIsBalanced(txns...); err != nil {
		t.Fatalf("LedgerIsBalanced over %d transactions: %v", len(txns), err)
	}

	// 2. The same claim recomputed with bare int64 arithmetic, so a bug in
	//    Money.Add cannot make both the ledger and the check agree on a wrong
	//    answer.
	var shadowTotal int64
	for _, v := range shadow {
		shadowTotal += v
	}
	if shadowTotal != 0 {
		t.Fatalf("the shadow ledger sums to %d minor units, want exactly 0", shadowTotal)
	}

	// 3. Every account balance the package derives matches the shadow.
	balances, err := Balances(txns...)
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	if len(balances) != len(shadow) {
		t.Fatalf("Balances covers %d accounts, the shadow covers %d", len(balances), len(shadow))
	}
	for account, want := range shadow {
		got, ok := balances[account]
		if !ok {
			t.Fatalf("%v is missing from Balances", account)
		}
		if got.MinorUnits() != want {
			t.Errorf("%v = %d minor units, want %d", account, got.MinorUnits(), want)
		}
	}

	// 4. Every escrow account is exactly flat, because every wager settled.
	for _, user := range userIDs {
		escrow, err := UserEscrowAccount(user)
		if err != nil {
			t.Fatalf("UserEscrowAccount: %v", err)
		}
		held, err := Balance(escrow, txns...)
		if err != nil {
			t.Fatalf("Balance: %v", err)
		}
		if !held.IsZero() {
			t.Errorf("%v holds %s after every wager settled, want 0.00", escrow, held)
		}
	}

	// 5. Issuance is the exact negative of everything created, and nothing but
	//    a grant ever touched it.
	issued, err := Balance(IssuanceAccount(), txns...)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if issued.Compare(mustMoney(t, "-5000.00")) != 0 {
		t.Errorf("issuance = %s, want -5000.00", issued)
	}
}

// TestLedgerIdentifiersAndZeroValues covers the transaction identifier
// constructor and the enum zero values.
func TestLedgerIdentifiersAndZeroValues(t *testing.T) {
	id, err := NewTransactionID("txn-1")
	if err != nil {
		t.Fatalf("NewTransactionID: %v", err)
	}
	if id.String() != "txn-1" || id.IsZero() {
		t.Errorf("NewTransactionID gave %q", id)
	}
	if !TransactionID("").IsZero() {
		t.Error("the empty TransactionID does not report IsZero")
	}
	if _, err := NewTransactionID("txn 1"); !errors.Is(err, ErrIDCharset) {
		t.Errorf("NewTransactionID with a space: %v", err)
	}

	if got := mustAccount(t, AccountKindUserEscrow, "usr-1").Kind(); got != AccountKindUserEscrow {
		t.Errorf("Kind() = %v", got)
	}
	if got := AccountKindUnknown.String(); got != "unknown" {
		t.Errorf("AccountKindUnknown.String() = %q", got)
	}
	if got := EntryKindUnknown.String(); got != "unknown" {
		t.Errorf("EntryKindUnknown.String() = %q", got)
	}
	if AccountKindUnknown.Valid() || EntryKindUnknown.Valid() {
		t.Error("a zero enum value reports as valid")
	}
	if AccountKindUnknown.IsUserOwned() || AccountKindUnknown.IsSystem() {
		t.Error("the zero AccountKind claims an ownership class")
	}

	var kind AccountKind
	if err := kind.UnmarshalText([]byte("nope")); !errors.Is(err, ErrUnknownAccountKind) {
		t.Errorf("UnmarshalText of an undefined kind: %v", err)
	}
	var entry EntryKind
	if err := entry.UnmarshalText([]byte("nope")); !errors.Is(err, ErrUnknownEntryKind) {
		t.Errorf("UnmarshalText of an undefined kind: %v", err)
	}
}

// TestLedgerDefensivePaths reaches the branches the happy paths cannot.
func TestLedgerDefensivePaths(t *testing.T) {
	t.Run("a system account builds through NewAccount too", func(t *testing.T) {
		house, err := NewAccount(AccountKindHouse, "")
		if err != nil {
			t.Fatalf("NewAccount(house): %v", err)
		}
		if house != HouseAccount() {
			t.Errorf("NewAccount(house) = %v, want %v", house, HouseAccount())
		}
		issuance, err := NewAccount(AccountKindIssuance, "")
		if err != nil {
			t.Fatalf("NewAccount(issuance): %v", err)
		}
		if issuance != IssuanceAccount() {
			t.Errorf("NewAccount(issuance) = %v, want %v", issuance, IssuanceAccount())
		}
	})

	t.Run("a malformed wager reference is refused", func(t *testing.T) {
		cash := mustAccount(t, AccountKindUserCash, "usr-1")
		escrow := mustAccount(t, AccountKindUserEscrow, "usr-1")
		ten := mustMoney(t, "10.00")
		_, err := NewTransaction(TransactionParams{
			ID: "txn-1", Kind: EntryKindStake, OccurredAt: ts(0),
			Entries: []LedgerEntry{
				mustEntry(t, cash, mustNeg(t, ten), EntryKindStake),
				mustEntry(t, escrow, ten, EntryKindStake),
			},
			WagerID: "wgr 1",
		})
		if !errors.Is(err, ErrIDCharset) {
			t.Fatalf("NewTransaction: %v, want ErrIDCharset", err)
		}
	})

	t.Run("settlementEntryKind refuses a running wager", func(t *testing.T) {
		for _, s := range []WagerStatus{WagerStatusUnknown, WagerStatusPlaced, WagerStatusOpen} {
			if _, err := settlementEntryKind(s); !errors.Is(err, ErrWagerNotSettled) {
				t.Errorf("settlementEntryKind(%v): %v, want ErrWagerNotSettled", s, err)
			}
		}
		for _, c := range []struct {
			status WagerStatus
			kind   EntryKind
		}{
			{WagerStatusWon, EntryKindPayout},
			{WagerStatusLost, EntryKindLoss},
			{WagerStatusVoid, EntryKindRefund},
			{WagerStatusPush, EntryKindRefund},
			{WagerStatusCashedOut, EntryKindCashOut},
		} {
			got, err := settlementEntryKind(c.status)
			if err != nil {
				t.Fatalf("settlementEntryKind(%v): %v", c.status, err)
			}
			if got != c.kind {
				t.Errorf("settlementEntryKind(%v) = %v, want %v", c.status, got, c.kind)
			}
		}
	})
}
