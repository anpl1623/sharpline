package pgstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/analytics"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// Store is internal/analytics's Postgres adapter.
//
// It holds the pool — for [postgres.DB.InTx], which is how the arbitrage write
// reaches its three statements — and a *gen.Queries bound to that pool for the
// two single-statement writes.
//
// There is no other state, and in particular no cache and no prepared-statement
// bookkeeping of its own: pgx already caches statements per connection, and a
// value held here would be a value that can be stale in a package whose entire
// job is to record what was true at a particular instant.
type Store struct {
	db *postgres.DB
	q  *gen.Queries
}

// New builds the adapter. This is what the composition root wires.
func New(db *postgres.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: pgstore needs a database", analytics.ErrInvalidOptions)
	}
	return &Store{db: db, q: gen.New(db.Pool())}, nil
}

// Compile-time proof that this adapter satisfies the port it claims. It is here
// rather than at the call site so that a port growing a method fails THIS
// package's build rather than failing at wire-up.
var _ analytics.Store = (*Store)(nil)

// RecordEVSignal implements [analytics.Store].
//
// One statement, upserting on ev_signals' natural key
// (selection_id, book_id, quote_observed_at). DO UPDATE rather than DO NOTHING,
// because a replay after a threshold change or a detector fix IS THE CORRECTION
// and must land — the query's own comment in analytics.sql labours the point, and
// `created_at` is deliberately absent from its SET list so that an original
// computation stays distinguishable from a later recomputation of it.
//
// Not wrapped in a transaction: a single statement runs inside the implicit one
// Postgres opens for it anyway, and an explicit BEGIN/COMMIT around it would buy
// two extra round trips and the appearance of an atomicity guarantee that was
// already there.
func (s *Store) RecordEVSignal(ctx context.Context, sig analytics.EVSignal) error {
	err := s.q.UpsertEVSignal(ctx, gen.UpsertEVSignalParams{
		SelectionID:          sig.SelectionID,
		MarketID:             sig.MarketID,
		MarketType:           sig.MarketType,
		LeagueID:             sig.LeagueID,
		BookID:               sig.BookID,
		ReferenceBookID:      sig.ReferenceBookID,
		DevigMethod:          sig.DevigMethod,
		OfferedDecimal:       sig.OfferedDecimal,
		OfferedImplied:       sig.OfferedImplied,
		Line:                 lineParam(sig.Line),
		FairProbability:      sig.FairProbability,
		FairDecimal:          sig.FairDecimal,
		ExpectedValue:        sig.ExpectedValue,
		ExpectedValuePercent: sig.ExpectedValuePercent,
		Edge:                 sig.Edge,
		EdgePercent:          sig.EdgePercent,
		Kelly:                sig.Kelly,
		FractionalKelly:      sig.FractionalKelly,
		KellyFraction:        sig.KellyFraction,
		QuoteObservedAt:      sig.QuoteObservedAt,
		QuoteAgeSeconds:      sig.QuoteAgeSeconds,
		ThresholdEvPercent:   sig.ThresholdEVPercent,
		MaxQuoteAgeSeconds:   sig.MaxQuoteAgeSeconds,
		DetectedAt:           sig.DetectedAt,
	})
	if err != nil {
		return writeErr("ev", err)
	}
	return nil
}

// RecordArbitrageSignal implements [analytics.Store].
//
// # Three statements, one transaction, in this order
//
//  1. UPSERT the parent and take back its id. The id is a server-generated UUID
//     the caller cannot predict, and on the UPDATE path RETURNING gives back the
//     EXISTING id — which is the whole reason the delete-then-reinsert below
//     works across a recomputation rather than orphaning a previous leg set under
//     a dead identifier.
//  2. DELETE every leg. A recomputation REPLACES the outcome set rather than
//     merging into it: if a book dropped out of the group, its leg must go, and a
//     merge would leave a leg the finding no longer contains.
//  3. UPSERT each leg.
//
// All three inside one [postgres.DB.InTx], because a finding and its outcome set
// are one fact — see doc.go. Step 3 is an upsert rather than a plain insert even
// though step 2 has just cleared the set, which costs nothing on a table that
// sees a handful of rows per finding and keeps the statement correct on its own
// terms rather than only in the presence of its neighbour.
func (s *Store) RecordArbitrageSignal(ctx context.Context, sig analytics.ArbitrageSignal) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.q.WithTx(tx)

		id, err := q.UpsertArbitrageSignal(ctx, gen.UpsertArbitrageSignalParams{
			MarketID:                 sig.MarketID,
			MarketType:               sig.MarketType,
			LeagueID:                 sig.LeagueID,
			Line:                     lineParam(sig.Line),
			SelectionCount:           int32(sig.SelectionCount),
			ImpliedSum:               sig.ImpliedSum,
			ReturnFraction:           sig.ReturnFraction,
			DistinctBooks:            int32(sig.DistinctBooks),
			ObservedSpreadSeconds:    sig.ObservedSpreadSeconds,
			OldestLegAgeSeconds:      sig.OldestLegAgeSeconds,
			ObservedAt:               sig.ObservedAt,
			LegsFingerprint:          sig.LegsFingerprint,
			MaxLegAgeSeconds:         sig.MaxLegAgeSeconds,
			MaxObservedSpreadSeconds: sig.MaxObservedSpreadSeconds,
			DetectedAt:               sig.DetectedAt,
		})
		if err != nil {
			return writeErr("arbitrage", err)
		}

		if err := q.DeleteArbitrageSignalLegs(ctx, id); err != nil {
			return writeErr("arbitrage legs (delete)", err)
		}
		for _, leg := range sig.Legs {
			err := q.UpsertArbitrageSignalLeg(ctx, gen.UpsertArbitrageSignalLegParams{
				SignalID:      id,
				LegIndex:      int32(leg.LegIndex),
				SelectionID:   leg.SelectionID,
				Role:          leg.Role,
				BookID:        leg.BookID,
				DecimalOdds:   leg.DecimalOdds,
				Line:          lineParam(leg.Line),
				StakeFraction: leg.StakeFraction,
				ObservedAt:    leg.ObservedAt,
				AgeSeconds:    leg.AgeSeconds,
			})
			if err != nil {
				return writeErr(fmt.Sprintf("arbitrage leg %d", leg.LegIndex), err)
			}
		}
		return nil
	})
}

// RecordSteamSignal implements [analytics.Store].
//
// One statement, upserting on steam_signals' natural key
// (market_id, selection_id, window_start, window_end). The window bounds are part
// of the key because the same selection steaming twice in one session is two
// findings, and because a hopping window's identity IS its bounds.
//
// The followers are marshalled here rather than in internal/analytics because
// JSONB is a storage detail: the detector produces a slice of values, and this
// package knows that the column wants bytes.
func (s *Store) RecordSteamSignal(ctx context.Context, sig analytics.SteamSignal) error {
	followers, err := marshalFollowers(sig.Followers)
	if err != nil {
		return err
	}
	err = s.q.UpsertSteamSignal(ctx, gen.UpsertSteamSignalParams{
		MarketID:                     sig.MarketID,
		MarketType:                   sig.MarketType,
		LeagueID:                     sig.LeagueID,
		SelectionID:                  sig.SelectionID,
		WindowStart:                  sig.WindowStart,
		WindowEnd:                    sig.WindowEnd,
		WindowSeconds:                sig.WindowSeconds,
		HopSeconds:                   sig.HopSeconds,
		Direction:                    sig.Direction,
		DeltaProbability:             sig.DeltaProbability,
		MagnitudeProbabilityPoints:   sig.MagnitudeProbabilityPoints,
		VelocityProbabilityPerMinute: sig.VelocityProbabilityPerMinute,
		DevigMethod:                  sig.DevigMethod,
		LeadBookID:                   sig.LeadBookID,
		LeadMovedAt:                  sig.LeadMovedAt,
		Followers:                    followers,
		FollowerCount:                int32(sig.FollowerCount),
		ParticipatingBooks:           int32(sig.ParticipatingBooks),
		CrossBookCorrelation:         sig.CrossBookCorrelation,
		ThresholdVelocity:            sig.ThresholdVelocity,
		ThresholdMagnitude:           sig.ThresholdMagnitude,
		ThresholdCorrelation:         sig.ThresholdCorrelation,
		MinFollowers:                 int32(sig.MinFollowers),
		MaxFollowerLagSeconds:        sig.MaxFollowerLagSeconds,
		DetectedAt:                   sig.DetectedAt,
	})
	if err != nil {
		return writeErr("steam", err)
	}
	return nil
}

// marshalFollowers renders the follower list for the JSONB column.
//
// # An empty list must never reach the column, and this is where that is caught
//
// migrations/00009 CHECKs follower_count ≥ 1 and
// follower_count = jsonb_array_length(followers), so an empty array is a row the
// database refuses. internal/analytics refuses it one layer earlier — a finding
// with no follower is one book's move and fails the MinFollowers gate — so
// reaching here with an empty slice means the two have drifted, and the error
// says so rather than deferring to a constraint violation whose message names a
// column instead of a rule.
//
// A nil slice is deliberately NOT rendered as JSON `null`. encoding/json marshals
// a nil slice to `null`, which would satisfy the column's type and fail its
// `jsonb_typeof = 'array'` check with a message about types rather than about
// followers.
func marshalFollowers(fs []analytics.SteamFollower) ([]byte, error) {
	if len(fs) == 0 {
		return nil, fmt.Errorf("%w: a steam finding reached the store with no followers; "+
			"migrations/00009 CHECKs follower_count >= 1 and internal/analytics's MinFollowers "+
			"gate refuses it, so the two have drifted", analytics.ErrInvalidConfig)
	}
	b, err := json.Marshal(fs)
	if err != nil {
		return nil, fmt.Errorf("analytics pgstore: marshal steam followers: %w", err)
	}
	return b, nil
}

// lineParam turns a [domain.Line] into the nullable DOUBLE PRECISION the column
// is.
//
// A line of ZERO and an ABSENT line are different facts — a pick 'em spread is a
// real handicap and a moneyline has none — and domain.Line is a
// presence-carrying struct rather than a *float64 precisely so the two cannot be
// confused. This is the one place where that distinction becomes a NULL, and
// getting it backwards would write `line = 0` on every moneyline, which
// migrations/00009's line rule would then reject.
//
// The inverse translation lives in the packages that READ these tables; it is not
// here because nothing in this package reads.
func lineParam(l domain.Line) pgtype.Float8 {
	v, ok := l.Value()
	return pgtype.Float8{Float64: v, Valid: ok}
}

// writeErr wraps a failed write with the kind of finding that failed and with
// the SQLSTATE, and says what a constraint violation MEANS here.
//
// internal/analytics validates every finding against the same CHECK constraints
// before calling this package, so a 23514 (check violation) reaching Postgres is
// not an ordinary bad input — it is a disagreement between a detector and
// migrations/00009 about what a finding may look like. That is worth saying in
// the error text, because the alternative is an operator reading
// `violates check constraint "steam_signals_meets_own_thresholds"` and having to
// work out from first principles which of the two halves is wrong.
//
// A 23503 (foreign key) is deliberately NOT filed under that heading. It is the
// catalogue-lag race, it is transient, and calling it a detector bug sends an
// operator looking for a defect that is not there — which is exactly what it did
// on the first cold start after phase 9 landed. See [analytics.ErrCatalogueLag].
func writeErr(kind string, err error) error {
	state := postgres.SQLState(err)

	// A lost lock-ordering race is not a schema disagreement and must not be
	// reported as one. 40P01 and 40001 both GUARANTEE the transaction was rolled
	// back — Postgres chose this backend as the victim and undid it — so nothing
	// was written and the caller may run it again. [analytics.ErrContended]
	// carries the whole argument for why this package classifies and
	// internal/analytics decides; postgres.IsSerializationFailure exists for
	// exactly this and refuses to do the retrying itself for the same reason.
	if postgres.IsSerializationFailure(err) {
		return fmt.Errorf("analytics pgstore: write %s signal (SQLSTATE %s): %w: %w",
			kind, state, analytics.ErrContended, err)
	}

	switch state {
	// A FOREIGN KEY violation is the one constraint failure that is NOT a
	// disagreement with the schema, and separating it from its neighbours is the
	// whole point of this switch having three arms rather than two. Every foreign
	// key on the phase-9 tables points at the CATALOGUE — leagues, books, markets,
	// selections — which `ingest` writes and this process only reads, so a 23503
	// means the parent has not committed yet rather than that a detector produced
	// something illegal. internal/analytics could not have caught it: it validates
	// findings against the CHECK constraints, and it has no view of the catalogue
	// to validate a reference against. [analytics.ErrCatalogueLag] carries the
	// topology that produces the gap and what actually recovers the finding.
	case "23503":
		return fmt.Errorf("analytics pgstore: write %s signal (SQLSTATE %s): %w: %w",
			kind, state, analytics.ErrCatalogueLag, err)
	case "23514", "23505":
		return fmt.Errorf("analytics pgstore: write %s signal rejected by the schema (SQLSTATE %s); "+
			"internal/analytics validates against the same constraints before calling, so this is a "+
			"drift between the detector and migrations/00009 rather than a bad record: %w",
			kind, state, err)
	default:
		return fmt.Errorf("analytics pgstore: write %s signal: %w", kind, err)
	}
}
