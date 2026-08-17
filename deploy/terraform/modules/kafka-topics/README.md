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

21 application partitions in total. `terraform output total_partitions` reports it.

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
