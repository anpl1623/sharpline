package synthetic

import (
	"fmt"
	"strings"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Turning the model's probabilities into markets, selections and prices.
//
// Every value produced here goes through a domain constructor, and any error
// from one aborts the whole fetch. That is the correct severity: the domain's
// validators encode the rules of the product, so a generator that produces
// something they refuse has not hit bad luck, it has a bug — and a bug that
// silently dropped one market would show up as a hole in the board that nobody
// could reproduce.

// Provider market keys.
//
// These are The Odds API's vocabulary, not an invented one, because
// normalizer/raw.go fixes it as the neutral shape's vocabulary: the market key
// participates in the derived market identifier, "so it has to be a published,
// stable string rather than something a generator picks". The synthetic feed
// speaking the same keys is what lets one mapper serve both providers.
const (
	rawKeyMoneyline = normalizer.MarketKeyH2H
	rawKeySpread    = normalizer.MarketKeySpreads
	rawKeyTotal     = normalizer.MarketKeyTotals
	rawKeyOutright  = normalizer.MarketKeyOutrights
)

// marketPlan is one market before it is priced: its identity, its selections,
// and each book's fair probability for each of them.
type marketPlan struct {
	id      domain.MarketID
	typ     domain.MarketType
	line    domain.Line
	subject string
	rawKey  string

	roles  []domain.SelectionRole
	names  []string
	selIDs []domain.SelectionID

	// probs[book][selection] is the fair probability book `book` believes.
	// nil when the market is not being priced (closed or suspended).
	probs [][]float64

	status domain.MarketStatus
}

// planFor builds every market in scope for one event.
func (a *Adapter) planFor(es *eventState, scope provider.Scope) ([]marketPlan, error) {
	if es.ev.isFutures() {
		return a.futuresPlans(es, scope)
	}
	return a.matchPlans(es, scope)
}

// matchPlans builds the moneyline, spread, total and player-prop markets.
func (a *Adapter) matchPlans(es *eventState, scope provider.Scope) ([]marketPlan, error) {
	l := es.ev.league
	nBooks := len(a.books)
	plans := make([]marketPlan, 0, 3+len(es.ev.props))

	if scope.HasMarket(domain.MarketTypeMoneyline) {
		roles := []domain.SelectionRole{domain.SelectionRoleHome, domain.SelectionRoleAway}
		names := []string{es.ev.home.Name(), es.ev.away.Name()}
		if l.threeWay {
			roles = []domain.SelectionRole{domain.SelectionRoleHome, domain.SelectionRoleDraw, domain.SelectionRoleAway}
			names = []string{es.ev.home.Name(), drawSelectionName, es.ev.away.Name()}
		}
		p := marketPlan{
			id:     marketID(es.ev.id, marketSuffixMoneyline),
			typ:    domain.MarketTypeMoneyline,
			line:   domain.NoLine(),
			rawKey: rawKeyMoneyline,
			roles:  roles,
			names:  names,
			status: a.marketStatus(es, es.margin),
		}
		if p.status == domain.MarketStatusOpen {
			p.probs = make([][]float64, nBooks)
			for i := range a.books {
				mean, sd := a.marginView(es, i)
				probs, err := moneylineProbs(mean, sd, l.threeWay)
				if err != nil {
					return nil, fmt.Errorf("moneyline %s: %w", p.id, err)
				}
				p.probs[i] = probs
			}
		}
		plans = append(plans, p)
	}

	if scope.HasMarket(domain.MarketTypeSpread) {
		mean, _ := a.trueMargin(es)
		line, err := domain.NewLine(-roundHalf(mean))
		if err != nil {
			return nil, fmt.Errorf("spread line for %s: %w", es.ev.id, err)
		}
		p := marketPlan{
			id:     marketID(es.ev.id, marketSuffixSpread),
			typ:    domain.MarketTypeSpread,
			line:   line,
			rawKey: rawKeySpread,
			roles:  []domain.SelectionRole{domain.SelectionRoleHome, domain.SelectionRoleAway},
			names:  []string{es.ev.home.Name(), es.ev.away.Name()},
			status: a.marketStatus(es, es.margin),
		}
		if p.status == domain.MarketStatusOpen {
			handicap, _ := line.Value()
			p.probs = make([][]float64, nBooks)
			for i := range a.books {
				m, sd := a.marginView(es, i)
				probs, err := spreadProbs(m, sd, handicap)
				if err != nil {
					return nil, fmt.Errorf("spread %s: %w", p.id, err)
				}
				p.probs[i] = probs
			}
		}
		plans = append(plans, p)
	}

	if scope.HasMarket(domain.MarketTypeTotal) {
		mean, _ := a.trueTotal(es)
		line, err := domain.NewLine(thresholdLine(mean))
		if err != nil {
			return nil, fmt.Errorf("total line for %s: %w", es.ev.id, err)
		}
		p := marketPlan{
			id:     marketID(es.ev.id, marketSuffixTotal),
			typ:    domain.MarketTypeTotal,
			line:   line,
			rawKey: rawKeyTotal,
			roles:  []domain.SelectionRole{domain.SelectionRoleOver, domain.SelectionRoleUnder},
			names:  []string{overSelectionName, underSelectionName},
			status: a.marketStatus(es, es.total),
		}
		if p.status == domain.MarketStatusOpen {
			threshold, _ := line.Value()
			p.probs = make([][]float64, nBooks)
			for i := range a.books {
				m, sd := a.totalView(es, i)
				probs, err := thresholdProbs(m, sd, threshold)
				if err != nil {
					return nil, fmt.Errorf("total %s: %w", p.id, err)
				}
				p.probs[i] = probs
			}
		}
		plans = append(plans, p)
	}

	if scope.HasMarket(domain.MarketTypePlayerProp) {
		for idx, prop := range es.ev.props {
			mean, _ := a.trueProp(es, prop, idx)
			line, err := domain.NewLine(thresholdLine(mean))
			if err != nil {
				return nil, fmt.Errorf("prop line for %s: %w", es.ev.id, err)
			}
			p := marketPlan{
				id:      propMarketID(es.ev.id, prop.idx),
				typ:     domain.MarketTypePlayerProp,
				line:    line,
				subject: prop.subject,
				rawKey:  playerMarketKey(l.propStat),
				roles:   []domain.SelectionRole{domain.SelectionRoleOver, domain.SelectionRoleUnder},
				names:   []string{overSelectionName, underSelectionName},
				status:  a.marketStatus(es, es.props[idx]),
			}
			if p.status == domain.MarketStatusOpen {
				threshold, _ := line.Value()
				p.probs = make([][]float64, nBooks)
				for i := range a.books {
					m, sd := a.propView(es, prop, idx, i)
					probs, err := thresholdProbs(m, sd, threshold)
					if err != nil {
						return nil, fmt.Errorf("prop %s: %w", p.id, err)
					}
					p.probs[i] = probs
				}
			}
			plans = append(plans, p)
		}
	}

	for i := range plans {
		if err := fillSelectionIDs(&plans[i]); err != nil {
			return nil, err
		}
	}
	return plans, nil
}

// futuresPlans builds the league's season-title outright market.
func (a *Adapter) futuresPlans(es *eventState, scope provider.Scope) ([]marketPlan, error) {
	if !scope.HasMarket(domain.MarketTypeFutures) {
		return nil, nil
	}
	p := marketPlan{
		id:     marketID(es.ev.id, marketSuffixFutures),
		typ:    domain.MarketTypeFutures,
		line:   domain.NoLine(),
		rawKey: rawKeyOutright,
		roles:  make([]domain.SelectionRole, len(es.ev.runners)),
		names:  append([]string(nil), es.ev.runners...),
		status: domain.MarketStatusOpen,
	}
	for i := range p.roles {
		p.roles[i] = domain.SelectionRoleOutright
	}
	p.selIDs = make([]domain.SelectionID, len(es.ev.runners))
	for i := range p.selIDs {
		p.selIDs[i] = runnerSelectionID(p.id, i)
	}

	strength := make([]float64, len(es.ev.runners))
	p.probs = make([][]float64, len(a.books))
	for b := range a.books {
		for r := range es.ev.runners {
			strength[r] = a.runnerStrength(es, r, b)
		}
		p.probs[b] = outrightProbs(strength)
	}
	return []marketPlan{p}, nil
}

// marketStatus decides a market's lifecycle from the event and the process that
// drives it.
//
// A finished contest closes its markets rather than dropping them: a closed
// market on a finished event "still belongs in the snapshot so the normalizer
// can close it" (provider.MarketSnapshot). A market whose latent process just
// jumped is SUSPENDED for suspendSteps, which is what a book does while it
// reprices — and pulling the price rather than showing a stale one is the same
// principle ADR 0003 states for quota exhaustion.
func (a *Adapter) marketStatus(es *eventState, p path) domain.MarketStatus {
	if es.status == domain.EventStatusEnded {
		return domain.MarketStatusClosed
	}
	if p.steamed(es.n, suspendSteps) {
		return domain.MarketStatusSuspended
	}
	return domain.MarketStatusOpen
}

// fillSelectionIDs derives the identifier of every selection from its role.
// Outright fields fill theirs at construction, because a role does not
// distinguish one runner from another.
func fillSelectionIDs(p *marketPlan) error {
	if p.selIDs != nil {
		return nil
	}
	if len(p.roles) != len(p.names) {
		return fmt.Errorf("synthetic: %w: market %s has %d roles and %d names",
			provider.ErrInvalidSnapshot, p.id, len(p.roles), len(p.names))
	}
	p.selIDs = make([]domain.SelectionID, len(p.roles))
	for i, r := range p.roles {
		p.selIDs[i] = selectionID(p.id, r.String())
	}
	return nil
}

// -----------------------------------------------------------------------------
// Assembly
// -----------------------------------------------------------------------------

// assemble turns a plan into the domain values one market contributes to the
// snapshot, and the per-book raw markets it contributes to the payload.
//
// The two are produced together, from one set of numbers, because the raw
// payload is the replayable record of what this "provider" said: if the parsed
// form and the bytes were computed separately they could disagree, and the raw
// topic would stop being evidence of anything.
func (a *Adapter) assemble(es *eventState, p marketPlan, sc *scratch) (provider.MarketSnapshot, []normalizer.RawMarket, error) {
	market, err := domain.NewMarket(domain.MarketParams{
		ID:        p.id,
		EventID:   es.ev.id,
		Type:      p.typ,
		Line:      p.line,
		Subject:   p.subject,
		Status:    p.status,
		UpdatedAt: es.at,
	})
	if err != nil {
		return provider.MarketSnapshot{}, nil, fmt.Errorf("synthetic market: %w", err)
	}

	sels := make([]domain.Selection, len(p.roles))
	lines := make([]domain.Line, len(p.roles))
	for i := range p.roles {
		sel, err := domain.NewSelection(domain.SelectionParams{
			ID:       p.selIDs[i],
			MarketID: p.id,
			Role:     p.roles[i],
			Name:     p.names[i],
		})
		if err != nil {
			return provider.MarketSnapshot{}, nil, fmt.Errorf("synthetic selection: %w", err)
		}
		effective, err := domain.EffectiveLine(market, sel)
		if err != nil {
			return provider.MarketSnapshot{}, nil, fmt.Errorf("synthetic selection %s: %w", sel.ID(), err)
		}
		sels[i] = sel
		lines[i] = effective
	}

	snap := provider.MarketSnapshot{Market: market, Selections: sels}
	if p.probs == nil {
		return snap, nil, nil
	}

	raws := make([]normalizer.RawMarket, len(a.books))
	if cap(sc.probs) < len(sels) {
		sc.probs = make([]float64, len(sels))
	}
	if cap(sc.quo) < len(sels) {
		sc.quo = make([]float64, len(sels))
	}
	for b, book := range a.books {
		fair := sc.probs[:len(sels)]
		copy(fair, p.probs[b])
		decimals := sc.quo[:len(sels)]
		if err := a.quote(fair, decimals, book); err != nil {
			return provider.MarketSnapshot{}, nil, fmt.Errorf("market %s: %w", p.id, err)
		}

		observed := a.stepTime(es.n - int64(book.lagSteps))
		outcomes := make([]normalizer.RawOutcome, len(sels))
		for i := range sels {
			price, err := domain.NewPrice(domain.PriceParams{
				SelectionID: sels[i].ID(),
				BookID:      bookID(book.slug),
				Decimal:     decimals[i],
				Line:        lines[i],
				ObservedAt:  observed,
			})
			if err != nil {
				return provider.MarketSnapshot{}, nil, fmt.Errorf("synthetic price: %w", err)
			}
			snap.Prices = append(snap.Prices, price)
			// The neutral shape's outcome label is the selection's own name for
			// every market type this generator emits: a competitor on h2h and
			// spreads, "Over"/"Under" on totals and props, the runner on
			// outrights. Description carries the prop's subject and is empty
			// everywhere else, which is exactly what RawOutcome documents.
			outcomes[i] = normalizer.RawOutcome{
				Name:        sels[i].Name(),
				Description: p.subject,
				Price:       decimals[i],
				Point:       linePointer(lines[i]),
			}
		}
		raws[b] = normalizer.RawMarket{Key: p.rawKey, LastUpdate: observed, Outcomes: outcomes}
	}
	return snap, raws, nil
}

// Selection display names. They are the provider's wording in the neutral shape,
// and normalizer/raw.go documents the two threshold labels by name ("Over"/
// "Under" on totals and player props), so they are constants rather than
// per-call-site literals.
const (
	overSelectionName  = "Over"
	underSelectionName = "Under"
	drawSelectionName  = "Draw"
)

// linePointer renders a domain.Line as the neutral shape's optional point.
// Absence and zero are different facts — a pick'em is a real, traded line — so
// the pointer is nil only when the line is genuinely absent.
func linePointer(l domain.Line) *float64 {
	v, ok := l.Value()
	if !ok {
		return nil
	}
	out := v
	return &out
}

// thresholdLine rounds an expected quantity to a quotable threshold.
//
// The floor at half a point is not cosmetic: domain.MarketType.validateLine
// rejects a total at or below zero, because combined scoring is non-negative by
// construction and a zero total is a parse error rather than a market. A
// low-scoring league whose latent total wanders near zero would otherwise fail
// the whole fetch.
func thresholdLine(mean float64) float64 {
	v := roundHalf(mean)
	if v < 0.5 {
		return 0.5
	}
	return v
}

// playerMarketKey renders a league's prop statistic as a provider market key in
// The Odds API's `player_*` family, which normalizer.MarketKeyPlayerPrefix
// matches by prefix.
func playerMarketKey(stat string) string {
	return normalizer.MarketKeyPlayerPrefix + strings.ToLower(strings.ReplaceAll(stat, " ", "_"))
}
