// The +EV finder: CLAUDE.md §6's "positive-EV finder against a sharp reference
// book".
//
// # This file computes no odds mathematics, and that is the whole design
//
// internal/pricing already scored every quote on every market against the sharp
// reference book's no-vig fair value, and put ExpectedValue, ExpectedValuePercent,
// Edge, EdgePercent, Kelly and FractionalKelly on each [pricing.QuoteAssessment].
// A second computation of any of them here would be a second answer to a
// question that must have exactly one (CLAUDE.md §10). So this file does four
// things and nothing else:
//
//	FILTER    which scored quotes qualify as a finding
//	RANK      in what order, totally and deterministically
//	THRESHOLD under what declared bounds, recorded on the finding itself
//	SHAPE     into an [EVSignal] the store and the bus both accept
//
// The filters are where the judgement lives, and each of the three is a
// different kind of statement about what a signal is.
//
// # 1. The expected value must clear a floor
//
// [DefaultMinEVPercent] is the bar. It is not zero, and the reason is not
// caution: internal/pricing scores every book against ONE book's devigged
// prices, so a fraction of a percent is inside the noise of the devig itself and
// a floor of zero turns the finder into a list of every soft book's rounding.
//
// # 2. The quote must be fresh, under a bound this package declares
//
// internal/pricing has already disqualified a book whose oldest quote exceeds
// its own MaxQuoteAge (10 minutes by default — two normalizer refresh ceilings,
// chosen so the multi-book comparison stays populated). That bound is right for
// a BOARD and much too loose for a SIGNAL: an expected value against a price
// nobody is still offering reads as an opportunity and is not one. So this file
// applies its own, tighter bound, and — crucially — writes it onto the finding
// as MaxQuoteAgeSeconds rather than leaving a consumer to guess.
//
// AGES ARE MEASURED AT THE SOURCE RECORD'S ANCHOR, NOT AT A CLOCK. The age is
// propagated verbatim from [pricing.QuoteAssessment.AgeSeconds], which
// internal/pricing computed against [pricing.MarketSnapshot.Anchor] — the
// record's own instant. Re-measuring against time.Now here would fold bus and
// consumer lag into the number, so the same market would yield a finding on a
// quiet system and none on a backed-up one, and a replay would produce different
// findings from the original run. A negative age (provider clock skew) passes
// the bound, deliberately: [domain.Price.Age] returns the negative rather than
// clamping so a monitor can see the skew, and clamping it here would hide it.
//
// # 3. The edge must exceed its own error bar
//
// This is the filter that is not obvious and it is the most valuable of the
// three. [pricing.FairValue.Disagreement] is the largest absolute probability
// difference between the devig method that produced the fair value and any other
// method that could also have priced the market, and internal/pricing's own doc
// states the consequence exactly:
//
//	"an EV of 1% on a market where the four methods span 3 percentage points is
//	 not a signal, and a consumer that cannot see the spread cannot tell the
//	 difference."
//
// The two quantities are in different units — Disagreement is probability
// points, expected value is profit per unit staked — so comparing them directly
// would be a category error. The conversion is the derivative: EV = q·d − 1, so
// ∂EV/∂q = d, and a fair-probability uncertainty of D translates into an
// expected-value uncertainty of D·d. The gate is therefore
//
//	ExpectedValue ≥ MinEdgeToErrorBar × Disagreement × OfferedDecimal
//
// with a default ratio of 1: the edge must be at least as large as the error bar
// on the number it was computed from. A longshot at d = 12 needs twelve times
// the edge of an even-money quote to clear the same disagreement, which is
// correct and is exactly the favourite–longshot asymmetry the four devig methods
// disagree about.
//
// Disagreement is negative when it was not computed
// ([pricing.Options.SkipMethodComparison]). The gate then DOES NOT BIND, and the
// finding is counted under a distinct outcome so the population of
// unconstrained findings is visible rather than mixed in.
//
// # Ranking must be total, and it must match the database's index
//
// An unstable sort is a different answer in SQL, so the comparator is total: the
// tuple (ExpectedValuePercent, QuoteObservedAt, SelectionID, BookID) is unique
// within one record, because (selection, book) identifies at most one quote.
//
// ALL FOUR ARE DESCENDING, and that is not a stylistic choice. It is the exact
// order of ev_signals_rank_idx in migrations/00009, which is all-DESC because a
// keyset cursor is a row-value comparison and PostgreSQL plans a mixed-direction
// ORDER BY as an OR-expansion rather than an index range. If this comparator and
// that index disagreed, the top N this package emits would not be the top N the
// API pages through, and nothing would report the difference.
package analytics

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// Defaults for the +EV finder. Each is overridable through [EVConfig]; a zero
// numeric field means the default named here.
const (
	// DefaultMinEVPercent is the expected-value floor, as a percentage of stake.
	//
	// One percent. Below it the finding is inside the devig's own noise: the fair
	// value comes from ONE book's prices through ONE of four models that
	// routinely disagree by a percentage point on an ordinary two-way market, so
	// a 0.3% "edge" is a statement about the choice of model rather than about
	// the market. It is also the level at which the number survives the round
	// trip to a screen: a board that lists a 0.2% edge invites a bet whose
	// expected value is smaller than the price move between seeing it and taking
	// it.
	//
	// The error-bar gate below is the principled filter and this is the blunt
	// one. Both are kept because they fail differently: the error bar is unset on
	// a market where the comparison could not run, and this floor is not.
	DefaultMinEVPercent = 1.0

	// DefaultMaxEVQuoteAge disqualifies a quote from being a signal.
	//
	// Three minutes, against internal/pricing's ten. ADR 0003 buys a 90-second
	// live poll cadence, so three minutes is two polls: a quote that has survived
	// two polls unchanged is a live price, and one that has not been seen for
	// longer has stopped being refreshed rather than stopped moving.
	//
	// It is deliberately tighter than the pricer's board bound. The pricer's job
	// is to keep the multi-book comparison populated, so it tolerates a soft book
	// lagging by several minutes; a SIGNAL is an instruction to go and place a
	// bet, and the bet has to still exist.
	DefaultMaxEVQuoteAge = 3 * time.Minute

	// DefaultMinEdgeToErrorBar is how many multiples of the fair value's own
	// cross-method disagreement the expected value must exceed. See the file
	// comment for the derivation of the units.
	//
	// One: the edge must be at least as large as the uncertainty in the number it
	// was computed from. Larger is defensible and would produce a shorter, higher
	// quality list; smaller is not, because it asserts an edge the arithmetic
	// cannot distinguish from a modelling choice.
	DefaultMinEdgeToErrorBar = 1.0
)

// EVConfig bounds what this package is willing to call a positive-EV finding.
//
// It is a value type with a Validate method rather than a set of package
// constants for the reason CLAUDE.md §12 gives about configuration generally —
// "fail fast and loudly on a bad config" — and for one specific to phase 9:
// every threshold here is written onto every finding it produces, so a
// deployment that changes one leaves a stored population that can still be
// separated into the two regimes. That only works if the value is a piece of
// data rather than a compiled-in constant.
type EVConfig struct {
	// MinEVPercent is the expected-value floor as a PERCENTAGE of stake, not a
	// fraction. Zero means [DefaultMinEVPercent]; explicitly negative is
	// rejected, because a negative floor would make "positive EV" a name rather
	// than a property.
	MinEVPercent float64

	// MaxQuoteAge is the freshness bound. Zero means [DefaultMaxEVQuoteAge].
	MaxQuoteAge time.Duration

	// MinEdgeToErrorBar is the multiple of [pricing.FairValue.Disagreement] ×
	// decimal price that the expected value must clear. Zero means
	// [DefaultMinEdgeToErrorBar]; exactly zero cannot be requested, and that is
	// deliberate — turning the gate off is a decision that should be spelled with
	// a sentinel rather than reached by leaving a field blank. Use
	// [EVConfig.DisableErrorBarGate] to say it out loud.
	MinEdgeToErrorBar float64

	// DisableErrorBarGate turns the error-bar filter off entirely.
	//
	// Negatively named so the zero value leaves the gate ON, which is the right
	// default: a fair probability with no error bar is an opinion presented as a
	// measurement, and a finder that ignores the error bar publishes those.
	DisableErrorBarGate bool

	// KellyFraction is the multiplier that produced
	// [pricing.QuoteAssessment.FractionalKelly], recorded on every finding so the
	// scaling is reproducible.
	//
	// IT IS A CONFIGURATION FACT THE PRICED RECORD DOES NOT CARRY. price.computed
	// publishes the full and fractional stakes but not the ratio between them,
	// and deriving the ratio by division would turn a float64 round-trip into a
	// value that a CHECK constraint on (0, 1] can reject on the last unit in the
	// last place. So it is supplied, and the composition root supplies the same
	// value it built the pricing engine with — see cmd/pricer/main.go, which
	// takes both from [pricing.DefaultKellyMultiplier] so the two cannot drift.
	//
	// Zero means [pricing.DefaultKellyMultiplier]. Outside (0, 1] is rejected.
	KellyFraction float64

	// MaxSignalsPerMarket caps how many findings one record may yield, applied
	// AFTER ranking so the cap keeps the strongest.
	//
	// Zero means no cap, which is the default and is right for this input: one
	// record carries at most books × selections quotes — tens, not thousands —
	// and silently discarding a genuine finding to bound a write that is already
	// bounded would be the wrong trade. The knob exists for a future provider
	// with a forty-book market.
	MaxSignalsPerMarket int
}

// DefaultEVConfig returns the configuration described on each field.
func DefaultEVConfig() EVConfig {
	return EVConfig{
		MinEVPercent:      DefaultMinEVPercent,
		MaxQuoteAge:       DefaultMaxEVQuoteAge,
		MinEdgeToErrorBar: DefaultMinEdgeToErrorBar,
		KellyFraction:     pricing.DefaultKellyMultiplier,
	}
}

// Validate reports a configuration that cannot mean what it says.
func (c EVConfig) Validate() error {
	switch {
	case math.IsNaN(c.MinEVPercent) || math.IsInf(c.MinEVPercent, 0):
		return fmt.Errorf("%w: MinEVPercent %v is not finite", ErrInvalidConfig, c.MinEVPercent)
	case c.MinEVPercent < 0:
		return fmt.Errorf("%w: MinEVPercent %v is negative; a negative floor makes "+
			"\"positive EV\" a name rather than a property", ErrInvalidConfig, c.MinEVPercent)
	case c.MaxQuoteAge < 0:
		return fmt.Errorf("%w: MaxQuoteAge %s is negative", ErrInvalidConfig, c.MaxQuoteAge)
	case math.IsNaN(c.MinEdgeToErrorBar) || math.IsInf(c.MinEdgeToErrorBar, 0):
		return fmt.Errorf("%w: MinEdgeToErrorBar %v is not finite", ErrInvalidConfig, c.MinEdgeToErrorBar)
	case c.MinEdgeToErrorBar < 0:
		return fmt.Errorf("%w: MinEdgeToErrorBar %v is negative; use DisableErrorBarGate to turn "+
			"the gate off deliberately", ErrInvalidConfig, c.MinEdgeToErrorBar)
	case c.KellyFraction < 0 || c.KellyFraction > 1:
		return fmt.Errorf("%w: KellyFraction %v is outside (0, 1]", ErrInvalidConfig, c.KellyFraction)
	case c.MaxSignalsPerMarket < 0:
		return fmt.Errorf("%w: MaxSignalsPerMarket %d is negative", ErrInvalidConfig, c.MaxSignalsPerMarket)
	}
	return nil
}

// resolved returns the configuration with every zero field replaced by its
// documented default. It is called once, in [NewEVFinder], so that the finder
// holds resolved values and every finding records what was actually applied
// rather than the zero the caller happened to pass.
func (c EVConfig) resolved() EVConfig {
	if c.MinEVPercent == 0 {
		c.MinEVPercent = DefaultMinEVPercent
	}
	if c.MaxQuoteAge == 0 {
		c.MaxQuoteAge = DefaultMaxEVQuoteAge
	}
	if c.MinEdgeToErrorBar == 0 {
		c.MinEdgeToErrorBar = DefaultMinEdgeToErrorBar
	}
	if c.KellyFraction == 0 {
		c.KellyFraction = pricing.DefaultKellyMultiplier
	}
	return c
}

// EVReason says why one scored quote did or did not become a finding.
//
// It is a bounded set because it becomes a Prometheus label value, and it is
// exhaustive: every quote on every record leaves [EVFinder.Scan] under exactly
// one of these, so the counters add up to the quotes examined and a discrepancy
// is a bug rather than a rounding artefact.
type EVReason string

// The reasons. Each is written by exactly one branch of assess.
const (
	// EVReasonSignal: the quote cleared every gate and became a finding.
	EVReasonSignal EVReason = "signal"

	// EVReasonNotPriced: internal/pricing did not score the quote at all — the
	// book was stale by ITS bound, or quoted a different line from the reference
	// book, or the arithmetic refused the price. Not a rejection by this file.
	EVReasonNotPriced EVReason = "not_priced"

	// EVReasonNotPositive: the quote was scored and its expected value is at or
	// below zero. The ordinary outcome for almost every quote on almost every
	// market, including every quote from the reference book itself, which cannot
	// beat the fair value devigged from its own prices.
	EVReasonNotPositive EVReason = "not_positive"

	// EVReasonBelowThreshold: positive, but under MinEVPercent.
	EVReasonBelowThreshold EVReason = "below_threshold"

	// EVReasonStale: the quote is older than MaxQuoteAge at the record's anchor.
	EVReasonStale EVReason = "stale"

	// EVReasonInsideErrorBar: the edge does not exceed the fair value's own
	// cross-method disagreement, scaled to expected-value units. See the file
	// comment.
	EVReasonInsideErrorBar EVReason = "inside_error_bar"

	// EVReasonUnbounded: the finding was accepted with the error-bar gate not
	// binding, because the record carries no disagreement measurement. Counted
	// separately from EVReasonSignal is NOT what happens — it IS a signal — but
	// the population is tracked so a board full of unconstrained findings is
	// visible. See [EVScanStats.UnboundedByErrorBar].
	EVReasonUnbounded EVReason = "unbounded"

	// EVReasonOutOfRange: the finding is arithmetically positive but falls
	// outside a bound migrations/00009 enforces — an edge at or above 100%, a
	// price beyond the representable range, a line that contradicts the market
	// type. Dropped and counted rather than written, because the alternative is a
	// constraint violation that fails the whole record's transaction and takes
	// every other finding on it down too.
	EVReasonOutOfRange EVReason = "out_of_range"

	// EVReasonCapped: the finding cleared every gate but fell outside
	// MaxSignalsPerMarket after ranking.
	EVReasonCapped EVReason = "capped"
)

// EVScanStats is one record's outcome, for the metrics.
//
// Examined is the denominator and every other field is a disjoint slice of it
// except UnboundedByErrorBar, which is a property of some of the Signals.
type EVScanStats struct {
	Examined int
	Signals  int
	Reasons  map[EVReason]int

	// UnboundedByErrorBar counts findings accepted while the error-bar gate did
	// not bind, either because the record carried no disagreement measurement or
	// because the gate is disabled. It OVERLAPS Signals rather than partitioning
	// it.
	UnboundedByErrorBar int
}

func (s *EVScanStats) note(r EVReason) {
	if s.Reasons == nil {
		s.Reasons = make(map[EVReason]int, 8)
	}
	s.Reasons[r]++
}

// EVFinder turns one priced market into ranked positive-EV findings.
//
// It is immutable after construction and safe for concurrent use: it holds
// configuration and nothing else. No clock, no cache, no per-market state — the
// input record is a complete description of one market at one instant, which is
// what makes a finding a pure function of it.
type EVFinder struct{ cfg EVConfig }

// NewEVFinder builds a finder. It does no I/O.
func NewEVFinder(cfg EVConfig) (*EVFinder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &EVFinder{cfg: cfg.resolved()}, nil
}

// Config returns the finder's configuration with defaults resolved, so a caller
// can report the bounds a finding was produced under rather than assume them.
func (f *EVFinder) Config() EVConfig { return f.cfg }

// Scan returns every positive-EV finding on one priced market, best first.
//
// detectedAt is stamped onto each finding's DetectedAt and is the ONLY clock
// reading that reaches the output. It is a parameter rather than a field so this
// function stays pure: two calls with the same record and the same instant
// produce byte-identical findings, which is the property the phase-12 comparison
// rests on.
//
// The returned slice is nil when there is nothing to report, which is the
// ordinary and correct state for most markets. An empty +EV board is a
// well-priced board, not a broken detector.
func (f *EVFinder) Scan(rec pricing.ComputedMarket, detectedAt time.Time) ([]EVSignal, EVScanStats) {
	var (
		out   []EVSignal
		stats EVScanStats
	)

	maxAge := f.cfg.MaxQuoteAge.Seconds()

	for _, book := range rec.Books {
		for _, q := range book.Quotes {
			stats.Examined++

			sig, reason, unbounded := f.assess(rec, book, q, maxAge, detectedAt)
			if reason != EVReasonSignal {
				stats.note(reason)
				continue
			}
			stats.note(EVReasonSignal)
			if unbounded {
				stats.UnboundedByErrorBar++
			}
			out = append(out, sig)
		}
	}

	slices.SortFunc(out, compareEVSignals)

	if n := f.cfg.MaxSignalsPerMarket; n > 0 && len(out) > n {
		for range out[n:] {
			stats.note(EVReasonCapped)
		}
		stats.Reasons[EVReasonSignal] -= len(out) - n
		out = out[:n]
	}
	stats.Signals = len(out)
	return out, stats
}

// assess applies the three gates to one scored quote and shapes the finding.
//
// It returns the reason rather than a bare bool so the caller can count WHY a
// quote was refused; a finder whose rejections are invisible is a finder nobody
// can tune.
func (f *EVFinder) assess(
	rec pricing.ComputedMarket,
	book pricing.BookAssessment,
	q pricing.QuoteAssessment,
	maxAgeSeconds float64,
	detectedAt time.Time,
) (EVSignal, EVReason, bool) {
	// internal/pricing's own verdict comes first and is never second-guessed. A
	// quote it did not score has no expected value to threshold: the fields are
	// zero, and treating a zero as "no edge" rather than "no measurement" is the
	// difference between an honest counter and a flattering one.
	if q.Status != pricing.QuoteStatusPriced {
		return EVSignal{}, EVReasonNotPriced, false
	}
	if q.ExpectedValue <= 0 {
		return EVSignal{}, EVReasonNotPositive, false
	}
	if q.ExpectedValuePercent < f.cfg.MinEVPercent {
		return EVSignal{}, EVReasonBelowThreshold, false
	}
	// A NEGATIVE age passes: provider clock skew is reported rather than clamped
	// everywhere else in this system and clamping it here would hide it.
	if q.AgeSeconds > maxAgeSeconds {
		return EVSignal{}, EVReasonStale, false
	}

	unbounded := f.cfg.DisableErrorBarGate || rec.Fair.Disagreement < 0
	if !unbounded {
		// Units: Disagreement is probability points, ExpectedValue is profit per
		// unit staked. ∂EV/∂q = d, so the error bar in EV units is
		// Disagreement × d. See the file comment for why this conversion and not
		// a direct comparison.
		errorBar := rec.Fair.Disagreement * float64(q.Decimal)
		if q.ExpectedValue < f.cfg.MinEdgeToErrorBar*errorBar {
			return EVSignal{}, EVReasonInsideErrorBar, false
		}
	}

	sig := EVSignal{
		SchemaVersion:        SchemaVersion,
		SelectionID:          q.SelectionID,
		MarketID:             domain.MarketID(rec.Market.ID),
		MarketType:           rec.Market.Type,
		LeagueID:             domain.LeagueID(rec.League.ID),
		BookID:               book.BookID,
		ReferenceBookID:      rec.Reference.BookID,
		DevigMethod:          rec.Fair.Method.String(),
		OfferedDecimal:       q.Decimal,
		OfferedImplied:       float64(q.Implied),
		Line:                 q.Line,
		FairProbability:      float64(q.FairProbability),
		FairDecimal:          q.FairDecimal,
		ExpectedValue:        q.ExpectedValue,
		ExpectedValuePercent: q.ExpectedValuePercent,
		Edge:                 q.Edge,
		EdgePercent:          q.EdgePercent,
		Kelly:                q.Kelly,
		FractionalKelly:      q.FractionalKelly,
		KellyFraction:        f.cfg.KellyFraction,
		QuoteObservedAt:      q.ObservedAt,
		QuoteAgeSeconds:      q.AgeSeconds,
		ThresholdEVPercent:   f.cfg.MinEVPercent,
		MaxQuoteAgeSeconds:   maxAgeSeconds,
		DetectedAt:           detectedAt,
	}
	if err := sig.validate(); err != nil {
		return EVSignal{}, EVReasonOutOfRange, false
	}
	return sig, EVReasonSignal, unbounded
}

// validate mirrors the CHECK constraints migrations/00009 puts on ev_signals.
// validate.go carries the argument for why the rules are restated in Go at all,
// and for why the two sets must be kept in step.
func (s EVSignal) validate() error {
	switch {
	case !finite(float64(s.OfferedDecimal), s.OfferedImplied, s.FairProbability, float64(s.FairDecimal),
		s.ExpectedValue, s.ExpectedValuePercent, s.Edge, s.EdgePercent,
		s.Kelly, s.FractionalKelly, s.KellyFraction, s.QuoteAgeSeconds):
		return fmt.Errorf("%w: a value on the finding is not finite", ErrInvalidConfig)
	case s.OfferedDecimal <= 1 || s.OfferedDecimal > maxDecimalOdds:
		return fmt.Errorf("%w: offered decimal %v outside (1, %v]",
			ErrInvalidConfig, s.OfferedDecimal, maxDecimalOdds)
	case s.FairDecimal <= 1 || s.FairDecimal > maxDecimalOdds:
		return fmt.Errorf("%w: fair decimal %v outside (1, %v]",
			ErrInvalidConfig, s.FairDecimal, maxDecimalOdds)
	case s.OfferedImplied <= 0 || s.OfferedImplied >= 1:
		return fmt.Errorf("%w: offered implied %v outside (0, 1)", ErrInvalidConfig, s.OfferedImplied)
	case s.FairProbability <= 0 || s.FairProbability >= 1:
		return fmt.Errorf("%w: fair probability %v outside (0, 1)", ErrInvalidConfig, s.FairProbability)
	case s.ExpectedValue <= 0 || s.ExpectedValuePercent <= 0:
		return fmt.Errorf("%w: expected value %v is not positive", ErrInvalidConfig, s.ExpectedValue)
	case s.Edge <= 0 || s.Edge >= 1:
		return fmt.Errorf("%w: edge %v outside (0, 1)", ErrInvalidConfig, s.Edge)
	case s.EdgePercent <= 0 || s.EdgePercent >= 100:
		return fmt.Errorf("%w: edge percent %v outside (0, 100)", ErrInvalidConfig, s.EdgePercent)
	case s.Kelly <= 0 || s.Kelly > 1:
		return fmt.Errorf("%w: kelly %v outside (0, 1]", ErrInvalidConfig, s.Kelly)
	case s.FractionalKelly <= 0 || s.FractionalKelly > s.Kelly:
		return fmt.Errorf("%w: fractional kelly %v outside (0, kelly=%v]",
			ErrInvalidConfig, s.FractionalKelly, s.Kelly)
	case s.KellyFraction <= 0 || s.KellyFraction > 1:
		return fmt.Errorf("%w: kelly fraction %v outside (0, 1]", ErrInvalidConfig, s.KellyFraction)
	case s.ExpectedValuePercent < s.ThresholdEVPercent:
		return fmt.Errorf("%w: expected value %v%% is below the threshold %v%% it claims to meet",
			ErrInvalidConfig, s.ExpectedValuePercent, s.ThresholdEVPercent)
	case s.MaxQuoteAgeSeconds <= 0:
		return fmt.Errorf("%w: max quote age %v is not positive", ErrInvalidConfig, s.MaxQuoteAgeSeconds)
	}
	return lineRule(s.MarketType, s.Line)
}

// compareEVSignals is the ranking, and it is TOTAL.
//
// The tuple is (ExpectedValuePercent, QuoteObservedAt, SelectionID, BookID), all
// DESCENDING, mirroring ev_signals_rank_idx in migrations/00009 exactly. The
// last two make it total within one record because (selection, book) identifies
// at most one quote, so no two findings can compare equal and the sort is
// insensitive to the order the books and quotes happened to arrive in.
//
// The direction of the tie-breakers is not cosmetic: the index is all-DESC
// because a keyset cursor is a row-value comparison and PostgreSQL will not plan
// a mixed-direction ORDER BY as one index range. Ranking differently here would
// mean this package's "top five" and the API's first page were different sets.
func compareEVSignals(a, b EVSignal) int {
	if c := cmp.Compare(b.ExpectedValuePercent, a.ExpectedValuePercent); c != 0 {
		return c
	}
	if c := b.QuoteObservedAt.Compare(a.QuoteObservedAt); c != 0 {
		return c
	}
	if c := cmp.Compare(b.SelectionID, a.SelectionID); c != 0 {
		return c
	}
	return cmp.Compare(b.BookID, a.BookID)
}
