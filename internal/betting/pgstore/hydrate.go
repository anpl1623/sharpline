package pgstore

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/platform/postgres/gen"
)

// HydrateWager rebuilds a [domain.Wager] from the row that stores it and the
// rows that store its legs.
//
// # Why this is exported, and why it lives in this package
//
// internal/settlement/pgstore needs the identical reconstruction — its
// Tx.WagerWithLegs returns a domain.Wager built from a projection that is
// column-for-column the same, differing only in taking FOR UPDATE. Two copies of
// the transition replay below would be two places for a subtle rule to drift,
// and the drift would be silent: a wager rehydrated at the wrong instant still
// looks like a wager. domain.Wager is the betting aggregate, so the one function
// that reconstructs it lives with the package that writes it, and settlement
// imports it. That is the whole reason for the export; nothing else in this
// package is public.
//
// # THE HARD PART: the domain has no rehydration constructor, on purpose
//
// domain.NewWager always returns a ticket in [domain.WagerStatusPlaced] with
// pending legs and no returned amount, and domain.validateLegSet REFUSES a leg
// that is not pending. There is no exported way to set a status directly. That
// is not an oversight to work around — it is what makes every stored wager pass
// through the same state machine the live one did, so a row that could not have
// been produced by a legal sequence of transitions cannot be rehydrated into a
// value the rest of the program will trust.
//
// So this function REPLAYS the transitions, and the order is load-bearing:
//
//	terminal ticket  grade the legs (ascending graded_at), then Settle or
//	                 CashOut at transitioned_at.
//	open ticket      Open at transitioned_at FIRST, then grade the legs.
//	placed ticket    grade the legs, and nothing else.
//
// The asymmetry between the first two is not arbitrary and is the thing to
// understand before editing this. Wager.stamp enforces MONOTONE instants — an
// update stamped before the last one is ErrStaleUpdate — so the replay must
// visit the stored instants in the order they actually happened:
//
//   - On a TERMINAL ticket, settlement grades the legs and settles the wager in
//     ONE transaction using one instant, so transitioned_at is at or after every
//     graded_at. Grading first and settling last is the true order.
//
//   - On an OPEN ticket, transitioned_at is the instant the ticket's first event
//     went live, written by OpenWager — and settlement.sql's GradeLeg touches
//     only `legs`, so a leg graded afterwards does NOT advance the wager's
//     transitioned_at. That makes transitioned_at EARLIER than any graded_at, so
//     Open has to come first or the replay stamps backwards and fails on a
//     perfectly valid row.
//
// A ticket that is still `placed` has no transition of its own to replay:
// nothing writes wagers.transitioned_at between INSERT and the first Open, so it
// equals placed_at, and grading a leg from `placed` is legal (Wager.GradeLeg
// refuses only a TERMINAL ticket).
//
// # A refusal here is a data fault, and is reported as one
//
// If the stored instants are not monotone in the order above, the domain refuses
// the stamp and this function returns that error rather than clamping the
// instant to make it fit. Clamping would rewrite a settled ticket's recorded
// transition time to a value that never happened, in the one subsystem whose
// purpose is being auditable. A row the domain refuses is a bug in whatever
// wrote it, and it should be loud.
//
// # What is deliberately NOT reproduced
//
// Whether a terminal ticket passed through `open` on its way. The stored row
// does not record it — `wagers.status` holds only the final value — and
// WagerStatus.CanTransitionTo permits placed -> every terminal status directly,
// so replaying straight from `placed` produces a value identical in every
// observable field. Inventing an Open call to "be faithful" would invent an
// instant, which is worse than omitting a transition nothing can observe.
func HydrateWager(row gen.GetWagerRow, legRows []gen.ListWagerLegsRow) (domain.Wager, error) {
	kind, err := domain.ParseWagerKind(row.Kind)
	if err != nil {
		return domain.Wager{}, fmt.Errorf("wager %s: %w", row.ID, err)
	}
	status, err := domain.ParseWagerStatus(row.Status)
	if err != nil {
		return domain.Wager{}, fmt.Errorf("wager %s: %w", row.ID, err)
	}
	rounding, err := domain.ParseRounding(row.Rounding)
	if err != nil {
		return domain.Wager{}, fmt.Errorf("wager %s: %w", row.ID, err)
	}

	legs := make([]domain.Leg, 0, len(legRows))
	gradings := make([]legGrading, 0, len(legRows))
	for _, lr := range legRows {
		leg, grading, err := hydrateLeg(lr)
		if err != nil {
			return domain.Wager{}, err
		}
		legs = append(legs, leg)
		if grading.graded {
			gradings = append(gradings, grading)
		}
	}

	// The ticket as it was written: placed, every leg pending.
	w, err := domain.NewWager(domain.WagerParams{
		ID:              row.ID,
		UserID:          row.UserID,
		Kind:            kind,
		Legs:            legs,
		Stake:           row.StakeMinor,
		AcceptedDecimal: float64(row.AcceptedDecimal),
		Rounding:        rounding,
		TeaserPoints:    row.TeaserPoints.Float64,
		RoundRobinID:    deref(row.RoundRobinID),
		PlacedAt:        row.PlacedAt,
	})
	if err != nil {
		return domain.Wager{}, fmt.Errorf("rehydrate wager %s: %w", row.ID, err)
	}

	// Ascending graded_at, so every stamp is at or after the one before it.
	// Ties are broken by leg id purely so the replay is deterministic; two legs
	// graded in one transaction really do share an instant, and either order
	// produces the same value.
	slices.SortFunc(gradings, func(a, b legGrading) int {
		if by := a.at.Compare(b.at); by != 0 {
			return by
		}
		return cmp.Compare(a.id, b.id)
	})

	// See the doc comment: an open ticket's transitioned_at PRECEDES the
	// graded_at of any leg graded after it went live, so Open is replayed first.
	if status == domain.WagerStatusOpen {
		if w, err = w.Open(row.TransitionedAt); err != nil {
			return domain.Wager{}, fmt.Errorf("rehydrate wager %s: open: %w", row.ID, err)
		}
	}

	for _, g := range gradings {
		if w, err = w.GradeLeg(g.id, g.status, g.at); err != nil {
			return domain.Wager{}, fmt.Errorf("rehydrate wager %s: grade leg %s: %w", row.ID, g.id, err)
		}
	}

	if !status.IsTerminal() {
		return w, nil
	}

	// wagers_return_iff_terminal makes returned_minor non-NULL exactly on a
	// terminal ticket, and wagers_return_pair_complete makes the two money
	// columns null or set together. A nil here therefore means the schema and
	// this code disagree, which is worth saying rather than dereferencing.
	if row.ReturnedMinor == nil {
		return domain.Wager{}, fmt.Errorf(
			"rehydrate wager %s: status %s is terminal but returned_minor is NULL; "+
				"wagers_return_iff_terminal should have made this unstorable", row.ID, status)
	}

	// CashOut rather than Settle for a cashed-out ticket, because Settle refuses
	// a status that is not a GRADED outcome (WagerStatus.IsGraded excludes
	// cashed_out) and CashOut applies its own rule: strictly positive and at most
	// the potential payout. They are different checks on different amounts and
	// the domain keeps them apart deliberately.
	if status == domain.WagerStatusCashedOut {
		if w, err = w.CashOut(*row.ReturnedMinor, row.TransitionedAt); err != nil {
			return domain.Wager{}, fmt.Errorf("rehydrate wager %s: cash out: %w", row.ID, err)
		}
		return w, nil
	}

	if w, err = w.Settle(status, *row.ReturnedMinor, row.TransitionedAt); err != nil {
		return domain.Wager{}, fmt.Errorf("rehydrate wager %s: settle %s: %w", row.ID, status, err)
	}
	return w, nil
}

// legGrading is the terminal status a stored leg carries, held aside while the
// wager is constructed with pending legs.
//
// It exists because domain.validateLegSet refuses a non-pending leg at
// construction, so the status cannot travel on the [domain.Leg] into NewWager —
// it has to be applied afterwards, through the wager, by Wager.GradeLeg.
type legGrading struct {
	id     domain.LegID
	status domain.LegStatus
	at     time.Time
	graded bool
}

// hydrateLeg rebuilds one leg IN ITS PENDING STATE, and returns its stored
// grading separately.
//
// The price is built through domain.NewPrice rather than assembled, so a stored
// row that the domain would refuse — a decimal outside (1.0, 1e5], a zero
// observation instant — is an error at the read. Every one of those is also a
// CHECK on the column, which is precisely why running the constructor is free
// and why it earns its place: it is the assertion that the schema and the domain
// still agree.
//
// The line comes from the leg's own price and is already from THIS SELECTION's
// perspective — domain.EffectiveLine inverted it for an away spread at placement
// — so it is passed through untouched. teased_line likewise: leg.go keeps the
// real market price and carries the teased line beside it rather than forging a
// price at the moved number, "which would corrupt the line history and destroy
// CLV, since the book never traded there".
func hydrateLeg(lr gen.ListWagerLegsRow) (domain.Leg, legGrading, error) {
	marketType, err := domain.ParseMarketType(lr.MarketType)
	if err != nil {
		return domain.Leg{}, legGrading{}, fmt.Errorf("leg %s: %w", lr.ID, err)
	}
	role, err := domain.ParseSelectionRole(lr.Role)
	if err != nil {
		return domain.Leg{}, legGrading{}, fmt.Errorf("leg %s: %w", lr.ID, err)
	}
	status, err := domain.ParseLegStatus(lr.Status)
	if err != nil {
		return domain.Leg{}, legGrading{}, fmt.Errorf("leg %s: %w", lr.ID, err)
	}

	priceLine, err := lineFrom(lr.PriceLine)
	if err != nil {
		return domain.Leg{}, legGrading{}, fmt.Errorf("leg %s price line: %w", lr.ID, err)
	}
	teasedLine, err := lineFrom(lr.TeasedLine)
	if err != nil {
		return domain.Leg{}, legGrading{}, fmt.Errorf("leg %s teased line: %w", lr.ID, err)
	}

	price, err := domain.NewPrice(domain.PriceParams{
		SelectionID: lr.SelectionID,
		BookID:      lr.PriceBookID,
		Decimal:     float64(lr.PriceDecimal),
		Line:        priceLine,
		ObservedAt:  lr.PriceObservedAt,
	})
	if err != nil {
		return domain.Leg{}, legGrading{}, fmt.Errorf("leg %s booked price: %w", lr.ID, err)
	}

	leg, err := domain.NewLeg(domain.LegParams{
		ID:          lr.ID,
		EventID:     lr.EventID,
		MarketID:    lr.MarketID,
		MarketType:  marketType,
		Role:        role,
		SelectionID: lr.SelectionID,
		Price:       price,
		TeasedLine:  teasedLine,
	})
	if err != nil {
		return domain.Leg{}, legGrading{}, fmt.Errorf("rehydrate leg %s: %w", lr.ID, err)
	}

	// legs_graded_at_iff_graded makes the biconditional structural, so
	// disagreement between the two columns is a schema fault rather than a case
	// to paper over.
	graded := status != domain.LegStatusPending
	if graded != lr.GradedAt.Valid {
		return domain.Leg{}, legGrading{}, fmt.Errorf(
			"leg %s is %s with graded_at present=%t; legs_graded_at_iff_graded should have "+
				"made this unstorable", lr.ID, status, lr.GradedAt.Valid)
	}

	return leg, legGrading{id: lr.ID, status: status, at: lr.GradedAt.Time, graded: graded}, nil
}

// -----------------------------------------------------------------------------
// Column <-> value helpers
// -----------------------------------------------------------------------------

// lineFrom turns a nullable DOUBLE PRECISION into a [domain.Line].
//
// NULL is domain.NoLine(); 0.0 is a stored PICK'EM, which is a real traded value
// and not an absent one. That distinction is the reason domain.Line is a
// presence-carrying value type rather than a float64 with a sentinel, and
// collapsing it here would turn every pick'em spread into a moneyline.
//
// The value goes through domain.NewLine, which refuses NaN and infinities —
// legs_price_line_finite and legs_teased_line_finite refuse the same values on
// the way in, so this is the read-side half of that agreement.
func lineFrom(v pgtype.Float8) (domain.Line, error) {
	if !v.Valid {
		return domain.NoLine(), nil
	}
	return domain.NewLine(v.Float64)
}

// float8From is lineFrom's inverse for a plain optional float — the shape
// domain.Wager.TeaserPoints() returns.
func float8From(v float64, ok bool) pgtype.Float8 {
	return pgtype.Float8{Float64: v, Valid: ok}
}

// optional turns a (value, present) pair into the pointer sqlc uses for a
// nullable column carrying a domain type.
//
// sqlc.yaml explains why those columns are pointers rather than a pgtype: "there
// is no pgtype that can carry a domain type and the type matters more here than
// the presence-flag symmetry does". This is the one place that asymmetry is
// converted, so the call sites read as the (value, ok) pairs the domain actually
// returns.
func optional[T any](v T, present bool) *T {
	if !present {
		return nil
	}
	return &v
}

// deref is optional's inverse: a nil pointer becomes the zero value.
//
// Safe for the identifier types because their zero value is the empty string,
// which every domain constructor refuses — so a nil that should not have been
// nil fails at construction rather than becoming a plausible-looking id.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// returnedPair projects domain.Wager's settlement amounts into the two nullable
// columns.
//
// They share ONE presence flag in the domain — Wager.Returned() and
// Wager.NetReturn() both report it — and the schema states the same rule as
// wagers_return_pair_complete: two nulls or two values, never one of each.
// Producing them together here is what makes that impossible to get wrong at the
// call site.
func returnedPair(w domain.Wager) (returned, netReturn *domain.Money) {
	r, ok := w.Returned()
	if !ok {
		return nil, nil
	}
	n, _ := w.NetReturn()
	return &r, &n
}

// oddsDecimal narrows a domain float64 price to the column's odds.Decimal.
//
// A bare conversion, and deliberately not odds.NewDecimal: the value came out of
// a constructed domain.Wager or domain.Leg, both of which already refused NaN,
// infinities and out-of-range prices, and the column's own CHECK refuses them
// again on arrival. Re-validating between two validations would add an error
// path that cannot be taken and cannot be tested.
func oddsDecimal(v float64) odds.Decimal { return odds.Decimal(v) }
