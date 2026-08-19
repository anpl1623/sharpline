package middleware

import (
	"encoding/json"
	"net/http"
)

// Error codes. A closed, stable set: these are part of the API contract that
// the frontend branches on, so they are slugs rather than prose and they do not
// change when the message does.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthorized    = "unauthorized"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodePayloadTooLarge = "payload_too_large"
	CodeRateLimited     = "rate_limited"
	CodeTimeout         = "timeout"
	CodeInternal        = "internal_error"
)

// Problem is an error this package returns to the client.
//
// Message is written for the client and is deliberately generic. It never
// contains an internal error string, a stack trace, a SQL state, a hostname or
// anything else that describes the inside of the system: those go to the log,
// correlated by request id, where an operator can see them and an attacker
// cannot.
type Problem struct {
	Status  int
	Code    string
	Message string
}

// The problems this package emits. Each message is deliberately incurious:
// "unauthorized" says nothing about whether the account exists, whether the
// token expired, or whether the signature failed, because all three must be
// indistinguishable to the caller (see auth.go).
var (
	problemUnauthorized = Problem{Status: http.StatusUnauthorized, Code: CodeUnauthorized, Message: "Authentication is required."}
	problemRateLimited  = Problem{Status: http.StatusTooManyRequests, Code: CodeRateLimited, Message: "Too many requests."}
	problemTooLarge     = Problem{Status: http.StatusRequestEntityTooLarge, Code: CodePayloadTooLarge, Message: "Request body is too large."}
	problemInternal     = Problem{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "Something went wrong."}
)

// Forbidden is the 403 this package does not itself emit.
//
// No middleware here answers 403: this chain decides WHO you are and how often
// you may ask, never WHAT you may reach — that is authorisation, and it lives
// with the handler that knows what the resource is. It is exported because the
// distinction only holds if a handler has a ready-made 403 in the same envelope
// as every other error the chain produces; without one, handlers invent their
// own shape and the API's error contract stops being one contract.
//
//	if !ownsWager(id, wagerID) {
//	    middleware.WriteProblem(w, r, middleware.Forbidden)
//	    return
//	}
var Forbidden = Problem{Status: http.StatusForbidden, Code: CodeForbidden, Message: "Not permitted."}

// ErrorWriter renders a Problem as an HTTP response.
//
// It is injectable so this package does not dictate the API's error envelope.
// internal/httpapi owns the public schema and its OpenAPI document; when it
// defines an envelope, it passes its own writer here and every error this chain
// produces matches every error a handler produces. Until then WriteProblem is
// the default and defines the shape.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, p Problem)

// errorBody is the default envelope.
//
// The request id is echoed on purpose: it is the one piece of internal state
// that is safe to give the client, and it turns "it broke" into a support
// interaction that can be resolved from the logs.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteProblem is the default ErrorWriter.
//
// It sets Cache-Control: no-store because an error response is specific to one
// caller and one moment; a shared cache holding a 429 or a 401 would serve it to
// somebody else.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	var body errorBody
	body.Error.Code = p.Code
	body.Error.Message = p.Message
	body.RequestID = RequestIDFrom(r.Context())

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(p.Status)

	// A write failure here means the client has already gone. There is nothing
	// to report to and nothing to recover; the access-log line still records
	// the status.
	_ = json.NewEncoder(w).Encode(body)
}

// writeProblemWith is what every middleware in this package calls, so an
// injected ErrorWriter is honoured everywhere and a nil one cannot panic.
func writeProblemWith(ew ErrorWriter, w http.ResponseWriter, r *http.Request, p Problem) {
	if ew == nil {
		ew = WriteProblem
	}
	ew(w, r, p)
}
