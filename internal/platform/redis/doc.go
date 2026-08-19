// Package redis is the Redis access layer every Sharpline service shares: a
// constructor-injected connection pool, a readiness check that performs a real
// round trip, a distributed token-bucket rate limiter, Prometheus metrics and
// OpenTelemetry spans.
//
// It is deliberately the same shape as internal/platform/postgres — Options,
// Connect, Close, Ping, Name/Check, a metrics struct owned by the client rather
// than a package global — so that a reader who has understood one has
// understood both.
//
// # go-redis, not rueidis or redigo
//
// github.com/redis/go-redis/v9 is pure Go. CLAUDE.md's prime directive requires
// CGO_ENABLED=0 so the service binaries are static and the runtime image can be
// gcr.io/distroless/static:nonroot with no shell and no libc. This is the same
// constraint that puts franz-go in the charter instead of confluent-kafka-go
// and pgx instead of libpq. Every serious Go Redis client is pure Go, so the
// constraint does not narrow the field here — it is restated because it is the
// reason nobody may later swap in a cgo-backed client for a benchmark win.
//
// Beyond that: go-redis exposes a Hook interface that intercepts every command
// on its way to the server. That is the exact analogue of the pgx tracer
// internal/platform/postgres installs, and it is why this package can export
// the raw client (see Client.Redis) without losing instrumentation. A wrapper
// that had to be threaded through every call site would measure only the calls
// that remembered to use it, which is the kind of metric that reads plausibly
// and is wrong.
//
// # "Never the source of truth" is a structural constraint, not a slogan
//
// CLAUDE.md §3 assigns Redis four jobs — "current-line snapshot cache,
// WebSocket presence, distributed rate limiting, idempotency keys" — and then
// says "Never the source of truth."
//
// Two rules make that hold rather than merely being written down:
//
//  1. Every read has a defined behaviour when the key is absent. Redis evicts,
//     expires, restarts empty and is deliberately not durable here (see the AOF
//     note below), so "the key is missing" is a normal operating state, not an
//     error. IsKeyNotFound exists so a caller cannot accidentally treat a cache
//     miss as a failure, and every helper in this package documents its
//     absent-key behaviour in its own doc comment.
//
//  2. Losing Redis entirely degrades the system; it never corrupts it. What
//     that means per assigned job:
//
//     Current-line snapshot cache — a miss falls through to Postgres (prices
//     hypertable) or to the compacted Kafka topic via kafka.Snapshotter. Slower,
//     correct. The cache is regenerated, never reconciled.
//
//     WebSocket presence — a lost presence set means `stream` replicas forget
//     which client subscribed to what. The protocol already handles this:
//     CLAUDE.md §5 gives every message a monotonic sequence number and "a gap
//     triggers client resync". A presence wipe is a fleet-wide resync, which is
//     expensive and visible in sharpline_ws_resyncs_total, and correct.
//
//     Distributed rate limiting — the limiter returns an error and the caller
//     decides. internal/httpapi/middleware fails OPEN by default and says why
//     at its own definition: rate limiting is an availability control, and
//     turning a Redis outage into a total API outage is a strictly worse
//     failure than briefly serving unthrottled traffic. The decision is
//     configurable, counted and logged, never silent.
//
//     Idempotency keys — a lost key means a retried request is processed twice
//     at this layer. Which is why the actual uniqueness guarantee for a wager
//     is a UNIQUE constraint in Postgres (phase 8), not this cache. Redis makes
//     the common case cheap; the database makes it correct.
//
// If a future use of Redis cannot be described in those terms, it is a use that
// belongs in Postgres.
//
// # Durability posture
//
// deploy/compose runs Redis with AOF enabled, which makes a restart lose
// approximately the last second of writes rather than everything. That is a
// convenience, not a guarantee, and nothing in this package or above it may
// depend on it. Design for the empty-Redis case; treat AOF as the thing that
// makes the empty case rare.
//
// # No repository interfaces here
//
// CLAUDE.md §12: "Interfaces are declared by the consumer, not the producer."
// This package exports a concrete Client, a concrete RateLimiter and the raw
// go-redis handle. The market, wsgw, httpapi and betting packages each declare
// the small interface they need over those.
package redis
