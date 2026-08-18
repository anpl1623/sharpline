package synthetic

import (
	"fmt"
	"math"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// The market model: from two latent numbers to every price on a contest.
//
// # One process, many markets
//
// A contest carries exactly two latent processes — the expected home margin μ
// and the expected combined total τ — plus one per player prop. Every quoted
// probability on the event is derived from those by the normal model a trader
// would use:
//
//	P(home wins)      = Φ(μ / σ)
//	P(home covers L)  = Φ((μ + L) / σ)
//	P(over T)         = 1 − Φ((T − τ) / σ_total)
//
// Deriving them rather than walking three independent prices is what makes the
// moneyline, the spread and the total on one event MUTUALLY CONSISTENT. Three
// independent walks would routinely produce a −7 favourite at +140 on the
// moneyline, which is not a line, it is an arbitrage against arithmetic; phase
// 9's +EV finder would report it as a signal and be right to.
//
// # In play, the remaining game is what is priced
//
// Once a contest starts, the quantity a market is about is the FINAL result, and
// part of that result has already happened. So the model prices the remainder:
// the expected final margin is the current lead plus μ·(1−f), with dispersion
// σ·√(1−f), where f is the fraction of the contest played. Two properties follow
// and both are visible on a board: an in-play line tightens as the clock runs,
// and the score the event carries can never disagree with the price on it,
// because the price is computed FROM the score.

// Live-pricing bounds.
const (
	// maxPricedFraction caps f. At f = 1 the remaining dispersion is zero, every
	// probability is 0 or 1, and there is no price. Real books close the market
	// before that point; this is the model's equivalent, and it keeps the last
	// minutes quotable rather than degenerate.
	maxPricedFraction = 0.95

	// bookBiasScale converts a book's biasSD into the process's own units.
	//
	// bookDef documents the bias as an opinion "in units of the league's
	// expected margin". It is applied here as a multiple of the process's
	// STATIONARY LINE DISPERSION instead, because the same knob has to price a
	// basketball spread, a hockey total and a passing-yards prop, and only the
	// line dispersion is defined for all three.
	//
	// The value is a modelling judgement and worth stating as one. It puts the
	// mean no-vig disagreement across the five books at roughly four percentage
	// points, which is WIDER than a real market — real books sit within one or
	// two — and deliberately so: CLAUDE.md §6 makes arbitrage, middles and +EV
	// the differentiating surface, and a feed tight enough to be realistic would
	// leave those screens empty on the offline path, which is the path a
	// reviewer sees. It is within an order of magnitude of a real soft book
	// rather than a caricature, and TestBooksDisagree measures it so the number
	// is a reported fact rather than a hope.
	bookBiasScale = 1.25
)

// eventState is one slate event evaluated at one model instant: its lifecycle,
// its score and clock, and every latent process it carries, at every book's
// view.
type eventState struct {
	ev  slateEvent
	now time.Time
	at  time.Time // the model instant, floor(now / step)
	n   int64     // the model step index

	status domain.EventStatus

	// frac is the fraction of the contest played, 0 before the start and 1 once
	// it has finished. priced is frac capped at maxPricedFraction.
	frac   float64
	priced float64

	score    domain.Score
	hasScore bool
	clock    domain.GameClock
	hasClock bool

	margin  path
	total   path
	props   []path
	runners []path
}

// processKey names one of an event's latent processes.
//
// The index is the one slateEvent.processCount and the process* constants
// define: 0 is the margin, 1 the total, and 2 onward the player props on a
// match, while on a futures event index r is runner r's strength. Keying by
// index rather than by, say, a runner's name is what keeps a process's identity
// tied to the event's own structure — the roster is fixed for a seed, so the
// index is stable, and a display string is not something a latent process should
// depend on.
func processKey(ev slateEvent, idx int) string {
	return fmt.Sprintf("proc:%s:%d", ev.id, idx)
}

// newEventState evaluates one event at step n.
func (a *Adapter) newEventState(sc *scratch, ev slateEvent, now, at time.Time, n int64) (*eventState, error) {
	es := &eventState{ev: ev, now: now, at: at, n: n, status: domain.EventStatusScheduled}

	procs := make([]path, ev.processCount())
	for i := range procs {
		procs[i] = a.evolve(sc, processKey(ev, i), n)
	}

	if ev.isFutures() {
		es.runners = procs
		return es, nil
	}

	elapsed := now.Sub(ev.start)
	switch {
	case elapsed < 0:
		es.status = domain.EventStatusScheduled
	case elapsed < liveDuration:
		es.status = domain.EventStatusLive
		es.frac = float64(elapsed) / float64(liveDuration)
	default:
		es.status = domain.EventStatusEnded
		es.frac = 1
	}
	es.priced = math.Min(es.frac, maxPricedFraction)

	es.margin = procs[processMargin]
	es.total = procs[processTotal]
	es.props = procs[processMatchProp:]

	if es.status.HasStarted() {
		home, away := a.scoreAt(ev, es.frac)
		s, err := domain.NewScore(home, away)
		if err != nil {
			return nil, fmt.Errorf("synthetic event %s: %w", ev.id, err)
		}
		es.score, es.hasScore = s, true
	}
	if es.status.IsInPlay() {
		c, err := a.clockAt(ev, es.frac)
		if err != nil {
			return nil, fmt.Errorf("synthetic event %s: %w", ev.id, err)
		}
		es.clock, es.hasClock = c, true
	}
	return es, nil
}

// scoreAt returns the score after a fraction f of the contest.
//
// It is built from the event's STATIC means and a static per-side pace draw,
// never from the moving latent process. That is what makes it monotone in f: a
// score derived from a wandering process would go down when the process did, and
// a board that un-scores a goal is worse than one that shows no score at all.
func (a *Adapter) scoreAt(ev slateEvent, f float64) (int, int) {
	if f <= 0 {
		return 0, 0
	}
	key := string(ev.id)
	paceHome := 1 + 0.12*normalAt(a.opts.Seed, "pace-home:"+key, 0)
	paceAway := 1 + 0.12*normalAt(a.opts.Seed, "pace-away:"+key, 0)

	home := f * math.Max(0, (ev.totalMean+ev.marginMean)/2) * math.Max(0, paceHome)
	away := f * math.Max(0, (ev.totalMean-ev.marginMean)/2) * math.Max(0, paceAway)
	return int(math.Round(home)), int(math.Round(away))
}

// clockAt returns the in-play clock after a fraction f of the contest.
//
// Periods are equal slices of liveDuration. That is a simplification — no real
// sport's periods are exactly equal once stoppages are counted — and it is the
// right one here: the clock exists so the board has something truthful to render
// and so domain.GameClock's in-play invariants are exercised, not so the
// simulation can claim to model a shot clock.
func (a *Adapter) clockAt(ev slateEvent, f float64) (domain.GameClock, error) {
	periods := ev.league.periods
	if periods < 1 {
		periods = 1
	}
	pos := clamp(f, 0, 1) * float64(periods)
	idx := int(math.Floor(pos))
	if idx >= periods {
		idx = periods - 1
	}
	within := pos - float64(idx)
	length := liveDuration / time.Duration(periods)
	return domain.NewGameClock(idx+1, time.Duration(within*float64(length)), true)
}

// -----------------------------------------------------------------------------
// Per-book views of the two match quantities
// -----------------------------------------------------------------------------

// bias returns book b's persistent opinion on one quantity of one event.
//
// It is a function of (seed, book, event, quantity) and of nothing else — no
// clock, no counter — so it is CONSTANT for the life of the event. That is what
// makes it disagreement rather than noise: a book that is two points high on
// this fixture stays two points high on it, which is what a real soft book looks
// like and what makes a +EV or arbitrage signal against it persist long enough
// to be actionable.
func (a *Adapter) bias(b bookDef, ev slateEvent, quantity string, lineSD float64) float64 {
	if b.biasSD == 0 {
		return 0
	}
	key := "bias:" + b.slug + ":" + quantity + ":" + string(ev.id)
	return b.biasSD * bookBiasScale * lineSD * normalAt(a.opts.Seed, key, 0)
}

// marginView returns the expected FINAL margin as book i sees it, and the
// dispersion of the remainder.
func (a *Adapter) marginView(es *eventState, i int) (mean, sd float64) {
	l := es.ev.league
	view := es.ev.marginMean + l.marginLineSD*es.margin.views[i]
	if i >= 0 && i < len(a.books) {
		view += a.bias(a.books[i], es.ev, "margin", l.marginLineSD)
	}
	lead := 0.0
	if es.hasScore {
		lead = float64(es.score.Margin())
	}
	rem := 1 - es.priced
	return lead + view*rem, l.resultSD * math.Sqrt(rem)
}

// trueMargin returns the expected final margin of the UNLAGGED, UNBIASED
// process. It is what the shared market line is set from, so that every book
// quotes the same handicap and their disagreement shows up in the price rather
// than in the line — which is what domain.Market's single Line field requires
// anyway, since one market carries one line.
func (a *Adapter) trueMargin(es *eventState) (mean, sd float64) {
	l := es.ev.league
	lead := 0.0
	if es.hasScore {
		lead = float64(es.score.Margin())
	}
	rem := 1 - es.priced
	return lead + (es.ev.marginMean+l.marginLineSD*es.margin.views[0])*rem, l.resultSD * math.Sqrt(rem)
}

// totalView returns the expected FINAL combined score as book i sees it.
func (a *Adapter) totalView(es *eventState, i int) (mean, sd float64) {
	l := es.ev.league
	view := es.ev.totalMean + l.totalLineSD*es.total.views[i]
	if i >= 0 && i < len(a.books) {
		view += a.bias(a.books[i], es.ev, "total", l.totalLineSD)
	}
	banked := 0.0
	if es.hasScore {
		banked = float64(es.score.Total())
	}
	rem := 1 - es.priced
	return banked + view*rem, l.totalResultSD * math.Sqrt(rem)
}

func (a *Adapter) trueTotal(es *eventState) (mean, sd float64) {
	l := es.ev.league
	banked := 0.0
	if es.hasScore {
		banked = float64(es.score.Total())
	}
	rem := 1 - es.priced
	return banked + (es.ev.totalMean+l.totalLineSD*es.total.views[0])*rem, l.totalResultSD * math.Sqrt(rem)
}

// propView returns the expected final value of prop p as book i sees it.
func (a *Adapter) propView(es *eventState, p propDef, pathIdx, i int) (mean, sd float64) {
	view := p.mean + p.lineSD*es.props[pathIdx].views[i]
	if i >= 0 && i < len(a.books) {
		view += a.bias(a.books[i], es.ev, fmt.Sprintf("prop%d", p.idx), p.lineSD)
	}
	rem := 1 - es.priced
	banked := p.mean * es.priced
	return banked + view*rem, p.resultSD * math.Sqrt(rem)
}

func (a *Adapter) trueProp(es *eventState, p propDef, pathIdx int) (mean, sd float64) {
	rem := 1 - es.priced
	return p.mean*es.priced + (p.mean+p.lineSD*es.props[pathIdx].views[0])*rem, p.resultSD * math.Sqrt(rem)
}

// -----------------------------------------------------------------------------
// Probability derivations
// -----------------------------------------------------------------------------

// phi is Φ with the domain's own implementation. The error cannot fire for a
// finite argument; it is checked rather than discarded because a NaN reaching a
// price is a rejected snapshot, and silently substituting 0.5 would be a wrong
// price rather than a loud failure.
func phi(x float64) (float64, error) {
	v, err := odds.NormalCDF(x)
	if err != nil {
		return 0, fmt.Errorf("synthetic: normal cdf: %w", err)
	}
	return v, nil
}

// moneylineProbs returns the two-way or three-way moneyline probabilities.
//
// The three-way case models the draw as the final margin landing exactly on
// zero, which for an integer-scored sport is the interval (−0.5, +0.5). Deriving
// it that way rather than assigning the draw a fixed share is what keeps the
// three probabilities summing to one EXACTLY and keeps the draw price moving
// with the fixture: a tight game has a fat draw, a mismatch has a thin one.
func moneylineProbs(mean, sd float64, threeWay bool) ([]float64, error) {
	if sd <= 0 {
		return nil, fmt.Errorf("synthetic: %w", odds.ErrNotFinite)
	}
	if !threeWay {
		p, err := phi(mean / sd)
		if err != nil {
			return nil, err
		}
		c := clampTwoSided(p)
		return []float64{c[0], c[1]}, nil
	}
	hi, err := phi((0.5 - mean) / sd)
	if err != nil {
		return nil, err
	}
	lo, err := phi((-0.5 - mean) / sd)
	if err != nil {
		return nil, err
	}
	out := []float64{1 - hi, hi - lo, lo} // home, draw, away
	clampField(out)
	return out, nil
}

// spreadProbs returns P(home covers) and P(away covers) at a home-perspective
// handicap of line.
//
// Home covers when margin + line > 0, so the probability is Φ((mean + line)/sd).
// Stating it from the home side and deriving the away side is the same
// home-perspective convention domain/market.go fixes, applied once here so no
// other call site has to remember which way the sign runs.
func spreadProbs(mean, sd, line float64) ([]float64, error) {
	if sd <= 0 {
		return nil, fmt.Errorf("synthetic: %w", odds.ErrNotFinite)
	}
	p, err := phi((mean + line) / sd)
	if err != nil {
		return nil, err
	}
	c := clampTwoSided(p)
	return []float64{c[0], c[1]}, nil
}

// thresholdProbs returns P(over) and P(under) at a threshold. It serves totals
// and player props alike, because they ask the identical question about
// different quantities.
func thresholdProbs(mean, sd, line float64) ([]float64, error) {
	if sd <= 0 {
		return nil, fmt.Errorf("synthetic: %w", odds.ErrNotFinite)
	}
	p, err := phi((mean - line) / sd)
	if err != nil {
		return nil, err
	}
	c := clampTwoSided(p)
	return []float64{c[0], c[1]}, nil
}

// outrightProbs turns a field of runner strengths into win probabilities.
//
// A softmax, which is the multinomial logit — the standard way to turn latent
// strengths into a normalised field, and the one that guarantees the
// probabilities sum to one however the strengths move. Subtracting the maximum
// before exponentiating is the usual overflow guard; without it a strong field
// produces +Inf/+Inf and every runner prices at NaN.
func outrightProbs(strength []float64) []float64 {
	out := make([]float64, len(strength))
	if len(strength) == 0 {
		return out
	}
	max := strength[0]
	for _, s := range strength[1:] {
		if s > max {
			max = s
		}
	}
	sum := 0.0
	for i, s := range strength {
		out[i] = math.Exp(s - max)
		sum += out[i]
	}
	for i := range out {
		out[i] /= sum
	}
	clampField(out)
	return out
}

// runnerStrength returns runner r's latent strength as book i sees it.
//
// The static component is drawn per (league, runner) so a club that is a
// favourite for the title stays one across restarts, and the moving component is
// the runner's own latent process — which is what lets a futures price drift on
// "news" rather than sitting frozen for a season.
func (a *Adapter) runnerStrength(es *eventState, r int, i int) float64 {
	name := es.ev.runners[r]
	base := normalAt(a.opts.Seed, "futures-base:"+string(es.ev.id)+":"+name, 0)
	s := base + futuresStrengthSD*es.runners[r].views[i]
	if i >= 0 && i < len(a.books) {
		s += a.bias(a.books[i], es.ev, "runner"+name, futuresStrengthSD)
	}
	return s
}

// futuresStrengthSD is how far a runner's latent strength wanders, in units of
// the field's own dispersion (the static strengths are standard normals). A
// third of a standard deviation is enough for a title price to move visibly over
// a week without the field reordering itself every afternoon.
const futuresStrengthSD = 0.35
