// The snapshot construction, the devig, and the guarded evaluation.
//
// Read doc.go first. It carries the definition of a closing price and the
// argument for every clause of it; this file is the code that definition
// describes, and a change here that is not also a change there has broken the
// cross-language contract phase 12 is validated against.
package clv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// -----------------------------------------------------------------------------
// Defaults
// -----------------------------------------------------------------------------

const (
	// DefaultDevigMethod is Shin, matching internal/pricing's default.
	//
	// The two must agree, and not merely by coincidence: a user's +EV signals and
	// their CLV are both statements about the same fair probability, and computing
	// them under two different margin models would let the analytics surface tell
	// a bettor that a price was +3% EV and then score it as having lost value on a
	// line that never moved. odds/devig.go's own framing is that the four methods
	// "disagree meaningfully on longshots"; disagreeing with ourselves is worse.
	DefaultDevigMethod = odds.MethodShin

	// DefaultClosingLookback bounds how far before the closing instant a quote may
	// have been observed and still count as the close.
	//
	// Twenty-four hours. The lower bound is set by the slowest tier a market that
	// anyone can bet is polled on (ADR 0003's cadence ladder tops out well inside
	// a day), so anything shorter would start dropping legitimate closes on quiet
	// markets. The upper bound is the point at which the number stops meaning
	// anything: two days out, a market nobody has repriced is not closing, it is
	// dormant, and a bettor scored against it would be scored against a number the
	// market had abandoned.
	//
	// It is also the only bound on the hypertable walk, so it is what keeps the
	// closing query to roughly two chunks per selection rather than every chunk
	// that has ever existed.
	DefaultClosingLookback = 24 * time.Hour

	// DefaultTakenLookback bounds the reconstruction of the market as it stood
	// when the leg was booked.
	//
	// The same twenty-four hours, and for a reason specific to this side: change
	// detection hashes a whole normalised market (CLAUDE.md §5), so a book's
	// selections are normally written together and the sibling quotes share the
	// leg's own instant to the microsecond. The window is not there for the normal
	// case at all — it is there for the market whose one quiet side has not been
	// republished in a while, and a day is long enough that such a side is still
	// found while short enough that a genuinely abandoned one is not.
	DefaultTakenLookback = 24 * time.Hour
)

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

var (
	// ErrInvalidOptions is returned by [New] when its options do not validate.
	// Configuration fails at construction rather than at the first leg, so a
	// misconfigured measurer never produces a single wrong row.
	ErrInvalidOptions = errors.New("clv: invalid options")

	// ErrUnusableLeg reports a work-queue row that is not a graded leg: a
	// malformed identifier, an unknown market type, a status that is not terminal,
	// a missing observation instant.
	//
	// It is NOT one of the unmeasurable reasons, deliberately. An unmeasurable leg
	// is a leg the data cannot answer for; this is a leg whose own row is
	// incoherent, which is a defect in whatever produced it and must not be
	// counted alongside the honest exclusions.
	ErrUnusableLeg = errors.New("clv: leg cannot be measured because its own record is unusable")

	// ErrMarketNotFound reports a leg naming a market that does not exist.
	// Genuinely exceptional: legs.market_id is a foreign key.
	ErrMarketNotFound = errors.New("clv: market not found")

	// ErrUnmeasurable reports a leg that has no closing line value, for one of the
	// reasons [Reason] enumerates. Every [UnmeasurableError] matches it under
	// errors.Is, so a caller that only wants "no row, and that is fine" can test
	// once, and a caller that wants the reason unwraps to the concrete type.
	//
	// It is an ERROR rather than a (Measurement, bool) pair because the reason is
	// the interesting part: migrations/00009 makes absence meaningful, and an
	// absence with no recorded cause is an absence nobody can act on.
	ErrUnmeasurable = errors.New("clv: leg has no measurable closing line value")
)

// -----------------------------------------------------------------------------
// Why a leg has no measurement
// -----------------------------------------------------------------------------

// Reason names why a graded leg produced no closing line value.
//
// It is a CLOSED SET and it is a metric label, so every value here is bounded
// cardinality and every one of them is documented in doc.go's outcome table. A
// new value is a change to that table and to the phase-12 contract, not a local
// addition.
type Reason uint8

const (
	// ReasonNone is the zero value and means the leg WAS measured.
	ReasonNone Reason = iota

	// ReasonNoClose: the market has no usable closing instant — the event never
	// started, or its scheduled start is unset. doc.go §1.
	ReasonNoClose

	// ReasonTakenIncomplete: the market as it stood when the leg was booked could
	// not be reconstructed — at least one selection had no eligible quote at the
	// leg's own book within TakenLookback. Devigging needs the whole outcome set,
	// so a subset is not a smaller answer, it is no answer.
	ReasonTakenIncomplete

	// ReasonTakenIncoherent: the reconstructed taken snapshot's selections do not
	// all describe the same line. doc.go §7.
	ReasonTakenIncoherent

	// ReasonTakenQuoteMismatch: the reconstruction found a quote for the leg's own
	// selection that is NOT the leg's own quote. doc.go §6 — the market it
	// describes is not the market the wager was struck in.
	ReasonTakenQuoteMismatch

	// ReasonClosingIncomplete: the closing snapshot was short. This absorbs the
	// suspension case: quotes observed inside a suspension episode are excluded,
	// so a market suspended through its whole lookback window leaves selections
	// unpriced and lands here.
	ReasonClosingIncomplete

	// ReasonClosingIncoherent: the closing snapshot's selections do not all
	// describe the same line.
	ReasonClosingIncoherent

	// ReasonOutcomeSetChanged: the market priced a different set of selections at
	// the close than when the leg was booked — a three-way market that lost its
	// draw, a futures field that lost a runner. The two devigged distributions are
	// over different sample spaces and no single component of them is comparable.
	// odds.ErrCLVOutcomeSetChanged.
	ReasonOutcomeSetChanged

	// ReasonCloseBeforeTake: the close precedes the take, which for this system
	// means the wager was struck IN PLAY. doc.go §5 argues at length why that is a
	// deliberate exclusion rather than a gap, and why a visible share of legs
	// landing here is not a defect.
	ReasonCloseBeforeTake

	// ReasonNotDevigable: neither the configured method nor the multiplicative
	// fallback could remove the margin from one of the two snapshots, or the
	// resulting probabilities would not build a FairMarketSnapshot. A market whose
	// prices multiplicative also refuses is a market whose prices are not a
	// market.
	ReasonNotDevigable
)

// String returns the metric-label form. The strings are the contract, not the
// constant names: a dashboard query and a Flink job both select on them.
func (r Reason) String() string {
	switch r {
	case ReasonNone:
		return "none"
	case ReasonNoClose:
		return "no_close"
	case ReasonTakenIncomplete:
		return "taken_incomplete"
	case ReasonTakenIncoherent:
		return "taken_incoherent"
	case ReasonTakenQuoteMismatch:
		return "taken_quote_mismatch"
	case ReasonClosingIncomplete:
		return "closing_incomplete"
	case ReasonClosingIncoherent:
		return "closing_incoherent"
	case ReasonOutcomeSetChanged:
		return "outcome_set_changed"
	case ReasonCloseBeforeTake:
		return "close_before_take"
	case ReasonNotDevigable:
		return "not_devigable"
	default:
		return "unknown"
	}
}

// Reasons returns every reason a leg can be unmeasurable, in declaration order.
// It exists so a metric can pre-create its label values and report a HONEST ZERO
// for a reason that has not fired yet, rather than an absent series that a
// dashboard renders as a gap.
func Reasons() []Reason {
	return []Reason{
		ReasonNoClose,
		ReasonTakenIncomplete,
		ReasonTakenIncoherent,
		ReasonTakenQuoteMismatch,
		ReasonClosingIncomplete,
		ReasonClosingIncoherent,
		ReasonOutcomeSetChanged,
		ReasonCloseBeforeTake,
		ReasonNotDevigable,
	}
}

// UnmeasurableError is a leg with no closing line value, and why.
//
// It wraps both [ErrUnmeasurable] and the underlying cause, so errors.Is matches
// the sentinel AND matches the odds package's own sentinel where one was the
// cause — errors.Is(err, odds.ErrCLVOutcomeSetChanged) works on the value this
// package returns, which is what lets a test assert the exact refusal rather than
// a paraphrase of it.
type UnmeasurableError struct {
	// Leg is the leg that could not be measured.
	Leg domain.LegID

	// Reason is the bounded classification, suitable as a metric label.
	Reason Reason

	// Cause is the underlying error, where there was one. It is nil for the
	// reasons this package decides for itself (an incomplete snapshot has no
	// error to carry — it is a row count).
	Cause error
}

func (e *UnmeasurableError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("clv: leg %s is unmeasurable (%s): %v", e.Leg, e.Reason, e.Cause)
	}
	return fmt.Sprintf("clv: leg %s is unmeasurable (%s)", e.Leg, e.Reason)
}

// Unwrap returns both the sentinel and the cause, so errors.Is matches either.
func (e *UnmeasurableError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrUnmeasurable, e.Cause}
	}
	return []error{ErrUnmeasurable}
}

// ReasonFor returns the classification carried by err, and whether err was an
// unmeasurable-leg error at all.
//
// Callers use it to label a metric. A non-unmeasurable error — a database
// failure, an unusable leg — reports (ReasonNone, false) and must be counted
// somewhere else: an infrastructure fault filed under an analytics exclusion is
// how a broken store looks like a quiet market.
func ReasonFor(err error) (Reason, bool) {
	var u *UnmeasurableError
	if errors.As(err, &u) {
		return u.Reason, true
	}
	return ReasonNone, false
}

func unmeasurable(leg domain.LegID, reason Reason, cause error) error {
	return &UnmeasurableError{Leg: leg, Reason: reason, Cause: cause}
}

// -----------------------------------------------------------------------------
// The measurement
// -----------------------------------------------------------------------------

// Measurement is one graded leg's closing line value, plus everything needed to
// store it, publish it and audit it.
//
// The arithmetic lives entirely in Result. This struct adds only PROVENANCE: who
// the leg belonged to, which market and league it was on, which book both prices
// came from, and which devig method produced the two fair probabilities. Every
// one of those is a column of wager_leg_clv, and every one of them is something a
// reader of a stored row would otherwise have to reconstruct.
type Measurement struct {
	// Leg is the graded leg this measures, carried whole so a publisher does not
	// need a second lookup for the user, the event or the price taken.
	Leg Leg

	// LeagueID is the league the event belongs to, read back with the market.
	LeagueID domain.LeagueID

	// ClosingBook is the book the closing snapshot came from. It equals
	// Leg.Book under this package's rules (doc.go §3) and is stored separately
	// anyway, because the column exists to survive that rule changing.
	ClosingBook domain.BookID

	// DevigMethod is the method that ACTUALLY produced both fair probabilities —
	// the configured one, or multiplicative if the configured one refused either
	// side. One value for both sides is the invariant; see doc.go's devig section.
	DevigMethod odds.DevigMethod

	// Result is odds.EvaluateCLV's output, or EvaluateCLVAcrossLineMove's when the
	// line moved. Result.LineMoved is the discriminator, and it is what excludes
	// the row from every aggregate.
	Result odds.CLVResult

	// ComputedAt is when this measurement was made, from the measurer's clock. It
	// is the ONLY value here that is not a provider instant, it is recorded and
	// never keyed, and nothing compares it to anything — migrations/00009 requires
	// exactly that of a replay key.
	ComputedAt time.Time
}

// Voided reports whether the leg had no action, which is the one exclusion
// odds.AggregateCLV applies beyond the line move.
//
// It is `status == void` and nothing else. A PUSH is not void: odds/clv.go is
// explicit that "a pushed wager IS included, at full weight, exactly like a win
// or a loss", because excluding pushes would make a bettor's CLV depend on the
// scoreboard, which is precisely the dependency CLV exists to remove.
func (m Measurement) Voided() bool { return m.Leg.Status == domain.LegStatusVoid }

// Sample projects the measurement onto the value odds.AggregateCLV consumes, so
// a caller aggregating in Go uses the domain's own compensated summation rather
// than writing a mean.
func (m Measurement) Sample() odds.CLVSample {
	return odds.CLVSample{Result: m.Result, Void: m.Voided()}
}

// -----------------------------------------------------------------------------
// Options and construction
// -----------------------------------------------------------------------------

// Options are [New]'s dependencies. Everything is constructor-injected; nothing
// is read from a global (CLAUDE.md §12).
type Options struct {
	// Store is the two reads. Required.
	Store Store

	// DevigMethod is the margin model applied to BOTH snapshots. Zero means
	// [DefaultDevigMethod].
	DevigMethod odds.DevigMethod

	// ClosingLookback bounds how far before the closing instant a quote may lie.
	// Zero means [DefaultClosingLookback]. It must be positive: a zero or negative
	// window admits nothing at all, which would report every market as having no
	// close.
	ClosingLookback time.Duration

	// TakenLookback bounds the reconstruction of the market at placement. Zero
	// means [DefaultTakenLookback].
	TakenLookback time.Duration

	// Clock stamps [Measurement.ComputedAt]. Nil means time.Now.
	//
	// Injected because CLAUDE.md §12 forbids reaching for a global, and because a
	// test that asserts a stored row byte for byte needs the one non-provider
	// instant on it to be deterministic. NOTHING THAT DECIDES ANYTHING READS IT.
	Clock func() time.Time
}

func (o Options) validate() error {
	switch {
	case o.Store == nil:
		return fmt.Errorf("%w: Store is nil", ErrInvalidOptions)
	case o.DevigMethod != odds.MethodUnknown && !o.DevigMethod.Valid():
		return fmt.Errorf("%w: devig method %d is not one of the four",
			ErrInvalidOptions, uint8(o.DevigMethod))
	case o.ClosingLookback < 0:
		return fmt.Errorf("%w: ClosingLookback is negative", ErrInvalidOptions)
	case o.TakenLookback < 0:
		return fmt.Errorf("%w: TakenLookback is negative", ErrInvalidOptions)
	}
	return nil
}

// Measurer measures one graded leg's closing line value against the market's
// close.
//
// It holds no mutable state, so one value is safe for concurrent use by as many
// goroutines as the store can serve.
type Measurer struct {
	store           Store
	method          odds.DevigMethod
	closingLookback time.Duration
	takenLookback   time.Duration
	clock           func() time.Time
}

// New validates the options and builds the measurer. It performs no I/O.
func New(opts Options) (*Measurer, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	method := opts.DevigMethod
	if method == odds.MethodUnknown {
		method = DefaultDevigMethod
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	return &Measurer{
		store:           opts.Store,
		method:          method,
		closingLookback: positiveOr(opts.ClosingLookback, DefaultClosingLookback),
		takenLookback:   positiveOr(opts.TakenLookback, DefaultTakenLookback),
		clock:           clock,
	}, nil
}

// DevigMethod returns the configured method. A measurement may still record
// multiplicative, if the configured method refused a side; see doc.go.
func (m *Measurer) DevigMethod() odds.DevigMethod { return m.method }

// ClosingLookback and TakenLookback return the declared windows. They are
// exported because they are PARAMETERS OF THE DEFINITION rather than tuning
// knobs — phase 12 has to be given the same two numbers, and a value that only
// exists inside a struct is a value nobody can hand over.
func (m *Measurer) ClosingLookback() time.Duration { return m.closingLookback }
func (m *Measurer) TakenLookback() time.Duration   { return m.takenLookback }

// -----------------------------------------------------------------------------
// Measure
// -----------------------------------------------------------------------------

// Measure computes one graded leg's closing line value.
//
// The sequence, which is the sequence doc.go describes:
//
//	validate the leg
//	read the market and its closing instant
//	reconstruct the market as it stood when the leg was booked   (taken snapshot)
//	read the market as it stood at the close                     (closing snapshot)
//	devig BOTH with one method
//	odds.EvaluateCLV, downgraded to EvaluateCLVAcrossLineMove on a line move
//
// It returns a [Measurement] or an error. Three kinds of error come out of it and
// they must be handled differently:
//
//	ErrUnusableLeg   the row handed in is not a graded leg. A defect in the
//	                 caller or in the query that produced it. Never retry.
//	ErrUnmeasurable  the data cannot answer for this leg, for one of the
//	                 enumerated reasons. NOT a failure — it is the documented
//	                 outcome for an in-play wager, a market that shut early, or a
//	                 field that lost a runner. Write no row and count the reason.
//	anything else    the store failed. Transient. Retry on the next pass.
//
// Conflating the second and the third is the failure mode this signature exists
// to prevent: a store outage counted as "the market had no close" is an outage
// nobody can see.
func (m *Measurer) Measure(ctx context.Context, leg Leg) (Measurement, error) {
	if err := leg.validate(); err != nil {
		return Measurement{}, err
	}

	market, err := m.store.MarketClose(ctx, leg.MarketID)
	if err != nil {
		return Measurement{}, fmt.Errorf("clv: leg %s: closing instant for market %s: %w",
			leg.LegID, leg.MarketID, err)
	}
	if err := market.validateClose(leg); err != nil {
		return Measurement{}, err
	}

	taken, err := m.snapshotFor(ctx, leg, market, snapshotSideTaken)
	if err != nil {
		return Measurement{}, err
	}
	closing, err := m.snapshotFor(ctx, leg, market, snapshotSideClosing)
	if err != nil {
		return Measurement{}, err
	}

	method, takenFair, closingFair, err := devigBoth(m.method, taken.quotes, closing.quotes)
	if err != nil {
		return Measurement{}, unmeasurable(leg.LegID, ReasonNotDevigable, err)
	}

	takenSnap, err := fairSnapshot(leg, taken, takenFair)
	if err != nil {
		return Measurement{}, unmeasurable(leg.LegID, ReasonNotDevigable, err)
	}
	closingSnap, err := fairSnapshot(leg, closing, closingFair)
	if err != nil {
		return Measurement{}, unmeasurable(leg.LegID, ReasonNotDevigable, err)
	}

	result, err := evaluate(leg, takenSnap, closingSnap)
	if err != nil {
		return Measurement{}, err
	}

	return Measurement{
		Leg:         leg,
		LeagueID:    market.LeagueID,
		ClosingBook: leg.Book,
		DevigMethod: method,
		Result:      result,
		ComputedAt:  m.clock().UTC(),
	}, nil
}

// evaluate runs the guarded comparison, downgrading a line move to the
// display-only form and mapping every other refusal onto its reason.
//
// The line move is the ONLY refusal that is downgraded, and it is downgraded
// rather than ignored: the result carries LineMoved, odds.AggregateCLV drops it
// from the mean and the beat rate, and the leaderboard query filters it in SQL.
// odds/clv.go's instruction is followed to the letter — "show it next to the two
// lines in a user interface; never rank anyone by it".
func evaluate(leg Leg, taken, closing odds.FairMarketSnapshot) (odds.CLVResult, error) {
	result, err := odds.EvaluateCLV(taken, closing, leg.SelectionID)
	switch {
	case err == nil:
		return result, nil

	case errors.Is(err, odds.ErrCLVLineMoved):
		moved, mErr := odds.EvaluateCLVAcrossLineMove(taken, closing, leg.SelectionID)
		if mErr != nil {
			// Unreachable in practice: the across-line-move form performs every
			// other check identically and the line check is the one that just
			// failed. Guarded because "unreachable" and "cannot happen" are
			// different claims, and the second one is the one that pages somebody.
			return odds.CLVResult{}, unmeasurable(leg.LegID, reasonForCLVError(mErr), mErr)
		}
		return moved, nil

	default:
		return odds.CLVResult{}, unmeasurable(leg.LegID, reasonForCLVError(err), err)
	}
}

// reasonForCLVError maps the odds package's refusals onto this package's bounded
// reasons.
//
// The two enumerations are deliberately not the same: odds.EvaluateCLV rejects
// several conditions this package has already made impossible (a mismatched
// market, a selection absent from a snapshot it built), and the reasons here also
// cover conditions that never reach EvaluateCLV at all (an incomplete snapshot).
// Anything unrecognised lands on ReasonNotDevigable rather than on a default that
// claims more than it knows.
func reasonForCLVError(err error) Reason {
	switch {
	case errors.Is(err, odds.ErrCLVOutcomeSetChanged):
		return ReasonOutcomeSetChanged
	case errors.Is(err, odds.ErrCLVClosingBeforeTaken):
		return ReasonCloseBeforeTake
	case errors.Is(err, odds.ErrCLVSelectionAbsent):
		// The leg's selection is not in one of the two snapshots. Both snapshots
		// are complete over the market's selection set by the time evaluation
		// runs, so this means the leg names a selection its own market does not
		// have — a taken-side reconstruction that does not describe the market the
		// wager was struck in.
		return ReasonTakenQuoteMismatch
	default:
		return ReasonNotDevigable
	}
}

// -----------------------------------------------------------------------------
// Snapshot construction
// -----------------------------------------------------------------------------

// snapshotSide names which of the two snapshots is being built. It exists so the
// bounds, the extra taken-side check and the failure reasons can be chosen from
// one place — the alternative is two nearly-identical functions that drift.
type snapshotSide uint8

const (
	snapshotSideTaken snapshotSide = iota
	snapshotSideClosing
)

// builtSnapshot is a validated snapshot: complete, coherent, with its market-frame
// line and its instant resolved.
type builtSnapshot struct {
	quotes     []Quote
	line       domain.Line
	observedAt time.Time
}

// snapshotFor reads and validates one side's snapshot.
func (m *Measurer) snapshotFor(
	ctx context.Context, leg Leg, market Market, side snapshotSide,
) (builtSnapshot, error) {
	var (
		req              SnapshotRequest
		incomplete       = ReasonClosingIncomplete
		incoherent       = ReasonClosingIncoherent
		requireLegsQuote bool
	)
	switch side {
	case snapshotSideTaken:
		req = SnapshotRequest{
			Market: leg.MarketID,
			Book:   leg.Book,
			// The leg's own quote instant, inclusive, so the leg's own row is
			// eligible for its own snapshot.
			AsOf:      leg.ObservedAt,
			NotBefore: leg.ObservedAt.Add(-m.takenLookback),
		}
		incomplete, incoherent = ReasonTakenIncomplete, ReasonTakenIncoherent
		requireLegsQuote = true
	default:
		req = SnapshotRequest{
			Market:    leg.MarketID,
			Book:      leg.Book,
			AsOf:      market.ScheduledStart,
			NotBefore: market.ScheduledStart.Add(-m.closingLookback),
		}
	}

	snap, err := m.store.Snapshot(ctx, req)
	if err != nil {
		return builtSnapshot{}, fmt.Errorf("clv: leg %s: snapshot of market %s at book %s as of %s: %w",
			leg.LegID, req.Market, req.Book, req.AsOf.UTC().Format(time.RFC3339Nano), err)
	}
	if !snap.Complete() {
		return builtSnapshot{}, unmeasurable(leg.LegID, incomplete, nil)
	}

	line, err := marketLine(market.MarketType, snap.Quotes)
	if err != nil {
		return builtSnapshot{}, unmeasurable(leg.LegID, incoherent, err)
	}

	if requireLegsQuote {
		if err := matchesLegsOwnQuote(leg, snap.Quotes); err != nil {
			return builtSnapshot{}, unmeasurable(leg.LegID, ReasonTakenQuoteMismatch, err)
		}
	}

	return builtSnapshot{quotes: snap.Quotes, line: line, observedAt: newestInstant(snap.Quotes)}, nil
}

// matchesLegsOwnQuote checks that the reconstruction found the leg's own quote
// for the leg's own selection.
//
// prices_natural_key_idx is UNIQUE on (selection_id, book_id, observed_at), and
// the snapshot query fixes the first two, so equality of the instant identifies
// the row uniquely — the price is then pinned as a consequence rather than as a
// second check. It is compared anyway, because the two values travel here through
// different columns of different tables (legs.price_decimal against
// prices.decimal_odds) and a disagreement between them is a copy that went wrong
// at placement, which is worth catching at the one place that ever compares them.
func matchesLegsOwnQuote(leg Leg, quotes []Quote) error {
	for _, q := range quotes {
		if q.Selection != leg.SelectionID {
			continue
		}
		if !q.ObservedAt.Equal(leg.ObservedAt) {
			return fmt.Errorf("reconstructed quote for selection %s was observed at %s, "+
				"the leg was booked off a quote observed at %s",
				leg.SelectionID,
				q.ObservedAt.UTC().Format(time.RFC3339Nano),
				leg.ObservedAt.UTC().Format(time.RFC3339Nano))
		}
		if float64(q.Decimal) != float64(leg.Decimal) {
			return fmt.Errorf("reconstructed quote for selection %s is %g, the leg was booked at %g",
				leg.SelectionID, float64(q.Decimal), float64(leg.Decimal))
		}
		return nil
	}
	return fmt.Errorf("selection %s is not priced in the reconstructed market", leg.SelectionID)
}

// marketLine converts every quote's line into the MARKET's frame and returns the
// one they agree on.
//
// domain.Price stores a line "from the selection's own perspective — the value
// EffectiveLine returns, already inverted for an away spread", and
// odds.FairMarketSnapshot wants the market's own value. The conversion is
// therefore exactly the inverse of domain.EffectiveLine, which inverts for one
// case and one case only: the away side of a SPREAD. Totals and player props
// share an absolute threshold across both sides; moneylines and futures have no
// line at all.
//
// Disagreement is refused rather than resolved. doc.go §7: a snapshot holding the
// home side at −3.5 and the away side at +3 is not a market, and picking one of
// them would silently score a wager against a line the market never had.
func marketLine(mt domain.MarketType, quotes []Quote) (domain.Line, error) {
	if len(quotes) == 0 {
		return domain.NoLine(), errors.New("no quotes")
	}

	var (
		agreed domain.Line
		first  = true
	)
	for _, q := range quotes {
		line := q.Line
		if mt == domain.MarketTypeSpread && q.Role == domain.SelectionRoleAway {
			line = line.Invert()
		}
		if first {
			agreed, first = line, false
			continue
		}
		if !agreed.Equal(line) {
			return domain.NoLine(), fmt.Errorf(
				"selection %s puts the market line at %s, an earlier selection put it at %s",
				q.Selection, line, agreed)
		}
	}
	return agreed, nil
}

// newestInstant returns the maximum observed_at across the quotes.
//
// doc.go §8: a snapshot assembled from n quotes with n instants is true, as a
// whole, only from the newest of them, and that instant is a provider
// observation — which is what lets migrations/00009 assert closed_at >= taken_at
// as a database constraint over two values from one clock.
func newestInstant(quotes []Quote) time.Time {
	var newest time.Time
	for _, q := range quotes {
		if q.ObservedAt.After(newest) {
			newest = q.ObservedAt
		}
	}
	return newest.UTC()
}

// -----------------------------------------------------------------------------
// The devig
// -----------------------------------------------------------------------------

// devigBoth removes the margin from both snapshots with ONE method.
//
// The configured method is tried on both sides. If either side refuses it, BOTH
// are recomputed with multiplicative — never just the failing side, because
// wager_leg_clv has one devig_method column precisely so that a Shin-devigged
// take can never be compared against a multiplicatively devigged close.
//
// Multiplicative is the fallback for internal/pricing/fairvalue.go's reason: it
// is TOTAL, so a market it also refuses is a market whose prices are not a
// market. It is reached only explicitly, and it is recorded on the row, because
// devig.go calls it "the worst possible silent default" and this is what stops it
// being silent.
func devigBoth(
	method odds.DevigMethod, taken, closing []Quote,
) (odds.DevigMethod, []odds.Probability, []odds.Probability, error) {
	takenImplied, err := impliedOf(taken)
	if err != nil {
		return odds.MethodUnknown, nil, nil, fmt.Errorf("taken snapshot: %w", err)
	}
	closingImplied, err := impliedOf(closing)
	if err != nil {
		return odds.MethodUnknown, nil, nil, fmt.Errorf("closing snapshot: %w", err)
	}

	takenRes, takenErr := odds.Devig(method, takenImplied)
	closingRes, closingErr := odds.Devig(method, closingImplied)
	if takenErr == nil && closingErr == nil {
		return method, takenRes.Probabilities, closingRes.Probabilities, nil
	}

	takenRes, takenErr = odds.Devig(odds.MethodMultiplicative, takenImplied)
	if takenErr != nil {
		return odds.MethodUnknown, nil, nil, fmt.Errorf(
			"taken snapshot refused %s and the multiplicative fallback: %w", method, takenErr)
	}
	closingRes, closingErr = odds.Devig(odds.MethodMultiplicative, closingImplied)
	if closingErr != nil {
		return odds.MethodUnknown, nil, nil, fmt.Errorf(
			"closing snapshot refused %s and the multiplicative fallback: %w", method, closingErr)
	}
	return odds.MethodMultiplicative, takenRes.Probabilities, closingRes.Probabilities, nil
}

// impliedOf converts the quoted prices into implied probabilities, IN THE ORDER
// THE SNAPSHOT CARRIES THEM.
//
// The order is selection-id order, fixed by the store. It does not change the
// mathematics — every devig method is a symmetric function of its inputs — but it
// does change the last bits of the floating-point reductions inside power and
// Shin, and a stored row that a replay cannot reproduce exactly is a row phase 12
// cannot be validated against.
func impliedOf(quotes []Quote) ([]odds.Probability, error) {
	out := make([]odds.Probability, len(quotes))
	for i, q := range quotes {
		p, err := q.Decimal.Probability()
		if err != nil {
			return nil, fmt.Errorf("selection %s at %g: %w", q.Selection, float64(q.Decimal), err)
		}
		out[i] = p
	}
	return out, nil
}

// fairSnapshot builds the value odds.EvaluateCLV consumes.
//
// NewFairMarketSnapshot is the gate that makes "CLV is computed on devigged
// prices" a property of the type system: it requires the complete outcome set and
// refuses probabilities that do not sum to 1 within odds.CLVDevigTolerance. There
// is deliberately no other path to a FairMarketSnapshot, so nothing in this
// package can hand a vigged price to the arithmetic.
func fairSnapshot(leg Leg, snap builtSnapshot, fair []odds.Probability) (odds.FairMarketSnapshot, error) {
	if len(fair) != len(snap.quotes) {
		return odds.FairMarketSnapshot{}, fmt.Errorf(
			"devig returned %d probabilities for %d quotes", len(fair), len(snap.quotes))
	}
	selections := make([]odds.FairSelection, len(snap.quotes))
	for i, q := range snap.quotes {
		selections[i] = odds.FairSelection{Selection: q.Selection, Fair: fair[i]}
	}
	return odds.NewFairMarketSnapshot(odds.FairMarketSnapshotParams{
		Market:     leg.MarketID,
		Book:       leg.Book,
		Line:       snap.line,
		ObservedAt: snap.observedAt,
		Fair:       selections,
	})
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

// validate reports whether the leg is one this package can act on.
//
// It is checked at the boundary rather than trusted, for the reason
// internal/settlement/ports.go gives about its own refs: everything crossing this
// seam arrives from a database row through an adapter this package does not own,
// and a row with a mis-mapped enum would otherwise be silently filed as an
// analytics exclusion instead of reported as the plumbing fault it is.
func (l Leg) validate() error {
	if _, err := domain.NewLegID(string(l.LegID)); err != nil {
		return fmt.Errorf("%w: %w", ErrUnusableLeg, err)
	}
	if _, err := domain.NewWagerID(string(l.WagerID)); err != nil {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, err)
	}
	if _, err := domain.NewUserID(string(l.UserID)); err != nil {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, err)
	}
	if _, err := domain.NewMarketID(string(l.MarketID)); err != nil {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, err)
	}
	if _, err := domain.NewSelectionID(string(l.SelectionID)); err != nil {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, err)
	}
	if _, err := domain.NewBookID(string(l.Book)); err != nil {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, err)
	}
	if !l.MarketType.Valid() {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, domain.ErrUnknownMarketType)
	}
	if err := l.Decimal.Validate(); err != nil {
		return fmt.Errorf("%w: leg %s: %w", ErrUnusableLeg, l.LegID, err)
	}
	// A PENDING leg has nothing to measure and migrations/00009's leg_status CHECK
	// refuses the value, so it is refused here rather than written and rejected by
	// the database on arrival.
	if !l.Status.IsTerminal() {
		return fmt.Errorf("%w: leg %s is %s; only a graded leg has a closing line value",
			ErrUnusableLeg, l.LegID, l.Status)
	}
	if l.ObservedAt.IsZero() {
		return fmt.Errorf("%w: leg %s has no price observation instant, so the market it was "+
			"booked in cannot be reconstructed", ErrUnusableLeg, l.LegID)
	}
	if l.GradedAt.IsZero() {
		return fmt.Errorf("%w: leg %s is %s with no grading instant", ErrUnusableLeg, l.LegID, l.Status)
	}
	return nil
}

// validateClose reports whether the market has a closing instant at all, and
// whether it is the market the leg claims to be on.
//
// The market type is compared rather than assumed: legs.market_type is a copy
// pinned to the market by a composite foreign key, so a disagreement is
// impossible through the schema and is therefore a sign that this value did not
// come through the schema. Measuring on regardless would apply the wrong line
// rule — a spread read as a moneyline loses its line entirely.
func (mk Market) validateClose(leg Leg) error {
	if mk.MarketType != leg.MarketType {
		return fmt.Errorf("%w: leg %s says market %s is a %s, the market says %s",
			ErrUnusableLeg, leg.LegID, mk.MarketID, leg.MarketType, mk.MarketType)
	}
	if mk.ScheduledStart.IsZero() {
		return unmeasurable(leg.LegID, ReasonNoClose,
			fmt.Errorf("market %s has no scheduled start", mk.MarketID))
	}
	// An event that has not started has not closed, whatever its scheduled start
	// says. `postponed` is the case that matters: domain.EventStatus admits
	// postponed → scheduled, so its scheduled_start has moved to a date in the
	// FUTURE, and reading a close at a future instant would measure the wager
	// against whatever the market happens to be quoting right now.
	switch mk.EventStatus {
	case domain.EventStatusScheduled, domain.EventStatusPostponed, domain.EventStatusUnknown:
		return unmeasurable(leg.LegID, ReasonNoClose,
			fmt.Errorf("event %s is %s, so market %s has not closed", mk.EventID, mk.EventStatus, mk.MarketID))
	}
	return nil
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// positiveOr returns d when it is positive and fallback otherwise.
func positiveOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}
