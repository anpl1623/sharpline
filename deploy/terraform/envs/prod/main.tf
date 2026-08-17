// ---------------------------------------------------------------------------
// envs/prod — the cluster's Kafka StatefulSet
// ---------------------------------------------------------------------------
// The SAME module and therefore THE SAME TOPICS as envs/local: identical names,
// identical partition counts, identical cleanup policies, identical retention and
// compaction windows. Nothing about a topic's semantics is environment-dependent,
// and that is the property CLAUDE.md §9 buys with Terraform here — "topic
// configuration … silently differs between laptop and cluster" is the failure this
// file exists to make impossible.
//
// Only three things differ from local, and each is a property of the BROKER
// TOPOLOGY rather than of the data:
//
//   bootstrap_servers    where the brokers are (no default — see variables.tf)
//   replication_factor   how many replicas the cluster can actually hold
//   min_insync_replicas  how many must acknowledge, which follows from the above
//
// If a fourth difference ever appears here, that is the moment to ask whether it
// is really environmental or whether prod is about to start behaving differently
// from the system that was tested.
// ---------------------------------------------------------------------------

module "kafka_topics" {
  source = "../../modules/kafka-topics"

  bootstrap_servers   = var.bootstrap_servers
  replication_factor  = var.replication_factor
  min_insync_replicas = var.min_insync_replicas
  raw_providers       = var.raw_providers
}
