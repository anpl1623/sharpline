// Package authsessions adapts [auth.Service] to [httpapi.Sessions].
//
// # Why this package exists at all
//
// internal/auth owns the security behaviour — argon2id, JWT issuance, refresh
// rotation with reuse detection, TOTP. internal/httpapi owns the HTTP contract
// and declares, as CLAUDE.md §12 requires, the narrow interface IT needs rather
// than importing the service's whole method set. The two were built against
// that seam and neither one closes it.
//
// Without this adapter cmd/api gets a nil Sessions, httpapi.NewAPI refuses to
// build, and the composition root mounts ZERO routes — including the catalogue
// and board routes, which need no session at all. That is not a hypothetical:
// it is what the api container did, logging
// `"api routes mounted","routes":0,"patterns":null` while every probe stayed
// green and `make check` stayed green, because cmd/api has no tests and every
// httpapi test supplies its own fake.
//
// # What this package is allowed to do
//
// Translate types and pass errors through UNCHANGED. Nothing more.
//
// The error rule is load-bearing rather than stylistic. internal/httpapi's
// respond.go maps auth's sentinels onto statuses and deliberately collapses
// auth.ErrCredentials, auth.ErrTokenUnknown, auth.ErrTokenExpired,
// auth.ErrSessionRevoked and auth.ErrTokenReuse into ONE indistinguishable 401.
// That collapse is what stops the API being an account-enumeration oracle and a
// reuse-detection oracle. An adapter that wrapped an error in a way errors.Is
// could not see through, or that helpfully distinguished two of them, would
// reopen exactly the hole the two packages each took care to close. So every
// error here is returned verbatim.
//
// # No secret crosses this boundary in a loggable form
//
// auth.Secret refuses to marshal and redacts in every string and log form, so
// the values that pass through here cannot be logged by accident. This package
// unwraps them at exactly two points — where httpapi's string-typed contract
// requires it — and never holds one in a struct field, a log attribute or an
// error.
package authsessions

import (
	"context"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi"
)

// Service is the subset of *auth.Service this adapter drives.
//
// Declared here, by the consumer, for the same reason httpapi declares
// httpapi.Sessions: it makes the dependency exactly as wide as the use, and it
// lets this package be tested without standing up argon2id, a keyring and a
// database.
type Service interface {
	Register(ctx context.Context, email string, password auth.Secret) (auth.UserRecord, error)
	Login(ctx context.Context, req auth.LoginRequest) (auth.Session, error)
	Refresh(ctx context.Context, presented auth.Secret) (auth.Session, error)
	Logout(ctx context.Context, presented auth.Secret) error

	BeginTOTPEnrolment(ctx context.Context, id domain.UserID) (auth.TOTPEnrolment, error)
	ConfirmTOTPEnrolment(ctx context.Context, id domain.UserID, code string) ([]auth.Secret, error)
	DisableTOTPWithCode(ctx context.Context, id domain.UserID, code string) error
	TOTPStatus(ctx context.Context, id domain.UserID) (auth.TOTPStatus, error)

	AccessTokenTTL() time.Duration
}

// Adapter implements [httpapi.Sessions] over a [Service].
type Adapter struct {
	svc Service
	now func() time.Time
}

// Options configures [New].
type Options struct {
	// Service is the auth service. Required.
	Service Service

	// Now is the clock used to express the access token's remaining lifetime.
	// Zero means time.Now.
	Now func() time.Time
}

// New builds an Adapter.
func New(opts Options) (*Adapter, error) {
	if opts.Service == nil {
		return nil, fmt.Errorf("authsessions: an auth service is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{svc: opts.Service, now: now}, nil
}

// compile-time assertion that this package actually closes the seam it exists
// to close. Without it the failure mode is a nil Sessions at run time, which is
// how the surface came to be unmounted in the first place.
var _ httpapi.Sessions = (*Adapter)(nil)

// Register creates an account and opens its first session.
//
// # Why this logs in rather than minting a session directly
//
// auth.Service.Register returns a UserRecord, not a Session — opening one is
// Service.openSession, which is unexported and deliberately so: it is reachable
// only from a path that has just VERIFIED a credential. This adapter is not
// such a path and must not be given a shortcut around one.
//
// So it registers and then authenticates, which costs a second argon2id
// verification on a request that already paid for a hash. That is the correct
// trade: the alternative is exporting a "give me a session for this user id"
// entry point, and a function that mints credentials without checking any is a
// far more dangerous thing to have in the tree than one slow registration.
//
// The password is passed straight through; no minimum or maximum is applied
// here, because auth.Service owns that policy and a second copy of it in an
// adapter is a second thing to forget to update.
func (a *Adapter) Register(
	ctx context.Context, email, password string, _ httpapi.AuditContext,
) (httpapi.Session, error) {
	secret := auth.NewSecret(password)

	if _, err := a.svc.Register(ctx, email, secret); err != nil {
		return httpapi.Session{}, err
	}

	sess, err := a.svc.Login(ctx, auth.LoginRequest{Email: email, Password: secret})
	if err != nil {
		return httpapi.Session{}, err
	}

	// A just-registered account cannot have a second factor, so the profile is
	// assembled without asking the store. Skipping the query is not an
	// optimisation for its own sake: TOTPStatus on a user with no enrolment is
	// a round trip whose only possible answer is the zero value.
	return a.session(sess, httpapi.Profile{
		ID:        sess.User.ID,
		Email:     sess.User.Email.String(),
		Status:    sess.User.Status,
		CreatedAt: sess.User.CreatedAt,
	}), nil
}

// Authenticate verifies a password and optional second factor.
//
// totpCode is forwarded as auth.LoginRequest.TOTPCode. It is NOT inspected
// here — not for length, not for emptiness. auth.Service treats an empty code
// for a user with no enrolment as fine and an empty code for a user WITH one as
// ErrSecondFactorRequired, and that distinction is the service's to make.
func (a *Adapter) Authenticate(
	ctx context.Context, email, password, totpCode string, _ httpapi.AuditContext,
) (httpapi.Session, error) {
	sess, err := a.svc.Login(ctx, auth.LoginRequest{
		Email:    email,
		Password: auth.NewSecret(password),
		TOTPCode: totpCode,
	})
	if err != nil {
		return httpapi.Session{}, err
	}
	return a.withProfile(ctx, sess)
}

// Redeem rotates a refresh token.
//
// Reuse detection lives entirely below this call. Presenting an
// already-redeemed token makes auth.Service revoke the WHOLE FAMILY and return
// auth.ErrTokenReuse, which respond.go renders as the same 401 an unknown token
// gets. Nothing here may soften that: telling a caller "that token was already
// used" confirms it was once real, and telling it "the family was revoked"
// confirms the detection fired.
func (a *Adapter) Redeem(
	ctx context.Context, refreshToken string, _ httpapi.AuditContext,
) (httpapi.Session, error) {
	sess, err := a.svc.Refresh(ctx, auth.NewSecret(refreshToken))
	if err != nil {
		return httpapi.Session{}, err
	}
	return a.withProfile(ctx, sess)
}

// Revoke ends the presented token's family.
//
// Idempotent, and an unknown token is not an error — auth.Service.Logout
// already has that contract, for the reason httpapi.Sessions states: telling a
// caller its token was unknown distinguishes "already dead" from "never
// existed", which is an oracle no legitimate caller can act on.
func (a *Adapter) Revoke(ctx context.Context, refreshToken string, _ httpapi.AuditContext) error {
	return a.svc.Logout(ctx, auth.NewSecret(refreshToken))
}

// BeginTOTP starts an enrolment and returns the provisioning URI once.
//
// The URI EMBEDS the shared secret. It is unwrapped from auth.Secret here
// because httpapi.Enrolment types it as a string, and that is the last point at
// which it is a value rather than a redacted one — so it goes straight into the
// return and is never assigned to anything else, logged, or wrapped in an
// error.
//
// ExpiresAt is left ZERO, honestly. auth.TOTPEnrolment carries no expiry
// because the enrolment does not have one: the unconfirmed row lives until it
// is confirmed or removed. Inventing a deadline here would put a number on the
// wire that nothing enforces, and a client that trusted it would abandon a
// perfectly valid enrolment.
func (a *Adapter) BeginTOTP(
	ctx context.Context, user domain.UserID, _ httpapi.AuditContext,
) (httpapi.Enrolment, error) {
	enrolment, err := a.svc.BeginTOTPEnrolment(ctx, user)
	if err != nil {
		return httpapi.Enrolment{}, err
	}
	return httpapi.Enrolment{ProvisioningURI: enrolment.URI.Expose()}, nil
}

// ConfirmTOTP proves a code and activates the factor.
//
// auth.Service.ConfirmTOTPEnrolment also returns the freshly minted recovery
// codes. They are DISCARDED here, because httpapi.Sessions.ConfirmTOTP returns
// only an error and openapi.yaml's 200 response for this operation is an
// Account, with no field for them.
//
// That is a real gap and it is recorded here rather than papered over: a user
// who confirms an enrolment through this API is never shown recovery codes, so
// a lost authenticator means a lost account. Closing it is a contract change —
// a new schema on the confirm response — not something an adapter may do by
// inventing a field. Until then, discarding is strictly better than logging
// them, which is the only other place they could go.
func (a *Adapter) ConfirmTOTP(
	ctx context.Context, user domain.UserID, code string, _ httpapi.AuditContext,
) error {
	_, err := a.svc.ConfirmTOTPEnrolment(ctx, user, code)
	return err
}

// RemoveTOTP strips the second factor on proof of a live code from it.
//
// It routes to DisableTOTPWithCode rather than DisableTOTP because openapi.yaml
// is the contract of record and it requires possession of the factor, not the
// account password: "so that a stolen access token alone cannot strip the
// second factor". See auth.Service.DisableTOTPWithCode for why possession is
// the stronger of the two step-ups for this particular action.
func (a *Adapter) RemoveTOTP(
	ctx context.Context, user domain.UserID, code string, _ httpapi.AuditContext,
) error {
	return a.svc.DisableTOTPWithCode(ctx, user, code)
}

// withProfile completes a session with the caller's second-factor state.
//
// A failure to read that state fails the whole call. Returning a session with
// TOTPConfirmed silently false would tell an account page that a user has no
// second factor when they do, which is the one wrong answer here that a user
// might act on.
func (a *Adapter) withProfile(ctx context.Context, sess auth.Session) (httpapi.Session, error) {
	status, err := a.svc.TOTPStatus(ctx, sess.User.ID)
	if err != nil {
		return httpapi.Session{}, err
	}
	return a.session(sess, httpapi.Profile{
		ID:            sess.User.ID,
		Email:         sess.User.Email.String(),
		Status:        sess.User.Status,
		CreatedAt:     sess.User.CreatedAt,
		TOTPConfirmed: status.Enrolled,
		TOTPPending:   status.Pending,
	}), nil
}

// session converts an auth.Session into the wire-facing shape.
//
// AccessExpiresIn is computed from the token's OWN `exp` claim rather than from
// Service.AccessTokenTTL, so the number a client paces its refresh against is
// the instant the verifier will actually start rejecting the token. The two are
// normally the same; where they differ, the claim is the one that is true.
//
// It is floored at zero. A negative "expires in" is not a thing a client can
// represent, and openapi.yaml types the field as a non-negative integer of
// seconds.
func (a *Adapter) session(sess auth.Session, profile httpapi.Profile) httpapi.Session {
	expiresIn := sess.AccessClaims.ExpiresAt.Sub(a.now())
	if expiresIn < 0 {
		expiresIn = 0
	}
	return httpapi.Session{
		AccessToken:      sess.AccessToken.Expose(),
		AccessExpiresIn:  expiresIn,
		RefreshToken:     sess.RefreshToken.Expose(),
		RefreshExpiresAt: sess.RefreshExpiresAt,
		Profile:          profile,
	}
}
