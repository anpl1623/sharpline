// ---------------------------------------------------------------------------
// envs/local — the compose stack's Kafka
// ---------------------------------------------------------------------------
// Target: the single-container KRaft broker from `make up`. Kafka 4.x is
// KRaft-only, so there is no ZooKeeper here or anywhere (CLAUDE.md §3).
//
// This root passes ONLY environment-shaped values. The topics themselves — names,
// partition counts, retention, compaction — are the module's default catalogue,
// shared verbatim with envs/prod. That is the whole point: §9 declares Terraform's
// value here to be that topic configuration cannot "silently differ between laptop
// and cluster", and two copies of the catalogue would put the difference one
// directory up instead of removing it.
// ---------------------------------------------------------------------------

module "kafka_topics" {
  source = "../../modules/kafka-topics"

  bootstrap_servers = var.bootstrap_servers

  // -------------------------------------------------------------------------
  // RF = 1: there is exactly ONE broker. This is not a relaxed development
  // setting, it is the only legal value — asking for 2 fails topic creation
  // outright with "Replication factor: 2 larger than available brokers: 1".
  //
  // The compose broker agrees with this by construction: KAFKA_*_REPLICATION_
  // FACTOR is 1 for the offsets, transaction-state and share-coordinator
  // internal topics too (deploy/compose/compose.yaml).
  // -------------------------------------------------------------------------
  replication_factor = 1

  // With one replica, 1 is the only value that works: min.isr = 2 on a
  // single-replica topic makes every acks=all produce fail with
  // NOT_ENOUGH_REPLICAS. The module enforces min.isr <= RF at plan time.
  min_insync_replicas = 1

  // Both providers, in every environment, on purpose: `ingest` chooses its
  // adapter at STARTUP from whether ODDS_API_KEY is set (CONTRACT.md — the
  // provider decision is resolved: The Odds API, with the synthetic stochastic
  // market-maker as the no-key fallback). Setting that key must not require a
  // `terraform apply` before the pipeline has anywhere to publish.
  //
  // Adding a third provider is one string here plus one in envs/prod — no new
  // resource block, and no way for it to get different retention than the others.
  raw_providers = ["synthetic", "the-odds-api"]
}
