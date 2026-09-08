// The record this package publishes to wager.events.
//
// # It is self-contained, and here that is a stronger requirement than usual
//
// kafka/topics.go calls wager.events "the settlement audit trail: retention-
// based, keyed by wager_id ... NOT compacted — the whole value of an audit trail
// is that superseded entries survive." An audit trail whose records are
// identifier-only is not an audit trail; it is an index into a database, and it
// tells you nothing on the day the database is the thing you are auditing.
//
// So this document carries the whole settlement: the ticket's booked terms, every
// leg with the price it was booked at and the status it graded to, the balanced
// ledger movement with all of its entries, and the result that decided it.
// Somebody holding nothing but this topic can reconstruct what happened, recompute
// the payout, and check that the entries sum to zero — which is exactly the
// question an audit asks.
//
// The denormalisation is therefore the point rather than a shortcut, and it is
// the same argument internal/pricing/payload.go and the normalizer's payload make
// for the odds path, applied to a topic where the stakes are higher.
//
// # Money is int64 minor units on the wire, and every field says so in its name
//
// CLAUDE.md §12: "All money and stake values are integer minor units. Floating
// point never touches a balance." That rule does not stop at the process
// boundary. Every money field here is an int64 named `*_minor`, converted through
// domain.Money.MinorUnits() rather than through any float, and there is no
// currency-formatted string anywhere in the document — a formatted amount is a
// display concern and putting one on the audit trail invites a consumer to parse
// it back.
//
// Odds and lines are floats, per the same sentence of §12 read the other way.
//
// # The shape follows internal/pricing/payload.go, deliberately
//
// schema_version first, an independently versioned document inside an
// independently versioned envelope, timestamps carried from the source rather
// than stamped fresh. Two topics with two record shapes that follow one
// convention cost a reader one act of learning; two that follow two cost them
// two, for ever.
//
// What is deliberately NOT here is a "published_at". The envelope's ProducedAt is
// when this system wrote the record and [SettledWager.SettledAt] is when the
// domain says the ticket closed. A third instant would invite a consumer to
// measure from the wrong one, which is the confusion the pricing payload spent a
// paragraph avoiding.
package settlement

import (
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// SchemaVersion is the version of the [SettledWager] document.
//
// It is versioned independently of kafka.EnvelopeVersion, which versions the
// frame around it. Adding an optional field is not a bump; removing, renaming,
// or changing the meaning or the UNIT of one is.
//
// wager.events is retention-based rather than compacted, so a bump behaves
// differently here from the way it does on price.computed: old records age out
// on the retention clock instead of surviving until their key is next written.
// A consumer therefore meets two versions only during the retention window
// following a deploy — but the type name travels on every record anyway, because
// "only during the retention window" is not a claim worth betting an audit trail
// on.
const SchemaVersion = 1

// MessageType is the kafka.Message.Type stamped on every record this package
// writes. Consumers switch on it.
const MessageType = "wager.settled.v1"

// SettledWager is one ticket's settlement, complete enough to reconstruct
// without the database.
type SettledWager struct {
	// SchemaVersion is [SchemaVersion] as written.
	SchemaVersion int `json:"schema_version"`

	// Wager is the ticket: its booked terms and its terminal outcome.
	Wager WagerRecord `json:"wager"`

	// Legs is every leg on the ticket, in the order the ticket carries them,
	// each with the price it was booked at and the status it graded to.
	//
	// ALL of them, not only the ones this result decided. A parlay settles once,
	// when its last leg grades, and the record of that settlement has to show
	// why it paid what it paid — which means showing the leg that pushed and the
	// leg that voided, not only the one that finished last.
	Legs []LegRecord `json:"legs"`

	// Settlement is the balanced ledger movement that paid the ticket. Its
	// entries sum to exactly zero, which a consumer can and should verify.
	Settlement TransactionRecord `json:"settlement"`

	// Result is the event outcome that decided the final leg. It is one result,
	// not the set of results that decided the whole ticket: the earlier legs
	// carry their own graded_at, and duplicating every event's final score onto
	// every ticket it touched would multiply the topic's volume by the parlay
	// rate for a fact the events feed already holds.
	Result ResultRecord `json:"result"`
}

// WagerRecord is the ticket, on the wire.
//
// It restates domain.Wager with explicit snake_case tags rather than reusing it,
// because domain.Wager has unexported fields and no tags of its own — the domain
// is a package that "may not know it is being serialised", which is the same
// argument internal/pricing/payload.go makes about odds.Margin. It also lets the
// units be spelled into the field names, which is the property that matters most
// on a money record.
type WagerRecord struct {
	ID     domain.WagerID     `json:"id"`
	UserID domain.UserID      `json:"user_id"`
	Kind   domain.WagerKind   `json:"kind"`
	Status domain.WagerStatus `json:"status"`

	// StakeMinor is what was risked.
	StakeMinor int64 `json:"stake_minor"`

	// AcceptedDecimal is the ticket price the customer took: total return per
	// unit staked with every leg winning. It is the STORED price, never a
	// product of the leg prices — a same-game parlay is correlation-adjusted and
	// a teaser is priced off a schedule, so re-deriving it would print a number
	// the customer was never shown.
	AcceptedDecimal float64 `json:"accepted_decimal"`

	// Rounding is the rule stake × price was collapsed under. It travels because
	// it is what makes PotentialPayoutMinor reproducible: three rounding modes
	// give three different answers on the same two inputs, and an audit that
	// cannot reproduce the number it is auditing is a filing cabinet.
	Rounding domain.Rounding `json:"rounding"`

	// PotentialPayoutMinor is the TOTAL RETURN if every leg had won, stake
	// included; PotentialProfitMinor is that minus the stake. Both were frozen
	// at placement. They are named as bluntly as domain.Wager names them,
	// because "conflating return with profit produces a plausible number of the
	// right magnitude".
	PotentialPayoutMinor int64 `json:"potential_payout_minor"`
	PotentialProfitMinor int64 `json:"potential_profit_minor"`

	// ReturnedMinor is what settlement actually paid back, and NetReturnMinor is
	// that minus the stake — negative on a loser. ReturnedMinor is the only
	// authority on what was owed: a partially-voided parlay returns less than
	// the headline payout.
	ReturnedMinor  int64 `json:"returned_minor"`
	NetReturnMinor int64 `json:"net_return_minor"`

	// TeaserPoints is the number of points every leg's line was moved by,
	// present exactly on a teaser.
	TeaserPoints *float64 `json:"teaser_points,omitempty"`

	// RoundRobinID names the round robin this ticket was expanded from, present
	// exactly on a round-robin ticket.
	RoundRobinID string `json:"round_robin_id,omitempty"`

	// PlacedAt is when the ticket was accepted; SettledAt is when it closed.
	PlacedAt  time.Time `json:"placed_at"`
	SettledAt time.Time `json:"settled_at"`
}

// LegRecord is one leg, with the price it was booked at and how it graded.
type LegRecord struct {
	ID          domain.LegID         `json:"id"`
	EventID     domain.EventID       `json:"event_id"`
	MarketID    domain.MarketID      `json:"market_id"`
	MarketType  domain.MarketType    `json:"market_type"`
	SelectionID domain.SelectionID   `json:"selection_id"`
	Role        domain.SelectionRole `json:"role"`

	// BookID is the book that quoted the price, and PriceDecimal is the price
	// itself — the number frozen at placement, which is what the ticket was
	// priced from and what a partial-void repricing divides out.
	BookID       domain.BookID `json:"book_id"`
	PriceDecimal float64       `json:"price_decimal"`

	// PriceLine is the line the QUOTE was made at and GradingLine is the line
	// the leg actually SETTLED at. They differ exactly on a teaser, and both are
	// carried for that reason: the booked line is what line history and CLV are
	// computed against, the grading line is what decided the bet, and a record
	// showing only one of them cannot answer both questions. Absent lines encode
	// as null.
	PriceLine   domain.Line `json:"price_line"`
	GradingLine domain.Line `json:"grading_line"`

	// PriceObservedAt is when the quote was observed — not when the bet was
	// placed. It is what makes "how stale was the price this customer took"
	// answerable about a settled ticket, which is the headline SLO's question
	// asked after the fact.
	PriceObservedAt time.Time `json:"price_observed_at"`

	Status domain.LegStatus `json:"status"`

	// GradedAt is when this leg was graded, from the result's own finalisation
	// instant. Per leg rather than per ticket, because the legs of a parlay
	// grade at different times: they are on different games.
	GradedAt *time.Time `json:"graded_at,omitempty"`
}

// TransactionRecord is the balanced ledger movement, on the wire.
type TransactionRecord struct {
	// ID is the ledger transaction's identifier, derived deterministically from
	// the wager — see [SettlementTransactionID]. A consumer seeing the same id
	// twice is seeing a redelivery, not a second payout.
	ID domain.TransactionID `json:"id"`

	// Kind is why the money moved: payout, loss, refund or cash_out.
	Kind domain.EntryKind `json:"kind"`

	// WagerID is the ticket this movement settles, restated here so an entry
	// list is interpretable if it is ever read on its own.
	WagerID domain.WagerID `json:"wager_id"`

	OccurredAt time.Time `json:"occurred_at"`

	// Entries are the signed halves of the movement. They sum to EXACTLY zero —
	// exact because they are integers, which is why CLAUDE.md §12 puts money in
	// integers in the first place. There are two or three of them: the escrow
	// release is always present, and the cash and house entries drop out when
	// they would be zero, because a zero row is not a movement.
	Entries []EntryRecord `json:"entries"`
}

// EntryRecord is one signed movement against one account.
//
// The account is spelled as (kind, owner) rather than as an identifier because
// that pair IS the account: domain.Account has no surrogate key and the schema
// has no accounts table. A consumer folding these into balances groups on the
// pair, exactly as the account_balances view does.
type EntryRecord struct {
	AccountKind domain.AccountKind `json:"account_kind"`

	// Owner is the customer the account belongs to, absent on the two system
	// singletons (house, issuance).
	Owner domain.UserID `json:"owner,omitempty"`

	// AmountMinor is the signed movement: positive credits the account, negative
	// debits it.
	AmountMinor int64 `json:"amount_minor"`

	// Kind restates the transaction's kind on every entry, matching the schema's
	// own redundancy and for the schema's own reason: an entry that arrives
	// without its transaction is still interpretable, and "sum every payout this
	// month" is a scan rather than a join.
	Kind domain.EntryKind `json:"kind"`
}

// ResultRecord is the event outcome that closed the ticket.
type ResultRecord struct {
	EventID domain.EventID     `json:"event_id"`
	Status  domain.EventStatus `json:"status"`

	// HomeScore and AwayScore are the final score, absent on a cancelled event.
	// They are pointers rather than zero values because 0-0 is a real final in
	// several of the sports in scope, and an audit record that cannot tell a
	// goalless draw from a missing score is one that cannot audit a soccer
	// moneyline.
	HomeScore *int `json:"home_score,omitempty"`
	AwayScore *int `json:"away_score,omitempty"`

	// FinalisedAt is the PROVIDER's observation instant for the terminal status,
	// carried unchanged. It is also every leg's graded_at, which is what makes a
	// replayed settlement re-apply the original instant rather than the wall
	// clock.
	FinalisedAt time.Time `json:"finalised_at"`
}

// Validate reports whether a decoded record is one this build can read and one
// that describes a coherent settlement.
//
// The version check is exact rather than a floor, matching the pricing payload's
// reasoning: a record written by a newer build may have changed the meaning of a
// field this build would read confidently and wrongly. On a money record that is
// not a theoretical concern — a `*_minor` field respelled in major units reads
// as a hundredfold underpayment with no syntax error anywhere.
//
// The zero-sum check is the one that earns its place. It is domain.Transaction's
// own invariant, re-asserted at the point where the type system's guarantee no
// longer reaches: these are decoded JSON integers, not a value that went through
// domain.NewTransaction, and CLAUDE.md §4's whole claim rests on the sum being
// zero.
func (s SettledWager) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("settlement: wager %q: schema version %d, this build reads %d",
			s.Wager.ID, s.SchemaVersion, SchemaVersion)
	}
	if _, err := domain.NewWagerID(string(s.Wager.ID)); err != nil {
		return fmt.Errorf("settlement: settled wager record: %w", err)
	}
	if !s.Wager.Status.IsTerminal() {
		return fmt.Errorf("settlement: wager %q is published as settled but its status is %s",
			s.Wager.ID, s.Wager.Status)
	}
	if len(s.Legs) == 0 {
		return fmt.Errorf("settlement: wager %q is published with no legs; a ticket on nothing "+
			"cannot be audited", s.Wager.ID)
	}
	if len(s.Settlement.Entries) < 2 {
		return fmt.Errorf("settlement: wager %q carries %d ledger entr(ies); a balanced movement "+
			"has at least two", s.Wager.ID, len(s.Settlement.Entries))
	}

	var sum int64
	for _, e := range s.Settlement.Entries {
		sum += e.AmountMinor
	}
	if sum != 0 {
		return fmt.Errorf("settlement: wager %q ledger entries sum to %d minor units, not zero: %w",
			s.Wager.ID, sum, domain.ErrUnbalancedTransaction)
	}
	return nil
}

// newSettledWager assembles the published record from a finished settlement.
//
// Every field is COPIED from the settled domain values rather than recomputed.
// The wager has already been through domain.Wager.Settle, which checked the
// returned amount against the outcome it is filed under, and the transaction has
// already been through domain.NewTransaction, which refused to exist unbalanced.
// Recomputing anything here would be a second implementation of a rule that has
// already been enforced, and the second implementation is the one that would be
// wrong.
func newSettledWager(w domain.Wager, t domain.Transaction, res Result) SettledWager {
	returned, _ := w.Returned()
	net, _ := w.NetReturn()
	settledAt, _ := w.SettledAt()

	rec := SettledWager{
		SchemaVersion: SchemaVersion,
		Wager: WagerRecord{
			ID:                   w.ID(),
			UserID:               w.UserID(),
			Kind:                 w.Kind(),
			Status:               w.Status(),
			StakeMinor:           w.Stake().MinorUnits(),
			AcceptedDecimal:      w.AcceptedDecimal(),
			Rounding:             w.Rounding(),
			PotentialPayoutMinor: w.PotentialPayout().MinorUnits(),
			PotentialProfitMinor: w.PotentialProfit().MinorUnits(),
			ReturnedMinor:        returned.MinorUnits(),
			NetReturnMinor:       net.MinorUnits(),
			PlacedAt:             w.PlacedAt(),
			SettledAt:            settledAt,
		},
		Legs:       legRecords(w),
		Settlement: transactionRecord(t),
		Result:     resultRecord(res),
	}

	if points, ok := w.TeaserPoints(); ok {
		rec.Wager.TeaserPoints = &points
	}
	if parent, ok := w.RoundRobinID(); ok {
		rec.Wager.RoundRobinID = parent.String()
	}
	return rec
}

// legRecords projects every leg of the settled ticket onto the wire shape.
func legRecords(w domain.Wager) []LegRecord {
	legs := w.Legs()
	out := make([]LegRecord, 0, len(legs))
	for _, leg := range legs {
		price := leg.Price()
		rec := LegRecord{
			ID:              leg.ID(),
			EventID:         leg.EventID(),
			MarketID:        leg.MarketID(),
			MarketType:      leg.MarketType(),
			SelectionID:     leg.SelectionID(),
			Role:            leg.Role(),
			BookID:          price.BookID(),
			PriceDecimal:    price.Decimal(),
			PriceLine:       price.Line(),
			GradingLine:     leg.GradingLine(),
			PriceObservedAt: price.ObservedAt(),
			Status:          leg.Status(),
		}
		if at, ok := leg.GradedAt(); ok {
			rec.GradedAt = &at
		}
		out = append(out, rec)
	}
	return out
}

// transactionRecord projects the balanced movement onto the wire shape.
func transactionRecord(t domain.Transaction) TransactionRecord {
	entries := t.Entries()
	out := TransactionRecord{
		ID:         t.ID(),
		Kind:       t.Kind(),
		OccurredAt: t.OccurredAt(),
		Entries:    make([]EntryRecord, 0, len(entries)),
	}
	if id, ok := t.WagerID(); ok {
		out.WagerID = id
	}
	for _, e := range entries {
		account := e.Account()
		rec := EntryRecord{
			AccountKind: account.Kind(),
			AmountMinor: e.Amount().MinorUnits(),
			Kind:        e.Kind(),
		}
		if owner, ok := account.Owner(); ok {
			rec.Owner = owner
		}
		out.Entries = append(out.Entries, rec)
	}
	return out
}

// resultRecord projects the deciding result onto the wire shape.
func resultRecord(res Result) ResultRecord {
	out := ResultRecord{
		EventID:     res.EventID,
		Status:      res.Status,
		FinalisedAt: res.FinalisedAt,
	}
	if res.HasScore {
		home, away := res.Score.Home(), res.Score.Away()
		out.HomeScore, out.AwayScore = &home, &away
	}
	return out
}
