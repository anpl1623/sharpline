// Package pgclv is the Postgres implementation of the seams the closing-line-value
// path declares: [Store] satisfies clv.Store, and the same type also satisfies
// the settle service's CLV work-queue and writer ports.
//
// It is a separate package from internal/analytics/clv for the reason that
// package gives itself: the closing-price rules in its doc.go are asserted
// against an in-memory store in a unit test and against a real TimescaleDB in the
// integration tier, with the same code under test, and that is only true while
// internal/analytics/clv does not import a database driver. Everything here is
// running generated queries, translating rows, and classifying failure. There is
// no closing-price rule in this package, and a review that finds one should move
// it to clv.go.
//
// It is the sibling of internal/settlement/pgstore and internal/betting/pgstore
// and follows their idioms deliberately rather than inventing a third: an
// options struct with a required *postgres.DB, a *gen.Queries over the POOL, and
// a compile-time assertion that the shipped type satisfies each declared
// interface.
//
// # Why nothing here opens a transaction
//
// Every statement in this package is either a single read or a single upsert.
//
// The two snapshot reads are independent by design, and a shared snapshot
// isolation would protect nothing: migrations/00003 makes a price row immutable
// once written — "a new price is a new row" — so neither read can observe a value
// that changes underneath it. The write is one upsert on one primary key, so
// there is no multi-statement invariant for a transaction to hold.
//
// That is a real difference from internal/settlement/pgstore, which wraps every
// ledger movement in postgres.DB.InTx because the double-entry assertion is
// DEFERRABLE INITIALLY DEFERRED and only surfaces at COMMIT. Nothing here touches
// money. The settle service's CLV pass runs entirely outside the settlement
// transaction, and that separation is what makes a failed measurement a missing
// report rather than an unpaid customer.
//
// # No mock data
//
// Every row this package reads is pipeline output: `prices` is written by
// internal/ingest/writer from the synthetic provider's own stochastic market
// maker, `legs` is written by internal/betting when a customer's slip is
// accepted, and `market_suspensions` is written by the admin path. Nothing here
// seeds anything, and a market the generator has not priced yields an incomplete
// snapshot and therefore no measurement — which is the correct empty state and is
// itself the evidence that the numbers are not invented.
package pgclv

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/analytics/clv"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// Store is the closing-line-value data plane.
//
// One type implements two seams — clv.Store's two reads and the settle service's
// queue-and-write pair — because they are one responsibility over one pool, and
// splitting them would give the composition root two adapters to build and keep
// consistent for no gain. Each seam is asserted separately below, so a change
// that breaks either fails this package's build.
type Store struct {
	q *gen.Queries
}

// Compile-time proof that the shipped type satisfies the read seam. The settle
// service's ports are asserted in that package, because asserting them here would
// make internal/settlement an import of this package and this package an import
// of internal/settlement.
var _ clv.Store = (*Store)(nil)

// NewStore builds the data plane over an existing pool.
func NewStore(db *postgres.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: clv store needs a database", clv.ErrInvalidOptions)
	}
	return &Store{q: gen.New(db.Pool())}, nil
}

// -----------------------------------------------------------------------------
// clv.Store
// -----------------------------------------------------------------------------

// MarketClose implements clv.Store: the market's identity and its closing
// instant.
//
// A missing market is [clv.ErrMarketNotFound] rather than an empty value.
// legs.market_id is a foreign key, so this cannot happen through the schema, and
// reporting it as "this market has no close" would file a referential defect as
// an analytics exclusion — which is exactly the confusion clv.Measure's three
// error kinds exist to prevent.
func (s *Store) MarketClose(ctx context.Context, id domain.MarketID) (clv.Market, error) {
	row, err := s.q.GetMarketClosingInstant(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return clv.Market{}, fmt.Errorf("pgclv: market %s: %w", id, clv.ErrMarketNotFound)
		}
		return clv.Market{}, fmt.Errorf("pgclv: closing instant for market %s: %w", id, err)
	}

	marketType, err := domain.ParseMarketType(row.MarketType)
	if err != nil {
		return clv.Market{}, fmt.Errorf("pgclv: market %s: %w", id, err)
	}
	eventStatus, err := domain.ParseEventStatus(row.EventStatus)
	if err != nil {
		return clv.Market{}, fmt.Errorf("pgclv: market %s event %s: %w", id, row.EventID, err)
	}

	return clv.Market{
		MarketID:    row.MarketID,
		MarketType:  marketType,
		EventID:     row.EventID,
		EventStatus: eventStatus,
		LeagueID:    row.LeagueID,
		// events.scheduled_start IS the closing instant. clv/doc.go §1 carries the
		// argument for why it is this column and not the actual kickoff.
		ScheduledStart: row.ScheduledStart.UTC(),
	}, nil
}

// Snapshot implements clv.Store: every eligible quote for one market at one book
// as of one instant, plus the market's selection count.
//
// The completeness rule is NOT applied here, deliberately. queries/analytics.sql
// states that the lateral is an inner join so an unpriced selection produces no
// row, and that the caller must compare the row count against market_selections
// and discard the whole snapshot if it is short. Applying that here would put the
// closing-price definition in two places; carrying the count across the seam
// keeps it in one, and clv.Snapshot.Complete is where it is decided.
//
// A market with no eligible quotes at all returns zero quotes, a positive count
// and a nil error. That is the shape of a market suspended through its entire
// lookback window, and it is an ordinary outcome rather than a not-found
// condition.
func (s *Store) Snapshot(ctx context.Context, req clv.SnapshotRequest) (clv.Snapshot, error) {
	if err := validSnapshotRequest(req); err != nil {
		return clv.Snapshot{}, err
	}

	rows, err := s.q.MarketSnapshotAtBookAsOf(ctx, gen.MarketSnapshotAtBookAsOfParams{
		MarketID:  req.Market,
		BookID:    req.Book,
		AsOf:      req.AsOf.UTC(),
		NotBefore: req.NotBefore.UTC(),
	})
	if err != nil {
		return clv.Snapshot{}, fmt.Errorf(
			"pgclv: snapshot of market %s at book %s as of %s (not before %s): %w",
			req.Market, req.Book,
			req.AsOf.UTC().Format(time.RFC3339Nano),
			req.NotBefore.UTC().Format(time.RFC3339Nano), err)
	}

	out := clv.Snapshot{Quotes: make([]clv.Quote, 0, len(rows))}
	for _, row := range rows {
		role, err := domain.ParseSelectionRole(row.Role)
		if err != nil {
			return clv.Snapshot{}, fmt.Errorf("pgclv: market %s selection %s: %w",
				req.Market, row.SelectionID, err)
		}
		out.Quotes = append(out.Quotes, clv.Quote{
			Selection: row.SelectionID,
			Role:      role,
			Decimal:   row.DecimalOdds,
			Line:      lineFrom(row.Line),
			// prices.observed_at is the PROVIDER's instant. It is what the
			// snapshot's own instant is taken from, and what makes closed_at and
			// taken_at two values off one clock.
			ObservedAt: row.ObservedAt.UTC(),
		})
		// The count is a scalar subquery repeated on every row, so they all agree
		// and the last one wins.
		out.MarketSelections = int(row.MarketSelections)
	}

	// NO ROWS carries no count, so MarketSelections stays zero — which is why
	// clv.Snapshot.Complete tests the count for POSITIVITY before comparing it to
	// the quote count. Without that test, a market whose every quote was excluded
	// would read 0 == 0 and be devigged as an empty market. There is deliberately
	// no second query to establish the real count: no answer it could give would
	// change the outcome, and a round trip for a value nothing consumes is a round
	// trip on the hot path of a backlog drain.
	return out, nil
}

// -----------------------------------------------------------------------------
// The settle service's CLV work queue and writer
// -----------------------------------------------------------------------------

// GradedLegsAwaitingCLV returns graded legs that have no measurement yet, oldest
// first, at most limit of them.
//
// It is the work queue that makes the measurement step RESTARTABLE, which is the
// property that lets it live outside the settlement transaction: a settle replica
// that died between grading a leg and measuring it finds the leg here on restart,
// and so does one whose measurement failed for a reason that has since been fixed
// — a backfilled price, a suspension episode recorded late.
//
// BOTH BOUNDS ARE REQUIRED and both are the caller's. The lower one is what stops
// a leg that is permanently unmeasurable — an in-play wager, a market that shut
// an hour before kickoff — from being retried on every pass for ever; it ages out
// of the window instead. The upper one is what makes one pass a bounded walk
// rather than a moving target.
func (s *Store) GradedLegsAwaitingCLV(
	ctx context.Context, from, to time.Time, limit int,
) ([]clv.Leg, error) {
	rowLimit, err := int32Limit(limit)
	if err != nil {
		return nil, err
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("%w: CLV queue window [%s, %s) is empty", clv.ErrInvalidOptions,
			from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	}

	rows, err := s.q.ListGradedLegsAwaitingCLV(ctx, gen.ListGradedLegsAwaitingCLVParams{
		// legs.graded_at is nullable in migrations/00006 — a pending leg has none
		// — so the generated parameters are pgtype. The predicate itself excludes
		// pending legs, so Valid is always true here.
		GradedFrom: pgtype.Timestamptz{Time: from.UTC(), Valid: true},
		GradedTo:   pgtype.Timestamptz{Time: to.UTC(), Valid: true},
		RowLimit:   rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("pgclv: graded legs awaiting CLV in [%s, %s): %w",
			from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), err)
	}

	legs := make([]clv.Leg, 0, len(rows))
	for _, row := range rows {
		marketType, err := domain.ParseMarketType(row.MarketType)
		if err != nil {
			return nil, fmt.Errorf("pgclv: leg %s: %w", row.LegID, err)
		}
		status, err := domain.ParseLegStatus(row.LegStatus)
		if err != nil {
			return nil, fmt.Errorf("pgclv: leg %s: %w", row.LegID, err)
		}
		if !row.GradedAt.Valid {
			// Unreachable through the query, whose predicate is status <> 'pending'
			// AND graded_at >= $1. Guarded because the alternative is a zero
			// instant travelling as a real one, and clv.Leg.validate would then
			// reject the leg as unusable rather than this adapter reporting the
			// column that was actually wrong.
			return nil, fmt.Errorf("pgclv: leg %s is %s with a null graded_at",
				row.LegID, row.LegStatus)
		}

		legs = append(legs, clv.Leg{
			LegID:       row.LegID,
			WagerID:     row.WagerID,
			UserID:      row.UserID,
			EventID:     row.EventID,
			MarketID:    row.MarketID,
			MarketType:  marketType,
			SelectionID: row.SelectionID,
			Book:        row.PriceBookID,
			Decimal:     row.PriceDecimal,
			// The leg's booked quote instant, which is the as-of bound of the
			// taken snapshot and the instant the reconstruction has to match.
			ObservedAt: row.PriceObservedAt.UTC(),
			Status:     status,
			GradedAt:   row.GradedAt.Time.UTC(),
		})
	}
	return legs, nil
}

// WriteLegCLV persists one measurement.
//
// It is an UPSERT on leg_id, which is the whole primary key: a leg has exactly
// one placement price and exactly one close, so replay idempotency is free here
// in a way it is not for the three signal tables, and there is no fingerprint to
// compute. DO UPDATE rather than DO NOTHING is deliberate and is migration
// 00009's rule for every derived table: a recomputation after a fix is the
// CORRECTION and must land.
//
// It writes a row only for a measurement that odds.EvaluateCLV actually produced.
// The four — now six — cases that produce no measurement never reach here,
// because clv.Measure returns an error for them and the caller writes nothing.
// migrations/00009 is explicit about why a row of nulls would be worse than no
// row: "'we could not measure it' and 'it measured zero' must not share a shape,
// or the leaderboard cannot tell them apart."
func (s *Store) WriteLegCLV(ctx context.Context, m clv.Measurement) error {
	params, err := upsertParams(m)
	if err != nil {
		return err
	}
	if err := s.q.UpsertWagerLegCLV(ctx, params); err != nil {
		return fmt.Errorf("pgclv: write CLV for leg %s: %w", m.Leg.LegID, err)
	}
	return nil
}

// upsertParams projects a measurement onto the generated parameter struct.
//
// Every value is COPIED from the measurement rather than recomputed. The
// arithmetic already went through odds.EvaluateCLV, which refused to produce a
// result that was not a like-for-like comparison, and re-deriving anything here
// would be a second implementation of a rule that has already been enforced —
// with the second one being the one that would be wrong. The two booleans are the
// exception and they are not re-derivations: `voided` and the devig method's
// string form are projections of values the domain holds in a different shape,
// and migrations/00009 asserts both identities in SQL on arrival.
func upsertParams(m clv.Measurement) (gen.UpsertWagerLegCLVParams, error) {
	if !m.DevigMethod.Valid() {
		// wager_leg_clv_devig_method_defined would refuse "unknown" at the
		// database, as a constraint violation with no leg identifier on it. Caught
		// here so the error names the row.
		return gen.UpsertWagerLegCLVParams{}, fmt.Errorf("pgclv: leg %s: devig method %d: %w",
			m.Leg.LegID, uint8(m.DevigMethod), odds.ErrUnknownDevigMethod)
	}
	r := m.Result
	return gen.UpsertWagerLegCLVParams{
		LegID:       m.Leg.LegID,
		WagerID:     m.Leg.WagerID,
		UserID:      m.Leg.UserID,
		MarketID:    m.Leg.MarketID,
		MarketType:  m.Leg.MarketType.String(),
		SelectionID: m.Leg.SelectionID,
		LeagueID:    m.LeagueID,
		TakenBookID: r.TakenBook,
		// Equal to TakenBookID under the same-book rule, and stored separately
		// because the column exists to survive that rule changing.
		ClosingBookID:  r.ClosingBook,
		DevigMethod:    m.DevigMethod.String(),
		TakenLine:      float8From(r.Line),
		ClosingLine:    float8From(r.ClosingLine),
		TakenAt:        r.TakenAt.UTC(),
		ClosedAt:       r.ClosedAt.UTC(),
		TakenFair:      float64(r.TakenFair),
		ClosingFair:    float64(r.ClosingFair),
		TakenPrice:     r.TakenPrice,
		ClosingPrice:   r.ClosingPrice,
		ProbabilityClv: r.ProbabilityCLV,
		PercentClv:     r.PercentCLV,
		Magnitude:      r.Magnitude,
		BeatClose:      r.Beat,
		LineMoved:      r.LineMoved,
		LegStatus:      m.Leg.Status.String(),
		// voided = (leg_status = 'void'), asserted by
		// wager_leg_clv_voided_identity. A PUSH is NOT void.
		Voided:     m.Voided(),
		GradedAt:   m.Leg.GradedAt.UTC(),
		ComputedAt: m.ComputedAt.UTC(),
	}, nil
}

// -----------------------------------------------------------------------------
// Column translation
// -----------------------------------------------------------------------------

// lineFrom turns a nullable DOUBLE PRECISION into a [domain.Line].
//
// NULL is domain.NoLine(); 0.0 is a stored PICK'EM, which is a real traded value
// and not an absent one. Collapsing the two would make every pick'em spread look
// unlined, and an unlined spread compared against another unlined spread would
// report "the line did not move" on a market that has no line at all.
//
// A non-finite stored value is impossible — migrations/00003 constrains
// prices.line to be finite — and is mapped to absent rather than propagated,
// because domain.NewLine would reject it and a diagnostic column must never be
// able to fail a measurement that the rest of the row supports. It cannot pass
// silently either: an absent line where the market type demands one fails
// marketLine's agreement check or migration 00009's line rule on arrival.
func lineFrom(v pgtype.Float8) domain.Line {
	if !v.Valid {
		return domain.NoLine()
	}
	line, err := domain.NewLine(v.Float64)
	if err != nil {
		return domain.NoLine()
	}
	return line
}

// float8From is lineFrom's inverse: an absent line is SQL NULL, a present one is
// its value including 0.0.
func float8From(l domain.Line) pgtype.Float8 {
	v, ok := l.Value()
	if !ok {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: v, Valid: true}
}

// validSnapshotRequest re-checks the bounds at the boundary.
//
// NotBefore is REQUIRED, and a zero value is refused rather than treated as "no
// bound". clv/doc.go §2 gives both reasons: without it the backward walk consults
// every chunk the hypertable has ever had, and a quote from six days out is not a
// closing line. A caller that forgot it would get a correct-looking answer and a
// query plan that degrades silently as the table grows, which is the worst
// combination available.
func validSnapshotRequest(req clv.SnapshotRequest) error {
	switch {
	case req.Market.IsZero():
		return fmt.Errorf("%w: snapshot request has no market", clv.ErrInvalidOptions)
	case req.Book.IsZero():
		return fmt.Errorf("%w: snapshot request for market %s has no book",
			clv.ErrInvalidOptions, req.Market)
	case req.AsOf.IsZero():
		return fmt.Errorf("%w: snapshot request for market %s has no as-of instant",
			clv.ErrInvalidOptions, req.Market)
	case req.NotBefore.IsZero():
		return fmt.Errorf("%w: snapshot request for market %s has no lower bound; an unbounded "+
			"read of the prices hypertable consults every chunk that has ever existed",
			clv.ErrInvalidOptions, req.Market)
	case !req.NotBefore.Before(req.AsOf):
		return fmt.Errorf("%w: snapshot request for market %s bounds [%s, %s] admit nothing",
			clv.ErrInvalidOptions, req.Market,
			req.NotBefore.UTC().Format(time.RFC3339Nano),
			req.AsOf.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// int32Limit narrows a batch size onto the column's range, refusing a
// non-positive one.
//
// LIMIT 0 returns no rows, which a caller would read as "nothing to measure" —
// indistinguishable from a drained queue, and permanent.
func int32Limit(n int) (int32, error) {
	if n <= 0 {
		return 0, fmt.Errorf("%w: batch limit %d must be positive", clv.ErrInvalidOptions, n)
	}
	if n > math.MaxInt32 {
		return 0, fmt.Errorf("%w: batch limit %d exceeds the column's range",
			clv.ErrInvalidOptions, n)
	}
	return int32(n), nil
}
