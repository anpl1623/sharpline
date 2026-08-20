package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// The bet slip: pricing a ticket without placing it.
//
// # This is a READ, and that is the whole reason it can live here
//
// Nothing on this path writes: no wager, no leg, no ledger entry, no
// idempotency key, no audit row. It reads the catalogue, reads the current
// quotes, folds the balance, and computes. Placement is internal/betting's
// because it is a transaction with an ordering that must not be re-derived;
// quoting is not a transaction at all, so composing it out of the ports this
// package already holds costs nothing and adds no second answer to any question
// that has one.
//
// # Every RULE here is borrowed, and none of it is re-implemented
//
// That distinction is what makes this file legitimate rather than a duplicate
// of the placement path:
//
//	slip shape          betting.Slip.Validate — the same pure validator the
//	                    placement path runs, called on the same value type
//	ticket price        the injected [TicketPricer], which is THE SAME
//	                    INSTANCE the placement service holds, so a slip that
//	                    quotes at X places at X or is refused for having moved
//	payout arithmetic   domain.Money.MulFloat under [wagerRounding]
//	ticket count        betting.Slip.TicketCount / TotalStake
//	combination set     domain.RoundRobin.Combinations
//	tradeability        domain.MarketStatus.AcceptsWagers,
//	                    domain.EventStatus.AcceptsWagers, auth.UserStatus.CanWager
//
// What this file contributes is the ORCHESTRATION of a read and the shape of the
// answer. The one thing it decides for itself is which of those reads counts as
// an impediment, and that decision is advisory by construction — see below.
//
// # One check is deliberately absent: responsible-gaming limits
//
// Evaluating a self-imposed limit is a period-scoped sum over ledger_entries
// taken under the placement lock, and internal/betting keeps that evaluator
// unexported precisely because there must be one of it. A second evaluation on
// this path would be a second answer to a responsible-gaming control, and the
// answer that matters is the one the transaction uses. So a slip that would
// breach a limit quotes as placeable and is refused 422 on submit.
//
// That is the safe direction to be wrong in. The control still binds; only the
// Place button's disabled state is optimistic, and a customer who is told "no"
// at submit is told it by the evaluator that actually decides.

// priceMatchTolerance is the RELATIVE tolerance for "is this the same quote".
//
// It mirrors internal/betting's constant of the same name and MUST NOT BE
// TIGHTER than it. A quote that flagged a difference the placement path treats
// as unchanged would put an "the price changed, accept?" prompt in front of a
// customer whose bet would have gone through untouched — a false alarm on the
// one interaction where the customer is being asked to trust the number.
//
// Exact equality would be defensible, since a float64 survives a JSON round trip
// today, but a client that formats to a string and re-parses does not. The
// magnitude is safe by an enormous margin: the smallest move a book can quote is
// a tick, which at even money is a relative difference near 5e-3, five million
// times this.
const priceMatchTolerance = 1e-9

// quotePlaceholderTicketDecimal satisfies the one validation rule that applies
// to a placement and cannot apply to a quote. See [validationCopy].
//
// The value is arbitrary and inert. It is inside every bound
// betting.validateTicketDecimal enforces, and it reaches nothing else: it is
// never compared, never priced against, and never sent to the placement service.
const quotePlaceholderTicketDecimal = 2.0

// validationCopy returns the slip as betting.Slip.Validate should see it on a
// QUOTE.
//
// Exactly one rule differs between quoting and placing, and it is right on both
// sides. Validate refuses a parlay or a teaser that does not state the
// whole-ticket price the customer SAW — correct for a placement, where booking
// at a price nobody agreed to is the entire defect that field exists to prevent,
// and impossible for a first quote, which is the request that PRODUCES that
// number. Enforced here, `/slip/quote` would be unreachable for the two kinds it
// most exists to price.
//
// The fix is a placeholder on a copy rather than a second validator. Two
// validators for one slip shape is precisely what this package refuses to
// maintain: they would agree today and diverge on the third rule somebody added
// to one of them. Everything else Validate checks — arity, duplicates, stake,
// teaser points, round-robin sizes, price bounds — applies unchanged and is
// enforced here exactly as it is at placement.
//
// The placeholder is applied ONLY where the rule bites. A round robin has no
// single ticket price and Validate refuses one, so nothing is set there.
func validationCopy(slip betting.Slip) betting.Slip {
	if slip.SeenTicketDecimal == 0 &&
		(slip.Kind == domain.WagerKindParlay || slip.Kind == domain.WagerKindTeaser) {
		slip.SeenTicketDecimal = quotePlaceholderTicketDecimal
	}
	return slip
}

// quoteLegIDPrefix namespaces the leg identifiers minted for a quote.
//
// domain.NewLeg requires an id, and a quote has no wager to derive one from. The
// ids exist only to satisfy the constructor and to keep the legs distinct for
// the pricer; they are never stored, never returned, and never reach a wagers
// row. Placement mints the real ones with betting.DeriveLegID, from the wager id
// the idempotency key produced.
const quoteLegIDPrefix = "quote."

// handleQuoteSlip prices a slip without placing it.
func (a *API) handleQuoteSlip(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	var body gen.SlipQuoteRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	slip, bad := parseSlipQuote(body)
	if len(bad) > 0 {
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable, msgUnprocessable, bad)
		return
	}
	// The same validator the placement path runs, on the same value. Running it
	// before any I/O keeps a malformed slip from doing work, and running the
	// SAME function is what stops the two paths disagreeing about what a ticket
	// is.
	if err := validationCopy(slip).Validate(); err != nil {
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable,
			msgSlipInvalid, slipParams(err))
		return
	}

	format := parseOddsFormat(r.URL.Query(), &badParams{})

	quote, err := a.quoteSlip(r.Context(), user, slip, body.SeenTicketDecimal, format)
	if err != nil {
		a.failBetting(w, r, "quote slip", err)
		return
	}
	respond(w, http.StatusOK, quote)
}

// quoteSlip assembles the priced slip.
//
// Split out from the handler so the assembly is testable without an HTTP round
// trip, and so the impediment rules have one home rather than being scattered
// through a response builder.
func (a *API) quoteSlip(
	ctx context.Context,
	user domain.UserID,
	slip betting.Slip,
	seenTicket *float64,
	format odds.Format,
) (gen.SlipQuote, error) {
	books, err := a.catalogue.Books(ctx)
	if err != nil {
		return gen.SlipQuote{}, fmt.Errorf("books: %w", err)
	}
	index := bookIndex(books)

	resolved, err := a.resolveSlipLegs(ctx, slip)
	if err != nil {
		return gen.SlipQuote{}, err
	}

	legs := make([]domain.Leg, 0, len(resolved))
	for _, leg := range resolved {
		legs = append(legs, leg.booked)
	}

	tickets, err := slip.TicketCount()
	if err != nil {
		return gen.SlipQuote{}, fmt.Errorf("ticket count: %w", err)
	}
	totalStake, err := slip.TotalStake()
	if err != nil {
		return gen.SlipQuote{}, fmt.Errorf("total stake: %w", err)
	}

	decimal, payout, err := a.priceTicket(ctx, slip, legs)
	if err != nil {
		return gen.SlipQuote{}, err
	}
	profit, err := payout.Sub(totalStake)
	if err != nil {
		return gen.SlipQuote{}, fmt.Errorf("potential profit: %w", err)
	}

	out := gen.SlipQuote{
		Kind:                 gen.WagerKind(slip.Kind.String()),
		StakeMinor:           slip.Stake.MinorUnits(),
		TicketCount:          int32(tickets),
		TotalStakeMinor:      totalStake.MinorUnits(),
		PotentialPayoutMinor: payout.MinorUnits(),
		PotentialProfitMinor: profit.MinorUnits(),
		Rounding:             gen.Rounding(wagerRounding.String()),
		Legs:                 make([]gen.SlipQuoteLeg, 0, len(resolved)),
		Impediments:          []gen.SlipImpediment{},
		AsOf:                 a.now().UTC(),
	}
	if decimal != nil {
		out.DecimalOdds = decimal
	}
	if seenTicket != nil && decimal != nil {
		movement := priceMovement(*seenTicket, *decimal)
		out.SeenTicketDecimal = seenTicket
		out.TicketMovement = &movement
	}

	sameGame := len(resolved) > 1
	firstEvent := domain.EventID("")
	for i, leg := range resolved {
		if i == 0 {
			firstEvent = leg.booked.EventID()
		} else if leg.booked.EventID() != firstEvent {
			sameGame = false
		}
		out.Legs = append(out.Legs, a.wireSlipLeg(leg, index, format))
	}
	out.IsSameGame = &sameGame

	for _, leg := range out.Legs {
		if leg.Movement != gen.Unchanged || (leg.LineMoved != nil && *leg.LineMoved) {
			out.PriceMoved = true
			break
		}
	}
	if out.TicketMovement != nil && *out.TicketMovement != gen.Unchanged {
		out.PriceMoved = true
	}

	cash, err := a.cashBalance(ctx, user)
	if err != nil {
		return gen.SlipQuote{}, err
	}
	out.CashBalanceMinor = cash.MinorUnits()

	out.Impediments, err = a.impediments(ctx, user, out, resolved, totalStake, cash)
	if err != nil {
		return gen.SlipQuote{}, err
	}
	out.Placeable = len(out.Impediments) == 0

	return out, nil
}

// resolvedLeg is one slip leg with everything the pricer and the response need.
type resolvedLeg struct {
	seen   betting.SlipLeg
	market Market
	event  Event
	quote  Quote

	// booked is the leg AS IT WOULD BE BOOKED — built from the store's own
	// quote, never from the request. The customer's numbers live in `seen` and
	// are used only for the comparison, which is the same separation
	// internal/betting maintains and for the same reason: there is no
	// expression here that turns a request field into a price.
	booked domain.Leg
}

// resolveSlipLegs reads the catalogue and the current quotes for every leg.
//
// # On the round-trip count
//
// This walks the slip leg by leg, memoising markets and events, so a 3-leg
// parlay across three games costs three selection reads, three market reads, one
// batched quote read and up to three event reads. Each is a primary-key lookup.
// A same-game slip collapses the event reads to one.
//
// A single batched statement would be fewer round trips, and it is deliberately
// not written: it would be a new statement in a query file this package does not
// own, added for a path whose cost is already bounded by domain.MaxWagerLegs
// (25). If the slip-quote path ever becomes hot the fix is that one statement,
// not a different design — and the shape of this function does not change.
func (a *API) resolveSlipLegs(ctx context.Context, slip betting.Slip) ([]resolvedLeg, error) {
	selectionIDs := make([]domain.SelectionID, 0, len(slip.Legs))
	for _, leg := range slip.Legs {
		selectionIDs = append(selectionIDs, leg.SelectionID)
	}

	// One read for every leg's current quote, through the same cache-then-
	// Postgres path the board uses, so the slip and the board cannot show
	// different numbers for one selection.
	quotes, err := a.currentQuotes(ctx, selectionIDs)
	if err != nil {
		return nil, fmt.Errorf("current quotes: %w", err)
	}
	byPair := make(map[quoteKey]Quote, len(quotes))
	for _, q := range quotes {
		byPair[quoteKey{q.SelectionID, q.BookID}] = q
	}

	markets := map[domain.MarketID]Market{}
	events := map[domain.EventID]Event{}

	out := make([]resolvedLeg, 0, len(slip.Legs))
	for _, leg := range slip.Legs {
		selection, err := a.catalogue.Selection(ctx, leg.SelectionID)
		if err != nil {
			return nil, fmt.Errorf("selection %s: %w", leg.SelectionID, err)
		}

		market, ok := markets[selection.MarketID]
		if !ok {
			if market, err = a.catalogue.Market(ctx, selection.MarketID); err != nil {
				return nil, fmt.Errorf("market %s: %w", selection.MarketID, err)
			}
			markets[market.ID] = market
		}

		event, ok := events[market.EventID]
		if !ok {
			if event, _, _, err = a.catalogue.EventWithBreadcrumb(ctx, market.EventID); err != nil {
				return nil, fmt.Errorf("event %s: %w", market.EventID, err)
			}
			events[event.ID] = event
		}

		quote, ok := byPair[quoteKey{leg.SelectionID, leg.BookID}]
		if !ok {
			// The same refusal internal/betting raises, wrapped in ITS sentinel
			// so the status mapping has one table rather than two. A slip with
			// an unquotable leg has no price, and a partial quote would invite
			// the client to render a payout for a bet it cannot place.
			return nil, fmt.Errorf("no current quote for selection %s at book %s: %w",
				leg.SelectionID, leg.BookID, betting.ErrQuoteUnavailable)
		}

		booked, err := a.bookableLeg(leg, selection, market, quote)
		if err != nil {
			return nil, err
		}
		out = append(out, resolvedLeg{seen: leg, market: market, event: event, quote: quote, booked: booked})
	}
	return out, nil
}

type quoteKey struct {
	selection domain.SelectionID
	book      domain.BookID
}

// bookableLeg builds the leg as it WOULD be booked, from the store's own quote.
//
// The identifier is synthetic and the value never leaves this request: it exists
// so the ticket pricer sees exactly the shape it will see at placement, which is
// what makes the quoted price and the placed price the same number.
func (a *API) bookableLeg(seen betting.SlipLeg, s Selection, m Market, q Quote) (domain.Leg, error) {
	id, err := domain.NewLegID(quoteLegIDPrefix + string(seen.SelectionID))
	if err != nil {
		return domain.Leg{}, fmt.Errorf("mint quote leg id: %w", err)
	}

	line := domain.NoLine()
	if q.Line != nil {
		if line, err = domain.NewLine(*q.Line); err != nil {
			return domain.Leg{}, fmt.Errorf("quote line for %s: %w", seen.SelectionID, err)
		}
	}

	price, err := domain.NewPrice(domain.PriceParams{
		SelectionID: q.SelectionID,
		BookID:      q.BookID,
		Decimal:     float64(q.Odds),
		Line:        line,
		ObservedAt:  q.ObservedAt,
	})
	if err != nil {
		return domain.Leg{}, fmt.Errorf("quote price for %s: %w", seen.SelectionID, err)
	}

	return domain.NewLeg(domain.LegParams{
		ID:          id,
		EventID:     m.EventID,
		MarketID:    m.ID,
		MarketType:  m.Type,
		Role:        s.Role,
		SelectionID: s.ID,
		Price:       price,
	})
}

// priceTicket returns the ticket price and the total potential payout.
//
// # A round robin has no single price, so it is not given one
//
// Its combinations are independent tickets at different prices, and a headline
// number would be an average nobody is offered. So the decimal is nil and the
// payout is summed by pricing every combination through the SAME pricer, at the
// SAME per-combination stake, that the placement path uses — which is what makes
// the quoted total and the booked total the same number rather than two
// estimates of one.
func (a *API) priceTicket(ctx context.Context, slip betting.Slip, legs []domain.Leg) (*float64, domain.Money, error) {
	if slip.Kind != domain.WagerKindRoundRobin {
		decimal, err := a.pricer.TicketDecimal(ctx, betting.Ticket{
			Kind:         slip.Kind,
			Legs:         legs,
			TeaserPoints: slip.TeaserPoints,
		})
		if err != nil {
			return nil, 0, err
		}
		payout, err := slip.Stake.MulFloat(decimal, wagerRounding)
		if err != nil {
			return nil, 0, fmt.Errorf("potential payout: %w", err)
		}
		return &decimal, payout, nil
	}

	// domain.NewRoundRobin does the expansion and enforces the bounds; the
	// identifier is synthetic for the same reason the leg ids are. Nothing here
	// is written, so nothing here needs a durable identity.
	id, err := domain.NewRoundRobinID(quoteLegIDPrefix + "rr")
	if err != nil {
		return nil, 0, fmt.Errorf("mint quote round robin id: %w", err)
	}
	rr, err := domain.NewRoundRobin(domain.RoundRobinParams{
		ID:                  id,
		UserID:              domain.UserID("quote"),
		Legs:                legs,
		Sizes:               slip.Sizes,
		StakePerCombination: slip.Stake,
		PlacedAt:            a.now().UTC(),
	})
	if err != nil {
		return nil, 0, err
	}

	payouts := make([]domain.Money, 0, rr.CombinationCount())
	for _, combination := range rr.Combinations() {
		decimal, err := a.pricer.TicketDecimal(ctx, betting.Ticket{
			Kind: domain.WagerKindRoundRobin,
			Legs: combination,
		})
		if err != nil {
			return nil, 0, err
		}
		payout, err := slip.Stake.MulFloat(decimal, wagerRounding)
		if err != nil {
			return nil, 0, fmt.Errorf("combination payout: %w", err)
		}
		payouts = append(payouts, payout)
	}
	total, err := domain.SumMoney(payouts...)
	if err != nil {
		return nil, 0, fmt.Errorf("round robin payout over %d ticket(s): %w", len(payouts), err)
	}
	return nil, total, nil
}

// wireSlipLeg maps one resolved leg onto the wire.
func (a *API) wireSlipLeg(leg resolvedLeg, books map[domain.BookID]Book, format odds.Format) gen.SlipQuoteLeg {
	eventStatus := gen.EventStatus(leg.event.Status.String())
	lineMoved := !leg.seen.SeenLine.Equal(leg.booked.Price().Line())

	return gen.SlipQuoteLeg{
		SelectionId:    leg.seen.SelectionID.String(),
		BookId:         leg.seen.BookID.String(),
		BookSlug:       bookSlug(books, leg.seen.BookID),
		MarketId:       leg.market.ID.String(),
		MarketType:     gen.MarketType(leg.market.Type.String()),
		MarketStatus:   gen.MarketStatus(leg.market.Status.String()),
		EventId:        leg.event.ID.String(),
		EventStatus:    &eventStatus,
		Role:           gen.SelectionRole(leg.booked.Role().String()),
		SeenDecimal:    leg.seen.SeenDecimal,
		SeenLine:       linePtr(leg.seen.SeenLine),
		CurrentDecimal: float64(leg.quote.Odds),
		CurrentDisplay: renderOdds(leg.quote.Odds, format),
		CurrentLine:    leg.quote.Line,
		Movement:       priceMovement(leg.seen.SeenDecimal, float64(leg.quote.Odds)),
		LineMoved:      &lineMoved,
		Tradeable:      a.tradeable(leg),
		ObservedAt:     leg.quote.ObservedAt.UTC(),
	}
}

// tradeable reports whether this leg could be booked right now.
//
// The three predicates are the domain's own and the freshness horizon is this
// package's `quoteFreshness`, which is also what [API.currentQuotes] filters on
// — so a quote old enough to be excluded from the board is old enough to be
// untradeable here, and the two cannot drift apart.
//
// It is not the placement decision. internal/betting applies its own MaxQuoteAge
// inside the transaction, and that is the one that refuses a bet.
func (a *API) tradeable(leg resolvedLeg) bool {
	switch {
	case !leg.market.Status.AcceptsWagers():
		return false
	case !leg.event.Status.AcceptsWagers():
		return false
	case a.now().Sub(leg.quote.ObservedAt) > quoteFreshness:
		return false
	default:
		return true
	}
}

// cashBalance folds the customer's spendable balance.
//
// Cash only, not cash plus escrow: escrow holds the stakes of open wagers, which
// have left the spendable balance. Adding them would tell a customer with three
// running parlays that they can afford a bet they cannot.
func (a *API) cashBalance(ctx context.Context, user domain.UserID) (domain.Money, error) {
	balances, err := a.ledger.Balances(ctx, user)
	if err != nil {
		return 0, fmt.Errorf("balance: %w", err)
	}
	for _, b := range balances {
		if b.Kind == domain.AccountKindUserCash {
			return b.Amount, nil
		}
	}
	// An account with no ledger movement does not appear in the view, and zero
	// is the correct answer for it — a brand-new customer has no money, which is
	// a different thing from a failed read.
	return domain.ZeroMoney, nil
}

// impediments lists the reasons this slip cannot be placed right now.
//
// ADVISORY, ALL OF THEM. Every value behind these was read outside a transaction
// and can be stale by the time the user presses Place; internal/betting
// re-evaluates each one under a lock on the customer's own row, and that is the
// evaluation that decides. What this buys is a disabled button with a reason on
// it instead of a submit that fails.
//
// The messages are constants, exactly as an error body's are, and no value from
// the request or from an error reaches one.
func (a *API) impediments(
	ctx context.Context,
	user domain.UserID,
	quote gen.SlipQuote,
	legs []resolvedLeg,
	totalStake, cash domain.Money,
) ([]gen.SlipImpediment, error) {
	out := []gen.SlipImpediment{}

	profile, err := a.accounts.Profile(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	switch {
	case profile.Status == auth.UserStatusSelfExcluded:
		out = append(out, gen.SlipImpediment{
			Code:    gen.SlipImpedimentCodeSelfExcluded,
			Message: msgSelfExcluded,
		})
	case !profile.Status.CanWager():
		out = append(out, gen.SlipImpediment{
			Code:    gen.SlipImpedimentCodeAccountNotActive,
			Message: msgAccountBlocked,
		})
	}

	if cash.Compare(totalStake) < 0 {
		out = append(out, gen.SlipImpediment{
			Code:    gen.SlipImpedimentCodeInsufficientFunds,
			Message: msgInsufficientFunds,
		})
	}

	for i, leg := range quote.Legs {
		if leg.Tradeable {
			continue
		}
		id := legs[i].seen.SelectionID.String()
		out = append(out, gen.SlipImpediment{
			Code:        gen.SlipImpedimentCodeMarketUnavailable,
			Message:     msgMarketUnavailable,
			SelectionId: &id,
		})
	}

	// One slip-level entry rather than one per moved leg: the legs already carry
	// their own `movement` and `line_moved`, so repeating them here would be the
	// same fact in two places, and the impediment list exists to answer "why is
	// the button disabled" rather than to re-describe the slip.
	if quote.PriceMoved {
		out = append(out, gen.SlipImpediment{
			Code:    gen.SlipImpedimentCodePriceMoved,
			Message: msgPriceMoved,
		})
	}

	return out, nil
}

// parseSlipQuote turns the wire request into internal/betting's slip.
//
// The acceptance fields are absent from gen.SlipLeg by design — an acceptance is
// consent to book at a re-quoted number, and this endpoint books nothing — so
// there is no path here by which one could be silently honoured.
func parseSlipQuote(body gen.SlipQuoteRequest) (betting.Slip, []gen.InvalidParam) {
	var bad badParams

	slip := betting.Slip{Rounding: wagerRounding}

	kind, err := domain.ParseWagerKind(string(body.Kind))
	if err != nil {
		bad.add("kind", "must be one of straight, parlay, round_robin, teaser")
	}
	slip.Kind = kind

	stake, err := domain.FromMinorUnits(body.StakeMinor)
	if err != nil || !stake.IsPositive() {
		bad.add("stake_minor", "must be a positive integer number of minor units")
	}
	slip.Stake = stake

	if body.TeaserPoints != nil {
		slip.TeaserPoints = *body.TeaserPoints
	}
	if body.RoundRobinSizes != nil {
		slip.Sizes = make([]int, 0, len(*body.RoundRobinSizes))
		for _, size := range *body.RoundRobinSizes {
			slip.Sizes = append(slip.Sizes, int(size))
		}
	}

	slip.Legs = make([]betting.SlipLeg, 0, len(body.Legs))
	for i, leg := range body.Legs {
		parsed, ok := parsePlacementLeg(i, gen.PlacementLeg{
			SelectionId: leg.SelectionId,
			BookId:      leg.BookId,
			SeenDecimal: leg.SeenDecimal,
			SeenLine:    leg.SeenLine,
		}, &bad)
		if !ok {
			continue
		}
		slip.Legs = append(slip.Legs, parsed)
	}

	return slip, bad.items
}
