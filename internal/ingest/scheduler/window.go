package scheduler

import (
	"errors"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// ErrUnknownWindow is returned by [ParseWindow] for a value outside the defined
// set. Callers match it with errors.Is.
var ErrUnknownWindow = errors.New("scheduler: unknown polling window")

// Window is an event's relationship to now, expressed as the polling tier it
// belongs in.
//
// It is the "high frequency for live and near-tip events, low for futures"
// clause of CLAUDE.md §5, made into a closed type so that a cadence decision is
// a switch over a finite set rather than a chain of ad-hoc duration
// comparisons scattered across the package.
//
// The zero value is invalid, matching every enum in internal/domain. That is
// not cosmetic: WindowUnknown is what an unclassified event would silently
// become, and giving it a plausible cadence would mean a bug polls at some
// arbitrary rate instead of failing.
type Window uint8

const (
	// WindowUnknown is the invalid zero value.
	WindowUnknown Window = iota

	// WindowLive is an event in play, halted included. The fastest tier and,
	// per ADR 0003, 81% of the credit budget.
	WindowLive

	// WindowNearTip is an event that has not started but is inside the
	// near-tip boundary — the last half hour before kickoff, when the line
	// moves fastest outside live play.
	WindowNearTip

	// WindowToday is an event starting later in the day. ADR 0003's "pregame"
	// window.
	WindowToday

	// WindowDistant is an event several days out. ADR 0003's "distant" window.
	WindowDistant

	// WindowFutures is an outright, or a match so far out that it is
	// effectively one. Moves on news, not on a clock.
	WindowFutures
)

// String implements fmt.Stringer. These lowercase forms are the values written
// to the `window` Prometheus label, so they are a contract with
// deploy/observability: sharpline_ingest_poll_interval_seconds{window="live"}
// is selected by name in the SLO-objective recording rule and by the
// OddsPollCadenceUnknown alert.
func (w Window) String() string {
	switch w {
	case WindowLive:
		return "live"
	case WindowNearTip:
		return "near_tip"
	case WindowToday:
		return "today"
	case WindowDistant:
		return "distant"
	case WindowFutures:
		return "futures"
	default:
		return "unknown"
	}
}

// Valid reports whether w is a defined window. WindowUnknown is not valid.
func (w Window) Valid() bool {
	return w >= WindowLive && w <= WindowFutures
}

// MoreUrgentThan reports whether w is polled more often than other.
//
// The ordinal order IS the urgency order, which is why the constants are
// declared live-first. Folding a league's events onto one window is a min over
// this relation ([FoldWindows]).
func (w Window) MoreUrgentThan(other Window) bool {
	if !other.Valid() {
		return w.Valid()
	}
	return w.Valid() && w < other
}

// ParseWindow is the inverse of String for the defined windows. Matching is
// exact, as in internal/domain: this reads back values this package wrote.
func ParseWindow(s string) (Window, error) {
	for _, w := range Windows() {
		if w.String() == s {
			return w, nil
		}
	}
	return WindowUnknown, fmt.Errorf("%w: %q", ErrUnknownWindow, s)
}

// Windows returns every valid window in urgency order, fastest first.
//
// It returns a fresh slice on every call rather than exposing a package-level
// one: a package-level slice is global mutable state, which CLAUDE.md §12
// forbids, and a caller that sorted it in place would silently reorder the
// metric label set for everyone else.
func Windows() []Window {
	return []Window{WindowLive, WindowNearTip, WindowToday, WindowDistant, WindowFutures}
}

// Boundaries are the wall-clock cutoffs between the non-live windows.
//
// Live is decided by status, not by a boundary — an event that is in play is
// live whatever the clock says about its advertised start, which matters
// because a delayed kickoff would otherwise be classified as "today" while the
// ball is already in the air.
type Boundaries struct {
	// NearTip is how long before the scheduled start an event enters the
	// near-tip window.
	NearTip time.Duration
	// Today is how far ahead the "today" window reaches.
	Today time.Duration
	// Distant is how far ahead the "distant" window reaches. Beyond it, an
	// event is treated as a future.
	Distant time.Duration
}

// DefaultBoundaries are the cutoffs ADR 0003's assumptions table implies.
//
// Today is 12h rather than "midnight local" deliberately: "today" in a
// multi-timezone slate is not a well-defined instant, and a rolling horizon
// cannot produce the bug where every event's cadence changes simultaneously at
// one moment of the day.
func DefaultBoundaries() Boundaries {
	return Boundaries{
		NearTip: 30 * time.Minute,
		Today:   12 * time.Hour,
		Distant: 7 * 24 * time.Hour,
	}
}

// Validate reports whether the boundaries are usable: each strictly inside the
// next, and all positive.
func (b Boundaries) Validate() error {
	switch {
	case b.NearTip <= 0:
		return fmt.Errorf("%w: Boundaries.NearTip must be positive, got %s", ErrInvalidConfig, b.NearTip)
	case b.Today <= b.NearTip:
		return fmt.Errorf("%w: Boundaries.Today (%s) must exceed NearTip (%s)", ErrInvalidConfig, b.Today, b.NearTip)
	case b.Distant <= b.Today:
		return fmt.Errorf("%w: Boundaries.Distant (%s) must exceed Today (%s)", ErrInvalidConfig, b.Distant, b.Today)
	}
	return nil
}

// ClassifyEvent reports which polling window e belongs in at now.
//
// The order of the checks is the whole specification, so it is worth stating
// plainly:
//
//  1. In play, HALTED INCLUDED, is live. Status wins over the clock here — see
//     [Boundaries] — and it wins over rule 2 as well, which is the subtle part.
//
//     "May a wager be placed on this?" and "is this worth spending a credit on?"
//     are DIFFERENT QUESTIONS, and domain.EventStatusSuspended is where they
//     diverge: a halted event refuses wagers (domain.EventStatus.AcceptsWagers
//     returns false for it) and is nonetheless the single most valuable thing on
//     the board to poll, because the reopen and the repriced line arrive
//     together and there is no other way to learn about either. Testing
//     AcceptsWagers first — which this function used to do — made a rain delay
//     drop out of its tier and, if it was the only fixture in the league, fall
//     all the way to Config.DiscoveryWindow, so play could resume an hour before
//     anything noticed. That is why in-play is checked first.
//
//  2. An event whose status is otherwise not wagerable is NOT POLLABLE. Ended,
//     settled, postponed and cancelled events have no market that will ever
//     reopen, so spending a credit on them is spending it on nothing. They
//     return WindowUnknown, and [FoldWindows] drops them.
//
//  3. An outright is a future regardless of when its notional "start" is. A
//     season winner market does not have a kickoff.
//
//  4. An event that should already have started but whose status still says
//     scheduled is treated as near-tip, not as distant. This is the delayed- or
//     late-status case, and it is the one place where being wrong is expensive:
//     the provider is about to flip it to live and the line is moving now.
//
//  5. Otherwise, time to start against the boundaries.
func ClassifyEvent(e domain.Event, now time.Time, b Boundaries) Window {
	if e.IsInPlay() {
		return WindowLive
	}
	if !e.AcceptsWagers() {
		return WindowUnknown
	}
	if e.Kind() == domain.EventKindOutright {
		return WindowFutures
	}

	ttl := e.TimeToStart(now)
	switch {
	case ttl <= b.NearTip:
		// Covers the negative case too: an event past its advertised start
		// that has not been flipped to live yet.
		return WindowNearTip
	case ttl <= b.Today:
		return WindowToday
	case ttl <= b.Distant:
		return WindowDistant
	default:
		return WindowFutures
	}
}

// FoldWindows reduces a league's events to the one window its sweep runs at:
// the most urgent window among the pollable events.
//
// It returns (WindowUnknown, false) when no event in the slice is pollable,
// which is how a league with nothing but settled fixtures stops being polled
// at all rather than being polled slowly for ever.
//
// Folding to the MINIMUM is what makes per-league scheduling correct rather
// than merely cheap: one league sweep returns every event in the league, so
// polling at the pace of the most urgent one gives every other event in the
// payload a free upgrade. Averaging, or taking the modal window, would
// under-poll the game that is actually in play.
func FoldWindows(events []domain.Event, now time.Time, b Boundaries) (Window, bool) {
	best := WindowUnknown
	for _, e := range events {
		w := ClassifyEvent(e, now, b)
		if !w.Valid() {
			continue
		}
		if !best.Valid() || w.MoreUrgentThan(best) {
			best = w
		}
	}
	return best, best.Valid()
}
