# ADR 0008: The WebSocket gateway — a groupless bus read, per-connection sequencing, a subprotocol-carried bearer, and Redis for subscriptions only

- **Status:** Accepted
- **Date:** 2026-08-19
- **Charter reference:** CLAUDE.md §3, §5, §6, §9

## Context

`stream` is the last hop of the pipeline `provider → ingest → normalizer → pricer → stream →
browser`. CLAUDE.md §3 gives it one job — "WebSocket gateway. Subscription routing,
snapshot-then-delta protocol, per-client backpressure" — and §5's *Fanout* paragraph turns
that into a list of requirements: channel subscription, a snapshot followed by deltas only,
a monotonic sequence number on every message, a gap that triggers a client resync, a bounded
per-client send queue that drops the client's buffer rather than the hub's throughput, and
heartbeat ping/pong with idle reaping.

Four constraints bound the design, and each of them is what makes an obvious answer wrong.

**The service must scale horizontally with no session affinity.** CLAUDE.md §9 is explicit:
"`stream` must be horizontally scalable, so no session affinity, which means subscription
state lives in Redis rather than in a pod." `deploy/proxy/Caddyfile` implements that with
`lb_policy round_robin` and a comment saying a future edit to `cookie` or `ip_hash` reads as
a charter violation. A client's socket therefore lands on an arbitrary replica, and the next
one may land on a different replica.

**Redis is never the source of truth.** CLAUDE.md §3 says so, and ADR 0004 spent its
strongest argument on why: a compacted topic keyed by `market_id` *is* the current-line
snapshot, which removes the class of cache-coherency bug where the bus and a snapshot store
disagree and a stale price appears on screen with no obvious cause. Any design that
re-derives a price from Redis puts that bug class straight back.

**Market data is public.** CLAUDE.md §6 puts the catalogue read surface — the odds board,
event detail, line history, multi-book comparison — in the public tier, and
`internal/httpapi/middleware` declines to require an identity chain-wide for exactly that
reason. A gateway that demanded a token to watch a price move would make the landing page
impossible. But the gateway must still be able to *know who a connection belongs to*, both
for the account-shaped stream that arrives later and for the durable subscription set below.

**The target is ten thousand concurrent subscribers on one node** (CLAUDE.md §10), driven by
a Locust worker pool against a custom-metric HPA. That number is not decoration: it
disqualifies any per-subscriber cost that is not O(1), and it makes memory-per-connection a
design parameter rather than an afterthought.

Nothing would decide itself. Left undecided, the defaults are all wrong in the same
direction — a consumer group, a hub-wide counter, a token in the query string, and a
snapshot read from Redis are each the path of least resistance, and each fails in a way that
produces no error anywhere.

## Decision

### D1 — the bus read is groupless, and the replay *is* the snapshot

`stream` reads the compacted topic `price.computed` through a new third consume shape,
`kafka.Follower`: **direct partition assignment, no consumer group, no committed offsets,
reset to the start of the log, continuing into the live tail on the same poll loop.**
Every replica reads every record. A replica reports ready only once, for every partition,
the next offset to read has reached the end offset listed when its run began.

There is consequently **no `Snapshotter` in this service and no snapshot phase**. A client's
snapshot is taken from the hub's in-memory fold of that log, not from a second read of the
bus, so there is no window between "the snapshot ended at offset N" and "the live consumer
started at offset M" in which a record is either dropped or applied twice.

### D2 — the state mutex is the linearisation point

The hub holds the slate in memory. A client's snapshot is read and its subscription is
registered in **one critical section under the same write lock that serialises delta
publication**, so no delta can be published between the read and the registration. The lock
is the linearisation point; the invariant is enforced by the API shape (`Store.Attach` takes
a callback) rather than by a comment a future caller might not honour.

### D3 — the sequence number is per connection and assigned at enqueue

A `uint64` starting at 1, advancing for **every** frame the server puts on a connection's
bounded send queue — `hello`, `ack`, `snapshot`, `delta`, `resync`, `error` and `pong`
alike — and stamped **at enqueue, never at write**.

Each connection announces its own `connection_id` in its `hello` frame and the client resets
its expectation per connection, so a reconnect starting again at 1 is not an epoch problem.

### D4 — backpressure removes the client's buffer, never the hub's throughput

Per-connection bounded queue (256 frames by default). Publication is a non-blocking send
with a default branch; on overflow the client's **entire** pending buffer is discarded, the
drop and the resync are counted under `reason="slow_consumer"`, and a `resync` frame is
enqueued whose sequence number continues from the highest already assigned.

### D5 — a bearer token rides in the subprotocol offer, and never in a URL

An anonymous connection is legal and is the default. When a token is presented it is
verified by **the same pinned verifier `cmd/api` uses** — `auth.TokenIssuer`, which takes the
signing algorithm from configuration and ignores the token's own `alg` header. There is
exactly one verifier in this repository; `internal/wsgw` declares a one-method
`TokenVerifier` and `cmd/stream` adapts the issuer to it in one function, mirroring
`cmd/api`'s `newAuthenticator`.

The credential is presented as an RFC 6455 subprotocol offer: the client offers
`["sharpline.v1", "sharpline.bearer.<jwt>"]` and the server selects and echoes **only**
`"sharpline.v1"`. `Authorization: Bearer` is also accepted, for clients that can set headers
(`pkg/client`, the tests, curl).

A token in the **query string is refused explicitly** — the connection is upgraded and then
closed with a policy violation naming the supported mechanisms. A token that is presented
and fails to verify closes the connection; it is never downgraded to anonymous. A gateway
with no verifier configured serves anonymous traffic in full and **refuses** any presented
credential.

The audience is `auth.DefaultAudience` ("sharpline-api"), because that is what the API mints
today.

### D6 — Redis holds the durable subscription set, and is still not the source of truth

The two charter statements are reconciled, not traded off:

- **The routing table** — the map from channel to live socket that decides which connections
  receive a frame — is necessarily in the pod. The socket is in the pod. Redis has no part
  in it.
- **What lives in Redis** is the durable subscription set per client session, plus fleet
  presence, so that a client reconnecting onto a *different* replica has its channel set
  restored and receives a correct snapshot without re-listing its channels.

Redis is never consulted to decide the **content** of a snapshot. That comes from the
compacted topic, always.

Keys, all namespaced through `redis.Client.Key`: `ws:sub:{session}` (the channel set),
`ws:sess:{session}` (last seen, replica, connection id, authenticated),
`ws:presence:{replica}` (connection ids), `ws:chan:{channel}` (fleet-wide subscriber count).
Every one of them carries a TTL refreshed on heartbeat.

Degradation is stated per case rather than as "Redis is optional": unreachable mid-life means
writes fail, are counted in `sharpline_ws_presence_errors_total{op}`, are logged at WARN
(rate-limited), and the socket is untouched; a wiped keyspace costs clients one re-subscribe
on their next reconnect and is visible in `sharpline_ws_resyncs_total`.

### D8 — the priced document is carried through byte for byte

A `delta` carries the `pricing.ComputedMarket` document exactly as it appears on
`price.computed`, as `json.RawMessage`. It is never re-marshalled and never re-shaped. The
hub renders the shared body **once** per publish and each connection prepends only its own
`{"seq":N,` prefix.

## Consequences

**What this buys.**

- Any replica can answer for any market, so `lb_policy round_robin` is correct rather than
  merely tolerated, and `docker compose up --scale stream=2` is a demonstration rather than a
  hazard.
- There is no snapshot/delta race anywhere: not at the bus (D1 removes the second reader) and
  not at the socket (D2 removes the window).
- A dropped buffer is **visible on the wire** as a sequence gap, so the resync CLAUDE.md §5
  requires can actually be triggered by a client rather than only inferred from a server log.
- A credential never enters a URL, so it never reaches an access log, a `Referer`, browser
  history, or a link pasted into a chat window.
- `sharpline_odds_staleness_seconds{stage="fanout"}` is finally emitted, which is the
  headline SLO (CLAUDE.md §9). The recording rules `sharpline:odds_staleness_seconds:p50`
  and `:p99` have returned "No data" since phase 0 waiting for it.

**What it costs, knowingly.**

- **Offsets are not committed, so a restart re-reads the whole log.** On a compacted topic
  that is bounded by the size of the slate rather than by the age of the deployment, and the
  fold is idempotent — but it is real startup latency, it is the number a Kubernetes
  `startupProbe` must be sized against, and it grows with the slate.
- **The full slate is held in memory on every replica.** Scaling `stream` scales the memory
  bill linearly for state that is identical across pods. This is the direct price of
  affinity-free routing and it is accepted; the alternative buys memory back by making some
  markets unreachable from some pods, which is not a trade.
- **A follower has no consumer-group lag to export.** `sharpline_kafka_consumer_lag_records`
  and `_lag_seconds` are aggregated `by (group, topic)` on the dashboard and behind two alert
  rules, and a member that belongs to no group cannot honestly appear in them. The staleness
  SLO answers the same question better — in seconds of price age rather than in records — so
  no substitute gauge is invented.
- **A stale replica is invisible except through readiness.** Nothing else catches "this pod's
  fold is missing a market", because an empty snapshot is a legitimate answer for a channel
  with no markets. Refusing traffic until catch-up completes is the only defence, which makes
  the readiness gate load-bearing in a way most readiness gates are not.
- **Sequence numbers are per connection, so they cannot be used for cross-replica ordering.**
  Anything that later needs a fleet-wide order — an audit trail, a replayable client log —
  needs its own identifier and will not get one from here.
- **Redis is a declared dependency and therefore appears in `/readyz`.** A Redis outage takes
  every replica out of rotation even though D6 says it should cost only
  resume-on-reconnect. Live sockets are untouched (a readiness failure does not close them),
  but no *new* client can connect. The alternative — a readiness check that reports Redis
  informationally and always passes — was rejected: a probe that returns 200 for a dependency
  the binary declared and opened is the phase-2 defect that had `/api/readyz` answering 200
  with the database stopped. The honest place to say "this gateway can run without Redis" is
  `config.Stream`'s `Requires`, not a probe that disagrees with it.
- **The gateway verifies tokens against the API's audience.** A token scoped to the WebSocket
  gateway can therefore also place a wager. It is safe *today* only because this gateway can
  do nothing a wager could be placed with, and it stops being safe the moment that changes.
  Splitting the audience requires the API to mint a second token, which is a phase-5 surface
  change deliberately not made here.
- **Anonymous connections cannot resume.** Their session key is random and dies with the
  socket, so a returning anonymous client re-lists its channels. Fixing it would require the
  client to present its previous session id on reconnect, and the only places to put one are
  a query parameter — which is where D5 spent an entire argument saying credentials do not
  go, and a session id that restores a watchlist is close enough to one — or a third
  subprotocol offer, which is a protocol addition. It is left undone rather than done in the
  URL.

## Alternatives considered

### D1: a shared consumer group across `stream` replicas — rejected

The default, and the reason this ADR exists. A group divides a topic's partitions among its
members, so with two replicas each would hold **half the slate**. Nothing fails: a client
whose market lives on the other pod's partitions subscribes successfully, receives an empty
snapshot, and then receives nothing, for ever. Which half of the board a client can see would
depend on which pod the load balancer picked. No metric in the service catches it, no error
is logged anywhere, and the failure gets *worse* as the deployment scales — the exact
direction CLAUDE.md §9 pushes.

### D1: a unique consumer group per replica — rejected

The obvious repair, and it fixes completeness: give every pod its own group and every pod
reads every partition. It loses on two counts.

Groups **accumulate**. A pod name or a random suffix in the group id means every restart,
every rollout and every HPA scale event leaves a group behind, with committed offsets, in the
broker's `__consumer_offsets` — visible for ever in `kafka-consumer-groups --list`, in
kafka-ui, and in any `by (group)` aggregate on the dashboard. Cleaning them up is a
scheduled job nobody writes.

Worse, a group **resumes from its committed offsets**, and resuming is precisely what this
service must not do. A restarted pod would fold only the records published since its last
commit and would hold a partial slate — the same silent half-slate failure the shared group
was rejected for, arrived at by a different route and harder to see, because it appears only
after a restart. Making it correct means committing nothing and always resetting to the
earliest offset, at which point the group is doing no work and the only thing it still
contributes is the litter.

### D1: read the slate from Redis or from Postgres at startup — rejected

Faster than replaying a log, and it reintroduces exactly the bug class ADR 0004 chose a
compacted topic to eliminate: two stores that must agree, a flush or an eviction that makes
them disagree, and a stale price on screen with no obvious cause. `cmd/pricer` already
declined the same shortcut for its own warm start, and for the same reason — "the thing this
reads is the thing `stream` reads".

### D2: buffer the deltas, then read the snapshot, then reconcile — rejected

The standard answer, and the one a system with no single point of serialisation is forced
into. It works. It also adds a second failure mode that D2 does not have: the buffer is
unbounded for as long as the snapshot takes, so a client subscribing to `league:nfl` on a
busy Sunday is a memory spike whose size is set by an external party's traffic. Trading a
bounded critical section for an unbounded buffer is the wrong direction at ten thousand
subscribers on one node.

### D3: stamp the sequence number as the frame leaves for the socket — rejected

It reads as the more accurate place to stamp — the number would then describe what was
actually written. It quietly destroys the resync. D4 discards a slow client's pending
buffer; frames that never reached the socket would never have consumed numbers, so the client
would see 4, 5, 6 with nothing missing while an arbitrary stretch of the odds board silently
never arrived. Stamping at enqueue makes the same discard appear on the wire as seq 4
followed by seq 41. **A resync that cannot be triggered is not a resync**, and this is the
one line of code that decides which of the two this service has.

### D3: one hub-wide sequence counter — rejected

Simpler to reason about for a single replica, and it fails twice. Ten thousand connections
contending on one atomic is a fanout hot spot for no benefit, and the number would still not
be fleet-wide — two replicas would issue overlapping sequences to clients that cannot tell
they are on different pods, which is worse than an obviously per-connection number.

### D5: `?token=` or `?access_token=` in the WebSocket URL — rejected, and refused

Overwhelmingly the most common way this is done, because it is the only mechanism a browser
appears to offer. It is disqualified by where a URL goes: into every access log on the path
(`deploy/proxy/Caddyfile`'s `uri` field among them), into `Referer` headers, into browser
history, and into the message a developer sends a colleague when they ask why their
connection is failing.

It is not merely unsupported here — it is **refused**, with the connection closed and a
readable reason. Silently ignoring the parameter would be worse than accepting it: the client
would fall back to anonymous, the developer would see market data arriving, and they would
ship a system that puts credentials in URLs believing it worked. A refusal is the only
outcome that teaches the right thing.

### D5: a cookie — rejected

A browser sends cookies on a WebSocket handshake automatically, so it needs no client code at
all. Two reasons it loses. **The same-origin policy does not apply to `new WebSocket()`** —
a page on any origin can open one to this service and the browser will attach the user's
cookies, with no preflight and no CORS header to stop it — so a cookie-authenticated gateway
is CSRF-shaped by construction and the `Origin` check becomes the only thing standing
between an attacker's page and an authenticated stream. And the API does not mint one: phase
5 issues bearer tokens, so this would mean adding a cookie to the auth surface for the
benefit of one route.

### D5: a short-lived ticket from a REST endpoint, redeemed in the URL — rejected for now

The textbook answer, and a good one: `POST /api/v1/stream/ticket` returns a single-use,
30-second, gateway-audience credential, and *that* goes in the query string, where its
appearance in a log is harmless because it is already spent. It is genuinely better than what
was chosen, and it is not chosen because it is a **phase-5 surface change** — a new
`openapi.yaml` path, a new generated client, a new store for redemption state, and a second
token type to mint and revoke. The subprotocol offer gets the same property that actually
matters (the credential is not in the URL) with no new API surface. If a distinct
`sharpline-stream` audience is ever built, this is the mechanism to build it with, and this
paragraph is the reason it was not built first.

### D6: keep subscription state entirely in the pod — rejected

Simplest, and it is what the routing table does anyway. It breaks the reconnect story that
affinity-free routing exists to make possible: a client that reconnects onto a different
replica would have to re-list its channels, and the demo CLAUDE.md §9 is describing — a
`hello` frame with `"resumed": true` on a different pod — would not exist. Worse, the
pressure to fix it by pinning the client back to its original pod is exactly how session
affinity gets reintroduced through the back door.

### D6: keep the price snapshot in Redis too — rejected

The obvious "while we are here". It is the cache-coherency bug ADR 0004 eliminated, and it
would make Redis a source of truth in the only place where CLAUDE.md §3 says it must never
be. The routing table is in the pod because a socket is in a pod; the snapshot is in Kafka
because Kafka is where the record of a price lives. Neither of those is negotiable.
