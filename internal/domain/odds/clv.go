package odds

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// -----------------------------------------------------------------------------
// Closing Line Value — semantics, and the cross-language contract
// -----------------------------------------------------------------------------
//
// Closing Line Value (CLV) answers exactly one question: was the price you took
// better than the price the market settled on immediately before the event
// started? It is the best available predictor of long-run betting skill, which
// is why CLAUDE.md §6 makes "CLV tracking per user" a differentiator and ranks
// the public leaderboard "on ROI and CLV, not raw profit". Raw profit over a few
// hundred wagers is mostly variance; CLV is not, because it is scored against
// the market's own final estimate rather than against the scoreboard.
//
// # Everything here is computed on FAIR (no-vig) prices
//
// A quoted price contains two things: the market's estimate of the outcome's
// probability, and the book's margin. CLV is a claim about the first. Comparing
// raw quoted prices therefore conflates a genuine line move with a change in the
// book's margin, and that is the classic error this file exists to prevent.
//
// The worked example, using the two standard American juice prices:
//
//	taken   home -110 / away -110   implied 0.5238095 each, Σ = 1.0476190 (4.545% hold)
//	closing home -105 / away -105   implied 0.5121951 each, Σ = 1.0243902 (2.381% hold)
//
// The market's estimate did not move at all: devigged, both snapshots are
// 0.5 / 0.5, so the true CLV is exactly zero. Compare the raw decimals and you
// get (21/11)/(41/21) − 1 = −441/451 + 1 ≈ −2.2173%, a confident report that the
// bettor lost 2.2 points of value on a line that never moved. Only the book's
// margin changed. That number is not CLV.
//
// This file does not implement devigging — the multiplicative, additive, power
// and Shin methods live in devig.go (CLAUDE.md §4) and exactly one
// implementation of each formula may exist. What this file does is make it
// impossible to hand CLV a vigged input: the only way to construct a
// FairMarketSnapshot is NewFairMarketSnapshot, which requires the COMPLETE set
// of selections for one market and rejects any set whose probabilities do not
// sum to 1 within CLVDevigTolerance. A vigged book cannot pass that check — the
// tightest overround any real book posts is on the order of 1e-2, seven orders
// of magnitude above the tolerance.
//
// The intended pipeline is therefore
//
//	DevigPrices(method, quotedPrices) → DevigResult.Probabilities
//	                                  → NewFairMarketSnapshot → EvaluateCLV
//
// and clv_test.go asserts that all four methods' output clears the gate on both
// two- and three-way markets. Which method produced the numbers is the caller's
// decision and belongs in the caller's audit record: the four disagree
// meaningfully on longshots, so a leaderboard must fix one method and say which.
//
// # The measures
//
// Let p_t be the fair probability implied by the price taken, p_c the fair
// probability implied by the closing price, and d = 1/p the corresponding fair
// decimal price. Then:
//
//	ProbabilityCLV = p_c − p_t                    probability points
//	PercentCLV     = (d_t / d_c − 1) × 100        percentage points of return
//	Beat           = ProbabilityCLV > CLVTieBand  boolean
//	Magnitude      = |PercentCLV|                 percentage points, unsigned
//
// The two scalar measures are monotone transforms of the same comparison and
// therefore always agree in sign: d_t > d_c ⟺ 1/d_t < 1/d_c ⟺ p_t < p_c ⟺
// p_c − p_t > 0. A positive value on either measure means the price taken was
// longer, i.e. better, than the close.
//
// Since d = 1/p, PercentCLV is algebraically identical to (p_c/p_t − 1) × 100.
// The code evaluates the decimal form, because that is the form a reader can
// re-derive from CLVResult.TakenPrice and CLVResult.ClosingPrice by hand. The
// two forms can differ by a few units in the last place; nothing in the
// contract below is tighter than that.
//
// # What may be compared
//
// Both prices must describe the same question. EvaluateCLV enforces, in order:
//
//   - same market id;
//   - the selection is present in both snapshots;
//   - the same set of selections in both snapshots (see "Outcome set" below);
//   - the same line, compared with domain.Line.Equal, which distinguishes an
//     absent line from a pick'em of 0.0;
//   - the closing observation is not earlier than the taken observation.
//
// The two snapshots may come from different books, and usually should: the
// standard construction takes the price at the book the wager was struck at and
// scores it against the closing line of a sharp reference book (CLAUDE.md §6,
// "Positive-EV finder against a sharp reference book"). Book identity is
// recorded on the result but never constrained.
//
// # The line move
//
// A spread of -3 closing at -3.5 is not the same market question. The fair
// probability of "home −3" and the fair probability of "home −3.5" are answers
// to different questions, and their difference is not value captured — it is
// mostly the probability mass sitting on a three-point margin. Converting
// between the two needs a distribution of game margins, which is a model, not
// arithmetic, and has no place in a pure package.
//
// EvaluateCLV therefore rejects a line move outright with ErrCLVLineMoved.
// EvaluateCLVAcrossLineMove computes the number anyway and stamps
// CLVResult.LineMoved, and AggregateCLV then excludes every such sample from the
// mean and from the beat rate while reporting how many it dropped. The
// indicative number is for a per-wager display that wants to show "you took -3,
// it closed -3.5"; it must never reach a leaderboard, and the aggregate makes
// sure it cannot.
//
// # Outcome set
//
// The two snapshots must contain the same selections. Fair probabilities are a
// distribution over a sample space; if a runner was removed between the wager
// and the close, the two distributions are over different sample spaces and no
// comparison of a single component is meaningful. This is stricter than it needs
// to be for the two- and three-way markets CLV is usually computed on, where the
// set never changes, and it is deliberately strict for the futures market, where
// it does. There is no opt-out; relaxing it needs a documented renormalisation
// model, which would be a change to this contract.
//
// # Void and push
//
// A VOIDED wager — market cancelled, event abandoned, selection withdrawn — is
// excluded from CLV entirely. A market that never closed has no closing line,
// so the quantity does not exist. Set CLVSample.Void and AggregateCLV drops the
// sample from both the numerator and the denominator of every statistic.
//
// A PUSHED wager — the result lands exactly on the line — IS included, at full
// weight, exactly like a win or a loss. A push is a settlement outcome, not a
// data problem: the wager was struck at a real price against a real closing
// line, and CLV measures the quality of that price, not the result. Excluding
// pushes would make a bettor's CLV depend on the scoreboard, which is precisely
// the dependency CLV exists to remove, and would bias the metric toward market
// types that cannot push. CLVSample carries no push flag because CLV does not
// need to know.
//
// # Aggregation
//
// AggregateCLV is a pure function over a slice. Mean CLV is the unweighted
// arithmetic mean over the counted samples — unweighted because CLV is a
// property of the price, not of the stake, and stake-weighting would let a
// bettor buy leaderboard position by sizing up. Sums use Kahan–Babuška–Neumaier
// compensation so a leaderboard over a hundred thousand wagers does not drift.
//
// # Contract for the phase-12 Flink SQL reimplementation
//
// CLAUDE.md §3 replaces this Go code with a Flink SQL job in phase 12 and
// validates the job against it: "same inputs, same outputs, or the Flink job is
// wrong". The semantics that must be reproduced:
//
//  1. Inputs are fair probabilities. The SQL job must devig the complete market
//     on both sides with the same method, and must assert |Σp − 1| ≤ 1e-9
//     (CLVDevigTolerance) before emitting a row.
//  2. clv_probability = p_close − p_taken.
//  3. clv_percent     = (1.0/p_taken) / (1.0/p_close) − 1.0, times 100.
//  4. beat            = clv_probability > 1e-12 (CLVTieBand). A tie is not a
//     beat. The band exists because two devig implementations may differ in the
//     last few bits; it is ten orders of magnitude below the smallest real price
//     increment, so it can never absorb a genuine one-tick move.
//  5. The join is on (market_id, selection_id) with equal line values, absent
//     lines matching only absent lines, over the same selection set, and with
//     the closing event time not earlier than the placement event time. Rows
//     failing any of these are errors, not silently-dropped rows.
//  6. Voided wagers are excluded. Pushes are included.
//  7. Aggregates exclude voided and line-moved samples and are unweighted means.
//
// Bit-identical agreement is not achievable and is not the standard: Flink's
// aggregation order varies with parallelism. Validate per-sample values to a
// relative tolerance of 1e-12 and aggregate values to 1e-9. Any disagreement
// larger than that is a defect in one of the two implementations.
//
// # Purity
//
// Nothing here reads a clock, performs I/O, mutates its arguments, or panics.
// Every observation instant is a parameter. FairMarketSnapshot copies the slice
// it is given on construction and hands out copies, so a snapshot is a genuine
// value even though its payload is variable-length.

// Tolerances. Both are exported because the phase-12 Flink SQL job has to use
// the same numbers, and a constant that only exists in one of two
// implementations of the same contract is a defect waiting to happen.
const (
	// CLVDevigTolerance is the maximum |Σp − 1| a FairMarketSnapshot may carry.
	//
	// The bound is absolute rather than relative because the target is exactly 1.
	//
	// It has to be loose enough to admit a genuine devig and tight enough to
	// refuse a vigged book, and those two requirements are six orders of
	// magnitude apart, so the choice is not delicate. Above: a multiplicative
	// devig of an n-way market is n divisions and n−1 additions, so its residual
	// is on the order of n·2⁻⁵² ≈ 1e-15 for any market a book posts, and the
	// iterative methods (power, Shin) converge to their own criterion, typically
	// 1e-9 to 1e-12. Below: the tightest overround seen in practice is around
	// 1.01 on a sharp two-way market, i.e. |Σp − 1| ≈ 1e-2. 1e-9 sits six orders
	// above the loosest legitimate residual and seven orders below the tightest
	// illegitimate one.
	CLVDevigTolerance = 1e-9

	// CLVTieBand is the dead band inside which two fair probabilities are treated
	// as the same price, so that Beat reports false rather than crowning float
	// noise a win.
	//
	// 1e-12 in probability points is roughly 4,500 ULPs at this scale, matching
	// the relative tolerance the rest of this domain's tests use, and is loose
	// enough to absorb the last-bit disagreement between two devig
	// implementations of the same method. It is also nine orders of magnitude
	// below the smallest price difference the domain can express: one cent of
	// decimal odds at even money moves the implied probability by 2.5e-3, and
	// even at the longest price this package admits — American +1,000,000 against
	// +1,000,001 — the probability difference is about 1e-13… which is the one
	// case where the band does bite. That price is a 10,000-to-1 shot that no
	// book quotes; at the longest price any book actually posts, +100000, a
	// one-tick move is ~1e-8, four orders above the band.
	CLVTieBand = 1e-12

	// clvMinSelections is the smallest number of selections a devigged market can
	// have. With one selection, Σp = 1 forces p = 1, which is not a market.
	clvMinSelections = 2
)

// Sentinel errors for closing line value. Every one is bare, is wrapped with %w
// at the call site, and is matched with errors.Is — never on message text. The
// CLV prefix is deliberate: these describe failures of the CLV comparison
// itself, not of the odds arithmetic, and the distinction matters to a caller
// deciding whether it has a bad price or a bad pairing.
var (
	// ErrCLVMissingIdentity reports a missing market id, book id or selection id,
	// including the case of a FairMarketSnapshot that was never built by
	// NewFairMarketSnapshot and is therefore the zero value.
	ErrCLVMissingIdentity = errors.New("closing line value requires a market, book and selection identity")

	// ErrCLVMarketIncomplete reports a market snapshot with fewer than
	// clvMinSelections selections. CLV is computed from fair probabilities, fair
	// probabilities come from devigging, and devigging needs every side of the
	// market.
	ErrCLVMarketIncomplete = errors.New("a fair market snapshot needs every selection in the market")

	// ErrCLVDuplicateSelection reports the same selection appearing twice in one
	// snapshot. It would be counted twice in the sum-to-one check and would make
	// the lookup ambiguous.
	ErrCLVDuplicateSelection = errors.New("a fair market snapshot lists the same selection twice")

	// ErrCLVNotDevigged reports probabilities that do not sum to 1 within
	// CLVDevigTolerance. In practice this means the caller passed raw implied
	// probabilities and the book's margin is still in them.
	ErrCLVNotDevigged = errors.New("fair probabilities do not sum to 1; the input is still vigged or incomplete")

	// ErrCLVZeroObservation reports a snapshot with the zero time. The
	// observation instant is the event-time key the phase-12 stream join uses;
	// an unset one is not recoverable downstream.
	ErrCLVZeroObservation = errors.New("a fair market snapshot needs its observation instant")

	// ErrCLVMarketMismatch reports two snapshots of different markets.
	ErrCLVMarketMismatch = errors.New("closing line value requires the same market on both sides")

	// ErrCLVSelectionAbsent reports a selection that one of the two snapshots
	// does not price.
	ErrCLVSelectionAbsent = errors.New("the selection is not priced in one of the snapshots")

	// ErrCLVOutcomeSetChanged reports two snapshots over different sets of
	// selections. Their fair probabilities are distributions over different
	// sample spaces and no single component of them is comparable.
	ErrCLVOutcomeSetChanged = errors.New("closing line value requires the same set of selections on both sides")

	// ErrCLVLineMoved reports two snapshots taken at different lines. See the
	// file comment: this is a different market question, not a price move.
	ErrCLVLineMoved = errors.New("closing line value requires the same line on both sides")

	// ErrCLVClosingBeforeTaken reports a closing observation stamped earlier than
	// the taken observation, which means the two arguments were swapped or the
	// event-time join matched the wrong pair.
	ErrCLVClosingBeforeTaken = errors.New("the closing observation precedes the taken observation")

	// ErrCLVNoSamples reports an aggregate over no countable samples, either
	// because the slice was empty or because every sample was excluded as void or
	// line-moved. The means of an empty set are undefined and are not reported as
	// zero.
	ErrCLVNoSamples = errors.New("closing line value aggregate needs at least one countable sample")
)

// -----------------------------------------------------------------------------
// The devigged market snapshot
// -----------------------------------------------------------------------------

// FairSelection is one selection's fair, no-vig probability.
type FairSelection struct {
	// Selection identifies the outcome.
	Selection domain.SelectionID

	// Fair is the no-vig probability of that outcome, in [0, 1]. Across a
	// FairMarketSnapshot these sum to 1.
	Fair Probability
}

// FairMarketSnapshotParams is the input to NewFairMarketSnapshot.
type FairMarketSnapshotParams struct {
	// Market is the market both snapshots of a CLV comparison must share.
	Market domain.MarketID

	// Book is the book the underlying quotes came from. It is recorded, never
	// constrained: scoring one book's price against another book's close is the
	// normal construction.
	Book domain.BookID

	// Line is the handicap or threshold the market traded at, from the MARKET's
	// perspective — the value domain.Market.Line reports, not the per-selection
	// value domain.EffectiveLine returns. Both sides of a comparison must use
	// this convention, and an absent line (domain.NoLine) matches only another
	// absent line.
	Line domain.Line

	// ObservedAt is when the quotes were seen. It is normalised to UTC.
	ObservedAt time.Time

	// Fair is every selection in the market with its no-vig probability. The
	// slice is copied; the caller may reuse or mutate its own after the call.
	Fair []FairSelection
}

// FairMarketSnapshot is a complete, devigged view of one market at one instant:
// every selection, with fair probabilities summing to 1.
//
// The fields are unexported and the only constructor validates, which is what
// makes "CLV is computed on devigged prices" a property of the type rather than
// a comment somebody will eventually stop reading. There is no path from a
// vigged book to a value of this type.
//
// The zero value is deliberately invalid and reports IsZero.
type FairMarketSnapshot struct {
	market     domain.MarketID
	book       domain.BookID
	line       domain.Line
	observedAt time.Time
	fair       []FairSelection
}

// NewFairMarketSnapshot validates its input and returns an immutable snapshot.
//
// It rejects, in order: a missing market, book or selection identity; the zero
// observation time; fewer than clvMinSelections selections; a repeated
// selection; a probability outside [0, 1] or not finite; and finally
// probabilities that do not sum to 1 within CLVDevigTolerance, which is the
// check that mechanically refuses vigged input.
//
// The sum is accumulated with Kahan–Babuška–Neumaier compensation so that the
// tolerance tests the caller's devig rather than this function's summation
// order.
func NewFairMarketSnapshot(p FairMarketSnapshotParams) (FairMarketSnapshot, error) {
	if p.Market.IsZero() {
		return FairMarketSnapshot{}, fmt.Errorf(
			"odds: clv: fair market snapshot has no market id: %w", ErrCLVMissingIdentity)
	}
	if p.Book.IsZero() {
		return FairMarketSnapshot{}, fmt.Errorf(
			"odds: clv: fair market snapshot for market %s has no book id: %w", p.Market, ErrCLVMissingIdentity)
	}
	if p.ObservedAt.IsZero() {
		return FairMarketSnapshot{}, fmt.Errorf(
			"odds: clv: fair market snapshot for market %s: %w", p.Market, ErrCLVZeroObservation)
	}
	if len(p.Fair) < clvMinSelections {
		return FairMarketSnapshot{}, fmt.Errorf(
			"odds: clv: market %s has %d selection(s), a devigged market needs at least %d: %w",
			p.Market, len(p.Fair), clvMinSelections, ErrCLVMarketIncomplete)
	}

	fair := make([]FairSelection, len(p.Fair))
	seen := make(map[domain.SelectionID]struct{}, len(p.Fair))
	var total clvSum
	for i, o := range p.Fair {
		if o.Selection.IsZero() {
			return FairMarketSnapshot{}, fmt.Errorf(
				"odds: clv: market %s selection %d has no id: %w", p.Market, i, ErrCLVMissingIdentity)
		}
		if _, dup := seen[o.Selection]; dup {
			return FairMarketSnapshot{}, fmt.Errorf(
				"odds: clv: market %s lists selection %s twice: %w",
				p.Market, o.Selection, ErrCLVDuplicateSelection)
		}
		seen[o.Selection] = struct{}{}
		if err := clvValidateFair(o.Fair, o.Selection); err != nil {
			return FairMarketSnapshot{}, err
		}
		total.add(float64(o.Fair))
		fair[i] = o
	}

	if sum := total.total(); math.Abs(sum-1) > CLVDevigTolerance {
		return FairMarketSnapshot{}, fmt.Errorf(
			"odds: clv: fair probabilities for market %s sum to %.15g, not 1 within %g: %w",
			p.Market, sum, CLVDevigTolerance, ErrCLVNotDevigged)
	}

	return FairMarketSnapshot{
		market:     p.Market,
		book:       p.Book,
		line:       p.Line,
		observedAt: p.ObservedAt.UTC(),
		fair:       fair,
	}, nil
}

// Market returns the market this snapshot prices.
func (m FairMarketSnapshot) Market() domain.MarketID { return m.market }

// Book returns the book the underlying quotes came from.
func (m FairMarketSnapshot) Book() domain.BookID { return m.book }

// Line returns the line the market traded at, which may be absent.
func (m FairMarketSnapshot) Line() domain.Line { return m.line }

// ObservedAt returns the observation instant, in UTC.
func (m FairMarketSnapshot) ObservedAt() time.Time { return m.observedAt }

// Selections returns a copy of the fair probabilities, in the order supplied.
// It is a copy so that a caller cannot reach through the returned slice and
// invalidate the sum-to-one invariant the constructor established.
func (m FairMarketSnapshot) Selections() []FairSelection {
	out := make([]FairSelection, len(m.fair))
	copy(out, m.fair)
	return out
}

// FairFor returns the fair probability of one selection and whether the snapshot
// prices it. A linear scan is correct and fastest here: markets carry two or
// three selections, and a futures market with fifty is still well inside the
// range where scanning a slice beats hashing a map.
func (m FairMarketSnapshot) FairFor(id domain.SelectionID) (Probability, bool) {
	for _, o := range m.fair {
		if o.Selection == id {
			return o.Fair, true
		}
	}
	return 0, false
}

// IsZero reports whether m is the zero snapshot, which no constructor produces.
func (m FairMarketSnapshot) IsZero() bool { return m.market.IsZero() }

// String implements fmt.Stringer.
func (m FairMarketSnapshot) String() string {
	if m.IsZero() {
		return "fairmarket(<zero>)"
	}
	return fmt.Sprintf("fairmarket(%s@%s line=%s n=%d %s)",
		m.market, m.book, m.line, len(m.fair), m.observedAt.Format(time.RFC3339Nano))
}

// -----------------------------------------------------------------------------
// The scalar measures
// -----------------------------------------------------------------------------

// ProbabilityCLV returns the closing line value in probability points:
//
//	CLV_p = p_close_fair − p_taken_fair
//
// Both arguments must already be devigged. A positive result means the market
// closed at a higher fair probability than the price taken implied — the market
// moved toward the bettor's side, so the price taken was the better one.
//
// This is the measure to use when comparing across markets of different price
// levels, because a percentage of return is not commensurable between a −110
// favourite and a +2500 longshot while a probability point is.
//
// The subtraction of two values in [0, 1] is correctly rounded and cannot
// overflow, so the only failure mode is invalid input. Unlike the percentage
// measure this one accepts the closed interval [0, 1], including the degenerate
// endpoints, because it never divides.
func ProbabilityCLV(takenFair, closingFair Probability) (float64, error) {
	if err := takenFair.Validate(); err != nil {
		return 0, err
	}
	if err := closingFair.Validate(); err != nil {
		return 0, err
	}
	return float64(closingFair) - float64(takenFair), nil
}

// PercentCLV returns the closing line value as a percentage of return:
//
//	CLV_% = (d_taken / d_closing − 1) × 100
//
// A positive result means the price taken was longer, and therefore better, than
// the close: staking one unit at d_taken returns CLV_% percent more than staking
// it at d_closing would have.
//
// Both arguments are expected to be FAIR decimal prices, d = 1/p_fair. This
// function is the arithmetic and cannot verify that; EvaluateCLV is the entry
// point that makes it structurally impossible to pass anything else. Passing raw
// quoted prices here computes a number that mixes line movement with a change in
// the book's margin — see the file comment for the worked example of how wrong
// that goes.
//
// The result is bounded below by −100 for any pair of valid decimal prices, but
// is unbounded above, so it is checked for overflow rather than assumed finite:
// odds.Decimal has no upper bound of its own, and a taken price near the top of
// the float64 range against a closing price barely above 1 does overflow.
func PercentCLV(taken, closing Decimal) (float64, error) {
	if err := taken.Validate(); err != nil {
		return 0, err
	}
	if err := closing.Validate(); err != nil {
		return 0, err
	}
	pct := (float64(taken)/float64(closing) - 1) * 100
	if math.IsNaN(pct) || math.IsInf(pct, 0) {
		return 0, fmt.Errorf("odds: clv: taken %g against closing %g has no finite percentage: %w",
			float64(taken), float64(closing), ErrNotFinite)
	}
	return pct, nil
}

// BeatTheClose reports whether the fair price taken beat the fair closing price,
// and by how much.
//
// The boolean is ProbabilityCLV > CLVTieBand: strictly better, with a dead band
// so that a difference in the last few bits of two devig implementations is not
// reported as a win. A tie is not a beat.
//
// The magnitude is |PercentCLV| in percentage points, always non-negative — the
// direction is in the boolean and is not repeated in the sign. Note the two
// results use different measures, which is deliberate: the boolean has to be
// stable under the tie band, while the magnitude is the number a user is shown.
//
// Both arguments must be strictly inside (0, 1); the degenerate endpoints have
// no decimal price and return ErrProbabilityNotPriceable.
func BeatTheClose(takenFair, closingFair Probability) (beat bool, magnitude float64, err error) {
	diff, err := ProbabilityCLV(takenFair, closingFair)
	if err != nil {
		return false, 0, err
	}
	takenPrice, err := takenFair.Decimal()
	if err != nil {
		return false, 0, err
	}
	closingPrice, err := closingFair.Decimal()
	if err != nil {
		return false, 0, err
	}
	pct, err := PercentCLV(takenPrice, closingPrice)
	if err != nil {
		return false, 0, err
	}
	return diff > CLVTieBand, math.Abs(pct), nil
}

// -----------------------------------------------------------------------------
// The guarded evaluation
// -----------------------------------------------------------------------------

// CLVResult is one wager's closing line value, with everything needed to audit
// it. It is a record: the settle service writes one per graded leg, the API
// serves it, and the phase-12 Flink job reproduces it.
type CLVResult struct {
	// Market and Selection identify what was compared. Both snapshots agreed on
	// these or evaluation would have failed.
	Market    domain.MarketID
	Selection domain.SelectionID

	// TakenBook and ClosingBook record where each price came from. They are
	// frequently different: a wager struck at one book is normally scored against
	// a sharp reference book's close.
	TakenBook   domain.BookID
	ClosingBook domain.BookID

	// Line and ClosingLine are the market lines of the two snapshots. They are
	// equal unless LineMoved is set.
	Line        domain.Line
	ClosingLine domain.Line

	// TakenAt and ClosedAt are the two observation instants, in UTC. ClosedAt is
	// never before TakenAt.
	TakenAt  time.Time
	ClosedAt time.Time

	// TakenFair and ClosingFair are the devigged probabilities that were
	// compared, and TakenPrice and ClosingPrice their decimal forms, 1/p. These
	// are fair prices, not the quotes the book displayed.
	TakenFair    Probability
	ClosingFair  Probability
	TakenPrice   Decimal
	ClosingPrice Decimal

	// ProbabilityCLV is ClosingFair − TakenFair, in probability points.
	ProbabilityCLV float64

	// PercentCLV is (TakenPrice/ClosingPrice − 1) × 100, in percentage points.
	PercentCLV float64

	// Beat reports ProbabilityCLV > CLVTieBand.
	Beat bool

	// Magnitude is |PercentCLV|.
	Magnitude float64

	// LineMoved reports that the two snapshots were taken at different lines and
	// that this result therefore came from EvaluateCLVAcrossLineMove. Such a
	// result is indicative only — AggregateCLV excludes it.
	LineMoved bool
}

// IsZero reports whether r is the zero result, which no evaluation produces.
func (r CLVResult) IsZero() bool { return r.Selection.IsZero() }

// String implements fmt.Stringer.
func (r CLVResult) String() string {
	if r.IsZero() {
		return "clv(<zero>)"
	}
	return fmt.Sprintf("clv(%s/%s line=%s taken=%.6g close=%.6g dp=%+.6f pct=%+.4f%% beat=%t moved=%t)",
		r.Market, r.Selection, r.Line,
		float64(r.TakenPrice), float64(r.ClosingPrice),
		r.ProbabilityCLV, r.PercentCLV, r.Beat, r.LineMoved)
}

// EvaluateCLV computes the closing line value of one selection, comparing a
// devigged snapshot taken when the wager was struck against a devigged snapshot
// of the close.
//
// It refuses anything that is not a like-for-like comparison: a different
// market, a selection missing from either side, a different set of selections, a
// line move, or a close stamped before the wager. See the file comment for why
// each of those is a correctness issue rather than pedantry.
//
// Use EvaluateCLVAcrossLineMove, and only for display, when the line moved.
func EvaluateCLV(taken, closing FairMarketSnapshot, selection domain.SelectionID) (CLVResult, error) {
	return evaluateCLV(taken, closing, selection, false)
}

// EvaluateCLVAcrossLineMove is EvaluateCLV with the line check downgraded from
// an error to the CLVResult.LineMoved flag. It is the explicit acknowledgement
// that the caller knows the two snapshots answer different questions.
//
// The number it returns is NOT closing line value. A spread of -3 and a spread
// of -3.5 differ by whatever probability mass sits on a three-point margin, and
// that mass is in the result with no way to separate it out. Show it next to the
// two lines in a user interface; never rank anyone by it. AggregateCLV enforces
// the second half of that sentence.
//
// Every other check EvaluateCLV performs still applies.
func EvaluateCLVAcrossLineMove(taken, closing FairMarketSnapshot, selection domain.SelectionID) (CLVResult, error) {
	return evaluateCLV(taken, closing, selection, true)
}

func evaluateCLV(taken, closing FairMarketSnapshot, selection domain.SelectionID, allowLineMove bool) (CLVResult, error) {
	if taken.IsZero() || closing.IsZero() {
		return CLVResult{}, fmt.Errorf(
			"odds: clv: a snapshot was not built by NewFairMarketSnapshot: %w", ErrCLVMissingIdentity)
	}
	if selection.IsZero() {
		return CLVResult{}, fmt.Errorf("odds: clv: no selection given: %w", ErrCLVMissingIdentity)
	}
	if taken.market != closing.market {
		return CLVResult{}, fmt.Errorf("odds: clv: taken market %s against closing market %s: %w",
			taken.market, closing.market, ErrCLVMarketMismatch)
	}

	takenFair, ok := taken.FairFor(selection)
	if !ok {
		return CLVResult{}, fmt.Errorf(
			"odds: clv: selection %s is not priced in the taken snapshot of market %s: %w",
			selection, taken.market, ErrCLVSelectionAbsent)
	}
	closingFair, ok := closing.FairFor(selection)
	if !ok {
		return CLVResult{}, fmt.Errorf(
			"odds: clv: selection %s is not priced in the closing snapshot of market %s: %w",
			selection, closing.market, ErrCLVSelectionAbsent)
	}
	if !clvSameOutcomeSet(taken, closing) {
		return CLVResult{}, fmt.Errorf(
			"odds: clv: market %s priced %d selection(s) when taken and %d at the close: %w",
			taken.market, len(taken.fair), len(closing.fair), ErrCLVOutcomeSetChanged)
	}

	lineMoved := !taken.line.Equal(closing.line)
	if lineMoved && !allowLineMove {
		return CLVResult{}, fmt.Errorf("odds: clv: market %s was taken at line %s and closed at line %s: %w",
			taken.market, taken.line, closing.line, ErrCLVLineMoved)
	}
	if closing.observedAt.Before(taken.observedAt) {
		return CLVResult{}, fmt.Errorf("odds: clv: market %s closed at %s, before the price taken at %s: %w",
			taken.market,
			closing.observedAt.Format(time.RFC3339Nano),
			taken.observedAt.Format(time.RFC3339Nano),
			ErrCLVClosingBeforeTaken)
	}

	takenPrice, err := takenFair.Decimal()
	if err != nil {
		return CLVResult{}, clvPriceErr("taken", taken.market, selection, takenFair, err)
	}
	closingPrice, err := closingFair.Decimal()
	if err != nil {
		return CLVResult{}, clvPriceErr("closing", closing.market, selection, closingFair, err)
	}

	probabilityCLV := float64(closingFair) - float64(takenFair)
	percentCLV, err := PercentCLV(takenPrice, closingPrice)
	if err != nil {
		return CLVResult{}, err
	}

	return CLVResult{
		Market:         taken.market,
		Selection:      selection,
		TakenBook:      taken.book,
		ClosingBook:    closing.book,
		Line:           taken.line,
		ClosingLine:    closing.line,
		TakenAt:        taken.observedAt,
		ClosedAt:       closing.observedAt,
		TakenFair:      takenFair,
		ClosingFair:    closingFair,
		TakenPrice:     takenPrice,
		ClosingPrice:   closingPrice,
		ProbabilityCLV: probabilityCLV,
		PercentCLV:     percentCLV,
		Beat:           probabilityCLV > CLVTieBand,
		Magnitude:      math.Abs(percentCLV),
		LineMoved:      lineMoved,
	}, nil
}

// clvSameOutcomeSet reports whether two snapshots price the same set of
// selections. Duplicates are impossible by construction, so equal cardinality
// plus one-way containment is set equality.
func clvSameOutcomeSet(a, b FairMarketSnapshot) bool {
	if len(a.fair) != len(b.fair) {
		return false
	}
	for _, o := range a.fair {
		if _, ok := b.FairFor(o.Selection); !ok {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Aggregation
// -----------------------------------------------------------------------------

// CLVSample is one wager leg's contribution to an aggregate.
//
// The only thing aggregation needs to know beyond the result itself is whether
// the wager was voided, because a voided wager has no closing line to be
// measured against. A push is not flagged, because a push counts exactly like a
// win or a loss: see the file comment.
type CLVSample struct {
	// Result is the evaluated closing line value.
	Result CLVResult

	// Void marks a wager that was cancelled, abandoned or otherwise given no
	// action. It is excluded from every statistic, numerator and denominator
	// alike. The zero value is false, so a sample counts unless it is said not
	// to, which is the safe default.
	Void bool
}

// CLVAggregate is the summary of a set of samples: one user's record, one
// league's, or a leaderboard row.
type CLVAggregate struct {
	// Samples is how many samples were supplied.
	Samples int

	// Counted is how many contributed to the statistics. The statistics are only
	// meaningful when this is positive, and AggregateCLV returns
	// ErrCLVNoSamples rather than reporting a mean over nothing.
	Counted int

	// VoidExcluded and LineMovedExcluded are why the rest were dropped. They are
	// reported rather than silently absorbed so that a leaderboard row can be
	// audited: a user whose CLV is computed from a third of their wagers is a
	// different claim from one whose CLV is computed from all of them.
	VoidExcluded      int
	LineMovedExcluded int

	// BeatCount is how many counted samples beat the close, and BeatRate is that
	// as a fraction of Counted, in [0, 1]. Multiply by 100 for display.
	BeatCount int
	BeatRate  float64

	// MeanProbabilityCLV and MeanPercentCLV are unweighted arithmetic means over
	// the counted samples, in probability points and percentage points
	// respectively. Unweighted by stake on purpose: CLV is a property of the
	// price, and stake-weighting would let a bettor buy leaderboard position by
	// sizing up.
	MeanProbabilityCLV float64
	MeanPercentCLV     float64
}

// AggregateCLV summarises a set of samples.
//
// It excludes voided samples and line-moved samples, counts what is left, and
// returns the mean of both measures plus the share that beat the close. It
// returns ErrCLVNoSamples when nothing is countable — an empty slice, or a slice
// in which everything was excluded — rather than reporting the mean of an empty
// set as zero, which would put a user with three voided wagers on the
// leaderboard at exactly par.
//
// A sample carrying a non-finite measure is an error rather than a skip: it can
// only come from a CLVResult that was not produced by EvaluateCLV, and silently
// dropping it would hide the defect that created it.
//
// Sums are accumulated with Kahan–Babuška–Neumaier compensation. Naive
// summation over a hundred thousand samples of magnitude ~1 carries a worst-case
// error around 2e-6, which is larger than the margin that separates adjacent
// leaderboard rows; compensated summation costs three extra flops per sample and
// removes the question.
func AggregateCLV(samples []CLVSample) (CLVAggregate, error) {
	agg := CLVAggregate{Samples: len(samples)}

	var sumProbability, sumPercent clvSum
	for i, s := range samples {
		switch {
		case s.Void:
			agg.VoidExcluded++
			continue
		case s.Result.LineMoved:
			agg.LineMovedExcluded++
			continue
		}
		if s.Result.IsZero() {
			return CLVAggregate{}, fmt.Errorf(
				"odds: clv: sample %d was not produced by EvaluateCLV: %w", i, ErrCLVMissingIdentity)
		}
		if !clvFinite(s.Result.ProbabilityCLV) || !clvFinite(s.Result.PercentCLV) {
			return CLVAggregate{}, fmt.Errorf(
				"odds: clv: sample %d carries probability %v and percent %v: %w",
				i, s.Result.ProbabilityCLV, s.Result.PercentCLV, ErrNotFinite)
		}

		agg.Counted++
		sumProbability.add(s.Result.ProbabilityCLV)
		sumPercent.add(s.Result.PercentCLV)
		if s.Result.Beat {
			agg.BeatCount++
		}
	}

	if agg.Counted == 0 {
		return CLVAggregate{}, fmt.Errorf(
			"odds: clv: %d sample(s), %d void and %d line-moved, none countable: %w",
			agg.Samples, agg.VoidExcluded, agg.LineMovedExcluded, ErrCLVNoSamples)
	}

	n := float64(agg.Counted)
	agg.MeanProbabilityCLV = sumProbability.total() / n
	agg.MeanPercentCLV = sumPercent.total() / n
	agg.BeatRate = float64(agg.BeatCount) / n
	return agg, nil
}

// -----------------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------------

// clvValidateFair mirrors Probability.Validate but reports which selection the
// bad value belongs to.
//
// It repeats the two bounds tests rather than delegating because the package
// convention is that an error message carries the "odds:" prefix exactly once,
// at the front, and wrapping an already-prefixed error would break that. The
// sentinels are the same ones Probability.Validate returns, so errors.Is is
// unaffected, and a test asserts the two agree on every value they are given.
func clvValidateFair(p Probability, sel domain.SelectionID) error {
	v := float64(p)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("odds: clv: fair probability %v for selection %s: %w", v, sel, ErrNotFinite)
	}
	if v < 0 || v > 1 {
		return fmt.Errorf("odds: clv: fair probability %g for selection %s: %w", v, sel, ErrProbabilityOutOfRange)
	}
	return nil
}

// clvPriceErr re-emits a Probability.Decimal failure with the single leading
// "odds:" prefix the package convention requires, naming the side, market and
// selection while preserving the underlying sentinel so errors.Is still matches
// ErrProbabilityNotPriceable.
func clvPriceErr(side string, market domain.MarketID, sel domain.SelectionID, p Probability, err error) error {
	return fmt.Errorf("odds: clv: %s fair probability %g for selection %s in market %s has no price: %w",
		side, float64(p), sel, market, unprefixed(err))
}

// clvFinite reports whether x is a real number.
func clvFinite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// clvSum is a Kahan–Babuška–Neumaier compensated accumulator.
//
// Neumaier's variant rather than plain Kahan because it stays correct when the
// incoming term is larger in magnitude than the running sum, which happens
// whenever a set of near-zero CLV values is followed by one large one — the
// exact shape of a real bettor's record. The compensation is folded in once at
// the end rather than at every step, which is what makes it cheap.
//
// It is a local accumulator, never package state: every user constructs its own
// zero value, and the zero value is a correct empty sum.
type clvSum struct {
	sum          float64
	compensation float64
}

func (s *clvSum) add(x float64) {
	t := s.sum + x
	if math.Abs(s.sum) >= math.Abs(x) {
		s.compensation += (s.sum - t) + x
	} else {
		s.compensation += (x - t) + s.sum
	}
	s.sum = t
}

func (s clvSum) total() float64 { return s.sum + s.compensation }
