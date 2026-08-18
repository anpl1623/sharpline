// The engine: one normalized market in, one computed price out.
//
// # It is a pure function of the record
//
// doc.go makes this a requirement of the seam rather than a nicety: the service
// suppresses a republication whose INPUT fingerprint has not changed, which is
// only sound if two calls over the same record produce the same answer. So
// nothing here reads a clock, a database, a cache or a random source. Every
// instant it uses is carried on the record, and state.go explains why the
// staleness anchor is IngestedAt rather than time.Now.
//
// The one consequence worth stating plainly: the engine cannot notice that a
// market has gone stale since it was last priced, because it never runs again
// until the market moves. That is correct rather than a gap. Absolute freshness
// is measured at fanout, where the SLO is defined, and
// `sharpline_odds_staleness_seconds{stage="priced"}` — emitted by the service,
// which does hold a clock — is what shows a priced board ageing.
//
// # The order of operations, and why each step can refuse
//
//  1. Decode and index the record ([NewMarketSnapshot]). A record that does not
//     describe a coherent market is refused, not repaired.
//  2. Resolve the sharp reference book (reference.go). No eligible reference
//     means no fair value, and the market is refused rather than averaged.
//  3. Devig the reference book (fairvalue.go), recording the method that
//     actually produced the answer and how far the other three land from it.
//  4. Score every book against that fair value (ev.go), refusing to compare
//     across a moved line.
//
// A refusal at 2 or 3 returns a typed error and no record. The service counts it
// and publishes nothing, which leaves the previous computed price in place on
// the compacted topic — the honest outcome, since the alternative is publishing
// a fair value derived from something other than a sharp book while claiming
// otherwise.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
)

// Defaults. Each is overridable through [Options]; a zero numeric field means
// the default, and a zero enum field means the default named here.
const (
	// DefaultDevigMethod is Shin.
	//
	// It is the only one of the four with an economic model of WHY a book's
	// margin is asymmetric across a market — Shin derives the margin as the
	// bookmaker's defence against a share z of insider money — so it shades a
	// longshot harder than a favourite for a stated reason rather than as a
	// side effect of the algebra. That is the direction the favourite–longshot
	// bias runs in observed markets, which is what odds/vig.go says too.
	//
	// The alternatives, and why they are not the default:
	//
	//	multiplicative  the crudest model, and devig.go calls it "the method that
	//	                is wrong about longshots, the worst possible silent
	//	                default". It is kept as the explicit fallback precisely
	//	                because it is total.
	//	additive        not total. It drives a long enough shot's fair
	//	                probability to zero or below and returns an error, so a
	//	                board with one big price loses its fair value.
	//	power           a good second choice and genuinely competitive. It is a
	//	                one-parameter fit with no story about what the parameter
	//	                means, where Shin's z is interpretable and auditable.
	//
	// The choice is NOT load-bearing on the strength of this argument, and that
	// is the point of recording it. Every computed record names the method that
	// produced it and carries FairValue.Disagreement, the spread across the
	// methods that could price the same market, so the default can be revised
	// from measurements rather than from taste. The synthetic provider's
	// reference book quotes with a POWER margin by construction, so a deployment
	// against it that wants the generator's exact latent probabilities back sets
	// MethodPower deliberately — see the recovery test.
	DefaultDevigMethod = odds.MethodShin

	// DefaultAttribution is proportional.
	//
	// odds.Attribution has no default of its own, on purpose: its zero value is
	// invalid so an unset attribution fails loudly rather than silently becoming
	// one convention. That discipline is about a bare parameter at a call site.
	// A service configured from the environment must be constructible without
	// every operator naming an attribution convention, so this package makes the
	// choice ONCE, here, and records it on every record it publishes — which is
	// the property the phase-1 rule was protecting.
	//
	// Proportional is chosen because it always succeeds where uniform does not,
	// and because it is the convention the published worked examples use.
	DefaultAttribution = odds.AttributionProportional

	// DefaultKellyMultiplier is quarter Kelly.
	//
	// odds/kelly.go states the reasoning and it applies exactly here: "quarter
	// Kelly is the common choice when the probability comes from a devigged
	// market rather than a validated model, which is exactly this system's
	// situation". Full Kelly is correct only when the probability is right, and
	// a probability devigged out of one book's prices carries the error bar
	// FairValue.Disagreement measures.
	DefaultKellyMultiplier = odds.QuarterKelly

	// DefaultMaxReferenceAge disqualifies a reference book whose oldest quote on
	// a market is more than this old at the record's anchor.
	//
	// It is sized against the pipeline's own cadence, not against a number that
	// sounds fast. ADR 0003 buys a 90-second live poll cadence, and
	// normalizer.DefaultRefreshAfter republishes an unchanged market every 5
	// minutes. So a quote up to one refresh ceiling old is a market that is
	// working normally and simply has not moved. Anything past that means the
	// market has stopped being refreshed rather than stopped moving, and the
	// whole fair value rests on this one book — so the reference gets the
	// tighter of the two bounds.
	DefaultMaxReferenceAge = 5 * time.Minute

	// DefaultMaxQuoteAge disqualifies a challenger book from being scored.
	//
	// Two refresh ceilings. A challenger's staleness costs one book rather than
	// the market, so it gets the looser bound: a book that lags the reference by
	// a few minutes is the normal state of a soft book, and disqualifying it at
	// the reference's threshold would empty the multi-book comparison CLAUDE.md
	// §6 asks for. Past two ceilings the book has stopped being quoted, and an
	// expected value against a price nobody is offering reads as an opportunity
	// and is not one.
	DefaultMaxQuoteAge = 10 * time.Minute
)

// Options configures an [Engine].
type Options struct {
	// ReferenceBooks is the ordered preference list of book slugs to treat as
	// the sharp reference, most preferred first.
	//
	// It is consulted only for books the catalogue has not already designated;
	// reference.go documents the ranking.
	//
	// Empty is legal and means "catalogue designation only", which is now a
	// working configuration rather than an empty surface: the designation
	// travels from the adapter through normalizer.BookRef.Reference and both
	// providers set it. It stays legal rather than required because a service
	// that refused to start without a hard-coded book list would be compiling a
	// trading judgement into a binary.
	ReferenceBooks []domain.Slug

	// DevigMethod selects the margin-removal model. Zero means
	// DefaultDevigMethod.
	DevigMethod odds.DevigMethod

	// Attribution selects the per-selection margin split reported for every
	// book. Zero means DefaultAttribution.
	Attribution odds.Attribution

	// KellyMultiplier is the fraction OF KELLY reported alongside the full
	// fraction. It must lie in (0, 1]; zero means DefaultKellyMultiplier.
	KellyMultiplier float64

	// MaxReferenceAge and MaxQuoteAge are the staleness policy. Zero means the
	// defaults above; negative is rejected.
	MaxReferenceAge time.Duration
	MaxQuoteAge     time.Duration

	// SkipMethodComparison turns off the cross-method disagreement measurement.
	//
	// Negatively named so the zero value leaves it ON, which is the right
	// default: it costs three extra devigs of a market already in cache — tens
	// of microseconds against a p99 budget of 250ms — and it is the only number
	// on the record that says how much the choice of method mattered. A fair
	// probability with no error bar is an opinion presented as a measurement.
	SkipMethodComparison bool

	// Arbitrage and Middles configure the two cross-book scanners CLAUDE.md §3
	// puts on this service alongside the devig. A ZERO VALUE means the package
	// default (DefaultArbitrageConfig / DefaultMiddleConfig) rather than an
	// invalid configuration, because both structs have a zero value that
	// Validate correctly rejects and a service configured from the environment
	// must be constructible without an operator naming a staleness window.
	//
	// The scanners are always on. There is no switch to disable them: they run
	// over a market the pricing pass has already decoded, they read no clock and
	// do no I/O, and a pricer that silently stopped looking for arbitrage would
	// look exactly like one that found none.
	Arbitrage ArbitrageConfig
	Middles   MiddleConfig

	// Registry receives this package's collectors. Nil builds them
	// unregistered, which is right for a unit test and for any process with no
	// /metrics endpoint; the observe calls stay live either way so no call site
	// needs a nil check.
	Registry prometheus.Registerer
}

func (o Options) validate() error {
	if o.DevigMethod != odds.MethodUnknown && !o.DevigMethod.Valid() {
		return fmt.Errorf("%w: devig method %d is not one of the four", ErrInvalidOptions, uint8(o.DevigMethod))
	}
	if o.Attribution != odds.AttributionUnknown && !o.Attribution.Valid() {
		return fmt.Errorf("%w: attribution %d is not a real convention", ErrInvalidOptions, uint8(o.Attribution))
	}
	if o.KellyMultiplier < 0 || o.KellyMultiplier > 1 {
		return fmt.Errorf("%w: kelly multiplier %g is outside (0, 1]", ErrInvalidOptions, o.KellyMultiplier)
	}
	if o.MaxReferenceAge < 0 {
		return fmt.Errorf("%w: MaxReferenceAge is negative", ErrInvalidOptions)
	}
	if o.MaxQuoteAge < 0 {
		return fmt.Errorf("%w: MaxQuoteAge is negative", ErrInvalidOptions)
	}
	for i, slug := range o.ReferenceBooks {
		if _, err := domain.NewSlug(string(slug)); err != nil {
			return fmt.Errorf("%w: ReferenceBooks[%d]: %w", ErrInvalidOptions, i, err)
		}
	}
	return nil
}

// Engine applies the phase-1 odds mathematics to one market at a time.
//
// It is immutable after construction and safe for concurrent use: it holds
// configuration and a metric set and no per-market state whatsoever.
type Engine struct {
	prefer      []domain.Slug
	method      odds.DevigMethod
	attribution odds.Attribution
	kelly       float64
	maxRefAge   time.Duration
	maxQuoteAge time.Duration
	compare     bool

	arb *ArbitrageScanner
	mid *MiddleScanner

	m *EngineMetrics
}

// NewEngine builds an engine. It does no I/O.
func NewEngine(o Options) (*Engine, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	m, err := NewEngineMetrics(o.Registry)
	if err != nil {
		return nil, err
	}

	e := &Engine{
		prefer:      append([]domain.Slug(nil), o.ReferenceBooks...),
		method:      o.DevigMethod,
		attribution: o.Attribution,
		kelly:       o.KellyMultiplier,
		maxRefAge:   o.MaxReferenceAge,
		maxQuoteAge: o.MaxQuoteAge,
		compare:     !o.SkipMethodComparison,
		m:           m,
	}
	if e.method == odds.MethodUnknown {
		e.method = DefaultDevigMethod
	}
	if e.attribution == odds.AttributionUnknown {
		e.attribution = DefaultAttribution
	}
	if e.kelly == 0 {
		e.kelly = DefaultKellyMultiplier
	}
	if e.maxRefAge == 0 {
		e.maxRefAge = DefaultMaxReferenceAge
	}
	if e.maxQuoteAge == 0 {
		e.maxQuoteAge = DefaultMaxQuoteAge
	}

	arbCfg := o.Arbitrage
	if arbCfg == (ArbitrageConfig{}) {
		arbCfg = DefaultArbitrageConfig()
	}
	if e.arb, err = NewArbitrageScanner(arbCfg); err != nil {
		return nil, err
	}
	midCfg := o.Middles
	if midCfg == (MiddleConfig{}) {
		midCfg = DefaultMiddleConfig()
	}
	if e.mid, err = NewMiddleScanner(midCfg); err != nil {
		return nil, err
	}
	return e, nil
}

// ConfigDigest is a stable short hash of EVERY setting that can change what this
// engine computes, defaults resolved.
//
// It exists because the service composes it into the message id it publishes and
// suppresses a republication whose id has not changed. Comparing the source
// fingerprint alone identifies the INPUT and says nothing about the function
// applied to it: change the devig method, the attribution, the Kelly multiplier,
// a staleness bound or a scanner's window, and every market that has not moved
// keeps the numbers computed under the old configuration — for a futures market,
// for ever.
//
// So it covers all of them, read back off the BUILT engine rather than off the
// caller's Options, which is what makes a default resolved inside NewEngine part
// of the digest. The earlier version hashed only the devig method and the
// reference list, and its own comment recorded the gap; this closes it.
//
// FNV-64a rather than SHA-256: it is a change detector, not a security boundary,
// and 16 hex characters keep a composed message id far inside the bus's
// identifier limit. Fields are separated by 0x1f, a byte no slug or enum name can
// contain, so ("ab","c") and ("a","bc") cannot digest alike.
func (e *Engine) ConfigDigest() string {
	h := fnv.New64a()
	sep := func() { _, _ = h.Write([]byte{0x1f}) }
	put := func(s string) { _, _ = h.Write([]byte(s)); sep() }

	put(e.method.String())
	put(e.attribution.String())
	put(strconv.FormatFloat(e.kelly, 'g', -1, 64))
	put(e.maxRefAge.String())
	put(e.maxQuoteAge.String())
	put(strconv.FormatBool(e.compare))
	for _, slug := range e.prefer {
		put(string(slug))
	}

	a := e.arb.Config()
	put(a.MaxLegAge.String())
	put(a.MaxLegSpread.String())
	put(strconv.FormatFloat(a.MinReturn, 'g', -1, 64))
	put(strconv.Itoa(a.MinDistinctBooks))

	m := e.mid.Config()
	put(m.MaxLegAge.String())
	put(m.MaxLegSpread.String())
	put(strconv.FormatFloat(m.MinWindow, 'g', -1, 64))
	put(strconv.FormatBool(m.RequireIntegerOutcome))
	put(strconv.FormatBool(m.RequireDistinctBooks))

	// The schema version is part of the identity because a record written under
	// a different document shape is a different record even when every number in
	// it is the same.
	put(strconv.Itoa(SchemaVersion))

	return strconv.FormatUint(h.Sum64(), 16)
}

// ArbitrageConfig returns the arbitrage scanner's configuration, defaults
// resolved. It is exported so a caller can report the staleness window a finding
// was produced under rather than assume it.
func (e *Engine) ArbitrageConfig() ArbitrageConfig { return e.arb.Config() }

// MiddleConfig returns the middles scanner's configuration, defaults resolved.
func (e *Engine) MiddleConfig() MiddleConfig { return e.mid.Config() }

// DevigMethod returns the method this engine is configured with.
func (e *Engine) DevigMethod() odds.DevigMethod { return e.method }

// ReferenceBooks returns the configured preference list. The slice is freshly
// allocated, so a caller cannot reorder the engine's own copy.
func (e *Engine) ReferenceBooks() []domain.Slug {
	return append([]domain.Slug(nil), e.prefer...)
}

// Price computes one market's fair value and per-book assessment.
//
// It satisfies the shape the service's PriceFunc seam expects — a decoded
// normalizer record in, a concrete payload out — and returns a typed error the
// caller can classify with errors.Is when the market cannot be priced.
//
// The context is checked once on entry and then not consulted. This function
// does no I/O and no unbounded work: the cost is linear in books × selections
// and is measured in microseconds, so a mid-computation cancellation check would
// cost more than the computation. Checking on entry is not ceremony — it is what
// stops a whole backlog of queued markets from being priced after the process
// has been told to stop.
func (e *Engine) Price(ctx context.Context, rec normalizer.NormalizedMarket) (ComputedMarket, error) {
	if err := ctx.Err(); err != nil {
		return ComputedMarket{}, fmt.Errorf("pricing: market %q: %w", rec.Market.ID, err)
	}

	snap, err := NewMarketSnapshot(rec)
	if err != nil {
		e.m.observeMarket(MarketResultUndecodable)
		return ComputedMarket{}, err
	}
	if !snap.Priceable() {
		e.m.observeMarket(MarketResultNotPriceable)
		return ComputedMarket{}, fmt.Errorf("pricing: market %s has %d selection(s), need at least %d: %w",
			snap.Market.ID(), len(snap.Selections), odds.MinMarketSelections, ErrMarketNotPriceable)
	}

	ref, err := resolveReference(snap, e.prefer, e.maxRefAge)
	if err != nil {
		e.m.observeMarket(referenceFailureResult(err))
		return ComputedMarket{}, err
	}
	e.m.observeReference(ref.source)

	fv, err := computeFairValue(snap, ref.state, e.method, e.attribution, e.compare)
	if err != nil {
		e.m.observeMarket(MarketResultDevigFailed)
		return ComputedMarket{}, err
	}
	if fv.Fallback {
		e.m.observeDevigFallback(fv.RequestedMethod, fv.Method)
	}

	index := fairIndex(fv)
	books := make([]BookAssessment, 0, len(snap.Books))
	for _, b := range snap.Books {
		a := assessBook(snap, b, ref.state, fv, index, e.kelly, e.attribution, e.maxQuoteAge)
		if err := assertQuoteCount(a, len(snap.Selections)); err != nil {
			// Unreachable: finaliseBook sets Complete from the same two lengths.
			// Returned rather than ignored because a book whose assessments and
			// quotes have drifted apart would silently mis-attribute an edge to
			// the wrong selection.
			e.m.observeMarket(MarketResultUndecodable)
			return ComputedMarket{}, err
		}
		books = append(books, a)
	}

	// The cross-book scan. A failure here is a DIAGNOSTIC failure: the devig
	// above already succeeded and its fair value is still correct, so the record
	// ships without findings rather than the market losing its price because an
	// arbitrage could not be looked for. signals.go argues this at length.
	arbs, mids, scanErr := e.scan(snap)
	if scanErr != nil {
		e.m.observeScanError()
	}

	e.m.observeMarket(MarketResultPriced)
	e.m.observeFairValue(fv)
	e.m.observeBooks(books)
	e.m.observeSignals(arbs, mids)

	out := newComputedMarket(rec, snap, ref, fv, books, e.attribution)
	out.Arbitrage = arbs
	out.Middles = mids
	return out, nil
}

// referenceFailureResult maps a reference-resolution error onto a bounded metric
// label. Classification is on sentinels, never on message text — the same rule
// the normalizer's Reason follows, for the same reason.
func referenceFailureResult(err error) MarketResult {
	switch {
	case errors.Is(err, ErrReferenceStale):
		return MarketResultReferenceStale
	case errors.Is(err, ErrIncompleteReference):
		return MarketResultReferenceIncomplete
	default:
		return MarketResultNoReference
	}
}
