// The cross-book scanners, exercised THROUGH the engine that publishes them.
//
// Every market here is built from latent probabilities by the same computed
// harness the rest of the suite uses — no canned arbitrage, no hand-written
// finding. Where a test needs an under-round market it says so by choosing the
// two books' prices, and it re-derives the expected return from those same
// prices rather than from a number typed into the assertion.
//
// The tests that matter most are the two staleness ones. An arbitrage whose legs
// were observed far apart is usually one book that has not repriced yet rather
// than free money, and a detector without that bound is a false-positive
// generator that looks exactly like a working feature.
package pricing

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/provider/synthetic"
)

// mustLine builds a line or fails.
func mustLine(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("NewLine(%v): %v", v, err)
	}
	return l
}

// underroundPair returns two books' decimal prices on a two-way market such that
// taking the BETTER price on each side sums below 1.
//
// Nothing is hand-picked: book A is generous on the home side and mean on the
// away side, book B the mirror, each with its own genuine margin. The best-of
// combination is what goes under 1 — which is exactly the shape a real
// cross-book arbitrage has, and neither book on its own is under-round.
func underroundPair(latent []float64) (a, b []float64) {
	a = []float64{latent[0] * 0.955, latent[1] * 1.070}
	b = []float64{latent[0] * 1.070, latent[1] * 0.955}
	return decimalsOf(a), decimalsOf(b)
}

// TestArbitrageIsFoundAndItsArithmeticIsRederived.
//
// The engine must publish the finding on the record it publishes the fair value
// on, and the finding's numbers must be reproducible from the legs it names. The
// implied sum and the return are recomputed here from the leg prices themselves,
// so this asserts the scanner's arithmetic rather than restating it.
func TestArbitrageIsFoundAndItsArithmeticIsRederived(t *testing.T) {
	t.Parallel()

	latent := []float64{0.52, 0.48}
	generousHome, generousAway := underroundPair(latent)

	rec := marketFixture{
		id:         "mkt-arb",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", reference: true, prices: decimalsOf(multiplicativeMargin(t, latent, 0.02))},
			{slug: "book-a", prices: generousHome},
			{slug: "book-b", prices: generousAway},
		},
	}.build(t)

	out, err := mustEngine(t, Options{}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(out.Arbitrage) != 1 {
		t.Fatalf("got %d arbitrage findings, want 1: %+v", len(out.Arbitrage), out.Arbitrage)
	}
	got := out.Arbitrage[0]

	if len(got.Legs) != len(twoWayRoles) {
		t.Fatalf("finding has %d legs, want one per outcome (%d)", len(got.Legs), len(twoWayRoles))
	}

	// Re-derive S and the return from the legs the finding itself names.
	sum := 0.0
	for _, l := range got.Legs {
		if l.Decimal <= 1 {
			t.Fatalf("leg %s carries decimal %v, which is not a price", l.SelectionID, l.Decimal)
		}
		sum += 1 / l.Decimal
	}
	if sum >= 1 {
		t.Fatalf("the finding's own legs sum to %.9f, which is not under-round", sum)
	}
	if !approxRel(got.Margin.ImpliedSum, sum, 1e-12) {
		t.Errorf("reported implied sum %.15f, legs give %.15f", got.Margin.ImpliedSum, sum)
	}
	if want := (1 - sum) / sum; !approxRel(got.Return, want, 1e-12) {
		t.Errorf("reported return %.15f, legs give %.15f", got.Return, want)
	}
	if got.DistinctBooks != 2 {
		t.Errorf("finding spans %d books, want 2 — one leg from each side's generous book",
			got.DistinctBooks)
	}

	// Stake fractions are the equalising split and must partition the outlay.
	stakes := 0.0
	for _, l := range got.Legs {
		stakes += l.StakeFraction
	}
	if !approxRel(stakes, 1, 1e-12) {
		t.Errorf("stake fractions sum to %.15f, want 1", stakes)
	}

	// A finding must not contradict the fair value on the same record: the
	// devigged probabilities still sum to one.
	fair := 0.0
	for _, f := range out.Fair.Selections {
		fair += float64(f.Probability)
	}
	if !approxRel(fair, 1, 1e-9) {
		t.Errorf("fair probabilities sum to %.15f on a record carrying an arbitrage", fair)
	}
}

// TestArbitrageStalenessWindowIsEnforced is the check that separates a feature
// from a firehose.
//
// The SAME under-round market is scanned twice. The only difference is when one
// book's quotes were observed. Beyond MaxLegSpread the finding must disappear,
// and it must come back when the bound is widened — which proves the bound is
// what suppressed it rather than the market having stopped being under-round.
func TestArbitrageStalenessWindowIsEnforced(t *testing.T) {
	t.Parallel()

	latent := []float64{0.52, 0.48}
	generousHome, generousAway := underroundPair(latent)

	const lag = 90 * time.Second
	build := func() marketFixture {
		return marketFixture{
			id:         "mkt-arb-stale",
			selections: twoWayRoles,
			books: []bookFixture{
				{slug: "sharpline", reference: true,
					prices: decimalsOf(multiplicativeMargin(t, latent, 0.02))},
				{slug: "book-a", prices: generousHome},
				// This book has not repriced in 90s. Its quote is what makes the
				// market look under-round, and that is the whole point: it is
				// stale, not generous.
				{slug: "book-b", prices: generousAway, observedAt: fixtureEpoch.Add(-lag)},
			},
		}
	}

	// Default policy: 30s spread bound, so a 90s gap is refused.
	tight, err := mustEngine(t, Options{}).Price(context.Background(), build().build(t))
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(tight.Arbitrage) != 0 {
		t.Fatalf("found %d arbitrage(s) whose legs are %s apart; the spread bound must refuse them: %+v",
			len(tight.Arbitrage), lag, tight.Arbitrage)
	}

	// Same market, bound widened past the gap. The finding returns, which is
	// the proof that the market really was under-round and that the suppression
	// above was the staleness policy doing its job.
	loose, err := mustEngine(t, Options{
		Arbitrage: ArbitrageConfig{
			MaxLegAge: 5 * time.Minute, MaxLegSpread: 2 * time.Minute,
			MinReturn: 0, MinDistinctBooks: 1,
		},
	}).Price(context.Background(), build().build(t))
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(loose.Arbitrage) != 1 {
		t.Fatalf("with a %s spread bound the same market yields %d finding(s), want 1",
			2*time.Minute, len(loose.Arbitrage))
	}
	if spread := loose.Arbitrage[0].ObservedSpreadSeconds; math.Abs(spread-lag.Seconds()) > 1e-9 {
		t.Errorf("reported spread %.3fs, want %.3fs", spread, lag.Seconds())
	}
}

// TestMiddleIsFoundWhenBooksQuoteDifferentLines.
//
// Two books quoting the SAME side at different lines is the input a middle is
// made of, and it is the case ev.go deliberately refuses to score as +EV. Here
// book-low takes the home side at -2.5 and book-high the away side at +4.5, so
// a home win by 3 or 4 wins both.
func TestMiddleIsFoundWhenBooksQuoteDifferentLines(t *testing.T) {
	t.Parallel()

	latent := []float64{0.5, 0.5}
	priced := decimalsOf(multiplicativeMargin(t, latent, 0.045))

	rec := marketFixture{
		id:         "mkt-middle",
		selections: twoWayRoles,
		line:       mustLine(t, -3.5),
		books: []bookFixture{
			{slug: "sharpline", reference: true,
				prices: decimalsOf(multiplicativeMargin(t, latent, 0.02)),
				lines:  []domain.Line{mustLine(t, -3.5), mustLine(t, 3.5)}},
			{slug: "book-low", prices: priced,
				lines: []domain.Line{mustLine(t, -2.5), mustLine(t, 2.5)}},
			{slug: "book-high", prices: priced,
				lines: []domain.Line{mustLine(t, -4.5), mustLine(t, 4.5)}},
		},
	}.build(t)
	rec.Market.Type = domain.MarketTypeSpread.String()

	out, err := mustEngine(t, Options{}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(out.Middles) == 0 {
		t.Fatalf("no middle found between a -2.5 home quote and a +4.5 away quote")
	}

	best := out.Middles[0]
	if best.High <= best.Low {
		t.Fatalf("window [%v, %v] is not a window", best.Low, best.High)
	}
	if best.WidthPoints != best.High-best.Low {
		t.Errorf("width %v does not equal High-Low (%v)", best.WidthPoints, best.High-best.Low)
	}
	if best.IntegerOutcomes < 1 {
		t.Errorf("window [%v, %v] contains no whole number, so it can never hit on an "+
			"integer-scored sport and should not have been reported", best.Low, best.High)
	}
	if best.Above.BookID == best.Below.BookID {
		t.Errorf("both legs come from %s; the default policy requires distinct books",
			best.Above.BookID)
	}
	if best.Axis != MiddleAxisHomeMargin.String() {
		t.Errorf("axis %q on a spread market, want %q", best.Axis, MiddleAxisHomeMargin)
	}

	// A middle is NOT an arbitrage: it costs its margin when the window is
	// missed, so the breakeven hit probability must be positive and equal the
	// pair's overround, which is the relation middles.go derives.
	if best.BreakevenHitProbability <= 0 {
		t.Errorf("breakeven hit probability %v; a middle that cannot lose would be an arbitrage",
			best.BreakevenHitProbability)
	}
	if !approxRel(best.BreakevenHitProbability, best.Margin.Overround, 1e-12) {
		t.Errorf("breakeven %.15f does not equal the pair's overround %.15f",
			best.BreakevenHitProbability, best.Margin.Overround)
	}
}

// TestMiddleStalenessWindowIsEnforced. Same argument as the arbitrage case: two
// books quoting different lines two minutes apart is one book that has not moved
// yet, and by the time both legs are struck the middle has closed.
func TestMiddleStalenessWindowIsEnforced(t *testing.T) {
	t.Parallel()

	latent := []float64{0.5, 0.5}
	priced := decimalsOf(multiplicativeMargin(t, latent, 0.045))
	const lag = 100 * time.Second

	build := func() marketFixture {
		return marketFixture{
			id:         "mkt-middle-stale",
			selections: twoWayRoles,
			line:       mustLine(t, -3.5),
			books: []bookFixture{
				{slug: "sharpline", reference: true,
					prices: decimalsOf(multiplicativeMargin(t, latent, 0.02)),
					lines:  []domain.Line{mustLine(t, -3.5), mustLine(t, 3.5)}},
				{slug: "book-low", prices: priced,
					lines: []domain.Line{mustLine(t, -2.5), mustLine(t, 2.5)}},
				{slug: "book-high", prices: priced,
					lines:      []domain.Line{mustLine(t, -4.5), mustLine(t, 4.5)},
					observedAt: fixtureEpoch.Add(-lag)},
			},
		}
	}

	rec := build().build(t)
	rec.Market.Type = domain.MarketTypeSpread.String()
	tight, err := mustEngine(t, Options{}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	for _, m := range tight.Middles {
		if m.ObservedSpreadSeconds > DefaultMiddleConfig().MaxLegSpread.Seconds() {
			t.Fatalf("reported a middle whose legs are %.0fs apart, past the %s bound: %+v",
				m.ObservedSpreadSeconds, DefaultMiddleConfig().MaxLegSpread, m)
		}
	}

	rec2 := build().build(t)
	rec2.Market.Type = domain.MarketTypeSpread.String()
	loose, err := mustEngine(t, Options{
		Middles: MiddleConfig{
			MaxLegAge: 5 * time.Minute, MaxLegSpread: 3 * time.Minute,
			MinWindow: 0.5, RequireIntegerOutcome: true, RequireDistinctBooks: true,
		},
	}).Price(context.Background(), rec2)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(loose.Middles) == 0 {
		t.Fatal("widening the spread bound found no middle, so the tight run proved nothing")
	}
}

// TestScanErrorDoesNotCostTheFairValue. The scanners are diagnostics over prices
// the devig has already used. A market they refuse must still publish its price.
func TestScanErrorDoesNotCostTheFairValue(t *testing.T) {
	t.Parallel()

	// A moneyline whose quotes carry a line. CrossBookMarket.Validate refuses it
	// (LineRuleForbidden), while nothing about the devig cares.
	rec := marketFixture{
		id:         "mkt-scan-refused",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", reference: true,
				prices: decimalsOf(multiplicativeMargin(t, []float64{0.55, 0.45}, 0.02)),
				lines:  []domain.Line{mustLine(t, -1.5), mustLine(t, 1.5)}},
		},
	}.build(t)

	out, err := mustEngine(t, Options{}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("a scan refusal cost the whole market its price: %v", err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("record does not validate: %v", err)
	}
	if len(out.Fair.Selections) != len(twoWayRoles) {
		t.Fatalf("fair value is incomplete: %d selections", len(out.Fair.Selections))
	}
	if len(out.Arbitrage) != 0 || len(out.Middles) != 0 {
		t.Errorf("a refused scan produced findings: %d arb, %d middles",
			len(out.Arbitrage), len(out.Middles))
	}
}

// TestScannersRunOverTheLiveSyntheticFeed drives the REAL generator — per-book
// margin, per-book bias, per-book view lag, American tick flooring — and reports
// what the scanners find over it.
//
// It asserts what must be true rather than what happens to be true today: every
// market is scanned without error, and every finding honours the configured
// staleness bounds. Whether the feed produces any arbitrage at all is a property
// of the generator's margins, not of this code, and is REPORTED rather than
// required — a test that demanded a finding would start failing the day the
// synthetic books were made tighter or looser.
func TestScannersRunOverTheLiveSyntheticFeed(t *testing.T) {
	t.Parallel()

	records := normalizeSyntheticFeed(t)
	engine := mustEngine(t, Options{})
	cfg := engine.ArbitrageConfig()
	midCfg := engine.MiddleConfig()

	var scanned, priced, arbs, mids, sameLine, diffLine int
	bestS := math.Inf(1)

	for _, rec := range records {
		out, err := engine.Price(context.Background(), rec)
		if err != nil {
			continue
		}
		priced++

		// The scan must have been attempted on a market the engine priced: the
		// snapshot it used is the snapshot the scanners take.
		snap, err := NewMarketSnapshot(rec)
		if err != nil {
			t.Fatalf("NewMarketSnapshot: %v", err)
		}
		cbm, err := CrossBookMarketFrom(snap)
		if err != nil {
			t.Fatalf("market %s: the pricing pass succeeded but the scan seam refused it: %v",
				rec.Market.ID, err)
		}
		scanned++

		// Counted in the market's own frame (a home -3.5 and an away +3.5 are
		// ONE line), which is the frame a middle is judged in.
		lines := map[float64]struct{}{}
		for _, p := range cbm.Prices {
			if v, ok := p.Line().Value(); ok {
				lines[math.Abs(v)] = struct{}{}
			}
		}
		if len(lines) > 1 {
			diffLine++
		} else if len(lines) == 1 {
			sameLine++
		}

		for _, a := range out.Arbitrage {
			arbs++
			if arbs <= 3 {
				// Printed, not asserted. A finding nobody ever reads is a
				// finding nobody can sanity-check, and the numbers below are
				// re-derivable from the legs by hand.
				t.Logf("finding %d: market=%s S=%.6f return=%+.4f%% books=%d spread=%s",
					arbs, rec.Market.ID, a.Margin.ImpliedSum, 100*a.Return,
					a.DistinctBooks, time.Duration(a.ObservedSpreadSeconds*float64(time.Second)))
				for _, l := range a.Legs {
					t.Logf("    %-6s %-28s dec=%.4f implied=%.6f stake=%.4f observed=%s",
						l.Role, l.BookID, l.Decimal, 1/l.Decimal, l.StakeFraction,
						l.ObservedAt.UTC().Format(time.RFC3339))
				}
			}
			if a.Margin.ImpliedSum < bestS {
				bestS = a.Margin.ImpliedSum
			}
			if a.ObservedSpreadSeconds > cfg.MaxLegSpread.Seconds() {
				t.Errorf("market %s: arbitrage legs %.0fs apart, past the %s bound",
					rec.Market.ID, a.ObservedSpreadSeconds, cfg.MaxLegSpread)
			}
			if a.OldestLegAgeSeconds > cfg.MaxLegAge.Seconds() {
				t.Errorf("market %s: oldest arbitrage leg %.0fs old, past the %s bound",
					rec.Market.ID, a.OldestLegAgeSeconds, cfg.MaxLegAge)
			}
			if a.Margin.ImpliedSum >= 1 {
				t.Errorf("market %s: reported an arbitrage whose implied sum is %.9f",
					rec.Market.ID, a.Margin.ImpliedSum)
			}
		}
		for _, m := range out.Middles {
			mids++
			if m.ObservedSpreadSeconds > midCfg.MaxLegSpread.Seconds() {
				t.Errorf("market %s: middle legs %.0fs apart, past the %s bound",
					rec.Market.ID, m.ObservedSpreadSeconds, midCfg.MaxLegSpread)
			}
			if m.High <= m.Low {
				t.Errorf("market %s: middle window [%v, %v] is empty", rec.Market.ID, m.Low, m.High)
			}
		}
	}

	if scanned == 0 {
		t.Fatal("no market reached the scanners; the seam is not wired")
	}
	t.Logf("synthetic slate: %d records, %d priced, %d scanned, %d arbitrage, %d middles, "+
		"best S=%v; line agreement: %d markets one line, %d markets several",
		len(records), priced, scanned, arbs, mids, bestS, sameLine, diffLine)
}

// TestSyntheticFeedQuotesOneLinePerMarket documents, as an assertion, WHY the
// middles count above is zero over the offline feed.
//
// internal/ingest/provider/synthetic/markets.go computes ONE line per market
// from the true unlagged latent process and applies each book's bias and view
// lag to the PROBABILITIES only. Every book therefore quotes the same line at a
// different price, and a middle needs two different lines. That is a property of
// the generator, not a gap in the detector — the constructed tests above prove
// the detector works — and stating it here means a future generator change that
// snapped lines per book would show up as this test failing rather than as a
// silent change in what the demo shows.
func TestSyntheticFeedQuotesOneLinePerMarket(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)
	adapter, err := synthetic.New(synthetic.Options{Seed: 20260817, Clock: func() time.Time { return at }})
	if err != nil {
		t.Fatalf("synthetic.New: %v", err)
	}
	ctx := context.Background()
	cat, err := adapter.Catalogue(ctx)
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}

	markets, multi := 0, 0
	for _, league := range cat.Leagues {
		snap, err := adapter.Fetch(ctx, provider.Scope{
			League:  league.ID(),
			Markets: []domain.MarketType{domain.MarketTypeSpread, domain.MarketTypeTotal},
		})
		if err != nil {
			t.Fatalf("Fetch(%s): %v", league.ID(), err)
		}
		for _, ev := range snap.Events {
			for _, m := range ev.Markets {
				markets++
				lines := map[float64]struct{}{}
				for _, p := range m.Prices {
					if v, ok := p.Line().Value(); ok {
						lines[math.Abs(v)] = struct{}{}
					}
				}
				if len(lines) > 1 {
					multi++
				}
			}
		}
	}
	if markets == 0 {
		t.Fatal("the generator produced no lined markets")
	}
	if multi != 0 {
		t.Fatalf("%d of %d generated markets carry more than one line. The generator has "+
			"changed: middles are now reachable from the offline feed, and "+
			"TestScannersRunOverTheLiveSyntheticFeed should assert findings rather than "+
			"report zero.", multi, markets)
	}
	t.Logf("%d lined markets generated, all quoted at a single line per market", markets)
}

// TestArbitrageStakesCannotLoseAfterRounding is the money-side invariant, and it
// is the one that would be silently wrong.
//
// Stake fractions are floats; stakes are int64 minor units (CLAUDE.md §12). The
// conversion is where an arbitrage stops being guaranteed: round the wrong way
// and a position that could not lose on paper loses a minor unit in fact. So the
// worst outcome is asserted to be strictly profitable at a realistic total, not
// merely close to break-even.
func TestArbitrageStakesCannotLoseAfterRounding(t *testing.T) {
	t.Parallel()

	latent := []float64{0.52, 0.48}
	generousHome, generousAway := underroundPair(latent)
	rec := marketFixture{
		id:         "mkt-arb-stakes",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", reference: true, prices: decimalsOf(multiplicativeMargin(t, latent, 0.02))},
			{slug: "book-a", prices: generousHome},
			{slug: "book-b", prices: generousAway},
		},
	}.build(t)

	out, err := mustEngine(t, Options{}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(out.Arbitrage) != 1 {
		t.Fatalf("expected one finding, got %d", len(out.Arbitrage))
	}

	// The engine publishes the wire shape; the Money arithmetic lives on the
	// domain finding, so rebuild it through the same scanner the engine ran.
	snap, err := NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	cbm, err := CrossBookMarketFrom(snap)
	if err != nil {
		t.Fatalf("CrossBookMarketFrom: %v", err)
	}
	found, err := NewArbitrageScanner(DefaultArbitrageConfig())
	if err != nil {
		t.Fatalf("NewArbitrageScanner: %v", err)
	}
	opps, err := found.ScanMarket(cbm, snap.Anchor())
	if err != nil || len(opps) != 1 {
		t.Fatalf("ScanMarket: %d opportunities, err %v", len(opps), err)
	}
	opp := opps[0]

	min, err := opp.MinimumTotalStake()
	if err != nil {
		t.Fatalf("MinimumTotalStake: %v", err)
	}
	if min <= 0 {
		t.Fatalf("minimum total stake %s is not positive", min)
	}

	for _, total := range []domain.Money{min, 10_000, 250_000} {
		stakes, err := opp.Stakes(total)
		if err != nil {
			t.Fatalf("Stakes(%s): %v", total, err)
		}
		if stakes.GuaranteedProfit <= 0 {
			t.Errorf("Stakes(%s): guaranteed profit %s; an arbitrage that can lose after "+
				"rounding is not an arbitrage", total, stakes.GuaranteedProfit)
		}
		if stakes.Outlay <= 0 {
			t.Errorf("Stakes(%s): outlay %s", total, stakes.Outlay)
		}
		sum := domain.Money(0)
		for _, st := range stakes.Stakes {
			sum += st
		}
		if sum != stakes.Outlay {
			t.Errorf("Stakes(%s): leg stakes sum to %s, outlay says %s", total, sum, stakes.Outlay)
		}
		// Every outcome must pay more than the whole outlay, which is the
		// guarantee restated over the integers actually staked.
		for i, r := range stakes.Returns {
			if r <= stakes.Outlay {
				t.Errorf("Stakes(%s): leg %d returns %s against an outlay of %s",
					total, i, r, stakes.Outlay)
			}
		}
	}

	if _, err := opp.Stakes(0); err == nil {
		t.Error("Stakes(0) succeeded; a non-positive total must be refused")
	}
}

// pointMass is a settlement distribution supplied BY THIS TEST, which is what
// [SettlementDistribution] exists for: nothing in this system estimates how
// often a window is hit, and a middle's expectation is only defined against a
// model the caller brings.
type pointMass struct {
	inside float64
	at     map[float64]float64
}

func (p pointMass) ProbabilityInInterval(float64, float64) (float64, error) { return p.inside, nil }
func (p pointMass) ProbabilityAt(v float64) (float64, error)                { return p.at[v], nil }

// TestMiddleStakesAndExpectation exercises the money and expectation halves of a
// middle, which the scanner path alone never reaches.
//
// The two claims that matter: a middle CAN lose (its worst profit is negative,
// which is what distinguishes it from an arbitrage), and the expectation crosses
// zero exactly at the breakeven hit probability the finding reports.
func TestMiddleStakesAndExpectation(t *testing.T) {
	t.Parallel()

	latent := []float64{0.5, 0.5}
	priced := decimalsOf(multiplicativeMargin(t, latent, 0.045))
	rec := marketFixture{
		id:         "mkt-middle-money",
		selections: twoWayRoles,
		line:       mustLine(t, -3.5),
		books: []bookFixture{
			{slug: "sharpline", reference: true,
				prices: decimalsOf(multiplicativeMargin(t, latent, 0.02)),
				lines:  []domain.Line{mustLine(t, -3.5), mustLine(t, 3.5)}},
			{slug: "book-low", prices: priced,
				lines: []domain.Line{mustLine(t, -2.5), mustLine(t, 2.5)}},
			{slug: "book-high", prices: priced,
				lines: []domain.Line{mustLine(t, -4.5), mustLine(t, 4.5)}},
		},
	}.build(t)
	rec.Market.Type = domain.MarketTypeSpread.String()

	snap, err := NewMarketSnapshot(rec)
	if err != nil {
		t.Fatalf("NewMarketSnapshot: %v", err)
	}
	cbm, err := CrossBookMarketFrom(snap)
	if err != nil {
		t.Fatalf("CrossBookMarketFrom: %v", err)
	}
	sc, err := NewMiddleScanner(DefaultMiddleConfig())
	if err != nil {
		t.Fatalf("NewMiddleScanner: %v", err)
	}
	found, err := sc.ScanMarket(cbm, snap.Anchor())
	if err != nil {
		t.Fatalf("ScanMarket: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no middle to price")
	}
	m := found[0]

	stakes, err := m.Stakes(100_000)
	if err != nil {
		t.Fatalf("Stakes: %v", err)
	}
	if stakes.HitProfit <= 0 {
		t.Errorf("hit profit %s; a middle that does not pay when it hits is not one", stakes.HitProfit)
	}
	if stakes.WorstProfit >= 0 {
		t.Errorf("worst profit %s; a middle that cannot lose would be an arbitrage and "+
			"must not be reported as a middle", stakes.WorstProfit)
	}
	if stakes.AboveStake+stakes.BelowStake != stakes.Outlay {
		t.Errorf("leg stakes %s + %s do not sum to outlay %s",
			stakes.AboveStake, stakes.BelowStake, stakes.Outlay)
	}
	if stakes.BreakevenHitProbability <= 0 || stakes.BreakevenHitProbability >= 1 {
		t.Fatalf("breakeven %v is not a probability", stakes.BreakevenHitProbability)
	}

	// Below the breakeven the position is negative; above it, positive. The
	// crossing point is the claim the finding makes, so it is asserted from both
	// sides rather than at one convenient value.
	below := stakes.BreakevenHitProbability / 2
	above := (stakes.BreakevenHitProbability + 1) / 2
	for _, tc := range []struct {
		p       float64
		wantPos bool
	}{{below, false}, {above, true}} {
		e, err := m.Expectation(stakes, pointMass{inside: tc.p, at: map[float64]float64{}})
		if err != nil {
			t.Fatalf("Expectation(hit=%v): %v", tc.p, err)
		}
		if gotPos := e.ExpectedProfitMinorUnits > 0; gotPos != tc.wantPos {
			t.Errorf("hit probability %.4f: expected profit %.2f minor units, want positive=%v "+
				"(breakeven %.4f)", tc.p, e.ExpectedProfitMinorUnits, tc.wantPos,
				stakes.BreakevenHitProbability)
		}
		if gotEdge := e.EdgeOverBreakeven > 0; gotEdge != tc.wantPos {
			t.Errorf("hit probability %.4f: edge over breakeven %v, want positive=%v",
				tc.p, e.EdgeOverBreakeven, tc.wantPos)
		}
	}

	if _, err := m.Expectation(stakes, nil); err == nil {
		t.Error("Expectation with no distribution succeeded; nothing in this system knows " +
			"the hit rate and a default would be a fabricated forecast")
	}
	// A model whose probabilities exceed 1 is a bug in the model, and letting it
	// through would produce an authoritative-looking arithmetic nonsense.
	if _, err := m.Expectation(stakes, pointMass{inside: 1.5, at: map[float64]float64{}}); err == nil {
		t.Error("Expectation accepted P(hit) = 1.5")
	}
}

// TestConfigDigestCoversEverySetting.
//
// The digest is what makes a configuration change REPRICE the slate instead of
// suppressing it: the service composes it into the message id and skips a
// republication whose id has not moved. A digest that covered only some settings
// would leave the rest silently frozen, which for a futures market means for
// ever. So every field is changed in turn and the digest must move.
func TestConfigDigestCoversEverySetting(t *testing.T) {
	t.Parallel()

	base := Options{ReferenceBooks: []domain.Slug{"sharpline"}}
	baseline := mustEngine(t, base).ConfigDigest()

	variants := map[string]Options{
		"devig method":     {ReferenceBooks: base.ReferenceBooks, DevigMethod: odds.MethodPower},
		"attribution":      {ReferenceBooks: base.ReferenceBooks, Attribution: odds.AttributionUniform},
		"kelly multiplier": {ReferenceBooks: base.ReferenceBooks, KellyMultiplier: 0.5},
		"max reference age": {ReferenceBooks: base.ReferenceBooks,
			MaxReferenceAge: 7 * time.Minute},
		"max quote age": {ReferenceBooks: base.ReferenceBooks, MaxQuoteAge: 3 * time.Minute},
		"method comparison": {ReferenceBooks: base.ReferenceBooks,
			SkipMethodComparison: true},
		"reference books": {ReferenceBooks: []domain.Slug{"pinnacle", "sharpline"}},
		"arbitrage config": {ReferenceBooks: base.ReferenceBooks, Arbitrage: ArbitrageConfig{
			MaxLegAge: 90 * time.Second, MaxLegSpread: 20 * time.Second,
			MinReturn: 0.002, MinDistinctBooks: 2,
		}},
		"middles config": {ReferenceBooks: base.ReferenceBooks, Middles: MiddleConfig{
			MaxLegAge: 90 * time.Second, MaxLegSpread: 20 * time.Second,
			MinWindow: 1, RequireIntegerOutcome: false, RequireDistinctBooks: false,
		}},
	}
	for name, o := range variants {
		if got := mustEngine(t, o).ConfigDigest(); got == baseline {
			t.Errorf("changing the %s left the digest at %s; that setting would be "+
				"silently frozen across a redeploy", name, got)
		}
	}

	// Stability is the other half: the same configuration must digest the same
	// on every process, or every restart would reprice the whole slate.
	if again := mustEngine(t, base).ConfigDigest(); again != baseline {
		t.Errorf("the same options digested %s then %s", baseline, again)
	}
}

// TestFindingsSurviveTheWire. The record is published to a compacted topic and
// read back by other services, so a finding that only exists in memory is not a
// finding. domain.Price has unexported fields and would marshal as an empty
// object, which is exactly why signals.go lifts the facts out explicitly.
func TestFindingsSurviveTheWire(t *testing.T) {
	t.Parallel()

	latent := []float64{0.52, 0.48}
	generousHome, generousAway := underroundPair(latent)
	rec := marketFixture{
		id:         "mkt-arb-wire",
		selections: twoWayRoles,
		books: []bookFixture{
			{slug: "sharpline", reference: true, prices: decimalsOf(multiplicativeMargin(t, latent, 0.02))},
			{slug: "book-a", prices: generousHome},
			{slug: "book-b", prices: generousAway},
		},
	}.build(t)

	out, err := mustEngine(t, Options{}).Price(context.Background(), rec)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ComputedMarket
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("the decoded record does not validate: %v", err)
	}
	if len(back.Arbitrage) != len(out.Arbitrage) {
		t.Fatalf("%d findings survived the wire, %d went in", len(back.Arbitrage), len(out.Arbitrage))
	}
	for i, a := range back.Arbitrage {
		want := out.Arbitrage[i]
		if len(a.Legs) != len(want.Legs) {
			t.Fatalf("finding %d: %d legs survived, %d went in", i, len(a.Legs), len(want.Legs))
		}
		for j, l := range a.Legs {
			w := want.Legs[j]
			if l.Decimal != w.Decimal || l.BookID != w.BookID || !l.ObservedAt.Equal(w.ObservedAt) {
				t.Errorf("finding %d leg %d: got %+v, want %+v", i, j, l, w)
			}
			if l.Decimal == 0 {
				t.Errorf("finding %d leg %d marshalled its price away", i, j)
			}
		}
		if a.Return != want.Return || a.Margin.ImpliedSum != want.Margin.ImpliedSum {
			t.Errorf("finding %d: return/S drifted across the wire", i)
		}
	}
}
