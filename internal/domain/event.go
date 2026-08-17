package domain

import (
	"fmt"
	"time"
)

// Clock sanity bounds. They exist to catch a unit error in a provider adapter —
// milliseconds read as seconds, a period field holding a jersey number — not to
// encode the rules of any particular sport.
const (
	// MaxClockPeriod bounds the period ordinal. Baseball's extra innings are the
	// long tail here; 50 is far beyond any real contest and far below anything a
	// parse error would produce.
	MaxClockPeriod = 50

	// MaxClockElapsed bounds time elapsed within a single period.
	MaxClockElapsed = 6 * time.Hour
)

// EventKind distinguishes the two shapes a betting event takes.
//
// Both are needed because CLAUDE.md §4 lists futures among the market types,
// and a futures market ("2027 NBA Champion") hangs off something that is not a
// contest between two sides. Collapsing them into one shape would mean either
// giving every futures event two fake competitors or making Home and Away
// optional on every event, and the second is how "the home team is empty"
// becomes a runtime surprise in the middle of a board render.
type EventKind uint8

const (
	// EventKindUnknown is the invalid zero value.
	EventKindUnknown EventKind = iota

	// EventKindMatch is a contest between exactly two competitors.
	EventKindMatch

	// EventKindOutright is a competition resolved among many runners — a
	// tournament or a season — with no home or away side.
	EventKindOutright
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values: they are what goes into Postgres, into Kafka JSON, and into the API.
func (k EventKind) String() string {
	switch k {
	case EventKindMatch:
		return "match"
	case EventKindOutright:
		return "outright"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a defined kind. EventKindUnknown is not valid: it
// means "unset", never a state an event can legitimately be in.
func (k EventKind) Valid() bool {
	return k == EventKindMatch || k == EventKindOutright
}

// ParseEventKind is the inverse of String for the defined kinds.
//
// Matching is exact and case-sensitive. This function reads back values this
// package wrote; normalising the many spellings a provider might use is the
// ingest normalizer's job, and doing it in both places is how the two
// definitions of "live" drift apart.
func ParseEventKind(s string) (EventKind, error) {
	switch s {
	case "match":
		return EventKindMatch, nil
	case "outright":
		return EventKindOutright, nil
	default:
		return EventKindUnknown, fmt.Errorf("event kind %q: %w", sample(s), ErrUnknownEventKind)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (k EventKind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("event kind %d: %w", uint8(k), ErrUnknownEventKind)
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. It is a construction path,
// not a mutation of a live value, which is why it may take a pointer receiver
// in a package that otherwise has none.
func (k *EventKind) UnmarshalText(b []byte) error {
	parsed, err := ParseEventKind(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// EventStatus is the event lifecycle.
//
// The legal transitions are:
//
//	scheduled  → live | postponed | cancelled
//	live       → suspended | ended | cancelled
//	suspended  → live | ended | postponed | cancelled
//	ended      → settled
//	postponed  → scheduled | cancelled
//	cancelled  → settled
//	settled    → (terminal)
//
// Two of those edges are worth defending.
//
// cancelled → settled exists because "settled" here means *grading is
// complete*, not "the event finished". A cancelled event still produces
// settlement work — every open wager on it is voided and every void is a
// balanced pair of ledger rows — and without this edge there is no event-level
// marker for "that work is done", so the settle service would have to hold that
// state somewhere else. It follows that settled is the only terminal status.
//
// suspended → postponed exists because a match abandoned after a weather delay
// is routinely rescheduled rather than cancelled outright.
type EventStatus uint8

const (
	// EventStatusUnknown is the invalid zero value.
	EventStatusUnknown EventStatus = iota

	// EventStatusScheduled means the event has not started. Markets on it may
	// be open.
	EventStatusScheduled

	// EventStatusLive means the event is in progress.
	EventStatusLive

	// EventStatusSuspended means the event is in progress but temporarily
	// halted — an injury delay, a weather stoppage, a review.
	EventStatusSuspended

	// EventStatusEnded means play is complete and a result exists, but wagers
	// have not yet been graded.
	EventStatusEnded

	// EventStatusSettled means every wager on the event has been graded. It is
	// the only terminal status.
	EventStatusSettled

	// EventStatusPostponed means the event will happen but not at the scheduled
	// time. It can return to scheduled once a new time is known.
	EventStatusPostponed

	// EventStatusCancelled means the event will not happen. Open wagers are
	// voided, after which the event moves to settled.
	EventStatusCancelled
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (s EventStatus) String() string {
	switch s {
	case EventStatusScheduled:
		return "scheduled"
	case EventStatusLive:
		return "live"
	case EventStatusSuspended:
		return "suspended"
	case EventStatusEnded:
		return "ended"
	case EventStatusSettled:
		return "settled"
	case EventStatusPostponed:
		return "postponed"
	case EventStatusCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// Valid reports whether s is a defined status.
func (s EventStatus) Valid() bool {
	switch s {
	case EventStatusScheduled, EventStatusLive, EventStatusSuspended,
		EventStatusEnded, EventStatusSettled, EventStatusPostponed, EventStatusCancelled:
		return true
	default:
		return false
	}
}

// ParseEventStatus is the inverse of String for the defined statuses.
func ParseEventStatus(s string) (EventStatus, error) {
	switch s {
	case "scheduled":
		return EventStatusScheduled, nil
	case "live":
		return EventStatusLive, nil
	case "suspended":
		return EventStatusSuspended, nil
	case "ended":
		return EventStatusEnded, nil
	case "settled":
		return EventStatusSettled, nil
	case "postponed":
		return EventStatusPostponed, nil
	case "cancelled":
		return EventStatusCancelled, nil
	default:
		return EventStatusUnknown, fmt.Errorf("event status %q: %w", sample(s), ErrUnknownEventStatus)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s EventStatus) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("event status %d: %w", uint8(s), ErrUnknownEventStatus)
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *EventStatus) UnmarshalText(b []byte) error {
	parsed, err := ParseEventStatus(string(b))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// IsInPlay reports whether the event is currently underway, halted included.
func (s EventStatus) IsInPlay() bool {
	return s == EventStatusLive || s == EventStatusSuspended
}

// HasStarted reports whether the event has begun by status. It says nothing
// about the wall clock — see Event.HasStartedBy for that.
func (s EventStatus) HasStarted() bool {
	switch s {
	case EventStatusLive, EventStatusSuspended, EventStatusEnded, EventStatusSettled, EventStatusCancelled:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether no further transition is possible. Only settled
// is terminal; see the type comment for why cancelled is not.
func (s EventStatus) IsTerminal() bool { return s == EventStatusSettled }

// AcceptsWagers reports whether the event's status permits new wagers. An
// individual market may still be closed or suspended — see Market.AcceptsWagers,
// which is the check that governs a bet slip.
func (s EventStatus) AcceptsWagers() bool {
	return s == EventStatusScheduled || s == EventStatusLive
}

// CanTransitionTo reports whether next is a legal successor of s.
//
// It is implemented as a switch rather than as a package-level map on purpose.
// A map would be package-level mutable state — any code in the package could
// add an edge at run time — which CLAUDE.md §12 forbids. A switch is a constant
// of the program.
//
// s → s is legal. Kafka delivers at least once (CLAUDE.md §3), so a consumer
// will receive "live" for an event that is already live as a matter of routine.
// Making the redelivery an error would force every consumer to special-case it,
// and the first consumer to forget would emit an error for a healthy system.
func (s EventStatus) CanTransitionTo(next EventStatus) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	if s == next {
		return true
	}
	switch s {
	case EventStatusScheduled:
		return next == EventStatusLive ||
			next == EventStatusPostponed ||
			next == EventStatusCancelled
	case EventStatusLive:
		return next == EventStatusSuspended ||
			next == EventStatusEnded ||
			next == EventStatusCancelled
	case EventStatusSuspended:
		return next == EventStatusLive ||
			next == EventStatusEnded ||
			next == EventStatusPostponed ||
			next == EventStatusCancelled
	case EventStatusEnded:
		return next == EventStatusSettled
	case EventStatusPostponed:
		return next == EventStatusScheduled ||
			next == EventStatusCancelled
	case EventStatusCancelled:
		return next == EventStatusSettled
	default: // EventStatusSettled is terminal.
		return false
	}
}

// Competitor is one side of a contest.
//
// It is called Competitor rather than Team because CLAUDE.md §4 puts player
// props and individual sports in scope: a competitor is a team in the NBA, a
// fighter in the UFC, and a player in tennis, and naming the type Team would
// make two of those three read as a modelling mistake at every call site.
//
// The identifier is optional. Providers frequently supply a display name and
// nothing else, and refusing the event over a missing surrogate key would drop
// real markets.
type Competitor struct {
	id   CompetitorID
	name string
}

// NewCompetitor validates and returns a Competitor. Pass the zero
// CompetitorID when the provider supplies no identifier.
func NewCompetitor(id CompetitorID, name string) (Competitor, error) {
	if !id.IsZero() {
		if err := validID(string(id)); err != nil {
			return Competitor{}, idErr("competitor id", string(id), err)
		}
	}
	n, err := validateName("competitor name", name)
	if err != nil {
		return Competitor{}, err
	}
	return Competitor{id: id, name: n}, nil
}

// ID returns the competitor's optional identifier.
func (c Competitor) ID() CompetitorID { return c.id }

// Name returns the competitor's display name.
func (c Competitor) Name() string { return c.name }

// IsZero reports whether c is unset.
func (c Competitor) IsZero() bool { return c == Competitor{} }

// String implements fmt.Stringer.
func (c Competitor) String() string {
	if c.IsZero() {
		return "<none>"
	}
	return c.name
}

// Score is the running or final score of a match event.
type Score struct {
	home int
	away int
}

// NewScore validates and returns a Score. Negative points are rejected; every
// scoring system Sharpline covers is non-negative, so a negative value is a
// parse error rather than a legitimate reading.
func NewScore(home, away int) (Score, error) {
	if home < 0 || away < 0 {
		return Score{}, fmt.Errorf("score %d-%d: %w", home, away, ErrNegativeScore)
	}
	return Score{home: home, away: away}, nil
}

// Home returns the home side's points.
func (s Score) Home() int { return s.home }

// Away returns the away side's points.
func (s Score) Away() int { return s.away }

// Margin returns home points minus away points. It is signed, and it is the
// quantity a spread market grades against.
func (s Score) Margin() int { return s.home - s.away }

// Total returns the combined points, which is the quantity a total market
// grades against.
func (s Score) Total() int { return s.home + s.away }

// String implements fmt.Stringer.
func (s Score) String() string { return fmt.Sprintf("%d-%d", s.home, s.away) }

// GameClock is the in-play clock.
//
// Elapsed time within the current period is stored, not remaining time, because
// elapsed is the only reading that is universal. Soccer counts up, basketball
// and hockey count down, and baseball has no clock at all — with elapsed, a
// count-down sport is rendered as periodLength minus elapsed at the
// presentation layer, and baseball carries period = inning with elapsed zero.
// Storing "remaining" would have forced every sport without a fixed period
// length into a lie.
type GameClock struct {
	period  int
	elapsed time.Duration
	running bool
}

// NewGameClock validates and returns a GameClock. Period is 1-based.
func NewGameClock(period int, elapsed time.Duration, running bool) (GameClock, error) {
	if period < 1 || period > MaxClockPeriod {
		return GameClock{}, fmt.Errorf("clock period %d: %w", period, ErrInvalidPeriod)
	}
	if elapsed < 0 || elapsed > MaxClockElapsed {
		return GameClock{}, fmt.Errorf("clock elapsed %s: %w", elapsed, ErrInvalidElapsed)
	}
	return GameClock{period: period, elapsed: elapsed, running: running}, nil
}

// Period returns the 1-based period ordinal — quarter, half, inning, round.
func (c GameClock) Period() int { return c.period }

// Elapsed returns time elapsed within the current period.
func (c GameClock) Elapsed() time.Duration { return c.elapsed }

// Running reports whether the clock is currently moving.
func (c GameClock) Running() bool { return c.running }

// IsZero reports whether c is unset.
func (c GameClock) IsZero() bool { return c == GameClock{} }

// String implements fmt.Stringer.
func (c GameClock) String() string {
	state := "stopped"
	if c.running {
		state = "running"
	}
	return fmt.Sprintf("P%d %s (%s)", c.period, c.elapsed, state)
}

// Event is a contest that markets are offered on.
//
// Values are immutable. Every state change returns a new Event: see WithStatus,
// WithClock, and WithScore.
type Event struct {
	id             EventID
	leagueID       LeagueID
	kind           EventKind
	name           string
	home           Competitor
	away           Competitor
	scheduledStart time.Time
	status         EventStatus
	clock          GameClock
	hasClock       bool
	score          Score
	hasScore       bool
	updatedAt      time.Time
}

// EventParams is the input to NewEvent.
type EventParams struct {
	ID       EventID
	LeagueID LeagueID
	Kind     EventKind

	// Name is the display name. For a match it is typically "Away at Home";
	// for an outright it is the competition ("2027 NBA Champion"). It is stored
	// rather than derived so that the provider's own wording survives.
	Name string

	// Home and Away are required for EventKindMatch and must be zero for
	// EventKindOutright. "Home" means the nominally-listed-first side; at a
	// neutral venue it still fixes a consistent order, which is what the
	// home-perspective line convention in market.go depends on.
	Home Competitor
	Away Competitor

	// ScheduledStart is the advertised start instant. It is normalised to UTC.
	ScheduledStart time.Time

	Status EventStatus

	// UpdatedAt stamps the observation this value came from. It is the
	// monotonicity guard for out-of-order bus delivery, so it must be the
	// provider's or ingester's observation time, never a display time.
	UpdatedAt time.Time
}

// NewEvent validates its input and returns an immutable Event.
//
// Times are normalised to UTC rather than rejected for carrying an offset.
// Providers emit RFC 3339 with zone offsets; normalising once here means every
// later comparison in the system is between two UTC instants, which removes an
// entire class of "correct answer, wrong zone" bug without rejecting real data.
func NewEvent(p EventParams) (Event, error) {
	if err := validID(string(p.ID)); err != nil {
		return Event{}, idErr("event id", string(p.ID), err)
	}
	if err := validID(string(p.LeagueID)); err != nil {
		return Event{}, idErr("league id", string(p.LeagueID), err)
	}
	if !p.Kind.Valid() {
		return Event{}, fmt.Errorf("event %s: %w", p.ID, ErrUnknownEventKind)
	}
	if !p.Status.Valid() {
		return Event{}, fmt.Errorf("event %s: %w", p.ID, ErrUnknownEventStatus)
	}
	name, err := validateName("event name", p.Name)
	if err != nil {
		return Event{}, fmt.Errorf("event %s: %w", p.ID, err)
	}

	switch p.Kind {
	case EventKindMatch:
		if p.Home.IsZero() || p.Away.IsZero() {
			return Event{}, fmt.Errorf("event %s: %w", p.ID, ErrCompetitorsRequired)
		}
	case EventKindOutright:
		if !p.Home.IsZero() || !p.Away.IsZero() {
			return Event{}, fmt.Errorf("event %s: %w", p.ID, ErrCompetitorsNotApplicable)
		}
	}

	if p.ScheduledStart.IsZero() {
		return Event{}, fmt.Errorf("event %s scheduled start: %w", p.ID, ErrZeroTime)
	}
	if p.UpdatedAt.IsZero() {
		return Event{}, fmt.Errorf("event %s updated at: %w", p.ID, ErrZeroTime)
	}

	return Event{
		id:             p.ID,
		leagueID:       p.LeagueID,
		kind:           p.Kind,
		name:           name,
		home:           p.Home,
		away:           p.Away,
		scheduledStart: p.ScheduledStart.UTC(),
		status:         p.Status,
		updatedAt:      p.UpdatedAt.UTC(),
	}, nil
}

// ID returns the event's identifier.
func (e Event) ID() EventID { return e.id }

// LeagueID returns the identifier of the league this event belongs to.
func (e Event) LeagueID() LeagueID { return e.leagueID }

// Kind returns whether the event is a match or an outright.
func (e Event) Kind() EventKind { return e.kind }

// Name returns the event's display name.
func (e Event) Name() string { return e.name }

// Home returns the home competitor. It is the zero Competitor for an outright.
func (e Event) Home() Competitor { return e.home }

// Away returns the away competitor. It is the zero Competitor for an outright.
func (e Event) Away() Competitor { return e.away }

// ScheduledStart returns the advertised start instant, in UTC.
func (e Event) ScheduledStart() time.Time { return e.scheduledStart }

// Status returns the event's lifecycle status.
func (e Event) Status() EventStatus { return e.status }

// UpdatedAt returns the observation instant this value carries, in UTC.
func (e Event) UpdatedAt() time.Time { return e.updatedAt }

// Clock returns the in-play clock and whether one is set. The second return is
// the whole point: an event with no clock is distinct from one stopped at 0:00
// in period 1.
func (e Event) Clock() (GameClock, bool) { return e.clock, e.hasClock }

// Score returns the score and whether one is set.
func (e Event) Score() (Score, bool) { return e.score, e.hasScore }

// IsInPlay reports whether the event is underway.
func (e Event) IsInPlay() bool { return e.status.IsInPlay() }

// IsTerminal reports whether the event has reached its terminal status.
func (e Event) IsTerminal() bool { return e.status.IsTerminal() }

// AcceptsWagers reports whether the event's status permits new wagers.
func (e Event) AcceptsWagers() bool { return e.status.AcceptsWagers() }

// HasStartedBy reports whether the scheduled start is at or before now.
//
// It takes the instant as a parameter because this package never reads a clock;
// that is what lets "what did the board look like at tip-off" be answered by a
// test as easily as by production code.
func (e Event) HasStartedBy(now time.Time) bool { return !now.Before(e.scheduledStart) }

// TimeToStart returns how long remains until the scheduled start, negative once
// the start is in the past.
func (e Event) TimeToStart(now time.Time) time.Duration { return e.scheduledStart.Sub(now) }

// WithStatus returns a copy of the event in the next status.
//
// It fails with ErrIllegalTransition if the edge is not in the lifecycle, and
// with ErrStaleUpdate if at precedes the event's current UpdatedAt — an
// out-of-order redelivery must not resurrect an earlier state. An update
// stamped at exactly UpdatedAt is accepted, since two observations can share an
// instant.
//
// Leaving in-play clears the clock: an ended event that still reported "Q3
// 7:34" would be a lie that the UI would happily render.
func (e Event) WithStatus(next EventStatus, at time.Time) (Event, error) {
	if !next.Valid() {
		return Event{}, fmt.Errorf("event %s → %d: %w", e.id, uint8(next), ErrUnknownEventStatus)
	}
	if !e.status.CanTransitionTo(next) {
		return Event{}, fmt.Errorf("event %s %s → %s: %w", e.id, e.status, next, ErrIllegalTransition)
	}
	stamped, err := e.stamp(at)
	if err != nil {
		return Event{}, err
	}
	stamped.status = next
	if !next.IsInPlay() {
		stamped.clock = GameClock{}
		stamped.hasClock = false
	}
	return stamped, nil
}

// WithClock returns a copy of the event carrying the given clock.
//
// A clock is only meaningful in play, so it is rejected with ErrClockNotInPlay
// in every other status. That is a conflict, not bad input: the same clock
// applied to the same event a minute earlier would have been correct.
func (e Event) WithClock(c GameClock, at time.Time) (Event, error) {
	if !e.status.IsInPlay() {
		return Event{}, fmt.Errorf("event %s is %s: %w", e.id, e.status, ErrClockNotInPlay)
	}
	if c.IsZero() {
		return Event{}, fmt.Errorf("event %s clock: %w", e.id, ErrInvalidPeriod)
	}
	stamped, err := e.stamp(at)
	if err != nil {
		return Event{}, err
	}
	stamped.clock = c
	stamped.hasClock = true
	return stamped, nil
}

// WithoutClock returns a copy of the event with no clock, for a stoppage where
// the provider stops reporting one.
func (e Event) WithoutClock(at time.Time) (Event, error) {
	stamped, err := e.stamp(at)
	if err != nil {
		return Event{}, err
	}
	stamped.clock = GameClock{}
	stamped.hasClock = false
	return stamped, nil
}

// WithScore returns a copy of the event carrying the given score.
//
// A score requires the event to have started by status. A scheduled or
// postponed event reporting a score is a data error, and accepting it would
// let a settled-looking result reach the grading path.
func (e Event) WithScore(s Score, at time.Time) (Event, error) {
	if !e.status.HasStarted() {
		return Event{}, fmt.Errorf("event %s is %s: %w", e.id, e.status, ErrScoreNotApplicable)
	}
	stamped, err := e.stamp(at)
	if err != nil {
		return Event{}, err
	}
	stamped.score = s
	stamped.hasScore = true
	return stamped, nil
}

// stamp copies the event with a new UpdatedAt, enforcing monotonicity.
func (e Event) stamp(at time.Time) (Event, error) {
	if at.IsZero() {
		return Event{}, fmt.Errorf("event %s update at: %w", e.id, ErrZeroTime)
	}
	u := at.UTC()
	if u.Before(e.updatedAt) {
		return Event{}, fmt.Errorf("event %s: update at %s precedes %s: %w",
			e.id, u.Format(time.RFC3339Nano), e.updatedAt.Format(time.RFC3339Nano), ErrStaleUpdate)
	}
	next := e
	next.updatedAt = u
	return next, nil
}

// IsZero reports whether e is the zero Event, which no constructor produces.
func (e Event) IsZero() bool { return e.id.IsZero() }

// String implements fmt.Stringer.
func (e Event) String() string {
	if e.IsZero() {
		return "event(<zero>)"
	}
	return fmt.Sprintf("event(%s %q %s)", e.id, e.name, e.status)
}
