// The storage seam, declared by the consumer (CLAUDE.md §12: "Interfaces are
// declared by the consumer, not the producer. Keep them small.").
package results

import (
	"context"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// PendingEvent is one row of the work queue: a contest that should be over and
// is not yet recorded as such.
//
// It is the shape queries/results.sql's ListEventsAwaitingResult returns, in the
// domain's own types rather than the database's. The translation happens in the
// pgstore adapter so that this package never sees a raw status string or a
// pgtype value, which is what keeps its unit tests free of a driver.
type PendingEvent struct {
	// EventID is the contest.
	EventID domain.EventID

	// League is the competition, which is how a results endpoint is addressed.
	League domain.LeagueID

	// Kind separates a contest between two sides from a competition resolved
	// among many runners.
	//
	// Nothing in the loop branches on it, and neither does the statement that
	// produced the row — queries/results.sql declines to filter on it because
	// "that horizon is the provider's knowledge, not this statement's". It is
	// carried for two reasons that survive that. It is what distinguishes, on
	// the face of a stranded queue row, a contest whose result is merely late
	// from an outright that nothing in this deployment will ever resolve; and
	// parsing it at the read is what turns a schema/Go divergence in the enum
	// into a wrapped error rather than a silent zero value.
	Kind domain.EventKind

	// Name is the fixture's display name. It is used only in log lines, and it
	// is worth the column: "syn-sba-20260817-3 has been waiting four hours" is a
	// row identifier, whereas the same line with the fixture name is something
	// an operator can act on without a database session.
	Name string

	// Status is what the row currently says: scheduled, live or suspended. The
	// query's predicate admits no others.
	Status domain.EventStatus

	// ScheduledStart is when the contest was due to begin. It is the ordering
	// key of the queue and the provider's only input to "is this old enough to
	// be over".
	ScheduledStart time.Time

	// ObservedAt is the provider instant of the row's last observation, which
	// for a stranded contest is the last time the odds path saw it alive. It is
	// what makes "this event has been silent for six hours" measurable from the
	// row itself.
	ObservedAt time.Time
}

// String implements fmt.Stringer.
func (e PendingEvent) String() string {
	return fmt.Sprintf("pending(%s %s %s %s @%s)",
		e.EventID, e.League, e.Kind, e.Status, e.ScheduledStart.UTC().Format(time.RFC3339))
}

// Store is the part of the `events` table this package uses.
//
// Two methods, because two is all the loop needs: read the work queue, and write
// one outcome. There is no update-in-place, no delete and no insert, and the
// absence of an insert is the interface-level form of the NO MOCK DATA rule —
// this package is structurally incapable of putting a contest on the books.
type Store interface {
	// EventsAwaitingResult returns contests that started before finishedBefore
	// and have not reached a terminal status, oldest first, at most limit of
	// them.
	//
	// finishedBefore is computed by the CALLER and is never the database's
	// clock: a query that reads now() cannot be tested at a fixed instant and
	// cannot be replayed. limit must be positive — a LIMIT 0 would return
	// nothing, which reads as "there is nothing to settle" and is a silent
	// permanent stall.
	//
	// It is a READ with no side effects, so a retry is free.
	EventsAwaitingResult(ctx context.Context, finishedBefore time.Time, limit int) ([]PendingEvent, error)

	// RecordResult writes one contest's terminal status and final score.
	//
	// The identifier arrives BESIDE the result rather than on it, and the split
	// is the whole shape of the results seam rather than an awkwardness. The
	// provider states an outcome in its own identifier space
	// (provider.FinalResult.EventKey); id is what internal/ingest/results derived
	// from that key with the same forward derivation the ingest path used, and
	// therefore the only form the `events` table can be addressed by. Putting a
	// domain identifier back onto the provider's value would recreate the field
	// whose meaning depended on who was holding it — which is what let a domain
	// identifier be compared against a native one in every results poll this
	// system issued, matching nothing, for ever.
	//
	// recorded reports whether THIS call was the one that wrote the row. It is
	// false with a NIL ERROR in three situations, all of them steady states a
	// poller meets constantly:
	//
	//   - the result was already recorded, by an earlier tick or another replica;
	//   - the stored row carries a newer observation, so the out-of-order guard
	//     declined it;
	//   - no such event exists, because this deployment never ingested the
	//     contest.
	//
	// An implementation MUST NOT report any of the three as an error, and MUST
	// NOT report a failed write as a false. The distinction is what the caller
	// counts on: a false is a state to be counted, an error is a stake that has
	// not been released and a tick that must try again.
	//
	// It MUST refuse to move an event out of a terminal status. Writing `ended`
	// over `settled` would un-settle a ticket that has already been graded and
	// paid; writing it over `cancelled` would restate a void as a game after the
	// stakes went back. The guard belongs in the statement, where a caller
	// cannot forget it.
	RecordResult(ctx context.Context, id domain.EventID, r provider.FinalResult) (recorded bool, err error)
}
