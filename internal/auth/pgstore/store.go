package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// DBTX is the statement seam. It is satisfied by *pgxpool.Pool and by pgx.Tx,
// which is what lets [NewTx] build a store over a transaction the caller
// already holds — the shape internal/betting needs so that a self-exclusion
// check happens inside the transaction that writes the wager.
//
// It is the same interface sqlc generates for the rest of this repository, so a
// caller that wires one wires the other.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store implements internal/auth's persistence interfaces against Postgres.
type Store struct {
	// db runs single statements.
	db DBTX

	// tx runs multi-statement units. Nil when the store was built over a
	// transaction the CALLER owns — in that case the caller's transaction is
	// the unit, and this store must not open a nested one.
	tx txRunner
}

// txRunner is the transaction seam: internal/platform/postgres.DB's InTx.
//
// It is an interface rather than the concrete *postgres.DB so that a store
// built over a caller's pgx.Tx has something honest to hold — nil — and so that
// the compile error for "you tried to open a transaction inside a transaction"
// is impossible to write rather than merely documented.
type txRunner interface {
	InTx(ctx context.Context, fn postgres.TxFunc) error
}

// New builds a store over a pool. This is what cmd/api wires.
func New(db *postgres.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: pgstore needs a database", auth.ErrInvalid)
	}
	return &Store{db: db.Pool(), tx: db}, nil
}

// NewTx builds a store over a transaction the caller already owns.
//
// The returned store implements the single-statement methods —
// [Store.UserStatus] above all — and REFUSES the multi-statement ones, because
// opening a transaction inside somebody else's is either a no-op that silently
// widens their unit of work or a nested transaction Postgres does not have.
//
// The intended caller is internal/betting: it holds the transaction that is
// about to insert a wager, builds a store over it, and reads users.status under
// the same snapshot. That is the only placement of the self-exclusion check
// with no window — see auth.UserStatus.CanWager.
func NewTx(tx pgx.Tx) (*Store, error) {
	if tx == nil {
		return nil, fmt.Errorf("%w: pgstore needs a transaction", auth.ErrInvalid)
	}
	return &Store{db: tx}, nil
}

// errNoTransaction is what a multi-statement method returns on a store built by
// [NewTx].
var errNoTransaction = fmt.Errorf(
	"%w: this store was built over a caller-owned transaction and cannot open its own",
	auth.ErrInternal)

func (s *Store) inTx(ctx context.Context, fn postgres.TxFunc) error {
	if s.tx == nil {
		return errNoTransaction
	}
	return s.tx.InTx(ctx, fn)
}

// -----------------------------------------------------------------------------
// users
// -----------------------------------------------------------------------------

const insertUserSQL = `
INSERT INTO users (id, email, password_hash, password_changed_at, status)
VALUES ($1, $2, $3, $4, $5)`

// CreateUser implements auth.UserStore.
//
// A duplicate address is detected from the UNIQUE violation, not from a prior
// SELECT: two registrations of one address arriving together both pass a SELECT
// and only the constraint can decide.
func (s *Store) CreateUser(ctx context.Context, u auth.NewUserRecord) error {
	_, err := s.db.Exec(ctx, insertUserSQL,
		u.ID.String(), u.Email.String(), u.PasswordHash, u.PasswordChangedAt, u.Status.String())
	if err != nil {
		if isEmailConflict(err) {
			return auth.ErrEmailTaken
		}
		return fmt.Errorf("pgstore: insert user: %w", err)
	}
	return nil
}

// isEmailConflict distinguishes "that address is taken" from any other unique
// violation on `users`.
//
// The constraint name is inspected rather than assumed, because the table has
// two unique constraints — the primary key on id and the unique on email — and
// reporting a primary-key collision as ErrEmailTaken would tell a user their
// address is registered when what actually happened is a 128-bit id collision
// or, far more likely, a bug in id generation.
func isEmailConflict(err error) bool {
	if !postgres.IsUniqueViolation(err) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.Contains(pgErr.ConstraintName, "email")
	}
	return false
}

const selectUserByEmailSQL = `
SELECT id, email, password_hash, password_changed_at, status, created_at
FROM users
WHERE email = $1`

const selectUserByIDSQL = `
SELECT id, email, password_hash, password_changed_at, status, created_at
FROM users
WHERE id = $1`

// UserByEmail implements auth.UserStore. The address must already be
// normalised, which auth.Email guarantees by construction.
func (s *Store) UserByEmail(ctx context.Context, email auth.Email) (auth.UserRecord, bool, error) {
	return s.scanUser(ctx, selectUserByEmailSQL, email.String())
}

// UserByID implements auth.UserStore.
func (s *Store) UserByID(ctx context.Context, id domain.UserID) (auth.UserRecord, bool, error) {
	return s.scanUser(ctx, selectUserByIDSQL, id.String())
}

func (s *Store) scanUser(ctx context.Context, query string, arg string) (auth.UserRecord, bool, error) {
	var (
		rec       auth.UserRecord
		id, email string
		status    string
	)
	err := s.db.QueryRow(ctx, query, arg).Scan(
		&id, &email, &rec.PasswordHash, &rec.PasswordChangedAt, &status, &rec.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return auth.UserRecord{}, false, nil
	case err != nil:
		return auth.UserRecord{}, false, fmt.Errorf("pgstore: select user: %w", err)
	}

	// Values are re-validated on the way out rather than converted. The CHECK
	// constraints make an invalid row unstorable, so a failure here means the
	// schema and this build disagree — which is exactly the drift the enum
	// spellings in internal/auth/status.go exist to catch, and a loud error is
	// better than a zero-valued status that reads as "unknown" and gets
	// treated as "not active" somewhere far away.
	rec.ID, err = domain.NewUserID(id)
	if err != nil {
		return auth.UserRecord{}, false, fmt.Errorf("pgstore: stored user id: %w", err)
	}
	rec.Email, err = auth.NewEmail(email)
	if err != nil {
		return auth.UserRecord{}, false, fmt.Errorf("pgstore: stored email: %w", err)
	}
	rec.Status, err = auth.ParseUserStatus(status)
	if err != nil {
		return auth.UserRecord{}, false, fmt.Errorf("pgstore: stored user status: %w", err)
	}
	return rec, true, nil
}

const selectUserStatusSQL = `SELECT status FROM users WHERE id = $1`

// UserStatus implements auth.StatusReader.
//
// This is the method internal/betting calls, through a store built by [NewTx],
// inside the transaction that inserts a wager. That placement is what makes
// self-exclusion a real block rather than a hidden button — see
// auth.UserStatus.CanWager.
func (s *Store) UserStatus(ctx context.Context, id domain.UserID) (auth.UserStatus, bool, error) {
	var status string
	err := s.db.QueryRow(ctx, selectUserStatusSQL, id.String()).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return auth.UserStatusUnknown, false, nil
	case err != nil:
		return auth.UserStatusUnknown, false, fmt.Errorf("pgstore: select user status: %w", err)
	}
	parsed, err := auth.ParseUserStatus(status)
	if err != nil {
		return auth.UserStatusUnknown, false, fmt.Errorf("pgstore: stored user status: %w", err)
	}
	return parsed, true, nil
}

const setPasswordSQL = `
UPDATE users
SET password_hash = $2, password_changed_at = $3
WHERE id = $1`

// SetPassword implements auth.UserStore. Both columns move in one statement:
// see auth.UserStore.RehashPassword for why the two operations are separate
// methods.
func (s *Store) SetPassword(ctx context.Context, id domain.UserID, hash string, changedAt time.Time) error {
	tag, err := s.db.Exec(ctx, setPasswordSQL, id.String(), hash, changedAt)
	if err != nil {
		return fmt.Errorf("pgstore: update password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such user", auth.ErrInternal)
	}
	return nil
}

const rehashPasswordSQL = `
UPDATE users
SET password_hash = $2
WHERE id = $1`

// RehashPassword implements auth.UserStore. password_changed_at is deliberately
// untouched.
func (s *Store) RehashPassword(ctx context.Context, id domain.UserID, hash string) error {
	tag, err := s.db.Exec(ctx, rehashPasswordSQL, id.String(), hash)
	if err != nil {
		return fmt.Errorf("pgstore: rehash password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such user", auth.ErrInternal)
	}
	return nil
}

// -----------------------------------------------------------------------------
// sessions: refresh_token_families + refresh_tokens
// -----------------------------------------------------------------------------

const insertFamilySQL = `
INSERT INTO refresh_token_families (id, user_id, started_at)
VALUES ($1, $2, $3)`

const insertRootTokenSQL = `
INSERT INTO refresh_tokens (id, family_id, parent_id, token_hash, issued_at, expires_at)
VALUES ($1, $2, NULL, $3, $4, $5)`

// CreateSession implements auth.SessionStore.
//
// One transaction, because a family with no root token is a session nothing can
// refresh and a root with no family violates the foreign key. refresh_tokens_
// one_root additionally guarantees that a second root can never be added later.
func (s *Store) CreateSession(ctx context.Context, ns auth.NewSession) error {
	err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, insertFamilySQL,
			ns.FamilyID, ns.UserID.String(), ns.StartedAt); err != nil {
			return fmt.Errorf("insert family: %w", err)
		}
		if _, err := tx.Exec(ctx, insertRootTokenSQL,
			ns.TokenID, ns.FamilyID, ns.TokenHash, ns.TokenIssuedAt, ns.TokenExpiresAt); err != nil {
			return fmt.Errorf("insert root token: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("pgstore: create session: %w", err)
	}
	return nil
}

// selectPresentedTokenSQL loads the presented token with its family and owner,
// locking the two rows this transaction may update.
//
// FOR NO KEY UPDATE rather than FOR UPDATE: the weaker mode still excludes
// another writer, and it does not block the foreign-key checks that
// refresh_tokens' composite FK back onto (id, family_id) performs. The lock
// order is (token, family) in every path, so two concurrent rotations queue
// rather than deadlock.
//
// `users` is joined but NOT locked. Its two columns here are read-only inputs,
// and locking a user row on every refresh would serialise all of one user's
// sessions against each other and against their own password change.
const selectPresentedTokenSQL = `
SELECT t.id,
       t.expires_at,
       t.used_at,
       f.id,
       f.user_id,
       f.started_at,
       f.revoked_at,
       u.password_changed_at,
       u.status
FROM refresh_tokens t
JOIN refresh_token_families f ON f.id = t.family_id
JOIN users u ON u.id = f.user_id
WHERE t.token_hash = $1
FOR NO KEY UPDATE OF t, f`

// markTokenUsedSQL redeems a token, and its `used_at IS NULL` predicate is a
// second line of defence rather than decoration: between the SELECT above and
// this UPDATE no other transaction can have touched the row (it is locked), but
// the predicate means that if the lock were ever removed the statement would
// still refuse to re-redeem, reporting zero rows instead of silently moving a
// write-once column. refresh_tokens_append_only would also refuse it.
const markTokenUsedSQL = `
UPDATE refresh_tokens
SET used_at = $2
WHERE id = $1 AND used_at IS NULL`

const insertSuccessorSQL = `
INSERT INTO refresh_tokens (id, family_id, parent_id, token_hash, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)`

const revokeFamilySQL = `
UPDATE refresh_token_families
SET revoked_at = $2, revoked_reason = $3
WHERE id = $1 AND revoked_at IS NULL`

// Rotate implements auth.SessionStore.
//
// The algorithm is migrations/00005's, in its order, and the ordering is
// load-bearing: a revoked family must be reported as revoked even if the token
// inside it has also expired, because "your session was ended" and "your
// session timed out" are different facts and the audit trail should say which.
//
// # Where reuse is actually caught
//
// Two places, and only the second is reliable on its own.
//
//   - `used_at IS NOT NULL` on the loaded row. A read. It catches the ordinary
//     case: a token redeemed a minute ago, presented again.
//
//   - A unique violation on refresh_tokens_one_successor when the successor is
//     inserted. This catches the case the read cannot: two presentations of the
//     SAME unused token arriving together. Both pass the read; the row lock
//     serialises them so the second sees used_at set — but if the lock were
//     ever weakened, or the two landed on different snapshots, the partial
//     unique index is what still refuses a second child for one parent.
//
// migrations/00005 calls the index "THE structural half of reuse detection" and
// the reason is exactly this: the property survives a mistake in this file.
//
// # Why the revocation happens in a second transaction
//
// A unique violation aborts the transaction it occurred in, so the revocation
// cannot be issued on that connection state. Rather than have two revocation
// paths — inline for the read case, separate for the race case — both go
// through one: the attempt reports what needs revoking, the attempt's
// transaction ends, and a fresh transaction revokes.
//
// The consequence is that a crash between the two leaves the lineage live. That
// is convergent rather than lost: the token is still marked used, so the next
// presentation detects the reuse again and retries the revocation. And if the
// revocation itself fails, this method returns an ERROR rather than
// auth.RotateReuse — because auth.SessionStore's contract is that RotateReuse
// means the revocation is committed, and returning it without that would be a
// lie the caller acts on.
func (s *Store) Rotate(ctx context.Context, req auth.RotateRequest) (auth.SessionRecord, auth.RotateOutcome, error) {
	rec, outcome, revokeWith, err := s.rotateAttempt(ctx, req)
	if err != nil {
		return auth.SessionRecord{}, auth.RotateUnknown, err
	}
	if revokeWith == auth.RevokedReasonUnknown {
		return rec, outcome, nil
	}

	if err := s.RevokeFamily(ctx, rec.FamilyID, req.Now, revokeWith); err != nil {
		return auth.SessionRecord{}, auth.RotateUnknown, fmt.Errorf(
			"pgstore: revoke family after detecting %s: %w", outcome, err)
	}
	return rec, outcome, nil
}

// rotateAttempt is the transactional half. revokeWith is
// auth.RevokedReasonUnknown when nothing needs revoking.
func (s *Store) rotateAttempt(
	ctx context.Context, req auth.RotateRequest,
) (rec auth.SessionRecord, outcome auth.RotateOutcome, revokeWith auth.RevokedReason, err error) {
	txErr := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			tokenID           string
			expiresAt         time.Time
			usedAt            *time.Time
			familyID          string
			userID            string
			startedAt         time.Time
			revokedAt         *time.Time
			passwordChangedAt time.Time
			status            string
		)

		scanErr := tx.QueryRow(ctx, selectPresentedTokenSQL, req.PresentedHash).Scan(
			&tokenID, &expiresAt, &usedAt,
			&familyID, &userID, &startedAt, &revokedAt,
			&passwordChangedAt, &status,
		)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			outcome = auth.RotateNotFound
			return nil
		}
		if scanErr != nil {
			return fmt.Errorf("select presented token: %w", scanErr)
		}

		owner, idErr := domain.NewUserID(userID)
		if idErr != nil {
			return fmt.Errorf("stored family owner: %w", idErr)
		}
		parsedStatus, statusErr := auth.ParseUserStatus(status)
		if statusErr != nil {
			return fmt.Errorf("stored user status: %w", statusErr)
		}

		rec = auth.SessionRecord{
			UserID:     owner,
			FamilyID:   familyID,
			StartedAt:  startedAt,
			TokenID:    tokenID,
			UserStatus: parsedStatus,
		}

		switch {
		case revokedAt != nil:
			outcome = auth.RotateRevoked
			return nil

		case startedAt.Before(passwordChangedAt):
			// The lineage predates the current credential. migrations/00005
			// built password_changed_at precisely so this is a comparison
			// rather than a scan-and-bulk-update at password-change time.
			outcome = auth.RotateCredentialChange
			revokeWith = auth.RevokedReasonCredentialChange
			return nil

		case !expiresAt.After(req.Now):
			outcome = auth.RotateExpired
			return nil

		case usedAt != nil:
			outcome = auth.RotateReuse
			revokeWith = auth.RevokedReasonReuseDetected
			return nil
		}

		tag, execErr := tx.Exec(ctx, markTokenUsedSQL, tokenID, req.Now)
		if execErr != nil {
			return fmt.Errorf("mark token used: %w", execErr)
		}
		if tag.RowsAffected() == 0 {
			// Unreachable while the row lock holds; handled because the
			// alternative to handling it is inserting a successor for a token
			// that was redeemed by somebody else.
			outcome = auth.RotateReuse
			revokeWith = auth.RevokedReasonReuseDetected
			return errRollbackForReuse
		}

		expires := cappedExpiry(req, startedAt)
		if _, execErr := tx.Exec(ctx, insertSuccessorSQL,
			req.SuccessorID, familyID, tokenID, req.SuccessorHash, req.Now, expires,
		); execErr != nil {
			if postgres.IsUniqueViolation(execErr) {
				// A concurrent redemption already created this token's one
				// permitted successor. refresh_tokens_one_successor is what
				// says so, and this branch is the whole reason the index
				// exists.
				outcome = auth.RotateReuse
				revokeWith = auth.RevokedReasonReuseDetected
				return errRollbackForReuse
			}
			return fmt.Errorf("insert successor token: %w", execErr)
		}

		rec.ExpiresAt = expires
		outcome = auth.RotateOK
		return nil
	})

	// errRollbackForReuse is a control-flow signal, not a failure: it exists to
	// roll the transaction back without committing a half-rotation, and the
	// outcome it carries has already been recorded.
	if txErr != nil && !errors.Is(txErr, errRollbackForReuse) {
		return auth.SessionRecord{}, auth.RotateUnknown, auth.RevokedReasonUnknown,
			fmt.Errorf("pgstore: rotate refresh token: %w", txErr)
	}
	return rec, outcome, revokeWith, nil
}

// errRollbackForReuse aborts a rotation transaction after reuse has been
// detected mid-flight, so nothing partial commits.
var errRollbackForReuse = errors.New("pgstore: rolling back a rotation because the token was already redeemed")

// cappedExpiry applies auth.RotateRequest.SessionLifetime.
//
// The absolute session lifetime is enforced HERE, by lowering the successor's
// expiry, and not by revoking the family — because
// refresh_token_families_revoked_reason_defined admits no 'expired' reason and
// filing an age-out under 'operator' would be a lie in the audit trail. See
// auth.RotateRequest.SessionLifetime.
func cappedExpiry(req auth.RotateRequest, familyStarted time.Time) time.Time {
	if req.SessionLifetime <= 0 {
		return req.SuccessorExpiresAt
	}
	limit := familyStarted.Add(req.SessionLifetime)
	if limit.Before(req.SuccessorExpiresAt) {
		return limit
	}
	return req.SuccessorExpiresAt
}

// RevokeFamily implements auth.SessionStore.
//
// The `revoked_at IS NULL` predicate makes revocation idempotent AND preserves
// the first reason: a lineage revoked for reuse and then logged out must keep
// saying 'reuse_detected', because that is the row a security review reads.
func (s *Store) RevokeFamily(ctx context.Context, familyID string, at time.Time, reason auth.RevokedReason) error {
	if !reason.Valid() {
		return fmt.Errorf("%w: revoked reason %q", auth.ErrInvalid, reason)
	}
	if _, err := s.db.Exec(ctx, revokeFamilySQL, familyID, at, reason.String()); err != nil {
		return fmt.Errorf("pgstore: revoke family: %w", err)
	}
	return nil
}

const selectFamilyByTokenSQL = `
SELECT f.id, f.user_id, f.started_at
FROM refresh_tokens t
JOIN refresh_token_families f ON f.id = t.family_id
WHERE t.token_hash = $1
FOR NO KEY UPDATE OF f`

// RevokeByToken implements auth.SessionStore: logout.
//
// One transaction, so the family cannot be rotated out from under the
// revocation between the lookup and the update.
func (s *Store) RevokeByToken(
	ctx context.Context, tokenHash []byte, at time.Time, reason auth.RevokedReason,
) (auth.SessionRecord, bool, error) {
	if !reason.Valid() {
		return auth.SessionRecord{}, false, fmt.Errorf("%w: revoked reason %q", auth.ErrInvalid, reason)
	}

	var (
		rec   auth.SessionRecord
		found bool
	)
	err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var familyID, userID string
		var startedAt time.Time

		scanErr := tx.QueryRow(ctx, selectFamilyByTokenSQL, tokenHash).Scan(&familyID, &userID, &startedAt)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return fmt.Errorf("select family by token: %w", scanErr)
		}

		owner, idErr := domain.NewUserID(userID)
		if idErr != nil {
			return fmt.Errorf("stored family owner: %w", idErr)
		}
		rec = auth.SessionRecord{UserID: owner, FamilyID: familyID, StartedAt: startedAt}
		found = true

		if _, execErr := tx.Exec(ctx, revokeFamilySQL, familyID, at, reason.String()); execErr != nil {
			return fmt.Errorf("revoke family: %w", execErr)
		}
		return nil
	})
	if err != nil {
		return auth.SessionRecord{}, false, fmt.Errorf("pgstore: revoke by token: %w", err)
	}
	return rec, found, nil
}

const revokeUserSessionsSQL = `
UPDATE refresh_token_families
SET revoked_at = $2, revoked_reason = $3
WHERE user_id = $1 AND revoked_at IS NULL`

// RevokeUserSessions implements auth.SessionStore: log out everywhere.
func (s *Store) RevokeUserSessions(
	ctx context.Context, id domain.UserID, at time.Time, reason auth.RevokedReason,
) (int, error) {
	if !reason.Valid() {
		return 0, fmt.Errorf("%w: revoked reason %q", auth.ErrInvalid, reason)
	}
	tag, err := s.db.Exec(ctx, revokeUserSessionsSQL, id.String(), at, reason.String())
	if err != nil {
		return 0, fmt.Errorf("pgstore: revoke user sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// -----------------------------------------------------------------------------
// user_totp
// -----------------------------------------------------------------------------

const selectTOTPSQL = `
SELECT secret_ciphertext, secret_nonce, key_id, confirmed_at
FROM user_totp
WHERE user_id = $1`

// LoadTOTP implements auth.TOTPStore.
func (s *Store) LoadTOTP(ctx context.Context, id domain.UserID) (auth.TOTPRecord, bool, error) {
	var (
		rec         auth.TOTPRecord
		confirmedAt *time.Time
	)
	err := s.db.QueryRow(ctx, selectTOTPSQL, id.String()).Scan(
		&rec.Sealed.Ciphertext, &rec.Sealed.Nonce, &rec.Sealed.KeyID, &confirmedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return auth.TOTPRecord{}, false, nil
	case err != nil:
		return auth.TOTPRecord{}, false, fmt.Errorf("pgstore: select second factor: %w", err)
	}
	rec.UserID = id
	if confirmedAt != nil {
		rec.ConfirmedAt = *confirmedAt
	}
	return rec, true, nil
}

// upsertTOTPSQL replaces an UNCONFIRMED enrolment and refuses a confirmed one.
//
// The `WHERE user_totp.confirmed_at IS NULL` on the DO UPDATE is the control.
// Without it, an attacker holding a stolen session could re-enrol their own
// authenticator over a working second factor and convert a session they might
// lose into one they own. With it, replacing a live factor requires
// auth.Service.DisableTOTP, which requires the password.
//
// A conflict that the predicate rejects updates zero rows, which is how
// ErrTOTPAlreadyEnrolled is detected — INSERT ... ON CONFLICT reports the row
// count, and zero means the guard fired.
const upsertTOTPSQL = `
INSERT INTO user_totp (user_id, secret_ciphertext, secret_nonce, key_id, confirmed_at)
VALUES ($1, $2, $3, $4, NULL)
ON CONFLICT (user_id) DO UPDATE
SET secret_ciphertext = EXCLUDED.secret_ciphertext,
    secret_nonce      = EXCLUDED.secret_nonce,
    key_id            = EXCLUDED.key_id,
    confirmed_at      = NULL
WHERE user_totp.confirmed_at IS NULL`

// SaveTOTP implements auth.TOTPStore.
func (s *Store) SaveTOTP(ctx context.Context, rec auth.TOTPRecord) error {
	tag, err := s.db.Exec(ctx, upsertTOTPSQL,
		rec.UserID.String(), rec.Sealed.Ciphertext, rec.Sealed.Nonce, rec.Sealed.KeyID)
	if err != nil {
		return fmt.Errorf("pgstore: upsert second factor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return auth.ErrTOTPAlreadyEnrolled
	}
	return nil
}

const confirmTOTPSQL = `
UPDATE user_totp
SET confirmed_at = $2
WHERE user_id = $1 AND confirmed_at IS NULL`

// ConfirmTOTP implements auth.TOTPStore. The `confirmed_at IS NULL` predicate
// makes confirmation write-once, so a replayed confirmation cannot move the
// instant.
func (s *Store) ConfirmTOTP(ctx context.Context, id domain.UserID, at time.Time) error {
	tag, err := s.db.Exec(ctx, confirmTOTPSQL, id.String(), at)
	if err != nil {
		return fmt.Errorf("pgstore: confirm second factor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return auth.ErrTOTPNotEnrolled
	}
	return nil
}

const deleteTOTPSQL = `DELETE FROM user_totp WHERE user_id = $1`

// DeleteTOTP implements auth.TOTPStore.
func (s *Store) DeleteTOTP(ctx context.Context, id domain.UserID) error {
	if _, err := s.db.Exec(ctx, deleteTOTPSQL, id.String()); err != nil {
		return fmt.Errorf("pgstore: delete second factor: %w", err)
	}
	return nil
}
