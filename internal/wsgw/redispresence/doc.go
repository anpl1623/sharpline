// Package redispresence is the `stream` service's Redis adapter: the DURABLE
// half of a WebSocket client's subscription state, plus fleet presence.
//
// It exists to make one sentence in CLAUDE.md §9 true — "`stream` must be
// horizontally scalable, so no session affinity, which means subscription state
// lives in Redis rather than in a pod" — without breaking the sentence in
// CLAUDE.md §3 that Redis is "never the source of truth". Those two clauses
// look like they contradict each other. They do not, and the reconciliation is
// the reason this package is shaped the way it is, so it is written down here
// rather than being rediscovered by whoever next wonders why the hub keeps its
// own map.
//
// # The split: what is in the pod and what is in Redis
//
// THE ROUTING TABLE IS NECESSARILY IN THE POD. The thing a delta is delivered
// to is a live TCP socket, and a socket exists in exactly one process. No amount
// of Redis changes that: a hub that had to ask Redis which of its own
// connections cared about a market would be doing a network round trip to learn
// something it already holds in memory, on the hot path of every price change,
// ten thousand times over. internal/wsgw keeps that map.
//
// WHAT LIVES HERE IS THE DURABLE SUBSCRIPTION SET AND FLEET PRESENCE — the
// channel set a session asked for, keyed by session rather than by socket, and
// which replica currently holds which connection. Its single job is this: a
// client whose connection drops and who reconnects onto a DIFFERENT replica has
// its channel set restored, is sent a correct snapshot, and never has to re-list
// its channels. That is precisely what makes affinity-free load balancing
// observable rather than merely claimed — without it, "no session affinity"
// would be a property nobody could see from the client side.
//
// REDIS IS NEVER CONSULTED FOR THE CONTENT OF A SNAPSHOT. The markets in a
// snapshot come from the hub's fold of the compacted `price.computed` topic and
// from nowhere else. CLAUDE.md §3 chose a compacted topic specifically because
// "a compacted topic keyed by market_id IS the current-line snapshot […] which
// removes a whole class of cache-coherency bugs between the bus and Redis".
// Caching market state here would reintroduce exactly that bug class. This
// package therefore stores channel NAMES and never a price, a market, or
// anything derived from one.
//
// So: Redis is authoritative for nothing. Lose the entire keyspace and every
// client still receives correct prices on every channel it re-lists; what is
// lost is the convenience of not having to re-list them.
//
// # Keyspace
//
// Every key goes through [redis.Client.Key], so every key carries the
// deployment's namespace prefix and every segment is sanitised — a segment
// cannot forge a ':' and address a namespace that is not its own.
//
//	ws:sub:{session}        SET    channel names this session subscribed to
//	ws:sess:{session}       HASH   last_seen, replica, connection_id, authenticated
//	ws:presence:{replica}   SET    connection ids currently held by this replica
//	ws:chan:{channel}       STRING integer count of fleet-wide subscribers
//
// {session} is NOT the caller's session key. It is a 128-bit SHA-256 digest of
// it — see "No secret is ever a key or a value" below.
//
// {channel} is the channel name as [redis.Client.Key] renders it. A ':' has no
// place inside a key segment — it is the separator — so the builder maps it to
// '_' and appends a short FNV-1a digest of the ORIGINAL string:
// `market:evt-1` becomes `…:ws:chan:market_evt-1.7a1c…`.
//
// The digest is not decoration, and it is the reason the channel is not
// pre-flattened into something prettier. This package deliberately does not
// validate the channel grammar (that is the hub's job — see validateChannels),
// so `market:evt-1` and the malformed `market_evt-1` can both arrive; a plain
// substitution would map them onto ONE counter, and a counter silently shared by
// two channels is wrong in a way nobody would think to look for. Because the
// digest is taken over the original, the mapping is injective by construction —
// and the readable stem survives at the front, so an operator debugging fanout
// can still run `KEYS sharpline:ws:chan:market_evt-1.*`.
//
// # Degradation, stated per failure and implemented
//
// LOSING REDIS DEGRADES; IT NEVER CORRUPTS. Nothing in this package may sit on
// a path whose failure closes a socket, and every method returns an error the
// caller is entitled to ignore.
//
//	Redis unreachable at connect   The connection is STILL SERVED. Subscription
//	                               state is pod-local for the life of that
//	                               socket; resume-on-reconnect is unavailable
//	                               and nothing else changes.
//	Redis unreachable mid-life     Writes fail and are reported as
//	                               [ErrUnavailable], which the hub counts into
//	                               sharpline_ws_presence_errors_total{op}. This
//	                               package logs them at WARN, rate limited, so an
//	                               outage costs one line per interval rather than
//	                               one per heartbeat per client. The socket is
//	                               unaffected.
//	Presence wiped                 A fleet-wide resync: every reconnecting
//	                               client re-lists its channels. Expensive,
//	                               correct, and visible in
//	                               sharpline_ws_resyncs_total.
//
// The caller distinguishes the two error classes with [errors.Is]:
//
//	errors.Is(err, ErrUnavailable)     Redis is degrading. Count it, log it,
//	                                   carry on serving the socket.
//	errors.Is(err, ErrInvalidArgument) The caller passed nonsense. Refuse the
//	                                   client's request; do not count it as an
//	                                   outage.
//	errors.Is(err, ErrTooManyChannels) The session is at its channel cap. A
//	                                   protocol-level refusal, not a failure.
//
// # Absent is normal, and is never an error
//
// A session with no stored channels returns an empty slice and a nil error. It
// is a client that has not subscribed yet, and it is deliberately
// INDISTINGUISHABLE from a Redis that restarted empty — which is the whole
// point. internal/platform/redis' package doc states the rule this obeys:
// "Every read has a defined behaviour when the key is absent […] a cache miss
// is a normal operating state, not an error." [redis.IsKeyNotFound] exists so a
// caller cannot accidentally treat one as a failure, and it is used here for
// the only read that can produce one ([Store.Subscribers]).
//
// # TTL, always
//
// EVERY key this package writes has an expiry. A replica that is SIGKILLed —
// no defer runs, no close frame is sent — must not leave a subscription set
// behind for ever, and a Redis whose memory grows monotonically with every
// client that ever connected is a slow outage rather than a cache. The TTL is
// refreshed by [Store.Touch] on the hub's heartbeat, so the natural expiry is a
// few missed heartbeats: at the phase-6 defaults (a 20s ping interval and
// [DefaultTTL] of 90s) that is four and a half.
//
// The TTL is also the resume window. A client that reconnects within TTL of its
// last heartbeat gets its channels back; one that reconnects later re-lists
// them. Both are correct; only the second costs a round trip.
//
// # No secret is ever a key or a value
//
// The session key is derived by the hub from the JWT's `sid` claim, or — for an
// anonymous connection, which CLAUDE.md §6 makes the DEFAULT for market data —
// from a server-generated opaque id. It is never the token.
//
// This package does not take that on trust. It hashes whatever it is given with
// SHA-256 and uses 128 bits of the digest as the key segment, so that:
//
//   - no caller mistake can put a bearer token into a Redis key in cleartext,
//     where it would land in a keyspace dump, a MONITOR trace, or an RDB file
//     shipped to a laptop for debugging;
//   - the key segment is fixed-length, so its size does not depend on the
//     caller at all;
//   - a Redis snapshot carries no session identifiers, only opaque digests.
//
// That is defence in depth, NOT a licence to pass a token: a digest of a bearer
// token would still be a capability if anything here trusted it, and nothing
// does — the value is used as a name and never as an authorisation. The real
// rule is still that the hub passes a `sid`.
//
// The stored VALUES are equally boring by construction: channel names, a
// replica hostname, a server-generated connection id, a boolean, and a
// millisecond timestamp. Nothing credential-adjacent is written anywhere.
//
// # Bounds, and refusal rather than truncation
//
// An unbounded SET keyed by a session id an anonymous client can obtain on
// demand is a memory-exhaustion primitive. So the number of channels a session
// may store is capped ([DefaultMaxChannels], matching the hub's per-connection
// cap), the length of a channel name is capped ([MaxChannelLen]), the number of
// channels one call may carry is capped, and the session key and connection id
// are length-bounded.
//
// Every one of those is a REFUSAL. Truncating would silently give a client a
// subscription set different from the one it asked for, and the divergence
// would surface later as "the board is missing a market" — a bug that looks
// like a fanout defect and is not one.
//
// # Atomicity: two Lua scripts, MULTI/EXEC everywhere else
//
// [Store.Subscribe] and [Store.Unsubscribe] are Lua scripts. A pipeline was
// considered first and is genuinely insufficient for both, for two independent
// reasons:
//
//  1. THE CAP MUST BE ENFORCED AGAINST THE STORED CARDINALITY. `SCARD` from the
//     client followed by `SADD` from the client is check-then-act, and the two
//     halves race — a reconnect legitimately straddles two connections on one
//     session, and CLAUDE.md §9 runs several `stream` replicas, so the racing
//     writers are the normal case rather than the exotic one. The script makes
//     the refusal and the write one step, and refuses BEFORE writing anything,
//     so a rejected subscribe leaves the stored set byte-for-byte unchanged.
//
//  2. THE COUNTER MUST COUNT EXACTLY THE MEMBERS `SADD` ACTUALLY ADDED. `SADD`
//     reports how many members were new but not WHICH, so a client doing its own
//     `INCR` per channel would double-count every re-subscribe. Only the server
//     knows which members were new, so the decision belongs on the server.
//
// Everything else — [Store.Touch], [Store.Connected], [Store.Disconnected] and
// the tail of [Store.Forget] — is a MULTI/EXEC transaction rather than a plain
// pipeline. A plain pipeline only batches round trips; MULTI/EXEC is the form
// that actually delivers "not half-applied", which is the property being asked
// for, and on a single-node Redis it costs nothing extra.
//
// The scripts declare every key they touch in KEYS, which is the Redis
// convention and the thing Redis Cluster's slot analysis needs. This deployment
// is a single Redis StatefulSet (CLAUDE.md §9), so multi-slot scripts are not a
// problem today; building key names inside the script from a prefix would have
// been shorter and would have made a future Cluster migration a rewrite, so it
// was not done.
//
// # The fleet subscriber counter is an ESTIMATE, and says so
//
// `ws:chan:{channel}` answers "how many sockets across the fleet care about this
// market". NOTHING ROUTES ON IT. It is not consulted to decide a snapshot, a
// delta, a subscription or a close; it exists so an operator and a dashboard can
// see which markets the fanout is actually spending its budget on.
//
// It drifts, in both directions, and the drift is bounded rather than hidden:
//
//   - UPWARD when a replica dies without running its close path, because the
//     matching decrement never happens. Bounded by the key's own TTL, which is
//     refreshed only by subscribe/unsubscribe traffic — a channel nobody
//     subscribes to or leaves for a TTL disappears and starts again from the
//     truth it can observe.
//   - DOWNWARD when the key expires while subscribers remain, after which a
//     decrement would go negative. [Store.Subscribers] clamps at zero and the
//     script deletes a counter that reaches zero, so a negative value is never
//     stored and never reported.
//
// A sorted set scored by last-seen would be exact and self-healing, at the cost
// of one ZADD per channel per heartbeat per connection — which at CLAUDE.md
// §10's target of 10k subscribers is tens of thousands of writes every heartbeat
// to keep a number nothing depends on accurate. Declined deliberately; the
// estimate is worth what it costs and the honest documentation is the other half
// of that trade.
//
// # Metrics
//
// This package owns exactly one series:
//
//	sharpline_ws_presence_duration_seconds{op}  histogram, PresenceBuckets()
//
// `op` is a closed set — the Op* constants — and names the METHOD that was
// called.
//
// D6's sharpline_ws_presence_errors_total{op} IS emitted by the `stream` binary,
// but it is declared and incremented by internal/wsgw, not here. Two
// incrementers would double-count every failure under two different `op` values,
// and the hub's vocabulary is the better one because only the caller knows
// whether a Channels read was a reconnect RESTORE or an operator poking at a
// session. What this package supplies instead is the classification the hub
// counts on: errors.Is(err, [ErrUnavailable]).
//
// The one series that is owned here still registers through the
// [prometheus.AlreadyRegisteredError] adoption pattern that
// internal/ingest/normalizer established, because `sharpline_ws_*` is
// internal/wsgw's namespace: if that package ever declares an identical
// descriptor the two land on one collector instead of failing startup, while a
// descriptor that DISAGREES about help text, labels or buckets still fails
// loudly, which is the behaviour worth keeping.
package redispresence
