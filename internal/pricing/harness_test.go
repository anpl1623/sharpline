// Test fixtures that are COMPUTED, never canned.
//
// The contract ledger's NO MOCK DATA rule forbids "hardcoded event/market/odds
// arrays" and "canned market snapshots […] that do not arise from real
// computation". Nothing in this file is a recorded market. What it provides is a
// builder that turns a set of LATENT PROBABILITIES — the thing a market is a
// noisy statement about — into the record shape the pipeline actually carries,
// by running the same margin relation and the same published root solver the
// synthetic provider's own generator runs.
//
// That is what makes the recovery test in engine_test.go a real assertion rather
// than a tautology. The test knows the right answer because it chose the latent
// probabilities; the engine is never told them, and has to invert a margin to
// get back to them.
package pricing

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider/synthetic"
)

// powerMargin inflates a fair probability vector to sum to 1 + margin by raising
// every entry to a common exponent.
//
// It CALLS THE SYNTHETIC PROVIDER'S OWN QUOTING MODEL rather than restating it.
// That is the whole point of the known-answer test in engine_test.go:
// synthetic/quote.go's ApplyMargin is the one definition of what "the
// generator's latent probabilities" means, and a private copy here would keep
// agreeing with an inverse that had drifted away from the generator — which is
// precisely the failure a known-answer test exists to catch.
//
// What is deliberately NOT applied is everything the adapter wraps around
// ApplyMargin: per-book bias, per-book view lag and American tick flooring. Those
// are what make an exact inversion impossible, and synthetic_test.go drives the
// real generator with all of them in place and asserts the properties that
// survive them.
func powerMargin(t *testing.T, fair []float64, margin float64) []float64 {
	t.Helper()
	out := make([]float64, len(fair))
	if err := synthetic.ApplyMargin(fair, out, margin, true); err != nil {
		t.Fatalf("synthetic.ApplyMargin(power, margin %g): %v", margin, err)
	}
	return out
}

// multiplicativeMargin is the other relation the synthetic book set uses: every
// implied probability scaled by the same factor. Two-way synthetic markets are
// quoted this way, three-way ones by the power relation, and knowing which is
// which is the difference between a devig that recovers the truth and one that
// is merely close. Same source, same reasoning.
func multiplicativeMargin(t *testing.T, fair []float64, margin float64) []float64 {
	t.Helper()
	out := make([]float64, len(fair))
	if err := synthetic.ApplyMargin(fair, out, margin, false); err != nil {
		t.Fatalf("synthetic.ApplyMargin(multiplicative, margin %g): %v", margin, err)
	}
	return out
}

// decimalsOf converts implied probabilities to the decimal prices a book would
// post. No tick snapping: this is the exact-arithmetic path.
func decimalsOf(implied []float64) []float64 {
	out := make([]float64, len(implied))
	for i, p := range implied {
		out[i] = 1 / p
	}
	return out
}

// bookFixture is one book's quotes on the market under construction.
type bookFixture struct {
	slug string
	name string

	// reference sets normalizer.BookRef.Reference on the wire, which is the
	// catalogue designation the synthetic adapter puts on its in-house book and
	// the theoddsapi adapter puts on Config.ReferenceBook.
	reference bool

	// prices are decimal odds, one per selection, in selection order.
	prices []float64

	// lines are optional per-selection lines. nil means every selection carries
	// no line, which is what a moneyline looks like.
	lines []domain.Line

	// observedAt is this book's quote instant. Zero means the market's own.
	observedAt time.Time

	// omit drops these selection indices from the book's quotes, producing an
	// incomplete book.
	omit []int
}

// marketFixture describes a market to be BUILT. Nothing here is asserted on; the
// assertions live in the tests and are made against what the engine computes.
type marketFixture struct {
	id         string
	selections []domain.SelectionRole
	line       domain.Line
	books      []bookFixture

	// observedAt is the market's observation instant, and ingestedAt is the
	// staleness anchor. Zero values are filled with a fixed instant so a fixture
	// never depends on the wall clock — the same determinism requirement the
	// engine itself is held to.
	observedAt time.Time
	ingestedAt time.Time
}

// fixtureEpoch is the instant every fixture is dated from. A constant rather
// than time.Now so that two runs of the suite build byte-identical records.
var fixtureEpoch = time.Date(2026, 8, 17, 18, 30, 0, 0, time.UTC)

// build renders a fixture as the record shape odds.normalized carries.
//
// It constructs normalizer.NormalizedMarket directly rather than driving the
// mapper, because the mapper's input is a provider payload and this builder's
// input is a probability vector. The end-to-end test drives the mapper.
func (f marketFixture) build(t *testing.T) normalizer.NormalizedMarket {
	t.Helper()

	if f.observedAt.IsZero() {
		f.observedAt = fixtureEpoch
	}
	if f.ingestedAt.IsZero() {
		f.ingestedAt = f.observedAt.Add(2 * time.Second)
	}
	if f.id == "" {
		f.id = "mkt-fixture"
	}

	marketType := domain.MarketTypeMoneyline
	rec := normalizer.NormalizedMarket{
		SchemaVersion: normalizer.SchemaVersion,
		Provider:      "synthetic",
		Fingerprint:   "fixture-" + f.id,
		Sport: normalizer.SportRef{
			ID: "sport-fixture", Slug: "fixture-sport", Name: "Fixture Sport",
		},
		League: normalizer.LeagueRef{
			ID: "league-fixture", SportID: "sport-fixture",
			Slug: "fixture-league", Name: "Fixture League",
		},
		Event: normalizer.EventRef{
			ID: "event-fixture", LeagueID: "league-fixture",
			Kind: domain.EventKindMatch.String(), Name: "Away at Home",
			Home:           normalizer.CompetitorRef{ID: "home-fixture", Name: "Home Side"},
			Away:           normalizer.CompetitorRef{ID: "away-fixture", Name: "Away Side"},
			ScheduledStart: fixtureEpoch.Add(3 * time.Hour),
			Status:         domain.EventStatusScheduled.String(),
		},
		Market: normalizer.MarketRef{
			ID: f.id, EventID: "event-fixture",
			Type: marketType.String(), ProviderKey: "h2h",
			Line: f.line, Status: domain.MarketStatusOpen.String(),
			UpdatedAt: f.observedAt,
		},
		ObservedAt: f.observedAt,
		IngestedAt: f.ingestedAt,
	}

	for i, role := range f.selections {
		rec.Selections = append(rec.Selections, normalizer.SelectionRef{
			ID:       fmt.Sprintf("%s-sel-%d", f.id, i),
			MarketID: f.id,
			Role:     role.String(),
			Name:     fmt.Sprintf("Selection %d", i),
		})
	}

	for _, b := range f.books {
		if len(b.prices) != len(f.selections) {
			t.Fatalf("fixture %s: book %s has %d prices for %d selections",
				f.id, b.slug, len(b.prices), len(f.selections))
		}
		name := b.name
		if name == "" {
			name = b.slug
		}
		rec.Books = append(rec.Books, normalizer.BookRef{
			ID: "book-" + b.slug, Slug: b.slug, Name: name,
			Kind: domain.BookKindSynthetic.String(), Reference: b.reference,
		})

		observed := b.observedAt
		if observed.IsZero() {
			observed = f.observedAt
		}
		for i := range f.selections {
			if omitted(b.omit, i) {
				continue
			}
			line := domain.NoLine()
			if b.lines != nil {
				line = b.lines[i]
			}
			rec.Prices = append(rec.Prices, normalizer.PriceRef{
				SelectionID: fmt.Sprintf("%s-sel-%d", f.id, i),
				BookID:      "book-" + b.slug,
				Decimal:     b.prices[i],
				Line:        line,
				ObservedAt:  observed,
			})
		}
	}
	return rec
}

// omitted reports whether index i is in the omit list.
func omitted(omit []int, i int) bool {
	for _, o := range omit {
		if o == i {
			return true
		}
	}
	return false
}

// approxRel compares two floats to a RELATIVE tolerance, which is the repo's
// convention and the only defensible one for probabilities that span three
// orders of magnitude between a favourite and a longshot: an absolute tolerance
// tight enough to be meaningful on a 0.9 favourite is looser than the value
// itself on a 0.005 outsider.
//
// The magnitude is the repository's stated 1e-12, four thousand ULPs at
// magnitude 1, which is ample for the accumulated rounding of a bracketed root
// solve and its exponentiations and is still eight orders of magnitude below any
// margin a book runs.
func approxRel(got, want, tol float64) bool {
	if got == want {
		return true
	}
	scale := math.Max(math.Abs(want), math.Abs(got))
	if scale == 0 {
		return true
	}
	return math.Abs(got-want)/scale <= tol
}

// mustEngine builds an engine or fails the test.
func mustEngine(t *testing.T, o Options) *Engine {
	t.Helper()
	e, err := NewEngine(o)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}
