// Semantics: one provider-neutral RawEvent becomes validated domain values.
//
// # This is the ONLY mapping, and that is what makes provider parity structural
//
// raw.go states the rule: the per-provider code stops at syntax, and RawEvent is
// where the providers converge. Everything below that line — this file, the
// fingerprint, the suppression — is shared. So "two providers produce IDENTICAL
// domain values for equivalent input" is a property of the architecture rather
// than an agreement two implementations have to keep. parity_test.go pins it by
// running one The Odds API documentation sample and its neutral-format twin
// through this mapper and requiring byte-identical published records.
//
// # It is a PURE FUNCTION
//
// No clock, no I/O, no state, no map iteration order. Every time-dependent
// derivation — is the event live, what is a market's UpdatedAt — is computed
// from the payload's OWN observation instants. Replaying odds.raw.{provider} six
// months from now therefore reproduces exactly the records it produced the first
// time, which is the only thing that makes retaining the raw topic worth the
// disk.
//
// The one consequence worth naming: an event is "live" here when its advertised
// start is before the NEWEST observation instant in its own payload, not before
// time.Now(). Those differ by the provider's own staleness, which is the honest
// answer — this package did not observe the contest, the provider did.
//
// # Nothing is coerced
//
// CLAUDE.md's phase brief: a payload that cannot be normalised "must be REJECTED
// AND COUNTED with a reason, never coerced into something the domain happens to
// accept". Every failure below produces a Reject carrying a bounded Reason, and
// the narrowest scope that still describes the loss — one unmappable outcome
// costs one selection, not the market, and one unmappable market never costs the
// event.
package normalizer

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// Outcome labels that carry a role rather than a competitor name. Matched
// case-insensitively after trimming, and otherwise exactly.
const (
	outcomeOver  = "over"
	outcomeUnder = "under"
	outcomeDraw  = "draw"
)

// MapperOptions configures a Mapper.
type MapperOptions struct {
	// Provider is the adapter slug the payload came from. It seeds every
	// derived identifier, so it is validated against the identifier budget at
	// construction (identity.go) rather than at the first record.
	Provider kafka.Provider

	// SlugNamespace prefixes every derived slug.
	//
	// leagues.slug and books.slug are UNIQUE GLOBALLY in the schema, so two
	// providers using the same key for one real-world league derive the same
	// slug and different identifiers, and the second write violates the
	// constraint. SlugFor's doc has the full argument. It is EMPTY BY DEFAULT
	// because the overwhelmingly common case is one provider at a time, and
	// because the synthetic generator already prefixes its own league keys with
	// "syn-" while its book slugs ("sharpline") are names no real book uses.
	// Set it only when two adapters genuinely run side by side.
	SlugNamespace string
}

func (o MapperOptions) validate() error {
	if err := ValidateProviderForIdentity(o.Provider); err != nil {
		return fmt.Errorf("%w: provider: %w", ErrInvalidOptions, err)
	}
	if o.SlugNamespace != "" {
		if _, err := SlugFor(o.SlugNamespace, "probe"); err != nil {
			return fmt.Errorf("%w: slug namespace %q: %w", ErrInvalidOptions, o.SlugNamespace, err)
		}
	}
	return nil
}

// Mapper turns neutral raw events into domain-typed market views.
//
// It is immutable after construction and safe for concurrent use, which matters
// because the ONLY reason this is a type rather than a package-level function is
// that it carries the provider slug and the slug namespace.
type Mapper struct {
	prov     kafka.Provider
	ns       string
	bookKind domain.BookKind
}

// NewMapper builds a mapper for one provider.
func NewMapper(o MapperOptions) (*Mapper, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	return &Mapper{prov: o.Provider, ns: o.SlugNamespace, bookKind: bookKindFor(o.Provider)}, nil
}

// Provider returns the slug this mapper derives identifiers under.
func (m *Mapper) Provider() kafka.Provider { return m.prov }

// bookKindFor decides whether the books in a payload are real or simulated.
//
// It is derived from the provider rather than read from the payload because the
// neutral shape has no field for it and inventing one would let a provider
// declare its own quotes real. ADR 0003 makes the distinction non-negotiable:
// the synthetic fallback "must not silently substitute for real data in a
// running deployment — that would be indistinguishable from fabricating market
// data", and BookRef.Kind is how every consumer downstream can say so.
func bookKindFor(p kafka.Provider) domain.BookKind {
	if p.String() == provider.NameSynthetic.String() {
		return domain.BookKindSynthetic
	}
	return domain.BookKindExternal
}

// MapResult is everything one raw event produced.
type MapResult struct {
	// Views are the markets that mapped cleanly, in payload order.
	Views []MarketView

	// Rejects are the narrower losses, each with its scope and bounded reason.
	// A non-empty Rejects with a non-empty Views is the normal case, not a
	// failure: providers publish markets this build does not map.
	Rejects []Reject
}

// Map converts one raw event into one MarketView per market.
//
// It returns an error only when the EVENT itself cannot be represented, in which
// case nothing on it can be. The error is always a Reject, so a caller can
// errors.As it to get the scope and reason without parsing text.
func (m *Mapper) Map(raw RawEvent) (MapResult, error) {
	var out MapResult

	eventID, err := EventIDFor(m.prov, raw.ID)
	if err != nil {
		return out, reject(ScopeEvent, ReasonMissingEventID, raw.ID, err)
	}

	leagueKey := strings.TrimSpace(raw.LeagueKey)
	if leagueKey == "" {
		return out, reject(ScopeEvent, ReasonMissingLeague, raw.ID,
			fmt.Errorf("event %s carries no league key", sample(raw.ID)))
	}
	sport, league, err := m.catalogue(raw, leagueKey)
	if err != nil {
		return out, reject(ScopeEvent, ReasonInvalidIdentifier, leagueKey, err)
	}

	if raw.CommenceTime.IsZero() {
		return out, reject(ScopeEvent, ReasonMissingStartTime, raw.ID,
			fmt.Errorf("event %s carries no commence time", sample(raw.ID)))
	}

	kind, home, away, name, err := m.competitors(raw)
	if err != nil {
		return out, err
	}

	groups, rejects := m.collect(raw)
	out.Rejects = rejects

	// The event's status is decided ONCE, from the newest observation instant in
	// the whole payload, so that two markets on one contest cannot disagree about
	// whether it has started because one book's view lags across the kickoff.
	newest := time.Time{}
	for _, g := range groups {
		if g.observed.After(newest) {
			newest = g.observed
		}
	}
	if newest.IsZero() {
		// Every market was rejected for want of an observation instant, or there
		// were none at all. Either way there is nothing to publish and no honest
		// instant to stamp an event with.
		return out, reject(ScopeEvent, ReasonNoObservationTime, raw.ID, ErrNoObservationTime)
	}
	status := domain.EventStatusScheduled
	if raw.CommenceTime.Before(newest) {
		// The Odds API's own documented in-play test, applied to the provider's
		// clock rather than to ours.
		status = domain.EventStatusLive
	}

	// Validated once at the event level so a malformed contest fails as an event
	// rather than N times as N markets.
	if _, err := domain.NewEvent(domain.EventParams{
		ID: eventID, LeagueID: league.ID(), Kind: kind, Name: name,
		Home: home, Away: away, ScheduledStart: raw.CommenceTime,
		Status: status, UpdatedAt: newest,
	}); err != nil {
		return out, reject(ScopeEvent, ReasonInvalidEvent, raw.ID, err)
	}

	for _, g := range groups {
		// UpdatedAt is the MARKET's own observation instant, not the event's
		// newest. payload.go rebuilds the event from the market's instant on the
		// way back, so anything else would not round-trip.
		event, err := domain.NewEvent(domain.EventParams{
			ID: eventID, LeagueID: league.ID(), Kind: kind, Name: name,
			Home: home, Away: away, ScheduledStart: raw.CommenceTime,
			Status: status, UpdatedAt: g.observed,
		})
		if err != nil {
			out.Rejects = append(out.Rejects, reject(ScopeMarket, ReasonInvalidEvent, g.key, err))
			continue
		}
		view, rejects, ok := m.buildMarket(eventID, g)
		out.Rejects = append(out.Rejects, rejects...)
		if !ok {
			continue
		}
		view.Sport = sport
		view.League = league
		view.Event = event
		out.Views = append(out.Views, view)
	}
	return out, nil
}

// catalogue derives the sport and league this event belongs to.
func (m *Mapper) catalogue(raw RawEvent, leagueKey string) (domain.Sport, domain.League, error) {
	sportKey := strings.TrimSpace(raw.SportKey)
	if sportKey == "" {
		// The provider's own grouping, taken from its league key's prefix. See
		// SportKeyFromLeagueKey — this is a fallback, not a guess.
		sportKey = SportKeyFromLeagueKey(leagueKey)
	}

	sportID, err := SportIDFor(m.prov, sportKey)
	if err != nil {
		return domain.Sport{}, domain.League{}, fmt.Errorf("sport id for %q: %w", sample(sportKey), err)
	}
	sportSlug, err := SlugFor(m.ns, sportKey)
	if err != nil {
		return domain.Sport{}, domain.League{}, fmt.Errorf("sport slug for %q: %w", sample(sportKey), err)
	}
	sport, err := domain.NewSport(domain.SportParams{
		ID: sportID, Slug: sportSlug,
		// The provider's own text, or its own key. Never a prettified guess:
		// synthesising a display name is inventing data the provider did not
		// supply, which the no-mock-data rule forbids.
		Name: firstNonEmpty(strings.TrimSpace(raw.SportName), sportKey),
	})
	if err != nil {
		return domain.Sport{}, domain.League{}, err
	}

	leagueID, err := LeagueIDFor(m.prov, leagueKey)
	if err != nil {
		return domain.Sport{}, domain.League{}, fmt.Errorf("league id for %q: %w", sample(leagueKey), err)
	}
	leagueSlug, err := SlugFor(m.ns, leagueKey)
	if err != nil {
		return domain.Sport{}, domain.League{}, fmt.Errorf("league slug for %q: %w", sample(leagueKey), err)
	}
	league, err := domain.NewLeague(domain.LeagueParams{
		ID: leagueID, SportID: sportID, Slug: leagueSlug,
		Name: firstNonEmpty(strings.TrimSpace(raw.LeagueName), leagueKey),
	})
	if err != nil {
		return domain.Sport{}, domain.League{}, err
	}
	return sport, league, nil
}

// competitors resolves the event's kind, sides and display name.
func (m *Mapper) competitors(raw RawEvent) (domain.EventKind, domain.Competitor, domain.Competitor, string, error) {
	home := strings.TrimSpace(raw.HomeTeam)
	away := strings.TrimSpace(raw.AwayTeam)

	switch {
	case home != "" && away != "":
		// The provider supplies names and no competitor identifiers. The domain
		// treats the identifier as optional for exactly this case, so the zero
		// CompetitorID is passed rather than a fabricated one.
		h, err := domain.NewCompetitor("", home)
		if err != nil {
			return 0, domain.Competitor{}, domain.Competitor{}, "",
				reject(ScopeEvent, ReasonMissingCompetitor, raw.ID, err)
		}
		a, err := domain.NewCompetitor("", away)
		if err != nil {
			return 0, domain.Competitor{}, domain.Competitor{}, "",
				reject(ScopeEvent, ReasonMissingCompetitor, raw.ID, err)
		}
		// RawEvent documents the derivation: The Odds API publishes no event
		// name, and "Away at Home" is the form domain.EventParams.Name calls
		// typical.
		return domain.EventKindMatch, h, a,
			firstNonEmpty(strings.TrimSpace(raw.Name), away+" at "+home), nil

	case home == "" && away == "":
		return domain.EventKindOutright, domain.Competitor{}, domain.Competitor{},
			firstNonEmpty(strings.TrimSpace(raw.Name), strings.TrimSpace(raw.LeagueName), raw.LeagueKey), nil

	default:
		// One side without the other. The home-perspective line convention has no
		// meaning without both, so a half-populated match is refused rather than
		// guessed at.
		return 0, domain.Competitor{}, domain.Competitor{}, "",
			reject(ScopeEvent, ReasonMissingCompetitor, raw.ID,
				fmt.Errorf("event %s carries one competitor", sample(raw.ID)))
	}
}

// quote is one book's price on one outcome, before the market's line is decided.
type quote struct {
	bookKey    string
	bookName   string
	role       domain.SelectionRole
	name       string
	decimal    float64
	point      float64
	hasPoint   bool
	observedAt time.Time
}

// marketGroup collects one market across every book quoting it.
//
// The grouping key is (provider market key, subject) — the same pair MarketIDFor
// takes — because one wire market such as "player_pass_tds" carries outcomes for
// several players, and each player is a separate MARKET rather than a separate
// selection.
type marketGroup struct {
	key      string
	subject  string
	typ      domain.MarketType
	observed time.Time
	quotes   []quote
}

// collect groups every book's outcomes by (market key, subject).
//
// Iteration order is the payload's: no map is ranged over, and the group slice
// keeps first-seen order, so the result is a deterministic function of the bytes.
func (m *Mapper) collect(raw RawEvent) ([]*marketGroup, []Reject) {
	var (
		groups  []*marketGroup
		rejects []Reject
		index   = make(map[[2]string]*marketGroup)
	)

	home := strings.TrimSpace(raw.HomeTeam)
	away := strings.TrimSpace(raw.AwayTeam)

	for _, bk := range raw.Books {
		for _, mk := range bk.Markets {
			typ, ok := marketTypeForKey(mk.Key)
			if !ok {
				// Expected and frequent: the provider serves hundreds of market
				// keys and this board shows five types. Counted, never logged per
				// occurrence.
				rejects = append(rejects, reject(ScopeMarket, ReasonUnsupportedMarket, mk.Key, nil))
				continue
			}

			// The provider's own recommendation: the market-level instant wins and
			// the bookmaker-level one is the fallback. Not cosmetic — the
			// bookmaker value is when that BOOK was last polled, which can be
			// materially older than when this market moved.
			observedAt := mk.LastUpdate
			if observedAt.IsZero() {
				observedAt = bk.LastUpdate
			}
			if observedAt.IsZero() {
				rejects = append(rejects, reject(ScopeMarket, ReasonNoObservationTime, mk.Key, ErrNoObservationTime))
				continue
			}

			for _, o := range mk.Outcomes {
				subject := ""
				if typ == domain.MarketTypePlayerProp {
					subject = strings.TrimSpace(o.Description)
					if subject == "" {
						// domain.NewMarket requires a subject on a player prop and
						// the description is the only thing naming the individual.
						rejects = append(rejects, reject(ScopeSelection, ReasonMissingSubject, o.Name, nil))
						continue
					}
				}

				role, ok := roleFor(typ, o.Name, home, away)
				if !ok {
					rejects = append(rejects, reject(ScopeSelection, ReasonUnknownRole, o.Name, nil))
					continue
				}

				k := [2]string{mk.Key, subject}
				g := index[k]
				if g == nil {
					g = &marketGroup{key: mk.Key, subject: subject, typ: typ}
					index[k] = g
					groups = append(groups, g)
				}
				if observedAt.After(g.observed) {
					g.observed = observedAt
				}
				q := quote{
					bookKey:    bk.Key,
					bookName:   bk.Name,
					role:       role,
					name:       strings.TrimSpace(o.Name),
					decimal:    o.Price,
					observedAt: observedAt,
				}
				if o.Point != nil {
					q.point = *o.Point
					q.hasPoint = true
				}
				g.quotes = append(g.quotes, q)
			}
		}
	}
	return groups, rejects
}

// buildMarket turns one group into a market view.
func (m *Mapper) buildMarket(eventID domain.EventID, g *marketGroup) (MarketView, []Reject, bool) {
	var (
		view    MarketView
		rejects []Reject
	)

	line, lineRejects, ok := m.marketLine(g)
	rejects = append(rejects, lineRejects...)
	if !ok {
		return view, rejects, false
	}

	marketID, err := MarketIDFor(eventID, g.key, g.subject)
	if err != nil {
		return view, append(rejects, reject(ScopeMarket, ReasonInvalidIdentifier, g.key, err)), false
	}
	market, err := domain.NewMarket(domain.MarketParams{
		ID: marketID, EventID: eventID, Type: g.typ, Line: line, Subject: g.subject,
		// Every market the provider is currently quoting accepts wagers. A market
		// it has stopped quoting simply stops appearing in the payload; see
		// doc.go for why this package does not infer a suspension from that.
		Status: domain.MarketStatusOpen, UpdatedAt: g.observed,
	})
	if err != nil {
		return view, append(rejects, reject(ScopeMarket, ReasonInvalidMarket, g.key, err)), false
	}

	var (
		selections = make(map[domain.SelectionID]domain.Selection, len(g.quotes))
		books      = make(map[domain.BookID]domain.Book, len(g.quotes))
		seen       = make(map[[2]string]bool, len(g.quotes))
		prices     []domain.Price
	)

	for _, q := range g.quotes {
		selectionID, err := SelectionIDFor(marketID, q.role, q.name)
		if err != nil {
			rejects = append(rejects, reject(ScopeSelection, ReasonInvalidIdentifier, q.name, err))
			continue
		}
		// Several books quote one selection, so it is built once and reused. The
		// value is only ever read back out of the map at the end (sortedSelections),
		// which is why nothing here holds on to it.
		if _, known := selections[selectionID]; !known {
			sel, err := domain.NewSelection(domain.SelectionParams{
				ID: selectionID, MarketID: marketID, Role: q.role, Name: q.name,
			})
			if err != nil {
				rejects = append(rejects, reject(ScopeSelection, ReasonInvalidSelection, q.name, err))
				continue
			}
			if err := domain.ValidateSelectionForMarket(market, sel); err != nil {
				rejects = append(rejects, reject(ScopeSelection, ReasonInvalidSelection, q.name, err))
				continue
			}
			selections[selectionID] = sel
		}

		book, err := m.book(q)
		if err != nil {
			rejects = append(rejects, reject(ScopePrice, ReasonInvalidBook, q.bookKey, err))
			continue
		}

		key := [2]string{string(selectionID), string(book.ID())}
		if seen[key] {
			// The same book quoted the same selection twice in one payload. The
			// first is kept: without an ordering rule the surviving quote would
			// depend on payload order, which is not a contract.
			rejects = append(rejects, reject(ScopePrice, ReasonDuplicateSelection, q.name, nil))
			continue
		}

		quoted, ok, lineReject := m.quotedLine(market, q)
		if !ok {
			rejects = append(rejects, lineReject)
			continue
		}

		price, err := domain.NewPrice(domain.PriceParams{
			SelectionID: selectionID, BookID: book.ID(), Decimal: q.decimal, Line: quoted,
			// The PROVIDER's instant, never ours. migrations/00003 calls this
			// "the subtrahend in every staleness measurement".
			ObservedAt: q.observedAt,
		})
		if err != nil {
			rejects = append(rejects, reject(ScopePrice, priceRejectReason(err), q.name, err))
			continue
		}

		seen[key] = true
		books[book.ID()] = book
		prices = append(prices, price)
	}

	if len(prices) == 0 {
		// Publishing would put an empty market on the board.
		return view, append(rejects, reject(ScopeMarket, ReasonNoPrices, g.key, nil)), false
	}

	view.Market = market
	view.ProviderKey = g.key
	view.Books = sortedBooks(books, prices)
	view.Selections = sortedSelections(selections, prices)
	view.Prices = prices
	slices.SortFunc(view.Prices, func(a, b domain.Price) int {
		if c := cmpString(string(a.SelectionID()), string(b.SelectionID())); c != 0 {
			return c
		}
		return cmpString(string(a.BookID()), string(b.BookID()))
	})
	return view, rejects, true
}

// quotedLine returns the line THIS BOOK quoted, from the selection's own
// perspective.
//
// It is deliberately NOT the market's effective line. payload.go states the
// reason: the market line is a CONSENSUS across books and "a book quoting -3
// against a -3.5 consensus is normal market disagreement rather than
// corruption", which is exactly what multi-book comparison exists to show. The
// adapter-side mapper in the-odds-api DROPS those quotes because
// provider.Snapshot.Validate enforces one line per market; nothing enforces that
// here, and nothing should — dropping them would delete the disagreement the
// board is meant to display.
//
// raw.go fixes the perspective: RawOutcome.Point is already stated from the
// outcome's own side (+6.5 on one, -6.5 on the other), so no inversion is
// applied anywhere.
func (m *Mapper) quotedLine(market domain.Market, q quote) (domain.Line, bool, Reject) {
	if market.Type().LineRule() == domain.LineRuleForbidden {
		// A moneyline or futures quote carrying a point is the provider being
		// generous, not a line. domain.NewPrice would accept it and
		// ValidatePriceForSelection would later refuse the pair.
		return domain.NoLine(), true, Reject{}
	}
	if !q.hasPoint {
		if market.Type().LineRule() == domain.LineRuleOptional {
			return domain.NoLine(), true, Reject{}
		}
		return domain.Line{}, false,
			reject(ScopePrice, ReasonMissingLine, q.name,
				fmt.Errorf("%s quote on a %s market carries no point", q.bookKey, market.Type()))
	}
	line, err := domain.NewLine(q.point)
	if err != nil {
		return domain.Line{}, false, reject(ScopePrice, ReasonInvalidLine, q.name, err)
	}
	return line, true, Reject{}
}

// priceRejectReason classifies a domain.NewPrice failure.
//
// The odds themselves are the overwhelmingly common cause and they get their own
// reason, because "the provider sent 1.00" and "the price was malformed in some
// other way" are different operational problems: the first is a provider data
// quality signal, the second is a bug here. Everything else falls through to
// ReasonInvalidPrice rather than being reported as bad odds, so the counter that
// says "the provider is publishing unusable prices" never absorbs our mistakes.
//
// The classification is on SENTINELS, never on error text — the same rule Reason
// itself exists for.
func priceRejectReason(err error) Reason {
	if errors.Is(err, domain.ErrOddsNotFinite) || errors.Is(err, domain.ErrOddsOutOfRange) {
		return ReasonInvalidOdds
	}
	return ReasonInvalidPrice
}

// book derives the domain book one quote came from.
func (m *Mapper) book(q quote) (domain.Book, error) {
	key := strings.TrimSpace(q.bookKey)
	if key == "" {
		return domain.Book{}, fmt.Errorf("bookmaker with no key: %w", domain.ErrEmptyID)
	}
	id, err := BookIDFor(m.prov, key)
	if err != nil {
		return domain.Book{}, err
	}
	slug, err := SlugFor(m.ns, key)
	if err != nil {
		return domain.Book{}, err
	}
	return domain.NewBook(domain.BookParams{
		ID: id, Slug: slug,
		// The provider's own title, or its own key verbatim. Prettifying
		// "draftkings" into "DraftKings" would be inventing display data.
		Name: firstNonEmpty(strings.TrimSpace(q.bookName), key),
		Kind: m.bookKind,
	})
}

// marketLine decides the one line the market trades at.
//
// The value is in the market's own (home) perspective, which is what
// domain.Market stores, so an away quote at +6.5 votes with a home quote at -6.5
// rather than against it. The MODAL line wins: it is the consensus main line,
// and the alternative — splitting the market by line — is unavailable because
// MarketIDFor deliberately excludes the line from the identifier.
func (m *Mapper) marketLine(g *marketGroup) (domain.Line, []Reject, bool) {
	switch g.typ.LineRule() {
	case domain.LineRuleForbidden:
		return domain.NoLine(), nil, true

	case domain.LineRuleRequired, domain.LineRuleOptional:
		counts := make(map[float64]int, len(g.quotes))
		for _, q := range g.quotes {
			if !q.hasPoint || math.IsNaN(q.point) || math.IsInf(q.point, 0) {
				continue
			}
			counts[homePerspective(g.typ, q.role, q.point)]++
		}
		if len(counts) == 0 {
			if g.typ.LineRule() == domain.LineRuleOptional {
				return domain.NoLine(), nil, true
			}
			return domain.Line{}, []Reject{reject(ScopeMarket, ReasonMissingLine, g.key, nil)}, false
		}

		// Sorted keys, then a stable max. Ranging the map directly would make the
		// chosen line depend on Go's randomised map order, which would produce a
		// DIFFERENT market line for the same payload on two consecutive polls and
		// read downstream as a line move that never happened — and, worse, would
		// flip the fingerprint on every poll and defeat suppression entirely.
		candidates := make([]float64, 0, len(counts))
		for v := range counts {
			candidates = append(candidates, v)
		}
		slices.Sort(candidates)

		best, bestCount := candidates[0], counts[candidates[0]]
		for _, v := range candidates[1:] {
			if counts[v] > bestCount {
				best, bestCount = v, counts[v]
			}
		}
		line, err := domain.NewLine(best)
		if err != nil {
			return domain.Line{}, []Reject{reject(ScopeMarket, ReasonInvalidLine, g.key, err)}, false
		}
		return line, nil, true

	default:
		return domain.Line{}, []Reject{reject(ScopeMarket, ReasonInvalidMarket, g.key, nil)}, false
	}
}

// homePerspective converts a quote's own point into the market's line.
func homePerspective(typ domain.MarketType, role domain.SelectionRole, point float64) float64 {
	if typ == domain.MarketTypeSpread && role == domain.SelectionRoleAway {
		if point == 0 {
			// Negating 0.0 yields -0.0, which compares equal but is a different
			// map key, so a pick'em would split its own vote.
			return 0
		}
		return -point
	}
	return point
}

// marketTypeForKey maps a provider market key onto a domain market type.
//
// An unknown key is NOT an error: the provider serves markets this board does
// not show, and a sweep that asked for three can still come back carrying a
// fourth.
func marketTypeForKey(key string) (domain.MarketType, bool) {
	switch key {
	case MarketKeyH2H:
		return domain.MarketTypeMoneyline, true
	case MarketKeySpreads:
		return domain.MarketTypeSpread, true
	case MarketKeyTotals:
		return domain.MarketTypeTotal, true
	case MarketKeyOutrights:
		return domain.MarketTypeFutures, true
	}
	if strings.HasPrefix(key, MarketKeyPlayerPrefix) {
		return domain.MarketTypePlayerProp, true
	}
	return domain.MarketTypeUnknown, false
}

// roleFor maps an outcome's label onto a domain selection role.
//
// Team-name matching is case- and whitespace-insensitive but otherwise EXACT.
// Fuzzy matching is deliberately absent: a near-match that picks the wrong side
// produces a plausible, silently inverted price, which is far worse than a
// counted rejection.
func roleFor(typ domain.MarketType, name, home, away string) (domain.SelectionRole, bool) {
	label := strings.ToLower(strings.TrimSpace(name))
	if label == "" {
		return domain.SelectionRoleUnknown, false
	}

	switch typ {
	case domain.MarketTypeMoneyline, domain.MarketTypeSpread:
		switch {
		case home != "" && label == strings.ToLower(home):
			return domain.SelectionRoleHome, true
		case away != "" && label == strings.ToLower(away):
			return domain.SelectionRoleAway, true
		case label == outcomeDraw && typ == domain.MarketTypeMoneyline:
			// A spread has no draw: the handicap is quoted in half points
			// precisely to eliminate the tie.
			return domain.SelectionRoleDraw, true
		}
		return domain.SelectionRoleUnknown, false

	case domain.MarketTypeTotal:
		switch label {
		case outcomeOver:
			return domain.SelectionRoleOver, true
		case outcomeUnder:
			return domain.SelectionRoleUnder, true
		}
		return domain.SelectionRoleUnknown, false

	case domain.MarketTypePlayerProp:
		switch label {
		case outcomeOver:
			return domain.SelectionRoleOver, true
		case outcomeUnder:
			return domain.SelectionRoleUnder, true
		}
		// A prop whose outcomes are named rather than over/under — "first
		// touchdown scorer" — is a set of runners, and the domain permits that
		// role on a player prop.
		return domain.SelectionRoleOutright, true

	case domain.MarketTypeFutures:
		return domain.SelectionRoleOutright, true

	default:
		return domain.SelectionRoleUnknown, false
	}
}

// sortedBooks returns the books a market's surviving prices actually reference,
// ordered by identifier.
//
// Filtering by the price set rather than emitting every book seen is what keeps
// the record self-describing: payload.go's contract is that a consumer can
// render the market from this record alone, and a BookRef with no PriceRef
// pointing at it would show a book on the board quoting nothing.
func sortedBooks(books map[domain.BookID]domain.Book, prices []domain.Price) []domain.Book {
	out := make([]domain.Book, 0, len(books))
	for _, p := range prices {
		b, ok := books[p.BookID()]
		if !ok {
			continue
		}
		delete(books, p.BookID())
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b domain.Book) int { return cmpString(string(a.ID()), string(b.ID())) })
	return out
}

// sortedSelections returns the selections a market's surviving prices reference,
// ordered by identifier. Same argument as sortedBooks.
func sortedSelections(sels map[domain.SelectionID]domain.Selection, prices []domain.Price) []domain.Selection {
	out := make([]domain.Selection, 0, len(sels))
	for _, p := range prices {
		s, ok := sels[p.SelectionID()]
		if !ok {
			continue
		}
		delete(sels, p.SelectionID())
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b domain.Selection) int { return cmpString(string(a.ID()), string(b.ID())) })
	return out
}

// firstNonEmpty returns the first argument that is not empty after trimming.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
