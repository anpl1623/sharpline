package auth

import (
	"context"
	"encoding/hex"
	"sync"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// memStore is an in-memory [Store] for the service tests.
//
// # What it is for, and what it is NOT for
//
// It exists so that service.go's RULES — the timing equality of the login
// paths, the order of the second-factor checks, what a password change revokes,
// which failures are swallowed and which are returned — can be tested without a
// container. Those rules are the interesting part of this package and they are
// independent of storage.
//
// It is emphatically NOT evidence that reuse detection works. This
// implementation is a mutex and a map, so its Rotate is atomic by construction;
// the real one leans on a partial unique index and a row lock, and the only
// honest test of THAT is pgstore's integration test against a real Postgres
// with the real constraints. CLAUDE.md §10 says as much: "no mocked databases".
//
// The division is deliberate: rules here, storage there, and neither test
// pretends to be the other.
type memStore struct {
	mu sync.Mutex

	users    map[domain.UserID]UserRecord
	byEmail  map[Email]domain.UserID
	families map[string]*memFamily
	tokens   map[string]*memToken // keyed by hex(token hash)
	totp     map[domain.UserID]TOTPRecord

	// failNext, when set, makes the named method return this error once. It is
	// how the tests reach the "the store failed" branches, which are the ones a
	// handler must not mistake for a bad password.
	failNext map[string]error
}

type memFamily struct {
	id            string
	userID        domain.UserID
	startedAt     time.Time
	revokedAt     time.Time
	revokedReason RevokedReason
}

type memToken struct {
	id        string
	familyID  string
	parentID  string
	issuedAt  time.Time
	expiresAt time.Time
	usedAt    time.Time
}

func newMemStore() *memStore {
	return &memStore{
		users:    make(map[domain.UserID]UserRecord),
		byEmail:  make(map[Email]domain.UserID),
		families: make(map[string]*memFamily),
		tokens:   make(map[string]*memToken),
		totp:     make(map[domain.UserID]TOTPRecord),
		failNext: make(map[string]error),
	}
}

func (s *memStore) fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = err
}

// take reports and clears a queued failure. Callers hold the lock.
func (s *memStore) take(method string) error {
	err := s.failNext[method]
	if err != nil {
		delete(s.failNext, method)
	}
	return err
}

func (s *memStore) CreateUser(_ context.Context, u NewUserRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("CreateUser"); err != nil {
		return err
	}
	if _, taken := s.byEmail[u.Email]; taken {
		return ErrEmailTaken
	}
	s.users[u.ID] = UserRecord{
		ID:                u.ID,
		Email:             u.Email,
		Status:            u.Status,
		PasswordHash:      u.PasswordHash,
		PasswordChangedAt: u.PasswordChangedAt,
		CreatedAt:         u.PasswordChangedAt,
	}
	s.byEmail[u.Email] = u.ID
	return nil
}

func (s *memStore) UserByEmail(_ context.Context, email Email) (UserRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("UserByEmail"); err != nil {
		return UserRecord{}, false, err
	}
	id, ok := s.byEmail[email]
	if !ok {
		return UserRecord{}, false, nil
	}
	return s.users[id], true, nil
}

func (s *memStore) UserByID(_ context.Context, id domain.UserID) (UserRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("UserByID"); err != nil {
		return UserRecord{}, false, err
	}
	rec, ok := s.users[id]
	return rec, ok, nil
}

func (s *memStore) UserStatus(_ context.Context, id domain.UserID) (UserStatus, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("UserStatus"); err != nil {
		return UserStatusUnknown, false, err
	}
	rec, ok := s.users[id]
	if !ok {
		return UserStatusUnknown, false, nil
	}
	return rec.Status, true, nil
}

func (s *memStore) SetPassword(_ context.Context, id domain.UserID, hash string, changedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("SetPassword"); err != nil {
		return err
	}
	rec, ok := s.users[id]
	if !ok {
		return ErrInternal
	}
	rec.PasswordHash = hash
	rec.PasswordChangedAt = changedAt
	s.users[id] = rec
	return nil
}

func (s *memStore) RehashPassword(_ context.Context, id domain.UserID, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("RehashPassword"); err != nil {
		return err
	}
	rec, ok := s.users[id]
	if !ok {
		return ErrInternal
	}
	// password_changed_at deliberately untouched. If this line ever appears
	// here, TestRehashOnLoginDoesNotRevokeOtherSessions fails.
	rec.PasswordHash = hash
	s.users[id] = rec
	return nil
}

func (s *memStore) setStatus(id domain.UserID, status UserStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.users[id]
	rec.Status = status
	s.users[id] = rec
}

func (s *memStore) CreateSession(_ context.Context, ns NewSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("CreateSession"); err != nil {
		return err
	}
	s.families[ns.FamilyID] = &memFamily{
		id: ns.FamilyID, userID: ns.UserID, startedAt: ns.StartedAt,
	}
	s.tokens[hex.EncodeToString(ns.TokenHash)] = &memToken{
		id: ns.TokenID, familyID: ns.FamilyID,
		issuedAt: ns.TokenIssuedAt, expiresAt: ns.TokenExpiresAt,
	}
	return nil
}

// Rotate reproduces migrations/00005's decision tree in the same order as
// pgstore, so a service test and an integration test exercise the same branch
// structure. The ATOMICITY is free here and is exactly what the integration
// test is for.
func (s *memStore) Rotate(_ context.Context, req RotateRequest) (SessionRecord, RotateOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("Rotate"); err != nil {
		return SessionRecord{}, RotateUnknown, err
	}

	tok, ok := s.tokens[hex.EncodeToString(req.PresentedHash)]
	if !ok {
		return SessionRecord{}, RotateNotFound, nil
	}
	fam := s.families[tok.familyID]
	user := s.users[fam.userID]

	rec := SessionRecord{
		UserID:     fam.userID,
		FamilyID:   fam.id,
		StartedAt:  fam.startedAt,
		TokenID:    tok.id,
		UserStatus: user.Status,
	}

	switch {
	case !fam.revokedAt.IsZero():
		return rec, RotateRevoked, nil
	case fam.startedAt.Before(user.PasswordChangedAt):
		fam.revokedAt, fam.revokedReason = req.Now, RevokedReasonCredentialChange
		return rec, RotateCredentialChange, nil
	case !tok.expiresAt.After(req.Now):
		return rec, RotateExpired, nil
	case !tok.usedAt.IsZero():
		fam.revokedAt, fam.revokedReason = req.Now, RevokedReasonReuseDetected
		return rec, RotateReuse, nil
	}

	tok.usedAt = req.Now
	expires := req.SuccessorExpiresAt
	if req.SessionLifetime > 0 {
		if limit := fam.startedAt.Add(req.SessionLifetime); limit.Before(expires) {
			expires = limit
		}
	}
	s.tokens[hex.EncodeToString(req.SuccessorHash)] = &memToken{
		id: req.SuccessorID, familyID: fam.id, parentID: tok.id,
		issuedAt: req.Now, expiresAt: expires,
	}
	rec.ExpiresAt = expires
	return rec, RotateOK, nil
}

func (s *memStore) RevokeFamily(_ context.Context, familyID string, at time.Time, reason RevokedReason) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("RevokeFamily"); err != nil {
		return err
	}
	fam, ok := s.families[familyID]
	if !ok || !fam.revokedAt.IsZero() {
		// Idempotent, and the FIRST reason survives.
		return nil
	}
	fam.revokedAt, fam.revokedReason = at, reason
	return nil
}

func (s *memStore) RevokeByToken(
	_ context.Context, tokenHash []byte, at time.Time, reason RevokedReason,
) (SessionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("RevokeByToken"); err != nil {
		return SessionRecord{}, false, err
	}
	tok, ok := s.tokens[hex.EncodeToString(tokenHash)]
	if !ok {
		return SessionRecord{}, false, nil
	}
	fam := s.families[tok.familyID]
	if fam.revokedAt.IsZero() {
		fam.revokedAt, fam.revokedReason = at, reason
	}
	return SessionRecord{UserID: fam.userID, FamilyID: fam.id, StartedAt: fam.startedAt}, true, nil
}

func (s *memStore) RevokeUserSessions(
	_ context.Context, id domain.UserID, at time.Time, reason RevokedReason,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("RevokeUserSessions"); err != nil {
		return 0, err
	}
	n := 0
	for _, fam := range s.families {
		if fam.userID == id && fam.revokedAt.IsZero() {
			fam.revokedAt, fam.revokedReason = at, reason
			n++
		}
	}
	return n, nil
}

func (s *memStore) LoadTOTP(_ context.Context, id domain.UserID) (TOTPRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("LoadTOTP"); err != nil {
		return TOTPRecord{}, false, err
	}
	rec, ok := s.totp[id]
	return rec, ok, nil
}

func (s *memStore) SaveTOTP(_ context.Context, rec TOTPRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("SaveTOTP"); err != nil {
		return err
	}
	// Mirrors upsertTOTPSQL's `WHERE user_totp.confirmed_at IS NULL`: an
	// unconfirmed enrolment is replaceable, a confirmed one is not.
	if existing, ok := s.totp[rec.UserID]; ok && existing.Confirmed() {
		return ErrTOTPAlreadyEnrolled
	}
	s.totp[rec.UserID] = rec
	return nil
}

func (s *memStore) ConfirmTOTP(_ context.Context, id domain.UserID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("ConfirmTOTP"); err != nil {
		return err
	}
	rec, ok := s.totp[id]
	if !ok || rec.Confirmed() {
		return ErrTOTPNotEnrolled
	}
	rec.ConfirmedAt = at
	s.totp[id] = rec
	return nil
}

func (s *memStore) DeleteTOTP(_ context.Context, id domain.UserID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take("DeleteTOTP"); err != nil {
		return err
	}
	delete(s.totp, id)
	return nil
}

// familyReason reports how a family was revoked, for assertions.
func (s *memStore) familyReason(familyID string) (RevokedReason, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fam, ok := s.families[familyID]
	if !ok || fam.revokedAt.IsZero() {
		return RevokedReasonUnknown, false
	}
	return fam.revokedReason, true
}

// memRecoveryStore is an in-memory [RecoveryCodeStore].
//
// It exists because there is NO table for recovery codes — migrations/00005
// does not have one and this phase's auth work does not own migrations. So the
// crypto and the service wiring are tested against this, and the handoff notes
// name the migration that has to land before there is anything to test in
// pgstore. See [RecoveryCodeStore] for the table shape it needs.
type memRecoveryStore struct {
	mu    sync.Mutex
	codes map[domain.UserID][]memRecoveryCode
}

type memRecoveryCode struct {
	digest []byte
	used   bool
}

func newMemRecoveryStore() *memRecoveryStore {
	return &memRecoveryStore{codes: make(map[domain.UserID][]memRecoveryCode)}
}

func (s *memRecoveryStore) ReplaceRecoveryCodes(
	_ context.Context, id domain.UserID, digests [][]byte, _ time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make([]memRecoveryCode, 0, len(digests))
	for _, d := range digests {
		set = append(set, memRecoveryCode{digest: d})
	}
	s.codes[id] = set
	return nil
}

func (s *memRecoveryStore) ConsumeRecoveryCode(
	_ context.Context, id domain.UserID, digest []byte, _ time.Time,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := hex.EncodeToString(digest)
	for i, c := range s.codes[id] {
		if hex.EncodeToString(c.digest) == want {
			if c.used {
				return false, nil
			}
			s.codes[id][i].used = true
			return true, nil
		}
	}
	return false, nil
}

func (s *memRecoveryStore) UnusedRecoveryCodeDigests(
	_ context.Context, id domain.UserID,
) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	for _, c := range s.codes[id] {
		if !c.used {
			out = append(out, c.digest)
		}
	}
	return out, nil
}

// Compile-time proof that the fakes satisfy the seams. Without these, a change
// to an interface would surface as a confusing failure inside a test helper
// rather than as a compile error here.
var (
	_ Store             = (*memStore)(nil)
	_ RecoveryCodeStore = (*memRecoveryStore)(nil)
)
