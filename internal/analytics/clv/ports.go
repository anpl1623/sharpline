// The two reads this package needs, and the neutral values that cross them.
//
// Both interfaces are declared BY THE CONSUMER (CLAUDE.md §12), which is this
// package. internal/analytics/clv/pgclv implements them over the generated
// queries and does not appear in this package's import graph, which is what lets
// the closing-price rules in doc.go be asserted against an in-memory store in a
// unit test and against a real TimescaleDB in the integration tier with the same
// code under test.
//
// # Why [Store] is two methods and not one
//
// They are two different reads with two different failure meanings. A market with
// no row at all is a referential problem — the leg names a market that does not
// exist — and is an error. A market whose snapshot comes back short is an
// ordinary, expected outcome that produces no measurement and no error at the
// storage layer. Folding them into one call would force the adapter to decide
// which of those it was looking at, and that decision belongs in clv.go where
// the reasons are enumerated.
package clv

import (
	"context"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// Leg is one GRADED leg, as closing line value needs it.
//
// It is deliberately not domain.Leg. A leg carries the teased line, the graded
// multiplier, the parlay's rounding mode and a status machine, none of which CLV
// has any use for; and it does NOT carry the user, the league or the market type
// as separate facts, all three of which have to reach the stored row. This is the
// projection of the work-queue query, and it is the same value the settle
// service's CLV pass hands back to be published.
type Leg struct {
	// LegID identifies the leg and is the primary key of the row this produces.
	LegID domain.LegID

	// WagerID is the ticket the leg is on. It is also the bus key: signals.clv is
	// keyed by wager so a wager's placement, settlement and CLV stay ordered on
	// one partition (internal/platform/kafka/topics.go).
	WagerID domain.WagerID

	// UserID owns the wager. It is denormalised onto the stored row so the
	// leaderboard does not join through `wagers` on every read, and
	// migrations/00009 records honestly that the pairing is a WRITER OBLIGATION
	// rather than a composite foreign key — `wagers` has no UNIQUE (id, user_id)
	// for one to point at. This field is where that obligation is discharged, so
	// it must come from the same row as WagerID and never from anywhere else.
	UserID domain.UserID

	// EventID is the contest. CLV does not use it; it travels because the
	// published record carries it, and re-reading it later would be a second
	// query for a fact this row already held.
	EventID domain.EventID

	// MarketID and MarketType are the market the leg answers. MarketType decides
	// how a quote's line is converted into the market's own frame (see
	// marketLine), and it is also what migrations/00009's line rule is checked
	// against.
	MarketID   domain.MarketID
	MarketType domain.MarketType

	// SelectionID is the outcome the customer took. It is the component of the
	// two devigged distributions that is actually compared.
	SelectionID domain.SelectionID

	// Book is the book that quoted the price. It is BOTH sides' book: doc.go §3
	// fixes the close at the same book the wager was struck at.
	Book domain.BookID

	// Decimal is the price the leg was booked at, before any devig. It is not
	// used in the arithmetic — the comparison is between FAIR probabilities — and
	// is carried so the published record can show what the customer actually
	// took beside what it was worth.
	Decimal odds.Decimal

	// ObservedAt is when that quote was seen, from the provider's clock. It is
	// the as-of bound of the taken snapshot and the instant the reconstructed
	// quote for SelectionID must match exactly; see doc.go §6.
	ObservedAt time.Time

	// Status is the leg's terminal grading. Never [domain.LegStatusPending]: an
	// ungraded leg has nothing to measure and migrations/00009's leg_status
	// CHECK refuses the value. `void` sets the stored `voided` flag, which
	// odds.AggregateCLV excludes; `push` does NOT, and is ranked at full weight.
	Status domain.LegStatus

	// GradedAt is when the leg was graded — the RESULT's own finalisation
	// instant, carried unchanged from settlement. It is the work queue's ordering
	// key and the event time stamped on the published record.
	GradedAt time.Time
}

// Market is one market's identity and the instant it closes.
//
// It is what [Store.MarketClose] answers with, and every field on it is needed
// either to bound the closing query or to fill a column of the stored row.
type Market struct {
	// MarketID and MarketType restate the market. MarketType is read back from
	// the market rather than trusted from the leg, so that a leg whose stored
	// market_type has drifted from the market's own is caught rather than
	// measured under the wrong line rule.
	MarketID   domain.MarketID
	MarketType domain.MarketType

	// EventID and EventStatus are the contest and where it has got to.
	// EventStatus is consulted for exactly one thing: an event that never
	// started has no close, however plausible its scheduled_start looks.
	EventID     domain.EventID
	EventStatus domain.EventStatus

	// LeagueID is denormalised onto the stored row so the per-league CLV
	// breakdown is a scan rather than a three-table join.
	LeagueID domain.LeagueID

	// ScheduledStart is THE CLOSING INSTANT. doc.go §1 carries the argument for
	// why it is this column and not the actual kickoff, the status transition, or
	// the last observation on the market.
	ScheduledStart time.Time
}

// SnapshotRequest names one market, one book and one instant, with a required
// lower bound.
//
// The same request shape builds both sides of a comparison; doc.go §6 explains
// why one rule for both is the property that makes them comparable.
type SnapshotRequest struct {
	// Market is the market to price.
	Market domain.MarketID

	// Book is the single book the whole outcome set must come from. A snapshot
	// assembled across books has no margin to remove and cannot be devigged.
	Book domain.BookID

	// AsOf is the upper bound, INCLUSIVE: a quote observed exactly at AsOf
	// counts. On the closing side it is [Market.ScheduledStart]; on the taken
	// side it is [Leg.ObservedAt], and the inclusiveness is what makes the leg's
	// own quote eligible for its own snapshot.
	AsOf time.Time

	// NotBefore is the lower bound, EXCLUSIVE, and it is REQUIRED. A zero value
	// is a programming error rather than "no bound": doc.go §2 gives both the
	// mechanical reason (an unbounded backward walk consults every chunk the
	// hypertable has ever had) and the semantic one (a quote from six days out is
	// not a closing line).
	NotBefore time.Time
}

// Quote is one selection's eligible price inside a snapshot.
type Quote struct {
	// Selection is the outcome.
	Selection domain.SelectionID

	// Role decides how Line is converted into the market's frame. It is the
	// selection's stored role, not a guess from the ordering.
	Role domain.SelectionRole

	// Decimal is the quoted price, margin included. It is devigged before it is
	// compared to anything.
	Decimal odds.Decimal

	// Line is the handicap or threshold, in the SELECTION's own frame — already
	// inverted for the away side of a spread, exactly as domain.Price stores it
	// and domain.EffectiveLine defines it. marketLine converts it back.
	Line domain.Line

	// ObservedAt is the provider's instant for this quote. The snapshot's own
	// instant is the maximum of these; see doc.go §8.
	ObservedAt time.Time
}

// Snapshot is what a store returned for one [SnapshotRequest]: the eligible
// quotes, and how many selections the market actually has.
//
// COMPLETENESS IS THE CALLER'S CHECK AND IT IS NOT OPTIONAL. The underlying
// query is an inner join, so a selection with no eligible quote produces no row
// at all — meaning a store CANNOT report incompleteness by returning an error,
// and a caller that only looked at len(Quotes) would happily devig a subset. The
// count travels for exactly that reason.
type Snapshot struct {
	// Quotes are the eligible prices, one per selection that had one, in
	// selection-id order. The order is fixed rather than incidental: the devig is
	// a floating-point reduction over this slice, so a stable order is what makes
	// a replay reproduce a stored row bit for bit.
	Quotes []Quote

	// MarketSelections is how many selections the market has. When it exceeds
	// len(Quotes) the snapshot is INCOMPLETE and must be discarded whole — not
	// devigged as a subset, not retried at another book.
	MarketSelections int
}

// Complete reports whether every selection of the market was priced.
func (s Snapshot) Complete() bool {
	return s.MarketSelections > 0 && len(s.Quotes) == s.MarketSelections
}

// Store is the two reads this package needs.
//
// Both are pure reads with no side effects, so a caller may retry either freely.
// Neither opens a transaction: the two snapshots are read independently and a
// price row is immutable once written (migrations/00003 — "a new price is a new
// row"), so there is nothing a shared snapshot isolation would protect.
type Store interface {
	// MarketClose returns the market's identity and its closing instant.
	//
	// It returns an error wrapping [ErrMarketNotFound] when no such market
	// exists. That is genuinely exceptional — the identifier came off a leg row
	// and legs.market_id is a foreign key — and is deliberately NOT one of the
	// unmeasurable reasons: a dangling reference is a defect in the data plane,
	// not a market that happens to lack a close.
	MarketClose(ctx context.Context, id domain.MarketID) (Market, error)

	// Snapshot returns every eligible quote for req, plus the market's selection
	// count so the caller can judge completeness.
	//
	// A market with NO eligible quotes returns an empty Quotes slice, a positive
	// MarketSelections and a nil error. That is an ordinary outcome — a market
	// suspended through the whole lookback window has exactly this shape — and it
	// is not a not-found condition.
	Snapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error)
}
