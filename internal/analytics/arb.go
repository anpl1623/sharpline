// The arbitrage surface: turning internal/pricing's findings into signals a
// consumer can act on, under a staleness discipline this package declares.
//
// # Nothing here detects an arbitrage
//
// internal/pricing/arbitrage.go does that, on every priced market, and publishes
// the result as [pricing.ArbitrageRef] on price.computed. It groups quotes by
// per-book line so a −2.5 is never netted against a −3.5, it requires the
// market's COMPLETE outcome set so a two-of-three-way sum cannot masquerade as
// an edge, and it routes the summation through [odds.NewMargin] so no two call
// sites can disagree about S. Re-detecting here would be a second answer to a
// question that must have one.
//
// What this file adds is the part §11's phase 9 asks for and that a detector
// cannot supply for itself: a declared staleness bound, a stable identity, a
// total ranking, and a shape both sinks accept.
//
// # THE STALENESS DISCIPLINE IS THE POINT OF THIS FILE
//
// The phase-4 gate measured 68 live arbitrages across 1,065 records with the
// leg-age bound binding constantly. That is not an artefact of the synthetic
// feed; it is what cross-book arbitrage IS. Books do not disagree by half a
// percent on a market they are both currently quoting — they disagree because
// one of them has not moved yet, and the price that makes the sum come out under
// 1 is a price that will be gone before a stake reaches it. A firehose of stale
// findings is strictly worse than no finder at all, because it teaches whoever
// reads the board that the board is wrong.
//
// So three things happen here, and all three are deliberate:
//
//  1. A TIGHTER BOUND THAN THE DETECTOR'S. internal/pricing's scanner defaults
//     to a 120-second leg age and a 30-second spread, sized so the scanner has
//     something to look at on a board polled every 90 seconds. A SIGNAL is an
//     instruction to go and place a bet, so it gets the tighter of the two.
//  2. THE BOUND TRAVELS ON THE FINDING. MaxLegAgeSeconds and
//     MaxObservedSpreadSeconds are columns, not configuration a reader has to go
//     and look up, and migrations/00009 CHECKs that the finding actually meets
//     them — `oldest_leg_age_seconds <= max_leg_age_seconds AND
//     observed_spread_seconds <= max_observed_spread_seconds` — which makes the
//     discipline a database fact rather than a claim in a comment.
//  3. THE EVIDENCE TRAVELS TOO. OldestLegAgeSeconds and ObservedSpreadSeconds
//     are on every finding and must be rendered wherever it is. A consumer that
//     can see "0.9% return, oldest leg 74 seconds old, legs 61 seconds apart"
//     can judge it; one that sees only "0.9%" cannot.
//
// # A single-book finding is kept, and is the STRONGER one
//
// [DefaultMinDistinctBooks] is 1. That looks like the loose setting and is the
// opposite: a book whose own market sums under 1 has no cross-book staleness
// explanation available to it at all — there is one book, one refresh, one
// instant — so the finding is either a genuine mispriced market or a bug in the
// book. odds/vig.go names under-round as "a feature (CLAUDE.md §6), not an error
// to be swallowed", and dropping the single-book case would swallow exactly the
// finding with the fewest ways to be wrong.
//
// # Ages are measured at the record's anchor, never at a clock
//
// Every age and spread here is propagated verbatim from the [pricing.ArbitrageRef],
// which internal/pricing measured against [pricing.MarketSnapshot.Anchor] — the
// source record's own instant. Re-measuring at time.Now would fold bus and
// consumer lag into the number, so the same market would yield a finding on a
// quiet system and none on a backed-up one, and a replay would disagree with the
// original run.
package analytics

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// Defaults for the arbitrage surface.
const (
	// DefaultMaxArbLegAge is the oldest a leg may be for the finding to be
	// reported, measured at the source record's anchor.
	//
	// Sixty seconds, against internal/pricing's 120. ADR 0003 buys a 90-second
	// live poll cadence, so this is less than one poll: every leg must have been
	// seen since the previous sweep. A leg older than that belongs to a book that
	// missed a refresh, and a book that missed a refresh is the single commonest
	// cause of a phantom arbitrage.
	DefaultMaxArbLegAge = 60 * time.Second

	// DefaultMaxArbSpread is the largest permitted gap between the oldest and the
	// newest leg.
	//
	// Twenty seconds, against internal/pricing's 30. The spread is the sharper of
	// the two staleness measurements: two legs a minute apart describe two
	// different markets even when both are individually fresh, and an
	// "arbitrage" assembled across them is an arbitrage against the passage of
	// time rather than against a book.
	DefaultMaxArbSpread = 20 * time.Second

	// DefaultMinArbReturn is the smallest guaranteed return fraction worth
	// reporting: 0.005, half a percent of the total outlay.
	//
	// internal/pricing's scanner reports down to 0.001 because a detector should
	// find what is there. A SIGNAL has to survive contact with reality: half a
	// percent is roughly the width of one tick on a soft book quoting on a
	// 10-cent American grid, so below it the finding is inside the granularity
	// the price was quoted at and would be erased by a single tick moving
	// against it.
	DefaultMinArbReturn = 0.005

	// DefaultMinDistinctBooks is 1. See the file comment: the single-book case is
	// the stronger finding, not the weaker one.
	DefaultMinDistinctBooks = 1
)

// fingerprintSep separates the fields of one leg and one leg from the next
// inside the digest. It is 0x1f (ASCII unit separator), a byte no identifier,
// role or formatted float can contain, so ("ab","c") and ("a","bc") cannot
// digest alike. internal/pricing's ConfigDigest uses the same byte for the same
// reason.
const fingerprintSep = 0x1f

// fingerprintNoLine is what an ABSENT line contributes to the digest.
//
// It has to be a value rather than an omission: skipping the field for a
// moneyline would make a moneyline leg and a spread leg at some line collide in
// principle, and — more practically — would make the Flink SQL reproduction
// depend on how the SQL engine renders a NULL inside a concatenation, which is
// exactly the kind of cross-language ambiguity the digest exists to avoid. A
// hyphen is chosen because strconv.FormatFloat can never produce it alone.
const fingerprintNoLine = "-"

// ArbConfig bounds what this package is willing to report as an arbitrage
// SIGNAL, on top of what internal/pricing was willing to detect.
type ArbConfig struct {
	// MaxLegAge is the freshness bound on the stalest leg. Zero means
	// [DefaultMaxArbLegAge].
	MaxLegAge time.Duration

	// MaxObservedSpread is the bound on the gap between the oldest and newest
	// leg. Zero means [DefaultMaxArbSpread].
	//
	// It must not exceed MaxLegAge: a spread wider than the age bound can never
	// bind, because every leg is inside the age bound and therefore inside that
	// span of each other. internal/pricing's ArbitrageConfig refuses the same
	// combination for the same reason.
	MaxObservedSpread time.Duration

	// MinReturn is the smallest guaranteed return fraction worth reporting, as a
	// FRACTION of total outlay and not a percentage. Zero means
	// [DefaultMinArbReturn].
	MinReturn float64

	// MinDistinctBooks is how many different books a finding must span. Zero
	// means [DefaultMinDistinctBooks], which is 1 — see the file comment.
	MinDistinctBooks int
}

// DefaultArbConfig returns the configuration described on each field.
func DefaultArbConfig() ArbConfig {
	return ArbConfig{
		MaxLegAge:         DefaultMaxArbLegAge,
		MaxObservedSpread: DefaultMaxArbSpread,
		MinReturn:         DefaultMinArbReturn,
		MinDistinctBooks:  DefaultMinDistinctBooks,
	}
}

// Validate reports a configuration that cannot mean what it says.
func (c ArbConfig) Validate() error {
	switch {
	case c.MaxLegAge < 0:
		return fmt.Errorf("%w: MaxLegAge %s is negative", ErrInvalidConfig, c.MaxLegAge)
	case c.MaxObservedSpread < 0:
		return fmt.Errorf("%w: MaxObservedSpread %s is negative", ErrInvalidConfig, c.MaxObservedSpread)
	case c.MaxLegAge > 0 && c.MaxObservedSpread > c.MaxLegAge:
		return fmt.Errorf("%w: MaxObservedSpread %s exceeds MaxLegAge %s, so it can never bind",
			ErrInvalidConfig, c.MaxObservedSpread, c.MaxLegAge)
	case math.IsNaN(c.MinReturn) || math.IsInf(c.MinReturn, 0):
		return fmt.Errorf("%w: MinReturn %v is not finite", ErrInvalidConfig, c.MinReturn)
	case c.MinReturn < 0:
		return fmt.Errorf("%w: MinReturn %v is negative; a finding with a non-positive return is "+
			"not an arbitrage", ErrInvalidConfig, c.MinReturn)
	case c.MinDistinctBooks < 0:
		return fmt.Errorf("%w: MinDistinctBooks %d is negative", ErrInvalidConfig, c.MinDistinctBooks)
	}
	return nil
}

// resolved returns the configuration with every zero field replaced by its
// documented default.
func (c ArbConfig) resolved() ArbConfig {
	if c.MaxLegAge == 0 {
		c.MaxLegAge = DefaultMaxArbLegAge
	}
	if c.MaxObservedSpread == 0 {
		c.MaxObservedSpread = DefaultMaxArbSpread
	}
	if c.MinReturn == 0 {
		c.MinReturn = DefaultMinArbReturn
	}
	if c.MinDistinctBooks == 0 {
		c.MinDistinctBooks = DefaultMinDistinctBooks
	}
	return c
}

// ArbReason says why one detected arbitrage did or did not become a signal.
// Bounded set: it becomes a Prometheus label value.
type ArbReason string

// The reasons. Each is written by exactly one branch of assessArb.
const (
	// ArbReasonSignal: the finding cleared every bound and was reported.
	ArbReasonSignal ArbReason = "signal"

	// ArbReasonStaleLeg: the stalest leg is older than MaxLegAge. THE COMMON
	// OUTCOME, and the whole reason the bound exists — see the file comment.
	ArbReasonStaleLeg ArbReason = "stale_leg"

	// ArbReasonWideSpread: the legs were observed too far apart, so they describe
	// different instants of the market rather than one.
	ArbReasonWideSpread ArbReason = "wide_spread"

	// ArbReasonThinReturn: the guaranteed return is below MinReturn — inside the
	// granularity the prices were quoted at.
	ArbReasonThinReturn ArbReason = "thin_return"

	// ArbReasonTooFewBooks: fewer distinct books than MinDistinctBooks. Not
	// reachable at the default, which is 1.
	ArbReasonTooFewBooks ArbReason = "too_few_books"

	// ArbReasonOutOfRange: the finding falls outside a bound migrations/00009
	// enforces — an implied sum at or above 1, a non-finite return, a line that
	// contradicts the market type, a leg count outside [2, 64]. Dropped and
	// counted rather than written; validate.go explains why the check is here.
	ArbReasonOutOfRange ArbReason = "out_of_range"
)

// ArbScanStats is one record's outcome, for the metrics. Examined is the
// denominator and the reasons partition it.
type ArbScanStats struct {
	Examined int
	Signals  int
	Reasons  map[ArbReason]int
}

func (s *ArbScanStats) note(r ArbReason) {
	if s.Reasons == nil {
		s.Reasons = make(map[ArbReason]int, 6)
	}
	s.Reasons[r]++
}

// ArbSurface turns the arbitrage findings on one priced market into signals.
//
// Immutable after construction and safe for concurrent use: configuration and
// nothing else. No clock, no cache, no per-market state.
type ArbSurface struct{ cfg ArbConfig }

// NewArbSurface builds the surface. It does no I/O.
func NewArbSurface(cfg ArbConfig) (*ArbSurface, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &ArbSurface{cfg: cfg.resolved()}, nil
}

// Config returns the configuration with defaults resolved.
func (s *ArbSurface) Config() ArbConfig { return s.cfg }

// Scan returns every arbitrage on one priced market that survives the staleness
// discipline, best return first.
//
// detectedAt is stamped onto each finding's DetectedAt and is the only clock
// reading that reaches the output, so two calls with the same record and the
// same instant produce byte-identical findings.
//
// A nil result is the ordinary state. A feed with a constant arbitrage on it is
// a feed with a bug, and internal/pricing says so in as many words.
func (s *ArbSurface) Scan(rec pricing.ComputedMarket, detectedAt time.Time) ([]ArbitrageSignal, ArbScanStats) {
	var (
		out   []ArbitrageSignal
		stats ArbScanStats
	)

	for _, ref := range rec.Arbitrage {
		stats.Examined++
		sig, reason := s.assess(rec, ref, detectedAt)
		stats.note(reason)
		if reason == ArbReasonSignal {
			out = append(out, sig)
		}
	}

	slices.SortFunc(out, compareArbitrageSignals)
	stats.Signals = len(out)
	return out, stats
}

// assess applies the bounds to one detected arbitrage and shapes the finding.
//
// The order of the gates is the order of their cost to a reader: staleness
// first, because that is the one that fires and the one whose count is the
// interesting number; the return floor after, because it is the one an operator
// tunes.
func (s *ArbSurface) assess(
	rec pricing.ComputedMarket, ref pricing.ArbitrageRef, detectedAt time.Time,
) (ArbitrageSignal, ArbReason) {
	maxAge := s.cfg.MaxLegAge.Seconds()
	maxSpread := s.cfg.MaxObservedSpread.Seconds()

	// A NEGATIVE age passes, for the reason every other age comparison in this
	// package lets one pass: provider clock skew is reported rather than clamped,
	// and a leg stamped in the future is a monitoring problem rather than a stale
	// price.
	if ref.OldestLegAgeSeconds > maxAge {
		return ArbitrageSignal{}, ArbReasonStaleLeg
	}
	if ref.ObservedSpreadSeconds > maxSpread {
		return ArbitrageSignal{}, ArbReasonWideSpread
	}
	if ref.Return < s.cfg.MinReturn {
		return ArbitrageSignal{}, ArbReasonThinReturn
	}
	if ref.DistinctBooks < s.cfg.MinDistinctBooks {
		return ArbitrageSignal{}, ArbReasonTooFewBooks
	}

	legs := make([]ArbitrageSignalLeg, 0, len(ref.Legs))
	for i, l := range ref.Legs {
		legs = append(legs, ArbitrageSignalLeg{
			LegIndex:      i,
			SelectionID:   l.SelectionID,
			Role:          l.Role,
			BookID:        l.BookID,
			DecimalOdds:   odds.Decimal(l.Decimal),
			Line:          l.Line,
			StakeFraction: l.StakeFraction,
			ObservedAt:    l.ObservedAt,
			AgeSeconds:    l.AgeSeconds,
		})
	}

	sig := ArbitrageSignal{
		SchemaVersion:            SchemaVersion,
		MarketID:                 domain.MarketID(rec.Market.ID),
		MarketType:               rec.Market.Type,
		LeagueID:                 domain.LeagueID(rec.League.ID),
		Line:                     ref.Line,
		SelectionCount:           len(ref.Legs),
		ImpliedSum:               ref.Margin.ImpliedSum,
		ReturnFraction:           ref.Return,
		DistinctBooks:            ref.DistinctBooks,
		ObservedSpreadSeconds:    ref.ObservedSpreadSeconds,
		OldestLegAgeSeconds:      ref.OldestLegAgeSeconds,
		ObservedAt:               ref.ObservedAt,
		LegsFingerprint:          FingerprintArbitrageLegs(legs),
		MaxLegAgeSeconds:         maxAge,
		MaxObservedSpreadSeconds: maxSpread,
		DetectedAt:               detectedAt,
		Legs:                     legs,
	}
	if err := sig.validate(); err != nil {
		return ArbitrageSignal{}, ArbReasonOutOfRange
	}
	return sig, ArbReasonSignal
}

// FingerprintArbitrageLegs is the finding's stable identity.
//
// # This function is a CROSS-LANGUAGE CONTRACT. Phase 12 must reproduce it.
//
// (market_id, observed_at) is not unique: one market quoted by several books can
// yield more than one under-round combination at a single instant, and
// migrations/00009 therefore keys arbitrage_signals on
// (market_id, observed_at, legs_fingerprint). Get this function wrong in one
// direction and a replay duplicates every finding; wrong in the other and two
// genuinely different findings collapse into one and the second silently
// overwrites the first.
//
// The definition, exactly:
//
//  1. THE INPUT IS THE LEG SET AND NOTHING ELSE — for each leg, its
//     selection_id, book_id, decimal odds and line. Not the return, not the
//     implied sum, not the ages: those are consequences of the legs, and folding
//     a derived value in would make a recomputation that fixed a rounding bug
//     produce a NEW finding rather than a correction to the old one.
//  2. NO CLOCK, NO RANDOM, NO DETECTOR VERSION. The digest must be identical
//     across processes, restarts, languages and years.
//  3. LEGS ARE SORTED BY selection_id BEFORE HASHING. Go map iteration order and
//     a SQL engine's arbitrary collect order are both unspecified, so the sort is
//     what makes the two agree. selection_id is unique within a finding —
//     migrations/00009 declares UNIQUE (signal_id, selection_id) — so the sort is
//     total and needs no tie-break.
//  4. FLOATS ARE FORMATTED WITH strconv.FormatFloat(v, 'g', -1, 64), the shortest
//     representation that round-trips a float64 exactly. A fixed number of
//     decimal places would collapse two distinguishable prices; %v is defined in
//     terms of this same format but is a documentation reference rather than a
//     guarantee.
//  5. AN ABSENT LINE CONTRIBUTES [fingerprintNoLine], never an empty field.
//  6. FIELDS AND LEGS ARE SEPARATED BY 0x1f, a byte no identifier or formatted
//     float can contain.
//
// SHA-256, hex, lowercase, all 64 characters. The column's CHECK admits 16 to 64
// and this uses the full width: the digest is an identity, collisions in it are
// silent data loss, and there is nothing to save by truncating a value that is
// written once per finding. FNV-64a would be enough for a change detector —
// internal/pricing's ConfigDigest uses it — but this is a KEY, and a key wants
// the margin.
func FingerprintArbitrageLegs(legs []ArbitrageSignalLeg) string {
	ordered := make([]ArbitrageSignalLeg, len(legs))
	copy(ordered, legs)
	slices.SortFunc(ordered, func(a, b ArbitrageSignalLeg) int {
		return cmp.Compare(a.SelectionID, b.SelectionID)
	})

	h := sha256.New()
	put := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{fingerprintSep})
	}
	for _, l := range ordered {
		put(string(l.SelectionID))
		put(string(l.BookID))
		put(strconv.FormatFloat(float64(l.DecimalOdds), 'g', -1, 64))
		if v, ok := l.Line.Value(); ok {
			put(strconv.FormatFloat(v, 'g', -1, 64))
		} else {
			put(fingerprintNoLine)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// validate mirrors the CHECK constraints migrations/00009 puts on
// arbitrage_signals and arbitrage_signal_legs. See validate.go.
func (s ArbitrageSignal) validate() error {
	switch {
	case !finite(s.ImpliedSum, s.ReturnFraction, s.ObservedSpreadSeconds, s.OldestLegAgeSeconds,
		s.MaxLegAgeSeconds, s.MaxObservedSpreadSeconds):
		return fmt.Errorf("%w: a value on the finding is not finite", ErrInvalidConfig)
	case s.SelectionCount < minArbLegs || s.SelectionCount > maxArbLegs:
		return fmt.Errorf("%w: %d selections, the table admits [%d, %d]",
			ErrInvalidConfig, s.SelectionCount, minArbLegs, maxArbLegs)
	case len(s.Legs) != s.SelectionCount:
		return fmt.Errorf("%w: %d legs for %d selections; an arbitrage covers every outcome exactly once",
			ErrInvalidConfig, len(s.Legs), s.SelectionCount)
	case s.ImpliedSum <= 0 || s.ImpliedSum >= 1:
		return fmt.Errorf("%w: implied sum %v is not under-round", ErrInvalidConfig, s.ImpliedSum)
	case s.ReturnFraction <= 0:
		return fmt.Errorf("%w: return %v is not positive", ErrInvalidConfig, s.ReturnFraction)
	case s.DistinctBooks < 1 || s.DistinctBooks > s.SelectionCount:
		return fmt.Errorf("%w: %d distinct books across %d legs",
			ErrInvalidConfig, s.DistinctBooks, s.SelectionCount)
	case s.ObservedSpreadSeconds < 0:
		return fmt.Errorf("%w: observed spread %v is negative", ErrInvalidConfig, s.ObservedSpreadSeconds)
	case s.MaxLegAgeSeconds <= 0 || s.MaxObservedSpreadSeconds <= 0:
		return fmt.Errorf("%w: a staleness bound on the finding is not positive", ErrInvalidConfig)
	case s.OldestLegAgeSeconds > s.MaxLegAgeSeconds:
		return fmt.Errorf("%w: oldest leg %vs exceeds the bound %vs it claims to meet",
			ErrInvalidConfig, s.OldestLegAgeSeconds, s.MaxLegAgeSeconds)
	case s.ObservedSpreadSeconds > s.MaxObservedSpreadSeconds:
		return fmt.Errorf("%w: spread %vs exceeds the bound %vs it claims to meet",
			ErrInvalidConfig, s.ObservedSpreadSeconds, s.MaxObservedSpreadSeconds)
	}
	if err := lineRule(s.MarketType, s.Line); err != nil {
		return err
	}
	seen := make(map[domain.SelectionID]struct{}, len(s.Legs))
	for i, l := range s.Legs {
		if l.LegIndex != i {
			return fmt.Errorf("%w: leg %d carries index %d; the index is half the primary key",
				ErrInvalidConfig, i, l.LegIndex)
		}
		if _, dup := seen[l.SelectionID]; dup {
			return fmt.Errorf("%w: selection %s appears on two legs", ErrInvalidConfig, l.SelectionID)
		}
		seen[l.SelectionID] = struct{}{}
		if err := l.validate(s.MarketType); err != nil {
			return fmt.Errorf("leg %d: %w", i, err)
		}
	}
	return nil
}

// minArbLegs and maxArbLegs are the bounds migrations/00009 puts on
// arbitrage_signals.selection_count and on the leg index.
//
// Two because a market with one outcome is not a market. Sixty-four because the
// leg index column is CHECKed to [0, 63], which is generous for a futures field
// and small enough that a runaway grouping is refused rather than stored.
const (
	minArbLegs = 2
	maxArbLegs = 64
)

// validate mirrors the leg table's CHECK constraints.
func (l ArbitrageSignalLeg) validate(marketType string) error {
	switch {
	case !finite(float64(l.DecimalOdds), l.StakeFraction, l.AgeSeconds):
		return fmt.Errorf("%w: a value on the leg is not finite", ErrInvalidConfig)
	case l.LegIndex < 0 || l.LegIndex >= maxArbLegs:
		return fmt.Errorf("%w: leg index %d outside [0, %d)", ErrInvalidConfig, l.LegIndex, maxArbLegs)
	case l.DecimalOdds <= 1 || float64(l.DecimalOdds) > maxDecimalOdds:
		return fmt.Errorf("%w: decimal odds %v outside (1, %v]",
			ErrInvalidConfig, l.DecimalOdds, maxDecimalOdds)
	case l.StakeFraction <= 0 || l.StakeFraction >= 1:
		return fmt.Errorf("%w: stake fraction %v outside (0, 1)", ErrInvalidConfig, l.StakeFraction)
	case !legRoleValid(l.Role):
		return fmt.Errorf("%w: role %q is not one of the six", ErrInvalidConfig, l.Role)
	}
	// The LEG's line is in the SELECTION frame, so an away spread carries the
	// inverted number and a total's under side still carries a positive
	// threshold. The rule is therefore the same one the parent obeys, applied to
	// a different frame — which is why it is checked twice with the same
	// function rather than assumed to follow from the parent's check.
	return lineRule(marketType, l.Line)
}

// legRoleValid reports whether a leg's role is one migrations/00009 admits.
//
// The six are the roles [domain.SelectionRole] defines, and the check is against
// the string form because that is what crosses the wire and lands in the column.
// Parsing it back into the domain type and comparing would be the same test with
// an extra failure mode.
func legRoleValid(role string) bool {
	switch role {
	case "home", "away", "draw", "over", "under", "outright":
		return true
	default:
		return false
	}
}

// compareArbitrageSignals is the ranking, and it is TOTAL.
//
// (ReturnFraction, ObservedAt, LegsFingerprint), all DESCENDING, mirroring
// arbitrage_signals_rank_idx in migrations/00009 — which sorts on
// (return_fraction DESC, observed_at DESC, id DESC).
//
// The index's last column is the row's surrogate UUID, which this package cannot
// predict because the database generates it. The fingerprint stands in for it
// and does the same job: it is a deterministic function of the finding, it is
// unique within (market, observed_at) by the same argument that makes it a key,
// and it makes the sort total so that two findings with an identical return at
// an identical instant have a defined order rather than whichever the sort
// happened to leave first. The two orders can differ from each other in that last
// position; neither is unstable, which is the property that matters.
func compareArbitrageSignals(a, b ArbitrageSignal) int {
	if c := cmp.Compare(b.ReturnFraction, a.ReturnFraction); c != 0 {
		return c
	}
	if c := b.ObservedAt.Compare(a.ObservedAt); c != 0 {
		return c
	}
	return cmp.Compare(b.LegsFingerprint, a.LegsFingerprint)
}
