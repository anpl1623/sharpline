package httpapi

import (
	"net/http"
	"strconv"

	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// The live odds board (CLAUDE.md §6 Core: "live odds board across leagues"),
// and search.
//
// Both are keyset-paginated. cursor.go carries the full argument for why offset
// pagination is wrong on a continuously-written set; the short version is that
// OFFSET silently skips rows when the set grows underneath the reader, and this
// set grows every few seconds.

func (a *API) handleBoard(w http.ResponseWriter, r *http.Request) {
	a.board(w, r, League{})
}

func (a *API) handleLeagueBoard(w http.ResponseWriter, r *http.Request) {
	slug, ok := pathSlug(r, "leagueSlug")
	if !ok {
		failNotFound(w, r)
		return
	}
	league, err := a.catalogue.LeagueBySlug(r.Context(), slug)
	if err != nil {
		// A 404 here rather than an empty board. "This league has no fixtures
		// today" and "there is no such league" are different answers, and a
		// client that cannot tell them apart cannot render either honestly.
		a.notFoundOr(w, r, "league board", err)
		return
	}
	a.board(w, r, league)
}

// board serves one page of the odds board, optionally scoped to a league.
//
// The league-scoped and cross-league forms share every line but the query
// predicate, because they must produce byte-identical entries for the same
// event: a fixture that reads differently on /board and on /leagues/nfl/board is
// the kind of inconsistency that makes a whole surface untrustworthy.
func (a *API) board(w http.ResponseWriter, r *http.Request, league League) {
	q := r.URL.Query()
	bad := &badParams{}

	limit := parseLimit(q, bad)
	format := parseOddsFormat(q, bad)
	startingBefore := parseTime(q, "starting_before", a.now().Add(defaultBoardHorizon).UTC(), bad)

	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "board: books", err)
		return
	}
	filter := parseBookFilter(q, books, bad)

	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	// THE SCOPE FINGERPRINT. Everything that decides WHICH ROWS are in the set
	// and in what order goes in; nothing that decides how they are rendered
	// does. `starting_before` is in because it bounds the set. `league` is in
	// because it filters it. `limit`, `odds_format` and `book` are OUT: they
	// change the page's size or its rendering, not its membership, and
	// rejecting a cursor because the reader resized the page would be
	// user-hostile for no benefit.
	scope := cursorScope("board", league.ID.String(), strconv.FormatInt(startingBefore.UTC().UnixNano(), 10))

	after, ok := a.pageCursor(w, r, q, scope)
	if !ok {
		return
	}

	page, err := a.catalogue.EventPage(r.Context(), EventPageQuery{
		LeagueID:       league.ID,
		StartingBefore: startingBefore,
		After:          after,
		Limit:          limit,
	})
	if err != nil {
		failWith(w, r, a.log, "board: events", err)
		return
	}

	tree, err := a.marketTree(r.Context(), page.Events, filter, books, format)
	if err != nil {
		failWith(w, r, a.log, "board: market tree", err)
		return
	}

	out := gen.BoardPage{
		Data: make([]gen.BoardEntry, 0, len(page.Events)),
		// as_of is read ONCE and stamped on the page. A client computes staleness
		// against this rather than against its own clock, so a skewed browser
		// clock cannot make a fresh board look stale — and every price in this
		// response is measured against the same instant.
		AsOf: a.now().UTC(),
	}
	for _, e := range page.Events {
		out.Data = append(out.Data, gen.BoardEntry{
			Event:   wireEvent(e),
			Markets: tree[e.ID],
		})
	}
	out.Page = wirePage(limit, page.HasMore, nextEventCursor(page.Events, page.HasMore, scope))

	respond(w, http.StatusOK, out)
}

func (a *API) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bad := &badParams{}

	prefix := parseSearchPrefix(q, bad)
	limit := parseLimit(q, bad)
	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	// The prefix is part of the scope: continuing a page for "cel" against the
	// results for "celt" would return rows the client never saw the start of.
	scope := cursorScope("search", prefix)

	after, ok := a.pageCursor(w, r, q, scope)
	if !ok {
		return
	}

	page, err := a.catalogue.SearchEvents(r.Context(), SearchQuery{
		Prefix: prefix,
		After:  after,
		Limit:  limit,
	})
	if err != nil {
		failWith(w, r, a.log, "search", err)
		return
	}

	out := gen.SearchPage{Data: make([]gen.SearchHit, 0, len(page.Events))}
	for _, e := range page.Events {
		out.Data = append(out.Data, wireSearchHit(e))
	}
	out.Page = wirePage(limit, page.HasMore, nextEventCursor(page.Events, page.HasMore, scope))

	respond(w, http.StatusOK, out)
}

// pageCursor decodes the `cursor` parameter against a scope, answering 400 and
// reporting false if it does not belong to this query.
//
// A nil return with ok==true means "first page", which is the correct reading of
// an absent cursor.
func (a *API) pageCursor(w http.ResponseWriter, r *http.Request, q map[string][]string, scope uint64) (*EventKey, bool) {
	raw := first(q, "cursor")
	if raw == "" {
		return nil, true
	}
	c, err := decodeCursor(raw, scope)
	if err != nil {
		// invalid_cursor rather than invalid_parameter: a client holding a
		// cursor from an older deployment or from a different query needs to
		// restart the listing, and that is a different remedy from "fix your
		// parameter".
		fail(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidCursor, msgInvalidCursor)
		return nil, false
	}
	key := c.key
	return &key, true
}

// nextEventCursor mints the continuation token from the LAST ROW of the page.
//
// It returns "" when there is no next page, so the envelope carries
// `next_cursor: null` rather than a token that would return nothing. A cursor
// that exists but yields an empty page is how a client ends up in a polling loop
// against the end of a list.
func nextEventCursor(events []Event, hasMore bool, scope uint64) string {
	if !hasMore || len(events) == 0 {
		return ""
	}
	last := events[len(events)-1]
	return encodeCursor(cursor{
		key:   EventKey{ScheduledStart: last.ScheduledStart, ID: last.ID},
		scope: scope,
	})
}
