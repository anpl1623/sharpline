package pricing

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// Middle detection across books.
//
// A middle is two quotes on OPPOSITE sides of one market, taken at DIFFERENT
// LINES, where both legs can win. Back over 44.5 at one book and under 46.5 at
// another and a settled total of 45 or 46 wins both; anything else wins exactly
// one and loses the other, for a small net loss. CLAUDE.md §6 lists it beside
// arbitrage under the analytics differentiator.
//
// # What this file computes, and the one thing it refuses to
//
// It computes the window (which settled values win both legs), the equalised
// stake split, the payoff when the middle hits, the cost when it misses, and the
// hit probability at which those two exactly cancel.
//
// It does NOT compute the probability of hitting. That needs a model of the
// sport's scoring distribution — the discrete mass a football game puts on a
// three-point margin, the shape of an NBA total around 228 — and this project
// has no such model. Inventing one would put a fabricated number on a screen and
// violate the ledger's no-mock-data rule directly, so the interface for
// supplying one is exported ([SettlementDistribution]) and no implementation
// ships with it. Until a caller provides one, the honest output is the breakeven
// probability and the settlement classifier, both of which are arithmetic.
//
// # The breakeven probability is derived, not assumed
//
// With the equalising split — stake in proportion to implied probability, which
// makes the two miss outcomes cost the same — the breakeven hit probability of a
// middle is EXACTLY the market's overround, S - 1, whatever the two prices are.
// Writing T for the total staked and S = 1/d_above + 1/d_below:
//
//	miss: exactly one leg wins and returns T/S, so profit  = T/S - T = -T(S-1)/S
//	hit:  both legs win and return T/S each,  so profit    = 2T/S - T = T(2-S)/S
//	p·T(2-S)/S = (1-p)·T(S-1)/S  ⟹  p(2-S) + p(S-1) = S-1  ⟹  p = S - 1
//
// Two -110 legs give S = 1.0476 and a breakeven of 4.76%, which is the figure
// the trade quotes. It needs no model of anything, which is why it is reported.
//
// Everything in this file is pure: no clock, no I/O, no metrics. See the header
// of arbitrage.go for why, including which process-level Prometheus series the
// caller — not this package — owns.

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

var (
	// ErrInvalidMiddleConfig reports a scanner configuration that cannot mean
	// what it says.
	ErrInvalidMiddleConfig = errors.New("pricing: invalid middle configuration")

	// ErrNotAMiddle reports a leg pair that does not middle: same side, no line,
	// identical lines, or lines that differ AGAINST the bettor (a sandwich,
	// where both legs can lose).
	ErrNotAMiddle = errors.New("pricing: the two quotes do not form a middle")

	// ErrInvalidDistribution reports a caller-supplied [SettlementDistribution]
	// that returned something that is not a probability, or a set of outcome
	// probabilities summing above one.
	ErrInvalidDistribution = errors.New("pricing: settlement distribution returned an invalid probability")
)

// -----------------------------------------------------------------------------
// The settlement axis
// -----------------------------------------------------------------------------

// MiddleAxis names the quantity a middle's window is measured on. It is carried
// on the finding because "the window is (2.5, 3.5)" means nothing without it.
type MiddleAxis uint8

const (
	// MiddleAxisUnknown is the invalid zero value.
	MiddleAxisUnknown MiddleAxis = iota

	// MiddleAxisHomeMargin is the home side's winning margin, home score minus
	// away score, which may be negative. Spread markets live on it.
	MiddleAxisHomeMargin

	// MiddleAxisTotal is combined scoring. Total markets live on it.
	MiddleAxisTotal

	// MiddleAxisPlayerStat is the individual quantity a player prop is about —
	// points, passing yards, whatever the market names in its subject.
	MiddleAxisPlayerStat
)

// String implements fmt.Stringer.
func (a MiddleAxis) String() string {
	switch a {
	case MiddleAxisHomeMargin:
		return "home_margin"
	case MiddleAxisTotal:
		return "total"
	case MiddleAxisPlayerStat:
		return "player_stat"
	default:
		return "unknown"
	}
}

// Valid reports whether a is a defined axis.
func (a MiddleAxis) Valid() bool {
	switch a {
	case MiddleAxisHomeMargin, MiddleAxisTotal, MiddleAxisPlayerStat:
		return true
	default:
		return false
	}
}

// axisFor returns the settlement axis a market type is graded on, and whether
// the type supports middles at all. Moneylines and futures do not: without a
// line there are no two lines to differ.
func axisFor(t domain.MarketType) (MiddleAxis, bool) {
	switch t {
	case domain.MarketTypeSpread:
		return MiddleAxisHomeMargin, true
	case domain.MarketTypeTotal:
		return MiddleAxisTotal, true
	case domain.MarketTypePlayerProp:
		return MiddleAxisPlayerStat, true
	default:
		return MiddleAxisUnknown, false
	}
}

// middleSide converts one quote into the pair (threshold, side) on the market's
// settlement axis, where the "above" side wins when the settled value exceeds
// its threshold and the "below" side wins when the settled value falls short of
// its own.
//
// # The spread algebra, written out, because the signs are the trap
//
// A side covers when its own margin plus its own handicap is positive. Write M
// for the home margin and note that the away side's margin is -M, and that
// [domain.Price.Line] already carries each side's line from ITS OWN perspective
// (the away quote on a -3.5 market reads +3.5):
//
//	home at ℓ_h covers  ⟺  M + ℓ_h > 0  ⟺  M > -ℓ_h   → above, threshold -ℓ_h
//	away at ℓ_a covers  ⟺  -M + ℓ_a > 0  ⟺  M < ℓ_a   → below, threshold  ℓ_a
//
// So home -2.5 is "M above 2.5" and away +3.5 is "M below 3.5", and the middle
// is M = 3 — a home win by exactly three points. The two thresholds are
// negations of each other's raw line, and only one of the two is negated, which
// is precisely the asymmetry that produces a plausible-looking wrong answer when
// it is glossed over.
//
// Totals and player props need no such flip: both sides quote the same absolute
// threshold and over is above, under is below.
func middleSide(t domain.MarketType, r domain.SelectionRole, l domain.Line) (threshold float64, above, ok bool) {
	v, present := l.Value()
	if !present {
		return 0, false, false
	}
	switch {
	case t == domain.MarketTypeSpread && r == domain.SelectionRoleHome:
		return -v, true, true
	case t == domain.MarketTypeSpread && r == domain.SelectionRoleAway:
		return v, false, true
	case (t == domain.MarketTypeTotal || t == domain.MarketTypePlayerProp) && r == domain.SelectionRoleOver:
		return v, true, true
	case (t == domain.MarketTypeTotal || t == domain.MarketTypePlayerProp) && r == domain.SelectionRoleUnder:
		return v, false, true
	default:
		return 0, false, false
	}
}

// -----------------------------------------------------------------------------
// The window
// -----------------------------------------------------------------------------

// MiddleSettlement is what a middle does at a given settled value.
type MiddleSettlement uint8

const (
	// MiddleSettlementUnknown is the invalid zero value.
	MiddleSettlementUnknown MiddleSettlement = iota

	// MiddleSettlementBothWin is the middle landing: both legs win.
	MiddleSettlementBothWin

	// MiddleSettlementWinAndPush is one leg winning while the other pushes, so
	// its stake comes back. It is reachable only where the pushing leg's
	// threshold is a whole number — a leg at 44.5 cannot push against an
	// integer-valued quantity — and it is strictly better than a miss.
	MiddleSettlementWinAndPush

	// MiddleSettlementWinAndLose is the miss: one leg wins, one loses. This is
	// the overwhelmingly common case and it is what the middle costs.
	MiddleSettlementWinAndLose
)

// String implements fmt.Stringer.
func (s MiddleSettlement) String() string {
	switch s {
	case MiddleSettlementBothWin:
		return "both_win"
	case MiddleSettlementWinAndPush:
		return "win_and_push"
	case MiddleSettlementWinAndLose:
		return "win_and_lose"
	default:
		return "unknown"
	}
}

// MiddleWindow is the set of settled values that win both legs: the OPEN
// interval (Low, High) on the named axis.
//
// Open, not closed, and the distinction is money. The above leg wins strictly
// above its threshold, so a settled value exactly equal to Low pushes that leg
// rather than winning it. Treating the interval as closed would count two push
// outcomes as hits and overstate the middle by exactly the mass an integer line
// carries — which for a football spread of 3 is the single most likely margin in
// the sport.
type MiddleWindow struct {
	Axis MiddleAxis

	// Low is the above leg's threshold and High the below leg's. High > Low
	// always; a constructor that cannot guarantee that returns ErrNotAMiddle.
	Low  float64
	High float64
}

// Width returns High - Low, the size of the middle in line units.
func (w MiddleWindow) Width() float64 { return w.High - w.Low }

// IntegerOutcomes counts the whole numbers strictly inside the window.
//
// It is the number that decides whether a middle is worth anything, because the
// settled value of every market this project models is an integer: points,
// combined points, a winning margin, a passing-yard count. A window of (44.5,
// 45.0) has a positive width and contains no integer at all, so it can never
// hit, and a filter on width alone would report it.
//
// The integrality of the axis is a fact about the sport rather than a theorem,
// which is why [MiddleConfig.RequireIntegerOutcome] is a switch rather than
// hard-coded.
func (w MiddleWindow) IntegerOutcomes() int {
	if !(w.High > w.Low) || math.IsInf(w.Low, 0) || math.IsInf(w.High, 0) {
		return 0
	}
	// The first integer strictly above Low, and the last strictly below High.
	first := math.Floor(w.Low) + 1
	last := math.Ceil(w.High) - 1
	if last < first {
		return 0
	}
	return int(last-first) + 1
}

// Outcomes returns the whole numbers strictly inside the window, in order. It is
// the enumeration [MiddleWindow.IntegerOutcomes] counts, for a caller that wants
// to name them ("this wins on 45 or 46") or to sum a per-outcome probability
// from a [SettlementDistribution].
func (w MiddleWindow) Outcomes() []float64 {
	n := w.IntegerOutcomes()
	if n == 0 {
		return nil
	}
	out := make([]float64, 0, n)
	for v := math.Floor(w.Low) + 1; v <= math.Ceil(w.High)-1; v++ {
		out = append(out, v)
	}
	return out
}

// LowPushes reports whether a settled value of exactly Low pushes the above leg
// — which happens exactly when Low is a whole number, since the leg is graded on
// an integer quantity. HighPushes is the same question at the other end.
func (w MiddleWindow) LowPushes() bool  { return w.Low == math.Trunc(w.Low) }
func (w MiddleWindow) HighPushes() bool { return w.High == math.Trunc(w.High) }

// Classify returns what the middle does at a settled value of x.
//
// The four cases are exhaustive and, because High > Low, no value can push both
// legs or lose both. That last part is the whole difference between a middle and
// a sandwich: back over 46.5 and under 44.5 and a total of 45 loses BOTH, which
// is why [NewMiddle] refuses that ordering rather than reporting it with a
// negative width.
func (w MiddleWindow) Classify(x float64) MiddleSettlement {
	switch {
	case math.IsNaN(x) || !(w.High > w.Low):
		return MiddleSettlementUnknown
	case x > w.Low && x < w.High:
		return MiddleSettlementBothWin
	case x == w.Low || x == w.High:
		return MiddleSettlementWinAndPush
	default:
		return MiddleSettlementWinAndLose
	}
}

// String implements fmt.Stringer.
func (w MiddleWindow) String() string {
	return fmt.Sprintf("(%g, %g) on %s", w.Low, w.High, w.Axis)
}

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

// MiddleConfig bounds what the scanner is willing to call a middle.
//
// The staleness discipline is identical to [ArbitrageConfig]'s and it is there
// for the same reason: two books quoting different lines forty seconds apart is
// usually one book that has not moved yet, and by the time both legs are struck
// the middle has closed.
type MiddleConfig struct {
	// MaxLegAge is the oldest a quote may be, measured to the instant passed to
	// Scan. Default 120s — the le="120" SLO bucket in
	// deploy/observability/rules/sharpline-alerts.yml.
	MaxLegAge time.Duration

	// MaxLegSpread is the largest permitted gap between the two legs'
	// observation instants. Default 30s. See [ArbitrageConfig.MaxLegSpread].
	MaxLegSpread time.Duration

	// MinWindow is the narrowest window worth reporting, in line units. Default
	// 0.5 — the smallest gap two quoted lines can actually differ by.
	MinWindow float64

	// RequireIntegerOutcome drops a window that contains no whole number, which
	// on an integer-scored sport can never hit. Default true; see
	// [MiddleWindow.IntegerOutcomes] for the assumption it encodes.
	RequireIntegerOutcome bool

	// RequireDistinctBooks demands the two legs come from different books.
	// Default true, matching CLAUDE.md §6's "middle detection ACROSS books". A
	// single book will not knowingly offer both sides of a middle on one market,
	// so a same-book pair is usually two alternate lines that this model
	// represents as one market, and reporting it would be reporting an artefact.
	RequireDistinctBooks bool
}

// DefaultMiddleConfig returns the configuration described on each field of
// [MiddleConfig].
func DefaultMiddleConfig() MiddleConfig {
	return MiddleConfig{
		MaxLegAge:             120 * time.Second,
		MaxLegSpread:          30 * time.Second,
		MinWindow:             0.5,
		RequireIntegerOutcome: true,
		RequireDistinctBooks:  true,
	}
}

// Validate reports whether the configuration is runnable.
func (c MiddleConfig) Validate() error {
	switch {
	case c.MaxLegAge <= 0:
		return fmt.Errorf("%w: MaxLegAge %s must be positive", ErrInvalidMiddleConfig, c.MaxLegAge)
	case c.MaxLegSpread < 0:
		return fmt.Errorf("%w: MaxLegSpread %s must not be negative", ErrInvalidMiddleConfig, c.MaxLegSpread)
	case c.MaxLegSpread > c.MaxLegAge:
		return fmt.Errorf("%w: MaxLegSpread %s exceeds MaxLegAge %s, so it can never bind",
			ErrInvalidMiddleConfig, c.MaxLegSpread, c.MaxLegAge)
	case math.IsNaN(c.MinWindow) || math.IsInf(c.MinWindow, 0):
		return fmt.Errorf("%w: MinWindow %v is not finite", ErrInvalidMiddleConfig, c.MinWindow)
	case c.MinWindow <= 0:
		return fmt.Errorf("%w: MinWindow %v must be positive; two identical lines are not a middle",
			ErrInvalidMiddleConfig, c.MinWindow)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Findings
// -----------------------------------------------------------------------------

// MiddleLeg is one side of a middle.
type MiddleLeg struct {
	SelectionID domain.SelectionID
	Role        domain.SelectionRole
	BookID      domain.BookID

	// Price is the quote, carried whole — its own line and its own observation
	// instant included.
	Price domain.Price

	// Threshold is this leg's cut on the settlement axis, already converted out
	// of the quote's own perspective by [middleSide]. The above leg wins
	// strictly above it; the below leg wins strictly below its own.
	Threshold float64

	// StakeFraction is this leg's share of the total outlay under the equalising
	// split: q / S. It is a probability ratio, not money.
	StakeFraction float64

	// Age is how old the quote was at the instant the scan was run.
	Age time.Duration
}

// MiddleOpportunity is one middle: an above leg and a below leg whose thresholds
// leave a gap that wins both.
type MiddleOpportunity struct {
	MarketID   domain.MarketID
	EventID    domain.EventID
	MarketType domain.MarketType

	// Above wins when the settled value exceeds Window.Low; Below wins when it
	// falls short of Window.High.
	Above MiddleLeg
	Below MiddleLeg

	Window MiddleWindow

	// Margin is the two legs' margin. It is normally OVERROUND — a middle costs
	// money when it misses, which is what distinguishes it from an arbitrage. It
	// can be under-round, and that case is the best finding this package
	// produces: a position that cannot lose and has an upside on top.
	Margin odds.Margin

	// BreakevenHitProbability is the hit probability at which the middle is a
	// coin flip, and it is exactly Margin.Overround (S - 1) under the equalising
	// split. See the file header for the derivation.
	//
	// It is a THRESHOLD, not a forecast. Nothing in this package estimates how
	// often the window is hit; supply a [SettlementDistribution] to
	// [MiddleOpportunity.Expectation] to compare against it.
	BreakevenHitProbability float64

	ObservedSpread time.Duration
	ObservedAt     time.Time
	OldestLegAge   time.Duration
}

// String renders the finding for a log line.
func (o MiddleOpportunity) String() string {
	return fmt.Sprintf("middle(%s %s %s wins on %v; %s@%s %.4g / %s@%s %.4g; S=%.6f breakeven=%.4f%% spread=%s)",
		o.MarketID, o.MarketType, o.Window, o.Window.Outcomes(),
		o.Above.Role, o.Above.BookID, o.Above.Price.Decimal(),
		o.Below.Role, o.Below.BookID, o.Below.Price.Decimal(),
		o.Margin.ImpliedSum, 100*o.BreakevenHitProbability, o.ObservedSpread)
}

// NewMiddle builds a middle from two quotes on one market, or reports why they
// are not one.
//
// It is exported because it is the whole of the middle arithmetic and is worth
// testing and reusing directly; [MiddleScanner] is the pairing loop around it
// plus the staleness and actionability policy.
//
// now is the instant leg ages are measured against, exactly as
// [domain.Price.Age] takes one. Nothing here reads a clock.
func NewMiddle(
	market domain.Market,
	a domain.Selection, aPrice domain.Price,
	b domain.Selection, bPrice domain.Price,
	now time.Time,
) (MiddleOpportunity, error) {
	axis, ok := axisFor(market.Type())
	if !ok {
		return MiddleOpportunity{}, fmt.Errorf("%w: a %s market has no line to differ on",
			ErrNotAMiddle, market.Type())
	}
	if err := domain.ValidateSelectionForMarket(market, a); err != nil {
		return MiddleOpportunity{}, fmt.Errorf("%w: %w", ErrInvalidCrossBookMarket, err)
	}
	if err := domain.ValidateSelectionForMarket(market, b); err != nil {
		return MiddleOpportunity{}, fmt.Errorf("%w: %w", ErrInvalidCrossBookMarket, err)
	}
	if aPrice.SelectionID() != a.ID() || bPrice.SelectionID() != b.ID() {
		return MiddleOpportunity{}, fmt.Errorf("%w: a price does not quote the selection it was paired with: %w",
			ErrInvalidCrossBookMarket, domain.ErrMismatchedParent)
	}
	if opposite, hasOpposite := a.Role().Opposite(); !hasOpposite || opposite != b.Role() {
		return MiddleOpportunity{}, fmt.Errorf("%w: %s and %s are not opposite sides",
			ErrNotAMiddle, a.Role(), b.Role())
	}

	aThresh, aAbove, aOK := middleSide(market.Type(), a.Role(), aPrice.Line())
	bThresh, bAbove, bOK := middleSide(market.Type(), b.Role(), bPrice.Line())
	if !aOK || !bOK {
		return MiddleOpportunity{}, fmt.Errorf("%w: a %s quote carries no usable line", ErrNotAMiddle, market.Type())
	}
	if aAbove == bAbove {
		// Unreachable given the Opposite check above; kept because the two
		// facts are established independently and a future role would break the
		// coupling silently.
		return MiddleOpportunity{}, fmt.Errorf("%w: both quotes are on the same side of the line", ErrNotAMiddle)
	}

	above, aboveSel, aboveThresh := aPrice, a, aThresh
	below, belowSel, belowThresh := bPrice, b, bThresh
	if !aAbove {
		above, aboveSel, aboveThresh = bPrice, b, bThresh
		below, belowSel, belowThresh = aPrice, a, aThresh
	}

	if !(belowThresh > aboveThresh) {
		// Equal thresholds are the ordinary two-sided market: that is an
		// arbitrage question, not a middle. Below < above is a SANDWICH — every
		// value in the gap loses both legs — and reporting it as a middle with a
		// negative window is the failure this branch exists to prevent.
		return MiddleOpportunity{}, fmt.Errorf(
			"%w: the above leg cuts at %g and the below leg at %g, so the lines do not differ in the bettor's favour",
			ErrNotAMiddle, aboveThresh, belowThresh)
	}

	aboveDec, err := odds.NewDecimal(above.Decimal())
	if err != nil {
		return MiddleOpportunity{}, fmt.Errorf("above leg: %w", err)
	}
	belowDec, err := odds.NewDecimal(below.Decimal())
	if err != nil {
		return MiddleOpportunity{}, fmt.Errorf("below leg: %w", err)
	}
	margin, err := odds.NewMargin([]odds.Decimal{aboveDec, belowDec})
	if err != nil {
		return MiddleOpportunity{}, fmt.Errorf("market %s: %w", market.ID(), err)
	}
	aboveQ, err := aboveDec.Probability()
	if err != nil {
		return MiddleOpportunity{}, fmt.Errorf("above leg: %w", err)
	}
	belowQ, err := belowDec.Probability()
	if err != nil {
		return MiddleOpportunity{}, fmt.Errorf("below leg: %w", err)
	}

	oldest, newest := above.ObservedAt(), below.ObservedAt()
	if newest.Before(oldest) {
		oldest, newest = newest, oldest
	}

	return MiddleOpportunity{
		MarketID:   market.ID(),
		EventID:    market.EventID(),
		MarketType: market.Type(),
		Above: MiddleLeg{
			SelectionID:   aboveSel.ID(),
			Role:          aboveSel.Role(),
			BookID:        above.BookID(),
			Price:         above,
			Threshold:     aboveThresh,
			StakeFraction: float64(aboveQ) / margin.ImpliedSum,
			Age:           above.Age(now),
		},
		Below: MiddleLeg{
			SelectionID:   belowSel.ID(),
			Role:          belowSel.Role(),
			BookID:        below.BookID(),
			Price:         below,
			Threshold:     belowThresh,
			StakeFraction: float64(belowQ) / margin.ImpliedSum,
			Age:           below.Age(now),
		},
		Window: MiddleWindow{Axis: axis, Low: aboveThresh, High: belowThresh},
		Margin: margin,
		// S - 1 exactly. odds/vig.go computes Overround as S-1 rather than by
		// rescaling, and notes the subtraction is exact by Sterbenz's lemma for
		// S in [0.5, 2] — which covers every two-price market, since each
		// implied probability is strictly under 1.
		BreakevenHitProbability: margin.Overround,
		ObservedSpread:          newest.Sub(oldest),
		ObservedAt:              oldest,
		OldestLegAge:            now.Sub(oldest),
	}, nil
}

// -----------------------------------------------------------------------------
// Money
// -----------------------------------------------------------------------------

// MiddleStakes is the money answer for a middle: what to stake on each leg, what
// it pays when the window is hit, and what it costs when it is not.
//
// Every amount is [domain.Money] — integer minor units.
type MiddleStakes struct {
	Requested domain.Money
	Outlay    domain.Money

	AboveStake domain.Money
	BelowStake domain.Money

	// HitReturn is what comes back when both legs win, and HitProfit is that
	// minus the outlay. This is the upside.
	HitReturn domain.Money
	HitProfit domain.Money

	// AboveOnlyReturn is what comes back when the settled value lands above the
	// window (the above leg wins, the below leg loses); BelowOnly is the mirror.
	// Their profits are normally negative and nearly equal — the split is chosen
	// to equalise them — and their difference is rounding.
	AboveOnlyReturn domain.Money
	AboveOnlyProfit domain.Money
	BelowOnlyReturn domain.Money
	BelowOnlyProfit domain.Money

	// LowPushReturn is what comes back when the settled value equals the
	// window's low edge: the above leg pushes and its stake is returned while
	// the below leg wins. HighPush is the mirror. Both are reachable only where
	// that edge is a whole number ([MiddleWindow.LowPushes]).
	LowPushReturn  domain.Money
	LowPushProfit  domain.Money
	HighPushReturn domain.Money
	HighPushProfit domain.Money

	// WorstProfit is the largest loss the position can take: the worse of the
	// two miss outcomes. It is what "the cost of the middle when it misses"
	// means, stated conservatively.
	WorstProfit domain.Money

	// BreakevenHitProbability is recomputed from the ROUNDED money —
	// -WorstProfit / (HitProfit - WorstProfit) — so it reflects what will
	// actually be staked rather than the ideal. It sits a hair above the
	// opportunity's exact S-1 because the rounding rule is conservative in both
	// directions. Zero when the position cannot lose.
	BreakevenHitProbability float64
}

// Stakes converts the equalising split into whole minor units.
//
// # The rounding rule is the same as [ArbitrageOpportunity.Stakes], and for a
// weaker but still sufficient reason
//
//	Each leg's STAKE is rounded UP to the next whole minor unit.
//	Each leg's RETURN is TRUNCATED to a whole minor unit.
//
// A middle carries no guarantee to protect — it loses money most of the time by
// design — so nothing here can be turned from a profit into a loss by rounding.
// What rounding CAN do is flatter the position, and the claim being made is
// "this costs at most C and pays P". Rounding the outlay up and the returns down
// overstates C and understates P, so both halves of the claim stay true of the
// integers. The alternative, rounding to nearest, would quote a cost the
// position can exceed.
//
// Unlike the arbitrage case there is no invariant to verify afterwards, so a
// small total is not an error: it produces a small, honest, probably
// unattractive position. The one thing the method refuses is a non-positive
// total.
func (o MiddleOpportunity) Stakes(total domain.Money) (MiddleStakes, error) {
	if total <= 0 {
		return MiddleStakes{}, fmt.Errorf("%w: %s", ErrStakeNotPositive, total)
	}

	aboveStake, err := ceilStake(total, o.Above.StakeFraction)
	if err != nil {
		return MiddleStakes{}, fmt.Errorf("above leg (%s at %s): %w", o.Above.Role, o.Above.BookID, err)
	}
	belowStake, err := ceilStake(total, o.Below.StakeFraction)
	if err != nil {
		return MiddleStakes{}, fmt.Errorf("below leg (%s at %s): %w", o.Below.Role, o.Below.BookID, err)
	}
	outlay, err := aboveStake.Add(belowStake)
	if err != nil {
		return MiddleStakes{}, fmt.Errorf("total outlay: %w", err)
	}

	aboveReturn, err := o.Above.Price.PayoutFor(aboveStake, domain.RoundTowardZero)
	if err != nil {
		return MiddleStakes{}, fmt.Errorf("above leg (%s at %s): %w", o.Above.Role, o.Above.BookID, err)
	}
	belowReturn, err := o.Below.Price.PayoutFor(belowStake, domain.RoundTowardZero)
	if err != nil {
		return MiddleStakes{}, fmt.Errorf("below leg (%s at %s): %w", o.Below.Role, o.Below.BookID, err)
	}

	hitReturn, err := aboveReturn.Add(belowReturn)
	if err != nil {
		return MiddleStakes{}, fmt.Errorf("both-win return: %w", err)
	}
	// A push returns the pushing leg's stake and pays the other leg in full.
	lowPushReturn, err := belowReturn.Add(aboveStake)
	if err != nil {
		return MiddleStakes{}, fmt.Errorf("low-edge push return: %w", err)
	}
	highPushReturn, err := aboveReturn.Add(belowStake)
	if err != nil {
		return MiddleStakes{}, fmt.Errorf("high-edge push return: %w", err)
	}

	s := MiddleStakes{
		Requested:       total,
		Outlay:          outlay,
		AboveStake:      aboveStake,
		BelowStake:      belowStake,
		HitReturn:       hitReturn,
		AboveOnlyReturn: aboveReturn,
		BelowOnlyReturn: belowReturn,
		LowPushReturn:   lowPushReturn,
		HighPushReturn:  highPushReturn,
	}
	for _, p := range []struct {
		from domain.Money
		into *domain.Money
		what string
	}{
		{hitReturn, &s.HitProfit, "both-win"},
		{aboveReturn, &s.AboveOnlyProfit, "above-only"},
		{belowReturn, &s.BelowOnlyProfit, "below-only"},
		{lowPushReturn, &s.LowPushProfit, "low-edge push"},
		{highPushReturn, &s.HighPushProfit, "high-edge push"},
	} {
		v, err := p.from.Sub(outlay)
		if err != nil {
			return MiddleStakes{}, fmt.Errorf("%s profit: %w", p.what, err)
		}
		*p.into = v
	}

	s.WorstProfit = s.AboveOnlyProfit
	if s.BelowOnlyProfit < s.WorstProfit {
		s.WorstProfit = s.BelowOnlyProfit
	}

	switch {
	case !s.WorstProfit.IsNegative():
		// The position cannot lose. Rare, and it means the two legs are
		// under-round as well as middled, which is an arbitrage with a bonus.
		s.BreakevenHitProbability = 0
	case s.HitProfit.IsPositive():
		// Both operands are exact as float64 (Money is bounded by 2^53-1).
		s.BreakevenHitProbability = float64(-s.WorstProfit) / (float64(s.HitProfit) - float64(s.WorstProfit))
	default:
		// Unreachable in exact arithmetic — two prices above 1.0 give S < 2, so
		// the both-win profit is positive — but reachable at a total so small
		// that truncation eats it. Reporting 1 says "this never breaks even at
		// this stake", which is true.
		s.BreakevenHitProbability = 1
	}
	return s, nil
}

// -----------------------------------------------------------------------------
// Expectation — the part this package refuses to guess
// -----------------------------------------------------------------------------

// SettlementDistribution is the caller's model of where the settled value lands
// on a market's axis.
//
// THIS PACKAGE SHIPS NO IMPLEMENTATION, ON PURPOSE. Turning a middle's window
// into an expected value needs the discrete scoring distribution of the sport —
// how much mass an NFL game puts on a three-point margin, how an NBA total is
// shaped around 228 — and this project has no such model. Writing one from
// plausible-looking constants would be fabricating the single number that
// decides whether the bet is good, which is exactly what the ledger's no-mock-
// data rule forbids and what would make the whole analytics surface untrustable.
//
// A real implementation would come from the line history already in the
// Timescale hypertable, or from a published scoring distribution, and it would
// arrive with its own tests and its own ADR. Until then a caller who has one
// passes it here; a caller who does not gets the breakeven probability and
// decides with their own judgement.
//
// Implementations must be pure and must not read a clock — the middle they are
// asked about is a value, not a live market.
type SettlementDistribution interface {
	// ProbabilityInInterval returns P(low < X < high) — the OPEN interval, to
	// match [MiddleWindow]. Returning a value outside [0, 1] is an error.
	ProbabilityInInterval(low, high float64) (float64, error)

	// ProbabilityAt returns P(X == v), the discrete mass at a single value. It
	// is what makes the push outcomes computable, and it is zero for a
	// continuous model.
	ProbabilityAt(v float64) (float64, error)
}

// MiddleExpectation is the value of a middle under a caller-supplied
// distribution.
type MiddleExpectation struct {
	HitProbability      float64
	LowPushProbability  float64
	HighPushProbability float64
	MissProbability     float64

	// ExpectedProfitMinorUnits is the probability-weighted profit, in minor
	// units, AS A FLOAT.
	//
	// It is deliberately not a [domain.Money]. CLAUDE.md §12 puts every money
	// value in integer minor units because a balance must be exact; an
	// expectation is not a balance, it is a weighted average of five outcomes,
	// and rounding it to a whole minor unit would make a tiny edge read as zero.
	// It must never be stored in a ledger or converted back into a Money.
	ExpectedProfitMinorUnits float64

	// EdgeOverBreakeven is HitProbability minus the breakeven probability. It is
	// positive exactly when the middle is worth taking under this distribution.
	EdgeOverBreakeven float64
}

// Expectation values the middle under d, at the stakes in s.
//
// It validates what the distribution returns: every probability must be in
// [0, 1] and the three win-or-push probabilities must not sum above 1. A model
// that violates that is a bug in the model, and letting it through would produce
// an expected value that looks authoritative and is arithmetic nonsense.
func (o MiddleOpportunity) Expectation(s MiddleStakes, d SettlementDistribution) (MiddleExpectation, error) {
	if d == nil {
		return MiddleExpectation{}, fmt.Errorf("%w: no distribution supplied", ErrInvalidDistribution)
	}
	hit, err := probabilityFrom(d.ProbabilityInInterval(o.Window.Low, o.Window.High))
	if err != nil {
		return MiddleExpectation{}, fmt.Errorf("P(%s): %w", o.Window, err)
	}

	var lowPush, highPush float64
	if o.Window.LowPushes() {
		if lowPush, err = probabilityFrom(d.ProbabilityAt(o.Window.Low)); err != nil {
			return MiddleExpectation{}, fmt.Errorf("P(X = %g): %w", o.Window.Low, err)
		}
	}
	if o.Window.HighPushes() {
		if highPush, err = probabilityFrom(d.ProbabilityAt(o.Window.High)); err != nil {
			return MiddleExpectation{}, fmt.Errorf("P(X = %g): %w", o.Window.High, err)
		}
	}

	// FairMarketTolerance is the project's single relative tolerance for
	// probability arithmetic (odds/vig.go: exact equality against 1.0 is wrong
	// here as a matter of arithmetic, not style — a fair three-way book sums to
	// 1 - 2^-53). Allowing it here keeps a correct model that sums to one from
	// being rejected by a rounding artefact.
	total := hit + lowPush + highPush
	if total > 1+odds.FairMarketTolerance {
		return MiddleExpectation{}, fmt.Errorf(
			"%w: P(both win) + P(low push) + P(high push) = %v exceeds 1", ErrInvalidDistribution, total)
	}
	miss := 1 - total
	if miss < 0 {
		miss = 0
	}

	// The miss mass is split across the two one-sided outcomes in proportion to
	// nothing the distribution was asked about, so the conservative reading is
	// used: the WORSE of the two miss profits is applied to the whole miss mass.
	// The two differ only by rounding under the equalising split, so this costs
	// at most a minor unit of pessimism and never overstates the position.
	e := MiddleExpectation{
		HitProbability:      hit,
		LowPushProbability:  lowPush,
		HighPushProbability: highPush,
		MissProbability:     miss,
		ExpectedProfitMinorUnits: hit*float64(s.HitProfit) +
			lowPush*float64(s.LowPushProfit) +
			highPush*float64(s.HighPushProfit) +
			miss*float64(s.WorstProfit),
		EdgeOverBreakeven: hit - s.BreakevenHitProbability,
	}
	return e, nil
}

// probabilityFrom validates a number a caller-supplied model returned.
func probabilityFrom(p float64, err error) (float64, error) {
	if err != nil {
		return 0, err
	}
	if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
		return 0, fmt.Errorf("%w: %v is not a probability", ErrInvalidDistribution, p)
	}
	return p, nil
}

// -----------------------------------------------------------------------------
// Scanner
// -----------------------------------------------------------------------------

// MiddleScanner finds middles across books.
//
// Construct it with [NewMiddleScanner]. Like [ArbitrageScanner] it holds no
// mutable state, reads no clock and does no I/O, so one value is safe to share.
type MiddleScanner struct {
	cfg MiddleConfig
}

// NewMiddleScanner validates the configuration and returns a scanner.
func NewMiddleScanner(cfg MiddleConfig) (*MiddleScanner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &MiddleScanner{cfg: cfg}, nil
}

// Config returns the configuration the scanner was built with.
func (s *MiddleScanner) Config() MiddleConfig { return s.cfg }

// Scan returns every middle in the given markets, as of the instant now,
// deterministically ordered: by market, then widest window first, then cheapest.
func (s *MiddleScanner) Scan(markets []CrossBookMarket, now time.Time) ([]MiddleOpportunity, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil scanner", ErrInvalidMiddleConfig)
	}
	var found []MiddleOpportunity
	for i, m := range markets {
		got, err := s.ScanMarket(m, now)
		if err != nil {
			return nil, fmt.Errorf("market %d of %d: %w", i, len(markets), err)
		}
		found = append(found, got...)
	}
	slices.SortFunc(found, compareMiddle)
	return found, nil
}

// ScanMarket returns the middles in one market.
//
// Every above-quote is paired against every below-quote, which is at most
// books² comparisons on a two-sided market and is not worth being clever about.
// Pairs that produce the SAME window are deduplicated down to the cheapest one:
// two pairs offering "wins on 45 or 46" are the same bet, and the one with the
// lower implied sum is strictly the better way to take it. Different windows are
// genuinely different bets and are all reported.
func (s *MiddleScanner) ScanMarket(m CrossBookMarket, now time.Time) ([]MiddleOpportunity, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil scanner", ErrInvalidMiddleConfig)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if _, ok := axisFor(m.Market.Type()); !ok {
		return nil, nil
	}
	// A middle on a market nobody can bet is noise: the brief for this phase is
	// explicit that a finding which cannot be acted on must not be published.
	if !m.Market.AcceptsWagers() {
		return nil, nil
	}

	byID, _ := m.selectionIndex()

	type quote struct {
		sel   domain.Selection
		price domain.Price
	}
	var aboveQuotes, belowQuotes []quote
	for _, p := range m.Prices {
		sel, ok := byID[p.SelectionID()]
		if !ok {
			continue // Unreachable: Validate rejected it.
		}
		if p.Age(now) > s.cfg.MaxLegAge {
			continue
		}
		_, above, ok := middleSide(m.Market.Type(), sel.Role(), p.Line())
		if !ok {
			continue
		}
		if above {
			aboveQuotes = append(aboveQuotes, quote{sel: sel, price: p})
		} else {
			belowQuotes = append(belowQuotes, quote{sel: sel, price: p})
		}
	}

	// Deterministic pairing order, so the deduplication below always keeps the
	// same representative when two pairs tie on cost.
	order := func(a, b quote) int {
		if c := cmp.Compare(a.price.BookID(), b.price.BookID()); c != 0 {
			return c
		}
		if !a.price.ObservedAt().Equal(b.price.ObservedAt()) {
			if a.price.ObservedAt().Before(b.price.ObservedAt()) {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.price.Decimal(), b.price.Decimal())
	}
	slices.SortFunc(aboveQuotes, order)
	slices.SortFunc(belowQuotes, order)

	best := make(map[MiddleWindow]MiddleOpportunity)
	for _, a := range aboveQuotes {
		for _, b := range belowQuotes {
			if s.cfg.RequireDistinctBooks && a.price.BookID() == b.price.BookID() {
				continue
			}
			o, err := NewMiddle(m.Market, a.sel, a.price, b.sel, b.price, now)
			if err != nil {
				if errors.Is(err, ErrNotAMiddle) {
					continue
				}
				return nil, err
			}
			if o.Window.Width() < s.cfg.MinWindow {
				continue
			}
			if s.cfg.RequireIntegerOutcome && o.Window.IntegerOutcomes() == 0 {
				continue
			}
			if o.ObservedSpread > s.cfg.MaxLegSpread {
				continue
			}
			if cur, seen := best[o.Window]; !seen || o.Margin.ImpliedSum < cur.Margin.ImpliedSum {
				best[o.Window] = o
			}
		}
	}

	found := make([]MiddleOpportunity, 0, len(best))
	for _, o := range best {
		found = append(found, o)
	}
	slices.SortFunc(found, compareMiddle)
	return found, nil
}

// compareMiddle is the deterministic output order: market, then the widest
// window, then the cheapest way to take it, then a total tiebreak on the books
// so two findings can never compare equal by accident.
func compareMiddle(a, b MiddleOpportunity) int {
	if c := cmp.Compare(a.MarketID, b.MarketID); c != 0 {
		return c
	}
	if c := cmp.Compare(b.Window.Width(), a.Window.Width()); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Window.Low, b.Window.Low); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Margin.ImpliedSum, b.Margin.ImpliedSum); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Above.BookID, b.Above.BookID); c != 0 {
		return c
	}
	return cmp.Compare(a.Below.BookID, b.Below.BookID)
}
