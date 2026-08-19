package authsessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi"
)

// fakeService records what it was called with and returns what it is told to.
//
// It is a fake rather than a mock: the assertions are on the VALUES that
// crossed the seam, because that is the whole job of the thing under test.
type fakeService struct {
	registerEmail string
	registerPass  string
	loginReq      auth.LoginRequest
	refreshTok    string
	logoutTok     string
	totpUser      domain.UserID
	totpCode      string
	removeCode    string

	registerErr error
	loginErr    error
	refreshErr  error
	logoutErr   error
	statusErr   error
	beginErr    error
	confirmErr  error
	removeErr   error

	session auth.Session
	status  auth.TOTPStatus
	enrol   auth.TOTPEnrolment

	loginCalls int
}

func (f *fakeService) Register(_ context.Context, email string, pw auth.Secret) (auth.UserRecord, error) {
	f.registerEmail, f.registerPass = email, pw.Expose()
	return auth.UserRecord{}, f.registerErr
}

func (f *fakeService) Login(_ context.Context, req auth.LoginRequest) (auth.Session, error) {
	f.loginReq = req
	f.loginCalls++
	return f.session, f.loginErr
}

func (f *fakeService) Refresh(_ context.Context, p auth.Secret) (auth.Session, error) {
	f.refreshTok = p.Expose()
	return f.session, f.refreshErr
}

func (f *fakeService) Logout(_ context.Context, p auth.Secret) error {
	f.logoutTok = p.Expose()
	return f.logoutErr
}

func (f *fakeService) BeginTOTPEnrolment(_ context.Context, id domain.UserID) (auth.TOTPEnrolment, error) {
	f.totpUser = id
	return f.enrol, f.beginErr
}

func (f *fakeService) ConfirmTOTPEnrolment(_ context.Context, id domain.UserID, code string) ([]auth.Secret, error) {
	f.totpUser, f.totpCode = id, code
	return []auth.Secret{auth.NewSecret("recovery-code")}, f.confirmErr
}

func (f *fakeService) DisableTOTPWithCode(_ context.Context, id domain.UserID, code string) error {
	f.totpUser, f.removeCode = id, code
	return f.removeErr
}

func (f *fakeService) TOTPStatus(_ context.Context, _ domain.UserID) (auth.TOTPStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeService) AccessTokenTTL() time.Duration { return 10 * time.Minute }

// fixedNow pins the clock so AccessExpiresIn is exact rather than approximate.
var fixedNow = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func newTestAdapter(t *testing.T, svc Service) *Adapter {
	t.Helper()
	a, err := New(Options{Service: svc, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func sampleSession() auth.Session {
	return auth.Session{
		User: auth.UserRecord{
			ID:        domain.UserID("usr_test"),
			Status:    auth.UserStatusActive,
			CreatedAt: fixedNow.Add(-24 * time.Hour),
		},
		AccessToken:      auth.NewSecret("access.jwt.value"),
		AccessClaims:     auth.Claims{ExpiresAt: fixedNow.Add(9 * time.Minute)},
		RefreshToken:     auth.NewSecret("refresh-opaque-value"),
		RefreshExpiresAt: fixedNow.Add(720 * time.Hour),
		SessionID:        "fam_test",
	}
}

func TestNewRequiresAService(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no service: want an error, got nil")
	}
}

// TestAuthenticateForwardsEveryCredentialField is the core translation test:
// the three client-supplied values must arrive at the service unchanged. A
// dropped TOTP code here would silently disable the second factor for every
// caller while every other test still passed.
func TestAuthenticateForwardsEveryCredentialField(t *testing.T) {
	svc := &fakeService{session: sampleSession(), status: auth.TOTPStatus{Enrolled: true}}
	a := newTestAdapter(t, svc)

	got, err := a.Authenticate(context.Background(), "user@example.test", "pw-value", "123456", httpapi.AuditContext{})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if svc.loginReq.Email != "user@example.test" {
		t.Errorf("email: got %q, want %q", svc.loginReq.Email, "user@example.test")
	}
	if svc.loginReq.Password.Expose() != "pw-value" {
		t.Errorf("password did not reach the service unchanged")
	}
	if svc.loginReq.TOTPCode != "123456" {
		t.Errorf("totp code: got %q, want %q", svc.loginReq.TOTPCode, "123456")
	}
	if got.AccessToken != "access.jwt.value" || got.RefreshToken != "refresh-opaque-value" {
		t.Errorf("tokens were not unwrapped onto the wire shape: %+v", got)
	}
	if !got.Profile.TOTPConfirmed {
		t.Error("TOTPConfirmed: got false, want true — the status query result was dropped")
	}
}

// TestAccessExpiresInComesFromTheClaim guards the choice documented on
// session(): the number a client paces its refresh against must be the token's
// own exp, not a nominal TTL that may disagree with it.
func TestAccessExpiresInComesFromTheClaim(t *testing.T) {
	svc := &fakeService{session: sampleSession()}
	a := newTestAdapter(t, svc)

	got, err := a.Authenticate(context.Background(), "e", "p", "", httpapi.AuditContext{})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	// The claim says 9 minutes even though AccessTokenTTL() reports 10.
	if got.AccessExpiresIn != 9*time.Minute {
		t.Errorf("AccessExpiresIn: got %s, want 9m (the claim, not the nominal TTL)", got.AccessExpiresIn)
	}
}

// TestAccessExpiresInFlooredAtZero: an already-expired claim must not produce a
// negative duration, which openapi.yaml cannot represent.
func TestAccessExpiresInFlooredAtZero(t *testing.T) {
	s := sampleSession()
	s.AccessClaims.ExpiresAt = fixedNow.Add(-time.Hour)
	svc := &fakeService{session: s}
	a := newTestAdapter(t, svc)

	got, err := a.Authenticate(context.Background(), "e", "p", "", httpapi.AuditContext{})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.AccessExpiresIn != 0 {
		t.Errorf("AccessExpiresIn: got %s, want 0", got.AccessExpiresIn)
	}
}

// TestErrorsPassThroughUnwrapped is the most important test in this file.
//
// internal/httpapi/respond.go maps auth's sentinels onto statuses with
// errors.Is, and it deliberately collapses several of them into ONE
// indistinguishable 401. If this adapter wrapped an error in a way errors.Is
// could not see through, every one of those would fall through to a 500 —
// turning "your token was replayed" into a distinguishable server error, which
// is precisely the oracle the collapse exists to close.
func TestErrorsPassThroughUnwrapped(t *testing.T) {
	sentinels := []error{
		auth.ErrCredentials,
		auth.ErrTokenUnknown,
		auth.ErrTokenExpired,
		auth.ErrSessionRevoked,
		auth.ErrTokenReuse,
		auth.ErrSecondFactorRequired,
		auth.ErrSecondFactorInvalid,
	}

	for _, want := range sentinels {
		t.Run(want.Error(), func(t *testing.T) {
			ctx := context.Background()
			ac := httpapi.AuditContext{}

			svc := &fakeService{loginErr: want}
			if _, err := newTestAdapter(t, svc).Authenticate(ctx, "e", "p", "", ac); !errors.Is(err, want) {
				t.Errorf("Authenticate: errors.Is(%v, %v) = false", err, want)
			}

			svc = &fakeService{refreshErr: want}
			if _, err := newTestAdapter(t, svc).Redeem(ctx, "tok", ac); !errors.Is(err, want) {
				t.Errorf("Redeem: errors.Is(%v, %v) = false", err, want)
			}

			svc = &fakeService{logoutErr: want}
			if err := newTestAdapter(t, svc).Revoke(ctx, "tok", ac); !errors.Is(err, want) {
				t.Errorf("Revoke: errors.Is(%v, %v) = false", err, want)
			}

			svc = &fakeService{registerErr: want}
			if _, err := newTestAdapter(t, svc).Register(ctx, "e", "p", ac); !errors.Is(err, want) {
				t.Errorf("Register: errors.Is(%v, %v) = false", err, want)
			}

			svc = &fakeService{removeErr: want}
			if err := newTestAdapter(t, svc).RemoveTOTP(ctx, domain.UserID("u"), "1", ac); !errors.Is(err, want) {
				t.Errorf("RemoveTOTP: errors.Is(%v, %v) = false", err, want)
			}
		})
	}
}

// TestRegisterOpensASessionByAuthenticating pins the documented decision that
// Register does NOT get a back door around credential verification.
func TestRegisterOpensASessionByAuthenticating(t *testing.T) {
	svc := &fakeService{session: sampleSession()}
	a := newTestAdapter(t, svc)

	got, err := a.Register(context.Background(), "new@example.test", "pw-value", httpapi.AuditContext{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if svc.registerEmail != "new@example.test" || svc.registerPass != "pw-value" {
		t.Errorf("registration args did not arrive unchanged: %q / %q", svc.registerEmail, svc.registerPass)
	}
	if svc.loginCalls != 1 {
		t.Errorf("login calls: got %d, want 1 — the session must come from a verified credential", svc.loginCalls)
	}
	if got.AccessToken == "" || got.RefreshToken == "" {
		t.Error("Register returned no session tokens")
	}
	// A brand-new account has no second factor and must not be reported as
	// having one.
	if got.Profile.TOTPConfirmed || got.Profile.TOTPPending {
		t.Error("a newly registered account was reported as having a second factor")
	}
}

// TestRegisterDoesNotLoginWhenRegistrationFails: a duplicate address must not
// then be probed with the supplied password, which would turn a failed
// registration into a free credential check against an existing account.
func TestRegisterDoesNotLoginWhenRegistrationFails(t *testing.T) {
	svc := &fakeService{registerErr: errors.New("already exists")}
	a := newTestAdapter(t, svc)

	if _, err := a.Register(context.Background(), "taken@example.test", "guess", httpapi.AuditContext{}); err == nil {
		t.Fatal("Register: want an error, got nil")
	}
	if svc.loginCalls != 0 {
		t.Errorf("login calls after a failed registration: got %d, want 0", svc.loginCalls)
	}
}

func TestRedeemAndRevokeForwardTheToken(t *testing.T) {
	svc := &fakeService{session: sampleSession()}
	a := newTestAdapter(t, svc)
	ctx := context.Background()

	if _, err := a.Redeem(ctx, "presented-refresh", httpapi.AuditContext{}); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if svc.refreshTok != "presented-refresh" {
		t.Errorf("Redeem forwarded %q, want %q", svc.refreshTok, "presented-refresh")
	}

	if err := a.Revoke(ctx, "revoke-me", httpapi.AuditContext{}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if svc.logoutTok != "revoke-me" {
		t.Errorf("Revoke forwarded %q, want %q", svc.logoutTok, "revoke-me")
	}
}

// TestBeginTOTPReturnsTheURIOnce checks the one place a secret is deliberately
// unwrapped, and that ExpiresAt is left zero rather than invented.
func TestBeginTOTPReturnsTheURIOnce(t *testing.T) {
	svc := &fakeService{enrol: auth.TOTPEnrolment{URI: auth.NewSecret("otpauth://totp/x?secret=ABC")}}
	a := newTestAdapter(t, svc)

	got, err := a.BeginTOTP(context.Background(), domain.UserID("usr_x"), httpapi.AuditContext{})
	if err != nil {
		t.Fatalf("BeginTOTP: %v", err)
	}
	if got.ProvisioningURI != "otpauth://totp/x?secret=ABC" {
		t.Errorf("ProvisioningURI: got %q", got.ProvisioningURI)
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt: got %s, want the zero time — the service has no expiry to report", got.ExpiresAt)
	}
	if svc.totpUser != domain.UserID("usr_x") {
		t.Errorf("user id: got %q", svc.totpUser)
	}
}

// TestRemoveTOTPRoutesToTheCodeBasedDisable pins the contract openapi.yaml
// states: removal is proven with a code from the factor, never with a password.
func TestRemoveTOTPRoutesToTheCodeBasedDisable(t *testing.T) {
	svc := &fakeService{}
	a := newTestAdapter(t, svc)

	if err := a.RemoveTOTP(context.Background(), domain.UserID("usr_y"), "654321", httpapi.AuditContext{}); err != nil {
		t.Fatalf("RemoveTOTP: %v", err)
	}
	if svc.removeCode != "654321" {
		t.Errorf("code forwarded to DisableTOTPWithCode: got %q, want %q", svc.removeCode, "654321")
	}
}

func TestConfirmTOTPForwardsTheCode(t *testing.T) {
	svc := &fakeService{}
	a := newTestAdapter(t, svc)

	if err := a.ConfirmTOTP(context.Background(), domain.UserID("usr_z"), "111222", httpapi.AuditContext{}); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	if svc.totpCode != "111222" {
		t.Errorf("code: got %q, want %q", svc.totpCode, "111222")
	}
}

// TestProfileFailsClosedWhenStatusIsUnreadable: a session must not be returned
// with TOTPConfirmed silently false, which would tell an account page a user
// has no second factor when they do.
func TestProfileFailsClosedWhenStatusIsUnreadable(t *testing.T) {
	svc := &fakeService{session: sampleSession(), statusErr: errors.New("store down")}
	a := newTestAdapter(t, svc)

	if _, err := a.Authenticate(context.Background(), "e", "p", "", httpapi.AuditContext{}); err == nil {
		t.Fatal("Authenticate: want an error when the TOTP status cannot be read, got nil")
	}
}
