// Integration tests for the Postgres auth store, against a REAL Postgres 17 +
// TimescaleDB spawned by testcontainers-go from inside the test container.
//
// CLAUDE.md §10 is the mandate: "Integration tests use testcontainers-go against
// real Postgres/Redis/Kafka — no mocked databases". For this package that is not
// a stylistic preference, it is the only way the interesting assertion can be
// made at all.
//
// # What only a real database can prove
//
// internal/auth's memStore has a Rotate that is atomic because it holds a mutex.
// That tells you the SERVICE's rules are right and tells you nothing about
// whether reuse detection works, because the real mechanism is not a mutex — it
// is `refresh_tokens_one_successor`, a partial unique index, plus a row lock.
// migrations/00005 calls the index "THE structural half of reuse detection" and
// the whole design rests on the claim that the DATABASE refuses a second
// successor even when the application's read said otherwise.
//
// TestConcurrentRedemptionsOfOneTokenProduceExactlyOneSuccessor is that claim,
// under real concurrency, against the real index. Nothing short of a real
// Postgres tests it.
//
// # Failure, not skip
//
// These tests FAIL when the docker socket is unreachable; they do not skip. A
// silently skipped integration test reports green while proving nothing, which
// is worse than no test at all — and the CI job that enforces the prime
// directive would become decorative.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/migrate"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// postgresImage is the compose stack's `postgres` image, pinned by digest, and
// copied from test/integration. It is the SAME image at the SAME digest as the
// one that will run in production: testing against a different engine than the
// one that will serve traffic defeats the point of using a real database.
const postgresImage = "timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6"

const (
	pgUser     = "sharpline_auth_it"
	pgPassword = "sharpline_auth_it_password"
	pgDatabase = "sharpline_auth_it"
)

// containerStartDeadline bounds one container boot including an image pull on a
// cold cache. TimescaleDB's entrypoint starts the server twice, which is why the
// log wait below asks for two occurrences.
const containerStartDeadline = 4 * time.Minute

var (
	sharedDB  *postgres.DB
	sharedErr error
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartDeadline+time.Minute)

	container, dsn, err := startPostgres(ctx)
	if err != nil {
		sharedErr = err
		cancel()
		os.Exit(runWithFailure(m))
	}

	if err := runMigrations(ctx, dsn); err != nil {
		sharedErr = fmt.Errorf("running migrations: %w", err)
	} else {
		sharedDB, sharedErr = postgres.Connect(ctx, postgres.Options{
			DSN:     dsn,
			Service: "auth-it",
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}

	code := m.Run()

	if sharedDB != nil {
		sharedDB.Close()
	}
	if container != nil {
		_ = container.Terminate(context.Background())
	}
	cancel()
	os.Exit(code)
}

// runWithFailure runs the suite anyway so every test reports the real cause
// rather than a nil-pointer panic twelve frames deep.
func runWithFailure(m *testing.M) int { return m.Run() }

// runMigrations applies the embedded migration set. The schema under test is
// the REAL one — migrations/00005 with every CHECK, trigger and partial unique
// index — because the properties this file asserts are properties of those
// constraints and of nothing else.
func runMigrations(ctx context.Context, dsn string) error {
	runner, err := migrate.New(migrate.Options{
		DSN:    dsn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return err
	}
	defer func() { _ = runner.Close() }()

	if _, err := runner.Run(ctx, migrate.Invocation{Command: migrate.CommandUp}); err != nil {
		return err
	}
	return nil
}

func startPostgres(ctx context.Context) (testcontainers.Container, string, error) {
	req := testcontainers.ContainerRequest{
		Image:        postgresImage,
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     pgUser,
			"POSTGRES_PASSWORD": pgPassword,
			"POSTGRES_DB":       pgDatabase,
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(containerStartDeadline),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("starting postgres: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		return container, "", fmt.Errorf("container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return container, "", fmt.Errorf("container port: %w", err)
	}

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, host, port.Port(), pgDatabase)
	return container, dsn, nil
}

// db returns the shared connection, failing loudly if the substrate is not up.
func db(t *testing.T) *postgres.DB {
	t.Helper()
	if sharedErr != nil {
		t.Fatalf("the integration substrate is not available: %v\n"+
			"These tests do not skip. `make test` mounts the docker socket so "+
			"testcontainers-go can spawn siblings; without it there is nothing to test against.",
			sharedErr)
	}
	return sharedDB
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(db(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// uniqueID mints an identifier unique to this process run, so every test owns
// its own key space and the suite is order-independent and safe under
// t.Parallel(). Nothing here reads a row another test wrote.
var idSeq atomic.Int64

func uniqueID(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano()%1_000_000, idSeq.Add(1))
}

// argon2idFixture is a shape-valid PHC string.
//
// users_password_hash_is_argon2id refuses anything that does not start with the
// argon2id prefix, so the fixture must satisfy the shape. It is NOT a hash of
// any real password and nothing here verifies against it — this package tests
// SQL, and the hashing is tested in internal/auth against the real hasher.
const argon2idFixture = "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2Fs$" +
	"ZGlnZXN0ZGlnZXN0ZGlnZXN0ZGlnZXN0ZGlnZXM"

// createUser inserts a user through the store under test.
func createUser(t *testing.T, s *Store, at time.Time) auth.UserRecord {
	t.Helper()

	id := domain.UserID(uniqueID("usr"))
	email, err := auth.NewEmail(strings.ToLower(string(id)) + "@example.com")
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}
	rec := auth.NewUserRecord{
		ID:                id,
		Email:             email,
		PasswordHash:      argon2idFixture,
		PasswordChangedAt: at,
		Status:            auth.UserStatusActive,
	}
	if err := s.CreateUser(context.Background(), rec); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return auth.UserRecord{
		ID: id, Email: email, Status: rec.Status,
		PasswordHash: rec.PasswordHash, PasswordChangedAt: rec.PasswordChangedAt,
	}
}

// openSession writes a family and its root token, and returns the root's hash.
func openSession(t *testing.T, s *Store, user domain.UserID, at time.Time, ttl time.Duration) (familyID string, rootHash []byte) {
	t.Helper()

	familyID = uniqueID("fam")
	rootHash = tokenHash(uniqueID("root"))
	err := s.CreateSession(context.Background(), auth.NewSession{
		FamilyID:       familyID,
		UserID:         user,
		StartedAt:      at,
		TokenID:        uniqueID("rt"),
		TokenHash:      rootHash,
		TokenIssuedAt:  at,
		TokenExpiresAt: at.Add(ttl),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return familyID, rootHash
}

// tokenHash builds a 32-byte digest from a label. refresh_tokens_hash_is_sha256
// CHECKs octet_length = 32, so anything else is unstorable.
func tokenHash(label string) []byte {
	return auth.HashToken(auth.NewSecret(label))
}

func rotateReq(presented []byte, now time.Time, ttl time.Duration) auth.RotateRequest {
	return auth.RotateRequest{
		PresentedHash:      presented,
		Now:                now,
		SuccessorID:        uniqueID("rt"),
		SuccessorHash:      tokenHash(uniqueID("succ")),
		SuccessorExpiresAt: now.Add(ttl),
	}
}

func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// -----------------------------------------------------------------------------
// users
// -----------------------------------------------------------------------------

func TestCreateUserAndLookups(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)

	got, found, err := s.UserByEmail(ctx, user.Email)
	if err != nil || !found {
		t.Fatalf("UserByEmail = %v, %v", found, err)
	}
	if got.ID != user.ID {
		t.Errorf("id = %q, want %q", got.ID, user.ID)
	}
	if got.Status != auth.UserStatusActive {
		t.Errorf("status = %s, want active", got.Status)
	}
	if !got.PasswordChangedAt.Equal(at) {
		t.Errorf("password_changed_at = %s, want %s; the application's instant must "+
			"survive the round trip unchanged (no trigger writes a domain instant)",
			got.PasswordChangedAt, at)
	}

	byID, found, err := s.UserByID(ctx, user.ID)
	if err != nil || !found {
		t.Fatalf("UserByID = %v, %v", found, err)
	}
	if byID.Email != user.Email {
		t.Errorf("email = %q, want %q", byID.Email, user.Email)
	}

	if _, found, err := s.UserByEmail(ctx, "nobody@example.com"); err != nil || found {
		t.Errorf("UserByEmail of an absent address = %v, %v; want false, nil", found, err)
	}
	if _, found, err := s.UserByID(ctx, "usr_absent"); err != nil || found {
		t.Errorf("UserByID of an absent id = %v, %v; want false, nil", found, err)
	}
}

// The duplicate must be detected from the CONSTRAINT, not from a prior SELECT:
// two registrations of one address arriving together both pass a SELECT.
func TestCreateUserReportsADuplicateEmailFromTheUniqueConstraint(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	user := createUser(t, s, nowUTC())

	err := s.CreateUser(ctx, auth.NewUserRecord{
		ID:                domain.UserID(uniqueID("usr")),
		Email:             user.Email,
		PasswordHash:      argon2idFixture,
		PasswordChangedAt: nowUTC(),
		Status:            auth.UserStatusActive,
	})
	if !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("CreateUser with a duplicate email = %v, want ErrEmailTaken", err)
	}

	// A primary-key collision is a DIFFERENT unique violation and must not be
	// reported as "that address is taken" — that would tell a user their
	// address is registered when the real problem is id generation.
	err = s.CreateUser(ctx, auth.NewUserRecord{
		ID:                user.ID,
		Email:             auth.Email("other-" + user.Email),
		PasswordHash:      argon2idFixture,
		PasswordChangedAt: nowUTC(),
		Status:            auth.UserStatusActive,
	})
	if err == nil {
		t.Fatal("CreateUser with a duplicate id succeeded")
	}
	if errors.Is(err, auth.ErrEmailTaken) {
		t.Fatal("a primary-key collision was reported as ErrEmailTaken")
	}
}

// The schema refuses anything that is not an argon2id PHC string. That CHECK is
// the last line of defence against a plaintext password reaching the column.
func TestCreateUserIsRefusedForANonArgon2idHash(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()

	for _, hash := range []string{
		"hunter2",
		"$2y$10$abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2Fs$ZGlnZXN0ZGlnZXN0ZGlnZXN0",
	} {
		err := s.CreateUser(ctx, auth.NewUserRecord{
			ID:                domain.UserID(uniqueID("usr")),
			Email:             auth.Email(uniqueID("e") + "@example.com"),
			PasswordHash:      hash,
			PasswordChangedAt: nowUTC(),
			Status:            auth.UserStatusActive,
		})
		if err == nil {
			t.Errorf("the database stored %q in users.password_hash", hash)
			continue
		}
		if !postgres.IsCheckViolation(err) {
			t.Errorf("CreateUser(%q) = %v, want a CHECK violation", hash, err)
		}
	}
}

// SetPassword and RehashPassword differ in exactly one column, and that
// difference is the whole reason they are two methods.
func TestSetPasswordMovesTheInstantAndRehashDoesNot(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)

	newHash := strings.Replace(argon2idFixture, "m=65536", "m=98304", 1)
	if err := s.RehashPassword(ctx, user.ID, newHash); err != nil {
		t.Fatalf("RehashPassword: %v", err)
	}
	got, _, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.PasswordHash != newHash {
		t.Error("RehashPassword did not replace the hash")
	}
	if !got.PasswordChangedAt.Equal(at) {
		t.Fatalf("RehashPassword moved password_changed_at from %s to %s; "+
			"a cost-parameter bump would log every active user out everywhere",
			at, got.PasswordChangedAt)
	}

	later := at.Add(time.Hour)
	if err := s.SetPassword(ctx, user.ID, argon2idFixture, later); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	got, _, err = s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !got.PasswordChangedAt.Equal(later) {
		t.Fatalf("password_changed_at = %s, want %s", got.PasswordChangedAt, later)
	}

	for _, err := range []error{
		s.SetPassword(ctx, "usr_absent", argon2idFixture, later),
		s.RehashPassword(ctx, "usr_absent", argon2idFixture),
	} {
		if !errors.Is(err, auth.ErrInternal) {
			t.Errorf("writing a password for an absent user = %v, want ErrInternal", err)
		}
	}
}

// UserStatus is the method internal/betting calls inside the transaction that
// writes a wager — the only placement of the self-exclusion check with no
// window.
func TestUserStatusIsReadableInsideACallersTransaction(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	user := createUser(t, s, nowUTC())

	err := db(t).InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// A caller's transaction. This is the shape internal/betting uses.
		txStore, err := NewTx(tx)
		if err != nil {
			return err
		}

		status, found, err := txStore.UserStatus(ctx, user.ID)
		if err != nil || !found {
			return fmt.Errorf("UserStatus = %v, %v", found, err)
		}
		if status != auth.UserStatusActive {
			return fmt.Errorf("status = %s, want active", status)
		}
		if !status.CanWager() {
			return errors.New("an active account cannot wager")
		}

		// Self-exclude inside the same transaction, then read again: the check
		// and the write see one snapshot, which is the property that closes the
		// window a JWT claim cannot.
		if _, err := tx.Exec(ctx,
			`UPDATE users SET status = 'self_excluded' WHERE id = $1`, user.ID.String()); err != nil {
			return err
		}
		status, _, err = txStore.UserStatus(ctx, user.ID)
		if err != nil {
			return err
		}
		if status.CanWager() {
			return errors.New("a self-excluded account may still wager")
		}

		// A store over a caller's transaction must REFUSE to open its own.
		if err := txStore.CreateSession(ctx, auth.NewSession{}); !errors.Is(err, auth.ErrInternal) {
			return fmt.Errorf("CreateSession on a tx-scoped store = %v, want ErrInternal", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	status, _, err := s.UserStatus(ctx, user.ID)
	if err != nil {
		t.Fatalf("UserStatus: %v", err)
	}
	if status != auth.UserStatusSelfExcluded {
		t.Fatalf("status = %s after commit, want self_excluded", status)
	}
}

// -----------------------------------------------------------------------------
// rotation and reuse detection
// -----------------------------------------------------------------------------

func TestRotateRedeemsAndSucceeds(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)
	familyID, root := openSession(t, s, user.ID, at, time.Hour)

	req := rotateReq(root, at.Add(time.Minute), time.Hour)
	rec, outcome, err := s.Rotate(ctx, req)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if outcome != auth.RotateOK {
		t.Fatalf("outcome = %s, want ok", outcome)
	}
	if rec.FamilyID != familyID {
		t.Errorf("family = %q, want %q", rec.FamilyID, familyID)
	}
	if rec.UserID != user.ID {
		t.Errorf("user = %q, want %q", rec.UserID, user.ID)
	}
	if rec.UserStatus != auth.UserStatusActive {
		t.Errorf("status = %s, want active; it must come from the rotation's own transaction",
			rec.UserStatus)
	}
	if !rec.ExpiresAt.Equal(req.SuccessorExpiresAt) {
		t.Errorf("successor expiry = %s, want %s", rec.ExpiresAt, req.SuccessorExpiresAt)
	}

	// The successor is redeemable and the parent is not.
	if _, outcome, err := s.Rotate(ctx, rotateReq(req.SuccessorHash, at.Add(2*time.Minute), time.Hour)); err != nil || outcome != auth.RotateOK {
		t.Fatalf("rotating the successor = %s, %v; want ok, nil", outcome, err)
	}
}

// THE test this package exists for, part one: the ordinary reuse case.
func TestRotateOfAnAlreadyRedeemedTokenRevokesTheWholeFamily(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)
	familyID, root := openSession(t, s, user.ID, at, time.Hour)

	first := rotateReq(root, at.Add(time.Minute), time.Hour)
	if _, outcome, err := s.Rotate(ctx, first); err != nil || outcome != auth.RotateOK {
		t.Fatalf("first Rotate = %s, %v", outcome, err)
	}

	// The thief presents the token the client already redeemed.
	_, outcome, err := s.Rotate(ctx, rotateReq(root, at.Add(2*time.Minute), time.Hour))
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if outcome != auth.RotateReuse {
		t.Fatalf("outcome = %s, want reuse", outcome)
	}

	// The contract is that the revocation is COMMITTED before RotateReuse is
	// returned — a caller that ignores the return value must still get the
	// protection.
	assertFamilyRevoked(t, familyID, "reuse_detected")

	// And the legitimate successor is dead too. That is the point: the thief
	// and the client cannot be told apart, so the lineage ends.
	if _, outcome, err := s.Rotate(ctx, rotateReq(first.SuccessorHash, at.Add(3*time.Minute), time.Hour)); err != nil || outcome != auth.RotateRevoked {
		t.Fatalf("rotating the legitimate successor after reuse = %s, %v; want revoked, nil", outcome, err)
	}
}

// THE test this package exists for, part two: the race the READ cannot catch.
//
// Two presentations of the SAME unused token, concurrently. Both pass step 5's
// `used_at IS NOT NULL` read — that read is racy on its own, which is exactly
// what migrations/00005 says. What stops both from minting a successor is
// `refresh_tokens_one_successor`, a partial unique index on parent_id, plus the
// FOR NO KEY UPDATE row lock.
//
// The assertion is on the DATABASE's state, not on the API's return values:
// exactly one child row for the parent, no matter how many callers tried.
func TestConcurrentRedemptionsOfOneTokenProduceExactlyOneSuccessor(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)
	familyID, root := openSession(t, s, user.ID, at, time.Hour)

	const racers = 8

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		outcomes = map[auth.RotateOutcome]int{}
		errs     []error
		start    = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, outcome, err := s.Rotate(ctx, rotateReq(root, at.Add(time.Minute), time.Hour))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			outcomes[outcome]++
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("Rotate returned an error: %v", err)
	}

	// EXACTLY ONE winner. Anything else means two clients hold live refresh
	// tokens for one lineage, which is the fork reuse detection exists to make
	// impossible.
	if outcomes[auth.RotateOK] != 1 {
		t.Fatalf("%d of %d concurrent redemptions succeeded, want exactly 1. Outcomes: %v",
			outcomes[auth.RotateOK], racers, outcomes)
	}
	// Every loser is refused, and refused as a COMPROMISE rather than as a
	// retryable failure. Two outcomes are legitimate here and the split between
	// them is timing, not correctness:
	//
	//   reuse    the racer reached the row while the family was still live and
	//            was the one to detect the second redemption.
	//   revoked  the racer reached the row AFTER an earlier loser had already
	//            committed the family revocation, so it saw a dead lineage.
	//
	// Both end with the caller holding nothing, which is the requirement. An
	// observed run of eight racers produced map[ok:1 revoked:1 reuse:6]; a
	// different scheduling produces map[ok:1 reuse:7]. Asserting the exact
	// split would be asserting the scheduler.
	//
	// What is NOT legitimate is any other outcome — an expired, a not_found or
	// an unknown here would mean the loser was told something retryable about a
	// lineage that has just been declared compromised.
	for outcome, n := range outcomes {
		switch outcome {
		case auth.RotateOK, auth.RotateReuse, auth.RotateRevoked:
		default:
			t.Errorf("%d racers got outcome %s; every loser must be refused as reuse or revoked",
				n, outcome)
		}
	}
	if got := outcomes[auth.RotateReuse] + outcomes[auth.RotateRevoked]; got != racers-1 {
		t.Fatalf("%d of %d losers were refused, want %d. Outcomes: %v", got, racers-1, racers-1, outcomes)
	}
	if outcomes[auth.RotateReuse] == 0 {
		t.Fatalf("no racer detected reuse at all. Outcomes: %v; "+
			"the family being revoked with no detection means the detection path was not taken",
			outcomes)
	}

	// The database's own account of it.
	assertFamilyRevoked(t, familyID, "reuse_detected")
	assertSuccessorCount(t, familyID, 1)
}

// The unique index is the mechanism, so it is worth asserting directly that the
// database refuses the second child — independently of anything this package
// does.
func TestTheDatabaseRefusesASecondSuccessorForOneToken(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)
	familyID, root := openSession(t, s, user.ID, at, time.Hour)

	var parentID string
	err := db(t).Pool().QueryRow(ctx,
		`SELECT id FROM refresh_tokens WHERE token_hash = $1`, root).Scan(&parentID)
	if err != nil {
		t.Fatalf("finding the root token: %v", err)
	}

	insert := func(id string) error {
		_, err := db(t).Pool().Exec(ctx,
			`INSERT INTO refresh_tokens (id, family_id, parent_id, token_hash, issued_at, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			id, familyID, parentID, tokenHash(id), at, at.Add(time.Hour))
		return err
	}

	if err := insert(uniqueID("rt")); err != nil {
		t.Fatalf("inserting the first successor: %v", err)
	}
	err = insert(uniqueID("rt"))
	if err == nil {
		t.Fatal("the database accepted a SECOND successor for one token; " +
			"refresh_tokens_one_successor is not doing its job and reuse detection is racy")
	}
	if !postgres.IsUniqueViolation(err) {
		t.Fatalf("second successor = %v, want a unique violation", err)
	}
}

func TestRotateOutcomes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("unknown token", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		_, outcome, err := s.Rotate(ctx, rotateReq(tokenHash(uniqueID("nope")), nowUTC(), time.Hour))
		if err != nil || outcome != auth.RotateNotFound {
			t.Fatalf("Rotate = %s, %v; want not_found, nil", outcome, err)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		at := nowUTC()
		user := createUser(t, s, at)
		_, root := openSession(t, s, user.ID, at, time.Minute)

		_, outcome, err := s.Rotate(ctx, rotateReq(root, at.Add(2*time.Minute), time.Hour))
		if err != nil || outcome != auth.RotateExpired {
			t.Fatalf("Rotate = %s, %v; want expired, nil", outcome, err)
		}
	})

	t.Run("revoked family", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		at := nowUTC()
		user := createUser(t, s, at)
		familyID, root := openSession(t, s, user.ID, at, time.Hour)

		if err := s.RevokeFamily(ctx, familyID, at, auth.RevokedReasonOperator); err != nil {
			t.Fatalf("RevokeFamily: %v", err)
		}
		_, outcome, err := s.Rotate(ctx, rotateReq(root, at.Add(time.Minute), time.Hour))
		if err != nil || outcome != auth.RotateRevoked {
			t.Fatalf("Rotate = %s, %v; want revoked, nil", outcome, err)
		}
	})

	// password_changed_at makes "log out everywhere on password change" a
	// comparison rather than a scan-and-bulk-update. This is that comparison.
	t.Run("family predating a password change", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		at := nowUTC()
		user := createUser(t, s, at)
		familyID, root := openSession(t, s, user.ID, at, time.Hour)

		if err := s.SetPassword(ctx, user.ID, argon2idFixture, at.Add(time.Minute)); err != nil {
			t.Fatalf("SetPassword: %v", err)
		}
		_, outcome, err := s.Rotate(ctx, rotateReq(root, at.Add(2*time.Minute), time.Hour))
		if err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		if outcome != auth.RotateCredentialChange {
			t.Fatalf("outcome = %s, want credential_change", outcome)
		}
		assertFamilyRevoked(t, familyID, "credential_change")
	})
}

// The absolute session lifetime is enforced by CAPPING the successor's expiry,
// not by revoking — because refresh_token_families_revoked_reason_defined has
// no 'expired' value and filing an age-out under 'operator' would be a lie in an
// audit trail.
func TestRotateCapsTheSuccessorAtTheSessionLifetime(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)
	familyID, root := openSession(t, s, user.ID, at, 24*time.Hour)

	const lifetime = 2 * time.Hour
	req := rotateReq(root, at.Add(time.Hour), 24*time.Hour)
	req.SessionLifetime = lifetime

	rec, outcome, err := s.Rotate(ctx, req)
	if err != nil || outcome != auth.RotateOK {
		t.Fatalf("Rotate = %s, %v", outcome, err)
	}
	want := at.Add(lifetime)
	if !rec.ExpiresAt.Equal(want) {
		t.Fatalf("successor expires at %s, want the session cap %s", rec.ExpiresAt, want)
	}
	// And the family is NOT revoked: the lineage ages out through expiry.
	if _, revoked := familyRevocation(t, familyID); revoked {
		t.Fatal("the family was revoked for ageing out; the schema has no reason for that")
	}
}

// -----------------------------------------------------------------------------
// revocation
// -----------------------------------------------------------------------------

func TestRevokeByTokenIsLogoutAndIsIdempotent(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)
	familyID, root := openSession(t, s, user.ID, at, time.Hour)

	rec, found, err := s.RevokeByToken(ctx, root, at.Add(time.Minute), auth.RevokedReasonLogout)
	if err != nil || !found {
		t.Fatalf("RevokeByToken = %v, %v", found, err)
	}
	if rec.FamilyID != familyID || rec.UserID != user.ID {
		t.Errorf("record = %+v, want family %q and user %q", rec, familyID, user.ID)
	}
	assertFamilyRevoked(t, familyID, "logout")

	// Idempotent: a retry after a network failure must not be a 500.
	if _, found, err := s.RevokeByToken(ctx, root, at.Add(2*time.Minute), auth.RevokedReasonLogout); err != nil || !found {
		t.Fatalf("second RevokeByToken = %v, %v", found, err)
	}
	// An unknown token is not an error either.
	if _, found, err := s.RevokeByToken(ctx, tokenHash(uniqueID("nope")), at, auth.RevokedReasonLogout); err != nil || found {
		t.Fatalf("RevokeByToken of an unknown token = %v, %v; want false, nil", found, err)
	}
}

// The FIRST reason survives. A lineage revoked for reuse and then logged out
// must keep saying 'reuse_detected' — that row is what a security review reads.
func TestRevocationPreservesTheFirstReason(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)
	familyID, root := openSession(t, s, user.ID, at, time.Hour)

	first := rotateReq(root, at.Add(time.Minute), time.Hour)
	if _, outcome, err := s.Rotate(ctx, first); err != nil || outcome != auth.RotateOK {
		t.Fatalf("Rotate = %s, %v", outcome, err)
	}
	if _, outcome, err := s.Rotate(ctx, rotateReq(root, at.Add(2*time.Minute), time.Hour)); err != nil || outcome != auth.RotateReuse {
		t.Fatalf("Rotate = %s, %v; want reuse", outcome, err)
	}

	if err := s.RevokeFamily(ctx, familyID, at.Add(3*time.Minute), auth.RevokedReasonLogout); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}
	assertFamilyRevoked(t, familyID, "reuse_detected")
}

func TestRevokeUserSessions(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)

	familyA, _ := openSession(t, s, user.ID, at, time.Hour)
	familyB, _ := openSession(t, s, user.ID, at, time.Hour)

	n, err := s.RevokeUserSessions(ctx, user.ID, at.Add(time.Minute), auth.RevokedReasonCredentialChange)
	if err != nil {
		t.Fatalf("RevokeUserSessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked %d sessions, want 2", n)
	}
	assertFamilyRevoked(t, familyA, "credential_change")
	assertFamilyRevoked(t, familyB, "credential_change")

	// A second sweep finds nothing live, so "log out everywhere" is idempotent.
	if n, err := s.RevokeUserSessions(ctx, user.ID, at.Add(2*time.Minute), auth.RevokedReasonOperator); err != nil || n != 0 {
		t.Fatalf("second RevokeUserSessions = %d, %v; want 0, nil", n, err)
	}
}

func TestRevocationRejectsAnUndefinedReason(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()

	if err := s.RevokeFamily(ctx, "fam_x", nowUTC(), auth.RevokedReasonUnknown); !errors.Is(err, auth.ErrInvalid) {
		t.Errorf("RevokeFamily with an undefined reason = %v, want ErrInvalid", err)
	}
	if _, err := s.RevokeUserSessions(ctx, "usr_x", nowUTC(), auth.RevokedReason(99)); !errors.Is(err, auth.ErrInvalid) {
		t.Errorf("RevokeUserSessions with an undefined reason = %v, want ErrInvalid", err)
	}
	if _, _, err := s.RevokeByToken(ctx, tokenHash("x"), nowUTC(), auth.RevokedReasonUnknown); !errors.Is(err, auth.ErrInvalid) {
		t.Errorf("RevokeByToken with an undefined reason = %v, want ErrInvalid", err)
	}
}

// -----------------------------------------------------------------------------
// user_totp
// -----------------------------------------------------------------------------

func TestTOTPEnrolmentLifecycle(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()
	at := nowUTC()
	user := createUser(t, s, at)

	if _, found, err := s.LoadTOTP(ctx, user.ID); err != nil || found {
		t.Fatalf("LoadTOTP before enrolment = %v, %v; want false, nil", found, err)
	}

	sealed := auth.Sealed{
		Ciphertext: []byte("not-really-ciphertext-but-nonempty"),
		Nonce:      []byte("123456789012"),
		KeyID:      "k1",
	}
	if err := s.SaveTOTP(ctx, auth.TOTPRecord{UserID: user.ID, Sealed: sealed}); err != nil {
		t.Fatalf("SaveTOTP: %v", err)
	}

	rec, found, err := s.LoadTOTP(ctx, user.ID)
	if err != nil || !found {
		t.Fatalf("LoadTOTP = %v, %v", found, err)
	}
	if rec.Confirmed() {
		t.Fatal("a freshly saved enrolment reports itself confirmed; a mis-scan would lock the account")
	}
	if string(rec.Sealed.Ciphertext) != string(sealed.Ciphertext) || rec.Sealed.KeyID != "k1" {
		t.Errorf("sealed value did not round-trip: %+v", rec.Sealed)
	}

	// An UNCONFIRMED enrolment is replaceable — that is how a mis-scan starts
	// again.
	replacement := sealed
	replacement.Ciphertext = []byte("a-different-ciphertext-value-here")
	if err := s.SaveTOTP(ctx, auth.TOTPRecord{UserID: user.ID, Sealed: replacement}); err != nil {
		t.Fatalf("replacing an unconfirmed enrolment = %v, want nil", err)
	}

	if err := s.ConfirmTOTP(ctx, user.ID, at.Add(time.Minute)); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	rec, _, err = s.LoadTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("LoadTOTP: %v", err)
	}
	if !rec.Confirmed() {
		t.Fatal("the enrolment is not confirmed after ConfirmTOTP")
	}
	if !rec.ConfirmedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("confirmed_at = %s, want %s", rec.ConfirmedAt, at.Add(time.Minute))
	}

	// Confirmation is write-once, so a replayed confirmation cannot move the
	// instant.
	if err := s.ConfirmTOTP(ctx, user.ID, at.Add(time.Hour)); !errors.Is(err, auth.ErrTOTPNotEnrolled) {
		t.Fatalf("second ConfirmTOTP = %v, want ErrTOTPNotEnrolled", err)
	}

	// THE control: a CONFIRMED enrolment cannot be overwritten. Without it, an
	// attacker holding a stolen session re-enrols their own authenticator over
	// a working second factor.
	if err := s.SaveTOTP(ctx, auth.TOTPRecord{UserID: user.ID, Sealed: replacement}); !errors.Is(err, auth.ErrTOTPAlreadyEnrolled) {
		t.Fatalf("overwriting a CONFIRMED enrolment = %v, want ErrTOTPAlreadyEnrolled", err)
	}

	if err := s.DeleteTOTP(ctx, user.ID); err != nil {
		t.Fatalf("DeleteTOTP: %v", err)
	}
	if _, found, err := s.LoadTOTP(ctx, user.ID); err != nil || found {
		t.Fatalf("LoadTOTP after delete = %v, %v; want false, nil", found, err)
	}
	// Deleting again is a no-op, not an error.
	if err := s.DeleteTOTP(ctx, user.ID); err != nil {
		t.Fatalf("second DeleteTOTP = %v, want nil", err)
	}
}

// -----------------------------------------------------------------------------
// assertions against the database's own state
// -----------------------------------------------------------------------------

func familyRevocation(t *testing.T, familyID string) (reason string, revoked bool) {
	t.Helper()

	var (
		at *time.Time
		r  *string
	)
	err := db(t).Pool().QueryRow(context.Background(),
		`SELECT revoked_at, revoked_reason FROM refresh_token_families WHERE id = $1`,
		familyID).Scan(&at, &r)
	if err != nil {
		t.Fatalf("reading the family's revocation: %v", err)
	}
	if at == nil {
		return "", false
	}
	if r == nil {
		// refresh_token_families_revocation_complete makes this impossible; if
		// it happens the constraint is gone.
		t.Fatal("a family is revoked with no reason; " +
			"refresh_token_families_revocation_complete is missing")
	}
	return *r, true
}

func assertFamilyRevoked(t *testing.T, familyID, wantReason string) {
	t.Helper()

	reason, revoked := familyRevocation(t, familyID)
	if !revoked {
		t.Fatalf("family %s is still live, want it revoked with reason %q", familyID, wantReason)
	}
	if reason != wantReason {
		t.Fatalf("family %s revoked_reason = %q, want %q", familyID, reason, wantReason)
	}
}

// assertSuccessorCount counts the non-root tokens in a family. Under the
// concurrent-redemption test this is the number that says whether the lineage
// forked.
func assertSuccessorCount(t *testing.T, familyID string, want int) {
	t.Helper()

	var got int
	err := db(t).Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_tokens WHERE family_id = $1 AND parent_id IS NOT NULL`,
		familyID).Scan(&got)
	if err != nil {
		t.Fatalf("counting successors: %v", err)
	}
	if got != want {
		t.Fatalf("family %s has %d successors, want %d; the lineage forked", familyID, got, want)
	}
}
