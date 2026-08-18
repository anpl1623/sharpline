package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
)

// mustBook builds a domain book or fails the test.
func mustBook(t *testing.T, slug string, reference bool) domain.Book {
	t.Helper()
	s, err := domain.NewSlug(slug)
	if err != nil {
		t.Fatalf("NewSlug(%q): %v", slug, err)
	}
	b, err := domain.NewBook(domain.BookParams{
		ID:        domain.BookID("book-" + slug),
		Slug:      s,
		Name:      slug,
		Kind:      domain.BookKindSynthetic,
		Reference: reference,
	})
	if err != nil {
		t.Fatalf("NewBook(%q): %v", slug, err)
	}
	return b
}

// snapshotOf builds a minimal snapshot carrying only what reference resolution
// reads: the books, their completeness and their quote instants.
func snapshotOf(t *testing.T, anchor time.Time, books ...BookState) MarketSnapshot {
	t.Helper()
	return MarketSnapshot{
		Market:     domain.Market{},
		Selections: make([]domain.Selection, 2),
		Books:      books,
		IngestedAt: anchor,
	}
}

func bookStateOf(b domain.Book, complete bool, oldest time.Time) BookState {
	quotes := 2
	if !complete {
		quotes = 1
	}
	return BookState{
		Book:     b,
		Quotes:   make([]BookQuote, quotes),
		Complete: complete,
		OldestAt: oldest,
		NewestAt: oldest,
	}
}

// TestCatalogueDesignationOutranksTheConfiguredList.
//
// The catalogue flag is the provider layer's own statement about a book; the
// configured list is this service's fallback. When both are available the
// authoritative one wins, and the record must say which was used — a consumer
// that cannot tell a designation from a default cannot tell a trading judgement
// from a config file.
func TestCatalogueDesignationOutranksTheConfiguredList(t *testing.T) {
	t.Parallel()

	anchor := fixtureEpoch
	s := snapshotOf(t, anchor,
		bookStateOf(mustBook(t, "configured-choice", false), true, anchor),
		bookStateOf(mustBook(t, "flagged-book", true), true, anchor),
	)

	got, err := resolveReference(s, []domain.Slug{"configured-choice"}, time.Minute)
	if err != nil {
		t.Fatalf("resolveReference: %v", err)
	}
	if got.state.Book.Slug() != "flagged-book" {
		t.Errorf("chose %s, want the catalogue-flagged book", got.state.Book.Slug())
	}
	if got.source != ReferenceSourceCatalogue {
		t.Errorf("source %s, want catalogue", got.source)
	}
}

// TestConfiguredPreferenceIsHonouredInOrder. A ranked list has to degrade to the
// next entry rather than to nothing, because on a real provider the first choice
// does not quote most props.
func TestConfiguredPreferenceIsHonouredInOrder(t *testing.T) {
	t.Parallel()

	anchor := fixtureEpoch
	s := snapshotOf(t, anchor,
		bookStateOf(mustBook(t, "third", false), true, anchor),
		bookStateOf(mustBook(t, "second", false), true, anchor),
	)

	got, err := resolveReference(s, []domain.Slug{"first", "second", "third"}, time.Minute)
	if err != nil {
		t.Fatalf("resolveReference: %v", err)
	}
	if got.state.Book.Slug() != "second" {
		t.Errorf("chose %s, want second — the highest-ranked book actually quoting", got.state.Book.Slug())
	}
	if got.source != ReferenceSourceConfigured {
		t.Errorf("source %s, want configured", got.source)
	}
}

// TestResolutionWalksPastAnIneligibleFirstChoice: a stale or incomplete top
// choice falls through to the next, and only an exhausted list is a refusal.
func TestResolutionWalksPastAnIneligibleFirstChoice(t *testing.T) {
	t.Parallel()

	anchor := fixtureEpoch
	s := snapshotOf(t, anchor,
		bookStateOf(mustBook(t, "stale-first", false), true, anchor.Add(-time.Hour)),
		bookStateOf(mustBook(t, "partial-second", false), false, anchor),
		bookStateOf(mustBook(t, "good-third", false), true, anchor),
	)

	got, err := resolveReference(s,
		[]domain.Slug{"stale-first", "partial-second", "good-third"}, 5*time.Minute)
	if err != nil {
		t.Fatalf("resolveReference: %v", err)
	}
	if got.state.Book.Slug() != "good-third" {
		t.Errorf("chose %s, want good-third", got.state.Book.Slug())
	}
}

// TestExhaustedResolutionReportsTheBestCandidatesProblem.
//
// "Pinnacle does not quote this market" and "Pinnacle's line is 40 minutes old"
// call for completely different responses, so the error names the specific
// failure of the highest-ranked candidate rather than a generic absence.
func TestExhaustedResolutionReportsTheBestCandidatesProblem(t *testing.T) {
	t.Parallel()

	anchor := fixtureEpoch
	stale := snapshotOf(t, anchor,
		bookStateOf(mustBook(t, "sharp", false), true, anchor.Add(-time.Hour)))
	if _, err := resolveReference(stale, []domain.Slug{"sharp"}, time.Minute); !errors.Is(err, ErrReferenceStale) {
		t.Errorf("stale reference: got %v, want ErrReferenceStale", err)
	}

	partial := snapshotOf(t, anchor, bookStateOf(mustBook(t, "sharp", false), false, anchor))
	if _, err := resolveReference(partial, []domain.Slug{"sharp"}, time.Minute); !errors.Is(err, ErrIncompleteReference) {
		t.Errorf("incomplete reference: got %v, want ErrIncompleteReference", err)
	}

	absent := snapshotOf(t, anchor, bookStateOf(mustBook(t, "soft", false), true, anchor))
	if _, err := resolveReference(absent, []domain.Slug{"sharp"}, time.Minute); !errors.Is(err, ErrNoReferenceBook) {
		t.Errorf("absent reference: got %v, want ErrNoReferenceBook", err)
	}
}

// TestReferenceSourceRefusesToSerialiseAnUnsetValue. A fair value whose
// provenance is "unknown" is a fair value nobody can reason about, so the zero
// value fails to marshal rather than emitting a plausible string.
func TestReferenceSourceRefusesToSerialiseAnUnsetValue(t *testing.T) {
	t.Parallel()

	if _, err := json.Marshal(ReferenceSourceUnknown); err == nil {
		t.Error("the zero ReferenceSource marshalled successfully; it must not")
	}
	for _, s := range []ReferenceSource{ReferenceSourceCatalogue, ReferenceSourceConfigured} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %s: %v", s, err)
		}
		var got ReferenceSource
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got != s {
			t.Errorf("round trip: got %s, want %s", got, s)
		}
	}
}

// TestCatalogueDesignationResolvesThroughTheWire is the assertion that replaced
// the KNOWN-DEFECT pin this phase inherited.
//
// The designation now travels the whole way: the adapter sets
// normalizer.RawBook.Reference, the mapper carries it onto domain.Book, the wire
// record carries it on BookRef and the fingerprint hashes it. So a flagged book
// resolves as `catalogue` — and, critically, it does so while the CONFIGURED
// list names a different book, which is the only arrangement that proves the
// flag was read rather than the fallback coincidentally agreeing.
func TestCatalogueDesignationResolvesThroughTheWire(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-designation",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "soft-book", prices: decimalsOf(multiplicativeMargin(t, []float64{0.55, 0.45}, 0.06))},
			{slug: "sharpline", reference: true, prices: decimalsOf(implied)},
		},
	}.build(t)

	// The preference list deliberately names the OTHER book. If the flag were
	// still being dropped, resolution would land on soft-book as `configured`.
	out, err := mustEngine(t, Options{ReferenceBooks: []domain.Slug{"soft-book"}}).
		Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if out.Reference.Source != ReferenceSourceCatalogue {
		t.Errorf("source %s, want catalogue: the wire carries the designation now", out.Reference.Source)
	}
	if got := string(out.Reference.Slug); got != "sharpline" {
		t.Errorf("reference book %s, want sharpline (the flagged book, not the configured one)", got)
	}
}

// TestReferenceFlagSurvivesTheWireRoundTrip. The record is serialised onto a
// compacted topic and read back by `stream` and by this service's own warm
// start, so a designation that only exists in memory is not a designation.
func TestReferenceFlagSurvivesTheWireRoundTrip(t *testing.T) {
	t.Parallel()

	rec := marketFixture{
		id:         "mkt-wire",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", reference: true,
				prices: decimalsOf(multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02))},
		},
	}.build(t)

	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back normalizer.NormalizedMarket
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Books) != 1 || !back.Books[0].Reference {
		t.Fatalf("the reference flag did not survive JSON: %s", raw)
	}

	view, err := back.Domain()
	if err != nil {
		t.Fatalf("Domain: %v", err)
	}
	if !view.Books[0].IsReference() {
		t.Error("domain.Book lost the designation on rehydration")
	}
}

// TestEmptyPreferenceListLeavesTheSurfaceEmptyRatherThanGuessing.
//
// An engine told nothing about which book is sharp must not pick one. It is the
// configuration that is wrong, and a service that guessed would publish fair
// values attributed to a book nobody designated.
func TestEmptyPreferenceListLeavesTheSurfaceEmptyRatherThanGuessing(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-unconfigured",
		selections: twoWayRoles,
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}.build(t)

	_, err := mustEngine(t, Options{}).Price(context.Background(), rec)
	if !errors.Is(err, ErrNoReferenceBook) {
		t.Fatalf("got %v, want ErrNoReferenceBook", err)
	}
}
