package pricing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// pricedFixture builds one fully priced market: a sharp reference and a soft
// challenger over the same latent probabilities.
func pricedFixture(t *testing.T) ComputedMarket {
	t.Helper()

	latent := []float64{0.42, 0.33, 0.25}
	rec := marketFixture{
		id:         "mkt-payload",
		selections: threeWayRoles,
		books: []bookFixture{
			{slug: "sharpline", name: "Sharpline Synthetic", prices: decimalsOf(powerMargin(t, latent, 0.020))},
			{slug: "lowtide", name: "Lowtide Sportsbook", prices: decimalsOf(powerMargin(t, latent, 0.065))},
		},
	}.build(t)

	out, err := mustEngine(t, Options{
		ReferenceBooks: []domain.Slug{"sharpline"},
		DevigMethod:    odds.MethodPower,
	}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	return out
}

// TestComputedMarketRoundTripsThroughJSON. price.computed is compacted, so a
// record written today is decoded by a build that does not exist yet; the shape
// has to survive the trip without a custom decoder.
func TestComputedMarketRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	want := pricedFixture(t)
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got ComputedMarket
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("a record this build wrote failed its own validation: %v", err)
	}

	again, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(again) != string(raw) {
		t.Errorf("round trip is not byte-stable:\nfirst:  %s\nsecond: %s", raw, again)
	}
}

// TestComputedMarketIsSelfContained.
//
// kafka/topics.go compacts price.computed "so that `stream` can build a client's
// initial snapshot from the log alone". A record that needed a second lookup to
// render would reintroduce the ordering problem the compacted topic was chosen
// to remove, so the catalogue travels with every record.
func TestComputedMarketIsSelfContained(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(pricedFixture(t))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	for _, needed := range []string{
		"Fixture League", "Away at Home", "Home Side", "Away Side",
		"Sharpline Synthetic", "Lowtide Sportsbook", "Selection 0",
		"\"provider\":\"synthetic\"", "\"reference\":", "\"fair\":", "\"books\":",
	} {
		if !strings.Contains(body, needed) {
			t.Errorf("record cannot be rendered without a second lookup: %q is absent", needed)
		}
	}
}

// TestValidateRejectsARecordWithNoProvenance. A fair value with no stated
// reference source or devig method is a number a consumer cannot reason about,
// so it is refused rather than accepted with a blank field.
func TestValidateRejectsARecordWithNoProvenance(t *testing.T) {
	t.Parallel()

	base := pricedFixture(t)

	t.Run("wrong schema version", func(t *testing.T) {
		t.Parallel()
		c := base
		c.SchemaVersion = SchemaVersion + 1
		if err := c.Validate(); err == nil {
			t.Error("a record from a newer schema validated")
		}
	})

	t.Run("no reference source", func(t *testing.T) {
		t.Parallel()
		c := base
		c.Reference.Source = ReferenceSourceUnknown
		if err := c.Validate(); err == nil {
			t.Error("a record with no reference provenance validated")
		}
	})

	t.Run("no devig method", func(t *testing.T) {
		t.Parallel()
		c := base
		c.Fair.Method = odds.MethodUnknown
		if err := c.Validate(); err == nil {
			t.Error("a record with no devig method validated")
		}
	})

	t.Run("too few selections", func(t *testing.T) {
		t.Parallel()
		c := base
		c.Fair.Selections = c.Fair.Selections[:1]
		if err := c.Validate(); err == nil {
			t.Error("a record carrying one fair selection validated")
		}
	})
}

// TestFairProbabilitiesSumToOne is the arithmetic identity the whole record
// rests on: after the margin is removed the probabilities are a distribution.
// If it fails, every expected value on the record is wrong.
func TestFairProbabilitiesSumToOne(t *testing.T) {
	t.Parallel()

	c := pricedFixture(t)
	sum := 0.0
	for _, f := range c.Fair.Selections {
		if f.Probability <= 0 || f.Probability >= 1 {
			t.Errorf("fair probability %g on %s is not strictly inside (0,1)",
				float64(f.Probability), f.SelectionID)
		}
		sum += float64(f.Probability)
	}
	if !approxRel(sum, 1, 1e-9) {
		t.Errorf("fair probabilities sum to %.17g, want 1", sum)
	}

	// The per-selection excesses are the margin, apportioned. They must add back
	// up to the overround the reference book was actually charging.
	excess := 0.0
	for _, f := range c.Fair.Selections {
		excess += f.Excess
	}
	if !approxRel(excess, c.Fair.Margin.Overround, 1e-9) {
		t.Errorf("per-selection excesses sum to %.17g, want the reference overround %.17g",
			excess, c.Fair.Margin.Overround)
	}
}

// TestMessageTypeAndSchemaVersionAreStated. Both are wire contract: the type is
// what a consumer switches on and the version is what stops an old decoder
// reading a newer record confidently and wrongly.
func TestMessageTypeAndSchemaVersionAreStated(t *testing.T) {
	t.Parallel()

	if MessageType != "price.computed.v1" {
		t.Errorf("MessageType is %q; renaming it is a breaking change for every consumer", MessageType)
	}
	if SchemaVersion != 1 {
		t.Errorf("SchemaVersion is %d; a bump obliges a migration of the compacted topic", SchemaVersion)
	}
	if got := pricedFixture(t).SchemaVersion; got != SchemaVersion {
		t.Errorf("published record stamps version %d, want %d", got, SchemaVersion)
	}
}
