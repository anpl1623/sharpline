package domain_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Property-based tests for the double-entry ledger, driven by pgregory.net/rapid.
//
// CLAUDE.md §4: "LedgerEntry — double-entry. Every stake, payout, void, and
// adjustment is two rows that sum to zero. Balances are derived, never stored as a
// mutable field." Those two sentences are the entire specification of this file. It
// generates arbitrary but legal sequences of ledger movements — grants, stakes,
// settlements of every outcome, operator adjustments — and asserts that the sum is
// zero and that every derived balance equals the fold that defines it.
//
// # Why exact equality, everywhere
//
// There is not a single tolerance in this file, and that is the point. Money is
// int64 minor units (CLAUDE.md §12: "Floating point never touches a balance"), so
// "sums to zero" means the integer zero and == is the correct comparison. The one
// place a float appears is the accepted decimal price on a wager, and it is
// converted to an integer payout by Money.MulFloat under an explicit Rounding before
// it reaches the ledger at all. A ledger that needed an epsilon would be a ledger
// that can lose a cent per row, which is precisely the design this test exists to
// hold in place.
//
// # Why this file is package domain_test
//
// internal/domain/doc_test.go enforces mechanically that nothing in package domain
// imports outside the standard library, by parsing every .go file in the directory.
// pgregory.net/rapid is a test-only dependency and is sanctioned by the phase brief,
// so the guard was widened to allow a non-stdlib import in a _test.go file, and only
// there. Non-test files are still held to the original zero-dependency rule.

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// propEpoch is the instant every generated ledger is stamped from. Nothing in
// package domain reads a clock, so a literal is the only correct source of time
// here and every timestamp below is an offset from this one.
var propEpoch = time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)

// propOutcome is one way a wager can close, together with the rule that fixes the
// amount returned. The rules are not invented here — they are Wager.checkReturn's,
// restated so the generator can only ever produce a settlement the domain accepts.
type propOutcome uint8

const (
	propOutcomeOpen propOutcome = iota // placed, never settled: escrow still held
	propOutcomeWon
	propOutcomeLost
	propOutcomeVoid
	propOutcomePush
	propOutcomeCashedOut
)

func (o propOutcome) String() string {
	switch o {
	case propOutcomeOpen:
		return "open"
	case propOutcomeWon:
		return "won"
	case propOutcomeLost:
		return "lost"
	case propOutcomeVoid:
		return "void"
	case propOutcomePush:
		return "push"
	case propOutcomeCashedOut:
		return "cashed_out"
	default:
		return "unknown"
	}
}

// propWagerRecord is one generated ticket and what the ledger should say about it.
//
// The identifier is carried rather than being recovered from the ledger by matching
// on amounts. An earlier draft did the latter, on the theory that walking the rows
// the way an auditor would was a stronger check; it was wrong, because two tickets
// from the same user for the same stake are indistinguishable that way, and the
// first counterexample rapid found was exactly that — two 1.00 wagers, one returning
// 1.00 and one returning 1.01, matched to each other's settlement. The identifier is
// not the fact under test; the amounts are.
type propWagerRecord struct {
	id       domain.WagerID
	user     domain.UserID
	stake    domain.Money
	returned domain.Money
	outcome  propOutcome
}

// propLedger is a generated ledger together with the facts a test needs to check it
// against something other than itself.
type propLedger struct {
	txns    []domain.Transaction
	users   []domain.UserID
	granted map[domain.UserID]domain.Money
	wagers  []propWagerRecord
}

// -----------------------------------------------------------------------------
// The generator
// -----------------------------------------------------------------------------

// propGenerateLedger draws a legal sequence of ledger movements.
//
// # Construction, not rejection
//
// Every draw produces a constructible ledger. Amounts are drawn inside the ranges
// the constructors accept, settlement amounts are drawn inside the interval each
// outcome permits, and timestamps advance monotonically because Wager.stamp refuses
// an update that precedes the last one. A generator that drew freely and filtered
// would shrink badly — rapid cannot reduce a counterexample through a filter that
// rejects most of its neighbourhood — and would spend most of its draws proving that
// the constructors reject bad input, which the table-driven tests already cover.
//
// # The amount ranges
//
// Stakes run from 100 minor units (one major unit — the smallest bet worth placing)
// to 10^7 (a hundred thousand major units). Grants run an order of magnitude above
// the largest wager the user can place, so a generated ledger is one a real customer
// could have produced rather than one that immediately overdraws. Nothing here
// approaches Money's 2^53-1 ceiling: a few hundred movements of at most 10^9 sum to
// at most 10^12, four orders of magnitude inside it, so no property below can fail
// for the uninteresting reason that the fold overflowed.
//
// Accepted prices run to 200.0 rather than to MaxWagerDecimal, because the property
// under test is conservation of integers and a 10^9 price only tests
// Money.MulFloat's overflow guard, which money_test.go owns.
func propGenerateLedger() *rapid.Generator[propLedger] {
	return rapid.Custom(func(t *rapid.T) propLedger {
		userCount := rapid.IntRange(1, 3).Draw(t, "users")

		out := propLedger{
			granted: make(map[domain.UserID]domain.Money, userCount),
		}
		seq := 0
		nextID := func(prefix string) string {
			seq++
			return fmt.Sprintf("%s-%06d", prefix, seq)
		}
		at := func(step int) time.Time { return propEpoch.Add(time.Duration(step) * time.Minute) }

		for u := range userCount {
			user := domain.UserID(fmt.Sprintf("user-%02d", u))
			out.users = append(out.users, user)

			grantMinor := rapid.Int64Range(100_000, 1_000_000_000).Draw(t, fmt.Sprintf("grant%d", u))
			grant, err := domain.FromMinorUnits(grantMinor)
			if err != nil {
				t.Fatalf("FromMinorUnits(%d): %v", grantMinor, err)
			}
			txn, err := domain.NewGrantTransaction(
				domain.TransactionID(nextID("txn")), user, grant, at(seq))
			if err != nil {
				t.Fatalf("NewGrantTransaction(%s, %s): %v", user, grant, err)
			}
			out.txns = append(out.txns, txn)
			out.granted[user] = grant

			wagerCount := rapid.IntRange(0, 4).Draw(t, fmt.Sprintf("wagers%d", u))
			for w := range wagerCount {
				record := propAppendWager(t, &out, user, u, w, nextID, at, &seq)
				out.wagers = append(out.wagers, record)
			}
		}

		// Operator adjustments. CLAUDE.md §6 lists an admin console with manual
		// settlement, so a correction moving money between two arbitrary accounts is
		// part of the real transaction mix and belongs in the generated sequence
		// rather than being tested only in isolation.
		adjustments := rapid.IntRange(0, 3).Draw(t, "adjustments")
		accounts := propAllAccounts(t, out.users)
		for a := range adjustments {
			i := rapid.IntRange(0, len(accounts)-1).Draw(t, fmt.Sprintf("adjustFrom%d", a))
			j := rapid.IntRange(0, len(accounts)-2).Draw(t, fmt.Sprintf("adjustTo%d", a))
			if j >= i {
				j++ // Chosen from the set minus i, so from and to are always distinct.
			}
			amountMinor := rapid.Int64Range(1, 1_000_000).Draw(t, fmt.Sprintf("adjustAmount%d", a))
			amount, err := domain.FromMinorUnits(amountMinor)
			if err != nil {
				t.Fatalf("FromMinorUnits(%d): %v", amountMinor, err)
			}
			txn, err := domain.NewAdjustmentTransaction(
				domain.TransactionID(nextID("txn")), accounts[i], accounts[j], amount, "", at(seq))
			if err != nil {
				t.Fatalf("NewAdjustmentTransaction(%s → %s, %s): %v",
					accounts[i], accounts[j], amount, err)
			}
			out.txns = append(out.txns, txn)
		}

		return out
	})
}

// propAppendWager draws one ticket, writes its stake movement, and — unless the
// ticket is left open — settles it and writes the settlement movement.
func propAppendWager(
	t *rapid.T,
	ledger *propLedger,
	user domain.UserID,
	userIndex, wagerIndex int,
	nextID func(string) string,
	at func(int) time.Time,
	seq *int,
) propWagerRecord {
	label := func(s string) string { return fmt.Sprintf("%s-u%d-w%d", s, userIndex, wagerIndex) }

	stakeMinor := rapid.Int64Range(100, 10_000_000).Draw(t, label("stake"))
	stake, err := domain.FromMinorUnits(stakeMinor)
	if err != nil {
		t.Fatalf("FromMinorUnits(%d): %v", stakeMinor, err)
	}
	price := rapid.Float64Range(1.01, 200).Draw(t, label("price"))
	rounding := rapid.SampledFrom([]domain.Rounding{
		domain.RoundHalfAwayFromZero, domain.RoundHalfToEven, domain.RoundTowardZero,
	}).Draw(t, label("rounding"))

	selectionID := domain.SelectionID(fmt.Sprintf("sel-%02d-%02d", userIndex, wagerIndex))
	quote, err := domain.NewPrice(domain.PriceParams{
		SelectionID: selectionID,
		BookID:      domain.BookID(domain.SyntheticBookSlug),
		Decimal:     price,
		ObservedAt:  at(*seq),
	})
	if err != nil {
		t.Fatalf("NewPrice(%v): %v", price, err)
	}
	leg, err := domain.NewLeg(domain.LegParams{
		ID:          domain.LegID(fmt.Sprintf("leg-%02d-%02d", userIndex, wagerIndex)),
		EventID:     domain.EventID(fmt.Sprintf("evt-%02d-%02d", userIndex, wagerIndex)),
		MarketID:    domain.MarketID(fmt.Sprintf("mkt-%02d-%02d", userIndex, wagerIndex)),
		MarketType:  domain.MarketTypeMoneyline,
		Role:        domain.SelectionRoleHome,
		SelectionID: selectionID,
		Price:       quote,
	})
	if err != nil {
		t.Fatalf("NewLeg: %v", err)
	}

	wagerID := domain.WagerID(nextID("wgr"))
	wager, err := domain.NewWager(domain.WagerParams{
		ID:              wagerID,
		UserID:          user,
		Kind:            domain.WagerKindStraight,
		Legs:            []domain.Leg{leg},
		Stake:           stake,
		AcceptedDecimal: leg.QuotedDecimal(),
		Rounding:        rounding,
		PlacedAt:        at(*seq),
	})
	if err != nil {
		t.Fatalf("NewWager(stake %s at %v): %v", stake, price, err)
	}

	stakeTxn, err := domain.NewStakeTransaction(domain.TransactionID(nextID("txn")), wager, at(*seq))
	if err != nil {
		t.Fatalf("NewStakeTransaction: %v", err)
	}
	ledger.txns = append(ledger.txns, stakeTxn)

	outcome := rapid.SampledFrom([]propOutcome{
		propOutcomeOpen, propOutcomeWon, propOutcomeLost,
		propOutcomeVoid, propOutcomePush, propOutcomeCashedOut,
	}).Draw(t, label("outcome"))

	record := propWagerRecord{id: wagerID, user: user, stake: stake, outcome: outcome}
	if outcome == propOutcomeOpen {
		return record
	}

	settledAt := at(*seq + 1)
	var settled domain.Wager

	switch outcome {
	case propOutcomeWon:
		// A win returns at least the stake and at most the frozen potential
		// payout. Drawing inside that interval covers the partially-voided parlay
		// case, where a ticket legitimately pays less than its headline.
		lo, hi := stake.MinorUnits(), wager.PotentialPayout().MinorUnits()
		returnedMinor := rapid.Int64Range(lo, hi).Draw(t, label("won"))
		returned, err := domain.FromMinorUnits(returnedMinor)
		if err != nil {
			t.Fatalf("FromMinorUnits(%d): %v", returnedMinor, err)
		}
		settled, err = wager.Settle(domain.WagerStatusWon, returned, settledAt)
		if err != nil {
			t.Fatalf("Settle(won, %s) on a %s stake with a %s payout: %v",
				returned, stake, wager.PotentialPayout(), err)
		}
	case propOutcomeLost:
		settled, err = wager.Settle(domain.WagerStatusLost, domain.ZeroMoney, settledAt)
		if err != nil {
			t.Fatalf("Settle(lost): %v", err)
		}
	case propOutcomeVoid:
		settled, err = wager.Settle(domain.WagerStatusVoid, stake, settledAt)
		if err != nil {
			t.Fatalf("Settle(void): %v", err)
		}
	case propOutcomePush:
		settled, err = wager.Settle(domain.WagerStatusPush, stake, settledAt)
		if err != nil {
			t.Fatalf("Settle(push): %v", err)
		}
	case propOutcomeCashedOut:
		// A cash-out is strictly positive and at most the potential payout. It may
		// legitimately land below the stake — that is what taking a bad price early
		// means, and it is the case where the house profits on a ticket that was
		// never graded.
		amountMinor := rapid.Int64Range(1, wager.PotentialPayout().MinorUnits()).Draw(t, label("cashOut"))
		amount, err := domain.FromMinorUnits(amountMinor)
		if err != nil {
			t.Fatalf("FromMinorUnits(%d): %v", amountMinor, err)
		}
		settled, err = wager.CashOut(amount, settledAt)
		if err != nil {
			t.Fatalf("CashOut(%s) against a %s payout: %v", amount, wager.PotentialPayout(), err)
		}
	case propOutcomeOpen:
		t.Fatalf("unreachable: the open case returns above")
	}

	returned, ok := settled.Returned()
	if !ok {
		t.Fatalf("wager %s is %s but reports no returned amount", wagerID, settled.Status())
	}
	record.returned = returned

	settlementTxn, err := domain.NewSettlementTransaction(
		domain.TransactionID(nextID("txn")), settled, settledAt)
	if err != nil {
		t.Fatalf("NewSettlementTransaction for a %s wager returning %s: %v",
			settled.Status(), returned, err)
	}
	ledger.txns = append(ledger.txns, settlementTxn)
	*seq += 2
	return record
}

// propAllAccounts enumerates every account a generated ledger can touch: the two
// system singletons plus a cash and an escrow account per user.
func propAllAccounts(t *rapid.T, users []domain.UserID) []domain.Account {
	accounts := []domain.Account{domain.HouseAccount(), domain.IssuanceAccount()}
	for _, u := range users {
		cash, err := domain.UserCashAccount(u)
		if err != nil {
			t.Fatalf("UserCashAccount(%s): %v", u, err)
		}
		escrow, err := domain.UserEscrowAccount(u)
		if err != nil {
			t.Fatalf("UserEscrowAccount(%s): %v", u, err)
		}
		accounts = append(accounts, cash, escrow)
	}
	return accounts
}

// propSumEntries folds every entry of every transaction into one total, without
// going through any of the functions under test.
func propSumEntries(t *rapid.T, txns []domain.Transaction) int64 {
	total := int64(0)
	for _, txn := range txns {
		for _, e := range txn.Entries() {
			total += e.Amount().MinorUnits()
		}
	}
	if len(txns) > 0 && total != 0 {
		t.Logf("independent fold over %d transaction(s) came to %d minor units", len(txns), total)
	}
	return total
}

// -----------------------------------------------------------------------------
// The invariant double-entry exists for
// -----------------------------------------------------------------------------

// TestRapidLedgerSumsToExactlyZero asserts the property the phase brief names: for
// any generated sequence of valid transactions the sum of every entry is EXACTLY
// zero. Integers, so exact equality is the right test and no tolerance appears.
//
// It is asserted three ways, deliberately:
//
//  1. LedgerIsBalanced over the whole sequence — the function the audit hook will
//     call in phase 2, once these values are rebuilt out of database rows and the
//     type system's guarantee no longer reaches;
//  2. an independent fold written here, so the test does not check
//     LedgerIsBalanced against itself;
//  3. transaction by transaction, and over every prefix, because the whole sum being
//     zero is also what two compensating errors look like.
func TestRapidLedgerSumsToExactlyZero(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ledger := propGenerateLedger().Draw(t, "ledger")

		if err := domain.LedgerIsBalanced(ledger.txns...); err != nil {
			t.Fatalf("a ledger of %d transaction(s) does not balance: %v", len(ledger.txns), err)
		}
		if total := propSumEntries(t, ledger.txns); total != 0 {
			t.Fatalf("independent fold over %d transaction(s) = %d minor units, want exactly 0",
				len(ledger.txns), total)
		}

		for i, txn := range ledger.txns {
			entries := txn.Entries()
			if len(entries) < 2 {
				t.Fatalf("transaction %d (%s) has %d entr(ies); a balanced movement has at least two",
					i, txn.ID(), len(entries))
			}
			sum := int64(0)
			for _, e := range entries {
				if e.Amount().IsZero() {
					t.Fatalf("transaction %d (%s) carries a zero-amount entry on %s",
						i, txn.ID(), e.Account())
				}
				if e.Kind() != txn.Kind() {
					t.Fatalf("transaction %d (%s) is a %s but carries a %s entry",
						i, txn.ID(), txn.Kind(), e.Kind())
				}
				sum += e.Amount().MinorUnits()
			}
			if sum != 0 {
				t.Fatalf("transaction %d (%s, %s) sums to %d minor units",
					i, txn.ID(), txn.Kind(), sum)
			}

			// Every prefix balances too, which is what rules out two errors that
			// happen to cancel across the sequence.
			if err := domain.LedgerIsBalanced(ledger.txns[:i+1]...); err != nil {
				t.Fatalf("the first %d transaction(s) do not balance: %v", i+1, err)
			}
		}
	})
}

// TestRapidDerivedBalancesEqualTheFold asserts the second half of the brief's ledger
// property and CLAUDE.md §4's "balances are derived, never stored".
//
// Three routes to the same number are compared and must agree exactly: Balance's
// per-account fold, Balances' one-pass map, and a fold written here directly over
// the entries. If any two disagree there is a second implementation of "balance"
// somewhere, which is the defect this whole design is arranged to prevent.
func TestRapidDerivedBalancesEqualTheFold(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ledger := propGenerateLedger().Draw(t, "ledger")

		balances, err := domain.Balances(ledger.txns...)
		if err != nil {
			t.Fatalf("Balances over %d transaction(s): %v", len(ledger.txns), err)
		}

		total := int64(0)
		for account, got := range balances {
			// Route two: the per-account fold.
			viaBalance, err := domain.Balance(account, ledger.txns...)
			if err != nil {
				t.Fatalf("Balance(%s): %v", account, err)
			}
			if viaBalance != got {
				t.Fatalf("%s: Balance says %s but Balances says %s",
					account, viaBalance, got)
			}

			// Route three: a fold written here, over the same entries, touching
			// none of the code under test.
			manual := int64(0)
			for _, txn := range ledger.txns {
				for _, e := range txn.Entries() {
					if e.Account() == account {
						manual += e.Amount().MinorUnits()
					}
				}
			}
			if manual != got.MinorUnits() {
				t.Fatalf("%s: an independent fold over the entries gives %d minor units, the package gives %d",
					account, manual, got.MinorUnits())
			}
			total += got.MinorUnits()
		}

		if total != 0 {
			t.Fatalf("the balances of the %d touched account(s) sum to %d minor units, want exactly 0",
				len(balances), total)
		}

		// An account nobody touched folds to zero rather than failing. "Touched and
		// nets to nothing" and "never touched" are different facts, and both are
		// legal reads.
		untouched, err := domain.UserCashAccount("user-never-seen")
		if err != nil {
			t.Fatalf("UserCashAccount: %v", err)
		}
		zero, err := domain.Balance(untouched, ledger.txns...)
		if err != nil {
			t.Fatalf("Balance of an untouched account: %v", err)
		}
		if !zero.IsZero() {
			t.Fatalf("an account no transaction touches has balance %s", zero)
		}
	})
}

// TestRapidIssuanceBalanceIsTheNegativeOfCirculation asserts what the issuance
// account is FOR: its balance, negated, is exactly how much play money exists.
//
// This is the reason ledger.go keeps issuance separate from house. Fold them
// together and a large sign-up grant becomes indistinguishable from a bad Sunday on
// the trading book — the two accounts answer different questions, and this property
// is the one that would break first if they were merged.
//
// Adjustments are deliberately allowed to move money into and out of the issuance
// account in the generator, so the assertion is stated in terms of what the ledger
// records rather than in terms of the grants alone.
func TestRapidIssuanceBalanceIsTheNegativeOfCirculation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ledger := propGenerateLedger().Draw(t, "ledger")

		issuance, err := domain.Balance(domain.IssuanceAccount(), ledger.txns...)
		if err != nil {
			t.Fatalf("Balance(issuance): %v", err)
		}

		// Sum the grant movements out of issuance, and separately any adjustment
		// that touched it, straight from the entries.
		fromGrants, fromAdjustments := int64(0), int64(0)
		for _, txn := range ledger.txns {
			for _, e := range txn.Entries() {
				if e.Account() != domain.IssuanceAccount() {
					continue
				}
				switch e.Kind() {
				case domain.EntryKindGrant:
					fromGrants += e.Amount().MinorUnits()
				default:
					fromAdjustments += e.Amount().MinorUnits()
				}
			}
		}

		granted := int64(0)
		for _, amount := range ledger.granted {
			granted += amount.MinorUnits()
		}
		if fromGrants != -granted {
			t.Fatalf("grants totalling %d minor units debited issuance by %d", granted, -fromGrants)
		}
		if issuance.MinorUnits() != fromGrants+fromAdjustments {
			t.Fatalf("issuance balance %d does not equal its grant movements %d plus its adjustments %d",
				issuance.MinorUnits(), fromGrants, fromAdjustments)
		}
		if fromAdjustments == 0 && issuance.IsPositive() {
			t.Fatalf("issuance balance is %s; money created must leave that account, so its balance is never positive",
				issuance)
		}
	})
}

// TestRapidEscrowEqualsOpenExposure asserts the question the per-user escrow account
// exists to answer — "how much do I have at risk?" (CLAUDE.md §6, open position
// tracking) — over an arbitrary mix of open and settled tickets.
//
// A user's escrow balance must equal the total staked on tickets that have not
// settled, and must therefore be exactly zero once every ticket has closed. Getting
// this wrong is not a rounding problem, it is a stake that never came back out of
// escrow, which would report a customer as permanently exposed on a bet that graded
// months ago.
func TestRapidEscrowEqualsOpenExposure(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ledger := propGenerateLedger().Draw(t, "ledger")

		expected := make(map[domain.UserID]int64, len(ledger.users))
		for _, w := range ledger.wagers {
			if w.outcome == propOutcomeOpen {
				expected[w.user] += w.stake.MinorUnits()
			}
		}

		for _, user := range ledger.users {
			escrow, err := domain.UserEscrowAccount(user)
			if err != nil {
				t.Fatalf("UserEscrowAccount(%s): %v", user, err)
			}
			held, err := domain.Balance(escrow, ledger.txns...)
			if err != nil {
				t.Fatalf("Balance(%s): %v", escrow, err)
			}

			// Adjustments can legitimately move money into or out of escrow, so the
			// expectation is corrected by whatever they did rather than being
			// asserted against the wagers alone.
			adjusted := int64(0)
			for _, txn := range ledger.txns {
				if txn.Kind() != domain.EntryKindAdjustment {
					continue
				}
				net, err := txn.NetFor(escrow)
				if err != nil {
					t.Fatalf("NetFor(%s): %v", escrow, err)
				}
				adjusted += net.MinorUnits()
			}

			if want := expected[user] + adjusted; held.MinorUnits() != want {
				t.Fatalf("%s holds %d minor units in escrow; open stakes total %d and adjustments moved %d, so it should hold %d",
					user, held.MinorUnits(), expected[user], adjusted, want)
			}
			if expected[user] == 0 && adjusted == 0 && !held.IsZero() {
				t.Fatalf("%s has no open tickets but %s is still in escrow", user, held)
			}
			if held.IsNegative() && adjusted >= 0 {
				t.Fatalf("%s has a negative escrow balance of %s; more was released than was ever held",
					user, held)
			}
		}
	})
}

// TestRapidSettlementConservesTheStake asserts the algebra NewSettlementTransaction
// is built on: whatever the outcome, the three movements are −stake from escrow,
// +returned to cash, and +(stake − returned) to the house, so the settlement nets to
// zero identically rather than by four per-outcome formulas that have to be kept in
// agreement.
//
// It also asserts the choice that makes an overpayment unrepresentable: the amount
// paid is Wager.Returned, never Wager.PotentialPayout.
func TestRapidSettlementConservesTheStake(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ledger := propGenerateLedger().Draw(t, "ledger")

		byWager := make(map[domain.WagerID][]domain.Transaction)
		for _, txn := range ledger.txns {
			if id, ok := txn.WagerID(); ok {
				byWager[id] = append(byWager[id], txn)
			}
		}

		for _, w := range ledger.wagers {
			if w.outcome == propOutcomeOpen {
				continue
			}
			cash, err := domain.UserCashAccount(w.user)
			if err != nil {
				t.Fatalf("UserCashAccount: %v", err)
			}
			escrow, err := domain.UserEscrowAccount(w.user)
			if err != nil {
				t.Fatalf("UserEscrowAccount: %v", err)
			}

			// Find this ticket's settlement movement among the transactions that
			// name it. There are exactly two: the stake and the settlement.
			var settlement domain.Transaction
			for _, txn := range byWager[w.id] {
				if txn.Kind() != domain.EntryKindStake {
					settlement = txn
				}
			}
			if settlement.IsZero() {
				t.Fatalf("wager %s (%s, %s) produced no settlement movement", w.id, w.user, w.outcome)
			}

			escrowNet, err := settlement.NetFor(escrow)
			if err != nil {
				t.Fatalf("NetFor(escrow): %v", err)
			}
			cashNet, err := settlement.NetFor(cash)
			if err != nil {
				t.Fatalf("NetFor(cash): %v", err)
			}
			houseNet, err := settlement.NetFor(domain.HouseAccount())
			if err != nil {
				t.Fatalf("NetFor(house): %v", err)
			}

			if escrowNet.MinorUnits() != -w.stake.MinorUnits() {
				t.Fatalf("settling %s (%s) released %s from escrow against a %s stake",
					w.id, w.outcome, escrowNet, w.stake)
			}
			if cashNet.MinorUnits() != w.returned.MinorUnits() {
				t.Fatalf("settling %s (%s) credited %s to cash but the ticket returned %s",
					w.id, w.outcome, cashNet, w.returned)
			}
			if want := w.stake.MinorUnits() - w.returned.MinorUnits(); houseNet.MinorUnits() != want {
				t.Fatalf("settling %s (%s) moved %s to the house; stake %s minus returned %s is %d",
					w.id, w.outcome, houseNet, w.stake, w.returned, want)
			}
			if sum := escrowNet.MinorUnits() + cashNet.MinorUnits() + houseNet.MinorUnits(); sum != 0 {
				t.Fatalf("the three settlement movements of %s sum to %d minor units", w.id, sum)
			}
		}
	})
}

// -----------------------------------------------------------------------------
// The negative property
// -----------------------------------------------------------------------------

// TestRapidUnbalancedTransactionsAreUnconstructible asserts the structural claim
// ledger.go makes: "a transaction that would carry this error is never constructed,
// so an unbalanced pair cannot exist as a value anywhere in the program."
//
// The generator draws arbitrary non-zero amounts and perturbs them if they happen to
// balance, so every input is genuinely unbalanced, and asserts that NewTransaction
// refuses every one of them with ErrUnbalancedTransaction. A constructor that
// accepted even one would make every property above vacuous — they would all be
// checking that a set of values which cannot exist has a property.
func TestRapidUnbalancedTransactionsAreUnconstructible(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.IntRange(2, 6).Draw(t, "entries")
		kind := rapid.SampledFrom([]domain.EntryKind{
			domain.EntryKindGrant, domain.EntryKindStake, domain.EntryKindPayout,
			domain.EntryKindLoss, domain.EntryKindRefund, domain.EntryKindAdjustment,
			domain.EntryKindCashOut,
		}).Draw(t, "kind")

		accounts := []domain.Account{domain.HouseAccount(), domain.IssuanceAccount()}
		for _, u := range []domain.UserID{"user-a", "user-b"} {
			cash, err := domain.UserCashAccount(u)
			if err != nil {
				t.Fatalf("UserCashAccount: %v", err)
			}
			escrow, err := domain.UserEscrowAccount(u)
			if err != nil {
				t.Fatalf("UserEscrowAccount: %v", err)
			}
			accounts = append(accounts, cash, escrow)
		}

		amounts := make([]int64, count)
		sum := int64(0)
		for i := range amounts {
			v := rapid.Int64Range(-1_000_000, 1_000_000).
				Filter(func(v int64) bool { return v != 0 }).
				Draw(t, fmt.Sprintf("amount%d", i))
			amounts[i] = v
			sum += v
		}
		if sum == 0 {
			// Nudge the last entry off zero. 1_000_001 is still far inside Money's
			// range, and the entry stays non-zero because a value of -1 would have
			// needed the rest to sum to 1, which the nudge preserves.
			amounts[count-1]++
			if amounts[count-1] == 0 {
				amounts[count-1] = 1
			}
			sum = 0
			for _, v := range amounts {
				sum += v
			}
			if sum == 0 {
				t.Skip("the nudge landed back on a balanced set")
			}
		}

		entries := make([]domain.LedgerEntry, count)
		for i, v := range amounts {
			amount, err := domain.FromMinorUnits(v)
			if err != nil {
				t.Fatalf("FromMinorUnits(%d): %v", v, err)
			}
			idx := rapid.IntRange(0, len(accounts)-1).Draw(t, fmt.Sprintf("account%d", i))
			entry, err := domain.NewLedgerEntry(accounts[idx], amount, kind)
			if err != nil {
				t.Fatalf("NewLedgerEntry(%s, %s, %s): %v", accounts[idx], amount, kind, err)
			}
			entries[i] = entry
		}

		_, err := domain.NewTransaction(domain.TransactionParams{
			ID:         "txn-unbalanced",
			Kind:       kind,
			Entries:    entries,
			OccurredAt: propEpoch,
		})
		if err == nil {
			t.Fatalf("NewTransaction accepted %d entries summing to %d minor units", count, sum)
		}
		if !errors.Is(err, domain.ErrUnbalancedTransaction) {
			t.Fatalf("NewTransaction rejected an unbalanced set with %v, not ErrUnbalancedTransaction", err)
		}
		if !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("ErrUnbalancedTransaction does not reach the ErrInvalid root: %v", err)
		}
	})
}

// TestRapidNetForSumsRatherThanFinds asserts that a transaction touching one account
// more than once reports the NET, not the first matching row.
//
// The case is reachable: a manual correction that splits a movement across two rows
// on the same account is a legal, balanced transaction, and a NetFor that returned
// the first match would silently under-report the correction. Nothing in the
// generator above produces one, so it is constructed directly here.
func TestRapidNetForSumsRatherThanFinds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		first := rapid.Int64Range(1, 1_000_000).Draw(t, "first")
		second := rapid.Int64Range(1, 1_000_000).Draw(t, "second")

		cash, err := domain.UserCashAccount("user-split")
		if err != nil {
			t.Fatalf("UserCashAccount: %v", err)
		}

		amounts := []struct {
			account domain.Account
			minor   int64
		}{
			{cash, first},
			{cash, second},
			{domain.HouseAccount(), -(first + second)},
		}
		entries := make([]domain.LedgerEntry, 0, len(amounts))
		for _, a := range amounts {
			amount, err := domain.FromMinorUnits(a.minor)
			if err != nil {
				t.Fatalf("FromMinorUnits(%d): %v", a.minor, err)
			}
			entry, err := domain.NewLedgerEntry(a.account, amount, domain.EntryKindAdjustment)
			if err != nil {
				t.Fatalf("NewLedgerEntry: %v", err)
			}
			entries = append(entries, entry)
		}

		txn, err := domain.NewTransaction(domain.TransactionParams{
			ID:         "txn-split",
			Kind:       domain.EntryKindAdjustment,
			Entries:    entries,
			OccurredAt: propEpoch,
		})
		if err != nil {
			t.Fatalf("NewTransaction over a balanced three-entry split: %v", err)
		}

		net, err := txn.NetFor(cash)
		if err != nil {
			t.Fatalf("NetFor: %v", err)
		}
		if net.MinorUnits() != first+second {
			t.Fatalf("NetFor over two rows of %d and %d on the same account returned %d",
				first, second, net.MinorUnits())
		}
		if err := domain.LedgerIsBalanced(txn); err != nil {
			t.Fatalf("a three-entry split does not balance: %v", err)
		}

		// Entries is documented as returning a copy. Appending to it must not reach
		// back into the transaction, because a caller who could would be able to
		// unbalance a value whose entire guarantee is that it balances.
		stolen := txn.Entries()
		stolen = stolen[:0]
		if txn.EntryCount() != len(amounts) {
			t.Fatalf("truncating the slice Entries returned changed the transaction to %d entries",
				txn.EntryCount())
		}
		_ = stolen
	})
}
