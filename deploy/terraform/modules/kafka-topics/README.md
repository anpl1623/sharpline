# `kafka-topics`

Declares Sharpline's Kafka topics. Nothing else in the repository is allowed to
create one: the broker runs with `KAFKA_AUTO_CREATE_TOPICS_ENABLE=false`
(`deploy/compose/compose.yaml`) and `make kafka-topics` is read-only.

> CLAUDE.md §9 — "Terraform owns … Kafka topics and their per-topic
> retention/compaction settings … Kafka topic configuration is exactly the kind
> of thing that gets created once with a CLI flag, forgotten, and then silently
> differs between laptop and cluster. Declaring it removes that failure mode
> entirely."

## Usage

```hcl
provider "kafka" {
  bootstrap_servers = split(",", var.bootstrap_servers)
  tls_enabled       = false
}

module "kafka_topics" {
  source = "../../modules/kafka-topics"

  bootstrap_servers   = var.bootstrap_servers
  replication_factor  = 1
  min_insync_replicas = 1
}
```

The topic catalogue is the module's own default. Callers supply only what is a
property of the **broker topology** — where it is, how many replicas it can hold.
See `variables.tf` for why the catalogue lives here rather than in each
environment root.

## Inputs

| Name | Type | Default | Notes |
|---|---|---|---|
| `bootstrap_servers` | `string` | — | Comma-separated `host:port`, same format as the app's `SHARPLINE_KAFKA_BROKERS` |
| `replication_factor` | `number` | — | No default: it is environment-specific by nature |
| `min_insync_replicas` | `number` | — | Must move together with `replication_factor` |
| `raw_providers` | `list(string)` | `["synthetic", "the-odds-api"]` | One `odds.raw.<slug>` topic each |
| `raw_topic` | `object` | see `variables.tf` | The single definition every raw topic is built from |
| `topics` | `map(object)` | the three named topics | Keyed by topic name |

## The catalogue

| Topic | Partitions | Policy | Window | Key |
|---|---|---|---|---|
| `odds.raw.synthetic` | 3 | `delete` | 72 h, ≤ 1 GiB/partition | provider-shaped |
| `odds.raw.the-odds-api` | 3 | `delete` | 72 h, ≤ 1 GiB/partition | provider-shaped |
| `odds.normalized` | 6 | `compact` | snapshot, forever | `market_id` |
| `price.computed` | 6 | `compact` | snapshot, forever | `market_id` |
| `wager.events` | 3 | `delete` | 90 d, unlimited bytes | wager-shaped |
| `signals.ev` | 3 | `delete` | 7 d, ≤ 512 MiB/partition | `market_id` |
| `signals.arb` | 3 | `delete` | 30 d, ≤ 256 MiB/partition | `market_id` |
| `signals.steam` | 3 | `delete` | 30 d, ≤ 256 MiB/partition | `market_id` |
| `signals.clv` | 3 | `delete` | 90 d, unlimited bytes | `wager_id` |

33 application partitions in total. `terraform output total_partitions` reports it.

### The signals family (phase 9)

Three of the four names come from CLAUDE.md §3's event-flow diagram
(`signals.steam | signals.arb | signals.clv`). **`signals.ev` is an addition** —
the diagram does not name it, but §6's Analytics bullet leads with the
positive-EV finder and phase 9 needs it on the bus for the same reason as the
other three.

They are declared in phase 9 rather than in phase 12 on purpose. §11 row 9 calls
the Go analytics "the reference implementation phase 12 validates against"; if
the Go detectors publish to the same topics with the same keys and the same
retention that the Flink jobs will, the cutover is a like-for-like swap that can
be diffed rather than a new pipeline that has to be trusted.

**All four are `delete`, never `compact`,** and that is the decision worth
guarding. Compaction is right when the newest record per key *supersedes* the
older ones — true of a market's current line, false of a finding. "The latest
steam move for market X" is one event, not a snapshot; the one before it is a
different event that also happened, and a consumer computing hit rates needs
both. Compacting these would do to the signal history what compacting
`wager.events` would do to the audit trail, and would do it invisibly, because
the head of the log would still look right.

These topics are not the system of record either — migration `00009_analytics.sql`
is, and its tables carry no expiry. The windows above are **replay** windows, each
sized by "how far back would someone reset a consumer group to?": a week to re-run
a slate through a corrected +EV detector, a month to audit whether reported
arbitrage and steam were real, and a season for CLV because it is measured from
the same graded legs `wager.events` keeps for 90 days.

**3 partitions each, not 6.** The co-partitioning argument that makes
`odds.normalized` and `price.computed` both 6 does not extend, because signals are
join *outputs*, not join inputs — a sink's partition count has no bearing on
whether the join upstream of it shuffles, and nothing joins two signals topics to
each other. What is left is volume (thresholded output, not a firehose) and
consumer parallelism (one low-concurrency group), and both say 3. It also keeps
`total_partitions` at 33 rather than 45 on a single-node deploy target.

## Two traps this module refuses to let you fall into

Both are enforced as plan-time `precondition`s in `main.tf`, so they apply to any
topic added later, not just the five above.

**1. A compacted topic whose cleaner never runs.** Kafka never compacts the
*active* segment. The defaults are `segment.bytes = 1 GiB` and
`segment.ms = 7 days`, so a topic producing less than a gigabyte per partition
per week has exactly one segment, that segment is always active, and compaction
**never happens at all**. The topic still answers correctly if you read to the
end — it just also carries every superseded value, unboundedly, forever. The
"compacted topic *is* the snapshot" claim quietly becomes false. Declaring a
compacted topic therefore requires `segment.ms`,
`min.cleanable.dirty.ratio`, `max.compaction.lag.ms` and `delete.retention.ms`.

**2. A retention topic whose data outlives its declared window.** Same mechanic
in reverse: deletion is per-segment and skips the active segment, so
`segment.ms > retention.ms` means the window is fiction. Declaring a
retention-based topic requires both values and enforces
`segment.ms < retention.ms`.

## Ordering

`terraform output partition_map` is the contract. A keyed topic gives **total
order per key** — all records for one key hash to one partition — and **no
ordering whatsoever across keys**. For `odds.normalized` and `price.computed` the
key is `market_id`, so:

- one market's updates arrive in production order, always;
- two markets — *including two markets on the same event* — have no defined
  relative order. Order by `observed_at` if that matters;
- same-game parlay correlation must read both legs from the state store, never
  from stream adjacency.

Partition count on a keyed topic is effectively permanent: raising it re-maps
`hash(key) % partitions`, splitting a market's history across two partitions with
no ordering relation between the halves.
