// The record this package publishes to price.computed.
//
// # It is self-contained, for the same reason odds.normalized's record is
//
// kafka/topics.go says why the topic is compacted and keyed by market: "so that
// `stream` can build a client's initial snapshot from the log alone". A record
// that carried only identifiers would force a second lookup and reintroduce the
// ordering problem — a computed price referencing an event nobody has seen yet —
// that the compacted topic was chosen to remove. So the sport, league, event and
// market travel with every record, denormalised on purpose, exactly as
// normalizer/payload.go argues for the topic upstream.
//
// # The catalogue refs are the NORMALIZER's types, deliberately
//
// SportRef, LeagueRef, EventRef and MarketRef are reused verbatim rather than
// re-declared here. They describe the same facts, they are already the canonical
// wire shape for them, and a second declaration is a second declaration that
// drifts — the failure normalizer/raw.go describes for provider mappings ("two
// mappings that must agree eventually stop agreeing, and the disagreement shows
// up as a subtly wrong line rather than as a failure").
//
// The coupling this creates is real and is the price of not drifting: a change
// to one of those four types is a schema change to BOTH topics, and bumping
// normalizer.SchemaVersion obliges a look at [SchemaVersion] here. The fields
// are propagated unchanged, never recomputed, so a computed record's event
// description is byte-identical to the normalized record it came from.
//
// # What it deliberately does NOT carry
//
// A computation timestamp. The engine is a pure function of its input (doc.go),
// so it holds no clock; the envelope's ProducedAt is when this system built the
// record and the record's own ObservedAt is when the provider saw the prices.
// Stamping a third instant here would invite a consumer to measure staleness
// from the wrong one, which is the confusion phase 2 spent a handoff note on.
package pricing

import (
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
)

// SchemaVersion is the version of the ComputedMarket document.
//
// It is versioned independently of kafka.EnvelopeVersion, which versions the
// frame around it — the same split the envelope's own doc describes. Adding an
// optional field is not a bump; removing, renaming, or changing the meaning or
// unit of one is.
const SchemaVersion = 1

// MessageType is the kafka.Message.Type this package publishes. Consumers switch
// on it.
const MessageType = "price.computed.v1"

// Margin is one book's margin triple on the wire.
//
// It restates odds.Margin with explicit snake_case tags, because odds.Margin is
// a pure value type in a package that may not know it is being serialised and
// carries no tags of its own. All three quantities travel together: phase 1 is
// emphatic that booking percentage, overround and vig are distinct, and a wire
// shape that carried one of them under the name "margin" would be the exact
// confusion the three-field struct exists to prevent.
type Margin struct {
	// Selections is n, the number of prices the book quoted.
	Selections int `json:"selections"`
	// ImpliedSum is S = Σ 1/d.
	ImpliedSum float64 `json:"implied_sum"`
	// BookingPercentage is 100·S — the "105% book". The only field that is a
	// percentage.
	BookingPercentage float64 `json:"booking_percentage"`
	// Overround is S − 1, as a fraction.
	Overround float64 `json:"overround"`
	// Vig is (S−1)/S, as a fraction: the hold on a balanced book.
	Vig float64 `json:"vig"`
}

// marginFrom converts the domain value onto the wire shape.
func marginFrom(m odds.Margin) Margin {
	return Margin{
		Selections:        m.Selections,
		ImpliedSum:        m.ImpliedSum,
		BookingPercentage: m.BookingPercentage,
		Overround:         m.Overround,
		Vig:               m.Vig,
	}
}

// ReferenceRef names the book the fair value was derived from, and how it came
// to be chosen.
//
// Source is on the record rather than in a log line because a consumer that
// cannot tell a catalogue-designated reference from a configured fallback cannot
// tell a deliberate trading judgement from a default, and CLAUDE.md §6's +EV
// finder is a claim about the former.
type ReferenceRef struct {
	BookID domain.BookID   `json:"book_id"`
	Slug   domain.Slug     `json:"slug"`
	Name   string          `json:"name"`
	Kind   domain.BookKind `json:"kind"`
	Source ReferenceSource `json:"source"`

	// ObservedAt is the reference book's OLDEST quote instant on this market —
	// the one the staleness policy judged — and AgeSeconds is its age at the
	// record's anchor.
	ObservedAt time.Time `json:"observed_at"`
	AgeSeconds float64   `json:"age_seconds"`
}

// ComputedMarket is one market's no-vig fair value and every book's price
// scored against it.
type ComputedMarket struct {
	// SchemaVersion is SchemaVersion as written.
	SchemaVersion int `json:"schema_version"`

	// Provider is the adapter slug the prices came from — "synthetic" or
	// "the-odds-api". Propagated unchanged so a consumer can tell a simulated
	// price from a real one, which ADR 0003 requires of every surface.
	Provider string `json:"provider"`

	// SourceFingerprint is the normalizer's hash of the record this was computed
	// from. It makes a computed price attributable to an exact market state, and
	// it is what a consumer compares to tell a recomputation from a movement.
	SourceFingerprint string `json:"source_fingerprint"`

	// SourceSchemaVersion is the odds.normalized document version this was
	// computed from. Carried because the catalogue refs below are that
	// document's types; see the file comment.
	SourceSchemaVersion int `json:"source_schema_version"`

	Sport  normalizer.SportRef  `json:"sport"`
	League normalizer.LeagueRef `json:"league"`
	Event  normalizer.EventRef  `json:"event"`
	Market normalizer.MarketRef `json:"market"`

	// Reference is the sharp book this market's fair value came from.
	Reference ReferenceRef `json:"reference"`

	// Fair is the no-vig fair value, one entry per selection.
	Fair FairValue `json:"fair"`

	// Books are every quoting book, scored, ordered by book identifier. The
	// reference book is included and scores itself; ev.go explains why.
	Books []BookAssessment `json:"books"`

	// Arbitrage is every under-round line group found across the books on this
	// record, best return first. Empty on almost every market, which is the
	// correct and expected state — a feed with a constant arbitrage on it is a
	// feed with a bug.
	//
	// It is on the market record rather than on a separate topic because an
	// arbitrage is a property OF THIS MARKET AT THIS INSTANT: the legs are the
	// same quotes Books scores, and splitting them onto two streams would let a
	// consumer render a finding beside prices it was not computed from.
	Arbitrage []ArbitrageRef `json:"arbitrage,omitempty"`

	// Middles is every pair of quotes at DIFFERENT lines leaving a window that
	// wins both, widest window first. Distinct from Arbitrage and never merged
	// with it: a middle can lose, which is exactly the fact a merged list would
	// hide. signals.go and middles.go both labour the point.
	Middles []MiddleRef `json:"middles,omitempty"`

	// ObservedAt is the newest provider observation instant on the source
	// record, propagated unchanged. It is the subtrahend in the staleness SLO
	// and is NOT interchangeable with IngestedAt.
	ObservedAt time.Time `json:"observed_at"`

	// IngestedAt is when ingest received the payload. (IngestedAt − ObservedAt)
	// is the provider-attributable share of staleness.
	IngestedAt time.Time `json:"ingested_at"`
}

// MarketID returns the record's market identifier, validated.
//
// A publisher must key the record from here rather than from the raw string, so
// that kafka.OddsProducer.PublishPrice's typed signature stays a compile-time
// guarantee rather than a convention.
func (c ComputedMarket) MarketID() (domain.MarketID, error) {
	return domain.NewMarketID(c.Market.ID)
}

// Validate reports whether a decoded record is one this build can read and one
// that describes a coherent computation.
//
// The version check is exact rather than a floor, matching
// NormalizedMarket.Domain: a record written by a newer build may have changed
// the meaning of a field this build would read confidently and wrongly.
func (c ComputedMarket) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("pricing: computed market %q: schema version %d, this build reads %d",
			c.Market.ID, c.SchemaVersion, SchemaVersion)
	}
	if _, err := c.MarketID(); err != nil {
		return err
	}
	if !c.Reference.Source.Valid() {
		return fmt.Errorf("pricing: computed market %q: reference source is unset; a fair value with no "+
			"stated provenance cannot be reasoned about", c.Market.ID)
	}
	if !c.Fair.Method.Valid() {
		return fmt.Errorf("pricing: computed market %q: devig method is unset", c.Market.ID)
	}
	if len(c.Fair.Selections) < odds.MinMarketSelections {
		return fmt.Errorf("pricing: computed market %q: %d fair selection(s), need at least %d: %w",
			c.Market.ID, len(c.Fair.Selections), odds.MinMarketSelections, ErrMarketNotPriceable)
	}
	return nil
}

// BestEdge returns the largest expected value across every priced quote on the
// market, and the book offering it.
//
// It is the market-level input to CLAUDE.md §6's positive-EV finder, computed
// here so that the API, the board and phase 9's scanner cannot each derive "the
// best price on this market" slightly differently. Quotes that were not scored —
// stale books, and books quoting a different line from the reference — are not
// candidates; ev.go argues why comparing across a moved line is a category
// error rather than a conservative omission.
func (c ComputedMarket) BestEdge() (BookAssessment, QuoteAssessment, bool) {
	var (
		bestBook  BookAssessment
		bestQuote QuoteAssessment
		found     bool
	)
	for _, b := range c.Books {
		q, ok := b.BestEdge()
		if !ok {
			continue
		}
		if !found || q.ExpectedValue > bestQuote.ExpectedValue {
			bestBook, bestQuote, found = b, q, true
		}
	}
	return bestBook, bestQuote, found
}

// newComputedMarket assembles the published record from a finished computation.
//
// The catalogue refs are copied from the SOURCE RECORD rather than rebuilt from
// the decoded domain values. Rebuilding them would be a second mapping of the
// same facts and would drift from the first; copying makes "the event
// description on price.computed is the event description on odds.normalized" a
// property of the code rather than of a test.
func newComputedMarket(
	rec normalizer.NormalizedMarket,
	snap MarketSnapshot,
	ref referenceCandidate,
	fv FairValue,
	books []BookAssessment,
	attribution odds.Attribution,
) ComputedMarket {
	fv.Attribution = attribution.String()
	return ComputedMarket{
		SchemaVersion:       SchemaVersion,
		Provider:            rec.Provider,
		SourceFingerprint:   rec.Fingerprint,
		SourceSchemaVersion: rec.SchemaVersion,
		Sport:               rec.Sport,
		League:              rec.League,
		Event:               rec.Event,
		Market:              rec.Market,
		Reference: ReferenceRef{
			BookID:     ref.state.Book.ID(),
			Slug:       ref.state.Book.Slug(),
			Name:       ref.state.Book.Name(),
			Kind:       ref.state.Book.Kind(),
			Source:     ref.source,
			ObservedAt: ref.state.OldestAt,
			AgeSeconds: ref.state.Age(snap.Anchor()).Seconds(),
		},
		Fair:       fv,
		Books:      books,
		ObservedAt: rec.ObservedAt,
		IngestedAt: rec.IngestedAt,
	}
}
