// ---------------------------------------------------------------------------
// modules/kafka-topics — the topics themselves
// ---------------------------------------------------------------------------
// CLAUDE.md §9, container inventory, `kafka` row: topics are "created by
// Terraform, not by hand". There is no `make kafka-create-topic`, and
// `make kafka-topics` is read-only by design (it lists and describes). The only
// way a topic comes into existence in this system is `make tf-apply`, and the
// broker is started with KAFKA_AUTO_CREATE_TOPICS_ENABLE=false so a typo in a
// producer cannot conjure one either.
// ---------------------------------------------------------------------------

locals {
  // Same comma-separated format as the application's SHARPLINE_KAFKA_BROKERS.
  brokers = compact([for s in split(",", var.bootstrap_servers) : trimspace(s)])

  // One `odds.raw.<provider>` per provider slug, every one built from the SAME
  // definition — which is the point: adding a provider cannot accidentally give
  // it different retention, a different size cap or a different partition count,
  // because there is only one place those are written.
  raw_topics = {
    for p in var.raw_providers : "odds.raw.${p}" => var.raw_topic
  }

  // `raw_topics` second so an accidental collision surfaces as the generated
  // definition winning rather than a silent half-merge. The collision is
  // additionally rejected outright by a validation on var.topics.
  declared = merge(var.topics, local.raw_topics)

  // Fold the two semantic settings that must never be forgotten into every
  // topic's config map:
  //
  //   cleanup.policy       promoted to a required top-level attribute (see
  //                        variables.tf) so it cannot be omitted, then written
  //                        back into the config the broker actually receives.
  //   min.insync.replicas  applied uniformly from the environment's topology,
  //                        because a topic that is durable in prod and not in
  //                        dev is a topic whose durability is untested.
  topics = {
    for name, t in local.declared : name => {
      partitions         = t.partitions
      replication_factor = coalesce(t.replication_factor, var.replication_factor)
      cleanup_policy     = t.cleanup_policy

      config = merge(t.config, {
        "cleanup.policy"      = t.cleanup_policy
        "min.insync.replicas" = tostring(var.min_insync_replicas)
      })
    }
  }
}

resource "kafka_topic" "this" {
  // Keyed by topic name, so adding or removing a topic touches exactly that
  // topic's state entry. A count-indexed list would renumber and therefore
  // propose destroying and recreating unrelated topics — on a compacted topic
  // that is not a cosmetic churn, it discards the snapshot.
  for_each = local.topics

  name               = each.key
  partitions         = each.value.partitions
  replication_factor = each.value.replication_factor
  config             = each.value.config

  lifecycle {
    // -- name legality ----------------------------------------------------
    precondition {
      condition     = can(regex("^[A-Za-z0-9._-]+$", each.key)) && length(each.key) <= 249
      error_message = "Topic ${each.key}: Kafka topic names allow only [A-Za-z0-9._-] and at most 249 characters."
    }

    precondition {
      condition     = !contains([".", ".."], each.key)
      error_message = "Topic ${each.key}: '.' and '..' are reserved — the broker maps a topic to a directory of that name."
    }

    precondition {
      // Kafka mangles `.` to `_` when deriving JMX metric names, so a topic
      // containing BOTH characters can collide with a differently-named topic in
      // the metrics namespace and silently merge two topics' series. Every topic
      // here uses `.` only; this keeps it that way.
      condition     = !(strcontains(each.key, ".") && strcontains(each.key, "_"))
      error_message = "Topic ${each.key}: do not mix '.' and '_' in one topic name — Kafka's metric names would collide (it rewrites '.' as '_')."
    }

    // -- sizing -----------------------------------------------------------
    precondition {
      condition     = each.value.partitions >= 1 && each.value.partitions == floor(each.value.partitions)
      error_message = "Topic ${each.key}: partitions must be a positive integer."
    }

    precondition {
      // min.isr above the replica count is not a degraded configuration, it is a
      // broken one: the topic is created successfully and then every produce with
      // acks=all fails at runtime with NOT_ENOUGH_REPLICAS. Caught at plan time
      // instead, because the runtime symptom points at the producer rather than
      // at the topic config that caused it.
      condition     = each.value.replication_factor >= var.min_insync_replicas
      error_message = "Topic ${each.key}: replication_factor (${each.value.replication_factor}) is below min_insync_replicas (${var.min_insync_replicas}) — every produce with acks=all would fail with NOT_ENOUGH_REPLICAS. Move both values together."
    }

    // -- cleanup policy ---------------------------------------------------
    precondition {
      condition     = contains(["delete", "compact", "compact,delete"], each.value.cleanup_policy)
      error_message = "Topic ${each.key}: cleanup_policy must be \"delete\", \"compact\" or \"compact,delete\", got ${each.value.cleanup_policy}."
    }

    // -- COMPACTION MUST ACTUALLY RUN -------------------------------------
    // The whole architectural claim in CLAUDE.md §3 is that a compacted topic
    // keyed by market_id IS the current-line snapshot. That claim is false if the
    // cleaner never runs, and with Kafka's defaults at this scale it never does:
    // the cleaner skips the ACTIVE segment, and the default segment.ms of 7 days
    // with segment.bytes of 1 GiB means a low-volume topic has exactly one
    // segment and it is always active.
    //
    // So a future compacted topic cannot be declared without saying how its
    // segments roll and how eagerly they are cleaned. This turns the subtlest
    // failure in the design into a plan-time error.
    precondition {
      condition = !strcontains(each.value.cleanup_policy, "compact") || alltrue([
        for k in [
          "segment.ms",
          "min.cleanable.dirty.ratio",
          "max.compaction.lag.ms",
          "delete.retention.ms",
        ] : contains(keys(each.value.config), k)
      ])
      error_message = "Topic ${each.key} is compacted but does not declare all of segment.ms, min.cleanable.dirty.ratio, max.compaction.lag.ms and delete.retention.ms. Kafka's defaults for these mean the cleaner never runs on a topic this size, so the snapshot property (CLAUDE.md §3) would silently not hold."
    }

    precondition {
      condition = !strcontains(each.value.cleanup_policy, "compact") || (
        try(tonumber(each.value.config["min.cleanable.dirty.ratio"]), -1) > 0 &&
        try(tonumber(each.value.config["min.cleanable.dirty.ratio"]), -1) <= 1
      )
      error_message = "Topic ${each.key}: min.cleanable.dirty.ratio must be in (0, 1]. Exactly 0 makes the cleaner re-scan an already-clean log continuously, which on a 2-core deploy target burns half the machine."
    }

    // -- RETENTION MUST ACTUALLY DELETE -----------------------------------
    // The mirror image of the compaction trap, and just as easy to miss. Kafka
    // deletes whole segments and never the active one, so a topic whose
    // segment.ms exceeds its retention.ms retains data far past its declared
    // window: with the 7-day segment default, a topic asking for 72h keeps
    // everything until a segment finally rolls.
    precondition {
      condition = !strcontains(each.value.cleanup_policy, "delete") || alltrue([
        for k in ["retention.ms", "segment.ms"] : contains(keys(each.value.config), k)
      ])
      error_message = "Topic ${each.key} is retention-based but does not declare both retention.ms and segment.ms. Without an explicit segment.ms the 7-day default silently overrides any shorter retention window."
    }

    precondition {
      condition = !strcontains(each.value.cleanup_policy, "delete") || (
        try(tonumber(each.value.config["retention.ms"]), 0) < 0 ||
        try(tonumber(each.value.config["segment.ms"]), 0) < try(tonumber(each.value.config["retention.ms"]), 0)
      )
      error_message = "Topic ${each.key}: segment.ms must be strictly less than retention.ms (or retention.ms must be -1 for unlimited). Kafka deletes whole segments and never the active one, so a segment longer than the retention window means the declared retention is fiction."
    }
  }
}
