// The detector, tested twice over: as arithmetic against hand-built
// observations, and as a detector against the REAL synthetic generator.
//
// The unit tests below pin the semantics that phase 12 has to reproduce — the
// half-open window boundary, the epoch-aligned grid, the lead/follower rule, the
// tie-breaks, the cooldown — because those are the terms a SQL rewrite gets
// wrong quietly. The generator-driven tests make the weaker claim over the harder
// input, and they are the two gate items: THE DETECTOR MUST FIRE ON A GENERATED
// STEAM MOVE, AND MUST NOT FIRE ON ORDINARY DRIFT. A detector that satisfies only
// the first is a detector that alerts on everything.
package steam

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/provider/synthetic"
)

// -----------------------------------------------------------------------------
// The window grid
// -----------------------------------------------------------------------------

// TestWindowGridIsEpochAlignedAndHalfOpen pins the two properties that decide
// which observations belong to which window.
//
// They are asserted rather than left to the implementation because they are the
// difference between this detector and a Flink HOP agreeing about a boundary and
// silently disagreeing about it, which is the failure phase 12 would find as "one
// extra finding here, one missing there" with no obvious cause.
func TestWindowGridIsEpochAlignedAndHalfOpen(t *testing.T) {
	const (
		window = 3 * time.Minute
		hop    = time.Minute
	)
	epoch := time.Unix(0, 0).UTC()

	tests := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{
			name: "exactly on a window end belongs to that end",
			at:   epoch.Add(window),
			want: epoch.Add(window),
		},
		{
			name: "one nanosecond before a window end falls to the previous one",
			at:   epoch.Add(window).Add(-time.Nanosecond),
			want: epoch.Add(window - hop),
		},
		{
			name: "a fractional instant floors to the grid",
			at:   epoch.Add(10*time.Minute + 37*time.Second),
			want: epoch.Add(10 * time.Minute),
		},
		{
			name: "an instant before the epoch floors toward negative infinity",
			at:   epoch.Add(-10 * time.Second),
			want: epoch.Add(-time.Minute),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := windowEndAtOrBefore(tc.at, window, hop)
			if !got.Equal(tc.want) {
				t.Fatalf("windowEndAtOrBefore(%s) = %s, want %s",
					tc.at.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano),
					tc.want.Format(time.RFC3339Nano))
			}
		})
	}
}

// TestWindowBoundIsHalfOpen asserts that an observation stamped exactly at a
// window's end is NOT in that window.
//
// It is the single most reproducible-in-SQL property in this package and the one
// a rewrite is most likely to get wrong, because `BETWEEN` in SQL is inclusive on
// both sides and reads as the obvious translation of a window.
func TestWindowBoundIsHalfOpen(t *testing.T) {
	start := time.Unix(0, 0).UTC()
	end := start.Add(time.Minute)

	s := []point{
		{at: start, p: 0.50},
		{at: end.Add(-time.Second), p: 0.55},
		{at: end, p: 0.90}, // must be excluded
	}
	m, ok := moveIn(s, start, end)
	if !ok {
		t.Fatal("moveIn refused a window with two observations in it")
	}
	if got, want := m.delta, 0.05; math.Abs(got-want) > 1e-12 {
		t.Fatalf("delta = %v, want %v: the observation at the window's end must belong to the "+
			"NEXT window, never to this one", got, want)
	}
}

// -----------------------------------------------------------------------------
// The lead/follower rule
// -----------------------------------------------------------------------------

// TestLeadIsTheEarliestMoverAndTiesAreTotal pins the lead selection rule,
// including both tie-breaks.
//
// The tie-breaks matter more than they look: two books sharing a view of one
// latent process reprice on the SAME event-time grid, so a tie on the move
// instant is the common case rather than the exotic one, and an implementation
// that left it to map order would choose a different lead on every run.
func TestLeadIsTheEarliestMoverAndTiesAreTotal(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()

	tests := []struct {
		name  string
		moves []bookMove
		want  domain.BookID
	}{
		{
			name: "earliest move instant wins",
			moves: []bookMove{
				{book: "b", delta: 0.09, movedAt: t0.Add(30 * time.Second)},
				{book: "a", delta: 0.04, movedAt: t0.Add(10 * time.Second)},
			},
			want: "a",
		},
		{
			// "a" moves the other way, so the corroboration has to come from a
			// third book: a lead with no follower is one book's move, whichever
			// tie-break selected it.
			name: "on an equal instant the larger absolute move wins",
			moves: []bookMove{
				{book: "a", delta: 0.04, movedAt: t0},
				{book: "b", delta: -0.09, movedAt: t0},
				{book: "c", delta: -0.05, movedAt: t0.Add(10 * time.Second)},
			},
			want: "b",
		},
		{
			name: "on an equal instant and an equal move the smaller book id wins",
			moves: []bookMove{
				{book: "z", delta: 0.05, movedAt: t0},
				{book: "a", delta: 0.05, movedAt: t0},
			},
			want: "a",
		},
	}

	d := mustDetector(t, Config{MinMagnitude: 0.03, MinFollowers: 1, MinCorrelation: -1})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, ok, _ := d.assess("m", "s", t0.Add(-time.Minute), t0.Add(time.Minute), tc.moves)
			if !ok {
				t.Fatalf("no finding; the fixture is meant to clear every gate")
			}
			if f.LeadBook != tc.want {
				t.Fatalf("lead = %s, want %s", f.LeadBook, tc.want)
			}
		})
	}
}

// TestFollowersRequireTheSameDirectionAndAreOrderedByLag pins what a follower is
// and the order the JSONB column is written in.
//
// The ordering is a writer's obligation: migrations/00009 can check that the
// array's length matches follower_count, but a database cannot enforce the
// ordering of an array, so nothing except this test stands between the contract
// and a rewrite that emits the same set in a different sequence.
func TestFollowersRequireTheSameDirectionAndAreOrderedByLag(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	d := mustDetector(t, Config{MinMagnitude: 0.03, MinFollowers: 1, MinCorrelation: -1})

	moves := []bookMove{
		{book: "sharp", delta: 0.06, movedAt: t0},
		{book: "slow", delta: 0.05, movedAt: t0.Add(80 * time.Second)},
		{book: "quick", delta: 0.04, movedAt: t0.Add(20 * time.Second)},
		{book: "contrarian", delta: -0.07, movedAt: t0.Add(10 * time.Second)}, // opposite sign
		{book: "tardy", delta: 0.05, movedAt: t0.Add(9 * time.Minute)},        // past MaxFollowerLag
	}
	f, ok, _ := d.assess("m", "s", t0.Add(-time.Minute), t0.Add(10*time.Minute), moves)
	if !ok {
		t.Fatal("no finding")
	}

	var got []domain.BookID
	for _, fol := range f.Followers {
		got = append(got, fol.Book)
	}
	want := []domain.BookID{"quick", "slow"}
	if len(got) != len(want) {
		t.Fatalf("followers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("followers = %v, want %v (ordered by lag ascending)", got, want)
		}
	}
	if f.ParticipatingBooks != len(want)+1 {
		t.Fatalf("participating books = %d, want %d", f.ParticipatingBooks, len(want)+1)
	}
	// One of five books moved against the lead; the other four agree. The
	// statistic is the mean signed agreement over every book with data.
	if wantCorr := 3.0 / 5.0; math.Abs(f.Correlation-wantCorr) > 1e-12 {
		t.Fatalf("correlation = %v, want %v", f.Correlation, wantCorr)
	}
}

// TestAloneBookDoesNotFire is the smallest statement of what the corroboration
// requirement is for: a single book's move, however large, is that book's
// problem and not the market's.
func TestAloneBookDoesNotFire(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	d := mustDetector(t, Config{})

	moves := []bookMove{
		{book: "loud", delta: 0.25, movedAt: t0},
		{book: "quiet", delta: 0.0001, movedAt: t0.Add(time.Second)},
	}
	if _, ok, reason := d.assess("m", "s", t0.Add(-time.Minute), t0.Add(time.Minute), moves); ok {
		t.Fatalf("a lone mover produced a finding (reason %v)", reason)
	}
}

// -----------------------------------------------------------------------------
// Determinism
// -----------------------------------------------------------------------------

// TestAssessIsInvariantUnderPermutationOfBooks asserts that the detector's answer
// does not depend on the order the books arrived in.
//
// Go's map iteration is randomised, and a detector whose lead selection or
// correlation depended on it would produce a different answer on every run — and
// would be impossible to compare against a SQL job whose collect order is equally
// arbitrary.
func TestAssessIsInvariantUnderPermutationOfBooks(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	d := mustDetector(t, Config{MinCorrelation: -1})

	base := []bookMove{
		{book: "a", delta: 0.06, movedAt: t0},
		{book: "b", delta: 0.05, movedAt: t0.Add(20 * time.Second)},
		{book: "c", delta: 0.04, movedAt: t0.Add(40 * time.Second)},
		{book: "d", delta: -0.002, movedAt: t0.Add(10 * time.Second)},
	}
	want, ok, _ := d.assess("m", "s", t0.Add(-time.Minute), t0.Add(2*time.Minute), base)
	if !ok {
		t.Fatal("no finding on the base ordering")
	}

	for _, perm := range [][]int{{3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}} {
		moves := make([]bookMove, len(base))
		for i, j := range perm {
			moves[i] = base[j]
		}
		got, ok, _ := d.assess("m", "s", t0.Add(-time.Minute), t0.Add(2*time.Minute), moves)
		if !ok {
			t.Fatalf("permutation %v produced no finding", perm)
		}
		if got.LeadBook != want.LeadBook || got.Delta != want.Delta ||
			got.Correlation != want.Correlation || len(got.Followers) != len(want.Followers) {
			t.Fatalf("permutation %v changed the answer:\n got %+v\nwant %+v", perm, got, want)
		}
		for i := range got.Followers {
			if got.Followers[i] != want.Followers[i] {
				t.Fatalf("permutation %v reordered the followers", perm)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// The cooldown
// -----------------------------------------------------------------------------

// TestCooldownSuppressesTheSameMoveSeenThroughOverlappingWindows is the reason
// the cooldown exists: with Window/Hop = 3 one jump lands in three consecutive
// windows, so a detector without suppression alerts three times on one event.
func TestCooldownSuppressesTheSameMoveSeenThroughOverlappingWindows(t *testing.T) {
	// The span below has to outlast AllowedLateness, which is why the loop runs
	// far past the jump: the watermark trails the newest observation by three
	// minutes by default, so a run that stopped at the jump would never close the
	// window containing it. That is the detector working as designed and it is the
	// single easiest thing to get wrong when writing a test against it.
	cfg := Config{
		Window:         3 * time.Minute,
		Hop:            time.Minute,
		MinCorrelation: -1,
	}
	d := mustDetector(t, cfg)

	base := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Minute)
	books := []domain.BookID{"alpha", "beta", "gamma"}

	// A flat run, one jump, then a flat run. Every book jumps together, which is
	// the shape a steam move takes once every book has caught up.
	var (
		findings []Finding
		total    Stats
	)
	for i := range 60 {
		at := base.Add(time.Duration(i) * 15 * time.Second)
		p := 0.40
		if i >= 8 {
			p = 0.50
		}
		u := Update{Market: "m", Anchor: at}
		for _, b := range books {
			u.Quotes = append(u.Quotes, Quote{
				Selection: "s", Book: b, Implied: odds.Probability(p), ObservedAt: at,
			})
		}
		f, st := d.Observe(u)
		findings = append(findings, f...)
		total.add(st)
	}

	if len(findings) != 1 {
		t.Fatalf("got %d findings for ONE jump, want exactly 1 (cooldown suppressed %d)",
			len(findings), total.SuppressedByCooldown)
	}
	if total.SuppressedByCooldown == 0 {
		t.Fatal("the cooldown never fired, so the single finding is a coincidence of the " +
			"window grid rather than evidence that suppression works")
	}
	if got := findings[0].Direction; got != DirectionShorten {
		t.Fatalf("direction = %q, want %q for a rising implied probability", got, DirectionShorten)
	}
}

// TestLateObservationsAreDroppedAndCounted asserts that a record arriving behind
// the watermark cannot reopen a decided window.
//
// Admitting it would make a window's answer depend on when the reader asked,
// which is the one property the watermark exists to remove and the one a replay
// would immediately expose.
func TestLateObservationsAreDroppedAndCounted(t *testing.T) {
	d := mustDetector(t, Config{AllowedLateness: time.Minute})
	base := time.Unix(1_700_000_000, 0).UTC()

	_, _ = d.Observe(Update{
		Market: "m", Anchor: base.Add(time.Hour),
		Quotes: []Quote{{Selection: "s", Book: "a", Implied: 0.5, ObservedAt: base.Add(time.Hour)}},
	})
	_, st := d.Observe(Update{
		Market: "m", Anchor: base.Add(time.Hour),
		Quotes: []Quote{{Selection: "s", Book: "a", Implied: 0.5, ObservedAt: base}},
	})
	if st.Late != 1 {
		t.Fatalf("late = %d, want 1", st.Late)
	}
}

// TestForgetReleasesMarketState asserts the tombstone path actually frees the
// windowed state, which is the difference between a bounded process and a leak on
// a slate that rolls over daily.
func TestForgetReleasesMarketState(t *testing.T) {
	d := mustDetector(t, Config{})
	at := time.Unix(1_700_000_000, 0).UTC()
	_, _ = d.Observe(Update{
		Market: "m", Anchor: at,
		Quotes: []Quote{{Selection: "s", Book: "a", Implied: 0.5, ObservedAt: at}},
	})
	if d.Markets() != 1 {
		t.Fatalf("markets = %d, want 1", d.Markets())
	}
	d.Forget("m")
	if d.Markets() != 0 {
		t.Fatalf("markets = %d after Forget, want 0", d.Markets())
	}
}

// -----------------------------------------------------------------------------
// Against the real generator — the two gate items
// -----------------------------------------------------------------------------

// TestFiresOnGeneratedSteamAndNotOnDrift is the phase-9 gate, and it is
// deliberately a SINGLE test asserting both directions, because either half alone
// is trivially satisfiable: a detector that fires on everything passes the first
// and a detector that fires on nothing passes the second.
//
// # What the input is
//
// The REAL synthetic generator, polled at ADR 0003's live cadence over six hours
// of model time, across a whole league's moneyline markets. Nothing is stubbed:
// the per-book margins, the per-book persistent bias, the per-book view lag and
// the American tick flooring are all in play, and the steam moves are the ones
// noise.go generates from the seed rather than ones this test planted.
//
// # What "fires" and "does not fire" mean here, quantitatively
//
// noise.go puts a steam move in a 10-minute block with probability
// steamProbability = 0.02, per latent process. A league slate of forty events over
// six hours is therefore expected to produce a handful of steam moves in total,
// against several thousand (market, window) pairs evaluated.
//
// So the two assertions are:
//
//	FIRES     at least one finding across the slate. Zero means the detector is
//	          blind to the one thing it exists to see.
//	NOT DRIFT the finding rate stays far below the window rate. Ordinary drift is
//	          present in EVERY window of EVERY market — that is what the
//	          mean-reverting mixture does — so a detector that mistook drift for
//	          steam would fire on a large fraction of windows rather than on a
//	          handful.
//
// The bound is deliberately loose. It is not a claim that the rate is exactly
// right; it is a claim that the detector separates the two populations by orders
// of magnitude, which is the property that would break if a threshold were
// mistyped or if the magnitude were measured in decimal odds instead of
// probability points.
func TestFiresOnGeneratedSteamAndNotOnDrift(t *testing.T) {
	const (
		pollInterval = 90 * time.Second // ADR 0003's live cadence
		span         = 6 * time.Hour
		polls        = int(span / pollInterval)

		// The bound on the finding rate. Ordinary drift is present in EVERY window
		// of EVERY market, so a detector that mistook it for steam would fire on a
		// large fraction of windows. The measured rate at the shipped thresholds is
		// between 0.02% and 0.23% depending on the league, and a threshold set one
		// step too low (0.030 rather than 0.050) produces 1.15% — so this bound sits
		// with headroom above the former and comfortably below the latter, and
		// catches exactly the mistake it is here to catch.
		maxRate = 0.005
	)

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	adapter, err := synthetic.New(synthetic.Options{Seed: 20260817, Clock: clock})
	if err != nil {
		t.Fatalf("synthetic.New: %v", err)
	}
	ctx := context.Background()
	cat, err := adapter.Catalogue(ctx)
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	if len(cat.Leagues) == 0 {
		t.Fatal("the synthetic catalogue offers no leagues")
	}

	// EVERY league, not one. The four have conversion factors from latent sigma to
	// probability points that differ by about a factor of two (see
	// DefaultMinMagnitude), so a threshold that separated the populations on
	// basketball and fired constantly on football would pass a single-league test.
	for _, l := range cat.Leagues {
		league := l.ID()
		t.Run(string(league), func(t *testing.T) {
			now = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

			d, err := New(DefaultConfig())
			if err != nil {
				t.Fatalf("steam.New: %v", err)
			}

			var (
				findings []Finding
				total    Stats
			)
			for range polls {
				snap, err := adapter.Fetch(ctx, provider.Scope{
					League:  league,
					Markets: []domain.MarketType{domain.MarketTypeMoneyline},
				})
				if err != nil {
					t.Fatalf("Fetch: %v", err)
				}
				for _, ev := range snap.Events {
					for _, m := range ev.Markets {
						u := Update{Market: m.Market.ID(), Anchor: snap.FetchedAt}
						for _, p := range m.Prices {
							implied, err := odds.Decimal(p.Decimal()).Probability()
							if err != nil {
								continue
							}
							u.Quotes = append(u.Quotes, Quote{
								Selection:  p.SelectionID(),
								Book:       p.BookID(),
								Implied:    implied,
								ObservedAt: p.ObservedAt(),
							})
						}
						if len(u.Quotes) == 0 {
							continue
						}
						f, st := d.Observe(u)
						findings = append(findings, f...)
						total.add(st)
					}
				}
				now = now.Add(pollInterval)
			}

			t.Logf("polls=%d quotes=%d windows=%d candidates=%d findings=%d "+
				"suppressed(threshold)=%d suppressed(cooldown)=%d late=%d",
				polls, total.Quotes, total.Windows, total.Candidates, len(findings),
				total.SuppressedByThreshold, total.SuppressedByCooldown, total.Late)

			if total.Windows == 0 {
				t.Fatal("no window ever closed: the harness never advanced event time far enough " +
					"for the watermark to pass a window end, so neither half of this test means " +
					"anything")
			}

			// FIRES. A generated steam move must reach the detector.
			if len(findings) == 0 {
				t.Fatalf("no steam finding across %d windows over %s of model time; noise.go "+
					"generates a steam move per 10-minute block with probability 0.02 per latent "+
					"process, so a whole league's slate over this span must contain several. The "+
					"detector is blind to the one thing it exists to see", total.Windows, span)
			}

			// DOES NOT FIRE ON DRIFT.
			if rate := float64(len(findings)) / float64(total.Windows); rate > maxRate {
				t.Fatalf("steam fired on %.4f%% of windows (%d of %d), above the %.1f%% bound. "+
					"Ordinary drift is present in every window, so a rate this high means the "+
					"detector is reporting drift as steam — check that the magnitude threshold is "+
					"in probability points and not in decimal odds",
					rate*100, len(findings), total.Windows, maxRate*100)
			}

			// Every finding must be internally consistent with the contract, because
			// a finding that fires and then fails migrations/00009's CHECKs is a
			// finding that never reaches a sink.
			for _, f := range findings {
				switch {
				case f.Magnitude < f.ThresholdMagnitude:
					t.Fatalf("finding below its own magnitude threshold: %s", f)
				case math.Abs(f.Velocity) < f.ThresholdVelocity:
					t.Fatalf("finding below its own velocity threshold: %s", f)
				case f.Correlation < f.ThresholdCorrelation:
					t.Fatalf("finding below its own correlation threshold: %s", f)
				case len(f.Followers) < f.MinFollowers:
					t.Fatalf("finding below its own follower minimum: %s", f)
				case f.LeadMovedAt.Before(f.WindowStart) || !f.LeadMovedAt.Before(f.WindowEnd):
					t.Fatalf("lead move instant outside the half-open window: %s", f)
				case (f.Direction == DirectionShorten) != (f.Delta > 0):
					t.Fatalf("direction disagrees with the sign of the delta: %s", f)
				case f.Magnitude != math.Abs(f.Delta):
					t.Fatalf("magnitude is not |delta|: %s", f)
				}
			}
		})
	}
}

// mustDetector builds a detector or fails the test.
func mustDetector(t *testing.T, cfg Config) *Detector {
	t.Helper()
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("steam.New(%+v): %v", cfg, err)
	}
	return d
}
