// The engine's view of one market: every book's current quote on it, indexed so
// the arithmetic can address a book and a selection without searching.
//
// # This is what "market state" means here, and why it is not a cache
//
// The phase brief framed the problem as "pricing needs all books' current prices
// for one market at once, but they arrive as separate records". That is true of
// the RAW topic, which is keyed by event and carries whatever one poll returned.
// It is not true of odds.normalized: normalizer/mapper.go groups a payload's
// quotes into one record PER MARKET carrying every book that quoted it, and
// normalizer/payload.go states the design goal outright — "a consumer that
// builds its state from the compacted log alone […] must be able to render a
// board from the log and nothing else".
//
// So one record is already a complete pricing input, and the compacted topic is
// already the board. A [MarketSnapshot] is that record decoded, validated
// through the domain constructors and indexed; it is built per record and thrown
// away, not accumulated.
//
// A persistent in-process store of every market was considered and rejected.
// CLAUDE.md §3 chose Kafka over NATS partly because "a compacted topic keyed by
// market_id IS the current-line snapshot […] which removes a whole class of
// cache-coherency bugs between the bus and Redis", and a second in-process copy
// of that snapshot reintroduces the same class in a different address space. It
// would also make an HPA'd service stateful: CLAUDE.md §9 puts an autoscaler on
// `pricer`, so a replica holding state no other replica shares turns scaling
// from a capacity decision into a correctness one. Redis is rejected here for
// exactly the reasons normalizer/doc.go rejects it for fingerprints, and the
// legitimate future use — a store SHARED across replicas after a rebalance — is
// a different feature from a per-process cache.
//
// # The staleness anchor is on the record, not on the clock
//
// Every judgement this file makes about age is measured from an instant the
// RECORD carries, never from time.Now. That is a requirement, not a preference:
// doc.go makes the engine a pure function of the record so the service can
// suppress a republication whose input did not change, and an engine that read
// the wall clock would make two calls over identical input disagree.
//
// The anchor is IngestedAt — "when ingest received the payload this record was
// derived from". Measuring a quote's age from it yields exactly the quantity
// migrations/00003 and the dashboard both call the provider-attributable share,
// (ingested_at − observed_at), which is a property of the data. Measuring from
// the wall clock instead would fold in bus lag and consumer lag, so the same
// market would be eligible on a quiet system and disqualified on a backed-up
// one — the pricing would depend on the health of the pipeline rather than on
// the freshness of the price.
package pricing

import (
	"fmt"
	"slices"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
)

// BookQuote is one book's quote on one selection.
//
// It is the domain price restated in the odds package's types, because that is
// what every function this engine calls expects: domain.Price.Decimal() returns
// a bare float64 and odds.Decimal is the canonical priced type (phase-1
// handoff). Converting once here means the arithmetic downstream never converts
// and can never convert differently.
type BookQuote struct {
	SelectionID domain.SelectionID

	// Decimal is the quoted price. Validated by odds.NewDecimal at construction,
	// so every consumer may use it without re-validating.
	Decimal odds.Decimal

	// Implied is 1/Decimal: the probability the quoted price implies, margin
	// included. Computed once, through the package's single definition, so no
	// caller derives it a second way.
	Implied odds.Probability

	// Line is the line THIS BOOK quoted, from this selection's own perspective.
	// It is not necessarily the market's consensus line; multi-book comparison
	// (CLAUDE.md §6) exists to show that disagreement, and middles detection
	// depends on it.
	Line domain.Line

	// ObservedAt is the provider's observation instant for this quote. It is
	// propagated unchanged from the record and is never re-stamped.
	ObservedAt time.Time
}

// BookState is one book's complete view of one market.
type BookState struct {
	// Book is the bookmaker, rebuilt through domain.NewBook.
	Book domain.Book

	// Quotes is one entry per selection the book priced, in the market's
	// selection order. A book that did not quote a selection has no entry —
	// there is no zero-valued placeholder, because a missing quote and a quote
	// of zero are different facts and only one of them is representable.
	Quotes []BookQuote

	// Complete reports whether the book quoted every selection on the market.
	//
	// It gates devigging and nothing else. The margin is the excess of Σ 1/d
	// over 1, so it is only defined over a whole market: a two-way book missing
	// one side sums to well under 1 and would "devig" to a fabricated
	// near-certainty on the side it did quote. An incomplete book may still be
	// displayed and may still be scored for EV against someone else's fair
	// value; it simply cannot produce one.
	Complete bool

	// NewestAt and OldestAt bracket this book's quotes in time. OldestAt is the
	// one the staleness policy judges, because a book whose market is half fresh
	// and half an hour old is not a book whose market is fresh.
	NewestAt time.Time
	OldestAt time.Time
}

// Quote returns the book's quote on one selection.
func (b BookState) Quote(id domain.SelectionID) (BookQuote, bool) {
	for _, q := range b.Quotes {
		if q.SelectionID == id {
			return q, true
		}
	}
	return BookQuote{}, false
}

// Decimals returns the book's prices in the market's selection order.
//
// It returns false unless the book is Complete, so a caller cannot accidentally
// devig a partial market — the slice a partial book would produce is shorter
// than the market and would devig without complaint.
func (b BookState) Decimals() ([]odds.Decimal, bool) {
	if !b.Complete {
		return nil, false
	}
	out := make([]odds.Decimal, len(b.Quotes))
	for i, q := range b.Quotes {
		out[i] = q.Decimal
	}
	return out, true
}

// Age returns this book's staleness relative to an anchor instant: the age of
// its OLDEST quote. A negative result means the provider's clock ran ahead of
// the anchor and is returned as such rather than clamped, exactly as
// domain.Price.Age does, so a monitor can see the skew instead of reading a
// clamped zero as health.
func (b BookState) Age(anchor time.Time) time.Duration {
	return anchor.Sub(b.OldestAt)
}

// MarketSnapshot is one market and every book's current quote on it, decoded
// from a normalizer record and validated through the domain constructors.
//
// It is immutable once built: nothing in this package mutates one, and the
// slices are freshly allocated per construction, so a snapshot may be handed to
// the arbitrage and middles scanners concurrently with the fair-value pass.
type MarketSnapshot struct {
	// Provider is the adapter slug the record came from — "synthetic" or
	// "the-odds-api". ADR 0003 requires a consumer be able to tell, so it is
	// carried through to the computed record rather than dropped here.
	Provider string

	// Fingerprint is the normalizer's hash of the source record. It is the
	// identity of the INPUT this snapshot was built from, which is what makes a
	// computed price attributable to a specific market state.
	Fingerprint string

	Sport      domain.Sport
	League     domain.League
	Event      domain.Event
	Market     domain.Market
	Selections []domain.Selection

	// Books are the quoting books, ordered by book identifier.
	//
	// Sorted rather than left in record order because record order is whatever
	// the provider listed, and a computed payload whose book order changed
	// between two identical inputs would make every diff, golden file and
	// eyeball comparison noisy for no reason.
	Books []BookState

	// ObservedAt is the newest observation instant on the record.
	ObservedAt time.Time

	// IngestedAt is when ingest received the payload. See the file comment: it
	// is the staleness anchor, and it is NOT interchangeable with ObservedAt.
	IngestedAt time.Time
}

// Anchor returns the instant ages are measured from.
//
// IngestedAt when the record carries one; otherwise the newest observation on
// the record, which makes every age relative — a book's lag behind the freshest
// book on the same market. The fallback is not hypothetical: a record written
// before IngestedAt existed on the payload still decodes, and a snapshot that
// answered with a zero anchor would report every quote as 55 years stale and
// disqualify the entire board.
func (s MarketSnapshot) Anchor() time.Time {
	if !s.IngestedAt.IsZero() {
		return s.IngestedAt
	}
	return s.ObservedAt
}

// Book returns the state of one book on this market.
func (s MarketSnapshot) Book(id domain.BookID) (BookState, bool) {
	for _, b := range s.Books {
		if b.Book.ID() == id {
			return b, true
		}
	}
	return BookState{}, false
}

// Priceable reports whether the market has enough selections for the margin to
// be defined at all. odds.MinMarketSelections is two.
func (s MarketSnapshot) Priceable() bool {
	return len(s.Selections) >= odds.MinMarketSelections
}

// NewMarketSnapshot decodes and indexes one normalizer record.
//
// It goes through NormalizedMarket.Domain rather than reading the wire structs,
// which normalizer/payload.go requires of every consumer and gives the reason:
// "a record that has been on a compacted topic for a month was written by an
// older build; running it back through the constructors is what catches the case
// where it no longer satisfies today's invariants".
//
// The record's own book, selection and price collections are cross-checked
// rather than trusted. A price naming a book or a selection the record does not
// carry is a corrupt record, not a recoverable one: silently dropping it would
// change a market's booking percentage without anything saying so, and the
// margin is the one number every other number here is derived from.
func NewMarketSnapshot(rec normalizer.NormalizedMarket) (MarketSnapshot, error) {
	view, err := rec.Domain()
	if err != nil {
		return MarketSnapshot{}, fmt.Errorf("pricing: decode market %q: %w", rec.Market.ID, err)
	}

	s := MarketSnapshot{
		Provider:    rec.Provider,
		Fingerprint: rec.Fingerprint,
		Sport:       view.Sport,
		League:      view.League,
		Event:       view.Event,
		Market:      view.Market,
		Selections:  view.Selections,
		ObservedAt:  rec.ObservedAt,
		IngestedAt:  rec.IngestedAt,
	}

	// Index the selections so a price naming an unknown one is caught rather
	// than quietly discarded.
	selIndex := make(map[domain.SelectionID]int, len(view.Selections))
	for i, sel := range view.Selections {
		if _, dup := selIndex[sel.ID()]; dup {
			return MarketSnapshot{}, fmt.Errorf("pricing: market %s lists selection %s twice: %w",
				view.Market.ID(), sel.ID(), ErrMarketNotPriceable)
		}
		selIndex[sel.ID()] = i
	}

	bookIndex := make(map[domain.BookID]int, len(view.Books))
	states := make([]BookState, 0, len(view.Books))
	for _, b := range view.Books {
		if _, dup := bookIndex[b.ID()]; dup {
			return MarketSnapshot{}, fmt.Errorf("pricing: market %s lists book %s twice: %w",
				view.Market.ID(), b.ID(), ErrMarketNotPriceable)
		}
		bookIndex[b.ID()] = len(states)
		states = append(states, BookState{Book: b, Quotes: make([]BookQuote, 0, len(view.Selections))})
	}

	// A book may legitimately quote one selection twice across two polls folded
	// into one record; the normalizer does not deduplicate, so the LAST quote by
	// observation instant wins. Tracked per (book, selection) rather than
	// resolved afterwards so the winner is decided by the provider's clock and
	// not by slice order.
	type slot struct{ bi, qi int }
	seen := make(map[[2]string]slot, len(view.Prices))

	for _, p := range view.Prices {
		bi, ok := bookIndex[p.BookID()]
		if !ok {
			return MarketSnapshot{}, fmt.Errorf("pricing: market %s carries a price from book %s "+
				"which the record does not list: %w", view.Market.ID(), p.BookID(), ErrMarketNotPriceable)
		}
		if _, ok := selIndex[p.SelectionID()]; !ok {
			return MarketSnapshot{}, fmt.Errorf("pricing: market %s carries a price on selection %s "+
				"which the record does not list: %w", view.Market.ID(), p.SelectionID(), ErrMarketNotPriceable)
		}

		d, err := odds.NewDecimal(p.Decimal())
		if err != nil {
			// Unreachable through domain.NewPrice, which applies the same
			// bounds. Checked anyway: an invalid decimal reaching the devig
			// would poison a whole market's fair value, and the two validations
			// are in different packages and can drift apart.
			return MarketSnapshot{}, fmt.Errorf("pricing: market %s, book %s, selection %s: %w",
				view.Market.ID(), p.BookID(), p.SelectionID(), err)
		}
		implied, err := d.Probability()
		if err != nil {
			return MarketSnapshot{}, fmt.Errorf("pricing: market %s, book %s, selection %s: %w",
				view.Market.ID(), p.BookID(), p.SelectionID(), err)
		}

		q := BookQuote{
			SelectionID: p.SelectionID(),
			Decimal:     d,
			Implied:     implied,
			Line:        p.Line(),
			ObservedAt:  p.ObservedAt(),
		}

		key := [2]string{string(p.BookID()), string(p.SelectionID())}
		if prev, dup := seen[key]; dup {
			if q.ObservedAt.After(states[prev.bi].Quotes[prev.qi].ObservedAt) {
				states[prev.bi].Quotes[prev.qi] = q
			}
			continue
		}
		seen[key] = slot{bi: bi, qi: len(states[bi].Quotes)}
		states[bi].Quotes = append(states[bi].Quotes, q)
	}

	for i := range states {
		finaliseBook(&states[i], view.Selections, selIndex)
	}

	// Drop books that ended up quoting nothing. The normalizer lists a book on a
	// record because it appeared in the payload, which is not the same as it
	// having quoted THIS market.
	states = slices.DeleteFunc(states, func(b BookState) bool { return len(b.Quotes) == 0 })

	slices.SortFunc(states, func(a, b BookState) int {
		return cmpBookID(a.Book.ID(), b.Book.ID())
	})
	s.Books = states
	return s, nil
}

// finaliseBook orders one book's quotes into the market's selection order and
// computes its completeness and time bracket.
func finaliseBook(b *BookState, sels []domain.Selection, index map[domain.SelectionID]int) {
	slices.SortFunc(b.Quotes, func(x, y BookQuote) int {
		return index[x.SelectionID] - index[y.SelectionID]
	})
	b.Complete = len(b.Quotes) == len(sels)
	for i, q := range b.Quotes {
		if i == 0 || q.ObservedAt.Before(b.OldestAt) {
			b.OldestAt = q.ObservedAt
		}
		if q.ObservedAt.After(b.NewestAt) {
			b.NewestAt = q.ObservedAt
		}
	}
}

// cmpBookID orders book identifiers. A helper rather than an inline closure so
// every sort over books in this package uses one comparison.
func cmpBookID(a, b domain.BookID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
