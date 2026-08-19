// The store: keys, bounds, the two Lua scripts, the MULTI/EXEC transactions,
// the error classification and the metric contract.
//
// Read doc.go first. It carries the argument for the pod/Redis split, for why
// losing this store degrades rather than corrupts, for why the session key is
// hashed, for why two operations are scripts and the rest are transactions, and
// for why the fleet subscriber counter is honest about being an estimate. This
// file is the code those arguments describe.
package redispresence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	"github.com/anpl1623/sharpline/internal/platform/redis"
)

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

// Sentinel errors. Two of them are a CLASSIFICATION the caller acts on, not
// decoration: the hub counts an [ErrUnavailable] and keeps serving the socket,
// and refuses the client's request on an [ErrInvalidArgument] or an
// [ErrTooManyChannels]. Nothing here may ever close a connection — see the
// degradation table in doc.go.
var (
	// ErrInvalidOptions means [Options] could not produce a usable store.
	// Returned before any I/O is attempted, so a misconfigured `stream` fails
	// at startup rather than on its first client.
	ErrInvalidOptions = errors.New("redispresence: invalid options")

	// ErrInvalidArgument means the CALLER passed something this package
	// refuses: an empty session key, a channel name that is empty, over-long or
	// carries bytes that have no business in one, an over-long connection id.
	// It is a programming or protocol error, never an outage, and it is
	// deliberately NOT counted in sharpline_ws_presence_errors_total.
	ErrInvalidArgument = errors.New("redispresence: invalid argument")

	// ErrTooManyChannels means the session already holds, or would exceed,
	// Options.MaxChannels. The subscription set is left byte-for-byte
	// unchanged — the cap is enforced inside the script, before it writes.
	//
	// It is separate from ErrInvalidArgument because the hub answers it
	// differently: a `rejected` entry in the ack frame naming the cap, rather
	// than a protocol error.
	ErrTooManyChannels = errors.New("redispresence: subscription set is full")

	// ErrUnavailable means the operation could not reach Redis, or Redis
	// refused it. THIS is the error the caller counts and then ignores.
	//
	// A server-side type error (WRONGTYPE, because somebody wrote a string
	// where this package keeps a set) also lands here, which is deliberate: the
	// caller's correct response is identical — count it, log it, keep the
	// socket — and the operator's signal is the metric and the log line rather
	// than a Go type nobody switches on.
	ErrUnavailable = errors.New("redispresence: subscription store is unavailable")

	// ErrClosed means Close has been called. Every method checks it before
	// touching Redis, so a store outliving its owner fails fast instead of
	// resurrecting keys during shutdown.
	ErrClosed = errors.New("redispresence: store is closed")
)

// -----------------------------------------------------------------------------
// Geometry
// -----------------------------------------------------------------------------

const (
	// DefaultTTL is how long a subscription set, a session hash and a presence
	// set survive without a heartbeat.
	//
	// It is sized against the hub's ping interval, not against a human sense of
	// "a while": phase 6 pings every 20s, so 90s is four and a half missed
	// heartbeats. Short enough that a SIGKILLed replica's keys are gone within
	// a minute and a half; long enough that a client riding out a subway tunnel
	// still resumes without re-listing its channels.
	DefaultTTL = 90 * time.Second

	// MinTTL and MaxTTL bound Options.TTL.
	//
	// The floor is a sanity bound and NOT a substitute for the caller's
	// judgement: a TTL below the hub's ping interval expires between
	// heartbeats, which turns every heartbeat into a resume-window loss, and
	// this package cannot see the ping interval to check it. The ceiling stops
	// a configuration typo from pinning a session's memory for a day.
	MinTTL = time.Second
	MaxTTL = time.Hour

	// DefaultMaxChannels caps the channels ONE SESSION may store. It matches
	// the hub's per-connection cap (D9) so the two refusals agree; a store cap
	// below the hub's would reject subscriptions the hub had already accepted,
	// which is the worst of both.
	DefaultMaxChannels = 256

	// MaxChannelsHardCap bounds Options.MaxChannels itself. The cap exists
	// because an unbounded SET keyed by a session id an anonymous client can
	// obtain on demand is a memory-exhaustion primitive (doc.go), and a bound a
	// caller may raise without limit is not a bound.
	MaxChannelsHardCap = 4096

	// MaxChannelsPerCall bounds ONE Subscribe or Unsubscribe request,
	// independently of what is already stored. It bounds the script's KEYS
	// array and the argument slice built for it, so a single malformed frame
	// cannot turn into a multi-megabyte command.
	MaxChannelsPerCall = 256

	// MaxChannelLen bounds one channel name.
	//
	// The longest legal channel is `market:` + domain.MaxIDLen, i.e. 135 bytes.
	// 160 leaves headroom for a fourth channel kind without letting an
	// unbounded string become a set member. This package does NOT re-validate
	// the channel grammar — see validateChannels.
	MaxChannelLen = 160

	// MaxSessionKeyLen bounds the caller-supplied session key. The stored key
	// segment is a fixed-length digest regardless, so this bounds only the
	// transient argument; it is here so a pathological caller cannot hand this
	// package a megabyte to hash.
	MaxSessionKeyLen = 256

	// MaxConnectionIDLen bounds the connection id. It is stored as a set member
	// and echoed in log lines, so it is bounded and charset-checked rather than
	// hashed — it is server-generated and carries nothing secret.
	MaxConnectionIDLen = 128

	// DefaultTimeout bounds ONE operation against Redis.
	//
	// CLAUDE.md §12: every external call has a timeout. This one is generous
	// relative to internal/platform/redis' per-command budget because a
	// subscribe is a script over several keys behind a pool that may be
	// momentarily saturated by 10k heartbeating clients — and tight relative to
	// anything a human would notice, because NOTHING waits on it: the socket is
	// already served whether this succeeds or not.
	DefaultTimeout = 500 * time.Millisecond
)

// sessionDigestLen is how many hex characters of the SHA-256 digest become the
// key segment. 32 hex characters is 128 bits, which is past any collision worth
// reasoning about for a keyspace bounded by concurrent sessions, and short
// enough that a key stays readable in redis-cli.
const sessionDigestLen = 32

// Key scopes. Each is a literal segment, so every key this package writes is
// greppable (`KEYS sharpline:ws:*`) and cannot collide with the rate limiter's
// or the replay guard's keyspace.
const (
	keyScope      = "ws"
	scopeSub      = "sub"
	scopeSession  = "sess"
	scopePresence = "presence"
	scopeChannel  = "chan"
)

// Session hash fields. Names are short because they are stored per session and
// read by a human in redis-cli, not by a schema.
const (
	fieldLastSeen     = "last_seen"
	fieldReplica      = "replica"
	fieldConnectionID = "connection_id"
	fieldAuthedFlag   = "authenticated"
)

// warnEvery rate-limits the degraded-mode log line.
//
// D6 requires the WARN to be rate limited, and the reason is arithmetic: at
// CLAUDE.md §10's 10k subscribers and a 20s heartbeat, an unrate-limited line
// per failed Touch is 500 lines a second for the duration of a Redis outage,
// which buries every other signal in the log exactly when it is needed. One line
// per interval, carrying the count it suppressed, says the same thing.
const warnEvery = 30 * time.Second

// -----------------------------------------------------------------------------
// Operation labels — the CLOSED `op` label set
// -----------------------------------------------------------------------------

// Op* are the values of the `op` label on both metrics. The set is closed: every
// constant appears in exactly one method below and nothing else is ever written
// to the label.
const (
	OpSubscribe    = "subscribe"
	OpUnsubscribe  = "unsubscribe"
	OpChannels     = "channels"
	OpTouch        = "touch"
	OpForget       = "forget"
	OpConnected    = "connected"
	OpDisconnected = "disconnected"
	OpSubscribers  = "subscribers"
	OpSession      = "session"
	OpPresent      = "present"
)

// -----------------------------------------------------------------------------
// Options and construction
// -----------------------------------------------------------------------------

// Options configures [New].
type Options struct {
	// Client is the connected Redis client. Required.
	//
	// The store does NOT own it: it was opened by the composition root and is
	// shared with the rate limiter and anything else in the process, so
	// [Store.Close] deliberately does not close it.
	Client *redis.Client

	// Logger receives the rate-limited degraded-mode warnings. Required: a
	// store that cannot report that it has stopped working is a store whose
	// failure is invisible, and its failure is invisible in every other signal
	// because the sockets keep working.
	Logger *slog.Logger

	// Replica is this pod's identity — the hostname in compose and in
	// Kubernetes. Required, and required to be usable as a key segment
	// unchanged (see New), so that the key an operator greps and the string a
	// log line prints are the same string.
	Replica string

	// TTL is the subscription and presence expiry. Zero means DefaultTTL; it
	// must otherwise fall between MinTTL and MaxTTL.
	TTL time.Duration

	// MaxChannels caps the channels one session may store. Zero means
	// DefaultMaxChannels; it must not exceed MaxChannelsHardCap.
	MaxChannels int

	// Timeout bounds one operation. Zero means DefaultTimeout. Negative is
	// refused: "no deadline" is never the right answer for a call whose result
	// nothing waits on.
	Timeout time.Duration

	// Registry is where the two metric series are registered. nil builds the
	// collectors WITHOUT registering them, which is correct for a unit test and
	// for a binary that serves no /metrics — the observe calls stay live and
	// cost a few nanoseconds, so no call site needs a nil check.
	Registry prometheus.Registerer
}

// Store is the Redis-backed subscription and presence store. It is safe for
// concurrent use.
type Store struct {
	client  *redis.Client
	log     *slog.Logger
	metrics *metrics

	replica     string
	presenceKey string
	ttl         time.Duration
	timeout     time.Duration
	maxChannels int

	closed atomic.Bool
	warn   warnLimiter
}

// New builds a Store. It performs no I/O — a Redis that is down at startup must
// not stop `stream` from accepting connections (doc.go), so there is nothing
// here to fail on.
func New(opts Options) (*Store, error) {
	switch {
	case opts.Client == nil:
		return nil, fmt.Errorf("%w: Client is nil", ErrInvalidOptions)
	case opts.Logger == nil:
		return nil, fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	case strings.TrimSpace(opts.Replica) == "":
		return nil, fmt.Errorf("%w: Replica is empty; it is this pod's identity "+
			"and fleet presence is meaningless without it", ErrInvalidOptions)
	case !isSafeKeySegment(opts.Replica):
		// redis.Client.Key would sanitise it silently. Refusing instead keeps
		// the key segment and the logged identity identical, so a Grafana
		// panel, a log line and a redis-cli session all name the same pod. Same
		// decision, and the same reason, as redis.NewRateLimiter's Scope check.
		return nil, fmt.Errorf("%w: Replica %q must match [A-Za-z0-9._-]{1,%d}",
			ErrInvalidOptions, opts.Replica, maxKeySegmentLen)
	case opts.TTL < 0:
		return nil, fmt.Errorf("%w: TTL is %s", ErrInvalidOptions, opts.TTL)
	case opts.TTL > 0 && opts.TTL < MinTTL:
		return nil, fmt.Errorf("%w: TTL %s is below the %s floor; state would expire between heartbeats",
			ErrInvalidOptions, opts.TTL, MinTTL)
	case opts.TTL > MaxTTL:
		return nil, fmt.Errorf("%w: TTL %s exceeds the %s ceiling", ErrInvalidOptions, opts.TTL, MaxTTL)
	case opts.MaxChannels < 0:
		return nil, fmt.Errorf("%w: MaxChannels is %d", ErrInvalidOptions, opts.MaxChannels)
	case opts.MaxChannels > MaxChannelsHardCap:
		return nil, fmt.Errorf("%w: MaxChannels %d exceeds the %d hard cap",
			ErrInvalidOptions, opts.MaxChannels, MaxChannelsHardCap)
	case opts.Timeout < 0:
		return nil, fmt.Errorf("%w: Timeout is %s; an unbounded call is never right here",
			ErrInvalidOptions, opts.Timeout)
	}

	m, err := newMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}

	s := &Store{
		client:      opts.Client,
		log:         opts.Logger,
		metrics:     m,
		replica:     opts.Replica,
		presenceKey: opts.Client.Key(keyScope, scopePresence, opts.Replica),
		ttl:         durationOr(opts.TTL, DefaultTTL),
		timeout:     durationOr(opts.Timeout, DefaultTimeout),
		maxChannels: intOr(opts.MaxChannels, DefaultMaxChannels),
	}
	return s, nil
}

// Replica reports this store's pod identity, as it appears in the presence key.
func (s *Store) Replica() string { return s.replica }

// TTL reports the configured expiry.
//
// It is exported so the hub can assert its own ping interval is comfortably
// below it — this package cannot make that check itself, and a heartbeat slower
// than the TTL silently disables resume-on-reconnect.
func (s *Store) TTL() time.Duration { return s.ttl }

// Close marks the store closed. Subsequent calls return [ErrClosed].
//
// It does NOT close the Redis client: the client was opened by the composition
// root and is shared with the rate limiter and everything else in the process,
// so closing it here would take down components this package does not own.
// Calling Close more than once is safe.
func (s *Store) Close() error {
	s.closed.Store(true)
	return nil
}

// -----------------------------------------------------------------------------
// Keys
// -----------------------------------------------------------------------------

// subKey is the SET of channels a session holds.
func (s *Store) subKey(session string) string {
	return s.client.Key(keyScope, scopeSub, hashSession(session))
}

// sessionKey is the HASH describing a session's current connection.
func (s *Store) sessionKey(session string) string {
	return s.client.Key(keyScope, scopeSession, hashSession(session))
}

// channelKey is the fleet-wide subscriber counter for one channel.
//
// The channel carries a ':' — `market:{id}` — which is the key separator, so
// redis.Client.Key rewrites it to '_' AND appends a digest of the original. That
// suffix is what keeps the mapping injective given that this package does not
// check the channel grammar; doc.go carries the argument. The channel is passed
// through unflattened precisely so the builder's sanitiser is the single place
// that decision is made.
func (s *Store) channelKey(channel string) string {
	return s.client.Key(keyScope, scopeChannel, channel)
}

// hashSession turns a caller-supplied session key into a fixed-length, opaque
// key segment.
//
// SHA-256 truncated to 128 bits. The full argument is in doc.go: the point is
// that no caller mistake — passing a raw bearer token instead of a `sid` — can
// put a credential into a Redis key, an RDB file or a MONITOR trace in
// cleartext. It is defence in depth and not an authorisation control; nothing
// here trusts the value, it only uses it as a name.
func hashSession(session string) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:])[:sessionDigestLen]
}

// -----------------------------------------------------------------------------
// Subscribe / Unsubscribe — the two Lua scripts
// -----------------------------------------------------------------------------

// subscribeScript adds channels to a session's set, refuses past the cap, and
// keeps the fleet counter consistent with the set. Atomic on the server.
//
//	KEYS[1]      subscription set
//	KEYS[2]      session hash
//	KEYS[3..]    per-channel counters, aligned with ARGV[6..]
//	ARGV[1]      ttl, milliseconds
//	ARGV[2]      max channels per session
//	ARGV[3]      replica
//	ARGV[4]      connection id
//	ARGV[5]      authenticated, "0" or "1"
//	ARGV[6..]    channel names
//
// Returns {status, stored_after, added} with status 0 = applied, 1 = refused
// because the cap would have been exceeded. On a refusal NOTHING is written —
// the SISMEMBER pass runs before the first SADD precisely so a rejected request
// cannot leave a partially-applied set behind.
//
// The clock is the SERVER's, read inside the script, for the reason
// internal/platform/redis' token bucket gives: every replica must measure
// against one clock, or the worst clock skew in the deployment becomes part of
// the semantics. Here it only stamps a diagnostic field, but there is no reason
// to introduce the skew for it. Redis has replicated command EFFECTS rather than
// scripts since 5.0, so a non-deterministic script is fully supported on the
// pinned Redis 7 image.
var subscribeScript = goredis.NewScript(`
local sub  = KEYS[1]
local sess = KEYS[2]
local ttl  = tonumber(ARGV[1])
local maxn = tonumber(ARGV[2])
local n    = #KEYS - 2

local fresh = {}
for i = 1, n do
  if redis.call('SISMEMBER', sub, ARGV[5 + i]) == 0 then
    fresh[#fresh + 1] = i
  end
end

local stored = redis.call('SCARD', sub)
if stored + #fresh > maxn then
  return {1, stored, 0}
end

for _, i in ipairs(fresh) do
  redis.call('SADD', sub, ARGV[5 + i])
  redis.call('INCR', KEYS[2 + i])
  redis.call('PEXPIRE', KEYS[2 + i], ttl)
end

redis.call('PEXPIRE', sub, ttl)

local t   = redis.call('TIME')
local now = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)
redis.call('HSET', sess,
  'last_seen', now,
  'replica', ARGV[3],
  'connection_id', ARGV[4],
  'authenticated', ARGV[5])
redis.call('PEXPIRE', sess, ttl)

return {0, stored + #fresh, #fresh}
`)

// unsubscribeScript removes channels and decrements exactly the counters whose
// member was actually removed. Atomic on the server.
//
//	KEYS[1]    subscription set
//	KEYS[2]    session hash
//	KEYS[3..]  per-channel counters, aligned with ARGV[2..]
//	ARGV[1]    ttl, milliseconds
//	ARGV[2..]  channel names
//
// Returns {removed, stored_after}.
//
// A counter that reaches zero is DELETED rather than left at zero: an absent
// counter and a zero counter mean the same thing, and deleting it is what stops
// the keyspace growing by one key per market that was ever subscribed to. A
// counter that would go negative (its key expired while subscribers remained —
// see doc.go) is deleted for the same reason, so a negative value is never
// stored.
var unsubscribeScript = goredis.NewScript(`
local sub  = KEYS[1]
local sess = KEYS[2]
local ttl  = tonumber(ARGV[1])
local n    = #KEYS - 2
local removed = 0

for i = 1, n do
  if redis.call('SREM', sub, ARGV[1 + i]) == 1 then
    removed = removed + 1
    local left = redis.call('DECR', KEYS[2 + i])
    if left <= 0 then
      redis.call('DEL', KEYS[2 + i])
    else
      redis.call('PEXPIRE', KEYS[2 + i], ttl)
    end
  end
end

local stored = redis.call('SCARD', sub)
if stored > 0 then
  redis.call('PEXPIRE', sub, ttl)
else
  redis.call('DEL', sub)
end
if redis.call('EXISTS', sess) == 1 then
  redis.call('PEXPIRE', sess, ttl)
end

return {removed, stored}
`)

// Subscribe records that a session holds channels, and refreshes every TTL it
// touches.
//
// It is IDEMPOTENT: re-subscribing a channel the session already holds is a
// no-op that does not double-count the fleet counter, because only the server
// knows which members were new (doc.go).
//
// It refuses, atomically and without writing anything, when the session already
// holds — or would exceed — Options.MaxChannels: [ErrTooManyChannels]. A
// refusal leaves the stored set unchanged, so the hub can report the rejection
// to the client and the client's next subscribe still sees the truth.
//
// authenticated is stamped on the session hash so an operator can see whether a
// session presented a token. It is diagnostic only; nothing in this package
// makes an authorisation decision, and CLAUDE.md §6 makes market data public
// anyway.
//
// A returned error never means the client's subscription failed — the hub's own
// routing table is the thing that delivers frames. It means resume-on-reconnect
// is unavailable for this session until Redis recovers.
func (s *Store) Subscribe(
	ctx context.Context, sessionKey string, channels []string, connectionID string, authenticated bool,
) error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := validateSessionKey(sessionKey); err != nil {
		return err
	}
	if err := validateConnectionID(connectionID); err != nil {
		return err
	}
	names, err := validateChannels(channels)
	if err != nil {
		return err
	}
	rdb, err := s.conn()
	if err != nil {
		return err
	}

	keys := make([]string, 0, len(names)+2)
	keys = append(keys, s.subKey(sessionKey), s.sessionKey(sessionKey))
	args := make([]any, 0, len(names)+5)
	args = append(args,
		s.ttl.Milliseconds(),
		s.maxChannels,
		s.replica,
		connectionID,
		boolArg(authenticated),
	)
	for _, c := range names {
		keys = append(keys, s.channelKey(c))
		args = append(args, c)
	}

	done := s.begin(OpSubscribe)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	raw, err := subscribeScript.Run(callCtx, rdb, keys, args...).Result()
	if err != nil {
		return done(s.fail(ctx, OpSubscribe, sessionKey, err))
	}

	status, stored, err := decodeTriple(raw)
	if err != nil {
		return done(s.fail(ctx, OpSubscribe, sessionKey, err))
	}
	if status == 1 {
		// A protocol-level refusal, not a failure: reported to the caller,
		// deliberately not counted as an outage.
		return done(fmt.Errorf("%w: session holds %d of %d channels, %d more requested",
			ErrTooManyChannels, stored, s.maxChannels, len(names)))
	}
	return done(nil)
}

// Unsubscribe removes channels from a session's set.
//
// Removing a channel the session does not hold is a no-op, not an error: an
// unsubscribe racing an expiry is normal, and turning it into an error would
// make the hub report a failure for a state it had already reached.
func (s *Store) Unsubscribe(ctx context.Context, sessionKey string, channels []string) error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := validateSessionKey(sessionKey); err != nil {
		return err
	}
	names, err := validateChannels(channels)
	if err != nil {
		return err
	}
	rdb, err := s.conn()
	if err != nil {
		return err
	}

	done := s.begin(OpUnsubscribe)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if err := s.removeChannels(callCtx, rdb, sessionKey, names); err != nil {
		return done(s.fail(ctx, OpUnsubscribe, sessionKey, err))
	}
	return done(nil)
}

// removeChannels runs unsubscribeScript. It is shared with Forget, which is why
// it takes an already-bounded context and returns the raw error: the two
// callers classify and label it differently.
func (s *Store) removeChannels(
	ctx context.Context, rdb *goredis.Client, sessionKey string, names []string,
) error {
	keys := make([]string, 0, len(names)+2)
	keys = append(keys, s.subKey(sessionKey), s.sessionKey(sessionKey))
	args := make([]any, 0, len(names)+1)
	args = append(args, s.ttl.Milliseconds())
	for _, c := range names {
		keys = append(keys, s.channelKey(c))
		args = append(args, c)
	}
	return unsubscribeScript.Run(ctx, rdb, keys, args...).Err()
}

// -----------------------------------------------------------------------------
// Reads
// -----------------------------------------------------------------------------

// Channels returns the channel set stored for a session — the set restored on
// reconnect and echoed in the hub's `hello` frame.
//
// AN ABSENT SESSION RETURNS AN EMPTY SLICE AND A NIL ERROR. That is a client
// which has not subscribed yet, and it is deliberately indistinguishable from a
// Redis that restarted empty: both mean "this store knows of no channels", and
// both are answered the same way — the client subscribes. Treating absence as an
// error would make an empty cache look like a broken one (doc.go, and the rule
// internal/platform/redis' package doc sets for every read here).
//
// The result is SORTED. `SMEMBERS` returns members in an unspecified order, and
// an unspecified order would leak into the hello frame's `channels` array, where
// it would make a client's reconnect non-deterministic and a test flaky for
// reasons that have nothing to do with subscriptions.
//
// It is a pure read: it does not refresh any TTL. Restoring a session is
// immediately followed by [Store.Connected] and then by heartbeats, and a read
// that silently extended a lease would make an operator's inspection change the
// thing being inspected.
func (s *Store) Channels(ctx context.Context, sessionKey string) ([]string, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	if err := validateSessionKey(sessionKey); err != nil {
		return nil, err
	}
	rdb, err := s.conn()
	if err != nil {
		return nil, err
	}

	done := s.begin(OpChannels)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	members, err := rdb.SMembers(callCtx, s.subKey(sessionKey)).Result()
	if err != nil {
		// SMEMBERS on a missing key returns an empty slice rather than
		// redis.Nil, so there is no absent case to fold in here; anything that
		// arrives is a real failure.
		return nil, done(s.fail(ctx, OpChannels, sessionKey, err))
	}
	slices.Sort(members)
	return members, done(nil)
}

// Session is the diagnostic view of `ws:sess:{session}`.
//
// Nothing in the fanout path reads it. It exists so the hash is not write-only:
// an operator answering "which replica is this session on and when did it last
// heartbeat" is a legitimate reader, and a key nothing can read is a key nobody
// can trust.
type Session struct {
	// Found reports whether the session hash existed. False means every other
	// field is zero — and is NORMAL, for the reason Channels gives.
	Found bool

	// LastSeen is when the session last subscribed or heartbeated, by the
	// writer's clock. Diagnostic only: liveness is enforced by the TTL, which
	// is the SERVER's, so a pod with a skewed clock produces a misleading
	// LastSeen and cannot extend or shorten anybody's lease.
	LastSeen time.Time

	// Replica is the pod that last wrote this session.
	Replica string

	// ConnectionID is the connection that last wrote it.
	ConnectionID string

	// Authenticated reports whether that connection presented a token.
	Authenticated bool
}

// Session reads the session hash. An absent session returns a zero [Session]
// with Found false and a nil error.
func (s *Store) Session(ctx context.Context, sessionKey string) (Session, error) {
	if err := s.usable(); err != nil {
		return Session{}, err
	}
	if err := validateSessionKey(sessionKey); err != nil {
		return Session{}, err
	}
	rdb, err := s.conn()
	if err != nil {
		return Session{}, err
	}

	done := s.begin(OpSession)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	fields, err := rdb.HGetAll(callCtx, s.sessionKey(sessionKey)).Result()
	if err != nil {
		return Session{}, done(s.fail(ctx, OpSession, sessionKey, err))
	}
	if len(fields) == 0 {
		return Session{}, done(nil)
	}

	out := Session{
		Found:         true,
		Replica:       fields[fieldReplica],
		ConnectionID:  fields[fieldConnectionID],
		Authenticated: fields[fieldAuthedFlag] == "1",
	}
	// A malformed timestamp is left zero rather than reported: this is a
	// diagnostic read, and refusing to answer "which replica holds this
	// session" because one field is unparseable helps nobody.
	if ms, convErr := strconv.ParseInt(fields[fieldLastSeen], 10, 64); convErr == nil {
		out.LastSeen = time.UnixMilli(ms).UTC()
	}
	return out, done(nil)
}

// Subscribers reports the fleet-wide subscriber count for one channel.
//
// IT IS AN ESTIMATE AND NOTHING ROUTES ON IT. doc.go carries the full argument:
// it drifts upward when a replica dies without running its close path and the
// matching decrement never happens, and the key's TTL is what bounds that drift.
// Use it for a gauge or an operator answer, never for a decision.
//
// An absent counter is zero, not an error — that is [redis.IsKeyNotFound]'s
// purpose. A negative stored value is clamped to zero rather than reported, so a
// drifted counter can never produce a nonsensical gauge.
func (s *Store) Subscribers(ctx context.Context, channel string) (int64, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	if err := validateChannel(channel); err != nil {
		return 0, err
	}
	rdb, err := s.conn()
	if err != nil {
		return 0, err
	}

	done := s.begin(OpSubscribers)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	n, err := rdb.Get(callCtx, s.channelKey(channel)).Int64()
	switch {
	case redis.IsKeyNotFound(err):
		return 0, done(nil)
	case err != nil:
		return 0, done(s.fail(ctx, OpSubscribers, "", err))
	case n < 0:
		return 0, done(nil)
	}
	return n, done(nil)
}

// -----------------------------------------------------------------------------
// Lifecycle: connect, heartbeat, disconnect, forget
// -----------------------------------------------------------------------------

// Connected records that this replica now holds connectionID for a session.
//
// It writes the session hash, adds the connection to this replica's presence set
// and refreshes the subscription set's TTL — the last of those is what makes a
// resumed session's channels survive the reconnect that just happened.
//
// The whole thing is one MULTI/EXEC transaction rather than a plain pipeline. A
// plain pipeline batches round trips; MULTI/EXEC is the form that delivers "not
// half-applied", and a session hash naming a replica that never got into the
// presence set is exactly the half-applied state worth preventing.
func (s *Store) Connected(ctx context.Context, sessionKey, connectionID string, authenticated bool) error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := validateSessionKey(sessionKey); err != nil {
		return err
	}
	if err := validateConnectionID(connectionID); err != nil {
		return err
	}
	rdb, err := s.conn()
	if err != nil {
		return err
	}

	done := s.begin(OpConnected)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	pipe := rdb.TxPipeline()
	pipe.HSet(callCtx, s.sessionKey(sessionKey),
		fieldLastSeen, time.Now().UnixMilli(),
		fieldReplica, s.replica,
		fieldConnectionID, connectionID,
		fieldAuthedFlag, boolArg(authenticated),
	)
	pipe.PExpire(callCtx, s.sessionKey(sessionKey), s.ttl)
	pipe.SAdd(callCtx, s.presenceKey, connectionID)
	pipe.PExpire(callCtx, s.presenceKey, s.ttl)
	pipe.PExpire(callCtx, s.subKey(sessionKey), s.ttl)

	if _, err := pipe.Exec(callCtx); err != nil {
		return done(s.fail(ctx, OpConnected, sessionKey, err))
	}
	return done(nil)
}

// Touch is the heartbeat: it refreshes every TTL this session owns.
//
// The hub calls it on its ping interval. That interval must be comfortably below
// [Store.TTL] — this package cannot check that, because it cannot see the
// interval, and a heartbeat slower than the TTL silently disables
// resume-on-reconnect rather than failing.
//
// It re-adds the connection to the presence set rather than assuming it is
// there, so a presence set that expired during a Redis hiccup heals on the next
// heartbeat instead of staying wrong until the client reconnects.
func (s *Store) Touch(ctx context.Context, sessionKey, connectionID string) error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := validateSessionKey(sessionKey); err != nil {
		return err
	}
	if err := validateConnectionID(connectionID); err != nil {
		return err
	}

	rdb, err := s.conn()
	if err != nil {
		return err
	}

	done := s.begin(OpTouch)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	pipe := rdb.TxPipeline()
	pipe.PExpire(callCtx, s.subKey(sessionKey), s.ttl)
	pipe.HSet(callCtx, s.sessionKey(sessionKey), fieldLastSeen, time.Now().UnixMilli())
	pipe.PExpire(callCtx, s.sessionKey(sessionKey), s.ttl)
	pipe.SAdd(callCtx, s.presenceKey, connectionID)
	pipe.PExpire(callCtx, s.presenceKey, s.ttl)

	if _, err := pipe.Exec(callCtx); err != nil {
		return done(s.fail(ctx, OpTouch, sessionKey, err))
	}
	return done(nil)
}

// Disconnected records that this replica no longer holds connectionID.
//
// It removes the connection from fleet presence and DELIBERATELY LEAVES the
// subscription set and the session hash alone. That is the resume window: a
// client whose socket dropped reconnects within the TTL of its last heartbeat,
// lands on whichever replica the load balancer chooses, and gets its channels
// back. Deleting the set here would make every reconnect a re-listing and would
// throw away the only observable consequence of affinity-free routing.
//
// Use [Store.Forget] instead when the client closed cleanly and its state should
// not survive.
func (s *Store) Disconnected(ctx context.Context, sessionKey, connectionID string) error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := validateSessionKey(sessionKey); err != nil {
		return err
	}
	if err := validateConnectionID(connectionID); err != nil {
		return err
	}

	rdb, err := s.conn()
	if err != nil {
		return err
	}

	done := s.begin(OpDisconnected)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	pipe := rdb.TxPipeline()
	pipe.SRem(callCtx, s.presenceKey, connectionID)
	pipe.PExpire(callCtx, s.presenceKey, s.ttl)

	if _, err := pipe.Exec(callCtx); err != nil {
		return done(s.fail(ctx, OpDisconnected, sessionKey, err))
	}
	return done(nil)
}

// Forget is the clean-close teardown: the client said goodbye, so its durable
// state goes away rather than waiting out the TTL.
//
// CALLER PRECONDITION: the session has no other live connection. Only the hub
// can know that — it holds the routing table — so this package cannot check it,
// and calling Forget while a sibling connection is still streaming would drop
// that connection's stored channel set (its frames keep flowing; its resume
// window does not).
//
// It reads the stored members and then removes them through the same script
// [Store.Unsubscribe] uses, rather than DEL-ing the set: a DEL would skip every
// decrement and leave the fleet counter permanently high for each channel, which
// is exactly the drift doc.go promises to bound. A channel subscribed between
// the read and the script survives in the set and expires on its own TTL, which
// is the harmless direction to be wrong in.
func (s *Store) Forget(ctx context.Context, sessionKey, connectionID string) error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := validateSessionKey(sessionKey); err != nil {
		return err
	}
	if err := validateConnectionID(connectionID); err != nil {
		return err
	}

	rdb, err := s.conn()
	if err != nil {
		return err
	}

	done := s.begin(OpForget)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	members, err := rdb.SMembers(callCtx, s.subKey(sessionKey)).Result()
	if err != nil {
		return done(s.fail(ctx, OpForget, sessionKey, err))
	}
	// Bounded even on the teardown path: the stored set is capped, but the cap
	// is configuration and configuration changes, and a script with an unbounded
	// KEYS array is not something to discover during an incident.
	for _, chunk := range chunks(members, MaxChannelsPerCall) {
		if err := s.removeChannels(callCtx, rdb, sessionKey, chunk); err != nil {
			return done(s.fail(ctx, OpForget, sessionKey, err))
		}
	}

	pipe := rdb.TxPipeline()
	pipe.Del(callCtx, s.sessionKey(sessionKey))
	pipe.SRem(callCtx, s.presenceKey, connectionID)
	pipe.PExpire(callCtx, s.presenceKey, s.ttl)

	if _, err := pipe.Exec(callCtx); err != nil {
		return done(s.fail(ctx, OpForget, sessionKey, err))
	}
	return done(nil)
}

// Present reports the connection ids this replica currently claims.
//
// Like [Store.Subscribers] it is an operator and dashboard read, never an input
// to routing. It over-reports when a connection dies without its close path
// running inside a live replica; when the REPLICA dies nothing refreshes the key
// and the whole set expires within the TTL, which is the property that matters —
// a SIGKILLed pod leaves nothing behind.
func (s *Store) Present(ctx context.Context) ([]string, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}

	rdb, err := s.conn()
	if err != nil {
		return nil, err
	}

	done := s.begin(OpPresent)
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	members, err := rdb.SMembers(callCtx, s.presenceKey).Result()
	if err != nil {
		return nil, done(s.fail(ctx, OpPresent, "", err))
	}
	slices.Sort(members)
	return members, done(nil)
}

// -----------------------------------------------------------------------------
// Guards, validation and small helpers
// -----------------------------------------------------------------------------

// usable reports whether the store itself is still open. It is the first check
// every method makes, before argument validation, so a store outliving its owner
// says so rather than reporting whatever else it finds wrong.
func (s *Store) usable() error {
	if s.closed.Load() {
		return ErrClosed
	}
	return nil
}

// conn returns the go-redis handle.
//
// It is nil once the injected client has been closed underneath this store,
// which happens during shutdown when the composition root closes the pool before
// the last connections have drained. That classifies as [ErrUnavailable] so the
// caller's error handling is unchanged, and it is checked here — before
// [Store.begin] — so it does NOT increment the error counter: a process being
// torn down is not a Redis outage, and it is the same judgement fail makes about
// a caller whose context was cancelled.
//
// The check exists at all because the alternative is a nil-pointer panic inside
// go-redis, and CLAUDE.md §12 does not permit this package to panic.
func (s *Store) conn() (*goredis.Client, error) {
	rdb := s.client.Redis()
	if rdb == nil {
		return nil, fmt.Errorf("%w: the redis client has no connection", ErrUnavailable)
	}
	return rdb, nil
}

// maxKeySegmentLen mirrors internal/platform/redis' own key-part bound. It is
// restated rather than imported because that constant is unexported; the check
// here exists so a Replica is REFUSED rather than silently rewritten (see New).
const maxKeySegmentLen = 96

// isSafeKeySegment reports whether s would survive redis.Client.Key unchanged.
func isSafeKeySegment(s string) bool {
	if s == "" || len(s) > maxKeySegmentLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// validateSessionKey bounds the caller-supplied session key.
//
// The charset is deliberately unconstrained: the key is hashed before it becomes
// a key segment, so no byte in it can forge a segment boundary, and constraining
// the shape of a `sid` this package does not mint would be inventing a rule the
// issuer never agreed to. Only the length is bounded, so a pathological caller
// cannot hand this package a megabyte to hash.
func validateSessionKey(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: session key is empty", ErrInvalidArgument)
	case len(s) > MaxSessionKeyLen:
		return fmt.Errorf("%w: session key is %d bytes, over the %d limit",
			ErrInvalidArgument, len(s), MaxSessionKeyLen)
	}
	return nil
}

// validateConnectionID bounds and charset-checks the connection id.
//
// Unlike the session key it is stored verbatim — as a presence set member and a
// session hash field — and it is echoed into log lines, so control bytes and
// whitespace are refused: a newline in a log field is how a log becomes forgeable.
// It is server-generated, so a violation is a bug in the hub, not client input.
func validateConnectionID(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: connection id is empty", ErrInvalidArgument)
	case len(s) > MaxConnectionIDLen:
		return fmt.Errorf("%w: connection id is %d bytes, over the %d limit",
			ErrInvalidArgument, len(s), MaxConnectionIDLen)
	case !isPrintableASCII(s):
		return fmt.Errorf("%w: connection id carries whitespace or a control byte", ErrInvalidArgument)
	}
	return nil
}

// validateChannels bounds, checks and DEDUPLICATES one request's channel list.
//
// # This package does not know the channel grammar
//
// `event:{id}`, `market:{id}` and `league:{slug}` are internal/wsgw's rules, and
// the hub parses them with domain.NewEventID, domain.NewMarketID and
// domain.NewSlug before anything reaches this adapter. Re-deriving the grammar
// here would be a second definition of the same rule, and the two would drift —
// the same argument internal/pricing/payload.go makes about re-shaping a
// document that already has a shape. What is enforced here is what THIS layer
// owns: the value is non-empty, bounded, and free of bytes that have no business
// in a Redis key or a log line.
//
// # Deduplication is not tidiness
//
// The subscribe script counts a channel as new by SISMEMBER, so a list carrying
// the same channel twice would pass the membership test twice and increment the
// fleet counter twice for one member. Deduplicating in Go is the cheapest place
// to make that impossible; order is preserved so an error message names channels
// in the order the client sent them.
func validateChannels(channels []string) ([]string, error) {
	switch {
	case len(channels) == 0:
		return nil, fmt.Errorf("%w: no channels given", ErrInvalidArgument)
	case len(channels) > MaxChannelsPerCall:
		return nil, fmt.Errorf("%w: %d channels in one call, over the %d limit",
			ErrInvalidArgument, len(channels), MaxChannelsPerCall)
	}

	out := make([]string, 0, len(channels))
	seen := make(map[string]struct{}, len(channels))
	for _, c := range channels {
		if err := validateChannel(c); err != nil {
			return nil, err
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

// validateChannel checks one channel name. See validateChannels for why the
// grammar itself is not checked here.
func validateChannel(c string) error {
	switch {
	case c == "":
		return fmt.Errorf("%w: channel name is empty", ErrInvalidArgument)
	case len(c) > MaxChannelLen:
		// The length is quoted, the VALUE is not: an over-long channel name is
		// untrusted input and echoing it whole into an error that reaches a log
		// is how a log becomes an attack surface (internal/domain/ids.go makes
		// the same call with its `sample` helper).
		return fmt.Errorf("%w: channel name is %d bytes, over the %d limit",
			ErrInvalidArgument, len(c), MaxChannelLen)
	case !isPrintableASCII(c):
		return fmt.Errorf("%w: channel name carries whitespace or a control byte", ErrInvalidArgument)
	}
	return nil
}

// isPrintableASCII reports whether s is entirely printable ASCII with no space.
// Space is excluded along with the control bytes because a space is what turns
// one log field into two.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// boolArg renders a flag the way it is stored: "0" or "1", so redis-cli shows
// something a human reads the same way every time.
func boolArg(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// decodeTriple reads the {status, stored, added} reply the subscribe script
// returns.
//
// The shape is a contract between the script above and this function. A mismatch
// is a programming error and is REPORTED rather than guessed at: guessing would
// mean silently deciding whether a cap was hit, which is the one thing the reply
// exists to say.
func decodeTriple(v any) (status, stored int64, err error) {
	vals, ok := v.([]any)
	if !ok || len(vals) != 3 {
		return 0, 0, fmt.Errorf("subscribe script returned %T with %d elements, want 3", v, sliceLen(v))
	}
	nums := make([]int64, 3)
	for i, raw := range vals {
		n, ok := raw.(int64)
		if !ok {
			return 0, 0, fmt.Errorf("subscribe script element %d is %T, want int64", i, raw)
		}
		nums[i] = n
	}
	return nums[0], nums[1], nil
}

func sliceLen(v any) int {
	if s, ok := v.([]any); ok {
		return len(s)
	}
	return 0
}

// chunks splits s into slices of at most n. It exists for Forget's teardown of a
// set larger than one call may carry.
func chunks(s []string, n int) [][]string {
	out := make([][]string, 0, (len(s)+n-1)/n)
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

func durationOr(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

func intOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

// -----------------------------------------------------------------------------
// Error classification and degraded-mode logging
// -----------------------------------------------------------------------------

// fail classifies a failed Redis call and, when it is an outage, logs it under
// the rate limiter.
//
// A CALLER WHOSE OWN CONTEXT IS DONE IS NOT AN OUTAGE. During shutdown, and
// whenever a client disconnects mid-heartbeat, the connection's context is
// cancelled and every in-flight call fails. The hub counts an [ErrUnavailable]
// into sharpline_ws_presence_errors_total, so classifying those as unavailable
// would make that series spike on every deploy and make the one signal that says
// "Redis is degrading" say "somebody pressed terminate" instead. The store's OWN
// timeout expiring is a different thing entirely and does classify as
// unavailable, which is why the parent context is what is inspected here rather
// than the error alone.
func (s *Store) fail(parent context.Context, op, sessionKey string, err error) error {
	if parent.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return fmt.Errorf("redispresence: %s: %w", op, err)
	}

	wrapped := fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
	if ok, suppressed := s.warn.allow(time.Now(), warnEvery); ok {
		// The session key is logged as its DIGEST, never raw: it may be a `sid`
		// and this package's whole posture is that a session identifier does not
		// belong in cleartext anywhere it can be read later (doc.go).
		attrs := []any{
			slog.String("op", op),
			slog.String("replica", s.replica),
			slog.String("error", err.Error()),
		}
		if sessionKey != "" {
			attrs = append(attrs, slog.String("session", hashSession(sessionKey)))
		}
		if suppressed > 0 {
			attrs = append(attrs, slog.Int64("suppressed", suppressed))
		}
		s.log.Warn("websocket subscription state is degraded; sockets are unaffected "+
			"and resume-on-reconnect is unavailable until redis recovers", attrs...)
	}
	return wrapped
}

// warnLimiter emits at most one line per interval and reports how many it
// swallowed since the last one, so an outage costs a bounded number of lines
// without hiding its scale. See warnEvery.
type warnLimiter struct {
	last       atomic.Int64 // unix nanoseconds of the last emitted line
	suppressed atomic.Int64
}

func (w *warnLimiter) allow(now time.Time, every time.Duration) (bool, int64) {
	n := now.UnixNano()
	for {
		last := w.last.Load()
		if last != 0 && n-last < int64(every) {
			w.suppressed.Add(1)
			return false, 0
		}
		if w.last.CompareAndSwap(last, n) {
			return true, w.suppressed.Swap(0)
		}
	}
}

// -----------------------------------------------------------------------------
// Metrics
// -----------------------------------------------------------------------------

// Metric namespace and subsystem: together, the `sharpline_ws_` prefix D7 fixes.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "ws"
)

// PresenceBuckets returns the boundaries for
// sharpline_ws_presence_duration_seconds.
//
// Exported for the same reason normalizer.PipelineLatencyBuckets is: if
// internal/wsgw ever declares this series too, the descriptors must be
// IDENTICAL or the adoption below turns into a startup failure. One exported
// slice makes that mechanical instead of a copy that eventually differs.
//
// The range is internal/platform/redis' command buckets: every operation here is
// one script or one MULTI/EXEC against an in-memory server on the same network,
// so the interesting region is hundreds of microseconds and a sample past 100ms
// is a pathology rather than a slow query.
func PresenceBuckets() []float64 {
	return []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}
}

// metrics is this package's collector set. It is a value on Store, not a package
// variable: CLAUDE.md §12 forbids global mutable state, and two stores in one
// process must not silently share collectors they did not both register.
//
// # sharpline_ws_presence_errors_total is NOT here, and that is deliberate
//
// D6 requires a failed write to be counted in
// sharpline_ws_presence_errors_total{op}. That series is DECLARED AND
// INCREMENTED BY internal/wsgw, and this package neither declares nor touches
// it, for two reasons that both point the same way:
//
//  1. TWO INCREMENTERS WOULD DOUBLE-COUNT. The hub calls this store and then
//     counts the failure itself. If the store counted as well, every failed
//     heartbeat would appear twice under two different `op` values, and the rate
//     an alert reads would be exactly double the truth.
//
//  2. THE HUB HAS THE BETTER VOCABULARY. Its label says WHY the call was made —
//     restore, heartbeat, forget — because only the caller knows. This package
//     only knows which method was invoked, and "touch" is a less useful thing to
//     read on a dashboard at three in the morning than "heartbeat".
//
// What this package owes the hub instead is the CLASSIFICATION: an error
// satisfying errors.Is(err, [ErrUnavailable]) is a Redis failure worth counting,
// and anything else is not. See fail for the two exclusions that make that
// series mean Redis and nothing else.
//
// sharpline_ws_presence_duration_seconds has no second declarer, so it is owned
// here — but it still registers through the adoption path below, because a
// series named `sharpline_ws_*` is in internal/wsgw's namespace and the day
// somebody adds it there should be the day the two descriptors are reconciled,
// not the day one of them silently wins.
type metrics struct {
	duration *prometheus.HistogramVec // op
}

// newMetrics builds the collectors and registers them on reg.
//
// A nil Registerer builds them WITHOUT registering, which is right for a unit
// test and for any binary with no /metrics endpoint: the observe calls stay live
// and cost a few nanoseconds, so no call site needs a nil check.
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "presence_duration_seconds",
			Help: "Wall time of one subscription-state operation against Redis, by store method. " +
				"Nothing waits on these — the socket is served whether they succeed or not — so this " +
				"is a capacity signal for the Redis pool rather than a client-facing latency. " +
				"The `op` label names the METHOD; sharpline_ws_presence_errors_total labels the same " +
				"work by PURPOSE (restore, heartbeat), because the hub knows why it called and this " +
				"adapter does not. " +
				"Panel: histogram_quantile(0.99, sum by (le, op) " +
				"(rate(sharpline_ws_presence_duration_seconds_bucket[$__rate_interval]))).",
			Buckets: PresenceBuckets(),
		}, []string{"op"}),
	}

	if reg == nil {
		return m, nil
	}

	var err error
	if m.duration, err = sharedHistogramVec(reg, m.duration); err != nil {
		return nil, err
	}
	return m, nil
}

// begin starts the timer for one operation and returns the function that closes
// it out.
//
// Duration is observed for every call that reached this point — i.e. every call
// that actually attempted Redis, since validation and the connection guard both
// return earlier — so the histogram describes round trips and not argument
// checking. A failed attempt is observed too: a Redis that is both failing and
// slow is exactly when the latency matters.
func (s *Store) begin(op string) func(error) error {
	start := time.Now()
	return func(err error) error {
		s.metrics.duration.WithLabelValues(op).Observe(time.Since(start).Seconds())
		return err
	}
}

// sharedHistogramVec registers a contract histogram that another package in this
// process may already own, and adopts the existing collector if so.
//
// AlreadyRegisteredError is returned only for an IDENTICAL descriptor. A
// disagreement about help text or label names is a different error and fails
// startup, which is the point: two packages emitting one series must agree about
// the series, and the registry is the only place that check can be mechanical.
// The pattern, and this argument, come from internal/ingest/normalizer.
func sharedHistogramVec(reg prometheus.Registerer, c *prometheus.HistogramVec) (*prometheus.HistogramVec, error) {
	existing, err := adopt(reg, c)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return c, nil
	}
	v, ok := existing.(*prometheus.HistogramVec)
	if !ok {
		return nil, fmt.Errorf("redispresence metrics: a collector of type %T is already registered "+
			"where a *prometheus.HistogramVec was expected", existing)
	}
	return v, nil
}

// adopt returns the already-registered collector, or nil when c was registered.
func adopt(reg prometheus.Registerer, c prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(c)
	if err == nil {
		return nil, nil
	}
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		return already.ExistingCollector, nil
	}
	return nil, fmt.Errorf("redispresence metrics: %w", err)
}
