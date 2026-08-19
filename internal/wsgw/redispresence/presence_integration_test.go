// Integration tests for the WebSocket subscription store, against a REAL Redis 7
// spawned by testcontainers-go from inside the test container.
//
// CLAUDE.md §10: "Integration tests use testcontainers-go against real
// Postgres/Redis/Kafka — no mocked databases, and no mocked broker either,
// because the interesting bugs live in […] offset handling". The equivalent here
// is that the interesting behaviour is all on the server.
//
// # What only a real Redis can prove
//
//   - THE PROPERTY THE PACKAGE EXISTS FOR. TestASecondReplicaReadsWhatTheFirstWrote
//     is the one assertion that says CLAUDE.md §9's "no session affinity" is
//     real: a client that reconnects onto a different pod gets its channel set
//     back. A fake would be testing that a Go map holds a key.
//   - THE CAP IS ATOMIC. TestConcurrentSubscribesCannotExceedTheCap fails for any
//     implementation that does SCARD from the client and then SADD, which is the
//     obvious implementation and is wrong the moment two replicas write one
//     session — the normal case during a reconnect.
//   - TTL IS REAL TIME ON A REAL SERVER. A replica that is SIGKILLed leaves
//     nothing behind, and only an expiry proves it.
//
// # Failure, not skip
//
// These tests FAIL when the docker socket is unreachable; they do not skip. A
// silently skipped integration test reports green while proving nothing, which
// is worse than no test at all — and the CI job that enforces the prime
// directive would become decorative.
package redispresence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/platform/redis"
)

// redisImage is deploy/compose/compose.yaml's `redis` service, pinned by the
// same digest. Testing against a different build than the one that will run is
// the failure this pin exists to prevent.
const redisImage = "redis:7-alpine@sha256:e7723ff73d963f5cc6d9c4643ea3d989527a402a319239054e9472a7fb9219a2"

const containerStartDeadline = 2 * time.Minute

var (
	sharedClient *redis.Client
	sharedAddr   string
	sharedErr    error
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartDeadline+time.Minute)

	container, addr, err := startRedis(ctx)
	if err != nil {
		sharedErr = err
	} else {
		sharedAddr = addr
		sharedClient, sharedErr = redis.Connect(ctx, redis.Options{
			Addr:    addr,
			Service: "wsgw-redispresence-it",
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

// newStore builds a store on the shared client. Options carries only the fields
// a test wants to vary; Client, Logger and Replica are filled in.
func newStore(t *testing.T, opts Options) *Store {
	t.Helper()
	opts.Client = client(t)
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Replica == "" {
		opts.Replica = uniqueReplica()
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Every test owns its own session, replica and channel names, so the suite is
// order-independent and safe under t.Parallel() against one shared server.
var seq atomic.Int64

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1_000_000, seq.Add(1))
}

func uniqueSession() string    { return unique("sid") }
func uniqueReplica() string    { return unique("stream") }
func uniqueChannel() string    { return "market:" + unique("mkt") }
func uniqueConnection() string { return unique("conn") }

func mustSubscribe(t *testing.T, s *Store, session, conn string, channels ...string) {
	t.Helper()
	if err := s.Subscribe(context.Background(), session, channels, conn, false); err != nil {
		t.Fatalf("Subscribe(%v): %v", channels, err)
	}
}

func channelsOf(t *testing.T, s *Store, session string) []string {
	t.Helper()
	got, err := s.Channels(context.Background(), session)
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	return got
}

func assertChannels(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("channels = %v, want %v", got, want)
	}
}

// -----------------------------------------------------------------------------
// The round trip
// -----------------------------------------------------------------------------

func TestSubscribeStoresAndRestoresAChannelSet(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{})
	session, conn := uniqueSession(), uniqueConnection()
	a, b := uniqueChannel(), uniqueChannel()

	mustSubscribe(t, s, session, conn, a, b)
	assertChannels(t, channelsOf(t, s, session), a, b)

	// The result is sorted, so a reconnect restores channels in a deterministic
	// order rather than whatever order SMEMBERS happened to return.
	got := channelsOf(t, s, session)
	if !slices.IsSorted(got) {
		t.Fatalf("Channels returned an unsorted set: %v", got)
	}
}

// ABSENT IS NORMAL, NEVER AN ERROR. A session with nothing stored is a client
// that has not subscribed yet, and it is deliberately indistinguishable from a
// Redis that restarted empty — both are answered the same way.
func TestAnAbsentSessionRestoresEmptyAndNoError(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{})

	got, err := s.Channels(context.Background(), uniqueSession())
	if err != nil {
		t.Fatalf("Channels on an unknown session = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("Channels on an unknown session = %v, want empty", got)
	}

	sess, err := s.Session(context.Background(), uniqueSession())
	if err != nil {
		t.Fatalf("Session on an unknown session = %v, want nil", err)
	}
	if sess.Found {
		t.Fatal("an unknown session reported Found")
	}

	n, err := s.Subscribers(context.Background(), uniqueChannel())
	if err != nil {
		t.Fatalf("Subscribers on an unknown channel = %v, want nil", err)
	}
	if n != 0 {
		t.Fatalf("Subscribers on an unknown channel = %d, want 0", n)
	}
}

// THE test this package exists for.
//
// CLAUDE.md §9: "`stream` must be horizontally scalable, so no session affinity,
// which means subscription state lives in Redis rather than in a pod". Two
// stores with different replica identities stand in for two pods behind an
// affinity-free Ingress: a client subscribes through one, its socket drops, it
// reconnects onto the other, and its channel set is still there.
//
// A pod-local map cannot pass this. That is the entire point.
func TestASecondReplicaReadsWhatTheFirstWrote(t *testing.T) {
	t.Parallel()

	podA := newStore(t, Options{Replica: uniqueReplica()})
	podB := newStore(t, Options{Replica: uniqueReplica()})
	ctx := context.Background()

	session := uniqueSession()
	first, second := uniqueConnection(), uniqueConnection()
	a, b := uniqueChannel(), uniqueChannel()

	// The client connects to pod A and subscribes.
	if err := podA.Connected(ctx, session, first, false); err != nil {
		t.Fatalf("Connected on pod A: %v", err)
	}
	mustSubscribe(t, podA, session, first, a, b)

	// Its socket drops. Disconnected removes it from fleet presence and
	// deliberately leaves the subscription set alone — that IS the resume window.
	if err := podA.Disconnected(ctx, session, first); err != nil {
		t.Fatalf("Disconnected on pod A: %v", err)
	}

	// It reconnects and the load balancer picks pod B.
	restored, err := podB.Channels(ctx, session)
	if err != nil {
		t.Fatalf("Channels on pod B: %v", err)
	}
	assertChannels(t, restored, a, b)

	if err := podB.Connected(ctx, session, second, false); err != nil {
		t.Fatalf("Connected on pod B: %v", err)
	}

	// The session hash now names pod B, which is what an operator asking "where
	// is this session" has to be able to see.
	sess, err := podB.Session(ctx, session)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if !sess.Found {
		t.Fatal("the session hash is absent after a reconnect")
	}
	if sess.Replica != podB.Replica() {
		t.Fatalf("session replica = %q, want %q", sess.Replica, podB.Replica())
	}
	if sess.ConnectionID != second {
		t.Fatalf("session connection = %q, want %q", sess.ConnectionID, second)
	}
	if sess.LastSeen.IsZero() {
		t.Fatal("last_seen was not stamped")
	}
}

// -----------------------------------------------------------------------------
// TTL
// -----------------------------------------------------------------------------

// TTL, ALWAYS. A replica that is SIGKILLed runs no defer and sends no close
// frame; if the keys it wrote had no expiry they would be there for ever. This
// is the only test that waits on real time, and it waits on the smallest TTL the
// package admits.
func TestSubscriptionStateExpiresWithoutAHeartbeat(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{TTL: MinTTL})
	ctx := context.Background()
	session, conn := uniqueSession(), uniqueConnection()
	a := uniqueChannel()

	if err := s.Connected(ctx, session, conn, false); err != nil {
		t.Fatalf("Connected: %v", err)
	}
	mustSubscribe(t, s, session, conn, a)
	assertChannels(t, channelsOf(t, s, session), a)

	time.Sleep(MinTTL + 500*time.Millisecond)

	if got := channelsOf(t, s, session); len(got) != 0 {
		t.Fatalf("channels survived the TTL: %v", got)
	}
	sess, err := s.Session(ctx, session)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if sess.Found {
		t.Fatal("the session hash survived the TTL")
	}
	present, err := s.Present(ctx)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if len(present) != 0 {
		t.Fatalf("presence survived the TTL: %v", present)
	}
}

// The heartbeat is what keeps a live session alive. Without this, every client
// would lose its resume window one TTL after subscribing, no matter how healthy
// its connection was.
func TestTouchRefreshesEveryTTLTheSessionOwns(t *testing.T) {
	t.Parallel()

	const ttl = 2 * time.Second
	s := newStore(t, Options{TTL: ttl})
	ctx := context.Background()
	session, conn := uniqueSession(), uniqueConnection()
	a := uniqueChannel()

	if err := s.Connected(ctx, session, conn, false); err != nil {
		t.Fatalf("Connected: %v", err)
	}
	mustSubscribe(t, s, session, conn, a)

	time.Sleep(ttl - 800*time.Millisecond)
	if err := s.Touch(ctx, session, conn); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	for name, key := range map[string]string{
		"subscriptions": s.subKey(session),
		"session":       s.sessionKey(session),
		"presence":      s.presenceKey,
	} {
		left, err := client(t).Redis().PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("PTTL(%s): %v", name, err)
		}
		if left <= ttl-500*time.Millisecond {
			t.Errorf("%s TTL is %s after a heartbeat, want close to %s", name, left, ttl)
		}
	}

	// Past the original expiry, and still there because of the heartbeat.
	time.Sleep(ttl - 800*time.Millisecond)
	assertChannels(t, channelsOf(t, s, session), a)
}

// -----------------------------------------------------------------------------
// The cap
// -----------------------------------------------------------------------------

// The cap is a REFUSAL, and the refusal is atomic: a rejected subscribe leaves
// the stored set byte-for-byte unchanged. Truncating instead would silently give
// the client a different subscription than it asked for, and the divergence
// would surface later as a market missing from the board.
func TestSubscribeRefusesPastTheCapWithoutWritingAnything(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{MaxChannels: 3})
	ctx := context.Background()
	session, conn := uniqueSession(), uniqueConnection()
	a, b := uniqueChannel(), uniqueChannel()
	c, d, e := uniqueChannel(), uniqueChannel(), uniqueChannel()

	mustSubscribe(t, s, session, conn, a, b)

	err := s.Subscribe(ctx, session, []string{c, d, e}, conn, false)
	if !errors.Is(err, ErrTooManyChannels) {
		t.Fatalf("Subscribe past the cap = %v, want ErrTooManyChannels", err)
	}
	assertChannels(t, channelsOf(t, s, session), a, b)

	// Nothing was written, so no counter moved for the rejected channels.
	for _, ch := range []string{c, d, e} {
		n, err := s.Subscribers(ctx, ch)
		if err != nil {
			t.Fatalf("Subscribers: %v", err)
		}
		if n != 0 {
			t.Fatalf("a rejected subscribe incremented the counter for %s to %d", ch, n)
		}
	}

	// A request that FITS is still admitted afterwards: the refusal is about the
	// request, not a latch on the session.
	if err := s.Subscribe(ctx, session, []string{c}, conn, false); err != nil {
		t.Fatalf("Subscribe within the cap after a refusal: %v", err)
	}
	assertChannels(t, channelsOf(t, s, session), a, b, c)
}

// The assertion an SCARD-then-SADD implementation cannot pass.
//
// Two `stream` replicas legitimately write one session at the same time — a
// reconnect straddles them — so a check-then-act cap would admit more than the
// cap under exactly the conditions the cap exists for. Here the check and the
// write are one script, so the server serialises them.
func TestConcurrentSubscribesCannotExceedTheCap(t *testing.T) {
	t.Parallel()

	const (
		limit   = 8
		racers  = 24
		timeout = 5 * time.Second
	)

	s := newStore(t, Options{MaxChannels: limit, Timeout: timeout})
	ctx := context.Background()
	session := uniqueSession()

	var (
		wg      sync.WaitGroup
		winners atomic.Int64
		start   = make(chan struct{})
		errs    = make(chan error, racers)
	)

	for i := 0; i < racers; i++ {
		channel := uniqueChannel()
		conn := uniqueConnection()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch err := s.Subscribe(ctx, session, []string{channel}, conn, false); {
			case err == nil:
				winners.Add(1)
			case errors.Is(err, ErrTooManyChannels):
				// The expected loser.
			default:
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("Subscribe returned an unexpected error: %v", err)
	}
	if got := winners.Load(); got != limit {
		t.Fatalf("%d of %d concurrent subscribes were admitted, want exactly %d; "+
			"the cap is not being enforced atomically", got, racers, limit)
	}
	if got := channelsOf(t, s, session); len(got) != limit {
		t.Fatalf("the session holds %d channels, want %d", len(got), limit)
	}
}

// -----------------------------------------------------------------------------
// Idempotence and the fleet counter
// -----------------------------------------------------------------------------

// Re-subscribing a channel the session already holds must not double-count the
// fleet counter. Only the server knows which members SADD actually added, which
// is the second reason the operation is a script rather than a pipeline.
func TestSubscribeIsIdempotentAndDoesNotDoubleCount(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{})
	ctx := context.Background()
	session, conn := uniqueSession(), uniqueConnection()
	a := uniqueChannel()

	mustSubscribe(t, s, session, conn, a)
	mustSubscribe(t, s, session, conn, a)
	mustSubscribe(t, s, session, conn, a, a)

	assertChannels(t, channelsOf(t, s, session), a)

	n, err := s.Subscribers(ctx, a)
	if err != nil {
		t.Fatalf("Subscribers: %v", err)
	}
	if n != 1 {
		t.Fatalf("Subscribers(%s) = %d after three subscribes of one channel, want 1", a, n)
	}
}

// The counter is an estimate of fleet-wide interest. It is exercised here across
// two sessions on two replicas, which is the only configuration in which it says
// anything a single pod could not have answered from memory.
func TestTheFleetCounterTracksTwoSessionsAcrossReplicas(t *testing.T) {
	t.Parallel()

	podA := newStore(t, Options{Replica: uniqueReplica()})
	podB := newStore(t, Options{Replica: uniqueReplica()})
	ctx := context.Background()

	shared := uniqueChannel()
	one, two := uniqueSession(), uniqueSession()
	connOne, connTwo := uniqueConnection(), uniqueConnection()

	mustSubscribe(t, podA, one, connOne, shared)
	mustSubscribe(t, podB, two, connTwo, shared)

	if n, err := podA.Subscribers(ctx, shared); err != nil || n != 2 {
		t.Fatalf("Subscribers = %d, %v; want 2, nil", n, err)
	}

	if err := podA.Unsubscribe(ctx, one, []string{shared}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if n, err := podB.Subscribers(ctx, shared); err != nil || n != 1 {
		t.Fatalf("Subscribers after one unsubscribe = %d, %v; want 1, nil", n, err)
	}

	// The last subscriber leaving deletes the counter rather than leaving a zero
	// behind, which is what stops the keyspace growing by one key per market
	// that was ever subscribed to.
	if err := podB.Forget(ctx, two, connTwo); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if n, err := podB.Subscribers(ctx, shared); err != nil || n != 0 {
		t.Fatalf("Subscribers after the last unsubscribe = %d, %v; want 0, nil", n, err)
	}
	exists, err := client(t).Redis().Exists(ctx, podB.channelKey(shared)).Result()
	if err != nil {
		t.Fatalf("EXISTS: %v", err)
	}
	if exists != 0 {
		t.Fatal("a counter that reached zero was left in the keyspace")
	}
}

func TestUnsubscribeRemovesOnlyTheNamedChannels(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{})
	ctx := context.Background()
	session, conn := uniqueSession(), uniqueConnection()
	a, b, c := uniqueChannel(), uniqueChannel(), uniqueChannel()

	mustSubscribe(t, s, session, conn, a, b, c)

	if err := s.Unsubscribe(ctx, session, []string{b}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	assertChannels(t, channelsOf(t, s, session), a, c)

	// Removing a channel the session does not hold is a NO-OP, not an error: an
	// unsubscribe racing an expiry is normal, and reporting a failure for a
	// state already reached would make the hub log an outage that is not one.
	if err := s.Unsubscribe(ctx, session, []string{b, uniqueChannel()}); err != nil {
		t.Fatalf("Unsubscribe of an absent channel = %v, want nil", err)
	}
	assertChannels(t, channelsOf(t, s, session), a, c)

	// And the counter for the channel that was never held did not go negative.
	if n, err := s.Subscribers(ctx, b); err != nil || n != 0 {
		t.Fatalf("Subscribers(%s) = %d, %v; want 0, nil", b, n, err)
	}
}

// -----------------------------------------------------------------------------
// Lifecycle
// -----------------------------------------------------------------------------

// Forget is the CLEAN close: the client said goodbye, so the durable state goes
// rather than waiting out the TTL. It must decrement every counter it removes —
// a DEL of the set would skip them all and leave the estimate permanently high.
func TestForgetRemovesTheDurableStateAndItsCounters(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{})
	ctx := context.Background()
	session, conn := uniqueSession(), uniqueConnection()
	a, b := uniqueChannel(), uniqueChannel()

	if err := s.Connected(ctx, session, conn, true); err != nil {
		t.Fatalf("Connected: %v", err)
	}
	mustSubscribe(t, s, session, conn, a, b)

	if err := s.Forget(ctx, session, conn); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if got := channelsOf(t, s, session); len(got) != 0 {
		t.Fatalf("channels survived Forget: %v", got)
	}
	sess, err := s.Session(ctx, session)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if sess.Found {
		t.Fatal("the session hash survived Forget")
	}
	for _, ch := range []string{a, b} {
		if n, err := s.Subscribers(ctx, ch); err != nil || n != 0 {
			t.Fatalf("Subscribers(%s) = %d, %v after Forget; want 0, nil", ch, n, err)
		}
	}
	present, err := s.Present(ctx)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if slices.Contains(present, conn) {
		t.Fatal("the connection is still in fleet presence after Forget")
	}

	// Forgetting a session that holds nothing is not an error. Shutdown paths
	// call it more than once and on sessions that never subscribed.
	if err := s.Forget(ctx, uniqueSession(), uniqueConnection()); err != nil {
		t.Fatalf("Forget of an unknown session = %v, want nil", err)
	}
}

// Disconnected is the UNCLEAN close, and it must NOT delete the subscription
// set. That set surviving is the resume window, and the resume window is the
// only client-visible consequence of affinity-free routing.
func TestDisconnectLeavesTheResumeWindowIntact(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{})
	ctx := context.Background()
	session, conn := uniqueSession(), uniqueConnection()
	a := uniqueChannel()

	if err := s.Connected(ctx, session, conn, false); err != nil {
		t.Fatalf("Connected: %v", err)
	}
	mustSubscribe(t, s, session, conn, a)

	if err := s.Disconnected(ctx, session, conn); err != nil {
		t.Fatalf("Disconnected: %v", err)
	}

	assertChannels(t, channelsOf(t, s, session), a)

	present, err := s.Present(ctx)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if slices.Contains(present, conn) {
		t.Fatal("a disconnected connection is still claimed by this replica")
	}
}

func TestPresenceTracksThisReplicasConnections(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{Replica: uniqueReplica()})
	other := newStore(t, Options{Replica: uniqueReplica()})
	ctx := context.Background()

	mine, theirs := uniqueConnection(), uniqueConnection()

	if err := s.Connected(ctx, uniqueSession(), mine, false); err != nil {
		t.Fatalf("Connected: %v", err)
	}
	if err := other.Connected(ctx, uniqueSession(), theirs, false); err != nil {
		t.Fatalf("Connected on the other replica: %v", err)
	}

	present, err := s.Present(ctx)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if !slices.Contains(present, mine) {
		t.Fatalf("Present = %v, missing this replica's own connection %s", present, mine)
	}
	// Presence is keyed by REPLICA. A pod that reported another pod's
	// connections would make the fleet view double-count on every scrape.
	if slices.Contains(present, theirs) {
		t.Fatalf("Present = %v, carries another replica's connection %s", present, theirs)
	}
}

func TestSessionRecordsWhetherTheConnectionWasAuthenticated(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{})
	ctx := context.Background()

	anon, authed := uniqueSession(), uniqueSession()

	if err := s.Connected(ctx, anon, uniqueConnection(), false); err != nil {
		t.Fatalf("Connected: %v", err)
	}
	if err := s.Connected(ctx, authed, uniqueConnection(), true); err != nil {
		t.Fatalf("Connected: %v", err)
	}

	got, err := s.Session(ctx, anon)
	if err != nil || !got.Found || got.Authenticated {
		t.Fatalf("anonymous session = %+v, %v; want Found and not Authenticated", got, err)
	}
	got, err = s.Session(ctx, authed)
	if err != nil || !got.Found || !got.Authenticated {
		t.Fatalf("authenticated session = %+v, %v; want Found and Authenticated", got, err)
	}
}

// -----------------------------------------------------------------------------
// Degradation
// -----------------------------------------------------------------------------

// LOSING REDIS DEGRADES, NEVER CORRUPTS. A store whose backend has gone away
// reports ErrUnavailable on every operation and returns nothing a caller could
// mistake for data. That classification is the whole contract with the hub: it
// is what the hub counts into sharpline_ws_presence_errors_total, which this
// package deliberately does not declare (see the metrics comment in
// presence.go).
func TestALostBackendIsClassifiedAsUnavailableAndMeasured(t *testing.T) {
	t.Parallel()

	_ = client(t) // fails loudly, with the harness message, if docker is unreachable

	ctx := context.Background()
	doomed, err := redis.Connect(ctx, redis.Options{
		Addr:    sharedAddr,
		Service: "wsgw-redispresence-it-doomed",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("connecting the second client: %v", err)
	}

	reg := prometheus.NewPedanticRegistry()
	s, err := New(Options{
		Client:   doomed,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Replica:  uniqueReplica(),
		Registry: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	session, conn := uniqueSession(), uniqueConnection()
	mustSubscribe(t, s, session, conn, uniqueChannel())

	// The backend goes away underneath a live store.
	if err := doomed.Close(); err != nil {
		t.Fatalf("closing the second client: %v", err)
	}

	if err := s.Touch(ctx, session, conn); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Touch against a dead backend = %v, want ErrUnavailable", err)
	}
	got, err := s.Channels(ctx, session)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Channels against a dead backend = %v, want ErrUnavailable", err)
	}
	if len(got) != 0 {
		t.Fatalf("a failed read returned %v; it must not invent a channel set", got)
	}

	// The duration histogram covers the attempt whether or not it succeeded: a
	// failing Redis that is also slow is exactly when the latency matters.
	if n := histogramCount(t, reg, "sharpline_ws_presence_duration_seconds", OpTouch); n == 0 {
		t.Error("presence_duration_seconds{op=touch} recorded no sample for a failed attempt")
	}
	if n := histogramCount(t, reg, "sharpline_ws_presence_duration_seconds", OpChannels); n == 0 {
		t.Error("presence_duration_seconds{op=channels} recorded no sample for a failed attempt")
	}
}

// A caller whose own context is cancelled is not an outage. The hub counts
// ErrUnavailable and nothing else, so without this distinction every deploy and
// every client disconnect would spike the one series that means "Redis is
// degrading".
func TestACancelledCallerIsNotClassifiedAsAnOutageAgainstARealServer(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Touch(ctx, uniqueSession(), uniqueConnection())
	if err == nil {
		t.Fatal("Touch against a cancelled context reported no error")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("a cancelled caller was classified as an outage: %v", err)
	}
}

// The keys this package writes are the ones doc.go documents, at the names an
// operator will type into redis-cli. A rename here is a breaking change for
// every runbook, so it is pinned against the real server rather than against the
// key builder alone.
func TestTheKeysExistWhereTheDocumentationSaysTheyDo(t *testing.T) {
	t.Parallel()

	s := newStore(t, Options{Replica: uniqueReplica()})
	ctx := context.Background()
	session, conn := uniqueSession(), uniqueConnection()
	a := uniqueChannel()

	if err := s.Connected(ctx, session, conn, false); err != nil {
		t.Fatalf("Connected: %v", err)
	}
	mustSubscribe(t, s, session, conn, a)

	prefix := client(t).KeyPrefix()

	// The counter key's stem is predictable and its digest suffix is not, so the
	// shape is asserted as a prefix and the existence check uses the key the
	// builder actually produced.
	channelKey := s.channelKey(a)
	if stem := prefix + ":ws:chan:market_" + a[len("market:"):] + "."; !strings.HasPrefix(channelKey, stem) {
		t.Errorf("the channel key %q does not start with the documented stem %q", channelKey, stem)
	}

	for name, key := range map[string]string{
		"subscriptions": prefix + ":ws:sub:" + hashSession(session),
		"session":       prefix + ":ws:sess:" + hashSession(session),
		"presence":      prefix + ":ws:presence:" + s.Replica(),
		"channel":       channelKey,
	} {
		n, err := client(t).Redis().Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("EXISTS %s: %v", key, err)
		}
		if n != 1 {
			t.Errorf("the %s key %q does not exist", name, key)
		}
		ttl, err := client(t).Redis().PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("PTTL %s: %v", key, err)
		}
		// TTL, ALWAYS: -1 is "no expiry", which is the state that leaks a
		// SIGKILLed replica's keys for ever.
		if ttl <= 0 {
			t.Errorf("the %s key %q has no expiry (PTTL %s)", name, key, ttl)
		}
	}
}
