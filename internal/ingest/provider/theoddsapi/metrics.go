// Prometheus instrumentation for the real provider adapter.
//
// # Three of these names are a CONTRACT and were fixed before this code existed
//
// deploy/observability was written in phase 0 against services that did not yet
// exist, so for this package those names are a specification to satisfy, not a
// description of what it emits. Each is matched character for character, with
// the labels the PromQL aggregates by:
//
//	sharpline_provider_quota_remaining{provider}
//	    dashboard panel 7 (stat, "Provider quota remaining") and panel 17;
//	    dashboard template variable  label_values(sharpline_provider_quota_remaining, provider);
//	    alerts ProviderQuotaLow and ProviderQuotaExhausted.
//
//	sharpline_provider_quota_limit{provider}
//	    the DENOMINATOR of ProviderQuotaLow:
//	      sharpline_provider_quota_remaining / clamp_min(sharpline_provider_quota_limit, 1) < 0.10
//	    That expression has no on()/ignoring(), so the two series must carry an
//	    IDENTICAL label set or the division matches nothing and the alert
//	    silently never fires. Both therefore carry exactly {provider} and
//	    nothing else. Do not add a label to one without adding it to the other.
//
//	sharpline_provider_requests_total{provider,…}
//	    dashboard panel 17, as sum by (provider) (rate(…[$__rate_interval])),
//	    legended "burn". It is a REQUEST counter, not a credit counter — the
//	    panel's unit is reqps. Credits are counted separately by
//	    sharpline_provider_credits_used_total, because on this provider one
//	    request costs `markets × regions` credits and conflating the two would
//	    make the burn graph wrong by a factor of three at the recommended tier.
//
// The `provider` label value is "the-odds-api", which is the same slug
// Terraform's raw_providers uses for the odds.raw.the-odds-api topic. One name
// for the provider across the bus, the dashboard and the alerts.
//
// # Names NOT emitted here, deliberately
//
//   - sharpline_odds_staleness_seconds{stage="received"} and
//     sharpline_odds_clock_skew_total. Those measure (ingested_at − observed_at)
//     and belong to the ingest pipeline, which is where ingested_at is stamped
//     and where the league label comes from. Emitting them here would mean two
//     packages registering one collector and racing to define its buckets — and
//     the bucket boundaries are themselves a contract (see the header of
//     deploy/observability/rules/sharpline-alerts.yml). This adapter's job is to
//     surface the provider's observed_at faithfully; measuring the gap is the
//     pipeline's.
//   - sharpline_ingest_polls_total and sharpline_ingest_poll_interval_seconds.
//     Those describe the SCHEDULER's behaviour — poll outcome and cadence per
//     window — and the scheduler is not in this package.
//
// # Labels deliberately NOT set
//
//   - `service`. prometheus.yml attaches it as a target label on every scrape.
//     A metric label of the same name is renamed exported_service on ingest and
//     the two drift.
//   - the sport key, the league, or the event id. Bounded by the config today,
//     unbounded the moment someone polls every sport the catalogue lists.
//   - the API key, the request URL, or any response body text. See doc.go.
package theoddsapi

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric namespace and subsystem, producing the `sharpline_provider_` prefix
// the dashboard and alert rules already reference.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "provider"
)

// ProviderSlug is the value of the `provider` metric label, and it is the same
// slug as the Kafka topic odds.raw.the-odds-api that Terraform declares in
// var.raw_providers. Changing it here without changing Terraform splits the
// dashboard's provider variable in two.
const ProviderSlug = "the-odds-api"

// Outcome label values. A closed set.
const (
	outcomeOK        = "ok"
	outcomeError     = "error"
	outcomeThrottled = "throttled"
)

// requestBuckets bounds one HTTP round trip to a third party over the public
// internet. It starts at 25ms because nothing crosses the internet faster, and
// runs to 30s because that is the ceiling on the per-request timeout — an
// observation in the top bucket is a request that is about to be abandoned.
var requestBuckets = []float64{
	0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30,
}

// payloadBuckets counts events per sweep response. Powers of two: the question
// it answers — "did the slate come back, or did we just pay a credit for an
// empty array?" — is answered by the order of magnitude. The boundary at 0 is
// load-bearing: an empty response is not billed by the provider, so a rise in
// the zero bucket is a rise in refunds, not in spend.
var payloadBuckets = []float64{0, 1, 2, 4, 8, 16, 32, 64, 128, 256, 512}

// Metrics holds every collector this package owns.
//
// It is a value, never a package-level variable (CLAUDE.md §12 forbids global
// mutable state), and it is safe for concurrent use — every field is a
// Prometheus collector.
type Metrics struct {
	// ---- contract series (dashboard + alerts already reference these) ----
	quotaRemaining *prometheus.GaugeVec   // provider
	quotaLimit     *prometheus.GaugeVec   // provider
	requests       *prometheus.CounterVec // provider, endpoint, outcome

	// ---- quota detail ----
	quotaUsed          *prometheus.GaugeVec   // provider
	quotaLastCost      *prometheus.GaugeVec   // provider
	quotaLocalEstimate *prometheus.GaugeVec   // provider
	quotaFromProvider  *prometheus.GaugeVec   // provider
	quotaHeaderMissing *prometheus.CounterVec // provider
	creditsUsed        *prometheus.CounterVec // provider, endpoint
	budgetTokens       *prometheus.GaugeVec   // provider, bucket

	// ---- request detail ----
	duration      *prometheus.HistogramVec // provider, endpoint, outcome
	errorsTotal   *prometheus.CounterVec   // provider, endpoint, code
	throttled     *prometheus.CounterVec   // provider, limiter
	retries       *prometheus.CounterVec   // provider, endpoint, reason
	responseBytes *prometheus.CounterVec   // provider, endpoint
	payloadEvents *prometheus.HistogramVec // provider, endpoint

	// ---- mapping detail ----
	mappingDropped *prometheus.CounterVec // provider, reason
}

// NewMetrics builds the collectors and registers them on reg.
//
// reg may be nil, which builds the collectors but registers nothing — right for
// a unit test and for a one-shot job that serves no /metrics endpoint. The
// observe calls stay live and cost a few nanoseconds, so no call site needs a
// nil check.
//
// A collector that is ALREADY registered under an identical description is
// reused rather than treated as a fatal error. That matters because the
// `provider` metric family is shared by design: the synthetic adapter is a peer
// implementation behind the same interface, and `ingest` may construct both in
// one process (the real one when ODDS_API_KEY is set, the synthetic one as the
// fallback). Two adapters registering the same GaugeVec is a legitimate,
// expected arrangement — they simply write different `provider` label values.
// A genuine conflict, where the existing collector has different labels or a
// different help string, still fails loudly.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{}
	r := reuser{reg: reg}

	// ------------------------------------------------------------- contract
	m.quotaRemaining = r.gauge("quota_remaining",
		"Provider usage credits remaining until the monthly quota resets. Fed from the provider's own "+
			"x-requests-remaining response header whenever it is present, and from this process's local estimate "+
			"only when it is not (ADR 0003: the header is authoritative because a local counter cannot see spend "+
			"from another process sharing the key, nor the month rolling over). "+
			"sharpline_provider_quota_from_provider says which source is live. "+
			"Panel: sharpline_provider_quota_remaining{provider=~\"$provider\"}.",
		"provider")

	m.quotaLimit = r.gauge("quota_limit",
		"The configured monthly credit budget — the denominator of ProviderQuotaLow. It carries EXACTLY the same "+
			"label set as quota_remaining ({provider} only) because the alert divides one by the other with no "+
			"on()/ignoring(); a label on one and not the other makes the division match nothing and the alert "+
			"never fires.",
		"provider")

	m.requests = r.counter("requests_total",
		"HTTP requests issued to the provider, by endpoint template and outcome. This counts REQUESTS, not "+
			"credits: one /v4/sports/{sport}/odds request costs markets x regions credits, and the dashboard's "+
			"burn panel reads this series in reqps. For credit spend see credits_used_total. "+
			"outcome=\"throttled\" means OUR limiter refused and no request left the process.",
		"provider", "endpoint", "outcome")

	// ---------------------------------------------------------- quota detail
	m.quotaUsed = r.gauge("quota_used",
		"Provider usage credits consumed since the last quota reset, from the x-requests-used header. "+
			"quota_used + quota_remaining is the subscription's true allowance, which is how a misconfigured "+
			"sharpline_provider_quota_limit is caught.",
		"provider")

	m.quotaLastCost = r.gauge("quota_last_cost",
		"What the provider charged for the most recent call, from x-requests-last. Compare against the predicted "+
			"markets x regions: a persistent disagreement means the cost model in SweepCost is wrong and the "+
			"budget is being paced against a fiction.",
		"provider")

	m.quotaLocalEstimate = r.gauge("quota_local_estimate",
		"Monthly budget minus everything THIS PROCESS has spent. Exported alongside the provider's own number so "+
			"the two can be compared rather than silently substituted: a widening gap means another consumer is "+
			"spending the same key, or the quota reset and this process did not notice.",
		"provider")

	m.quotaFromProvider = r.gauge("quota_from_provider",
		"1 when sharpline_provider_quota_remaining is the provider's own x-requests-remaining, 0 when it is this "+
			"process's local estimate. A quota gauge running on an estimate is a quota gauge that can drift, and "+
			"this is what makes that state visible instead of indistinguishable.",
		"provider")

	m.quotaHeaderMissing = r.counter("quota_header_missing_total",
		"Responses that arrived without an x-requests-remaining header. The provider documents it on every "+
			"endpoint, so a non-zero rate here is either a provider change or a proxy stripping headers — and it "+
			"is the precondition for the quota gauge falling back to the local estimate.",
		"provider")

	m.creditsUsed = r.counter("credits_used_total",
		"Usage credits spent, by endpoint template, taken from the provider's x-requests-last header rather than "+
			"from the predicted cost. This is the series to integrate against the monthly budget. "+
			"/v4/sports and /v4/sports/{sport}/events are documented as free and contribute zero.",
		"provider", "endpoint")

	m.budgetTokens = r.gauge("budget_tokens",
		"Tokens currently held by each of the limiter's two buckets. bucket=\"credits\" is the monthly credit "+
			"pacing bucket (refilling at MonthlyCredits/BudgetWindow); bucket=\"frequency\" is the per-second "+
			"request bucket that keeps sweeps under the provider's documented 30/s ceiling. This is NOT the same "+
			"quantity as quota_remaining: that is what the subscription has left, this is what the limiter is "+
			"presently willing to spend, and the pacing lives in the gap.",
		"provider", "bucket")

	// -------------------------------------------------------- request detail
	m.duration = r.histogram("request_duration_seconds",
		"Round-trip time to the provider, including body read. This is a third-party internet call and it is the "+
			"floor on ingest's own latency; it is NOT the odds staleness SLO, which is measured from the "+
			"provider's observed_at and is dominated by polling cadence rather than by this.",
		requestBuckets, "provider", "endpoint", "outcome")

	m.errorsTotal = r.counter("errors_total",
		"Failed provider calls by endpoint and BOUNDED error code. The code is one of the provider's own "+
			"documented error-code tokens (OUT_OF_USAGE_CREDITS, INVALID_KEY, EXCEEDED_FREQ_LIMIT, …) when the "+
			"response body named one, otherwise a status class (http_401, http_5xx) or a transport cause. Never "+
			"the response text: a provider can put arbitrary bytes there and the label set would be unbounded.",
		"provider", "endpoint", "code")

	m.throttled = r.counter("throttled_total",
		"Requests THIS PROCESS refused to send, by which bucket refused. limiter=\"credits\" means the configured "+
			"monthly budget is being outrun by the configured cadence — the two are inconsistent and one must "+
			"change. limiter=\"frequency\" means sweeps are bunching and need jitter.",
		"provider", "limiter")

	m.retries = r.counter("retries_total",
		"Retried attempts by reason. reason=\"rate_limited\" honours the provider's Retry-After; \"server_error\" "+
			"and \"transport\" use jittered exponential backoff. A quota exhaustion is NEVER retried — the "+
			"credits return at the monthly reset, not after a backoff — so it cannot appear here.",
		"provider", "endpoint", "reason")

	m.responseBytes = r.counter("response_bytes_total",
		"Response body bytes read from the provider. Divided by requests_total it is the mean payload size, which "+
			"is what decides whether a sweep's decode cost is about to matter.",
		"provider", "endpoint")

	m.payloadEvents = r.histogram("payload_events",
		"Events returned per response. The zero bucket is load-bearing: the provider documents that a request "+
			"returning no events is not charged, so growth there is growth in refunds rather than in spend, and "+
			"is the signal that a league has gone out of season and should be polled far less often.",
		payloadBuckets, "provider", "endpoint")

	// -------------------------------------------------------- mapping detail
	m.mappingDropped = r.counter("mapping_dropped_total",
		"Quotes, outcomes and events discarded while turning a provider payload into domain values, by a "+
			"CLOSED reason label (see DropReasons). This is the counter that keeps a lossy mapping honest: "+
			"reason=\"line_disagreement\" is the structural one and it is expected to be non-zero — the domain "+
			"models ONE line per market while books quote several, so a book off the consensus line is dropped "+
			"rather than reconciled. A rise in reason=\"no_observation_instant\" or \"unmapped_outcome\" is a "+
			"provider format change; a rise in \"invalid_odds\" is a decode or odds-format mismatch. Nothing "+
			"here is lost from the record of truth: odds.raw.the-odds-api still carries the payload verbatim.",
		"provider", "reason")

	if err := r.err(); err != nil {
		return nil, err
	}
	return m, nil
}

// reuser builds collectors and registers them, tolerating an identical
// collector that some other adapter already registered.
type reuser struct {
	reg  prometheus.Registerer
	errs []error
}

func (r *reuser) err() error { return errors.Join(r.errs...) }

func (r *reuser) counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
	}, labels)
	return register(r, c, name)
}

func (r *reuser) gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	c := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
	}, labels)
	return register(r, c, name)
}

func (r *reuser) histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	c := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
		Buckets: buckets,
	}, labels)
	return register(r, c, name)
}

// register registers c, or returns the equivalent collector already present.
//
// prometheus.AlreadyRegisteredError carries the existing collector, and the
// registry only produces it when the descriptions match — a same-named
// collector with different labels or a different help string produces a
// different, fatal error. So this reuses exactly the safe case and fails on
// every other.
func register[T prometheus.Collector](r *reuser, c T, name string) T {
	if r.reg == nil {
		return c
	}
	err := r.reg.Register(c)
	if err == nil {
		return c
	}
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		if existing, ok := already.ExistingCollector.(T); ok {
			return existing
		}
		r.errs = append(r.errs, fmt.Errorf(
			"theoddsapi: metric %q is already registered as a different collector type", name))
		return c
	}
	r.errs = append(r.errs, fmt.Errorf("theoddsapi: register metric %q: %w", name, err))
	return c
}

// -----------------------------------------------------------------------------
// Observation helpers
//
// Every metric mutation in this package goes through one of these, so a label
// ordering cannot be wrong at a call site and the closed label-value sets stay
// closed.
// -----------------------------------------------------------------------------

// observeQuota publishes a Quota snapshot onto the gauges. It is called after
// every response — including failed ones, since a 401 still carries the
// headers — so the dashboard's quota panel tracks the provider's own number as
// closely as the scrape interval allows.
func (m *Metrics) observeQuota(q Quota, frequencyTokens float64, headerMissing int64) {
	m.quotaRemaining.WithLabelValues(ProviderSlug).Set(float64(q.Remaining))
	m.quotaLimit.WithLabelValues(ProviderSlug).Set(float64(q.Limit))
	m.quotaUsed.WithLabelValues(ProviderSlug).Set(float64(q.Used))
	m.quotaLastCost.WithLabelValues(ProviderSlug).Set(float64(q.LastCost))
	m.quotaLocalEstimate.WithLabelValues(ProviderSlug).Set(float64(q.LocalEstimate))
	m.budgetTokens.WithLabelValues(ProviderSlug, "credits").Set(q.CreditTokens)
	m.budgetTokens.WithLabelValues(ProviderSlug, "frequency").Set(frequencyTokens)

	fromProvider := 0.0
	if q.FromProvider {
		fromProvider = 1
	}
	m.quotaFromProvider.WithLabelValues(ProviderSlug).Set(fromProvider)
	_ = headerMissing
}

func (m *Metrics) observeHeaderMissing() {
	m.quotaHeaderMissing.WithLabelValues(ProviderSlug).Inc()
}

func (m *Metrics) observeRequest(endpoint, outcome string, seconds float64) {
	m.requests.WithLabelValues(ProviderSlug, endpoint, outcome).Inc()
	m.duration.WithLabelValues(ProviderSlug, endpoint, outcome).Observe(seconds)
}

func (m *Metrics) observeThrottled(endpoint, limiter string) {
	m.requests.WithLabelValues(ProviderSlug, endpoint, outcomeThrottled).Inc()
	m.throttled.WithLabelValues(ProviderSlug, limiter).Inc()
}

func (m *Metrics) observeError(endpoint, code string) {
	m.errorsTotal.WithLabelValues(ProviderSlug, endpoint, code).Inc()
}

func (m *Metrics) observeRetry(endpoint, reason string) {
	m.retries.WithLabelValues(ProviderSlug, endpoint, reason).Inc()
}

func (m *Metrics) observeCredits(endpoint string, credits int64) {
	if credits <= 0 {
		return
	}
	m.creditsUsed.WithLabelValues(ProviderSlug, endpoint).Add(float64(credits))
}

// observeDropped counts n values lost to a mapping decision. n <= 0 is a no-op
// so call sites can pass a count without guarding it.
//
// reason MUST be one of the DropReason* constants. It is a metric label, and a
// label built from provider text is a cardinality incident waiting for a
// provider to change its wording.
func (m *Metrics) observeDropped(reason string, n int) {
	if n <= 0 || reason == "" {
		return
	}
	m.mappingDropped.WithLabelValues(ProviderSlug, reason).Add(float64(n))
}

func (m *Metrics) observePayload(endpoint string, bytesRead int, events int) {
	if bytesRead > 0 {
		m.responseBytes.WithLabelValues(ProviderSlug, endpoint).Add(float64(bytesRead))
	}
	m.payloadEvents.WithLabelValues(ProviderSlug, endpoint).Observe(float64(events))
}

// metricCode maps an error onto a BOUNDED label value.
//
// The provider's own documented error-code tokens are a fixed enumerated set
// and they are the strings an operator will search for, so they are used
// verbatim when the body named one. Everything else collapses onto a status
// class or a transport cause. The response TEXT is never a label.
func metricCode(err error) string {
	if err == nil {
		return ""
	}
	if code := ErrorCode(err); code != "" {
		return code
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode >= 500:
			return "http_5xx"
		case apiErr.StatusCode > 0:
			return fmt.Sprintf("http_%d", apiErr.StatusCode)
		}
	}

	switch {
	case errors.Is(err, ErrBudgetExhausted):
		return "local_budget"
	case errors.Is(err, ErrMalformedResponse):
		return "malformed_response"
	case errors.Is(err, ErrTransport):
		return transportCode(err)
	default:
		return "unknown"
	}
}
