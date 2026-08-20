package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// Wager reads: the customer's own placed tickets.
//
// # This file reads. It does not, and cannot, write a wager
//
// Placement is internal/betting's transaction and settlement is
// internal/settlement's. There is no INSERT and no UPDATE below, and there must
// not be: migration 00006 freezes a ticket's booked terms with a trigger after
// insert, so a second writer here would not merely be a layering mistake, it
// would be a writer that the database refuses in a way nobody would discover
// until it ran.
//
// # Why these rows are NOT rehydrated through domain.NewWager
//
// The obvious implementation reconstructs a domain.Wager from the row and its
// legs, and it is the wrong one for a READ path. domain.NewWager validates: it
// checks the ticket price against the single leg on a straight, checks a
// teaser's points against every leg's teased line, checks arity against the
// kind. Those are the rules for ACCEPTING a bet, and they are exactly right
// there.
//
// A row read back has already been accepted. If this build's ticket pricer or
// its teaser ladder differs from the one that wrote the row — a real
// possibility, since internal/betting refuses to price a teaser without a ladder
// and a ladder is deployment configuration — then NewWager rejects a ticket the
// book has already taken the customer's money for, and a history page 500s on
// one old row. Refusing to RENDER a ticket that exists is a worse failure in
// every direction than rendering it.
//
// So the enums are parsed (an unrecognised value is still an error, because a
// schema/Go divergence must surface) and nothing else is re-derived. The read
// model httpapi.Wager exists for exactly this reason and says so.
//
// # Ownership is a comparison, and it happens HERE
//
// betting.sql's GetWager is deliberately not scoped by user — `settle` finds
// wagers through an event and has no customer in hand — and its own comment
// carries the requirement this file honours: "An API handler serving wager
// history MUST compare the returned user_id against the authenticated subject
// and answer 404 on a mismatch -- returning 403 would confirm the ticket exists,
// which is an enumeration oracle over other customers' wager ids."
//
// The comparison is one branch, in one place, immediately on the read, and it
// produces httpapi.ErrNotFound — the SAME value a missing row produces, from a
// function whose two failure paths are indistinguishable to every caller. The
// page read needs no such branch: its statement is scoped by user_id.

// maxFilterScans bounds the work a status-filtered page will do.
//
// The two statements that serve wager history take no status parameter, so a
// filter is applied to the rows the scan returned. Without a bound, a customer
// asking for `?status=void` with ten thousand settled tickets and no voids would
// walk their whole history inside one request.
//
// With it, a filtered page reads at most this many batches of Limit+1 rows and
// then returns what it has. `has_more` still reports the SCAN honestly, so the
// client follows the cursor and the work is spread across requests instead of
// concentrated in one — which is the same reason the page is keyset rather than
// counted.
//
// Eight is chosen so that the overwhelmingly common filter — "everything still
// running", two statuses out of seven — fills a page in one batch, and a narrow
// filter over a long history makes visible progress per request rather than
// returning a single row at a time.
const maxFilterScans = 8

// Wager returns one ticket with its legs, scoped to its owner.
func (s *Store) Wager(ctx context.Context, user domain.UserID, id domain.WagerID) (httpapi.Wager, error) {
	row, err := s.q.GetWager(ctx, id)
	if err != nil {
		return httpapi.Wager{}, notFound("get wager", err)
	}
	if row.UserID != user {
		// The same error a missing row produces, from the same function, with
		// nothing above able to tell the two apart. See the file header.
		return httpapi.Wager{}, fmt.Errorf("get wager: %w", httpapi.ErrNotFound)
	}

	wager, err := wagerFromRow(wagerRow(row))
	if err != nil {
		return httpapi.Wager{}, fmt.Errorf("get wager: %w", err)
	}

	legs, err := s.q.ListWagerLegs(ctx, id)
	if err != nil {
		return httpapi.Wager{}, fmt.Errorf("get wager legs: %w", err)
	}
	wager.Legs = make([]httpapi.WagerLeg, 0, len(legs))
	for _, l := range legs {
		leg, err := legFromRow(legRow{
			ID:              l.ID,
			EventID:         l.EventID,
			MarketID:        l.MarketID,
			MarketType:      l.MarketType,
			SelectionID:     l.SelectionID,
			Role:            l.Role,
			PriceBookID:     l.PriceBookID,
			PriceDecimal:    l.PriceDecimal,
			PriceLine:       l.PriceLine,
			PriceObservedAt: l.PriceObservedAt,
			TeasedLine:      l.TeasedLine,
			Status:          l.Status,
			GradedAt:        l.GradedAt,
		})
		if err != nil {
			return httpapi.Wager{}, fmt.Errorf("get wager legs: %w", err)
		}
		wager.Legs = append(wager.Legs, leg)
	}
	return wager, nil
}

// WagerPage returns one keyset page of a customer's history, newest first.
//
// Two statements rather than one with a nullable cursor, because
// `(@before IS NULL OR (placed_at, id) < (...))` is not sargable and the OR
// defeats the index the whole design depends on. betting.sql says so at the
// query and this is the caller that depends on it.
func (s *Store) WagerPage(ctx context.Context, q httpapi.WagerPageQuery) (httpapi.WagerPage, error) {
	if q.Limit <= 0 {
		return httpapi.WagerPage{}, fmt.Errorf("wager page: limit must be positive")
	}

	want := make(map[domain.WagerStatus]struct{}, len(q.Statuses))
	for _, status := range q.Statuses {
		want[status] = struct{}{}
	}

	page := httpapi.WagerPage{Wagers: make([]httpapi.Wager, 0, q.Limit)}
	cursor := q.After

	// One batch when there is no filter; up to maxFilterScans when there is.
	// The loop exists only so a narrow filter still fills a page — with no
	// filter the first iteration always either fills it or exhausts the history.
	scans := 1
	if len(want) > 0 {
		scans = maxFilterScans
	}

	for scan := 0; scan < scans; scan++ {
		rows, err := s.scanWagers(ctx, q.UserID, cursor, q.Limit)
		if err != nil {
			return httpapi.WagerPage{}, err
		}
		if len(rows) == 0 {
			page.HasMore = false
			return s.attachLegs(ctx, page)
		}

		// Limit+1 rows were asked for: the extra one answers "is there another
		// page" without a second COUNT(*), which over a continuously-written set
		// is stale before it is serialised.
		page.HasMore = int32(len(rows)) > q.Limit
		if page.HasMore {
			rows = rows[:q.Limit]
		}

		for _, row := range rows {
			wager, err := wagerFromRow(row)
			if err != nil {
				return httpapi.WagerPage{}, fmt.Errorf("wager page: %w", err)
			}
			// The cursor is minted from the last row SCANNED, not the last one
			// kept. Minting from a kept row would restart the next page before
			// the rows this one filtered out and serve them a second time.
			page.Last = httpapi.WagerKey{PlacedAt: wager.PlacedAt, ID: wager.ID}
			if len(want) > 0 {
				if _, ok := want[wager.Status]; !ok {
					continue
				}
			}
			page.Wagers = append(page.Wagers, wager)
			if int32(len(page.Wagers)) == q.Limit {
				// The page is full. HasMore already reflects whether the scan
				// had more rows, and rows this batch has not examined are
				// reachable through the cursor.
				if !page.HasMore {
					page.HasMore = true
				}
				return s.attachLegs(ctx, page)
			}
		}

		if !page.HasMore {
			return s.attachLegs(ctx, page)
		}
		next := page.Last
		cursor = &next
	}

	// The scan budget ran out with the page unfilled. HasMore is true, so the
	// client continues from the cursor rather than concluding the history ended.
	return s.attachLegs(ctx, page)
}

// scanWagers runs whichever of the two statements the cursor selects.
func (s *Store) scanWagers(
	ctx context.Context,
	user domain.UserID,
	after *httpapi.WagerKey,
	limit int32,
) ([]wagerRow, error) {
	if after == nil {
		rows, err := s.q.ListWagersForUserFirstPage(ctx, gen.ListWagersForUserFirstPageParams{
			UserID:   user,
			RowLimit: limit + 1,
		})
		if err != nil {
			return nil, fmt.Errorf("wager page: %w", err)
		}
		out := make([]wagerRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, wagerRow(r))
		}
		return out, nil
	}

	rows, err := s.q.ListWagersForUserAfterCursor(ctx, gen.ListWagersForUserAfterCursorParams{
		UserID:         user,
		BeforePlacedAt: after.PlacedAt,
		BeforeID:       after.ID.String(),
		RowLimit:       limit + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("wager page: %w", err)
	}
	out := make([]wagerRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, wagerRow(r))
	}
	return out, nil
}

// attachLegs fills in the legs for a whole page in ONE round trip.
//
// A page of thirty tickets served one ListWagerLegs at a time is thirty round
// trips whose per-call overhead dominates what is otherwise a bounded index
// scan; betting.sql provides ListLegsForWagers for exactly this and says so.
// The rows arrive ordered by (wager_id, selection_id), so they group in one pass
// without a map — and, more usefully, the tree does not reshuffle between
// renders.
func (s *Store) attachLegs(ctx context.Context, page httpapi.WagerPage) (httpapi.WagerPage, error) {
	if len(page.Wagers) == 0 {
		return page, nil
	}
	ids := make([]string, 0, len(page.Wagers))
	for _, w := range page.Wagers {
		ids = append(ids, w.ID.String())
	}

	rows, err := s.q.ListLegsForWagers(ctx, ids)
	if err != nil {
		return httpapi.WagerPage{}, fmt.Errorf("wager page legs: %w", err)
	}

	byWager := make(map[domain.WagerID][]httpapi.WagerLeg, len(page.Wagers))
	for _, r := range rows {
		leg, err := legFromRow(legRow{
			ID:              r.ID,
			EventID:         r.EventID,
			MarketID:        r.MarketID,
			MarketType:      r.MarketType,
			SelectionID:     r.SelectionID,
			Role:            r.Role,
			PriceBookID:     r.PriceBookID,
			PriceDecimal:    r.PriceDecimal,
			PriceLine:       r.PriceLine,
			PriceObservedAt: r.PriceObservedAt,
			TeasedLine:      r.TeasedLine,
			Status:          r.Status,
			GradedAt:        r.GradedAt,
		})
		if err != nil {
			return httpapi.WagerPage{}, fmt.Errorf("wager page legs: %w", err)
		}
		byWager[r.WagerID] = append(byWager[r.WagerID], leg)
	}
	for i := range page.Wagers {
		page.Wagers[i].Legs = byWager[page.Wagers[i].ID]
	}
	return page, nil
}

// wagerRow is the shape the three wager statements share.
//
// sqlc emits a distinct struct per query even when the SELECT list is identical,
// so this type exists to convert once rather than three times. The conversions
// are unchecked struct casts, which is safe precisely BECAUSE the field sets are
// identical — and if a query's SELECT list ever diverges, the cast stops
// compiling, which is the failure mode worth having.
type wagerRow struct {
	ID                   domain.WagerID
	UserID               domain.UserID
	Kind                 string
	Status               string
	StakeMinor           domain.Money
	AcceptedDecimal      odds.Decimal
	Rounding             string
	PotentialPayoutMinor domain.Money
	PotentialProfitMinor domain.Money
	TeaserPoints         pgtype.Float8
	RoundRobinID         *domain.RoundRobinID
	ReturnedMinor        *domain.Money
	NetReturnMinor       *domain.Money
	PlacedAt             time.Time
	TransitionedAt       time.Time
}

type legRow struct {
	ID              domain.LegID
	EventID         domain.EventID
	MarketID        domain.MarketID
	MarketType      string
	SelectionID     domain.SelectionID
	Role            string
	PriceBookID     domain.BookID
	PriceDecimal    odds.Decimal
	PriceLine       pgtype.Float8
	PriceObservedAt time.Time
	TeasedLine      pgtype.Float8
	Status          string
	GradedAt        pgtype.Timestamptz
}

// wagerFromRow parses the enums and nothing else.
//
// Every ParseX here errors on an unrecognised value, which is the point:
// sqlc.yaml keeps enum columns as `string` so that "a schema/Go divergence
// surfaces as a wrapped error at the read, not as a silent zero value". A wager
// whose status this build does not know is a real failure and is reported as
// one; a wager whose PRICE this build could not have quoted is not, and is
// rendered — see the file header.
func wagerFromRow(r wagerRow) (httpapi.Wager, error) {
	kind, err := domain.ParseWagerKind(r.Kind)
	if err != nil {
		return httpapi.Wager{}, fmt.Errorf("wager %s: %w", r.ID, err)
	}
	status, err := domain.ParseWagerStatus(r.Status)
	if err != nil {
		return httpapi.Wager{}, fmt.Errorf("wager %s: %w", r.ID, err)
	}
	rounding, err := domain.ParseRounding(r.Rounding)
	if err != nil {
		return httpapi.Wager{}, fmt.Errorf("wager %s: %w", r.ID, err)
	}

	out := httpapi.Wager{
		ID:              r.ID,
		UserID:          r.UserID,
		Kind:            kind,
		Status:          status,
		Stake:           r.StakeMinor,
		Decimal:         r.AcceptedDecimal,
		Rounding:        rounding,
		PotentialPayout: r.PotentialPayoutMinor,
		PotentialProfit: r.PotentialProfitMinor,
		TeaserPoints:    float8Ptr(r.TeaserPoints),
		RoundRobinID:    r.RoundRobinID,
		PlacedAt:        r.PlacedAt,
		UpdatedAt:       r.TransitionedAt,
	}
	// wagers_return_pair_complete makes "one set, one null" unstorable, so the
	// pair is copied together rather than field by field — which keeps the read
	// model unable to express a state the database refuses.
	if r.ReturnedMinor != nil && r.NetReturnMinor != nil {
		returned, net := *r.ReturnedMinor, *r.NetReturnMinor
		out.Returned = &returned
		out.NetReturn = &net
	}
	return out, nil
}

func legFromRow(r legRow) (httpapi.WagerLeg, error) {
	marketType, err := domain.ParseMarketType(r.MarketType)
	if err != nil {
		return httpapi.WagerLeg{}, fmt.Errorf("leg %s: %w", r.ID, err)
	}
	role, err := domain.ParseSelectionRole(r.Role)
	if err != nil {
		return httpapi.WagerLeg{}, fmt.Errorf("leg %s: %w", r.ID, err)
	}
	status, err := domain.ParseLegStatus(r.Status)
	if err != nil {
		return httpapi.WagerLeg{}, fmt.Errorf("leg %s: %w", r.ID, err)
	}

	line, err := lineFrom(r.PriceLine)
	if err != nil {
		return httpapi.WagerLeg{}, fmt.Errorf("leg %s price line: %w", r.ID, err)
	}
	teased, err := lineFrom(r.TeasedLine)
	if err != nil {
		return httpapi.WagerLeg{}, fmt.Errorf("leg %s teased line: %w", r.ID, err)
	}

	out := httpapi.WagerLeg{
		ID:              r.ID,
		EventID:         r.EventID,
		MarketID:        r.MarketID,
		MarketType:      marketType,
		SelectionID:     r.SelectionID,
		Role:            role,
		Status:          status,
		BookID:          r.PriceBookID,
		Decimal:         r.PriceDecimal,
		Line:            line,
		TeasedLine:      teased,
		PriceObservedAt: r.PriceObservedAt,
	}
	if r.GradedAt.Valid {
		at := r.GradedAt.Time
		out.GradedAt = &at
	}
	return out, nil
}

// lineFrom turns a nullable float8 column into a domain.Line.
//
// NULL is domain.NoLine() and a present 0.0 is a stored pick'em — a real traded
// value, and a different fact. domain.Line exists to keep those apart, and this
// is the boundary where a column that cannot express the difference is turned
// into a type that can.
func lineFrom(v pgtype.Float8) (domain.Line, error) {
	if !v.Valid {
		return domain.NoLine(), nil
	}
	return domain.NewLine(v.Float64)
}
