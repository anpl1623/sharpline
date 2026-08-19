package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Session lifetime defaults.
const (
	// DefaultRefreshTTL is how long one refresh token lives before it must be
	// redeemed. It is the IDLE timeout of a session: a client that goes quiet
	// for longer than this comes back to a sign-in prompt.
	//
	// Seven days is chosen against the product: a sports-betting board is
	// something people open on match days, so a one-day idle timeout would log
	// out a weekly bettor every week, and a thirty-day one would leave a stolen
	// token useful for a month. A week is the interval at which the user's own
	// usage pattern refreshes the token naturally.
	DefaultRefreshTTL = 7 * 24 * time.Hour

	// DefaultSessionLifetime is the ABSOLUTE lifetime of a login lineage,
	// measured from refresh_token_families.started_at.
	//
	// Without it, rotation makes sessions immortal: each redemption issues a
	// successor with a fresh seven-day clock, so a client that refreshes weekly
	// never re-authenticates and a token stolen from that client is a permanent
	// credential. Thirty days is the point at which "prove you still know the
	// password" is reasonable to ask.
	//
	// It is enforced by capping each successor's expiry, not by revoking the
	// family — see [RotateRequest.SessionLifetime] for why the schema forces
	// that and why it is the better mechanism anyway.
	DefaultSessionLifetime = 30 * 24 * time.Hour
)

// Clock is the time seam. Every instant this package persists comes through
// one, so a test can make rotation, expiry and TOTP steps deterministic without
// sleeping.
//
// It exists for a second reason that is specific to this schema.
// migrations/00005 forbids any trigger from writing a domain instant, so
// issued_at, expires_at, used_at, started_at and revoked_at are all values the
// application computes and passes down. A redelivered event re-applies the same
// instant and produces the same row — which is only true if there is exactly
// one place the instant comes from.
type Clock func() time.Time

// Options configures [NewService]. Constructor injection, per CLAUDE.md §12;
// there is no package-level state anywhere in this package.
type Options struct {
	// Store is persistence. Required.
	Store Store

	// Hasher derives and verifies passwords. Required.
	Hasher *Hasher

	// Tokens mints and verifies access tokens. Required.
	Tokens *TokenIssuer

	// Keyring seals TOTP secrets at rest. Optional: without it, second-factor
	// enrolment is refused and password authentication is unaffected. That is a
	// deliberate degradation rather than a startup failure, because a
	// deployment that has not yet provisioned SHARPLINE_TOTP_ENCRYPTION_KEYS
	// should still be able to serve logins.
	Keyring *Keyring

	// ReplayGuard burns a TOTP step so a code cannot be used twice inside its
	// own window. Nil means [NewMemoryReplayGuard], which is correct for a
	// single replica and not for more — see [MemoryReplayGuard].
	ReplayGuard ReplayGuard

	// RecoveryCodes persists single-use recovery codes. Optional; nil means
	// none are minted and none are accepted. See [RecoveryCodeStore] for the
	// schema gap this is waiting on.
	RecoveryCodes RecoveryCodeStore

	// TOTP is the second-factor parameter set. The zero value is the RFC 6238
	// defaults.
	TOTP TOTPConfig

	// TOTPIssuer is the label an authenticator app shows. Empty means
	// [DefaultIssuer].
	TOTPIssuer string

	// RefreshTTL is one refresh token's lifetime. Zero means
	// [DefaultRefreshTTL].
	RefreshTTL time.Duration

	// SessionLifetime is a lineage's absolute lifetime. Zero means
	// [DefaultSessionLifetime].
	SessionLifetime time.Duration

	// Now is the clock. Nil means time.Now.
	Now Clock

	// Registry receives this package's Prometheus collectors. Nil registers
	// nothing and keeps the observe calls live.
	Registry prometheus.Registerer

	// Logger receives structured events. Nil means slog.Default().
	//
	// NOTHING this package logs is a credential. Every log call below carries
	// identifiers (user id, family id, token id, jti), outcomes and durations,
	// and the values that ARE secret are held in a [Secret] whose LogValue
	// redacts — so even a future `slog.Any("result", res)` is safe.
	Logger *slog.Logger
}

// Service is the authentication core: registration, login, session rotation,
// credential change, and the optional second factor.
//
// It holds no mutable state of its own. Everything is either injected at
// construction or lives in the store, which is what lets `api` run as several
// replicas behind a load balancer without session affinity (CLAUDE.md §9).
type Service struct {
	store    Store
	hasher   *Hasher
	tokens   *TokenIssuer
	keyring  *Keyring
	replay   ReplayGuard
	recovery RecoveryCodeStore

	totpCfg    TOTPConfig
	totpIssuer string

	refreshTTL      time.Duration
	sessionLifetime time.Duration

	now     Clock
	log     *slog.Logger
	metrics *metrics
}

// NewService validates the options and builds a Service.
func NewService(opts Options) (*Service, error) {
	switch {
	case opts.Store == nil:
		return nil, fmt.Errorf("%w: auth service needs a store", ErrInvalid)
	case opts.Hasher == nil:
		return nil, fmt.Errorf("%w: auth service needs a password hasher", ErrInvalid)
	case opts.Tokens == nil:
		return nil, fmt.Errorf("%w: auth service needs a token issuer", ErrInvalid)
	}

	totpCfg, err := opts.TOTP.normalise()
	if err != nil {
		return nil, err
	}

	refreshTTL := opts.RefreshTTL
	if refreshTTL == 0 {
		refreshTTL = DefaultRefreshTTL
	}
	if refreshTTL <= 0 {
		return nil, fmt.Errorf("%w: refresh TTL %s is not positive", ErrInvalid, refreshTTL)
	}

	sessionLifetime := opts.SessionLifetime
	if sessionLifetime == 0 {
		sessionLifetime = DefaultSessionLifetime
	}
	if sessionLifetime < 0 {
		return nil, fmt.Errorf("%w: session lifetime %s is negative", ErrInvalid, sessionLifetime)
	}
	// A session that expires before its first refresh token does is a session
	// whose absolute cap is doing nothing, and it means somebody set one of the
	// two and forgot the other.
	if sessionLifetime > 0 && sessionLifetime < refreshTTL {
		return nil, fmt.Errorf("%w: session lifetime %s is shorter than the refresh TTL %s",
			ErrInvalid, sessionLifetime, refreshTTL)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	replay := opts.ReplayGuard
	if replay == nil {
		// The guard gets THIS service's clock, not the wall clock. It compares
		// the absolute expiry this service computes against its own clock, so
		// two clocks means an entry that is swept the moment it is written —
		// and a replay guard that never holds anything fails open silently.
		replay = NewMemoryReplayGuard(now)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	issuer := opts.TOTPIssuer
	if issuer == "" {
		issuer = DefaultIssuer
	}

	m, err := newMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}

	return &Service{
		store:           opts.Store,
		hasher:          opts.Hasher,
		tokens:          opts.Tokens,
		keyring:         opts.Keyring,
		replay:          replay,
		recovery:        opts.RecoveryCodes,
		totpCfg:         totpCfg,
		totpIssuer:      issuer,
		refreshTTL:      refreshTTL,
		sessionLifetime: sessionLifetime,
		now:             now,
		log:             log.With(slog.String("component", "auth")),
		metrics:         m,
	}, nil
}

// AccessTokenTTL reports the access-token lifetime, so a handler can tell the
// client when to refresh without repeating the constant.
func (s *Service) AccessTokenTTL() time.Duration { return s.tokens.TTL() }

// Register creates an account.
//
// The password is hashed before anything is written, so a failure to hash never
// leaves a half-created user. The insert relies on the unique constraint on
// users.email to detect a duplicate: a SELECT-then-INSERT would race two
// concurrent registrations of one address and the loser would get a 500 instead
// of [ErrEmailTaken].
//
// # This is an enumeration oracle and it cannot not be
//
// A system that refuses duplicate addresses tells you which addresses are
// registered. [ErrEmailTaken] documents the mitigation, which is at the
// transport and is the handler's decision, not this function's: hard per-IP rate
// limiting, and optionally answering uniformly and sending mail instead.
func (s *Service) Register(ctx context.Context, rawEmail string, password Secret) (UserRecord, error) {
	email, err := NewEmail(rawEmail)
	if err != nil {
		s.metrics.registrations.WithLabelValues(registerInvalidInput).Inc()
		return UserRecord{}, emailErr(rawEmail, err)
	}
	if err := ValidatePassword(password); err != nil {
		s.metrics.registrations.WithLabelValues(registerInvalidInput).Inc()
		return UserRecord{}, err
	}

	hash, err := s.hashPassword(ctx, password)
	if err != nil {
		s.metrics.registrations.WithLabelValues(outcomeError).Inc()
		return UserRecord{}, err
	}

	id, err := NewUserID()
	if err != nil {
		s.metrics.registrations.WithLabelValues(outcomeError).Inc()
		return UserRecord{}, err
	}

	now := s.instant()
	rec := UserRecord{
		ID:                id,
		Email:             email,
		Status:            UserStatusActive,
		PasswordHash:      hash,
		PasswordChangedAt: now,
		CreatedAt:         now,
	}

	if err := s.store.CreateUser(ctx, NewUserRecord{
		ID:                rec.ID,
		Email:             rec.Email,
		PasswordHash:      rec.PasswordHash,
		PasswordChangedAt: rec.PasswordChangedAt,
		Status:            rec.Status,
	}); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			s.metrics.registrations.WithLabelValues(registerEmailTaken).Inc()
			return UserRecord{}, err
		}
		s.metrics.registrations.WithLabelValues(outcomeError).Inc()
		return UserRecord{}, fmt.Errorf("auth: create user: %w", err)
	}

	s.metrics.registrations.WithLabelValues(outcomeOK).Inc()
	// The email address is NOT logged. It is personal data and a registration
	// log is exactly the file that ends up in a shared index.
	s.log.Info("user registered", slog.String("user_id", rec.ID.String()))
	return rec, nil
}

// LoginRequest is one sign-in attempt.
type LoginRequest struct {
	// Email is the raw address as typed. Normalisation happens here, once — see
	// [NewEmail].
	Email string

	// Password is the presented password.
	Password Secret

	// TOTPCode is the second factor, when one is enrolled. Empty on the first
	// leg of a two-step flow, which returns [ErrSecondFactorRequired].
	TOTPCode string

	// RecoveryCode is an alternative to TOTPCode for a user who has lost their
	// device.
	//
	// It is a SEPARATE field rather than an overload of TOTPCode, and that is
	// not cosmetic: sniffing which one was presented from its shape would mean
	// a mistyped TOTP code could be silently checked against the recovery-code
	// set, burning attempts against a much more valuable credential and
	// muddying the rate limiter that protects it.
	RecoveryCode Secret
}

// Session is a minted session: an access token and the refresh token that
// rotates it.
//
// Both tokens are [Secret]s, so this struct cannot be logged or serialised into
// a leak. Rendering it into a response is one call site in internal/httpapi and
// requires two explicit Expose() calls, which is the review signal this design
// is built around.
type Session struct {
	// User is the authenticated account.
	User UserRecord

	// AccessToken is the signed JWT.
	AccessToken Secret
	// AccessClaims are its claims, already decoded, so the caller does not
	// re-parse a token it just minted.
	AccessClaims Claims

	// RefreshToken is the opaque rotation credential. It is returned exactly
	// once, at this moment; the server keeps only its SHA-256.
	RefreshToken Secret
	// RefreshExpiresAt is when the refresh token stops being redeemable, after
	// the absolute session cap has been applied.
	RefreshExpiresAt time.Time

	// SessionID is refresh_token_families.id — the `sid` claim, and the value
	// an audit-log row should carry.
	SessionID string
}

// Login authenticates a password (and second factor, if enrolled) and opens a
// session.
//
// # The timing contract
//
// This phase's brief: "a login against an unknown email and a login with a
// wrong password must be indistinguishable in response body, status code AND
// timing."
//
//   - Body and status: both return [ErrCredentials], one value, and the caller
//     renders one response.
//   - Timing: the unknown-email path calls [Hasher.VerifyDecoy], which performs
//     the same argon2id work against a hash minted at construction under the
//     same parameters, through the same concurrency limiter. argon2id dominates
//     a login by three orders of magnitude, so equalising it equalises the
//     request.
//
// A malformed address takes the decoy path too. It is not strictly necessary —
// a malformed address cannot be registered, so nothing is revealed — but a path
// that returns early is a path somebody later extends with a database lookup.
//
// # What is NOT equalised, and why that is acceptable
//
// A correct password against a suspended account returns
// [ErrAccountSuspended] and takes one hash. A correct password against an
// account with 2FA returns [ErrSecondFactorRequired]. Both distinguish accounts
// — from a caller who has ALREADY proved the password. An attacker who can do
// that has no use for an enumeration oracle.
func (s *Service) Login(ctx context.Context, req LoginRequest) (Session, error) {
	start := s.now()
	sess, outcome, err := s.login(ctx, req)
	s.metrics.logins.WithLabelValues(outcome).Inc()
	s.metrics.loginDuration.WithLabelValues(outcome).Observe(s.now().Sub(start).Seconds())
	return sess, err
}

// login is Login without the instrumentation, so every return path is one
// statement and none of them can forget to record an outcome.
func (s *Service) login(ctx context.Context, req LoginRequest) (Session, string, error) {
	// Length is bounded before any work, so an oversized body cannot buy CPU.
	// The MINIMUM is deliberately not applied: a stored password may predate a
	// raised minimum and refusing to check it would lock out a user whose
	// credential is fine.
	if req.Password.Len() > MaxPasswordLen {
		return Session{}, loginBadCredentials, ErrCredentials
	}

	email, err := NewEmail(req.Email)
	if err != nil {
		if decoyErr := s.hasher.VerifyDecoy(ctx, req.Password); decoyErr != nil {
			return Session{}, outcomeError, decoyErr
		}
		return Session{}, loginBadCredentials, ErrCredentials
	}

	user, found, err := s.store.UserByEmail(ctx, email)
	if err != nil {
		return Session{}, outcomeError, fmt.Errorf("auth: load user for login: %w", err)
	}
	if !found {
		if decoyErr := s.hasher.VerifyDecoy(ctx, req.Password); decoyErr != nil {
			return Session{}, outcomeError, decoyErr
		}
		return Session{}, loginBadCredentials, ErrCredentials
	}

	ok, err := s.verifyPassword(ctx, user.PasswordHash, req.Password)
	if err != nil {
		return Session{}, outcomeError, err
	}
	if !ok {
		return Session{}, loginBadCredentials, ErrCredentials
	}

	// Status is checked AFTER verification, so the refusal cannot be used to
	// discover that an account exists.
	if !user.Status.CanAuthenticate() {
		return Session{}, loginNotPermitted, statusError(user.Status)
	}

	if err := s.checkSecondFactor(ctx, user.ID, req); err != nil {
		switch {
		case errors.Is(err, ErrSecondFactorRequired):
			return Session{}, login2FARequired, err
		case errors.Is(err, ErrSecondFactorInvalid):
			return Session{}, login2FAInvalid, err
		default:
			return Session{}, outcomeError, err
		}
	}

	// Rehash on login. This is the ONE moment the plaintext is in hand and
	// verified, so it is the only moment a stronger hash can be derived without
	// asking the user to type their password again. It costs a second argon2id
	// hash on the success path only, and only until the active user base has
	// cycled through a parameter bump.
	s.maybeRehash(ctx, user, req.Password)

	sess, err := s.openSession(ctx, user)
	if err != nil {
		return Session{}, outcomeError, err
	}
	return sess, outcomeOK, nil
}

// checkSecondFactor enforces an enrolled TOTP, if there is one.
//
// A user with NO confirmed enrolment passes through, and a TOTP or recovery
// code presented by such a user is ignored rather than rejected: the second
// factor is optional (CLAUDE.md §6) and a client that sends an empty extra
// field must not fail.
func (s *Service) checkSecondFactor(ctx context.Context, id domain.UserID, req LoginRequest) error {
	rec, found, err := s.store.LoadTOTP(ctx, id)
	if err != nil {
		return fmt.Errorf("auth: load second factor: %w", err)
	}
	if !found || !rec.Confirmed() {
		return nil
	}

	if !req.RecoveryCode.IsZero() {
		return s.consumeRecoveryCode(ctx, id, req.RecoveryCode)
	}
	if req.TOTPCode == "" {
		return ErrSecondFactorRequired
	}
	return s.consumeTOTPCode(ctx, id, rec, req.TOTPCode)
}

// consumeTOTPCode validates a code and burns its step.
//
// The ORDER is the control. Validation alone answers "is this code arithmetically
// correct for some step in the window", which stays true for the whole 30
// seconds; burning the step is what makes the answer usable once. A code that
// validates but whose step is already burnt is reported as invalid, identically
// to a wrong code, because from the presenter's side there is nothing useful to
// distinguish and telling them "that code was already used" confirms it was the
// right one.
func (s *Service) consumeTOTPCode(ctx context.Context, id domain.UserID, rec TOTPRecord, code string) error {
	if s.keyring == nil {
		// An enrolment exists and there is no key to open it. Refusing the
		// login is the only safe answer: treating it as "no second factor"
		// would turn a misconfigured deployment into a 2FA bypass.
		s.metrics.secondFactor.WithLabelValues(factorTOTP, outcomeError).Inc()
		return fmt.Errorf("%w: a second factor is enrolled but no encryption keyring is configured", ErrInternal)
	}

	secret, err := s.keyring.Open(id, rec.Sealed)
	if err != nil {
		s.metrics.secondFactor.WithLabelValues(factorTOTP, outcomeError).Inc()
		return fmt.Errorf("auth: open second-factor secret: %w", err)
	}

	now := s.now()
	step, ok, err := ValidateTOTPCode(secret, code, now, s.totpCfg)
	if err != nil {
		s.metrics.secondFactor.WithLabelValues(factorTOTP, outcomeError).Inc()
		return err
	}
	if !ok {
		s.metrics.secondFactor.WithLabelValues(factorTOTP, login2FAInvalid).Inc()
		return ErrSecondFactorInvalid
	}

	// The entry must outlive every presentation that could match this step. The
	// far edge of the window is skew steps past now, so one extra step of
	// slack makes the guard's memory strictly longer than the code's usefulness.
	expiry := now.Add(time.Duration(s.totpCfg.SkewSteps+2) * s.totpCfg.Period)
	first, err := s.replay.Consume(ctx, id, step, expiry)
	if err != nil {
		s.metrics.secondFactor.WithLabelValues(factorTOTP, outcomeError).Inc()
		return fmt.Errorf("auth: burn second-factor step: %w", err)
	}
	if !first {
		s.metrics.secondFactor.WithLabelValues(factorTOTP, login2FAInvalid).Inc()
		s.log.Warn("second-factor code replayed inside its own step",
			slog.String("user_id", id.String()),
			slog.Int64("step", step),
		)
		return ErrSecondFactorInvalid
	}

	s.metrics.secondFactor.WithLabelValues(factorTOTP, outcomeOK).Inc()
	return nil
}

// consumeRecoveryCode matches a presented code against the unused set and burns
// it.
func (s *Service) consumeRecoveryCode(ctx context.Context, id domain.UserID, code Secret) error {
	if s.recovery == nil {
		s.metrics.secondFactor.WithLabelValues(factorRecovery, outcomeError).Inc()
		return ErrSecondFactorInvalid
	}

	digests, err := s.recovery.UnusedRecoveryCodeDigests(ctx, id)
	if err != nil {
		s.metrics.secondFactor.WithLabelValues(factorRecovery, outcomeError).Inc()
		return fmt.Errorf("auth: load recovery codes: %w", err)
	}

	idx := MatchRecoveryCode(digests, code)
	if idx < 0 {
		s.metrics.secondFactor.WithLabelValues(factorRecovery, login2FAInvalid).Inc()
		return ErrSecondFactorInvalid
	}

	// Consume through the store rather than trusting the match: two concurrent
	// presentations of one code both reach here, and only the store can decide
	// which of them used it.
	used, err := s.recovery.ConsumeRecoveryCode(ctx, id, digests[idx], s.instant())
	if err != nil {
		s.metrics.secondFactor.WithLabelValues(factorRecovery, outcomeError).Inc()
		return fmt.Errorf("auth: consume recovery code: %w", err)
	}
	if !used {
		s.metrics.secondFactor.WithLabelValues(factorRecovery, login2FAInvalid).Inc()
		return ErrSecondFactorInvalid
	}

	s.metrics.secondFactor.WithLabelValues(factorRecovery, outcomeOK).Inc()
	s.log.Warn("recovery code used in place of a second factor",
		slog.String("user_id", id.String()),
		slog.Int("remaining", len(digests)-1),
	)
	return nil
}

// openSession mints a family, its root refresh token and an access token.
func (s *Service) openSession(ctx context.Context, user UserRecord) (Session, error) {
	familyID, err := NewFamilyID()
	if err != nil {
		return Session{}, err
	}
	tokenID, err := NewTokenID()
	if err != nil {
		return Session{}, err
	}
	refresh, err := NewRefreshTokenSecret()
	if err != nil {
		return Session{}, err
	}

	now := s.instant()
	expires := s.cappedExpiry(now, now)

	if err := s.store.CreateSession(ctx, NewSession{
		FamilyID:       familyID,
		UserID:         user.ID,
		StartedAt:      now,
		TokenID:        tokenID,
		TokenHash:      HashToken(refresh),
		TokenIssuedAt:  now,
		TokenExpiresAt: expires,
	}); err != nil {
		return Session{}, fmt.Errorf("auth: create session: %w", err)
	}

	access, claims, err := s.tokens.Issue(user.ID, familyID)
	if err != nil {
		// The family is already written. It is left alone rather than
		// compensated: the refresh token was never returned, so nothing can
		// redeem it, and it expires on its own. A compensating delete would
		// need its own failure handling and would fight the RESTRICT foreign
		// keys that exist to keep a lineage's audit trail intact.
		return Session{}, err
	}

	s.log.Info("session opened",
		slog.String("user_id", user.ID.String()),
		slog.String("session_id", familyID),
		slog.String("token_id", tokenID),
		slog.String("jti", claims.ID),
	)

	return Session{
		User:             user,
		AccessToken:      access,
		AccessClaims:     claims,
		RefreshToken:     refresh,
		RefreshExpiresAt: expires,
		SessionID:        familyID,
	}, nil
}

// Refresh redeems a refresh token for a new access token and a new refresh
// token, and detects reuse.
//
// The successor's identity and secret are minted BEFORE the store is entered,
// so the store's job is purely the atomic state transition and the secret never
// exists inside a transaction the caller cannot see the outcome of. See
// [RotateRequest].
//
// Every failure path returns a value wrapping [ErrUnauthenticated] and the
// caller must render them identically. The distinction between them is for the
// `sharpline_auth_refresh_total{outcome}` series, and outcome="reuse" is the
// one worth alerting on.
func (s *Service) Refresh(ctx context.Context, presented Secret) (Session, error) {
	if presented.IsZero() {
		s.metrics.refreshes.WithLabelValues(RotateNotFound.String()).Inc()
		return Session{}, ErrTokenUnknown
	}

	successorID, err := NewTokenID()
	if err != nil {
		s.metrics.refreshes.WithLabelValues(outcomeError).Inc()
		return Session{}, err
	}
	successor, err := NewRefreshTokenSecret()
	if err != nil {
		s.metrics.refreshes.WithLabelValues(outcomeError).Inc()
		return Session{}, err
	}

	now := s.instant()
	rec, outcome, err := s.store.Rotate(ctx, RotateRequest{
		PresentedHash:      HashToken(presented),
		Now:                now,
		SuccessorID:        successorID,
		SuccessorHash:      HashToken(successor),
		SuccessorExpiresAt: now.Add(s.refreshTTL),
		SessionLifetime:    s.sessionLifetime,
	})
	if err != nil {
		s.metrics.refreshes.WithLabelValues(outcomeError).Inc()
		return Session{}, fmt.Errorf("auth: rotate refresh token: %w", err)
	}

	s.metrics.refreshes.WithLabelValues(outcome.String()).Inc()

	switch outcome {
	case RotateOK:
		// Fall through.
	case RotateNotFound:
		return Session{}, ErrTokenUnknown
	case RotateExpired:
		return Session{}, ErrTokenExpired
	case RotateRevoked:
		return Session{}, ErrSessionRevoked
	case RotateCredentialChange:
		s.metrics.revocations.WithLabelValues(RevokedReasonCredentialChange.String()).Inc()
		return Session{}, ErrSessionRevoked
	case RotateReuse:
		s.metrics.revocations.WithLabelValues(RevokedReasonReuseDetected.String()).Inc()
		// This is the security event of the phase. It is logged at Error, with
		// the identifiers needed to reconstruct the lineage from the database
		// and NOTHING that could be replayed.
		s.log.Error("refresh token reuse detected; session lineage revoked",
			slog.String("user_id", rec.UserID.String()),
			slog.String("session_id", rec.FamilyID),
			slog.String("token_id", rec.TokenID),
		)
		return Session{}, ErrTokenReuse
	case RotateUnknown:
		return Session{}, fmt.Errorf("%w: store reported no rotation outcome", ErrInternal)
	default:
		return Session{}, fmt.Errorf("%w: store reported rotation outcome %d", ErrInternal, outcome)
	}

	// The status came back from the SAME transaction as the rotation, so it is
	// not a second, later snapshot. A user suspended while holding a live
	// session loses it here rather than at their next login.
	if !rec.UserStatus.CanAuthenticate() {
		if err := s.store.RevokeFamily(ctx, rec.FamilyID, now, RevokedReasonOperator); err != nil {
			s.log.Error("revoking a session for a non-authenticable account failed",
				slog.String("user_id", rec.UserID.String()),
				slog.String("session_id", rec.FamilyID),
				slog.String("error", err.Error()),
			)
		} else {
			s.metrics.revocations.WithLabelValues(RevokedReasonOperator.String()).Inc()
		}
		return Session{}, statusError(rec.UserStatus)
	}

	user, found, err := s.store.UserByID(ctx, rec.UserID)
	if err != nil {
		return Session{}, fmt.Errorf("auth: load user after rotation: %w", err)
	}
	if !found {
		// A refresh token whose family references a user that is gone. The FKs
		// are RESTRICT so this should be unreachable; it is handled rather than
		// assumed away, because "unreachable" plus a nil dereference is worse
		// than a 401.
		return Session{}, ErrSessionRevoked
	}

	access, claims, err := s.tokens.Issue(user.ID, rec.FamilyID)
	if err != nil {
		return Session{}, err
	}

	return Session{
		User:             user,
		AccessToken:      access,
		AccessClaims:     claims,
		RefreshToken:     successor,
		RefreshExpiresAt: rec.ExpiresAt,
		SessionID:        rec.FamilyID,
	}, nil
}

// Logout ends the lineage a refresh token belongs to.
//
// It revokes the FAMILY rather than the single token, because rotation means
// the token the client holds is one link in a chain and killing only that link
// would leave the rest redeemable.
//
// Presenting an unknown token is NOT an error. Logout is idempotent by
// necessity: a client that retries after a network failure, or that logs out
// twice, must not be handed a 401 for successfully being logged out.
func (s *Service) Logout(ctx context.Context, presented Secret) error {
	if presented.IsZero() {
		return nil
	}

	rec, found, err := s.store.RevokeByToken(ctx, HashToken(presented), s.instant(), RevokedReasonLogout)
	if err != nil {
		return fmt.Errorf("auth: revoke session on logout: %w", err)
	}
	if !found {
		// An unknown token. Either it was never issued, it was already swept,
		// or the client is retrying a logout that already worked. All three
		// mean "you are logged out", which is what the caller asked for.
		return nil
	}

	s.metrics.revocations.WithLabelValues(RevokedReasonLogout.String()).Inc()
	s.log.Info("session closed",
		slog.String("user_id", rec.UserID.String()),
		slog.String("session_id", rec.FamilyID),
	)
	return nil
}

// LogoutEverywhere revokes every live lineage for a user and reports how many
// it ended.
func (s *Service) LogoutEverywhere(ctx context.Context, id domain.UserID, reason RevokedReason) (int, error) {
	if !reason.Valid() {
		return 0, fmt.Errorf("%w: revoked reason %d", ErrInvalid, uint8(reason))
	}
	n, err := s.store.RevokeUserSessions(ctx, id, s.instant(), reason)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke all sessions: %w", err)
	}
	s.metrics.revocations.WithLabelValues(reason.String()).Add(float64(n))
	s.log.Info("all sessions revoked",
		slog.String("user_id", id.String()),
		slog.String("reason", reason.String()),
		slog.Int("sessions", n),
	)
	return n, nil
}

// ChangePassword rewrites a credential and logs the user out everywhere.
//
// The current password is required even though the caller is already
// authenticated. Without it, a stolen access token — which is not checked
// against the database and survives a revocation for its remaining lifetime —
// would be enough to take the account permanently.
//
// The revocation is the second half and it matters as much as the first: a
// password change that leaves old sessions alive does not evict whoever the
// user is changing their password because of. It is belt AND braces here —
// [SessionStore.Rotate] independently refuses any family older than
// password_changed_at, so even a failed revocation leaves no usable session.
func (s *Service) ChangePassword(ctx context.Context, id domain.UserID, current, next Secret) error {
	user, found, err := s.store.UserByID(ctx, id)
	if err != nil {
		return fmt.Errorf("auth: load user for password change: %w", err)
	}
	if !found {
		return ErrCredentials
	}

	if current.Len() > MaxPasswordLen {
		return ErrCredentials
	}
	ok, err := s.verifyPassword(ctx, user.PasswordHash, current)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCredentials
	}

	if err := ValidatePassword(next); err != nil {
		return err
	}
	// Refuse a no-op. Accepting it would move password_changed_at and revoke
	// every session for a change that did not happen — a self-inflicted denial
	// of service dressed as a security event.
	same, err := s.verifyPassword(ctx, user.PasswordHash, next)
	if err != nil {
		return err
	}
	if same {
		return ErrPasswordUnchanged
	}

	hash, err := s.hashPassword(ctx, next)
	if err != nil {
		return err
	}

	now := s.instant()
	if err := s.store.SetPassword(ctx, id, hash, now); err != nil {
		return fmt.Errorf("auth: store new password: %w", err)
	}

	n, err := s.store.RevokeUserSessions(ctx, id, now, RevokedReasonCredentialChange)
	if err != nil {
		// The password IS changed. Reporting a failure here would invite the
		// caller to retry the whole operation with a `current` that no longer
		// works. It is logged loudly and not returned, because
		// password_changed_at already makes every one of those sessions
		// unrefreshable.
		s.log.Error("password changed but session revocation failed; "+
			"sessions remain unrefreshable via password_changed_at",
			slog.String("user_id", id.String()),
			slog.String("error", err.Error()),
		)
	} else {
		s.metrics.revocations.WithLabelValues(RevokedReasonCredentialChange.String()).Add(float64(n))
	}

	s.log.Info("password changed",
		slog.String("user_id", id.String()),
		slog.Int("sessions_revoked", n),
	)
	return nil
}

// VerifyAccessToken checks a presented access token.
//
// It performs NO database access, which is the whole trade a JWT makes: a
// request is authorised from a signature instead of a round trip, and the price
// is that a revoked session keeps working until its current access token
// expires. [DefaultAccessTTL] is that price, stated in minutes.
//
// The corollary is the one [UserStatus.CanWager] insists on: a decision that
// must reflect the CURRENT state of the account — self-exclusion above all —
// cannot be made from these claims.
func (s *Service) VerifyAccessToken(token Secret) (Claims, error) {
	claims, err := s.tokens.Verify(token)
	if err != nil {
		s.metrics.accessTokens.WithLabelValues(outcomeError).Inc()
		return Claims{}, err
	}
	s.metrics.accessTokens.WithLabelValues(outcomeOK).Inc()
	return claims, nil
}

// AuthorizeWagering reports whether a user may place a wager RIGHT NOW.
//
// # This is the self-exclusion enforcement point
//
// CLAUDE.md §6 asks for "responsible-gaming-style self-imposed limits", and
// this phase's brief adds that self_excluded "must genuinely block wagering,
// not merely hide a button". [UserStatus.CanWager] argues at length why the
// check cannot live in a JWT claim or in HTTP middleware over one; the short
// version is that both read a snapshot, and the minutes right after somebody
// self-excludes are exactly when a snapshot is wrong.
//
// So this function reads users.status fresh, every time. internal/betting must
// call it — or, better, construct a [StatusReader] over the pgx.Tx it is about
// to insert the wager on and read the row inside that transaction, which closes
// the remaining window between this call returning and the insert committing.
func (s *Service) AuthorizeWagering(ctx context.Context, id domain.UserID) error {
	status, found, err := s.store.UserStatus(ctx, id)
	if err != nil {
		return fmt.Errorf("auth: read status for wagering check: %w", err)
	}
	if !found {
		return fmt.Errorf("%w: no such account", ErrForbidden)
	}
	if status.CanWager() {
		return nil
	}
	return statusError(status)
}

// statusError maps a non-permitting status to its error.
func statusError(status UserStatus) error {
	switch status {
	case UserStatusSelfExcluded:
		return ErrSelfExcluded
	case UserStatusSuspended:
		return ErrAccountSuspended
	case UserStatusClosed:
		return ErrAccountClosed
	case UserStatusActive:
		// Reachable only from AuthorizeWagering's caller passing an active
		// status, which cannot happen because CanWager already returned. Kept
		// so the switch is exhaustive and a future status cannot fall through
		// to a permissive answer.
		return fmt.Errorf("%w: account is active", ErrForbidden)
	case UserStatusUnknown:
		return fmt.Errorf("%w: account status is unknown", ErrForbidden)
	default:
		return fmt.Errorf("%w: account status %d", ErrForbidden, uint8(status))
	}
}

// hashPassword derives a hash and records the cost.
func (s *Service) hashPassword(ctx context.Context, password Secret) (string, error) {
	start := s.now()
	hash, err := s.hasher.Hash(ctx, password)
	s.metrics.hashDuration.WithLabelValues(hashOpDerive).Observe(s.now().Sub(start).Seconds())
	return hash, err
}

// verifyPassword checks a hash and records the cost.
func (s *Service) verifyPassword(ctx context.Context, encoded string, password Secret) (bool, error) {
	start := s.now()
	ok, err := s.hasher.Verify(ctx, encoded, password)
	s.metrics.hashDuration.WithLabelValues(hashOpVerify).Observe(s.now().Sub(start).Seconds())
	if err != nil {
		return false, fmt.Errorf("auth: verify password: %w", err)
	}
	return ok, nil
}

// maybeRehash upgrades a stored hash whose parameters are weaker than policy.
//
// Every failure here is logged and swallowed. The user has ALREADY
// authenticated; failing their login because an optimisation did not work would
// convert a cost-parameter bump into an outage. The hash they have still
// verifies, and the next login tries again.
func (s *Service) maybeRehash(ctx context.Context, user UserRecord, password Secret) {
	needs, err := s.hasher.NeedsRehash(user.PasswordHash)
	if err != nil || !needs {
		if err != nil {
			s.log.Warn("could not evaluate stored hash parameters",
				slog.String("user_id", user.ID.String()),
				slog.String("error", err.Error()),
			)
		}
		return
	}

	hash, err := s.hashPassword(ctx, password)
	if err != nil {
		s.log.Warn("rehash on login failed",
			slog.String("user_id", user.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	// RehashPassword, not SetPassword: the credential has not changed, only its
	// encoding, so password_changed_at must not move and the user's other
	// sessions must not be revoked. See [UserStore.RehashPassword].
	if err := s.store.RehashPassword(ctx, user.ID, hash); err != nil {
		s.log.Warn("storing rehashed password failed",
			slog.String("user_id", user.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	s.metrics.rehashes.Inc()
	s.log.Info("password hash upgraded to current parameters",
		slog.String("user_id", user.ID.String()),
		slog.String("params", s.hasher.Params().String()),
	)
}

// cappedExpiry applies the absolute session lifetime to a token expiry.
func (s *Service) cappedExpiry(issued, familyStarted time.Time) time.Time {
	expires := issued.Add(s.refreshTTL)
	if s.sessionLifetime <= 0 {
		return expires
	}
	if limit := familyStarted.Add(s.sessionLifetime); limit.Before(expires) {
		return limit
	}
	return expires
}

// instant is the clock, normalised.
//
// UTC and microsecond truncation, because TIMESTAMPTZ stores microseconds:
// without the truncation a value written by Go and read back compares unequal
// to itself, and every test that asserts "the instant we passed is the instant
// stored" fails for a reason that has nothing to do with what it is testing.
func (s *Service) instant() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}
