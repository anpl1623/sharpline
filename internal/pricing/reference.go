// Choosing the sharp book, and saying which way it was chosen.
//
// CLAUDE.md §6 makes the analytics differentiator a "Positive-EV finder against
// a sharp reference book". doc.go argues why that cannot be a consensus. This
// file is the mechanism, and the interesting part of it is that a fair price is
// only interpretable if the consumer can see WHOSE opinion it is and WHY that
// book was picked — so [ReferenceSource] is on every computed record, not just
// the book identifier.
//
// # Two designations, ranked, because they answer different questions
//
// The catalogue's flag (domain.Book.IsReference) is the provider layer's own
// statement about a book. internal/ingest/provider/synthetic/universe.go sets it
// on the in-house book, and internal/ingest/provider/theoddsapi resolves it from
// SHARPLINE_ODDS_REFERENCE_BOOK. It is the authoritative answer where it exists.
//
// The configured slug list is this service's fallback, in priority order. It
// exists for three reasons and none of them is a workaround:
//
//   - Sharpness is an OPINION, not a fact a provider reports. The Odds API does
//     not label its bookmakers; theoddsapi/config.go already models the choice
//     as ours, with "pinnacle" as a default it can be argued out of. A pricing
//     service that could not be told which book to trust would be hard-coding a
//     trading judgement into a binary.
//   - A ranked list degrades gracefully. When the first choice does not quote a
//     market — routine on props and futures — the second is used and the record
//     says so, instead of the market silently losing its fair value.
//   - It keeps the +EV surface working against a provider that designates
//     nothing, which is every provider that publishes no sharpness label.
//
// # The designation's full path (it was once dropped, and is not any more)
//
// The flag travels: the adapter sets normalizer.RawBook.Reference (synthetic
// from universe.go's in-house book, theoddsapi from Config.ReferenceBook), the
// mapper carries it onto domain.Book, normalizer.BookRef carries it on the wire
// and the fingerprint hashes it, and internal/ingest/writer writes it into
// books.is_reference. An earlier build dropped it at the adapter → raw boundary
// and made [ReferenceSourceCatalogue] unreachable in a running system; the
// end-to-end assertion in reference_test.go exists so that cannot regress
// silently.
//
// Recording the source rather than assuming it is what made that defect visible
// in `sharpline_pricer_reference_book_total{source}` instead of invisible, and
// it is why the field stays on every record now that both values occur.
package pricing

import (
	"fmt"
	"slices"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// ReferenceSource records how the sharp book for a market was chosen.
//
// The zero value is invalid, so a computed record that never resolved a
// reference cannot serialise a plausible-looking one.
type ReferenceSource uint8

const (
	// ReferenceSourceUnknown is the invalid zero value.
	ReferenceSourceUnknown ReferenceSource = iota

	// ReferenceSourceCatalogue means the book carried the catalogue's own
	// reference designation (domain.Book.IsReference).
	ReferenceSourceCatalogue

	// ReferenceSourceConfigured means the book matched an entry in the
	// service's configured reference-book preference list.
	ReferenceSourceConfigured
)

// String returns the canonical lowercase name.
func (s ReferenceSource) String() string {
	switch s {
	case ReferenceSourceCatalogue:
		return "catalogue"
	case ReferenceSourceConfigured:
		return "configured"
	case ReferenceSourceUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Valid reports whether s names a real source.
func (s ReferenceSource) Valid() bool {
	return s == ReferenceSourceCatalogue || s == ReferenceSourceConfigured
}

// MarshalText implements encoding.TextMarshaler. An invalid source is an error
// rather than the string "unknown", so a half-computed record cannot be
// published as though it had a reference.
func (s ReferenceSource) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("pricing: reference source %d: %w", uint8(s), ErrNoReferenceBook)
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *ReferenceSource) UnmarshalText(text []byte) error {
	switch string(text) {
	case "catalogue":
		*s = ReferenceSourceCatalogue
	case "configured":
		*s = ReferenceSourceConfigured
	default:
		return fmt.Errorf("pricing: reference source %q: %w", string(text), ErrNoReferenceBook)
	}
	return nil
}

// referenceCandidate is one book that could serve as the reference, with the
// designation that put it forward.
type referenceCandidate struct {
	state  BookState
	source ReferenceSource
}

// resolveReference picks the sharp book for one market.
//
// Candidates are ranked catalogue-designated first, then by position in the
// configured preference list, and the FIRST ELIGIBLE one wins — eligible meaning
// it quoted every selection and its oldest quote is no older than maxAge.
//
// Walking the list rather than taking the top candidate and failing is
// deliberate. A ranked preference whose first entry is missing or stale on a
// given market should fall to the second and say so; collapsing to "no
// reference" would blank the +EV surface for every prop the first-choice book
// does not price, which on a real provider is most of them.
//
// When nothing is eligible the error names the BEST candidate's specific problem
// rather than a generic absence, because "Pinnacle quoted this market but its
// line is 40 minutes old" and "Pinnacle does not quote this market" call for
// completely different responses from an operator.
func resolveReference(s MarketSnapshot, prefer []domain.Slug, maxAge time.Duration) (referenceCandidate, error) {
	candidates := referenceCandidates(s, prefer)
	if len(candidates) == 0 {
		return referenceCandidate{}, fmt.Errorf(
			"pricing: market %s: none of the %d quoting book(s) is designated sharp "+
				"(no catalogue flag, and none matched the configured preference list): %w",
			s.Market.ID(), len(s.Books), ErrNoReferenceBook)
	}

	var firstErr error
	for _, c := range candidates {
		if !c.state.Complete {
			if firstErr == nil {
				firstErr = fmt.Errorf("pricing: market %s: reference book %s quoted %d of %d selections: %w",
					s.Market.ID(), c.state.Book.Slug(), len(c.state.Quotes), len(s.Selections),
					ErrIncompleteReference)
			}
			continue
		}
		if age := c.state.Age(s.Anchor()); age > maxAge {
			if firstErr == nil {
				firstErr = fmt.Errorf("pricing: market %s: reference book %s quote is %s old, limit is %s: %w",
					s.Market.ID(), c.state.Book.Slug(), age, maxAge, ErrReferenceStale)
			}
			continue
		}
		return c, nil
	}
	return referenceCandidate{}, firstErr
}

// referenceCandidates ranks the books that could serve as the reference.
//
// Catalogue-designated books come first. That a catalogue should designate
// exactly one is a property of the catalogue and domain/book.go says so
// explicitly ("that exactly one book should carry this flag is a property of the
// catalogue, not of any single Book"), but this function is handed whatever the
// bus delivered and must be total over it — so several flagged books are ordered
// among themselves by the configured preference and then by identifier, which is
// deterministic and replay-stable rather than dependent on record order.
func referenceCandidates(s MarketSnapshot, prefer []domain.Slug) []referenceCandidate {
	rank := make(map[domain.Slug]int, len(prefer))
	for i, slug := range prefer {
		if _, seen := rank[slug]; !seen {
			rank[slug] = i
		}
	}
	// A book not named in the preference list sorts after every book that is.
	unranked := len(prefer)
	rankOf := func(b BookState) int {
		if r, ok := rank[b.Book.Slug()]; ok {
			return r
		}
		return unranked
	}

	var flagged, configured []referenceCandidate
	for _, b := range s.Books {
		switch {
		case b.Book.IsReference():
			flagged = append(flagged, referenceCandidate{state: b, source: ReferenceSourceCatalogue})
		case rankOf(b) < unranked:
			configured = append(configured, referenceCandidate{state: b, source: ReferenceSourceConfigured})
		}
	}

	order := func(a, b referenceCandidate) int {
		if c := rankOf(a.state) - rankOf(b.state); c != 0 {
			return c
		}
		return cmpBookID(a.state.Book.ID(), b.state.Book.ID())
	}
	slices.SortFunc(flagged, order)
	slices.SortFunc(configured, order)
	return append(flagged, configured...)
}
