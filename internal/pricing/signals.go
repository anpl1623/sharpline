// The arbitrage and middles scanners, wired onto the record the engine
// publishes.
//
// # Why this file exists
//
// CLAUDE.md §3's service table gives `pricer` six responsibilities, and
// "arbitrage + middles detection" is two of them. arbitrage.go and middles.go
// implement the mathematics; before this file they were implemented and NOT
// CONSTRUCTED — nothing in the engine, the service or cmd/pricer built a
// scanner, so a complete pricing pass produced a fair value and no findings.
// That is the phase-3a failure the contract ledger names explicitly ("phase 3a
// shipped a whole bus package nothing instantiated"), and it fails silently:
// every container is healthy, every record validates, and the analytics surface
// CLAUDE.md §6 calls "the differentiator" is empty for a reason no metric shows.
//
// # Why the scan runs inside Price rather than beside it
//
// A cross-book scan needs every book's quote on one market at one instant. A
// normalizer record already IS that (state.go and doc.go both say so), so the
// scan needs no state the pricing pass has not already decoded: it reuses the
// same [MarketSnapshot], through the seam [CrossBookMarketFrom] was written for.
// Running it as a second pass over a second decode would double the work and
// introduce the possibility of the two halves of one record disagreeing about
// the market they describe.
//
// It also keeps the engine a pure function of the record, which doc.go makes a
// REQUIREMENT of the service seam rather than a nicety: the scanners take the
// instant to measure ages against as a parameter, and the parameter is
// [MarketSnapshot.Anchor] — the record's own instant — not a clock.
//
// # A scan failure never costs the fair value
//
// The two scanners are diagnostics over the same prices the fair value came
// from. If [CrossBookMarketFrom] refuses the market, or a scanner does, the
// devig that already succeeded is still correct and is still worth publishing.
// So a scan error is counted on sharpline_pricer_scan_errors_total and the
// record ships without findings, rather than a market losing its price because
// an arbitrage could not be looked for.
package pricing

import (
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// ArbitrageLegRef is one leg of an arbitrage on the wire.
//
// It restates [ArbitrageLeg] with explicit tags for the same reason Margin
// restates odds.Margin: domain.Price is an immutable value type with unexported
// fields and no serialisation of its own, so the facts a consumer acts on — the
// book, the price, the line and the instant — are lifted out explicitly rather
// than left to marshal as an empty object.
type ArbitrageLegRef struct {
	SelectionID domain.SelectionID `json:"selection_id"`
	Role        string             `json:"role"`
	BookID      domain.BookID      `json:"book_id"`

	// Decimal is the quote. Decimal is the canonical price format everywhere in
	// this system; American and fractional are display conversions made at the
	// edge.
	Decimal float64 `json:"decimal"`

	// Line is the line THIS BOOK quoted, which for an arbitrage equals every
	// other leg's line by construction. It is carried per leg anyway so a
	// finding is auditable without reference to its parent.
	Line domain.Line `json:"line"`

	// StakeFraction is this leg's share of the total outlay, q_i / S. Staking in
	// these proportions is what equalises the return across outcomes. It is a
	// probability ratio; the Money-denominated answer is
	// [ArbitrageOpportunity.Stakes].
	StakeFraction float64 `json:"stake_fraction"`

	ObservedAt time.Time `json:"observed_at"`
	AgeSeconds float64   `json:"age_seconds"`
}

// ArbitrageRef is one arbitrage finding on the wire: a set of quotes, one per
// outcome, all at the same line, whose implied probabilities sum below 1.
type ArbitrageRef struct {
	// Line is the line every leg was quoted at, in the market's home frame.
	// Absent for moneylines and futures.
	Line domain.Line `json:"line"`

	// Legs is one leg per outcome, in selection display order.
	Legs []ArbitrageLegRef `json:"legs"`

	// Margin is the finding's own margin triple. It is under-round by
	// construction — that is what makes it a finding — so Overround is negative
	// and Vig is negative.
	Margin Margin `json:"margin"`

	// Return is the guaranteed profit per unit of total outlay, (1−S)/S.
	Return float64 `json:"return"`

	// DistinctBooks is how many different books the legs span. One means a
	// single book's own market is under-round, which odds/vig.go names as a real
	// and findable condition rather than an error.
	DistinctBooks int `json:"distinct_books"`

	// ObservedSpreadSeconds is the gap between the oldest and newest leg. It is
	// on the record because it is the number that separates a credible finding
	// from a stale one, and a consumer must be able to judge that for itself
	// rather than trust that a threshold was applied.
	ObservedSpreadSeconds float64 `json:"observed_spread_seconds"`

	// ObservedAt is the OLDEST leg's instant and OldestLegAgeSeconds its age at
	// the record's anchor. An opportunity is exactly as fresh as its stalest leg.
	ObservedAt          time.Time `json:"observed_at"`
	OldestLegAgeSeconds float64   `json:"oldest_leg_age_seconds"`
}

// MiddleLegRef is one side of a middle on the wire.
type MiddleLegRef struct {
	SelectionID domain.SelectionID `json:"selection_id"`
	Role        string             `json:"role"`
	BookID      domain.BookID      `json:"book_id"`
	Decimal     float64            `json:"decimal"`
	Line        domain.Line        `json:"line"`

	// Threshold is this leg's cut on the settlement axis, converted out of the
	// quote's own perspective — so an away +3.5 and a home −3.5 report the same
	// number and can be compared.
	Threshold float64 `json:"threshold"`

	StakeFraction float64   `json:"stake_fraction"`
	ObservedAt    time.Time `json:"observed_at"`
	AgeSeconds    float64   `json:"age_seconds"`
}

// MiddleRef is one middle finding on the wire: two quotes at DIFFERENT lines
// leaving a window in which both win.
//
// A middle is not an arbitrage and the record must not let the two be confused.
// An arbitrage cannot lose; a middle costs its margin whenever the window is
// missed, which is why BreakevenHitProbability is carried and Margin is normally
// over-round rather than under.
type MiddleRef struct {
	// Axis names what the window is measured on — "home_margin" for a spread,
	// "total" for a total.
	Axis string `json:"axis"`

	// Low and High bound the window; a settled value strictly between them wins
	// both legs. WidthPoints is High − Low and IntegerOutcomes is how many whole
	// numbers the window contains, which on an integer-scored sport is how many
	// ways it can actually hit.
	Low             float64 `json:"low"`
	High            float64 `json:"high"`
	WidthPoints     float64 `json:"width_points"`
	IntegerOutcomes int     `json:"integer_outcomes"`

	Above MiddleLegRef `json:"above"`
	Below MiddleLegRef `json:"below"`

	Margin Margin `json:"margin"`

	// BreakevenHitProbability is the hit rate at which the position is a coin
	// flip. It is a THRESHOLD, not a forecast: nothing in this system estimates
	// how often the window is hit, and a record that implied otherwise would be
	// presenting an assumption as a measurement.
	BreakevenHitProbability float64 `json:"breakeven_hit_probability"`

	ObservedSpreadSeconds float64   `json:"observed_spread_seconds"`
	ObservedAt            time.Time `json:"observed_at"`
	OldestLegAgeSeconds   float64   `json:"oldest_leg_age_seconds"`
}

// arbitrageRefFrom converts a finding onto the wire shape.
func arbitrageRefFrom(o ArbitrageOpportunity) ArbitrageRef {
	legs := make([]ArbitrageLegRef, 0, len(o.Legs))
	for _, l := range o.Legs {
		legs = append(legs, ArbitrageLegRef{
			SelectionID:   l.SelectionID,
			Role:          l.Role.String(),
			BookID:        l.BookID,
			Decimal:       l.Price.Decimal(),
			Line:          l.Price.Line(),
			StakeFraction: l.StakeFraction,
			ObservedAt:    l.Price.ObservedAt(),
			AgeSeconds:    l.Age.Seconds(),
		})
	}
	return ArbitrageRef{
		Line:                  o.Line,
		Legs:                  legs,
		Margin:                marginFrom(o.Margin),
		Return:                o.Return,
		DistinctBooks:         o.DistinctBooks,
		ObservedSpreadSeconds: o.ObservedSpread.Seconds(),
		ObservedAt:            o.ObservedAt,
		OldestLegAgeSeconds:   o.OldestLegAge.Seconds(),
	}
}

// middleLegRefFrom converts one side of a middle onto the wire shape.
func middleLegRefFrom(l MiddleLeg) MiddleLegRef {
	return MiddleLegRef{
		SelectionID:   l.SelectionID,
		Role:          l.Role.String(),
		BookID:        l.BookID,
		Decimal:       l.Price.Decimal(),
		Line:          l.Price.Line(),
		Threshold:     l.Threshold,
		StakeFraction: l.StakeFraction,
		ObservedAt:    l.Price.ObservedAt(),
		AgeSeconds:    l.Age.Seconds(),
	}
}

// middleRefFrom converts a finding onto the wire shape.
func middleRefFrom(o MiddleOpportunity) MiddleRef {
	axis, _ := axisFor(o.MarketType)
	return MiddleRef{
		Axis:                    axis.String(),
		Low:                     o.Window.Low,
		High:                    o.Window.High,
		WidthPoints:             o.Window.Width(),
		IntegerOutcomes:         o.Window.IntegerOutcomes(),
		Above:                   middleLegRefFrom(o.Above),
		Below:                   middleLegRefFrom(o.Below),
		Margin:                  marginFrom(o.Margin),
		BreakevenHitProbability: o.BreakevenHitProbability,
		ObservedSpreadSeconds:   o.ObservedSpread.Seconds(),
		ObservedAt:              o.ObservedAt,
		OldestLegAgeSeconds:     o.OldestLegAge.Seconds(),
	}
}

// scan runs both scanners over one decoded market and returns the findings in
// wire form.
//
// The instant every leg age is measured against is the SNAPSHOT'S ANCHOR, not a
// clock reading. That is what keeps the engine a pure function of the record —
// state.go and doc.go both require it — and it is also the better measurement:
// an age taken from a wall clock folds in bus and consumer lag, so the same
// market would yield a finding on a quiet system and none on a backed-up one.
//
// An error is returned only for a market the scanners cannot reason about, and
// [Engine.Price] treats it as a diagnostic failure rather than a pricing one.
func (e *Engine) scan(snap MarketSnapshot) ([]ArbitrageRef, []MiddleRef, error) {
	cbm, err := CrossBookMarketFrom(snap)
	if err != nil {
		return nil, nil, err
	}
	at := snap.Anchor()

	arbs, err := e.arb.ScanMarket(cbm, at)
	if err != nil {
		return nil, nil, err
	}
	mids, err := e.mid.ScanMarket(cbm, at)
	if err != nil {
		return nil, nil, err
	}

	var (
		arbRefs []ArbitrageRef
		midRefs []MiddleRef
	)
	for _, a := range arbs {
		arbRefs = append(arbRefs, arbitrageRefFrom(a))
	}
	for _, m := range mids {
		midRefs = append(midRefs, middleRefFrom(m))
	}
	return arbRefs, midRefs, nil
}
