// Package wsgw is the `stream` service: the WebSocket gateway that turns the
// compacted price.computed topic into a live board in a browser.
//
// CLAUDE.md §3 puts it at the end of the event flow
//
//	[odds.normalized] ──▶ pricer ──▶ [price.computed] ──▶ stream ──▶ browser
//
// and gives it one responsibility: "WebSocket gateway. Subscription routing,
// snapshot-then-delta protocol, per-client backpressure."
//
// # CLAUDE.md §5's Fanout paragraph, clause by clause
//
// Every sentence of it is a requirement, and each maps onto something in this
// package rather than onto a comment somewhere:
//
//	"Client subscribes to channels (event:{id}, market:{id}, league:{slug})"
//	    channel.go. Channel is a value type; ParseChannel is the only way to
//	    build one from client input, and it goes through the domain
//	    constructors so an identifier that could not name a real entity never
//	    becomes a subscription.
//
//	"receives a snapshot, then deltas only"
//	    state.go. The slate is a fold of the compacted topic, and
//	    Store.Attach reads a channel's snapshot and registers the subscription
//	    in ONE critical section, so there is no window between the two.
//
//	"Every message carries a monotonic sequence number"
//	    protocol.go. Body.Frame(seq) is the only way a frame reaches the wire,
//	    and it cannot render one without a sequence number.
//
//	"a gap triggers client resync"
//	    protocol.go's ResyncFrame carries the reason, the number of frames
//	    discarded and the [from_seq, to_seq] the client did not receive, so a
//	    resync is diagnosable from a browser console rather than only from the
//	    server's logs.
//
//	"Per-client bounded send queue — on overflow, drop the client's buffer and
//	 force a resync rather than letting one slow consumer apply backpressure to
//	 the entire hub"
//	    options.go's SendQueueCapacity, and metrics.go's
//	    sharpline_ws_clients_dropped_total{reason="slow_consumer"} — a name the
//	    alert rules already select on.
//
//	"Heartbeat ping/pong with idle reaping"
//	    options.go's PingInterval and PongTimeout, and
//	    sharpline_ws_clients_dropped_total{reason="idle_timeout"}.
//
// # What is in THIS half of the package
//
// The vocabulary the gateway speaks: the wire types, the channel algebra, the
// collector set, the identity extraction and the in-memory slate. The hub, the
// per-connection loop and the HTTP upgrade handler are built on top of it. The
// split is deliberate — everything here is a pure value type or a data
// structure with no I/O, which is what makes it testable without a socket.
//
// The rest of this comment is the set of decisions the design rests on. They
// are here rather than scattered because each of them is the kind of thing that
// gets re-argued, and because several of them look wrong until the alternative
// is spelled out.
//
// # D1 — the bus read is GROUPLESS, so there is no snapshot/delta seam at all
//
// `stream` reads the compacted topic price.computed with DIRECT PARTITION
// ASSIGNMENT: no consumer group, no committed offsets, reset to the start of
// the log, running on to the live end without a seam.
//
// The obvious alternative — a consumer group — is not merely different, it is
// WRONG here. A group splits the partitions between its members, so with two
// `stream` replicas each would hold half the slate; a client whose market lived
// on the other replica's partitions would subscribe successfully, receive an
// empty snapshot, and then receive nothing, for ever, with no error anywhere.
// That failure is invisible to every metric in this package and it scales with
// the replica count, which is exactly the direction CLAUDE.md §9 pushes: "no
// session affinity, which means subscription state lives in Redis rather than
// in a pod". Affinity-free routing REQUIRES that every replica can serve every
// market.
//
// The consequence worth stating plainly: reading a compacted topic from the
// beginning IS the snapshot (CLAUDE.md §3, "a compacted topic keyed by
// market_id *is* the current-line snapshot, replayable from scratch"), and it
// continues into the live tail without a handover. There is therefore no
// separate snapshot reader in this service and no snapshot-versus-delta race at
// the bus layer to get wrong. The only place the two words mean anything is at
// the SOCKET, which is D2.
//
// The price paid is that offsets are not committed, so a restart re-reads the
// whole log. On a compacted topic that is bounded by the size of the slate, not
// by the age of the deployment, and the fold is idempotent (see state.go), so
// the cost is a few seconds of startup and nothing else.
//
// # D2 — the state mutex is the linearisation point between snapshot and delta
//
// A client that receives a snapshot and then deltas has exactly one hazard: a
// delta published in the window between "the snapshot was read" and "this
// client is registered as a subscriber" is lost, and nothing downstream can
// tell. The market simply shows a price that stopped moving.
//
// The fix here is structural rather than compensatory. [Store] owns a mutex,
// and BOTH sides run under it:
//
//   - [Store.Fold] applies one bus record and hands the resulting delta to the
//     publish callback while still holding the write lock;
//   - [Store.Attach] reads the snapshot for each requested channel and runs the
//     subscription-registration callback while still holding the same write
//     lock.
//
// So the two are mutually exclusive by construction: no delta can be published
// between the read and the registration, because publishing requires the lock
// the registration is holding. The lock IS the linearisation point, and the
// invariant is enforced by the API shape rather than by a comment the hub might
// not honour — which is why Attach takes a callback instead of returning a
// snapshot and trusting the caller to be quick.
//
// The alternative, and why it is rejected: buffer-then-reconcile — start
// buffering deltas for the client, then read the snapshot, then replay the
// buffer, discarding anything the snapshot already reflects. It works, and it
// is what a system with no single point of serialisation has to do. It also
// introduces a SECOND failure mode this design does not have: the buffer is
// unbounded for as long as the snapshot takes, so a client subscribing to
// `league:nfl` on a busy Sunday is a memory spike whose size is set by an
// external party's traffic. Trading a bounded critical section for an unbounded
// buffer is the wrong direction on a service whose stated target is 10k
// concurrent subscribers on one node (CLAUDE.md §10).
//
// Attach takes the WRITE lock even though reading a snapshot is a read. That is
// deliberate: the callback mutates the hub's routing table, and taking the read
// lock would let two registrations race each other and force a second mutex —
// reintroducing exactly the two-lock ordering problem this design removes.
// [Store.Snapshot] exists for the read-only case (a client-requested resync)
// and takes the read lock.
//
// # D3 — the sequence number is PER CONNECTION and assigned AT ENQUEUE
//
// It is a uint64, it starts at 1, and it advances for EVERY frame the server
// puts on a connection's send queue: hello, ack, snapshot, delta, resync, error
// and pong alike.
//
// Assigned at ENQUEUE, never at write, and that is the whole point. D4 discards
// a slow client's pending buffer; if the number were stamped as the frame left
// for the socket, the discarded frames would never have consumed numbers and
// the client would see 4, 5, 6 with nothing missing — a silent hole in the odds
// board that only the server knows about. Stamping at enqueue makes the same
// discard show up on the wire as seq 4 followed by seq 41, which is a gap the
// client can SEE. A resync that cannot be triggered is not a resync, and this
// is the one line of code that decides which of those two this service has.
//
// The number is per connection rather than per hub, so there is no shared
// counter for 10k connections to contend on, and no cross-replica ordering
// problem to invent. Each connection announces its own `connection_id` in its
// `hello` frame and the client resets its expectation for each connection, so a
// reconnect starting again at 1 is not an epoch problem — it is a different
// connection.
//
// # D4 — backpressure removes the client, never the hub
//
// Each connection has a bounded send queue (options.go, default 256). The hub
// NEVER blocks on a client: publication is a non-blocking send with a default
// branch. On overflow the ENTIRE pending buffer for that client is discarded,
// [DropSlowConsumer] and [ResyncSlowConsumer] are counted, and a `resync` frame
// is enqueued whose sequence number continues from the highest already assigned
// — so the gap is visible, per D3.
//
// The alternative is to block the publisher until the slow client catches up,
// which converts one slow consumer (a laptop on hotel wifi) into fanout latency
// for every other client on the replica. CLAUDE.md §5 rules on it directly:
// "rather than letting one slow consumer apply backpressure to the entire hub".
//
// # D5 — market data is PUBLIC, and a token never travels in a URL
//
// An anonymous connection is legal and is the default. CLAUDE.md §6 puts the
// catalogue read surface — the odds board, event detail, line history, book
// comparison — in the public tier, and internal/httpapi/middleware says so in
// those words at RequireIdentity. A gateway that demanded a token to watch a
// price move would make the landing page impossible.
//
// When a token IS presented it is verified, and verified by the SAME pinned
// verifier cmd/api uses (internal/auth's TokenIssuer, which takes the algorithm
// from configuration and ignores the token's own `alg` header). There must not
// be a second verifier in this tree; auth.go declares only the one-method seam
// and cmd/stream adapts the issuer to it in one line, exactly as cmd/api does.
// The audience is auth.DefaultAudience ("sharpline-api") because that is what
// the API actually mints today. A distinct "sharpline-stream" audience is the
// better end state — it would stop a token scoped to the gateway from placing a
// wager — but it requires the API to mint a second token, which is a phase-5
// surface change deliberately not made here.
//
// PRESENTATION. The credential rides in the RFC 6455 subprotocol offer: the
// client offers ["sharpline.v1", "sharpline.bearer.<jwt>"] and the server
// selects and echoes ONLY "sharpline.v1". This is not a trick, it is the only
// request header a browser's `new WebSocket()` constructor can set — there is
// no place to put an Authorization header — and unlike a query parameter it is
// not part of the URL, so it never lands in an access log, a Referer, browser
// history, or a link somebody pastes into a chat. `Authorization: Bearer` is
// also accepted, for the clients that can set headers: pkg/client, the tests
// and curl.
//
// A token in the QUERY STRING is REFUSED, explicitly and as a distinct
// outcome — [ErrTokenInQuery], the connection closed with a policy violation
// naming the supported mechanisms. Silently ignoring it would be worse than
// accepting it: the client would fall back to anonymous, the developer would
// see market data arriving, and they would keep shipping credentials in URLs
// believing it worked. A refusal is the only outcome that teaches the right
// thing.
//
// A token that is presented and does not verify CLOSES the connection. It is
// never downgraded to anonymous — a client that believes it is authenticated
// and is quietly not is the failure internal/httpapi/middleware's
// credentialMalformed branch exists to prevent, for the same reason.
//
// # D6 — Redis holds the subscription set, and is still not the source of truth
//
// Two statements in the charter look like they conflict, and reconciling them
// is a design decision rather than a wording problem:
//
//	CLAUDE.md §9: "subscription state lives in Redis rather than in a pod"
//	CLAUDE.md §3: Redis is "never the source of truth"
//
// Both hold, because they are about different things:
//
//   - THE ROUTING TABLE — the map from channel to live socket that decides
//     which connections receive a frame — is necessarily in the pod. The socket
//     is in the pod. Nothing else is possible, and Redis has no part in it.
//
//   - WHAT LIVES IN REDIS is the DURABLE subscription set per client session,
//     plus fleet presence. It exists so that a client that reconnects and lands
//     on a DIFFERENT replica has its channel set restored and receives a
//     correct snapshot without re-listing its channels. That is precisely what
//     makes affinity-free load balancing observable rather than theoretical: a
//     `hello` frame with `"resumed": true` on a different pod is the demo.
//
// Redis is NEVER consulted to decide the CONTENT of a snapshot. The content
// comes from the compacted Kafka topic, always. That is the cache-coherency bug
// class CLAUDE.md §3 chose a compacted topic to eliminate, and re-deriving a
// price from Redis would put it straight back.
//
// Keys, all built through redis.Client.Key so they are namespaced and their
// parts sanitised:
//
//	ws:sub:{sessionKey}       SET of channel strings, TTL refreshed on heartbeat
//	ws:sess:{sessionKey}      HASH: last_seen, replica, connection_id, authenticated
//	ws:presence:{replicaID}   SET of connection ids, TTL refreshed on heartbeat
//	ws:chan:{channel}         fleet-wide subscriber counter (INCR/DECR, TTL)
//
// DEGRADATION, stated per use because "Redis is optional" is not a design:
//
//	Redis unreachable at connect   the connection is STILL SERVED. Subscription
//	                               state is pod-local for its lifetime and
//	                               resume-on-reconnect is unavailable. Market
//	                               data is public and comes from Kafka, so
//	                               refusing the connection would trade a
//	                               degraded feature for a total outage.
//	Redis unreachable mid-life     writes fail, are counted in
//	                               sharpline_ws_presence_errors_total{op}, are
//	                               logged at WARN (rate-limited), and the socket
//	                               is untouched.
//	Presence wiped                 a fleet-wide resync: expensive, correct, and
//	                               visible in sharpline_ws_resyncs_total. This is
//	                               the exact case internal/platform/redis/doc.go
//	                               already describes for this service.
//
// # D7 — the metric names are a contract that was written down before the code
//
// deploy/observability/grafana/dashboards/sharpline-overview.json and
// deploy/observability/rules/sharpline-alerts.yml both select
// `sharpline_ws_*` series by name and by exact label value. metrics.go emits
// those names and nothing beside them; the label values come from the typed
// constants in protocol.go so that a value spelled in two places cannot come to
// differ.
//
// This package also contributes stage="fanout" to the shared
// sharpline_odds_staleness_seconds histogram. That stage IS the headline SLO
// (CLAUDE.md §9), and the recording rules sharpline:odds_staleness_seconds:p50
// and :p99 read only it — they have returned "No data" since phase 0 waiting
// for this service to exist. metrics.go explains where in the fanout path the
// observation is taken, why it is once per fanout event rather than once per
// subscriber, and why sharpline_ws_write_delay_seconds exists to measure the
// gap between where it is taken and where the bytes actually leave.
//
// # D8, D9, D10, D11
//
// The wire protocol, the channel grammar, the heartbeat and the shutdown drain
// are argued at the code that implements them: protocol.go, channel.go and
// options.go respectively. They are not restated here.
//
// # No fabricated values, anywhere
//
// A subscription to a channel with no markets yields an EMPTY snapshot, and
// that is the CORRECT answer. Nothing in this package invents a placeholder
// market, a canned snapshot or a synthetic sequence number to make a screen
// look populated. Every value a client receives has travelled the real
// pipeline: state.go carries the price.computed payload through BYTE FOR BYTE
// and refuses to hold a record it cannot validate.
package wsgw
