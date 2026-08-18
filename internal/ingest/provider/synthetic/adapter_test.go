package synthetic

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/scheduler"
	"github.com/anpl1623/sharpline/internal/platform/config"
)

// The instants every test is written against.
//
// They are fixed rather than derived from time.Now because the whole contract
// under test is "the same seed and the same instants give the same answer": a
// test that read the wall clock would be asserting a weaker property and would
// fail on one afternoon a year when the slate happened to be empty.
var (
	testNow  = time.Date(2026, 8, 17, 19, 42, 11, 0, time.UTC)
	testSeed = int64(4242)
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func newTestAdapter(t *testing.T, seed int64, now time.Time) *Adapter {
	t.Helper()
	a, err := New(Options{Seed: seed, Clock: fixedClock(now)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// allMarkets is every market type the generator can serve.
func allMarkets() []domain.MarketType {
	return []domain.MarketType{
		domain.MarketTypeMoneyline,
		domain.MarketTypeSpread,
		domain.MarketTypeTotal,
		domain.MarketTypePlayerProp,
		domain.MarketTypeFutures,
	}
}

func fullScope(l leagueDef) provider.Scope {
	return provider.Scope{League: l.leagueID(), Markets: allMarkets()}
}

func fetch(t *testing.T, a *Adapter, scope provider.Scope) provider.Snapshot {
	t.Helper()
	snap, err := a.Fetch(context.Background(), scope)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", scope, err)
	}
	return snap
}

// -----------------------------------------------------------------------------
// The seam
// -----------------------------------------------------------------------------

func TestAdapterSatisfiesProviderAdapter(t *testing.T) {
	var a provider.ProviderAdapter = newTestAdapter(t, testSeed, testNow)
	if a.Name() != provider.NameSynthetic {
		t.Fatalf("Name() = %q, want %q", a.Name(), provider.NameSynthetic)
	}
}

func TestZeroOptionsIsUsable(t *testing.T) {
	// The offline path is literally synthetic.New(synthetic.Options{}); if that
	// stops working, a clone of the repository with no API key has no feed.
	a, err := New(Options{})
	if err != nil {
		t.Fatalf("New(zero): %v", err)
	}
	if a.opts.Seed != DefaultSeed {
		t.Fatalf("seed = %d, want DefaultSeed %d", a.opts.Seed, DefaultSeed)
	}
	if _, err := a.Catalogue(context.Background()); err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
}

func TestOptionsValidation(t *testing.T) {
	cases := map[string]Options{
		"step too small":   {Step: time.Millisecond},
		"step too large":   {Step: 2 * time.Hour},
		"no slate":         {SlateDays: -1},
		"slate too long":   {SlateDays: maxSlateDays + 1},
		"no events":        {EventsPerLeaguePerDay: -1},
		"too many events":  {EventsPerLeaguePerDay: maxEventsPerLeaguePerDay + 1},
		"negative budget":  {QuotaBudget: -1},
		"negative period":  {QuotaPeriod: -time.Second},
		"negative timeout": {Timeout: -time.Second},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("New(%+v) error = %v, want ErrInvalidOptions", opts, err)
			}
		})
	}
}

// TestEnvSeedMatchesPlatformConfig keeps the two spellings of the frozen
// environment variable from drifting.
//
// internal/platform/config parses it and `ingest` passes the result to New; this
// package only names it. Two constants holding one string is acceptable — a
// dependency from the provider to the platform config would be worse — but only
// while something checks that they are the same string.
func TestEnvSeedMatchesPlatformConfig(t *testing.T) {
	if EnvSeed != config.EnvSyntheticSeed {
		t.Fatalf("synthetic.EnvSeed = %q but config.EnvSyntheticSeed = %q", EnvSeed, config.EnvSyntheticSeed)
	}
}

// TestUnsetSeedIsAFixedConstantNotTheClock pins the behaviour an unconfigured
// clone gets.
//
// internal/platform/config leaves SyntheticSeed at zero when the variable is
// unset, and `ingest` passes that zero straight into Options.Seed. Zero must
// therefore mean a FIXED default, not "seed from the clock": a generator that
// reseeded itself per process would give a different board on every restart,
// which is exactly the property the seed exists to remove and which no test
// downstream could then rely on.
func TestUnsetSeedIsAFixedConstantNotTheClock(t *testing.T) {
	l := leagues()[0]
	first := fetch(t, newTestAdapter(t, 0, testNow), fullScope(l))
	second := fetch(t, newTestAdapter(t, 0, testNow), fullScope(l))
	if diff := diffSnapshots(first, second); diff != "" {
		t.Fatalf("an unset seed produced two different boards: %s", diff)
	}
	explicit := fetch(t, newTestAdapter(t, DefaultSeed, testNow), fullScope(l))
	if diff := diffSnapshots(first, explicit); diff != "" {
		t.Fatalf("seed 0 does not resolve to DefaultSeed: %s", diff)
	}
}

// -----------------------------------------------------------------------------
// Determinism
// -----------------------------------------------------------------------------

func TestSameSeedProducesIdenticalSnapshots(t *testing.T) {
	for _, l := range leagues() {
		a := newTestAdapter(t, testSeed, testNow)
		b := newTestAdapter(t, testSeed, testNow)
		first := fetch(t, a, fullScope(l))
		second := fetch(t, b, fullScope(l))
		if diff := diffSnapshots(first, second); diff != "" {
			t.Fatalf("league %s: two adapters on seed %d disagree: %s", l.key, testSeed, diff)
		}
	}
}

// TestRestartReproducesTheSameBoard is the cross-restart half of the contract.
//
// Constructing a second adapter is exactly what a process restart does — the
// type holds no model state — so this is the in-process form of "stop the
// binary, start it again, get the same board". It is worth asserting separately
// from the two-adapters-at-once case because the failure it guards against is a
// generator that seeds itself from its construction instant, which would pass
// that test and fail this one only when the two constructions were seconds
// apart.
func TestRestartReproducesTheSameBoard(t *testing.T) {
	l := leagues()[0]
	before := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))

	// A "restart": a brand-new adapter, built later, asked about the same
	// instant. Only the model instant is shared; nothing else survives.
	restarted, err := New(Options{Seed: testSeed, Clock: fixedClock(testNow)})
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	after := fetch(t, restarted, fullScope(l))
	if diff := diffSnapshots(before, after); diff != "" {
		t.Fatalf("a restart changed the board: %s", diff)
	}
}

func TestDifferentSeedsProduceDifferentBoards(t *testing.T) {
	l := leagues()[0]
	a := fetch(t, newTestAdapter(t, testSeed, testNow), fullScope(l))
	b := fetch(t, newTestAdapter(t, testSeed+1, testNow), fullScope(l))
	if diffSnapshots(a, b) == "" {
		t.Fatal("seeds 4242 and 4243 produced identical boards; the seed is not reaching the model")
	}
}

// TestMarketIdentityIsStableAcrossPollsAndRestarts is the property compaction
// depends on.
//
// odds.normalized is keyed by market id and compacted, so a market whose
// identifier moved between polls stops collapsing and starts accumulating —
// silently. The scan runs across six hours so that events cross from scheduled
// into live and out again, which is when a naive identifier derived from status
// or from the current day would change.
func TestMarketIdentityIsStableAcrossPollsAndRestarts(t *testing.T) {
	l := leagues()[1]
	seen := map[domain.EventID]map[domain.MarketID]domain.MarketType{}

	for minutes := 0; minutes <= 6*60; minutes += 17 {
		at := testNow.Add(time.Duration(minutes) * time.Minute)
		// A fresh adapter on every poll: the strongest form of the claim, since
		// it also re-derives the slate from scratch each time.
		snap := fetch(t, newTestAdapter(t, testSeed, at), fullScope(l))
		for _, ev := range snap.Events {
			known, ok := seen[ev.Event.ID()]
			if !ok {
				known = map[domain.MarketID]domain.MarketType{}
				seen[ev.Event.ID()] = known
			}
			for _, m := range ev.Markets {
				if prev, ok := known[m.Market.ID()]; ok && prev != m.Market.Type() {
					t.Fatalf("market %s was a %s and is now a %s", m.Market.ID(), prev, m.Market.Type())
				}
				known[m.Market.ID()] = m.Market.Type()
				if m.Market.EventID() != ev.Event.ID() {
					t.Fatalf("market %s claims event %s inside event %s",
						m.Market.ID(), m.Market.EventID(), ev.Event.ID())
				}
			}
		}
	}

	// Every event must have carried a stable core market set for its whole life,
	// not a set that churned identifiers as the clock moved.
	for id, markets := range seen {
		types := map[domain.MarketType]int{}
		for _, ty := range markets {
			types[ty]++
		}
		for _, ty := range []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread, domain.MarketTypeTotal} {
			if n := types[ty]; n > 1 {
				t.Fatalf("event %s accumulated %d distinct %s market identifiers over its life", id, n, ty)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no events observed across six hours of polling")
	}
}

// diffSnapshots reports the first difference between two snapshots, or "".
func diffSnapshots(a, b provider.Snapshot) string {
	if a.Provider != b.Provider {
		return "provider"
	}
	if len(a.Events) != len(b.Events) {
		return "event count"
	}
	for i := range a.Events {
		ea, eb := a.Events[i], b.Events[i]
		if ea.Event.ID() != eb.Event.ID() || ea.Event.Status() != eb.Event.Status() {
			return "event " + string(ea.Event.ID())
		}
		if string(ea.Raw.Body) != string(eb.Raw.Body) {
			return "raw payload for event " + string(ea.Event.ID())
		}
		if len(ea.Markets) != len(eb.Markets) {
			return "market count on event " + string(ea.Event.ID())
		}
		for j := range ea.Markets {
			ma, mb := ea.Markets[j], eb.Markets[j]
			if ma.Market.ID() != mb.Market.ID() || ma.Market.Status() != mb.Market.Status() {
				return "market " + string(ma.Market.ID())
			}
			if !ma.Market.Line().Equal(mb.Market.Line()) {
				return "line on market " + string(ma.Market.ID())
			}
			if len(ma.Prices) != len(mb.Prices) {
				return "price count on market " + string(ma.Market.ID())
			}
			for k := range ma.Prices {
				if !ma.Prices[k].Equal(mb.Prices[k]) {
					return "price on market " + string(ma.Market.ID())
				}
			}
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// The slate
// -----------------------------------------------------------------------------

// TestSlateCoversEveryPollingWindow is the reason the calendar is shaped the way
// it is: a scheduler tier with no events in it is a tier nobody has run.
//
// The census is taken over a full fixture cycle rather than at one instant, and
// that is a deliberate weakening of the claim rather than a convenience. Near-tip
// is the last half hour before a start, and with eight fixtures a day per league
// staggered 45 minutes apart there are stretches of the cycle with no start
// imminent anywhere — which is also true of a real Tuesday afternoon. What must
// hold, and what is asserted, is that every tier is reached within one cycle, and
// that the tiers that carry the demo (live and futures) are populated at EVERY
// instant.
func TestSlateCoversEveryPollingWindow(t *testing.T) {
	b := scheduler.DefaultBoundaries()
	cycle := 24 * time.Hour / time.Duration(DefaultEventsPerLeaguePerDay)

	total := map[scheduler.Window]int{}
	instants := 0
	nearTipInstants := 0
	for offset := time.Duration(0); offset < cycle; offset += 15 * time.Minute {
		now := testNow.Add(offset)
		instants++
		at := map[scheduler.Window]int{}
		for _, l := range leagues() {
			snap := fetch(t, newTestAdapter(t, testSeed, now), fullScope(l))
			for _, ev := range snap.Events {
				w := scheduler.ClassifyEvent(ev.Event, now, b)
				at[w]++
				total[w]++
			}
		}
		if at[scheduler.WindowNearTip] > 0 {
			nearTipInstants++
		}
		for _, w := range []scheduler.Window{scheduler.WindowLive, scheduler.WindowFutures, scheduler.WindowDistant} {
			if at[w] == 0 {
				t.Errorf("at %s there is no event in window %s", now.Format(time.RFC3339), w)
			}
		}
	}

	for _, w := range scheduler.Windows() {
		if total[w] == 0 {
			t.Errorf("no event in window %s anywhere in a fixture cycle; the scheduler tier is untested by the offline feed", w)
		}
	}
	t.Logf("census over %d instants: %v; near-tip populated at %d of them", instants, total, nearTipInstants)
	if nearTipInstants == 0 {
		t.Error("the near-tip tier is never populated")
	}
}

// TestLeagueGapsDoNotOverlap checks the tiling claim universe.go makes about
// leagueOffset: with the default grid the four leagues' between-fixture gaps are
// disjoint, so at least three leagues are in play at every instant.
//
// It is asserted rather than assumed because it is a numeric coincidence between
// three constants in two files — the fixture spacing (24h / DefaultEventsPer-
// LeaguePerDay), liveDuration, and leagueOffset's 45-minute step — and changing
// any one of them silently breaks it.
func TestLeagueGapsDoNotOverlap(t *testing.T) {
	spacing := 24 * time.Hour / time.Duration(DefaultEventsPerLeaguePerDay)
	worst := len(leagues())
	for offset := time.Duration(0); offset < spacing; offset += time.Minute {
		now := testNow.Truncate(spacing).Add(offset)
		live := 0
		for _, l := range leagues() {
			snap := fetch(t, newTestAdapter(t, testSeed, now), fullScope(l))
			for _, ev := range snap.Events {
				if ev.Event.IsInPlay() {
					live++
					break
				}
			}
		}
		if live < worst {
			worst = live
		}
	}
	if worst < 3 {
		t.Fatalf("at the worst instant only %d of %d leagues were in play; universe.go claims at least three",
			worst, len(leagues()))
	}
}

// TestScopeNarrowing checks that both narrowings the interface offers are
// honoured. An adapter that ignored Scope.Markets would bill the scheduler for
// markets it did not ask for; one that ignored Scope.Events would defeat the
// live-events-only sweep entirely.
func TestScopeNarrowing(t *testing.T) {
	l := leagues()[0]
	a := newTestAdapter(t, testSeed, testNow)

	only := provider.Scope{League: l.leagueID(), Markets: []domain.MarketType{domain.MarketTypeTotal}}
	snap := fetch(t, a, only)
	for _, ev := range snap.Events {
		for _, m := range ev.Markets {
			if m.Market.Type() != domain.MarketTypeTotal {
				t.Fatalf("scope asked for totals only, got a %s market", m.Market.Type())
			}
		}
	}

	full := fetch(t, a, fullScope(l))
	if len(full.Events) == 0 {
		t.Fatal("empty slate")
	}
	want := full.Events[0].Event.ID()
	narrowed := fetch(t, a, provider.Scope{League: l.leagueID(), Markets: allMarkets(), Events: []domain.EventID{want}})
	if len(narrowed.Events) != 1 || narrowed.Events[0].Event.ID() != want {
		t.Fatalf("event narrowing returned %d events, want exactly %s", len(narrowed.Events), want)
	}
}

func TestUnknownLeagueIsFatal(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	_, err := a.Fetch(context.Background(), provider.Scope{
		League:  domain.LeagueID("no-such-league"),
		Markets: allMarkets(),
	})
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if !provider.IsFatal(err) {
		t.Fatalf("disposition = %s, want fatal: retrying cannot conjure a league", provider.Classify(err))
	}
}

// -----------------------------------------------------------------------------
// The universe is visibly a simulation
// -----------------------------------------------------------------------------

// TestEverythingIsMarkedSynthetic enforces CLAUDE.md §0's line: the simulation
// must never be mistakable for a real market. Every book is
// domain.BookKindSynthetic, so a +EV or arbitrage signal computed against one is
// identifiable as a statement about a random number generator.
func TestEverythingIsMarkedSynthetic(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	c, err := a.Catalogue(context.Background())
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	if len(c.Books) == 0 || len(c.Leagues) == 0 || len(c.Sports) == 0 {
		t.Fatalf("catalogue is empty: %d sports, %d leagues, %d books", len(c.Sports), len(c.Leagues), len(c.Books))
	}
	for _, b := range c.Books {
		if !b.IsSynthetic() {
			t.Errorf("book %s is %s, every simulated book must be synthetic", b.ID(), b.Kind())
		}
	}
	if _, ok := c.ReferenceBook(); !ok {
		t.Error("no sharp reference book; the +EV surface would be permanently empty")
	}
	for _, l := range c.Leagues {
		if len(l.Name()) < len("Synthetic") || l.Name()[:len("Synthetic")] != "Synthetic" {
			t.Errorf("league %q is not self-identifying as a simulation", l.Name())
		}
	}
}

// -----------------------------------------------------------------------------
// Quota
// -----------------------------------------------------------------------------

func TestCostMirrorsMarketCount(t *testing.T) {
	// A clock that fails the test if it is read proves the no-clock half of the
	// interface contract mechanically rather than by inspection.
	var reads int
	a, err := New(Options{Seed: testSeed, Clock: func() time.Time {
		reads++
		return testNow
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before := reads
	got := a.Cost(provider.Scope{League: leagues()[0].leagueID(), Markets: allMarkets()})
	if got != len(allMarkets()) {
		t.Fatalf("Cost = %d, want %d", got, len(allMarkets()))
	}
	if reads != before {
		t.Fatalf("Cost read the clock %d times; the interface forbids it", reads-before)
	}
}

func TestQuotaIsChargedAndExhausts(t *testing.T) {
	l := leagues()[0]
	scope := fullScope(l)
	// A budget of exactly two sweeps, refilling over a century so the refill
	// cannot mask the drain during the test.
	budget := int64(2 * len(allMarkets()))
	a, err := New(Options{
		Seed:        testSeed,
		Clock:       fixedClock(testNow),
		QuotaBudget: budget,
		QuotaPeriod: 100 * 365 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if q := a.Quota(); !q.Known || q.Remaining != budget || q.Limit != budget {
		t.Fatalf("initial quota = %s, want a full known budget of %d", q, budget)
	}

	snap := fetch(t, a, scope)
	if snap.Quota.Remaining != budget-int64(len(allMarkets())) {
		t.Fatalf("after one sweep remaining = %d, want %d", snap.Quota.Remaining, budget-int64(len(allMarkets())))
	}
	fetch(t, a, scope)

	_, err = a.Fetch(context.Background(), scope)
	if !provider.IsQuotaExhausted(err) {
		t.Fatalf("third sweep error = %v, want quota exhaustion", err)
	}
	if !errors.Is(err, provider.ErrQuotaExhausted) {
		t.Fatalf("error does not wrap ErrQuotaExhausted: %v", err)
	}
}

func TestQuotaRefills(t *testing.T) {
	now := testNow
	clock := func() time.Time { return now }
	a, err := New(Options{Seed: testSeed, Clock: clock, QuotaBudget: 100, QuotaPeriod: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Fetch(context.Background(), fullScope(leagues()[0])); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	drained := a.Quota().Remaining
	now = now.Add(30 * time.Minute)
	if got := a.Quota().Remaining; got <= drained {
		t.Fatalf("after half a period remaining = %d, want more than %d", got, drained)
	}
	now = now.Add(24 * time.Hour)
	if got := a.Quota().Remaining; got != 100 {
		t.Fatalf("after a full period remaining = %d, want the budget cap 100", got)
	}
}

// -----------------------------------------------------------------------------
// Concurrency
// -----------------------------------------------------------------------------

// TestConcurrentFetchIsSafe exercises the "a goroutine per poller, sharing one
// adapter" shape provider.Adapter documents. Under -race it is the check that
// the credit bucket is the only mutable state and that it is guarded.
func TestConcurrentFetchIsSafe(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	var wg sync.WaitGroup
	errs := make(chan error, len(leagues())*4)
	for i := 0; i < 4; i++ {
		for _, l := range leagues() {
			wg.Add(1)
			go func(l leagueDef) {
				defer wg.Done()
				if _, err := a.Fetch(context.Background(), fullScope(l)); err != nil {
					errs <- err
				}
			}(l)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Fetch: %v", err)
	}
}

func TestCancelledContextIsReported(t *testing.T) {
	a := newTestAdapter(t, testSeed, testNow)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Fetch(ctx, fullScope(leagues()[0])); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := a.Catalogue(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Catalogue error = %v, want context.Canceled", err)
	}
}

// -----------------------------------------------------------------------------
// Shared helpers for the pricing tests
// -----------------------------------------------------------------------------

// bookBySlug indexes the simulated books by the identifier their prices carry.
func bookBySlug() map[domain.BookID]bookDef {
	out := map[domain.BookID]bookDef{}
	for _, b := range books() {
		out[bookID(b.slug)] = b
	}
	return out
}

// pricesByBook groups a market's prices by the book that quoted them, preserving
// the market's selection order so the vector can be summed as a market.
func pricesByBook(m provider.MarketSnapshot) map[domain.BookID][]odds.Decimal {
	out := map[domain.BookID][]odds.Decimal{}
	for _, sel := range m.Selections {
		for _, p := range m.Prices {
			if p.SelectionID() == sel.ID() {
				out[p.BookID()] = append(out[p.BookID()], odds.Decimal(p.Decimal()))
			}
		}
	}
	return out
}

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
