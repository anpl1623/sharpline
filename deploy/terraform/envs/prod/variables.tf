// ---------------------------------------------------------------------------
// envs/prod — inputs
// ---------------------------------------------------------------------------

variable "bootstrap_servers" {
  description = <<-EOT
    Comma-separated Kafka bootstrap brokers in the cluster, e.g.
    `kafka-0.kafka.sharpline-prod.svc.cluster.local:9092`. Same string format as
    the application's SHARPLINE_KAFKA_BROKERS.

    THERE IS DELIBERATELY NO DEFAULT. A default here would be a loaded gun: the
    only broker address that resolves from a developer's laptop is the compose
    one, so any convenient default would make `TF_ENV=prod make tf-apply`
    reconfigure the LOCAL broker's topics under prod's state file. Failing with
    "No value for required variable" is the correct outcome.

    Supply it explicitly:
      TF_ENV=prod make tf-plan TF_BOOTSTRAP=kafka-0.kafka.sharpline-prod.svc:9092
  EOT
  type        = string
}

variable "tls_enabled" {
  description = <<-EOT
    Whether the provider speaks TLS to the broker.

    Note the Mongey/kafka default is TRUE, so this is not a case of "off unless
    you ask" — it is off because it is set off. Inside the cluster the confinement
    comes from the default-deny NetworkPolicy (CLAUDE.md §9) and, from phase 12,
    mesh mTLS (§13.5). Set to true and supply certificates once the broker
    publishes a TLS listener.
  EOT
  type        = bool
  default     = false
}

variable "timeout_seconds" {
  description = <<-EOT
    AdminClient timeout. Higher than the local default of 10s because a cluster
    broker may be mid-rolling-restart behind a StatefulSet: a StatefulSet rolls
    pods one at a time and the new pod has to catch up on its log before it serves
    metadata, so a short timeout turns a normal restart into a failed apply.
  EOT
  type        = number
  default     = 30
}

// ---------------------------------------------------------------------------
// Replication factor and min.insync.replicas.
//
// These are variables rather than literals in main.tf for one reason: they are
// the values that CHANGE when the cluster gains brokers, and that change must be
// a reviewed one-line diff with the two values moving together, not an
// archaeology exercise.
// ---------------------------------------------------------------------------

variable "replication_factor" {
  description = <<-EOT
    Replication factor for every topic.

    THE HONEST VALUE TODAY IS 1, AND IT IS 1 BECAUSE OF THE HARDWARE, NOT BECAUSE
    OF LAXITY. The deploy target is a single Oracle Cloud Always-Free
    VM.Standard.A1.Flex — 2 OCPU / 12 GB, Ampere ARM (CONTRACT.md, superseding
    CLAUDE.md §13.2). CLAUDE.md §9 runs Kafka as a StatefulSet on that cluster,
    which is ONE node, which means ONE broker. RF=3 there is not "safer", it is
    impossible: topic creation fails with "Replication factor: 3 larger than
    available brokers: 1", and even a 3-replica StatefulSet on a single node
    would put all three replicas on one failure domain, which is theatre.

    WHAT TO CHANGE, AND WHEN. The moment there are three or more brokers on
    distinct nodes:

        replication_factor  = 3
        min_insync_replicas = 2

    Both, in the same commit. RF=3 with min.isr=1 is the worst of the three
    combinations — it looks replicated while a producer using acks=all is
    satisfied by the leader alone, so a failover loses acknowledged writes. On
    `wager.events` that is a lost settlement event; on `odds.normalized` it is a
    hole in the snapshot the whole design leans on. `unclean.leader.election.enable`
    is already declared false on every topic so that bump needs no config hunt.

    Raising RF on an existing topic is a partition-reassignment operation, not a
    field edit — plan for it rather than expecting `terraform apply` to be
    seamless.
  EOT
  type        = number
  default     = 1
}

variable "min_insync_replicas" {
  description = <<-EOT
    Topic-level `min.insync.replicas`. 1 today because replication_factor is 1 —
    2 on a single-replica topic makes every acks=all produce fail with
    NOT_ENOUGH_REPLICAS. Move it to 2 in the same commit that moves
    replication_factor to 3. The module enforces min.isr <= RF at plan time.
  EOT
  type        = number
  default     = 1
}

variable "raw_providers" {
  description = <<-EOT
    Provider slugs that get an `odds.raw.<slug>` topic. Identical to envs/local on
    purpose — `ingest` selects its adapter at startup from ODDS_API_KEY, so the
    topic for the OTHER adapter has to already exist or switching providers
    becomes a deploy-plus-apply instead of a restart. An unused raw topic costs
    three partitions' worth of file handles.
  EOT
  type        = list(string)
  default     = ["synthetic", "the-odds-api"]
}
