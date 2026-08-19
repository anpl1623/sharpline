// Integration tests for the Redis replay guard, against a REAL Redis 7 spawned
// by testcontainers-go from inside the test container.
//
// CLAUDE.md §10: "Integration tests use testcontainers-go against real
// Postgres/Redis/Kafka — no mocked databases, and no mocked broker either".
//
// # What only a real Redis can prove
//
// The whole implementation is `SET key 1 NX PX ttl`, and everything that makes
// it a control lives on the server: the atomicity of NX, and the fact that a
// skipped write reports itself distinguishably from an applied one. A fake
// client would be testing that a Go map holds a key.
//
// The test that matters is
// TestConcurrentConsumersOfOneStepProduceExactlyOneWinner, and it matters
// specifically because auth.MemoryReplayGuard cannot pass the equivalent across
// processes — which is the reason this package exists.
package redisguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/redis"
)

// redisImage is deploy/compose/compose.yaml's `redis` service, pinned by the
// same digest. Testing against a different build than the one that will run is
// the failure this pin exists to prevent.
const redisImage = "redis:7-alpine@sha256:e7723ff73d963f5cc6d9c4643ea3d989527a402a319239054e9472a7fb9219a2"

const containerStartDeadline = 2 * time.Minute

var (
	sharedClient *redis.Client
	sharedErr    error
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartDeadline+time.Minute)

	container, addr, err := startRedis(ctx)
	if err != nil {
		sharedErr = err
	} else {
		sharedClient, sharedErr = redis.Connect(ctx, redis.Options{
			Addr:    addr,
			Service: "auth-redisguard-it",
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}

	code := m.Run()

	if sharedClient != nil {
		_ = sharedClient.Close()
	}
	if container != nil {
		_ = container.Terminate(context.Background())
	}
	cancel()
	os.Exit(code)
}

func startRedis(ctx context.Context) (testcontainers.Container, string, error) {
	req := testcontainers.ContainerRequest{
		Image:        redisImage,
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor: wait.ForLog("Ready to accept connections").
			WithStartupTimeout(containerStartDeadline),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("starting redis: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		return container, "", fmt.Errorf("container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		return container, "", fmt.Errorf("container port: %w", err)
	}
	return container, host + ":" + port.Port(), nil
}

// client fails loudly rather than skipping. A silently skipped integration test
// reports green while proving nothing.
func client(t *testing.T) *redis.Client {
	t.Helper()
	if sharedErr != nil {
		t.Fatalf("the integration substrate is not available: %v\n"+
			"These tests do not skip. `make test` mounts the docker socket so "+
			"testcontainers-go can spawn siblings.", sharedErr)
	}
	return sharedClient
}

func newGuard(t *testing.T, now func() time.Time) *Guard {
	t.Helper()
	g, err := New(Options{Client: client(t), Now: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

// uniqueUser gives every test its own key space, so the suite is
// order-independent and safe under t.Parallel().
var userSeq atomic.Int64

func uniqueUser() domain.UserID {
	return domain.UserID(fmt.Sprintf("usr_it_%d_%d", time.Now().UnixNano()%1_000_000, userSeq.Add(1)))
}

func TestConsumeBurnsAStepExactlyOnce(t *testing.T) {
	t.Parallel()

	g := newGuard(t, nil)
	ctx := context.Background()
	user := uniqueUser()
	expiry := time.Now().Add(2 * time.Minute)

	first, err := g.Consume(ctx, user, 42, expiry)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !first {
		t.Fatal("the first consumption of a step was refused")
	}

	second, err := g.Consume(ctx, user, 42, expiry)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if second {
		t.Fatal("a step was consumed twice; a TOTP code is replayable inside its own window")
	}

	// A different user's identical step, and the same user's next step, are
	// both untouched. Without both in the key, one login would burn everybody's
	// current step — or the user's entire future.
	if ok, err := g.Consume(ctx, uniqueUser(), 42, expiry); err != nil || !ok {
		t.Fatalf("another user's step = %v, %v; want true, nil", ok, err)
	}
	if ok, err := g.Consume(ctx, user, 43, expiry); err != nil || !ok {
		t.Fatalf("the next step = %v, %v; want true, nil", ok, err)
	}
}

// THE test this package exists for.
//
// auth.MemoryReplayGuard cannot pass the cross-process form of this: two api
// pods each hold their own map, so a code burnt on one is fresh on the other.
// Here the atomicity is the server's, so it holds across as many callers as
// there are — which is what CLAUDE.md §9's "no session affinity" requires.
func TestConcurrentConsumersOfOneStepProduceExactlyOneWinner(t *testing.T) {
	t.Parallel()

	g := newGuard(t, nil)
	ctx := context.Background()
	user := uniqueUser()
	expiry := time.Now().Add(2 * time.Minute)

	const racers = 16

	var (
		wg      sync.WaitGroup
		winners atomic.Int64
		start   = make(chan struct{})
		errs    = make(chan error, racers)
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := g.Consume(ctx, user, 7, expiry)
			if err != nil {
				errs <- err
				return
			}
			if ok {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("Consume returned an error: %v", err)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("%d of %d concurrent consumers won, want exactly 1; "+
			"SET NX is not serialising and one TOTP code authenticates %d sessions",
			got, racers, got)
	}
}

// The TTL comes from the caller's absolute expiry, computed against the SAME
// clock the caller uses. A guard on a different clock either holds entries
// forever or sweeps them immediately, and the second failure mode is the
// control failing open.
func TestConsumeSetsATTLFromTheCallersClock(t *testing.T) {
	t.Parallel()

	base := time.Now()
	g := newGuard(t, func() time.Time { return base })
	ctx := context.Background()
	user := uniqueUser()

	const window = 90 * time.Second
	if ok, err := g.Consume(ctx, user, 11, base.Add(window)); err != nil || !ok {
		t.Fatalf("Consume = %v, %v", ok, err)
	}

	key := client(t).Key(keyScope, user.String(), "11")
	ttl, err := client(t).Redis().TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	// Redis rounds to whole seconds and a round trip has passed, so the bound
	// is a range rather than an equality.
	if ttl <= 0 || ttl > window {
		t.Fatalf("TTL = %s, want (0, %s]", ttl, window)
	}
	if ttl < window-5*time.Second {
		t.Fatalf("TTL = %s, want close to %s; the entry expires before the code does", ttl, window)
	}
}

// A far-future expiry must not pin memory forever.
func TestConsumeCapsTheTTL(t *testing.T) {
	t.Parallel()

	base := time.Now()
	g := newGuard(t, func() time.Time { return base })
	ctx := context.Background()
	user := uniqueUser()

	if ok, err := g.Consume(ctx, user, 12, base.Add(365*24*time.Hour)); err != nil || !ok {
		t.Fatalf("Consume = %v, %v", ok, err)
	}
	key := client(t).Key(keyScope, user.String(), "12")
	ttl, err := client(t).Redis().TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl > maxTTL {
		t.Fatalf("TTL = %s, over the %s cap", ttl, maxTTL)
	}
}

// An entry that has expired is gone, and the step is consumable again. That is
// harmless — no code from that step can still verify — and it is what keeps the
// keyspace bounded.
func TestAnExpiredEntryReleasesItsStep(t *testing.T) {
	t.Parallel()

	g := newGuard(t, nil)
	ctx := context.Background()
	user := uniqueUser()

	if ok, err := g.Consume(ctx, user, 13, time.Now().Add(time.Second)); err != nil || !ok {
		t.Fatalf("Consume = %v, %v", ok, err)
	}
	if ok, _ := g.Consume(ctx, user, 13, time.Now().Add(time.Second)); ok {
		t.Fatal("the guard did not hold the step")
	}

	// A real expiry, so a real sleep. One second is the smallest TTL Redis
	// expresses in whole seconds, and this is the only test here that waits.
	time.Sleep(1500 * time.Millisecond)

	if ok, err := g.Consume(ctx, user, 13, time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("Consume after expiry = %v, %v; want true, nil", ok, err)
	}
}

func TestConsumeRejectsANonFutureExpiry(t *testing.T) {
	t.Parallel()

	base := time.Now()
	g := newGuard(t, func() time.Time { return base })
	ctx := context.Background()

	for _, expiry := range []time.Time{base, base.Add(-time.Minute), {}} {
		_, err := g.Consume(ctx, uniqueUser(), 14, expiry)
		if !errors.Is(err, auth.ErrInvalid) {
			t.Errorf("Consume with expiry %s = %v, want ErrInvalid", expiry, err)
		}
	}
}

// A Redis failure must be an ERROR, not a "true". The caller turns an error
// into a failed second factor, so an outage makes 2FA logins fail rather than
// making the replay protection silently disappear.
func TestConsumeFailsClosedWhenRedisIsUnreachable(t *testing.T) {
	t.Parallel()

	g := newGuard(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ok, err := g.Consume(ctx, uniqueUser(), 15, time.Now().Add(time.Minute))
	if err == nil {
		t.Fatal("Consume against a cancelled context reported no error")
	}
	if ok {
		t.Fatal("Consume returned true on a failure; the guard failed OPEN")
	}
}

func TestNewRejectsAMissingClient(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{}); !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("New with no client = %v, want ErrInvalid", err)
	}
}

// The guard must satisfy the seam it exists for. Without this, a change to the
// interface would surface far from here.
var _ auth.ReplayGuard = (*Guard)(nil)
