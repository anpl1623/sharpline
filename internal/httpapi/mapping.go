package httpapi

import (
	"cmp"
	"slices"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// Read model -> wire.
//
// One function per type, all of them total, none of them able to fail. A mapping
// that can return an error is a mapping that will, halfway through serialising a
// board, and there is no good answer at that point — so every conversion here is
// either infallible or has a documented, deliberate fallback.
//
// Enums cross the boundary through the domain's own String(): the spec's enum
// members are declared to BE those strings (openapi.yaml says so on each one),
// so a divergence is caught by mapping_test.go, which asserts every domain
// member round-trips through the generated enum's Valid().

// -----------------------------------------------------------------------------
// Catalogue
// -----------------------------------------------------------------------------

func wireSport(s Sport) gen.Sport {
	return gen.Sport{Id: s.ID.String(), Slug: s.Slug.String(), Name: s.Name}
}

func wireLeague(l League) gen.League {
	return gen.League{
		Id:      l.ID.String(),
		SportId: l.SportID.String(),
		Slug:    l.Slug.String(),
		Name:    l.Name,
	}
}

func wireBook(b Book) gen.Book {
	return gen.Book{
		Id:          b.ID.String(),
		Slug:        b.Slug.String(),
		Name:        b.Name,
		Kind:        gen.BookKind(b.Kind.String()),
		IsReference: b.Reference,
	}
}

func wireEvent(e Event) gen.EventSummary {
	out := gen.EventSummary{
		Id:             e.ID.String(),
		LeagueId:       e.LeagueID.String(),
		Kind:           gen.EventKind(e.Kind.String()),
		Name:           e.Name,
		ScheduledStart: e.ScheduledStart.UTC(),
		Status:         gen.EventStatus(e.Status.String()),
		ObservedAt:     e.ObservedAt.UTC(),
	}

	// An outright event has no competitors AT ALL, which is a different fact
	// from two empty ones. domain/event.go makes the same point: making Home and
	// Away optional on every event is how "the home team is empty" becomes a
	// runtime surprise in the middle of a board render. So the field is absent
	// rather than present-and-blank.
	if c, ok := wireCompetitor(e.HomeCompetitorID, e.HomeCompetitorName); ok {
		out.HomeCompetitor = &c
	}
	if c, ok := wireCompetitor(e.AwayCompetitorID, e.AwayCompetitorName); ok {
		out.AwayCompetitor = &c
	}

	if e.Clock != nil {
		clock := gen.GameClock{Running: e.Clock.Running, Period: e.Clock.Period}
		if e.Clock.Elapsed != nil {
			// Seconds on the wire, nanoseconds in the domain. A JSON number of
			// nanoseconds for a 3-hour game is ~1.08e13, still inside 2^53, but
			// no consumer of a game clock wants nanosecond resolution and a
			// browser rendering it would divide by 1e9 in three places.
			secs := int64(*e.Clock.Elapsed / time.Second)
			clock.ElapsedSeconds = &secs
		}
		out.Clock = &clock
	}
	if e.Score != nil {
		out.Score = &gen.Score{Home: e.Score.Home, Away: e.Score.Away}
	}
	return out
}

func wireCompetitor(id *domain.CompetitorID, name string) (gen.Competitor, bool) {
	if name == "" && id == nil {
		return gen.Competitor{}, false
	}
	c := gen.Competitor{Name: name}
	if id != nil {
		s := id.String()
		c.Id = &s
	}
	return c, true
}

func wireSearchHit(e Event) gen.SearchHit {
	hit := gen.SearchHit{
		Id:             e.ID.String(),
		LeagueId:       e.LeagueID.String(),
		Kind:           gen.EventKind(e.Kind.String()),
		Name:           e.Name,
		ScheduledStart: e.ScheduledStart.UTC(),
		Status:         gen.EventStatus(e.Status.String()),
	}
	if e.HomeCompetitorName != "" {
		n := e.HomeCompetitorName
		hit.HomeCompetitorName = &n
	}
	if e.AwayCompetitorName != "" {
		n := e.AwayCompetitorName
		hit.AwayCompetitorName = &n
	}
	return hit
}

func wireMarketHeader(m Market) gen.MarketHeader {
	h := gen.MarketHeader{
		Id:         m.ID.String(),
		EventId:    m.EventID.String(),
		Type:       gen.MarketType(m.Type.String()),
		Status:     gen.MarketStatus(m.Status.String()),
		ObservedAt: m.ObservedAt.UTC(),
		Line:       m.Line,
	}
	if m.Subject != "" {
		s := m.Subject
		h.Subject = &s
	}
	return h
}

// wireMarket builds one market with its selections and their current prices.
//
// `quotes` is keyed by selection and already filtered by any book filter; books
// is keyed by id so a price can carry the book's slug without a lookup per row.
func wireMarket(
	m Market,
	selections []Selection,
	quotes map[domain.SelectionID][]Quote,
	books map[domain.BookID]Book,
	format odds.Format,
) gen.Market {
	out := gen.Market{
		Id:         m.ID.String(),
		EventId:    m.EventID.String(),
		Type:       gen.MarketType(m.Type.String()),
		Status:     gen.MarketStatus(m.Status.String()),
		ObservedAt: m.ObservedAt.UTC(),
		Line:       m.Line,
		Selections: make([]gen.Selection, 0, len(selections)),
	}
	if m.Subject != "" {
		s := m.Subject
		out.Subject = &s
	}

	// DISPLAY ORDER IS A DOMAIN FACT AND IS APPLIED HERE.
	// domain.SelectionRole.DisplayOrder() is home, draw, away, over, under,
	// outright — which is not the lexicographic order of those strings, so no
	// index and no SQL ORDER BY can produce it (catalogue.sql says exactly this
	// and returns the rows unordered on purpose). Sorting in the API rather than
	// in each client is what makes every surface render the same tree.
	ordered := slices.Clone(selections)
	slices.SortStableFunc(ordered, func(x, y Selection) int {
		if c := cmp.Compare(x.Role.DisplayOrder(), y.Role.DisplayOrder()); c != 0 {
			return c
		}
		// A total order, so two selections sharing a role (two named runners in
		// an outright market) do not swap between renders.
		return cmp.Compare(x.ID.String(), y.ID.String())
	})

	for _, sel := range ordered {
		out.Selections = append(out.Selections, wireSelection(sel, quotes[sel.ID], books, format))
	}
	return out
}

func wireSelection(s Selection, quotes []Quote, books map[domain.BookID]Book, format odds.Format) gen.Selection {
	out := gen.Selection{
		Id:       s.ID.String(),
		MarketId: s.MarketID.String(),
		Role:     gen.SelectionRole(s.Role.String()),
		Name:     s.Name,
		// Non-nil even when empty: `[]` means "no book is quoting this
		// selection inside the freshness window", which is a correct answer,
		// where JSON `null` means "this field was not computed" and makes a
		// client branch on a distinction the API never intends.
		Prices: make([]gen.Price, 0, len(quotes)),
	}

	ordered := slices.Clone(quotes)
	slices.SortFunc(ordered, func(x, y Quote) int {
		return cmp.Compare(bookSlug(books, x.BookID), bookSlug(books, y.BookID))
	})

	var best *gen.Price
	for _, q := range ordered {
		p := wirePrice(q, books, format)
		out.Prices = append(out.Prices, p)

		// Best = longest odds = biggest return per unit staked. Computed here so
		// "best" means one thing on every surface, and computed over the quotes
		// in THIS response so a client can check the arithmetic.
		if best == nil || p.DecimalOdds > best.DecimalOdds {
			cp := p
			best = &cp
		}
	}
	out.BestPrice = best
	return out
}

func wirePrice(q Quote, books map[domain.BookID]Book, format odds.Format) gen.Price {
	// Probability() fails only for odds outside (1, 100000], which the database
	// CHECK on prices.decimal_odds already forbids. The fallback is the plain
	// reciprocal rather than a zero, because a zero would render as "0%
	// implied", a number a UI would happily draw.
	prob, err := q.Odds.Probability()
	if err != nil {
		prob = odds.Probability(1 / float64(q.Odds))
	}
	return gen.Price{
		BookId:             q.BookID.String(),
		BookSlug:           bookSlug(books, q.BookID),
		DecimalOdds:        float64(q.Odds),
		ImpliedProbability: float64(prob),
		Display:            renderOdds(q.Odds, format),
		Line:               q.Line,
		ObservedAt:         q.ObservedAt.UTC(),
		IngestedAt:         q.IngestedAt.UTC(),
	}
}

// bookSlug resolves a book id to its slug, falling back to the id.
//
// The fallback cannot happen against a consistent database — every price has a
// foreign key to `books` — but a board page that 500s because one book row was
// deleted mid-request is worse than one that labels a column with an id.
func bookSlug(books map[domain.BookID]Book, id domain.BookID) string {
	if b, ok := books[id]; ok {
		return b.Slug.String()
	}
	return id.String()
}

// -----------------------------------------------------------------------------
// Pagination
// -----------------------------------------------------------------------------

// wirePage builds the page envelope. `next` is empty when there is no next page.
func wirePage(limit int32, hasMore bool, next string) gen.PageInfo {
	p := gen.PageInfo{Limit: limit, HasMore: hasMore}
	if hasMore && next != "" {
		p.NextCursor = &next
	}
	return p
}

// singlePage is the envelope for a list whose row count is bounded by the
// catalogue rather than by time or traffic: sports, leagues in a sport, books.
//
// They carry the same envelope as a paginated list so a client has one shape to
// handle, and they always report has_more false. Paginating them would be
// ceremony over a result that fits in one screen — and the query comments in
// catalogue.sql make the same argument for why those tables have no index worth
// adding.
func singlePage(n int) gen.PageInfo {
	return gen.PageInfo{Limit: int32(n), HasMore: false}
}
