// The detector: observations in, findings out.
//
// Read doc.go first. It carries the argument for the unit, the window grid, the
// watermark, the lead/follower rule, the correlation statistic and the cooldown.
// This file is the code those arguments describe, and where a choice here looks
// arbitrary the reason is in that document rather than restated on every line.
package steam

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// -----------------------------------------------------------------------------
// Input
// -----------------------------------------------------------------------------

// Quote is one book's price on one selection at one PROVIDER instant.
//
// Implied is the raw implied probability, 1/d, with the book's margin still in
// it. doc.go argues why it is not devigged; the short form is that devigging
// needs a book's complete outcome set at one instant, and a book that has
// refreshed half its market is exactly the book whose lag carries the signal.
type Quote struct {
	Selection domain.SelectionID
	Book      domain.BookID
	Implied   odds.Probability

	// ObservedAt is the provider's own instant for this quote — the EVENT TIME
	// the whole detector is expressed in. It is never a receive time and never a
	// clock reading of ours; substituting one would make a replay produce
	// different windows from the original run.
	ObservedAt time.Time
}

// Update is every quote on one market from one pipeline record.
//
// It is a whole-market statement rather than a per-quote call because that is
// what the pipeline actually carries: a [pricing.ComputedMarket] describes one
// market at one instant with every book's quote on it, and splitting it into N
// calls would let the watermark advance halfway through one record's worth of
// evidence.
type Update struct {
	Market domain.MarketID

	// Anchor is the source record's own observation instant. It participates in
	// the watermark alongside the quotes' instants, so a record that carries no
	// usable quote still advances event time and lets a pending window close.
	// Without it, a market that stopped moving would hold its last window open
	// for ever.
	Anchor time.Time

	Quotes []Quote
}

// -----------------------------------------------------------------------------
// Output
// -----------------------------------------------------------------------------

// Direction is which way the selection's implied probability moved.
//
// It is a string rather than a bool or an enum because it lands verbatim in a
// TEXT column that migrations/00009 constrains to these two values, and because
// a bool named "shortened" reads as a claim about the price where the finding is
// about the probability. migrations/00009 additionally CHECKs
// (direction = 'shorten') = (delta_probability > 0), so the two cannot disagree.
type Direction string

// The two directions.
const (
	// DirectionShorten means the implied probability ROSE: the market thinks the
	// outcome is more likely and the price has shortened.
	DirectionShorten Direction = "shorten"

	// DirectionDrift means the implied probability FELL: the price has drifted
	// out. It is the bookmaking term and it is deliberately not "lengthen",
	// because "drift" is what a trader would search the board for.
	DirectionDrift Direction = "drift"
)

// Follower is one book that moved the same way as the lead, afterwards.
type Follower struct {
	Book domain.BookID

	// MovedAt is this book's move instant, inside the window by construction.
	// Lag is MovedAt − the lead's move instant and is never negative: a book that
	// moved before the lead would BE the lead.
	MovedAt time.Time
	Lag     time.Duration

	// Delta is this book's own signed window change in probability points, the
	// same sign as the lead's by construction.
	Delta float64
}

// Finding is one steam move.
//
// It is this package's own type rather than internal/analytics's wire document,
// and the direction of that dependency is the point: internal/analytics imports
// steam, so steam importing internal/analytics would be a cycle, and — more
// usefully — a detector that knew about Kafka topics and database columns would
// be a detector that could not be unit-tested without them. The caller maps this
// onto the wire shape, supplying the catalogue facts (market type, league) that
// a detector has no business knowing.
type Finding struct {
	Market    domain.MarketID
	Selection domain.SelectionID

	// WindowStart and WindowEnd bound the window, HALF-OPEN: [start, end).
	WindowStart time.Time
	WindowEnd   time.Time

	// Window and Hop are the grid this finding was produced on, carried because
	// a magnitude and a velocity are only comparable between findings produced on
	// the same grid.
	Window time.Duration
	Hop    time.Duration

	Direction Direction

	// Delta is the LEAD book's signed change over the window, in probability
	// points; Magnitude is its absolute value; Velocity is Delta divided by the
	// window length in minutes. Not an average across books — doc.go explains why
	// averaging understates a move that is still propagating.
	Delta     float64
	Magnitude float64
	Velocity  float64

	LeadBook    domain.BookID
	LeadMovedAt time.Time

	// Followers are ordered by Lag ascending, then by Book ascending. The order
	// is part of the contract: the column is JSONB and a database cannot enforce
	// the ordering of an array.
	Followers []Follower

	// ParticipatingBooks is len(Followers) + 1. Carried explicitly because
	// migrations/00009 CHECKs it against the array and a reader should be able to
	// see the redundancy rather than discover it.
	ParticipatingBooks int

	// Correlation is the mean signed agreement across every book with enough data
	// in the window, in [−1, 1]. See doc.go for what it does and does not
	// discriminate.
	Correlation float64

	// The thresholds this finding was produced under, so a stored population
	// spanning a re-tuning can still be separated.
	ThresholdMagnitude   float64
	ThresholdVelocity    float64
	ThresholdCorrelation float64
	MinFollowers         int
	MaxFollowerLag       time.Duration
}

// Stats is what one [Detector.Observe] did, for the metrics.
//
// Every counter here answers a question an operator will actually ask when the
// board is empty, which is the state this detector is in almost all the time and
// which is indistinguishable from a broken detector without them.
type Stats struct {
	// Quotes is how many observations were offered and Late how many were older
	// than the market's watermark and therefore dropped. A rising Late is the
	// signature of AllowedLateness being too small for the feed's own lag.
	Quotes int
	Late   int

	// Windows is how many windows were evaluated and Candidates how many
	// (window, selection) pairs had at least one book clearing the magnitude
	// floor. Findings is how many survived every gate.
	Windows    int
	Candidates int
	Findings   int

	// SuppressedByCooldown counts findings that cleared every threshold and were
	// dropped as a repeat of a move already reported. With Window/Hop = 3 this is
	// EXPECTED to be roughly twice Findings; a value near zero means the cooldown
	// is not doing its job or nothing is firing at all.
	SuppressedByCooldown int

	// SuppressedByThreshold counts candidates rejected by magnitude, velocity,
	// correlation or follower count. It is the number to look at when tuning.
	SuppressedByThreshold int

	// SkippedWindows counts windows the evaluation cursor jumped over because a
	// watermark advanced further in one step than MaxWindowsPerAdvance allows —
	// a restart, a replay, or a gap in the feed. Counted rather than absorbed,
	// because a silently skipped hour is an hour nobody investigates.
	SkippedWindows int64
}

func (s *Stats) add(o Stats) {
	s.Quotes += o.Quotes
	s.Late += o.Late
	s.Windows += o.Windows
	s.Candidates += o.Candidates
	s.Findings += o.Findings
	s.SuppressedByCooldown += o.SuppressedByCooldown
	s.SuppressedByThreshold += o.SuppressedByThreshold
	s.SkippedWindows += o.SkippedWindows
}

// -----------------------------------------------------------------------------
// State
// -----------------------------------------------------------------------------

// point is one retained observation. Two fields and no book identifier, because
// the series it lives in already fixes the (selection, book) pair.
type point struct {
	at time.Time
	p  float64
}

// seriesKey identifies one retained series inside a market.
//
// A flat comparable key rather than a nested map because every operation on it
// is a whole-key lookup, and a map of maps costs a second hash and an allocation
// per selection for no benefit. internal/platform/kafka's topicPartition makes
// the same choice for the same reason.
type seriesKey struct {
	selection domain.SelectionID
	book      domain.BookID
}

// cooldownKey identifies one suppression slot.
//
// The direction is part of it because a move out and a move back are two events:
// suppressing the second as a duplicate of the first would hide precisely the
// reversal a trader most wants to see.
type cooldownKey struct {
	selection domain.SelectionID
	direction Direction
}

// marketState is everything the detector remembers about one market.
type marketState struct {
	// watermark is MONOTONE NON-DECREASING. Once it has advanced it never
	// retreats, whatever order records arrive in, which is what makes a closed
	// window closed for ever rather than reopenable by a late record.
	watermark time.Time

	// cursor is the greatest window END already evaluated. Windows ending at or
	// before it are finished; windows ending after it and at or before the
	// watermark are due.
	//
	// Zero means "no window has been evaluated yet", which is distinguishable
	// from any real cursor because event time here comes from a provider clock
	// and migrations/00009 refuses an instant before 1900.
	cursor time.Time

	series   map[seriesKey][]point
	cooldown map[cooldownKey]time.Time
}

// Detector holds the windowed state for every market it has seen.
//
// IT IS NOT SAFE FOR CONCURRENT USE. It is driven from one consumer's handler
// goroutine, where records are delivered sequentially, and a mutex would buy
// nothing except a suggestion that a second caller is expected. The one
// concurrency rule that matters is stated rather than enforced: [Detector.Observe]
// and [Detector.Forget] must be called from the same goroutine.
type Detector struct {
	cfg Config

	// markets is the per-market state. It grows with the slate and shrinks with
	// Forget; there is no time-based eviction, because a market that stops
	// producing records also stops advancing its watermark and therefore stops
	// costing anything but its pruned buffer.
	markets map[domain.MarketID]*marketState
}

// New builds a detector. It does no I/O and starts nothing.
func New(cfg Config) (*Detector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Detector{
		cfg:     cfg.resolved(),
		markets: make(map[domain.MarketID]*marketState),
	}, nil
}

// Config returns the detector's configuration with defaults resolved, so a
// caller can report the thresholds a finding was produced under rather than
// assume them.
func (d *Detector) Config() Config { return d.cfg }

// Markets returns how many markets the detector holds state for. It exists for
// a gauge: compare it against the priced-market count, because a number that
// only grows means Forget is not being called and tombstones are being ignored.
func (d *Detector) Markets() int { return len(d.markets) }

// Forget releases a market's state.
//
// The caller invokes it when the market is tombstoned upstream. It is NOT a
// correctness requirement — a forgotten market's windows would simply never
// close again, because nothing advances its watermark — but it is a memory one:
// a slate rolls over daily and a detector that never forgot would accumulate a
// day's markets per day for as long as the process ran.
func (d *Detector) Forget(m domain.MarketID) { delete(d.markets, m) }

// Observe folds one market update into the detector and returns every finding
// whose window the update closed.
//
// # Ordering of the two halves is load-bearing
//
// The quotes are RECORDED FIRST and the watermark is advanced SECOND. The
// reverse order would let this record's own newest quote close a window that
// this record's other quotes belong in, so a market whose books all report at
// once would evaluate a window that was still being filled — and would do it
// differently depending on the order the quotes happened to sit in the slice.
//
// A nil result is the ordinary and correct state: most updates close no window,
// and most closed windows contain no steam.
func (d *Detector) Observe(u Update) ([]Finding, Stats) {
	var stats Stats

	if u.Market.IsZero() {
		return nil, stats
	}
	st := d.markets[u.Market]
	if st == nil {
		st = &marketState{
			series:   make(map[seriesKey][]point),
			cooldown: make(map[cooldownKey]time.Time),
		}
		d.markets[u.Market] = st
	}

	// 1. Record. A quote older than the watermark is LATE: the window it belongs
	//    to has already been evaluated (or is about to be, with an answer this
	//    record cannot be part of), and admitting it would make a window's result
	//    depend on when the reader asked.
	newest := u.Anchor
	for _, q := range u.Quotes {
		stats.Quotes++
		if q.ObservedAt.IsZero() || q.Selection.IsZero() || q.Book.IsZero() {
			continue
		}
		if !finite(float64(q.Implied)) || q.Implied <= 0 || q.Implied >= 1 {
			continue
		}
		if !st.watermark.IsZero() && q.ObservedAt.Before(st.watermark) {
			stats.Late++
			continue
		}
		st.record(seriesKey{selection: q.Selection, book: q.Book}, point{
			at: q.ObservedAt,
			p:  float64(q.Implied),
		}, d.cfg.MaxSamplesPerSeries)
		if q.ObservedAt.After(newest) {
			newest = q.ObservedAt
		}
	}

	// 2. Advance. The watermark trails the newest event time by AllowedLateness
	//    and never retreats.
	if newest.IsZero() {
		return nil, stats
	}
	if w := newest.Add(-d.cfg.AllowedLateness); w.After(st.watermark) {
		st.watermark = w
	}

	findings, wstats := d.advance(u.Market, st)
	stats.add(wstats)
	return findings, stats
}

// record appends one observation to a series, keeping it sorted by instant and
// bounded in length.
//
// A repeat of an instant already held REPLACES it rather than appending. The
// pipeline can legitimately redeliver a record — a rebalance, a retry — and two
// points at one instant would make the "first" and "last" of a window depend on
// which copy landed second. migrations/00009 makes the same statement at the
// storage layer with an upsert on the natural key.
//
// The common case is an append at the end, because observations arrive in
// roughly event-time order; the binary search costs nothing on that path and
// makes the out-of-order path correct rather than merely unlikely.
func (st *marketState) record(k seriesKey, pt point, maxLen int) {
	s := st.series[k]

	i, found := slices.BinarySearchFunc(s, pt, func(a, b point) int { return a.at.Compare(b.at) })
	switch {
	case found:
		s[i] = pt
	case i == len(s):
		s = append(s, pt)
	default:
		s = slices.Insert(s, i, pt)
	}

	// Drop from the FRONT when the bound is reached: the oldest observation is
	// the one no future window can still need.
	if n := len(s) - maxLen; n > 0 {
		s = append(s[:0], s[n:]...)
	}
	st.series[k] = s
}

// -----------------------------------------------------------------------------
// Window evaluation
// -----------------------------------------------------------------------------

// advance evaluates every window this market's watermark has closed since the
// last call, oldest first.
//
// Oldest first is required rather than convenient: the cooldown is stateful, so
// evaluating a later window before an earlier one would suppress the wrong one
// of the pair and the answer would depend on the traversal order.
func (d *Detector) advance(market domain.MarketID, st *marketState) ([]Finding, Stats) {
	var (
		out   []Finding
		stats Stats
	)

	w := st.watermark
	if w.IsZero() {
		return nil, stats
	}

	// The newest window end at or before the watermark. Every window ending after
	// it is still open.
	last := windowEndAtOrBefore(w, d.cfg.Window, d.cfg.Hop)
	if last.IsZero() {
		return nil, stats
	}

	// A cold market starts AT the current window rather than at the beginning of
	// time. Without this, the first record for a market would try to evaluate
	// every window since the Unix epoch.
	if st.cursor.IsZero() {
		st.cursor = last.Add(-d.cfg.Hop)
	}
	if !last.After(st.cursor) {
		return nil, stats
	}

	due := int(last.Sub(st.cursor) / d.cfg.Hop)
	if due > d.cfg.MaxWindowsPerAdvance {
		// The watermark jumped — a restart, a replay, or a gap in the feed. The
		// windows in between hold nothing, because the buffer that would have fed
		// them has been pruned or was never filled, so evaluating them would be a
		// stall inside a Kafka handler for no output. Skip to the newest and SAY
		// SO: a silently absorbed gap is a gap nobody investigates.
		stats.SkippedWindows = int64(due - d.cfg.MaxWindowsPerAdvance)
		st.cursor = last.Add(-time.Duration(d.cfg.MaxWindowsPerAdvance) * d.cfg.Hop)
	}

	for end := st.cursor.Add(d.cfg.Hop); !end.After(last); end = end.Add(d.cfg.Hop) {
		stats.Windows++
		f, s := d.evaluate(market, st, end.Add(-d.cfg.Window), end)
		stats.add(s)
		out = append(out, f...)
	}
	st.cursor = last

	st.prune(st.cursor.Add(-d.cfg.Window), d.cfg.Cooldown)
	return out, stats
}

// evaluate runs the detector over one closed window, across every selection.
//
// Selections are visited in sorted order so that the findings a window produces
// are in a defined sequence rather than in Go's randomised map order. Nothing
// downstream depends on it — every finding is keyed independently — but a
// nondeterministic output ordering is exactly the kind of thing that makes a
// phase-12 diff impossible to read.
func (d *Detector) evaluate(
	market domain.MarketID, st *marketState, start, end time.Time,
) ([]Finding, Stats) {
	var (
		out   []Finding
		stats Stats
	)

	for _, sel := range st.selections() {
		moves := st.movesIn(sel, start, end)
		if len(moves) == 0 {
			continue
		}
		f, ok, reason := d.assess(market, sel, start, end, moves)
		if reason != assessNoCandidate {
			stats.Candidates++
		}
		switch {
		case ok:
			// The cooldown is applied LAST, after every threshold, so that
			// SuppressedByCooldown counts genuine repeats rather than being
			// polluted by findings that would have failed anyway.
			k := cooldownKey{selection: sel, direction: f.Direction}
			if prev, held := st.cooldown[k]; held && end.Sub(prev) < d.cfg.Cooldown {
				stats.SuppressedByCooldown++
				continue
			}
			st.cooldown[k] = end
			stats.Findings++
			out = append(out, f)
		case reason == assessBelowThreshold:
			stats.SuppressedByThreshold++
		}
	}
	return out, stats
}

// assessReason says why one (window, selection) did or did not yield a finding.
type assessReason int

const (
	// assessSignal: every gate cleared.
	assessSignal assessReason = iota

	// assessNoCandidate: no book moved far enough to be considered the lead, so
	// there was nothing to assess. The ordinary outcome, and deliberately NOT
	// counted as a suppression: a quiet market is not a rejected signal, and
	// conflating the two would make the suppression counter meaningless.
	assessNoCandidate

	// assessBelowThreshold: a lead existed and the finding failed the velocity,
	// correlation or follower gate.
	assessBelowThreshold
)

// bookMove is one book's behaviour inside one window.
type bookMove struct {
	book    domain.BookID
	delta   float64
	movedAt time.Time
}

// assess is the detector proper: the lead/follower rule, the correlation
// statistic and the thresholds, over one selection's books in one window.
//
// It is a pure function of `moves` — no state, no clock — which is what makes
// the semantics testable in isolation and reproducible in SQL.
func (d *Detector) assess(
	market domain.MarketID, sel domain.SelectionID, start, end time.Time, moves []bookMove,
) (Finding, bool, assessReason) {
	// The lead is the EARLIEST mover among the books that moved far enough to be
	// a lead at all. Ties are broken by the greater absolute move and then by the
	// lexicographically smaller book identifier, which makes the choice TOTAL:
	// two books cannot tie on all three, because a book appears at most once per
	// (selection, window).
	var (
		lead  bookMove
		found bool
	)
	for _, m := range moves {
		if math.Abs(m.delta) < d.cfg.MinMagnitude {
			continue
		}
		if !found || betterLead(m, lead) {
			lead, found = m, true
		}
	}
	if !found {
		return Finding{}, false, assessNoCandidate
	}

	dir := DirectionShorten
	if lead.delta < 0 {
		dir = DirectionDrift
	}
	leadSign := sign(lead.delta)

	// Followers: same direction, moved at or after the lead, inside the lag
	// bound. A lag of exactly zero is legal and common — books that share a view
	// of one latent process reprice on the same event-time grid, and simultaneity
	// is corroboration rather than a disqualification.
	var followers []Follower
	agree, counted := 0.0, 0
	for _, m := range moves {
		// The correlation statistic counts EVERY book with enough data in the
		// window — that is what makes it a mean rather than a ratio over the
		// movers — and a book whose move is inside the noise floor contributes
		// zero rather than a sign. Without the floor, a book that did not move at
		// all still has a sign, because the last unit in the last place of a
		// float64 is never exactly zero after a tick conversion, and the
		// correlation would be a coin flip weighted by rounding.
		counted++
		switch {
		case math.Abs(m.delta) < d.cfg.NoiseFloor:
			// Present and unmoved: contributes nothing either way.
		case sign(m.delta) == leadSign:
			agree++
		default:
			agree--
		}

		if m.book == lead.book {
			continue
		}
		if sign(m.delta) != leadSign || math.Abs(m.delta) < d.cfg.MinMagnitude {
			continue
		}
		lag := m.movedAt.Sub(lead.movedAt)
		if lag < 0 || lag > d.cfg.MaxFollowerLag {
			continue
		}
		followers = append(followers, Follower{
			Book:    m.book,
			MovedAt: m.movedAt,
			Lag:     lag,
			Delta:   m.delta,
		})
	}

	// Ordered by lag ascending, then by book ascending. The order is part of the
	// contract because the storage column is JSONB and a database cannot enforce
	// the ordering of an array.
	slices.SortFunc(followers, func(a, b Follower) int {
		if c := cmp.Compare(a.Lag, b.Lag); c != 0 {
			return c
		}
		return cmp.Compare(a.Book, b.Book)
	})

	correlation := 0.0
	if counted > 0 {
		correlation = agree / float64(counted)
	}
	velocity := lead.delta / d.cfg.Window.Minutes()
	magnitude := math.Abs(lead.delta)

	if len(followers) < d.cfg.MinFollowers ||
		math.Abs(velocity) < d.cfg.MinVelocity ||
		correlation < d.cfg.MinCorrelation {
		return Finding{}, false, assessBelowThreshold
	}

	return Finding{
		Market:               market,
		Selection:            sel,
		WindowStart:          start,
		WindowEnd:            end,
		Window:               d.cfg.Window,
		Hop:                  d.cfg.Hop,
		Direction:            dir,
		Delta:                lead.delta,
		Magnitude:            magnitude,
		Velocity:             velocity,
		LeadBook:             lead.book,
		LeadMovedAt:          lead.movedAt,
		Followers:            followers,
		ParticipatingBooks:   len(followers) + 1,
		Correlation:          correlation,
		ThresholdMagnitude:   d.cfg.MinMagnitude,
		ThresholdVelocity:    d.cfg.MinVelocity,
		ThresholdCorrelation: d.cfg.MinCorrelation,
		MinFollowers:         d.cfg.MinFollowers,
		MaxFollowerLag:       d.cfg.MaxFollowerLag,
	}, true, assessSignal
}

// betterLead reports whether a should displace b as the lead, under the total
// order the file comment states: earliest move instant, then greater absolute
// delta, then smaller book identifier.
func betterLead(a, b bookMove) bool {
	if c := a.movedAt.Compare(b.movedAt); c != 0 {
		return c < 0
	}
	if c := cmp.Compare(math.Abs(b.delta), math.Abs(a.delta)); c != 0 {
		return c < 0
	}
	return a.book < b.book
}

// sign returns −1, 0 or +1. Written out rather than reached for through a
// library because the zero case has to be a distinct answer: a book that did not
// move must not be counted as agreeing with anybody.
func sign(v float64) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}

// -----------------------------------------------------------------------------
// Series arithmetic
// -----------------------------------------------------------------------------

// selections returns the market's selections in sorted order, so a window's
// findings come out in a defined sequence rather than in Go's randomised map
// order.
func (st *marketState) selections() []domain.SelectionID {
	seen := make(map[domain.SelectionID]struct{}, len(st.series))
	out := make([]domain.SelectionID, 0, len(st.series))
	for k := range st.series {
		if _, dup := seen[k.selection]; dup {
			continue
		}
		seen[k.selection] = struct{}{}
		out = append(out, k.selection)
	}
	slices.Sort(out)
	return out
}

// movesIn computes each book's behaviour for one selection inside the half-open
// window [start, end).
//
// Books are returned in sorted order for determinism, and a book with fewer than
// two observations inside the window is omitted entirely: there is no delta to
// measure, and treating a single observation as a zero move would let a book
// that simply had not reported drag the correlation statistic toward zero.
func (st *marketState) movesIn(sel domain.SelectionID, start, end time.Time) []bookMove {
	var out []bookMove
	for k, s := range st.series {
		if k.selection != sel {
			continue
		}
		m, ok := moveIn(s, start, end)
		if !ok {
			continue
		}
		m.book = k.book
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b bookMove) int { return cmp.Compare(a.book, b.book) })
	return out
}

// moveIn computes one series' delta and move instant inside [start, end).
//
// Δ is FIRST TO LAST, not a sum of absolute steps: the finding is about where
// the market ended up, and a book that moved out and back has not steamed.
//
// The move instant is the END of the largest single step IN THE DIRECTION OF Δ,
// with ties broken by the earliest such instant. Two properties make it the
// right statistic rather than, say, the window's last instant: it is where the
// move actually happened, which is what separates a lead from a follower; and it
// is an argmax over a `lag` window function in SQL, which keeps the phase-12
// reproduction a query rather than a program.
func moveIn(s []point, start, end time.Time) (bookMove, bool) {
	lo, _ := slices.BinarySearchFunc(s, point{at: start}, func(a, b point) int { return a.at.Compare(b.at) })
	hi, _ := slices.BinarySearchFunc(s, point{at: end}, func(a, b point) int { return a.at.Compare(b.at) })
	w := s[lo:hi]
	if len(w) < 2 {
		return bookMove{}, false
	}

	delta := w[len(w)-1].p - w[0].p
	if delta == 0 {
		// A book that ended where it started has no direction, so it cannot lead
		// and cannot follow. It is still reported, with a zero delta, so that the
		// correlation statistic counts it as present-and-unmoved rather than
		// absent — the two are different facts about the window.
		return bookMove{delta: 0, movedAt: w[len(w)-1].at}, true
	}

	dir := float64(sign(delta))
	best, bestAt := math.Inf(-1), w[len(w)-1].at
	for i := 1; i < len(w); i++ {
		step := (w[i].p - w[i-1].p) * dir
		if step > best {
			best, bestAt = step, w[i].at
		}
	}
	return bookMove{delta: delta, movedAt: bestAt}, true
}

// prune drops everything no future window can still need.
//
// The oldest window that can still be evaluated starts at the evaluation
// cursor's own start, so any observation before that is unreachable. Cooldown
// slots older than the cooldown itself are dropped for the same reason: they can
// no longer suppress anything, and a map that only grew would be a per-selection
// leak on a slate that rolls over daily.
func (st *marketState) prune(before time.Time, cooldown time.Duration) {
	for k, s := range st.series {
		i, _ := slices.BinarySearchFunc(s, point{at: before}, func(a, b point) int { return a.at.Compare(b.at) })
		switch {
		case i >= len(s):
			// Every point is older than the horizon. The series is kept rather
			// than deleted only when it still holds its newest point, which a
			// later window may need as a window's opening observation; here it
			// does not, so the key goes with it.
			delete(st.series, k)
		case i > 0:
			st.series[k] = append(s[:0], s[i:]...)
		}
	}
	for k, at := range st.cooldown {
		if before.Sub(at) > cooldown {
			delete(st.cooldown, k)
		}
	}
}

// -----------------------------------------------------------------------------
// The window grid
// -----------------------------------------------------------------------------

// windowEndAtOrBefore returns the greatest window END at or before t, on the
// epoch-aligned hopping grid.
//
// # The grid, stated exactly, because phase 12 has to land on the same one
//
// Window k is [k·hop, k·hop + window) counted in NANOSECONDS FROM THE UNIX
// EPOCH IN UTC. So window ends are at k·hop + window, and the greatest one at or
// before t is
//
//	k    = floor((t − window) / hop)
//	end  = k·hop + window
//
// The division floors toward NEGATIVE INFINITY rather than toward zero. Go's `/`
// truncates toward zero, which for an instant before 1970 would put it in the
// window AFTER the one it belongs to — a bug that only appears on the wrong side
// of the epoch and therefore survives every test written against today's date.
// The synthetic generator's own floorDiv carries the same warning for the same
// reason.
func windowEndAtOrBefore(t time.Time, window, hop time.Duration) time.Time {
	if window <= 0 || hop <= 0 {
		return time.Time{}
	}
	k := floorDiv(t.UnixNano()-int64(window), int64(hop))
	return time.Unix(0, k*int64(hop)+int64(window)).UTC()
}

// floorDiv divides rounding toward negative infinity. See windowEndAtOrBefore.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// String renders a finding for a log line. It leads with the market, the
// selection and the direction because those are what an operator greps for.
func (f Finding) String() string {
	return fmt.Sprintf("steam %s/%s %s %.4f pts (%.4f/min) lead=%s followers=%d corr=%.2f window=[%s, %s)",
		f.Market, f.Selection, f.Direction, f.Magnitude, f.Velocity,
		f.LeadBook, len(f.Followers), f.Correlation,
		f.WindowStart.UTC().Format(time.RFC3339), f.WindowEnd.UTC().Format(time.RFC3339))
}
