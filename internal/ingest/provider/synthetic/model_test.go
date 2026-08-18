package synthetic

import (
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// -----------------------------------------------------------------------------
// Randomness
// -----------------------------------------------------------------------------

// TestStreamMatchesDraw pins the optimisation in noise.go to rand.go's
// definition.
//
// stream hoists the (seed, key) half of draw's mix out of the inner loop, which
// is worth roughly an order of magnitude at the volume a fetch works at. If the
// two ever disagree the package has two generators rather than one, and the
// determinism contract would hold for each of them separately while the model
// silently used a different sequence from the one rand.go documents.
func TestStreamMatchesDraw(t *testing.T) {
	for _, seed := range []int64{0, 1, -1, 4242, math.MaxInt64} {
		for _, key := range []string{"", "margin:syn-sba-20260817-3", "slow:x"} {
			s := newStream(seed, key)
			for c := int64(-3); c < 8; c++ {
				if got, want := s.bits(c), draw(seed, key, c); got != want {
					t.Fatalf("seed %d key %q counter %d: stream %d != draw %d", seed, key, c, got, want)
				}
				if got, want := s.unit(c), uniformAt(seed, key, c); got != want {
					t.Fatalf("seed %d key %q counter %d: unit %v != uniformAt %v", seed, key, c, got, want)
				}
				if got, want := s.normal(c), normalAt(seed, key, c); got != want {
					t.Fatalf("seed %d key %q counter %d: normal %v != normalAt %v", seed, key, c, got, want)
				}
			}
		}
	}
}

// TestNameMatchesKafkaProviderCharset is the assertion provider.Name's own
// comment names: this package's provider name must also be a legal
// kafka.Provider, and the two validators must agree on every candidate.
//
// A name one package accepted and the other rejected would produce a service
// publishing to a topic that cannot be created, with auto-creation disabled —
// which fails at run time, in the ingest path, rather than at build time.
func TestNameMatchesKafkaProviderCharset(t *testing.T) {
	if _, err := kafka.NewProvider(provider.NameSynthetic.String()); err != nil {
		t.Fatalf("kafka rejects this adapter's own name %q: %v", provider.NameSynthetic, err)
	}
	if string(neutralProviderSlug) != provider.NameSynthetic.String() {
		t.Fatalf("neutral decoder slug %q != provider name %q", neutralProviderSlug, provider.NameSynthetic)
	}

	corpus := []string{
		"synthetic", "the-odds-api", "a", "ab", "a-b", "0", "a1b2",
		"", "-x", "x-", "A", "a_b", "a.b", "a b", "a--b", "ä",
	}
	for _, c := range corpus {
		_, perr := provider.NewName(c)
		_, kerr := kafka.NewProvider(c)
		if (perr == nil) != (kerr == nil) {
			t.Errorf("%q: provider.NewName err=%v but kafka.NewProvider err=%v", c, perr, kerr)
		}
	}
}

func TestFloorDivRoundsTowardNegativeInfinity(t *testing.T) {
	cases := []struct{ a, b, want int64 }{
		{7, 3, 2}, {-7, 3, -3}, {6, 3, 2}, {-6, 3, -2}, {0, 3, 0}, {-1, 60, -1},
	}
	for _, c := range cases {
		if got := floorDiv(c.a, c.b); got != c.want {
			t.Errorf("floorDiv(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFloorToTickNeverLeavesTheAmericanBand(t *testing.T) {
	for _, tick := range []int64{1, 5, 10} {
		for _, v := range []int64{100, 101, 104, 109, -100, -101, -109, -3233, 49900} {
			got := floorToTick(v, tick)
			if got > v {
				t.Errorf("floorToTick(%d, %d) = %d, rounded up", v, tick, got)
			}
			if abs64(got) < 100 {
				t.Errorf("floorToTick(%d, %d) = %d, inside the (-100, +100) band", v, tick, got)
			}
			if got%tick != 0 {
				t.Errorf("floorToTick(%d, %d) = %d, not a multiple of the tick", v, tick, got)
			}
		}
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// -----------------------------------------------------------------------------
// The latent process
// -----------------------------------------------------------------------------

// TestLatentProcessIsStationaryAndUnitVariance checks the kernel normalisation.
//
// The league table states its dispersions as "the STATIONARY standard deviations
// of the two latent processes: how far the line itself wanders over a day", and
// the model multiplies a unit-variance path by them. A path whose variance was
// 0.8 would make every league's line move 10% less than the table says, for
// ever, with nothing failing.
func TestLatentProcessIsStationaryAndUnitVariance(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	sc := &scratch{}
	base := a.stepIndex(testNow)

	// Samples are spaced far enough apart to be effectively independent: the
	// slow component's half-life is twelve coarse steps, so 400 coarse steps is
	// past any correlation the kernel retains.
	const samples = 400
	sum, sumSq := 0.0, 0.0
	for i := 0; i < samples; i++ {
		p := a.evolve(sc, "variance-probe", base+int64(i)*slowGrid*slowKernelLen)
		v := p.views[0]
		sum += v
		sumSq += v * v
	}
	mean := sum / samples
	variance := sumSq/samples - mean*mean
	if math.Abs(mean) > 0.35 {
		t.Errorf("latent mean = %.3f, want near 0", mean)
	}
	// The tolerance is wide, and asymmetric upward, for two reasons: 400 samples
	// of a variance estimate carry real sampling error, and steam sits ON TOP of
	// the unit-variance mixture by design — it contributes roughly a further
	// quarter, so the observed variance should be a little above 1, never below.
	// The failure this guards against is a kernel normalisation bug, which
	// misses by tens of percent rather than by a few.
	if variance < 0.7 || variance > 2.2 {
		t.Errorf("latent variance = %.3f, want near 1 (kernel normalisation)", variance)
	}
}

// TestPathIndependence is the property rand.go's counter-based scheme exists
// for: the state at a step must not depend on how the caller got there.
func TestPathIndependence(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	n := a.stepIndex(testNow)

	// One scratch walked step by step, another jumping straight to the answer,
	// on two different adapters so no buffer is shared.
	walked := &scratch{}
	for i := int64(10); i > 0; i-- {
		a.evolve(walked, "path-probe", n-i)
	}
	direct := a.evolve(&scratch{}, "path-probe", n)
	after := a.evolve(walked, "path-probe", n)
	for i := range direct.views {
		if direct.views[i] != after.views[i] {
			t.Fatalf("view %d: walked %.17g != direct %.17g", i, after.views[i], direct.views[i])
		}
	}
}

// TestSteamMovesPropagateToBooksWithLag is the property phase 9's steam detector
// keys on.
//
// A steam move is a correlated jump in the latent process; each book quotes off
// its own lagged view of that process, so the sharp book reprices at the jump
// and the deepest-lagged book reprices maxBookLag steps later. Averaging over
// several jumps rather than asserting on one keeps the test from depending on
// whether one particular jump happened to land alongside a large noise step.
func TestSteamMovesPropagateToBooksWithLag(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	sc := &scratch{}
	const key = "steam-probe"

	occ := newStream(a.opts.Seed, "steam-occ:"+key)
	off := newStream(a.opts.Seed, "steam-off:"+key)
	amp := newStream(a.opts.Seed, "steam-amp:"+key)

	last := len(a.lags) - 1
	lag := int64(a.lags[last])
	if lag == 0 {
		t.Fatal("no lagged book in the book table; book disagreement cannot emerge")
	}

	var atJumpSharp, atJumpLagged, laterLagged float64
	found := 0
	b0 := a.stepIndex(testNow) / steamBlockSteps
	for b := b0; b < b0+4000 && found < 8; b++ {
		if occ.unit(b) >= steamProbability {
			continue
		}
		// Only large jumps, so the signal is unambiguously above step noise.
		if math.Abs((steamMinAbsZ+math.Abs(amp.normal(b)))*steamAmplitude) < 1.0 {
			continue
		}
		jn := b*steamBlockSteps + int64(off.index(b, steamBlockSteps))
		found++

		atJumpSharp += math.Abs(delta(a, sc, key, jn, 0))
		atJumpLagged += math.Abs(delta(a, sc, key, jn, last))
		laterLagged += math.Abs(delta(a, sc, key, jn+lag, last))
	}
	if found < 4 {
		t.Fatalf("only %d large steam moves in 4000 blocks; steamProbability is too low to test", found)
	}

	sharp := atJumpSharp / float64(found)
	quiet := atJumpLagged / float64(found)
	followed := laterLagged / float64(found)
	t.Logf("mean |Δ| — sharp at jump %.3f, lagged at jump %.3f, lagged %d steps later %.3f",
		sharp, quiet, lag, followed)

	if sharp < 2*quiet {
		t.Errorf("the sharp book barely moved at the jump (%.3f) relative to the untouched lagged book (%.3f)", sharp, quiet)
	}
	if followed < 2*quiet {
		t.Errorf("the lagged book did not follow %d steps later: %.3f vs a quiet %.3f", lag, followed, quiet)
	}
}

// delta is the one-step change in book i's view of a process.
func delta(a *Adapter, sc *scratch, key string, n int64, book int) float64 {
	before := a.evolve(sc, key, n-1)
	after := a.evolve(sc, key, n)
	return after.views[book] - before.views[book]
}

// TestSteamSuspendsTheMarketBriefly checks that suspension is short enough to
// leave the staggered reconvergence visible. A suspension covering the deepest
// book's lag would hide the entire steam signature, which is the opposite of
// what the model is for.
func TestSteamSuspendsTheMarketBriefly(t *testing.T) {
	if suspendSteps >= maxBookLag() {
		t.Fatalf("suspendSteps %d >= maxBookLag %d: suspension would hide the steam signal it exists to mark",
			suspendSteps, maxBookLag())
	}
}

// -----------------------------------------------------------------------------
// Vig
// -----------------------------------------------------------------------------

// TestPowerMarginInvertsUnderDevigPower is the sharpest statement of what this
// generator gives phase 4: a feed whose fair probabilities are KNOWN.
//
// The power overround is applied with the same q_i = p_i^k relation
// odds.DevigPower inverts, so devigging a power-quoted market must recover the
// exact probabilities the model started from.
func TestPowerMarginInvertsUnderDevigPower(t *testing.T) {
	fairs := [][]float64{
		{0.5, 0.5},
		{0.62, 0.38},
		{0.88, 0.12},
		{0.45, 0.27, 0.28},
		{0.30, 0.18, 0.14, 0.12, 0.10, 0.06, 0.05, 0.03, 0.015, 0.005},
	}
	for _, margin := range []float64{0.02, 0.045, 0.065} {
		for _, fair := range fairs {
			out := make([]float64, len(fair))
			if err := applyMargin(fair, out, margin, true); err != nil {
				t.Fatalf("applyMargin(%v, %g): %v", fair, margin, err)
			}
			sum := 0.0
			implied := make([]odds.Probability, len(out))
			for i, q := range out {
				sum += q
				implied[i] = odds.Probability(q)
			}
			if !approx(sum, 1+margin, 1e-9) {
				t.Errorf("power margin %g on %v: implied sum %.12f, want %.12f", margin, fair, sum, 1+margin)
			}
			res, err := odds.DevigPower(implied)
			if err != nil {
				t.Fatalf("DevigPower: %v", err)
			}
			for i := range fair {
				if !approx(float64(res.Probabilities[i]), fair[i], 1e-9) {
					t.Errorf("power margin %g: selection %d devigged to %.12f, want %.12f",
						margin, i, float64(res.Probabilities[i]), fair[i])
				}
			}
		}
	}
}

func TestMultiplicativeMarginHitsItsTarget(t *testing.T) {
	fair := []float64{0.62, 0.38}
	out := make([]float64, len(fair))
	for _, margin := range []float64{0.02, 0.038, 0.055} {
		if err := applyMargin(fair, out, margin, false); err != nil {
			t.Fatalf("applyMargin: %v", err)
		}
		sum := out[0] + out[1]
		if !approx(sum, 1+margin, 1e-12) {
			t.Errorf("multiplicative margin %g: sum %.12f, want %.12f", margin, sum, 1+margin)
		}
	}
}

// TestEveryQuotedMarketIsOverround is the property phase 4 needs to exist at
// all: a feed that emitted fair prices would leave the devigger nothing to
// remove and make the whole pricing engine untestable.
//
// The lower bound is the book's configured margin, because tick flooring only
// ever moves a price in the book's favour. The upper bound is loose because the
// coarsest book quotes in ten-cent American steps, which on a near-even market
// can add a couple of points of overround on its own.
func TestEveryQuotedMarketIsOverround(t *testing.T) {
	byBook := bookBySlug()
	checked := 0
	for _, l := range leagues() {
		snap := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))
		for _, ev := range snap.Events {
			for _, m := range ev.Markets {
				if len(m.Prices) == 0 {
					continue
				}
				for id, prices := range pricesByBook(m) {
					book, ok := byBook[id]
					if !ok {
						t.Fatalf("price from unknown book %s", id)
					}
					if len(prices) != len(m.Selections) {
						t.Fatalf("market %s book %s quoted %d of %d selections",
							m.Market.ID(), id, len(prices), len(m.Selections))
					}
					sum, err := odds.ImpliedSum(prices)
					if err != nil {
						t.Fatalf("market %s book %s: %v", m.Market.ID(), id, err)
					}
					if sum < 1+book.margin-1e-9 {
						t.Errorf("market %s book %s: implied sum %.6f is below the configured margin %.4f",
							m.Market.ID(), book.slug, sum, 1+book.margin)
					}
					if sum > 1+book.margin+0.08 {
						t.Errorf("market %s book %s: implied sum %.6f is far above the configured margin %.4f",
							m.Market.ID(), book.slug, sum, 1+book.margin)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no priced markets in the whole slate")
	}
	t.Logf("checked %d (market, book) quotes", checked)
}

// TestSharpBookIsTheTightest checks the ordering the book table encodes: the
// in-house reference book charges the least, so a +EV signal against a soft book
// is a statement about that book and not about the reference's own margin.
func TestSharpBookIsTheTightest(t *testing.T) {
	var reference bookDef
	for _, b := range books() {
		if b.reference {
			reference = b
			break
		}
	}
	if reference.slug == "" {
		t.Fatal("no reference book")
	}
	for _, b := range books() {
		if b.reference {
			continue
		}
		if b.margin <= reference.margin {
			t.Errorf("book %s charges %.4f, at or below the sharp reference's %.4f", b.slug, b.margin, reference.margin)
		}
	}
}

// -----------------------------------------------------------------------------
// Book disagreement
// -----------------------------------------------------------------------------

// TestBooksDisagree is CLAUDE.md §5's "book disagreement", asserted as a
// property of the model rather than of a comment.
//
// The measurement is on no-vig probabilities, so it is disagreement about the
// MARKET and not merely about the margin: two books with identical opinions and
// different margins quote different prices but devig to the same number.
func TestBooksDisagree(t *testing.T) {
	var (
		markets int
		spreads []float64
	)
	for _, l := range leagues() {
		snap := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))
		for _, ev := range snap.Events {
			for _, m := range ev.Markets {
				if len(m.Prices) == 0 || len(m.Selections) < 2 {
					continue
				}
				lo, hi := math.Inf(1), math.Inf(-1)
				n := 0
				for _, prices := range pricesByBook(m) {
					if len(prices) != len(m.Selections) {
						continue
					}
					implied := make([]odds.Probability, len(prices))
					for i, d := range prices {
						p, err := d.Probability()
						if err != nil {
							t.Fatalf("market %s: %v", m.Market.ID(), err)
						}
						implied[i] = p
					}
					res, err := odds.Devig(odds.MethodMultiplicative, implied)
					if err != nil {
						t.Fatalf("market %s: %v", m.Market.ID(), err)
					}
					v := float64(res.Probabilities[0])
					lo, hi = math.Min(lo, v), math.Max(hi, v)
					n++
				}
				if n < 2 {
					continue
				}
				markets++
				spreads = append(spreads, hi-lo)
			}
		}
	}
	if markets == 0 {
		t.Fatal("no multi-book markets in the slate")
	}

	agreed, best := 0, 0.0
	total := 0.0
	for _, s := range spreads {
		if s < 1e-9 {
			agreed++
		}
		total += s
		best = math.Max(best, s)
	}
	mean := total / float64(len(spreads))
	t.Logf("%d markets: mean no-vig disagreement %.4f, widest %.4f, unanimous on %d", markets, mean, best, agreed)

	if mean < 0.005 {
		t.Errorf("mean no-vig disagreement across books is %.5f; the books are effectively one book", mean)
	}
	if best < 0.03 {
		t.Errorf("the widest disagreement anywhere in the slate is %.5f; arbitrage and +EV surfaces would be empty", best)
	}
	if float64(agreed)/float64(len(spreads)) > 0.5 {
		t.Errorf("%d of %d markets have every book in exact agreement", agreed, len(spreads))
	}
}

// -----------------------------------------------------------------------------
// Change detection
// -----------------------------------------------------------------------------

// TestMostPollsReturnUnchangedPrices is what makes CLAUDE.md §5's change
// detection a measurable claim.
//
// "Most polls return identical data and must not generate bus traffic" is only
// demonstrable against a generator that does NOT manufacture a new number on
// every request. A per-call random walk would make every poll a change and the
// suppression rate would read 0% for ever — the metric would be live, the alert
// would be wired, and neither would ever have been exercised.
func TestMostPollsReturnUnchangedPrices(t *testing.T) {
	l := leagues()[0]
	scope := fullScope(l)

	// One model step apart: the finest granularity at which the board can move
	// at all, which is the hardest case for suppression.
	first := fetch(t, newTestAdapter(t, testSeed, testNow), scope)
	second := fetch(t, newTestAdapter(t, testSeed, testNow.Add(DefaultStep)), scope)

	prev := map[[2]string]domain.Price{}
	for _, ev := range first.Events {
		for _, m := range ev.Markets {
			for _, p := range m.Prices {
				prev[[2]string{string(p.SelectionID()), string(p.BookID())}] = p
			}
		}
	}

	same, changed := 0, 0
	for _, ev := range second.Events {
		for _, m := range ev.Markets {
			for _, p := range m.Prices {
				old, ok := prev[[2]string{string(p.SelectionID()), string(p.BookID())}]
				if !ok {
					continue
				}
				if old.SameQuoteAs(p) {
					same++
				} else {
					changed++
				}
			}
		}
	}
	total := same + changed
	if total == 0 {
		t.Fatal("no comparable prices across two polls")
	}
	rate := float64(same) / float64(total)
	t.Logf("one step apart: %d/%d prices unchanged (%.1f%%)", same, total, rate*100)
	if rate < 0.5 {
		t.Errorf("only %.1f%% of prices were unchanged one step apart; change detection has nothing to suppress", rate*100)
	}
	if changed == 0 {
		t.Error("nothing moved at all between two steps; the board would look frozen")
	}
}

// TestPricesMoveOverTheAfternoon is the other half: a generator that suppressed
// everything would pass the test above and show a dead board.
func TestPricesMoveOverTheAfternoon(t *testing.T) {
	l := leagues()[0]
	scope := fullScope(l)
	first := fetch(t, newTestAdapter(t, testSeed, testNow), scope)
	later := fetch(t, newTestAdapter(t, testSeed, testNow.Add(90*time.Minute)), scope)

	prev := map[[2]string]domain.Price{}
	for _, ev := range first.Events {
		for _, m := range ev.Markets {
			for _, p := range m.Prices {
				prev[[2]string{string(p.SelectionID()), string(p.BookID())}] = p
			}
		}
	}
	same, changed := 0, 0
	for _, ev := range later.Events {
		for _, m := range ev.Markets {
			for _, p := range m.Prices {
				old, ok := prev[[2]string{string(p.SelectionID()), string(p.BookID())}]
				if !ok {
					continue
				}
				if old.SameQuoteAs(p) {
					same++
				} else {
					changed++
				}
			}
		}
	}
	total := same + changed
	if total == 0 {
		t.Fatal("no comparable prices ninety minutes apart")
	}
	rate := float64(changed) / float64(total)
	t.Logf("ninety minutes apart: %d/%d prices moved (%.1f%%)", changed, total, rate*100)
	if rate < 0.25 {
		t.Errorf("only %.1f%% of prices moved over ninety minutes; the board is effectively static", rate*100)
	}
}

// -----------------------------------------------------------------------------
// Observation instants
// -----------------------------------------------------------------------------

// TestObservedAtIsTheModelInstantNotTheFetchInstant guards the failure
// provider.go calls out by name: an adapter that stamps its own receipt time
// into ObservedAt "does not produce a slightly-wrong metric; it produces a
// metric that reports zero provider staleness for ever".
//
// It also checks the staleness the lagged books introduce stays inside the
// le="120" bucket that sharpline-alerts.yml treats as the SLO boundary — a
// synthetic feed that blew the SLO by construction would make the offline demo
// permanently red.
func TestObservedAtIsTheModelInstantNotTheFetchInstant(t *testing.T) {
	// An instant deliberately off a step boundary, so a stamped fetch time and a
	// stamped model instant cannot coincide by luck.
	now := testNow.Add(3*time.Second + 271*time.Millisecond)
	a := newTestAdapter(t, testSeed, now)
	snap := fetch(t, a, fullScope(leagues()[0]))

	if !snap.FetchedAt.Equal(now) {
		t.Fatalf("FetchedAt = %s, want the fetch instant %s", snap.FetchedAt, now)
	}
	sawLag := false
	worst := time.Duration(0)
	prices := 0
	for _, ev := range snap.Events {
		for _, m := range ev.Markets {
			for _, p := range m.Prices {
				prices++
				age := p.Age(snap.FetchedAt)
				if age < 0 {
					t.Fatalf("price %s observed in the future of the fetch", p)
				}
				if p.ObservedAt().Equal(snap.FetchedAt) {
					t.Fatalf("price %s carries the fetch instant as its observation instant", p)
				}
				if age > worst {
					worst = age
				}
				if age > DefaultStep {
					sawLag = true
				}
			}
		}
	}
	if prices == 0 {
		t.Fatal("no prices")
	}
	if !sawLag {
		t.Error("no book quoted a lagged view; book disagreement would be margin-only")
	}
	if worst > 120*time.Second {
		t.Errorf("worst provider staleness %s exceeds the le=120 SLO bucket", worst)
	}
	t.Logf("%d prices, worst provider-attributable staleness %s", prices, worst)
}

// TestLiveEventsCarryAScoreAndClock checks that the in-play state and the price
// come from one model: the score is what the price is computed against, so an
// event that showed a clock but no score, or a score the market did not know
// about, would be two simulations rather than one.
func TestLiveEventsCarryAScoreAndClock(t *testing.T) {
	live, ended, scheduled := 0, 0, 0
	for _, l := range leagues() {
		snap := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))
		for _, ev := range snap.Events {
			_, hasClock := ev.Event.Clock()
			score, hasScore := ev.Event.Score()
			switch ev.Event.Status() {
			case domain.EventStatusLive:
				live++
				if !hasClock || !hasScore {
					t.Errorf("live event %s: clock=%v score=%v", ev.Event.ID(), hasClock, hasScore)
				}
				if score.Home() < 0 || score.Away() < 0 {
					t.Errorf("live event %s has a negative score %s", ev.Event.ID(), score)
				}
			case domain.EventStatusEnded:
				ended++
				if hasClock {
					t.Errorf("ended event %s still reports a clock", ev.Event.ID())
				}
				if !hasScore {
					t.Errorf("ended event %s has no final score", ev.Event.ID())
				}
				for _, m := range ev.Markets {
					if m.Market.Status() != domain.MarketStatusClosed {
						t.Errorf("ended event %s has a %s market", ev.Event.ID(), m.Market.Status())
					}
					if len(m.Prices) != 0 {
						t.Errorf("ended event %s still quotes %d prices", ev.Event.ID(), len(m.Prices))
					}
				}
			case domain.EventStatusScheduled:
				scheduled++
				if hasClock || hasScore {
					t.Errorf("scheduled event %s reports clock=%v score=%v", ev.Event.ID(), hasClock, hasScore)
				}
			default:
				t.Errorf("unexpected event status %s", ev.Event.Status())
			}
		}
	}
	t.Logf("slate: %d scheduled, %d live, %d ended", scheduled, live, ended)
	if live == 0 || ended == 0 || scheduled == 0 {
		t.Fatalf("slate does not exercise every lifecycle state: %d scheduled, %d live, %d ended", scheduled, live, ended)
	}
}

// TestScoreIsMonotoneOverTheContest checks the property the static-pace draw
// exists to guarantee. A board that un-scores a goal is worse than one showing
// no score at all, and it would happen automatically if the score were derived
// from the wandering latent process rather than from the event's static means.
func TestScoreIsMonotoneOverTheContest(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	l := leagues()[0]
	ev := a.buildMatch(l, testNow, 0, testNow)
	prevHome, prevAway := 0, 0
	for f := 0.0; f <= 1.0001; f += 0.01 {
		home, away := a.scoreAt(ev, f)
		if home < prevHome || away < prevAway {
			t.Fatalf("at f=%.2f the score went backwards: %d-%d after %d-%d", f, home, away, prevHome, prevAway)
		}
		prevHome, prevAway = home, away
	}
	if prevHome+prevAway == 0 {
		t.Fatal("the contest finished 0-0 at every fraction; the score model is not running")
	}
}
