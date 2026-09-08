// Package pgstore is the Postgres implementation of the storage seam
// internal/ingest/results declares.
//
// It is a separate package from internal/ingest/results for the reason
// internal/settlement/pgstore gives about itself: the loop's POLICY — the
// horizon, the batching, the backoff, what counts as a steady state and what
// counts as a stake stuck in escrow — is testable against a fake, and keeping it
// in a package that does not import a database driver is what makes that true
// rather than aspirational. Everything here is SQL, row-to-domain translation,
// and error wrapping.
//
// Two jobs, and nothing else:
//
//  1. RUN THE GENERATED QUERIES. Both statements are named queries in
//     internal/platform/postgres/queries/results.sql, which is what keeps them
//     inside the `sqlc diff` drift gate and the `make query-plans` index check.
//     There is no hand-written SQL here, and there must not be: an earlier round
//     of this work carried a hand-written UPDATE that looked identical and was
//     not — it guarded with `IS DISTINCT FROM` rather than on the source status,
//     so it permitted `settled → ended` and would have let a result overwrite an
//     event whose wagers had already been graded and paid. That is exactly the
//     divergence a second copy of a statement produces, and the reason this
//     package exists is so there is only one.
//
//  2. TRANSLATE THE ROW. sqlc rows carry pgtype.Int4 and raw enum strings,
//     because that is what the columns are. internal/ingest/results should not
//     know that. Enum strings go through the domain's own ParseX functions, each
//     of which errors on an unrecognised value, so a schema/Go divergence
//     surfaces as a wrapped error at the read rather than as a silent zero.
//
// # Why the write is not wrapped in a transaction
//
// [Store.RecordResult] issues ONE guarded UPDATE of ONE row, and a single
// statement is already atomic. Wrapping it in postgres.InTx would hold a
// connection for an extra round trip and buy nothing, because there is no second
// statement whose outcome it has to be consistent with. That is a different
// situation from internal/betting's writes, which move a ledger and a wager and
// an audit row together and must therefore commit together.
package pgstore

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/results"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// Store is results.Store over the `events` table.
//
// It reads and writes through the POOL rather than holding a transaction,
// because neither operation has a second statement to be consistent with; see
// the package comment.
type Store struct {
	q *gen.Queries
}

// NewStore builds the adapter. This is what cmd/ingest wires.
func NewStore(db *postgres.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: results pgstore needs a database", results.ErrInvalidOptions)
	}
	return &Store{q: gen.New(db.Pool())}, nil
}

// Compile-time proof that this package satisfies the interface
// internal/ingest/results declares. It is asserted HERE and not there, because
// this package imports that one and the reverse assertion would be an import
// cycle.
var _ results.Store = (*Store)(nil)

// EventsAwaitingResult implements results.Store: the work queue, oldest first.
//
// A row whose STATUS or KIND does not parse is returned as an ERROR rather than
// skipped. That is the opposite call from settlement's results feed, which skips
// an unusable row and warns, and the difference is which failure each one is
// looking at: settlement is reading a value some provider stated about one
// contest, where one bad row is one bad row. This query's status column is
// constrained by the statement's own `IN` list and its kind by the schema's
// enum, so a value here that the domain does not recognise is a schema/Go
// divergence affecting every row with that value. Continuing past it would turn
// a deployment mistake into an unbounded number of unsettled tickets, silently.
func (s *Store) EventsAwaitingResult(
	ctx context.Context, finishedBefore time.Time, limit int,
) ([]results.PendingEvent, error) {
	rowLimit, err := int32Limit(limit)
	if err != nil {
		return nil, err
	}

	rows, err := s.q.ListEventsAwaitingResult(ctx, gen.ListEventsAwaitingResultParams{
		FinishedBefore: finishedBefore.UTC(),
		RowLimit:       rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: read events awaiting a result before %s: %w",
			finishedBefore.UTC().Format(time.RFC3339Nano), err)
	}

	out := make([]results.PendingEvent, 0, len(rows))
	for _, r := range rows {
		kind, err := domain.ParseEventKind(r.Kind)
		if err != nil {
			return nil, fmt.Errorf("pgstore: event %s: %w", r.ID, err)
		}
		status, err := domain.ParseEventStatus(r.Status)
		if err != nil {
			return nil, fmt.Errorf("pgstore: event %s: %w", r.ID, err)
		}
		out = append(out, results.PendingEvent{
			EventID:        r.ID,
			League:         r.LeagueID,
			Kind:           kind,
			Name:           r.Name,
			Status:         status,
			ScheduledStart: r.ScheduledStart,
			ObservedAt:     r.ObservedAt,
		})
	}
	return out, nil
}

// RecordResult implements results.Store: one guarded UPDATE of one row.
//
// # A zero row count is a success
//
// UpsertEventResult refuses a source status that is already terminal and refuses
// an observation older than the stored one, so it matches no row when the result
// is already recorded, when a newer observation is stored, and when this
// deployment never ingested the contest. All three are steady states the poller
// meets constantly — results.Store's own contract requires that none of them be
// reported as an error — and the row count is the only signal that separates
// them from a write that landed.
//
// # The result is validated before it is sent
//
// provider.FinalResult.Validate is asked again here even though the poller
// already asked it. It is not a redundant check: this is the last point before
// the value becomes a row, and the failure it catches — an `ended` result with
// no score — is one the schema permits (events_score_all_or_nothing constrains
// the score PAIR, not its presence) and which would then grade every spread on
// the contest against a 0-0 zero value. A plausible wrong number is worse than
// an error, and the cost of asking twice is a switch statement.
func (s *Store) RecordResult(ctx context.Context, id domain.EventID, r provider.FinalResult) (bool, error) {
	if err := r.Validate(); err != nil {
		return false, fmt.Errorf("pgstore: %w", err)
	}

	// The two score columns move together because events_score_all_or_nothing
	// constrains the pair: both Valid for an ended contest, both invalid for a
	// cancelled one, which has no score because it will not be played.
	// FinalResult.Validate has already established that HasScore and the status
	// agree, so this reads the one field rather than re-deciding.
	var home, away pgtype.Int4
	if r.HasScore {
		home = pgtype.Int4{Int32: int32(r.Score.Home()), Valid: true}
		away = pgtype.Int4{Int32: int32(r.Score.Away()), Valid: true}
	}

	n, err := s.q.UpsertEventResult(ctx, gen.UpsertEventResultParams{
		Status:    r.Status.String(),
		ScoreHome: home,
		ScoreAway: away,
		// The PROVIDER's instant, never this container's clock. settlement
		// stamps every leg it grades from this row with it, so a replayed
		// result must re-apply the original observation time rather than the
		// replay's.
		ObservedAt: r.FinalisedAt.UTC(),
		// The DOMAIN identifier, resolved by the poller from the provider's own
		// key. It is the only form `events.id` is addressable by; the key on the
		// result is the provider's and names nothing in this schema.
		ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("pgstore: record result for event %s: %w", id, err)
	}
	return n > 0, nil
}

// int32Limit narrows a batch size to the INTEGER a LIMIT parameter is.
//
// A non-positive limit is refused rather than sent: `LIMIT 0` returns no rows,
// which the poller would read as "there is nothing to settle" and act on by
// settling nothing — a silent, permanent stall on every customer's escrow.
// results.Store makes the same demand of its own limit, and
// internal/settlement/pgstore refuses it the same way.
func int32Limit(n int) (int32, error) {
	if n <= 0 {
		return 0, fmt.Errorf("%w: batch limit %d must be positive; LIMIT 0 returns no rows, "+
			"which reads as 'there is nothing to settle'", results.ErrInvalidOptions, n)
	}
	if n > math.MaxInt32 {
		return 0, fmt.Errorf("%w: batch limit %d exceeds the column's range",
			results.ErrInvalidOptions, n)
	}
	return int32(n), nil
}
