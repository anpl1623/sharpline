# ADR 0002: Every process runs in a container

- **Status:** Accepted
- **Date:** 2026-08-16
- **Charter reference:** CLAUDE.md, prime directive; §9, §10, §11, §12

## Context

Sharpline's central claim is that it **runs on self-hosted compute** — that a reviewer, or
the author on a different machine, or a bare CI runner, can bring the whole system up and
watch odds move. That claim is either true or it is marketing, and what makes the
difference is whether the repository has any hidden dependency on one particular
developer's laptop.

The failure mode is specific and it is the normal outcome:

1. Someone scaffolds a Go module and runs `go test ./...` on the host "just to get
   started". It works, because Go is installed.
2. A Makefile target grows around it: `test: go test -race ./...`.
3. Six weeks later `make test` is load-bearing, three more targets have copied its shape,
   and CI has an `actions/setup-go` step to make them pass.
4. A contributor with only Docker clones the repository and nothing works. The README
   grows a "Prerequisites" section listing five toolchains and three version constraints.
5. The self-hosted claim is now false and the retrofit costs more than the original build.

Nothing in that sequence is a mistake anyone notices at the time. Each step is locally
reasonable. That is exactly why the constraint has to be structural rather than
aspirational — a guideline that says "prefer containers" loses to convenience every single
time, because convenience wins every individual argument on its own local merits.

There is a second motivation. This project's Kubernetes work (phase 10) is a stated
deliverable. If application code is developed against a host toolchain and only
containerized at the end, containerization becomes a bolt-on: bind mounts that only work
on macOS, binaries that need a libc that distroless does not have, config read from paths
that exist on a laptop. Building container-first makes Kubernetes a natural extension of
what already exists rather than a port.

## Decision

**Every process in this system runs in a container. The host is allowed exactly one
dependency: a container runtime.**

That means Docker, plus `kind` for local Kubernetes — and `kind` qualifies because it runs
its cluster nodes as containers, so the directive holds all the way down.

A contributor with nothing but Docker installed — no Go toolchain, no Node, no `psql`, no
`goose`, no `golangci-lint`, no Terraform, no Python — must be able to clone this
repository and reach a fully running system with `make up`.

**This outranks every convenience argument that will come up later.** There is no "just
run this one thing on the host" exception, and the absence of an exception is the entire
mechanism.

### Consequences that are honored, not worked around

- **Builds happen in containers.** `make build` invokes a builder image. It does not shell
  out to a host `go build`.
- **Tests run in containers.** `make test` runs the Go test image with the Docker socket
  mounted, so `testcontainers-go` spawns real Postgres, Redis, and Kafka as *sibling*
  containers on the host daemon.
- **The frontend dev server runs in a container with hot reload intact.** `web/src` is
  bind-mounted; `node_modules` and `.next` live on named volumes so host-architecture
  binaries — `sharp`, SWC, esbuild — never collide with the container's.
  `WATCHPACK_POLLING=true`, because bind-mount filesystem events are unreliable across the
  Docker VM boundary on macOS.
- **Migrations run as a container** — a compose service locally, a Kubernetes Job in the
  cluster. Never a binary someone runs by hand.
- **One-off tooling is a `make` target wrapping `docker run`.** Linting, codegen, OpenAPI
  generation, `psql` and Kafka CLI shells, Terraform, Locust, Playwright. Nothing in the
  Makefile assumes a host toolchain.
- **Dependency changes go through the container.** `npm install` runs inside the container
  and writes the updated lockfile back through the mount. Never install into
  `web/node_modules` from the host.
- **CI installs no toolchains.** No `actions/setup-go`, no `actions/setup-node`. Every job
  runs `make <target>`. The runner is treated as a bare machine with Docker and nothing
  else.
- **Base images are pinned by `@sha256:` digest**, not by floating tag. A digest is what
  makes "reproducible" mean reproducible; Renovate or Dependabot proposes the bumps as
  reviewed changes.
- **Only the proxy publishes a host port.** Debugging a service means going through the
  proxy or `docker compose exec`, not publishing another port.

### The enforcement mechanism

The rule is not enforced by good intentions. It is enforced by CI running on a bare
runner: **any step that quietly depends on a host toolchain fails in CI even though it
passed on the author's Mac.** That is the whole design. Go 1.26 and Node being installed
on the author's machine is a convenience for editor LSP and nothing else, and CI is the
oracle that says so.

Rejected review states, listed so they are unambiguous:

- any Makefile recipe invoking `go`, `npm`, `npx`, `node`, `psql`, `goose`, `terraform`,
  or `golangci-lint` directly
- any CI job using `actions/setup-go` or `actions/setup-node`
- any base image pinned by floating tag rather than digest
- any published host port other than the proxy's

## Consequences

**Made easier.**

- The self-hosted claim is *true*, and demonstrably so, rather than aspirational.
- Onboarding is one sentence: install Docker, run `make up`. A project that needs a README
  ritual to start does not survive contact with a reviewer.
- Kubernetes (phase 10) becomes a natural extension. The compose topology — internal
  network, service-name DNS, one published entrypoint, migrations as a job — is the same
  shape as the cluster topology, so the two cannot drift into being different systems.
- Reproducibility is real. Digest-pinned images plus containerized builds means the build
  from three months ago still produces the same artifact.
- Security posture is uniform and strong: static binaries into `distroless/static:nonroot`,
  non-root UID, read-only root filesystem, `cap_drop: ALL`, no shell in the final layer.

**Made harder, and accepted.**

- **The inner loop is slower.** A containerized `go build` cannot use a warm host build
  cache by default. This is mitigated with BuildKit cache mounts on the Go module and
  build caches — and the mitigation is *load-bearing*, not an optimization. Without it,
  containerized builds get slow enough that people start cheating and running `go build`
  on the host, which breaks the directive. Slow builds are the primary threat to this ADR.
- **Editor integration is a manual step.** Go LSP and TypeScript language services on the
  host need a host toolchain to be useful. This is permitted **for editor tooling only**,
  and the versions installed on a developer's machine are explicitly not part of any build
  path. They are recorded in the charter solely so that "it works on my machine because I
  have Go 1.26" can be diagnosed.
- **Debugging is more indirect.** No attaching a host debugger to a host process; it is
  `docker compose exec`, delve inside the container, or logs.
- **The Makefile carries real complexity.** Volume mounts, UID mapping so container-written
  files are not root-owned on the host, and the Docker-socket mount for testcontainers all
  have to be right. This complexity is centralized in one file on purpose — it is paid
  once instead of by every contributor.
- **Docker-socket mounting for testcontainers is a genuine privilege concern.** A test
  process with the daemon socket can control the host's containers. Accepted because the
  alternative — Docker-in-Docker — is slower and has its own privileged-container problem,
  and because the socket is mounted only into the test image, only in the `test` target.
- **File ownership on Linux hosts.** Containers writing into bind mounts as root produces
  root-owned files on the host. Handled with UID/GID mapping in the tooling targets.

**Costs knowingly paid.**

Convenience, in small daily increments, in exchange for one large structural guarantee.
Every individual violation of this directive would save a few seconds and cost nothing
visible; that is precisely why the answer to each of them has to be no.

## Alternatives considered

### A host toolchain with containers only for deployment — rejected

The industry-normal arrangement: develop natively, build a container in CI. It is faster
day to day and every developer already knows how it works.

Rejected because **it makes the project's central claim false.** The whole framing is that
this runs on self-hosted compute reproducibly; a repository requiring five host toolchains
does not demonstrate that. It also reintroduces the exact drift this ADR exists to
prevent — the "works locally, fails in the image" class of bug — precisely at the point in
the pipeline where it is most expensive to find.

### Nix or devcontainers for reproducible host environments — rejected

Both give genuine reproducibility. Nix is arguably *more* rigorous than containers.

Rejected on the same criterion, applied honestly: they add a host dependency that is not a
container runtime. Nix additionally has a steep learning curve orthogonal to everything
else this project is meant to teach, and devcontainers couple the workflow to a specific
editor. Docker Compose is the thing a reviewer already has installed and already
understands, and legibility to a reviewer is a real requirement here.

### "Prefer containers" as a guideline rather than a mandate — rejected

This is the failure mode described in Context, not an alternative to it. A soft preference
loses to convenience in every individual case, because convenience genuinely does win each
argument on its own local merits. The mandate is valuable *because* it is absolute: with
no exception, there is no first exception, and no slope to slide down.

### Podman / containerd instead of Docker — not rejected, deferred

Nothing in this ADR requires Docker specifically; it requires *a container runtime*. Docker
is named because it is what the author has, what CI runners have, and what
`testcontainers-go` and `kind` are best exercised against. A Podman-compatible path is a
welcome future contribution and would not violate anything here — the mandate is about the
host having exactly one dependency, not about which one.
