package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults. Every one is overridable through [Options]; none is zero-valued
// into something dangerous.
const (
	// DefaultBasePath is the path openapi.yaml declares as the server URL.
	// [New] appends it when the configured base URL has no path of its own, so
	// "https://sharpline.example" and "https://sharpline.example/api/v1" both
	// work and neither produces a double prefix.
	DefaultBasePath = "/api/v1"

	// DefaultUserAgent identifies this SDK in the server's access log. It
	// carries no user, host or account information.
	DefaultUserAgent = "sharpline-go"

	// DefaultTimeout bounds one HTTP attempt when [Options.HTTPClient] is not
	// supplied. A caller passing their own client owns their own deadline; a
	// caller passing none must not get net/http's default of "no timeout at
	// all", which is how a CLI hangs forever on a black-holed connection.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxAttempts is the total number of attempts, including the first.
	DefaultMaxAttempts = 3
	// DefaultRetryBaseDelay is the first backoff interval.
	DefaultRetryBaseDelay = 100 * time.Millisecond
	// DefaultRetryMaxDelay caps the backoff.
	DefaultRetryMaxDelay = 2 * time.Second

	// maxErrorBody caps how much of an error body is read before decoding. A
	// proxy or a misconfigured upstream can answer with an HTML page of
	// arbitrary size, and a client that reads all of it into memory to discover
	// it is not JSON has handed the server a way to exhaust it.
	maxErrorBody = 1 << 20
)

// Doer is the consumer-declared seam over the HTTP transport (CLAUDE.md §12:
// "Interfaces are declared by the consumer, not the producer"). *http.Client
// satisfies it, and so does a test double, an instrumented round tripper, or a
// client with a custom TLS configuration — none of which this package needs to
// know about.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// RetryPolicy bounds automatic retries.
//
// It applies ONLY to requests this SDK considers safe to repeat, which is GET
// and HEAD and nothing else. See [Client.do] for why no mutating request is
// ever retried, however transient the failure looked.
type RetryPolicy struct {
	// MaxAttempts is the total attempts including the first. Zero means
	// [DefaultMaxAttempts]; 1 disables retrying.
	MaxAttempts int
	// BaseDelay is the first backoff interval. Zero means
	// [DefaultRetryBaseDelay].
	BaseDelay time.Duration
	// MaxDelay caps the backoff. Zero means [DefaultRetryMaxDelay].
	MaxDelay time.Duration
	// Jitter returns a value in [0,1). Zero means math/rand/v2. It exists so a
	// test can make backoff deterministic without sleeping for real.
	Jitter func() float64
}

func (p RetryPolicy) attempts() int {
	if p.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return p.MaxAttempts
}

// delay implements FULL JITTER: a uniform draw from [0, exponential cap].
//
// Equal jitter and decorrelated jitter are the usual alternatives. Full jitter
// is chosen because the failure this policy exists for is a fleet of clients
// retrying a rate-limited or restarting API at once, and full jitter is the
// variant that spreads that thundering herd most evenly — the retries of N
// clients that failed in the same millisecond are uniform over the window
// rather than clustered at its end.
func (p RetryPolicy) delay(attempt int) time.Duration {
	base := p.BaseDelay
	if base <= 0 {
		base = DefaultRetryBaseDelay
	}
	maxDelay := p.MaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultRetryMaxDelay
	}

	// The shift overflows for a large attempt count, which is why the <= 0 arm
	// exists: a negative window would produce a negative delay and a retry
	// storm rather than a capped one.
	window := base << (attempt - 1)
	if window > maxDelay || window <= 0 {
		window = maxDelay
	}

	jitter := p.Jitter
	if jitter == nil {
		jitter = rand.Float64
	}
	return time.Duration(jitter() * float64(window))
}

// Options configures a [Client]. Everything is injected; the package holds no
// global state and reads no environment variable (CLAUDE.md §12).
type Options struct {
	// BaseURL is the API's address. Required.
	//
	// A URL with no path — "https://sharpline.example" — gets
	// [DefaultBasePath] appended. A URL that already names a path is used as
	// given, so a deployment behind a different prefix needs no code change.
	BaseURL string

	// HTTPClient performs the requests. nil means a client with
	// [DefaultTimeout] and connection pooling suited to an SDK.
	//
	// Injecting one is the supported way to add TLS settings, a proxy, OTel
	// instrumentation or a test double. This package never mutates it.
	HTTPClient Doer

	// Tokens supplies the bearer token for authenticated calls. nil means the
	// client is anonymous: the public catalogue, board, history and search work,
	// and every account call returns [ErrNoSession].
	//
	// The usual way to set this is [Client.WithSession] rather than by hand.
	Tokens TokenSource

	// UserAgent is sent on every request. Empty means [DefaultUserAgent].
	UserAgent string

	// Retry bounds automatic retries of safe requests.
	Retry RetryPolicy
}

// Client is a Sharpline API client. It is safe for concurrent use by multiple
// goroutines and holds no mutable state of its own — a rotating credential
// lives in a [Session], which has its own synchronisation.
//
// # This package deliberately has no logger
//
// Not "no logger by default": none at all. An SDK that logs requests is an SDK
// that eventually logs an Authorization header, a refresh token or a TOTP
// provisioning URI, and it does so in the caller's log pipeline where this
// package's redaction rules do not reach. Callers who want request logging
// wrap [Doer], which puts the decision — and the redaction — where they can see
// it.
type Client struct {
	base   *url.URL
	http   Doer
	tokens TokenSource
	ua     string
	retry  RetryPolicy
}

// New builds a client.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, fmt.Errorf("%w: BaseURL is empty", ErrInvalidOptions)
	}
	base, err := url.Parse(strings.TrimSpace(opts.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("%w: BaseURL: %w", ErrInvalidOptions, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("%w: BaseURL scheme is %q, want http or https", ErrInvalidOptions, base.Scheme)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("%w: BaseURL has no host", ErrInvalidOptions)
	}
	if p := strings.TrimRight(base.Path, "/"); p == "" {
		base.Path = DefaultBasePath
	} else {
		base.Path = p
	}
	// A query or fragment on a base URL is always a mistake and would be
	// silently dropped when paths are joined; refusing is kinder than
	// discarding.
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("%w: BaseURL must not carry a query or fragment", ErrInvalidOptions)
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = DefaultHTTPClient()
	}

	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}

	return &Client{
		base:   base,
		http:   httpClient,
		tokens: opts.Tokens,
		ua:     ua,
		retry:  opts.Retry,
	}, nil
}

// DefaultHTTPClient returns the transport used when [Options.HTTPClient] is
// nil: a bounded per-attempt timeout and a connection pool sized for an SDK
// rather than for a server.
func DefaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{Timeout: DefaultTimeout, Transport: transport}
}

// BaseURL reports the resolved API root, including the path prefix.
func (c *Client) BaseURL() string { return c.base.String() }

// WithSession returns a copy of this client that authenticates with s.
//
// A copy rather than a mutation: a Client is safe for concurrent use precisely
// because it is immutable after construction, and a method that swapped the
// credential on a shared client would change what an in-flight request on
// another goroutine is authenticating as.
//
// The session keeps a reference to THIS client for its refresh calls, so the
// unauthenticated client and the authenticated one share one connection pool.
func (c *Client) WithSession(s *Session) *Client {
	cp := *c
	cp.tokens = s
	return &cp
}

// -----------------------------------------------------------------------------
// request execution
// -----------------------------------------------------------------------------

// call describes one request. It is a struct rather than eight parameters so
// that a new endpoint method is a literal a reviewer can read top to bottom.
type call struct {
	// op is the human label used in errors: "GET /sports". Never a URL.
	op     string
	method string
	// path is relative to the base URL and is already escaped by the caller
	// via pathEscape.
	path  string
	query url.Values
	// body is marshalled as JSON when non-nil.
	body any
	// out receives the decoded response when non-nil and the status has a body.
	out any
	// auth requires a bearer token.
	auth bool
}

// safe reports whether this request may be repeated after a transient failure.
//
// ONLY GET AND HEAD. Not "GET, HEAD and the POSTs that look harmless": the one
// endpoint where a duplicate is catastrophic is POST /auth/refresh, and it is
// catastrophic in a way that is easy to miss. A refresh token is single-use;
// presenting a redeemed one is indistinguishable from theft, so the server
// revokes the ENTIRE login family and the user is logged out everywhere. A
// retry after a response that was lost on the wire — the exact case a retry
// policy exists for — would do precisely that. So the SDK never retries a
// mutating request, and the cost is that a caller occasionally sees a transient
// error it could have absorbed.
func (c call) safe() bool {
	return c.method == http.MethodGet || c.method == http.MethodHead
}

// do executes a call: authenticate, send, retry, decode.
func (c *Client) do(ctx context.Context, cl call) error {
	if ctx == nil {
		return fmt.Errorf("sharpline: %s: nil context", cl.op)
	}

	var payload []byte
	if cl.body != nil {
		var err error
		payload, err = json.Marshal(cl.body)
		if err != nil {
			return fmt.Errorf("sharpline: %s: encode request: %w", cl.op, err)
		}
	}

	attempts := 1
	if cl.safe() {
		attempts = c.retry.attempts()
	}

	// refreshed guards the once-only re-authentication below.
	refreshed := false

	for attempt := 1; ; attempt++ {
		token, generation, err := c.credential(ctx, cl)
		if err != nil {
			return err
		}

		resp, err := c.send(ctx, cl, payload, token)
		if err != nil {
			if attempt < attempts && retryableTransport(err) {
				if werr := wait(ctx, c.retry.delay(attempt)); werr != nil {
					return wrapOp(cl.op, werr)
				}
				continue
			}
			return wrapOp(cl.op, err)
		}

		// A 401 on an authenticated call means the access token was rejected —
		// which also means the server did nothing, so replaying the request
		// after refreshing is safe even when the method is not. This is the one
		// retry that applies to a mutating call, and it is safe for a reason
		// that has nothing to do with idempotency.
		if resp.StatusCode == http.StatusUnauthorized && cl.auth && !refreshed && c.tokens != nil {
			drain(resp)
			refreshed = true
			if err := c.tokens.Refresh(ctx, generation); err != nil {
				return err
			}
			continue
		}

		apiErr, err := c.finish(cl, resp)
		if err != nil {
			return err
		}
		if apiErr == nil {
			return nil
		}
		if attempt < attempts && apiErr.Retryable() {
			delay := apiErr.RetryAfter
			if delay <= 0 {
				delay = c.retry.delay(attempt)
			}
			if werr := wait(ctx, delay); werr != nil {
				return wrapOp(cl.op, werr)
			}
			continue
		}
		return apiErr
	}
}

// credential resolves the bearer token for this call, if it needs one.
func (c *Client) credential(ctx context.Context, cl call) (string, uint64, error) {
	if !cl.auth {
		return "", 0, nil
	}
	if c.tokens == nil {
		return "", 0, fmt.Errorf("sharpline: %s: %w", cl.op, ErrNoSession)
	}
	token, generation, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return "", 0, err
	}
	return token, generation, nil
}

// send builds and performs one attempt.
func (c *Client) send(ctx context.Context, cl call, payload []byte, token string) (*http.Response, error) {
	u := *c.base
	u.Path = c.base.Path + cl.path
	if len(cl.query) > 0 {
		u.RawQuery = cl.query.Encode()
	}

	var body io.Reader
	if payload != nil {
		// A fresh reader per attempt: a retried request must resend the whole
		// body, and a consumed reader would send an empty one.
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, cl.method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(payload))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, sanitizeTransportError(err)
	}
	return resp, nil
}

// finish consumes the response: decodes the body into cl.out on success, or
// builds an *APIError. It returns (nil, nil) on a successful call.
func (c *Client) finish(cl call, resp *http.Response) (*APIError, error) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if cl.out == nil || resp.StatusCode == http.StatusNoContent {
			drainBody(resp)
			return nil, nil
		}
		// Unknown fields are ACCEPTED, deliberately. The server may add a field
		// to a response at any time; a client that refused the whole payload
		// over one it did not recognise would break every existing caller on a
		// purely additive change.
		if err := json.NewDecoder(resp.Body).Decode(cl.out); err != nil {
			return nil, fmt.Errorf("sharpline: %s: decode response: %w", cl.op, err)
		}
		return nil, nil
	}

	var envelope *Error
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err == nil && len(raw) > 0 {
		var decoded Error
		if json.Unmarshal(raw, &decoded) == nil && decoded.Code != "" {
			envelope = &decoded
		}
	}
	return asAPIError(cl.op, resp.StatusCode, envelope, retryAfter(resp.Header)), nil
}

// drain reads and closes a response whose body is not wanted, so the
// connection returns to the pool instead of being torn down.
func drain(resp *http.Response) {
	drainBody(resp)
	_ = resp.Body.Close()
}

func drainBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
}

// retryAfter parses the server's Retry-After advice, in either of the two forms
// RFC 9110 allows.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// retryableTransport reports whether a transport failure is worth repeating.
//
// A cancelled or expired context is not: the caller asked to stop, and
// retrying would ignore them. Everything else — a refused connection, a reset,
// a DNS miss — is treated as transient, because at this layer the SDK cannot
// tell a restarting deployment from a permanent one and the retry budget is
// small.
func retryableTransport(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// sanitizeTransportError strips the URL out of a *url.Error.
//
// net/http puts the full request URL in the error string. A URL is not usually
// a secret, but this string reaches a caller's logs, and the SDK's rule is that
// nothing it produces carries a credential — a caller who appended something
// sensitive to a query would otherwise have it logged by us. The method and
// path are already in [APIError.Op].
func sanitizeTransportError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// wait sleeps for d, or returns early if ctx ends.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pathEscape escapes one path segment.
//
// url.PathEscape, not url.QueryEscape: the latter turns a space into "+", which
// is correct in a query string and wrong in a path. Every identifier this SDK
// puts in a path is already constrained to [A-Za-z0-9._-] by the spec, so this
// is defence against a caller passing something else rather than a routine
// transformation.
func pathEscape(segment string) string { return url.PathEscape(segment) }
