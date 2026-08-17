// ---------------------------------------------------------------------------
// envs/local — outputs
// ---------------------------------------------------------------------------
// `make tf-output` prints these. They are the fast way to answer "what does the
// bus look like right now" without a Kafka CLI, and `topic_configs` is what you
// diff against `make kafka-topics ARGS="--describe"` to prove that declared
// state equals real state.
// ---------------------------------------------------------------------------

output "brokers" {
  description = "Broker list these topics were declared against."
  value       = module.kafka_topics.brokers
}

output "topic_names" {
  description = "Every managed topic. This is the contract phase 3b's producers and consumers must match exactly."
  value       = module.kafka_topics.topic_names
}

output "partition_map" {
  description = "topic -> partitions. Caps consumer-group parallelism, and defines which keys share an ordering guarantee."
  value       = module.kafka_topics.partition_map
}

output "total_partitions" {
  description = "Application partitions in total, excluding Kafka's internal topics."
  value       = module.kafka_topics.total_partitions
}

output "compacted_topics" {
  description = "Topics that ARE a snapshot rather than a log (CLAUDE.md §3)."
  value       = module.kafka_topics.compacted_topics
}

output "retention_topics" {
  description = "Replayable logs, including the wager.events settlement audit trail."
  value       = module.kafka_topics.retention_topics
}

output "topic_configs" {
  description = "Full per-topic config as sent to the broker."
  value       = module.kafka_topics.topic_configs
}
