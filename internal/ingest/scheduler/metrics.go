package scheduler

import (
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/prometheus/client_golang/prometheus"
)

// Metric namespace and subsystems. Together they produce the `sharpline_ingest_`
// and `sharpline_provider_` prefixes deploy/observability already references.
const (
	metricNamespace       = "sharpline"
	metricSubsystemIngest = "ingest"
)

// Poll result label values. A closed set: every one is written by exactly one
// branch of recordPoll below and nothing else ever reaches these labels.
//
// resultUnchanged is the one the dashboard singles out — "result=\"unchanged\"
// is the change-detection hash suppressing a no-op poll (CLAUDE.md §5). A
// healthy pipeline is mostly unchanged; a system where everything is
// \"changed\" means the hash is broken and the bus is carrying junk."
const (
	resultChanged   = "changed"
	resultUnchanged = "unchanged"
	resultEmpty     = "empty"
	resultError     = "error"
)

// Catalogue refresh outcomes.
const (
	outcomeOK    = "ok"
	outcomeError = "error"
)

// pollBuckets bounds one provider sweep. It runs to 30s because
// DefaultPollTimeout is 20s and a sweep in the top bucket is one that is being
// abandoned; it starts at 5ms because the synthetic generator answers in
// microseconds and a histogram whose floor is above the common case reports
// nothing useful for the offline path.
var pollBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30,
}

// quotaWaitBuckets measures time blocked on the shared limiter. It runs to
// 300s: at ADR 0003's recommended tier the average refill is 0.039 credits/s,
// so a 3-credit sweep that arrives at an empty bucket waits 77 seconds, and a
// histogram that topped out below that would report every real wait as an
// overflow.
var quotaWaitBuckets = []float64{
	0.001, 0.01, 0.1, 0.5, 1, 5, 15, 30, 60, 120, 300,
}

// Metrics holds every collector this package owns.
//
// # Four of these names are a CONTRACT with deploy/observability
//
// prometheus.yml states the rule ("every application series is prefixed
// `sharpline_`") and the dashboard plus rules/sharpline-alerts.yml are written
// against specific names, matched character for character with the labels their
// PromQL aggregates by. Renaming one, or dropping a label its PromQL groups by,
// breaks a panel or an alert SILENTLY — Prometheus answers a query for a series
// that no longer exists with no data rather than with an error.
//
//	sharpline_ingest_poll_interval_seconds{provider,league,window}
//	    Dashboard panel "Polling cadence & clock skew" (legend
//	    "{{provider}} {{league}} {{window}}").
//	    Recording rule sharpline:odds_staleness_objective_seconds:p99 =
//	        max(sharpline_ingest_poll_interval_seconds{window="live"}) * 1.25 + 2
//	    Alert OddsPollCadenceUnknown fires on absent(...{window="live"}).
//
//	    THIS IS HALF OF THE HEADLINE SLO'S THRESHOLD. The freshness objective is
//	    cadence-relative rather than a constant, so without this gauge the
//	    objective rule evaluates to empty and OddsFreshnessBeyondCadence can
//	    never fire — a silently unguarded SLO. It reports the CONFIGURED
//	    interval for every window of every managed league, not the momentarily
//	    backed-off one, because that is what the alert header and the panel
//	    description both specify ("the configured live poll interval, which is
//	    the INPUT to the freshness objective") and because a value that moved
//	    with backoff would make the SLO threshold chase the thing it is
//	    supposed to be judging.
//
//	sharpline_ingest_polls_total{provider,league,window,result}
//	    Dashboard panel "Ingest — poll outcomes":
//	        sum by (result) (rate(sharpline_ingest_polls_total{provider=~"$provider"}[...]))
//	    league and window aggregate away under that sum and are here because
//	    "which league is erroring" is the first question the panel provokes.
//
// # Series deliberately NOT emitted here, and who owns them instead
//
//   - sharpline_provider_quota_remaining / _limit / sharpline_provider_requests_total.
//     internal/ingest/provider owns all three and emits them from the
//     PROVIDER'S OWN x-requests-remaining header, which ADR 0003 makes
//     authoritative over any local estimate. Registering them here as well
//     would fail the process at startup — which is the correct outcome and is
//     why this package does not try. The scheduler's local pacing ledger is
//     exported under its own sharpline_ingest_budget_* names below, and it
//     feeds the provider's number back into that ledger through
//     [Budget.Reconcile] rather than publishing a competing gauge.
//   - sharpline_odds_staleness_seconds and sharpline_odds_clock_skew_total.
//     They are per-PRICE observations of (stage instant − observed_at) and the
//     scheduler never sees a price. The adapter and the normalizer own them.
//   - `service`. prometheus.yml attaches it as a target label on every scrape
//     job; a metric label of the same name would be renamed `exported_service`
//     and the two would drift.
type Metrics struct {
	// ---- contract series ----
	polls        *prometheus.CounterVec // provider, league, window, result
	pollInterval *prometheus.GaugeVec   // provider, league, window

	// ---- local budget ledger (NOT the sharpline_provider_quota_* contract) ----
	budgetLeft  *prometheus.GaugeVec // provider
	budgetLimit *prometheus.GaugeVec // provider

	// ---- internal to this package ----
	pollDuration     *prometheus.HistogramVec // provider, league, result
	quotaWait        *prometheus.HistogramVec // provider
	quotaBlocked     *prometheus.CounterVec   // provider, league
	marketsObserved  *prometheus.CounterVec   // provider, league
	marketsChanged   *prometheus.CounterVec   // provider, league
	backoffSteps     *prometheus.GaugeVec     // provider, league
	leaguesScheduled *prometheus.GaugeVec     // provider, window
	catalogueRuns    *prometheus.CounterVec   // provider, outcome
	catalogueEvents  *prometheus.GaugeVec     // provider
}

// NewMetrics builds the collectors and registers them on reg.
//
// Call it once per process and inject the result. reg may be nil, which builds
// the collectors but registers nothing — correct for a unit test and for any
// caller that serves no /metrics endpoint. Every observe call site is
// unconditional, so nothing needs a nil check.
//
// Registration failure is returned rather than swallowed. If a provider adapter
// in the same process also registers sharpline_provider_quota_*, that is a
// startup failure and it should be: two halves of one process reporting the
// same series under different label sets produces plausible nonsense, and this
// package's [Budget] is the single accounting the ProviderQuotaLow ratio has to
// come from.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	counter := func(sub, name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: sub, Name: name, Help: help,
		}, labels)
	}
	gauge := func(sub, name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Subsystem: sub, Name: name, Help: help,
		}, labels)
	}
	histogram := func(sub, name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: sub, Name: name, Help: help, Buckets: buckets,
		}, labels)
	}

	m := &Metrics{
		polls: counter(metricSubsystemIngest, "polls_total",
			"Provider sweeps that completed, by league, window and outcome. result=\"unchanged\" is change "+
				"detection suppressing a no-op (CLAUDE.md §5) and is expected to dominate outside live play; "+
				"result=\"empty\" is a league whose payload carried no markets at all, which is a real state "+
				"(no fixtures) and not an error. Backing off on unchanged is what saves credits — the credit for "+
				"THIS poll was already spent when the request went out. "+
				"Panel: sum by (result) (rate(sharpline_ingest_polls_total[$__rate_interval])).",
			"provider", "league", "window", "result"),

		pollInterval: gauge(metricSubsystemIngest, "poll_interval_seconds",
			"CONFIGURED polling interval, in seconds, for each window of each managed league. This is the INPUT to "+
				"the headline freshness SLO: sharpline:odds_staleness_objective_seconds:p99 is "+
				"max(...{window=\"live\"}) × 1.25 + 2, so without a live series the objective is empty and "+
				"OddsFreshnessBeyondCadence can never fire. It reports the configured cadence, not the "+
				"momentarily backed-off one — a threshold that moved with backoff would chase the thing it judges.",
			"provider", "league", "window"),

		budgetLeft: gauge(metricSubsystemIngest, "budget_remaining",
			"Credits left in the scheduler's own budget ledger for the current period — the number the token "+
				"bucket paces against and refuses sweeps on. It follows the provider's x-requests-remaining once "+
				"an adapter has reported one. It is NOT sharpline_provider_quota_remaining, which "+
				"internal/ingest/provider emits from the provider's own headers; this is the local accounting "+
				"that exists even before the first successful request, and for the synthetic adapter, which has "+
				"no quota to report. Reaching 0 freezes the board deliberately: the scheduler refuses the sweep "+
				"rather than failing over to synthetic data, which would be indistinguishable from fabricating "+
				"market data.",
			"provider"),

		budgetLimit: gauge(metricSubsystemIngest, "budget_limit",
			"The configured credit budget for one period (CLAUDE.md §5: \"the budget as a config value\"). "+
				"Denominator for the local burn ratio; the alerting ratio ProviderQuotaLow uses the provider's "+
				"own pair instead.",
			"provider"),

		pollDuration: histogram(metricSubsystemIngest, "poll_duration_seconds",
			"Wall-clock time for one provider sweep: fetch, normalize and publish. Excludes time spent blocked on "+
				"the quota limiter, which is measured separately — otherwise a healthy-but-throttled system would "+
				"read as a slow provider.",
			pollBuckets, "provider", "league", "result"),

		quotaWait: histogram(metricSubsystemIngest, "quota_wait_seconds",
			"Time a sweep spent blocked on the shared token bucket before it was admitted. Non-zero means the "+
				"schedule is asking for credits faster than the budget refills, which is the signal to lengthen a "+
				"cadence or raise the tier — not to raise the concurrency limit.",
			quotaWaitBuckets, "provider"),

		quotaBlocked: counter(metricSubsystemIngest, "quota_blocked_total",
			"Sweeps that were refused outright because the period budget was exhausted. Distinct from waiting: "+
				"these polls did not happen and the board is frozen for that league.",
			"provider", "league"),

		marketsObserved: counter(metricSubsystemIngest, "markets_observed_total",
			"Markets carried by provider payloads, changed or not. Divide markets_changed_total by this for the "+
				"change ratio the adaptive backoff is reacting to.",
			"provider", "league"),

		marketsChanged: counter(metricSubsystemIngest, "markets_changed_total",
			"Markets whose normalized content differed from the previous observation and therefore generated bus "+
				"traffic. CLAUDE.md §5: most polls return identical data and must not generate bus traffic.",
			"provider", "league"),

		backoffSteps: gauge(metricSubsystemIngest, "backoff_steps",
			"Consecutive unchanged sweeps for a league, i.e. the number of doublings currently applied to its "+
				"configured interval. Zero means the league is polling at its configured cadence. This is the only "+
				"place the EFFECTIVE (as opposed to configured) cadence is observable, and it is deliberately not "+
				"folded into poll_interval_seconds, which the SLO threshold reads.",
			"provider", "league"),

		leaguesScheduled: gauge(metricSubsystemIngest, "leagues_scheduled",
			"Leagues currently being polled, by window. Summed, it is the size of the schedule; a league with no "+
				"wagerable events appears in no window at all and is not polled.",
			"provider", "window"),

		catalogueRuns: counter(metricSubsystemIngest, "catalogue_refreshes_total",
			"Event-catalogue refreshes by outcome. The catalogue endpoint is free at The Odds API (ADR 0003), so "+
				"this rate is unrelated to credit burn; outcome=\"error\" means the schedule is running on a stale "+
				"event list and a league that has gone live may still be polled at its pregame cadence.",
			"provider", "outcome"),

		catalogueEvents: gauge(metricSubsystemIngest, "catalogue_events",
			"Pollable events in the most recent catalogue refresh — those whose status still accepts wagers. Zero "+
				"with a healthy refresh rate means there is genuinely nothing to price, which is a correct empty "+
				"state and not a fault.",
			"provider"),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range m.collectors() {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("scheduler: register metrics collector: %w", err)
		}
	}
	return m, nil
}

// collectors lists every collector, for registration. Kept as one method so a
// new field cannot be added and silently left unregistered.
func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.polls, m.pollInterval, m.budgetLeft, m.budgetLimit,
		m.pollDuration, m.quotaWait, m.quotaBlocked,
		m.marketsObserved, m.marketsChanged, m.backoffSteps,
		m.leaguesScheduled, m.catalogueRuns, m.catalogueEvents,
	}
}

// -----------------------------------------------------------------------------
// Observation helpers. Every mutation goes through one of these so a label
// ordering can never be wrong at a call site and the closed label-value sets
// above stay closed.
// -----------------------------------------------------------------------------

// publishCadence writes the configured interval for every window of one league.
//
// All five windows are published, not just the league's current one, and that
// is load-bearing: the SLO objective rule selects {window="live"} with max(),
// so publishing only the current window would make the headline threshold blink
// out of existence the moment no league happened to be in play — and
// OddsPollCadenceUnknown would then fire on a perfectly healthy pregame system.
func (m *Metrics) publishCadence(provider string, league domain.LeagueID, tiers Tiers) {
	for _, w := range Windows() {
		tier, ok := tiers.For(w)
		if !ok {
			continue
		}
		m.pollInterval.WithLabelValues(provider, league.String(), w.String()).
			Set(tier.Interval.Seconds())
	}
}

// retireCadence removes a league's cadence series when it leaves the schedule.
// Leaving them behind would keep a settled league contributing to the max()
// that sets the SLO threshold for ever.
func (m *Metrics) retireCadence(provider string, league domain.LeagueID) {
	for _, w := range Windows() {
		m.pollInterval.DeleteLabelValues(provider, league.String(), w.String())
	}
	m.backoffSteps.DeleteLabelValues(provider, league.String())
}

// recordPoll records one completed sweep.
func (m *Metrics) recordPoll(
	provider string, league domain.LeagueID, w Window, res PollResult, err error, d time.Duration,
) string {
	result := classifyResult(res, err)
	l := league.String()

	m.polls.WithLabelValues(provider, l, w.String(), result).Inc()
	m.pollDuration.WithLabelValues(provider, l, result).Observe(d.Seconds())

	if res.Markets > 0 {
		m.marketsObserved.WithLabelValues(provider, l).Add(float64(res.Markets))
	}
	if res.Changed > 0 {
		m.marketsChanged.WithLabelValues(provider, l).Add(float64(res.Changed))
	}
	return result
}

// classifyResult maps an outcome onto the closed result label set.
func classifyResult(res PollResult, err error) string {
	switch {
	case err != nil:
		return resultError
	case res.Markets == 0:
		return resultEmpty
	case res.Changed == 0:
		return resultUnchanged
	default:
		return resultChanged
	}
}

func (m *Metrics) recordQuotaWait(provider string, d time.Duration) {
	m.quotaWait.WithLabelValues(provider).Observe(d.Seconds())
}

func (m *Metrics) recordQuotaBlocked(provider string, league domain.LeagueID) {
	m.quotaBlocked.WithLabelValues(provider, league.String()).Inc()
}

func (m *Metrics) recordBackoff(provider string, league domain.LeagueID, steps int) {
	m.backoffSteps.WithLabelValues(provider, league.String()).Set(float64(steps))
}

func (m *Metrics) recordQuota(provider string, b *Budget) {
	m.budgetLeft.WithLabelValues(provider).Set(float64(b.Remaining()))
	m.budgetLimit.WithLabelValues(provider).Set(float64(b.Limit()))
}

func (m *Metrics) recordCatalogue(provider string, events int, err error) {
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
	}
	m.catalogueRuns.WithLabelValues(provider, outcome).Inc()
	if err == nil {
		m.catalogueEvents.WithLabelValues(provider).Set(float64(events))
	}
}

// recordSchedule republishes the per-window league counts. Every window is
// written on every call, zeros included, so a window that empties reports 0
// rather than keeping its last non-zero value for ever.
func (m *Metrics) recordSchedule(provider string, counts map[Window]int) {
	for _, w := range Windows() {
		m.leaguesScheduled.WithLabelValues(provider, w.String()).Set(float64(counts[w]))
	}
}
