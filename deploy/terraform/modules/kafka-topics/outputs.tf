// ---------------------------------------------------------------------------
// modules/kafka-topics — outputs
// ---------------------------------------------------------------------------
// These exist to be READ, not just to satisfy convention. `make tf-output` after
// an apply is how you confirm what the bus actually looks like without opening a
// Kafka CLI, and `partition_map` is the authoritative statement of the ordering
// guarantee downstream code may rely on.
// ---------------------------------------------------------------------------

output "brokers" {
  description = "Broker list the topics were declared against, parsed from var.bootstrap_servers. Printed so an apply can never leave you guessing WHICH Kafka just changed."
  value       = local.brokers
}

output "topic_names" {
  description = "Every topic this module manages, sorted. The contract 3b's producers and consumers must match exactly."
  value       = sort(keys(local.topics))
}

output "raw_topic_names" {
  description = "Just the odds.raw.<provider> family, sorted — one per entry in var.raw_providers."
  value       = sort(keys(local.raw_topics))
}

output "partition_map" {
  description = <<-EOT
    topic -> partition count. This is the ordering contract: a keyed topic
    guarantees total order per KEY (all records for one key hash to one
    partition) and guarantees nothing whatsoever across keys. Consumer
    parallelism within one group is capped at this number — extra instances sit
    idle.
  EOT
  value       = { for name, t in local.topics : name => t.partitions }
}

output "total_partitions" {
  description = <<-EOT
    Sum of all application partitions. Worth watching: it is the number that has
    to fit the deploy target (2 OCPU / 12 GB Ampere, CONTRACT.md) alongside
    Kafka's own internal topics — __consumer_offsets alone defaults to 50
    partitions. Every partition costs file handles, an index and a time-index,
    and on compacted topics a share of the single default log-cleaner thread.
  EOT
  value       = sum([for t in local.topics : t.partitions])
}

output "compacted_topics" {
  description = "Topics whose cleanup.policy includes `compact` — i.e. the ones whose content IS a snapshot rather than a log (CLAUDE.md §3)."
  value       = sort([for name, t in local.topics : name if strcontains(t.cleanup_policy, "compact")])
}

output "retention_topics" {
  description = "Topics whose cleanup.policy includes `delete` — the replayable logs, including the wager.events settlement audit trail."
  value       = sort([for name, t in local.topics : name if strcontains(t.cleanup_policy, "delete")])
}

output "topic_configs" {
  description = "Full per-topic config as sent to the broker, cleanup.policy and min.insync.replicas folded in. Diff this against `make kafka-topics ARGS='--describe'` to prove declared state equals real state."
  value       = { for name, t in local.topics : name => t.config }
}
