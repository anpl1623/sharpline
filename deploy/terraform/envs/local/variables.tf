// ---------------------------------------------------------------------------
// envs/local — inputs
// ---------------------------------------------------------------------------

variable "bootstrap_servers" {
  description = <<-EOT
    Comma-separated Kafka bootstrap brokers. Same string format as the
    application's SHARPLINE_KAFKA_BROKERS so there is one format and one value
    across Terraform and the Go services.

    The default is the FROZEN local topology (CONTRACT.md): a single KRaft broker
    reachable as `kafka:9092` by service-name DNS on the compose bridge network.
    It is a default rather than a required input because `make tf-apply` on a
    clean clone has to work with no arguments — and because there is exactly one
    correct answer here: nothing binds to a host port except the proxy
    (CLAUDE.md §12), so the broker is only ever reachable at this address, from
    inside a container on that network.

    Override with `make tf-plan TF_BOOTSTRAP=other:9092`.
  EOT
  type        = string
  default     = "kafka:9092"
}
