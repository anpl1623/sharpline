package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// ErrInvalidCredential is what an Authenticator returns when a presented token
// does not verify, for any reason.
//
// "For any reason" is the contract, and it is a security requirement rather than
// laziness. An Authenticator MUST NOT distinguish, in anything that reaches the
// client, between a token that is expired, one whose signature is wrong, one
// naming a user that does not exist and one whose session was revoked: each
// distinction is an oracle. The reason belongs in the error's wrapped chain,
// which is logged and never rendered.
var ErrInvalidCredential = errors.New("middleware: invalid credential")

// Authenticator verifies an access token and produces the identity it names.
//
// It is declared HERE, by the consumer, rather than by internal/auth (CLAUDE.md
// §12: "Interfaces are declared by the consumer, not the producer. Keep them
// small."). internal/auth owns token minting, argon2id, TOTP and the refresh
// family lifecycle; this package owns one question — "who is this request?" —
// and this is the whole of it.
//
// # Requirements on any implementation
//
// These are not suggestions. This middleware cannot enforce them from the
// outside, so they are stated here as the contract the implementation is
// reviewed against:
//
//   - Pin the signing algorithm. Take the expected alg from CONFIGURATION and
//     compare it to the token's; never take it from the token header. "alg":
//     "none" must be rejected, and so must a token presenting HS256 when the
//     verifier holds an RSA public key — that is the classic confusion attack
//     that turns a public key into a symmetric secret.
//   - Verify issuer, audience and expiry, and reject a token whose nbf is in
//     the future. An access token minted for the WebSocket gateway must not be
//     accepted by the REST API.
//   - Keep the access-token lifetime short. CLAUDE.md §6 pairs it with rotating
//     refresh tokens precisely so this one can be minutes.
//   - Compare every secret in constant time. hmac.Equal, never ==.
//   - Return ErrInvalidCredential (wrapping the real cause) for every failure.
//   - Never put the token, or any part of it, in the returned error.
type Authenticator interface {
	// Authenticate verifies token and returns the identity it names. The token
	// is the raw credential: it must not be logged, traced, or included in the
	// returned error.
	Authenticate(ctx context.Context, token string) (Identity, error)
}

// AuthenticatorFunc adapts a function to Authenticator, so internal/auth does
// not have to import this package to satisfy it — a cmd/ entrypoint can adapt
// its verifier in one line.
type AuthenticatorFunc func(ctx context.Context, token string) (Identity, error)

// Authenticate implements Authenticator.
func (f AuthenticatorFunc) Authenticate(ctx context.Context, token string) (Identity, error) {
	return f(ctx, token)
}

// AuthOptions configures the Authenticate middleware.
type AuthOptions struct {
	// Authenticator verifies tokens. Required.
	Authenticator Authenticator

	// Metrics records outcomes. Optional.
	Metrics *Metrics

	// ErrorWriter renders the 401. Optional; WriteProblem is the default.
	ErrorWriter ErrorWriter

	// CookieName, when non-empty, additionally accepts the access token from
	// that cookie.
	//
	// EMPTY BY DEFAULT, and that default is a decision. A credential the browser
	// attaches automatically is a credential an attacker's page can cause the
	// browser to attach automatically — that is what CSRF is. A bearer header
	// has to be set by script that can only run on the origin holding the
	// token, so it is not forgeable cross-site at all.
	//
	// Turning this on is legitimate (an httpOnly cookie is out of reach of XSS
	// in a way localStorage is not) but it is not free: it REQUIRES SameSite
	// and an Origin check on every state-changing request, and neither is
	// implemented here because neither belongs in a middleware that does not
	// mint the cookie. Whoever sets this owns that work.
	CookieName string
}

// Authenticate validates a presented access token and puts the Identity in the
// request context.
//
// # It fails closed
//
// The three cases are distinct and only one of them proceeds:
//
//	no credential presented        -> the request continues, ANONYMOUS. Handlers
//	                                  that need an identity are wrapped in
//	                                  RequireIdentity, which answers 401.
//	a credential that is malformed -> 401. An Authorization header this package
//	                                  cannot parse is a client that believes it
//	                                  is authenticated; treating it as anonymous
//	                                  would silently downgrade it.
//	a credential that is presented
//	  and does not verify          -> 401. Including when the failure is an
//	                                  INTERNAL error inside the verifier — a
//	                                  database timeout, a missing key. An error
//	                                  in validation is never a pass-through.
//
// That last clause is the one worth being explicit about: `if err != nil { next
// } ` is a one-character-looking mistake that converts every verifier outage
// into an authentication bypass. There is no branch here that calls next after
// a non-nil error.
//
// # It cannot distinguish itself
//
// Every 401 this middleware writes is byte-identical: the same status, the same
// code, the same message, no headers that vary with the cause. The reason goes
// to the log and to sharpline_http_auth_total, where the operator can see it.
func Authenticate(opts AuthOptions) (Middleware, error) {
	if opts.Authenticator == nil {
		return nil, invalidOption("Authenticator is nil")
	}
	auth := opts.Authenticator
	m := opts.Metrics
	ew := opts.ErrorWriter
	cookie := opts.CookieName

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, state := extractToken(r, cookie)

			switch state {
			case credentialAbsent:
				countAuth(m, authAbsent)
				next.ServeHTTP(w, r)
				return
			case credentialMalformed:
				countAuth(m, authMalformed)
				LoggerFrom(r.Context()).LogAttrs(r.Context(), slog.LevelWarn,
					"rejecting malformed credential",
					slog.String("route", routeLabel(RouteFrom(r.Context()))),
				)
				writeProblemWith(ew, w, r, problemUnauthorized)
				return
			}

			id, err := auth.Authenticate(r.Context(), token)
			if err != nil {
				countAuth(m, authRejected)
				// err may name the internal cause (expired, bad signature,
				// unknown key id). It goes here and nowhere else. It must not
				// contain the token; that is a documented requirement on the
				// Authenticator, restated at the interface.
				LoggerFrom(r.Context()).LogAttrs(r.Context(), slog.LevelWarn,
					"rejecting access token",
					slog.String("route", routeLabel(RouteFrom(r.Context()))),
					slog.String("error", err.Error()),
				)
				writeProblemWith(ew, w, r, problemUnauthorized)
				return
			}
			if id.IsZero() {
				// A verifier that returns no error and no user is a bug in the
				// verifier. Failing closed means treating it as a rejection
				// rather than as an anonymous pass-through.
				countAuth(m, authRejected)
				LoggerFrom(r.Context()).LogAttrs(r.Context(), slog.LevelError,
					"authenticator returned an empty identity with no error",
					slog.String("route", routeLabel(RouteFrom(r.Context()))),
				)
				writeProblemWith(ew, w, r, problemUnauthorized)
				return
			}

			countAuth(m, authOK)

			ctx := withIdentity(r.Context(), id)
			// Re-derive the request logger so every subsequent line, including
			// the access-log line, carries the user id.
			ctx = withLogger(ctx, LoggerFrom(ctx).With(slog.String("user_id", id.UserID.String())))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}, nil
}

// RequireIdentity rejects a request that reached it without an authenticated
// identity.
//
// It is applied per route group rather than chain-wide because the catalogue
// read surface (CLAUDE.md §6, Core: the odds board, event detail, line history,
// book comparison) is public, while the account and wagering surface is not. A
// chain-wide requirement would make the landing page impossible.
//
// It answers the same indistinguishable 401 as Authenticate.
func RequireIdentity(m *Metrics, ew ErrorWriter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := IdentityFrom(r.Context()); !ok {
				countAuth(m, authRequired)
				writeProblemWith(ew, w, r, problemUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// credentialState distinguishes the three cases Authenticate branches on.
type credentialState int

const (
	credentialAbsent credentialState = iota
	credentialPresent
	credentialMalformed
)

// bearerPrefixLen is len("Bearer ").
const bearerPrefixLen = 7

// extractToken pulls the raw credential out of the request.
//
// The scheme match is case-insensitive because RFC 7235 says the scheme token is
// case-insensitive and real clients send "bearer". Everything after the single
// space is the token, trimmed — a second space or a comma-separated credential
// list is not a bearer token this API issued, and is reported as malformed
// rather than being partially parsed.
func extractToken(r *http.Request, cookieName string) (string, credentialState) {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) <= bearerPrefixLen || !strings.EqualFold(h[:bearerPrefixLen], "bearer ") {
			return "", credentialMalformed
		}
		token := strings.TrimSpace(h[bearerPrefixLen:])
		if token == "" || strings.ContainsAny(token, " \t,") {
			return "", credentialMalformed
		}
		return token, credentialPresent
	}

	if cookieName != "" {
		if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
			return c.Value, credentialPresent
		}
	}

	return "", credentialAbsent
}

func countAuth(m *Metrics, outcome string) {
	if m != nil {
		m.auth.WithLabelValues(outcome).Inc()
	}
}
