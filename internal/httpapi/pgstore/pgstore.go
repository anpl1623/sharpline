// Package pgstore is internal/httpapi's Postgres adapter.
//
// It implements the ports internal/httpapi declares — Catalogue, Prices, Ledger,
// Accounts, Limits, Audit — over the sqlc-generated queries in
// internal/platform/postgres/gen. It contains no HTTP, no policy that belongs to
// a handler, and no SQL of its own: every statement it runs is a named query in
// internal/platform/postgres/queries, which is what keeps the whole database
// surface inside one `sqlc diff` drift gate and one `make query-plans` index
// check.
//
// # What this package is actually FOR
//
// Three jobs, and nothing else:
//
//  1. TRANSLATE THE ROW SHAPE INTO THE READ MODEL. sqlc rows carry pgtype.Text,
//     pgtype.Int4 and raw enum strings, because that is what the columns are.
//     Handlers should not know that, and a column rename should not be an API
//     change — which is exactly why sqlc.yaml sets `emit_json_tags: false`.
//
//  2. PARSE ENUMS AT THE BOUNDARY. sqlc.yaml is explicit that enum columns stay
//     `string` and that the conversion happens through the domain's own
//     ParseX functions, "each of which returns an error for an unrecognised
//     value, so a schema/Go divergence surfaces as a wrapped error at the read,
//     not as a silent zero value". This package is that boundary.
//
//  3. CLASSIFY ABSENCE. pgx.ErrNoRows becomes httpapi.ErrNotFound, so a handler
//     distinguishes "no such event" from "the database is unreachable" without
//     importing a database driver.
//
// # Read-only, with three exceptions, all of them audited
//
// The API writes exactly three things: a user limit (and the row it supersedes),
// and audit entries. Everything else here is a read. There is no balance write
// and there is no path to one — CLAUDE.md §4 makes the balance a fold over
// ledger_entries and money moves only through internal/betting's double-entry
// transaction (phase 8).
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// Store is the Postgres adapter.
//
// It holds the pool (for the one path that needs a transaction) and a *gen.Queries
// bound to it. Both are needed: the queries type is the whole read surface, and
// the pool is what lets [Store.Set] run the limit change and its audit entry in
// ONE transaction.
type Store struct {
	db *postgres.DB
	q  *gen.Queries

	// idgen mints the identifiers for the rows this store inserts. Injected so a
	// test gets deterministic ids without stubbing the whole store.
	idgen func() (string, error)

	// cooling is how long a LOOSENING of a self-imposed limit waits before it
	// binds. A tightening is always immediate.
	cooling time.Duration
}

// Options configures [New].
type Options struct {
	DB *postgres.DB

	// CoolingOff is the delay a loosening serves. Zero means [DefaultCoolingOff].
	//
	// It is configurable but NOT disableable: a zero or negative value takes the
	// default rather than being honoured, because a cooling-off period of zero
	// turns a self-imposed limit into a setting the user can revoke at the exact
	// moment they most want to, which is the moment the control exists for.
	CoolingOff time.Duration

	// NewID mints identifiers for inserted rows. nil means auth.NewOpaqueID
	// with the "lim" prefix, which is 256 bits of crypto/rand.
	NewID func() (string, error)
}

// DefaultCoolingOff is the delay a loosening of a self-imposed limit serves.
//
// 24 hours is the shape of the control in the regulated world: long enough that
// the decision is not made inside the impulse that prompted it, short enough
// that a user who genuinely wants a higher cap is not locked out for a week.
// This is a simulation and the number is a product decision, not a legal one —
// it is named here so it is one edit rather than a literal in a handler.
const DefaultCoolingOff = 24 * time.Hour

// New builds the adapter.
func New(opts Options) (*Store, error) {
	if opts.DB == nil {
		return nil, errors.New("pgstore: DB is nil")
	}
	cooling := opts.CoolingOff
	if cooling <= 0 {
		cooling = DefaultCoolingOff
	}
	idgen := opts.NewID
	if idgen == nil {
		idgen = func() (string, error) { return auth.NewOpaqueID("lim") }
	}
	return &Store{
		db:      opts.DB,
		q:       gen.New(opts.DB.Pool()),
		idgen:   idgen,
		cooling: cooling,
	}, nil
}

// Compile-time proof that this adapter satisfies every port it claims. If a port
// grows a method, this file fails to build rather than failing at wire-up.
var (
	_ httpapi.Catalogue = (*Store)(nil)
	_ httpapi.Prices    = (*Store)(nil)
	_ httpapi.Ledger    = (*Store)(nil)
	_ httpapi.Accounts  = (*Store)(nil)
	_ httpapi.Limits    = (*Store)(nil)
	_ httpapi.Audit     = (*Store)(nil)
)

// notFound maps pgx's no-rows signal onto the consumer's sentinel.
//
// Every other error passes through wrapped, so a connection failure stays a
// connection failure and becomes a 500 rather than a 404 — collapsing the two is
// how a database outage gets reported to users as "no such event" and takes an
// hour to find.
func notFound(op string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, httpapi.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// -----------------------------------------------------------------------------
// Catalogue
// -----------------------------------------------------------------------------

func (s *Store) Sports(ctx context.Context) ([]httpapi.Sport, error) {
	rows, err := s.q.ListSports(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sports: %w", err)
	}
	out := make([]httpapi.Sport, 0, len(rows))
	for _, r := range rows {
		out = append(out, httpapi.Sport{ID: r.ID, Slug: r.Slug, Name: r.Name})
	}
	return out, nil
}

func (s *Store) LeaguesInSport(ctx context.Context, sport domain.SportID) ([]httpapi.League, error) {
	rows, err := s.q.ListLeaguesInSport(ctx, sport)
	if err != nil {
		return nil, fmt.Errorf("list leagues in sport: %w", err)
	}
	out := make([]httpapi.League, 0, len(rows))
	for _, r := range rows {
		out = append(out, httpapi.League{ID: r.ID, SportID: r.SportID, Slug: r.Slug, Name: r.Name})
	}
	return out, nil
}

func (s *Store) LeagueBySlug(ctx context.Context, slug domain.Slug) (httpapi.League, error) {
	r, err := s.q.FindLeagueBySlug(ctx, slug)
	if err != nil {
		return httpapi.League{}, notFound("find league by slug", err)
	}
	return httpapi.League{ID: r.ID, SportID: r.SportID, Slug: r.Slug, Name: r.Name}, nil
}

func (s *Store) Books(ctx context.Context) ([]httpapi.Book, error) {
	rows, err := s.q.ListBooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list books: %w", err)
	}
	out := make([]httpapi.Book, 0, len(rows))
	for _, r := range rows {
		kind, err := domain.ParseBookKind(r.Kind)
		if err != nil {
			return nil, fmt.Errorf("list books: book %s: %w", r.ID, err)
		}
		out = append(out, httpapi.Book{
			ID: r.ID, Slug: r.Slug, Name: r.Name, Kind: kind, Reference: r.IsReference,
		})
	}
	return out, nil
}

func (s *Store) EventWithBreadcrumb(ctx context.Context, id domain.EventID) (httpapi.Event, httpapi.League, httpapi.Sport, error) {
	r, err := s.q.GetEventWithLeague(ctx, id)
	if err != nil {
		return httpapi.Event{}, httpapi.League{}, httpapi.Sport{}, notFound("get event", err)
	}

	event, err := eventFrom(eventColumns{
		ID: r.ID, LeagueID: r.LeagueID, Kind: r.Kind, Name: r.Name,
		HomeCompetitorID: r.HomeCompetitorID, HomeCompetitorName: r.HomeCompetitorName,
		AwayCompetitorID: r.AwayCompetitorID, AwayCompetitorName: r.AwayCompetitorName,
		ScheduledStart: r.ScheduledStart, Status: r.Status,
		ClockPeriod: r.ClockPeriod, ClockElapsedNs: r.ClockElapsedNs, ClockRunning: r.ClockRunning,
		ScoreHome: r.ScoreHome, ScoreAway: r.ScoreAway, ObservedAt: r.ObservedAt,
	})
	if err != nil {
		return httpapi.Event{}, httpapi.League{}, httpapi.Sport{}, fmt.Errorf("get event: %w", err)
	}

	league := httpapi.League{ID: r.LeagueID, SportID: r.SportID, Slug: r.LeagueSlug, Name: r.LeagueName}
	sport := httpapi.Sport{ID: r.SportID, Slug: r.SportSlug, Name: r.SportName}
	return event, league, sport, nil
}

func (s *Store) Market(ctx context.Context, id domain.MarketID) (httpapi.Market, error) {
	r, err := s.q.GetMarketWithEvent(ctx, id)
	if err != nil {
		return httpapi.Market{}, notFound("get market", err)
	}
	return marketFrom(gen.ListMarketsForEventsRow{
		ID: r.ID, EventID: r.EventID, Type: r.Type, Line: r.Line,
		Subject: r.Subject, Status: r.Status, ObservedAt: r.ObservedAt,
	})
}

func (s *Store) Selection(ctx context.Context, id domain.SelectionID) (httpapi.Selection, error) {
	r, err := s.q.GetSelectionWithMarket(ctx, id)
	if err != nil {
		return httpapi.Selection{}, notFound("get selection", err)
	}
	role, err := domain.ParseSelectionRole(r.Role)
	if err != nil {
		return httpapi.Selection{}, fmt.Errorf("get selection %s: %w", id, err)
	}
	mtype, err := domain.ParseMarketType(r.MarketType)
	if err != nil {
		return httpapi.Selection{}, fmt.Errorf("get selection %s: %w", id, err)
	}
	return httpapi.Selection{
		ID: r.ID, MarketID: r.MarketID, MarketType: mtype, Role: role, Name: r.Name,
	}, nil
}

func (s *Store) MarketsForEvents(ctx context.Context, ids []domain.EventID) ([]httpapi.Market, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.ListMarketsForEvents(ctx, stringsOf(ids))
	if err != nil {
		return nil, fmt.Errorf("list markets for events: %w", err)
	}
	out := make([]httpapi.Market, 0, len(rows))
	for _, r := range rows {
		m, err := marketFrom(r)
		if err != nil {
			return nil, fmt.Errorf("list markets for events: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) SelectionsForMarkets(ctx context.Context, ids []domain.MarketID) ([]httpapi.Selection, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.ListSelectionsForMarkets(ctx, stringsOf(ids))
	if err != nil {
		return nil, fmt.Errorf("list selections for markets: %w", err)
	}
	out := make([]httpapi.Selection, 0, len(rows))
	for _, r := range rows {
		role, err := domain.ParseSelectionRole(r.Role)
		if err != nil {
			return nil, fmt.Errorf("list selections: selection %s: %w", r.ID, err)
		}
		mtype, err := domain.ParseMarketType(r.MarketType)
		if err != nil {
			return nil, fmt.Errorf("list selections: selection %s: %w", r.ID, err)
		}
		out = append(out, httpapi.Selection{
			ID: r.ID, MarketID: r.MarketID, MarketType: mtype, Role: role, Name: r.Name,
		})
	}
	return out, nil
}

// EventPage runs one of the four keyset board statements.
//
// WHICH statement is chosen by two booleans — league or not, cursor or not — and
// the four are separate queries rather than one with nullable parameters because
// `(@after IS NULL OR (a, b) > (@after_a, @after_b))` is not sargable: the OR
// defeats the index the whole design depends on. See the header of api.sql.
//
// It asks for Limit+1 rows and reports HasMore from whether it got them. One
// extra row beats a second COUNT(*), and a count over a continuously-written set
// is stale before it is serialised.
func (s *Store) EventPage(ctx context.Context, q httpapi.EventPageQuery) (httpapi.EventPage, error) {
	fetch := q.Limit + 1

	var (
		cols []eventColumns
		err  error
	)
	switch {
	case !q.LeagueID.IsZero() && q.After != nil:
		var rows []gen.ListLeagueBoardEventsAfterCursorRow
		rows, err = s.q.ListLeagueBoardEventsAfterCursor(ctx, gen.ListLeagueBoardEventsAfterCursorParams{
			LeagueID:       q.LeagueID,
			StartingBefore: q.StartingBefore,
			AfterStart:     q.After.ScheduledStart,
			AfterID:        q.After.ID.String(),
			RowLimit:       fetch,
		})
		for _, r := range rows {
			cols = append(cols, eventColumns(r))
		}
	case !q.LeagueID.IsZero():
		var rows []gen.ListLeagueBoardEventsFirstPageRow
		rows, err = s.q.ListLeagueBoardEventsFirstPage(ctx, gen.ListLeagueBoardEventsFirstPageParams{
			LeagueID:       q.LeagueID,
			StartingBefore: q.StartingBefore,
			RowLimit:       fetch,
		})
		for _, r := range rows {
			cols = append(cols, eventColumns(r))
		}
	case q.After != nil:
		var rows []gen.ListBoardEventsAfterCursorRow
		rows, err = s.q.ListBoardEventsAfterCursor(ctx, gen.ListBoardEventsAfterCursorParams{
			StartingBefore: q.StartingBefore,
			AfterStart:     q.After.ScheduledStart,
			AfterID:        q.After.ID.String(),
			RowLimit:       fetch,
		})
		for _, r := range rows {
			cols = append(cols, eventColumns(r))
		}
	default:
		var rows []gen.ListBoardEventsFirstPageRow
		rows, err = s.q.ListBoardEventsFirstPage(ctx, gen.ListBoardEventsFirstPageParams{
			StartingBefore: q.StartingBefore,
			RowLimit:       fetch,
		})
		for _, r := range rows {
			cols = append(cols, eventColumns(r))
		}
	}
	if err != nil {
		return httpapi.EventPage{}, fmt.Errorf("board page: %w", err)
	}

	page := httpapi.EventPage{Events: make([]httpapi.Event, 0, q.Limit)}
	if int32(len(cols)) > q.Limit {
		page.HasMore = true
		cols = cols[:q.Limit]
	}
	for _, c := range cols {
		e, err := eventFrom(c)
		if err != nil {
			return httpapi.EventPage{}, fmt.Errorf("board page: %w", err)
		}
		page.Events = append(page.Events, e)
	}
	return page, nil
}

// SearchEvents runs the prefix search, escaping LIKE metacharacters first.
//
// THE ESCAPE IS NOT OPTIONAL. `%` and `_` are LIKE wildcards; a query of `%`
// left unescaped turns a prefix search into a leading-wildcard scan of the whole
// events table, which is a denial of service one character long. `\` is escaped
// first or it would double the escapes this function itself introduces.
func (s *Store) SearchEvents(ctx context.Context, q httpapi.SearchQuery) (httpapi.SearchPage, error) {
	prefix := escapeLike(q.Prefix)
	fetch := q.Limit + 1

	type hit struct {
		ID                 domain.EventID
		LeagueID           domain.LeagueID
		Kind               string
		Name               string
		HomeCompetitorName pgtype.Text
		AwayCompetitorName pgtype.Text
		ScheduledStart     time.Time
		Status             string
	}

	var (
		hits []hit
		err  error
	)
	if q.After != nil {
		var rows []gen.SearchBoardEventsAfterCursorRow
		rows, err = s.q.SearchBoardEventsAfterCursor(ctx, gen.SearchBoardEventsAfterCursorParams{
			Prefix:     prefix,
			AfterStart: q.After.ScheduledStart,
			AfterID:    q.After.ID.String(),
			RowLimit:   fetch,
		})
		for _, r := range rows {
			hits = append(hits, hit(r))
		}
	} else {
		var rows []gen.SearchBoardEventsFirstPageRow
		rows, err = s.q.SearchBoardEventsFirstPage(ctx, gen.SearchBoardEventsFirstPageParams{
			Prefix:   prefix,
			RowLimit: fetch,
		})
		for _, r := range rows {
			hits = append(hits, hit(r))
		}
	}
	if err != nil {
		return httpapi.SearchPage{}, fmt.Errorf("search events: %w", err)
	}

	page := httpapi.SearchPage{Events: make([]httpapi.Event, 0, q.Limit)}
	if int32(len(hits)) > q.Limit {
		page.HasMore = true
		hits = hits[:q.Limit]
	}
	for _, h := range hits {
		kind, err := domain.ParseEventKind(h.Kind)
		if err != nil {
			return httpapi.SearchPage{}, fmt.Errorf("search events: event %s: %w", h.ID, err)
		}
		status, err := domain.ParseEventStatus(h.Status)
		if err != nil {
			return httpapi.SearchPage{}, fmt.Errorf("search events: event %s: %w", h.ID, err)
		}
		page.Events = append(page.Events, httpapi.Event{
			ID:                 h.ID,
			LeagueID:           h.LeagueID,
			Kind:               kind,
			Name:               h.Name,
			HomeCompetitorName: h.HomeCompetitorName.String,
			AwayCompetitorName: h.AwayCompetitorName.String,
			ScheduledStart:     h.ScheduledStart,
			Status:             status,
		})
	}
	return page, nil
}

// -----------------------------------------------------------------------------
// Prices
// -----------------------------------------------------------------------------

func (s *Store) CurrentQuotes(ctx context.Context, selections []domain.SelectionID, since time.Time) ([]httpapi.Quote, error) {
	if len(selections) == 0 {
		return nil, nil
	}
	rows, err := s.q.LatestPriceForEachBookOnSelections(ctx, gen.LatestPriceForEachBookOnSelectionsParams{
		SelectionIDs:  stringsOf(selections),
		ObservedAfter: since,
	})
	if err != nil {
		return nil, fmt.Errorf("current quotes: %w", err)
	}
	out := make([]httpapi.Quote, 0, len(rows))
	for _, r := range rows {
		out = append(out, httpapi.Quote{
			SelectionID: r.SelectionID,
			BookID:      r.BookID,
			Odds:        r.DecimalOdds,
			Line:        float8Ptr(r.Line),
			ObservedAt:  r.ObservedAt,
			IngestedAt:  r.IngestedAt,
		})
	}
	return out, nil
}

// History returns a raw or a bucketed series.
//
// A RAW series still fills in Open/High/Low/Close from the single stored quote
// and reports Samples 1, so the wire shape is identical either way and a client
// renders one thing. The alternative — a different point shape per resolution —
// makes every consumer branch, and the branch is always in the chart library
// where it is hardest to fix.
func (s *Store) History(ctx context.Context, q httpapi.HistoryQuery) ([]httpapi.HistoryPoint, error) {
	if q.Bucket <= 0 {
		rows, err := s.q.ListPriceHistoryForSelectionAtBook(ctx, gen.ListPriceHistoryForSelectionAtBookParams{
			SelectionID:   q.SelectionID,
			BookID:        q.BookID,
			FromInclusive: q.From,
			ToExclusive:   q.To,
		})
		if err != nil {
			return nil, fmt.Errorf("price history: %w", err)
		}
		out := make([]httpapi.HistoryPoint, 0, len(rows))
		for _, r := range rows {
			if int32(len(out)) >= q.MaxPoints {
				break
			}
			out = append(out, httpapi.HistoryPoint{
				At:      r.ObservedAt,
				Open:    r.DecimalOdds,
				High:    r.DecimalOdds,
				Low:     r.DecimalOdds,
				Close:   r.DecimalOdds,
				Line:    float8Ptr(r.Line),
				Samples: 1,
			})
		}
		return out, nil
	}

	rows, err := s.q.ListBucketedPriceHistory(ctx, gen.ListBucketedPriceHistoryParams{
		BucketSeconds: q.Bucket.Seconds(),
		SelectionID:   q.SelectionID,
		BookID:        q.BookID,
		FromInclusive: q.From,
		ToExclusive:   q.To,
		RowLimit:      q.MaxPoints,
	})
	if err != nil {
		return nil, fmt.Errorf("bucketed price history: %w", err)
	}
	out := make([]httpapi.HistoryPoint, 0, len(rows))
	for _, r := range rows {
		p := httpapi.HistoryPoint{
			At:      r.BucketStart,
			Open:    odds.Decimal(r.OpenOdds),
			High:    odds.Decimal(r.HighOdds),
			Low:     odds.Decimal(r.LowOdds),
			Close:   odds.Decimal(r.CloseOdds),
			Samples: int32(r.Samples),
		}
		// has_line distinguishes "the line at this bucket's close was 0.0" from
		// "this market carries no line". The query coalesces the value to zero
		// and projects its presence separately, because a NULL cannot be scanned
		// into the float64 the cast produces. See api.sql.
		if r.HasLine {
			line := r.CloseLine
			p.Line = &line
		}
		out = append(out, p)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Ledger and account
// -----------------------------------------------------------------------------

// Balances folds ledger_entries through the account_balances view.
//
// It ALWAYS returns both customer accounts, filling in a zero for one the view
// reports no row for. The view groups by account and a user with no entries
// simply has no row — which is correct, and which a caller must not mistake for
// "account not found". Materialising the zero here means the handler never has
// to decide what an absent account means.
func (s *Store) Balances(ctx context.Context, user domain.UserID) ([]httpapi.Balance, error) {
	rows, err := s.q.GetUserCashAndEscrowBalances(ctx, &user)
	if err != nil {
		return nil, fmt.Errorf("account balances: %w", err)
	}

	out := []httpapi.Balance{
		{Kind: domain.AccountKindUserCash},
		{Kind: domain.AccountKindUserEscrow},
	}
	for _, r := range rows {
		kind, err := domain.ParseAccountKind(r.AccountKind)
		if err != nil {
			return nil, fmt.Errorf("account balances: %w", err)
		}
		for i := range out {
			if out[i].Kind == kind {
				out[i].Amount = r.BalanceMinor
				out[i].Entries = r.EntryCount
			}
		}
	}
	return out, nil
}

func (s *Store) Profile(ctx context.Context, user domain.UserID) (httpapi.Profile, error) {
	r, err := s.q.GetAccountProfile(ctx, user)
	if err != nil {
		return httpapi.Profile{}, notFound("account profile", err)
	}
	status, err := auth.ParseUserStatus(r.Status)
	if err != nil {
		return httpapi.Profile{}, fmt.Errorf("account profile: %w", err)
	}
	return httpapi.Profile{
		ID:            r.ID,
		Email:         r.Email,
		Status:        status,
		CreatedAt:     r.CreatedAt,
		TOTPConfirmed: r.TotpConfirmed,
		TOTPPending:   r.TotpEnrolmentStarted && !r.TotpConfirmed,
	}, nil
}

// -----------------------------------------------------------------------------
// Self-imposed limits
// -----------------------------------------------------------------------------

func (s *Store) Current(ctx context.Context, user domain.UserID) ([]httpapi.Limit, error) {
	rows, err := s.q.ListCurrentUserLimits(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("current limits: %w", err)
	}
	out := make([]httpapi.Limit, 0, len(rows))
	for _, r := range rows {
		l, err := limitFrom(limitColumns(r))
		if err != nil {
			return nil, fmt.Errorf("current limits: %w", err)
		}
		out = append(out, l)
	}
	return out, nil
}

// Set records a new self-imposed limit, supersedes the one it replaces, and
// writes the audit entry — ALL IN ONE TRANSACTION.
//
// # Why one transaction and not three statements
//
// The three writes are one fact. If the supersede commits and the insert does
// not, the user has NO limit where they had one — the failure mode is a control
// silently disappearing, which is the worst available outcome for a
// responsible-gaming feature. If the change commits and the audit row does not,
// the record of who changed what is gone. So they commit together or not at all.
//
// # Why the cooling-off decision is here
//
// It needs the current row and the new one under the same lock. Deciding it in
// the handler would mean reading the current limit, deciding, and then writing —
// with a window in between where a concurrent request changes the answer. Here
// the read and the write are in one transaction and the partial unique index
// `user_limits_current_key` closes the remaining race in the database.
func (s *Store) Set(ctx context.Context, req httpapi.SetLimit) (httpapi.Limit, error) {
	id, err := s.idgen()
	if err != nil {
		return httpapi.Limit{}, fmt.Errorf("set limit: mint id: %w", err)
	}

	var out httpapi.Limit
	err = s.db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.q.WithTx(tx)

		var current *httpapi.Limit
		row, err := q.GetCurrentUserLimit(ctx, gen.GetCurrentUserLimitParams{
			UserID: req.UserID,
			Kind:   req.Kind.String(),
			Period: req.Period.String(),
		})
		switch {
		case err == nil:
			l, convErr := limitFrom(limitColumns(row))
			if convErr != nil {
				return convErr
			}
			current = &l
		case errors.Is(err, pgx.ErrNoRows):
			// No limit in force. Introducing one is always a TIGHTENING.
		default:
			return fmt.Errorf("read current limit: %w", err)
		}

		requestedAt := req.Audit.At
		effectiveFrom := requestedAt
		if isLoosening(current, req) {
			effectiveFrom = requestedAt.Add(s.cooling)
		}

		if current != nil {
			n, err := q.SupersedeUserLimit(ctx, gen.SupersedeUserLimitParams{
				ID:           current.ID,
				SupersededAt: pgtype.Timestamptz{Time: requestedAt, Valid: true},
			})
			if err != nil {
				return fmt.Errorf("supersede current limit: %w", err)
			}
			if n == 0 {
				// Another request closed the same row first. The partial unique
				// index would refuse a second open row one statement later with
				// a worse error; failing here lets the handler answer 409 and
				// the client re-read.
				return httpapi.ErrConflict
			}
		}

		// Both nil means REMOVE the limit: the current row is superseded and no
		// successor is written, so the (user, kind, period) pair has no row with
		// superseded_at IS NULL and the limit is gone. That is still a loosening
		// and still serves the cooling-off period — which is why the removal is
		// only recorded after `effectiveFrom` is computed above, and why a
		// removal request with a pending cooling-off writes a successor with the
		// future effective_from rather than deleting outright.
		if req.Amount == nil && req.Duration == nil {
			return s.auditLimit(ctx, q, req, current, nil)
		}

		params := gen.InsertUserLimitParams{
			ID:            id,
			UserID:        req.UserID,
			Kind:          req.Kind.String(),
			Period:        req.Period.String(),
			RequestedAt:   requestedAt,
			EffectiveFrom: effectiveFrom,
		}
		if req.Amount != nil {
			params.AmountMinor = pgtype.Int8{Int64: req.Amount.MinorUnits(), Valid: true}
		}
		if req.Duration != nil {
			params.DurationSeconds = pgtype.Int4{Int32: int32(*req.Duration / time.Second), Valid: true}
		}
		if err := q.InsertUserLimit(ctx, params); err != nil {
			if postgres.IsUniqueViolation(err) {
				return httpapi.ErrConflict
			}
			return fmt.Errorf("insert limit: %w", err)
		}

		out = httpapi.Limit{
			ID:            id,
			UserID:        req.UserID,
			Kind:          req.Kind,
			Period:        req.Period,
			Amount:        req.Amount,
			Duration:      req.Duration,
			RequestedAt:   requestedAt,
			EffectiveFrom: effectiveFrom,
		}
		return s.auditLimit(ctx, q, req, current, &out)
	})
	if err != nil {
		if errors.Is(err, httpapi.ErrConflict) {
			return httpapi.Limit{}, err
		}
		return httpapi.Limit{}, fmt.Errorf("set limit: %w", err)
	}
	return out, nil
}

// isLoosening reports whether the requested limit is weaker than the one in
// force.
//
// The rule, stated so it is checkable:
//
//   - No limit in force -> introducing one is a TIGHTENING (immediate).
//   - Removing a limit entirely -> LOOSENING.
//   - A money limit -> loosening if the new cap is HIGHER.
//   - A session limit -> loosening if the new duration is LONGER.
//   - Equal -> not a loosening, so it binds immediately. Re-asserting the same
//     limit must never cost the user a cooling-off period.
func isLoosening(current *httpapi.Limit, req httpapi.SetLimit) bool {
	if current == nil {
		return false
	}
	switch {
	case req.Amount == nil && req.Duration == nil:
		return true
	case req.Amount != nil && current.Amount != nil:
		return *req.Amount > *current.Amount
	case req.Duration != nil && current.Duration != nil:
		return *req.Duration > *current.Duration
	default:
		// The kind changed shape between the stored row and the request, which
		// the database's biconditionals make unstorable. Treating an
		// unclassifiable change as a loosening is the safe direction: the worst
		// outcome is that a tightening waits, never that a loosening takes
		// effect immediately.
		return true
	}
}

// auditLimit writes the audit row for a limit change, inside the caller's
// transaction.
//
// The before/after states carry the CHANGED FIELDS ONLY — kind, period and the
// value — never a whole entity dump, and never anything secret. There is nothing
// secret in a limit.
func (s *Store) auditLimit(ctx context.Context, q *gen.Queries, req httpapi.SetLimit, before, after *httpapi.Limit) error {
	entry := httpapi.AuditEntry{
		Context:    req.Audit,
		ActorKind:  "user",
		ActorID:    req.UserID.String(),
		Action:     "user_limit.set",
		EntityType: "user_limit",
		EntityID:   req.Kind.String() + ":" + req.Period.String(),
		Outcome:    "success",
		Before:     limitState(before),
		After:      limitState(after),
	}
	if err := s.recordWith(ctx, q, entry); err != nil {
		return fmt.Errorf("audit limit change: %w", err)
	}
	return nil
}

func limitState(l *httpapi.Limit) map[string]any {
	if l == nil {
		return nil
	}
	m := map[string]any{"kind": l.Kind.String(), "period": l.Period.String()}
	if l.Amount != nil {
		m["amount_minor"] = l.Amount.MinorUnits()
	}
	if l.Duration != nil {
		m["duration_seconds"] = int64(*l.Duration / time.Second)
	}
	m["effective_from"] = l.EffectiveFrom.UTC().Format(time.RFC3339Nano)
	return m
}

// -----------------------------------------------------------------------------
// Audit
// -----------------------------------------------------------------------------

// Record writes one audit entry outside any transaction.
//
// Used only by the actions that have no transaction of their own — the TOTP
// enrolment steps, whose state change lives in internal/auth. Anything that
// writes its own rows audits inside its own transaction (see [Store.Set]).
func (s *Store) Record(ctx context.Context, e httpapi.AuditEntry) error {
	return s.recordWith(ctx, s.q, e)
}

func (s *Store) recordWith(ctx context.Context, q *gen.Queries, e httpapi.AuditEntry) error {
	before, err := marshalState(e.Before)
	if err != nil {
		return fmt.Errorf("audit: encode before state: %w", err)
	}
	after, err := marshalState(e.After)
	if err != nil {
		return fmt.Errorf("audit: encode after state: %w", err)
	}

	occurred := e.Context.At
	if occurred.IsZero() {
		occurred = time.Now()
	}

	params := gen.InsertAuditEntryParams{
		OccurredAt:  occurred.UTC(),
		ActorKind:   e.ActorKind,
		ActorID:     e.ActorID,
		Action:      e.Action,
		EntityType:  e.EntityType,
		EntityID:    e.EntityID,
		Outcome:     e.Outcome,
		Reason:      text(e.Reason),
		StateBefore: before,
		StateAfter:  after,
		TraceID:     text(e.Context.TraceID),
		SpanID:      text(e.Context.SpanID),
		RequestID:   text(e.Context.RequestID),
	}
	if e.Context.ClientIP.IsValid() {
		ip := e.Context.ClientIP
		params.ClientIp = &ip
	}

	if err := q.InsertAuditEntry(ctx, params); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

func marshalState(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return json.Marshal(m)
}

// -----------------------------------------------------------------------------
// Row conversion
// -----------------------------------------------------------------------------

// eventColumns is the shared shape of the four board queries' rows and of
// GetEventWithLeague's event half.
//
// The four generated row structs are structurally identical — sqlc emits one per
// query — so a direct conversion `eventColumns(r)` is legal and cheap, and there
// is exactly one function that turns these columns into a domain-typed Event. A
// per-query converter would be four places for the enum parsing to drift.
type eventColumns struct {
	ID                 domain.EventID
	LeagueID           domain.LeagueID
	Kind               string
	Name               string
	HomeCompetitorID   *domain.CompetitorID
	HomeCompetitorName pgtype.Text
	AwayCompetitorID   *domain.CompetitorID
	AwayCompetitorName pgtype.Text
	ScheduledStart     time.Time
	Status             string
	ClockPeriod        pgtype.Int4
	ClockElapsedNs     pgtype.Int8
	ClockRunning       pgtype.Bool
	ScoreHome          pgtype.Int4
	ScoreAway          pgtype.Int4
	ObservedAt         time.Time
}

func eventFrom(c eventColumns) (httpapi.Event, error) {
	kind, err := domain.ParseEventKind(c.Kind)
	if err != nil {
		return httpapi.Event{}, fmt.Errorf("event %s: %w", c.ID, err)
	}
	status, err := domain.ParseEventStatus(c.Status)
	if err != nil {
		return httpapi.Event{}, fmt.Errorf("event %s: %w", c.ID, err)
	}

	e := httpapi.Event{
		ID:                 c.ID,
		LeagueID:           c.LeagueID,
		Kind:               kind,
		Name:               c.Name,
		HomeCompetitorID:   c.HomeCompetitorID,
		HomeCompetitorName: c.HomeCompetitorName.String,
		AwayCompetitorID:   c.AwayCompetitorID,
		AwayCompetitorName: c.AwayCompetitorName.String,
		ScheduledStart:     c.ScheduledStart,
		Status:             status,
		ObservedAt:         c.ObservedAt,
	}

	// A clock exists only if the running flag does. An event that has not
	// started has no clock at all, which is a different fact from a clock
	// reading zero, and the wire distinguishes them.
	if c.ClockRunning.Valid {
		clock := httpapi.GameClock{Running: c.ClockRunning.Bool}
		if c.ClockPeriod.Valid {
			p := c.ClockPeriod.Int32
			clock.Period = &p
		}
		if c.ClockElapsedNs.Valid {
			d := time.Duration(c.ClockElapsedNs.Int64)
			clock.Elapsed = &d
		}
		e.Clock = &clock
	}
	// Both halves of a score or neither: a score with one side missing is not a
	// score, and rendering it would put a blank where a number belongs.
	if c.ScoreHome.Valid && c.ScoreAway.Valid {
		e.Score = &httpapi.Score{Home: c.ScoreHome.Int32, Away: c.ScoreAway.Int32}
	}
	return e, nil
}

func marketFrom(r gen.ListMarketsForEventsRow) (httpapi.Market, error) {
	mtype, err := domain.ParseMarketType(r.Type)
	if err != nil {
		return httpapi.Market{}, fmt.Errorf("market %s: %w", r.ID, err)
	}
	status, err := domain.ParseMarketStatus(r.Status)
	if err != nil {
		return httpapi.Market{}, fmt.Errorf("market %s: %w", r.ID, err)
	}
	return httpapi.Market{
		ID:         r.ID,
		EventID:    r.EventID,
		Type:       mtype,
		Line:       float8Ptr(r.Line),
		Subject:    r.Subject.String,
		Status:     status,
		ObservedAt: r.ObservedAt,
	}, nil
}

// limitColumns is the shared shape of the two user_limits read queries.
type limitColumns struct {
	ID              string
	UserID          domain.UserID
	Kind            string
	Period          string
	AmountMinor     int64
	HasAmount       bool
	DurationSeconds int32
	HasDuration     bool
	RequestedAt     time.Time
	EffectiveFrom   time.Time
}

func limitFrom(c limitColumns) (httpapi.Limit, error) {
	kind, err := auth.ParseLimitKind(c.Kind)
	if err != nil {
		return httpapi.Limit{}, fmt.Errorf("limit %s: %w", c.ID, err)
	}
	period, err := auth.ParseLimitPeriod(c.Period)
	if err != nil {
		return httpapi.Limit{}, fmt.Errorf("limit %s: %w", c.ID, err)
	}

	l := httpapi.Limit{
		ID:            c.ID,
		UserID:        c.UserID,
		Kind:          kind,
		Period:        period,
		RequestedAt:   c.RequestedAt,
		EffectiveFrom: c.EffectiveFrom,
	}
	if c.HasAmount {
		amount, err := domain.FromMinorUnits(c.AmountMinor)
		if err != nil {
			return httpapi.Limit{}, fmt.Errorf("limit %s: %w", c.ID, err)
		}
		l.Amount = &amount
	}
	if c.HasDuration {
		d := time.Duration(c.DurationSeconds) * time.Second
		l.Duration = &d
	}
	return l, nil
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// stringsOf converts a slice of domain identifiers to the []string an
// `ANY($1::TEXT[])` parameter takes.
//
// It is generic over ~string so a caller cannot pass a []MarketID where selection
// ids are wanted — the exact substitution the distinct identifier types exist to
// refuse, and the reason sqlc.yaml says "nothing outside that package should
// build the []string itself".
func stringsOf[T ~string](ids []T) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

func float8Ptr(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// escapeLike neutralises PostgreSQL LIKE's three metacharacters.
//
// `\` is replaced FIRST. Doing it last would double the backslashes this
// function itself introduced and turn a search for `50%` into a search for a
// literal backslash followed by a wildcard.
func escapeLike(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
