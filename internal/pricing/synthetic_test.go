// The engine driven over the REAL synthetic provider, through the REAL
// normalizer, with nothing stubbed in between.
//
// The unit tests in engine_test.go build a market from latent probabilities and
// assert exact recovery, which is the sharpest claim available but a claim about
// arithmetic. This file makes the weaker claim over the harder input: the actual
// generator, with its per-book margins, per-book bias, per-book view lag and
// American tick flooring all in play, decoded by the actual mapper and priced by
// the engine.
//
// Tick flooring is why exact recovery is NOT asserted here. quote.go snaps every
// price down to the book's American quoting granularity — one cent for the
// reference book, ten for the softest — precisely so change detection is
// measurable, and that snap moves the implied probabilities off the exact power
// relation by up to half a tick. What survives it are the properties asserted
// below, and they are the ones that would actually break if the engine were
// wrong.
package pricing

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/provider/synthetic"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// capturingPublisher records what the normalizer would have put on
// odds.normalized. It is a test double for the BUS, not for any data: every
// record it captures was computed by the real generator and mapped by the real
// mapper.
type capturingPublisher struct{ records []normalizer.NormalizedMarket }

func (p *capturingPublisher) PublishNormalized(_ context.Context, _ domain.MarketID, msg kafka.Message) error {
	rec, ok := msg.Payload.(normalizer.NormalizedMarket)
	if !ok {
		return nil
	}
	p.records = append(p.records, rec)
	return nil
}

// emptySnapshotter stands in for a cold compacted topic: there is nothing to
// warm from, which is the correct state for a normalizer that has never run.
type emptySnapshotter struct{}

func (emptySnapshotter) Read(context.Context, func(context.Context, *kafka.Delivery) error) (kafka.SnapshotStats, error) {
	return kafka.SnapshotStats{}, nil
}

// pricingTestLogger discards the normalizer's output. The normalizer requires a
// logger and this test asserts on records, not on log lines.
func pricingTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// normalizeSyntheticFeed drives the real synthetic adapter through the real
// normalizer and returns the records that reached the bus.
func normalizeSyntheticFeed(t *testing.T) []normalizer.NormalizedMarket {
	t.Helper()

	// A fixed clock, because the generator's determinism contract is stated over
	// clock readings: "two adapters given the same seed and the same instants
	// must produce byte-identical snapshots".
	now := time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)
	adapter, err := synthetic.New(synthetic.Options{
		Seed:  20260817,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("synthetic.New: %v", err)
	}

	ctx := context.Background()
	cat, err := adapter.Catalogue(ctx)
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("catalogue does not validate: %v", err)
	}

	decoder, err := normalizer.NewNeutralDecoder(kafka.Provider(provider.NameSynthetic))
	if err != nil {
		t.Fatalf("NewNeutralDecoder: %v", err)
	}
	pub := &capturingPublisher{}
	norm, err := normalizer.New(normalizer.Options{
		Provider:    kafka.Provider(provider.NameSynthetic),
		Decoder:     decoder,
		Producer:    pub,
		Snapshotter: emptySnapshotter{},
		Logger:      pricingTestLogger(),
		Clock:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("normalizer.New: %v", err)
	}

	for _, league := range cat.Leagues {
		snap, err := adapter.Fetch(ctx, provider.Scope{
			League: league.ID(),
			Markets: []domain.MarketType{
				domain.MarketTypeMoneyline, domain.MarketTypeSpread, domain.MarketTypeTotal,
			},
		})
		if err != nil {
			t.Fatalf("Fetch(%s): %v", league.ID(), err)
		}
		for _, ev := range snap.Events {
			if ev.Raw.IsZero() {
				continue
			}
			d := &kafka.Delivery{
				Topic:     kafka.TopicOddsRawPrefix + provider.NameSynthetic.String(),
				Partition: 0,
				Key:       string(ev.Event.ID()),
				Timestamp: snap.FetchedAt,
				Envelope: kafka.Envelope{
					Version:    kafka.EnvelopeVersion,
					Type:       normalizer.RawMessageType,
					Producer:   "ingest",
					ProducedAt: snap.FetchedAt,
					ObservedAt: ev.Raw.ObservedAt,
					Data:       json.RawMessage(ev.Raw.Body),
				},
			}
			if err := norm.HandleMessage(ctx, d); err != nil {
				t.Fatalf("normalizer.HandleMessage: %v", err)
			}
		}
	}
	if len(pub.records) == 0 {
		t.Fatal("the synthetic feed produced no normalized markets; there is nothing to price")
	}
	return pub.records
}

// TestEngineOverTheSyntheticFeed prices the whole generated slate and asserts
// the properties that must hold over every market on it.
func TestEngineOverTheSyntheticFeed(t *testing.T) {
	t.Parallel()

	records := normalizeSyntheticFeed(t)

	// The preference list is EMPTY on purpose. The in-house book is the
	// generator's sharp reference — tightest margin, finest tick, no lag, no
	// bias (universe.go) — and it says so with the catalogue flag, so an engine
	// told nothing about which book to trust must still resolve it. If the
	// designation stopped reaching this record, every market below would refuse
	// with ErrNoReferenceBook rather than quietly falling back to a slug this
	// test had supplied.
	engine := mustEngine(t, Options{DevigMethod: odds.MethodPower})

	var (
		priced      int
		refused     int
		threeWay    int
		lineMatched int
		mismatched  int
		positiveEV  int
	)

	for _, rec := range records {
		out, err := engine.Price(context.Background(), rec)
		if err != nil {
			refused++
			continue
		}
		priced++

		if out.Reference.Slug != domain.SyntheticBookSlug {
			t.Fatalf("market %s: reference is %s, want the in-house book",
				out.Market.ID, out.Reference.Slug)
		}
		// The synthetic adapter DESIGNATES its in-house book (universe.go), and
		// that designation now travels the whole pipeline, so every resolution
		// over this feed must read as `catalogue`. This engine's preference list
		// is deliberately empty below, which means a `configured` resolution is
		// not merely unexpected here — it is impossible — and a market that
		// priced at all is proof the flag arrived.
		if out.Reference.Source != ReferenceSourceCatalogue {
			t.Fatalf("market %s: reference source %s, want catalogue; the synthetic adapter "+
				"designates its in-house book and the flag must survive the pipeline",
				out.Market.ID, out.Reference.Source)
		}

		// The generator is GENUINELY OVERROUND: the reference book is configured
		// at a 2.0% margin and tick flooring can only increase it. A fair or
		// underround reference would mean the feed had stopped being a market
		// and the +EV surface would be measuring rounding error.
		if out.Fair.Margin.Overround <= 0 {
			t.Fatalf("market %s: reference overround %.6g is not positive; the synthetic feed is "+
				"quoted with a margin and must arrive with one", out.Market.ID, out.Fair.Margin.Overround)
		}
		if out.Fair.Margin.Vig >= out.Fair.Margin.Overround {
			t.Fatalf("market %s: vig %.6g is not below overround %.6g",
				out.Market.ID, out.Fair.Margin.Vig, out.Fair.Margin.Overround)
		}

		sum := 0.0
		for _, f := range out.Fair.Selections {
			sum += float64(f.Probability)
		}
		if !approxRel(sum, 1, 1e-9) {
			t.Fatalf("market %s: fair probabilities sum to %.15f", out.Market.ID, sum)
		}
		if len(out.Fair.Selections) >= 3 {
			threeWay++
		}

		for _, b := range out.Books {
			for _, q := range b.Quotes {
				switch q.Status {
				case QuoteStatusPriced:
					lineMatched++
					if q.ExpectedValue > 0 {
						positiveEV++
					}
					if b.Reference && q.ExpectedValue > relTol {
						t.Fatalf("market %s: the reference book shows %+.6g EV against fair value "+
							"devigged from its own prices", out.Market.ID, q.ExpectedValue)
					}
				case QuoteStatusLineMismatch:
					mismatched++
				}
			}
		}
	}

	t.Logf("synthetic slate: %d records, %d priced, %d refused, %d three-way, "+
		"%d quotes scored, %d line-mismatched, %d positive-EV quotes",
		len(records), priced, refused, threeWay, lineMatched, mismatched, positiveEV)

	if priced == 0 {
		t.Fatal("no market on the synthetic slate produced a fair value")
	}
	if threeWay == 0 {
		t.Fatal("the slate carried no three-way market; the football league's moneyline is the " +
			"one shape whose power margin devigs back exactly, and it is the case this phase " +
			"is judged on")
	}
	if lineMatched == 0 {
		t.Fatal("no quote was scored against fair value")
	}
	// Book disagreement is a property of the model — universe.go gives every
	// book its own bias and view lag — so a slate with no soft book ever
	// offering value would mean the disagreement had stopped arriving.
	if positiveEV == 0 {
		t.Error("no book on the whole slate priced above the sharp book's fair value; " +
			"the feed's book disagreement is not reaching the engine")
	}
}

// TestSyntheticFeedIsDeterministicThroughTheEngine. The generator promises
// byte-identical snapshots for one seed and one instant; the normalizer's
// mapping is pure; the engine is a pure function of the record. So the whole
// chain must be reproducible, which is what makes a golden comparison or a
// bisect over pricing behaviour possible at all.
func TestSyntheticFeedIsDeterministicThroughTheEngine(t *testing.T) {
	t.Parallel()

	engine := mustEngine(t, Options{
		ReferenceBooks: []domain.Slug{domain.SyntheticBookSlug},
		DevigMethod:    odds.MethodPower,
	})

	priceAll := func() []byte {
		t.Helper()
		var out []ComputedMarket
		for _, rec := range normalizeSyntheticFeed(t) {
			c, err := engine.Price(context.Background(), rec)
			if err != nil {
				continue
			}
			out = append(out, c)
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	first, second := priceAll(), priceAll()
	if len(first) == 0 {
		t.Fatal("nothing was priced")
	}
	if string(first) != string(second) {
		t.Fatalf("two runs over the same seed and instant produced different computed prices "+
			"(%d vs %d bytes)", len(first), len(second))
	}
}
