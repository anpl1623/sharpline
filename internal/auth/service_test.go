package auth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/anpl1623/sharpline/internal/domain"
)

const testPassword = "correct horse battery staple"

// fixture is one Service with everything it needs, plus handles on the pieces a
// test wants to manipulate.
type fixture struct {
	svc      *Service
	store    *memStore
	recovery *memRecoveryStore
	keyring  *Keyring
	clock    *testClock
	registry *prometheus.Registry
}

// testClock is a settable clock. Every instant the service persists comes
// through it, so a test can move a session past its expiry without sleeping —
// which is also why migrations/00005 forbids a trigger from writing a domain
// instant.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newFixture(t *testing.T, mutate ...func(*Options)) *fixture {
	t.Helper()

	clock := &testClock{now: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)}
	store := newMemStore()
	recovery := newMemRecoveryStore()

	hasher, err := NewHasher(HasherOptions{Params: testParams})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	tokens, err := NewTokenIssuer(TokenIssuerOptions{
		SigningKey: testSigningKey,
		Now:        clock.Now,
	})
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	keyring := testKeyring(t)
	registry := prometheus.NewRegistry()

	opts := Options{
		Store:         store,
		Hasher:        hasher,
		Tokens:        tokens,
		Keyring:       keyring,
		RecoveryCodes: recovery,
		Now:           clock.Now,
		Registry:      registry,
		// Discard: these tests assert on behaviour, and a package that logs a
		// security event per test would drown the failure output. The redaction
		// of what IS logged is asserted in secret_test.go, structurally.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, m := range mutate {
		m(&opts)
	}

	svc, err := NewService(opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &fixture{
		svc: svc, store: store, recovery: recovery,
		keyring: keyring, clock: clock, registry: registry,
	}
}

func (f *fixture) register(t *testing.T, email string) UserRecord {
	t.Helper()
	rec, err := f.svc.Register(context.Background(), email, NewSecret(testPassword))
	if err != nil {
		t.Fatalf("Register(%q): %v", email, err)
	}
	return rec
}

func (f *fixture) login(t *testing.T, email string) Session {
	t.Helper()
	sess, err := f.svc.Login(context.Background(), LoginRequest{
		Email: email, Password: NewSecret(testPassword),
	})
	if err != nil {
		t.Fatalf("Login(%q): %v", email, err)
	}
	return sess
}

// -----------------------------------------------------------------------------
// registration
// -----------------------------------------------------------------------------

func TestRegister(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rec := f.register(t, "  Person@Example.COM ")

	if rec.Email != Email("person@example.com") {
		t.Errorf("stored email = %q; it must be normalised or users_email_normalised refuses the row", rec.Email)
	}
	if rec.Status != UserStatusActive {
		t.Errorf("status = %s, want active", rec.Status)
	}
	if !strings.HasPrefix(rec.PasswordHash, phcPrefix) {
		t.Errorf("password hash %q is not argon2id", rec.PasswordHash)
	}
	if rec.PasswordHash == testPassword {
		t.Fatal("the password was stored in plaintext")
	}
	if !rec.PasswordChangedAt.Equal(f.clock.Now().UTC().Truncate(time.Microsecond)) {
		t.Errorf("password_changed_at = %s, want the service clock", rec.PasswordChangedAt)
	}
	if rec.ID.IsZero() {
		t.Error("no user id was minted")
	}
	if _, err := domain.NewUserID(rec.ID.String()); err != nil {
		t.Errorf("minted user id %q is not a valid domain id: %v", rec.ID, err)
	}
}

func TestRegisterRejectsADuplicateAddressRegardlessOfCase(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.register(t, "person@example.com")

	_, err := f.svc.Register(context.Background(), "PERSON@example.com", NewSecret(testPassword))
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("Register of a case variant = %v, want ErrEmailTaken", err)
	}
}

func TestRegisterValidatesItsInput(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.svc.Register(ctx, "not-an-address", NewSecret(testPassword)); !errors.Is(err, ErrEmailShape) {
		t.Errorf("Register with a malformed address = %v, want ErrEmailShape", err)
	}
	if _, err := f.svc.Register(ctx, "person@example.com", NewSecret("short")); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("Register with a short password = %v, want ErrPasswordTooShort", err)
	}
	// A validation failure must not have written anything.
	if _, found, _ := f.store.UserByEmail(ctx, "person@example.com"); found {
		t.Error("a rejected registration created a user")
	}
}

// -----------------------------------------------------------------------------
// login
// -----------------------------------------------------------------------------

func TestLoginIssuesAWorkingSession(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	sess := f.login(t, "person@example.com")

	if sess.User.ID != user.ID {
		t.Errorf("session user = %q, want %q", sess.User.ID, user.ID)
	}
	claims, err := f.svc.VerifyAccessToken(sess.AccessToken)
	if err != nil {
		t.Fatalf("the access token it just minted does not verify: %v", err)
	}
	if claims.Subject != user.ID {
		t.Errorf("sub = %q, want %q", claims.Subject, user.ID)
	}
	if claims.SessionID != sess.SessionID {
		t.Errorf("sid = %q, want the family id %q", claims.SessionID, sess.SessionID)
	}
	if sess.RefreshToken.IsZero() {
		t.Fatal("no refresh token was issued")
	}
	if got, want := sess.RefreshToken.Len(), b64id.EncodedLen(TokenEntropyBytes); got != want {
		t.Errorf("refresh token is %d characters, want %d (%d bytes of entropy)",
			got, want, TokenEntropyBytes)
	}
	// The absolute session cap applies from the first token, not only from the
	// first rotation.
	wantExpiry := f.clock.Now().UTC().Truncate(time.Microsecond).Add(DefaultRefreshTTL)
	if !sess.RefreshExpiresAt.Equal(wantExpiry) {
		t.Errorf("refresh expiry = %s, want %s", sess.RefreshExpiresAt, wantExpiry)
	}
}

// The core anti-enumeration property, on the two axes a caller controls.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.register(t, "person@example.com")
	ctx := context.Background()

	cases := []struct {
		name string
		req  LoginRequest
	}{
		{"unknown email", LoginRequest{Email: "nobody@example.com", Password: NewSecret(testPassword)}},
		{"wrong password", LoginRequest{Email: "person@example.com", Password: NewSecret("wrong password here")}},
		{"malformed email", LoginRequest{Email: "not-an-address", Password: NewSecret(testPassword)}},
		{"empty password", LoginRequest{Email: "person@example.com", Password: Secret{}}},
	}

	var messages []string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.svc.Login(ctx, c.req)
			if !errors.Is(err, ErrCredentials) {
				t.Fatalf("Login = %v, want ErrCredentials", err)
			}
			// One sentinel means one rendered response. A caller cannot leak
			// which case it was because it is not told.
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("Login error %v is outside the ErrUnauthenticated root", err)
			}
			messages = append(messages, err.Error())
		})
	}

	for i := 1; i < len(messages); i++ {
		if messages[i] != messages[0] {
			t.Errorf("login failure messages differ:\n  %q\n  %q", messages[0], messages[i])
		}
	}
}

// The timing half. argon2id dominates a login by orders of magnitude, so the
// question is whether the unknown-email path pays for it — and the decoy is how
// it does.
//
// This is a STATISTICAL assertion with a deliberately loose bound. A tight one
// would be flaky on a shared CI runner and would then be disabled, which is
// worse than a loose one that catches the actual regression: somebody adding an
// early `return ErrCredentials` before the decoy, which makes the unknown-email
// path ~1000x faster rather than ~1.2x.
func TestLoginTimingDoesNotDistinguishAnUnknownEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison")
	}
	t.Parallel()

	f := newFixture(t)
	f.register(t, "person@example.com")
	ctx := context.Background()

	const samples = 12

	measure := func(req LoginRequest) time.Duration {
		// Warm up so the first iteration's allocation does not dominate.
		_, _ = f.svc.Login(ctx, req)

		var total time.Duration
		for i := 0; i < samples; i++ {
			start := time.Now()
			if _, err := f.svc.Login(ctx, req); !errors.Is(err, ErrCredentials) {
				t.Fatalf("Login = %v, want ErrCredentials", err)
			}
			total += time.Since(start)
		}
		return total / samples
	}

	wrongPassword := measure(LoginRequest{
		Email: "person@example.com", Password: NewSecret("wrong password here")})
	unknownEmail := measure(LoginRequest{
		Email: "nobody@example.com", Password: NewSecret("wrong password here")})

	t.Logf("wrong password %s, unknown email %s (ratio %.2f)",
		wrongPassword, unknownEmail, float64(unknownEmail)/float64(wrongPassword))

	// The regression this catches is an order of magnitude, not a percentage.
	ratio := float64(unknownEmail) / float64(wrongPassword)
	if ratio < 0.2 || ratio > 5 {
		t.Fatalf("the unknown-email path took %s against %s for a wrong password (ratio %.2f). "+
			"That is a remote user-enumeration oracle: an attacker can tell which addresses "+
			"are registered by timing the response. Check that Login still calls "+
			"Hasher.VerifyDecoy on every path where no user matched.",
			unknownEmail, wrongPassword, ratio)
	}
}

func TestLoginRefusesSuspendedAndClosedAccounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status UserStatus
		want   error
	}{
		{UserStatusSuspended, ErrAccountSuspended},
		{UserStatusClosed, ErrAccountClosed},
	}
	for _, c := range cases {
		t.Run(c.status.String(), func(t *testing.T) {
			f := newFixture(t)
			user := f.register(t, "person@example.com")
			f.store.setStatus(user.ID, c.status)

			_, err := f.svc.Login(context.Background(), LoginRequest{
				Email: "person@example.com", Password: NewSecret(testPassword)})
			if !errors.Is(err, c.want) {
				t.Fatalf("Login = %v, want %v", err, c.want)
			}

			// The refusal must come AFTER password verification, or it is an
			// enumeration oracle: an attacker who does not know the password
			// would learn that the account exists and is suspended.
			_, err = f.svc.Login(context.Background(), LoginRequest{
				Email: "person@example.com", Password: NewSecret("wrong password here")})
			if !errors.Is(err, ErrCredentials) {
				t.Fatalf("Login with a wrong password on a %s account = %v, want ErrCredentials",
					c.status, err)
			}
		})
	}
}

// The responsible-gaming asymmetry. CLAUDE.md §6 asks for self-imposed limits;
// locking a self-excluded customer out of their own account would make the tool
// that protects them a punishment for using it.
func TestSelfExcludedCanSignInButCannotWager(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	ctx := context.Background()

	if err := f.svc.AuthorizeWagering(ctx, user.ID); err != nil {
		t.Fatalf("AuthorizeWagering for an active account = %v, want nil", err)
	}

	f.store.setStatus(user.ID, UserStatusSelfExcluded)

	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword)}); err != nil {
		t.Fatalf("Login while self-excluded = %v; a self-excluded customer must be able to "+
			"read their history and manage the exclusion", err)
	}
	if err := f.svc.AuthorizeWagering(ctx, user.ID); !errors.Is(err, ErrSelfExcluded) {
		t.Fatalf("AuthorizeWagering while self-excluded = %v, want ErrSelfExcluded", err)
	}
}

func TestAuthorizeWageringRefusesAnUnknownAccount(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	if err := f.svc.AuthorizeWagering(context.Background(), "usr_nobody"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("AuthorizeWagering for an unknown account = %v, want ErrForbidden", err)
	}
}

// A store failure must not be reported as a bad password: a database outage
// would then look like a wave of credential stuffing.
func TestLoginReportsStoreFailuresAsInternal(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.register(t, "person@example.com")

	boom := errors.New("connection refused")
	f.store.fail("UserByEmail", boom)

	_, err := f.svc.Login(context.Background(), LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword)})
	if errors.Is(err, ErrCredentials) {
		t.Fatal("a store failure was reported as a credential failure")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Login = %v, want it to wrap the store error", err)
	}
}

// -----------------------------------------------------------------------------
// rehash on login
// -----------------------------------------------------------------------------

// Rehash-on-login is the ONLY moment a stronger hash can be derived without
// asking the user to retype their password. The subtle requirement is that it
// must not look like a credential change.
func TestRehashOnLoginUpgradesTheStoredHashWithoutRevokingSessions(t *testing.T) {
	t.Parallel()

	// Register under weak parameters...
	weak := Params{MemoryKiB: minMemoryKiB, Time: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16}
	f := newFixture(t, func(o *Options) {
		h, err := NewHasher(HasherOptions{Params: weak})
		if err != nil {
			t.Fatalf("NewHasher: %v", err)
		}
		o.Hasher = h
	})
	user := f.register(t, "person@example.com")
	before := f.store.users[user.ID]

	// ...then raise the policy, as a deploy would.
	strong := Params{MemoryKiB: minMemoryKiB, Time: 2, Parallelism: 1, SaltLen: 16, KeyLen: 32}
	upgraded, err := NewHasher(HasherOptions{Params: strong})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	f.svc.hasher = upgraded

	// Open a second session, so there is something a spurious revocation would
	// break.
	other := f.login(t, "person@example.com")

	f.clock.advance(time.Hour)
	f.login(t, "person@example.com")

	after := f.store.users[user.ID]
	params, _, _, err := DecodeHash(after.PasswordHash)
	if err != nil {
		t.Fatalf("DecodeHash: %v", err)
	}
	if !params.AtLeastAsStrongAs(strong) {
		t.Fatalf("stored hash is still at %s after a login under policy %s", params, strong)
	}

	// THE assertion. If the rehash had gone through SetPassword rather than
	// RehashPassword, password_changed_at would have moved and every other
	// session this user holds — on every device — would be revoked, for a
	// change the user did not make.
	if !after.PasswordChangedAt.Equal(before.PasswordChangedAt) {
		t.Fatalf("password_changed_at moved from %s to %s during a rehash; "+
			"a cost-parameter bump would log every active user out everywhere",
			before.PasswordChangedAt, after.PasswordChangedAt)
	}
	if _, err := f.svc.Refresh(context.Background(), other.RefreshToken); err != nil {
		t.Fatalf("a session opened before the rehash no longer refreshes: %v", err)
	}
}

// A rehash that fails must not fail the login. The user has already
// authenticated; failing them because an optimisation did not work turns a
// parameter bump into an outage.
func TestRehashFailureDoesNotFailTheLogin(t *testing.T) {
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
	f.store.fail("RehashPassword", errors.New("disk full"))

	if _, err := f.svc.Login(context.Background(), LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword)}); err != nil {
		t.Fatalf("Login = %v; a failed rehash must not fail an authenticated login", err)
	}
}

// -----------------------------------------------------------------------------
// refresh, rotation and reuse
// -----------------------------------------------------------------------------

func TestRefreshRotatesTheToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.register(t, "person@example.com")
	first := f.login(t, "person@example.com")

	f.clock.advance(time.Minute)
	second, err := f.svc.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if second.RefreshToken.Equal(first.RefreshToken) {
		t.Fatal("Refresh returned the same token; nothing rotated")
	}
	if second.SessionID != first.SessionID {
		t.Errorf("session id changed on refresh: %q -> %q", first.SessionID, second.SessionID)
	}
	if second.AccessToken.Equal(first.AccessToken) {
		t.Fatal("Refresh returned the same access token")
	}
	if _, err := f.svc.VerifyAccessToken(second.AccessToken); err != nil {
		t.Fatalf("the refreshed access token does not verify: %v", err)
	}
}

// THE security property of this phase.
//
// A presented-twice token is either a replay by a thief or a replay by the
// legitimate client whose newer token the thief stole. They cannot be told
// apart, so the only safe response is to end the whole lineage.
func TestRefreshReuseRevokesTheWholeLineage(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.register(t, "person@example.com")
	ctx := context.Background()

	first := f.login(t, "person@example.com")
	f.clock.advance(time.Minute)
	second, err := f.svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// The thief replays the token the client already redeemed.
	f.clock.advance(time.Minute)
	if _, err := f.svc.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrTokenReuse) {
		t.Fatalf("Refresh of an already-redeemed token = %v, want ErrTokenReuse", err)
	}

	// Revoking only the replayed token would leave the thief's freshly-rotated
	// successor working. The WHOLE family must be dead, which means the
	// legitimate client's current token is dead too — that is the point, and it
	// is why the reason has to be recorded.
	if _, err := f.svc.Refresh(ctx, second.RefreshToken); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("Refresh of the legitimate successor after reuse = %v, want ErrSessionRevoked", err)
	}
	reason, revoked := f.store.familyReason(first.SessionID)
	if !revoked {
		t.Fatal("the family was not revoked")
	}
	if reason != RevokedReasonReuseDetected {
		t.Fatalf("revoked_reason = %s, want reuse_detected; "+
			"\"how many sessions did we kill for token reuse this week\" depends on this value",
			reason)
	}

	// And the metric a security alert fires on.
	assertCounter(t, f.registry, "sharpline_auth_refresh_total", map[string]string{"outcome": "reuse"}, 1)
}

func TestRefreshRejectsUnknownExpiredAndRevokedTokens(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("unknown", func(t *testing.T) {
		f := newFixture(t)
		bogus, err := NewRefreshTokenSecret()
		if err != nil {
			t.Fatalf("NewRefreshTokenSecret: %v", err)
		}
		if _, err := f.svc.Refresh(ctx, bogus); !errors.Is(err, ErrTokenUnknown) {
			t.Fatalf("Refresh of an unknown token = %v, want ErrTokenUnknown", err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.svc.Refresh(ctx, Secret{}); !errors.Is(err, ErrTokenUnknown) {
			t.Fatalf("Refresh of an empty token = %v, want ErrTokenUnknown", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		f := newFixture(t)
		f.register(t, "person@example.com")
		sess := f.login(t, "person@example.com")

		f.clock.advance(DefaultRefreshTTL + time.Second)
		if _, err := f.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, ErrTokenExpired) {
			t.Fatalf("Refresh past the idle timeout = %v, want ErrTokenExpired", err)
		}
	})

	t.Run("revoked by logout", func(t *testing.T) {
		f := newFixture(t)
		f.register(t, "person@example.com")
		sess := f.login(t, "person@example.com")

		if err := f.svc.Logout(ctx, sess.RefreshToken); err != nil {
			t.Fatalf("Logout: %v", err)
		}
		if _, err := f.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("Refresh after logout = %v, want ErrSessionRevoked", err)
		}
		reason, revoked := f.store.familyReason(sess.SessionID)
		if !revoked || reason != RevokedReasonLogout {
			t.Fatalf("family revoked=%v reason=%s, want logout", revoked, reason)
		}
	})
}

// Without an absolute cap, rotation makes sessions immortal: every redemption
// issues a successor with a fresh idle clock, so a client that refreshes weekly
// never re-authenticates and a stolen token is a permanent credential.
func TestSessionLifetimeCapsRotationRatherThanRevoking(t *testing.T) {
	t.Parallel()

	const (
		refreshTTL = time.Hour
		lifetime   = 3 * time.Hour
	)
	f := newFixture(t, func(o *Options) {
		o.RefreshTTL = refreshTTL
		o.SessionLifetime = lifetime
	})
	f.register(t, "person@example.com")
	ctx := context.Background()

	sess := f.login(t, "person@example.com")
	started := f.clock.Now().UTC().Truncate(time.Microsecond)

	// Rotate repeatedly. Each successor's expiry must be capped at
	// started + lifetime rather than issued + refreshTTL.
	for i := 0; i < 4; i++ {
		f.clock.advance(30 * time.Minute)
		next, err := f.svc.Refresh(ctx, sess.RefreshToken)
		if err != nil {
			break
		}
		if next.RefreshExpiresAt.After(started.Add(lifetime)) {
			t.Fatalf("successor %d expires at %s, past the absolute session limit %s; "+
				"rotation is making the session immortal",
				i, next.RefreshExpiresAt, started.Add(lifetime))
		}
		sess = next
	}

	// Past the absolute lifetime the lineage is over — as an EXPIRY, not a
	// revocation, because refresh_token_families_revoked_reason_defined has no
	// 'expired' value and filing an age-out under 'operator' would be a lie.
	f.clock.advance(lifetime)
	if _, err := f.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Refresh past the absolute session lifetime = %v, want ErrTokenExpired", err)
	}
	if _, revoked := f.store.familyReason(sess.SessionID); revoked {
		t.Fatal("the family was revoked for ageing out; the schema has no reason for that")
	}
}

func TestRefreshRefusesAnAccountThatCanNoLongerAuthenticate(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	sess := f.login(t, "person@example.com")

	f.store.setStatus(user.ID, UserStatusSuspended)
	f.clock.advance(time.Minute)

	if _, err := f.svc.Refresh(context.Background(), sess.RefreshToken); !errors.Is(err, ErrAccountSuspended) {
		t.Fatalf("Refresh on a suspended account = %v, want ErrAccountSuspended", err)
	}
	// And the lineage is closed, so a suspension does not have to wait for the
	// refresh token to age out.
	if _, revoked := f.store.familyReason(sess.SessionID); !revoked {
		t.Error("a suspended account's session was left live")
	}
}

func TestLogoutIsIdempotentAndDoesNotLeak(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.register(t, "person@example.com")
	sess := f.login(t, "person@example.com")
	ctx := context.Background()

	if err := f.svc.Logout(ctx, sess.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	// A retry after a network failure, or a second tab, must not be a 401 for
	// successfully being logged out.
	if err := f.svc.Logout(ctx, sess.RefreshToken); err != nil {
		t.Fatalf("second Logout = %v, want nil", err)
	}
	// An unknown token likewise: answering differently would tell a caller
	// whether a token they guessed exists.
	bogus, err := NewRefreshTokenSecret()
	if err != nil {
		t.Fatalf("NewRefreshTokenSecret: %v", err)
	}
	if err := f.svc.Logout(ctx, bogus); err != nil {
		t.Fatalf("Logout of an unknown token = %v, want nil", err)
	}
	if err := f.svc.Logout(ctx, Secret{}); err != nil {
		t.Fatalf("Logout of an empty token = %v, want nil", err)
	}
}

func TestLogoutEverywhere(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	a := f.login(t, "person@example.com")
	b := f.login(t, "person@example.com")
	ctx := context.Background()

	n, err := f.svc.LogoutEverywhere(ctx, user.ID, RevokedReasonOperator)
	if err != nil {
		t.Fatalf("LogoutEverywhere: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked %d sessions, want 2", n)
	}
	for _, sess := range []Session{a, b} {
		if _, err := f.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, ErrSessionRevoked) {
			t.Errorf("Refresh after LogoutEverywhere = %v, want ErrSessionRevoked", err)
		}
	}
	if _, err := f.svc.LogoutEverywhere(ctx, user.ID, RevokedReasonUnknown); !errors.Is(err, ErrInvalid) {
		t.Error("LogoutEverywhere accepted an undefined revocation reason")
	}
}

// -----------------------------------------------------------------------------
// password change
// -----------------------------------------------------------------------------

func TestChangePassword(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	sess := f.login(t, "person@example.com")
	ctx := context.Background()

	const next = "a completely different passphrase"
	f.clock.advance(time.Minute)
	if err := f.svc.ChangePassword(ctx, user.ID, NewSecret(testPassword), NewSecret(next)); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// The new credential works and the old one does not.
	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(next)}); err != nil {
		t.Fatalf("Login with the new password = %v", err)
	}
	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword)}); !errors.Is(err, ErrCredentials) {
		t.Fatalf("Login with the old password = %v, want ErrCredentials", err)
	}

	// Every session that predates the change is gone. A password change that
	// leaves old sessions alive does not evict whoever the user is changing
	// their password because of.
	if _, err := f.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a pre-change session still refreshes: %v", err)
	}
	reason, revoked := f.store.familyReason(sess.SessionID)
	if !revoked || reason != RevokedReasonCredentialChange {
		t.Fatalf("family revoked=%v reason=%s, want credential_change", revoked, reason)
	}
}

// Belt AND braces: password_changed_at independently condemns any family that
// predates it, so even a revocation that failed leaves no usable session.
func TestPasswordChangedAtInvalidatesOlderFamiliesEvenWhenRevocationFails(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	sess := f.login(t, "person@example.com")
	ctx := context.Background()

	f.clock.advance(time.Minute)
	f.store.fail("RevokeUserSessions", errors.New("connection reset"))

	// The password IS changed, so the call must succeed: reporting a failure
	// would invite the caller to retry with a `current` that no longer works.
	if err := f.svc.ChangePassword(ctx, user.ID,
		NewSecret(testPassword), NewSecret("a completely different passphrase")); err != nil {
		t.Fatalf("ChangePassword = %v; the password was already stored", err)
	}

	if _, err := f.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a pre-change session refreshed after a failed revocation: %v", err)
	}
	reason, revoked := f.store.familyReason(sess.SessionID)
	if !revoked || reason != RevokedReasonCredentialChange {
		t.Fatalf("family revoked=%v reason=%s, want credential_change from the rotation path",
			revoked, reason)
	}
}

func TestChangePasswordValidatesItsInput(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	ctx := context.Background()

	if err := f.svc.ChangePassword(ctx, user.ID,
		NewSecret("wrong password here"), NewSecret("a new passphrase entirely")); !errors.Is(err, ErrCredentials) {
		t.Errorf("ChangePassword with a wrong current password = %v, want ErrCredentials", err)
	}
	if err := f.svc.ChangePassword(ctx, user.ID,
		NewSecret(testPassword), NewSecret("short")); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("ChangePassword to a short password = %v, want ErrPasswordTooShort", err)
	}
	// A no-op change would move password_changed_at and revoke every session
	// for nothing — a self-inflicted denial of service dressed as a security
	// event.
	if err := f.svc.ChangePassword(ctx, user.ID,
		NewSecret(testPassword), NewSecret(testPassword)); !errors.Is(err, ErrPasswordUnchanged) {
		t.Errorf("ChangePassword to the same password = %v, want ErrPasswordUnchanged", err)
	}
	if err := f.svc.ChangePassword(ctx, "usr_nobody",
		NewSecret(testPassword), NewSecret("a new passphrase entirely")); !errors.Is(err, ErrCredentials) {
		t.Errorf("ChangePassword for an unknown user = %v, want ErrCredentials", err)
	}
}

// -----------------------------------------------------------------------------
// second factor
// -----------------------------------------------------------------------------

// enrol takes a user all the way through the two-step flow and returns the
// shared secret so the test can compute codes.
func (f *fixture) enrol(t *testing.T, id domain.UserID) []byte {
	t.Helper()
	ctx := context.Background()

	if _, err := f.svc.BeginTOTPEnrolment(ctx, id); err != nil {
		t.Fatalf("BeginTOTPEnrolment: %v", err)
	}
	rec, found, err := f.store.LoadTOTP(ctx, id)
	if err != nil || !found {
		t.Fatalf("LoadTOTP = %v, %v", found, err)
	}
	secret, err := f.keyring.Open(id, rec.Sealed)
	if err != nil {
		t.Fatalf("opening the sealed secret: %v", err)
	}

	code, err := TOTPCodeAt(secret, f.clock.Now(), TOTPConfig{})
	if err != nil {
		t.Fatalf("TOTPCodeAt: %v", err)
	}
	if _, err := f.svc.ConfirmTOTPEnrolment(ctx, id, code); err != nil {
		t.Fatalf("ConfirmTOTPEnrolment: %v", err)
	}
	return secret
}

func TestTOTPEnrolmentIsTwoStepsAndArmsNothingUntilConfirmed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	ctx := context.Background()

	enrolment, err := f.svc.BeginTOTPEnrolment(ctx, user.ID)
	if err != nil {
		t.Fatalf("BeginTOTPEnrolment: %v", err)
	}
	if enrolment.URI.IsZero() || enrolment.SecretBase32.IsZero() {
		t.Fatal("enrolment returned no provisioning material")
	}
	// Both carry the shared secret and must be unprintable.
	for _, s := range []Secret{enrolment.URI, enrolment.SecretBase32} {
		if !strings.Contains(s.String(), Redacted) {
			t.Error("enrolment material printed itself")
		}
	}

	// migrations/00005: "An unconfirmed row must NOT be treated as an enrolled
	// second factor -- otherwise a mis-scan locks the account out."
	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword)}); err != nil {
		t.Fatalf("Login with an UNCONFIRMED enrolment = %v; a mis-scan just locked the account", err)
	}
	status, err := f.svc.TOTPStatus(ctx, user.ID)
	if err != nil {
		t.Fatalf("TOTPStatus: %v", err)
	}
	if status.Enrolled || !status.Pending {
		t.Fatalf("status = %+v, want pending and not enrolled", status)
	}

	// A mis-scan can start again. Nothing is armed, so nothing is at risk.
	if _, err := f.svc.BeginTOTPEnrolment(ctx, user.ID); err != nil {
		t.Fatalf("re-enrolling over an unconfirmed row = %v, want nil", err)
	}

	secret := f.enrol(t, user.ID)

	status, err = f.svc.TOTPStatus(ctx, user.ID)
	if err != nil {
		t.Fatalf("TOTPStatus: %v", err)
	}
	if !status.Enrolled || status.Pending {
		t.Fatalf("status = %+v, want enrolled", status)
	}
	if status.RecoveryCodesRemaining != RecoveryCodeCount {
		t.Fatalf("recovery codes remaining = %d, want %d", status.RecoveryCodesRemaining, RecoveryCodeCount)
	}

	// Now the factor bites.
	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword)}); !errors.Is(err, ErrSecondFactorRequired) {
		t.Fatalf("Login without a code = %v, want ErrSecondFactorRequired", err)
	}

	f.clock.advance(TOTPPeriod)
	code, err := TOTPCodeAt(secret, f.clock.Now(), TOTPConfig{})
	if err != nil {
		t.Fatalf("TOTPCodeAt: %v", err)
	}
	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword), TOTPCode: code}); err != nil {
		t.Fatalf("Login with a correct code = %v", err)
	}
}

// Re-enrolling over a CONFIRMED factor is how an attacker with a stolen session
// converts it into a second factor they own.
func TestReEnrolmentOverAConfirmedFactorIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	f.enrol(t, user.ID)

	if _, err := f.svc.BeginTOTPEnrolment(context.Background(), user.ID); !errors.Is(err, ErrTOTPAlreadyEnrolled) {
		t.Fatalf("BeginTOTPEnrolment over a confirmed factor = %v, want ErrTOTPAlreadyEnrolled", err)
	}
}

// The replay guard. A code valid for 30 seconds that works twice is a weaker
// control than it looks.
func TestTOTPCodeCannotBeUsedTwiceInsideItsWindow(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	secret := f.enrol(t, user.ID)
	ctx := context.Background()

	f.clock.advance(TOTPPeriod)
	code, err := TOTPCodeAt(secret, f.clock.Now(), TOTPConfig{})
	if err != nil {
		t.Fatalf("TOTPCodeAt: %v", err)
	}

	req := LoginRequest{Email: "person@example.com", Password: NewSecret(testPassword), TOTPCode: code}
	if _, err := f.svc.Login(ctx, req); err != nil {
		t.Fatalf("first Login with the code = %v", err)
	}
	// Same code, same step, one second later — the shoulder-surfing case.
	f.clock.advance(time.Second)
	if _, err := f.svc.Login(ctx, req); !errors.Is(err, ErrSecondFactorInvalid) {
		t.Fatalf("second Login with the SAME code = %v, want ErrSecondFactorInvalid", err)
	}

	// A code from the next step still works, or a user could log in once per
	// account lifetime.
	f.clock.advance(TOTPPeriod)
	next, err := TOTPCodeAt(secret, f.clock.Now(), TOTPConfig{})
	if err != nil {
		t.Fatalf("TOTPCodeAt: %v", err)
	}
	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword), TOTPCode: next}); err != nil {
		t.Fatalf("Login with the next step's code = %v", err)
	}
}

// A misconfigured deployment must not become a 2FA bypass.
func TestAnEnrolledFactorWithNoKeyringRefusesTheLogin(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	f.enrol(t, user.ID)

	// The key goes missing, as it would if SHARPLINE_TOTP_ENCRYPTION_KEYS were
	// dropped from a deployment.
	f.svc.keyring = nil

	_, err := f.svc.Login(context.Background(), LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword), TOTPCode: "123456"})
	if err == nil {
		t.Fatal("an enrolled second factor with no keyring let the login through")
	}
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("Login = %v, want an ErrInternal; treating a missing key as \"no second factor\" "+
			"would be a bypass", err)
	}
}

func TestRecoveryCodeIsAcceptedOnceInPlaceOfTheSecondFactor(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	ctx := context.Background()

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
	codes, err := f.svc.ConfirmTOTPEnrolment(ctx, user.ID, code)
	if err != nil {
		t.Fatalf("ConfirmTOTPEnrolment: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d recovery codes, want %d", len(codes), RecoveryCodeCount)
	}

	f.clock.advance(TOTPPeriod)
	req := LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword), RecoveryCode: codes[0]}
	if _, err := f.svc.Login(ctx, req); err != nil {
		t.Fatalf("Login with a recovery code = %v", err)
	}
	// Single-use.
	f.clock.advance(TOTPPeriod)
	if _, err := f.svc.Login(ctx, req); !errors.Is(err, ErrSecondFactorInvalid) {
		t.Fatalf("second Login with the SAME recovery code = %v, want ErrSecondFactorInvalid", err)
	}
	// A different one still works.
	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword), RecoveryCode: codes[1]}); err != nil {
		t.Fatalf("Login with a second recovery code = %v", err)
	}

	status, err := f.svc.TOTPStatus(ctx, user.ID)
	if err != nil {
		t.Fatalf("TOTPStatus: %v", err)
	}
	if status.RecoveryCodesRemaining != RecoveryCodeCount-2 {
		t.Fatalf("recovery codes remaining = %d, want %d",
			status.RecoveryCodesRemaining, RecoveryCodeCount-2)
	}
}

func TestDisableTOTPRequiresThePasswordAndClearsRecoveryCodes(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	f.enrol(t, user.ID)
	ctx := context.Background()

	// An access token is not checked against the database, so a stolen one must
	// not be enough to strip a control the account's owner added.
	if err := f.svc.DisableTOTP(ctx, user.ID, NewSecret("wrong password here")); !errors.Is(err, ErrCredentials) {
		t.Fatalf("DisableTOTP with a wrong password = %v, want ErrCredentials", err)
	}
	if err := f.svc.DisableTOTP(ctx, user.ID, NewSecret(testPassword)); err != nil {
		t.Fatalf("DisableTOTP: %v", err)
	}

	status, err := f.svc.TOTPStatus(ctx, user.ID)
	if err != nil {
		t.Fatalf("TOTPStatus: %v", err)
	}
	if status.Enrolled || status.Pending {
		t.Fatalf("status = %+v after DisableTOTP", status)
	}
	// The bypass must go with the factor: a "disabled" second factor whose
	// recovery codes still work is worse than either state on its own, because
	// nothing in the UI would show it.
	if status.RecoveryCodesRemaining != 0 {
		t.Fatalf("%d recovery codes survived DisableTOTP", status.RecoveryCodesRemaining)
	}

	if _, err := f.svc.Login(ctx, LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword)}); err != nil {
		t.Fatalf("Login after DisableTOTP = %v", err)
	}
	if err := f.svc.DisableTOTP(ctx, user.ID, NewSecret(testPassword)); !errors.Is(err, ErrTOTPNotEnrolled) {
		t.Fatalf("DisableTOTP twice = %v, want ErrTOTPNotEnrolled", err)
	}
}

func TestRegenerateRecoveryCodesRequiresThePassword(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	user := f.register(t, "person@example.com")
	f.enrol(t, user.ID)
	ctx := context.Background()

	if _, err := f.svc.RegenerateRecoveryCodes(ctx, user.ID, NewSecret("wrong password here")); !errors.Is(err, ErrCredentials) {
		t.Fatalf("RegenerateRecoveryCodes with a wrong password = %v, want ErrCredentials", err)
	}

	before, err := f.recovery.UnusedRecoveryCodeDigests(ctx, user.ID)
	if err != nil {
		t.Fatalf("UnusedRecoveryCodeDigests: %v", err)
	}
	codes, err := f.svc.RegenerateRecoveryCodes(ctx, user.ID, NewSecret(testPassword))
	if err != nil {
		t.Fatalf("RegenerateRecoveryCodes: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d codes, want %d", len(codes), RecoveryCodeCount)
	}
	// The old set is discarded, not appended to.
	after, err := f.recovery.UnusedRecoveryCodeDigests(ctx, user.ID)
	if err != nil {
		t.Fatalf("UnusedRecoveryCodeDigests: %v", err)
	}
	if len(after) != RecoveryCodeCount {
		t.Fatalf("%d codes after regeneration, want %d", len(after), RecoveryCodeCount)
	}
	for _, old := range before {
		for _, fresh := range after {
			if bytes.Equal(old, fresh) {
				t.Fatal("an old recovery-code digest survived regeneration; " +
					"the old set must be discarded, not appended to")
			}
		}
	}
	// And the newly-minted codes are the ones that are stored.
	if MatchRecoveryCode(after, codes[0]) < 0 {
		t.Error("a freshly minted recovery code is not in the stored set")
	}
}

// -----------------------------------------------------------------------------
// construction
// -----------------------------------------------------------------------------

func TestNewServiceValidatesItsOptions(t *testing.T) {
	t.Parallel()

	hasher, err := NewHasher(HasherOptions{Params: testParams})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	tokens, err := NewTokenIssuer(TokenIssuerOptions{SigningKey: testSigningKey})
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	base := func() Options {
		return Options{Store: newMemStore(), Hasher: hasher, Tokens: tokens}
	}

	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no store", func(o *Options) { o.Store = nil }},
		{"no hasher", func(o *Options) { o.Hasher = nil }},
		{"no token issuer", func(o *Options) { o.Tokens = nil }},
		{"negative refresh TTL", func(o *Options) { o.RefreshTTL = -time.Hour }},
		{"negative session lifetime", func(o *Options) { o.SessionLifetime = -time.Hour }},
		// A session that expires before its first refresh token does is a cap
		// doing nothing, and it means somebody set one and forgot the other.
		{"session lifetime under the refresh TTL", func(o *Options) {
			o.RefreshTTL = 48 * time.Hour
			o.SessionLifetime = time.Hour
		}},
		{"invalid TOTP config", func(o *Options) { o.TOTP = TOTPConfig{Digits: 4} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := base()
			c.mutate(&opts)
			if _, err := NewService(opts); !errors.Is(err, ErrInvalid) {
				t.Fatalf("NewService = %v, want ErrInvalid", err)
			}
		})
	}

	// Two services on one registry is a programming error and must fail at
	// startup rather than produce two services' numbers under one series.
	reg := prometheus.NewRegistry()
	first := base()
	first.Registry = reg
	if _, err := NewService(first); err != nil {
		t.Fatalf("NewService: %v", err)
	}
	second := base()
	second.Registry = reg
	if _, err := NewService(second); err == nil {
		t.Fatal("two services registered on one registry without an error")
	}
}

func TestServiceWithoutAKeyringStillServesPasswordLogins(t *testing.T) {
	t.Parallel()

	// A deployment that has not provisioned SHARPLINE_TOTP_ENCRYPTION_KEYS
	// should still be able to serve logins; only enrolment is refused.
	f := newFixture(t, func(o *Options) { o.Keyring = nil })
	user := f.register(t, "person@example.com")

	if _, err := f.svc.Login(context.Background(), LoginRequest{
		Email: "person@example.com", Password: NewSecret(testPassword)}); err != nil {
		t.Fatalf("Login without a keyring = %v", err)
	}
	if _, err := f.svc.BeginTOTPEnrolment(context.Background(), user.ID); !errors.Is(err, ErrInternal) {
		t.Fatalf("BeginTOTPEnrolment without a keyring = %v, want ErrInternal", err)
	}
}

// -----------------------------------------------------------------------------
// metrics
// -----------------------------------------------------------------------------

func TestLoginOutcomesAreCounted(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.register(t, "person@example.com")
	ctx := context.Background()

	f.login(t, "person@example.com")
	_, _ = f.svc.Login(ctx, LoginRequest{Email: "person@example.com", Password: NewSecret("wrong password here")})
	_, _ = f.svc.Login(ctx, LoginRequest{Email: "nobody@example.com", Password: NewSecret("wrong password here")})

	assertCounter(t, f.registry, "sharpline_auth_logins_total", map[string]string{"outcome": "ok"}, 1)
	assertCounter(t, f.registry, "sharpline_auth_logins_total", map[string]string{"outcome": "bad_credentials"}, 2)
	assertCounter(t, f.registry, "sharpline_auth_registrations_total", map[string]string{"outcome": "ok"}, 1)
}

// assertCounter reads one labelled counter out of a registry.
func assertCounter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string, want float64) {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if !labelsMatch(got, labels) {
				continue
			}
			if v := m.GetCounter().GetValue(); v != want {
				t.Fatalf("%s%v = %v, want %v", name, labels, v, want)
			}
			return
		}
	}
	t.Fatalf("no series %s with labels %v; known families: %s", name, labels, familyNames(families))
}

func labelsMatch(got, want map[string]string) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func familyNames(families []*dto.MetricFamily) string {
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
