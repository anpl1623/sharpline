package httpapi

import (
	"context"
	"net/http"
	"slices"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// Catalogue reads: sports, leagues, books, the event detail page and the
// multi-book comparison grid.
//
// EVERY VALUE HERE COMES OUT OF POSTGRES. There is no sample event, no canned
// league list and no placeholder price anywhere in this file. Against a database
// that `ingest` has not yet filled, every endpoint returns an empty collection
// with a 200 — which is the CORRECT answer, and the one that makes an empty
// board diagnosable as "the pipeline is not running" rather than mistakable for
// "the API is broken".

func (a *API) handleListSports(w http.ResponseWriter, r *http.Request) {
	sports, err := a.catalogue.Sports(r.Context())
	if err != nil {
		failWith(w, r, a.log, "list sports", err)
		return
	}

	out := gen.SportPage{
		Data: make([]gen.Sport, 0, len(sports)),
		Page: singlePage(len(sports)),
	}
	for _, s := range sports {
		out.Data = append(out.Data, wireSport(s))
	}
	respond(w, http.StatusOK, out)
}

func (a *API) handleListLeagues(w http.ResponseWriter, r *http.Request) {
	slug, ok := pathSlug(r, "sportSlug")
	if !ok {
		failNotFound(w, r)
		return
	}

	// The sport is resolved first so an unknown slug is a 404 rather than an
	// empty list. "This sport has no leagues yet" and "there is no such sport"
	// are different answers and a client that cannot tell them apart cannot
	// render either honestly.
	sports, err := a.catalogue.Sports(r.Context())
	if err != nil {
		failWith(w, r, a.log, "list leagues: sports", err)
		return
	}
	idx := slices.IndexFunc(sports, func(s Sport) bool { return s.Slug == slug })
	if idx < 0 {
		failNotFound(w, r)
		return
	}

	leagues, err := a.catalogue.LeaguesInSport(r.Context(), sports[idx].ID)
	if err != nil {
		failWith(w, r, a.log, "list leagues", err)
		return
	}

	out := gen.LeaguePage{
		Data: make([]gen.League, 0, len(leagues)),
		Page: singlePage(len(leagues)),
	}
	for _, l := range leagues {
		out.Data = append(out.Data, wireLeague(l))
	}
	respond(w, http.StatusOK, out)
}

func (a *API) handleListBooks(w http.ResponseWriter, r *http.Request) {
	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "list books", err)
		return
	}

	out := gen.BookPage{
		Data: make([]gen.Book, 0, len(books)),
		Page: singlePage(len(books)),
	}
	for _, b := range books {
		out.Data = append(out.Data, wireBook(b))
	}
	respond(w, http.StatusOK, out)
}

// handleGetEvent serves the event detail page: the event, its breadcrumb, its
// full market tree and every book's current price on every selection.
//
// Four round trips, not one per market: event+league+sport (one join), markets
// (one), selections (one, over the market set), quotes (one, over the selection
// set). The N+1 shape is what makes a detail page with forty player props take a
// second to render.
func (a *API) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	eventID, ok := pathEventID(r)
	if !ok {
		failNotFound(w, r)
		return
	}

	q := r.URL.Query()
	bad := &badParams{}
	format := parseOddsFormat(q, bad)

	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "event detail: books", err)
		return
	}
	filter := parseBookFilter(q, books, bad)
	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	event, league, sport, err := a.catalogue.EventWithBreadcrumb(r.Context(), eventID)
	if err != nil {
		a.notFoundOr(w, r, "event detail", err)
		return
	}

	tree, err := a.marketTree(r.Context(), []Event{event}, filter, books, format)
	if err != nil {
		failWith(w, r, a.log, "event detail: market tree", err)
		return
	}

	respond(w, http.StatusOK, gen.EventDetail{
		Event:   wireEvent(event),
		League:  wireLeague(league),
		Sport:   wireSport(sport),
		Markets: tree[event.ID],
		AsOf:    a.now().UTC(),
	})
}

// marketTree builds the markets-with-prices sub-document for a set of events.
//
// Shared by the board and the detail page so the two render an identical tree —
// the board showing a different price from the detail page for the same
// selection is the single most damaging inconsistency this API could have.
func (a *API) marketTree(
	ctx context.Context,
	events []Event,
	filter map[domain.BookID]struct{},
	books []Book,
	format odds.Format,
) (map[domain.EventID][]gen.Market, error) {
	out := make(map[domain.EventID][]gen.Market, len(events))
	if len(events) == 0 {
		return out, nil
	}

	eventIDs := make([]domain.EventID, 0, len(events))
	for _, e := range events {
		eventIDs = append(eventIDs, e.ID)
		out[e.ID] = []gen.Market{}
	}

	markets, err := a.catalogue.MarketsForEvents(ctx, eventIDs)
	if err != nil {
		return nil, err
	}
	if len(markets) == 0 {
		return out, nil
	}

	marketIDs := make([]domain.MarketID, 0, len(markets))
	for _, m := range markets {
		marketIDs = append(marketIDs, m.ID)
	}
	selections, err := a.catalogue.SelectionsForMarkets(ctx, marketIDs)
	if err != nil {
		return nil, err
	}

	byMarket := make(map[domain.MarketID][]Selection, len(markets))
	selectionIDs := make([]domain.SelectionID, 0, len(selections))
	for _, s := range selections {
		byMarket[s.MarketID] = append(byMarket[s.MarketID], s)
		selectionIDs = append(selectionIDs, s.ID)
	}

	quotes, err := a.currentQuotes(ctx, selectionIDs)
	if err != nil {
		return nil, err
	}

	bySelection := make(map[domain.SelectionID][]Quote, len(selectionIDs))
	for _, q := range quotes {
		// The book filter is applied HERE rather than in SQL. The quote query is
		// a DISTINCT ON served entirely by prices_natural_key_idx and adding a
		// book predicate to it would change the access path for a filter that
		// removes at most a handful of rows from a set already bounded by
		// (selections x books). Correctness is identical; the plan is better.
		if filter != nil {
			if _, ok := filter[q.BookID]; !ok {
				continue
			}
		}
		bySelection[q.SelectionID] = append(bySelection[q.SelectionID], q)
	}

	bookIndex := make(map[domain.BookID]Book, len(books))
	for _, b := range books {
		bookIndex[b.ID] = b
	}

	for _, m := range markets {
		out[m.EventID] = append(out[m.EventID], wireMarket(m, byMarket[m.ID], bySelection, bookIndex, format))
	}
	return out, nil
}

// currentQuotes reads the newest quote per (selection, book), through the cache
// when there is one.
//
// # The cache is never the source of truth, and this is what that means in code
//
// A cache MISS falls through to Postgres and the answer is identical. A cache
// ERROR is logged and treated as a total miss — the whole selection set goes to
// Postgres — because a degraded Redis must never turn into a degraded board. And
// a cache HIT is only ever used for selections the cache actually held; there is
// no path here where a partial cache answer is served as if it were complete.
func (a *API) currentQuotes(ctx context.Context, selections []domain.SelectionID) ([]Quote, error) {
	if len(selections) == 0 {
		return nil, nil
	}
	since := a.now().Add(-quoteFreshness)

	if a.cache == nil {
		return a.prices.CurrentQuotes(ctx, selections, since)
	}

	found, missing, err := a.cache.Quotes(ctx, selections)
	if err != nil {
		a.log.WarnContext(ctx, "price cache unavailable; reading through to postgres",
			"error", err.Error())
		found, missing = nil, selections
	}
	if len(missing) == 0 {
		return found, nil
	}

	fresh, err := a.prices.CurrentQuotes(ctx, missing, since)
	if err != nil {
		return nil, err
	}

	// Best effort. A failed cache write has no effect on the correctness of the
	// response already computed, so it is logged and dropped rather than
	// failing a request that has its answer.
	if err := a.cache.Store(ctx, fresh, quoteFreshness); err != nil {
		a.log.WarnContext(ctx, "price cache write failed", "error", err.Error())
	}
	return append(found, fresh...), nil
}

// handleCompareMarket serves the multi-book comparison grid for one market.
//
// The overround per book is computed HERE, from the quotes in this very
// response, rather than read from anywhere. Two reasons. It is a pure function
// of those quotes (odds.Overround), so a client can check the arithmetic against
// the numbers beside it. And a stored copy would be a second truth able to
// disagree with the prices it sits next to — which is exactly the bug a
// margin display must never have.
func (a *API) handleCompareMarket(w http.ResponseWriter, r *http.Request) {
	marketID, ok := pathMarketID(r)
	if !ok {
		failNotFound(w, r)
		return
	}

	q := r.URL.Query()
	bad := &badParams{}
	format := parseOddsFormat(q, bad)

	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "compare market: books", err)
		return
	}
	filter := parseBookFilter(q, books, bad)
	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	market, err := a.catalogue.Market(r.Context(), marketID)
	if err != nil {
		a.notFoundOr(w, r, "compare market", err)
		return
	}

	selections, err := a.catalogue.SelectionsForMarkets(r.Context(), []domain.MarketID{marketID})
	if err != nil {
		failWith(w, r, a.log, "compare market: selections", err)
		return
	}
	slices.SortStableFunc(selections, func(x, y Selection) int {
		if x.Role.DisplayOrder() != y.Role.DisplayOrder() {
			return x.Role.DisplayOrder() - y.Role.DisplayOrder()
		}
		return compareStrings(x.ID.String(), y.ID.String())
	})

	selectionIDs := make([]domain.SelectionID, 0, len(selections))
	for _, s := range selections {
		selectionIDs = append(selectionIDs, s.ID)
	}
	quotes, err := a.currentQuotes(r.Context(), selectionIDs)
	if err != nil {
		failWith(w, r, a.log, "compare market: quotes", err)
		return
	}

	respond(w, http.StatusOK, a.comparison(market, selections, quotes, books, filter, format))
}

// comparison assembles the grid. Split out from the handler so it is testable
// without an HTTP round trip and so the overround rule has one home.
func (a *API) comparison(
	market Market,
	selections []Selection,
	quotes []Quote,
	books []Book,
	filter map[domain.BookID]struct{},
	format odds.Format,
) gen.MarketComparison {
	bookIndex := make(map[domain.BookID]Book, len(books))
	for _, b := range books {
		bookIndex[b.ID] = b
	}
	roles := make(map[domain.SelectionID]domain.SelectionRole, len(selections))
	for _, s := range selections {
		roles[s.ID] = s.Role
	}

	byBook := make(map[domain.BookID][]Quote)
	best := make(map[domain.SelectionID]Quote)
	for _, qt := range quotes {
		if filter != nil {
			if _, ok := filter[qt.BookID]; !ok {
				continue
			}
		}
		byBook[qt.BookID] = append(byBook[qt.BookID], qt)
		if cur, ok := best[qt.SelectionID]; !ok || qt.Odds > cur.Odds {
			best[qt.SelectionID] = qt
		}
	}

	out := gen.MarketComparison{
		Market: wireMarketHeader(market),
		Books:  make([]gen.BookQuote, 0, len(byBook)),
		Best:   make([]gen.BestPrice, 0, len(selections)),
		AsOf:   a.now().UTC(),
	}

	bookIDs := make([]domain.BookID, 0, len(byBook))
	for id := range byBook {
		bookIDs = append(bookIDs, id)
	}
	slices.SortFunc(bookIDs, func(x, y domain.BookID) int {
		return compareStrings(bookSlug(bookIndex, x), bookSlug(bookIndex, y))
	})

	for _, id := range bookIDs {
		bq := gen.BookQuote{
			BookId:      id.String(),
			BookSlug:    bookSlug(bookIndex, id),
			BookName:    bookIndex[id].Name,
			BookKind:    gen.BookKind(bookIndex[id].Kind.String()),
			IsReference: bookIndex[id].Reference,
			Quotes:      make([]gen.SelectionQuote, 0, len(byBook[id])),
		}

		rows := byBook[id]
		slices.SortStableFunc(rows, func(x, y Quote) int {
			return roles[x.SelectionID].DisplayOrder() - roles[y.SelectionID].DisplayOrder()
		})

		prices := make([]odds.Decimal, 0, len(rows))
		for _, qt := range rows {
			prob, err := qt.Odds.Probability()
			if err != nil {
				prob = odds.Probability(1 / float64(qt.Odds))
			}
			bq.Quotes = append(bq.Quotes, gen.SelectionQuote{
				SelectionId:        qt.SelectionID.String(),
				Role:               gen.SelectionRole(roles[qt.SelectionID].String()),
				DecimalOdds:        float64(qt.Odds),
				ImpliedProbability: float64(prob),
				Display:            renderOdds(qt.Odds, format),
				Line:               qt.Line,
				ObservedAt:         qt.ObservedAt.UTC(),
				IngestedAt:         qt.IngestedAt.UTC(),
			})
			prices = append(prices, qt.Odds)
		}

		// AN OVERROUND OVER A PARTIAL MARKET IS NOT A MARGIN. If a book has not
		// quoted every selection, sum(1/odds) is missing terms and comes out
		// smaller — which would render as a tighter, more attractive margin than
		// any book on the board actually offers. So it is null, and the client
		// shows nothing rather than something wrong.
		if len(prices) == len(selections) && len(prices) > 0 {
			if over, err := odds.Overround(prices); err == nil {
				bq.Overround = &over
			}
		}
		out.Books = append(out.Books, bq)
	}

	for _, s := range selections {
		qt, ok := best[s.ID]
		if !ok {
			continue
		}
		out.Best = append(out.Best, gen.BestPrice{
			SelectionId: s.ID.String(),
			Role:        gen.SelectionRole(s.Role.String()),
			BookId:      qt.BookID.String(),
			BookSlug:    bookSlug(bookIndex, qt.BookID),
			DecimalOdds: float64(qt.Odds),
			Display:     renderOdds(qt.Odds, format),
			ObservedAt:  qt.ObservedAt.UTC(),
		})
	}
	return out
}

func compareStrings(x, y string) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	default:
		return 0
	}
}
