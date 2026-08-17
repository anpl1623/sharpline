# ADR 0004: Apache Kafka over NATS JetStream, with the `franz-go` client

- **Status:** Accepted
- **Date:** 2026-08-16
- **Charter reference:** CLAUDE.md §3, §9, §14

## Context

Sharpline needs an event backbone. The pipeline is
`provider → ingest → normalizer → pricer → stream → browser`, with a Timescale writer and
(from phase 12) Flink SQL jobs tapping the same streams. The bus has to carry four
distinct topics with genuinely different retention semantics:

| Topic | Semantics |
|---|---|
| `odds.raw.{provider}` | Raw provider payloads. Retention-based; a debugging and replay artifact. |
| `odds.normalized` | Normalized market state. **Compacted, keyed by `market_id`.** |
| `price.computed` | Priced output — fair value, EV, edge. **Compacted.** |
| `wager.events` | Retention-based. The settlement audit trail. |

Two candidates were serious: **Apache Kafka** in KRaft mode, and **NATS JetStream**.

The honest starting position favours JetStream. It is one small Go binary, it has no
partition model to reason about, no consumer-group rebalancing, no offset management, and
its operational burden is close to zero. For a solo project, "nearly free" is a powerful
argument and it should not be waved away.

There is a separate, narrower question that the charter treats as non-negotiable and that
belongs in the same ADR because it is a consequence of the first: **which Go Kafka client**,
given that the container mandate (ADR 0002) requires `CGO_ENABLED=0`.

## Decision

**Apache Kafka in KRaft mode is the event backbone.** KRaft removes ZooKeeper, so this is
one container rather than a cluster of them — a meaningful part of why the operational cost
is bearable at all. (The pinned image is Kafka 4.x, which is KRaft-only; ZooKeeper support
was removed from the 4.x line entirely.)

**The Go client is `franz-go`. Never `confluent-kafka-go`.**

Kafka was chosen over JetStream on three independent grounds, each of which would be
insufficient alone and which together are decisive.

### 1. Log compaction is the right primitive for odds

This is the strongest argument and it is a design argument, not a feature-checklist one.

**A compacted topic keyed by `market_id` *is* the current-line snapshot.** Not a thing that
has to be kept in sync with the snapshot — the snapshot itself. Kafka guarantees the latest
value per key survives compaction, so the topic is a durable, replayable materialized view
of every market's current state, reconstructible from scratch by replaying from offset zero.

That removes an entire class of bug. The alternative architecture is: bus for transport,
Redis for the snapshot, and code somewhere responsible for keeping the two consistent. That
code has to handle a Redis flush, a service restart mid-stream, a consumer that fell behind
and skipped an update, and a cache entry that expired while its topic message had already
been retained past its usefulness. **Cache-coherency bugs between the bus and the snapshot
store are exactly the kind of defect that shows up as a stale price on screen with no
obvious cause**, and they are among the hardest classes of bug to reproduce.

With compaction, Redis is demoted to what §3 already says it should be: a read cache that
is never the source of truth and can be flushed at any moment with no data loss, because
the topic can rebuild it.

JetStream has `WorkQueue` and `Limits` retention and per-subject last-value semantics via
KV buckets — but the KV store is a *separate* abstraction from the stream, which
reintroduces the two-systems-to-keep-in-sync problem the compacted topic eliminates.

### 2. Flink's native source and sink is Kafka

Phase 12 replaces the Go analytics of phase 9 with Flink SQL jobs: steam detection over
hopping windows, arbitrage via interval joins, CLV as an event-time join with watermarks.

**Flink's Kafka connector is first-party, maintained in lockstep with Flink releases, and
supports exactly-once via the transactional producer.** Every documented Flink SQL example,
every `CREATE TABLE ... WITH ('connector' = 'kafka', ...)` DDL snippet, and every watermark
and offset-handling guide assumes Kafka.

Any alternative bus means a connector fighting the grain — either a community connector
with weaker guarantees, or a bridging service, either of which converts phase 12 from
"write SQL" into "debug a connector". Given that §3 already identifies Flink as the
steepest learning curve in this project and the likeliest thing to become a half-finished
distraction, adding a connector-integration problem on top of it is precisely the wrong
risk to take.

### 3. It is the most transferable infrastructure knowledge in this project

Stated plainly because it is a real input and pretending otherwise would be dishonest.
Kafka is on a large fraction of relevant job descriptions, including FanDuel's stack that
this project is framed against (§14). Partitions, consumer groups, offset management,
compaction, and rebalancing are concepts that transfer to essentially any streaming role.
JetStream's operational model, elegant as it is, transfers to fewer places.

This ground is listed **third** deliberately. If the first two arguments did not hold, this
one would not be enough — that reasoning is the same one ADR 0001 uses to reject a Java
service that exists only to name-drop a language.

### The cost, paid deliberately

**Partitions, consumer groups, offset management, and rebalancing are genuine concepts to
learn, and JetStream is nearly free by comparison.** That cost is real and it is being paid
on purpose, not overlooked.

It is also why `kafka-ui` is in the container inventory and is described in §9 as
non-negotiable while learning: inspecting topics, offsets, and consumer lag visually is the
difference between diagnosing a rebalancing problem in a day and diagnosing it in a week.
It runs in the dev profile only.

### Client: `franz-go`, and why `confluent-kafka-go` is forbidden

`confluent-kafka-go` is the more widely used client and the one most tutorials assume. It
is **disqualified here**, and the reason is a direct consequence of ADR 0002:

**`confluent-kafka-go` binds `librdkafka` through cgo.** The chain of consequences is
mechanical, not a matter of preference:

1. cgo means **`CGO_ENABLED=0` cannot be set**.
2. Without `CGO_ENABLED=0`, the build produces a **dynamically linked binary** against the
   builder image's libc.
3. A dynamically linked binary **cannot run on `gcr.io/distroless/static:nonroot`** — that
   image has no libc, no dynamic loader, and no shell. The runtime image would have to
   become `distroless/base` or a full distro, growing the image and reintroducing a CVE
   surface the static image does not have.
4. It also **couples the builder image's libc to the runtime image's**. Building on
   Alpine (musl) and running on a glibc base, or the reverse, produces a binary that fails
   at startup with a loader error — a class of failure that appears only in the container
   and never in local testing.
5. It makes **multi-arch buildx builds materially harder**, since cgo cross-compilation
   needs a cross-toolchain per target architecture. §9 requires arm64 for the dev Mac and
   amd64 for the server.

**`franz-go` is pure Go.** It keeps `CGO_ENABLED=0`, keeps the static binary, keeps the
distroless runtime image, and keeps buildx multi-arch trivial. It also supports everything
this project needs: consumer groups, transactions, exactly-once semantics, and the
admin API.

**This constraint is not optional and is not a style preference.** A pull request
introducing `confluent-kafka-go` breaks the prime directive at the image layer and is
rejected on those grounds alone, regardless of any benchmark.

## Consequences

**Made easier.**

- The current-line snapshot is durable, replayable, and rebuildable from the log. Redis
  becomes a genuine cache with no correctness responsibility.
- Phase 12 is a Flink SQL exercise rather than a connector-integration exercise.
- Terraform can declare topics with their per-topic `cleanup.policy`, `retention.ms`,
  `min.compaction.lag.ms`, and partition counts (§9). Compaction settings created once at
  a CLI and forgotten are exactly the drift Terraform exists to eliminate here.
- Consumer lag is a first-class, scrapeable metric — it goes straight onto the Grafana
  dashboard §9 requires.
- Replay is a debugging superpower: a pricing bug can be diagnosed by resetting a consumer
  group's offset and re-running the exact input.
- The static binary and distroless image survive intact, which keeps ADR 0002 whole.

**Made harder, and accepted.**

- **A real learning curve**, and the largest of any single component here. Rebalancing
  behaviour in particular is subtle: a consumer that blocks too long is evicted from the
  group, triggering a rebalance that stalls the partitions it held.
- **Partition-count decisions are effectively permanent.** Increasing partitions on a keyed
  topic breaks key-to-partition affinity and therefore ordering guarantees per market. The
  count must be chosen with the eventual scale in mind, in Terraform, up front.
- **Compaction is not immediate.** A compacted topic still contains superseded values until
  the cleaner runs, so "read the topic to get current state" means "read to the end", not
  "read one record per key". Bootstrap time for a new `stream` pod scales with the
  uncompacted tail.
- **Heavier than JetStream at rest.** The single Kafka container is the largest memory
  consumer in the compose stack after Postgres, which matters on a laptop running the full
  stack plus the observability profile.
- **Ordering is per-partition, not global.** Keying `odds.normalized` by `market_id` gives
  per-market ordering, which is the guarantee that actually matters. Cross-market ordering
  does not exist and no consumer may assume it.
- **`franz-go` is less widely deployed than `confluent-kafka-go`**, so there is less
  community troubleshooting material. Accepted without hesitation — the alternative breaks
  the container mandate, and no amount of Stack Overflow coverage compensates for that.

## Alternatives considered

### NATS JetStream — rejected, and it was close on operational cost

Genuinely excellent and materially simpler: one small Go binary, trivial clustering,
subject-based routing that maps naturally onto `event:{id}` / `market:{id}` /
`league:{slug}` subscription patterns, and a Go client that is first-party and pure Go
(so it would have satisfied the container mandate without argument).

Rejected on the three grounds above, in order of weight:

1. **No equivalent of a compacted topic as the snapshot.** The KV store provides last-value
   semantics but as a separate abstraction from the stream, which is the two-systems
   problem compaction eliminates. This was the decisive point.
2. **Flink integration is not first-party.** Phase 12 would begin with a connector problem
   rather than a windowing problem.
3. Less transferable operationally.

Worth recording that if Flink were dropped from the roadmap entirely and the snapshot lived
authoritatively in Postgres, **JetStream would probably be the better choice** and this ADR
would read differently.

### Redis Streams — rejected

Already in the stack, so it costs no new container, and it has consumer groups.

Rejected because it collapses the cache and the bus into one component with one failure
domain — precisely the coupling §3 avoids by declaring Redis "never the source of truth".
Retention semantics are weaker, there is no compaction, and persistence guarantees under
AOF are not what an audit trail like `wager.events` requires.

### Postgres as a queue (`LISTEN`/`NOTIFY`, or a polled table) — rejected

The simplest possible thing, and defensible for a system this size. Transactional
enqueue-with-write is a genuine advantage for `wager.events`.

Rejected because it does not scale to the fanout shape and it makes phase 12 impossible
without an intermediate bus. `NOTIFY` payloads are size-limited and delivery is at-most-once
with no replay. A polled outbox table would work but puts the odds firehose through the same
database that serves user queries, coupling the two under load — and load is the thing the
Locust demo is built to exercise.

### An in-process Go channel bus with no broker — rejected as the production path, retained for dev

§3's modular-monolith design explicitly supports an in-memory bus so all six services can
run in one process locally. That capability is kept.

It cannot be the production path: no durability, no replay, no cross-process fanout, and no
Flink. Retained as a development convenience and as proof that the seams are real
interfaces rather than Kafka-shaped assumptions leaking through the codebase.

### Confluent Cloud or another managed Kafka — rejected

Removes the operational burden entirely.

Rejected because it contradicts the project's premise. §9 is explicit: every workload,
stateful ones included, runs in-cluster as a StatefulSet with a PVC, "not as external
managed services", because the point is that the whole thing computes on hardware the
author controls. Outsourcing the hardest stateful component would hollow out the claim the
project exists to make.
