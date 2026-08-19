package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
	"github.com/anpl1623/sharpline/internal/httpapi/middleware"
)

// The one error shape, and the only place a Go error becomes an HTTP status.
//
// # The rule this file enforces
//
// AN ERROR BODY IS AN UNTRUSTED OUTPUT SURFACE. Nothing derived from an error
// value ever reaches it: not err.Error(), not a wrapped cause, not a SQLSTATE,
// not a driver message, not a file path, not a query. Every `message` written
// here is a constant declared in this file. The cause is logged once, under the
// request id the client was given, and dropped.
//
// That is structural rather than remembered: [fail] takes a message from a
// closed set of constants and has no parameter through which an error could
// reach the wire, and [failWith] — the only function that accepts an error —
// logs it and then calls [fail] with a constant. There is no third path.
//
// # Status codes, and what each one MEANS here
//
//	400  The request is malformed: an unparseable body, a parameter that is
//	     not of its declared type, an unknown book slug, an undecodable cursor.
//	401  No credential, or one that failed verification. NEVER used to mean
//	     "you are known but not allowed" — that is 403.
//	403  The credential is good and the actor is still refused. The account is
//	     suspended, closed, or self-excluded.
//	404  No such entity. NEVER used to hide an authorization failure: this API
//	     has no per-user catalogue, so there is nothing whose existence a 404
//	     could usefully conceal, and using 404 for authorization is how a
//	     client ends up retrying forever against a resource it may not have.
//	409  The request conflicts with current state and the client can retry
//	     after re-reading: a duplicate registration, a limit superseded
//	     concurrently.
//	422  Syntactically valid, semantically impossible: a `from` after its `to`,
//	     a window and resolution that exceed max_points, a session limit
//	     denominated in money.
//	429  Rate limited (written by the limiter middleware, not by this file).
//	500  This service failed. The body carries a request id and nothing else.
//
// The distinction between 400 and 422 is the one most often collapsed. It is
// kept because it tells a client whether to fix its SERIALISER or its LOGIC,
// and those are different bugs in different places.

// Error messages. Every one is a fixed string; none is derived from an error.
const (
	msgBadRequest      = "the request could not be understood"
	msgInvalidParam    = "one or more parameters are invalid"
	msgInvalidCursor   = "the cursor is not valid for this query"
	msgUnauthenticated = "authentication is required"
	msgBadCredentials  = "email or password is incorrect"
	msgTOTPRequired    = "a second factor is required"
	msgBadTOTP         = "the second-factor code is incorrect"
	msgAccountBlocked  = "this account cannot start a session"
	msgNotFound        = "no such resource"
	msgConflict        = "the request conflicts with the current state"
	msgAlreadyExists   = "that already exists"
	msgUnprocessable   = "the request is valid but cannot be satisfied"
	msgInternal        = "internal error"
)

// respond writes a successful JSON body.
//
// It marshals into a buffer BEFORE writing the status line. Encoding straight
// into the ResponseWriter commits 200 before the encoder can fail, so a
// marshalling error halfway through produces a 200 with a truncated body — which
// a client cannot distinguish from success, the worst available outcome.
func respond(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"code":"internal","message":"internal error","request_id":""}`,
			http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(status)
	// The status line is already written; a failed write means the client has
	// gone and net/http reports the connection error.
	_, _ = w.Write(buf)
}

// fail writes the canonical error envelope.
//
// It takes a CODE and a MESSAGE, both from the constant sets above, and never an
// error. That is the whole point: there is no argument here that could carry
// internal detail to a client.
func fail(w http.ResponseWriter, r *http.Request, status int, code gen.ErrorCode, message string) {
	respond(w, status, gen.Error{
		Code:      code,
		Message:   message,
		RequestId: middleware.RequestIDFrom(r.Context()),
	})
}

// failInvalid writes a 400 or 422 naming the offending parameters.
//
// The `reason` strings come from the call site and are fixed there for the same
// reason `message` is fixed here. A parameter NAME is echoed because the client
// supplied it and already knows it; a parameter VALUE is never echoed, so a
// reflected-value XSS in a client that renders the error unescaped is not
// reachable through this API.
func failInvalid(w http.ResponseWriter, r *http.Request, status int, code gen.ErrorCode, message string, params []gen.InvalidParam) {
	body := gen.Error{
		Code:      code,
		Message:   message,
		RequestId: middleware.RequestIDFrom(r.Context()),
	}
	if len(params) > 0 {
		body.InvalidParams = &params
	}
	respond(w, status, body)
}

// failWith logs an unexpected error and answers 500.
//
// THE ONLY FUNCTION IN THIS PACKAGE THAT SEES AN ERROR AND WRITES A RESPONSE,
// and it writes a constant. The error is logged with the request id and the
// route pattern so an operator joins the client's `request_id` to the cause;
// the client learns nothing else.
//
// A cancelled request is logged at debug rather than error: a browser navigating
// away mid-board-load is not an incident, and a 500 rate that counts them is a
// 500 rate nobody trusts.
func failWith(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	ctx := r.Context()
	attrs := []any{
		slog.String("op", op),
		slog.String("request_id", middleware.RequestIDFrom(ctx)),
		slog.String("route", middleware.RouteFrom(ctx)),
		slog.String("error", err.Error()),
	}

	switch {
	case errors.Is(err, context.Canceled):
		log.DebugContext(ctx, "request cancelled", attrs...)
		// The client is gone. 499 is not a real status; writing anything is
		// best-effort and net/http will discard it.
		fail(w, r, http.StatusInternalServerError, gen.ErrorCodeInternal, msgInternal)
	case errors.Is(err, context.DeadlineExceeded):
		log.WarnContext(ctx, "request deadline exceeded", attrs...)
		fail(w, r, http.StatusGatewayTimeout, gen.ErrorCodeInternal, msgInternal)
	default:
		log.ErrorContext(ctx, "request failed", attrs...)
		fail(w, r, http.StatusInternalServerError, gen.ErrorCodeInternal, msgInternal)
	}
}

// failNotFound answers 404.
func failNotFound(w http.ResponseWriter, r *http.Request) {
	fail(w, r, http.StatusNotFound, gen.ErrorCodeNotFound, msgNotFound)
}

// failAuth maps an internal/auth sentinel onto the wire.
//
// # This function is where the no-enumeration guarantee is kept or lost
//
// internal/auth returns auth.ErrCredentials for an unknown address AND for a
// wrong password, having done the same amount of work in both cases. This
// function must not undo that by reporting them differently, so there is exactly
// ONE arm for auth.ErrCredentials and no arm that could distinguish them — there
// is no "user not found" sentinel in the auth package to write an arm for.
//
// The same holds for refresh: auth.ErrTokenUnknown, auth.ErrTokenExpired,
// auth.ErrSessionRevoked and auth.ErrTokenReuse all become the SAME 401 with the
// SAME code and message. A thief replaying a stolen token must not learn from
// the response that they tripped the reuse detector, because that tells them the
// legitimate user is still active and that the family is now dead — which is
// exactly the signal that makes a targeted follow-up worth attempting.
//
// auth.ErrSecondFactorRequired is the one 401 that says something specific, and
// it is safe because it is only reachable AFTER the password has been verified:
// it discloses nothing to anyone who does not already hold the password.
//
// Anything not matched here is treated as an internal failure, which is the
// correct default: a sentinel this function has never heard of must not fall
// through to a 200 or to a status chosen by accident.
func failAuth(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	switch {
	case errors.Is(err, auth.ErrSecondFactorRequired):
		fail(w, r, http.StatusUnauthorized, gen.ErrorCodeTotpRequired, msgTOTPRequired)

	case errors.Is(err, auth.ErrSecondFactorInvalid):
		fail(w, r, http.StatusUnauthorized, gen.ErrorCodeInvalidTotpCode, msgBadTOTP)

	case errors.Is(err, auth.ErrCredentials),
		errors.Is(err, auth.ErrTokenUnknown),
		errors.Is(err, auth.ErrTokenExpired),
		errors.Is(err, auth.ErrTokenReuse),
		errors.Is(err, auth.ErrSessionRevoked),
		errors.Is(err, auth.ErrAccessTokenInvalid):
		fail(w, r, http.StatusUnauthorized, gen.ErrorCodeInvalidCredentials, msgBadCredentials)

	// A blocked account is 403 and not 401: the credential was correct, and
	// telling a self-excluded user "wrong password" would be both a lie and an
	// invitation to keep trying. The three statuses collapse into one code
	// because "why" is a conversation with support, not a field a client
	// branches on.
	case errors.Is(err, auth.ErrAccountSuspended),
		errors.Is(err, auth.ErrAccountClosed),
		errors.Is(err, auth.ErrSelfExcluded),
		errors.Is(err, auth.ErrForbidden):
		fail(w, r, http.StatusForbidden, gen.ErrorCodeAccountNotActive, msgAccountBlocked)

	case errors.Is(err, auth.ErrEmailTaken):
		fail(w, r, http.StatusConflict, gen.ErrorCodeAlreadyExists, msgAlreadyExists)

	case errors.Is(err, auth.ErrTOTPAlreadyEnrolled):
		fail(w, r, http.StatusConflict, gen.ErrorCodeConflict, msgConflict)

	case errors.Is(err, auth.ErrTOTPNotEnrolled):
		failNotFound(w, r)

	// A malformed email or a too-short password is the client's mistake and is
	// safe to report as such: the value came from the client and the constraint
	// is published in the spec. The specific violated rule is NOT named, because
	// naming it on the login path would distinguish "no such account" from
	// "wrong password" for an attacker who probes with a deliberately invalid
	// password.
	case errors.Is(err, auth.ErrInvalid):
		fail(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam)

	case errors.Is(err, auth.ErrConflict), errors.Is(err, domain.ErrConflict):
		fail(w, r, http.StatusConflict, gen.ErrorCodeConflict, msgConflict)

	default:
		failWith(w, r, log, op, err)
	}
}
