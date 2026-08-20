// The results endpoint: how a simulated contest's final score leaves this
// package.
//
// # This is the generator's own output, not a fixture
//
// The distinction doc.go draws for prices holds here without weakening, because
// the score is produced by THE SAME function the live model already uses.
// model.go's newEventState calls scoreAt(ev, frac) on every fetch of an event
// that has started; this file calls scoreAt(ev, 1) — the whole contest played —
// for an event whose contest has finished. There is no second scoring function,
// no stored answer and no table of finals: the score is computed on the call
// from the event's static means and a seeded per-side pace draw, which is the
// same latent structure that sets the total the market is priced against.
//
// The consequence worth stating is that the score and the line are two views of
// ONE simulated contest rather than two independent inventions. A market that
// closed with the home side a 6-point favourite settles a 6-point spread against
// a final that came from the same μ that set the 6. That property is what makes
// the settlement path demonstrable at all — grading a real pipeline's wagers
// against numbers no market ever priced would be worse than not grading them.
//
// # A window query, and why it is not asked per contest
//
// [Adapter.Results] is handed a span of finishing instants and answers with the
// contests that finished inside it, each named by the generator's OWN identifier
// — the same string its raw payloads carry, which is what the normalizer derives
// the domain identifier from. It is never handed one of those domain
// identifiers, and provider.ResultWindow carries the argument for why: the two
// spaces met in the old per-contest shape, the comparison could not match for
// any event ever, and the result was a settlement feed that reported every
// contest as unresolved for ever while looking healthy.
//
// It is also simply what a scores endpoint is. The Odds API's route is
// /v4/sports/{sport}/scores?daysFrom=3 — addressed by sport, bounded by a
// lookback, answering with what finished. Enumerating the grid over a window is
// the generator's faithful imitation of that, and it is the reason the outright
// declines itself structurally here rather than by a check: the enumeration
// walks the MATCH grid, and the season-title competition is not on it.
//
// # Why the results window is longer than the board's grace period
//
// buildSlate retires a contest endedGrace (45 minutes) after the final whistle,
// which is right for the ODDS board: a book takes its prices down and the
// fixture leaves the screen. A scores endpoint is a different endpoint with a
// different window — The Odds API's /scores route takes a `daysFrom` of up to
// three — and modelling it as three days rather than as 45 minutes is both
// faithful to the real thing and load-bearing here. With a 45-minute window, an
// ingest process that was down over a deploy, a migration or a laptop lid would
// permanently strand every contest that finished during the gap, and the stakes
// riding on them would sit in escrow with nothing left in the system able to
// release them.
//
// So this file does not consult the current board slate. It re-lays the fixture
// grid over the days the window reaches, from the generator's own construction
// path, and answers for any contest that finished inside both the window and
// [resultsLookback]. Older than that and the answer is silence, which is exactly
// what a real scores endpoint returns past its own window.
package synthetic

import (
	"context"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// resultsLookback is how long after the final whistle this adapter will still
// state a contest's outcome.
//
// Three days, matching the deepest lookback the real candidate provider's scores
// route offers. It is deliberately far wider than endedGrace — see the file
// comment for why a 45-minute results window would strand a customer's stake
// across an ordinary deploy — and it is finite rather than unbounded because an
// endpoint that answers for every contest ever played is not a thing any
// provider sells, and pretending otherwise would hide the operational problem
// that a multi-day ingest outage genuinely creates.
//
// It also BOUNDS THE WORK. The caller's window is its own; clamping it here is
// what stops a single stranded row on the poller's queue — one whose scheduled
// start is a month old — from asking this adapter to re-lay a month of fixture
// grid on every tick.
const resultsLookback = 3 * 24 * time.Hour

// Compile-time proof that this package satisfies the results seam. It is
// separate from the provider.Adapter assertion in adapter.go because they are
// separate interfaces on purpose (see provider.ResultsProvider), and deleting
// one should not silently un-assert the other.
var _ provider.ResultsProvider = (*Adapter)(nil)

// Results implements provider.ResultsProvider: every contest this generator
// staged whose final whistle fell inside the window.
//
// The answer is ordered — league, then day, then slot — and that order is
// stable, so two calls with the same window and seed produce byte-identical
// slices rather than a permutation.
//
// # What is omitted, and why each omission is the right answer
//
//   - A contest that has not finished. The model's clock is the authority, not
//     the caller's window: the poller asks about a span that reaches up to its
//     own clock reading, and this is where "old enough to be over" is resolved
//     into a fact. The boundary is spelled the same way model.go's newEventState
//     spells it — finished at exactly start+liveDuration, not one step later —
//     so the score this returns is byte-identical to the one a fetch at that
//     instant would have carried.
//
//   - A contest that finished more than [resultsLookback] ago, even where the
//     caller asked about it. Past the window the endpoint has nothing to say.
//     The poller will keep the row on its work queue and its queue-depth gauge
//     will show it, which is the honest outcome: a permanently unresolvable
//     event is an operator's decision (CLAUDE.md §6's admin console has manual
//     settlement for exactly this), not something a generator should paper over
//     by inventing a late score.
//
//   - An outright. The season-title event carries no clock-driven end: its
//     notional start is futuresHorizonDays out and the model holds no "who
//     lifted the trophy" draw, only per-runner strengths that keep moving. There
//     is no instant at which this generator knows a champion, so it says so by
//     saying nothing — structurally, by walking only the match grid. A futures
//     ticket in this simulation does not settle, and that is a stated limit
//     rather than a silent one.
//
// # No cancellations
//
// Every result this adapter states is `ended` with a score. The model has no
// abandonment process — nothing in it postpones or cancels a fixture — so
// emitting a cancellation would be inventing an event that did not happen in the
// simulation, which is the one thing this package must not do. The cancellation
// shape is still real and still reachable: provider.FinalResult carries it, the
// poller writes it, queries/results.sql accepts it, and settlement voids on it.
// It is a real provider's to state, and it is covered end to end by the
// settlement tier's own tests.
func (a *Adapter) Results(ctx context.Context, window provider.ResultWindow) ([]provider.FinalResult, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, a.contextError("results", err)
	}
	if err := window.Validate(); err != nil {
		// Already wrapped in provider.ErrInvalidScope, so the classification and
		// the errors.Is chain both survive this wrap. Fatal: a malformed window
		// cannot become well formed by being asked again.
		return nil, &provider.Error{
			Op: "results", Provider: provider.NameSynthetic,
			Disposition: provider.DispositionFatal, Err: err,
		}
	}

	// One clock reading for the whole call, taken from the adapter's OWN clock
	// rather than accepted as a parameter. Both halves matter: a per-contest
	// reading would let two fixtures in one answer be judged against different
	// instants, and a caller-supplied instant would put the results path on a
	// different clock from Fetch, which is the one way the score and the price
	// could come to disagree about whether a contest is over.
	now := a.opts.Clock().UTC()
	served, empty := servedWindow(window, now)
	if empty {
		return nil, nil
	}

	var out []provider.FinalResult
	for _, l := range leagues() {
		for _, ev := range a.finishedIn(l, served, now) {
			if err := ctx.Err(); err != nil {
				return nil, a.contextError("results", err)
			}
			r, err := a.finalResult(ev)
			if err != nil {
				return nil, err
			}
			// The generator's own output is validated before it leaves, exactly
			// as Fetch validates its snapshot. The failure this catches is an
			// `ended` result with no score, which the events table would happily
			// store and which would then grade every spread on the contest
			// against a 0-0 zero value — a plausible wrong number rather than an
			// error, which is the worst kind.
			if err := r.Validate(); err != nil {
				return nil, a.badResult(ev.id, err)
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// servedWindow narrows the caller's window to the span this adapter is willing
// and able to answer over. The bool reports that nothing is left of it.
//
// Two clamps, each of which the caller cannot be trusted to have applied:
//
//   - The FUTURE end. A window may reach past this adapter's clock — the
//     poller's reading and the generator's are two clocks — and a contest cannot
//     have finished after the instant the generator is being asked at. Trusting
//     the caller here would report a final for a contest still in play.
//   - The PAST end. resultsLookback is this endpoint's own lookback, exactly as
//     `daysFrom` is a real one's, and it is what bounds the enumeration below.
//
// An emptied window is not an error. A poller whose oldest queued contest is
// older than the lookback asks a perfectly well-formed question this endpoint
// has nothing to say about, which is the same silence any contest past the
// window gets.
func servedWindow(w provider.ResultWindow, now time.Time) (provider.ResultWindow, bool) {
	served := provider.ResultWindow{Since: w.Since.UTC(), Until: w.Until.UTC()}
	if floor := now.Add(-resultsLookback); served.Since.Before(floor) {
		served.Since = floor
	}
	if served.Until.After(now) {
		served.Until = now
	}
	return served, served.Until.Before(served.Since)
}

// finalResult states the outcome of one finished contest.
//
// FinalisedAt is the instant play ended. It is a fixture fact — start plus the
// league's contest duration — and therefore exact, deterministic and stable
// across restarts and replays, which is what events.observed_at has to be.
//
// It is NOT the current model instant, and the difference is the whole point of
// the poller's lag histogram: (write instant − FinalisedAt) is how long a
// customer waited between the final whistle and their ticket becoming
// settleable. Stamping `now` here would make that number zero for ever, the same
// failure the provider package comment warns about for Price.ObservedAt.
//
// EventKey is the generator's OWN identifier, the same string its raw payloads
// carry, and never a domain identifier derived from it. The poller performs that
// derivation; see provider.ResultWindow.
func (a *Adapter) finalResult(ev slateEvent) (provider.FinalResult, error) {
	// f = 1: the whole contest played. This is model.go's scoreAt, unmodified
	// and uncopied, which is what makes the final agree with the score a fetch
	// at the whistle would have carried.
	home, away := a.scoreAt(ev, 1)
	score, err := domain.NewScore(home, away)
	if err != nil {
		return provider.FinalResult{}, a.badResult(ev.id, err)
	}
	return provider.FinalResult{
		EventKey:    string(ev.id),
		Status:      domain.EventStatusEnded,
		Score:       score,
		HasScore:    true,
		FinalisedAt: ev.start.Add(liveDuration),
	}, nil
}

// finishedIn re-lays the league's fixture grid across the days the window can
// reach and returns every contest whose final whistle fell inside it.
//
// # Why the grid is re-laid rather than read off the board
//
// buildSlate retires a fixture endedGrace after the whistle, so the contests
// this endpoint most needs to answer for — the ones stranded by an ingest outage
// — are exactly the ones the board no longer holds. Re-laying the grid is what
// makes the results window able to outlive the board's, which the file comment
// argues for at length.
//
// It is the generator's own construction path on both sides: the start comes
// from the same `base + slot·spacing + leagueOffset` arithmetic buildSlate uses,
// and the contest is produced by buildMatch with the arguments buildSlate would
// have called it with, so there is no second construction to drift. Everything
// scoreAt reads — marginMean, totalMean and the identifier — is derived from
// (seed, event id) alone, so it is stable even for a contest that has long since
// left the board.
//
// # The two-day underhang
//
// The day loop walks fixture-grid BASES, and a start can sit up to 24h plus the
// league's stagger past its base, so the earliest base that can produce a start
// inside the window is up to two calendar days before that start's day.
// Inverting the arithmetic to compute the base exactly is a trap — the stagger
// pushes a dense grid's last slots past midnight, so the day a start falls in is
// not the day its grid was built from — whereas overshooting by two days and
// filtering on the whistle is arithmetic-free and self-verifying. It costs at
// most two extra days of slots, and the window is already clamped to
// [resultsLookback], so the whole enumeration is bounded by five days of grid.
func (a *Adapter) finishedIn(l leagueDef, window provider.ResultWindow, now time.Time) []slateEvent {
	slots := a.opts.EventsPerLeaguePerDay
	spacing := time.Duration(int64(24*time.Hour) / int64(slots))
	offset := leagueOffset(l)

	first := dayStart(window.Since.Add(-liveDuration)).AddDate(0, 0, -2)
	last := dayStart(window.Until.Add(-liveDuration))

	var out []slateEvent
	for base := first; !base.After(last); base = base.AddDate(0, 0, 1) {
		for slot := 0; slot < slots; slot++ {
			start := base.Add(time.Duration(slot)*spacing + offset)
			// newEventState draws the boundary at the same instant — `elapsed <
			// liveDuration` is live, anything else is ended — so a contest is
			// never both quotable there and finished here.
			if !window.Covers(start.Add(liveDuration)) {
				continue
			}
			out = append(out, a.buildMatch(l, start, slot, now))
		}
	}
	return out
}

// badResult reports a result the generator produced and the domain refused.
//
// It is FATAL for the same reason [Adapter.internal] is: every error reachable
// from here is a violation of an invariant this package is supposed to maintain,
// and retrying cannot make a generator's bug intermittent. It wraps
// provider.ErrMalformedPayload rather than ErrInvalidSnapshot because a result
// is not a snapshot — that sentinel already means "decoded into something the
// domain refuses", which is exactly this, and it is what
// provider.FinalResult.Validate wraps at the other end of the same check.
func (a *Adapter) badResult(id domain.EventID, err error) error {
	return &provider.Error{
		Op:          "results",
		Provider:    provider.NameSynthetic,
		Disposition: provider.DispositionFatal,
		Err:         fmt.Errorf("%w: event %s: %w", provider.ErrMalformedPayload, id, err),
	}
}
