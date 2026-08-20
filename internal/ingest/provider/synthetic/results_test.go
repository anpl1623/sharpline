package synthetic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// The results endpoint's tests. The property under all of them is the one
// doc.go claims for prices and results.go claims for scores: this is the
// GENERATOR'S OWN OUTPUT, computed from the same latent structure that prices
// the market, and not a fixture table.
//
// A second property is asserted throughout and is worth naming, because its
// absence is what once stopped this system settling anything: every identifier
// that leaves this package is the GENERATOR'S OWN, the same string its raw
// payloads carry. The domain identifier the database holds is derived from that
// key by internal/ingest/normalizer, and deriving it is the poller's job — see
// provider.ResultWindow, and the crossing test in internal/ingest/results.

// wholeWindow is the widest span this adapter will answer over: its own lookback
// up to the caller's instant. Tests that are not about the window itself use it,
// so that what they assert is the generator's answer rather than the clipping.
func wholeWindow(now time.Time) provider.ResultWindow {
	return provider.ResultWindow{Since: now.Add(-resultsLookback), Until: now}
}

// firstLeague is the league every test here uses; which one it is does not
// matter, only that it is one the generator stages.
func firstLeague(t *testing.T) leagueDef {
	t.Helper()
	all := leagues()
	if len(all) == 0 {
		t.Fatal("the generator stages no leagues")
	}
	return all[0]
}

// gridContest snaps `want` onto the league's fixture grid and returns the
// generator's own identifier for the contest starting there, its start, and its
// slot.
//
// It reconstructs the grid the way buildSlate lays it out — start = dayStart +
// slot·spacing + leagueOffset — rather than reading buildSlate's output, because
// the case this file is mostly about is the contest that has LEFT the board:
// buildSlate retires a fixture endedGrace after the whistle, and a helper built
// on it could not produce one. The identifier still comes from the generator's
// own matchEventID, so nothing here re-implements an id format.
func gridContest(t *testing.T, a *Adapter, l leagueDef, want time.Time) (domain.EventID, time.Time, int) {
	t.Helper()
	slots := a.opts.EventsPerLeaguePerDay
	spacing := time.Duration(int64(24*time.Hour) / int64(slots))
	origin := dayStart(want).Add(leagueOffset(l))

	slot := int(want.Sub(origin) / spacing)
	switch {
	case slot < 0:
		slot = 0
	case slot >= slots:
		slot = slots - 1
	}
	start := origin.Add(time.Duration(slot) * spacing)
	return l.matchEventID(start, slot), start, slot
}

// aFinishedContest returns the identifier of a contest that finished `since`
// before `now`, together with the instant it finished and its grid slot. The
// offset is approximate — it lands on the nearest grid slot at or before the one
// asked for — so callers give it comfortable margins rather than boundary
// values.
func aFinishedContest(t *testing.T, a *Adapter, l leagueDef, now time.Time, since time.Duration) (
	domain.EventID, time.Time, int,
) {
	t.Helper()
	id, start, slot := gridContest(t, a, l, now.Add(-since-liveDuration))
	end := start.Add(liveDuration)
	if !end.Before(now) {
		t.Fatalf("gridContest produced a contest ending at %s, which is not before %s", end, now)
	}
	return id, end, slot
}

func results(t *testing.T, a *Adapter, w provider.ResultWindow) []provider.FinalResult {
	t.Helper()
	out, err := a.Results(context.Background(), w)
	if err != nil {
		t.Fatalf("Results(%s): %v", w, err)
	}
	return out
}

// reported finds one contest's outcome in an answer, by the generator's own
// identifier.
func reported(out []provider.FinalResult, id domain.EventID) (provider.FinalResult, bool) {
	for _, r := range out {
		if r.EventKey == string(id) {
			return r, true
		}
	}
	return provider.FinalResult{}, false
}

// -----------------------------------------------------------------------------
// The seam
// -----------------------------------------------------------------------------

func TestAdapterSatisfiesResultsProvider(t *testing.T) {
	t.Parallel()

	var p provider.ResultsProvider = newTestAdapter(t, testSeed, testNow)
	if p.Name() != provider.NameSynthetic {
		t.Fatalf("Name() = %q, want %q", p.Name(), provider.NameSynthetic)
	}
}

// -----------------------------------------------------------------------------
// What is answered
// -----------------------------------------------------------------------------

func TestAFinishedContestIsResulted(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	l := firstLeague(t)
	id, end, _ := aFinishedContest(t, a, l, testNow, time.Hour)

	r, ok := reported(results(t, a, wholeWindow(testNow)), id)
	if !ok {
		t.Fatalf("the generator did not report %s, a contest it staged and finished an hour ago", id)
	}
	if r.Status != domain.EventStatusEnded {
		t.Errorf("status = %s, want ended", r.Status)
	}
	if !r.HasScore {
		t.Fatal("an ended contest was resulted with no score")
	}
	if err := r.Validate(); err != nil {
		t.Errorf("the generator produced a result the domain refuses: %v", err)
	}
	// FinalisedAt is a FIXTURE FACT — start plus the league's contest duration —
	// and never the model's current instant. Stamping `now` would make the
	// poller's settlement-lag histogram read zero for ever, which is the one
	// number an operator asks for when a ticket has not settled.
	if !r.FinalisedAt.Equal(end) {
		t.Errorf("FinalisedAt = %s, want the final whistle %s", r.FinalisedAt, end)
	}
	if !r.FinalisedAt.Before(testNow) {
		t.Error("a contest was reported as finishing in the future")
	}
}

// TestTheKeyIsTheGeneratorsOwnIdentifier is the assertion that keeps the two
// identifier spaces apart at this end of the seam.
//
// The key an adapter states must be the one its RAW PAYLOADS carry, because that
// is the string internal/ingest/normalizer derives the database's identifier
// from. Any other spelling — a domain identifier, a prefixed one, a display name
// — resolves to a row that does not exist, silently, for every contest.
func TestTheKeyIsTheGeneratorsOwnIdentifier(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	l := firstLeague(t)
	id, _, slot := aFinishedContest(t, a, l, testNow, time.Hour)

	r, ok := reported(results(t, a, wholeWindow(testNow)), id)
	if !ok {
		t.Fatalf("the generator did not report %s", id)
	}
	// The same identifier the fixture carries on the board and in its raw
	// payload, reached through the generator's own construction path.
	ev := a.buildMatch(l, r.FinalisedAt.Add(-liveDuration), slot, testNow)
	if r.EventKey != string(ev.id) {
		t.Errorf("EventKey = %q, want the generator's own identifier %q", r.EventKey, ev.id)
	}
}

// TestTheFinalIsTheSameScoreALiveFetchWouldHaveCarried is the claim results.go
// makes and the reason this endpoint is not mock data: the final is
// scoreAt(ev, 1) — the same pure function of the event's static means and a
// seeded pace draw that the live model calls on every fetch — not a second
// scoring path that happens to look similar.
func TestTheFinalIsTheSameScoreALiveFetchWouldHaveCarried(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	l := firstLeague(t)
	id, end, slot := aFinishedContest(t, a, l, testNow, time.Hour)

	r, ok := reported(results(t, a, wholeWindow(testNow)), id)
	if !ok {
		t.Fatalf("the generator did not report %s", id)
	}

	// Rebuild the contest through the generator's own construction path and ask
	// the model for the whole contest played.
	ev := a.buildMatch(l, end.Add(-liveDuration), slot, testNow)
	home, away := a.scoreAt(ev, 1)
	if r.Score.Home() != home || r.Score.Away() != away {
		t.Errorf("result score = %s, want the model's own %d-%d", r.Score, home, away)
	}
}

// TestResultsAreDeterministicForASeed. A demo replays and a test is repeatable
// only if the same seed, event and clock give the same final every time. If this
// ever fails, the settlement path has become unreproducible and every golden
// assertion downstream is worthless.
func TestResultsAreDeterministicForASeed(t *testing.T) {
	t.Parallel()

	l := firstLeague(t)
	first := newTestAdapter(t, testSeed, testNow)
	id, _, _ := aFinishedContest(t, first, l, testNow, time.Hour)

	want, ok := reported(results(t, first, wholeWindow(testNow)), id)
	if !ok {
		t.Fatalf("the generator did not report %s", id)
	}

	// A second adapter with the same seed, and the SAME contest asked about at a
	// different (still-past-the-whistle) instant: the final is a fact about the
	// contest, not about when it was asked for.
	later := testNow.Add(11 * time.Hour)
	second := newTestAdapter(t, testSeed, later)
	got, ok := reported(results(t, second, wholeWindow(later)), id)
	if !ok {
		t.Fatalf("the second adapter did not report %s", id)
	}
	if got.Score != want.Score {
		t.Errorf("score = %s on the second adapter, want %s", got.Score, want.Score)
	}
	if !got.FinalisedAt.Equal(want.FinalisedAt) {
		t.Errorf("FinalisedAt = %s, want %s", got.FinalisedAt, want.FinalisedAt)
	}
}

// TestADifferentSeedGivesADifferentSlate guards the other direction: a
// deterministic generator that ignored its seed would be deterministic and
// useless, and every "the same seed replays" assertion above would pass
// vacuously.
func TestADifferentSeedGivesADifferentSlate(t *testing.T) {
	t.Parallel()

	// One contest is a coin flip away from matching by chance, so compare a
	// whole window of them and require that they are not all identical.
	w := wholeWindow(testNow)
	mine := results(t, newTestAdapter(t, testSeed, testNow), w)
	theirs := results(t, newTestAdapter(t, testSeed+1, testNow), w)
	if len(mine) == 0 || len(mine) != len(theirs) {
		t.Fatalf("the two seeds resulted %d and %d contests", len(mine), len(theirs))
	}
	for i := range mine {
		if mine[i].EventKey != theirs[i].EventKey {
			t.Fatalf("the two seeds staged different fixtures (%s vs %s); the calendar is a "+
				"function of the clock alone", mine[i].EventKey, theirs[i].EventKey)
		}
		if mine[i].Score != theirs[i].Score {
			return
		}
	}
	t.Error("two different seeds produced identical finals for every contest")
}

// TestTheAnswerIsOrderedAndStable. The order is league, then day, then slot, and
// it holds across calls. A permutation would make every golden comparison
// downstream a flake and would make two identical answers look different on the
// bus.
func TestTheAnswerIsOrderedAndStable(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	w := wholeWindow(testNow)

	first := results(t, a, w)
	second := results(t, a, w)
	if len(first) == 0 {
		t.Fatal("the generator reported nothing over its whole lookback window")
	}
	if len(first) != len(second) {
		t.Fatalf("two identical calls returned %d and %d results", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("result %d differs between two identical calls: %s vs %s",
				i, first[i], second[i])
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i].EventKey == first[i-1].EventKey {
			t.Fatalf("the same contest %s was reported twice in one answer", first[i].EventKey)
		}
	}
}

// -----------------------------------------------------------------------------
// The window
// -----------------------------------------------------------------------------

// TestOnlyContestsThatFinishedInsideTheWindowAreReported is the shape of the
// endpoint. A scores route answers over a span; a caller that asked about the
// last hour must not be handed yesterday's slate to filter itself.
func TestOnlyContestsThatFinishedInsideTheWindowAreReported(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	l := firstLeague(t)

	recent, recentEnd, _ := aFinishedContest(t, a, l, testNow, time.Hour)
	older, olderEnd, _ := aFinishedContest(t, a, l, testNow, 20*time.Hour)

	// A window that opens after the older contest finished and closes at the
	// caller's instant.
	w := provider.ResultWindow{Since: olderEnd.Add(time.Minute), Until: testNow}
	out := results(t, a, w)

	if _, ok := reported(out, recent); !ok {
		t.Errorf("a contest that finished at %s was not reported inside %s", recentEnd, w)
	}
	if _, ok := reported(out, older); ok {
		t.Errorf("a contest that finished at %s was reported for %s, which opens after it", olderEnd, w)
	}
	for _, r := range out {
		if !w.Covers(r.FinalisedAt) {
			t.Errorf("%s finished at %s, outside %s", r.EventKey, r.FinalisedAt, w)
		}
	}
}

// TestAWindowReachingIntoTheFutureIsClampedToTheModelsClock. The caller's clock
// and the generator's are two clocks. Trusting a window that reaches past this
// adapter's own instant would report a final for a contest still being played,
// which is the one answer a results endpoint must never give.
func TestAWindowReachingIntoTheFutureIsClampedToTheModelsClock(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	l := firstLeague(t)

	// A contest that will not have finished by testNow: it is still in play.
	inPlay, start, _ := gridContest(t, a, l, testNow.Add(-liveDuration/2))
	if !testNow.Before(start.Add(liveDuration)) {
		t.Fatalf("the grid slot at %s has already finished; this case is not exercised", start)
	}

	out := results(t, a, provider.ResultWindow{
		Since: testNow.Add(-resultsLookback),
		Until: testNow.Add(48 * time.Hour),
	})
	if _, ok := reported(out, inPlay); ok {
		t.Error("a contest still in play was resulted because the caller's window reached past " +
			"the model's own clock")
	}
	for _, r := range out {
		if r.FinalisedAt.After(testNow) {
			t.Errorf("%s was reported as finishing at %s, after the model's instant %s",
				r.EventKey, r.FinalisedAt, testNow)
		}
	}
}

// TestExactlyAtTheWhistleIsResulted pins the boundary from both sides, so the
// test above cannot pass by the adapter simply refusing everything near it.
// newEventState draws it at the same instant — `elapsed < liveDuration` is live
// and anything else is ended — so this is the first instant at which the contest
// is over.
func TestExactlyAtTheWhistleIsResulted(t *testing.T) {
	t.Parallel()

	l := firstLeague(t)
	probe := newTestAdapter(t, testSeed, testNow)
	id, end, _ := aFinishedContest(t, probe, l, testNow, time.Hour)

	// A window that is open across the whistle in both cases, so what decides
	// the answer is the adapter's clock and nothing else.
	w := provider.ResultWindow{Since: end.Add(-time.Hour), Until: end.Add(time.Hour)}

	atWhistle := newTestAdapter(t, testSeed, end)
	if _, ok := reported(results(t, atWhistle, w), id); !ok {
		t.Error("a contest at exactly its final whistle was not resulted")
	}

	justBefore := newTestAdapter(t, testSeed, end.Add(-time.Nanosecond))
	if _, ok := reported(results(t, justBefore, w), id); ok {
		t.Error("a contest one nanosecond before its final whistle was resulted")
	}
}

// TestAContestOlderThanTheLookbackIsNotResulted. Past the window the endpoint
// has nothing to say, whatever the caller asked for. The poller keeps the row on
// its work queue and the queue-depth gauge shows it — an honest permanent stall
// an operator can act on, rather than an invented late score.
func TestAContestOlderThanTheLookbackIsNotResulted(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	l := firstLeague(t)

	// A caller asking about a month, which is more than this endpoint sells.
	w := provider.ResultWindow{Since: testNow.AddDate(0, 0, -30), Until: testNow}

	// Comfortably inside the lookback, and comfortably outside it. The exact
	// boundary is not pinned here on purpose: it is a policy constant, and a
	// test that asserted resultsLookback ± a nanosecond would fail on any
	// deliberate retune of it while proving nothing extra.
	inside, _, _ := aFinishedContest(t, a, l, testNow, resultsLookback-6*time.Hour)
	outside, _, _ := aFinishedContest(t, a, l, testNow, resultsLookback+6*time.Hour)

	out := results(t, a, w)
	if _, ok := reported(out, inside); !ok {
		t.Error("a contest inside the lookback was not resulted")
	}
	if _, ok := reported(out, outside); ok {
		t.Error("a contest past the lookback window was resulted")
	}
}

// TestAWindowEntirelyPastTheLookbackIsEmptyAndNotAnError. A poller whose oldest
// queued contest is older than this endpoint's window asks a perfectly
// well-formed question it has nothing to say about. Erroring would turn a
// stranded backlog — which the queue-depth gauge already makes visible — into a
// provider alert and a backoff.
func TestAWindowEntirelyPastTheLookbackIsEmptyAndNotAnError(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	w := provider.ResultWindow{
		Since: testNow.Add(-resultsLookback - 30*24*time.Hour),
		Until: testNow.Add(-resultsLookback - time.Hour),
	}
	if got := results(t, a, w); len(got) != 0 {
		t.Errorf("a window entirely past the lookback produced %d results", len(got))
	}
}

// TestTheResultsWindowOutlivesTheBoard is the deliberate divergence results.go
// argues for, asserted so it cannot be "tidied" back into agreement.
//
// buildSlate retires a contest endedGrace after the whistle, which is right for
// the ODDS board: the book takes its prices down and the fixture leaves the
// screen. A results endpoint is a different endpoint with a different window,
// and it MUST be wider — with a 45-minute results window an ingest process that
// was down over a deploy would permanently strand every contest that finished
// during the gap, and the stakes on them would sit in escrow with nothing left
// able to release them.
func TestTheResultsWindowOutlivesTheBoard(t *testing.T) {
	t.Parallel()

	if resultsLookback <= liveDuration+endedGrace {
		t.Fatalf("resultsLookback %s is not wider than the board's retirement horizon %s",
			resultsLookback, liveDuration+endedGrace)
	}

	a := newTestAdapter(t, testSeed, testNow)
	l := firstLeague(t)
	// Long past endedGrace, so the contest has left the board entirely.
	id, _, _ := aFinishedContest(t, a, l, testNow, endedGrace+6*time.Hour)

	for _, ev := range a.buildSlate(l, testNow) {
		if ev.id == id {
			t.Fatal("the fixture is still on the board; this test is not exercising a retired contest")
		}
	}
	if _, ok := reported(results(t, a, wholeWindow(testNow)), id); !ok {
		t.Errorf("the retired contest %s was not resulted; its stakes would be stranded", id)
	}
}

// -----------------------------------------------------------------------------
// What is refused, and why each refusal is the right answer
// -----------------------------------------------------------------------------

// TestAnUnfinishedContestIsNotResulted is the refusal that matters most. The
// poller asks about a span that reaches up to its own clock; this is where "old
// enough to be over" is resolved into a fact, and inventing a final for a
// contest still being played would grade a customer's ticket against a game in
// progress.
func TestAnUnfinishedContestIsNotResulted(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	l := firstLeague(t)
	out := results(t, a, wholeWindow(testNow))

	for _, tc := range []struct {
		name  string
		start time.Duration // relative to testNow; negative is in the past
	}{
		{"in play", -liveDuration / 2},
		{"just tipped", -time.Minute},
		{"not yet started", 2 * time.Hour},
		{"tomorrow", 26 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, start, _ := gridContest(t, a, l, testNow.Add(tc.start))
			if !testNow.Before(start.Add(liveDuration)) {
				t.Fatalf("the grid slot at %s has already finished; this case is not exercised", start)
			}
			if _, ok := reported(out, id); ok {
				t.Errorf("the contest %s, which has not finished, was resulted", id)
			}
		})
	}
}

// TestAnOutrightIsNotResulted. The season-title event carries no clock-driven
// end and the model holds no "who lifted the trophy" draw, so there is no
// instant at which this generator knows a champion. It says so by saying
// nothing — structurally, because the enumeration walks the match grid and the
// season-title competition is not on it. A stated limit rather than an invented
// one.
func TestAnOutrightIsNotResulted(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	out := results(t, a, wholeWindow(testNow))

	for _, l := range leagues() {
		// The futures identifier carries the season's year, and the window can
		// straddle a new year's eve, so both years the lookback can reach are
		// checked.
		for _, year := range []int{testNow.Year(), testNow.Add(-resultsLookback).Year()} {
			if _, ok := reported(out, l.futuresEventID(year)); ok {
				t.Errorf("the %s %d season title was resulted", l.name, year)
			}
		}
	}
}

// TestAMalformedWindowIsFatal. A malformed window cannot become well formed by
// being asked again, so it is DispositionFatal rather than something the poller
// retries for ever.
func TestAMalformedWindowIsFatal(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)

	for _, tc := range []struct {
		name string
		w    provider.ResultWindow
	}{
		{"no start instant", provider.ResultWindow{Until: testNow}},
		{"no end instant", provider.ResultWindow{Since: testNow.Add(-time.Hour)}},
		{"zero", provider.ResultWindow{}},
		{
			name: "ends before it begins",
			w:    provider.ResultWindow{Since: testNow, Until: testNow.Add(-time.Hour)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Results(context.Background(), tc.w)
			if err == nil {
				t.Fatal("a malformed window was accepted")
			}
			if !errors.Is(err, provider.ErrInvalidScope) {
				t.Errorf("error = %v, want it to wrap ErrInvalidScope", err)
			}
			if got := provider.Classify(err); got != provider.DispositionFatal {
				t.Errorf("disposition = %s, want fatal; a malformed window cannot be retried into "+
					"a well-formed one", got)
			}
		})
	}
}

// TestAnInstantaneousWindowIsLegal. Since == Until asks about one instant, which
// is a well-formed question with an almost always empty answer — and it is the
// window a poller produces on its very first tick if the queue's oldest contest
// was due to start exactly now.
func TestAnInstantaneousWindowIsLegal(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	if _, err := a.Results(context.Background(), provider.ResultWindow{
		Since: testNow, Until: testNow,
	}); err != nil {
		t.Errorf("a zero-length window was refused: %v", err)
	}
}

// TestResultsHonoursACancelledContext. Same rule as Fetch, same reason: the
// poller bounds one whole tick, and an adapter that ignored the deadline would
// let one call outlive the tick that owns it.
func TestResultsHonoursACancelledContext(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := a.Results(ctx, wholeWindow(testNow)); err == nil {
		t.Fatal("a cancelled context was ignored")
	}
}

// TestEveryResultIsEndedWithAScore. The model has no abandonment process —
// nothing in it postpones or cancels a fixture — so a cancellation from this
// adapter would be inventing an event that did not happen in the simulation.
// The cancellation SHAPE is still real and is covered by the settlement tier;
// what is asserted here is that this generator never manufactures one.
func TestEveryResultIsEndedWithAScore(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, testSeed, testNow)
	got := results(t, a, wholeWindow(testNow))
	if len(got) == 0 {
		t.Fatal("no contest in the whole lookback window was resulted")
	}
	for _, r := range got {
		if r.Status != domain.EventStatusEnded || !r.HasScore {
			t.Errorf("result %s is %s (score: %t), want ended with a score",
				r.EventKey, r.Status, r.HasScore)
		}
		if r.Score.Home() < 0 || r.Score.Away() < 0 {
			t.Errorf("result %s carries a negative score %s", r.EventKey, r.Score)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("the generator produced a result the domain refuses: %v", err)
		}
	}
}
