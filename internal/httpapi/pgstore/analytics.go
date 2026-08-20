package pgstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// The phase 9 analytics reads: +EV, arbitrage, steam, CLV and the leaderboard.
//
// This file does the three jobs pgstore.go's header names and nothing else:
// translate the row shape into the read model, parse enums at the boundary, and
// classify absence. It is READ-ONLY throughout — every row it serves was written
// by `pricer` or by `settle`, and there is no INSERT or UPDATE against
// `ev_signals`, `arbitrage_signals`, `steam_signals` or `wager_leg_clv` here nor
// any path to one.
//
// # Why the row types are collapsed into one shape per feed
//
// sqlc emits a distinct row struct per statement, so the +EV feed alone arrives
// as four structurally identical types — {cross-league, one league} x {first
// page, after cursor} — and the steam and CLV feeds as two each. The alternative
// to collapsing them is four copies of the same twenty-four-field conversion,
// which is four places for a field to be forgotten and only one of them to be
// noticed. The `*Columns` structs below are the same device
// [pgstore.eventColumns] already uses for the four board statements, and for the
// same reason: WHICH statement ran is a planning decision, not a shape.
//
// # Where the enum parsing happens, and why it is here
//
// sqlc.yaml keeps enum columns as `string` and requires the conversion to go
// through the domain's own ParseX functions, "each of which returns an error for
// an unrecognised value, so a schema/Go divergence surfaces as a wrapped error
// at the read, not as a silent zero value". Every conversion below does that,
// including the two enums this package owns rather than the domain
// ([httpapi.ParseSteamDirection]) and the one that has a legal absent member
// (steam's `devig_method: 'none'`).

// Compile-time proof that this adapter satisfies the three phase 9 ports. If a
// port grows a method, this file fails to build rather than failing at wire-up.
var (
	_ httpapi.Signals     = (*Store)(nil)
	_ httpapi.CLV         = (*Store)(nil)
	_ httpapi.Leaderboard = (*Store)(nil)
)

// -----------------------------------------------------------------------------
// +EV
// -----------------------------------------------------------------------------

// evColumns is the shape the four +EV statements share.
type evColumns struct {
	SelectionID          domain.SelectionID
	MarketID             domain.MarketID
	MarketType           string
	LeagueID             domain.LeagueID
	BookID               domain.BookID
	ReferenceBookID      domain.BookID
	DevigMethod          string
	OfferedDecimal       odds.Decimal
	OfferedImplied       float64
	Line                 pgtype.Float8
	FairProbability      float64
	FairDecimal          odds.Decimal
	ExpectedValue        float64
	ExpectedValuePercent float64
	Edge                 float64
	EdgePercent          float64
	Kelly                float64
	FractionalKelly      float64
	KellyFraction        float64
	QuoteObservedAt      time.Time
	QuoteAgeSeconds      float64
	ThresholdEvPercent   float64
	MaxQuoteAgeSeconds   float64
	DetectedAt           time.Time
}

// EVSignals runs one of the four keyset +EV statements.
//
// WHICH statement is chosen by two booleans — league or not, cursor or not — and
// the four are separate queries rather than one with nullable parameters because
// `(@after IS NULL OR (a, b) > (@after_a, @after_b))` is not sargable: the OR
// defeats the index the whole ranking depends on. [Store.EventPage] makes the
// same split for the same reason.
//
// It asks for Limit+1 rows and reports HasMore from whether it got them. One
// extra row beats a second COUNT(*), and a count over a continuously-written set
// is stale before it is serialised.
func (s *Store) EVSignals(ctx context.Context, q httpapi.EVSignalQuery) (httpapi.EVSignalPage, error) {
	fetch := q.Limit + 1
	books := stringsOf(q.Books)
	types := marketTypeStrings(q.MarketTypes)

	var (
		cols []evColumns
		err  error
	)
	switch {
	case !q.LeagueID.IsZero() && q.After != nil:
		var rows []gen.ListLeagueEVSignalsAfterCursorRow
		rows, err = s.q.ListLeagueEVSignalsAfterCursor(ctx, gen.ListLeagueEVSignalsAfterCursorParams{
			LeagueID:             q.LeagueID,
			ObservedAfter:        q.ObservedAfter,
			MinEvPercent:         q.MinEVPercent,
			BookIds:              books,
			MarketTypes:          types,
			AfterEvPercent:       q.After.ExpectedValuePercent,
			AfterQuoteObservedAt: q.After.QuoteObservedAt,
			AfterSelectionID:     q.After.SelectionID.String(),
			AfterBookID:          q.After.BookID.String(),
			RowLimit:             fetch,
		})
		cols = mapRows(rows, func(r gen.ListLeagueEVSignalsAfterCursorRow) evColumns { return evColumns(r) })

	case !q.LeagueID.IsZero():
		var rows []gen.ListLeagueEVSignalsFirstPageRow
		rows, err = s.q.ListLeagueEVSignalsFirstPage(ctx, gen.ListLeagueEVSignalsFirstPageParams{
			LeagueID:      q.LeagueID,
			ObservedAfter: q.ObservedAfter,
			MinEvPercent:  q.MinEVPercent,
			BookIds:       books,
			MarketTypes:   types,
			RowLimit:      fetch,
		})
		cols = mapRows(rows, func(r gen.ListLeagueEVSignalsFirstPageRow) evColumns { return evColumns(r) })

	case q.After != nil:
		var rows []gen.ListEVSignalsAfterCursorRow
		rows, err = s.q.ListEVSignalsAfterCursor(ctx, gen.ListEVSignalsAfterCursorParams{
			ObservedAfter:        q.ObservedAfter,
			MinEvPercent:         q.MinEVPercent,
			BookIds:              books,
			MarketTypes:          types,
			AfterEvPercent:       q.After.ExpectedValuePercent,
			AfterQuoteObservedAt: q.After.QuoteObservedAt,
			AfterSelectionID:     q.After.SelectionID.String(),
			AfterBookID:          q.After.BookID.String(),
			RowLimit:             fetch,
		})
		cols = mapRows(rows, func(r gen.ListEVSignalsAfterCursorRow) evColumns { return evColumns(r) })

	default:
		var rows []gen.ListEVSignalsFirstPageRow
		rows, err = s.q.ListEVSignalsFirstPage(ctx, gen.ListEVSignalsFirstPageParams{
			ObservedAfter: q.ObservedAfter,
			MinEvPercent:  q.MinEVPercent,
			BookIds:       books,
			MarketTypes:   types,
			RowLimit:      fetch,
		})
		cols = mapRows(rows, func(r gen.ListEVSignalsFirstPageRow) evColumns { return evColumns(r) })
	}
	if err != nil {
		return httpapi.EVSignalPage{}, fmt.Errorf("list ev signals: %w", err)
	}

	hasMore := int32(len(cols)) > q.Limit
	if hasMore {
		cols = cols[:q.Limit]
	}

	out := make([]httpapi.EVSignal, 0, len(cols))
	for _, c := range cols {
		sig, err := evSignalFrom(c)
		if err != nil {
			return httpapi.EVSignalPage{}, fmt.Errorf("list ev signals: %w", err)
		}
		out = append(out, sig)
	}
	return httpapi.EVSignalPage{Signals: out, HasMore: hasMore}, nil
}

func evSignalFrom(c evColumns) (httpapi.EVSignal, error) {
	mtype, err := domain.ParseMarketType(c.MarketType)
	if err != nil {
		return httpapi.EVSignal{}, fmt.Errorf("ev signal on selection %s: %w", c.SelectionID, err)
	}
	method, err := odds.ParseDevigMethod(c.DevigMethod)
	if err != nil {
		return httpapi.EVSignal{}, fmt.Errorf("ev signal on selection %s: %w", c.SelectionID, err)
	}
	return httpapi.EVSignal{
		SelectionID:     c.SelectionID,
		MarketID:        c.MarketID,
		MarketType:      mtype,
		LeagueID:        c.LeagueID,
		BookID:          c.BookID,
		ReferenceBookID: c.ReferenceBookID,

		DevigMethod:    method,
		OfferedDecimal: c.OfferedDecimal,
		OfferedImplied: c.OfferedImplied,
		Line:           float8Ptr(c.Line),

		FairProbability: c.FairProbability,
		FairDecimal:     c.FairDecimal,

		ExpectedValue:        c.ExpectedValue,
		ExpectedValuePercent: c.ExpectedValuePercent,
		Edge:                 c.Edge,
		EdgePercent:          c.EdgePercent,
		Kelly:                c.Kelly,
		FractionalKelly:      c.FractionalKelly,
		KellyFraction:        c.KellyFraction,

		QuoteObservedAt: c.QuoteObservedAt.UTC(),
		QuoteAge:        durationFromSeconds(c.QuoteAgeSeconds),

		ThresholdEVPercent: c.ThresholdEvPercent,
		MaxQuoteAge:        durationFromSeconds(c.MaxQuoteAgeSeconds),
		DetectedAt:         c.DetectedAt.UTC(),
	}, nil
}

// -----------------------------------------------------------------------------
// Arbitrage
// -----------------------------------------------------------------------------

// arbColumns is the shape the two arbitrage statements share.
type arbColumns struct {
	ID                       pgtype.UUID
	MarketID                 domain.MarketID
	MarketType               string
	LeagueID                 domain.LeagueID
	Line                     pgtype.Float8
	SelectionCount           int32
	ImpliedSum               float64
	ReturnFraction           float64
	DistinctBooks            int32
	ObservedSpreadSeconds    float64
	OldestLegAgeSeconds      float64
	ObservedAt               time.Time
	LegsFingerprint          string
	MaxLegAgeSeconds         float64
	MaxObservedSpreadSeconds float64
	DetectedAt               time.Time
}

// ArbitrageSignals returns the live arbitrage set with each finding's legs
// attached.
//
// TWO QUERIES, NOT N+1. The findings come back first, then every leg of every
// finding in one round trip ordered by (signal_id, leg_index) — so grouping is a
// single forward pass with no map and the legs arrive already in display order.
// A page of twenty findings served one leg query at a time would be twenty-one
// round trips whose per-call overhead dominates a query that is otherwise a
// bounded index scan.
//
// A finding with no legs cannot occur — `arbitrage_signal_legs` is written in
// the same transaction as its parent and the parent's `selection_count` is
// checked against it — but this function does not assume so: a finding whose
// legs are missing comes back with an empty slice rather than being dropped,
// because silently hiding a row is how a data problem becomes invisible.
func (s *Store) ArbitrageSignals(ctx context.Context, q httpapi.ArbitrageQuery) ([]httpapi.ArbitrageSignal, error) {
	types := marketTypeStrings(q.MarketTypes)

	var (
		cols []arbColumns
		err  error
	)
	if q.LeagueID.IsZero() {
		var rows []gen.ListLiveArbitrageSignalsRow
		rows, err = s.q.ListLiveArbitrageSignals(ctx, gen.ListLiveArbitrageSignalsParams{
			ObservedAfter:     q.ObservedAfter,
			MaxLegAgeSeconds:  q.MaxLegAge.Seconds(),
			MaxSpreadSeconds:  q.MaxObservedSpread.Seconds(),
			MinReturnFraction: q.MinReturnFraction,
			MinDistinctBooks:  q.MinDistinctBooks,
			MarketTypes:       types,
			RowLimit:          q.Limit,
		})
		cols = mapRows(rows, func(r gen.ListLiveArbitrageSignalsRow) arbColumns { return arbColumns(r) })
	} else {
		var rows []gen.ListLeagueLiveArbitrageSignalsRow
		rows, err = s.q.ListLeagueLiveArbitrageSignals(ctx, gen.ListLeagueLiveArbitrageSignalsParams{
			LeagueID:          q.LeagueID,
			ObservedAfter:     q.ObservedAfter,
			MaxLegAgeSeconds:  q.MaxLegAge.Seconds(),
			MaxSpreadSeconds:  q.MaxObservedSpread.Seconds(),
			MinReturnFraction: q.MinReturnFraction,
			MinDistinctBooks:  q.MinDistinctBooks,
			MarketTypes:       types,
			RowLimit:          q.Limit,
		})
		cols = mapRows(rows, func(r gen.ListLeagueLiveArbitrageSignalsRow) arbColumns { return arbColumns(r) })
	}
	if err != nil {
		return nil, fmt.Errorf("list arbitrage signals: %w", err)
	}
	if len(cols) == 0 {
		// An empty set is a correct and frequent answer on this feed, and it is
		// returned as an empty slice rather than by asking the database for the
		// legs of nothing.
		return nil, nil
	}

	ids := make([]pgtype.UUID, 0, len(cols))
	for _, c := range cols {
		ids = append(ids, c.ID)
	}
	legRows, err := s.q.ListArbitrageSignalLegs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list arbitrage signal legs: %w", err)
	}

	legsBySignal := make(map[string][]httpapi.ArbitrageLeg, len(cols))
	for _, r := range legRows {
		role, err := domain.ParseSelectionRole(r.Role)
		if err != nil {
			return nil, fmt.Errorf("arbitrage leg on selection %s: %w", r.SelectionID, err)
		}
		id := uuidString(r.SignalID)
		legsBySignal[id] = append(legsBySignal[id], httpapi.ArbitrageLeg{
			Index:         r.LegIndex,
			SelectionID:   r.SelectionID,
			Role:          role,
			BookID:        r.BookID,
			DecimalOdds:   r.DecimalOdds,
			Line:          float8Ptr(r.Line),
			StakeFraction: r.StakeFraction,
			ObservedAt:    r.ObservedAt.UTC(),
			Age:           durationFromSeconds(r.AgeSeconds),
		})
	}

	out := make([]httpapi.ArbitrageSignal, 0, len(cols))
	for _, c := range cols {
		mtype, err := domain.ParseMarketType(c.MarketType)
		if err != nil {
			return nil, fmt.Errorf("arbitrage signal on market %s: %w", c.MarketID, err)
		}
		id := uuidString(c.ID)
		out = append(out, httpapi.ArbitrageSignal{
			ID:         id,
			MarketID:   c.MarketID,
			MarketType: mtype,
			LeagueID:   c.LeagueID,
			Line:       float8Ptr(c.Line),

			SelectionCount: c.SelectionCount,
			ImpliedSum:     c.ImpliedSum,
			ReturnFraction: c.ReturnFraction,
			DistinctBooks:  c.DistinctBooks,

			ObservedSpread: durationFromSeconds(c.ObservedSpreadSeconds),
			OldestLegAge:   durationFromSeconds(c.OldestLegAgeSeconds),
			ObservedAt:     c.ObservedAt.UTC(),

			MaxLegAge:         durationFromSeconds(c.MaxLegAgeSeconds),
			MaxObservedSpread: durationFromSeconds(c.MaxObservedSpreadSeconds),
			DetectedAt:        c.DetectedAt.UTC(),

			Legs: legsBySignal[id],
		})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Steam
// -----------------------------------------------------------------------------

// steamColumns is the shape the two steam feed statements share.
type steamColumns struct {
	MarketID                     domain.MarketID
	MarketType                   string
	LeagueID                     domain.LeagueID
	SelectionID                  domain.SelectionID
	WindowStart                  time.Time
	WindowEnd                    time.Time
	WindowSeconds                float64
	HopSeconds                   float64
	Direction                    string
	DeltaProbability             float64
	MagnitudeProbabilityPoints   float64
	VelocityProbabilityPerMinute float64
	DevigMethod                  string
	LeadBookID                   domain.BookID
	LeadMovedAt                  time.Time
	Followers                    []byte
	FollowerCount                int32
	ParticipatingBooks           int32
	CrossBookCorrelation         float64
	ThresholdVelocity            float64
	ThresholdMagnitude           float64
	ThresholdCorrelation         float64
	MinFollowers                 int32
	MaxFollowerLagSeconds        float64
	DetectedAt                   time.Time
}

// steamFollowerJSON is the element shape of `steam_signals.followers`.
//
// It is a JSONB array rather than a child table because a follower list is a
// PART OF the finding and is only ever read whole with it — there is no query
// that asks "which findings did book X follow" that a child table would serve
// better than a GIN index would, and a child table would make the write two
// statements where the replay key demands one atomic upsert.
//
// The field names are the writer's contract (migration 00009 states it) and are
// not FK-enforced inside the JSONB, so this decoder validates what it can: an
// unparseable element is an error at the read rather than a zero-valued
// follower on a display.
type steamFollowerJSON struct {
	BookID           string  `json:"book_id"`
	MovedAt          string  `json:"moved_at"`
	LagSeconds       float64 `json:"lag_seconds"`
	DeltaProbability float64 `json:"delta_probability"`
}

// SteamSignals runs one of the two keyset steam statements.
func (s *Store) SteamSignals(ctx context.Context, q httpapi.SteamQuery) (httpapi.SteamSignalPage, error) {
	fetch := q.Limit + 1
	types := marketTypeStrings(q.MarketTypes)

	var (
		cols []steamColumns
		err  error
	)
	if q.After != nil {
		var rows []gen.ListSteamSignalsAfterCursorRow
		rows, err = s.q.ListSteamSignalsAfterCursor(ctx, gen.ListSteamSignalsAfterCursorParams{
			WindowEndAfter:        q.WindowEndAfter,
			MinMagnitude:          q.MinMagnitude,
			MinParticipatingBooks: q.MinParticipatingBooks,
			MarketTypes:           types,
			AfterWindowEnd:        q.After.WindowEnd,
			AfterMarketID:         q.After.MarketID.String(),
			AfterSelectionID:      q.After.SelectionID.String(),
			RowLimit:              fetch,
		})
		cols = mapRows(rows, func(r gen.ListSteamSignalsAfterCursorRow) steamColumns { return steamColumns(r) })
	} else {
		var rows []gen.ListSteamSignalsFirstPageRow
		rows, err = s.q.ListSteamSignalsFirstPage(ctx, gen.ListSteamSignalsFirstPageParams{
			WindowEndAfter:        q.WindowEndAfter,
			MinMagnitude:          q.MinMagnitude,
			MinParticipatingBooks: q.MinParticipatingBooks,
			MarketTypes:           types,
			RowLimit:              fetch,
		})
		cols = mapRows(rows, func(r gen.ListSteamSignalsFirstPageRow) steamColumns { return steamColumns(r) })
	}
	if err != nil {
		return httpapi.SteamSignalPage{}, fmt.Errorf("list steam signals: %w", err)
	}

	hasMore := int32(len(cols)) > q.Limit
	if hasMore {
		cols = cols[:q.Limit]
	}

	out := make([]httpapi.SteamSignal, 0, len(cols))
	for _, c := range cols {
		sig, err := steamSignalFrom(c)
		if err != nil {
			return httpapi.SteamSignalPage{}, fmt.Errorf("list steam signals: %w", err)
		}
		out = append(out, sig)
	}
	return httpapi.SteamSignalPage{Signals: out, HasMore: hasMore}, nil
}

func steamSignalFrom(c steamColumns) (httpapi.SteamSignal, error) {
	mtype, err := domain.ParseMarketType(c.MarketType)
	if err != nil {
		return httpapi.SteamSignal{}, fmt.Errorf("steam signal on market %s: %w", c.MarketID, err)
	}
	direction, err := httpapi.ParseSteamDirection(c.Direction)
	if err != nil {
		return httpapi.SteamSignal{}, fmt.Errorf("steam signal on market %s: %w", c.MarketID, err)
	}
	method, err := steamDevigMethod(c.DevigMethod)
	if err != nil {
		return httpapi.SteamSignal{}, fmt.Errorf("steam signal on market %s: %w", c.MarketID, err)
	}

	var raw []steamFollowerJSON
	if err := json.Unmarshal(c.Followers, &raw); err != nil {
		return httpapi.SteamSignal{}, fmt.Errorf("steam signal on market %s: decode followers: %w", c.MarketID, err)
	}
	followers := make([]httpapi.SteamFollower, 0, len(raw))
	for _, f := range raw {
		// Through the domain constructor, for the same reason a path parameter
		// is: the book id inside a JSONB document is NOT foreign-key enforced by
		// the database (a JSONB member cannot carry an FK), so this is the only
		// place that can refuse a value the rest of the system considers
		// impossible.
		book, err := domain.NewBookID(f.BookID)
		if err != nil {
			return httpapi.SteamSignal{}, fmt.Errorf("steam signal on market %s: follower book: %w", c.MarketID, err)
		}
		movedAt, err := time.Parse(time.RFC3339, f.MovedAt)
		if err != nil {
			return httpapi.SteamSignal{}, fmt.Errorf("steam signal on market %s: follower instant: %w", c.MarketID, err)
		}
		followers = append(followers, httpapi.SteamFollower{
			BookID:           book,
			MovedAt:          movedAt.UTC(),
			Lag:              durationFromSeconds(f.LagSeconds),
			DeltaProbability: f.DeltaProbability,
		})
	}

	return httpapi.SteamSignal{
		MarketID:    c.MarketID,
		MarketType:  mtype,
		LeagueID:    c.LeagueID,
		SelectionID: c.SelectionID,

		WindowStart: c.WindowStart.UTC(),
		WindowEnd:   c.WindowEnd.UTC(),
		Window:      durationFromSeconds(c.WindowSeconds),
		Hop:         durationFromSeconds(c.HopSeconds),

		Direction:        direction,
		DeltaProbability: c.DeltaProbability,
		Magnitude:        c.MagnitudeProbabilityPoints,
		Velocity:         c.VelocityProbabilityPerMinute,
		DevigMethod:      method,

		LeadBookID:  c.LeadBookID,
		LeadMovedAt: c.LeadMovedAt.UTC(),

		Followers:          followers,
		FollowerCount:      c.FollowerCount,
		ParticipatingBooks: c.ParticipatingBooks,

		CrossBookCorrelation: c.CrossBookCorrelation,

		ThresholdVelocity:    c.ThresholdVelocity,
		ThresholdMagnitude:   c.ThresholdMagnitude,
		ThresholdCorrelation: c.ThresholdCorrelation,
		MinFollowers:         c.MinFollowers,
		MaxFollowerLag:       durationFromSeconds(c.MaxFollowerLagSeconds),

		DetectedAt: c.DetectedAt.UTC(),
	}, nil
}

// steamDevigMethod parses the five-member column, where `none` is legal.
//
// nil is returned for `none` rather than a zero method, because
// odds.MethodUnknown means "unrecognised" and this value is recognised: the
// detector deliberately measured raw implied probability. Two different facts
// must not share a representation.
func steamDevigMethod(s string) (*odds.DevigMethod, error) {
	if s == "none" {
		return nil, nil
	}
	m, err := odds.ParseDevigMethod(s)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// -----------------------------------------------------------------------------
// Closing line value
// -----------------------------------------------------------------------------

// clvColumns is the shape the two CLV page statements share.
type clvColumns struct {
	LegID          domain.LegID
	WagerID        domain.WagerID
	MarketID       domain.MarketID
	MarketType     string
	SelectionID    domain.SelectionID
	LeagueID       domain.LeagueID
	TakenBookID    domain.BookID
	ClosingBookID  domain.BookID
	DevigMethod    string
	TakenLine      pgtype.Float8
	ClosingLine    pgtype.Float8
	TakenAt        time.Time
	ClosedAt       time.Time
	TakenFair      float64
	ClosingFair    float64
	TakenPrice     odds.Decimal
	ClosingPrice   odds.Decimal
	ProbabilityClv float64
	PercentClv     float64
	Magnitude      float64
	BeatClose      bool
	LineMoved      bool
	LegStatus      string
	Voided         bool
	GradedAt       time.Time
}

// UserCLV returns one keyset page of a customer's graded legs.
//
// It RETURNS LINE-MOVED AND VOIDED ROWS, which [Store.UserCLVAggregate]
// excludes, and that asymmetry is the design rather than an oversight:
// odds/clv.go says of a line-moved result "Show it next to the two lines in a
// user interface; never rank anyone by it", so the display path carries the row
// with its flags and the aggregate path drops it.
func (s *Store) UserCLV(ctx context.Context, q httpapi.CLVQuery) (httpapi.CLVPage, error) {
	fetch := q.Limit + 1

	var (
		cols []clvColumns
		err  error
	)
	if q.After != nil {
		var rows []gen.ListUserCLVAfterCursorRow
		rows, err = s.q.ListUserCLVAfterCursor(ctx, gen.ListUserCLVAfterCursorParams{
			UserID:        q.UserID,
			GradedFrom:    q.GradedFrom,
			AfterGradedAt: q.After.GradedAt,
			AfterLegID:    q.After.LegID.String(),
			RowLimit:      fetch,
		})
		cols = mapRows(rows, func(r gen.ListUserCLVAfterCursorRow) clvColumns { return clvColumns(r) })
	} else {
		var rows []gen.ListUserCLVFirstPageRow
		rows, err = s.q.ListUserCLVFirstPage(ctx, gen.ListUserCLVFirstPageParams{
			UserID:     q.UserID,
			GradedFrom: q.GradedFrom,
			RowLimit:   fetch,
		})
		cols = mapRows(rows, func(r gen.ListUserCLVFirstPageRow) clvColumns { return clvColumns(r) })
	}
	if err != nil {
		return httpapi.CLVPage{}, fmt.Errorf("list user clv: %w", err)
	}

	hasMore := int32(len(cols)) > q.Limit
	if hasMore {
		cols = cols[:q.Limit]
	}

	out := make([]httpapi.CLVEntry, 0, len(cols))
	for _, c := range cols {
		e, err := clvEntryFrom(c)
		if err != nil {
			return httpapi.CLVPage{}, fmt.Errorf("list user clv: %w", err)
		}
		out = append(out, e)
	}
	return httpapi.CLVPage{Entries: out, HasMore: hasMore}, nil
}

func clvEntryFrom(c clvColumns) (httpapi.CLVEntry, error) {
	mtype, err := domain.ParseMarketType(c.MarketType)
	if err != nil {
		return httpapi.CLVEntry{}, fmt.Errorf("clv on leg %s: %w", c.LegID, err)
	}
	method, err := odds.ParseDevigMethod(c.DevigMethod)
	if err != nil {
		return httpapi.CLVEntry{}, fmt.Errorf("clv on leg %s: %w", c.LegID, err)
	}
	status, err := domain.ParseLegStatus(c.LegStatus)
	if err != nil {
		return httpapi.CLVEntry{}, fmt.Errorf("clv on leg %s: %w", c.LegID, err)
	}
	return httpapi.CLVEntry{
		LegID:       c.LegID,
		WagerID:     c.WagerID,
		MarketID:    c.MarketID,
		MarketType:  mtype,
		SelectionID: c.SelectionID,
		LeagueID:    c.LeagueID,

		TakenBookID:   c.TakenBookID,
		ClosingBookID: c.ClosingBookID,
		DevigMethod:   method,

		TakenLine:   float8Ptr(c.TakenLine),
		ClosingLine: float8Ptr(c.ClosingLine),
		TakenAt:     c.TakenAt.UTC(),
		ClosedAt:    c.ClosedAt.UTC(),

		TakenFair:    c.TakenFair,
		ClosingFair:  c.ClosingFair,
		TakenPrice:   c.TakenPrice,
		ClosingPrice: c.ClosingPrice,

		ProbabilityCLV: c.ProbabilityClv,
		PercentCLV:     c.PercentClv,
		Magnitude:      c.Magnitude,

		BeatClose: c.BeatClose,
		LineMoved: c.LineMoved,
		Status:    status,
		Voided:    c.Voided,
		GradedAt:  c.GradedAt.UTC(),
	}, nil
}

// UserCLVAggregate returns the SQL form of odds.AggregateCLV over the window.
//
// It ALWAYS returns a value and never [httpapi.ErrNotFound]. The statement is a
// three-CTE shape that yields exactly one row even for a customer with no
// history at all — honest zeros with null means, rather than pgx.ErrNoRows,
// because "this customer has no measurable wagers" is an ANSWER and "there is no
// such customer" is not reachable here: the caller is authenticated.
//
// The two means stay POINTERS across this boundary. odds.AggregateCLV reports
// ErrCLVNoSamples rather than returning zero for the same reason: a mean over
// zero samples does not exist, and a surface that rendered it as 0.00% would
// report an average of no numbers as break-even.
func (s *Store) UserCLVAggregate(ctx context.Context, q httpapi.CLVWindowQuery) (httpapi.CLVAggregate, error) {
	r, err := s.q.GetUserCLVAggregate(ctx, gen.GetUserCLVAggregateParams{
		UserID:     q.UserID,
		GradedFrom: q.From,
		GradedTo:   q.To,
	})
	if err != nil {
		return httpapi.CLVAggregate{}, fmt.Errorf("get user clv aggregate: %w", err)
	}
	return httpapi.CLVAggregate{
		Samples:            r.Samples,
		Counted:            r.Counted,
		VoidExcluded:       r.VoidExcluded,
		LineMovedExcluded:  r.LineMovedExcluded,
		BeatCount:          r.BeatCount,
		MeanProbabilityCLV: float8Ptr(r.MeanProbabilityClv),
		MeanPercentCLV:     float8Ptr(r.MeanPercentClv),
	}, nil
}

// UserCLVByLeague returns the same summary cut by league.
//
// The statement's HAVING clause admits only leagues with at least one countable
// leg, which is why the means here are plain float64 rather than pointers: a
// league with nothing to measure is ABSENT rather than present with a null.
func (s *Store) UserCLVByLeague(ctx context.Context, q httpapi.CLVWindowQuery) ([]httpapi.CLVLeagueSummary, error) {
	rows, err := s.q.ListUserCLVByLeague(ctx, gen.ListUserCLVByLeagueParams{
		UserID:     q.UserID,
		GradedFrom: q.From,
		GradedTo:   q.To,
	})
	if err != nil {
		return nil, fmt.Errorf("list user clv by league: %w", err)
	}
	out := make([]httpapi.CLVLeagueSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, httpapi.CLVLeagueSummary{
			LeagueID:           r.LeagueID,
			Counted:            r.Counted,
			BeatCount:          r.BeatCount,
			MeanProbabilityCLV: r.MeanProbabilityClv,
			MeanPercentCLV:     r.MeanPercentClv,
		})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// The leaderboard
// -----------------------------------------------------------------------------

// leaderboardColumns is the shape the two leaderboard statements share. They
// differ ONLY in their ORDER BY, which is why the row is identical and the basis
// selects a statement rather than a projection.
type leaderboardColumns struct {
	UserID             domain.UserID
	SettledWagers      int64
	StakedMinor        int64
	NetReturnMinor     int64
	Roi                float64
	ClvSamples         int64
	BeatCount          int64
	BeatRate           float64
	MeanProbabilityClv float64
	MeanPercentClv     float64
}

// Leaderboard runs whichever of the two ranking statements the basis names.
//
// # The money crosses this boundary through domain.FromMinorUnits
//
// `staked_minor` and `net_return_minor` are `sum(bigint)::BIGINT` and arrive as
// bare int64, because sqlc gives an aggregate no type override. They become
// domain.Money HERE, through the constructor that enforces the ±(2^53−1) bound
// every money column already carries — so a sum that somehow exceeded it is a
// wrapped error at the read rather than a number a browser would silently round.
// CLAUDE.md §12: floating point never touches a balance, and neither does an
// unchecked int64.
func (s *Store) Leaderboard(ctx context.Context, q httpapi.LeaderboardQuery) ([]httpapi.LeaderboardEntry, error) {
	var (
		cols []leaderboardColumns
		err  error
	)
	switch q.Basis {
	case httpapi.LeaderboardByCLV:
		var rows []gen.LeaderboardByCLVRow
		rows, err = s.q.LeaderboardByCLV(ctx, gen.LeaderboardByCLVParams{
			MinSettledWagers: q.MinSettledWagers,
			MinClvSamples:    q.MinCLVSamples,
			RowLimit:         q.Limit,
			FromInclusive:    q.From,
			ToExclusive:      q.To,
		})
		cols = mapRows(rows, func(r gen.LeaderboardByCLVRow) leaderboardColumns { return leaderboardColumns(r) })

	case httpapi.LeaderboardByROI:
		var rows []gen.LeaderboardByROIRow
		rows, err = s.q.LeaderboardByROI(ctx, gen.LeaderboardByROIParams{
			MinSettledWagers: q.MinSettledWagers,
			MinClvSamples:    q.MinCLVSamples,
			RowLimit:         q.Limit,
			FromInclusive:    q.From,
			ToExclusive:      q.To,
		})
		cols = mapRows(rows, func(r gen.LeaderboardByROIRow) leaderboardColumns { return leaderboardColumns(r) })

	default:
		// Unreachable through the HTTP surface, which parses the basis through
		// httpapi.ParseLeaderboardBasis. It is an error rather than a silent
		// fallback to ROI because a caller that reached here has a bug, and
		// serving one ranking under another's label is the worst way to report
		// it.
		return nil, fmt.Errorf("leaderboard: %w", httpapi.ErrUnknownLeaderboardBasis)
	}
	if err != nil {
		return nil, fmt.Errorf("leaderboard: %w", err)
	}

	out := make([]httpapi.LeaderboardEntry, 0, len(cols))
	for _, c := range cols {
		staked, err := domain.FromMinorUnits(c.StakedMinor)
		if err != nil {
			return nil, fmt.Errorf("leaderboard: staked for %s: %w", c.UserID, err)
		}
		net, err := domain.FromMinorUnits(c.NetReturnMinor)
		if err != nil {
			return nil, fmt.Errorf("leaderboard: net return for %s: %w", c.UserID, err)
		}
		out = append(out, httpapi.LeaderboardEntry{
			UserID:             c.UserID,
			SettledWagers:      c.SettledWagers,
			Staked:             staked,
			NetReturn:          net,
			ROI:                c.Roi,
			CLVSamples:         c.ClvSamples,
			BeatCount:          c.BeatCount,
			BeatRate:           c.BeatRate,
			MeanProbabilityCLV: c.MeanProbabilityClv,
			MeanPercentCLV:     c.MeanPercentClv,
		})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Shared plumbing
// -----------------------------------------------------------------------------

// mapRows converts a slice of sqlc rows into the shared column struct.
//
// The conversions it is called with are all direct struct conversions — the
// generated row and the local struct have identical field names and types — so
// the compiler checks the correspondence field by field. A column added to a
// statement, or a type changed by a migration, breaks this file at build time
// rather than producing a zero value at runtime, which is the entire reason the
// collapse is safe to do at all.
func mapRows[R any, C any](rows []R, convert func(R) C) []C {
	out := make([]C, 0, len(rows))
	for _, r := range rows {
		out = append(out, convert(r))
	}
	return out
}

// marketTypeStrings renders the market-type filter for `ANY($1::TEXT[])`.
//
// domain.MarketType is a uint8 enum rather than a ~string, so [stringsOf] cannot
// serve it. A nil filter becomes an EMPTY slice and not a nil one, because the
// statements test `cardinality($n) = 0 OR col = ANY($n)` and pgx encodes a nil
// slice as SQL NULL — against which `cardinality` is NULL, the OR is NULL, and
// the predicate silently excludes every row.
func marketTypeStrings(types []domain.MarketType) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, t.String())
	}
	return out
}

// durationFromSeconds converts a stored `DOUBLE PRECISION` seconds column.
//
// SIGNED, deliberately: a quote age may be negative when a provider's clock runs
// ahead of ours (domain.Price.Age() reports it that way), and clamping here
// would hide clock skew inside the one number whose job is to expose staleness.
func durationFromSeconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// uuidString renders a pgtype.UUID as its canonical lowercase form.
//
// Hand-rolled rather than via a UUID library because the module has no UUID
// dependency and this is the only place one would be wanted — the identifier is
// generated by `gen_random_uuid()` in the database, is never parsed by this
// system, and travels as an opaque string on the wire. An invalid value renders
// as the empty string, which cannot occur for a primary key but is a defined
// answer rather than a panic on a NULL.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], u.Bytes[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u.Bytes[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u.Bytes[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u.Bytes[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], u.Bytes[10:16])
	return string(buf)
}
