package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Sports lists every sport in the catalogue.
//
// Bounded by the catalogue rather than by time or traffic, so it returns a
// single page with a nil [PageInfo.NextCursor]. The page envelope is kept
// anyway so a caller has one shape to handle across every list endpoint.
func (c *Client) Sports(ctx context.Context) (*SportPage, error) {
	var out SportPage
	err := c.do(ctx, call{
		op:     "GET /sports",
		method: http.MethodGet,
		path:   "/sports",
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Leagues lists the leagues in one sport.
func (c *Client) Leagues(ctx context.Context, sportSlug string) (*LeaguePage, error) {
	var out LeaguePage
	err := c.do(ctx, call{
		op:     "GET /sports/{sportSlug}/leagues",
		method: http.MethodGet,
		path:   "/sports/" + pathEscape(sportSlug) + "/leagues",
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Books lists the sportsbooks whose prices are ingested, including the
// synthetic in-house book used in development and the one designated as the
// sharp reference.
func (c *Client) Books(ctx context.Context) (*BookPage, error) {
	var out BookPage
	err := c.do(ctx, call{
		op:     "GET /books",
		method: http.MethodGet,
		path:   "/books",
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Board returns the live odds board across every league.
//
// # Paging
//
// Pagination is keyset, never offset, because `ingest` writes the events and
// prices tables continuously: an offset re-evaluates the whole ordered set on
// every page, so a row inserted ahead of the offset between page N and N+1
// pushes one row across the boundary and the reader NEVER SEES IT. On a board
// that changes every few seconds that is the normal case, not a rare race.
//
// Pass [PageInfo.NextCursor] back as [GetBoardParams.Cursor] and change nothing
// else — a cursor is bound to the ordering and the filters it was minted under
// and is rejected [ErrInvalidCursor] if presented with different ones. The
// cursor is opaque; parsing one is a bug that will surface as a 400 when the
// encoding changes.
//
// # Staleness
//
// Compute freshness against [BoardPage.AsOf], not against the local clock: a
// skewed client clock would otherwise make a fresh board look stale, and
// staleness is this system's headline SLO.
func (c *Client) Board(ctx context.Context, params GetBoardParams) (*BoardPage, error) {
	q := url.Values{}
	setTime(q, "starting_before", params.StartingBefore)
	setInt32(q, "limit", params.Limit)
	setString(q, "cursor", params.Cursor)
	setOddsFormat(q, params.OddsFormat)
	setBooks(q, params.Book)

	var out BoardPage
	err := c.do(ctx, call{
		op:     "GET /board",
		method: http.MethodGet,
		path:   "/board",
		query:  q,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// LeagueBoard is [Client.Board] restricted to one league.
func (c *Client) LeagueBoard(ctx context.Context, leagueSlug string, params GetLeagueBoardParams) (*BoardPage, error) {
	q := url.Values{}
	setTime(q, "starting_before", params.StartingBefore)
	setInt32(q, "limit", params.Limit)
	setString(q, "cursor", params.Cursor)
	setOddsFormat(q, params.OddsFormat)
	setBooks(q, params.Book)

	var out BoardPage
	err := c.do(ctx, call{
		op:     "GET /leagues/{leagueSlug}/board",
		method: http.MethodGet,
		path:   "/leagues/" + pathEscape(leagueSlug) + "/board",
		query:  q,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Event returns one event with its full market tree.
func (c *Client) Event(ctx context.Context, eventID string, params GetEventParams) (*EventDetail, error) {
	q := url.Values{}
	setOddsFormat(q, params.OddsFormat)
	setBooks(q, params.Book)

	var out EventDetail
	err := c.do(ctx, call{
		op:     "GET /events/{eventId}",
		method: http.MethodGet,
		path:   "/events/" + pathEscape(eventID),
		query:  q,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CompareMarketPrices returns every book's current quote for one market, side
// by side, with the best price per selection.
//
// This is the multi-book comparison the analytics surface is built on: the
// no-vig fair value is derived from the book designated sharp, and every other
// book's price is measured against it.
func (c *Client) CompareMarketPrices(ctx context.Context, marketID string, params CompareMarketPricesParams) (*MarketComparison, error) {
	q := url.Values{}
	setOddsFormat(q, params.OddsFormat)
	setBooks(q, params.Book)

	var out MarketComparison
	err := c.do(ctx, call{
		op:     "GET /markets/{marketId}/prices",
		method: http.MethodGet,
		path:   "/markets/" + pathEscape(marketID) + "/prices",
		query:  q,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SelectionHistory returns line movement for one selection at one book, from
// the Timescale hypertable.
//
// [GetSelectionHistoryParams.Book] and .From are REQUIRED by the server: a
// chart mixing two books' quotes on one series is not a line-movement chart but
// two of them overlaid, and an unbounded window over a continuously-written
// hypertable is a table scan. This method sends both unconditionally, so a
// zero-valued From is a 400 from the server rather than a silently unbounded
// query.
//
// A window and resolution that would exceed
// [GetSelectionHistoryParams.MaxPoints] is refused with [ErrInvalidRequest]
// (422) rather than truncated — a truncated series is a chart that is wrong
// without looking wrong.
func (c *Client) SelectionHistory(ctx context.Context, selectionID string, params GetSelectionHistoryParams) (*HistorySeries, error) {
	q := url.Values{}
	q.Set("book", params.Book)
	q.Set("from", params.From.UTC().Format(time.RFC3339Nano))
	setTime(q, "to", params.To)
	if params.Resolution != nil {
		q.Set("resolution", string(*params.Resolution))
	}
	setInt32(q, "max_points", params.MaxPoints)
	setOddsFormat(q, params.OddsFormat)

	var out HistorySeries
	err := c.do(ctx, call{
		op:     "GET /selections/{selectionId}/history",
		method: http.MethodGet,
		path:   "/selections/" + pathEscape(selectionID) + "/history",
		query:  q,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Search finds events by competitor name prefix.
//
// The query is matched literally — the server escapes `%` and `_` before it
// reaches SQL — so a caller cannot turn a prefix search into a leading-wildcard
// scan, deliberately or otherwise.
func (c *Client) Search(ctx context.Context, params SearchEventsParams) (*SearchPage, error) {
	q := url.Values{}
	q.Set("q", params.Q)
	setInt32(q, "limit", params.Limit)
	setString(q, "cursor", params.Cursor)

	var out SearchPage
	err := c.do(ctx, call{
		op:     "GET /search",
		method: http.MethodGet,
		path:   "/search",
		query:  q,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// -----------------------------------------------------------------------------
// query encoding
// -----------------------------------------------------------------------------
//
// Optional parameters are pointers in the generated structs, and a nil pointer
// must produce NO parameter rather than an empty one. That distinction is not
// cosmetic: `?cursor=` is a present-but-empty cursor, which the server rejects
// as undecodable, where an absent cursor means "the first page".

func setString(q url.Values, key string, v *string) {
	if v != nil && *v != "" {
		q.Set(key, *v)
	}
}

func setInt32(q url.Values, key string, v *int32) {
	if v != nil {
		q.Set(key, strconv.FormatInt(int64(*v), 10))
	}
}

// setTime encodes an instant as RFC 3339 in UTC.
//
// UTC rather than the caller's zone so two clients asking for the same instant
// send the same string — which matters because a cursor is bound to the filters
// it was minted under, and "the same filter" has to be a byte comparison
// somewhere.
func setTime(q url.Values, key string, v *time.Time) {
	if v != nil && !v.IsZero() {
		q.Set(key, v.UTC().Format(time.RFC3339Nano))
	}
}

func setOddsFormat(q url.Values, v *OddsFormat) {
	if v != nil && *v != "" {
		q.Set("odds_format", string(*v))
	}
}

// setBooks encodes the repeatable `book` filter.
//
// Repeated `book=a&book=b`, which is `style: form, explode: true` in the spec —
// NOT a comma-joined list. A comma-joined value would be one book slug
// containing a comma as far as the server is concerned, and a slug cannot
// contain one, so the request would fail as an unknown book rather than
// filtering.
func setBooks(q url.Values, v *BookFilter) {
	if v == nil {
		return
	}
	for _, slug := range *v {
		if slug != "" {
			q.Add("book", slug)
		}
	}
}
