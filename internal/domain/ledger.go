package domain

import (
	"fmt"
	"slices"
	"time"
)

// TransactionID identifies a balanced ledger Transaction.
type TransactionID string

// NewTransactionID validates and returns a TransactionID.
func NewTransactionID(s string) (TransactionID, error) {
	if err := validID(s); err != nil {
		return "", idErr("transaction id", s, err)
	}
	return TransactionID(s), nil
}

// String returns the identifier as a bare string.
func (id TransactionID) String() string { return string(id) }

// IsZero reports whether the identifier is unset.
func (id TransactionID) IsZero() bool { return id == "" }

// Ledger failures.
var (
	ErrUnknownAccountKind = fmt.Errorf("%w: not a defined account kind", ErrInvalid)
	ErrUnknownEntryKind   = fmt.Errorf("%w: not a defined ledger entry kind", ErrInvalid)

	// ErrAccountOwnerRequired reports a per-user account built without a user.
	ErrAccountOwnerRequired = fmt.Errorf("%w: this account kind belongs to a user", ErrInvalid)

	// ErrAccountOwnerNotApplicable reports a system account built with a user.
	// The house and the issuance account are singletons; one per user would
	// make "what is the book's position" a sum nobody remembers to take.
	ErrAccountOwnerNotApplicable = fmt.Errorf("%w: this account kind is a system singleton and has no owner", ErrInvalid)

	// ErrUnbalancedTransaction reports entries that do not sum to exactly zero.
	// It is the sentinel that makes double-entry structural: a transaction that
	// would carry this error is never constructed, so an unbalanced pair cannot
	// exist as a value anywhere in the program.
	ErrUnbalancedTransaction = fmt.Errorf("%w: the entries of a transaction must sum to exactly zero", ErrInvalid)

	// ErrTooFewEntries reports a transaction with fewer than two entries. One
	// entry cannot balance unless it is zero, and a zero entry is not a
	// movement of money.
	ErrTooFewEntries = fmt.Errorf("%w: a balanced transaction has at least two entries", ErrInvalid)

	// ErrZeroEntryAmount reports an entry moving nothing. Zero-amount rows
	// balance trivially and would let a "transaction" be filed that moved no
	// money, which is noise in the one table that must be readable.
	ErrZeroEntryAmount = fmt.Errorf("%w: a ledger entry moves a non-zero amount", ErrInvalid)

	// ErrMixedEntryKinds reports entries of different kinds inside one
	// transaction. Both halves of a movement describe the same event, so they
	// carry the same kind; a mixed pair means two events were merged.
	ErrMixedEntryKinds = fmt.Errorf("%w: every entry in a transaction shares the transaction's kind", ErrInvalid)

	// ErrSameAccountTransfer reports an adjustment from an account to itself,
	// which nets to nothing while looking like a correction was applied.
	ErrSameAccountTransfer = fmt.Errorf("%w: a transfer needs two distinct accounts", ErrInvalid)

	// ErrWagerRequired reports a ledger movement built from the zero Wager,
	// which no constructor produces.
	ErrWagerRequired = fmt.Errorf("%w: this movement needs the wager it settles", ErrInvalid)

	// ErrAmountNotPositive reports a transfer or grant amount that is not
	// strictly positive. Direction is expressed by which account is debited,
	// never by the sign of the argument — a negative "grant" would be a
	// confiscation wearing the wrong name.
	ErrAmountNotPositive = fmt.Errorf("%w: the amount must be greater than zero", ErrInvalid)
)

// AccountKind names the four accounts the domain needs. Each one exists because
// a question the product asks cannot be answered without it.
//
//	user_cash   → "what can I bet with?"          (CLAUDE.md §6, play-money balance)
//	user_escrow → "how much do I have at risk?"   (§6, open position tracking)
//	house       → "is the book up or down?"       (§6, risk accounting)
//	issuance    → "how much play money exists?"   (the counterparty every grant needs)
//
// # Why escrow is per user and not one pooled account
//
// Because summing a pool tells you the system's exposure and nothing else. A
// per-user escrow makes a customer's exposure a single balance read, which is
// what the open-positions screen needs, and the system total is still available
// as the sum over users. The reverse is not true.
//
// # Why issuance is separate from house
//
// They measure different things and merging them destroys both. Issuance
// records money CREATED — its balance is the negative of every grant ever made,
// which is exactly "how much play money is in circulation". House records money
// WON AND LOST — its balance is the book's trading P&L. Fold them together and
// a large sign-up bonus is indistinguishable from a bad Sunday.
//
// # Why there is no bonus, promo, or fee account
//
// Because the charter describes none. CLAUDE.md §0 is explicit that no real
// money moves, no payments are processed, and no funds are held; there is no
// deposit, no withdrawal, no rake, and no promotion in the feature surface of
// §6. An account kind with no transaction that writes to it is a column that
// will be wrong the first time somebody finally uses it.
type AccountKind uint8

const (
	// AccountKindUnknown is the invalid zero value.
	AccountKindUnknown AccountKind = iota

	// AccountKindUserCash is a customer's spendable play-money balance. Owned
	// by a user.
	AccountKindUserCash

	// AccountKindUserEscrow holds the stakes of a customer's open wagers, out
	// of their spendable balance and not yet the house's. Owned by a user.
	AccountKindUserEscrow

	// AccountKindHouse is the book's trading position — it gains losing stakes
	// and funds winning payouts. A system singleton.
	AccountKindHouse

	// AccountKindIssuance is the source of play money. Every grant debits it,
	// so its balance is the negative of all currency ever created. A system
	// singleton.
	AccountKindIssuance
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (k AccountKind) String() string {
	switch k {
	case AccountKindUserCash:
		return "user_cash"
	case AccountKindUserEscrow:
		return "user_escrow"
	case AccountKindHouse:
		return "house"
	case AccountKindIssuance:
		return "issuance"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a defined kind.
func (k AccountKind) Valid() bool {
	switch k {
	case AccountKindUserCash, AccountKindUserEscrow, AccountKindHouse, AccountKindIssuance:
		return true
	default:
		return false
	}
}

// ParseAccountKind is the inverse of String for the defined kinds.
func ParseAccountKind(s string) (AccountKind, error) {
	switch s {
	case "user_cash":
		return AccountKindUserCash, nil
	case "user_escrow":
		return AccountKindUserEscrow, nil
	case "house":
		return AccountKindHouse, nil
	case "issuance":
		return AccountKindIssuance, nil
	default:
		return AccountKindUnknown, fmt.Errorf("account kind %q: %w", sample(s), ErrUnknownAccountKind)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (k AccountKind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("account kind %d: %w", uint8(k), ErrUnknownAccountKind)
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *AccountKind) UnmarshalText(b []byte) error {
	parsed, err := ParseAccountKind(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// IsUserOwned reports whether the kind belongs to a customer.
func (k AccountKind) IsUserOwned() bool {
	return k == AccountKindUserCash || k == AccountKindUserEscrow
}

// IsSystem reports whether the kind is a singleton owned by the platform.
func (k AccountKind) IsSystem() bool {
	return k == AccountKindHouse || k == AccountKindIssuance
}

// Account is a place money sits.
//
// # There is no AccountID
//
// An account IS its (kind, owner) pair, exactly as a [Price] is its
// (selection, book, instant) and for the same reason: a surrogate key would add
// a uniqueness constraint to maintain and an allocation step to run, without
// answering any question the natural key does not. It also keeps this package
// free of identifier generation, which would need either a clock or a random
// source — both of which are I/O this package does not do.
//
// The consequence is a good one: Account is a comparable value, so it is usable
// as a map key, which is what makes [Balances] a one-pass fold rather than a
// join.
//
// # Sign convention
//
// A positive amount CREDITS the account and a negative amount DEBITS it. For a
// user's cash that reads the way a customer expects — positive means they have
// more. For issuance it runs the other way and should: money created leaves
// that account, so its balance is negative and its magnitude is the currency in
// circulation. Every entry in the system summed together is exactly zero, which
// is the invariant [LedgerIsBalanced] asserts.
type Account struct {
	kind  AccountKind
	owner UserID
}

// NewAccount validates and returns an Account. Pass the zero [UserID] for a
// system account.
func NewAccount(kind AccountKind, owner UserID) (Account, error) {
	if !kind.Valid() {
		return Account{}, fmt.Errorf("account kind %d: %w", uint8(kind), ErrUnknownAccountKind)
	}
	if kind.IsUserOwned() {
		if err := validID(string(owner)); err != nil {
			return Account{}, fmt.Errorf("%s account: %w", kind, ErrAccountOwnerRequired)
		}
		return Account{kind: kind, owner: owner}, nil
	}
	if !owner.IsZero() {
		return Account{}, fmt.Errorf("%s account named owner %s: %w",
			kind, owner, ErrAccountOwnerNotApplicable)
	}
	return Account{kind: kind}, nil
}

// UserCashAccount returns the given customer's spendable-balance account.
func UserCashAccount(u UserID) (Account, error) { return NewAccount(AccountKindUserCash, u) }

// UserEscrowAccount returns the given customer's open-wager escrow account.
func UserEscrowAccount(u UserID) (Account, error) { return NewAccount(AccountKindUserEscrow, u) }

// HouseAccount returns the book's singleton trading account. It takes no
// argument and cannot fail, so call sites need no error branch for a value that
// is a constant of the system.
func HouseAccount() Account { return Account{kind: AccountKindHouse} }

// IssuanceAccount returns the singleton account play money is created from.
func IssuanceAccount() Account { return Account{kind: AccountKindIssuance} }

// Kind returns the account's kind.
func (a Account) Kind() AccountKind { return a.kind }

// Owner returns the account's owner and whether it has one.
func (a Account) Owner() (UserID, bool) { return a.owner, a.kind.IsUserOwned() }

// IsZero reports whether a is the zero Account, which no constructor produces.
func (a Account) IsZero() bool { return a.kind == AccountKindUnknown }

// String implements fmt.Stringer.
func (a Account) String() string {
	if a.IsZero() {
		return "account(<zero>)"
	}
	if a.kind.IsUserOwned() {
		return fmt.Sprintf("account(%s:%s)", a.kind, a.owner)
	}
	return fmt.Sprintf("account(%s)", a.kind)
}

// EntryKind names why money moved.
//
// The phase brief asks for "stake, payout, void/refund, adjustment" and this
// list is a superset, because that four is not sufficient to keep the ledger
// balanced. Two additions are forced:
//
//   - [EntryKindLoss]. Without it a losing wager's stake stays in escrow
//     forever and a customer's "at risk" figure never comes down.
//   - [EntryKindGrant]. A user's opening balance has to be credited FROM
//     somewhere, or the system-wide sum is not zero on the very first row.
//
// [EntryKindCashOut] is the third and is optional in principle — it moves money
// the same way a payout does — but CLAUDE.md §6 makes cash-out a headline
// feature, and folding it into payout would make "how much do customers leave
// on the table by cashing out early" unanswerable.
//
// Void and push share one kind. They are the identical movement — the stake
// goes back — and the distinction between "cancelled" and "landed on the
// number" is already recorded on the wager, where it belongs. Duplicating it
// here would be a second spelling of one fact.
type EntryKind uint8

const (
	// EntryKindUnknown is the invalid zero value.
	EntryKindUnknown EntryKind = iota

	// EntryKindGrant credits a customer with play money from issuance.
	EntryKindGrant

	// EntryKindStake moves a stake from a customer's cash into their escrow at
	// placement.
	EntryKindStake

	// EntryKindPayout releases escrow and the house's share into a customer's
	// cash on a winning wager.
	EntryKindPayout

	// EntryKindLoss releases escrow to the house on a losing wager.
	EntryKindLoss

	// EntryKindRefund returns escrow to a customer's cash on a voided or pushed
	// wager. The house takes no part.
	EntryKindRefund

	// EntryKindCashOut closes a wager early at an agreed price.
	EntryKindCashOut

	// EntryKindAdjustment is a manual correction — a re-graded result, an
	// operator fix from the admin console (CLAUDE.md §6). It is how history is
	// corrected WITHOUT rewriting it: the original entries stay on the record
	// and the adjustment sits beside them.
	EntryKindAdjustment
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (k EntryKind) String() string {
	switch k {
	case EntryKindGrant:
		return "grant"
	case EntryKindStake:
		return "stake"
	case EntryKindPayout:
		return "payout"
	case EntryKindLoss:
		return "loss"
	case EntryKindRefund:
		return "refund"
	case EntryKindCashOut:
		return "cash_out"
	case EntryKindAdjustment:
		return "adjustment"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a defined kind.
func (k EntryKind) Valid() bool {
	switch k {
	case EntryKindGrant, EntryKindStake, EntryKindPayout, EntryKindLoss,
		EntryKindRefund, EntryKindCashOut, EntryKindAdjustment:
		return true
	default:
		return false
	}
}

// ParseEntryKind is the inverse of String for the defined kinds.
func ParseEntryKind(s string) (EntryKind, error) {
	switch s {
	case "grant":
		return EntryKindGrant, nil
	case "stake":
		return EntryKindStake, nil
	case "payout":
		return EntryKindPayout, nil
	case "loss":
		return EntryKindLoss, nil
	case "refund":
		return EntryKindRefund, nil
	case "cash_out":
		return EntryKindCashOut, nil
	case "adjustment":
		return EntryKindAdjustment, nil
	default:
		return EntryKindUnknown, fmt.Errorf("ledger entry kind %q: %w", sample(s), ErrUnknownEntryKind)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (k EntryKind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("ledger entry kind %d: %w", uint8(k), ErrUnknownEntryKind)
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *EntryKind) UnmarshalText(b []byte) error {
	parsed, err := ParseEntryKind(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// LedgerEntry is one signed movement against one account.
//
// It is half of a movement, never a whole one. An entry only ever exists inside
// a [Transaction], which cannot be constructed unless its entries sum to
// exactly zero — so while a lone LedgerEntry is a legal value, an UNBALANCED
// SET of them is not, and that is the property double-entry exists for.
//
// The kind is carried on the entry as well as the transaction, and the two are
// checked to agree. That is one redundant byte per row, spent so that the
// stored rows are self-describing: "sum every stake entry this month" is a
// query against one table rather than a join, and a row that arrives without
// its transaction is still interpretable.
type LedgerEntry struct {
	account Account
	amount  Money
	kind    EntryKind
}

// NewLedgerEntry validates and returns an entry. A zero amount is refused: an
// entry that moves nothing is not a movement.
func NewLedgerEntry(account Account, amount Money, kind EntryKind) (LedgerEntry, error) {
	if account.IsZero() {
		return LedgerEntry{}, fmt.Errorf("ledger entry: %w", ErrUnknownAccountKind)
	}
	if !kind.Valid() {
		return LedgerEntry{}, fmt.Errorf("ledger entry on %s: %w", account, ErrUnknownEntryKind)
	}
	if amount.IsZero() {
		return LedgerEntry{}, fmt.Errorf("%s entry on %s: %w", kind, account, ErrZeroEntryAmount)
	}
	if amount > MaxSafeMoney || amount < MinSafeMoney {
		return LedgerEntry{}, fmt.Errorf("%s entry on %s for %d minor units: %w",
			kind, account, int64(amount), ErrMoneyOverflow)
	}
	return LedgerEntry{account: account, amount: amount, kind: kind}, nil
}

// Account returns the account the entry moves.
func (e LedgerEntry) Account() Account { return e.account }

// Amount returns the signed movement: positive credits, negative debits.
func (e LedgerEntry) Amount() Money { return e.amount }

// Kind returns why the money moved.
func (e LedgerEntry) Kind() EntryKind { return e.kind }

// IsZero reports whether e is the zero entry, which no constructor produces.
func (e LedgerEntry) IsZero() bool { return e.account.IsZero() }

// String implements fmt.Stringer.
func (e LedgerEntry) String() string {
	if e.IsZero() {
		return "entry(<zero>)"
	}
	return fmt.Sprintf("entry(%s %s %s)", e.kind, e.account, e.amount)
}

// Transaction is a set of entries that sum to exactly zero.
//
// # It cannot be constructed unbalanced
//
// [NewTransaction] sums the entries with [SumMoney] and refuses anything but
// [ZeroMoney]. The fields are unexported and there are no setters, so there is
// no expression in the program that produces an unbalanced Transaction value —
// not a struct literal, not a decode, not a later mutation. "Every stake,
// payout, void, and adjustment is two rows that sum to zero" (CLAUDE.md §4)
// is therefore enforced by the type system rather than by a validation step
// somebody has to remember to call.
//
// The sum is over [Money], which is an int64 count of minor units, so "sums to
// zero" is EXACT integer equality. This is the one place in the codebase where
// exact equality is the right test rather than a mistake — and it is exactly
// why CLAUDE.md §12 puts money in integers in the first place. A float ledger
// would need a tolerance, and a ledger with a tolerance is a ledger that can
// lose a cent per row.
type Transaction struct {
	id         TransactionID
	kind       EntryKind
	entries    []LedgerEntry
	wagerID    WagerID
	hasWager   bool
	occurredAt time.Time
}

// TransactionParams is the input to NewTransaction.
type TransactionParams struct {
	ID TransactionID

	// Kind is why the money moved. Every entry must carry the same kind.
	Kind EntryKind

	// Entries are the movements. At least two, none of them zero, summing to
	// exactly zero. The slice is copied.
	Entries []LedgerEntry

	// WagerID is the wager this movement settles, where there is one. A grant
	// and an operator adjustment may have none.
	WagerID WagerID

	// OccurredAt is when the movement happened. It is normalised to UTC.
	OccurredAt time.Time
}

// NewTransaction validates its input and returns an immutable, balanced
// Transaction.
func NewTransaction(p TransactionParams) (Transaction, error) {
	if err := validID(string(p.ID)); err != nil {
		return Transaction{}, idErr("transaction id", string(p.ID), err)
	}
	if !p.Kind.Valid() {
		return Transaction{}, fmt.Errorf("transaction %s: %w", p.ID, ErrUnknownEntryKind)
	}
	if len(p.Entries) < 2 {
		return Transaction{}, fmt.Errorf("transaction %s has %d entr(ies): %w",
			p.ID, len(p.Entries), ErrTooFewEntries)
	}

	amounts := make([]Money, 0, len(p.Entries))
	for i, e := range p.Entries {
		if e.IsZero() {
			return Transaction{}, fmt.Errorf("transaction %s entry %d: %w", p.ID, i, ErrUnknownAccountKind)
		}
		if e.Amount().IsZero() {
			return Transaction{}, fmt.Errorf("transaction %s entry %d: %w", p.ID, i, ErrZeroEntryAmount)
		}
		if e.Kind() != p.Kind {
			return Transaction{}, fmt.Errorf("transaction %s is a %s but entry %d is a %s: %w",
				p.ID, p.Kind, i, e.Kind(), ErrMixedEntryKinds)
		}
		amounts = append(amounts, e.Amount())
	}

	total, err := SumMoney(amounts...)
	if err != nil {
		return Transaction{}, fmt.Errorf("transaction %s: %w", p.ID, err)
	}
	if !total.IsZero() {
		return Transaction{}, fmt.Errorf("transaction %s entries sum to %s: %w",
			p.ID, total, ErrUnbalancedTransaction)
	}

	hasWager := !p.WagerID.IsZero()
	if hasWager {
		if err := validID(string(p.WagerID)); err != nil {
			return Transaction{}, idErr("wager id", string(p.WagerID), err)
		}
	}
	if p.OccurredAt.IsZero() {
		return Transaction{}, fmt.Errorf("transaction %s occurred at: %w", p.ID, ErrZeroTime)
	}

	return Transaction{
		id:         p.ID,
		kind:       p.Kind,
		entries:    slices.Clone(p.Entries),
		wagerID:    p.WagerID,
		hasWager:   hasWager,
		occurredAt: p.OccurredAt.UTC(),
	}, nil
}

// ID returns the transaction's identifier.
func (t Transaction) ID() TransactionID { return t.id }

// Kind returns why the money moved.
func (t Transaction) Kind() EntryKind { return t.kind }

// Entries returns a copy of the transaction's entries. The copy is what keeps a
// caller from appending an unbalanced row into a value whose whole guarantee is
// that it balances.
func (t Transaction) Entries() []LedgerEntry { return slices.Clone(t.entries) }

// EntryCount returns the number of entries without copying them.
func (t Transaction) EntryCount() int { return len(t.entries) }

// WagerID returns the wager this movement settles, and whether there is one.
func (t Transaction) WagerID() (WagerID, bool) { return t.wagerID, t.hasWager }

// OccurredAt returns when the movement happened, in UTC.
func (t Transaction) OccurredAt() time.Time { return t.occurredAt }

// IsZero reports whether t is the zero Transaction, which no constructor
// produces.
func (t Transaction) IsZero() bool { return t.id.IsZero() }

// String implements fmt.Stringer.
func (t Transaction) String() string {
	if t.IsZero() {
		return "txn(<zero>)"
	}
	return fmt.Sprintf("txn(%s %s %d entries %s)",
		t.id, t.kind, len(t.entries), t.occurredAt.Format(time.RFC3339Nano))
}

// NetFor returns the transaction's net effect on one account. Several entries
// may touch the same account in one transaction, so this sums rather than
// finds.
func (t Transaction) NetFor(account Account) (Money, error) {
	total := ZeroMoney
	for _, e := range t.entries {
		if e.Account() != account {
			continue
		}
		next, err := total.Add(e.Amount())
		if err != nil {
			return 0, fmt.Errorf("transaction %s net for %s: %w", t.id, account, err)
		}
		total = next
	}
	return total, nil
}

// Balance folds a sequence of transactions into one account's balance.
//
// CLAUDE.md §4: "Balances are derived, never stored as a mutable field." This
// function is that sentence. There is no settable balance anywhere in the
// package, no cached total on Account, and no incremental update path — a
// balance is a pure fold over entries and nothing else, so it cannot drift from
// the rows that produced it, which is the single most common defect in
// hand-rolled ledgers.
//
// The performance objection is real and is deliberately not answered here.
// Folding every entry to read one balance is O(n) in a customer's whole
// history, and the answer to that is a materialised view maintained by the
// database in phase 2 — a projection that can be dropped and rebuilt from these
// same entries. It is emphatically NOT a mutable field on this type, because
// the moment a balance is writable, "derived" becomes a comment rather than a
// property.
func Balance(account Account, txns ...Transaction) (Money, error) {
	if account.IsZero() {
		return 0, fmt.Errorf("balance: %w", ErrUnknownAccountKind)
	}
	total := ZeroMoney
	for _, t := range txns {
		delta, err := t.NetFor(account)
		if err != nil {
			return 0, err
		}
		next, err := total.Add(delta)
		if err != nil {
			return 0, fmt.Errorf("balance of %s: %w", account, err)
		}
		total = next
	}
	return total, nil
}

// Balances folds a sequence of transactions into every account they touch, in
// one pass. It returns a fresh map, so a caller cannot reach back into any
// transaction through it.
//
// Accounts with a net movement of zero are still present, with a zero balance —
// "this account was touched and nets to nothing" is a different fact from "this
// account does not exist", and the ledger is the wrong place to blur the two.
func Balances(txns ...Transaction) (map[Account]Money, error) {
	out := make(map[Account]Money)
	for _, t := range txns {
		for _, e := range t.entries {
			next, err := out[e.Account()].Add(e.Amount())
			if err != nil {
				return nil, fmt.Errorf("balance of %s: %w", e.Account(), err)
			}
			out[e.Account()] = next
		}
	}
	return out, nil
}

// LedgerIsBalanced sums EVERY entry of EVERY transaction and returns an error
// unless the total is exactly [ZeroMoney].
//
// This is the invariant double-entry exists for, stated as executable code. It
// holds transaction by transaction because [NewTransaction] enforces it, so
// this function can only fail on an overflow — which is precisely why it is
// worth having: it is the audit hook that will still be true after phase 2
// rebuilds these values out of database rows, where the type system's guarantee
// no longer reaches.
func LedgerIsBalanced(txns ...Transaction) error {
	total := ZeroMoney
	for _, t := range txns {
		for _, e := range t.entries {
			next, err := total.Add(e.Amount())
			if err != nil {
				return fmt.Errorf("ledger total at transaction %s: %w", t.id, err)
			}
			total = next
		}
	}
	if !total.IsZero() {
		return fmt.Errorf("ledger sums to %s across %d transaction(s): %w",
			total, len(txns), ErrUnbalancedTransaction)
	}
	return nil
}

// NewGrantTransaction issues play money to a customer:
//
//	issuance  −amount
//	user cash +amount
//
// It is the only way currency enters the system, which is what makes the
// issuance account's balance a trustworthy count of everything in circulation.
func NewGrantTransaction(id TransactionID, user UserID, amount Money, at time.Time) (Transaction, error) {
	if !amount.IsPositive() {
		return Transaction{}, fmt.Errorf("grant of %s to %s: %w", amount, user, ErrAmountNotPositive)
	}
	cash, err := UserCashAccount(user)
	if err != nil {
		return Transaction{}, err
	}
	entries, err := transferEntries(EntryKindGrant, IssuanceAccount(), cash, amount)
	if err != nil {
		return Transaction{}, err
	}
	return NewTransaction(TransactionParams{
		ID: id, Kind: EntryKindGrant, Entries: entries, OccurredAt: at,
	})
}

// NewAdjustmentTransaction moves an amount between two accounts as a manual
// correction. Pass the zero [WagerID] when the correction is not about a wager.
//
// Direction is expressed by the account order, never by the sign of amount: a
// negative "adjustment from A to B" would be a credit to A wearing the name of
// a debit, and that is exactly the kind of row nobody notices in a reconciliation.
func NewAdjustmentTransaction(id TransactionID, from, to Account, amount Money, wager WagerID, at time.Time) (Transaction, error) {
	if from.IsZero() || to.IsZero() {
		return Transaction{}, fmt.Errorf("adjustment %s: %w", id, ErrUnknownAccountKind)
	}
	if from == to {
		return Transaction{}, fmt.Errorf("adjustment %s on %s: %w", id, from, ErrSameAccountTransfer)
	}
	if !amount.IsPositive() {
		return Transaction{}, fmt.Errorf("adjustment %s of %s: %w", id, amount, ErrAmountNotPositive)
	}
	entries, err := transferEntries(EntryKindAdjustment, from, to, amount)
	if err != nil {
		return Transaction{}, err
	}
	return NewTransaction(TransactionParams{
		ID: id, Kind: EntryKindAdjustment, Entries: entries, WagerID: wager, OccurredAt: at,
	})
}

// NewStakeTransaction locks a placed wager's stake:
//
//	user cash   −stake
//	user escrow +stake
//
// The stake leaves the spendable balance but is not yet the house's; who ends
// up with it is decided at settlement. Modelling it any other way — debiting
// the customer straight to the house and crediting back on a win — would make a
// customer's "at risk" figure unrecoverable and would report the book as up by
// the whole handle at all times.
func NewStakeTransaction(id TransactionID, w Wager, at time.Time) (Transaction, error) {
	if w.IsZero() {
		return Transaction{}, fmt.Errorf("stake transaction %s: %w", id, ErrWagerRequired)
	}
	cash, err := UserCashAccount(w.UserID())
	if err != nil {
		return Transaction{}, err
	}
	escrow, err := UserEscrowAccount(w.UserID())
	if err != nil {
		return Transaction{}, err
	}
	entries, err := transferEntries(EntryKindStake, cash, escrow, w.Stake())
	if err != nil {
		return Transaction{}, err
	}
	return NewTransaction(TransactionParams{
		ID: id, Kind: EntryKindStake, Entries: entries, WagerID: w.ID(), OccurredAt: at,
	})
}

// NewSettlementTransaction closes a settled wager's money position.
//
// Every outcome is the SAME three movements, which is the point:
//
//	user escrow −stake
//	user cash   +returned
//	house       +(stake − returned)
//
// They sum to −stake + returned + stake − returned = 0 identically, for any
// returned amount, so the transaction balances by algebra rather than by four
// per-outcome formulas that have to be kept in agreement. Entries that come out
// to zero are dropped, since a zero row is not a movement:
//
//	lost       → returned 0     → cash entry drops; escrow → house
//	void, push → returned stake → house entry drops; escrow → cash
//	won        → returned > stake → all three; the house funds the profit
//	cashed out → returned as agreed → house takes or funds the difference
//
// The amount used is [Wager.Returned], never [Wager.PotentialPayout]: a
// partially-voided parlay pays less than its headline, and paying the headline
// is a real overpayment bug that this choice makes unrepresentable.
func NewSettlementTransaction(id TransactionID, w Wager, at time.Time) (Transaction, error) {
	returned, ok := w.Returned()
	if !ok {
		return Transaction{}, fmt.Errorf("wager %s is %s: %w", w.ID(), w.Status(), ErrWagerNotSettled)
	}
	kind, err := settlementEntryKind(w.Status())
	if err != nil {
		return Transaction{}, err
	}
	cash, err := UserCashAccount(w.UserID())
	if err != nil {
		return Transaction{}, err
	}
	escrow, err := UserEscrowAccount(w.UserID())
	if err != nil {
		return Transaction{}, err
	}

	releasedEscrow, err := w.Stake().Neg()
	if err != nil {
		return Transaction{}, fmt.Errorf("wager %s escrow release: %w", w.ID(), err)
	}
	houseDelta, err := w.Stake().Sub(returned)
	if err != nil {
		return Transaction{}, fmt.Errorf("wager %s house delta: %w", w.ID(), err)
	}

	var entries []LedgerEntry
	for _, move := range []struct {
		account Account
		amount  Money
	}{
		{escrow, releasedEscrow},
		{cash, returned},
		{HouseAccount(), houseDelta},
	} {
		if move.amount.IsZero() {
			continue
		}
		entry, err := NewLedgerEntry(move.account, move.amount, kind)
		if err != nil {
			return Transaction{}, err
		}
		entries = append(entries, entry)
	}

	return NewTransaction(TransactionParams{
		ID: id, Kind: kind, Entries: entries, WagerID: w.ID(), OccurredAt: at,
	})
}

// settlementEntryKind maps a wager's terminal status onto the kind of movement
// its settlement is.
func settlementEntryKind(s WagerStatus) (EntryKind, error) {
	switch s {
	case WagerStatusWon:
		return EntryKindPayout, nil
	case WagerStatusLost:
		return EntryKindLoss, nil
	case WagerStatusVoid, WagerStatusPush:
		return EntryKindRefund, nil
	case WagerStatusCashedOut:
		return EntryKindCashOut, nil
	default:
		return EntryKindUnknown, fmt.Errorf("wager status %s: %w", s, ErrWagerNotSettled)
	}
}

// transferEntries builds the debit/credit pair for moving amount from one
// account to another. It is the only place in the package that writes a
// two-sided movement by hand, so the sign convention has exactly one home.
func transferEntries(kind EntryKind, from, to Account, amount Money) ([]LedgerEntry, error) {
	debitAmount, err := amount.Neg()
	if err != nil {
		return nil, fmt.Errorf("%s from %s: %w", kind, from, err)
	}
	debit, err := NewLedgerEntry(from, debitAmount, kind)
	if err != nil {
		return nil, err
	}
	credit, err := NewLedgerEntry(to, amount, kind)
	if err != nil {
		return nil, err
	}
	return []LedgerEntry{debit, credit}, nil
}
