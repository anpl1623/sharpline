# ADR 0005: Helm is the sole Kubernetes deploy path; no Kustomize

- **Status:** Accepted
- **Date:** 2026-08-16
- **Charter reference:** CLAUDE.md §8, §9, §14

## Context

Phase 10 deploys Sharpline to Kubernetes: six Go services, Postgres, Redis, and Kafka as
StatefulSets with PVCs, a migration Job, an Ingress, HPAs, PodDisruptionBudgets, and
default-deny NetworkPolicies with explicit allows. Every workload runs in-cluster —
§9 is explicit that the stateful components are **not** external managed services, because
the entire point of the project is that it computes on hardware the author controls.

That is a lot of manifests, and they have to differ between at least two environments:

| | `dev` (kind) | `prod` |
|---|---|---|
| Replicas | 1 everywhere | ≥2 where it matters |
| Resource requests/limits | small | real |
| Storage class | kind's local-path | whatever the target provides |
| Ingress host / TLS | `localhost`, internal CA | real host, real certificate |
| Image tags | locally built, `kind load`-ed | GHCR digests |
| Observability | may be off | always on |
| Secrets | `.env`-derived | sealed-secrets or SOPS |

Kubernetes offers two mainstream answers, and the default instinct on a greenfield project
is to reach for both: Kustomize for environment overlays because it is native to `kubectl`,
and Helm for packaging because it is what charts are distributed as. Using both is common
enough to feel like the safe choice.

It is not the safe choice, and this ADR exists so that instinct is met with a written
answer rather than being acted on.

## Decision

**Helm is the only Kubernetes deploy path in this repository. Kustomize is deliberately not
used — not as an overlay layer, not for a single resource, not for `kubectl apply -k` as a
convenience.**

One chart at `deploy/helm/`, with:

```
deploy/helm/
├── Chart.yaml
├── values.yaml          # defaults — the complete, documented surface
├── values-dev.yaml      # kind
├── values-prod.yaml     # the real target
└── templates/
```

Every environment difference is expressed as a **value**, never as a patch. Deployment is
`helm upgrade --install -f values-<env>.yaml`, run from the `tools` container like
everything else (ADR 0002).

### Why Helm rather than Kustomize, given only one may be chosen

- **Helm has release lifecycle; Kustomize has none.** `helm upgrade`, `helm rollback`,
  `helm history`, and `helm diff` operate on a named release with recorded revisions.
  Kustomize renders YAML and hands it to `kubectl apply`; rollback means finding the
  previous git commit, re-rendering, and re-applying, and there is no server-side record
  of what was deployed when. For a project whose CI/CD story (§9) includes a deploy job
  and whose demo involves changing things live, this is the decisive difference.
- **Hooks.** §9 requires `migrate` to run as a Job with `pre-install` / `pre-upgrade`
  hooks — schema migrations must complete before the new `api` pods roll. That is a
  first-class Helm annotation. Kustomize has no ordering concept at all; the equivalent is
  an external script or an Argo/Flux sync-wave, which means adopting a third tool to
  compensate.
- **Templating handles the actual variation here, and patching does not.** The six Go
  services are *the same shape*: same probe paths (`/healthz`, `/readyz`), same metrics
  port, same security context, same config wiring. One templated Deployment plus a
  `services:` map in `values.yaml` expresses that in one place. Kustomize's strategic-merge
  patches would require six near-identical base manifests plus per-environment patches on
  each — a 12-file matrix maintaining what one template and one map express, and every
  cross-cutting change (a new probe, a changed security context) would need six edits
  instead of one.
- **`values.yaml` is self-documenting.** A reader opening one file sees the entire
  configurable surface. Reconstructing the same picture from a Kustomize overlay tree means
  reading base plus overlay plus patches and mentally executing the merge.
- **Helm is the industry-standard packaging story**, and §14 lists it as a parity item with
  FanDuel's stack. `helm package` produces a versioned, distributable artifact; Kustomize
  produces a directory.

### Why not both

This is the part that needs writing down, because "Kustomize for overlays, Helm for
packaging" is a real and defensible pattern in larger organizations.

**Maintaining two deploy systems on a solo project buys nothing and costs continuously.**
Concretely:

- **Two places to look when a deploy is wrong.** The most common failure is a value that
  did not reach the pod. With one system that is one render to inspect. With two it is a
  render, then a patch application, then working out which layer won.
- **Two mental models of "how does dev differ from prod".** A contributor — including the
  author in six months — has to know that replicas come from values but the storage class
  comes from a patch. Every such split is a place to guess wrong.
- **Two toolchain entries in the `tools` image**, both of which must be pinned, both of
  which get version bumps, and both of which are in the critical path of every deploy.
- **`helm template | kustomize build` composition is fragile.** It loses Helm's release
  tracking and hooks — the two things Helm was chosen for — leaving the worst of both.

The charter's phrasing is exact and worth quoting: *"maintaining two deploy systems on a
solo project buys nothing, and Helm is the industry-standard packaging story."* Note that
`kubectl` on the author's machine ships with Kustomize v5.8.1 built in, so it is
permanently one keystroke away. Availability is not a reason to adopt it.

## Consequences

**Made easier.**

- One command deploys any environment:
  `helm upgrade --install sharpline ./deploy/helm -f values-<env>.yaml`.
- Rollback is real and server-side: `helm rollback sharpline <revision>`.
- The migration ordering problem — Job completes, then `api` rolls — is solved by an
  annotation rather than by orchestration glue.
- The six Go services share one templated Deployment. A new probe, a changed security
  context, or an added env var is one edit, not six.
- `helm diff upgrade` in CI gives a genuine review artifact showing exactly what a merge
  would change in the cluster.
- The chart is a versioned, publishable artifact, which makes the "deployable from a single
  command" claim in §0 concrete.

**Made harder, and accepted.**

- **Go templating inside YAML is genuinely unpleasant.** Whitespace control (`{{-` / `-}}`),
  `nindent`, and `toYaml` are error-prone, and a template bug produces invalid YAML with an
  unhelpful error. Mitigated by `helm lint` and `helm template --validate` in CI, both of
  which are mandatory gates and not optional niceties.
- **Values structure has to be designed up front.** A `values.yaml` shape that turns out
  wrong is expensive to change once `values-dev.yaml` and `values-prod.yaml` both depend on
  it. This is deliberate design work in phase 10, not something to grow organically.
- **The chart becomes a second place where topology lives**, alongside the compose stack.
  Port numbers, service names, env var names, and health-check paths are stated in both and
  can drift. Mitigated by both being generated from the same frozen topology and by the
  proxy/Ingress shape being deliberately identical, but the risk is real and there is no
  compile-time check on it.
- **Debugging requires `helm template` as a routine step**, since the applied manifest is
  never the file you edited. This is a habit to build, not a blocker.
- **Secret handling needs a decision Helm does not make.** Values files are committed;
  credentials cannot be. Sealed-secrets or SOPS is required before anything is pushed
  public (§9), and that is a separate concern this ADR does not resolve.
- **No Kustomize means no `kubectl apply -k` quick hack.** Every change goes through the
  chart, including one-off experiments. That friction is a feature: it is what stops the
  cluster's real state from diverging from what is in git.

## Alternatives considered

### Kustomize only — rejected

Native to `kubectl`, no templating language, patches are plain YAML, and the rendered
output is trivially inspectable. For a mostly-static manifest set it is genuinely the more
pleasant tool, and the "no templating in YAML" argument is a good one.

Rejected on three specific gaps, each of which is load-bearing here:

1. **No release lifecycle** — no `rollback`, no `history`, no server-side record of what is
   deployed.
2. **No hooks** — the `pre-install` / `pre-upgrade` migration Job §9 requires has no
   equivalent, and the workaround is a third tool.
3. **Patch fan-out on six same-shaped services** — a 12-file matrix expressing what one
   template and one values map express.

### Both, Kustomize overlaying `helm template` output — rejected

The pattern larger organizations converge on, and it is defensible at organizational scale
where different teams own the chart and the environment configuration.

Rejected because composing them **discards exactly the two capabilities Helm was chosen
for**: `helm template` emits YAML with no release tracking and no hook execution, so
rollback and migration ordering are both lost. What remains is Helm's templating plus
Kustomize's patching plus neither one's lifecycle — strictly worse than either alone. On a
solo project there is also no organizational boundary for the split to align with.

### Plain manifests plus `envsubst` or `sed` — rejected

Zero dependencies beyond `kubectl` and maximally transparent.

Rejected because it reinvents templating badly. No schema, no validation, no lint, no
dry-run, no ordering, and no rollback. It works until the first environment-specific
difference that is structural rather than a scalar — a NetworkPolicy that exists in prod
and not in dev, say — at which point it needs conditionals, and now it is a templating
language with none of the tooling.

### Argo CD / Flux with GitOps — deferred, not rejected

Genuinely the right end state and it composes with Helm rather than competing: both render
charts and reconcile continuously, which would give drift detection this ADR's approach
lacks.

Deferred because it requires a persistently running cluster to be meaningful, and §13.2
lists the cluster target as still open — kind locally is settled, but whether the live demo
runs on a home server, a VPS with k3s, or a managed cluster is not. GitOps on an ephemeral
kind cluster demonstrates nothing that `helm upgrade` does not. **Revisit once the cluster
target is decided**; nothing in this ADR obstructs it, since Argo and Flux both deploy the
chart this ADR mandates.

### Terraform's `helm_release` provider as the deploy mechanism — rejected for the app, retained for infrastructure

Terraform already owns the kind cluster, Kafka topics, Grafana dashboards, and namespaces
(§9), so deploying the chart from Terraform too would give one entrypoint.

Rejected for the application chart because it puts the application's release lifecycle
inside Terraform state, where a failed apply can leave the state and the cluster
disagreeing, and where a routine image bump becomes a `terraform apply` on infrastructure
state. Application deploys should be fast, frequent, and independently rollback-able.

The division of labour stands as §9 describes it: **Terraform owns infrastructure that
changes rarely; Helm owns the application that changes constantly.**
