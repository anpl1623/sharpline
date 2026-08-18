// The `odds.normalized` wire contract, and its conversion into domain values.
//
// # The contract is normalizer.NormalizedMarket, and it is not respelled here
//
// The normalizer is the producer; this package is one of the consumers. CLAUDE.md
// §12 says interfaces are declared by the consumer — but a WIRE FORMAT cannot be,
// because both sides must agree on one spelling of every field name. So the
// producer's type is the contract and this package decodes THAT type, rather than
// maintaining a parallel set of structs that happen to describe the same JSON.
//
// This file used to hold that parallel set, and the parallel set had drifted:
// the producer published `prices`, `market.updated_at` and no event instant at
// all; the copy here read `quotes`, `market.observed_at` and a required
// `event.observed_at`. Nothing caught it, because each side's tests marshalled
// and unmarshalled its OWN struct and round-tripped perfectly. It surfaced only
// against a live broker, as every record on the topic being rejected with
// "timestamp is the zero time" and an empty prices hypertable behind a board
// that looked healthy. Two spellings of one contract is the defect; deleting one
// of them is the fix.
//
// Two properties make the contract cheap to hold, and they belong to the
// producer's type:
//
//   - EVERY FIELD IS EXPRESSED IN DOMAIN VOCABULARY, and
//     normalizer.NormalizedMarket.Domain() is the single function that turns the
//     wire form into domain values. Both consumers call it, so "two providers
//     produce identical domain values for equivalent input" stays a property of
//     the architecture rather than of a test.
//   - EVERY PAYLOAD IS RUN THROUGH domain's CONSTRUCTORS before any SQL is
//     emitted. The database's CHECK constraints were written from those same
//     rules — migrations/00002 enforces MarketType.LineRule() and AllowsRole() as
//     CHECKs — so a payload that satisfies the domain cannot fail a constraint,
//     and one that does not is rejected here with a domain error naming the field
//     rather than as a SQLSTATE from three layers down.
//
// # What this file still owns
//
// Domain() validates each value in isolation. It does not, and should not, know
// what a STORABLE record requires. The integrity rules below are this package's,
// because they are exactly the invariants the schema's keys enforce:
//
//   - the record key is the market the payload describes (compaction acts on the
//     key, so a mismatch silently stores one market's snapshot under another's);
//   - the catalogue chain is internally consistent (market → event → league →
//     sport), so a foreign key can never be the thing that reports a mismatch;
//   - nothing referenced is undeclared, so a quote naming an absent selection
//     fails here by name instead of as a 23503 three layers down;
//   - no natural key repeats inside one record, because ON CONFLICT refuses to
//     touch a row twice in one statement (SQLSTATE 21000) and DO NOTHING would
//     silently keep whichever of two conflicting prices arrived first.
//
// # Why a record carries the whole spine
//
// The topic is compacted and keyed by market, so the value must be the complete
// current state of that market: compaction discards everything a later record
// omits. The catalogue chain (sport → league → event → market → selection) rides
// along because `prices` has foreign keys to `selections` and `books` and cannot
// be written before they exist. See doc.go.
//
// # Adding a field is not a version bump; changing one is
//
// internal/platform/kafka's Envelope states the rule: encoding/json ignores
// unknown fields, so an ADDED optional field is transparent to an old consumer.
// Removing, renaming, or changing the meaning or unit of a field is a change to
// Envelope.Type ("odds.normalized.v1" → v2) and must be handled by decoding both
// for as long as v1 records survive on the compacted log.
package writer

import (
	"errors"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
)

// MessageType is the Envelope.Type this package accepts.
//
// It is checked rather than assumed. `odds.normalized` is a compacted topic
// whose whole purpose is to be replayable from scratch, so a record of an
// unexpected shape must be a loud failure and not a silently empty struct.
//
// It is deliberately the same string as normalizer.MessageType, and
// TestMessageTypeMatchesTheProducer asserts the two agree — a consumer that
// silently accepted a type the producer never sends would sit at zero throughput
// and look healthy.
const MessageType = "odds.normalized.v1"

// Record is the JSON value of one `odds.normalized` record: the complete current
// state of one market, as the normalizer publishes it.
//
// It is an alias for the producer's type, not a copy. The name exists so call
// sites in this package read as "the record we were handed" without importing
// the producer's vocabulary wholesale, and so that the day a v2 arrives there is
// one identifier here to point at the other type.
type Record = normalizer.NormalizedMarket

// Errors this package returns for a payload it cannot use. All are permanent:
// redelivering the same bytes cannot change the outcome.
var (
	// ErrWrongMessageType is returned for a record whose Envelope.Type is not
	// MessageType.
	ErrWrongMessageType = errors.New("writer: unexpected message type")

	// ErrKeyMismatch is returned when the record key is not the market the
	// payload describes. The key is what compaction and partitioning act on, so
	// a mismatch means one market's snapshot would be stored under another's
	// key and would be deleted by the next write to either.
	ErrKeyMismatch = errors.New("writer: record key does not match the payload market")

	// ErrIncompletePayload is returned when the snapshot omits something a row
	// depends on — a quote whose selection is not in the record, a book that is
	// not declared, an event that is not the market's parent.
	ErrIncompletePayload = errors.New("writer: payload is not a complete market snapshot")
)

// -----------------------------------------------------------------------------
// Conversion to domain values
// -----------------------------------------------------------------------------

// snapshot is a Record that has passed every domain constraint and every
// integrity rule above: the resolved, immutable values the SQL in catalogue.go
// and prices.go writes.
//
// It is unexported because it is an internal staging form. What callers hand in
// is a Record; what comes out is rows.
type snapshot struct {
	provider   string
	ingestedAt time.Time

	sport      domain.Sport
	league     domain.League
	event      domain.Event
	market     domain.Market
	selections []domain.Selection
	books      []domain.Book
	prices     []domain.Price
}

// resolve validates a Record against internal/domain and returns the values to
// be written.
//
// key is the record key. It is checked against the payload rather than trusted,
// because the key is what compaction and partition assignment act on: a
// mismatch means this market's snapshot lives under another market's key and
// will be deleted by the next write to either of them. That is a normalizer bug
// which is invisible until a snapshot rebuild comes back wrong, so it is caught
// at the first hop that can see both values.
//
// Every failure here is PERMANENT. Redelivering the same bytes produces the same
// error, so a caller must not treat it as retryable.
func resolve(r Record, key domain.MarketID) (snapshot, error) {
	var s snapshot

	if r.IngestedAt.IsZero() {
		return s, fmt.Errorf("%w: ingested_at is zero", ErrIncompletePayload)
	}

	// One decoder for the whole spine. Every per-value rule — identifier syntax,
	// enum membership, the line legality for the market type, whether a role is
	// allowed on this market — is applied by the producer's own conversion, so
	// the two consumers of this topic cannot disagree about what a record means.
	view, err := r.Domain()
	if err != nil {
		return s, fmt.Errorf("writer: %w", err)
	}

	if key != view.Market.ID() {
		return s, fmt.Errorf("%w: key %q, payload market %q", ErrKeyMismatch, key, view.Market.ID())
	}

	// The catalogue chain, checked here rather than left to the foreign keys.
	// A 23503 names a constraint; these name the two identifiers that disagree.
	if !view.Market.BelongsTo(view.Event) {
		return s, fmt.Errorf("%w: market %s names event %s but the payload carries event %s",
			ErrIncompletePayload, view.Market.ID(), view.Market.EventID(), view.Event.ID())
	}
	if view.Event.LeagueID() != view.League.ID() {
		return s, fmt.Errorf("%w: event %s names league %s but the payload carries league %s",
			ErrIncompletePayload, view.Event.ID(), view.Event.LeagueID(), view.League.ID())
	}
	if view.League.SportID() != view.Sport.ID() {
		return s, fmt.Errorf("%w: league %s names sport %s but the payload carries sport %s",
			ErrIncompletePayload, view.League.ID(), view.League.SportID(), view.Sport.ID())
	}

	if s.selections, err = checkSelections(view.Selections, view.Market); err != nil {
		return snapshot{}, err
	}
	if s.books, err = checkBooks(view.Books); err != nil {
		return snapshot{}, err
	}
	if s.prices, err = checkPrices(view.Prices, s.selections, s.books); err != nil {
		return snapshot{}, err
	}

	s.provider = r.Provider
	s.ingestedAt = r.IngestedAt.UTC()
	s.sport = view.Sport
	s.league = view.League
	s.event = view.Event
	s.market = view.Market
	return s, nil
}

// checkSelections rejects an empty or self-conflicting selection set.
//
// The per-selection domain rules — including ValidateSelectionForMarket, the
// cross-check domain.NewSelection cannot do alone and the same rule the
// selections_role_allowed CHECK encodes — were already applied by Domain(). What
// is left is the set-level rule the schema cares about.
func checkSelections(sels []domain.Selection, m domain.Market) ([]domain.Selection, error) {
	if len(sels) == 0 {
		return nil, fmt.Errorf("%w: market %s carries no selections", ErrIncompletePayload, m.ID())
	}
	seen := make(map[domain.SelectionID]struct{}, len(sels))
	for _, sel := range sels {
		if _, dup := seen[sel.ID()]; dup {
			// Not merely wasteful: the selections upsert is one multi-row
			// statement, and Postgres refuses to let ON CONFLICT DO UPDATE
			// affect a row twice (SQLSTATE 21000). Catching it here names the
			// offending id.
			return nil, fmt.Errorf("%w: selection %s appears twice", ErrIncompletePayload, sel.ID())
		}
		seen[sel.ID()] = struct{}{}
	}
	return sels, nil
}

// checkBooks rejects an empty or self-conflicting book set.
func checkBooks(books []domain.Book) ([]domain.Book, error) {
	if len(books) == 0 {
		return nil, fmt.Errorf("%w: no books declared", ErrIncompletePayload)
	}
	seen := make(map[domain.BookID]struct{}, len(books))
	for _, b := range books {
		if _, dup := seen[b.ID()]; dup {
			return nil, fmt.Errorf("%w: book %s appears twice", ErrIncompletePayload, b.ID())
		}
		seen[b.ID()] = struct{}{}
	}
	return books, nil
}

// checkPrices proves every quote is reachable from this record alone.
//
// The two membership checks are what make the snapshot self-contained. Without
// them a quote naming a selection or a book the record does not declare would
// reach Postgres and fail on a foreign key — correctly, but as a 23503 with no
// indication of which of the two references was the missing one, and only if
// that row happened never to have been written by some earlier record.
//
// Duplicate natural keys within one record are rejected rather than deduplicated.
// ON CONFLICT DO NOTHING would absorb them silently, and two different prices for
// the same (selection, book, instant) in one snapshot is a normalizer defect —
// the second one would be discarded by whichever arrived first, which is
// arbitrary. Redelivery of a whole record is a different situation and IS
// absorbed; see doc.go.
func checkPrices(prices []domain.Price, sels []domain.Selection, books []domain.Book) ([]domain.Price, error) {
	if len(prices) == 0 {
		return nil, fmt.Errorf("%w: no quotes", ErrIncompletePayload)
	}

	known := make(map[domain.SelectionID]struct{}, len(sels))
	for _, s := range sels {
		known[s.ID()] = struct{}{}
	}
	quoting := make(map[domain.BookID]struct{}, len(books))
	for _, b := range books {
		quoting[b.ID()] = struct{}{}
	}

	type naturalKey struct {
		selection  domain.SelectionID
		book       domain.BookID
		observedAt time.Time
	}

	seen := make(map[naturalKey]struct{}, len(prices))
	for i, price := range prices {
		if _, ok := known[price.SelectionID()]; !ok {
			return nil, fmt.Errorf("%w: quotes[%d] prices selection %s, which the record does not declare",
				ErrIncompletePayload, i, price.SelectionID())
		}
		if _, ok := quoting[price.BookID()]; !ok {
			return nil, fmt.Errorf("%w: quotes[%d] is from book %s, which the record does not declare",
				ErrIncompletePayload, i, price.BookID())
		}

		// price.ObservedAt() is already UTC-normalised by NewPrice, so the key
		// compares instants and not zone representations.
		k := naturalKey{price.SelectionID(), price.BookID(), price.ObservedAt()}
		if _, dup := seen[k]; dup {
			return nil, fmt.Errorf("%w: two quotes for selection %s at book %s at %s",
				ErrIncompletePayload, k.selection, k.book, k.observedAt.Format(time.RFC3339Nano))
		}
		seen[k] = struct{}{}
	}
	return prices, nil
}
