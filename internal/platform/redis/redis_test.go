package redis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The engine under test is the SAME image and the SAME digest as the compose
// stack's `redis` service (deploy/compose/compose.yaml), and it is started with
// the same --requirepass and --appendonly arguments. Testing the access layer
// against a differently configured server defeats the point of using a real one
// (CLAUDE.md §10: "no mocked databases, and no mocked broker either").
const redisImage = "redis:7-alpine@sha256:e7723ff73d963f5cc6d9c4643ea3d989527a402a319239054e9472a7fb9219a2"

const testPassword = "sharpline-test-redis"

var (
	testAddr     string
	containerErr error
)

// TestMain starts one Redis container for the whole package.
//
// One container, not one per test: startup dominates the runtime, and every test
// below isolates itself with a unique key prefix rather than by needing a fresh
// server.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	terminate, err := startRedis(ctx)
	if err != nil {
		containerErr = err
	}

	code := m.Run()

	if terminate != nil {
		terminate()
	}
	os.Exit(code)
}

func startRedis(ctx context.Context) (func(), error) {
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        redisImage,
			ExposedPorts: []string{"6379/tcp"},
			Cmd: []string{
				"redis-server",
				"--appendonly", "yes",
				"--appendfsync", "everysec",
				"--requirepass", testPassword,
			},
			WaitingFor: wait.ForAll(
				wait.ForLog("Ready to accept connections"),
				wait.ForListeningPort("6379/tcp"),
			).WithDeadline(2 * time.Minute),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("start redis container: %w", err)
	}
	terminate := func() { _ = container.Terminate(context.Background()) }

	host, err := container.Host(ctx)
	if err != nil {
		terminate()
		return nil, fmt.Errorf("container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		terminate()
		return nil, fmt.Errorf("container port: %w", err)
	}

	testAddr = net.JoinHostPort(host, port.Port())
	return terminate, nil
}

func requireContainer(t *testing.T) {
	t.Helper()
	if containerErr != nil {
		t.Fatalf("redis container unavailable: %v", containerErr)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newTestClient connects with a per-test key prefix, so tests sharing the
// container cannot collide on a key.
func newTestClient(t *testing.T, mutate ...func(*Options)) *Client {
	t.Helper()
	requireContainer(t)

	opts := Options{
		Addr:      testAddr,
		Password:  testPassword,
		Service:   "test",
		Logger:    discardLogger(),
		KeyPrefix: strings.ReplaceAll(t.Name(), "/", "_"),
	}
	for _, f := range mutate {
		f(&opts)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := Connect(ctx, opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// -----------------------------------------------------------------------------
// Connect and options validation
// -----------------------------------------------------------------------------

func TestConnectRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts Options
	}{
		{"empty addr", Options{Service: "s", Logger: discardLogger()}},
		{"empty service", Options{Addr: "127.0.0.1:6379", Logger: discardLogger()}},
		{"nil logger", Options{Addr: "127.0.0.1:6379", Service: "s"}},
		{"addr without port", Options{Addr: "redis", Service: "s", Logger: discardLogger()}},
		{"negative db", Options{Addr: "127.0.0.1:6379", Service: "s", Logger: discardLogger(), DB: -1}},
		{"min idle over pool size", Options{
			Addr: "127.0.0.1:6379", Service: "s", Logger: discardLogger(),
			PoolSize: 2, MinIdleConns: 5,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Connect(context.Background(), tc.opts)
			if !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("want ErrInvalidOptions, got %v", err)
			}
		})
	}
}

// TestConnectWithWrongPasswordIsNotRetried is the load-bearing half of the
// retry classification: a credential failure must fail FAST and report the
// cause, not burn the whole startup budget rediscovering it.
func TestConnectWithWrongPasswordIsNotRetried(t *testing.T) {
	requireContainer(t)

	reg := prometheus.NewRegistry()
	start := time.Now()
	_, err := Connect(context.Background(), Options{
		Addr:     testAddr,
		Password: "definitely-not-the-password",
		Service:  "test",
		Logger:   discardLogger(),
		Registry: reg,
		// Generous budget: if this were retried the test would take ~18s.
		ConnectAttempts: 8,
	})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("credential failure took %s; it was retried when it must not be", elapsed)
	}
	if got := counterValue(t, reg, "sharpline_redis_connect_attempts_total", map[string]string{"outcome": "fatal"}); got != 1 {
		t.Fatalf("connect_attempts_total{outcome=fatal} = %v, want 1", got)
	}
	if got := counterValue(t, reg, "sharpline_redis_connect_attempts_total", map[string]string{"outcome": "retryable"}); got != 0 {
		t.Fatalf("connect_attempts_total{outcome=retryable} = %v, want 0", got)
	}
	// The error must name the server's cause and must NOT contain the password.
	if strings.Contains(err.Error(), "definitely-not-the-password") {
		t.Fatalf("the error echoed the password: %v", err)
	}
}

func TestConnectUnreachableExhaustsBudget(t *testing.T) {
	t.Parallel()

	// Port 1 on loopback refuses instantly, so this exercises the retry path
	// without waiting on a dial timeout.
	_, err := Connect(context.Background(), Options{
		Addr:              "127.0.0.1:1",
		Service:           "test",
		Logger:            discardLogger(),
		ConnectAttempts:   2,
		ConnectBackoff:    time.Millisecond,
		ConnectBackoffMax: time.Millisecond,
		PingTimeout:       200 * time.Millisecond,
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Readiness
// -----------------------------------------------------------------------------

// TestCheckIsARealRoundTrip is the phase-2 regression: a probe that latches a
// boolean at startup answers "did this process once connect", which is not the
// question a load balancer asks. Stopping the server must turn the check red.
//
// The server is not actually stopped here (it is shared by the package); the
// equivalent is closing the client, which is the other way a latched probe would
// keep answering 200.
func TestCheckReflectsRealReachability(t *testing.T) {
	c := newTestClient(t)

	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check on a live server: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Check(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Check after Close = %v, want ErrClosed", err)
	}
}

// TestCheckFailsWhenServerIsGone stops a DEDICATED container and asserts the
// probe goes red — the real form of the phase-2 regression.
func TestCheckFailsWhenServerIsGone(t *testing.T) {
	requireContainer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        redisImage,
			ExposedPorts: []string{"6379/tcp"},
			Cmd:          []string{"redis-server", "--requirepass", testPassword},
			WaitingFor: wait.ForAll(
				wait.ForLog("Ready to accept connections"),
				wait.ForListeningPort("6379/tcp"),
			).WithDeadline(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start dedicated redis: %v", err)
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	reg := prometheus.NewRegistry()
	c, err := Connect(ctx, Options{
		Addr:        net.JoinHostPort(host, port.Port()),
		Password:    testPassword,
		Service:     "test",
		Logger:      discardLogger(),
		Registry:    reg,
		PingTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Check(ctx); err != nil {
		t.Fatalf("Check on a live server: %v", err)
	}
	if got := gaugeValue(t, reg, "sharpline_redis_up"); got != 1 {
		t.Fatalf("sharpline_redis_up = %v, want 1", got)
	}

	if err := container.Stop(ctx, nil); err != nil {
		t.Fatalf("stop redis: %v", err)
	}

	if err := c.Check(ctx); err == nil {
		t.Fatal("Check returned nil with the server stopped — this is exactly the phase-2 bug")
	}
	if got := gaugeValue(t, reg, "sharpline_redis_up"); got != 0 {
		t.Fatalf("sharpline_redis_up = %v with the server stopped, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// Instrumentation
// -----------------------------------------------------------------------------

// TestHookNeverRecordsCommandArguments is the structural credential control for
// this package: the metric label is the command VERB and nothing else, so an
// AUTH command cannot put a password into a series name, and a SET cannot put a
// user's data there.
func TestHookNeverRecordsCommandArguments(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := newTestClient(t, func(o *Options) { o.Registry = reg })

	ctx := context.Background()
	key := c.Key("secret-holder")
	if err := c.Redis().Set(ctx, key, "a-value-that-must-not-be-a-label", time.Minute).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}
	// AUTH is the worst case: its argument IS the password.
	_ = c.Redis().Do(ctx, "AUTH", testPassword).Err()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				v := lp.GetValue()
				if strings.Contains(v, testPassword) {
					t.Fatalf("metric %s label %s=%q contains the password", mf.GetName(), lp.GetName(), v)
				}
				if strings.Contains(v, "a-value-that-must-not-be-a-label") {
					t.Fatalf("metric %s label %s=%q contains a command argument", mf.GetName(), lp.GetName(), v)
				}
				if strings.Contains(v, key) {
					t.Fatalf("metric %s label %s=%q contains a key", mf.GetName(), lp.GetName(), v)
				}
			}
		}
	}

	if got := counterValue(t, reg, "sharpline_redis_command_duration_seconds_count",
		map[string]string{"command": "SET", "outcome": "ok"}); got != 1 {
		t.Fatalf("command_duration_seconds_count{command=SET} = %v, want 1", got)
	}
}

// TestPingIsNotCountedAsACommand proves the readiness probe has its own series
// and does not swamp the command histogram — the reason
// withoutInstrumentation exists.
func TestPingIsNotCountedAsACommand(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := newTestClient(t, func(o *Options) { o.Registry = reg })

	for i := 0; i < 5; i++ {
		if err := c.Ping(context.Background()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	}

	if got := counterValue(t, reg, "sharpline_redis_command_duration_seconds_count",
		map[string]string{"command": "PING", "outcome": "ok"}); got != 0 {
		t.Fatalf("PING appeared in the command histogram %v times; it must not", got)
	}
	// Connect itself pings once, so 5 more makes 6.
	if got := histogramCount(t, reg, "sharpline_redis_ping_duration_seconds"); got < 5 {
		t.Fatalf("ping_duration_seconds count = %v, want >= 5", got)
	}
}

func TestKeyNotFoundIsNotAnError(t *testing.T) {
	c := newTestClient(t)

	_, err := c.Redis().Get(context.Background(), c.Key("no-such-key")).Result()
	if !IsKeyNotFound(err) {
		t.Fatalf("IsKeyNotFound(%v) = false, want true", err)
	}
	if !errors.Is(err, goredis.Nil) {
		t.Fatalf("want the go-redis sentinel to still be matchable, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Key namespacing
// -----------------------------------------------------------------------------

func TestKeySanitisation(t *testing.T) {
	t.Parallel()

	c := &Client{keyPrefix: "sharpline"}

	cases := []struct {
		name  string
		parts []string
		want  string
	}{
		{"plain", []string{"rl", "ip", "1.2.3.4"}, "sharpline:rl:ip:1.2.3.4"},
		{"empty part", []string{"rl", ""}, "sharpline:rl:_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Key(tc.parts...); got != tc.want {
				t.Fatalf("Key(%v) = %q, want %q", tc.parts, got, tc.want)
			}
		})
	}

	// A colon in a subject must not be able to forge a segment boundary: if it
	// survived, a user whose id was "ip:1.2.3.4" would be editing the per-IP
	// bucket for 1.2.3.4.
	forged := c.Key("rl", "user", "ip:1.2.3.4")
	honest := c.Key("rl", "ip", "1.2.3.4")
	if strings.Count(forged, ":") != 3 {
		t.Fatalf("Key(%q) = %q; the colon in the subject survived sanitisation", "ip:1.2.3.4", forged)
	}
	if forged == honest {
		t.Fatalf("a forged subject produced the same key as an honest one: %q", forged)
	}

	// Two distinct over-long parts must not collapse onto one bucket.
	longA := strings.Repeat("a", 200)
	longB := strings.Repeat("a", 199) + "b"
	if c.Key("rl", "user", longA) == c.Key("rl", "user", longB) {
		t.Fatal("two distinct over-long subjects collapsed onto the same key")
	}
}

// -----------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------

func TestIsTransientConnectError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"caller cancelled", context.Canceled, false},
		{"caller deadline", context.DeadlineExceeded, false},
		{"wrong password", goredis.Error(errString("WRONGPASS invalid username-password pair")), false},
		{"no auth", goredis.Error(errString("NOAUTH Authentication required.")), false},
		{"loading", goredis.Error(errString("LOADING Redis is loading the dataset in memory")), true},
		{"busy", goredis.Error(errString("BUSY Redis is busy running a script")), true},
		{"unknown command", goredis.Error(errString("ERR unknown command 'FOO'")), false},
		{"connection refused", errors.New("dial tcp 127.0.0.1:6379: connect: connection refused"), true},
		{"eof", io.EOF, true},
		{"client closed", goredis.ErrClosed, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientConnectError(tc.err); got != tc.want {
				t.Fatalf("IsTransientConnectError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// errString adapts a string to goredis.Error — the interface go-redis uses to
// mark a SERVER-side error, as opposed to a transport failure — so the
// classification table can construct the exact error shapes a real server
// produces.
type errString string

func (e errString) Error() string { return string(e) }
func (e errString) RedisError()   {}

// -----------------------------------------------------------------------------
// Metric helpers
// -----------------------------------------------------------------------------

func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	if m == nil {
		return 0
	}
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	if h := m.GetHistogram(); h != nil {
		return float64(h.GetSampleCount())
	}
	return 0
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	if reg == nil {
		return 0
	}
	m := findMetric(t, reg, name, nil)
	if m == nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func histogramCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	m := findMetric(t, reg, name, nil)
	if m == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

func findMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	if reg == nil {
		return nil
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	base := strings.TrimSuffix(name, "_count")
	for _, mf := range families {
		if mf.GetName() != name && mf.GetName() != base {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m, labels) {
				return m
			}
		}
	}
	return nil
}

func matchLabels(m *dto.Metric, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, lp := range m.GetLabel() {
			if lp.GetName() == k && lp.GetValue() == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
