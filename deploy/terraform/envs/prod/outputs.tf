// ---------------------------------------------------------------------------
// envs/prod — outputs
// ---------------------------------------------------------------------------
// Identical set to envs/local, on purpose: the two roots must be diffable. If
// `TF_ENV=local make tf-output` and `TF_ENV=prod make tf-output` ever disagree on
// anything but `brokers`, something environment-shaped has leaked into the topic
// definitions and that is a defect.
// ---------------------------------------------------------------------------

output "brokers" {
  description = "Broker list these topics were declared against."
  value       = module.kafka_topics.brokers
}

output "topic_names" {
  description = "Every managed topic. Must be byte-identical to envs/local's list."
  value       = module.kafka_topics.topic_names
}

output "partition_map" {
  description = "topic -> partitions. Must be identical to envs/local — partition count is a semantic property, not an environment knob."
  value       = module.kafka_topics.partition_map
}

output "total_partitions" {
  description = "Application partitions in total, excluding Kafka's internal topics. Sized against 2 OCPU / 12 GB (CONTRACT.md)."
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
  description = "Full per-topic config as sent to the broker. min.insync.replicas is the only key expected to differ from envs/local."
  value       = module.kafka_topics.topic_configs
}
