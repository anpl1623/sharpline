package pricing

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// Cross-book arbitrage detection.
//
// CLAUDE.md §6 lists "arbitrage and middle detection across books" under the
// analytics differentiator and §3 assigns it to `pricer`. An arbitrage is a set
// of prices, one per outcome of one market, taken at whatever books quote them
// best, whose implied probabilities sum to LESS THAN ONE. The market is then
// under-round across books, and a stake split in proportion to those implied
// probabilities returns more than it costs whichever way the market lands.
//
// Nothing in this file re-derives odds arithmetic. internal/domain/odds owns
// every formula (CLAUDE.md §10 is explicit about what a second implementation of
// one formula costs), so the sum of implied probabilities comes from
// [odds.NewMargin] — which uses Neumaier compensated summation, and which the
// phase-1 ledger requires every summation of implied probabilities to route
// through so that two call sites cannot disagree about S. Under-round is not an
// error here: [odds.Margin.IsUnderround] documents it as "a feature (CLAUDE.md
// §6), not an error to be swallowed", and this file is that feature.
//
// # Nothing here reads a clock or does I/O
//
// Every function in this file is pure. The observation instant to measure
// against is a parameter, exactly as [domain.Price.Age] takes one, so a test can
// state it and two runs over the same input produce byte-identical findings.
// CLAUDE.md §12's "context.Context is the first parameter of anything doing I/O"
// therefore does not apply: there is no I/O to time out.
//
// # Metrics
//
// This file emits none, deliberately. `sharpline_pricing_duration_seconds` (the
// Grafana "Pricing latency" panel and the PricingLatencyHigh alert) and
// `sharpline_odds_staleness_seconds{stage="priced"}` are process-level series
// owned by the pricer service that CONSTRUCTS a scanner, not by the scanner
// itself. A pure, allocation-light function that registered a collector could
// not be instantiated twice in one process, and the timing the histogram wants
// is the caller's whole pricing pass rather than this one step. The caller wraps
// [ArbitrageScanner.Scan] and observes the elapsed time into that histogram.

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

var (
	// ErrInvalidCrossBookMarket reports a [CrossBookMarket] that does not
	// describe a market: no market, too few outcomes, a selection that does not
	// answer the market, or a price quoting a selection the market does not
	// carry. It always wraps the underlying domain error so the specific cause
	// survives errors.Is.
	ErrInvalidCrossBookMarket = errors.New("pricing: invalid cross-book market")

	// ErrInvalidArbitrageConfig reports a scanner configuration that cannot mean
	// what it says. CLAUDE.md §12: "fail fast and loudly on a bad config".
	ErrInvalidArbitrageConfig = errors.New("pricing: invalid arbitrage configuration")

	// ErrStakeNotPositive reports a non-positive total stake. There is no
	// arbitrage to size at zero.
	ErrStakeNotPositive = errors.New("pricing: total stake must be positive")

	// ErrStakeTooSmall reports a total stake at which the arbitrage does not
	// survive rounding to whole minor units.
	//
	// This is the failure the rounding rule exists to make loud. A 0.3% edge on
	// a two-leg arbitrage is worth 3 minor units per 1000 staked; round two
	// stakes the wrong way, or size the position at ten dollars, and the
	// "guaranteed" profit is a guaranteed loss. Returning a number that is
	// negative-but-plausible would be the worst possible behaviour, so the
	// method refuses instead and [ArbitrageOpportunity.MinimumTotalStake] says
	// how much is enough.
	ErrStakeTooSmall = errors.New("pricing: total stake too small to survive rounding")

	// ErrStakeFractionInvalid reports a stake weight that is not a usable
	// fraction. It cannot arise from a scanner-produced opportunity; it guards
	// an [ArbitrageOpportunity] assembled by hand.
	ErrStakeFractionInvalid = errors.New("pricing: stake fraction is not in (0, 1]")
)

// -----------------------------------------------------------------------------
// Input
// -----------------------------------------------------------------------------

// CrossBookMarket is one market, its COMPLETE outcome set, and every book's
// quote on it.
//
// The shape mirrors what already flows through the pipeline (`provider`'s market
// snapshot and the normalizer's published market view: a market, its selections,
// and a flat list of prices), but it is declared here rather than imported —
// CLAUDE.md §12, "interfaces are declared by the consumer". A pricing package
// that imported internal/ingest to name its input would invert the dependency
// the event flow in §3 draws.
//
// # The outcome set must be complete, and that is why Selections exists
//
// It would be convenient to infer the outcomes from the prices. It would also be
// wrong. Summing the two implied probabilities of a THREE-way soccer moneyline
// yields a number under 1 essentially always, and reporting that as an arbitrage
// would be a firehose of losing bets. Requiring the caller to state the market's
// selections means a missing outcome is a missing outcome rather than a phantom
// edge: a line group that does not cover every selection is skipped.
//
// # Prices carry each book's OWN line
//
// [domain.Price.Line] is "the line THIS BOOK quoted", while [domain.Market.Line]
// is the CONSENSUS across books (internal/ingest/normalizer/doc.go, and
// migrations/00003 from the schema side). Both facts are needed and they are
// different: arbitrage groups by the per-book line so that a -2.5 quote is never
// netted against a -3.5 quote, and middles.go exists precisely because those two
// lines can differ.
//
// Validate therefore does NOT call [domain.ValidatePriceForSelection] — that
// function requires the quote's line to equal the market's, which is exactly the
// disagreement this package is looking for.
type CrossBookMarket struct {
	// Market is the market being quoted. Its status decides whether any finding
	// is actionable at all.
	Market domain.Market

	// Selections is every outcome of the market, in any order. At least
	// [odds.MinMarketSelections] of them.
	Selections []domain.Selection

	// Prices is every book's quote on any of those selections, in any order. A
	// selection may be quoted by many books, one book, or none.
	Prices []domain.Price
}

// Validate reports whether the value describes a market this package can reason
// about. Every failure wraps [ErrInvalidCrossBookMarket] and, where one exists,
// the domain sentinel underneath it.
func (m CrossBookMarket) Validate() error {
	if m.Market.IsZero() {
		return fmt.Errorf("%w: no market", ErrInvalidCrossBookMarket)
	}
	if len(m.Selections) < odds.MinMarketSelections {
		return fmt.Errorf("%w: market %s has %d selection(s), need at least %d: %w",
			ErrInvalidCrossBookMarket, m.Market.ID(), len(m.Selections),
			odds.MinMarketSelections, odds.ErrTooFewSelections)
	}

	seen := make(map[domain.SelectionID]struct{}, len(m.Selections))
	for _, s := range m.Selections {
		if err := domain.ValidateSelectionForMarket(m.Market, s); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidCrossBookMarket, err)
		}
		if _, dup := seen[s.ID()]; dup {
			return fmt.Errorf("%w: market %s lists selection %s twice",
				ErrInvalidCrossBookMarket, m.Market.ID(), s.ID())
		}
		seen[s.ID()] = struct{}{}
	}

	rule := m.Market.Type().LineRule()
	for _, p := range m.Prices {
		if p.IsZero() {
			return fmt.Errorf("%w: market %s carries a zero price", ErrInvalidCrossBookMarket, m.Market.ID())
		}
		if _, ok := seen[p.SelectionID()]; !ok {
			return fmt.Errorf("%w: price quotes selection %s, which market %s does not carry: %w",
				ErrInvalidCrossBookMarket, p.SelectionID(), m.Market.ID(), domain.ErrMismatchedParent)
		}
		switch rule {
		case domain.LineRuleForbidden:
			if p.Line().Present() {
				return fmt.Errorf("%w: %s quote on a %s market carries line %s: %w",
					ErrInvalidCrossBookMarket, p.BookID(), m.Market.Type(), p.Line(), domain.ErrLineNotApplicable)
			}
		case domain.LineRuleRequired:
			if !p.Line().Present() {
				return fmt.Errorf("%w: %s quote on a %s market carries no line: %w",
					ErrInvalidCrossBookMarket, p.BookID(), m.Market.Type(), domain.ErrLineRequired)
			}
		case domain.LineRuleOptional, domain.LineRuleUnknown:
			// A player prop may or may not carry one; an unknown rule is
			// unreachable because domain.NewMarket rejects an invalid type.
		}
	}
	return nil
}

// CrossBookMarketFrom rebuilds a [CrossBookMarket] from the engine's decoded
// [MarketSnapshot].
//
// This is the seam between the pricing pass and the two scanners, and state.go
// names it from the other side: a snapshot "may be handed to the arbitrage and
// middles scanners concurrently with the fair-value pass". The snapshot stores a
// book's quotes as [BookQuote] hanging off a [BookState] — prices already
// converted into the odds package's types — while the scanners work in
// [domain.Price], which is the value that carries a book identifier, a line and
// an observation instant as one immutable fact. This function is the only place
// that conversion happens.
//
// Pair it with [MarketSnapshot.Anchor] for the instant to scan against:
//
//	m, err := CrossBookMarketFrom(snap)
//	found, err := scanner.ScanMarket(m, snap.Anchor())
//
// A quote the domain refuses fails the whole call rather than being dropped.
// Dropping one would silently change the market's implied sum, and the implied
// sum is the number both scanners are entirely built on.
func CrossBookMarketFrom(s MarketSnapshot) (CrossBookMarket, error) {
	out := CrossBookMarket{
		Market:     s.Market,
		Selections: slices.Clone(s.Selections),
	}
	for _, b := range s.Books {
		for _, q := range b.Quotes {
			p, err := domain.NewPrice(domain.PriceParams{
				SelectionID: q.SelectionID,
				BookID:      b.Book.ID(),
				Decimal:     float64(q.Decimal),
				Line:        q.Line,
				ObservedAt:  q.ObservedAt,
			})
			if err != nil {
				return CrossBookMarket{}, fmt.Errorf("%w: market %s, %s quote on %s: %w",
					ErrInvalidCrossBookMarket, s.Market.ID(), b.Book.ID(), q.SelectionID, err)
			}
			out.Prices = append(out.Prices, p)
		}
	}
	if err := out.Validate(); err != nil {
		return CrossBookMarket{}, err
	}
	return out, nil
}

// selectionIndex returns the market's selections keyed by identifier, and the
// outcome order every finding's legs are reported in.
//
// The order is (DisplayOrder, ID). [domain.SelectionRole.DisplayOrder] exists so
// "every surface renders a market's selections in the same order without each
// one inventing its own comparator", and a finding printed into a log or handed
// to the frontend is such a surface. The identifier breaks ties, which a futures
// field of forty outright runners needs.
func (m CrossBookMarket) selectionIndex() (map[domain.SelectionID]domain.Selection, []domain.Selection) {
	byID := make(map[domain.SelectionID]domain.Selection, len(m.Selections))
	ordered := make([]domain.Selection, len(m.Selections))
	copy(ordered, m.Selections)
	for _, s := range m.Selections {
		byID[s.ID()] = s
	}
	slices.SortFunc(ordered, func(a, b domain.Selection) int {
		if c := cmp.Compare(a.Role().DisplayOrder(), b.Role().DisplayOrder()); c != 0 {
			return c
		}
		return cmp.Compare(a.ID(), b.ID())
	})
	return byID, ordered
}

// homeFrameLine maps a quote's own line into the market's frame, which is the
// home side's for a spread and absolute for everything else.
//
// It is the same transformation the normalizer applies when it votes on the
// consensus line, and it exists for the same reason: "an away quote at +6.5
// votes with a home quote at -6.5 rather than against it". Without it a
// perfectly ordinary spread would look like two different lines and every
// cross-book arbitrage on a spread would be invisible.
func homeFrameLine(t domain.MarketType, r domain.SelectionRole, l domain.Line) domain.Line {
	if t == domain.MarketTypeSpread && r == domain.SelectionRoleAway {
		return l.Invert()
	}
	return l
}

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

// ArbitrageConfig bounds what the scanner is willing to call an arbitrage.
//
// The staleness bounds are the important part of this struct and they are not
// tuning knobs. REAL CROSS-BOOK ARBITRAGE IS MOSTLY STALE PRICES: two books
// disagreeing by more than their combined margin usually means one of them has
// not repriced yet, and a bettor who takes both legs finds the second one gone.
// Reporting those is the difference between a feature and a firehose, so the
// scanner refuses a finding whose legs were observed too far apart, and refuses
// one whose legs are too old to act on at all.
type ArbitrageConfig struct {
	// MaxLegAge is the oldest a quote may be, measured from its own observation
	// instant to the instant passed to Scan. A quote older than this is dropped
	// before any grouping happens, so a stale best price never hides a fresh
	// second-best one.
	//
	// The default is 120s because that is the number the project has already
	// committed to: deploy/observability/rules/sharpline-alerts.yml treats
	// le="120" on sharpline_odds_staleness_seconds as the SLO-1 compliance
	// bucket, so a leg older than that is, by the system's own published
	// definition, not fresh.
	MaxLegAge time.Duration

	// MaxLegSpread is the largest permitted difference between the oldest and
	// the newest leg's observation instant.
	//
	// This is the check that separates a credible feature from a false-positive
	// generator, and it is a judgement rather than a derivation: at some
	// separation "the books disagree" stops being the likely explanation and
	// "one book has not moved yet" starts being it. The default is 30s.
	//
	// The effect is directly observable rather than asserted. The synthetic
	// provider gives each simulated book a view lag of 0, 2, 4, 7 or 9 model
	// steps of 10s, and stamps its quote with the instant of the view it is
	// quoting, so tightening this value visibly excludes the deep-lag books from
	// findings over the same real feed. arbitrage_test.go measures exactly that.
	//
	// Must not exceed MaxLegAge: a spread wider than the age bound is
	// unreachable, so a configuration claiming one is a mistake worth failing on.
	MaxLegSpread time.Duration

	// MinReturn is the smallest guaranteed return fraction worth reporting,
	// where the return fraction is (1-S)/S — profit per unit of total outlay.
	// 0.001 is one tenth of one percent. Zero reports every under-round market.
	MinReturn float64

	// MinDistinctBooks is how many different books a finding must span.
	//
	// The default is 1, not 2, and that is deliberate. Phase 1 decided that a
	// single book's own market can be under-round — [odds.Margin.IsUnderround]
	// names odds boosts, stale legs and outright pricing mistakes as the causes
	// and says "finding it is a feature". Demanding two books would silently
	// discard the cleanest arbitrage of all, the one where a single book has
	// mispriced its own market and both legs can be struck in one place. Set it
	// to 2 to restrict the scan to genuinely cross-book findings.
	MinDistinctBooks int
}

// DefaultArbitrageConfig returns the configuration described on each field of
// [ArbitrageConfig].
func DefaultArbitrageConfig() ArbitrageConfig {
	return ArbitrageConfig{
		MaxLegAge:        120 * time.Second,
		MaxLegSpread:     30 * time.Second,
		MinReturn:        0.001,
		MinDistinctBooks: 1,
	}
}

// Validate reports whether the configuration is runnable.
func (c ArbitrageConfig) Validate() error {
	switch {
	case c.MaxLegAge <= 0:
		return fmt.Errorf("%w: MaxLegAge %s must be positive", ErrInvalidArbitrageConfig, c.MaxLegAge)
	case c.MaxLegSpread < 0:
		return fmt.Errorf("%w: MaxLegSpread %s must not be negative", ErrInvalidArbitrageConfig, c.MaxLegSpread)
	case c.MaxLegSpread > c.MaxLegAge:
		return fmt.Errorf("%w: MaxLegSpread %s exceeds MaxLegAge %s, so it can never bind",
			ErrInvalidArbitrageConfig, c.MaxLegSpread, c.MaxLegAge)
	case math.IsNaN(c.MinReturn) || math.IsInf(c.MinReturn, 0):
		return fmt.Errorf("%w: MinReturn %v is not finite", ErrInvalidArbitrageConfig, c.MinReturn)
	case c.MinReturn < 0:
		return fmt.Errorf("%w: MinReturn %v must not be negative", ErrInvalidArbitrageConfig, c.MinReturn)
	case c.MinDistinctBooks < 1:
		return fmt.Errorf("%w: MinDistinctBooks %d must be at least 1",
			ErrInvalidArbitrageConfig, c.MinDistinctBooks)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Findings
// -----------------------------------------------------------------------------

// ArbitrageLeg is one outcome of an arbitrage, and the quote that covers it.
type ArbitrageLeg struct {
	SelectionID domain.SelectionID
	Role        domain.SelectionRole

	// BookID is the book the quote came from. It is also on Price; it is lifted
	// here because a consumer acting on the finding reads it on every leg.
	BookID domain.BookID

	// Price is the quote, carried whole. It holds its own line and its own
	// observation instant, which are the two facts that make the finding
	// auditable after the fact.
	Price domain.Price

	// StakeFraction is this leg's share of the total outlay: q_i / S, where
	// q_i is the leg's implied probability and S their sum. Staking in these
	// proportions is what equalises the return across outcomes.
	//
	// It is a float because it is a probability ratio, not money. CLAUDE.md §12:
	// "Odds and probabilities are floats; ledger amounts are not." The
	// Money-denominated answer is [ArbitrageOpportunity.Stakes].
	StakeFraction float64

	// Age is how old the quote was at the instant the scan was run.
	Age time.Duration
}

// ArbitrageOpportunity is one under-round market: a set of quotes, one per
// outcome, all at the same line, whose implied probabilities sum below 1.
type ArbitrageOpportunity struct {
	MarketID   domain.MarketID
	EventID    domain.EventID
	MarketType domain.MarketType

	// Line is the line every leg was quoted at, in the market's own (home) frame
	// — so a spread arbitrage between a -3.5 home quote and a +3.5 away quote
	// reports -3.5. Absent for moneylines and futures.
	//
	// Every leg is at the SAME line and that is a hard rule, not a filter. An
	// "arbitrage" pairing an over 44.5 with an under 46.5 is a middle (both legs
	// can win, so the arithmetic here understates it) and one pairing an over
	// 46.5 with an under 44.5 is a sandwich (both legs can LOSE, so the
	// arithmetic here is a lie). Grouping by line before summing is what makes
	// the sum mean what the formula assumes it means.
	Line domain.Line

	// Legs is one leg per outcome, in [domain.SelectionRole.DisplayOrder].
	Legs []ArbitrageLeg

	// Margin carries the whole margin picture, ImpliedSum included. It is
	// under-round by construction: Margin.IsUnderround() is true.
	Margin odds.Margin

	// Return is the guaranteed profit per unit of total outlay, (1-S)/S.
	//
	// It is computed in that form rather than as 1/S - 1 for the reason
	// odds/vig.go gives for Vig: for S in [0.5, 2] the subtraction S-1 is exact
	// by Sterbenz's lemma, so the quotient rounds once and stays well
	// conditioned as the edge shrinks — and a thin edge is the only kind this
	// function ever reports.
	Return float64

	// DistinctBooks is how many different books the legs span. One means a
	// single book's own market is under-round.
	DistinctBooks int

	// ObservedSpread is the gap between the oldest and the newest leg's
	// observation instant. Small is good; see [ArbitrageConfig.MaxLegSpread].
	ObservedSpread time.Duration

	// ObservedAt is the OLDEST leg's observation instant, and OldestLegAge is
	// its age at scan time. An opportunity is exactly as fresh as its stalest
	// leg, so reporting the newest instant would flatter it.
	ObservedAt   time.Time
	OldestLegAge time.Duration
}

// String renders the finding for a log line.
func (o ArbitrageOpportunity) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "arb(%s %s line=%s S=%.6f return=%+.4f%% books=%d spread=%s legs=[",
		o.MarketID, o.MarketType, o.Line, o.Margin.ImpliedSum, 100*o.Return, o.DistinctBooks, o.ObservedSpread)
	for i, l := range o.Legs {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s@%s %.4g×%.4f", l.Role, l.BookID, l.Price.Decimal(), l.StakeFraction)
	}
	b.WriteString("])")
	return b.String()
}

// MinimumTotalStake returns a total stake at which the arbitrage is guaranteed
// to survive rounding.
//
// It is SUFFICIENT, not necessary — a smaller total often works, and
// [ArbitrageOpportunity.Stakes] is the authority because it checks the actual
// integers. The bound follows directly from the rounding rule stated on Stakes.
// With n legs, ideal stakes t_i summing to T and ideal return T/S on every leg:
//
//	outlay  = Σ ceil(t_i) ≤ T + n - 1        (each ceiling adds strictly under 1,
//	                                          and the total is an integer)
//	return  = min_i floor(s_i·d_i) ≥ T/S - 1  (s_i ≥ t_i, so no leg falls below
//	                                          the ideal; one truncation costs < 1)
//	profit  ≥ (T/S - 1) - (T + n - 1) = T(1-S)/S - n = T·Return - n
//
// so any T strictly greater than n/Return leaves a positive profit. In minor
// units: a two-leg arbitrage returning 0.3% needs 2/0.003 = 666.67, i.e. 6.67
// major units, before the rounding is affordable.
func (o ArbitrageOpportunity) MinimumTotalStake() (domain.Money, error) {
	if len(o.Legs) == 0 {
		return 0, fmt.Errorf("%w: opportunity has no legs", ErrInvalidCrossBookMarket)
	}
	if !(o.Return > 0) || math.IsInf(o.Return, 0) {
		return 0, fmt.Errorf("%w: return %v is not a positive finite fraction",
			ErrStakeTooSmall, o.Return)
	}
	bound := math.Floor(float64(len(o.Legs))/o.Return) + 1
	if bound > float64(domain.MaxSafeMoney) {
		return 0, fmt.Errorf("%w: a %v return needs more than %v minor units, which is not a representable amount",
			ErrStakeTooSmall, o.Return, bound)
	}
	return domain.FromMinorUnits(int64(bound))
}

// -----------------------------------------------------------------------------
// Money
// -----------------------------------------------------------------------------

// ArbitrageStakes is the money answer: what to stake on each leg, what it costs,
// and what comes back in the worst case.
//
// Every field is [domain.Money] — integer minor units, per CLAUDE.md §12.
type ArbitrageStakes struct {
	// Requested is the total the caller asked to stake.
	Requested domain.Money

	// Outlay is what the rounded split actually costs, which is Requested or a
	// few minor units more; see [ArbitrageOpportunity.Stakes] for the rule and
	// the bound.
	Outlay domain.Money

	// Stakes is index-aligned with the opportunity's Legs.
	Stakes []domain.Money

	// Returns is the TOTAL RETURN on each leg if that leg wins — stake × decimal
	// odds, stake included, truncated. Index-aligned with Stakes.
	Returns []domain.Money

	// WorstReturn is the smallest entry of Returns: the outcome that pays least.
	WorstReturn domain.Money

	// GuaranteedProfit is WorstReturn - Outlay, and it is strictly positive.
	// Stakes returns ErrStakeTooSmall rather than a non-positive value here.
	GuaranteedProfit domain.Money

	// RealisedReturn is GuaranteedProfit / Outlay as a fraction, for comparison
	// against the opportunity's ideal Return. It is a float because it is a
	// ratio; it is never a balance and must not be stored as one.
	RealisedReturn float64
}

// Stakes converts the opportunity's fractional split into whole minor units.
//
// # The rounding rule, and why it is not configurable
//
//	Each leg's STAKE is rounded UP to the next whole minor unit.
//	Each leg's RETURN is TRUNCATED to a whole minor unit.
//
// Both directions are the conservative one for the claim being made, which is
// "this profits whatever happens". Rounding a stake up can only raise that leg's
// return above the ideal equalised return T/S, never below it — so the leg that
// pays least still pays at least the ideal, minus the one minor unit truncation
// costs. Truncating the return can only understate what a winning leg pays. The
// cost of that safety is bounded and tiny: the outlay exceeds the requested
// total by at most n-1 minor units, four cents on a five-way market.
//
// Every other rule is unsafe in a way that is invisible. Rounding stakes to
// NEAREST lowers a leg's return below T/S whenever it rounds down, and on a 0.3%
// arbitrage — 3 minor units per 1000 staked — one such leg is enough to turn the
// guarantee into a loss on exactly one outcome, which is the outcome that will
// happen. Rounding DOWN does it on every leg at once. So [domain.Rounding] is
// not a parameter of this method: unlike a settlement policy, where phase 1
// deliberately refused a default because the choice is a reviewable policy
// question, here the arithmetic dictates one answer.
//
// # The invariant is checked after rounding, not assumed from before it
//
// The guarantee is a statement about the integers that will actually be staked,
// so it is verified against them: every entry of Returns must strictly exceed
// Outlay. If it does not, the method returns [ErrStakeTooSmall] and
// [ArbitrageOpportunity.MinimumTotalStake] names a total that works. It never
// returns a "guaranteed" profit that is zero or negative.
func (o ArbitrageOpportunity) Stakes(total domain.Money) (ArbitrageStakes, error) {
	if len(o.Legs) < odds.MinMarketSelections {
		return ArbitrageStakes{}, fmt.Errorf("%w: an arbitrage needs at least %d legs, got %d",
			ErrInvalidCrossBookMarket, odds.MinMarketSelections, len(o.Legs))
	}
	if total <= 0 {
		return ArbitrageStakes{}, fmt.Errorf("%w: %s", ErrStakeNotPositive, total)
	}

	stakes := make([]domain.Money, len(o.Legs))
	returns := make([]domain.Money, len(o.Legs))
	outlay := domain.ZeroMoney
	for i, leg := range o.Legs {
		s, err := ceilStake(total, leg.StakeFraction)
		if err != nil {
			return ArbitrageStakes{}, fmt.Errorf("leg %d (%s at %s): %w", i, leg.Role, leg.BookID, err)
		}
		stakes[i] = s
		outlay, err = outlay.Add(s)
		if err != nil {
			return ArbitrageStakes{}, fmt.Errorf("total outlay: %w", err)
		}
	}

	worst := domain.Money(0)
	for i, leg := range o.Legs {
		// RoundTowardZero on a positive product is truncation, which is the
		// "never overstate a payout" half of the rule above.
		r, err := leg.Price.PayoutFor(stakes[i], domain.RoundTowardZero)
		if err != nil {
			return ArbitrageStakes{}, fmt.Errorf("leg %d (%s at %s): %w", i, leg.Role, leg.BookID, err)
		}
		returns[i] = r
		if i == 0 || r < worst {
			worst = r
		}
	}

	profit, err := worst.Sub(outlay)
	if err != nil {
		return ArbitrageStakes{}, fmt.Errorf("guaranteed profit: %w", err)
	}
	if !profit.IsPositive() {
		minimum, minErr := o.MinimumTotalStake()
		if minErr != nil {
			return ArbitrageStakes{}, fmt.Errorf("%w: staking %s leaves %s on the worst outcome",
				ErrStakeTooSmall, total, profit)
		}
		return ArbitrageStakes{}, fmt.Errorf(
			"%w: staking %s over %d legs costs %s and returns %s on the worst outcome (%s); stake at least %s",
			ErrStakeTooSmall, total, len(o.Legs), outlay, worst, profit, minimum)
	}

	return ArbitrageStakes{
		Requested:        total,
		Outlay:           outlay,
		Stakes:           stakes,
		Returns:          returns,
		WorstReturn:      worst,
		GuaranteedProfit: profit,
		// Both operands are exact as float64 (Money is bounded by 2^53-1), so
		// this quotient carries a single rounding. It is a display and
		// comparison quantity only.
		RealisedReturn: float64(profit) / float64(outlay),
	}, nil
}

// ceilStake returns total × fraction rounded UP to a whole minor unit.
//
// [domain.Money.MulFloat] is the only place in the project where a float decides
// a Money, and it offers half-away-from-zero, half-to-even and toward-zero —
// there is no ceiling mode, because settlement never wants one. Truncation of a
// positive product is a floor, so the ceiling is that floor plus one whenever
// the product was not already whole.
//
// The comparison is against the float product, which carries at most one
// rounding. If that rounding lands the product a ULP above an exact integer the
// result is one minor unit larger than strictly necessary — safe, since the only
// consequence is a marginally larger outlay, and the caller's invariant check
// runs on the integers regardless.
func ceilStake(total domain.Money, fraction float64) (domain.Money, error) {
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction <= 0 || fraction > 1 {
		return 0, fmt.Errorf("%w: %v", ErrStakeFractionInvalid, fraction)
	}
	floor, err := total.MulFloat(fraction, domain.RoundTowardZero)
	if err != nil {
		return 0, err
	}
	if float64(floor) >= float64(total)*fraction {
		return floor, nil
	}
	one, err := domain.FromMinorUnits(1)
	if err != nil {
		return 0, err
	}
	return floor.Add(one)
}

// -----------------------------------------------------------------------------
// Scanner
// -----------------------------------------------------------------------------

// ArbitrageScanner finds under-round markets across books.
//
// Construct it with [NewArbitrageScanner]; the zero value has a zero
// configuration and every method on it fails. It holds no mutable state, reads
// no clock and does no I/O, so one value is safe to share across goroutines.
type ArbitrageScanner struct {
	cfg ArbitrageConfig
}

// NewArbitrageScanner validates the configuration and returns a scanner.
// CLAUDE.md §12: dependencies are constructor-injected and configuration is
// validated at startup rather than at first use.
func NewArbitrageScanner(cfg ArbitrageConfig) (*ArbitrageScanner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &ArbitrageScanner{cfg: cfg}, nil
}

// Config returns the configuration the scanner was built with.
func (s *ArbitrageScanner) Config() ArbitrageConfig { return s.cfg }

// Scan returns every arbitrage in the given markets, as of the instant now.
//
// The result is deterministic: sorted by market, then by line, then by return
// descending. An empty result is the normal case and is not an error — an
// efficient market has no arbitrage in it, and a scanner that had to invent one
// would be worse than useless.
//
// An invalid [CrossBookMarket] fails the whole call rather than being skipped.
// A malformed market is a bug in the caller, and silently dropping it would turn
// that bug into a permanently empty analytics surface with no signal anywhere.
func (s *ArbitrageScanner) Scan(markets []CrossBookMarket, now time.Time) ([]ArbitrageOpportunity, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil scanner", ErrInvalidArbitrageConfig)
	}
	var found []ArbitrageOpportunity
	for i, m := range markets {
		got, err := s.ScanMarket(m, now)
		if err != nil {
			return nil, fmt.Errorf("market %d of %d: %w", i, len(markets), err)
		}
		found = append(found, got...)
	}
	slices.SortFunc(found, compareArbitrage)
	return found, nil
}

// ScanMarket returns the arbitrages in one market — at most one per line the
// market is quoted at, since within a line there is exactly one best price per
// outcome and any other combination sums higher.
func (s *ArbitrageScanner) ScanMarket(m CrossBookMarket, now time.Time) ([]ArbitrageOpportunity, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil scanner", ErrInvalidArbitrageConfig)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}

	// A finding on a market nobody can bet is noise. CLAUDE.md's brief for this
	// phase is explicit: do not publish a finding that cannot be acted on.
	// Suspended is the interesting case — the synthetic feed suspends a market
	// for a few steps after a steam move, which is exactly when the cross-book
	// numbers look most attractive and exactly when they cannot be struck.
	if !m.Market.AcceptsWagers() {
		return nil, nil
	}

	byID, ordered := m.selectionIndex()

	// best is the best live quote per (line, outcome). Filtering by age happens
	// here, BEFORE the best price is chosen, so a stale outlier can never mask
	// the fresh price underneath it.
	best := make(map[arbLineKey]domain.Price, len(m.Prices))
	lines := make(map[domain.Line]struct{})
	for _, p := range m.Prices {
		sel, ok := byID[p.SelectionID()]
		if !ok {
			// Unreachable: Validate rejected it.
			continue
		}
		if p.Age(now) > s.cfg.MaxLegAge {
			continue
		}
		frame := homeFrameLine(m.Market.Type(), sel.Role(), p.Line())
		k := arbLineKey{line: frame, sel: p.SelectionID()}
		if cur, seen := best[k]; !seen || betterQuote(p, cur) {
			best[k] = p
		}
		lines[frame] = struct{}{}
	}

	candidates := make([]domain.Line, 0, len(lines))
	for l := range lines {
		candidates = append(candidates, l)
	}
	slices.SortFunc(candidates, compareLine)

	var found []ArbitrageOpportunity
	for _, line := range candidates {
		o, ok, err := s.evaluate(m, ordered, best, line, now)
		if err != nil {
			return nil, err
		}
		if ok {
			found = append(found, o)
		}
	}
	return found, nil
}

// arbLineKey addresses one outcome at one line. It is a package-level type
// rather than a local one so that the map can cross a function boundary — two
// identically-shaped struct types declared in different function bodies are not
// the same type, and the map types built from them are not assignable.
type arbLineKey struct {
	line domain.Line
	sel  domain.SelectionID
}

// evaluate tests one line of one market for an arbitrage.
func (s *ArbitrageScanner) evaluate(
	m CrossBookMarket,
	ordered []domain.Selection,
	best map[arbLineKey]domain.Price,
	line domain.Line,
	now time.Time,
) (ArbitrageOpportunity, bool, error) {
	legs := make([]ArbitrageLeg, 0, len(ordered))
	prices := make([]odds.Decimal, 0, len(ordered))
	books := make(map[domain.BookID]struct{}, len(ordered))
	var oldest, newest time.Time

	for _, sel := range ordered {
		p, ok := best[arbLineKey{line: line, sel: sel.ID()}]
		if !ok {
			// This line does not cover every outcome, so its implied
			// probabilities do not sum to anything meaningful. Skip it.
			return ArbitrageOpportunity{}, false, nil
		}
		d, err := odds.NewDecimal(p.Decimal())
		if err != nil {
			// Unreachable: domain.NewPrice enforces the same bounds.
			return ArbitrageOpportunity{}, false, fmt.Errorf(
				"market %s selection %s: %w", m.Market.ID(), sel.ID(), err)
		}
		prices = append(prices, d)
		books[p.BookID()] = struct{}{}
		if oldest.IsZero() || p.ObservedAt().Before(oldest) {
			oldest = p.ObservedAt()
		}
		if newest.IsZero() || p.ObservedAt().After(newest) {
			newest = p.ObservedAt()
		}
		legs = append(legs, ArbitrageLeg{
			SelectionID: sel.ID(),
			Role:        sel.Role(),
			BookID:      p.BookID(),
			Price:       p,
			Age:         p.Age(now),
		})
	}

	margin, err := odds.NewMargin(prices)
	if err != nil {
		return ArbitrageOpportunity{}, false, fmt.Errorf("market %s at line %s: %w", m.Market.ID(), line, err)
	}
	if !margin.IsUnderround() {
		return ArbitrageOpportunity{}, false, nil
	}

	ret := (1 - margin.ImpliedSum) / margin.ImpliedSum
	if ret < s.cfg.MinReturn {
		return ArbitrageOpportunity{}, false, nil
	}
	if len(books) < s.cfg.MinDistinctBooks {
		return ArbitrageOpportunity{}, false, nil
	}
	spread := newest.Sub(oldest)
	if spread > s.cfg.MaxLegSpread {
		return ArbitrageOpportunity{}, false, nil
	}

	for i := range legs {
		q, err := odds.Decimal(legs[i].Price.Decimal()).Probability()
		if err != nil {
			return ArbitrageOpportunity{}, false, fmt.Errorf("market %s leg %d: %w", m.Market.ID(), i, err)
		}
		legs[i].StakeFraction = float64(q) / margin.ImpliedSum
	}

	return ArbitrageOpportunity{
		MarketID:       m.Market.ID(),
		EventID:        m.Market.EventID(),
		MarketType:     m.Market.Type(),
		Line:           line,
		Legs:           legs,
		Margin:         margin,
		Return:         ret,
		DistinctBooks:  len(books),
		ObservedSpread: spread,
		ObservedAt:     oldest,
		OldestLegAge:   now.Sub(oldest),
	}, true, nil
}

// betterQuote reports whether candidate should displace incumbent as an
// outcome's best price.
//
// Longer odds win, because a longer price on the same outcome lowers the implied
// sum and can only improve an arbitrage. Ties are broken by the FRESHER quote
// and then by the lower book identifier — freshness because two identical prices
// are equally profitable and the newer one is likelier to still be there, and
// the identifier because determinism is a requirement: the same input must
// produce the same finding on every run and in every process.
func betterQuote(candidate, incumbent domain.Price) bool {
	if candidate.Decimal() != incumbent.Decimal() {
		return candidate.Decimal() > incumbent.Decimal()
	}
	if !candidate.ObservedAt().Equal(incumbent.ObservedAt()) {
		return candidate.IsNewerThan(incumbent)
	}
	return candidate.BookID() < incumbent.BookID()
}

// compareLine orders lines: absent first, then by value.
func compareLine(a, b domain.Line) int {
	av, aok := a.Value()
	bv, bok := b.Value()
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	}
	return cmp.Compare(av, bv)
}

// compareArbitrage is the deterministic output order: market, then line, then
// the richest finding first.
func compareArbitrage(a, b ArbitrageOpportunity) int {
	if c := cmp.Compare(a.MarketID, b.MarketID); c != 0 {
		return c
	}
	if c := compareLine(a.Line, b.Line); c != 0 {
		return c
	}
	return cmp.Compare(b.Return, a.Return)
}
