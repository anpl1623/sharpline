package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// A store failure must never be reported as a credential failure.
//
// This is a security property, not a tidiness one, and it fails in a specific
// direction. If a database outage surfaces as [ErrCredentials], then:
//
//   - every user sees "wrong password" and starts retyping, which multiplies
//     the load on the thing that is already failing;
//   - the login-failure metric spikes, so an outage is indistinguishable from a
//     credential-stuffing wave on the dashboard, and whoever is on call
//     investigates the wrong incident;
//   - and any rate limiter keyed on failed logins locks out the entire user
//     base for a fault none of them caused.
//
// The rule is therefore mechanical: every method, with every store method it
// touches failing in turn, must return something that does NOT wrap
// [ErrUnauthenticated].
func TestStoreFailuresAreNeverReportedAsCredentialFailures(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset by peer")

	cases := []struct {
		name string
		// method is the store method to break.
		method string
		// call exercises the service path that touches it. The fixture is
		// populated with a registered, logged-in user -- NOT an enrolled one.
		call func(t *testing.T, f *fixture, user UserRecord, sess Session) error
		// setup runs before the store is broken, for cases whose path is only
		// reachable from a state the bare fixture does not have. Without it a
		// case can pass while never reaching the method it names: DisableTOTP
		// returns ErrTOTPNotEnrolled at its LoadTOTP guard and never calls
		// DeleteTOTP at all, so breaking DeleteTOTP proved nothing.
		setup func(t *testing.T, f *fixture, user UserRecord)
	}{
		{name: "Login/UserByEmail", method: "UserByEmail", call: func(t *testing.T, f *fixture, _ UserRecord, _ Session) error {
			_, err := f.svc.Login(context.Background(), LoginRequest{
				Email: "person@example.com", Password: NewSecret(testPassword)})
			return err
		}},
		{name: "Login/LoadTOTP", method: "LoadTOTP", call: func(t *testing.T, f *fixture, _ UserRecord, _ Session) error {
			_, err := f.svc.Login(context.Background(), LoginRequest{
				Email: "person@example.com", Password: NewSecret(testPassword)})
			return err
		}},
		{name: "Login/CreateSession", method: "CreateSession", call: func(t *testing.T, f *fixture, _ UserRecord, _ Session) error {
			_, err := f.svc.Login(context.Background(), LoginRequest{
				Email: "person@example.com", Password: NewSecret(testPassword)})
			return err
		}},
		{name: "Register/CreateUser", method: "CreateUser", call: func(t *testing.T, f *fixture, _ UserRecord, _ Session) error {
			_, err := f.svc.Register(context.Background(), "other@example.com", NewSecret(testPassword))
			return err
		}},
		{name: "Refresh/Rotate", method: "Rotate", call: func(t *testing.T, f *fixture, _ UserRecord, sess Session) error {
			_, err := f.svc.Refresh(context.Background(), sess.RefreshToken)
			return err
		}},
		{name: "Refresh/UserByID", method: "UserByID", call: func(t *testing.T, f *fixture, _ UserRecord, sess Session) error {
			_, err := f.svc.Refresh(context.Background(), sess.RefreshToken)
			return err
		}},
		{name: "Logout/RevokeByToken", method: "RevokeByToken", call: func(t *testing.T, f *fixture, _ UserRecord, sess Session) error {
			return f.svc.Logout(context.Background(), sess.RefreshToken)
		}},
		{name: "LogoutEverywhere/RevokeUserSessions", method: "RevokeUserSessions", call: func(t *testing.T, f *fixture, user UserRecord, _ Session) error {
			_, err := f.svc.LogoutEverywhere(context.Background(), user.ID, RevokedReasonOperator)
			return err
		}},
		{name: "ChangePassword/UserByID", method: "UserByID", call: func(t *testing.T, f *fixture, user UserRecord, _ Session) error {
			return f.svc.ChangePassword(context.Background(), user.ID,
				NewSecret(testPassword), NewSecret("a completely different passphrase"))
		}},
		{name: "ChangePassword/SetPassword", method: "SetPassword", call: func(t *testing.T, f *fixture, user UserRecord, _ Session) error {
			return f.svc.ChangePassword(context.Background(), user.ID,
				NewSecret(testPassword), NewSecret("a completely different passphrase"))
		}},
		{name: "AuthorizeWagering/UserStatus", method: "UserStatus", call: func(t *testing.T, f *fixture, user UserRecord, _ Session) error {
			return f.svc.AuthorizeWagering(context.Background(), user.ID)
		}},
		{name: "BeginTOTPEnrolment/LoadTOTP", method: "LoadTOTP", call: func(t *testing.T, f *fixture, user UserRecord, _ Session) error {
			_, err := f.svc.BeginTOTPEnrolment(context.Background(), user.ID)
			return err
		}},
		{name: "BeginTOTPEnrolment/SaveTOTP", method: "SaveTOTP", call: func(t *testing.T, f *fixture, user UserRecord, _ Session) error {
			_, err := f.svc.BeginTOTPEnrolment(context.Background(), user.ID)
			return err
		}},
		{name: "TOTPStatus/LoadTOTP", method: "LoadTOTP", call: func(t *testing.T, f *fixture, user UserRecord, _ Session) error {
			_, err := f.svc.TOTPStatus(context.Background(), user.ID)
			return err
		}},
		{name: "DisableTOTP/DeleteTOTP", method: "DeleteTOTP", call: func(t *testing.T, f *fixture, user UserRecord, _ Session) error {
			return f.svc.DisableTOTP(context.Background(), user.ID, NewSecret(testPassword))
		}, setup: func(t *testing.T, f *fixture, user UserRecord) {
			f.enrol(t, user.ID)
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			user := f.register(t, "person@example.com")
			sess := f.login(t, "person@example.com")
			f.clock.advance(time.Minute)

			if c.setup != nil {
				c.setup(t, f, user)
			}

			f.store.fail(c.method, boom)
			err := c.call(t, f, user, sess)

			if err == nil {
				t.Fatalf("%s failed and the call succeeded", c.method)
			}
			if errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("a failing %s produced %v, which wraps ErrUnauthenticated. "+
					"A database outage would render as a wave of failed logins.", c.method, err)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("a failing %s produced %v, which does not wrap the store's error; "+
					"the real cause is unrecoverable from the log", c.method, err)
			}
		})
	}
}

// Some failures are deliberately SWALLOWED, and each one is a considered
// decision rather than an oversight. This test pins them, because "swallowed"
// and "forgotten" look identical in the code and only one of them is correct.
func TestDeliberatelySwallowedFailures(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset by peer")
	ctx := context.Background()

	t.Run("a failed rehash does not fail an authenticated login", func(t *testing.T) {
		t.Parallel()

		weak := Params{MemoryKiB: minMemoryKiB, Time: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16}
		f := newFixture(t, func(o *Options) {
			h, err := NewHasher(HasherOptions{Params: weak})
			if err != nil {
				t.Fatalf("NewHasher: %v", err)
			}
			o.Hasher = h
		})
		f.register(t, "person@example.com")

		strong := Params{MemoryKiB: minMemoryKiB, Time: 2, Parallelism: 1, SaltLen: 16, KeyLen: 32}
		upgraded, err := NewHasher(HasherOptions{Params: strong})
		if err != nil {
			t.Fatalf("NewHasher: %v", err)
		}
		f.svc.hasher = upgraded
		f.store.fail("RehashPassword", boom)

		// The user proved their password. Failing them because an optimisation
		// did not work converts a cost-parameter bump into an outage.
		if _, err := f.svc.Login(ctx, LoginRequest{
			Email: "person@example.com", Password: NewSecret(testPassword)}); err != nil {
			t.Fatalf("Login = %v, want nil", err)
		}
	})

	t.Run("a failed revocation does not fail a completed password change", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		user := f.register(t, "person@example.com")
		f.clock.advance(time.Minute)
		f.store.fail("RevokeUserSessions", boom)

		// The password IS stored by this point. Returning an error would invite
		// the caller to retry with a `current` that no longer works, and
		// password_changed_at already condemns every pre-change family.
		if err := f.svc.ChangePassword(ctx, user.ID,
			NewSecret(testPassword), NewSecret("a completely different passphrase")); err != nil {
			t.Fatalf("ChangePassword = %v, want nil", err)
		}
		if _, err := f.svc.Login(ctx, LoginRequest{
			Email: "person@example.com", Password: NewSecret("a completely different passphrase")}); err != nil {
			t.Fatalf("Login with the new password = %v; the change did not take effect", err)
		}
	})

	t.Run("a failed recovery-code mint does not un-arm a confirmed second factor", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, func(o *Options) { o.RecoveryCodes = failingRecoveryStore{err: boom} })
		user := f.register(t, "person@example.com")

		if _, err := f.svc.BeginTOTPEnrolment(ctx, user.ID); err != nil {
			t.Fatalf("BeginTOTPEnrolment: %v", err)
		}
		rec, _, err := f.store.LoadTOTP(ctx, user.ID)
		if err != nil {
			t.Fatalf("LoadTOTP: %v", err)
		}
		secret, err := f.keyring.Open(user.ID, rec.Sealed)
		if err != nil {
			t.Fatalf("opening the sealed secret: %v", err)
		}
		code, err := TOTPCodeAt(secret, f.clock.Now(), TOTPConfig{})
		if err != nil {
			t.Fatalf("TOTPCodeAt: %v", err)
		}

		// The factor is ALREADY armed by the time the codes are minted. Failing
		// here would leave the user believing 2FA did not enrol — so they would
		// not save recovery codes for a factor that is about to lock them out.
		codes, err := f.svc.ConfirmTOTPEnrolment(ctx, user.ID, code)
		if err != nil {
			t.Fatalf("ConfirmTOTPEnrolment = %v, want nil", err)
		}
		if len(codes) != 0 {
			t.Fatalf("got %d recovery codes from a failing store", len(codes))
		}

		status, err := f.svc.TOTPStatus(ctx, user.ID)
		if err == nil && !status.Enrolled {
			t.Fatal("the second factor is not armed after a successful confirmation")
		}
	})
}

// failingRecoveryStore fails every call.
type failingRecoveryStore struct{ err error }

func (s failingRecoveryStore) ReplaceRecoveryCodes(context.Context, domain.UserID, [][]byte, time.Time) error {
	return s.err
}

func (s failingRecoveryStore) ConsumeRecoveryCode(context.Context, domain.UserID, []byte, time.Time) (bool, error) {
	return false, s.err
}

func (s failingRecoveryStore) UnusedRecoveryCodeDigests(context.Context, domain.UserID) ([][]byte, error) {
	return nil, s.err
}

var _ RecoveryCodeStore = failingRecoveryStore{}

// The small accessors. They are one line each and would otherwise be the only
// uncovered statements in their files, which makes the coverage report harder
// to read than the code.
func TestAccessors(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	if got := f.svc.AccessTokenTTL(); got != DefaultAccessTTL {
		t.Errorf("AccessTokenTTL = %s, want %s", got, DefaultAccessTTL)
	}
	if got := f.svc.tokens.TTL(); got != DefaultAccessTTL {
		t.Errorf("TokenIssuer.TTL = %s, want %s", got, DefaultAccessTTL)
	}

	// A keyring logged whole reports its shape and never its contents.
	v := f.keyring.LogValue()
	if v.String() == "" {
		t.Error("Keyring.LogValue produced nothing")
	}
	for _, attr := range v.Group() {
		if attr.Key == "active_key_id" && attr.Value.String() != f.keyring.ActiveKeyID() {
			t.Errorf("LogValue active_key_id = %s, want %s", attr.Value, f.keyring.ActiveKeyID())
		}
	}

	// Secret.UnmarshalText is the encoding.TextUnmarshaler half of the
	// asymmetry: decoding INTO a secret is safe, marshalling out is refused.
	var s Secret
	if err := s.UnmarshalText([]byte("hunter2")); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if s.Expose() != "hunter2" {
		t.Errorf("UnmarshalText produced %q", s.Expose())
	}
}
