// The provider layer's error vocabulary.
//
// # The scheduler needs three answers, not a string
//
// An ingest poller that cannot tell one failure from another has exactly one
// behaviour available to it, and every choice is wrong for two of the three
// cases: retrying a bad API key for ever, or giving up on a five-second network
// blip, or hammering a provider whose budget is gone. So every error an Adapter
// returns must be classifiable into one of three dispositions, and Classify is
// the single function that decides.
//
// # Credentials never appear in an error
//
// The contract ledger is explicit: ODDS_API_KEY "must never be logged, never
// appear in an error message, never be committed, and never be sent anywhere but
// the provider." That is not a matter of care at each call site — The Odds API
// passes its key as the `apiKey` QUERY PARAMETER, so the natural, idiomatic,
// otherwise-correct thing to do (wrap the failing request URL into the error)
// leaks the credential into every log line and every Prometheus exemplar.
//
// RedactURL exists so that the safe thing is also the easy thing, and Error's
// documentation says the rule out loud. A leaked key is not recoverable by
// deleting a log line; it is recoverable by rotating a key, which nobody
// remembers to do.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors. Callers match with errors.Is; the scheduler should prefer
// Classify, which maps all of them plus context and *Error.
var (
	// ErrQuotaExhausted means the request budget is gone. Repeating the request
	// cannot succeed until the budget refills.
	//
	// ADR 0003: "The limiter must fail to synthetic, not fail to stale. When
	// the budget is exhausted the correct behaviour is a loud alert and a
	// visible degraded state — never a board that silently shows hour-old
	// prices as if they were live."
	ErrQuotaExhausted = errors.New("provider: request quota exhausted")

	// ErrRateLimited means the provider refused this request for being too
	// frequent (HTTP 429). Retryable after a delay; see RetryAfter.
	ErrRateLimited = errors.New("provider: rate limited")

	// ErrUnavailable means the provider could not be reached or failed the
	// request for a transient reason — a dial failure, a timeout, a 5xx.
	ErrUnavailable = errors.New("provider: temporarily unavailable")

	// ErrUnauthorized means the credential was missing, wrong, or not entitled
	// to what was asked for. Fatal: retrying will not fix a key.
	ErrUnauthorized = errors.New("provider: unauthorized")

	// ErrNotFound means the provider does not know the league or event that was
	// asked for.
	ErrNotFound = errors.New("provider: scope not found")

	// ErrNotSupported means this adapter cannot serve the request at all — a
	// market type outside its plan, or a narrowing it does not implement.
	ErrNotSupported = errors.New("provider: unsupported request")

	// ErrProviderRejected means the provider refused the request for a reason
	// that repeating it will not change, and that is none of the above.
	ErrProviderRejected = errors.New("provider: request rejected")

	// ErrMalformedPayload means the response could not be decoded, or decoded
	// into something the domain refuses. It is a contract violation between us
	// and the provider, and it must be loud: silently dropping it would leave
	// the board frozen with no failure visible anywhere.
	ErrMalformedPayload = errors.New("provider: malformed payload")

	// ErrInvalidName means a provider name failed NewName's charset.
	ErrInvalidName = errors.New("provider: invalid provider name")

	// ErrInvalidScope means the caller asked for something that is not a
	// well-formed request.
	ErrInvalidScope = errors.New("provider: invalid scope")

	// ErrInvalidCatalogue means an adapter returned a catalogue that
	// contradicts itself.
	ErrInvalidCatalogue = errors.New("provider: invalid catalogue")

	// ErrInvalidSnapshot means an adapter returned a snapshot the domain
	// refuses. It is always an adapter bug, never bad luck.
	ErrInvalidSnapshot = errors.New("provider: invalid snapshot")
)

// Disposition is what the scheduler should do about an error.
type Disposition uint8

const (
	// DispositionUnknown is the zero value and is never a classification this
	// package returns; it exists so an unset Error.Disposition is detectable.
	DispositionUnknown Disposition = iota

	// DispositionRetryable means the same request may succeed if repeated after
	// a delay. Back off and try again.
	DispositionRetryable

	// DispositionFatal means repeating the request will not help: a credential,
	// a configuration, or a contract problem. Stop issuing it and surface it.
	DispositionFatal

	// DispositionQuotaExhausted means the request cannot succeed until the
	// budget refills. Distinct from retryable because the correct backoff is
	// the budget window rather than a few seconds, and because it has its own
	// alert (ProviderQuotaExhausted) and its own degraded state.
	DispositionQuotaExhausted
)

// String implements fmt.Stringer. The lowercase forms are the values written to
// the `outcome` Prometheus label, so they are a closed set.
func (d Disposition) String() string {
	switch d {
	case DispositionRetryable:
		return "retryable"
	case DispositionFatal:
		return "fatal"
	case DispositionQuotaExhausted:
		return "quota_exhausted"
	default:
		return "unknown"
	}
}

// Valid reports whether d is a defined disposition.
func (d Disposition) Valid() bool {
	return d == DispositionRetryable || d == DispositionFatal || d == DispositionQuotaExhausted
}

// Error is an adapter failure carrying its classification.
//
// # Never put a credential in one
//
// Op, Err, and anything they wrap end up in logs, in error messages, and
// potentially in a Prometheus exemplar. The Odds API passes its key as a query
// parameter, so a request URL MUST be passed through RedactURL before it is
// named in an error. There is no safe shortcut here: `fmt.Errorf("get %s: %w",
// req.URL, err)` leaks the key.
type Error struct {
	// Op is the operation that failed: "fetch", "catalogue". A short, closed
	// vocabulary — not a formatted request description.
	Op string

	// Provider is the adapter that failed.
	Provider Name

	// Disposition is what the caller should do. Required; an *Error with
	// DispositionUnknown falls through to sentinel classification.
	Disposition Disposition

	// RetryAfter is how long to wait before retrying, when the provider said so
	// (a Retry-After header). Zero means "no advice given"; it does not mean
	// "retry immediately".
	RetryAfter time.Duration

	// Status is the HTTP status code when the source is an HTTP API, zero
	// otherwise.
	Status int

	// Err is the wrapped cause: one of this file's sentinels, or a transport
	// error, or both.
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("provider")
	if !e.Provider.IsZero() {
		b.WriteString(" ")
		b.WriteString(e.Provider.String())
	}
	if e.Op != "" {
		b.WriteString(" ")
		b.WriteString(e.Op)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, " (http %d)", e.Status)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// Newf builds an *Error wrapping a sentinel with formatted context.
//
// The format arguments are the caller's responsibility: pass identifiers and
// counts, never a URL that has not been through RedactURL and never a header
// value.
func Newf(op string, name Name, d Disposition, sentinel error, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	var cause error
	switch {
	case sentinel == nil:
		cause = errors.New(msg)
	case msg == "":
		cause = sentinel
	default:
		cause = fmt.Errorf("%w: %s", sentinel, msg)
	}
	return &Error{Op: op, Provider: name, Disposition: d, Err: cause}
}

// QuotaExhausted builds the error an adapter returns when its budget is gone.
//
// It is a named constructor rather than a raw sentinel because a provider may
// signal exhaustion through a status code that means something else in general —
// so the adapter has to make the translation explicitly, at the point where it
// knows the provider's convention, rather than a generic status mapper guessing.
func QuotaExhausted(op string, name Name, q Quota) *Error {
	return &Error{
		Op:          op,
		Provider:    name,
		Disposition: DispositionQuotaExhausted,
		Err:         fmt.Errorf("%w: %s", ErrQuotaExhausted, q),
	}
}

// FromHTTPStatus maps an HTTP status onto a classified error.
//
// It exists so the two adapters cannot disagree about what a 429 means. The
// mapping is deliberately conservative about quota: nothing here returns
// DispositionQuotaExhausted, because providers signal exhaustion through
// different statuses and guessing would either page falsely or hide a real
// exhaustion. An adapter that knows its provider's convention calls
// QuotaExhausted explicitly.
//
// retryAfter carries a parsed Retry-After header, or zero when none was given.
// body is NOT accepted as a parameter on purpose: an error message is not the
// place to echo an untrusted response body, and a provider that reflects the
// request URL in its error body would reflect the API key with it.
func FromHTTPStatus(op string, name Name, status int, retryAfter time.Duration) *Error {
	e := &Error{Op: op, Provider: name, Status: status, RetryAfter: retryAfter}
	switch {
	case status == 401, status == 403:
		e.Disposition = DispositionFatal
		e.Err = ErrUnauthorized
	case status == 404:
		e.Disposition = DispositionFatal
		e.Err = ErrNotFound
	case status == 422:
		e.Disposition = DispositionFatal
		e.Err = ErrInvalidScope
	case status == 429:
		e.Disposition = DispositionRetryable
		e.Err = ErrRateLimited
	case status >= 500:
		e.Disposition = DispositionRetryable
		e.Err = ErrUnavailable
	case status >= 400:
		e.Disposition = DispositionFatal
		e.Err = ErrProviderRejected
	default:
		// A 1xx/2xx/3xx reaching here means the caller treated a non-failure as
		// a failure, or followed no redirect. Either is a bug in the adapter,
		// not a transient condition, so it must not be retried for ever.
		e.Disposition = DispositionFatal
		e.Err = fmt.Errorf("%w: unexpected status", ErrProviderRejected)
	}
	return e
}

// Classify decides what to do about an error.
//
// # Why an unrecognised error is retryable
//
// The default has to be wrong in one direction, and the two failure modes are
// not symmetric. Defaulting to fatal stops ingestion outright on any error
// nobody thought to classify — a wrapped net.OpError from a library update, say
// — and a board frozen at the last observation is the single worst outcome this
// system has. Defaulting to retryable keeps polling, and the condition is still
// visible: sharpline_provider_requests_total{outcome="retryable"} climbs and
// staleness climbs with it, which is what the alerts watch.
//
// The cost of the default is that a genuinely permanent unclassified failure is
// retried indefinitely. That is a bounded, observable cost, and the fix is to
// classify the error rather than to change the default.
func Classify(err error) Disposition {
	if err == nil {
		return DispositionUnknown
	}

	var pe *Error
	if errors.As(err, &pe) && pe.Disposition.Valid() {
		return pe.Disposition
	}

	switch {
	case errors.Is(err, ErrQuotaExhausted):
		return DispositionQuotaExhausted

	case errors.Is(err, ErrRateLimited),
		errors.Is(err, ErrUnavailable),
		errors.Is(err, context.DeadlineExceeded):
		// A deadline exceeded on one poll says nothing about the next one; the
		// scheduler's own context cancellation is what stops the loop, not this
		// classification.
		return DispositionRetryable

	case errors.Is(err, ErrUnauthorized),
		errors.Is(err, ErrNotFound),
		errors.Is(err, ErrNotSupported),
		errors.Is(err, ErrProviderRejected),
		errors.Is(err, ErrMalformedPayload),
		errors.Is(err, ErrInvalidName),
		errors.Is(err, ErrInvalidScope),
		errors.Is(err, ErrInvalidCatalogue),
		errors.Is(err, ErrInvalidSnapshot),
		errors.Is(err, context.Canceled):
		return DispositionFatal

	default:
		return DispositionRetryable
	}
}

// IsRetryable reports whether the same request is worth repeating after a delay.
func IsRetryable(err error) bool { return Classify(err) == DispositionRetryable }

// IsQuotaExhausted reports whether the request failed because the budget is gone.
func IsQuotaExhausted(err error) bool { return Classify(err) == DispositionQuotaExhausted }

// IsFatal reports whether repeating the request cannot help.
func IsFatal(err error) bool { return Classify(err) == DispositionFatal }

// RetryAfter returns the delay a provider asked for, and whether it gave one.
func RetryAfter(err error) (time.Duration, bool) {
	var pe *Error
	if errors.As(err, &pe) && pe.RetryAfter > 0 {
		return pe.RetryAfter, true
	}
	return 0, false
}

// redactedValue replaces a credential in a redacted URL. It is a fixed string
// rather than a length-preserving mask, because a mask leaks the key's length.
const redactedValue = "REDACTED"

// redactedURL is what RedactURL returns when it cannot parse its input. It never
// returns the raw string in that case: an unparseable URL is exactly the shape a
// malformed one takes, and the credential may still be inside it.
const redactedURL = "[unparseable url redacted]"

// sensitiveParams are the query parameter names whose values are redacted.
//
// `apiKey` is the one that matters — it is how The Odds API authenticates — but
// the rest are here because the cost of over-redacting a query parameter is
// nothing and the cost of under-redacting one is a rotated credential.
var sensitiveParams = map[string]bool{
	"apikey":       true,
	"api_key":      true,
	"key":          true,
	"token":        true,
	"access_token": true,
	"auth":         true,
	"password":     true,
	"secret":       true,
	"signature":    true,
}

// RedactURL renders a URL with every credential-bearing query parameter and any
// userinfo replaced, so it is safe to put in a log line or an error.
//
// Use it on EVERY provider URL that reaches an error, a log, or a span
// attribute. The contract ledger's rule — the key "must never be logged, never
// appear in an error message" — is mechanical here rather than a matter of
// remembering, and that is the point.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redactedURL
	}
	return redactURL(u)
}

// RedactURLValue is RedactURL for an already-parsed URL. It does not mutate u.
func RedactURLValue(u *url.URL) string {
	if u == nil {
		return ""
	}
	return redactURL(u)
}

func redactURL(u *url.URL) string {
	clone := *u
	if clone.User != nil {
		clone.User = url.User(redactedValue)
	}
	if clone.RawQuery != "" {
		q := clone.Query()
		for k := range q {
			if sensitiveParams[strings.ToLower(k)] {
				q.Set(k, redactedValue)
			}
		}
		clone.RawQuery = q.Encode()
	}
	// Fragments carry no provider credential today, but they are attacker- and
	// caller-controlled and cost nothing to drop.
	clone.Fragment = ""
	clone.RawFragment = ""
	return clone.String()
}
