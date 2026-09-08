// ---------------------------------------------------------------------------
// modules/kafka-topics — inputs
// ---------------------------------------------------------------------------
// WHY THE TOPIC CATALOGUE LIVES HERE AS A DEFAULT RATHER THAN IN EACH ENV ROOT
//
// CLAUDE.md §9 states the reason Terraform earns its place: "Kafka topic
// configuration is exactly the kind of thing that gets created once with a CLI
// flag, forgotten, and then silently differs between laptop and cluster.
// Declaring it removes that failure mode entirely."
//
// Putting the four topics' partition counts and configs in `envs/local` AND
// `envs/prod` would re-create that exact failure mode one level up: two copies
// of the same catalogue, and nothing forcing them to agree. So the catalogue is
// declared ONCE, here, as the default of `var.topics` / `var.raw_topic`, and
// both env roots consume it without overriding.
//
// The module stays mechanically generic — any caller may pass its own `topics`
// map and get the same validation and the same `kafka_topic` fan-out — but the
// DEFAULT is Sharpline's catalogue, so "local and prod have the same topics" is
// a property of the code rather than of somebody's diligence.
//
// What is legitimately per-environment is only what is a property of the BROKER
// TOPOLOGY, not of the topic's semantics: `bootstrap_servers`,
// `replication_factor`, `min_insync_replicas`. Those are separate variables with
// no shared default. Partition counts and cleanup policy are semantics — they
// determine ordering guarantees and whether the snapshot property holds — and a
// topic that behaves differently on the laptop than in the cluster is the bug
// this whole file exists to prevent.
// ---------------------------------------------------------------------------

variable "bootstrap_servers" {
  description = <<-EOT
    Kafka bootstrap brokers, comma-separated — the SAME string format as the
    application's SHARPLINE_KAFKA_BROKERS environment variable (see .env.example
    → "Apache Kafka (KRaft)"). One format, one value, so Terraform and the Go
    services cannot disagree about where the broker is. Split into a list inside
    the module because the provider takes a list.
  EOT
  type        = string

  validation {
    condition     = length(compact([for s in split(",", var.bootstrap_servers) : trimspace(s)])) > 0
    error_message = "bootstrap_servers must contain at least one host:port entry."
  }

  validation {
    condition = alltrue([
      for s in split(",", var.bootstrap_servers) :
      can(regex("^[A-Za-z0-9._-]+:[0-9]{1,5}$", trimspace(s)))
    ])
    error_message = "Each bootstrap_servers entry must be host:port (e.g. kafka:9092)."
  }
}

variable "replication_factor" {
  description = <<-EOT
    Default replication factor for every topic that does not override it.

    THIS VALUE IS ENVIRONMENT-SPECIFIC BY NATURE AND IS THEREFORE NOT DEFAULTED.
    Replication factor is a property of how many brokers exist, not of what a
    topic means: the identical topic needs RF=1 against the single-container
    KRaft broker in compose and RF=3 against a three-broker cluster. Hardcoding
    either value in the module would make the module wrong in the other
    environment — and getting it wrong is not a soft failure. Asking for RF=3
    with one broker makes topic creation fail outright ("Replication factor: 3
    larger than available brokers: 1"); leaving RF=1 on a multi-broker cluster
    silently means a single broker loss loses the compacted snapshot the whole
    design leans on.

    Each env root states its own value with its own justification. See
    deploy/terraform/README.md → "Replication factor".
  EOT
  type        = number

  validation {
    condition     = var.replication_factor >= 1 && var.replication_factor == floor(var.replication_factor)
    error_message = "replication_factor must be a positive integer."
  }
}

variable "min_insync_replicas" {
  description = <<-EOT
    Topic-level `min.insync.replicas`. Also environment-specific, and it must
    move IN THE SAME COMMIT as replication_factor: RF=3 with min.isr=1 is a
    configuration that looks replicated and is not — a producer using acks=all
    is satisfied by the leader alone, so an unclean failover loses acknowledged
    writes. The durable combination is RF=3 + min.isr=2 + producer acks=all.

    With RF=1 the only legal value is 1: min.isr=2 on a single-replica topic
    makes every produce fail with NOT_ENOUGH_REPLICAS.
  EOT
  type        = number

  validation {
    condition     = var.min_insync_replicas >= 1 && var.min_insync_replicas == floor(var.min_insync_replicas)
    error_message = "min_insync_replicas must be a positive integer."
  }
}

// ---------------------------------------------------------------------------
// The raw-provider tier: `odds.raw.{provider}`, one topic per provider
// ---------------------------------------------------------------------------
// CLAUDE.md §3 names the topic `odds.raw.{provider}` — a FAMILY, not a single
// topic. The provider decision is resolved (CONTRACT.md, superseding §13.1):
// The Odds API is the real provider and a synthetic stochastic market-maker is
// the no-key fallback, selected at `ingest` startup by whether ODDS_API_KEY is
// set. Both exist today.
//
// So the provider set is data, not structure: adding a third provider is one
// string in this list, and the topic, its retention, its size cap and its
// segment policy come out identical to the other two by construction. No
// Terraform edit per provider, and no chance of a new provider's topic quietly
// getting different retention because someone copy-pasted a resource block.
variable "raw_providers" {
  description = <<-EOT
    Provider slugs that get an `odds.raw.<slug>` topic. Both entries exist in
    every environment on purpose: `ingest` picks its adapter at STARTUP from the
    presence of ODDS_API_KEY, so setting that key must not require a
    `terraform apply` before the pipeline has somewhere to publish. An unused
    raw topic costs its partitions' file handles and nothing else.
  EOT
  type        = list(string)
  default     = ["synthetic", "the-odds-api"]

  validation {
    condition     = length(var.raw_providers) > 0
    error_message = "At least one provider slug is required — `ingest` always runs one adapter."
  }

  validation {
    // Lowercase, digits and internal hyphens only. Deliberately excludes `.`
    // (which would silently graft a new level onto the odds.raw.* hierarchy)
    // and `_` (see the topic-name validation in main.tf for why mixing `.` and
    // `_` in one Kafka topic name is a metric-collision hazard).
    condition = alltrue([
      for p in var.raw_providers : can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$", p))
    ])
    error_message = "Provider slugs must be lowercase alphanumeric with internal hyphens (e.g. the-odds-api)."
  }

  validation {
    condition     = length(distinct(var.raw_providers)) == length(var.raw_providers)
    error_message = "Duplicate provider slug: two entries would declare the same topic twice."
  }
}

variable "raw_topic" {
  description = <<-EOT
    The single definition every `odds.raw.<provider>` topic is built from.
    See the `default` below for the partition-count and retention reasoning.
  EOT

  type = object({
    partitions         = number
    replication_factor = optional(number)
    cleanup_policy     = string
    config             = map(string)
  })

  // -------------------------------------------------------------------------
  // PARTITIONS = 3
  //
  // Raw is a forensic tape: the exact bytes the provider returned, kept so a
  // normalization bug can be reproduced from its real input (CLAUDE.md §10's
  // golden files are recordings taken from here). Exactly ONE consumer group
  // reads it — the normalizer.
  //
  // Why not 1: a single partition pins the entire raw firehose to one leader
  // log and makes replay strictly serial, so re-running three days of payloads
  // through a fixed normalizer cannot be parallelised at all.
  //
  // Why not 6: the natural key here is provider+sport/page, a key space of
  // TENS, not the tens of thousands of `market_id`. More partitions than that
  // buys no parallelism and guarantees skew — some partitions would take every
  // NFL payload and others would sit empty.
  //
  // 3 also matches the broker's own KAFKA_NUM_PARTITIONS default in
  // deploy/compose/compose.yaml, so the topic holds no surprise for someone
  // reading the broker config next to this file.
  // -------------------------------------------------------------------------
  default = {
    partitions     = 3
    cleanup_policy = "delete"

    config = {
      // 72 hours. The topic's job is "reproduce Sunday's normalization bug on
      // Monday", so the window has to span a weekend. Beyond that it buys
      // nothing: once a payload is normalized, the durable artifacts are the
      // compacted `odds.normalized` record and the `prices` hypertable, and the
      // raw bytes are dead weight.
      "retention.ms" = "259200000"

      // 1 GiB PER PARTITION — a hard ceiling, and the reason it exists is
      // specific: the deploy target is a single Oracle Ampere box (2 OCPU /
      // 12 GB, CONTRACT.md) where Kafka, Postgres and Redis share one volume.
      // This is the highest-volume topic with the largest payloads, so an
      // ingest misconfiguration that polls far too fast is exactly how that
      // volume fills. Whichever of size or time trips first wins, which turns
      // "we lost old debug payloads" into the failure mode instead of "the
      // broker died with a full disk and took the database with it".
      // 3 partitions x 1 GiB x 2 providers = 6 GiB worst case.
      "retention.bytes" = "1073741824"

      // 6 hours. THIS IS LOAD-BEARING, NOT COSMETIC. Kafka deletes whole
      // SEGMENTS, never individual records, and it never deletes the ACTIVE
      // segment. With the default segment.ms of 7 days, a topic asking for 72h
      // retention that produces less than segment.bytes in a week has exactly
      // one segment, which is always the active one — so nothing is ever
      // deleted and the declared retention is fiction. 6h makes the real
      // retention 72h..78h instead of unbounded.
      "segment.ms" = "21600000"

      // 64 MiB rather than the 1 GiB default: closes the segment on VOLUME
      // during a busy slate so deletion keeps up there too, and keeps each
      // segment's index small enough to be cheap on a 2-core box.
      "segment.bytes" = "67108864"

      // 4 MiB. A single provider response is a whole page of events with every
      // bookmaker and every market attached; The Odds API's per-sport response
      // with all books is the largest message this system carries. The 1 MiB
      // default would reject it with RecordTooLargeException at the producer,
      // which is a data-loss bug that only appears on a busy slate. Raising a
      // ceiling permits, it does not require — 3b's producer still sets its own
      // batch limits.
      "max.message.bytes" = "4194304"

      // Compress once, at the producer. `producer` means the broker stores the
      // batch as-received rather than re-compressing it, which on 2 shared
      // OCPU is a cost worth refusing explicitly rather than inheriting.
      "compression.type" = "producer"

      // No effect at RF=1, declared so the RF=3 bump is a one-line change to
      // the env root and not a hunt for the config that lets a stale replica
      // become leader and silently truncate the tail.
      "unclean.leader.election.enable" = "false"
    }
  }
}

// ---------------------------------------------------------------------------
// The named topics: odds.normalized, price.computed, wager.events
// ---------------------------------------------------------------------------
variable "topics" {
  description = <<-EOT
    Topic catalogue, keyed by Kafka topic name. `cleanup_policy` is a separate
    attribute rather than just another entry in `config` because it is the one
    setting that carries the topic's SEMANTICS — whether the topic is a snapshot
    or a log — and main.tf validates the rest of the config against it. Burying
    it in the map would make "declared a compacted topic and forgot
    cleanup.policy" a possible mistake; here it cannot be omitted.

    `replication_factor` is optional per topic and falls back to
    var.replication_factor.
  EOT

  type = map(object({
    partitions         = number
    replication_factor = optional(number)
    cleanup_policy     = string
    config             = map(string)
  }))

  default = {
    // =====================================================================
    // odds.normalized — THE CURRENT-LINE SNAPSHOT
    // =====================================================================
    // CLAUDE.md §3: "a compacted topic keyed by market_id IS the current-line
    // snapshot, replayable from scratch, which removes a whole class of
    // cache-coherency bugs between the bus and Redis."
    //
    // Every compaction setting below exists to make that sentence TRUE. Kafka's
    // defaults are tuned for large clusters where the cleaner running "eventually"
    // is fine; at this scale the defaults mean it runs NEVER, and a snapshot that
    // is never compacted is just a log with a marketing name.
    //
    // ---------------------------------------------------------------------
    // PARTITIONS = 6, and what ordering that buys
    // ---------------------------------------------------------------------
    // This is the fan-in point and the most-read topic in the system: the
    // pricer, the Timescale writer, and (phase 12) the Flink SQL jobs are three
    // independent consumer groups over it.
    //
    // 6 = 3x the deploy target's 2 OCPU. More partitions than cores is right
    // here because these consumers are I/O-bound — a Postgres COPY and a Redis
    // pipeline — not CPU-bound, so useful concurrency exceeds core count. And
    // 6 divides evenly by 1, 2, 3 and 6, so a consumer group scaled to any of
    // those sizes gets a perfectly balanced assignment with no straggler
    // partition holding double the load.
    //
    // Not more, because partition count on a KEYED topic is effectively
    // permanent: `hash(market_id) % partitions` means increasing the count
    // re-maps existing markets, so a market's history splits across two
    // partitions with NO ordering relation between the halves. That silently
    // breaks the per-market ordering guarantee below. 6 is chosen as headroom
    // we never have to revisit rather than a number to grow later.
    // Also: each compacted partition is work for the log cleaner, whose
    // `log.cleaner.threads` default is 1.
    //
    // >>> THE ORDERING GUARANTEE A CONSUMER MAY RELY ON <<<
    // Per-`market_id` TOTAL ORDER, and nothing else. All records for one market
    // hash to one partition, so a single consumer instance observes that
    // market's updates in production order. Two DIFFERENT markets — including
    // two markets on the SAME event — may sit on different partitions, and
    // their relative order is undefined and unknowable. Consequences 3b and
    // phase 4 must honour:
    //   * Never infer cross-market causality from arrival order ("the spread
    //     moved before the total"). Order by `observed_at` if that is needed.
    //   * Same-game parlay correlation must read both legs from the state
    //     store, never from stream adjacency.
    //   * Cross-market global ordering DOES NOT EXIST. ADR 0004 says this too.
    // =====================================================================
    "odds.normalized" = {
      partitions     = 6
      cleanup_policy = "compact"

      config = {
        // 1 HOUR. THE SINGLE MOST IMPORTANT VALUE IN THIS FILE.
        //
        // The log cleaner never touches the ACTIVE segment. Defaults are
        // segment.bytes = 1 GiB and segment.ms = 7 days, so a topic producing
        // less than 1 GiB per partition per week has exactly one segment, that
        // segment is always active, and COMPACTION NEVER RUNS AT ALL. The
        // "snapshot" property would silently not hold — and it would not hold in
        // the way that is hardest to notice, because the topic still returns the
        // right current value if you read to the end; it just also returns every
        // superseded value, unboundedly, forever.
        //
        // 1h closes a segment every hour so the cleaner always has something
        // eligible. It is a balance: shorter means more segment/index files and
        // more file handles; longer means a bootstrapping `stream` or `pricer`
        // pod reads a longer tail of dead values before it has current state.
        "segment.ms" = "3600000"

        // 64 MiB, so a busy slate closes segments on volume long before the
        // hour is up and the cleaner keeps pace with the firehose rather than
        // falling an hour behind it.
        "segment.bytes" = "67108864"

        // 0.1 instead of the 0.5 default. The default says "only bother
        // compacting once half the log is superseded values", which means a
        // bootstrapping consumer reads roughly 2x the data it needs, forever, by
        // design. The entire value proposition of this topic is that reading it
        // is a cheap way to get current state, so the cleaner is made
        // aggressive. Affordable because the key space is small — markets in a
        // live slate are thousands to low tens of thousands, trivial for the
        // cleaner's dedup buffer. NOT 0.0: that makes the cleaner re-scan an
        // already-clean log continuously, burning one of two available cores.
        "min.cleanable.dirty.ratio" = "0.1"

        // 1 MINUTE. A record is not eligible for removal until it is at least
        // this old. The default of 0 lets a superseded record vanish the instant
        // its segment closes — which can be before a consumer that is a few
        // seconds behind has read it. That converts "at-least-once delivery of
        // every line movement" into "intermediate movements are silently
        // skipped when the consumer lags", and intermediate movements are the
        // dataset: steam detection and CLV are computed from them (§6
        // Analytics). 1 minute is ample headroom for a healthy consumer while
        // still keeping the log tight.
        "min.compaction.lag.ms" = "60000"

        // 1 HOUR, and this closes the last hole. Dirty-ratio cleaning is
        // THROUGHPUT-DRIVEN: a quiet overnight slate may never reach 10% dirty,
        // so the cleaner would not run — and a quiet period is exactly when a
        // bloated snapshot is least visible and most misleading. The default is
        // effectively infinite. With this plus segment.ms=1h, any superseded
        // record is provably gone within ~2h regardless of volume.
        "max.compaction.lag.ms" = "3600000"

        // 7 DAYS for tombstone visibility (default is 24h). A market being
        // suspended or withdrawn — the admin console's market suspension, §6
        // Platform — is published as a null-valued record. `delete.retention.ms`
        // is how long that tombstone survives compaction so an offline consumer
        // can still learn the market is gone. At 24h, a `stream` pod or a Flink
        // job that was down over a weekend comes back, never sees the tombstone,
        // and RESURRECTS a suspended market's stale line into its own state.
        // 7 days covers "the laptop was shut on Friday". Tombstones lingering
        // costs a few bytes per removed market.
        "delete.retention.ms" = "604800000"

        "compression.type"               = "producer"
        "unclean.leader.election.enable" = "false"
        // 1 MiB (the Kafka default), declared rather than inherited so that the
        // contrast with odds.raw's 4 MiB is a deliberate statement: a normalized
        // market delta is small, and a message here approaching 1 MiB means the
        // normalizer is passing raw payloads through and should fail loudly.
        "max.message.bytes" = "1048576"
      }
    }

    // =====================================================================
    // price.computed — COMPACTED (CLAUDE.md §3)
    // =====================================================================
    // The pricer's output: no-vig fair value, EV%, edge, Kelly fraction. `stream`
    // consumes it and fans it out to browsers.
    //
    // PARTITION COUNT DELIBERATELY EQUALS odds.normalized's 6, and that is a
    // decision rather than symmetry for its own sake:
    //
    //   1. The pricer is a keyed 1:1 transform — one normalized market record in,
    //      one priced record out, same `market_id` key. Equal partition counts
    //      mean the same key lands on the same partition INDEX on both topics, so
    //      per-market ordering survives the hop with no repartition and no
    //      re-keying stage. Unequal counts would silently reshuffle markets
    //      between the two topics.
    //   2. Phase 12's Flink SQL interval and temporal joins run between these two
    //      topics. Co-partitioned sources on the join key is the difference
    //      between a local join and a network shuffle inside the job graph — on
    //      2 OCPU that is not a micro-optimisation.
    //
    // Compaction settings are IDENTICAL to odds.normalized on purpose: it is the
    // same snapshot property over the same key space, and two nearly-identical
    // tunings would be two things to keep in sync for no benefit. The one
    // difference is delete.retention.ms, noted below.
    // =====================================================================
    "price.computed" = {
      partitions     = 6
      cleanup_policy = "compact"

      config = {
        "segment.ms"                = "3600000"
        "segment.bytes"             = "67108864"
        "min.cleanable.dirty.ratio" = "0.1"
        "min.compaction.lag.ms"     = "60000"
        "max.compaction.lag.ms"     = "3600000"

        // 7 days, same as odds.normalized and for the same reason: a suspended
        // market's price must be withdrawn from `stream`'s state, and a stream
        // pod that missed the tombstone would keep serving a price for a market
        // that is no longer offered. That is the worst possible stale-data bug
        // in a betting UI — a user can act on it.
        "delete.retention.ms" = "604800000"

        "compression.type"               = "producer"
        "unclean.leader.election.enable" = "false"
        "max.message.bytes"              = "1048576"
      }
    }

    // =====================================================================
    // wager.events — RETENTION-BASED, "the settlement audit trail" (§3)
    // =====================================================================
    // cleanup.policy IS DELIBERATELY `delete`, NOT `compact`, and this is the
    // most consequential line in the block. Compacting by wager id would collapse
    // a wager's lifecycle — placed, priced, graded, settled, paid — down to its
    // final state and DESTROY the audit trail the topic exists to be. An audit
    // trail is the sequence, not the outcome. A compacted `wager.events` would
    // look fine on the dashboard and be worthless the first time a settlement is
    // disputed.
    //
    // ---------------------------------------------------------------------
    // PARTITIONS = 3
    // ---------------------------------------------------------------------
    // Volume here is human-rate: people place bets at people speed, orders of
    // magnitude below the odds firehose. The binding constraint is durability and
    // per-wager ordering (placed must precede settled for one wager), not
    // throughput, and both are satisfied by keying on the wager and having more
    // than one partition. `settle` is a single low-concurrency consumer group; 3
    // gives it room to run 1 or 3 instances evenly and costs almost nothing.
    // 6 would be provisioning parallelism for a workload that will never need it.
    //
    // ---------------------------------------------------------------------
    // RETENTION = 90 DAYS, and an honest note about what that does and does not
    // mean
    // ---------------------------------------------------------------------
    // "An audit trail with a short retention is not an audit trail" — agreed, and
    // the corollary matters just as much: KAFKA IS NOT THIS AUDIT TRAIL'S SYSTEM
    // OF RECORD. Postgres is. Phase 2 built `ledger_entries` with UPDATE, DELETE
    // and TRUNCATE rejected by trigger and zero-sum enforced by a deferred
    // constraint, plus an `audit_log` hypertable. Those have no expiry. This topic
    // is the REPLAY and RECOVERY window on top of that record, so the question
    // "how long?" is really two questions:
    //
    //   * How long may a consumer be down and still catch up from the bus
    //     without a Postgres backfill? Days, comfortably.
    //   * How far back would someone want to re-run grading to debug a
    //     settlement bug? A SEASON. "This parlay graded wrong in week 3" is a
    //     realistic complaint in week 14, and being able to reset a consumer
    //     group's offset and replay the exact input is the whole reason ADR 0004
    //     lists replay as a debugging superpower.
    //
    // 90 days is the second answer: an NFL regular season plus playoffs. And it
    // is affordable, which is why it can be chosen on merit rather than on cost —
    // at a generous 10k wager events/day at ~1 KB each that is ~10 MB/day, under
    // 1 GB over the full window across 3 partitions. Compare odds.raw, where 90
    // days would be hundreds of GB and the number would have to be a compromise.
    //
    // If a real regulatory retention were ever required, the answer is Postgres
    // plus object-storage archival — not a longer Kafka retention. Do not let this
    // topic be mistaken for the book of record.
    // =====================================================================
    "wager.events" = {
      partitions     = 3
      cleanup_policy = "delete"

      config = {
        // 90 days.
        "retention.ms" = "7776000000"

        // UNLIMITED, explicitly. Time is the only retention policy on the audit
        // trail. A byte cap here would mean a busy period could silently evict
        // week 3 before the 90 days elapsed — a size-triggered hole in an audit
        // trail is precisely the failure that makes it untrustworthy, and unlike
        // odds.raw there is no disk-exhaustion risk to trade against because the
        // volume is human-rate.
        "retention.bytes" = "-1"

        // 7 days. Same segment/active-segment mechanic as odds.raw: deletion
        // granularity is a whole segment, so real retention is 90..97 days. At
        // 8% slop on a 90-day window that is fine, and 7d keeps the segment
        // count over the window at ~13 per partition instead of ~360 with a 6h
        // roll. Declared rather than inherited so the arithmetic is visible.
        "segment.ms" = "604800000"

        "segment.bytes"                  = "67108864"
        "compression.type"               = "producer"
        "max.message.bytes"              = "1048576"
        "unclean.leader.election.enable" = "false"
      }
    }

    // =====================================================================
    // THE SIGNALS FAMILY — signals.ev / .arb / .steam / .clv  (phase 9)
    // =====================================================================
    // CLAUDE.md §3's event-flow diagram draws the phase-12 Flink jobs emitting
    // `signals.steam | signals.arb | signals.clv`. `signals.ev` is an ADDITION,
    // flagged as one here and in internal/platform/kafka/topics.go: the diagram
    // does not name it, but §6's Analytics bullet leads with the positive-EV
    // finder and phase 9 needs it as a first-class signal for the same reason the
    // other three are — a finding that has to reach a subscriber, be recorded and
    // be replayable.
    //
    // These four are declared NOW, in phase 9, even though the Go implementation
    // is what publishes to them today. That is the whole point of §11 row 9
    // calling phase 9 "the reference implementation phase 12 validates against":
    // if the Go detectors publish to the same topics with the same keys and the
    // same retention that the Flink jobs will, then the phase-12 cutover is a
    // like-for-like swap that can be diffed, rather than a new pipeline that has
    // to be trusted.
    //
    // ---------------------------------------------------------------------
    // ALL FOUR ARE cleanup.policy = "delete". THIS IS THE DECISION.
    // ---------------------------------------------------------------------
    // The tempting reading is that signals look like odds.normalized — keyed by
    // market, superseded constantly — and should therefore be compacted. They are
    // not, and the distinction is the same one that keeps wager.events
    // retention-based.
    //
    // Compaction is correct when the latest record per key SUPERSEDES the earlier
    // ones, so that reading to the end of the log reconstructs current state. A
    // market's current line has that property. A FINDING DOES NOT. "The latest
    // steam move for market X" is not a snapshot of market X; it is one event, and
    // the one before it is a different event that also happened and that a
    // consumer computing hit rates needs to see. Compacting these would destroy
    // the signal history in exactly the way §3 says compacting wager.events would
    // destroy the audit trail — and it would do it invisibly, because the topic
    // would still return something plausible at the head.
    //
    // The corollary is that these topics are NOT the system of record either.
    // Migration 00009's four tables are, and they carry no expiry. The retention
    // windows below are REPLAY windows on top of that record, and each one is
    // sized by the question "how far back would someone reset a consumer group
    // to?", not by "how long is this data worth keeping?".
    //
    // ---------------------------------------------------------------------
    // PARTITIONS = 3 FOR ALL FOUR, and why not 6
    // ---------------------------------------------------------------------
    // odds.normalized and price.computed are 6, and the co-partitioning argument
    // in this file's price.computed block would seem to extend here: same key
    // (market_id), so equal partition counts would put a market on the same
    // partition index across all of them.
    //
    // It does not extend, because SIGNALS ARE JOIN OUTPUTS, NOT JOIN INPUTS. The
    // phase-12 jobs read odds.normalized and price.computed and WRITE these; a
    // sink's partition count has no bearing on whether the join upstream of it
    // shuffles. Nothing in the design joins two signals topics to each other.
    //
    // What is left is volume and consumer parallelism, and both point at 3:
    //
    //   * Volume is thresholded output, not a firehose. ev is the busiest and is
    //     a filter over `prices` that keeps a low single-digit percent of quotes
    //     (migration 00009 sizes it at ~10^4..10^5 rows/day against that table's
    //     ~2.7x10^6). steam is rarer still, by construction. arb is rarest — the
    //     phase-4 gate found 68 findings over 1,065 records with the leg-age bound
    //     binding constantly. clv is human-rate: one record per graded leg.
    //   * The consumers are `api`'s alerting and persistence path, which is a
    //     single low-concurrency group. 3 lets it run 1 or 3 instances with a
    //     perfectly balanced assignment and costs almost nothing.
    //
    // And partitions are not free. `total_partitions` goes from 21 to 33 with
    // this block, and outputs.tf explains why that number is watched: every
    // partition costs file handles, an index and a time-index on a single-node
    // deploy target that also runs Postgres, Redis and six Go services. Choosing 6
    // apiece would have made it 45 for parallelism nothing asks for.
    //
    // Partition count on a keyed topic is effectively permanent — raising it
    // re-maps hash(key) % partitions and splits a key's history across two
    // partitions with no ordering between the halves — so 3 is chosen as a floor
    // that will not need revisiting rather than as a number to grow later.
    //
    // >>> THE ORDERING GUARANTEE <<<
    // Per-KEY total order and nothing else, same as every other keyed topic here.
    // For ev/arb/steam the key is market_id; for clv it is wager_id. Two findings
    // on DIFFERENT markets have no defined relative order even if one caused the
    // other. Order by the payload's own event-time field if that matters — which
    // for steam is the window edge and for ev is the quote's observed_at.
    // =====================================================================

    // signals.ev — the +EV finder's output. The busiest of the four.
    "signals.ev" = {
      partitions     = 3
      cleanup_policy = "delete"

      config = {
        // 7 DAYS. The replay question for +EV is "re-run last week's slate
        // through a corrected detector and diff the findings", which is a
        // weekend-plus-a-work-week window. Longer buys nothing: the durable
        // artifact is the ev_signals hypertable, which has no retention policy,
        // and evaluating a month-old signal against its close is a database
        // query rather than a bus replay.
        "retention.ms" = "604800000"

        // 512 MiB per partition. A byte ceiling as well as a time one, for the
        // reason odds.raw has one: this is the highest-volume signals topic and a
        // misconfigured threshold — someone setting min EV to 0 — is exactly how
        // its volume goes up two orders of magnitude without anyone noticing.
        // Whichever of size or time trips first wins, so the failure mode is
        // "we lost old findings" rather than "the broker filled the volume the
        // database is on". 3 x 512 MiB = 1.5 GiB worst case.
        "retention.bytes" = "536870912"

        // 6 hours. Kafka deletes whole SEGMENTS and never the ACTIVE one, so with
        // the 7-day segment.ms default a topic asking for 7-day retention would
        // have one perpetually-active segment and delete nothing, ever. 6h makes
        // the real retention 7d..7d6h instead of unbounded. Same mechanic
        // odds.raw's block spells out.
        "segment.ms" = "21600000"

        // 64 MiB, so a busy slate closes segments on volume too.
        "segment.bytes" = "67108864"

        // 1 MiB, the Kafka default, declared rather than inherited: a single +EV
        // finding is a few hundred bytes, and a message here approaching 1 MiB
        // means the pricer is publishing a whole ComputedMarket where it should
        // be publishing one finding.
        "max.message.bytes" = "1048576"

        "compression.type"               = "producer"
        "unclean.leader.election.enable" = "false"
      }
    }

    // signals.arb — arbitrage findings. The lowest-volume of the three
    // market-keyed topics, and the one whose history is most worth keeping.
    "signals.arb" = {
      partitions     = 3
      cleanup_policy = "delete"

      config = {
        // 30 DAYS, four times signals.ev's window, and the asymmetry is
        // deliberate. Arbitrage findings are RARE and each one is a claim that
        // wants auditing after the fact — "we showed 40 arbs last month; how many
        // were still live 30 seconds later?" is a question about a month of
        // findings, and answering it from the bus means being able to replay a
        // month. The volume makes that free: at the phase-4 gate's rate this is
        // single-digit MB over the whole window.
        "retention.ms" = "2592000000"

        // 256 MiB per partition. Half of signals.ev's cap, because the expected
        // volume is orders of magnitude lower and a breach here means the
        // staleness bounds have been disabled rather than merely loosened.
        "retention.bytes" = "268435456"

        // 24 hours. Segment granularity is a whole day against a 30-day window —
        // real retention 30d..31d, about 3% slop — and it keeps the segment count
        // at ~30 per partition instead of ~120 with a 6h roll. Same trade
        // wager.events makes with its 7-day segments over 90 days.
        "segment.ms" = "86400000"

        "segment.bytes"                  = "67108864"
        "max.message.bytes"              = "1048576"
        "compression.type"               = "producer"
        "unclean.leader.election.enable" = "false"
      }
    }

    // signals.steam — steam-move detections. Same sizing as signals.arb and for
    // the same reasons: rare by construction, and the interesting question about
    // it is a month-scale one ("did the books that followed actually follow?").
    "signals.steam" = {
      partitions     = 3
      cleanup_policy = "delete"

      config = {
        "retention.ms"    = "2592000000"
        "retention.bytes" = "268435456"
        "segment.ms"      = "86400000"

        "segment.bytes" = "67108864"

        // 1 MiB. Worth a note because steam records are the largest in the
        // family: each carries a `followers` array with one entry per book that
        // followed. Twenty books is still well under a kilobyte, so 1 MiB is a
        // ceiling that only a bug can reach.
        "max.message.bytes" = "1048576"

        "compression.type"               = "producer"
        "unclean.leader.election.enable" = "false"
      }
    }

    // signals.clv — per-graded-leg closing line value, written by `settle`.
    //
    // The odd one out in three ways, all of which follow from it being a fact
    // about a WAGER rather than about a market: it is keyed by wager_id, it is
    // written by settle rather than by the pricer, and it is sized like
    // wager.events rather than like its siblings.
    "signals.clv" = {
      partitions     = 3
      cleanup_policy = "delete"

      config = {
        // 90 DAYS, matching wager.events exactly, and for the same reason that
        // block gives: "how far back would someone want to re-run grading to
        // debug a settlement bug? A SEASON." CLV is computed from the same graded
        // legs, so a replay that re-grades week 3 in week 14 must be able to
        // re-measure it too. Splitting the two windows would mean a replay that
        // could reproduce the settlement but not the CLV attached to it.
        "retention.ms" = "7776000000"

        // UNLIMITED, explicitly, again matching wager.events. Volume is
        // human-rate — one record per graded leg — so there is no
        // disk-exhaustion risk to trade against, and a size-triggered hole in a
        // user's CLV record is the failure that makes a leaderboard
        // untrustworthy.
        "retention.bytes" = "-1"

        // 7 days, matching wager.events: real retention 90..97 days, ~13 segments
        // per partition over the window.
        "segment.ms" = "604800000"

        "segment.bytes"                  = "67108864"
        "max.message.bytes"              = "1048576"
        "compression.type"               = "producer"
        "unclean.leader.election.enable" = "false"
      }
    }
  }

  validation {
    condition     = length(var.topics) > 0
    error_message = "topics must not be empty — a module call that declares no topics is a silent no-op that reads like success."
  }

  validation {
    // The raw family is GENERATED from var.raw_providers. If a caller also spells
    // one out here, the two definitions merge and one silently wins. Reject it
    // instead: the generated definition is the single source of truth for
    // odds.raw.* precisely so no provider can end up with different retention.
    condition = length(setintersection(
      keys(var.topics),
      toset([for p in var.raw_providers : "odds.raw.${p}"])
    )) == 0
    error_message = "A topic in `topics` collides with a generated odds.raw.<provider> topic. Remove it from `topics` — the raw family is defined once in var.raw_topic and expanded over var.raw_providers."
  }
}
