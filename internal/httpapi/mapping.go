package httpapi

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/anpl1623/sharpline/internal/betting"
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

// -----------------------------------------------------------------------------
// Betting
// -----------------------------------------------------------------------------

// wireWager maps a placed ticket onto the wire.
//
// Every price on it is HISTORICAL. gen.Wager.DecimalOdds is the ticket price the
// customer accepted and gen.WagerLeg.DecimalOdds is the price that leg was
// booked at; neither tracks the market, and there is no field on either that
// could. A client wanting the current line asks the catalogue for it.
func wireWager(w Wager, books map[domain.BookID]Book, format odds.Format) gen.Wager {
	out := gen.Wager{
		Id:                   w.ID.String(),
		Kind:                 gen.WagerKind(w.Kind.String()),
		Status:               gen.WagerStatus(w.Status.String()),
		StakeMinor:           w.Stake.MinorUnits(),
		DecimalOdds:          float64(w.Decimal),
		Display:              renderOdds(w.Decimal, format),
		Rounding:             gen.Rounding(w.Rounding.String()),
		PotentialPayoutMinor: w.PotentialPayout.MinorUnits(),
		PotentialProfitMinor: w.PotentialProfit.MinorUnits(),
		TeaserPoints:         w.TeaserPoints,
		PlacedAt:             w.PlacedAt.UTC(),
		UpdatedAt:            w.UpdatedAt.UTC(),
		Legs:                 make([]gen.WagerLeg, 0, len(w.Legs)),
	}
	if w.RoundRobinID != nil {
		id := w.RoundRobinID.String()
		out.RoundRobinId = &id
	}
	// The pair travels together or not at all: migration 00006's
	// wagers_return_pair_complete makes "one set, one null" unstorable, and
	// emitting one without the other would invent a state the database refuses.
	if w.Returned != nil && w.NetReturn != nil {
		returned := w.Returned.MinorUnits()
		net := w.NetReturn.MinorUnits()
		out.ReturnedMinor = &returned
		out.NetReturnMinor = &net
	}
	if at, ok := w.SettledAt(); ok {
		settled := at.UTC()
		out.SettledAt = &settled
	}
	for _, leg := range w.Legs {
		out.Legs = append(out.Legs, wireWagerLeg(leg, books, format))
	}
	return out
}

func wireWagerLeg(l WagerLeg, books map[domain.BookID]Book, format odds.Format) gen.WagerLeg {
	out := gen.WagerLeg{
		Id:              l.ID.String(),
		EventId:         l.EventID.String(),
		MarketId:        l.MarketID.String(),
		MarketType:      gen.MarketType(l.MarketType.String()),
		SelectionId:     l.SelectionID.String(),
		Role:            gen.SelectionRole(l.Role.String()),
		Status:          gen.LegStatus(l.Status.String()),
		BookId:          l.BookID.String(),
		BookSlug:        bookSlug(books, l.BookID),
		DecimalOdds:     float64(l.Decimal),
		Display:         renderOdds(l.Decimal, format),
		Line:            linePtr(l.Line),
		TeasedLine:      linePtr(l.TeasedLine),
		GradingLine:     linePtr(l.GradingLine()),
		PriceObservedAt: l.PriceObservedAt.UTC(),
	}
	if l.GradedAt != nil {
		at := l.GradedAt.UTC()
		out.GradedAt = &at
	}
	return out
}

// linePtr renders a domain.Line as the wire's `number | null`.
//
// The distinction it preserves is the reason domain.Line is a struct rather than
// a *float64 in the first place: an ABSENT line (a moneyline, a futures market)
// and a line of 0.0 (a traded pick'em) are different facts, and collapsing them
// would make a pick'em render as "no handicap". domain.Line.MarshalJSON draws
// the line in the same place; this is that rule applied through the generated
// wire type, which cannot hold a domain.Line.
func linePtr(l domain.Line) *float64 {
	v, ok := l.Value()
	if !ok {
		return nil
	}
	return &v
}

// wirePlacement maps the result of a placement onto the wire.
//
// The totals are summed HERE, over the tickets in this very response, rather
// than reported by the service. Two reasons, and they are the same two the
// board's overround argument makes: they are a pure fold over numbers the client
// can see, so the arithmetic is checkable against the rows beside it; and a
// separately-carried total could disagree with the tickets it claims to
// describe, which is exactly the failure a receipt must not have.
func wirePlacement(p betting.Placement, books map[domain.BookID]Book, format odds.Format) (gen.Placement, error) {
	out := gen.Placement{
		Wagers:   make([]gen.Wager, 0, len(p.Wagers)),
		Replayed: p.Replayed,
	}

	stakes := make([]domain.Money, 0, len(p.Wagers))
	payouts := make([]domain.Money, 0, len(p.Wagers))
	for _, w := range p.Wagers {
		out.Wagers = append(out.Wagers, wireWager(wagerFromDomain(w), books, format))
		stakes = append(stakes, w.Stake())
		payouts = append(payouts, w.PotentialPayout())
	}

	// domain.SumMoney rather than a `+=` loop: it reports overflow instead of
	// wrapping, and a round robin can carry a thousand tickets. A wrapped total
	// on a receipt would be a negative number where the customer's money went.
	totalStake, err := domain.SumMoney(stakes...)
	if err != nil {
		return gen.Placement{}, fmt.Errorf("total stake across %d ticket(s): %w", len(stakes), err)
	}
	totalPayout, err := domain.SumMoney(payouts...)
	if err != nil {
		return gen.Placement{}, fmt.Errorf("total payout across %d ticket(s): %w", len(payouts), err)
	}
	totalProfit, err := totalPayout.Sub(totalStake)
	if err != nil {
		return gen.Placement{}, fmt.Errorf("total profit: %w", err)
	}

	out.TotalStakeMinor = totalStake.MinorUnits()
	out.PotentialPayoutMinor = totalPayout.MinorUnits()
	out.PotentialProfitMinor = totalProfit.MinorUnits()

	if rr := p.RoundRobin; !rr.IsZero() {
		set := wireRoundRobin(rr)
		out.RoundRobin = &set
	}
	return out, nil
}

// wireRoundRobin maps the parent expansion.
//
// Its selection SET is deliberately not on the wire. Every selection it was
// built from appears on at least one of the tickets in the same response — for
// any 2 <= k <= n every index is in some k-subset — so a second copy here could
// only ever disagree with the tickets it supposedly generated, and the copy is
// the one nobody would notice was wrong. Migration 00006 declines to store it
// for the same reason.
func wireRoundRobin(rr domain.RoundRobin) gen.RoundRobinTicketSet {
	sizes := rr.Sizes()
	out := gen.RoundRobinTicketSet{
		Id:                       rr.ID().String(),
		SelectionCount:           int32(len(rr.Legs())),
		Sizes:                    make([]int32, 0, len(sizes)),
		StakePerCombinationMinor: rr.StakePerCombination().MinorUnits(),
		CombinationCount:         int32(rr.CombinationCount()),
	}
	for _, size := range sizes {
		out.Sizes = append(out.Sizes, int32(size))
	}
	return out
}

// wireCashOutQuote maps a cash-out quote onto the wire.
//
// The stake, the net return and the margin in cash are derived here from the
// quote and the ticket rather than carried on either. All three are exact
// integer subtractions over numbers already in this response, so a client can
// check them — which is the whole point of quoting off the fair value and
// naming the haircut instead of burying it in a price.
func wireCashOutQuote(q betting.CashOutQuote, w Wager, pending int) (gen.CashOutQuote, error) {
	margin, err := q.FairValue.Sub(q.Value)
	if err != nil {
		return gen.CashOutQuote{}, fmt.Errorf("cash-out margin: %w", err)
	}
	net, err := q.Value.Sub(w.Stake)
	if err != nil {
		return gen.CashOutQuote{}, fmt.Errorf("cash-out net return: %w", err)
	}
	return gen.CashOutQuote{
		WagerId:              q.WagerID.String(),
		StakeMinor:           w.Stake.MinorUnits(),
		PotentialPayoutMinor: w.PotentialPayout.MinorUnits(),
		SurvivalProbability:  q.SurvivalProbability,
		FairValueMinor:       q.FairValue.MinorUnits(),
		MarginBps:            int32(q.MarginBps),
		MarginMinor:          margin.MinorUnits(),
		ValueMinor:           q.Value.MinorUnits(),
		NetReturnMinor:       net.MinorUnits(),
		PendingLegCount:      int32(pending),
		QuotedAt:             q.QuotedAt.UTC(),
	}, nil
}

// priceMovement classifies a re-quote from the CUSTOMER's side.
//
// `lengthened` pays more per unit staked than they saw, `shortened` pays less.
// Named that way rather than up/down because "the odds went up" is ambiguous in
// exactly the direction that matters — an American price of -110 moving to -105
// is a bigger number and a worse bet.
//
// The tolerance is RELATIVE and mirrors internal/betting's own. The two must
// agree, and this one must never be TIGHTER: a quote that flagged a change the
// placement path then treats as unchanged would put a "price changed, accept?"
// prompt in front of a customer whose bet would have gone through untouched.
// The magnitude is safe by an enormous margin — the smallest move a book can
// quote is a tick, which at even money is a relative difference near 5e-3, five
// million times this.
func priceMovement(seen, current float64) gen.PriceMovement {
	if seen <= 0 || current <= 0 {
		// Not a comparable pair. Reporting "unchanged" would be a claim; the
		// caller decides what to do with a leg it could not compare.
		return gen.Unchanged
	}
	scale := math.Max(math.Abs(seen), math.Abs(current))
	if math.Abs(seen-current)/scale <= priceMatchTolerance {
		return gen.Unchanged
	}
	if current > seen {
		return gen.Lengthened
	}
	return gen.Shortened
}

// -----------------------------------------------------------------------------
// Analytics -> wire
//
// Every function below is total and infallible, like the rest of this file. The
// enums have already been parsed at the store boundary (internal/httpapi/pgstore
// is where a raw column string becomes a domain or odds value, and where a
// schema/Go divergence surfaces as a wrapped error), so nothing here can fail on
// an unrecognised member — by the time a finding reaches this file it is already
// well-typed.
//
// DURATIONS BECOME SECONDS, as float64. The domain carries time.Duration because
// nanoseconds are the unit Go compares in; no consumer of a staleness figure
// wants nanosecond resolution, and a browser rendering one would divide by 1e9
// at three call sites. This is the same conversion wireEvent already makes for a
// game clock.
// -----------------------------------------------------------------------------

// seconds renders a duration for the wire.
//
// Signed, and deliberately so: a quote age may be negative when a provider's
// clock runs ahead of ours, and clamping it here would hide clock skew inside
// the one number whose job is to expose staleness.
func seconds(d time.Duration) float64 { return d.Seconds() }

// copyFloat copies an optional float so the wire value cannot alias the read
// model's. Cheap insurance: a caller that mutated a returned page would
// otherwise mutate the store's row through a shared pointer.
func copyFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func wireEVSignal(s EVSignal) gen.EVSignal {
	return gen.EVSignal{
		SelectionId:     s.SelectionID.String(),
		MarketId:        s.MarketID.String(),
		MarketType:      gen.MarketType(s.MarketType.String()),
		LeagueId:        s.LeagueID.String(),
		BookId:          s.BookID.String(),
		ReferenceBookId: s.ReferenceBookID.String(),

		DevigMethod:    gen.DevigMethod(s.DevigMethod.String()),
		OfferedDecimal: float64(s.OfferedDecimal),
		OfferedImplied: s.OfferedImplied,
		Line:           copyFloat(s.Line),

		FairProbability: s.FairProbability,
		FairDecimal:     float64(s.FairDecimal),

		ExpectedValue:        s.ExpectedValue,
		ExpectedValuePercent: s.ExpectedValuePercent,
		Edge:                 s.Edge,
		EdgePercent:          s.EdgePercent,
		Kelly:                s.Kelly,
		FractionalKelly:      s.FractionalKelly,
		KellyFraction:        s.KellyFraction,

		QuoteObservedAt: s.QuoteObservedAt.UTC(),
		QuoteAgeSeconds: seconds(s.QuoteAge),

		ThresholdEvPercent: s.ThresholdEVPercent,
		MaxQuoteAgeSeconds: seconds(s.MaxQuoteAge),
		DetectedAt:         s.DetectedAt.UTC(),
	}
}

// wireArbitrageSignal maps a finding and its legs together.
//
// ReturnPercent is derived HERE rather than stored, for migration 00009's stated
// reason about the margin family: it is a single operation on a number already
// in the response, and a stored derivation is a stored opportunity for two
// surfaces to disagree. The client could compute it; it is on the wire because
// every surface that renders a return should render the same digits.
func wireArbitrageSignal(s ArbitrageSignal) gen.ArbitrageSignal {
	out := gen.ArbitrageSignal{
		Id:         s.ID,
		MarketId:   s.MarketID.String(),
		MarketType: gen.MarketType(s.MarketType.String()),
		LeagueId:   s.LeagueID.String(),
		Line:       copyFloat(s.Line),

		SelectionCount: s.SelectionCount,
		ImpliedSum:     s.ImpliedSum,
		ReturnFraction: s.ReturnFraction,
		ReturnPercent:  s.ReturnFraction * 100,
		DistinctBooks:  s.DistinctBooks,

		ObservedSpreadSeconds: seconds(s.ObservedSpread),
		OldestLegAgeSeconds:   seconds(s.OldestLegAge),
		ObservedAt:            s.ObservedAt.UTC(),

		MaxLegAgeSeconds:         seconds(s.MaxLegAge),
		MaxObservedSpreadSeconds: seconds(s.MaxObservedSpread),
		DetectedAt:               s.DetectedAt.UTC(),

		Legs: make([]gen.ArbitrageLeg, 0, len(s.Legs)),
	}
	for _, l := range s.Legs {
		out.Legs = append(out.Legs, gen.ArbitrageLeg{
			LegIndex:      l.Index,
			SelectionId:   l.SelectionID.String(),
			Role:          gen.SelectionRole(l.Role.String()),
			BookId:        l.BookID.String(),
			DecimalOdds:   float64(l.DecimalOdds),
			Line:          copyFloat(l.Line),
			StakeFraction: l.StakeFraction,
			ObservedAt:    l.ObservedAt.UTC(),
			AgeSeconds:    seconds(l.Age),
		})
	}
	return out
}

// wireSteamSignal maps a steam detection.
//
// A nil DevigMethod becomes the string `none`, which is the schema's own member
// and the EXPECTED value rather than a fallback: a book's margin is very nearly
// constant across a window of seconds, so devigging mostly subtracts the same
// constant from both ends of a difference.
func wireSteamSignal(s SteamSignal) gen.SteamSignal {
	out := gen.SteamSignal{
		MarketId:    s.MarketID.String(),
		MarketType:  gen.MarketType(s.MarketType.String()),
		LeagueId:    s.LeagueID.String(),
		SelectionId: s.SelectionID.String(),

		WindowStart:   s.WindowStart.UTC(),
		WindowEnd:     s.WindowEnd.UTC(),
		WindowSeconds: seconds(s.Window),
		HopSeconds:    seconds(s.Hop),

		Direction:                    gen.SteamDirection(s.Direction),
		DeltaProbability:             s.DeltaProbability,
		MagnitudeProbabilityPoints:   s.Magnitude,
		VelocityProbabilityPerMinute: s.Velocity,
		DevigMethod:                  steamBasis(s.DevigMethod),

		LeadBookId:  s.LeadBookID.String(),
		LeadMovedAt: s.LeadMovedAt.UTC(),

		Followers:            make([]gen.SteamFollower, 0, len(s.Followers)),
		FollowerCount:        s.FollowerCount,
		ParticipatingBooks:   s.ParticipatingBooks,
		CrossBookCorrelation: s.CrossBookCorrelation,

		ThresholdVelocity:     s.ThresholdVelocity,
		ThresholdMagnitude:    s.ThresholdMagnitude,
		ThresholdCorrelation:  s.ThresholdCorrelation,
		MinFollowers:          s.MinFollowers,
		MaxFollowerLagSeconds: seconds(s.MaxFollowerLag),

		DetectedAt: s.DetectedAt.UTC(),
	}
	for _, f := range s.Followers {
		out.Followers = append(out.Followers, gen.SteamFollower{
			BookId:           f.BookID.String(),
			MovedAt:          f.MovedAt.UTC(),
			LagSeconds:       seconds(f.Lag),
			DeltaProbability: f.DeltaProbability,
		})
	}
	return out
}

// steamBasis renders the optional devig method as the wire's five-member enum.
func steamBasis(m *odds.DevigMethod) gen.SteamBasis {
	if m == nil {
		return gen.SteamBasisNone
	}
	return gen.SteamBasis(m.String())
}

func wireCLVEntry(e CLVEntry) gen.CLVEntry {
	return gen.CLVEntry{
		LegId:       e.LegID.String(),
		WagerId:     e.WagerID.String(),
		MarketId:    e.MarketID.String(),
		MarketType:  gen.MarketType(e.MarketType.String()),
		SelectionId: e.SelectionID.String(),
		LeagueId:    e.LeagueID.String(),

		TakenBookId:   e.TakenBookID.String(),
		ClosingBookId: e.ClosingBookID.String(),
		DevigMethod:   gen.DevigMethod(e.DevigMethod.String()),

		TakenLine:   copyFloat(e.TakenLine),
		ClosingLine: copyFloat(e.ClosingLine),
		TakenAt:     e.TakenAt.UTC(),
		ClosedAt:    e.ClosedAt.UTC(),

		TakenFair:    e.TakenFair,
		ClosingFair:  e.ClosingFair,
		TakenPrice:   float64(e.TakenPrice),
		ClosingPrice: float64(e.ClosingPrice),

		ProbabilityClv: e.ProbabilityCLV,
		PercentClv:     e.PercentCLV,
		Magnitude:      e.Magnitude,

		BeatClose: e.BeatClose,
		LineMoved: e.LineMoved,
		LegStatus: gen.LegStatus(e.Status.String()),
		Voided:    e.Voided,
		GradedAt:  e.GradedAt.UTC(),
	}
}

// wireCLVAggregate maps the summary, keeping the null means NULL.
//
// BeatRate is derived here and is nil under exactly the same condition the means
// are, rather than being computed as 0/0 or as 0. A rate over zero samples is
// not zero, it does not exist, and a surface that rendered it as 0% would report
// "never beat the close" about a customer who has no countable wagers at all.
func wireCLVAggregate(a CLVAggregate) gen.CLVAggregate {
	out := gen.CLVAggregate{
		Samples:            a.Samples,
		Counted:            a.Counted,
		VoidExcluded:       a.VoidExcluded,
		LineMovedExcluded:  a.LineMovedExcluded,
		BeatCount:          a.BeatCount,
		MeanProbabilityClv: copyFloat(a.MeanProbabilityCLV),
		MeanPercentClv:     copyFloat(a.MeanPercentCLV),
	}
	if a.Counted > 0 {
		rate := float64(a.BeatCount) / float64(a.Counted)
		out.BeatRate = &rate
	}
	return out
}

func wireCLVLeagueSummary(s CLVLeagueSummary) gen.CLVLeagueSummary {
	out := gen.CLVLeagueSummary{
		LeagueId:           s.LeagueID.String(),
		Counted:            s.Counted,
		BeatCount:          s.BeatCount,
		MeanProbabilityClv: s.MeanProbabilityCLV,
		MeanPercentClv:     s.MeanPercentCLV,
	}
	// The statement's HAVING clause guarantees Counted > 0, so this division is
	// safe. The guard is here anyway because "the query cannot return that" is a
	// property of a file this one does not import.
	if s.Counted > 0 {
		out.BeatRate = float64(s.BeatCount) / float64(s.Counted)
	}
	return out
}

// wireLeaderboardEntry maps one ranked customer.
//
// `rank` is assigned by the caller from the returned order rather than being a
// stored fact: it is a position under one basis, one window and one pair of
// sample floors, and storing it would make it wrong the moment any of those
// changed.
//
// THE ACCOUNT IDENTIFIER DOES NOT APPEAR. [publicHandle] derives the pseudonym;
// see its doc comment for why a handle exists at all.
func wireLeaderboardEntry(e LeaderboardEntry, rank int32) gen.LeaderboardEntry {
	return gen.LeaderboardEntry{
		Rank: rank,
		User: publicHandle(e.UserID),

		SettledWagers:  e.SettledWagers,
		StakedMinor:    e.Staked.MinorUnits(),
		NetReturnMinor: e.NetReturn.MinorUnits(),
		Roi:            e.ROI,
		RoiPercent:     e.ROI * 100,

		ClvSamples:         e.CLVSamples,
		BeatCount:          e.BeatCount,
		BeatRate:           e.BeatRate,
		MeanProbabilityClv: e.MeanProbabilityCLV,
		MeanPercentClv:     e.MeanPercentCLV,
	}
}

// publicHandle derives the pseudonym a leaderboard row is published under.
//
// # Why there is a handle at all
//
// The leaderboard is public (CLAUDE.md §6) and this system stores no display
// name: `users` holds an email address and nothing else, so "show the user's
// name" would mean publishing an email address, and "show the user id" would
// publish an account identifier on an unauthenticated page. Neither is
// acceptable, and a board with no row identity at all cannot be read — a
// customer must be able to find themselves on it.
//
// # Why a hash, and what it is and is not
//
// SHA-256 truncated to 12 hex characters, prefixed. The properties that matter:
// it is DETERMINISTIC, so the same customer is the same handle on every refresh
// and across replicas; it is derived from a value that is already 256 bits of
// crypto/rand (auth.NewOpaqueID), so it cannot be reversed by enumerating a
// small input space the way a hashed email or a hashed sequential id could; and
// it contains no clock and no per-process state, so two api pods agree.
//
// It is NOT a secret and is NOT an authentication token. Anybody who already
// holds a user id can compute the handle for it — which is fine, because holding
// the id is the harder half. The collision probability at 48 bits is
// irrelevant at this scale, and a collision would merge two rows on a display,
// not confuse two accounts anywhere it matters.
func publicHandle(id domain.UserID) string {
	sum := sha256.Sum256([]byte(id.String()))
	return "sl_" + hex.EncodeToString(sum[:6])
}
