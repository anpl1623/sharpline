package results

import (
	"context"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/provider/synthetic"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// The two identifier spaces, and the seam that has to cross between them.
//
// # The bug this file exists to prevent recurring
//
// A provider names a contest in ITS OWN space — the generator calls one
// `syn-sba-20260820-2` — and the database names the same contest in the DOMAIN
// space normalizer.EventIDFor derives from it, `synthetic.e.syn-sba-20260820-2`.
// The work queue is read out of the database, so every identifier the poller
// holds is a domain one; the adapter's own identifier function answers only in
// the native one.
//
// For the whole of phase 8 those two spaces met nowhere. The poller passed a
// domain identifier straight through to an adapter that compared it against a
// native one, so the comparison could not match for any event, ever: 325
// contests were queried across 135 polls and every single one came back
// `unresolved`, including contests that had finished hours earlier. Nothing
// settled, and every stake sat in escrow with nothing in the system able to
// release it.
//
// # Why the other tests could not see it
//
// Every test in internal/ingest/provider/synthetic builds its query with the
// generator's own matchEventID on BOTH sides, and every test in this package
// uses a fake provider that echoes whatever identifier it was handed. Both are
// reasonable in isolation and both are blind to this defect by construction: an
// id space that is wrong everywhere is consistent with itself.
//
// So the test below deliberately crosses the two spaces. It takes an identifier
// as the DATABASE holds it — derived with normalizer.EventIDFor, exactly as the
// ingest pipeline derived the row — puts it on the work queue, and drives one
// tick of the real poller against the real generator. Nothing here re-derives a
// fixture identifier or a fixture time: both come from the generator's own board
// through Fetch, because a test that computed the identifier itself is precisely
// what let the two spaces diverge unnoticed.

// aStartedContest returns one contest the generator is currently staging that
// has already tipped off, taken from the generator's own board.
//
// It is discovered through Fetch rather than constructed, so the identifier and
// the scheduled start are the generator's own values and not a second opinion
// about its fixture grid.
func aStartedContest(t *testing.T, gen *synthetic.Adapter, league domain.League, now time.Time) domain.Event {
	t.Helper()

	snap, err := gen.Fetch(context.Background(), provider.Scope{
		League:  league.ID(),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline},
	})
	if err != nil {
		t.Fatalf("Fetch(%s): %v", league.ID(), err)
	}
	for _, e := range snap.Events {
		if e.Event.Kind() == domain.EventKindMatch && e.Event.HasStartedBy(now) {
			return e.Event
		}
	}
	t.Fatalf("the generator staged no contest in %s that had started by %s", league.ID(), now)
	return domain.Event{}
}

// TestThePollerResolvesAContestNamedTheWayTheDatabaseNamesIt is the regression
// test for the id-space mismatch described above. It fails against a poller that
// passes a domain identifier into an adapter's native space, and it fails in the
// way the live system failed: nothing is recorded, and the contest is reported
// as unresolved for ever.
func TestThePollerResolvesAContestNamedTheWayTheDatabaseNamesIt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// One clock, shared by the generator and the poller, moved once. Two clocks
	// would let the contest be over for one and in play for the other, which is
	// the one way this test could pass or fail for a reason other than the one
	// it is about.
	var clock time.Time
	now := func() time.Time { return clock }

	clock = testNow
	gen, err := synthetic.New(synthetic.Options{Seed: 7, Clock: now})
	if err != nil {
		t.Fatalf("synthetic.New: %v", err)
	}
	cat, err := gen.Catalogue(ctx)
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	if len(cat.Leagues) == 0 {
		t.Fatal("the generator stages no leagues")
	}
	league := cat.Leagues[0]
	ev := aStartedContest(t, gen, league, clock)

	// Past the final whistle by a comfortable margin, and comfortably inside any
	// scores lookback window. The exact contest duration is the generator's
	// business and is deliberately not asserted here.
	clock = ev.ScheduledStart().Add(4 * time.Hour)

	// The identifiers as the DATABASE holds them: the forward derivation
	// internal/ingest/normalizer applied when it wrote the catalogue row.
	prov, err := kafka.NewProvider(provider.NameSynthetic.String())
	if err != nil {
		t.Fatalf("kafka.NewProvider: %v", err)
	}
	eventID, err := normalizer.EventIDFor(prov, ev.ID().String())
	if err != nil {
		t.Fatalf("EventIDFor(%s): %v", ev.ID(), err)
	}
	leagueID, err := normalizer.LeagueIDFor(prov, league.ID().String())
	if err != nil {
		t.Fatalf("LeagueIDFor(%s): %v", league.ID(), err)
	}
	if eventID.String() == ev.ID().String() {
		t.Fatalf("the domain identifier %s is the generator's own; the two id spaces have "+
			"collapsed into one and this test proves nothing", eventID)
	}

	pending := PendingEvent{
		EventID:        eventID,
		League:         leagueID,
		Kind:           domain.EventKindMatch,
		Name:           ev.Name(),
		Status:         domain.EventStatusLive,
		ScheduledStart: ev.ScheduledStart(),
		// The last time the odds path saw the contest alive, which is what a
		// stranded row carries: the books pull their prices well before the
		// final whistle.
		ObservedAt: ev.ScheduledStart().Add(time.Hour),
	}
	store := newFakeStore(pending)

	p, err := New(Options{
		Config:   Config{Now: now},
		Provider: gen,
		Store:    store,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	got, ok := store.result(eventID)
	if !ok {
		t.Fatalf("the poller recorded no result for %s, a contest the generator staged as %s "+
			"and finished four hours ago; every stake on it stays in escrow", eventID, ev.ID())
	}
	if got.Status != domain.EventStatusEnded {
		t.Errorf("status = %s, want ended", got.Status)
	}
	if !got.HasScore {
		t.Error("an ended contest was recorded with no score")
	}
	// FinalisedAt is the final whistle — a fixture fact — and never the tick's
	// own clock. Stamping the tick would make the settlement-lag histogram read
	// zero for ever.
	if !got.FinalisedAt.After(ev.ScheduledStart()) || !got.FinalisedAt.Before(clock) {
		t.Errorf("FinalisedAt = %s, want an instant between the start %s and now %s",
			got.FinalisedAt, ev.ScheduledStart(), clock)
	}
}
