package auth

import (
	"context"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// The persistence seams.
//
// CLAUDE.md §12: "Interfaces are declared by the consumer, not the producer.
// Keep them small." These are the consumer's declarations. The pgx
// implementation lives in internal/auth/pgstore and satisfies them; a test
// satisfies them with a map. Nothing in this file imports a database driver,
// which is what makes every rule in service.go testable without a container.
//
// # Why lookups return (record, found, error) instead of a sentinel
//
// A `ErrNoSuchUser` sentinel would immediately be the thing [Service.Login]
// must not leak: the whole design of the login path is that "no such user" and
// "wrong password" are one outcome, and an error value that says which is a
// value somebody will eventually map to a distinct status code. A bool cannot
// be mistaken for an error, so the merge happens at the only place that has the
// information and the code reads as the deliberate thing it is.

// UserRecord is one row of `users`, minus nothing — the table holds identity
// and credentials only. There is no balance here (CLAUDE.md §4: balances are a
// fold over ledger_entries), no KYC field and no geolocation, per CLAUDE.md §0
// and the standing list in migrations/00005.
type UserRecord struct {
	ID        domain.UserID
	Email     Email
	Status    UserStatus
	CreatedAt time.Time

	// PasswordHash is the full PHC-format argon2id string.
	// users_password_hash_is_argon2id makes anything else unstorable.
	//
	// It is a plain string rather than a [Secret]. A [Secret] guards values
	// that are useful to an attacker who reads one log line; a hash is not one
	// — that is the entire premise of hashing — and wrapping it would force an
	// Expose() call on the hot verification path, which devalues the Expose()
	// call as a review signal everywhere else.
	PasswordHash string

	// PasswordChangedAt is when the credential last changed. Every refresh
	// family started before this instant is invalid by definition, which is
	// what makes "log out everywhere on password change" a comparison rather
	// than a scan (migrations/00005).
	PasswordChangedAt time.Time
}

// NewUserRecord is the insert shape for `users`.
type NewUserRecord struct {
	ID                domain.UserID
	Email             Email
	PasswordHash      string
	PasswordChangedAt time.Time
	Status            UserStatus
}

// UserStore is what this package needs from the `users` table.
type UserStore interface {
	// CreateUser inserts a user. It MUST return [ErrEmailTaken] when the
	// address is already registered — the unique violation on users.email is
	// the only correct way to detect that, because a SELECT-then-INSERT races
	// with a concurrent registration of the same address.
	CreateUser(ctx context.Context, u NewUserRecord) error

	// UserByEmail loads by NORMALISED address. found is false when no row
	// matches; err is reserved for a store failure.
	UserByEmail(ctx context.Context, email Email) (rec UserRecord, found bool, err error)

	// UserByID loads by identifier.
	UserByID(ctx context.Context, id domain.UserID) (rec UserRecord, found bool, err error)

	// SetPassword rewrites password_hash and password_changed_at together. The
	// two must move atomically: a hash updated without the instant leaves every
	// pre-change session alive, and an instant updated without the hash locks
	// the user out of a credential that still works.
	SetPassword(ctx context.Context, id domain.UserID, hash string, changedAt time.Time) error

	// RehashPassword replaces password_hash and DOES NOT touch
	// password_changed_at.
	//
	// The two methods exist separately because conflating them is a
	// self-inflicted outage. Rehash-on-login re-derives the stored hash under
	// stronger parameters using the plaintext the user just proved — the
	// credential has not changed, only its encoding. If that wrote
	// password_changed_at, then the first login after a parameter bump would
	// revoke every one of that user's other sessions, on every device, for a
	// change they did not make. Multiply by "every user, over the days after a
	// deploy" and a cost bump becomes a mass logout event.
	//
	// password_changed_at means "the secret the user knows is different now".
	// Only [UserStore.SetPassword] may move it.
	RehashPassword(ctx context.Context, id domain.UserID, hash string) error
}

// StatusReader reads a user's account status.
//
// It is a separate, one-method interface from [UserStore] on purpose. The
// self-exclusion check has to happen inside the transaction that writes a wager
// (see [UserStatus.CanWager]), so internal/betting needs to construct a reader
// over a pgx.Tx it already holds — and it should not have to satisfy, or even
// see, the credential-handling half of UserStore to do it.
type StatusReader interface {
	UserStatus(ctx context.Context, id domain.UserID) (status UserStatus, found bool, err error)
}

// SessionRecord is a refresh-token family joined to the token presented from
// it. It is what [SessionStore.Rotate] reports back.
type SessionRecord struct {
	// UserID owns the session.
	UserID domain.UserID
	// FamilyID is refresh_token_families.id, and the `sid` claim on every
	// access token minted under it.
	FamilyID string
	// StartedAt is when the user actually authenticated. The absolute session
	// lifetime is measured from here, not from the current token's issue time —
	// otherwise a lineage rotated every ten minutes lives forever.
	StartedAt time.Time
	// TokenID is refresh_tokens.id for the presented token. Safe to log.
	TokenID string
	// ExpiresAt is when the newly-minted successor expires. Zero unless the
	// outcome was [RotateOK].
	ExpiresAt time.Time

	// UserStatus is the owner's status, read in the SAME transaction as the
	// rotation.
	//
	// It is here rather than fetched by a second call because the transaction
	// already joins `users` for password_changed_at, so it costs nothing — and
	// because a status read after the rotation committed would be a snapshot
	// taken at a different instant than the decision that used it. A user
	// suspended between the two would get a fresh access token from a
	// suspension that was already in force.
	UserStatus UserStatus
}

// NewSession is the insert shape for a login: one family plus its root token,
// which must be written in ONE transaction. A family with no root is a session
// nothing can refresh; a root with no family violates the foreign key.
type NewSession struct {
	FamilyID  string
	UserID    domain.UserID
	StartedAt time.Time

	TokenID        string
	TokenHash      []byte
	TokenIssuedAt  time.Time
	TokenExpiresAt time.Time
}

// RotateRequest asks the store to redeem a presented refresh token.
//
// The successor's identity and secret are minted by the CALLER, before the
// store is entered. That looks backwards and is deliberate: minting inside the
// transaction would mean the secret exists only in a scope the caller cannot
// reach, and passing a callback into the store would put application logic
// inside a transaction boundary the store owns. Here the store's job is purely
// the atomic state transition, and the caller keeps the one value that must
// never be persisted.
type RotateRequest struct {
	// PresentedHash is SHA-256 of the token the client sent. The token itself
	// never reaches the store.
	PresentedHash []byte

	// Now is the instant the rotation happens: used_at on the redeemed token
	// and issued_at on its successor. Supplied rather than read from the
	// database clock because migrations/00005 is emphatic that no trigger
	// writes a domain instant, and because a redelivered event must be able to
	// re-apply the same value.
	Now time.Time

	// SuccessorID and SuccessorHash identify and authenticate the new token.
	SuccessorID   string
	SuccessorHash []byte

	// SuccessorExpiresAt is the successor's expiry BEFORE the session cap is
	// applied. The store must lower it to StartedAt+SessionLifetime when that
	// is earlier — see [RotateRequest.SessionLifetime].
	SuccessorExpiresAt time.Time

	// SessionLifetime is the absolute lifetime of a login lineage, measured
	// from refresh_token_families.started_at.
	//
	// It is enforced by CAPPING the successor's expires_at rather than by
	// revoking the family, and the reason is a schema constraint rather than a
	// preference: refresh_token_families_revoked_reason_defined admits only
	// logout | reuse_detected | credential_change | operator. There is no
	// 'expired', so recording an age-out as a revocation would require either a
	// migration or a lie in the audit trail. Capping needs neither: the last
	// token of an over-age family simply expires, the normal expiry path
	// rejects it, and the expiry sweep collects it.
	//
	// Zero means no absolute cap.
	SessionLifetime time.Duration
}

// RotateOutcome is what happened to a presented refresh token. Every value maps
// to exactly one error in service.go, and all but the first map to a 401.
type RotateOutcome uint8

const (
	// RotateUnknown is the invalid zero value.
	RotateUnknown RotateOutcome = iota

	// RotateOK means the token was redeemed and its successor written.
	RotateOK

	// RotateNotFound means no row has that hash.
	RotateNotFound

	// RotateExpired means the token existed and its expiry had passed.
	RotateExpired

	// RotateRevoked means the family was already revoked.
	RotateRevoked

	// RotateReuse means an already-redeemed token was presented, or two
	// redemptions of one token raced and this one lost.
	//
	// THE STORE MUST HAVE COMMITTED THE FAMILY REVOCATION BEFORE RETURNING
	// THIS. The protection is the revocation, not the error; a caller that
	// ignores the return value must still end up with a dead lineage.
	RotateReuse

	// RotateCredentialChange means the family predates users.password_changed_at.
	// The store revokes it with reason 'credential_change' before returning.
	RotateCredentialChange
)

// String implements fmt.Stringer. These values are metric label values, so the
// spellings are a contract with the dashboard.
func (o RotateOutcome) String() string {
	switch o {
	case RotateOK:
		return "ok"
	case RotateNotFound:
		return "not_found"
	case RotateExpired:
		return "expired"
	case RotateRevoked:
		return "revoked"
	case RotateReuse:
		return "reuse"
	case RotateCredentialChange:
		return "credential_change"
	default:
		return "unknown"
	}
}

// SessionStore is what this package needs from refresh_token_families and
// refresh_tokens.
type SessionStore interface {
	// CreateSession writes a family and its root token in one transaction.
	CreateSession(ctx context.Context, s NewSession) error

	// Rotate performs the redeem-and-succeed transition atomically.
	//
	// The contract, which mirrors the algorithm migrations/00005 spells out:
	//
	//  1. Find the token by hash. Absent -> [RotateNotFound].
	//  2. Family revoked -> [RotateRevoked].
	//  3. Family older than the owner's password_changed_at ->
	//     revoke with 'credential_change', then [RotateCredentialChange].
	//  4. Token expired -> [RotateExpired].
	//  5. Token already used -> revoke the FAMILY with 'reuse_detected',
	//     commit that, then [RotateReuse].
	//  6. Otherwise set used_at and INSERT the successor with
	//     parent_id = the presented token, in ONE transaction. If that insert
	//     violates refresh_tokens_one_successor, a concurrent redemption won
	//     the race and this presentation is a reuse: roll back, revoke the
	//     family, and return [RotateReuse].
	//
	// Step 6's unique-violation branch is the part that cannot be skipped.
	// Step 5 is a READ, and on its own it is racy — two concurrent
	// presentations of the same unused token both pass it. The partial unique
	// index is what makes the second one fail, and honouring that failure is
	// what turns reuse detection from "detectable if the code remembers to
	// look" into a property of the database.
	Rotate(ctx context.Context, req RotateRequest) (SessionRecord, RotateOutcome, error)

	// RevokeFamily ends one lineage. Revoking an already-revoked family is a
	// no-op, not an error: logout is idempotent and a client that retries must
	// not get a 500.
	RevokeFamily(ctx context.Context, familyID string, at time.Time, reason RevokedReason) error

	// RevokeByToken finds the family a token belongs to and revokes it, in one
	// transaction. found is false when no row has that hash.
	//
	// This is logout, and it is a distinct method from [SessionStore.Rotate]
	// rather than "rotate and then revoke" because the composite is wrong in a
	// specific way: rotating first mints a successor whose secret is
	// immediately discarded, so a crash between the two leaves a live lineage
	// whose only redeemable token exists nowhere. A caller cannot recover from
	// that; one transaction cannot get into it.
	//
	// It deliberately does NOT resolve a token to a session for any other
	// purpose. An interface method that returned session metadata for an
	// arbitrary token hash would be a lookup oracle; this one only revokes.
	RevokeByToken(ctx context.Context, tokenHash []byte, at time.Time, reason RevokedReason) (rec SessionRecord, found bool, err error)

	// RevokeUserSessions ends every live lineage for a user and reports how
	// many it ended. This is "log out everywhere".
	RevokeUserSessions(ctx context.Context, id domain.UserID, at time.Time, reason RevokedReason) (int, error)
}

// TOTPRecord is one row of `user_totp`. CREDENTIAL MATERIAL: the secret is
// never in it — only its ciphertext, the nonce, and the id of the key that
// sealed it.
type TOTPRecord struct {
	UserID domain.UserID
	Sealed Sealed

	// ConfirmedAt is zero until the user proves a code from the secret.
	// migrations/00005: "An unconfirmed row must NOT be treated as an enrolled
	// second factor -- otherwise a mis-scan locks the account out."
	ConfirmedAt time.Time
}

// Confirmed reports whether this enrolment is a live second factor.
func (r TOTPRecord) Confirmed() bool { return !r.ConfirmedAt.IsZero() }

// TOTPStore is what this package needs from user_totp.
//
// Note what is NOT here: no "list all enrolments", no join against users. That
// is migrations/00005's fourth requirement on this phase — "This table is never
// included in a `SELECT *` join against users, is never returned by any API
// handler in any shape, and is redacted from logs" — expressed as an interface
// with no method that could serve such a query.
type TOTPStore interface {
	// LoadTOTP reads one enrolment, confirmed or not.
	LoadTOTP(ctx context.Context, id domain.UserID) (rec TOTPRecord, found bool, err error)

	// SaveTOTP writes an enrolment, replacing any UNCONFIRMED one. It must
	// refuse to overwrite a confirmed enrolment with [ErrTOTPAlreadyEnrolled] —
	// re-enrolling over a working second factor is how an attacker with a
	// session, but not the device, takes the second factor over.
	SaveTOTP(ctx context.Context, rec TOTPRecord) error

	// ConfirmTOTP stamps confirmed_at. Confirming an already-confirmed
	// enrolment must be refused, so a replayed confirmation cannot move the
	// instant.
	ConfirmTOTP(ctx context.Context, id domain.UserID, at time.Time) error

	// DeleteTOTP removes an enrolment. ON DELETE CASCADE on user_totp.user_id
	// means this is the only place a TOTP row is deliberately destroyed.
	DeleteTOTP(ctx context.Context, id domain.UserID) error
}

// RecoveryCodeStore persists single-use recovery codes as digests.
//
// # This interface has no implementation yet, and that is a schema gap
//
// migrations/00005 has no recovery-code table. It was not an oversight in that
// migration — the table it does have covers CLAUDE.md §6's "optional TOTP 2FA"
// exactly — but this phase's brief adds "Recovery codes: single-use, hashed at
// rest", and there is nowhere to put them.
//
// So the vocabulary and the pure crypto are here ([NewRecoveryCodes],
// [HashRecoveryCode], [MatchRecoveryCode], all tested), the seam is declared,
// and [Service] uses it when one is supplied and does without when it is not.
// The missing piece is a migration this phase's auth work does not own; the
// handoff names it. The shape it needs is small:
//
//	user_recovery_codes(
//	  user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
//	  code_hash BYTEA NOT NULL CHECK (octet_length(code_hash) = 32),
//	  used_at   TIMESTAMPTZ,
//	  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
//	  PRIMARY KEY (user_id, code_hash)
//	)
//
// with used_at write-once under the same append-only trigger pattern
// refresh_tokens uses, because a recovery code that can be un-used is not
// single-use.
type RecoveryCodeStore interface {
	// ReplaceRecoveryCodes discards any existing set and stores digests. It is
	// the only writer: a set is minted whole and replaced whole, so "how many
	// do I have left" is always answerable.
	ReplaceRecoveryCodes(ctx context.Context, id domain.UserID, digests [][]byte, at time.Time) error

	// ConsumeRecoveryCode marks the digest used and reports whether this call
	// was the one that used it. It MUST be atomic: two concurrent
	// presentations of one code must not both succeed.
	ConsumeRecoveryCode(ctx context.Context, id domain.UserID, digest []byte, at time.Time) (bool, error)

	// UnusedRecoveryCodeDigests lists the digests still available, so a
	// presented code can be matched in constant time with
	// [MatchRecoveryCode].
	UnusedRecoveryCodeDigests(ctx context.Context, id domain.UserID) ([][]byte, error)
}

// Store is the union an api binary wires up. It exists so that cmd/api passes
// one value; every function in this package takes the narrowest interface it
// actually uses.
type Store interface {
	UserStore
	StatusReader
	SessionStore
	TOTPStore
}
