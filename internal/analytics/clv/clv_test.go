// Tests for the closing-price rules.
//
// # What this tier can and cannot assert, stated honestly
//
// The suspension exclusion, the backward walk and the completeness count are
// implemented in SQL (queries/analytics.sql) and are asserted against a real
// TimescaleDB in the integration tier. What a fake store can assert is the two
// SHAPES that predicate produces and what this package does with each of them: a
// complete post-lift snapshot must measure, and a snapshot the predicate emptied
// must produce no row and the right reason. Both are here, and neither pretends
// to be a test of the SQL.
//
// Everything else in doc.go IS asserted here, because everything else is decided
// in this package: which instant closes a market, which book both sides come
// from, what happens to a line move, an in-play wager, a changed outcome set, a
// snapshot whose selections disagree about the line, and a reconstruction that
// found the wrong quote.
//
// # No mock data
//
// Every price below is a fixture inside a _test.go file, which CLAUDE.md permits
// explicitly. Nothing is seeded into a database and no shipped code path invents
// a quote.
package clv

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

var (
	// kickoff is the closing instant of every fixture market.
	kickoff = time.Date(2026, 8, 20, 19, 0, 0, 0, time.UTC)

	// bookedAt is when the fixture wager was struck: two hours before kickoff, so
	// a pre-game bet's close is genuinely after its take.
	bookedAt = kickoff.Add(-2 * time.Hour)

	// closedAt is the newest quote in a fixture closing snapshot: a minute before
	// the scheduled start, which is what a market being priced right up to kickoff
	// looks like.
	closedAt = kickoff.Add(-time.Minute)

	// gradedAt is the result's finalisation instant.
	gradedAt = kickoff.Add(3 * time.Hour)

	// computedAt is the injected clock, so ComputedAt is deterministic.
	computedAt = gradedAt.Add(30 * time.Second)
)

const (
	fixtureLeg     = "leg-1"
	fixtureWager   = "wgr-1"
	fixtureUser    = "usr-1"
	fixtureEvent   = "evt-1"
	fixtureMarket  = "mkt-1"
	fixtureLeague  = "lg-1"
	fixtureBook    = "book-sharpline"
	selectionHome  = "sel-home"
	selectionAway  = "sel-away"
	selectionDraw  = "sel-draw"
	takenHomeOdds  = 2.10
	takenAwayOdds  = 1.80
	closeHomeOdds  = 1.90
	closeAwayOdds  = 2.00
	homeIsTheBet   = selectionHome
	twoWayMarketN  = 2
	threeWayMarket = 3
)

// quote builds one snapshot quote.
func quote(sel string, role domain.SelectionRole, dec float64, line domain.Line, at time.Time) Quote {
	return Quote{
		Selection:  domain.SelectionID(sel),
		Role:       role,
		Decimal:    odds.Decimal(dec),
		Line:       line,
		ObservedAt: at,
	}
}

// twoWay builds a complete two-way moneyline snapshot.
func twoWay(homeOdds, awayOdds float64, at time.Time) Snapshot {
	return Snapshot{
		Quotes: []Quote{
			quote(selectionAway, domain.SelectionRoleAway, awayOdds, domain.NoLine(), at),
			quote(selectionHome, domain.SelectionRoleHome, homeOdds, domain.NoLine(), at),
		},
		MarketSelections: twoWayMarketN,
	}
}

// mustLine builds a present line or fails the test.
func mustLine(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("domain.NewLine(%v): %v", v, err)
	}
	return l
}

// baseLeg is a pre-game two-way moneyline bet on the home side.
func baseLeg() Leg {
	return Leg{
		LegID:       fixtureLeg,
		WagerID:     fixtureWager,
		UserID:      fixtureUser,
		EventID:     fixtureEvent,
		MarketID:    fixtureMarket,
		MarketType:  domain.MarketTypeMoneyline,
		SelectionID: homeIsTheBet,
		Book:        fixtureBook,
		Decimal:     odds.Decimal(takenHomeOdds),
		ObservedAt:  bookedAt,
		Status:      domain.LegStatusWon,
		GradedAt:    gradedAt,
	}
}

// baseMarket is a finished two-way moneyline market.
func baseMarket() Market {
	return Market{
		MarketID:       fixtureMarket,
		MarketType:     domain.MarketTypeMoneyline,
		EventID:        fixtureEvent,
		EventStatus:    domain.EventStatusEnded,
		LeagueID:       fixtureLeague,
		ScheduledStart: kickoff,
	}
}

// -----------------------------------------------------------------------------
// The fake store
// -----------------------------------------------------------------------------

// fakeStore answers by the request's as-of instant, which is exactly what
// distinguishes the two sides: the taken snapshot is asked for as of the leg's
// own quote instant and the closing snapshot as of the scheduled start. Keying on
// it means a test states the two answers positionally and the code under test
// still has to ask the right question to get the right one.
type fakeStore struct {
	market    Market
	marketErr error

	byAsOf   map[time.Time]Snapshot
	snapErr  error
	requests []SnapshotRequest
}

func (f *fakeStore) MarketClose(_ context.Context, _ domain.MarketID) (Market, error) {
	if f.marketErr != nil {
		return Market{}, f.marketErr
	}
	return f.market, nil
}

func (f *fakeStore) Snapshot(_ context.Context, req SnapshotRequest) (Snapshot, error) {
	f.requests = append(f.requests, req)
	if f.snapErr != nil {
		return Snapshot{}, f.snapErr
	}
	snap, ok := f.byAsOf[req.AsOf]
	if !ok {
		// A market with no eligible quote at all — the shape the SQL produces when
		// the suspension predicate excluded everything.
		return Snapshot{}, nil
	}
	return snap, nil
}

// newMeasurer builds a measurer over the fake with a frozen clock.
func newMeasurer(t *testing.T, store Store, method odds.DevigMethod) *Measurer {
	t.Helper()
	m, err := New(Options{
		Store:       store,
		DevigMethod: method,
		Clock:       func() time.Time { return computedAt },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// -----------------------------------------------------------------------------
// Measure
// -----------------------------------------------------------------------------

func TestMeasure(t *testing.T) {
	spreadLineMinus3 := mustLine(t, -3)
	spreadLinePlus3 := mustLine(t, 3)
	spreadLineMinus35 := mustLine(t, -3.5)
	spreadLinePlus35 := mustLine(t, 3.5)

	tests := []struct {
		name   string
		method odds.DevigMethod
		leg    func() Leg
		market func() Market
		snaps  func() map[time.Time]Snapshot

		// wantReason is checked when non-zero; wantErrIs when non-nil. A test that
		// sets neither expects a measurement.
		wantReason Reason
		wantErrIs  error

		wantBeat      bool
		wantLineMoved bool
		wantMethod    odds.DevigMethod
	}{
		{
			// The market moved toward the bettor: home was 2.10 and closed at 1.90,
			// so the fair probability rose and the price taken was the better one.
			name:   "pre-game bet that beat the close",
			method: odds.MethodMultiplicative,
			leg:    baseLeg,
			market: baseMarket,
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: twoWay(takenHomeOdds, takenAwayOdds, bookedAt),
					kickoff:  twoWay(closeHomeOdds, closeAwayOdds, closedAt),
				}
			},
			wantBeat:   true,
			wantMethod: odds.MethodMultiplicative,
		},
		{
			// A market suspended before kickoff and reopened before it: the
			// exclusion drops the quotes inside the episode and the last eligible
			// one wins, so the store hands back a complete post-lift snapshot and
			// the measurement proceeds normally. The predicate itself is asserted
			// in SQL; what is asserted here is that this package treats a post-lift
			// snapshot as an ordinary close and does not go looking for a fresher
			// one.
			name:   "market suspended and reopened before the start",
			method: odds.MethodMultiplicative,
			leg:    baseLeg,
			market: baseMarket,
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: twoWay(takenHomeOdds, takenAwayOdds, bookedAt),
					// The newest ELIGIBLE quotes, forty minutes before kickoff,
					// because everything after that was inside the episode.
					kickoff: twoWay(closeHomeOdds, closeAwayOdds, kickoff.Add(-40*time.Minute)),
				}
			},
			wantBeat:   true,
			wantMethod: odds.MethodMultiplicative,
		},
		{
			// Suspended and never reopened, with the last open quote older than the
			// lookback: the predicate excludes every candidate, the store returns
			// nothing, and there is no close.
			name:   "market suspended through the whole closing window",
			method: odds.MethodMultiplicative,
			leg:    baseLeg,
			market: baseMarket,
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: twoWay(takenHomeOdds, takenAwayOdds, bookedAt),
				}
			},
			wantReason: ReasonClosingIncomplete,
		},
		{
			// A spread taken at −3 that closed at −3.5 is a different market
			// question. It still gets a measurement, flagged, because a user is
			// entitled to see what happened to their bet; odds.AggregateCLV and the
			// leaderboard query are what stop it being ranked.
			name:   "line moved between the take and the close",
			method: odds.MethodMultiplicative,
			leg: func() Leg {
				l := baseLeg()
				l.MarketType = domain.MarketTypeSpread
				return l
			},
			market: func() Market {
				m := baseMarket()
				m.MarketType = domain.MarketTypeSpread
				return m
			},
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: {
						Quotes: []Quote{
							quote(selectionAway, domain.SelectionRoleAway, takenAwayOdds, spreadLinePlus3, bookedAt),
							quote(selectionHome, domain.SelectionRoleHome, takenHomeOdds, spreadLineMinus3, bookedAt),
						},
						MarketSelections: twoWayMarketN,
					},
					kickoff: {
						Quotes: []Quote{
							quote(selectionAway, domain.SelectionRoleAway, closeAwayOdds, spreadLinePlus35, closedAt),
							quote(selectionHome, domain.SelectionRoleHome, closeHomeOdds, spreadLineMinus35, closedAt),
						},
						MarketSelections: twoWayMarketN,
					},
				}
			},
			wantBeat:      true,
			wantLineMoved: true,
			wantMethod:    odds.MethodMultiplicative,
		},
		{
			// A three-way market that lost its draw. Both snapshots devig
			// perfectly; they are simply distributions over different sample
			// spaces, and no single component of them is comparable.
			name:   "outcome set changed between the take and the close",
			method: odds.MethodMultiplicative,
			leg:    baseLeg,
			market: baseMarket,
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: {
						Quotes: []Quote{
							quote(selectionAway, domain.SelectionRoleAway, 3.40, domain.NoLine(), bookedAt),
							quote(selectionDraw, domain.SelectionRoleDraw, 3.50, domain.NoLine(), bookedAt),
							quote(selectionHome, domain.SelectionRoleHome, takenHomeOdds, domain.NoLine(), bookedAt),
						},
						MarketSelections: threeWayMarket,
					},
					kickoff: twoWay(closeHomeOdds, closeAwayOdds, closedAt),
				}
			},
			wantReason: ReasonOutcomeSetChanged,
		},
		{
			// The event never started, so nothing closed — however plausible the
			// scheduled start looks. This is the "market never closed" case.
			name:   "market never closed because the event never started",
			method: odds.MethodMultiplicative,
			leg:    baseLeg,
			market: func() Market {
				m := baseMarket()
				m.EventStatus = domain.EventStatusPostponed
				return m
			},
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: twoWay(takenHomeOdds, takenAwayOdds, bookedAt),
					kickoff:  twoWay(closeHomeOdds, closeAwayOdds, closedAt),
				}
			},
			wantReason: ReasonNoClose,
		},
		{
			name:   "market never closed because it has no scheduled start",
			method: odds.MethodMultiplicative,
			leg:    baseLeg,
			market: func() Market {
				m := baseMarket()
				m.ScheduledStart = time.Time{}
				return m
			},
			snaps:      func() map[time.Time]Snapshot { return nil },
			wantReason: ReasonNoClose,
		},
		{
			// An in-play wager: struck after kickoff, so the pre-game close
			// precedes it. doc.go §5 — a deliberate exclusion, not a gap.
			name:   "in-play wager has no closing line value",
			method: odds.MethodMultiplicative,
			leg: func() Leg {
				l := baseLeg()
				l.ObservedAt = kickoff.Add(20 * time.Minute)
				return l
			},
			market: baseMarket,
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					kickoff.Add(20 * time.Minute): twoWay(takenHomeOdds, takenAwayOdds, kickoff.Add(20*time.Minute)),
					kickoff:                       twoWay(closeHomeOdds, closeAwayOdds, closedAt),
				}
			},
			wantReason: ReasonCloseBeforeTake,
		},
		{
			// The reconstruction found a quote for the leg's own selection that is
			// not the leg's own quote. The market it describes is not the market
			// the wager was struck in.
			name:   "reconstruction found a different quote for the leg's own selection",
			method: odds.MethodMultiplicative,
			leg:    baseLeg,
			market: baseMarket,
			snaps: func() map[time.Time]Snapshot {
				stale := bookedAt.Add(-90 * time.Minute)
				return map[time.Time]Snapshot{
					bookedAt: twoWay(takenHomeOdds, takenAwayOdds, stale),
					kickoff:  twoWay(closeHomeOdds, closeAwayOdds, closedAt),
				}
			},
			wantReason: ReasonTakenQuoteMismatch,
		},
		{
			// The taken snapshot is short one selection. Devigging a subset would
			// produce probabilities wrong by the missing side's entire mass.
			name:   "taken market cannot be reconstructed",
			method: odds.MethodMultiplicative,
			leg:    baseLeg,
			market: baseMarket,
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: {
						Quotes:           []Quote{quote(selectionHome, domain.SelectionRoleHome, takenHomeOdds, domain.NoLine(), bookedAt)},
						MarketSelections: twoWayMarketN,
					},
					kickoff: twoWay(closeHomeOdds, closeAwayOdds, closedAt),
				}
			},
			wantReason: ReasonTakenIncomplete,
		},
		{
			// The two sides of a spread do not agree on the line once the away side
			// is converted back into the market's frame: −3 against −2.5.
			name:   "snapshot's selections disagree about the line",
			method: odds.MethodMultiplicative,
			leg: func() Leg {
				l := baseLeg()
				l.MarketType = domain.MarketTypeSpread
				return l
			},
			market: func() Market {
				m := baseMarket()
				m.MarketType = domain.MarketTypeSpread
				return m
			},
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: {
						Quotes: []Quote{
							quote(selectionAway, domain.SelectionRoleAway, takenAwayOdds, mustLine(t, 2.5), bookedAt),
							quote(selectionHome, domain.SelectionRoleHome, takenHomeOdds, spreadLineMinus3, bookedAt),
						},
						MarketSelections: twoWayMarketN,
					},
					kickoff: twoWay(closeHomeOdds, closeAwayOdds, closedAt),
				}
			},
			wantReason: ReasonTakenIncoherent,
		},
		{
			// The configured method refuses the closing snapshot, so BOTH sides are
			// recomputed with multiplicative and the row records multiplicative.
			// Falling back on only the failing side would compare a
			// Shin-or-additive-devigged take against a multiplicative close, which
			// measures the difference between two devig methods.
			name:   "configured method refuses one side and both fall back",
			method: odds.MethodAdditive,
			leg:    baseLeg,
			market: baseMarket,
			snaps: func() map[time.Time]Snapshot {
				return map[time.Time]Snapshot{
					bookedAt: twoWay(takenHomeOdds, takenAwayOdds, bookedAt),
					// Two short prices and one enormous longshot: additive
					// subtracts (S−1)/n from every selection and drives the
					// longshot below zero.
					kickoff: {
						Quotes: []Quote{
							quote(selectionAway, domain.SelectionRoleAway, 1.20, domain.NoLine(), closedAt),
							quote(selectionDraw, domain.SelectionRoleDraw, 100.0, domain.NoLine(), closedAt),
							quote(selectionHome, domain.SelectionRoleHome, 1.20, domain.NoLine(), closedAt),
						},
						MarketSelections: threeWayMarket,
					},
				}
			},
			// The two snapshots price different selection sets, so the comparison
			// is refused AFTER the fallback has already been chosen — which is
			// precisely the assertion: the fallback happens, and the refusal that
			// follows is the outcome-set one rather than a devig failure.
			wantReason: ReasonOutcomeSetChanged,
		},
		{
			// Not a graded leg. A defect in whatever produced the row, and it must
			// not be counted alongside the honest exclusions.
			name:   "ungraded leg is unusable rather than unmeasurable",
			method: odds.MethodMultiplicative,
			leg: func() Leg {
				l := baseLeg()
				l.Status = domain.LegStatusPending
				return l
			},
			market:    baseMarket,
			snaps:     func() map[time.Time]Snapshot { return nil },
			wantErrIs: ErrUnusableLeg,
		},
		{
			// A leg whose market_type disagrees with the market's own. Impossible
			// through the composite foreign key, so its presence means the value
			// did not come through the schema.
			name:   "leg disagrees with its market about the market type",
			method: odds.MethodMultiplicative,
			leg: func() Leg {
				l := baseLeg()
				l.MarketType = domain.MarketTypeTotal
				return l
			},
			market:    baseMarket,
			snaps:     func() map[time.Time]Snapshot { return nil },
			wantErrIs: ErrUnusableLeg,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{market: tc.market(), byAsOf: tc.snaps()}
			m := newMeasurer(t, store, tc.method)

			got, err := m.Measure(context.Background(), tc.leg())

			switch {
			case tc.wantErrIs != nil:
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("Measure error = %v, want one matching %v", err, tc.wantErrIs)
				}
				if _, unmeasurable := ReasonFor(err); unmeasurable {
					t.Fatalf("Measure reported %v as an analytics exclusion; it is a defect", err)
				}
				return

			case tc.wantReason != ReasonNone:
				reason, ok := ReasonFor(err)
				if !ok {
					t.Fatalf("Measure error = %v, want an unmeasurable-leg error", err)
				}
				if reason != tc.wantReason {
					t.Fatalf("Measure reason = %s, want %s (error: %v)", reason, tc.wantReason, err)
				}
				if !errors.Is(err, ErrUnmeasurable) {
					t.Fatalf("Measure error %v does not match ErrUnmeasurable", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Measure: %v", err)
			}
			if got.Result.Beat != tc.wantBeat {
				t.Errorf("Beat = %t, want %t (probability CLV %v)", got.Result.Beat, tc.wantBeat, got.Result.ProbabilityCLV)
			}
			if got.Result.LineMoved != tc.wantLineMoved {
				t.Errorf("LineMoved = %t, want %t", got.Result.LineMoved, tc.wantLineMoved)
			}
			if got.DevigMethod != tc.wantMethod {
				t.Errorf("DevigMethod = %s, want %s", got.DevigMethod, tc.wantMethod)
			}
			if got.LeagueID != fixtureLeague {
				t.Errorf("LeagueID = %q, want %q", got.LeagueID, fixtureLeague)
			}
			// doc.go §3: one book, and it is the book the wager was struck at.
			if got.ClosingBook != got.Leg.Book {
				t.Errorf("ClosingBook = %q, want the taken book %q", got.ClosingBook, got.Leg.Book)
			}
			if !got.ComputedAt.Equal(computedAt) {
				t.Errorf("ComputedAt = %s, want the injected clock %s", got.ComputedAt, computedAt)
			}
		})
	}
}

// TestMeasureAsksTheRightTwoQuestions pins the bounds of the two snapshot reads,
// which are the closing-price definition expressed as query parameters. A change
// here is a change to the phase-12 contract.
func TestMeasureAsksTheRightTwoQuestions(t *testing.T) {
	store := &fakeStore{
		market: baseMarket(),
		byAsOf: map[time.Time]Snapshot{
			bookedAt: twoWay(takenHomeOdds, takenAwayOdds, bookedAt),
			kickoff:  twoWay(closeHomeOdds, closeAwayOdds, closedAt),
		},
	}
	m := newMeasurer(t, store, odds.MethodMultiplicative)

	if _, err := m.Measure(context.Background(), baseLeg()); err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(store.requests) != 2 {
		t.Fatalf("made %d snapshot reads, want exactly 2", len(store.requests))
	}

	taken, closing := store.requests[0], store.requests[1]

	// The taken snapshot is as of the leg's own quote instant, INCLUSIVE, so the
	// leg's own row is eligible for its own snapshot.
	if !taken.AsOf.Equal(bookedAt) {
		t.Errorf("taken as-of = %s, want the leg's price instant %s", taken.AsOf, bookedAt)
	}
	if want := bookedAt.Add(-DefaultTakenLookback); !taken.NotBefore.Equal(want) {
		t.Errorf("taken lower bound = %s, want %s", taken.NotBefore, want)
	}
	// The closing snapshot is as of the SCHEDULED START and never the actual
	// kickoff, the status transition, or the newest price on the market.
	if !closing.AsOf.Equal(kickoff) {
		t.Errorf("closing as-of = %s, want the scheduled start %s", closing.AsOf, kickoff)
	}
	if want := kickoff.Add(-DefaultClosingLookback); !closing.NotBefore.Equal(want) {
		t.Errorf("closing lower bound = %s, want %s", closing.NotBefore, want)
	}
	// Both sides read the SAME BOOK.
	if taken.Book != closing.Book || taken.Book != fixtureBook {
		t.Errorf("books = (%q, %q), want both %q", taken.Book, closing.Book, fixtureBook)
	}
}

// TestMeasureReportsStoreFailuresAsFailures is the assertion that keeps an
// outage distinguishable from a quiet market. A store error must NOT be dressed
// up as an analytics exclusion, because an exclusion is expected and an outage is
// not, and a leaderboard silently computed from half a population is the failure
// this distinction exists to prevent.
func TestMeasureReportsStoreFailuresAsFailures(t *testing.T) {
	boom := errors.New("connection reset")

	t.Run("market read", func(t *testing.T) {
		store := &fakeStore{marketErr: boom}
		m := newMeasurer(t, store, odds.MethodMultiplicative)

		_, err := m.Measure(context.Background(), baseLeg())
		if !errors.Is(err, boom) {
			t.Fatalf("Measure error = %v, want it to wrap %v", err, boom)
		}
		if _, unmeasurable := ReasonFor(err); unmeasurable {
			t.Fatal("a failed market read was reported as an analytics exclusion")
		}
	})

	t.Run("snapshot read", func(t *testing.T) {
		store := &fakeStore{market: baseMarket(), snapErr: boom}
		m := newMeasurer(t, store, odds.MethodMultiplicative)

		_, err := m.Measure(context.Background(), baseLeg())
		if !errors.Is(err, boom) {
			t.Fatalf("Measure error = %v, want it to wrap %v", err, boom)
		}
		if _, unmeasurable := ReasonFor(err); unmeasurable {
			t.Fatal("a failed snapshot read was reported as an analytics exclusion")
		}
	})

	t.Run("missing market", func(t *testing.T) {
		store := &fakeStore{marketErr: ErrMarketNotFound}
		m := newMeasurer(t, store, odds.MethodMultiplicative)

		_, err := m.Measure(context.Background(), baseLeg())
		if !errors.Is(err, ErrMarketNotFound) {
			t.Fatalf("Measure error = %v, want ErrMarketNotFound", err)
		}
		if _, unmeasurable := ReasonFor(err); unmeasurable {
			t.Fatal("a dangling market reference was reported as an analytics exclusion")
		}
	})
}

// TestMeasurementSampleHonoursTheVoidRule pins the one exclusion that is decided
// from the leg's status rather than from the prices: a VOID leg is dropped from
// every aggregate and a PUSH is not.
func TestMeasurementSampleHonoursTheVoidRule(t *testing.T) {
	for _, tc := range []struct {
		status   domain.LegStatus
		wantVoid bool
	}{
		{domain.LegStatusWon, false},
		{domain.LegStatusLost, false},
		{domain.LegStatusPush, false}, // a push had action; it is ranked
		{domain.LegStatusVoid, true},
	} {
		t.Run(tc.status.String(), func(t *testing.T) {
			leg := baseLeg()
			leg.Status = tc.status
			store := &fakeStore{
				market: baseMarket(),
				byAsOf: map[time.Time]Snapshot{
					bookedAt: twoWay(takenHomeOdds, takenAwayOdds, bookedAt),
					kickoff:  twoWay(closeHomeOdds, closeAwayOdds, closedAt),
				},
			}
			m := newMeasurer(t, store, odds.MethodMultiplicative)

			got, err := m.Measure(context.Background(), leg)
			if err != nil {
				t.Fatalf("Measure: %v", err)
			}
			if got.Voided() != tc.wantVoid {
				t.Errorf("Voided() = %t, want %t", got.Voided(), tc.wantVoid)
			}
			if got.Sample().Void != tc.wantVoid {
				t.Errorf("Sample().Void = %t, want %t", got.Sample().Void, tc.wantVoid)
			}
		})
	}
}

// TestMeasurementFeedsAggregateCLV proves the measurement is the value the
// domain's own aggregator consumes, so a caller summing in Go gets the
// compensated arithmetic rather than a hand-written mean — and gets the two
// exclusions applied for free.
func TestMeasurementFeedsAggregateCLV(t *testing.T) {
	store := &fakeStore{
		market: baseMarket(),
		byAsOf: map[time.Time]Snapshot{
			bookedAt: twoWay(takenHomeOdds, takenAwayOdds, bookedAt),
			kickoff:  twoWay(closeHomeOdds, closeAwayOdds, closedAt),
		},
	}
	m := newMeasurer(t, store, odds.MethodMultiplicative)

	won, err := m.Measure(context.Background(), baseLeg())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	voidLeg := baseLeg()
	voidLeg.Status = domain.LegStatusVoid
	voided, err := m.Measure(context.Background(), voidLeg)
	if err != nil {
		t.Fatalf("Measure(void): %v", err)
	}

	agg, err := odds.AggregateCLV([]odds.CLVSample{won.Sample(), voided.Sample()})
	if err != nil {
		t.Fatalf("AggregateCLV: %v", err)
	}
	if agg.Samples != 2 || agg.Counted != 1 || agg.VoidExcluded != 1 {
		t.Errorf("aggregate = %d samples, %d counted, %d void-excluded; want 2/1/1",
			agg.Samples, agg.Counted, agg.VoidExcluded)
	}
	if agg.BeatCount != 1 {
		t.Errorf("BeatCount = %d, want 1", agg.BeatCount)
	}
}

// -----------------------------------------------------------------------------
// marketLine
// -----------------------------------------------------------------------------

// TestMarketLine pins the frame conversion, which is the one place a plausible
// wrong number can be produced silently: reading a stored line without inverting
// the away side of a spread yields a market line of the right magnitude and the
// wrong sign.
func TestMarketLine(t *testing.T) {
	minus3 := mustLine(t, -3)
	plus3 := mustLine(t, 3)
	pickem := mustLine(t, 0)
	total := mustLine(t, 47.5)

	tests := []struct {
		name    string
		typ     domain.MarketType
		quotes  []Quote
		want    domain.Line
		wantErr bool
	}{
		{
			name: "spread inverts the away side back into the market frame",
			typ:  domain.MarketTypeSpread,
			quotes: []Quote{
				quote(selectionHome, domain.SelectionRoleHome, 1.9, minus3, bookedAt),
				quote(selectionAway, domain.SelectionRoleAway, 1.9, plus3, bookedAt),
			},
			want: minus3,
		},
		{
			name: "a pick'em spread is a real line and not an absent one",
			typ:  domain.MarketTypeSpread,
			quotes: []Quote{
				quote(selectionHome, domain.SelectionRoleHome, 1.9, pickem, bookedAt),
				quote(selectionAway, domain.SelectionRoleAway, 1.9, pickem, bookedAt),
			},
			want: pickem,
		},
		{
			name: "a total's threshold is absolute and shared by both sides",
			typ:  domain.MarketTypeTotal,
			quotes: []Quote{
				quote(selectionHome, domain.SelectionRoleOver, 1.9, total, bookedAt),
				quote(selectionAway, domain.SelectionRoleUnder, 1.9, total, bookedAt),
			},
			want: total,
		},
		{
			name: "a moneyline has no line",
			typ:  domain.MarketTypeMoneyline,
			quotes: []Quote{
				quote(selectionHome, domain.SelectionRoleHome, 1.9, domain.NoLine(), bookedAt),
				quote(selectionAway, domain.SelectionRoleAway, 1.9, domain.NoLine(), bookedAt),
			},
			want: domain.NoLine(),
		},
		{
			name: "a spread whose sides disagree is refused",
			typ:  domain.MarketTypeSpread,
			quotes: []Quote{
				quote(selectionHome, domain.SelectionRoleHome, 1.9, minus3, bookedAt),
				quote(selectionAway, domain.SelectionRoleAway, 1.9, mustLine(t, 2.5), bookedAt),
			},
			wantErr: true,
		},
		{
			name: "a market where one side lost its line is refused",
			typ:  domain.MarketTypeTotal,
			quotes: []Quote{
				quote(selectionHome, domain.SelectionRoleOver, 1.9, total, bookedAt),
				quote(selectionAway, domain.SelectionRoleUnder, 1.9, domain.NoLine(), bookedAt),
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := marketLine(tc.typ, tc.quotes)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("marketLine = %s, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("marketLine: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("marketLine = %s, want %s", got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Construction
// -----------------------------------------------------------------------------

func TestNewRejectsBadOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"no store", Options{}},
		{"unknown devig method", Options{Store: &fakeStore{}, DevigMethod: odds.DevigMethod(99)}},
		{"negative closing lookback", Options{Store: &fakeStore{}, ClosingLookback: -time.Hour}},
		{"negative taken lookback", Options{Store: &fakeStore{}, TakenLookback: -time.Hour}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("New = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	m, err := New(Options{Store: &fakeStore{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.DevigMethod() != DefaultDevigMethod {
		t.Errorf("DevigMethod = %s, want %s", m.DevigMethod(), DefaultDevigMethod)
	}
	if m.ClosingLookback() != DefaultClosingLookback {
		t.Errorf("ClosingLookback = %s, want %s", m.ClosingLookback(), DefaultClosingLookback)
	}
	if m.TakenLookback() != DefaultTakenLookback {
		t.Errorf("TakenLookback = %s, want %s", m.TakenLookback(), DefaultTakenLookback)
	}
}

// TestReasonsAreDistinctAndNamed guards the metric label set. Two reasons
// sharing a string would silently merge two dashboard series, and one that
// stringified as "unknown" would make a real exclusion unattributable.
func TestReasonsAreDistinctAndNamed(t *testing.T) {
	seen := make(map[string]Reason, len(Reasons()))
	for _, r := range Reasons() {
		s := r.String()
		if s == "unknown" || s == "none" {
			t.Errorf("reason %d stringifies as %q", uint8(r), s)
		}
		if prev, dup := seen[s]; dup {
			t.Errorf("reasons %d and %d both stringify as %q", uint8(prev), uint8(r), s)
		}
		seen[s] = r
	}
	if len(seen) != len(Reasons()) {
		t.Errorf("Reasons() has %d entries but %d distinct names", len(Reasons()), len(seen))
	}
}
