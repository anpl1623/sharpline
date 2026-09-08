# `deploy/terraform`

Terraform owns the Kafka topics. Nothing creates one by hand.

> CLAUDE.md §9 — "**Terraform.** Nothing is provisioned by hand. […] The reason
> Terraform earns its place beyond parity: Kafka topic configuration is exactly
> the kind of thing that gets created once with a CLI flag, forgotten, and then
> silently differs between laptop and cluster. Declaring it removes that failure
> mode entirely. Runs from the `tools` container like everything else."

Three things enforce that mechanically, not by convention:

- the broker starts with `KAFKA_AUTO_CREATE_TOPICS_ENABLE=false`, so a typo in a
  producer cannot conjure a topic (`deploy/compose/compose.yaml`);
- `make kafka-topics` is read-only — it lists and describes, it does not create;
- there is no `terraform` on the host. It lives in `deploy/docker/tools.Dockerfile`
  and every `make tf-*` target is a `docker compose run`.

```
deploy/terraform/
├── modules/kafka-topics/    the topic catalogue + validation  (the interesting part)
└── envs/
    ├── local/               the compose broker            (kafka:9092, RF 1)
    └── prod/                the cluster StatefulSet        (authored, not yet applied)
```

## Targets

Every one runs inside the `tools` container. `TF_ENV` selects the environment root
and defaults to `local`.

| Target | What it does |
|---|---|
| `make tf-init` | Provider download + backend init. Writes the **committed** `.terraform.lock.hcl` |
| `make tf-validate` | Syntax, types, module and variable wiring. No broker needed |
| `make tf-fmt` / `tf-fmt-check` | Canonical formatting; `-check` writes nothing and is CI-safe |
| `make tf-plan` | Shows the diff |
| `make tf-drift` | Same plan with `-detailed-exitcode` — **fails if anything is pending** |
| `make tf-apply` | Creates/updates topics. `ARGS=-auto-approve` to skip the prompt |
| `make tf-output` | Topic names, partition map, per-topic config as JSON |
| `make tf-show` | Recorded state |
| `make tf-lock` | Re-records provider checksums for **every** target platform |
| `make tf-providers` | Terraform version + which provider versions are locked |
| `make tf-destroy` | Deletes the managed topics. Destroys topic data |

Prod:

```sh
TF_ENV=prod make tf-init
TF_ENV=prod make tf-validate
TF_ENV=prod make tf-plan TF_BOOTSTRAP=kafka-0.kafka.sharpline-prod.svc.cluster.local:9092
```

### `tf-drift` is the target that matters

An empty plan after an apply is not a nicety. A Terraform config that never
converges proposes changes on every run, which makes real drift indistinguishable
from the config's own noise — and at that point "declared state *is* the real
state" is no longer a checkable claim, which is the entire argument §9 makes for
declaring topics at all. `tf-drift` exits non-zero on any pending change, so it
can be wired into CI.

## Ownership boundary

Terraform owns **topics and their configuration**. It does *not* own:

- **the broker.** That is a container (`deploy/compose/compose.yaml` locally, a
  StatefulSet in the chart from phase 10). Terraform connects to a broker that is
  already running; there is no offline mode, and `make tf-plan` refuses to run
  against a stopped local Kafka rather than timing out cryptically.
- **consumer groups or offsets.** Those are created by the consumers themselves and
  are runtime state, not declared infrastructure. Resetting an offset is a
  `make kafka-groups` operation.
- **message content.** Terraform creates empty topics. Empty topics after
  `make up` + `make tf-apply` are the *correct* state — no seeding, no canned
  payloads (CONTRACT.md → "NO MOCK DATA").

§9 also assigns Terraform the kind cluster, namespaces, and Grafana dashboards and
alert rules. Those arrive with phase 10; the Grafana artifacts currently live as
provisioned files under `deploy/observability/` and moving them behind the Grafana
provider is a phase-10 task, not a phase-3 one.

## Provider

[`Mongey/kafka`](https://registry.terraform.io/providers/Mongey/kafka/latest) — the
de facto Terraform Kafka provider — pinned to an **exact** version, `0.13.1`. Not a
`~>` range: CLAUDE.md §12 pins base images by digest rather than tag, and a provider
is the same class of dependency. A minor bump that changed how topic configs are
diffed would turn a converged config into a permanently non-empty plan, and it would
do it on whichever machine ran `init` most recently.

One provider default is a genuine trap: **`tls_enabled` defaults to `true`.**
Omitting it against a `PLAINTEXT` listener does not mean "no TLS", it means every
AdminClient call fails during the handshake with an error that reads like a network
fault. Both env roots set it explicitly.

### Lock files

`.terraform.lock.hcl` is committed in each env root (`/.gitignore` says so
explicitly: "Lockfiles ARE committed"). It carries checksums for **`linux_amd64`
and `linux_arm64`**, and both are required:

- the deploy target is an Oracle Cloud Ampere box — **arm64**, and so is the dev
  Mac (CONTRACT.md corrects §9's "amd64 for the server" here);
- CI runners are amd64, and `tf-init` on an amd64 runner against a lock file
  holding only arm64 hashes fails with *"checksums previously recorded in the
  dependency lock file … no matching hash"*. That break cannot be reproduced on the
  machine that caused it.

`terraform init` records hashes for the current platform only, so regenerate with:

```sh
make tf-lock            # both platforms, from TF_LOCK_PLATFORMS
```

## Replication factor

`replication_factor` and `min_insync_replicas` are the **only** topic-affecting
values that differ between environments, and they have no shared default. That is
deliberate: replication factor is a property of *how many brokers exist*, not of
what a topic means. The identical topic needs RF 1 against the single-container
KRaft broker and RF 3 against a three-broker cluster, so a hardcoded value in the
module would be wrong in one of the two — and wrong loudly in one direction
(creation fails with *"Replication factor: 3 larger than available brokers: 1"*)
and silently in the other (a single broker loss loses the compacted snapshot).

| | RF | `min.insync.replicas` | Why |
|---|---|---|---|
| `local` | 1 | 1 | One broker. Any other value fails outright |
| `prod` **today** | 1 | 1 | The cluster is one Oracle A1 node — one broker |
| `prod` **at ≥3 brokers** | 3 | 2 | Survives a single broker loss with `acks=all` |

The two must move **in the same commit**. RF 3 with `min.isr` 1 is the worst of the
three combinations: it looks replicated while a producer using `acks=all` is
satisfied by the leader alone, so a failover loses acknowledged writes — a dropped
settlement event on `wager.events`, a hole in the snapshot on `odds.normalized`.
`unclean.leader.election.enable` is already declared `false` on every topic so that
bump needs no config archaeology.

Everything else — names, partition counts, cleanup policy, retention and compaction
windows — is identical across environments by construction. `tf-output` is
deliberately the same set of outputs in both roots so the two can be diffed; if
anything but `brokers` and `min.insync.replicas` differs, something
environment-shaped has leaked into the topic definitions.

## State and the remote-backend path

State is **local** today, which is what CLAUDE.md §9 asks for ("state local for now
with a documented path to remote backend + locking"). `*.tfstate` is gitignored —
state can hold provider credentials in plaintext.

The two environments are local-state for different reasons, and only one of them is
a stopgap:

- **`local` — local state is correct, not provisional.** The state describes topics
  on a container that lives and dies with `make down-v`. Sharing that in a remote
  backend would invite two laptops to share one state file describing two different
  ephemeral brokers, which is strictly worse than not sharing.
- **`prod` — local state is a real liability** and migrating it should be the first
  thing phase 10 does, *before* a second operator or a CI job ever applies this
  root. Two concurrent applies with two private state files will each believe it
  owns the topics.

### The migration

The deploy target is Oracle Cloud, so the natural backend is **OCI Object Storage
through its S3-compatible endpoint**, with Terraform's native S3 state lock. Add to
`envs/prod/versions.tf`, replacing the `backend "local"` block:

```hcl
backend "s3" {
  bucket = "sharpline-tfstate"
  key    = "kafka-topics/prod.tfstate"
  region = "us-chicago-1"

  endpoints = {
    s3 = "https://<namespace>.compat.objectstorage.us-chicago-1.oraclecloud.com"
  }

  use_path_style = true
  use_lockfile   = true   # see the caveat below — verify before trusting it

  # OCI's S3 compatibility layer is not AWS; these skips are required, not sloppy.
  skip_region_validation      = true
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_s3_checksum            = true
}
```

Credentials are an OCI *Customer Secret Key* supplied as
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` from the environment. Never in a
`.tfvars` (`/.gitignore` refuses `*.tfvars` for this reason) and never in the
backend block.

Two things to get right, stated as open items rather than glossed:

1. **`use_lockfile` needs conditional writes.** Terraform 1.10+ implements S3 state
   locking with a `.tflock` object written under `If-None-Match`, which replaced the
   old DynamoDB table — good news here, since there is no DynamoDB on OCI. But it
   requires the endpoint to honour S3 conditional PUT. **Verify that against OCI's
   compatibility layer before relying on it**; if it is not honoured the lock
   silently does nothing, which is worse than an obvious absence of locking. The
   test is two concurrent applies: one must fail to acquire the lock.
2. **Migrating carries the existing state.** `terraform init -migrate-state` after
   swapping the block, then confirm with `make tf-show` that the topics are still
   tracked — an empty state against live topics makes the next apply propose
   creating everything, and on a compacted topic recreation discards the snapshot.

If OCI's conditional-write support turns out to be the blocker, the fallback is any
S3-compatible store that does honour it (Cloudflare R2, MinIO ≥ RELEASE.2024) or an
HTTP backend with locking. Do not fall back to "local state and be careful".

## Applying prod

`TF_ENV=prod` can be `init`ed and `validate`d today — worth doing in CI, since it
catches a broken module contract without a broker. It **cannot be applied yet**, and
the reason is reachability, not readiness: §9 runs Kafka as a StatefulSet, so its
brokers answer on in-cluster DNS that a Terraform container on a laptop's Docker
bridge cannot route to.

Two candidate answers, both phase 10:

1. **`kubectl port-forward` from the `tools` container** (which already carries
   `kubectl`), with `TF_BOOTSTRAP=localhost:9092`. Simple, and adequate for a
   human-run apply. It does not survive being put in CI — the forward is a
   foreground process with no health signal.
2. **Run Terraform as a Job inside the cluster**, alongside the `migrate` Job that
   already exists in the chart. This is the same argument §9 makes for migrations
   ("never a binary someone runs by hand") and it is the answer that composes with
   a deploy pipeline. It needs the state backend above to exist first.

There is deliberately **no default `bootstrap_servers` in `envs/prod`**. The only
broker address that resolves from a developer's laptop is the compose one, so any
convenient default would make `TF_ENV=prod make tf-apply` reconfigure the *local*
broker's topics under prod's state file. Failing with *"No value for required
variable"* is the correct outcome.

## The topics

Full reasoning — partition counts, the ordering guarantee, every compaction and
retention value — is in `modules/kafka-topics/variables.tf`, next to the values it
explains. Summary:

| Topic | Partitions | Policy | Window | Key |
|---|---|---|---|---|
| `odds.raw.synthetic` | 3 | `delete` | 72 h, ≤ 1 GiB/partition | provider-shaped |
| `odds.raw.the-odds-api` | 3 | `delete` | 72 h, ≤ 1 GiB/partition | provider-shaped |
| `odds.normalized` | 6 | `compact` | snapshot, no expiry | `market_id` |
| `price.computed` | 6 | `compact` | snapshot, no expiry | `market_id` |
| `wager.events` | 3 | `delete` | 90 d, unlimited bytes | wager-shaped |
| `signals.ev` | 3 | `delete` | 7 d, ≤ 512 MiB/partition | `market_id` |
| `signals.arb` | 3 | `delete` | 30 d, ≤ 256 MiB/partition | `market_id` |
| `signals.steam` | 3 | `delete` | 30 d, ≤ 256 MiB/partition | `market_id` |
| `signals.clv` | 3 | `delete` | 90 d, unlimited bytes | `wager_id` |

The four `signals.*` topics are phase 9's analytics output — the +EV finder,
arbitrage, steam and CLV. Three are named in CLAUDE.md §3's event-flow diagram;
`signals.ev` is a flagged addition. **None of them is compacted**, because a
finding is an event rather than a snapshot and the newest one supersedes nothing.
`modules/kafka-topics/README.md` has the argument and the sizing.

33 application partitions. Adding a provider is one string in `raw_providers` — the
topic, its retention, its size cap and its partition count come out identical to the
others by construction.

**Two traps the module refuses to let you fall into**, enforced as plan-time
preconditions so they apply to any topic added later:

1. **A compacted topic whose cleaner never runs.** Kafka never compacts the *active*
   segment, and the defaults (`segment.bytes` 1 GiB, `segment.ms` 7 days) mean a
   topic producing less than a gigabyte per partition per week has exactly one
   segment, always active — so compaction **never happens at all**. The topic still
   answers correctly if you read to the end; it just also carries every superseded
   value forever, and §3's "the compacted topic *is* the snapshot" quietly becomes
   false. Declaring a compacted topic therefore requires `segment.ms`,
   `min.cleanable.dirty.ratio`, `max.compaction.lag.ms` and `delete.retention.ms`.
2. **A retention topic whose data outlives its window.** Same mechanic in reverse:
   deletion is per-segment and skips the active segment, so `segment.ms >
   retention.ms` makes the declared window fiction. Both values are required, and
   `segment.ms < retention.ms` is enforced.
