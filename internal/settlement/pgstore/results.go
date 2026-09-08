package pgstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
	"github.com/anpl1623/sharpline/internal/settlement"
)

// Results is settlement.ResultsSource over the `events` table.
//
// doc.go carries the argument for why reading that table is reading a live
// generator's own output rather than fixture data, and why a real provider's
// results adapter drops in behind the same interface without internal/settlement
// changing.
//
// It reads from the POOL and never opens a transaction. Since is a read with no
// side effects, which ports.go requires of it ("It is a READ. Since must not
// write, and must not have side effects a retry would double"), and wrapping a
// single statement in a transaction would hold a connection for the round trip
// and buy nothing.
type Results struct {
	q   *gen.Queries
	log *slog.Logger
}

// ResultsOptions configures [NewResults].
type ResultsOptions struct {
	// DB is the pool to read from. Required.
	DB *postgres.DB

	// Logger receives one WARN per row this source declines to hand on. Nil
	// means slog.Default().
	//
	// It is not optional in spirit even though it is in code: [Results.Since]
	// SKIPS a row that is not a usable result, per ports.go's contract, and a
	// skipped result is a customer's stake sitting in escrow with nothing to
	// release it. Dropping it silently would make that invisible, which is
	// exactly the failure internal/settlement's own ServiceOptions refuses a nil
	// logger over.
	Logger *slog.Logger
}

// NewResults builds the results feed.
func NewResults(opts ResultsOptions) (*Results, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("%w: results source needs a database", settlement.ErrInvalidOptions)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Results{q: gen.New(opts.DB.Pool()), log: log}, nil
}

// Since implements settlement.ResultsSource: results that became final at or
// after watermark, oldest first, at most limit of them.
//
// THE BOUNDARY IS INCLUSIVE, and that is the contract rather than an off-by-one.
// A provider poll finalises a whole slate at one observation instant, so ties at
// the boundary are the common case; an exclusive bound would drop every result
// sharing the instant the cursor names. The cost is that the last batch's final
// instant is re-read on every poll, which grading absorbs because it is
// idempotent — re-grading an already-graded leg costs one UPDATE matching zero
// rows, whereas losing a result costs a customer their stake.
//
// A ROW THAT IS NOT A USABLE RESULT IS SKIPPED, NOT RETURNED. ports.go requires
// it: "settlement counts what it is given and a source that leaks junk makes the
// count meaningless." The junk is real and the schema permits it — the catalogue
// migration constrains the score PAIR (events_score_all_or_nothing) and
// deliberately does NOT require a score for a started status, so an `ended` row
// with no score is storable, and handing one on would grade every spread on the
// event against a 0-0 zero value.
//
// Every skip is logged at WARN with the event and the reason, because a skipped
// result is a stake stuck in escrow. This is the one place in the package that
// logs, and it logs because the alternative is a silent, permanent stall.
//
// A row whose STATUS does not parse is a different matter and is returned as an
// error rather than skipped: that is a schema/domain divergence affecting every
// row with that value, not one bad event, and continuing past it would turn a
// deployment mistake into an unbounded number of unsettled tickets.
func (r *Results) Since(ctx context.Context, watermark time.Time, limit int) ([]settlement.Result, error) {
	rowLimit, err := int32Limit(limit)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListFinalisedEventsSince(ctx, gen.ListFinalisedEventsSinceParams{
		Since:    watermark.UTC(),
		RowLimit: rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: read finalised events since %s: %w",
			watermark.UTC().Format(time.RFC3339Nano), err)
	}

	results := make([]settlement.Result, 0, len(rows))
	for _, row := range rows {
		status, err := domain.ParseEventStatus(row.Status)
		if err != nil {
			return nil, fmt.Errorf("pgstore: event %s: %w", row.ID, err)
		}

		result := settlement.Result{
			EventID: row.ID,
			Status:  status,
			// events.observed_at is the PROVIDER's instant for the terminal
			// status, which is what every leg graded from this result is
			// stamped with. Using the row's created_at or updated_at instead
			// would restamp a replayed settlement with the replay's own time.
			FinalisedAt: row.ObservedAt,
		}

		// events_score_all_or_nothing constrains the pair, so either both are
		// present or neither is. Both are tested anyway: a half-scored row would
		// otherwise build a Score from one real number and one zero, which is a
		// plausible final and therefore the worst possible failure mode.
		if row.ScoreHome.Valid && row.ScoreAway.Valid {
			score, err := domain.NewScore(int(row.ScoreHome.Int32), int(row.ScoreAway.Int32))
			if err != nil {
				r.skip(row.ID, "stored score is not a valid final score", err)
				continue
			}
			result.Score = score
			result.HasScore = true
		}

		if err := result.Validate(); err != nil {
			r.skip(row.ID, "row is not a usable result", err)
			continue
		}
		results = append(results, result)
	}
	return results, nil
}

// skip records a row this source declined to hand on.
//
// WARN and not ERROR: the settle service is healthy and the pipeline is running;
// what is wrong is one event's data. ERROR would page somebody for a row that
// the next provider poll may well fix. WARN with the event id is what lets
// somebody answer "why is this customer's stake still in escrow" without a
// database session.
func (r *Results) skip(id domain.EventID, why string, err error) {
	r.log.Warn("results source skipped an event row",
		slog.String("event_id", id.String()),
		slog.String("reason", why),
		slog.String("error", err.Error()),
	)
}
