package theoddsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName identifies this instrumentation library in the trace. It is the
// name under which the provider hop appears in Jaeger, and it is the FIRST span
// in the ingest → pricer → stream trace CLAUDE.md §9 asks for.
const tracerName = "github.com/anpl1623/sharpline/internal/ingest/provider/theoddsapi"

// Span attribute keys, following the OpenTelemetry HTTP semantic conventions.
// Written as literals rather than imported from a semconv/vN package for the
// same reason internal/platform/kafka does it: a semconv version bump must not
// silently rename an attribute a saved Jaeger query depends on.
const (
	attrHTTPMethod      = "http.request.method"
	attrHTTPStatus      = "http.response.status_code"
	attrHTTPResendCount = "http.request.resend_count"
	attrHTTPBodySize    = "http.response.body.size"
	attrURLTemplate     = "url.template"
	attrURLFull         = "url.full"
	attrServerAddress   = "server.address"

	attrProvider     = "sharpline.provider"
	attrCreditCost   = "sharpline.provider.credit_cost"
	attrCreditActual = "sharpline.provider.credits_charged"
	attrQuotaLeft    = "sharpline.provider.quota_remaining"
	attrErrorCode    = "sharpline.provider.error_code"
	attrEventCount   = "sharpline.provider.event_count"
	attrSportKey     = "sharpline.provider.sport_key"
)

// Backoff bounds for retryable failures. The provider's rate-limit guidance is
// "retry the request after a couple of seconds", so the base is deliberately
// closer to a second than to a millisecond — hammering a 429 is what produced
// it.
const (
	baseBackoff = 500 * time.Millisecond
	maxBackoff  = 10 * time.Second
)

// Client is the HTTP client for The Odds API v4.
//
// It is safe for concurrent use: the limiter guards its own state and
// everything else is read-only after construction.
type Client struct {
	cfg     Config
	base    *url.URL
	httpc   *http.Client
	limiter *Limiter
	metrics *Metrics
	log     *slog.Logger
	tracer  trace.Tracer
	redact  redactor

	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// Option customises a Client. Dependencies are constructor-injected
// (CLAUDE.md §12) — there is no package-level http.Client, logger or tracer.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client. The replacement's CheckRedirect is
// overwritten: refusing a cross-host redirect is a security property of this
// package, not a preference, because a redirect carries the apiKey query
// parameter to whatever host it points at.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpc = h
		}
	}
}

// WithMetrics injects the collectors. Passing nil-built metrics (NewMetrics(nil))
// keeps every observe call live but registers nothing.
func WithMetrics(m *Metrics) Option {
	return func(c *Client) {
		if m != nil {
			c.metrics = m
		}
	}
}

// WithLogger injects the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithTracerProvider injects the tracer provider. The default is the global
// one, matching internal/platform/postgres: a no-op tracer provider is a
// visible absence of spans, which is a safe default.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *Client) {
		if tp != nil {
			c.tracer = tp.Tracer(tracerName)
		}
	}
}

// WithClock injects the clock, so limiter refill and retry timing are
// deterministic in tests rather than wall-clock dependent.
func WithClock(now func() time.Time) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// WithSleep injects the backoff sleep. A test substitutes an instant one so a
// retry path is exercised without a real delay.
func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if sleep != nil {
			c.sleep = sleep
		}
	}
}

// WithJitter injects the backoff jitter function. The default is full jitter
// over [0, d); a test substitutes the identity to make backoff exact.
func WithJitter(j func(time.Duration) time.Duration) Option {
	return func(c *Client) {
		if j != nil {
			c.jitter = j
		}
	}
}

// WithLimiter injects a pre-built limiter, so several clients can share one
// budget — which is what a process polling four leagues needs, since the
// monthly quota is shared across every call made with the key.
func WithLimiter(l *Limiter) Option {
	return func(c *Client) {
		if l != nil {
			c.limiter = l
		}
	}
}

// New builds a Client, applying defaults and validating the result.
//
// Validation happens HERE, at construction, not at the first poll: CLAUDE.md
// §12 requires config be validated at startup and fail fast, and a bad
// configuration discovered on the first sweep costs a credit to learn.
func New(cfg Config, opts ...Option) (*Client, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		// Unreachable: Validate already parsed it. Kept because a silent nil
		// base URL would panic on the first request.
		return nil, fmt.Errorf("%w: BaseURL: %w", ErrInvalidConfig, err)
	}

	metrics, err := NewMetrics(nil)
	if err != nil {
		return nil, err
	}

	c := &Client{
		cfg:     cfg,
		base:    base,
		metrics: metrics,
		log:     slog.New(slog.DiscardHandler),
		tracer:  otel.GetTracerProvider().Tracer(tracerName),
		redact:  newRedactor(cfg.APIKey),
		now:     time.Now,
		sleep:   sleepContext,
		jitter:  fullJitter,
	}
	for _, opt := range opts {
		opt(c)
	}

	if c.limiter == nil {
		limCfg := cfg.Limiter
		if limCfg.Now == nil {
			limCfg.Now = c.now
		}
		c.limiter = NewLimiter(limCfg)
	}
	if c.httpc == nil {
		c.httpc = &http.Client{}
	}
	// Unconditional, and after the options: a caller-supplied client must not
	// be able to reintroduce redirect following. The apiKey travels in the
	// query string, so a 302 to another host hands the credential to whoever
	// controls it.
	c.httpc.CheckRedirect = refuseCrossHostRedirect

	// Publish the configured budget immediately, so the quota panel and
	// ProviderQuotaLow have a denominator before the first poll rather than
	// after it.
	c.publishQuota()
	return c, nil
}

// refuseCrossHostRedirect allows a same-host redirect (http -> https on the
// same host, or a trailing-slash normalisation, both of which this API does)
// and refuses anything that would send the key somewhere else — or send it in
// cleartext.
//
// Two independent refusals, because a redirect can leak the credential two
// ways:
//
//   - A DIFFERENT HOST gets the key handed to whoever controls it. That is the
//     obvious one.
//   - A DOWNGRADE from https to http on the SAME host puts the key on the wire
//     in cleartext, where any middlebox on the path can read it. The host check
//     alone does not catch this, and it is exactly the shape a hostile network
//     would use, so it is refused separately.
//
// The scheme comparison is one-directional on purpose: http -> https is an
// upgrade and is allowed, because a base URL configured without TLS should
// still end up encrypted rather than failing.
func refuseCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 5 {
		return errors.New("theoddsapi: too many redirects")
	}
	origin := via[0].URL
	if !strings.EqualFold(req.URL.Hostname(), origin.Hostname()) {
		// The target host is NOT named in the error: an attacker-controlled
		// redirect target in a log line is a small injection surface, and the
		// origin host is enough to diagnose.
		return fmt.Errorf("theoddsapi: refusing cross-host redirect away from %s "+
			"(the apiKey travels in the query string)", origin.Hostname())
	}
	if strings.EqualFold(origin.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("theoddsapi: refusing redirect that downgrades %s from https to %s "+
			"(the apiKey travels in the query string and would go out in cleartext)",
			origin.Hostname(), req.URL.Scheme)
	}
	return nil
}

// Quota returns the current provider credit accounting.
func (c *Client) Quota() Quota { return c.limiter.Quota() }

// Limiter exposes the shared budget, so a second Client can be constructed
// against the same one with WithLimiter.
func (c *Client) Limiter() *Limiter { return c.limiter }

// SweepCost returns the credit cost of one Odds call under this client's
// configuration, for a scheduler deciding whether it can afford a sweep.
func (c *Client) SweepCost() int { return c.cfg.SweepCost() }

// -----------------------------------------------------------------------------
// Results
// -----------------------------------------------------------------------------

// OddsSweep is one odds response: the decoded events, the exact bytes the
// provider sent, and the accounting for the call.
//
// Raw is retained because CLAUDE.md §3's event flow publishes the untouched
// payload to odds.raw.{provider} BEFORE normalization. Keeping the provider's
// own bytes is what makes the raw topic replayable against a future normalizer,
// and it is the only artefact that can settle an argument about whether a
// normalization bug was ours or theirs.
type OddsSweep struct {
	// SportKey is the sport this sweep was for.
	SportKey string

	// OddsFormat records how Events' prices must be read. It comes from the
	// request, not from the payload — the numbers themselves are ambiguous.
	OddsFormat OddsFormat

	// Events is the decoded payload.
	Events []EventOdds

	// Raw is exactly what the provider sent, for odds.raw.{provider}.
	Raw json.RawMessage

	// FetchedAt is when the response finished arriving. This is the
	// `ingested_at` of the phase-2 schema — NOT `observed_at`, which is the
	// provider's own market-level last_update and is carried per market. The
	// phase-2 handoff is explicit that the two are not interchangeable: the
	// headline staleness SLO measures from observed_at and the difference
	// between them is the provider-attributable share.
	FetchedAt time.Time

	// Quota is the accounting after this call.
	Quota Quota

	// CreditsCharged is what the provider actually billed, from
	// x-requests-last, or the predicted cost when the header was absent.
	CreditsCharged int64
}

// -----------------------------------------------------------------------------
// Endpoints
// -----------------------------------------------------------------------------

// Sports fetches the sport catalogue.
//
// This endpoint is FREE — "This endpoint does not count against the usage
// quota" — so it may be polled aggressively (ADR 0003 requirement 2). It still
// takes a slot in the frequency bucket, because free does not mean exempt from
// the 30/s limit.
func (c *Client) Sports(ctx context.Context, includeOutOfSeason bool) ([]Sport, error) {
	q := url.Values{}
	if includeOutOfSeason {
		q.Set("all", "true")
	}

	resp, err := c.fetch(ctx, request{
		endpoint: EndpointSports,
		path:     "/v4/sports/",
		query:    q,
		cost:     0,
	})
	if err != nil {
		return nil, err
	}

	var sports []Sport
	if err := c.decode(resp, EndpointSports, &sports); err != nil {
		return nil, err
	}
	c.metrics.observePayload(EndpointSports, len(resp.body), len(sports))
	return sports, nil
}

// Odds sweeps every live and upcoming event for one sport.
//
// One request returns the whole slate: "One /odds request returns every
// upcoming and live event for that sport. Cost does not scale with the number
// of events" (ADR 0003). The cost is markets × region-equivalents, computed by
// SweepCost from the configuration.
func (c *Client) Odds(ctx context.Context, sportKey string) (*OddsSweep, error) {
	return c.OddsWithMarkets(ctx, sportKey, nil)
}

// OddsWithMarkets is Odds for an explicit market set.
//
// It exists because the SCOPE decides which markets a sweep covers, not the
// process-wide configuration: the scheduler polls a live league for
// h2h/spreads/totals and a futures league for outrights, and billing is
// multiplicative in the market count (ADR 0003), so sending the union would
// cost more and return markets the sport does not have.
//
// An empty markets slice falls back to Config.Markets. The cost reserved is
// computed from the markets ACTUALLY sent, never from the configuration, or the
// token bucket would be pacing against a different request than the one issued.
func (c *Client) OddsWithMarkets(ctx context.Context, sportKey string, markets []string) (*OddsSweep, error) {
	sportKey = strings.TrimSpace(sportKey)
	if sportKey == "" {
		return nil, fmt.Errorf("%w: empty sport key", ErrInvalidRequest)
	}
	if len(markets) == 0 {
		markets = c.cfg.Markets
	}
	if len(markets) == 0 {
		return nil, fmt.Errorf("%w: no markets requested", ErrInvalidRequest)
	}

	q := c.commonOddsQuery()
	q.Set("markets", strings.Join(markets, ","))

	resp, err := c.fetch(ctx, request{
		endpoint: EndpointOdds,
		path:     "/v4/sports/" + url.PathEscape(sportKey) + "/odds/",
		query:    q,
		cost:     SweepCost(len(markets), len(c.cfg.Regions), len(c.cfg.Bookmakers)),
		sportKey: sportKey,
	})
	if err != nil {
		return nil, err
	}

	var events []EventOdds
	if err := c.decode(resp, EndpointOdds, &events); err != nil {
		return nil, err
	}
	c.metrics.observePayload(EndpointOdds, len(resp.body), len(events))

	return &OddsSweep{
		SportKey:       sportKey,
		OddsFormat:     c.cfg.OddsFormat,
		Events:         events,
		Raw:            resp.body,
		FetchedAt:      resp.fetchedAt,
		Quota:          c.limiter.Quota(),
		CreditsCharged: resp.charged,
	}, nil
}

// EventOdds fetches one event's odds, which is the only way to reach
// non-featured markets such as player props.
//
// The cost model is DIFFERENT and worse here: the provider bills "1 per unique
// market RETURNED per region", so the charge is not knowable before the call.
// The reservation uses the requested market count as an upper bound and the
// x-requests-last header reconciles it afterwards. ADR 0003 scenario E prices
// this out: "One afternoon of NFL player props costs 6,144 credits — 6.1% of
// the entire 100K monthly tier, spent in four hours."
//
// markets may be empty, in which case the configured featured markets are used.
func (c *Client) EventOdds(ctx context.Context, sportKey, eventID string, markets []string) (*OddsSweep, error) {
	sportKey = strings.TrimSpace(sportKey)
	eventID = strings.TrimSpace(eventID)
	if sportKey == "" {
		return nil, fmt.Errorf("%w: empty sport key", ErrInvalidRequest)
	}
	if eventID == "" {
		return nil, fmt.Errorf("%w: empty event id", ErrInvalidRequest)
	}
	if len(markets) == 0 {
		markets = c.cfg.Markets
	}

	q := c.commonOddsQuery()
	q.Set("markets", strings.Join(markets, ","))

	resp, err := c.fetch(ctx, request{
		endpoint: EndpointEventOdds,
		path: "/v4/sports/" + url.PathEscape(sportKey) +
			"/events/" + url.PathEscape(eventID) + "/odds",
		query:    q,
		cost:     SweepCost(len(markets), len(c.cfg.Regions), len(c.cfg.Bookmakers)),
		sportKey: sportKey,
	})
	if err != nil {
		return nil, err
	}

	var event EventOdds
	if err := c.decode(resp, EndpointEventOdds, &event); err != nil {
		return nil, err
	}
	c.metrics.observePayload(EndpointEventOdds, len(resp.body), 1)

	return &OddsSweep{
		SportKey:       sportKey,
		OddsFormat:     c.cfg.OddsFormat,
		Events:         []EventOdds{event},
		Raw:            resp.body,
		FetchedAt:      resp.fetchedAt,
		Quota:          c.limiter.Quota(),
		CreditsCharged: resp.charged,
	}, nil
}

// commonOddsQuery builds the parameters both odds endpoints share.
//
// `bookmakers` and `regions` are mutually exclusive HERE even though the
// provider accepts both, because it documents that "if both bookmakers and
// regions are specified, bookmakers takes precedence" — and sending a
// parameter the provider will ignore makes the cost model ambiguous at exactly
// the point where the credit charge is being predicted. One or the other, and
// SweepCost prices whichever was sent.
func (c *Client) commonOddsQuery() url.Values {
	q := url.Values{}
	if len(c.cfg.Bookmakers) > 0 {
		q.Set("bookmakers", strings.Join(c.cfg.Bookmakers, ","))
	} else {
		q.Set("regions", strings.Join(c.cfg.Regions, ","))
	}
	q.Set("oddsFormat", string(c.cfg.OddsFormat))
	q.Set("dateFormat", string(c.cfg.DateFormat))
	return q
}

// -----------------------------------------------------------------------------
// Transport
// -----------------------------------------------------------------------------

// request is one logical provider call.
type request struct {
	// endpoint is the bounded path TEMPLATE, used for metric labels and span
	// names. Never the concrete path.
	endpoint string

	// path is the concrete path, with identifiers substituted.
	path string

	// query holds every parameter EXCEPT apiKey, which is added at the last
	// possible moment so that a query value logged anywhere upstream of that
	// point cannot contain it.
	query url.Values

	// cost is the predicted credit cost.
	cost int

	// sportKey is a span attribute only. It is bounded by configuration but
	// not by the metric label set, which is why it is a span attribute and not
	// a label.
	sportKey string
}

// rawResponse is a successful (2xx) provider response.
type rawResponse struct {
	body      []byte
	fetchedAt time.Time
	charged   int64
}

// fetch performs the request, retrying the failures that are worth retrying.
//
// Every attempt reserves from the limiter first. That ordering matters: a
// retry is a real request that can cost a real credit, so a retry storm must be
// bounded by the budget in the same way a poll storm is.
func (c *Client) fetch(ctx context.Context, req request) (*rawResponse, error) {
	ctx, span := c.tracer.Start(ctx, "theoddsapi "+req.endpoint,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(attrHTTPMethod, http.MethodGet),
			attribute.String(attrURLTemplate, req.endpoint),
			attribute.String(attrServerAddress, c.base.Host),
			attribute.String(attrProvider, ProviderSlug),
			attribute.Int(attrCreditCost, req.cost),
		))
	defer span.End()
	if req.sportKey != "" {
		span.SetAttributes(attribute.String(attrSportKey, req.sportKey))
	}

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		if err := c.limiter.Reserve(req.cost); err != nil {
			var budgetErr *BudgetError
			limiterName := "credits"
			if errors.As(err, &budgetErr) {
				limiterName = budgetErr.Limiter
			}
			c.metrics.observeThrottled(req.endpoint, limiterName)
			c.metrics.observeError(req.endpoint, metricCode(err))
			c.publishQuota()
			c.recordSpanError(span, err)
			// A budget refusal is never retried in-place: waiting inside the
			// adapter would hide the stall from the scheduler that owns
			// cadence, and the wait can be hours.
			return nil, err
		}

		resp, err := c.attempt(ctx, req, attempt)
		c.publishQuota()
		if err == nil {
			span.SetAttributes(attribute.Int64(attrCreditActual, resp.charged))
			span.SetAttributes(attribute.Int64(attrQuotaLeft, c.limiter.Quota().Remaining))
			return resp, nil
		}
		lastErr = err

		if !Retryable(err) || attempt == c.cfg.MaxAttempts {
			break
		}

		delay := c.retryDelay(err, attempt)
		c.metrics.observeRetry(req.endpoint, retryReason(err))
		c.log.WarnContext(ctx, "provider request failed, retrying",
			slog.String("provider", ProviderSlug),
			slog.String("endpoint", req.endpoint),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", c.cfg.MaxAttempts),
			slog.Duration("delay", delay),
			// err is already sanitized by attempt; logging it cannot leak the
			// key. TestRedaction proves it.
			slog.String("error", err.Error()),
		)
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			// A cancelled context during backoff is the caller's decision, and
			// it supersedes the provider error.
			c.recordSpanError(span, sleepErr)
			return nil, sleepErr
		}
	}

	c.recordSpanError(span, lastErr)
	return nil, lastErr
}

// attempt issues exactly one HTTP request.
func (c *Client) attempt(ctx context.Context, req request, attempt int) (*rawResponse, error) {
	// The key is attached HERE, to a copy, at the last possible moment.
	u := *c.base
	u.Path = strings.TrimSuffix(c.base.Path, "/") + req.path
	q := make(url.Values, len(req.query)+1)
	for k, vs := range req.query {
		q[k] = vs
	}
	q.Set(apiKeyParam, c.cfg.APIKey)
	u.RawQuery = q.Encode()

	redactedURL := c.redact.URL(&u)

	ctx, span := c.tracer.Start(ctx, "GET "+req.endpoint,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(attrHTTPMethod, http.MethodGet),
			attribute.String(attrURLTemplate, req.endpoint),
			// The REDACTED URL. There is no code path that puts the real one
			// in a span.
			attribute.String(attrURLFull, redactedURL),
			attribute.String(attrServerAddress, c.base.Host),
			attribute.Int(attrHTTPResendCount, attempt-1),
		))
	defer span.End()

	// CLAUDE.md §12: every external call has a timeout.
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		wrapped := fmt.Errorf("%w: build request: %w", ErrTransport, sanitizeError(c.redact, err))
		c.recordSpanError(span, wrapped)
		return nil, wrapped
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", c.cfg.UserAgent)

	start := c.now()
	httpResp, err := c.httpc.Do(httpReq)
	elapsed := c.now().Sub(start)
	if err != nil {
		// *url.Error carries the FULL URL including the key. sanitizeError
		// rebuilds it around the redacted one before it can reach a log.
		wrapped := fmt.Errorf("%w: %w", ErrTransport, sanitizeError(c.redact, err))
		c.metrics.observeRequest(req.endpoint, outcomeError, elapsed.Seconds())
		c.metrics.observeError(req.endpoint, transportCode(err))
		c.recordSpanError(span, wrapped)
		return nil, wrapped
	}
	defer func() {
		// Drain a little before closing so the connection can be reused; an
		// undrained body forces a new TCP+TLS handshake on the next poll.
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 4<<10))
		_ = httpResp.Body.Close()
	}()

	span.SetAttributes(attribute.Int(attrHTTPStatus, httpResp.StatusCode))

	// Quota headers arrive on EVERY response, error responses included, so
	// they are read before the status is judged. A 401 for an exhausted quota
	// is exactly the response whose headers matter most.
	obs := c.limiter.ObserveHeaders(httpResp.Header)
	if !obs.HaveRemaining {
		c.metrics.observeHeaderMissing()
	}

	charged := int64(req.cost)
	if obs.HaveLastCost {
		charged = obs.LastCost
		// The provider documents that "if no events are returned, the request
		// will not count against the usage quota", and bills the event-odds
		// endpoint on markets RETURNED. Both make the real charge lower than
		// the reservation, and the difference has to go back in the bucket or
		// an out-of-season league drains the month polling an empty slate.
		if refund := int64(req.cost) - charged; refund > 0 {
			c.limiter.Refund(int(refund))
		}
	}
	c.metrics.observeCredits(req.endpoint, charged)

	body, readErr := readLimited(httpResp.Body, c.cfg.MaxResponseBytes)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		apiErr := c.apiError(req.endpoint, httpResp, body, obs)
		c.metrics.observeRequest(req.endpoint, outcomeError, elapsed.Seconds())
		c.metrics.observeError(req.endpoint, metricCode(apiErr))
		span.SetAttributes(attribute.String(attrErrorCode, apiErr.Code))
		c.recordSpanError(span, apiErr)
		return nil, apiErr
	}

	if readErr != nil {
		wrapped := fmt.Errorf("%w: read body: %w", ErrMalformedResponse, sanitizeError(c.redact, readErr))
		c.metrics.observeRequest(req.endpoint, outcomeError, elapsed.Seconds())
		c.metrics.observeError(req.endpoint, "malformed_response")
		c.recordSpanError(span, wrapped)
		return nil, wrapped
	}

	c.metrics.observeRequest(req.endpoint, outcomeOK, elapsed.Seconds())
	span.SetAttributes(
		attribute.Int(attrHTTPBodySize, len(body)),
		attribute.Int64(attrCreditActual, charged),
	)
	span.SetStatus(codes.Ok, "")

	return &rawResponse{body: body, fetchedAt: c.now(), charged: charged}, nil
}

// apiError builds the typed error for a non-2xx response.
func (c *Client) apiError(endpoint string, resp *http.Response, body []byte, obs HeaderObservation) *APIError {
	excerpt := string(body)
	if len(excerpt) > errorBodyExcerpt {
		excerpt = excerpt[:errorBodyExcerpt] + "…"
	}
	// Redacted before it is stored, not before it is printed: a field that is
	// only sanitised on the way out gets read directly by someone eventually.
	excerpt = c.redact.String(strings.TrimSpace(excerpt))

	code := classifyErrorCode(string(body))
	return &APIError{
		Endpoint:   endpoint,
		StatusCode: resp.StatusCode,
		Code:       code,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now()),
		Body:       excerpt,
		kind:       classifyStatus(resp.StatusCode, code, obs.Remaining, obs.HaveRemaining),
	}
}

// decode unmarshals a successful response into v.
func (c *Client) decode(resp *rawResponse, endpoint string, v any) error {
	// Unknown fields are ACCEPTED. The provider's own schema asks callers to
	// "allow for the addition of new market types in future", and several
	// fields appear only when an include* parameter is set. Strict decoding
	// would turn every additive provider change into an outage.
	if err := json.Unmarshal(resp.body, v); err != nil {
		excerpt := string(resp.body)
		if len(excerpt) > errorBodyExcerpt {
			excerpt = excerpt[:errorBodyExcerpt] + "…"
		}
		c.metrics.observeError(endpoint, "malformed_response")
		return fmt.Errorf("%w: %s: %w: %s",
			ErrMalformedResponse, endpoint, err, c.redact.String(strings.TrimSpace(excerpt)))
	}
	return nil
}

// publishQuota pushes the limiter's current state onto the gauges. It runs
// after every attempt, successful or not, because a 401 carries the headers
// that matter most.
func (c *Client) publishQuota() {
	c.metrics.observeQuota(c.limiter.Quota(), c.limiter.FrequencyTokens(), c.limiter.HeaderMissingCount())
}

// recordSpanError marks a span failed. The message is already sanitized —
// every error that reaches here has passed through sanitizeError or was
// constructed from redacted parts — so the key cannot land in a trace.
func (c *Client) recordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, c.redact.String(err.Error()))
}

// retryDelay picks how long to wait before the next attempt.
//
// The provider's Retry-After, when it sends one, WINS over the computed
// backoff even if it is longer: it is the only party that knows when its own
// throttle window closes, and ignoring it is how a 429 becomes a ban.
func (c *Client) retryDelay(err error, attempt int) time.Duration {
	if after, ok := RetryAfter(err); ok {
		return after
	}
	backoff := baseBackoff << (attempt - 1)
	if backoff > maxBackoff || backoff <= 0 {
		backoff = maxBackoff
	}
	return c.jitter(backoff)
}

// retryReason is the bounded label for why an attempt was retried.
func retryReason(err error) string {
	switch {
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrProviderFailure):
		return "server_error"
	case errors.Is(err, ErrTransport):
		return "transport"
	default:
		return "unknown"
	}
}

// fullJitter spreads a backoff uniformly over [0, d).
//
// Full jitter rather than a fixed delay because ingest polls several leagues on
// the same schedule: without it, a provider blip makes four sweeps fail
// together and then retry together, which is the same burst that produced the
// 429. ADR 0003's verification section flags this explicitly — "the ingest
// scheduler should jitter its sweeps and handle HTTP 429 with backoff
// regardless".
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}

// sleepContext waits for d, or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// readLimited reads at most limit bytes, and reports a body that exceeded it as
// an error rather than silently truncating — a truncated JSON document fails to
// decode with a confusing message far from the cause.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return body, nil
}

// parseRetryAfter reads the Retry-After header in either documented form:
// delta-seconds, or an HTTP-date.
func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// transportCode maps a transport failure onto a BOUNDED metric label.
func transportCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "connection"
	}
	return "transport"
}
