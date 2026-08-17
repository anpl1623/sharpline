// ---------------------------------------------------------------------------
// envs/prod — Terraform + provider requirements, and the state backend
// ---------------------------------------------------------------------------
// READ THIS BEFORE RUNNING ANYTHING HERE.
//
// This root is AUTHORED AND VALIDATED, NOT YET APPLIED, and that is a statement
// about reachability rather than about readiness. CLAUDE.md §9 puts Kafka in the
// cluster as a StatefulSet, so its brokers answer on in-cluster DNS
// (`kafka-0.kafka.<ns>.svc.cluster.local:9092`) which a Terraform container on a
// laptop's docker bridge cannot resolve or route to. Phase 10 decides the wiring;
// deploy/terraform/README.md → "Applying prod" lists the two candidate answers.
//
// Until then `TF_ENV=prod make tf-init` and `tf-validate` work and are worth
// running in CI; `tf-plan` fails at the missing required variable, and if given
// one it fails on connect. It does NOT quietly create topics somewhere wrong —
// there is no default broker here for exactly that reason.
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
  // STATE: local for now, per CLAUDE.md §9 ("state local for now with a
  // documented path to remote backend + locking").
  //
  // Unlike envs/local, local state here is a genuine stopgap with a real failure
  // mode, and the migration is the FIRST thing phase 10 should do before a second
  // operator or a CI job ever applies this root: two concurrent applies against
  // one Kafka with two private state files will each believe it owns the topics.
  //
  // The full migration — backend block, credentials, and the one property the
  // backend MUST have for locking to be real — is written out in
  // deploy/terraform/README.md → "State and the remote-backend path". It is
  // deliberately not enabled here, because a backend block pointing at a bucket
  // that does not exist yet breaks `terraform init` for everyone, including the
  // CI job that only wants to validate.
  // -------------------------------------------------------------------------
  backend "local" {
    path = "terraform.tfstate"
  }
}

// ---------------------------------------------------------------------------
// The provider.
//
// `tls_enabled` is a VARIABLE here, not the hardcoded `false` of envs/local,
// because it is the one provider setting that legitimately changes with the
// deployment and changing it must not require editing this file under pressure.
//
// It defaults to false, and that default is honest rather than lazy: CLAUDE.md §9
// puts a default-deny NetworkPolicy with explicit allows in front of every
// workload, so broker traffic is confined to the pods permitted to speak to it,
// and §13.5 defers mTLS between services to phase 12 behind a service mesh. In
// that topology PLAINTEXT inside the mesh is the design, not an oversight. Flip
// this to true (and supply ca_cert / client_cert / client_key) the moment the
// broker gets a TLS listener — see the commented block below.
// ---------------------------------------------------------------------------
provider "kafka" {
  bootstrap_servers = split(",", var.bootstrap_servers)
  tls_enabled       = var.tls_enabled
  timeout           = var.timeout_seconds

  // When the broker gets a TLS listener, uncomment and feed these from
  // environment variables (TF_VAR_ca_cert, …) sourced from the cluster Secret —
  // never from a committed file. /.gitignore already refuses *.tfvars for this
  // reason.
  //
  // ca_cert     = var.ca_cert
  // client_cert = var.client_cert
  // client_key  = var.client_key
}
