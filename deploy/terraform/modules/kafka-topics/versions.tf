// ---------------------------------------------------------------------------
// modules/kafka-topics — provider requirements
// ---------------------------------------------------------------------------
// A child module declares WHICH providers it needs and at what versions. It
// must never contain a `provider` block: provider configuration belongs to the
// root module (deploy/terraform/envs/{local,prod}), which is what makes the
// same module usable against the compose broker and against the cluster's
// StatefulSet without an edit here.
//
// The version is an EXACT pin, not a range. CLAUDE.md §12: "Base images are
// pinned by digest, not by floating tag" — a provider is the same class of
// dependency, and `~> 0.13` would let a minor bump change topic-config diffing
// behaviour between a laptop init and a CI init. The committed
// `.terraform.lock.hcl` in each env root freezes the artifact checksums on top
// of this, for BOTH linux/arm64 and linux/amd64 (see deploy/terraform/README.md
// → "Lock files").
//
// `required_version` intentionally tracks the minor series rather than the
// patch: the real pin is TERRAFORM_VERSION in deploy/docker/tools.Dockerfile,
// which is the only Terraform that ever runs (prime directive — Terraform is
// not a host dependency). This constraint exists to fail loudly if someone
// points a much older binary at the config, notably one predating the 1.10
// S3-backend `use_lockfile` support the documented remote-backend path relies
// on.
// ---------------------------------------------------------------------------

terraform {
  required_version = "~> 1.15"

  required_providers {
    kafka = {
      source  = "Mongey/kafka"
      version = "0.13.1"
    }
  }
}
