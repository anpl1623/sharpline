package pricing

import (
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
)

// TestSnapshotAnchorsStalenessOnTheRecordAndNotTheClock is the property that
// makes the engine a pure function, which the service's suppression depends on.
func TestSnapshotAnchorsStalenessOnTheRecordAndNotTheClock(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-anchor",
		selections: twoWayRoles,
		observedAt: fixtureEpoch,
		ingestedAt: fixtureEpoch.Add(90 * time.Second),
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}.build(t)

	s, err := NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	if !s.Anchor().Equal(fixtureEpoch.Add(90 * time.Second)) {
		t.Errorf("anchor %s, want the record's ingested_at", s.Anchor())
	}
	if got := s.Books[0].Age(s.Anchor()); got != 90*time.Second {
		t.Errorf("book age %s, want 90s — (ingested_at − observed_at), the provider's share", got)
	}

	// With no ingest instant the anchor falls back to the newest observation, so
	// ages become RELATIVE — a book's lag behind the freshest book on the same
	// market — rather than 55 years, which is what a zero anchor would produce.
	rec.IngestedAt = time.Time{}
	s, err = NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	if !s.Anchor().Equal(fixtureEpoch) {
		t.Errorf("fallback anchor %s, want the record's newest observation", s.Anchor())
	}
	if got := s.Books[0].Age(s.Anchor()); got != 0 {
		t.Errorf("relative age %s, want 0 for the freshest book", got)
	}
}

// TestSnapshotOrdersBooksDeterministically. Record order is whatever the
// provider listed; a computed payload whose book order moved between two
// identical inputs would make every diff and golden file noisy.
func TestSnapshotOrdersBooksDeterministically(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-order",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "zulu", prices: decimalsOf(implied)},
			{slug: "alpha", prices: decimalsOf(implied)},
			{slug: "mike", prices: decimalsOf(implied)},
		},
	}.build(t)

	s, err := NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	want := []domain.Slug{"alpha", "mike", "zulu"}
	for i, b := range s.Books {
		if b.Book.Slug() != want[i] {
			t.Errorf("book %d is %s, want %s", i, b.Book.Slug(), want[i])
		}
	}
}

// TestSnapshotRefusesARecordWhosePartsDisagree.
//
// A price naming a book or a selection the record does not list is a corrupt
// record. Dropping it silently would change the market's booking percentage —
// the number every other number here is derived from — with nothing saying so.
func TestSnapshotRefusesARecordWhosePartsDisagree(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	base := marketFixture{
		id:         "mkt-corrupt",
		selections: twoWayRoles,
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}

	t.Run("price from an unlisted book", func(t *testing.T) {
		t.Parallel()
		rec := base.build(t)
		rec.Prices = append(rec.Prices, normalizer.PriceRef{
			SelectionID: "mkt-corrupt-sel-0",
			BookID:      "book-ghost",
			Decimal:     2.0,
			ObservedAt:  fixtureEpoch,
		})
		if _, err := NewMarketSnapshot(rec); !errors.Is(err, ErrMarketNotPriceable) {
			t.Fatalf("got %v, want ErrMarketNotPriceable", err)
		}
	})

	t.Run("price on an unlisted selection", func(t *testing.T) {
		t.Parallel()
		rec := base.build(t)
		rec.Prices = append(rec.Prices, normalizer.PriceRef{
			SelectionID: "mkt-corrupt-sel-99",
			BookID:      "book-sharpline",
			Decimal:     2.0,
			ObservedAt:  fixtureEpoch,
		})
		if _, err := NewMarketSnapshot(rec); !errors.Is(err, ErrMarketNotPriceable) {
			t.Fatalf("got %v, want ErrMarketNotPriceable", err)
		}
	})

	t.Run("duplicate book", func(t *testing.T) {
		t.Parallel()
		rec := base.build(t)
		rec.Books = append(rec.Books, rec.Books[0])
		if _, err := NewMarketSnapshot(rec); !errors.Is(err, ErrMarketNotPriceable) {
			t.Fatalf("got %v, want ErrMarketNotPriceable", err)
		}
	})
}

// TestBookQuotedTwiceKeepsTheNewerQuote. The normalizer does not deduplicate,
// and the winner has to be decided by the provider's clock rather than by slice
// order, or a redelivery could reinstate an older price.
func TestBookQuotedTwiceKeepsTheNewerQuote(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-dup",
		selections: twoWayRoles,
		books:      []bookFixture{{slug: "sharpline", prices: decimalsOf(implied)}},
	}.build(t)

	const newerDecimal = 1.91
	rec.Prices = append(rec.Prices, normalizer.PriceRef{
		SelectionID: "mkt-dup-sel-0",
		BookID:      "book-sharpline",
		Decimal:     newerDecimal,
		ObservedAt:  fixtureEpoch.Add(time.Second),
	})

	s, err := NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	q, ok := s.Books[0].Quote("mkt-dup-sel-0")
	if !ok {
		t.Fatal("selection 0 lost its quote")
	}
	if float64(q.Decimal) != newerDecimal {
		t.Errorf("kept decimal %g, want the later-observed %g", float64(q.Decimal), newerDecimal)
	}
	if len(s.Books[0].Quotes) != 2 {
		t.Errorf("book holds %d quotes, want 2 — the duplicate must replace, not append",
			len(s.Books[0].Quotes))
	}
}

// TestIncompleteBookCannotProduceDecimals. Decimals is the only path into a
// devig, and a partial market devigs without complaint into a fabricated
// near-certainty, so the guard belongs on the accessor rather than at each call.
func TestIncompleteBookCannotProduceDecimals(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-incomplete",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(implied)},
			{slug: "partial", prices: decimalsOf(implied), omit: []int{1}},
		},
	}.build(t)

	s, err := NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	for _, b := range s.Books {
		prices, ok := b.Decimals()
		if b.Book.Slug() == "partial" {
			if ok || prices != nil {
				t.Error("an incomplete book returned prices for devigging")
			}
			continue
		}
		if !ok || len(prices) != 2 {
			t.Errorf("complete book returned %d prices, ok=%v", len(prices), ok)
		}
	}
}

// TestBookQuotingNothingIsDropped. The normalizer lists a book because it
// appeared in the payload, which is not the same as it having quoted this
// market; carrying it would put a book with no prices on the board.
func TestBookQuotingNothingIsDropped(t *testing.T) {
	t.Parallel()

	implied := multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)
	rec := marketFixture{
		id:         "mkt-silent",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", prices: decimalsOf(implied)},
			{slug: "silent", prices: decimalsOf(implied), omit: []int{0, 1}},
		},
	}.build(t)

	s, err := NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	if len(s.Books) != 1 {
		t.Fatalf("snapshot holds %d books, want 1", len(s.Books))
	}
	if s.Books[0].Book.Slug() != "sharpline" {
		t.Errorf("kept %s, want sharpline", s.Books[0].Book.Slug())
	}
}
