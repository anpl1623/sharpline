// Window classification: the "high frequency for live and near-tip events, low
// for futures" half of CLAUDE.md §5.
//
// The tests are an external package (scheduler_test) on purpose. Everything
// asserted here is reachable through the exported surface, and a test that can
// only see the exported surface cannot accidentally pin an implementation
// detail that the next refactor is entitled to move.
package scheduler_test

import (
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/scheduler"
)

// testNow is the fixed instant every classification test is computed against.
// A frozen clock rather than time.Now: window boundaries are duration
// comparisons, and a test that read the wall clock would be a test whose result
// depends on how long the previous test took.
var testNow = time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)

// newMatch builds a real domain.Event through the domain constructor. Nothing
// here is a stub — an Event that had not passed domain.NewEvent would prove
// nothing about what ClassifyEvent does to the values the pipeline actually
// carries.
func newMatch(t *testing.T, id string, status domain.EventStatus, start time.Time) domain.Event {
	t.Helper()

	home, err := domain.NewCompetitor(domain.CompetitorID("home-"+id), "Home Team")
	if err != nil {
		t.Fatalf("build home competitor: %v", err)
	}
	away, err := domain.NewCompetitor(domain.CompetitorID("away-"+id), "Away Team")
	if err != nil {
		t.Fatalf("build away competitor: %v", err)
	}

	e, err := domain.NewEvent(domain.EventParams{
		ID:             domain.EventID("event-" + id),
		LeagueID:       domain.LeagueID("league-" + id),
		Kind:           domain.EventKindMatch,
		Name:           "Away at Home",
		Home:           home,
		Away:           away,
		ScheduledStart: start,
		Status:         status,
		UpdatedAt:      testNow,
	})
	if err != nil {
		t.Fatalf("build match: %v", err)
	}
	return e
}

// newOutright builds a futures event: no competitors, EventKindOutright.
func newOutright(t *testing.T, id string, status domain.EventStatus, start time.Time) domain.Event {
	t.Helper()

	e, err := domain.NewEvent(domain.EventParams{
		ID:             domain.EventID("event-" + id),
		LeagueID:       domain.LeagueID("league-" + id),
		Kind:           domain.EventKindOutright,
		Name:           "2027 Champion",
		ScheduledStart: start,
		Status:         status,
		UpdatedAt:      testNow,
	})
	if err != nil {
		t.Fatalf("build outright: %v", err)
	}
	return e
}

// TestWindowStringIsTheMetricLabelContract pins the five label values.
//
// These strings are not cosmetic. metrics.go states that
// sharpline_ingest_poll_interval_seconds{window="live"} is selected BY NAME in
// the SLO objective recording rule and by the OddsPollCadenceUnknown alert, so
// renaming one silently empties a panel and disarms an alert — Prometheus
// answers a query for a series that does not exist with no data rather than an
// error. This test is the thing that makes such a rename fail loudly.
func TestWindowStringIsTheMetricLabelContract(t *testing.T) {
	t.Parallel()

	want := map[scheduler.Window]string{
		scheduler.WindowUnknown: "unknown",
		scheduler.WindowLive:    "live",
		scheduler.WindowNearTip: "near_tip",
		scheduler.WindowToday:   "today",
		scheduler.WindowDistant: "distant",
		scheduler.WindowFutures: "futures",
	}
	for w, s := range want {
		if got := w.String(); got != s {
			t.Errorf("Window(%d).String() = %q, want %q — this is a Prometheus label value "+
				"read by name from deploy/observability", uint8(w), got, s)
		}
	}
}

// TestWindowsIsUrgencyOrderedAndFresh covers both properties of Windows().
func TestWindowsIsUrgencyOrderedAndFresh(t *testing.T) {
	t.Parallel()

	ws := scheduler.Windows()
	if len(ws) != 5 {
		t.Fatalf("Windows() returned %d windows, want 5", len(ws))
	}
	for i, w := range ws {
		if !w.Valid() {
			t.Errorf("Windows()[%d] = %s, which is not valid", i, w)
		}
		if i > 0 && !ws[i-1].MoreUrgentThan(w) {
			t.Errorf("Windows() is not urgency-ordered: %s does not precede %s", ws[i-1], w)
		}
	}

	// Mutating the returned slice must not affect the next caller: a
	// package-level slice would be global mutable state (CLAUDE.md §12) and a
	// caller that sorted it in place would reorder the metric label set for
	// everyone.
	ws[0] = scheduler.WindowFutures
	if again := scheduler.Windows(); again[0] != scheduler.WindowLive {
		t.Errorf("Windows()[0] = %s after a caller mutated an earlier result; it must return a fresh slice",
			again[0])
	}
}

// TestWindowValidRejectsTheZeroValue: WindowUnknown is what an unclassified
// event silently becomes, and giving it a plausible cadence would mean a bug
// polls at some arbitrary rate instead of failing.
func TestWindowValidRejectsTheZeroValue(t *testing.T) {
	t.Parallel()

	if scheduler.WindowUnknown.Valid() {
		t.Error("WindowUnknown.Valid() = true; the zero value must be invalid")
	}
	if scheduler.Window(200).Valid() {
		t.Error("Window(200).Valid() = true; an out-of-range value must be invalid")
	}
}

func TestMoreUrgentThan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		w     scheduler.Window
		other scheduler.Window
		want  bool
	}{
		{"live beats near-tip", scheduler.WindowLive, scheduler.WindowNearTip, true},
		{"near-tip does not beat live", scheduler.WindowNearTip, scheduler.WindowLive, false},
		{"equal is not more urgent", scheduler.WindowToday, scheduler.WindowToday, false},
		{"futures beats unknown", scheduler.WindowFutures, scheduler.WindowUnknown, true},
		{"unknown beats nothing", scheduler.WindowUnknown, scheduler.WindowFutures, false},
		{"unknown does not beat unknown", scheduler.WindowUnknown, scheduler.WindowUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.w.MoreUrgentThan(tc.other); got != tc.want {
				t.Errorf("%s.MoreUrgentThan(%s) = %v, want %v", tc.w, tc.other, got, tc.want)
			}
		})
	}
}

func TestParseWindowRoundTrips(t *testing.T) {
	t.Parallel()

	for _, w := range scheduler.Windows() {
		got, err := scheduler.ParseWindow(w.String())
		if err != nil {
			t.Errorf("ParseWindow(%q): %v", w.String(), err)
			continue
		}
		if got != w {
			t.Errorf("ParseWindow(%q) = %s, want %s", w.String(), got, w)
		}
	}
}

func TestParseWindowRejectsUnknown(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "unknown", "LIVE", "near-tip", "nearTip", "pregame"} {
		got, err := scheduler.ParseWindow(s)
		if !errors.Is(err, scheduler.ErrUnknownWindow) {
			t.Errorf("ParseWindow(%q) error = %v, want ErrUnknownWindow", s, err)
		}
		if got != scheduler.WindowUnknown {
			t.Errorf("ParseWindow(%q) = %s, want WindowUnknown", s, got)
		}
	}
}

// TestClassifyEvent is the specification in doc order: status first, then kind,
// then the clock.
func TestClassifyEvent(t *testing.T) {
	t.Parallel()

	b := scheduler.DefaultBoundaries()

	cases := []struct {
		name  string
		event func(t *testing.T) domain.Event
		want  scheduler.Window
	}{
		{
			// Rule 1. No live market to price, so a credit spent here is spent on
			// nothing.
			name: "ended is not pollable",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "ended", domain.EventStatusEnded, testNow.Add(-3*time.Hour))
			},
			want: scheduler.WindowUnknown,
		},
		{
			name: "settled is not pollable",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "settled", domain.EventStatusSettled, testNow.Add(-3*time.Hour))
			},
			want: scheduler.WindowUnknown,
		},
		{
			name: "postponed is not pollable",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "postponed", domain.EventStatusPostponed, testNow.Add(time.Hour))
			},
			want: scheduler.WindowUnknown,
		},
		{
			name: "cancelled is not pollable",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "cancelled", domain.EventStatusCancelled, testNow.Add(time.Hour))
			},
			want: scheduler.WindowUnknown,
		},
		{
			// Rule 2. Status wins over the clock: a delayed kickoff that is
			// already in play must not be classified by its advertised start.
			name: "in play is live even when the advertised start is days away",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "live", domain.EventStatusLive, testNow.Add(72*time.Hour))
			},
			want: scheduler.WindowLive,
		},
		{
			name: "suspended is still in play",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "susp", domain.EventStatusSuspended, testNow.Add(-time.Hour))
			},
			want: scheduler.WindowLive,
		},
		{
			// Rule 3. A season winner has no kickoff.
			name: "an outright starting in an hour is still a future",
			event: func(t *testing.T) domain.Event {
				return newOutright(t, "out", domain.EventStatusScheduled, testNow.Add(time.Hour))
			},
			want: scheduler.WindowFutures,
		},
		{
			// Rule 4. The expensive case: the provider is about to flip it to
			// live and the line is moving now.
			name: "past its start but still scheduled is near-tip, not distant",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "late", domain.EventStatusScheduled, testNow.Add(-45*time.Minute))
			},
			want: scheduler.WindowNearTip,
		},
		{
			name: "exactly on the near-tip boundary is near-tip",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "edge1", domain.EventStatusScheduled, testNow.Add(b.NearTip))
			},
			want: scheduler.WindowNearTip,
		},
		{
			name: "one nanosecond past the near-tip boundary is today",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "edge2", domain.EventStatusScheduled, testNow.Add(b.NearTip+1))
			},
			want: scheduler.WindowToday,
		},
		{
			name: "exactly on the today boundary is today",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "edge3", domain.EventStatusScheduled, testNow.Add(b.Today))
			},
			want: scheduler.WindowToday,
		},
		{
			name: "one nanosecond past the today boundary is distant",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "edge4", domain.EventStatusScheduled, testNow.Add(b.Today+1))
			},
			want: scheduler.WindowDistant,
		},
		{
			name: "exactly on the distant boundary is distant",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "edge5", domain.EventStatusScheduled, testNow.Add(b.Distant))
			},
			want: scheduler.WindowDistant,
		},
		{
			name: "beyond the distant boundary is a future",
			event: func(t *testing.T) domain.Event {
				return newMatch(t, "edge6", domain.EventStatusScheduled, testNow.Add(b.Distant+time.Hour))
			},
			want: scheduler.WindowFutures,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scheduler.ClassifyEvent(tc.event(t), testNow, b); got != tc.want {
				t.Errorf("ClassifyEvent = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestFoldWindowsTakesTheMostUrgent is the argument doc.go makes for scheduling
// per league: one sweep returns every event in the league, so polling at the
// pace of the most urgent one gives every other event in the payload a free
// upgrade. Averaging would under-poll the game that is actually in play.
func TestFoldWindowsTakesTheMostUrgent(t *testing.T) {
	t.Parallel()

	b := scheduler.DefaultBoundaries()
	events := []domain.Event{
		newMatch(t, "f1", domain.EventStatusScheduled, testNow.Add(6*24*time.Hour)), // distant
		newMatch(t, "f2", domain.EventStatusScheduled, testNow.Add(4*time.Hour)),    // today
		newMatch(t, "f3", domain.EventStatusLive, testNow.Add(-time.Hour)),          // live
		newMatch(t, "f4", domain.EventStatusScheduled, testNow.Add(10*time.Minute)), // near-tip
	}

	got, ok := scheduler.FoldWindows(events, testNow, b)
	if !ok {
		t.Fatal("FoldWindows reported no pollable event; four of them are pollable")
	}
	if got != scheduler.WindowLive {
		t.Errorf("FoldWindows = %s, want live — the fold must be a minimum over urgency", got)
	}
}

// TestFoldWindowsIgnoresUnpollableEvents: a league of nothing but settled
// fixtures stops being polled rather than being polled slowly for ever.
func TestFoldWindowsIgnoresUnpollableEvents(t *testing.T) {
	t.Parallel()

	b := scheduler.DefaultBoundaries()
	settled := []domain.Event{
		newMatch(t, "s1", domain.EventStatusSettled, testNow.Add(-4*time.Hour)),
		newMatch(t, "s2", domain.EventStatusEnded, testNow.Add(-2*time.Hour)),
	}
	if got, ok := scheduler.FoldWindows(settled, testNow, b); ok {
		t.Errorf("FoldWindows over settled fixtures = (%s, true), want (unknown, false)", got)
	}
	if got, ok := scheduler.FoldWindows(nil, testNow, b); ok {
		t.Errorf("FoldWindows(nil) = (%s, true), want (unknown, false)", got)
	}

	// A settled fixture alongside a live one must not drag the fold down.
	mixed := append(append([]domain.Event(nil), settled...),
		newMatch(t, "s3", domain.EventStatusLive, testNow))
	got, ok := scheduler.FoldWindows(mixed, testNow, b)
	if !ok || got != scheduler.WindowLive {
		t.Errorf("FoldWindows over a settled+live league = (%s, %v), want (live, true)", got, ok)
	}
}

func TestBoundariesValidate(t *testing.T) {
	t.Parallel()

	if err := scheduler.DefaultBoundaries().Validate(); err != nil {
		t.Fatalf("DefaultBoundaries().Validate(): %v", err)
	}

	cases := map[string]scheduler.Boundaries{
		"zero near-tip":        {NearTip: 0, Today: time.Hour, Distant: 24 * time.Hour},
		"negative near-tip":    {NearTip: -time.Minute, Today: time.Hour, Distant: 24 * time.Hour},
		"today inside neartip": {NearTip: time.Hour, Today: time.Minute, Distant: 24 * time.Hour},
		"today equals neartip": {NearTip: time.Hour, Today: time.Hour, Distant: 24 * time.Hour},
		"distant inside today": {NearTip: time.Minute, Today: time.Hour, Distant: time.Minute},
		"distant equals today": {NearTip: time.Minute, Today: time.Hour, Distant: time.Hour},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := b.Validate()
			if !errors.Is(err, scheduler.ErrInvalidConfig) {
				t.Errorf("Validate() = %v, want an error wrapping ErrInvalidConfig", err)
			}
		})
	}
}
