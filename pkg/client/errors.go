package client

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The sentinels a caller switches on.
//
// # Why there are two families and why both match
//
// A caller usually wants one of two granularities. "Was this an auth problem?"
// is answered by the status-class sentinel; "was it a wrong password, a missing
// second factor, or a suspended account?" is answered by the code sentinel.
// Every [APIError] matches BOTH — errors.Is(err, ErrUnauthenticated) and
// errors.Is(err, ErrTOTPRequired) are simultaneously true for a login that
// needs a code — so neither audience has to learn the other's vocabulary.
//
// The code family is the closed `Error.code` enum from openapi.yaml. There is
// deliberately no sentinel for a code the spec does not define: a token the
// server cannot emit is a branch that can never be taken.
var (
	// ErrInvalidRequest covers 400 and 422 — the request was malformed or was
	// syntactically valid and semantically impossible.
	ErrInvalidRequest = errors.New("sharpline: invalid request")
	// ErrUnauthenticated is 401: no access token, or one that failed
	// verification.
	ErrUnauthenticated = errors.New("sharpline: unauthenticated")
	// ErrForbidden is 403: authenticated, but not permitted.
	ErrForbidden = errors.New("sharpline: forbidden")
	// ErrNotFound is 404. It is never used to signal an authorization failure,
	// which is a promise the spec makes and this SDK relies on.
	ErrNotFound = errors.New("sharpline: not found")
	// ErrConflict is 409: the state the request was based on has moved.
	ErrConflict = errors.New("sharpline: conflict")
	// ErrRateLimited is 429. [APIError.RetryAfter] carries the server's advice.
	ErrRateLimited = errors.New("sharpline: rate limited")
	// ErrServer is any 5xx. The cause is in the server's logs under
	// [APIError.RequestID]; nothing about it is knowable from here, by design.
	ErrServer = errors.New("sharpline: server error")

	// ErrInvalidCredentials is a wrong email or password. The server cannot
	// distinguish the two and neither can this — an API that could would be an
	// account enumeration oracle.
	ErrInvalidCredentials = errors.New("sharpline: invalid credentials")
	// ErrTOTPRequired means the account has a confirmed second factor and the
	// login must be repeated with Credentials.TOTPCode set.
	ErrTOTPRequired = errors.New("sharpline: totp code required")
	// ErrInvalidTOTPCode means the code was wrong or already used.
	ErrInvalidTOTPCode = errors.New("sharpline: invalid totp code")
	// ErrAccountNotActive means the account is suspended, self-excluded or
	// closed. Credentials were correct; the account may not be used.
	ErrAccountNotActive = errors.New("sharpline: account is not active")
	// ErrAlreadyExists is a 409 on register: that email is taken.
	ErrAlreadyExists = errors.New("sharpline: already exists")
	// ErrInvalidCursor means a page cursor was presented against a different
	// ordering or a different filter than the one it was minted under. Restart
	// the listing from the first page rather than trying to repair it.
	ErrInvalidCursor = errors.New("sharpline: invalid cursor")
	// ErrInvalidParameter is a specific query or path parameter the server
	// rejected. [APIError.InvalidParams] names which.
	ErrInvalidParameter = errors.New("sharpline: invalid parameter")
)

// APIError is a non-2xx answer from the Sharpline API, decoded.
//
// It carries the request id, which is the whole point: the server drops
// everything a caller must not learn and logs it under that id, so quoting it
// is the only way to have a productive conversation about a 500.
type APIError struct {
	// Op is the operation that failed, as "POST /auth/login". It is the SDK's
	// own label, never a URL — a URL could carry a token a caller put in a
	// query string, and this string ends up in log output.
	Op string
	// Status is the HTTP status code.
	Status int
	// Code is the server's stable machine-readable token. Prefer matching a
	// sentinel with errors.Is over comparing this directly: the sentinels are
	// what stay stable if the enum grows.
	Code ErrorCode
	// Message is written for a human and is chosen from a closed set on the
	// server. It never contains a Go error, a SQLSTATE, a query or a path.
	Message string
	// RequestID correlates this failure with the server log line and trace
	// span that produced it.
	RequestID string
	// InvalidParams names the offending parameters on a 400 or 422, so a form
	// can highlight them.
	InvalidParams []InvalidParam
	// RetryAfter is the server's Retry-After advice on a 429, or zero.
	RetryAfter time.Duration
}

// Error implements error.
func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("sharpline: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString(strconv.Itoa(e.Status))
	if e.Code != "" {
		b.WriteString(" ")
		b.WriteString(string(e.Code))
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.RequestID != "" {
		b.WriteString(" (request_id=")
		b.WriteString(e.RequestID)
		b.WriteString(")")
	}
	return b.String()
}

// Is reports whether this error matches one of the package sentinels, so a
// caller writes errors.Is rather than comparing status codes by hand.
//
// It matches on the CODE first and falls back to the status class, because the
// code is the finer fact and the server is explicit that "the HTTP status is a
// coarser classification of the same fact".
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrInvalidCredentials:
		return e.Code == ErrorCodeInvalidCredentials
	case ErrTOTPRequired:
		return e.Code == ErrorCodeTotpRequired
	case ErrInvalidTOTPCode:
		return e.Code == ErrorCodeInvalidTotpCode
	case ErrAccountNotActive:
		return e.Code == ErrorCodeAccountNotActive
	case ErrAlreadyExists:
		return e.Code == ErrorCodeAlreadyExists
	case ErrInvalidCursor:
		return e.Code == ErrorCodeInvalidCursor
	case ErrInvalidParameter:
		return e.Code == ErrorCodeInvalidParameter

	case ErrInvalidRequest:
		return e.Status == http.StatusBadRequest || e.Status == http.StatusUnprocessableEntity
	case ErrUnauthenticated:
		return e.Status == http.StatusUnauthorized
	case ErrForbidden:
		return e.Status == http.StatusForbidden
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	case ErrRateLimited:
		return e.Status == http.StatusTooManyRequests
	case ErrServer:
		return e.Status >= http.StatusInternalServerError
	}
	return false
}

// Retryable reports whether repeating the same request could plausibly
// succeed.
//
// It answers only "is this failure transient", never "may this request be
// repeated" — that second question depends on the method and is decided in
// client.go, because repeating a mutating call is unsafe no matter how
// transient the failure was. The refresh endpoint is the sharpest example: a
// second redemption of a rotated token is indistinguishable from theft and
// revokes the entire login family.
func (e *APIError) Retryable() bool {
	switch e.Status {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// asAPIError builds an APIError from a decoded body, filling in what the body
// could not carry.
func asAPIError(op string, status int, body *Error, retryAfter time.Duration) *APIError {
	e := &APIError{Op: op, Status: status, RetryAfter: retryAfter}
	if body != nil {
		e.Code = body.Code
		e.Message = body.Message
		e.RequestID = body.RequestId
		if body.InvalidParams != nil {
			e.InvalidParams = *body.InvalidParams
		}
	}
	if e.Message == "" {
		// A proxy 502 or a truncated body has no envelope to decode. Say so
		// plainly rather than inventing a code the server never sent.
		e.Message = http.StatusText(status)
	}
	return e
}

// ErrEmptyBaseURL and friends: configuration failures, reported at
// construction so a misconfigured client cannot make a request at all.
var (
	// ErrInvalidOptions is returned by [New] for unusable [Options].
	ErrInvalidOptions = errors.New("sharpline: invalid client options")
	// ErrNoSession is returned when an authenticated call is made on a client
	// that has no token source. Use [Client.WithSession].
	ErrNoSession = errors.New("sharpline: no session; call Login or Resume and then WithSession")
	// ErrSessionExpired is returned when the refresh token itself has expired
	// or been revoked. The user must authenticate again; there is no recovery.
	ErrSessionExpired = errors.New("sharpline: session expired; authenticate again")
)

// wrapOp annotates a transport-level failure with the operation, without ever
// including the URL — a URL can carry a token a caller put in a query string,
// and an error string ends up in logs.
func wrapOp(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("sharpline: %s: %w", op, err)
}
