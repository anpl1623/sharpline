// Prometheus instrumentation for the ENGINE, as distinct from the service.
//
// # The split, and why it is not arbitrary
//
// metrics.go owns the numbers the service can see from outside the seam: how
// long a price took (`sharpline_pricing_duration_seconds`, the contract series
// the dashboard's "Pricing latency" panel and the PricingLatencyHigh alert are
// written against), how fresh the data was at this stage
// (`sharpline_odds_staleness_seconds{stage="priced"}` and
// `sharpline_pipeline_latency_seconds{stage="priced"}`), and whether a market
// was published or suppressed.
//
// This file owns the numbers only the MATHEMATICS knows, and none of them can be
// derived from the outside because the service receives the payload as `any`:
//
//	why a market got no fair value at all
//	which book was chosen as sharp, and by which designation
//	how many quotes were disqualified, and for which of the three reasons
//	how far the four devig methods disagreed on this market
//	what margin the sharp book is actually running
//
// Nothing here duplicates a series metrics.go owns, and nothing here contributes
// to one. Two collectors observing the same histogram from inside and outside
// the same call would double its count and pull its quantiles toward the inner
// measurement, which is worse than either measurement alone.
//
// # Labels deliberately not set
//
//   - `service`. deploy/observability/prometheus.yml attaches it as a TARGET
//     label; a metric label of the same name is renamed `exported_service` and
//     the two drift. Every other package in this repository makes the same call.
//   - a book slug. It is the label an operator most wants — "Lowtide is
//     permanently stale" is a sentence a dashboard should be able to say — and
//     it is left off anyway, because it multiplies against every status on a
//     provider that serves twenty-plus bookmakers and the same question is
//     answerable from the computed records themselves. Adding it is a deliberate
//     cardinality decision, not a convenience.
//   - a market, event or selection identifier. Tens of thousands of values.
//   - error text. Bounded classifications only; the result and status types are
//     closed sets and every value is written by exactly one branch.
package pricing

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Metric namespace and subsystem: together, the `sharpline_pricer_` prefix.
//
// The subsystem is `pricer` and not `pricing` on purpose, so that no name here
// can collide with the frozen contract series `sharpline_pricing_duration_seconds`
// or with anything the service adds beside it. deploy/observability/prometheus.yml
// states the rule these both satisfy: every application series is prefixed
// `sharpline_`.
const (
	engineMetricNamespace = "sharpline"
	engineMetricSubsystem = "pricer"
)

// MarketResult is the outcome of trying to price one market. A closed set: every
// value is written by exactly one branch of Engine.Price.
type MarketResult string

// The market results.
const (
	// MarketResultPriced is a market that produced a fair value.
	MarketResultPriced MarketResult = "priced"

	// MarketResultNoReference is a market no designated sharp book quoted. On a
	// healthy deployment this is the count of markets outside the reference
	// book's coverage — routine on props and futures, and NOT routine on a main
	// line. A board where this dominates means the reference designation is not
	// reaching the pricer, which is exactly the KNOWN DEFECT reference.go
	// records.
	MarketResultNoReference MarketResult = "no_reference"

	// MarketResultReferenceStale is a market whose reference book quoted it but
	// whose quote is older than MaxReferenceAge. This is the staleness policy
	// firing on the input everything else depends on.
	MarketResultReferenceStale MarketResult = "reference_stale"

	// MarketResultReferenceIncomplete is a market whose reference book did not
	// quote every selection, so there is no complete market to devig.
	MarketResultReferenceIncomplete MarketResult = "reference_incomplete"

	// MarketResultDevigFailed is a market that the configured method AND the
	// multiplicative fallback both refused. It should be zero; a non-zero rate
	// means prices are reaching the engine that are not a market.
	MarketResultDevigFailed MarketResult = "devig_failed"

	// MarketResultNotPriceable is a market with fewer than
	// odds.MinMarketSelections selections.
	MarketResultNotPriceable MarketResult = "not_priceable"

	// MarketResultUndecodable is a record that did not survive the domain
	// constructors, or whose books, selections and prices do not agree with each
	// other. It is a corrupt record, not a market.
	MarketResultUndecodable MarketResult = "undecodable"
)

// bookResult labels the book-level outcome of the staleness policy. Two values:
// a book is either scored or disqualified by age. Incompleteness is a quote-level
// fact and is counted there.
const (
	bookResultPriced = "priced"
	bookResultStale  = "stale"
)

// disagreementBuckets covers the largest absolute probability difference between
// devig methods on one market.
//
// The interesting region is small and the tail is the point. On a balanced
// two-way market the four methods agree to well under a tenth of a percentage
// point; on a market with a genuine longshot they can differ by several
// percentage points, which is the case CLAUDE.md §4 means by "disagree
// meaningfully on longshots". The boundaries straddle both so the histogram
// distinguishes "the method choice does not matter here" from "the method choice
// is the biggest term in this fair value".
var disagreementBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25,
}

// overroundBuckets covers the reference book's own margin, S − 1.
//
// A sharp book on a major line runs 1.5–3%; the synthetic in-house reference is
// configured at 2.0%; a soft book runs 5–7%. The boundaries resolve that range
// finely because the whole claim of a "sharp reference" is that its margin is
// small, and a reference book whose overround drifts into soft territory is a
// reference book that has stopped being one — which no other signal would show.
var overroundBuckets = []float64{
	0.0025, 0.005, 0.01, 0.015, 0.02, 0.025, 0.03, 0.04, 0.05, 0.075, 0.1, 0.15, 0.25,
}

// EngineMetrics is the engine's collector set.
//
// Registration happens once, in NewEngineMetrics, and the value is injected —
// the pattern internal/platform/kafka, internal/ingest/provider,
// internal/ingest/normalizer and internal/ingest/writer all follow, for the same
// reason: one process may legitimately build more than one Engine, and duplicate
// registration should fail its startup rather than its code review.
//
// A nil Registerer builds the collectors WITHOUT registering them. That is right
// for a unit test and for any process with no /metrics endpoint: the observe
// calls stay live and cost a few nanoseconds, so no call site needs a nil check.
// Every method is nil-receiver safe for the same reason.
type EngineMetrics struct {
	markets      *prometheus.CounterVec
	reference    *prometheus.CounterVec
	books        *prometheus.CounterVec
	quotes       *prometheus.CounterVec
	fallback     *prometheus.CounterVec
	disagreement prometheus.Histogram
	overround    prometheus.Histogram
	quoteAge     prometheus.Histogram

	scanned    *prometheus.CounterVec
	scanErrors prometheus.Counter
	arbReturn  prometheus.Histogram
	midWindow  prometheus.Histogram
}

// NewEngineMetrics builds the collectors and registers them on reg.
//
// It returns an error rather than panicking: CLAUDE.md §12 forbids a panic
// outside main, and a registration conflict is a wiring mistake the caller
// reports with the rest of its startup context.
func NewEngineMetrics(reg prometheus.Registerer) (*EngineMetrics, error) {
	m := &EngineMetrics{
		markets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "fair_value_total",
			Help: "Markets the engine attempted to price, by outcome. Everything except " +
				"result=\"priced\" produced NO fair value and therefore no computed record — " +
				"CLAUDE.md §6's +EV finder measures against a sharp reference book, and a " +
				"market with no eligible reference is refused rather than quietly scored " +
				"against a consensus of the books that happen to be present.",
		}, []string{"result"}),

		reference: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "reference_book_total",
			Help: "Reference-book resolutions by designation. source=\"catalogue\" is the " +
				"provider layer's own flag (domain.Book.IsReference); source=\"configured\" " +
				"is this service's preference list. While the catalogue flag is lost at the " +
				"adapter to raw boundary this reads 100% configured, which is how that " +
				"defect is visible rather than invisible.",
		}, []string{"source"}),

		books: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "books_total",
			Help: "Books assessed on a priced market, by whether the staleness policy " +
				"admitted them. A stale book keeps its prices on the record but is given " +
				"no expected value: an EV against a price nobody is offering reads as an " +
				"opportunity and is not one.",
		}, []string{"result"}),

		quotes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "quotes_total",
			Help: "Individual book quotes by scoring status. status=\"line_mismatch\" is the " +
				"engine refusing to score a price quoted at a different line from the " +
				"reference book's — the same rule odds.CLV applies to a wager across a " +
				"moved line — so a high rate here is book disagreement about the LINE and " +
				"is the middles detector's input, not a defect.",
		}, []string{"status"}),

		fallback: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "devig_fallback_total",
			Help: "Markets where the configured devig method refused and the multiplicative " +
				"fallback produced the fair value instead. Multiplicative is the method " +
				"that is wrong about longshots, so a non-zero rate means those markets' " +
				"fair values are the least trustworthy on the board.",
		}, []string{"from", "to"}),

		disagreement: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "devig_disagreement",
			Help: "Largest absolute probability difference between the devig method used and " +
				"any other method that could price the same market. It is the error bar on " +
				"a fair probability: an edge of 1% on a market where the methods span 3 " +
				"percentage points is not a signal.",
			Buckets: disagreementBuckets,
		}),

		overround: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "reference_overround",
			Help: "The reference book's overround (S-1) on each priced market. NOT the vig, " +
				"which is (S-1)/S and is a smaller number; the two differ by enough to " +
				"mis-state a margin by a relative 5%. A reference book drifting into soft " +
				"territory is a reference book that has stopped being one.",
			Buckets: overroundBuckets,
		}),

		quoteAge: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "quote_age_seconds",
			Help: "Age of each assessed quote at the record's own anchor (ingested_at), not " +
				"at the wall clock — the engine is a pure function of its input. This is " +
				"the distribution the staleness policy's thresholds are sized against; the " +
				"wall-clock SLO is sharpline_odds_staleness_seconds{stage=\"priced\"}.",
			Buckets: provider.StalenessBuckets(),
		}),

		// The two scanners' output, counted separately from the fair value
		// because "the pricer is running" and "the pricer is finding anything"
		// are different questions and the second one is CLAUDE.md §6's
		// differentiator. A permanently zero arbitrage counter over a feed with
		// real book disagreement is a signal; the absence of a counter is not.
		scanned: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "signals_total",
			Help: "Cross-book findings published, by kind. arbitrage = an under-round line " +
				"group (cannot lose); middle = two quotes at different lines leaving a " +
				"window that wins both (can lose its margin, and is never merged with the " +
				"first). Both are counted per FINDING, not per market.",
		}, []string{"kind"}),

		scanErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "scan_errors_total",
			Help: "Markets whose fair value was computed but whose cross-book scan was " +
				"refused. Non-zero means the analytics surface is incomplete for a reason " +
				"no other series shows, because the record still validates and still ships.",
		}),

		arbReturn: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "arbitrage_return",
			Help: "Guaranteed profit per unit of total outlay, (1-S)/S, on each arbitrage " +
				"found. Buckets run fine because a real cross-book arbitrage is thin; a " +
				"fat one is nearly always a stale leg the spread bound should have caught.",
			Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.02, 0.03, 0.05, 0.1},
		}),

		midWindow: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: engineMetricNamespace,
			Subsystem: engineMetricSubsystem,
			Name:      "middle_window_points",
			Help: "Width of each middle's window in line points. Wider is better and much " +
				"rarer; the count matters more than the shape while the feed is synthetic.",
			Buckets: []float64{0.5, 1, 1.5, 2, 3, 4, 5, 7, 10},
		}),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.markets, m.reference, m.books, m.quotes, m.fallback,
		m.disagreement, m.overround, m.quoteAge,
		m.scanned, m.scanErrors, m.arbReturn, m.midWindow,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("pricing: engine metrics: %w", err)
		}
	}
	return m, nil
}

// observeMarket counts one market-level outcome.
func (m *EngineMetrics) observeMarket(r MarketResult) {
	if m == nil {
		return
	}
	m.markets.WithLabelValues(string(r)).Inc()
}

// observeReference counts one reference-book resolution.
func (m *EngineMetrics) observeReference(s ReferenceSource) {
	if m == nil {
		return
	}
	m.reference.WithLabelValues(s.String()).Inc()
}

// observeDevigFallback counts one fall back to the multiplicative method.
func (m *EngineMetrics) observeDevigFallback(from, to odds.DevigMethod) {
	if m == nil {
		return
	}
	m.fallback.WithLabelValues(from.String(), to.String()).Inc()
}

// observeSignals records one market's cross-book findings.
//
// A market with no findings observes nothing rather than a zero, because a
// counter that ticked on every market would make "scanned 40,000 markets" and
// "found 40,000 arbitrages" indistinguishable at a glance on a dashboard.
func (m *EngineMetrics) observeSignals(arbs []ArbitrageRef, mids []MiddleRef) {
	if m == nil {
		return
	}
	for _, a := range arbs {
		m.scanned.WithLabelValues("arbitrage").Inc()
		m.arbReturn.Observe(a.Return)
	}
	for _, mid := range mids {
		m.scanned.WithLabelValues("middle").Inc()
		m.midWindow.Observe(mid.WidthPoints)
	}
}

// observeScanError counts one market whose fair value shipped without findings.
func (m *EngineMetrics) observeScanError() {
	if m == nil {
		return
	}
	m.scanErrors.Inc()
}

// observeFairValue records the two diagnostics of a finished devig.
//
// A negative Disagreement means the comparison was switched off, and is skipped
// rather than observed: a histogram cannot represent "not measured", and
// observing a sentinel would put a spike at the bottom bucket that reads as
// perfect agreement.
func (m *EngineMetrics) observeFairValue(fv FairValue) {
	if m == nil {
		return
	}
	if fv.Disagreement >= 0 {
		m.disagreement.Observe(fv.Disagreement)
	}
	m.overround.Observe(fv.Margin.Overround)
}

// observeBooks records the book- and quote-level outcomes of one priced market.
//
// Quote ages are CLAMPED at zero before observation and are not clamped on the
// record itself. A provider clock running ahead of ours produces a negative age
// that a histogram would file in the lowest bucket and report as excellent
// freshness, which sharpline-alerts.yml calls out as the failure to avoid. The
// clamp is not silent: the service counts the same condition against
// sharpline_odds_clock_skew_total, and QuoteAssessment.AgeSeconds keeps the
// signed value so the skew is legible on the record.
func (m *EngineMetrics) observeBooks(books []BookAssessment) {
	if m == nil {
		return
	}
	for _, b := range books {
		result := bookResultPriced
		if !b.Eligible {
			result = bookResultStale
		}
		m.books.WithLabelValues(result).Inc()
		for _, q := range b.Quotes {
			m.quotes.WithLabelValues(string(q.Status)).Inc()
			m.quoteAge.Observe(max(q.AgeSeconds, 0))
		}
	}
}
