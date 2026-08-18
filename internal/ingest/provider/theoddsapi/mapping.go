package theoddsapi

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// Semantics: the neutral shape becomes domain values.
//
// # Identifiers come from the normalizer, they are not derived here
//
// internal/ingest/normalizer/identity.go owns the derivation and says why it is
// exported: "odds.raw.{provider} is keyed by EventID […] so the raw producer and
// this package must agree on the derivation exactly. Two implementations of one
// identifier is the failure this export exists to prevent." odds.normalized is a
// COMPACTED topic keyed by market id — a second, subtly different derivation
// would not produce an error, it would produce two live keys for one market and
// freeze one of them for ever. So every identifier below is a call into that
// package.
//
// # Books disagree about the line, and the domain models one line per market
//
// This is the sharpest edge in the file and it is worth stating plainly.
// domain.ValidatePriceForSelection requires every price on a market to be quoted
// at that market's line (inverted for an away spread), and
// provider.Snapshot.Validate enforces it on every adapter's output. Real books
// do not oblige: The Odds API's own published /odds sample has eleven books at
// ±6.5 and one at ±6 on the same spread.
//
// Splitting the market by line is not available — normalizer.MarketIDFor
// deliberately excludes the line from the identifier, because a market whose
// line moves from -3.5 to -4 is the same market and folding the line in would
// shatter the line-movement series that CLV is computed from.
//
// So the market takes the MODAL line — the one the most books quote, which is
// the consensus main line — and a price at any other line is DROPPED and
// counted in sharpline_provider_mapping_dropped_total{reason="line_disagreement"}.
// Nothing is fabricated and nothing is silently reconciled: the discarded quote
// survives verbatim on odds.raw.the-odds-api, and the counter makes the loss a
// number on a dashboard rather than an absence nobody notices.
//
// This is a real limitation of the phase-1 domain model, not a preference. It is
// recorded here because the fix — an alternate-line market identity — is a
// domain change, not an adapter change.

// Drop reasons. A CLOSED set, because they are Prometheus label values: an
// unbounded reason label built from provider text is how a metric becomes a
// cardinality incident.
const (
	// DropReasonUnsupportedMarket is a market key this build does not map —
	// "alternate_spreads", "btts", a period market. Not an error: the provider
	// offers more than the charter's board shows.
	DropReasonUnsupportedMarket = "unsupported_market"

	// DropReasonNoObservationInstant is a quote carrying neither a market-level
	// nor a bookmaker-level last_update. wire.go is explicit that this must not
	// be papered over with time.Now(): stamping our clock onto a price we did
	// not observe makes the staleness SLO report perfect freshness for data of
	// unknown age.
	DropReasonNoObservationInstant = "no_observation_instant"

	// DropReasonUnmappedOutcome is an outcome whose name matches neither
	// competitor, nor Over/Under, nor Draw — so no domain.SelectionRole applies.
	DropReasonUnmappedOutcome = "unmapped_outcome"

	// DropReasonInvalidOdds is a price that is not representable as legal
	// decimal odds.
	DropReasonInvalidOdds = "invalid_odds"

	// DropReasonLineDisagreement is a quote at a line other than the market's
	// modal one. See the file comment.
	DropReasonLineDisagreement = "line_disagreement"

	// DropReasonMissingLine is a spread or total for which no book supplied a
	// point at all, so the market cannot be constructed.
	DropReasonMissingLine = "missing_line"

	// DropReasonDuplicateQuote is a second price for one (selection, book) pair
	// in a single payload.
	DropReasonDuplicateQuote = "duplicate_quote"

	// DropReasonInvalidEvent is an event the domain refuses — a match missing a
	// competitor, a zero commence_time, a name that will not validate.
	DropReasonInvalidEvent = "invalid_event"

	// DropReasonInvalidMarket is a market the domain refuses.
	DropReasonInvalidMarket = "invalid_market"

	// DropReasonInvalidSelection is a selection the domain refuses.
	DropReasonInvalidSelection = "invalid_selection"

	// DropReasonInvalidPrice is a price the domain refuses.
	DropReasonInvalidPrice = "invalid_price"

	// DropReasonWrongLeague is an event the provider returned under a league key
	// other than the one that was asked for.
	DropReasonWrongLeague = "wrong_league"
)

// DropReasons is every reason label this package can emit, so a dashboard or a
// test can enumerate the closed set.
func DropReasons() []string {
	return []string{
		DropReasonUnsupportedMarket,
		DropReasonNoObservationInstant,
		DropReasonUnmappedOutcome,
		DropReasonInvalidOdds,
		DropReasonLineDisagreement,
		DropReasonMissingLine,
		DropReasonDuplicateQuote,
		DropReasonInvalidEvent,
		DropReasonInvalidMarket,
		DropReasonInvalidSelection,
		DropReasonInvalidPrice,
		DropReasonWrongLeague,
	}
}

// Provider market keys this adapter maps onto domain market types.
const (
	marketKeyH2H       = "h2h"
	marketKeySpreads   = "spreads"
	marketKeyTotals    = "totals"
	marketKeyOutrights = "outrights"

	// marketKeyPlayerPrefix matches the player-prop family, which is large and
	// grows. normalizer/raw.go matches it by prefix for the same reason.
	marketKeyPlayerPrefix = "player_"
)

// Outcome names that carry a role rather than a competitor.
const (
	outcomeOver  = "over"
	outcomeUnder = "under"
	outcomeDraw  = "draw"
)

// marketTypeForKey maps a provider market key onto a domain market type.
//
// Unknown keys are NOT an error — the provider serves markets the charter's
// board does not show, and a sweep that asked for three markets can still come
// back carrying a fourth. They are skipped and counted.
func marketTypeForKey(key string) (domain.MarketType, bool) {
	switch key {
	case marketKeyH2H:
		return domain.MarketTypeMoneyline, true
	case marketKeySpreads:
		return domain.MarketTypeSpread, true
	case marketKeyTotals:
		return domain.MarketTypeTotal, true
	case marketKeyOutrights:
		return domain.MarketTypeFutures, true
	}
	if strings.HasPrefix(key, marketKeyPlayerPrefix) {
		return domain.MarketTypePlayerProp, true
	}
	return domain.MarketTypeUnknown, false
}

// featuredMarketKeyFor maps a domain market type onto the provider key the
// /odds endpoint accepts. Player props have no single key — they are a family,
// and which members to request is configuration — so they are absent here and
// handled by Config.PlayerPropMarkets.
func featuredMarketKeyFor(t domain.MarketType) (string, bool) {
	switch t {
	case domain.MarketTypeMoneyline:
		return marketKeyH2H, true
	case domain.MarketTypeSpread:
		return marketKeySpreads, true
	case domain.MarketTypeTotal:
		return marketKeyTotals, true
	case domain.MarketTypeFutures:
		return marketKeyOutrights, true
	default:
		return "", false
	}
}

// -----------------------------------------------------------------------------
// Book registry
// -----------------------------------------------------------------------------

// bookRegistry remembers the books the provider has actually quoted.
//
// It exists because The Odds API publishes no bookmaker endpoint: the book list
// is discovered from odds payloads, where the display title is the provider's
// own text. Synthesising a title from a key ("draftkings" -> "DraftKings")
// would be inventing display data, which the no-mock-data rule forbids, so a
// book appears in the catalogue only once the provider has named it.
//
// Entries are never removed. A book that stops quoting still has prices and
// wagers pointing at it (migrations/00002 declares those edges ON DELETE
// RESTRICT), so forgetting it would break the catalogue projection.
type bookRegistry struct {
	mu    sync.RWMutex
	byKey map[string]domain.Book
	order []string
}

func newBookRegistry() *bookRegistry {
	return &bookRegistry{byKey: make(map[string]domain.Book)}
}

// observe records a book the provider quoted and returns its domain value.
func (r *bookRegistry) observe(prov kafka.Provider, key, title string, reference bool) (domain.Book, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return domain.Book{}, fmt.Errorf("%w: bookmaker with no key", ErrMalformedResponse)
	}

	r.mu.RLock()
	existing, ok := r.byKey[key]
	r.mu.RUnlock()
	if ok {
		return existing, nil
	}

	id, err := normalizer.BookIDFor(prov, key)
	if err != nil {
		return domain.Book{}, fmt.Errorf("%w: book id for %q: %w", ErrMalformedResponse, key, err)
	}
	slug, err := normalizer.SlugFor("", key)
	if err != nil {
		return domain.Book{}, fmt.Errorf("%w: book slug for %q: %w", ErrMalformedResponse, key, err)
	}
	name := strings.TrimSpace(title)
	if name == "" {
		// The provider's own key, verbatim. Not a prettified guess.
		name = key
	}
	book, err := domain.NewBook(domain.BookParams{
		ID:        id,
		Slug:      slug,
		Name:      name,
		Kind:      domain.BookKindExternal,
		Reference: reference,
	})
	if err != nil {
		return domain.Book{}, fmt.Errorf("%w: book %q: %w", ErrMalformedResponse, key, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if again, dup := r.byKey[key]; dup {
		return again, nil
	}
	r.byKey[key] = book
	r.order = append(r.order, key)
	return book, nil
}

// books returns every observed book, in first-seen order so the catalogue is
// stable across calls.
func (r *bookRegistry) books() []domain.Book {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.Book, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, r.byKey[k])
	}
	return out
}

// -----------------------------------------------------------------------------
// The mapping
// -----------------------------------------------------------------------------

// mapper turns neutral raw events into domain snapshots.
//
// It holds no clock: every instant it writes comes from the provider's payload
// or from the FetchedAt the caller measured. Reading a clock here would be the
// staleness bug provider.go's package comment describes.
type mapper struct {
	prov kafka.Provider

	// oddsFormat is the format the payload's prices were requested in. It is
	// carried so the raw bytes published to the bus can declare it; see
	// decode.go for why the payload alone is not enough.
	oddsFormat OddsFormat

	// reference is the provider bookmaker key marked as the sharp reference
	// CLAUDE.md §6's +EV finder prices against.
	reference string

	metrics *Metrics
	books   *bookRegistry
}

// quote is one book's price for one outcome of one market, before the market's
// line has been decided.
type quote struct {
	bookKey    string
	bookTitle  string
	role       domain.SelectionRole
	name       string
	decimal    float64
	point      float64
	hasPoint   bool
	observedAt time.Time
}

// marketGroup collects one market across every book quoting it.
//
// The grouping key is (provider market key, subject) — the same pair
// normalizer.MarketIDFor takes — because one wire market such as
// "player_pass_tds" carries outcomes for several players, and each player is a
// separate market rather than a separate selection.
type marketGroup struct {
	key      string
	subject  string
	typ      domain.MarketType
	observed time.Time
	quotes   []quote
}

// mapEvent converts one raw event into an event snapshot.
//
// It returns ok=false when the event cannot be represented at all; every
// narrower loss is a counted drop and the rest of the event still maps. A
// provider blip on one market must not remove a contest from the board.
func (m *mapper) mapEvent(
	raw normalizer.RawEvent,
	leagueID domain.LeagueID,
	fetchedAt time.Time,
	body []byte,
) (provider.EventSnapshot, bool) {
	eventID, err := normalizer.EventIDFor(m.prov, raw.ID)
	if err != nil {
		m.metrics.observeDropped(DropReasonInvalidEvent, 1)
		return provider.EventSnapshot{}, false
	}

	event, err := m.buildEvent(raw, eventID, leagueID, fetchedAt)
	if err != nil {
		m.metrics.observeDropped(DropReasonInvalidEvent, 1)
		return provider.EventSnapshot{}, false
	}

	groups := m.collect(raw)
	markets := make([]provider.MarketSnapshot, 0, len(groups))
	newest := time.Time{}
	for _, g := range groups {
		snap, ok := m.buildMarket(eventID, g)
		if !ok {
			continue
		}
		markets = append(markets, snap)
		for _, p := range snap.Prices {
			if p.ObservedAt().After(newest) {
				newest = p.ObservedAt()
			}
		}
	}

	observed := newest
	if observed.IsZero() {
		observed = fetchedAt
	}

	out := provider.EventSnapshot{Event: event, Markets: markets}
	if len(body) > 0 {
		out.Raw = provider.RawPayload{
			// The format travels with the bytes; see decode.go.
			ContentType: RawContentType(m.oddsFormat),
			Body:        body,
			ObservedAt:  observed,
		}
	}
	return out, true
}

// buildEvent constructs the domain event.
func (m *mapper) buildEvent(
	raw normalizer.RawEvent,
	eventID domain.EventID,
	leagueID domain.LeagueID,
	fetchedAt time.Time,
) (domain.Event, error) {
	home := strings.TrimSpace(raw.HomeTeam)
	away := strings.TrimSpace(raw.AwayTeam)

	var (
		kind       domain.EventKind
		homeC      domain.Competitor
		awayC      domain.Competitor
		name       string
		err        error
		bothAbsent = home == "" && away == ""
	)
	switch {
	case home != "" && away != "":
		kind = domain.EventKindMatch
		// The provider supplies names and no competitor identifiers. domain
		// treats the identifier as optional for exactly this case — "refusing
		// the event over a missing surrogate key would drop real markets" — so
		// the zero CompetitorID is passed rather than a fabricated one.
		if homeC, err = domain.NewCompetitor("", home); err != nil {
			return domain.Event{}, err
		}
		if awayC, err = domain.NewCompetitor("", away); err != nil {
			return domain.Event{}, err
		}
		// normalizer.RawEvent documents this derivation: The Odds API publishes
		// no event name, and "Away at Home" is the form
		// domain.EventParams.Name calls typical.
		name = firstNonEmpty(strings.TrimSpace(raw.Name), away+" at "+home)
	case bothAbsent:
		kind = domain.EventKindOutright
		name = firstNonEmpty(strings.TrimSpace(raw.Name), strings.TrimSpace(raw.LeagueName), raw.LeagueKey)
	default:
		// One side without the other. The home-perspective line convention in
		// domain/market.go depends on both being known, so a half-populated
		// match is refused rather than guessed at.
		return domain.Event{}, fmt.Errorf("%w: event %s has one competitor", ErrMalformedResponse, raw.ID)
	}

	// The provider's own documented in-play test: "If commence_time is less
	// than the current time, the event is in-play." The clock is the fetch
	// instant, which is a measurement, not time.Now() taken later.
	status := domain.EventStatusScheduled
	if !raw.CommenceTime.IsZero() && raw.CommenceTime.Before(fetchedAt) {
		status = domain.EventStatusLive
	}

	return domain.NewEvent(domain.EventParams{
		ID:             eventID,
		LeagueID:       leagueID,
		Kind:           kind,
		Name:           name,
		Home:           homeC,
		Away:           awayC,
		ScheduledStart: raw.CommenceTime,
		Status:         status,
		// The provider stamps no event-level observation instant. FetchedAt is
		// the ingester's own observation time, which domain.EventParams.UpdatedAt
		// explicitly admits, and unlike a per-market timestamp it is monotonic
		// across polls — which is what the writer's monotonicity guard needs.
		UpdatedAt: fetchedAt,
	})
}

// collect groups every book's quotes by (market key, subject).
//
// Iteration order is the payload's, so the result is deterministic for a given
// payload: no map is ranged over here, and the group slice preserves first-seen
// order.
func (m *mapper) collect(raw normalizer.RawEvent) []*marketGroup {
	var (
		groups []*marketGroup
		index  = make(map[[2]string]*marketGroup)
	)

	home := strings.TrimSpace(raw.HomeTeam)
	away := strings.TrimSpace(raw.AwayTeam)

	for _, bk := range raw.Books {
		for _, mk := range bk.Markets {
			typ, ok := marketTypeForKey(mk.Key)
			if !ok {
				m.metrics.observeDropped(DropReasonUnsupportedMarket, len(mk.Outcomes))
				continue
			}

			// The provider's own recommendation: the market-level timestamp
			// wins, the bookmaker-level one is the fallback. The distinction is
			// not cosmetic — the bookmaker value is when that BOOK was last
			// polled, which can be materially older than when this market moved.
			observedAt := mk.LastUpdate
			if observedAt.IsZero() {
				observedAt = bk.LastUpdate
			}
			if observedAt.IsZero() {
				m.metrics.observeDropped(DropReasonNoObservationInstant, len(mk.Outcomes))
				continue
			}

			for _, o := range mk.Outcomes {
				subject := ""
				if typ == domain.MarketTypePlayerProp {
					subject = strings.TrimSpace(o.Description)
					if subject == "" {
						// domain.NewMarket requires a subject on a player prop,
						// and the description is the only thing that names the
						// player.
						m.metrics.observeDropped(DropReasonUnmappedOutcome, 1)
						continue
					}
				}

				role, ok := roleFor(typ, o.Name, home, away)
				if !ok {
					m.metrics.observeDropped(DropReasonUnmappedOutcome, 1)
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
					bookTitle:  bk.Name,
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
	return groups
}

// roleFor maps an outcome's label onto a domain selection role.
//
// Team-name matching is case- and whitespace-insensitive but otherwise exact.
// Fuzzy matching is deliberately absent: a near-match that picks the wrong side
// produces a plausible, silently inverted price, which is worse than a counted
// drop.
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
		// touchdown scorer" — is a set of runners. domain permits that role on a
		// player prop.
		return domain.SelectionRoleOutright, true

	case domain.MarketTypeFutures:
		return domain.SelectionRoleOutright, true

	default:
		return domain.SelectionRoleUnknown, false
	}
}

// buildMarket turns one group into a market snapshot.
func (m *mapper) buildMarket(eventID domain.EventID, g *marketGroup) (provider.MarketSnapshot, bool) {
	line, ok := m.marketLine(g)
	if !ok {
		return provider.MarketSnapshot{}, false
	}

	marketID, err := normalizer.MarketIDFor(eventID, g.key, g.subject)
	if err != nil {
		m.metrics.observeDropped(DropReasonInvalidMarket, len(g.quotes))
		return provider.MarketSnapshot{}, false
	}

	market, err := domain.NewMarket(domain.MarketParams{
		ID:      marketID,
		EventID: eventID,
		Type:    g.typ,
		Line:    line,
		Subject: g.subject,
		// Every market the provider is currently quoting accepts wagers. A
		// market it has stopped quoting simply stops appearing; closing it is
		// the pipeline's job, not the adapter's.
		Status:    domain.MarketStatusOpen,
		UpdatedAt: g.observed,
	})
	if err != nil {
		m.metrics.observeDropped(DropReasonInvalidMarket, len(g.quotes))
		return provider.MarketSnapshot{}, false
	}

	var (
		selections []domain.Selection
		byID       = make(map[domain.SelectionID]domain.Selection, len(g.quotes))
		prices     []domain.Price
		seen       = make(map[[2]string]bool, len(g.quotes))
	)

	for _, q := range g.quotes {
		selectionID, err := normalizer.SelectionIDFor(marketID, q.role, q.name)
		if err != nil {
			m.metrics.observeDropped(DropReasonInvalidSelection, 1)
			continue
		}
		sel, known := byID[selectionID]
		if !known {
			sel, err = domain.NewSelection(domain.SelectionParams{
				ID:       selectionID,
				MarketID: marketID,
				Role:     q.role,
				Name:     q.name,
			})
			if err != nil {
				m.metrics.observeDropped(DropReasonInvalidSelection, 1)
				continue
			}
			if err := domain.ValidateSelectionForMarket(market, sel); err != nil {
				m.metrics.observeDropped(DropReasonInvalidSelection, 1)
				continue
			}
			byID[selectionID] = sel
			selections = append(selections, sel)
		}

		// EffectiveLine applies the home-perspective convention: the away side
		// of a spread trades at the inverse of the market's line. Reading
		// Market.Line() directly here and forgetting to invert is the exact bug
		// that convention exists to prevent, and it yields a plausible wrong
		// number rather than an error.
		effective, err := domain.EffectiveLine(market, sel)
		if err != nil {
			m.metrics.observeDropped(DropReasonInvalidSelection, 1)
			continue
		}
		quoted := domain.NoLine()
		if q.hasPoint {
			quoted, err = domain.NewLine(q.point)
			if err != nil {
				m.metrics.observeDropped(DropReasonInvalidPrice, 1)
				continue
			}
		}
		if !quoted.Equal(effective) {
			// This book is on a different line. See the file comment.
			m.metrics.observeDropped(DropReasonLineDisagreement, 1)
			continue
		}

		book, err := m.books.observe(m.prov, q.bookKey, q.bookTitle, q.bookKey == m.reference)
		if err != nil {
			m.metrics.observeDropped(DropReasonInvalidPrice, 1)
			continue
		}

		key := [2]string{string(selectionID), string(book.ID())}
		if seen[key] {
			m.metrics.observeDropped(DropReasonDuplicateQuote, 1)
			continue
		}

		price, err := domain.NewPrice(domain.PriceParams{
			SelectionID: selectionID,
			BookID:      book.ID(),
			Decimal:     q.decimal,
			Line:        effective,
			// The PROVIDER's instant, never ours. migrations/00003 calls this
			// "the subtrahend in every staleness measurement".
			ObservedAt: q.observedAt,
		})
		if err != nil {
			m.metrics.observeDropped(DropReasonInvalidPrice, 1)
			continue
		}
		seen[key] = true
		prices = append(prices, price)
	}

	if len(selections) == 0 {
		return provider.MarketSnapshot{}, false
	}
	return provider.MarketSnapshot{Market: market, Selections: selections, Prices: prices}, true
}

// marketLine decides the one line the market trades at.
//
// The value returned is in the market's own (home) perspective, which is what
// domain.Market stores. For a spread that means a quote on the away side is
// negated before it votes, so the eleven books at away +6.5 and the eleven at
// home -6.5 are counted as agreeing rather than as two rival lines.
func (m *mapper) marketLine(g *marketGroup) (domain.Line, bool) {
	switch g.typ.LineRule() {
	case domain.LineRuleForbidden:
		return domain.NoLine(), true

	case domain.LineRuleRequired, domain.LineRuleOptional:
		counts := make(map[float64]int, len(g.quotes))
		for _, q := range g.quotes {
			if !q.hasPoint {
				continue
			}
			counts[homePerspective(g.typ, q.role, q.point)]++
		}
		if len(counts) == 0 {
			if g.typ.LineRule() == domain.LineRuleOptional {
				return domain.NoLine(), true
			}
			m.metrics.observeDropped(DropReasonMissingLine, len(g.quotes))
			return domain.Line{}, false
		}

		// Sorted keys, then a stable max: ranging a map directly would make the
		// chosen line depend on Go's randomised map order, which would produce a
		// DIFFERENT market line for the same payload on two consecutive polls
		// and read downstream as a line move that never happened.
		candidates := make([]float64, 0, len(counts))
		for v := range counts {
			candidates = append(candidates, v)
		}
		sort.Float64s(candidates)

		best, bestCount := candidates[0], counts[candidates[0]]
		for _, v := range candidates[1:] {
			if counts[v] > bestCount {
				best, bestCount = v, counts[v]
			}
		}
		line, err := domain.NewLine(best)
		if err != nil {
			m.metrics.observeDropped(DropReasonMissingLine, len(g.quotes))
			return domain.Line{}, false
		}
		return line, true

	default:
		m.metrics.observeDropped(DropReasonInvalidMarket, len(g.quotes))
		return domain.Line{}, false
	}
}

// homePerspective converts a quote's own point into the market's line.
func homePerspective(typ domain.MarketType, role domain.SelectionRole, point float64) float64 {
	if typ == domain.MarketTypeSpread && role == domain.SelectionRoleAway {
		if point == 0 {
			// Negating 0.0 yields -0.0, which compares equal but is a different
			// map key from 0.0 — so a pick'em would split its own vote.
			return 0
		}
		return -point
	}
	return point
}
