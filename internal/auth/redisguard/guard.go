// Package redisguard is the Redis implementation of internal/auth's
// [auth.ReplayGuard]: the store that burns a TOTP step so a code cannot be used
// twice inside its own 30-second window.
//
// # Why this is not auth.MemoryReplayGuard
//
// The in-process guard in internal/auth is a map, and a map is per-pod. CLAUDE.md
// §9 requires the deployment to scale without session affinity — "subscription
// state lives in Redis rather than in a pod" — and the moment `api` has two
// replicas, a code burnt on pod A is still fresh on pod B. Two replicas plus an
// in-memory guard is a replay window sized by the number of pods, and nothing
// about it is visible in a test that runs one process.
//
// CLAUDE.md §3 assigns exactly this workload to Redis: "current-line snapshot
// cache, WebSocket presence, distributed rate limiting, IDEMPOTENCY KEYS. Never
// the source of truth." A burnt TOTP step is an idempotency key in the precise
// sense — it makes a repeated presentation of one code a no-op — and it is
// correctly not a source of truth: losing the whole keyspace degrades the
// control back to "a code is valid for its 30 seconds" rather than breaking
// authentication.
//
// # The whole implementation is SET NX PX
//
//	SET <key> 1 NX PX <ttl>
//
// One round trip, atomic on the server, and the reply distinguishes "I created
// it" from "it already existed" — which is exactly the question
// [auth.ReplayGuard.Consume] asks. There is no read-then-write and therefore no
// race between two pods presenting the same code in the same millisecond.
package redisguard

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/redis"
)

// keyScope is the first key segment, so every entry this package writes is
// greppable in redis-cli and cannot collide with the rate limiter's keyspace.
const keyScope = "auth:totp:step"

// maxTTL bounds how long an entry may live.
//
// It is a guard against a caller passing a far-future expiry and pinning
// memory: a burnt step only needs to outlive the window in which its code could
// still verify, which is a couple of minutes at the default skew. Ten minutes is
// generous headroom and still bounded.
const maxTTL = 10 * time.Minute

// Guard implements auth.ReplayGuard against Redis.
type Guard struct {
	client *redis.Client
	now    func() time.Time
}

// Options configures [New].
type Options struct {
	// Client is the connected Redis client. Required.
	Client *redis.Client

	// Now is the clock seam. Nil means time.Now. It is used only to turn an
	// absolute expiry into the relative TTL Redis wants, so a test can assert
	// the TTL without waiting for one.
	Now func() time.Time
}

// New builds a Guard.
func New(opts Options) (*Guard, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("%w: redis replay guard needs a client", auth.ErrInvalid)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Guard{client: opts.Client, now: now}, nil
}

// Consume implements auth.ReplayGuard.
//
// # Failing closed
//
// A Redis failure returns an error rather than true. The caller —
// auth.Service.consumeTOTPCode — turns that into a failed second factor, so an
// outage in the guard makes 2FA logins fail rather than making the replay
// protection silently disappear. That is the correct direction for a security
// control: a user who cannot log in files a ticket; a replay window nobody can
// see does not.
//
// # The key
//
// (scope, user id, step). The user id goes through redis.Client.Key, which
// sanitises it — a colon in a key part could otherwise forge a segment boundary
// and address another user's namespace. auth's user ids cannot contain a colon
// by construction, so the sanitiser is defence in depth rather than the only
// thing standing between two users' steps.
//
// The VALUE is a constant "1" and carries nothing. It is deliberately not the
// code, not the user id and not a timestamp: the key already encodes everything
// the decision needs, and a Redis instance that is dumped or replicated should
// not carry a second copy of anything credential-adjacent.
func (g *Guard) Consume(
	ctx context.Context, user domain.UserID, step int64, expiry time.Time,
) (bool, error) {
	ttl := expiry.Sub(g.now())
	switch {
	case ttl <= 0:
		// An expiry already in the past. The step cannot be burnt for any
		// useful duration, and writing a key with a non-positive TTL is a
		// Redis error rather than a no-op — so this is refused explicitly
		// instead of being turned into an unbounded key.
		return false, fmt.Errorf("%w: replay guard expiry %s is not in the future",
			auth.ErrInvalid, expiry.Format(time.RFC3339))
	case ttl > maxTTL:
		ttl = maxTTL
	}

	key := g.client.Key(keyScope, user.String(), fmt.Sprintf("%d", step))

	created, err := g.client.Redis().SetArgs(ctx, key, "1", goredis.SetArgs{
		Mode: "NX",
		TTL:  ttl,
	}).Result()
	if err != nil {
		// SET ... NX returns redis.Nil when the key already existed and the
		// write was therefore skipped. That is the REPLAY case, not a failure,
		// and treating it as one would turn every replay into a 500.
		if errors.Is(err, goredis.Nil) {
			return false, nil
		}
		return false, fmt.Errorf("redisguard: burn TOTP step: %w", err)
	}
	// A non-empty reply means the SET was applied, i.e. this caller created the
	// key and is the first to present this code.
	return created == "OK", nil
}
