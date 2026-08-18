package theoddsapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors. Each one is a DIFFERENT decision for the caller, which is
// why there are seven of them and not one:
//
//	ErrUnauthenticated  a human must fix the deployment. Retrying never helps.
//	ErrQuotaExhausted   the month's credits are gone. Alert, degrade visibly.
//	ErrInvalidRequest   this package built a request the provider rejects. Bug.
//	ErrNotFound         this one event is gone. Skip it, keep the sweep going.
//	ErrRateLimited      too fast. Back off for RetryAfter and retry.
//	ErrProviderFailure  their fault, probably transient. Retry.
//	ErrTransport        never reached them. Retry.
//	ErrBudgetExhausted  OUR limiter refused. No request was sent, no credit spent.
//	ErrMalformedResponse they answered 200 with something undecodable.
//
// CLAUDE.md §12 puts domain sentinels in the domain package; these are adapter
// level, matched with errors.Is.
var (
	ErrUnauthenticated   = errors.New("theoddsapi: api key missing, invalid or deactivated")
	ErrQuotaExhausted    = errors.New("theoddsapi: provider usage credits exhausted")
	ErrInvalidRequest    = errors.New("theoddsapi: provider rejected the request parameters")
	ErrNotFound          = errors.New("theoddsapi: event not found or expired")
	ErrRateLimited       = errors.New("theoddsapi: rate limited by provider")
	ErrProviderFailure   = errors.New("theoddsapi: provider server error")
	ErrTransport         = errors.New("theoddsapi: transport failure reaching provider")
	ErrBudgetExhausted   = errors.New("theoddsapi: local request budget exhausted")
	ErrMalformedResponse = errors.New("theoddsapi: provider response could not be decoded")
)

// Documented error-code tokens, transcribed from
// https://the-odds-api.com/liveapi/guides/v4/api-error-codes.html
// (retrieved 2026-08-17; the same list is recorded in
// testdata/docsamples/SOURCE.md).
//
// These are the strings the provider names in its documentation. They are used
// as METRIC LABEL VALUES, which is only safe because the set is closed: a body
// that contains none of them yields the empty code and is labelled by status
// class instead. Never label a metric with an unmatched substring of a response
// body — a provider can put arbitrary bytes there.
const (
	CodeMissingKey        = "MISSING_KEY"
	CodeInvalidKey        = "INVALID_KEY"
	CodeDeactivatedKey    = "DEACTIVATED_KEY"
	CodeExceededFreqLimit = "EXCEEDED_FREQ_LIMIT"
	CodeOutOfUsageCredits = "OUT_OF_USAGE_CREDITS"
	CodeEventNotFound     = "EVENT_NOT_FOUND"
)

// documentedCodes is every error code the provider's error-codes page names, in
// the order that page lists them. Longest-match-first ordering matters for the
// substring scan: INVALID_EVENT_ID is a prefix of INVALID_EVENT_IDS, and
// INVALID_MARKET is a prefix of INVALID_MARKET_COMBO, so the longer token must
// be tested first or a INVALID_MARKET_COMBO body would report INVALID_MARKET.
var documentedCodes = []string{
	// Authentication / quota — all arrive as 401.
	CodeMissingKey,
	CodeInvalidKey,
	CodeDeactivatedKey,
	CodeOutOfUsageCredits,

	// Throttling — 429.
	CodeExceededFreqLimit,

	// Not found — 404, event-scoped endpoints only.
	CodeEventNotFound,

	// Parameter validation — 422. Longer tokens before their prefixes.
	"HISTORICAL_UNAVAILABLE_ON_FREE_USAGE_PLAN",
	"HISTORICAL_MARKETS_UNAVAILABLE_AT_DATE",
	"INVALID_INCLUDE_ROTATION_NUMBERS",
	"INVALID_INCLUDE_BET_LIMITS",
	"INVALID_INCLUDE_MULTIPLIERS",
	"MISSING_HISTORICAL_TIMESTAMP",
	"INVALID_HISTORICAL_TIMESTAMP",
	"INVALID_COMMENCE_TIME_RANGE",
	"INVALID_COMMENCE_TIME_FROM",
	"INVALID_ALL_SPORTS_PARAM",
	"INVALID_SCORES_DAYS_FROM",
	"INVALID_COMMENCE_TIME_TO",
	"INVALID_PARTICIPANT_ID",
	"INVALID_MARKET_COMBO",
	"INVALID_INCLUDE_LINKS",
	"INVALID_INCLUDE_SIDS",
	"INVALID_BOOKMAKERS",
	"INVALID_DATE_FORMAT",
	"INVALID_ODDS_FORMAT",
	"INVALID_EVENT_IDS",
	"INVALID_EVENT_ID",
	"MISSING_REGION",
	"INVALID_REGION",
	"MISSING_MARKET",
	"INVALID_MARKET",
	"UNKNOWN_SPORT",
	"INVALID_SPORT",
	"INVALID_STATUS",
}

// classifyErrorCode returns the first documented error code that appears in
// body, or "" when none does.
//
// It is a SUBSTRING SCAN, not a JSON decode, and that is deliberate. The
// provider documents the code names but never publishes the envelope they
// arrive in — no example error body appears anywhere in its documentation or in
// its OpenAPI document. Decoding a guessed `{"error_code": …}` shape would make
// this package depend on a field name nobody has ever seen, and would fail
// silently (yielding "") the moment the guess is wrong. Scanning for the tokens
// themselves works whatever wraps them.
func classifyErrorCode(body string) string {
	if body == "" {
		return ""
	}
	upper := strings.ToUpper(body)
	for _, code := range documentedCodes {
		if strings.Contains(upper, code) {
			return code
		}
	}
	return ""
}

// APIError is a non-2xx response from the provider.
//
// It carries the status code, the documented error code when one was
// recognised, and a truncated, REDACTED excerpt of the body. It unwraps to
// exactly one of the sentinels above, so `errors.Is(err, ErrQuotaExhausted)`
// works without the caller knowing this type exists.
type APIError struct {
	// Endpoint is the bounded path TEMPLATE ("/v4/sports/{sport}/odds"), never
	// the concrete path, so it is safe as a metric label.
	Endpoint string

	// StatusCode is the HTTP status returned.
	StatusCode int

	// Code is the documented error code found in the body, or "" if the body
	// contained none.
	Code string

	// RetryAfter is the delay the provider asked for via the Retry-After
	// header, or 0 when it sent none. Only meaningful with ErrRateLimited.
	RetryAfter time.Duration

	// Body is a redacted, truncated excerpt of the response body, kept for the
	// error message only. It is never used as a metric label.
	Body string

	// kind is the sentinel this error unwraps to.
	kind error
}

// Error implements error. The API key cannot appear here: Body is redacted at
// construction and Endpoint is a template with no query string.
func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("theoddsapi: ")
	b.WriteString(e.Endpoint)
	b.WriteString(": http ")
	fmt.Fprint(&b, e.StatusCode)
	if e.Code != "" {
		b.WriteString(" ")
		b.WriteString(e.Code)
	}
	if e.RetryAfter > 0 {
		b.WriteString(" (retry after ")
		b.WriteString(e.RetryAfter.String())
		b.WriteString(")")
	}
	b.WriteString(": ")
	b.WriteString(e.kind.Error())
	if e.Body != "" {
		b.WriteString(": ")
		b.WriteString(e.Body)
	}
	return b.String()
}

// Unwrap returns the sentinel, so errors.Is discriminates the failure mode.
func (e *APIError) Unwrap() error { return e.kind }

// classifyStatus maps a response onto its sentinel.
//
// The interesting case is 401. The provider's OpenAPI document describes it as
// "the API key might be missing or invalid (unauthenticated), or it might at
// its usage limit (unauthorized)" — two failures with opposite remedies behind
// one status code. Discrimination is therefore attempted twice:
//
//  1. by documented error code in the body (OUT_OF_USAGE_CREDITS vs the key
//     codes), which is authoritative when present;
//  2. failing that, by the provider's own quota header: a 401 arriving
//     alongside `x-requests-remaining: 0` is a quota exhaustion, because a key
//     that is invalid has no quota to report.
//
// When neither signal is available a 401 falls back to ErrUnauthenticated. That
// is the safer default of the two: it is fatal, so it stops the poller and
// surfaces a human-fixable error, whereas mislabelling a bad key as a quota
// problem would have ingest wait for a monthly reset that changes nothing.
func classifyStatus(status int, code string, remaining int64, haveRemaining bool) error {
	switch {
	case status == 401:
		switch code {
		case CodeOutOfUsageCredits:
			return ErrQuotaExhausted
		case CodeMissingKey, CodeInvalidKey, CodeDeactivatedKey:
			return ErrUnauthenticated
		}
		if haveRemaining && remaining <= 0 {
			return ErrQuotaExhausted
		}
		return ErrUnauthenticated
	case status == 404:
		return ErrNotFound
	case status == 422:
		return ErrInvalidRequest
	case status == 429:
		return ErrRateLimited
	case status >= 500:
		return ErrProviderFailure
	case status >= 400:
		// Undocumented 4xx. Treat as a request defect rather than as something
		// worth retrying — a client error that repeats is a client error.
		return ErrInvalidRequest
	default:
		return ErrProviderFailure
	}
}

// Retryable reports whether re-issuing the identical request could plausibly
// succeed without anything else changing.
//
// ErrQuotaExhausted is deliberately NOT retryable: the credits return at the
// monthly reset, not after a backoff, and a poller that retries into an
// exhausted quota generates 401s for the rest of the month. ADR 0003 is
// explicit that this state must "fail to synthetic, not fail to stale" — that
// decision belongs to the caller, and it can only make it if this returns
// false.
func Retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// A cancelled parent context is a decision, not a failure. Honour it.
		return false
	case errors.Is(err, ErrRateLimited),
		errors.Is(err, ErrProviderFailure),
		errors.Is(err, ErrTransport):
		return true
	default:
		return false
	}
}

// Fatal reports whether the error will persist until a human changes something
// — a wrong key, or a request this package builds incorrectly. A poller seeing
// a fatal error should stop rather than spin.
func Fatal(err error) bool {
	return errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrInvalidRequest)
}

// RetryAfter returns the delay the provider asked for, if it asked for one.
func RetryAfter(err error) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter, true
	}
	var budgetErr *BudgetError
	if errors.As(err, &budgetErr) && budgetErr.RetryAfter > 0 {
		return budgetErr.RetryAfter, true
	}
	return 0, false
}

// ErrorCode returns the documented provider error code carried by err, if any.
// The empty string means the response body named no documented code.
func ErrorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// BudgetError is returned when OUR limiter refused to issue a request. No HTTP
// call was made and no credit was spent.
//
// It is separate from APIError because the remedy is different: an APIError
// means the provider said no, a BudgetError means we said no, and the second
// one is a signal that the configured cadence exceeds the configured budget.
type BudgetError struct {
	// Limiter names which bucket refused: "credits" or "frequency".
	Limiter string

	// Cost is the credit cost of the request that was refused.
	Cost int

	// RetryAfter is how long until the bucket holds enough tokens. Zero means
	// the request can never be satisfied — its cost exceeds bucket capacity,
	// which is a configuration error rather than a wait.
	RetryAfter time.Duration
}

// Error implements error.
func (e *BudgetError) Error() string {
	if e.RetryAfter <= 0 {
		return fmt.Sprintf("theoddsapi: %s budget cannot ever satisfy a %d-credit request "+
			"(cost exceeds bucket capacity — raise the burst or lower the request cost): %s",
			e.Limiter, e.Cost, ErrBudgetExhausted.Error())
	}
	return fmt.Sprintf("theoddsapi: %s budget exhausted, %d credits available in %s: %s",
		e.Limiter, e.Cost, e.RetryAfter.Round(time.Millisecond), ErrBudgetExhausted.Error())
}

// Unwrap returns ErrBudgetExhausted.
func (e *BudgetError) Unwrap() error { return ErrBudgetExhausted }

// sanitizeError strips the API key out of a transport error.
//
// net/http returns *url.Error from Client.Do, and url.Error.Error() formats the
// FULL request URL — including `?apiKey=…`. Wrapping one of those with %w and
// logging it publishes the credential. So the *url.Error is rebuilt around the
// redacted URL, and the result is passed through r.String as a second pass in
// case the key reached the message by some other route.
//
// The returned error still unwraps to whatever the original wrapped (a
// net.Error, context.DeadlineExceeded, …), so timeout detection upstream is
// unaffected.
func sanitizeError(r redactor, err error) error {
	if err == nil {
		return nil
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return &url.Error{
			Op: urlErr.Op,
			// RawURL, not String: the URL in a *url.Error can be a REDIRECT
			// TARGET chosen by whoever answered, so the key may be spelled
			// differently there than the literal this redactor holds. Parsing
			// and redacting by parameter name catches that; a value replacement
			// would not.
			URL: r.RawURL(urlErr.URL),
			Err: sanitizeError(r, urlErr.Err),
		}
	}

	msg := err.Error()
	if redacted := r.String(msg); redacted != msg {
		return &redactedError{msg: redacted, err: err}
	}
	return err
}

// redactedError replaces an error's message while preserving its identity for
// errors.Is and errors.As.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
