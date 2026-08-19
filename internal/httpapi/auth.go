package httpapi

import (
	"net/http"

	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// Registration, login, refresh-token rotation, logout, and TOTP enrolment.
//
// # What this file does NOT do
//
// It does no cryptography. It does not hash a password, does not compare one,
// does not mint or verify a JWT, does not derive or check a TOTP code, and does
// not decide when a refresh-token family is compromised. All of that is
// internal/auth's, reached through the [Sessions] port. This file is the HTTP
// around it: decode, delegate, map the sentinel onto a status, and serialise.
//
// That split is the security argument, not an aesthetic one. Every property this
// phase has to get right — constant-time comparison, identical work on the
// unknown-user path, algorithm pinning, one-shot token redemption — is a
// property of an implementation, and having exactly one implementation is what
// makes it checkable.
//
// # What this file MUST get right, because nothing downstream can fix it
//
//  1. NO SECRET IS EVER LOGGED, TRACED, SPAN-ATTRIBUTED OR ECHOED. There is no
//     call in this file that puts a request body, a password, a token or a TOTP
//     code into a logger, an error, an audit entry or a metric label. The audit
//     entries below carry the user id, the outcome and the request id, and
//     [AuditContext] has no field a secret could be placed in.
//
//  2. THE ERROR MAPPING PRESERVES INDISTINGUISHABILITY. See [failAuth]: unknown
//     address and wrong password are one arm; unknown, expired, revoked and
//     REUSED refresh tokens are one arm. A thief must not learn from a status
//     code that they tripped the reuse detector.
//
//  3. THE REFRESH TOKEN IS RETURNED IN THE BODY AND NOT AS A COOKIE. This API is
//     consumed cross-origin through a proxy by a Next.js client and by
//     pkg/client; a cookie-bearing API is a CSRF surface that would then need a
//     second mechanism to defend. The client stores the refresh token and holds
//     the access token in memory.

func (a *API) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body gen.RegisterRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	ac := a.auditContext(r)
	session, err := a.sessions.Register(r.Context(), string(body.Email), body.Password, ac)
	if err != nil {
		// Register DOES distinguish a duplicate address (409), and that is a
		// deliberate, bounded trade rather than an oversight: a registration
		// form that accepts a duplicate silently is unusable, and the same fact
		// is obtainable from any password-reset flow that exists. /auth/login
		// leaks nothing, which is where it matters.
		failAuth(w, r, a.log, "register", err)
		return
	}
	respond(w, http.StatusCreated, wireSession(session))
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body gen.LoginRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	code := ""
	if body.TotpCode != nil {
		code = *body.TotpCode
	}

	ac := a.auditContext(r)
	session, err := a.sessions.Authenticate(r.Context(), string(body.Email), body.Password, code, ac)
	if err != nil {
		failAuth(w, r, a.log, "login", err)
		return
	}
	respond(w, http.StatusOK, wireSession(session))
}

// handleRefresh rotates a refresh token.
//
// Redeeming invalidates the presented token and issues its successor in ONE
// transaction, so a family has exactly one live token at any instant. Presenting
// an already-redeemed token means either a thief is replaying a stolen token or
// the legitimate client is replaying one whose successor was stolen — the two
// are indistinguishable, so the whole family is revoked and both parties must
// authenticate again.
//
// That detection is structural rather than a read-then-write: the partial unique
// index `refresh_tokens_one_successor` permits at most one child per token, so
// two concurrent redemptions of the same token cannot both mint a successor
// whatever the application believed it read.
func (a *API) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var body gen.RefreshRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	ac := a.auditContext(r)
	session, err := a.sessions.Redeem(r.Context(), body.RefreshToken, ac)
	if err != nil {
		failAuth(w, r, a.log, "refresh", err)
		return
	}
	respond(w, http.StatusOK, wireSession(session))
}

// handleLogout revokes the presented token's whole family.
//
// Idempotent, and it answers 204 for an unknown or already-revoked token.
// Reporting "that token was already dead" would be an enumeration oracle and
// there is nothing a caller could do with the distinction — the session is over
// either way.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	var body gen.RefreshRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	if err := a.sessions.Revoke(r.Context(), body.RefreshToken, a.auditContext(r)); err != nil {
		// A genuine infrastructure failure still has to be reported: answering
		// 204 when the revocation did not happen would tell a user their
		// session is over while it is still live, which is the one wrong answer
		// that matters here.
		failWith(w, r, a.log, "logout", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBeginTOTP starts an enrolment and returns the provisioning URI once.
//
// THE URI EMBEDS THE SHARED SECRET. It is written to the response body and to
// nothing else — not a log line, not a span attribute, not the audit entry
// below, which records only that an enrolment began.
//
// The enrolment is NOT a second factor until /account/totp/confirm proves a code
// from it. An unconfirmed row must never be treated as enrolled: a user whose QR
// scan failed would otherwise be locked out of their own account.
func (a *API) handleBeginTOTP(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	ac := a.auditContext(r)
	enrolment, err := a.sessions.BeginTOTP(r.Context(), user, ac)
	if err != nil {
		failAuth(w, r, a.log, "totp enrol", err)
		return
	}

	a.record(r.Context(), AuditEntry{
		Context:    ac,
		ActorKind:  "user",
		ActorID:    user.String(),
		Action:     "totp.enrol_begin",
		EntityType: "user_totp",
		EntityID:   user.String(),
		Outcome:    "success",
	})

	respond(w, http.StatusCreated, gen.TOTPEnrolment{
		ProvisioningUri: enrolment.ProvisioningURI,
		ExpiresAt:       enrolment.ExpiresAt.UTC(),
	})
}

func (a *API) handleConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	var body gen.TOTPCodeRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	ac := a.auditContext(r)
	if err := a.sessions.ConfirmTOTP(r.Context(), user, body.Code, ac); err != nil {
		// The failure is audited too: a run of failed second-factor attempts is
		// exactly the signal the audit log exists to surface. The CODE is not
		// recorded — only that a confirmation was attempted and refused.
		a.record(r.Context(), AuditEntry{
			Context:    ac,
			ActorKind:  "user",
			ActorID:    user.String(),
			Action:     "totp.enrol_confirm",
			EntityType: "user_totp",
			EntityID:   user.String(),
			Outcome:    "failure",
		})
		failAuth(w, r, a.log, "totp confirm", err)
		return
	}

	a.record(r.Context(), AuditEntry{
		Context:    ac,
		ActorKind:  "user",
		ActorID:    user.String(),
		Action:     "totp.enrol_confirm",
		EntityType: "user_totp",
		EntityID:   user.String(),
		Outcome:    "success",
	})

	profile, err := a.accounts.Profile(r.Context(), user)
	if err != nil {
		failWith(w, r, a.log, "totp confirm: profile", err)
		return
	}
	respond(w, http.StatusOK, wireProfile(profile))
}

// handleRemoveTOTP removes the second factor, and REQUIRES a valid code from
// the factor being removed.
//
// Without that requirement a stolen access token alone would be enough to strip
// the second factor, which would make the factor worth nothing against exactly
// the attack it exists to stop.
func (a *API) handleRemoveTOTP(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	var body gen.TOTPCodeRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	ac := a.auditContext(r)
	if err := a.sessions.RemoveTOTP(r.Context(), user, body.Code, ac); err != nil {
		a.record(r.Context(), AuditEntry{
			Context:    ac,
			ActorKind:  "user",
			ActorID:    user.String(),
			Action:     "totp.remove",
			EntityType: "user_totp",
			EntityID:   user.String(),
			Outcome:    "failure",
		})
		failAuth(w, r, a.log, "totp remove", err)
		return
	}

	a.record(r.Context(), AuditEntry{
		Context:    ac,
		ActorKind:  "user",
		ActorID:    user.String(),
		Action:     "totp.remove",
		EntityType: "user_totp",
		EntityID:   user.String(),
		Outcome:    "success",
	})
	w.WriteHeader(http.StatusNoContent)
}

// wireSession serialises a session response.
//
// Both tokens travel here and nowhere else in the process: they are not logged,
// not traced, not audited and not cached. `expires_in` is seconds so a client
// can schedule a refresh without parsing a timestamp, and `refresh_expires_at`
// is absolute because it is measured from when the FAMILY started, not from this
// token's issue — without that, a session rotated every ten minutes would live
// forever.
func wireSession(s Session) gen.SessionResponse {
	return gen.SessionResponse{
		AccessToken:      s.AccessToken,
		TokenType:        gen.Bearer,
		ExpiresIn:        int32(s.AccessExpiresIn.Seconds()),
		RefreshToken:     s.RefreshToken,
		RefreshExpiresAt: s.RefreshExpiresAt.UTC(),
		Account:          wireProfile(s.Profile),
	}
}
