// ---------------------------------------------------------------------------
// envs/local — Terraform + provider requirements, and the state backend
// ---------------------------------------------------------------------------

terraform {
  required_version = "~> 1.15"

  required_providers {
    kafka = {
      source  = "Mongey/kafka"
      version = "0.13.1"
    }
  }

  // -------------------------------------------------------------------------
  // STATE: local, deliberately, with a documented path off it.
  //
  // CLAUDE.md §9: "state local for now with a documented path to remote backend
  // + locking." The path is written out in full in deploy/terraform/README.md →
  // "State and the remote-backend path", including the reason the migration is
  // not being made pre-emptively.
  //
  // For THIS environment local state is not a stopgap, it is correct: the state
  // describes topics on a Kafka container that lives and dies with
  // `make down-v`. Putting that in a shared remote backend would invite two
  // laptops to share one state file describing two different ephemeral brokers,
  // which is strictly worse than no sharing at all.
  //
  // `terraform.tfstate` is gitignored (see /.gitignore → "Terraform"), because
  // state can contain provider credentials in plaintext. `.terraform.lock.hcl`
  // in this directory IS committed.
  //
  // `path` is relative to this directory, so `make tf-*` and a `cd` here agree
  // on which file is the state.
  // -------------------------------------------------------------------------
  backend "local" {
    path = "terraform.tfstate"
  }
}

// ---------------------------------------------------------------------------
// The provider talks PLAINTEXT to the compose broker.
//
// `tls_enabled` DEFAULTS TO TRUE in Mongey/kafka, so omitting it here does not
// mean "no TLS" — it means every AdminClient call fails on a handshake against
// a PLAINTEXT listener, with an error that reads like a network problem.
// deploy/compose/compose.yaml puts the broker on
// `PLAINTEXT://0.0.0.0:9092` with no TLS listener at all, and that is correct
// for a single internal bridge network where nothing is published to the host
// (CLAUDE.md §12). So it is set explicitly and false.
//
// This is also the line that must change first when Kafka moves into the
// cluster: see envs/prod/versions.tf.
// ---------------------------------------------------------------------------
provider "kafka" {
  bootstrap_servers = split(",", var.bootstrap_servers)
  tls_enabled       = false

  // The AdminClient default is generous enough that a broker which is up but not
  // yet accepting metadata requests produces a long unexplained hang. 10s turns
  // "make tf-plan appears stuck" into a prompt error naming the broker.
  timeout = 10
}
