package integration

import (
	"context"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/results"
	resultspg "github.com/anpl1623/sharpline/internal/ingest/results/pgstore"
)

// internal/ingest/results/pgstore against a real Postgres.
//
// The property under test is the ROW-COUNT CONTRACT of UpsertEventResult, and it
// cannot be proved anywhere else. Three of its four behaviours are enforced by
// the STATEMENT and by migration 00002's CHECK constraints rather than by any Go
// code:
//
//   - it only ever moves a row INTO the results feed, so a result cannot
//     un-settle a ticket that has already been graded and paid;
//   - it refuses an observation older than the stored one;
//   - it must NULL all three clock columns in the same statement, because
//     events_clock_only_in_play refuses a clock on a row that is not in play and
//     events_clock_all_or_nothing requires all three or none. A statement that
//     cleared two of them, or none, fails the CHECK the instant a live row
//     becomes `ended` — and a fake would happily accept it.
//
// It is also where the earlier hand-written duplicate of this statement would
// have been caught: it guarded with `IS DISTINCT FROM` instead of on the source
// status, so it permitted settled → ended. TestAResultCannotUnsettleAnEvent is
// that test.

func resultsStore(t *testing.T) *resultspg.Store {
	t.Helper()

	db, _ := sharedPool(t)
	store, err := resultspg.NewStore(db)
	if err != nil {
		t.Fatalf("build results pgstore: %v", err)
	}
	return store
}

// endedResultFor is the ordinary outcome: a contest played to a final score.
//
// The key on it is the PROVIDER'S own spelling and is deliberately NOT the row's
// identifier. The statement is addressed by the domain identifier passed beside
// the result — the one internal/ingest/results derived from a key like this one
// — and a fixture that used one string for both could not tell a store that read
// the wrong field from one that read the right one.
func endedResultFor(t *testing.T, at time.Time) provider.FinalResult {
	t.Helper()

	score, err := domain.NewScore(104, 99)
	if err != nil {
		t.Fatalf("NewScore: %v", err)
	}
	return provider.FinalResult{
		EventKey:    "syn-fixture-1",
		Status:      domain.EventStatusEnded,
		Score:       score,
		HasScore:    true,
		FinalisedAt: at.UTC(),
	}
}

// eventRow reads back the columns this statement is allowed to touch.
func eventRow(t *testing.T, ctx context.Context, id domain.EventID) (
	status string, home, away *int32, observedAt time.Time,
	period *int32, elapsed *int64, running *bool,
) {
	t.Helper()

	db, _ := sharedPool(t)
	err := db.Pool().QueryRow(ctx, `
SELECT status, score_home, score_away, observed_at,
       clock_period, clock_elapsed_ns, clock_running
  FROM events WHERE id = $1`, id).
		Scan(&status, &home, &away, &observedAt, &period, &elapsed, &running)
	if err != nil {
		t.Fatalf("read event %s: %v", id, err)
	}
	return
}

// TestRecordResultWritesTheOutcomeAndClearsTheClock is the happy path, and the
// clock half of it is the part a fake cannot check. The row is put in play with
// a running clock first, exactly as the odds path would leave it, so the UPDATE
// has three columns it MUST clear in the same statement or fail the CHECK.
func TestRecordResultWritesTheOutcomeAndClearsTheClock(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, _ := sharedPool(t)
	c := newCatalogue(t, ctx, db.Pool())
	store := resultsStore(t)

	observed := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	mustExec(t, ctx, db.Pool(), `
UPDATE events
   SET status = 'live', clock_period = 4, clock_elapsed_ns = $2, clock_running = TRUE,
       observed_at = $3
 WHERE id = $1`, c.EventID, int64(11*time.Minute), observed)

	finalisedAt := observed.Add(30 * time.Minute)
	wrote, err := store.RecordResult(ctx, c.EventID, endedResultFor(t, finalisedAt))
	if err != nil {
		t.Fatalf("RecordResult: %v", err)
	}
	if !wrote {
		t.Fatal("RecordResult reported no write for a contest that had not been resulted")
	}

	status, home, away, storedAt, period, elapsed, running := eventRow(t, ctx, c.EventID)
	if status != "ended" {
		t.Errorf("status = %q, want ended", status)
	}
	if home == nil || away == nil || *home != 104 || *away != 99 {
		t.Errorf("score = (%v, %v), want (104, 99)", home, away)
	}
	if !storedAt.Equal(finalisedAt) {
		// The PROVIDER's instant. settlement stamps every leg graded from this
		// row with it, so writing the container's clock here would restamp a
		// replayed settlement with the replay's own time.
		t.Errorf("observed_at = %s, want the provider's %s", storedAt, finalisedAt)
	}
	if period != nil || elapsed != nil || running != nil {
		t.Errorf("the clock survived the result: period=%v elapsed=%v running=%v",
			period, elapsed, running)
	}
}

// TestRecordResultIsIdempotent. A zero row count is a SUCCESS, and it is what
// lets the poller keep no memo of what it has recorded — a memo would be a
// second, divergeable answer to a question the database already answers exactly,
// and it would have to survive a restart.
func TestRecordResultIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, _ := sharedPool(t)
	c := newCatalogue(t, ctx, db.Pool())
	store := resultsStore(t)

	// The fixture's observed_at is `now`, and the out-of-order guard is
	// `observed_at <= @observed_at`, so a result must be stamped at or after it
	// to land at all. Pushing the row's own observation back is how a real
	// pending contest looks: the odds path last saw it alive before it ended.
	at := time.Now().UTC().Truncate(time.Microsecond)
	mustExec(t, ctx, db.Pool(),
		`UPDATE events SET status = 'live', observed_at = $2 WHERE id = $1`,
		c.EventID, at.Add(-2*time.Hour))

	if wrote, err := store.RecordResult(ctx, c.EventID, endedResultFor(t, at)); err != nil || !wrote {
		t.Fatalf("first RecordResult = (%t, %v), want (true, nil)", wrote, err)
	}
	wrote, err := store.RecordResult(ctx, c.EventID, endedResultFor(t, at))
	if err != nil {
		t.Fatalf("a replayed result was reported as an error: %v", err)
	}
	if wrote {
		t.Error("a replayed result reported a second write; the poller would count it as newly settleable")
	}
}

// TestAResultCannotUnsettleAnEvent is the guard the whole statement exists for.
//
// `settled` is the domain's only terminal status and nothing may write past it:
// the wagers on that contest have already been graded and paid. A statement
// guarded on `IS DISTINCT FROM` instead of on the source status would permit
// this, which is exactly why the hand-written duplicate of this UPDATE was
// deleted rather than kept.
func TestAResultCannotUnsettleAnEvent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, _ := sharedPool(t)
	store := resultsStore(t)
	at := time.Now().UTC().Truncate(time.Microsecond)

	for _, from := range []string{"settled", "cancelled", "ended"} {
		t.Run("from "+from, func(t *testing.T) {
			c := newCatalogue(t, ctx, db.Pool())
			// A settled or ended row carries a score; a cancelled one must not.
			// events_score_all_or_nothing constrains the pair either way.
			switch from {
			case "cancelled":
				mustExec(t, ctx, db.Pool(),
					`UPDATE events SET status = $2, observed_at = $3 WHERE id = $1`,
					c.EventID, from, at.Add(-time.Hour))
			default:
				mustExec(t, ctx, db.Pool(),
					`UPDATE events SET status = $2, score_home = 1, score_away = 2, observed_at = $3
					   WHERE id = $1`,
					c.EventID, from, at.Add(-time.Hour))
			}

			// A NEWER observation, so the only thing that can refuse it is the
			// status guard.
			wrote, err := store.RecordResult(ctx, c.EventID, endedResultFor(t, at))
			if err != nil {
				t.Fatalf("RecordResult: %v", err)
			}
			if wrote {
				t.Fatalf("a result overwrote a %s event; wagers on it have already been graded", from)
			}

			status, home, away, _, _, _, _ := eventRow(t, ctx, c.EventID)
			if status != from {
				t.Errorf("status = %q, want it left at %q", status, from)
			}
			if from != "cancelled" && (home == nil || *home != 1 || away == nil || *away != 2) {
				t.Errorf("score = (%v, %v), want it left at (1, 2)", home, away)
			}
		})
	}
}

// TestAnOlderObservationIsRefused. A redelivery, a replay, or two ingest
// replicas racing must not overwrite a newer observation with an older one.
func TestAnOlderObservationIsRefused(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, _ := sharedPool(t)
	c := newCatalogue(t, ctx, db.Pool())
	store := resultsStore(t)

	stored := time.Now().UTC().Truncate(time.Microsecond)
	mustExec(t, ctx, db.Pool(),
		`UPDATE events SET status = 'live', observed_at = $2 WHERE id = $1`, c.EventID, stored)

	wrote, err := store.RecordResult(ctx, c.EventID, endedResultFor(t, stored.Add(-time.Hour)))
	if err != nil {
		t.Fatalf("an out-of-order result was reported as an error: %v", err)
	}
	if wrote {
		t.Error("an older observation overwrote a newer one")
	}
	if status, _, _, _, _, _, _ := eventRow(t, ctx, c.EventID); status != "live" {
		t.Errorf("status = %q, want it left at live", status)
	}
}

// TestACancelledResultCarriesNoScore. A cancelled contest will not be played,
// every leg voids and every stake comes back — there is no score, and
// events_score_all_or_nothing means the pair has to move together or the write
// fails outright.
func TestACancelledResultCarriesNoScore(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, _ := sharedPool(t)
	c := newCatalogue(t, ctx, db.Pool())
	store := resultsStore(t)

	at := time.Now().UTC().Truncate(time.Microsecond)
	wrote, err := store.RecordResult(ctx, c.EventID, provider.FinalResult{
		EventKey:    "syn-fixture-1",
		Status:      domain.EventStatusCancelled,
		HasScore:    false,
		FinalisedAt: at,
	})
	if err != nil {
		t.Fatalf("RecordResult: %v", err)
	}
	if !wrote {
		t.Fatal("a cancellation was not recorded")
	}

	status, home, away, _, _, _, _ := eventRow(t, ctx, c.EventID)
	if status != "cancelled" {
		t.Errorf("status = %q, want cancelled", status)
	}
	if home != nil || away != nil {
		t.Errorf("a cancelled contest carries a score (%v, %v)", home, away)
	}
}

// TestAResultForAnUnknownEventWritesNothing. The statement is an UPDATE and
// never an INSERT, which is the mechanical form of the NO MOCK DATA rule: a
// result for a contest this deployment never ingested cannot create a contest.
// Zero rows, and not an error.
func TestAResultForAnUnknownEventWritesNothing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := resultsStore(t)
	ghost := eventID(t, uniqueID("event"))

	wrote, err := store.RecordResult(ctx, ghost, endedResultFor(t, time.Now().UTC()))
	if err != nil {
		t.Fatalf("a result for an unknown contest was reported as an error: %v", err)
	}
	if wrote {
		t.Fatal("a result created a row for a contest that was never ingested")
	}

	db, _ := sharedPool(t)
	var n int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE id = $1`, ghost).
		Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Errorf("the results path inserted %d catalogue rows; it must only ever UPDATE", n)
	}
}

// TestAnUnusableResultNeverReachesTheDatabase. An `ended` result with no score
// is storable — the schema constrains the score PAIR, not its presence — and
// would then grade every spread on the contest against a 0-0 zero value. A
// plausible wrong number is worse than an error, so the adapter refuses it
// before the statement runs.
func TestAnUnusableResultNeverReachesTheDatabase(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, _ := sharedPool(t)
	c := newCatalogue(t, ctx, db.Pool())
	store := resultsStore(t)

	wrote, err := store.RecordResult(ctx, c.EventID, provider.FinalResult{
		EventKey:    "syn-fixture-1",
		Status:      domain.EventStatusEnded,
		HasScore:    false,
		FinalisedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("a scoreless ended result was accepted")
	}
	if wrote {
		t.Error("a refused result reported a write")
	}
	if status, _, _, _, _, _, _ := eventRow(t, ctx, c.EventID); status != "scheduled" {
		t.Errorf("status = %q, want it untouched at scheduled", status)
	}
}

// TestEventsAwaitingResultReturnsTheWorkQueue. The queue's membership rule is
// the partial index's own predicate, and `postponed` is the one interesting
// exclusion: a postponed contest is awaiting a new START TIME, not a result —
// the domain admits postponed → scheduled — and its start recedes further into
// the past every day, so including it would park it permanently at the head of
// this queue and ask the provider for the score of a game nobody played.
func TestEventsAwaitingResultReturnsTheWorkQueue(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, _ := sharedPool(t)
	store := resultsStore(t)

	// Well before any other test's fixtures, so the horizon below selects this
	// test's rows and nothing else's.
	base := time.Now().UTC().AddDate(-2, 0, 0).Truncate(time.Microsecond)
	horizon := base.Add(time.Hour)

	type fixture struct {
		status string
		queued bool
	}
	want := map[domain.EventID]fixture{}
	for _, f := range []fixture{
		{"scheduled", true},
		{"live", true},
		{"suspended", true},
		{"postponed", false},
		{"ended", false},
		{"settled", false},
		{"cancelled", false},
	} {
		c := newCatalogue(t, ctx, db.Pool())
		switch f.status {
		case "ended", "settled":
			mustExec(t, ctx, db.Pool(), `
UPDATE events SET status = $2, score_home = 1, score_away = 0, scheduled_start = $3
 WHERE id = $1`, c.EventID, f.status, base)
		default:
			mustExec(t, ctx, db.Pool(),
				`UPDATE events SET status = $2, scheduled_start = $3 WHERE id = $1`,
				c.EventID, f.status, base)
		}
		want[c.EventID] = f
	}

	queue, err := store.EventsAwaitingResult(ctx, horizon, 500)
	if err != nil {
		t.Fatalf("EventsAwaitingResult: %v", err)
	}

	got := map[domain.EventID]results.PendingEvent{}
	for _, e := range queue {
		got[e.EventID] = e
	}
	for id, f := range want {
		e, present := got[id]
		if present != f.queued {
			t.Errorf("a %s contest queued=%t, want %t", f.status, present, f.queued)
			continue
		}
		if !f.queued {
			continue
		}
		if e.Status.String() != f.status {
			t.Errorf("queued status = %s, want %s", e.Status, f.status)
		}
		if e.Kind != domain.EventKindMatch {
			t.Errorf("queued kind = %s, want match", e.Kind)
		}
		if e.Name == "" {
			t.Error("queued row carries no fixture name; an operator would see only an identifier")
		}
	}

	// Ordered oldest-first, so the contest whose customer has been waiting
	// longest is polled first.
	for i := 1; i < len(queue); i++ {
		if queue[i].ScheduledStart.Before(queue[i-1].ScheduledStart) {
			t.Fatalf("the work queue is not oldest-first at index %d", i)
		}
	}
}

// TestEventsAwaitingResultRefusesANonPositiveLimit. `LIMIT 0` returns no rows,
// which reads as "there is nothing to settle" and is a silent, permanent stall
// on every customer's escrow. It is refused rather than sent.
func TestEventsAwaitingResultRefusesANonPositiveLimit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := resultsStore(t)
	for _, limit := range []int{0, -1} {
		if _, err := store.EventsAwaitingResult(ctx, time.Now().UTC(), limit); err == nil {
			t.Errorf("a limit of %d was accepted", limit)
		}
	}
}
