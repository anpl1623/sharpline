package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Sentinel errors. CLAUDE.md §12 puts domain sentinels in the domain package;
// these are platform-level, matched with errors.Is by callers and tests, and
// follow the precedent already set by internal/platform/postgres and
// internal/platform/kafka.
var (
	// ErrInvalidOptions means Options could not produce a usable client.
	// Returned before any network I/O is attempted.
	ErrInvalidOptions = errors.New("redis: invalid options")

	// ErrUnavailable means the server could not be reached within the
	// configured retry budget. Every attempt failed transiently.
	ErrUnavailable = errors.New("redis: server unreachable")

	// ErrUnauthorized means the server rejected the credentials — a wrong
	// SHARPLINE_REDIS_PASSWORD, a password sent to a server that wants none, or
	// none sent to a server that requires one. Never retried: no amount of
	// waiting fixes a wrong password.
	ErrUnauthorized = errors.New("redis: server rejected the credentials")

	// ErrClosed means the client has been closed.
	ErrClosed = errors.New("redis: client is closed")

	// ErrKeyNotFound is what go-redis calls redis.Nil, restated as a Sharpline
	// sentinel so callers do not have to import go-redis to test for a cache
	// miss. See IsKeyNotFound — a miss is a normal operating state in this
	// system, never an error condition (see the package doc).
	ErrKeyNotFound = errors.New("redis: key not found")
)

// Client geometry defaults. Each is explained at its use site in Options.
const (
	// DefaultPoolSize is the per-service connection ceiling.
	//
	// The deploy target is an Oracle Ampere box with 2 OCPU and 12 GB, and
	// Redis is single-threaded: past a handful of connections per client the
	// commands queue on the server anyway, so a large pool buys latency
	// variance rather than throughput. go-redis' own default is 10*GOMAXPROCS,
	// which on a 2-core node is 20 per client and, across api + stream
	// replicas, is hundreds of sockets against a server that executes one
	// command at a time.
	//
	// 8 is sized for the actual traffic: the api issues at most two commands
	// per request (an IP bucket and a user bucket), both sub-millisecond, so 8
	// in flight covers a sustained few thousand requests per second per
	// replica.
	DefaultPoolSize = 8

	// DefaultMinIdleConns keeps connections warm. Not zero, because a cold pool
	// pays TCP + AUTH + HELLO on the first command of every idle period and
	// that cost lands on a rate-limit check, which is on the hot path of every
	// single request.
	DefaultMinIdleConns = 2

	// DefaultPoolTimeout bounds how long a caller waits for a pooled
	// connection when all of them are busy. Short on purpose: a rate-limit
	// check that queues for a second has already cost more than the request it
	// was protecting.
	DefaultPoolTimeout = 500 * time.Millisecond

	// DefaultConnMaxIdleTime returns capacity to the server when traffic ebbs.
	DefaultConnMaxIdleTime = 5 * time.Minute

	// DefaultConnMaxLifetime bounds how long a connection may live, so a
	// rolling Redis restart or a failover drains through natural expiry instead
	// of pinning every client to a server that is going away.
	DefaultConnMaxLifetime = 30 * time.Minute

	// DefaultDialTimeout bounds ONE dial and handshake, not the whole retry
	// budget.
	DefaultDialTimeout = 3 * time.Second

	// DefaultReadTimeout and DefaultWriteTimeout bound one command's socket
	// I/O. Deliberately tight: every command this system issues is O(1) against
	// an in-memory server on the same network, so a second is already a
	// pathology. A caller that needs longer (a blocking read) must set them
	// explicitly rather than raising the default for everyone.
	DefaultReadTimeout  = time.Second
	DefaultWriteTimeout = time.Second

	// DefaultMaxRetries is how many times go-redis retries one command against
	// a retryable error. One retry covers the common case — a connection the
	// pool believed was alive and the server had already closed — without
	// turning a real outage into a latency multiplier.
	DefaultMaxRetries = 1

	// DefaultPingTimeout bounds the readiness round trip. Deliberately below
	// httpx.DefaultReadinessTimeout (3s) so /readyz reports "redis" as the
	// failing check instead of the whole probe timing out with no detail.
	DefaultPingTimeout = time.Second

	// DefaultKeyPrefix namespaces every key this package writes, so a Redis
	// shared with anything else (a dev box, a future second application) cannot
	// collide. See Key.
	DefaultKeyPrefix = "sharpline"
)

// Startup retry defaults. Worst case with these values is
// 0.25+0.5+1+2+4+5+5 = 17.75s of backoff plus up to 8 ping timeouts.
//
// Sized for Kubernetes rather than for compose. Compose gates every service on
// `redis` being service_healthy so the race barely exists there; a StatefulSet
// has no such gate, and a Redis pod still loading its AOF answers -LOADING for
// as long as the replay takes.
const (
	// DefaultConnectAttempts counts the first try, so N attempts means N-1
	// retries.
	DefaultConnectAttempts = 8
	// DefaultConnectBackoff is the first sleep; it doubles per attempt.
	DefaultConnectBackoff = 250 * time.Millisecond
	// DefaultConnectBackoffMax caps the doubling.
	DefaultConnectBackoffMax = 5 * time.Second
)

// checkerName is the identifier this client reports in the /readyz payload.
const checkerName = "redis"

// Options configures the client. Everything except Addr, Service and Logger has
// a documented default; nothing here is read from the process environment,
// because configuration is loaded once by internal/platform/config and injected
// (CLAUDE.md §12).
type Options struct {
	// Addr is SHARPLINE_REDIS_ADDR, already validated as host:port by
	// internal/platform/config (Config.RedisAddr). Required.
	Addr string

	// Password is SHARPLINE_REDIS_PASSWORD (Config.RedisPassword). Optional: a
	// passwordless Redis is valid in a test environment, which is why config
	// does not require it even when it requires the address.
	//
	// It is never logged. Not in the ready line, not in an error, not in a
	// span: LogValue below reports this field as a boolean, and nothing in this
	// package ever writes its value into a string.
	Password string

	// Username is the ACL user. Empty means the legacy AUTH form against the
	// `default` user, which is what deploy/compose's `--requirepass` sets up.
	Username string

	// DB is the logical database index. Zero unless something deliberately
	// partitions; note that SELECT does not exist in Redis Cluster, so anything
	// non-zero is a decision to stay single-node.
	DB int

	// Service is the binary name. It becomes the client name, which is what
	// `CLIENT LIST` reports — the join key between a slow command on the server
	// and the slog line that caused it. Required, for the same reason
	// application_name is required by internal/platform/postgres.
	Service string

	// Logger receives lifecycle events. Required: a client that cannot report
	// its own retries is a client whose startup failures are invisible.
	Logger *slog.Logger

	// Registry is where metrics are registered. nil registers nothing, which is
	// correct for unit tests and for a binary that serves no /metrics. Services
	// pass httpx.Server.Registry().
	Registry prometheus.Registerer

	// TracerProvider supplies the tracer for command spans. nil uses the OTel
	// global provider, which is a no-op until a cmd/ entrypoint installs a real
	// one — so an un-instrumented binary costs nothing and a traced one needs
	// no change here.
	TracerProvider trace.TracerProvider

	// KeyPrefix namespaces every key built by Key. Empty means
	// DefaultKeyPrefix.
	KeyPrefix string

	// Pool geometry. Zero means the corresponding Default* constant.
	PoolSize        int
	MinIdleConns    int
	PoolTimeout     time.Duration
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration

	// Per-command socket budgets. Zero means the Default* constant; a negative
	// value means "no deadline", which go-redis spells as -1.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// MaxRetries bounds go-redis' own per-command retry. Zero means
	// DefaultMaxRetries; negative disables retrying entirely.
	MaxRetries int

	// PingTimeout bounds the readiness round trip.
	PingTimeout time.Duration

	// ConnectAttempts, ConnectBackoff and ConnectBackoffMax bound startup
	// retries. Retries apply ONLY to establishing connectivity — see
	// IsTransientConnectError.
	ConnectAttempts   int
	ConnectBackoff    time.Duration
	ConnectBackoffMax time.Duration
}

// LogValue implements slog.LogValuer so that logging an Options value cannot
// leak the password. This is the structural half of the rule; the other half is
// that nothing in this package ever writes opts.Password into a string.
func (o Options) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", o.Addr),
		slog.String("service", o.Service),
		slog.Int("db", o.DB),
		slog.String("username", o.Username),
		slog.Bool("password_set", o.Password != ""),
		slog.String("key_prefix", o.KeyPrefix),
	)
}

// Client is a Redis connection pool with instrumentation and a readiness check.
// It is safe for concurrent use.
type Client struct {
	rdb       *goredis.Client
	log       *slog.Logger
	metrics   *metrics
	closed    closedFlag
	keyPrefix string

	pingTimeout time.Duration
}

// Connect builds the pool and proves it can reach the server before returning,
// retrying transient failures. A non-nil Client is connected; the caller does
// not have to probe it first.
//
// ctx bounds the whole startup sequence including every retry. Cancelling it
// aborts immediately.
func Connect(ctx context.Context, opts Options) (*Client, error) {
	cfg, err := buildOptions(opts)
	if err != nil {
		return nil, err
	}

	m, err := newMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}

	tp := opts.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}

	rdb := goredis.NewClient(cfg)
	rdb.AddHook(&commandHook{
		tracer:  tp.Tracer(tracerName),
		metrics: m,
	})

	c := &Client{
		rdb:         rdb,
		log:         opts.Logger,
		metrics:     m,
		keyPrefix:   keyPrefixOr(opts.KeyPrefix),
		pingTimeout: positiveOr(opts.PingTimeout, DefaultPingTimeout),
	}

	// Late-bind the stats source now that the pool exists, mirroring
	// postgres.DB.registerPoolStats. c.Close, not rdb.Close, on the failure
	// path below: the collector is already registered on the caller's registry
	// by that point and a failed Connect does not unregister it, so setting the
	// closed flag is what makes the next scrape emit nothing instead of the
	// frozen statistics of a pool that no longer exists.
	if err := c.registerPoolStats(opts.Registry); err != nil {
		// The close error is deliberately discarded on both failure paths: the
		// caller is already receiving the reason Connect failed, and replacing
		// it with "close: ..." would hide it.
		_ = c.Close()
		return nil, err
	}

	if err := c.awaitReady(ctx, opts); err != nil {
		_ = c.Close()
		return nil, err
	}

	opts.Logger.Info("redis client ready",
		slog.Any("redis", opts),
		slog.Int("pool_size", cfg.PoolSize),
		slog.Int("min_idle_conns", cfg.MinIdleConns),
		slog.String("dial_timeout", cfg.DialTimeout.String()),
		slog.String("read_timeout", cfg.ReadTimeout.String()),
		slog.String("write_timeout", cfg.WriteTimeout.String()),
	)
	return c, nil
}

// registerPoolStats attaches the pool-statistics collector.
func (c *Client) registerPoolStats(reg prometheus.Registerer) error {
	if reg == nil {
		return nil
	}
	if err := reg.Register(newPoolStatsCollector(c.poolStats)); err != nil {
		return fmt.Errorf("redis: register pool stats collector: %w", err)
	}
	return nil
}

// buildOptions validates Options and resolves the pool geometry.
func buildOptions(opts Options) (*goredis.Options, error) {
	switch {
	case strings.TrimSpace(opts.Addr) == "":
		return nil, fmt.Errorf("%w: Addr is empty", ErrInvalidOptions)
	case opts.Service == "":
		return nil, fmt.Errorf("%w: Service is empty", ErrInvalidOptions)
	case opts.Logger == nil:
		return nil, fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	case opts.DB < 0:
		return nil, fmt.Errorf("%w: DB is %d", ErrInvalidOptions, opts.DB)
	case opts.PoolSize < 0:
		return nil, fmt.Errorf("%w: PoolSize is %d", ErrInvalidOptions, opts.PoolSize)
	case opts.MinIdleConns < 0:
		return nil, fmt.Errorf("%w: MinIdleConns is %d", ErrInvalidOptions, opts.MinIdleConns)
	}

	if _, _, err := net.SplitHostPort(opts.Addr); err != nil {
		// The address is not a secret, so it may be quoted back.
		return nil, fmt.Errorf("%w: Addr %q is not host:port: %w", ErrInvalidOptions, opts.Addr, err)
	}

	poolSize := positiveIntOr(opts.PoolSize, DefaultPoolSize)
	minIdle := positiveIntOr(opts.MinIdleConns, DefaultMinIdleConns)
	if minIdle > poolSize {
		return nil, fmt.Errorf("%w: MinIdleConns (%d) exceeds PoolSize (%d)", ErrInvalidOptions, minIdle, poolSize)
	}

	return &goredis.Options{
		Addr:     opts.Addr,
		Username: opts.Username,
		Password: opts.Password,
		DB:       opts.DB,

		// ClientName issues CLIENT SETNAME on every new connection, so
		// `CLIENT LIST` attributes a connection to a binary. Same role as
		// application_name in internal/platform/postgres.
		ClientName: opts.Service,

		DialTimeout:  socketTimeout(opts.DialTimeout, DefaultDialTimeout),
		ReadTimeout:  socketTimeout(opts.ReadTimeout, DefaultReadTimeout),
		WriteTimeout: socketTimeout(opts.WriteTimeout, DefaultWriteTimeout),

		PoolSize:        poolSize,
		MinIdleConns:    minIdle,
		PoolTimeout:     positiveOr(opts.PoolTimeout, DefaultPoolTimeout),
		ConnMaxIdleTime: positiveOr(opts.ConnMaxIdleTime, DefaultConnMaxIdleTime),
		ConnMaxLifetime: positiveOr(opts.ConnMaxLifetime, DefaultConnMaxLifetime),

		MaxRetries: retriesOr(opts.MaxRetries, DefaultMaxRetries),

		// go-redis 9 defaults to RESP3 (HELLO 3). Left on: RESP3 is what
		// carries per-command error typing and push messages, and Redis 7 —
		// the pinned image — speaks it natively.
	}, nil
}

// awaitReady proves connectivity, retrying only transient failures.
//
// There is no general-purpose retry helper in this package, deliberately and
// for the same reason internal/platform/postgres has none: a retry that does
// not know whether the operation is idempotent is a retry that will eventually
// duplicate a side effect. Startup connectivity is the one case where the
// answer is unambiguous, so it is the one case that retries.
func (c *Client) awaitReady(ctx context.Context, opts Options) error {
	attempts := positiveIntOr(opts.ConnectAttempts, DefaultConnectAttempts)
	base := positiveOr(opts.ConnectBackoff, DefaultConnectBackoff)
	maxBackoff := positiveOr(opts.ConnectBackoffMax, DefaultConnectBackoffMax)

	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := c.ping(ctx)
		if err == nil {
			c.metrics.connectAttempts.WithLabelValues(connectOK).Inc()
			return nil
		}
		last = err

		// A deadline that expired is the PING's OWN budget (PingTimeout)
		// unless the caller's context is also done. That distinction is
		// load-bearing: a server that is not listening yet makes go-redis
		// exhaust its dial retries and surface context.DeadlineExceeded from
		// the ping's timeout, which is exactly the StatefulSet startup race
		// this loop exists for — classifying it as fatal would abandon the
		// retry budget on the one failure it was written for. A deadline that
		// came from the CALLER is a caller who has given up, and is fatal.
		transient := IsTransientConnectError(err)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			transient = true
		}

		if !transient {
			c.metrics.connectAttempts.WithLabelValues(connectFatal).Inc()
			if isAuthError(err) {
				// The error text from the server may echo the ACL username but
				// never the password. Wrapping keeps the server's wording,
				// which is what tells an operator whether the problem is
				// WRONGPASS or NOAUTH.
				return fmt.Errorf("%w: %w", ErrUnauthorized, err)
			}
			return fmt.Errorf("redis: connect to %s: %w", opts.Addr, err)
		}

		c.metrics.connectAttempts.WithLabelValues(connectRetryable).Inc()
		if attempt == attempts {
			break
		}

		d := jitter(backoffFor(base, maxBackoff, attempt))
		c.log.Warn("redis unreachable, retrying",
			slog.String("addr", opts.Addr),
			slog.Int("attempt", attempt),
			slog.Int("attempts", attempts),
			slog.String("backoff", d.String()),
			slog.String("error", err.Error()),
		)
		if err := sleep(ctx, d); err != nil {
			return fmt.Errorf("%w: %w (last error: %w)", ErrUnavailable, err, last)
		}
	}

	return fmt.Errorf("%w after %d attempts: %w", ErrUnavailable, attempts, last)
}

// Redis exposes the underlying go-redis client.
//
// It is exported for the same reason postgres.DB.Pool is: CLAUDE.md §12 puts
// interfaces with the consumer, so market, wsgw and betting each declare the
// small interface they need over this handle rather than this package guessing
// at their needs. Every command issued through it is instrumented, because the
// instrumentation is a go-redis Hook rather than a wrapper.
//
// Returns nil after Close.
func (c *Client) Redis() *goredis.Client { return c.rdb }

// KeyPrefix reports the namespace every key built by Key carries.
func (c *Client) KeyPrefix() string { return c.keyPrefix }

// Key builds a namespaced key from parts, joined with ':'.
//
// Every part is sanitised: bytes outside [A-Za-z0-9._-] are replaced with '_'
// and an over-long part is truncated and suffixed with a short hash of the
// original. That is not cosmetic. Key parts carry user-influenced values — a
// client IP, an email, a subject from a token — and an unsanitised part lets a
// caller with a colon in their input address a key namespace that is not theirs
// (write "rl:ip:1.2.3.4" as a user id and you are editing somebody else's
// bucket). Bounding the length stops a megabyte header from becoming a megabyte
// key.
func (c *Client) Key(parts ...string) string {
	out := make([]string, 0, len(parts)+1)
	if c.keyPrefix != "" {
		out = append(out, c.keyPrefix)
	}
	for _, p := range parts {
		out = append(out, sanitiseKeyPart(p))
	}
	return strings.Join(out, ":")
}

// Close releases every pooled connection. It is safe to call more than once.
func (c *Client) Close() error {
	if !c.closed.set() {
		return nil
	}
	c.metrics.up.Set(0)
	if c.rdb == nil {
		return nil
	}
	if err := c.rdb.Close(); err != nil {
		return fmt.Errorf("redis: close: %w", err)
	}
	return nil
}

// poolStats reports go-redis' pool statistics, or nil once closed.
func (c *Client) poolStats() *goredis.PoolStats {
	if c.closed.isSet() || c.rdb == nil {
		return nil
	}
	return c.rdb.PoolStats()
}

// ping performs one readiness round trip.
//
// It carries its own timeout on top of ctx so a caller that passes
// context.Background does not get an unbounded probe (CLAUDE.md §12: "every
// external call has a timeout"). Whichever deadline is nearer wins, which is
// what context composition already guarantees.
//
// The command runs with instrumentation suppressed — see withoutInstrumentation
// for why a readiness probe must not produce a span or a command histogram
// sample.
func (c *Client) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(withoutInstrumentation(ctx), c.pingTimeout)
	defer cancel()

	start := time.Now()
	err := c.rdb.Ping(ctx).Err()
	c.metrics.pingDuration.Observe(time.Since(start).Seconds())

	if err != nil {
		c.metrics.up.Set(0)
		return err
	}
	c.metrics.up.Set(1)
	return nil
}

// IsKeyNotFound reports whether err means "the key does not exist".
//
// It matches both go-redis' own redis.Nil and this package's ErrKeyNotFound, so
// a caller can test a miss without importing go-redis. A miss is a normal
// operating state in this system — Redis expires, evicts and restarts empty
// (see the package doc) — so every read path must have an answer for it that is
// not "return an error to the user".
func IsKeyNotFound(err error) bool {
	return errors.Is(err, goredis.Nil) || errors.Is(err, ErrKeyNotFound)
}

// IsTransientConnectError reports whether err is a connectivity failure that
// waiting might fix.
//
// The classification is deliberately narrow, exactly as it is in
// internal/platform/postgres. Retrying a wrong password or an unknown ACL user
// wastes the startup budget and then reports the wrong cause; retrying a
// command that already reached the server risks duplicating its effect. Only
// these are transient:
//
//   - the connection was refused, reset, or never established (the server is
//     not listening yet — the ordinary StatefulSet startup race)
//   - the dial or the command timed out
//   - the server answered -LOADING (it is replaying its AOF or RDB) or
//     -BUSY (a script is monopolising it), both of which end on their own
//   - the server answered -MASTERDOWN or -CLUSTERDOWN, which a failover clears
func IsTransientConnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The CALLER's deadline expiring is not the server's fault, and
		// retrying against an already-dead context accomplishes nothing. But a
		// go-redis dial timeout surfaces as os.ErrDeadlineExceeded wrapped in a
		// net.Error, which the net.Error branch below catches instead.
		return false
	}
	if isAuthError(err) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// go-redis reports server-side errors as its own redis.Error string type.
	// The prefix is the RESP error code and it is a stable part of the Redis
	// protocol.
	var redisErr goredis.Error
	if errors.As(err, &redisErr) {
		msg := redisErr.Error()
		for _, prefix := range transientServerPrefixes {
			if strings.HasPrefix(msg, prefix) {
				return true
			}
		}
		return false
	}

	// Everything else that is not a server error and not a net.Error: a closed
	// pool, an EOF mid-command, a broken pipe.
	return errors.Is(err, goredis.ErrClosed) || errors.Is(err, net.ErrClosed) ||
		strings.Contains(err.Error(), "EOF") ||
		strings.Contains(err.Error(), "broken pipe") ||
		strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "connection reset")
}

// transientServerPrefixes are the RESP error codes that clear on their own.
var transientServerPrefixes = []string{
	"LOADING",     // replaying AOF/RDB after a restart
	"BUSY",        // a script is running; it will be killed or finish
	"MASTERDOWN",  // replica lost its primary
	"CLUSTERDOWN", // the slot map is not covered yet
	"TRYAGAIN",    // a cluster multi-key command during resharding
}

// isAuthError reports whether the server rejected the credentials. Never
// retried.
//
// The three codes are the three distinct ways this can be wrong, and they are
// worth keeping distinguishable in the error text because they point at
// different fixes:
//
//	NOAUTH    the server wants a password and SHARPLINE_REDIS_PASSWORD is empty
//	WRONGPASS the password (or the ACL username) is wrong
//	ERR Client sent AUTH, but no password is set
//	          the server wants none and one was supplied
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "NOAUTH") ||
		strings.Contains(msg, "WRONGPASS") ||
		strings.Contains(msg, "NOPERM") ||
		strings.Contains(msg, "Client sent AUTH, but no password is set") ||
		strings.Contains(msg, "without any password configured")
}

// sanitiseKeyPart makes one key segment safe to concatenate.
//
// Colons are the segment separator, so a value carrying one could otherwise
// forge a segment boundary and address a namespace it does not own. Anything
// outside the conservative set becomes '_'; a part longer than maxKeyPartLen is
// truncated and suffixed with a FNV-1a hash of the original so two long,
// distinct inputs cannot collapse onto the same bucket.
func sanitiseKeyPart(s string) string {
	if s == "" {
		return "_"
	}

	safe := true
	for i := 0; i < len(s); i++ {
		if !isSafeKeyByte(s[i]) {
			safe = false
			break
		}
	}
	if safe && len(s) <= maxKeyPartLen {
		return s
	}

	b := make([]byte, 0, maxKeyPartLen+9)
	for i := 0; i < len(s) && len(b) < maxKeyPartLen; i++ {
		if isSafeKeyByte(s[i]) {
			b = append(b, s[i])
			continue
		}
		b = append(b, '_')
	}
	return string(b) + "." + fnv1aHex(s)
}

const maxKeyPartLen = 96

func isSafeKeyByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-':
		return true
	default:
		return false
	}
}

// fnv1aHex is a short, non-cryptographic digest used only to keep two distinct
// over-long or unsafe key parts from collapsing onto one bucket. It is not a
// security control and nothing depends on it being hard to invert.
func fnv1aHex(s string) string {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		out[i] = hexDigits[h&0xf]
		h >>= 4
	}
	return string(out)
}

func keyPrefixOr(s string) string {
	if strings.TrimSpace(s) == "" {
		return DefaultKeyPrefix
	}
	return sanitiseKeyPart(s)
}

// closedFlag makes a client's methods safe to call from a goroutine that races
// Close. Same shape as internal/platform/kafka's.
type closedFlag struct{ v atomic.Bool }

func (f *closedFlag) set() bool   { return f.v.CompareAndSwap(false, true) }
func (f *closedFlag) isSet() bool { return f.v.Load() }

// positiveOr resolves a non-positive duration to its default.
func positiveOr(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

// positiveIntOr resolves a non-positive count to its default.
func positiveIntOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

// socketTimeout resolves a per-command budget. A negative value is an explicit
// "no deadline", which go-redis spells as -1 rather than 0 (0 means "use my
// default").
func socketTimeout(v, fallback time.Duration) time.Duration {
	switch {
	case v < 0:
		return -1
	case v == 0:
		return fallback
	default:
		return v
	}
}

// retriesOr resolves the per-command retry budget. Negative means "never
// retry", which go-redis also spells as -1.
func retriesOr(v, fallback int) int {
	switch {
	case v < 0:
		return -1
	case v == 0:
		return fallback
	default:
		return v
	}
}

// backoffFor returns base * 2^(attempt-1), capped, without overflowing.
func backoffFor(base, maxBackoff time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		if d >= maxBackoff/2 {
			return maxBackoff
		}
		d *= 2
	}
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// jitter spreads retries across replicas so a Redis restart is not followed by
// every api and stream pod reconnecting in the same millisecond. Full jitter
// over [d/2, d).
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// sleep waits for d or until ctx is done, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
