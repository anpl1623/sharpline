package auth

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// The second-factor enrolment flow.
//
// CLAUDE.md §6 asks for "optional TOTP 2FA". Optional is why enrolment is two
// steps and why the first step does not arm anything:
//
//	Begin   -> mint a secret, seal it, store it UNCONFIRMED, hand back a URI
//	Confirm -> the user proves a code from it; only now is it a second factor
//
// migrations/00005 requires exactly this and says what happens without it:
// "An unconfirmed row must NOT be treated as an enrolled second factor --
// otherwise a mis-scan locks the account out." A user who scans a QR code into
// the wrong app, or closes the page before saving, must be able to walk away
// with their account still working.

// TOTPEnrolment is the one-time output of [Service.BeginTOTPEnrolment].
//
// Both secret-bearing fields are [Secret]s and both are returned EXACTLY ONCE.
// There is deliberately no "show me the QR code again" path: the URI contains
// the shared secret, so re-deriving it later would mean the server could hand
// out a permanent 2FA bypass to anyone holding a session. A user who lost the
// code re-enrols, which mints a new secret.
type TOTPEnrolment struct {
	// URI is the otpauth:// string an authenticator app scans.
	URI Secret

	// SecretBase32 is the same secret in the form a user types when their app
	// cannot scan. It is the same credential in a different encoding, and it is
	// wrapped for the same reason.
	SecretBase32 Secret
}

// TOTPStatus is the non-secret view of a user's second factor: what an account
// settings page may show and what an API may return.
//
// It carries no secret, no ciphertext, no nonce and no key id — migrations/
// 00005's fourth requirement is that user_totp "is never returned by any API
// handler in any shape", and this type is what makes honouring that easy.
type TOTPStatus struct {
	// Enrolled reports a CONFIRMED second factor.
	Enrolled bool
	// Pending reports an enrolment that has been started and not confirmed.
	Pending bool
	// RecoveryCodesRemaining is how many unused recovery codes are left, or -1
	// when recovery codes are not configured.
	RecoveryCodesRemaining int
}

// BeginTOTPEnrolment mints a secret and stores it unconfirmed.
//
// It refuses when a CONFIRMED enrolment already exists. That refusal is a real
// control: without it, an attacker holding a stolen session — but not the
// user's device — could re-enrol their own authenticator over the top and
// convert a session they might lose into a second factor they own. Replacing a
// working second factor requires removing it first, and
// [Service.DisableTOTP] requires the password.
//
// An UNCONFIRMED enrolment is replaced freely: it arms nothing, and a user who
// mis-scanned needs to be able to start again.
func (s *Service) BeginTOTPEnrolment(ctx context.Context, id domain.UserID) (TOTPEnrolment, error) {
	if s.keyring == nil {
		return TOTPEnrolment{}, fmt.Errorf(
			"%w: second-factor enrolment needs an encryption keyring", ErrInternal)
	}

	user, found, err := s.store.UserByID(ctx, id)
	if err != nil {
		return TOTPEnrolment{}, fmt.Errorf("auth: load user for enrolment: %w", err)
	}
	if !found {
		return TOTPEnrolment{}, fmt.Errorf("%w: no such account", ErrForbidden)
	}

	existing, hasExisting, err := s.store.LoadTOTP(ctx, id)
	if err != nil {
		return TOTPEnrolment{}, fmt.Errorf("auth: load second factor: %w", err)
	}
	if hasExisting && existing.Confirmed() {
		return TOTPEnrolment{}, ErrTOTPAlreadyEnrolled
	}

	secret, err := NewTOTPSecret()
	if err != nil {
		return TOTPEnrolment{}, err
	}

	sealed, err := s.keyring.Seal(id, secret)
	if err != nil {
		return TOTPEnrolment{}, fmt.Errorf("auth: seal second-factor secret: %w", err)
	}

	if err := s.store.SaveTOTP(ctx, TOTPRecord{UserID: id, Sealed: sealed}); err != nil {
		return TOTPEnrolment{}, fmt.Errorf("auth: store second-factor enrolment: %w", err)
	}

	// The account label is the user's email address, because that is what an
	// authenticator app shows and a user with two accounts needs to tell them
	// apart. It goes into a URI the user receives and never into a log.
	uri, err := ProvisioningURI(s.totpIssuer, user.Email.String(), secret, s.totpCfg)
	if err != nil {
		return TOTPEnrolment{}, err
	}

	s.log.Info("second-factor enrolment started",
		slog.String("user_id", id.String()),
		slog.String("key_id", sealed.KeyID),
	)

	return TOTPEnrolment{
		URI:          uri,
		SecretBase32: NewSecret(b32.EncodeToString(secret)),
	}, nil
}

// ConfirmTOTPEnrolment arms a pending enrolment once the user proves a code
// from it, and returns a fresh set of recovery codes.
//
// The code is burnt through the [ReplayGuard] on the way in, exactly as at
// login. Skipping that here would leave a window where the code the user typed
// to confirm is still usable for a login on another device.
//
// The recovery codes are returned ONCE. If no [RecoveryCodeStore] is
// configured, the enrolment still confirms and the slice is empty — that
// degradation is deliberate and documented on [RecoveryCodeStore], which is
// waiting on a table migrations/00005 does not have.
func (s *Service) ConfirmTOTPEnrolment(ctx context.Context, id domain.UserID, code string) ([]Secret, error) {
	if s.keyring == nil {
		return nil, fmt.Errorf("%w: second-factor enrolment needs an encryption keyring", ErrInternal)
	}

	rec, found, err := s.store.LoadTOTP(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("auth: load second factor: %w", err)
	}
	if !found {
		return nil, ErrTOTPNotEnrolled
	}
	if rec.Confirmed() {
		return nil, ErrTOTPAlreadyEnrolled
	}

	if err := s.consumeTOTPCode(ctx, id, rec, code); err != nil {
		return nil, err
	}

	now := s.instant()
	if err := s.store.ConfirmTOTP(ctx, id, now); err != nil {
		return nil, fmt.Errorf("auth: confirm second factor: %w", err)
	}

	codes, err := s.mintRecoveryCodes(ctx, id, now)
	if err != nil {
		// The second factor IS armed. Failing the call would leave the user
		// with a working 2FA they believe did not enrol, which is the single
		// worst outcome available here — they would not save recovery codes for
		// a factor they think is off, and then be locked out by it.
		s.log.Error("second factor confirmed but recovery codes could not be minted",
			slog.String("user_id", id.String()),
			slog.String("error", err.Error()),
		)
		return nil, nil
	}

	s.log.Info("second factor confirmed",
		slog.String("user_id", id.String()),
		slog.Int("recovery_codes", len(codes)),
	)
	return codes, nil
}

// RegenerateRecoveryCodes replaces a user's recovery codes, discarding the old
// set. The password is required: recovery codes are a second factor's bypass,
// so minting new ones from a stolen session alone would be the bypass.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, id domain.UserID, password Secret) ([]Secret, error) {
	if s.recovery == nil {
		return nil, fmt.Errorf("%w: recovery codes are not configured", ErrInternal)
	}
	if err := s.requirePassword(ctx, id, password); err != nil {
		return nil, err
	}

	rec, found, err := s.store.LoadTOTP(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("auth: load second factor: %w", err)
	}
	if !found || !rec.Confirmed() {
		return nil, ErrTOTPNotEnrolled
	}

	codes, err := s.mintRecoveryCodes(ctx, id, s.instant())
	if err != nil {
		return nil, err
	}
	s.log.Info("recovery codes regenerated",
		slog.String("user_id", id.String()),
		slog.Int("recovery_codes", len(codes)),
	)
	return codes, nil
}

// DisableTOTP removes a second factor.
//
// The password is required for the same reason [Service.ChangePassword]
// requires it: an access token is not checked against the database, so a stolen
// one must not be sufficient to strip a control the account's owner added.
//
// Recovery codes go with it. Leaving them would mean a "disabled" second factor
// whose bypass still works — which is worse than either state on its own,
// because nothing in the UI would show it.
func (s *Service) DisableTOTP(ctx context.Context, id domain.UserID, password Secret) error {
	if err := s.requirePassword(ctx, id, password); err != nil {
		return err
	}

	if _, found, err := s.store.LoadTOTP(ctx, id); err != nil {
		return fmt.Errorf("auth: load second factor: %w", err)
	} else if !found {
		return ErrTOTPNotEnrolled
	}

	if err := s.store.DeleteTOTP(ctx, id); err != nil {
		return fmt.Errorf("auth: remove second factor: %w", err)
	}

	if s.recovery != nil {
		if err := s.recovery.ReplaceRecoveryCodes(ctx, id, nil, s.instant()); err != nil {
			s.log.Error("second factor removed but recovery codes remain",
				slog.String("user_id", id.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	s.log.Warn("second factor removed",
		slog.String("user_id", id.String()),
	)
	return nil
}

// TOTPStatus reports the non-secret state of a user's second factor.
func (s *Service) TOTPStatus(ctx context.Context, id domain.UserID) (TOTPStatus, error) {
	rec, found, err := s.store.LoadTOTP(ctx, id)
	if err != nil {
		return TOTPStatus{}, fmt.Errorf("auth: load second factor: %w", err)
	}

	status := TOTPStatus{
		Enrolled:               found && rec.Confirmed(),
		Pending:                found && !rec.Confirmed(),
		RecoveryCodesRemaining: -1,
	}
	if s.recovery != nil {
		digests, err := s.recovery.UnusedRecoveryCodeDigests(ctx, id)
		if err != nil {
			return TOTPStatus{}, fmt.Errorf("auth: count recovery codes: %w", err)
		}
		status.RecoveryCodesRemaining = len(digests)
	}
	return status, nil
}

// mintRecoveryCodes generates a fresh set and replaces whatever was stored.
//
// It returns no codes and no error when recovery codes are not configured, so
// every caller can treat "none configured" and "none minted" alike rather than
// branching on a nil store.
//
// The codes are minted BEFORE the store call and returned only after it
// succeeds. Returning codes that were not persisted would hand a user a set
// that does not work, which they would discover at the worst possible moment.
func (s *Service) mintRecoveryCodes(ctx context.Context, id domain.UserID, at time.Time) ([]Secret, error) {
	if s.recovery == nil {
		return nil, nil
	}

	codes, digests, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, err
	}
	if err := s.recovery.ReplaceRecoveryCodes(ctx, id, digests, at); err != nil {
		return nil, fmt.Errorf("auth: store recovery codes: %w", err)
	}
	return codes, nil
}

// requirePassword verifies a user's password for a sensitive account change.
//
// It returns [ErrCredentials] for a missing user as well as a wrong password,
// so a caller cannot use it to probe for accounts — although at this point the
// caller already holds a valid access token for the id it is asking about, so
// there is nothing left to probe.
func (s *Service) requirePassword(ctx context.Context, id domain.UserID, password Secret) error {
	if password.Len() > MaxPasswordLen {
		return ErrCredentials
	}
	user, found, err := s.store.UserByID(ctx, id)
	if err != nil {
		return fmt.Errorf("auth: load user: %w", err)
	}
	if !found {
		return ErrCredentials
	}
	ok, err := s.verifyPassword(ctx, user.PasswordHash, password)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCredentials
	}
	return nil
}
