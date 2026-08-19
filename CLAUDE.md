# Sharpline

A real-time sports odds platform — FanDuel-parity feature surface, built to run entirely on self-hosted compute.

This file is the project charter. It was written before any code existed. Read it fully before the first commit.

---

## Prime directive: everything runs in a container

Non-negotiable, and it outranks every convenience argument that will come up later.

**Every process in this system runs in a container.** Backend services, the Next.js frontend, Postgres, Redis, Kafka, migrations, the observability stack, the reverse proxy, load tests, browser tests, and the developer tooling itself. There is no "just run this one thing on the host" exception.

**The host is allowed exactly one dependency: a container runtime.** Docker (plus `kind` for local Kubernetes, which itself runs its nodes as containers). A contributor with nothing but Docker installed — no Go toolchain, no Node, no `psql`, no `golangci-lint` — must be able to clone the repo and reach a fully running system.

**Consequences that must be honored, not worked around:**

- Builds happen in containers. `make build` invokes a builder image; it does not shell out to a host `go build`.
- Tests run in containers. `make test` runs the Go test image; `testcontainers-go` spawns real dependencies from inside it via a mounted Docker socket.
- The frontend dev server runs in a container with hot reload intact (source bind-mounted, `node_modules` on a named volume so the host arch never leaks into the image).
- Migrations run as a container — a compose service locally, a Kubernetes Job in the cluster. Never a binary someone runs by hand.
- One-off tooling (linting, codegen, OpenAPI generation, `psql` and Kafka CLI shells, Terraform, Locust, Playwright) is exposed as `make` targets that wrap `docker run`. Nothing in the Makefile assumes a host toolchain.
- Go and Node being installed on the author's Mac is a convenience for editor LSP and nothing else. **If a build or test step only works because the host has Go 1.26 installed, that step is broken.** Verify by running CI, which starts from a bare runner.

Rationale: this is the property that makes the "runs on my own server" claim true rather than aspirational, makes the Kubernetes work a natural extension rather than a bolt-on, and makes the repo reproducible for anyone who looks at it.

---

## 0. What this is (and is not)

**Is:** a production-shaped, self-hosted sportsbook *simulation*. Real odds ingested from a licensed data provider, normalized, priced, and streamed live to a browser over WebSocket. Play-money wagering with a real double-entry ledger, real settlement, real risk accounting.

**Is not:** a licensed sportsbook. No real money moves. No KYC, no geolocation gating, no payment processing, no custody of funds. This distinction must be stated in the README and on the landing page — an unlicensed real-money book is a legal liability on a resume; a rigorous simulation of one is an engineering credential.

**Success condition:** the author opens a laptop, points a browser at a service running on their own hardware, and watches odds move in real time — with the ingestion, pricing, storage, and fanout all computed locally, observable in Grafana, and deployable to a Kubernetes cluster from a single command.

---

## 1. Environment (verified 2026-08-16)

| Tool | Status |
|---|---|
| Go | 1.26.5 darwin/arm64 — installed |
| Node | v26.6.0 — installed |
| Docker CLI | 29.7.1 — installed, **daemon not running** (start Docker Desktop before any compose work) |
| kubectl | v1.36.3, Kustomize v5.8.1 — installed, **no cluster contexts configured** |
| Rust | not installed |
| Python | 3.14.5 |

Per the prime directive, **only Docker is a real dependency.** Go, Node, and Python on this machine exist for editor tooling and are not part of any build path. Their versions are recorded here to diagnose the failure mode where someone accidentally couples a build step to the host toolchain.

Local Kubernetes will need `kind` installed (it runs cluster nodes as Docker containers, so it fits the directive). Prefer `kind` over `k3d` — it is the closest to a real cluster and the manifests written against it port cleanly to a managed cloud cluster later.

---

## 2. Language choice: Go (backend)

The decision, and the reasoning, so it does not get relitigated:

**The workload is a fan-in / fan-out problem.** N provider connections stream odds in; M browser clients stream odds out; in between sits a pricing stage that must not block either side. This is precisely the shape Go's scheduler and channels were designed for. A goroutine per poller and per client connection is idiomatic and cheap (~2–8KB stack), so tens of thousands of concurrent subscribers fit on one modest box.

**Latency is predictable.** Go's concurrent collector holds sub-millisecond pause times at this heap size. Odds staleness is the product's core quality metric — a 200ms GC pause is a visible defect in a live line ticker.

**Deployment story is the best of any candidate.** `CGO_ENABLED=0` produces one static binary; the container is `FROM scratch` or distroless at ~15–25MB. Fast cold starts make HPA scale-up meaningful and keep the k8s demo honest rather than sluggish.

**Rejected alternatives, briefly:**

- **Rust** — better raw throughput, but the bottleneck here is network I/O and Postgres, not CPU. The async ecosystem's borrow-checker friction on long-lived shared state (the live market snapshot) costs iteration speed that a solo project cannot spare. Revisit only if the pricing engine becomes CPU-bound.
- **TypeScript / Node** — single-threaded event loop is a poor host for the pricing math (devigging, correlation matrices, Kelly sizing across a large slate). Would require worker threads, which reintroduces the concurrency problem Go solves natively. TypeScript stays on the frontend where it belongs.
- **Elixir / Phoenix** — genuinely excellent for this (BEAM supervision trees, Channels are a better WebSocket abstraction than anything in Go). Rejected on ecosystem breadth for odds/data tooling and on hiring-signal reach. Worth an ADR noting it was the close runner-up.
- **Java / Kotlin** — strong ecosystem, but JVM startup and image size undercut the container/k8s story that is a stated goal of the project.

### Go for all backend services. No JVM service in this repo.

Explicitly decided, because the temptation will recur: FanDuel runs Java, and a polyglot "one Spring Boot service for the Java bullet" is a tempting move. It is declined. A service that exists to name-drop a language is worse than no service — it doubles the CI toolchain, splits the domain model across two type systems, and a reviewer will ask why it exists and get no good answer.

Where Java would normally appear is Flink jobs. Those are written as **Flink SQL** instead (see §3), which is declarative and language-free, with PyFlink for UDFs if SQL runs out. Full streaming capability, zero JVM code in the repository.

**Frontend:** TypeScript + Next.js 16 (App Router). Not negotiable — the UI is the part a recruiter actually looks at.
(Originally pinned to 15; moved to 16 on 2026-08-19 — see ADR 0007.)

**Python** earns a place in exactly two spots: Locust load tests, and the backtesting / quant notebooks under `analysis/`. Never in the serving path.

---

## 3. Architecture

**Modular monolith with service-ready seams.** One Go module, multiple `cmd/` entrypoints. Every service can run in-process for local development and as its own container in Kubernetes without code changes — the difference is which `cmd/` binary starts and whether the event bus is in-memory or Kafka.

This is the right call because premature microservices on a solo project produce distributed-monolith pain with none of the organizational benefit. The seams (interfaces at the consumer, event-bus boundaries) are what make it scalable; splitting the binaries is a deployment decision made later, cheaply.

### Services

| Binary | Responsibility |
|---|---|
| `ingest` | Provider adapters, rate limiting, payload normalization, change detection, publish deltas to bus |
| `pricer` | Devig, no-vig fair value, EV%, Kelly sizing, arbitrage + middles detection, parlay correlation |
| `stream` | WebSocket gateway. Subscription routing, snapshot-then-delta protocol, per-client backpressure |
| `api` | REST + OpenAPI. Auth, catalog, bet slip, wagers, account, history |
| `settle` | Consumes results feed, grades open wagers, writes ledger entries, emits settlement events |
| `migrate` | Schema migrations (goose). Runs as a k8s Job / init container |

### Data plane

- **Postgres 17 + TimescaleDB** — relational core plus hypertables for the odds time-series. Line history is the interesting dataset (CLV, steam detection, book disagreement) and it is inherently time-series. One engine, two access patterns, no second database to operate.
- **Redis 7** — current-line snapshot cache, WebSocket presence, distributed rate limiting, idempotency keys. Never the source of truth.
- **Apache Kafka (KRaft mode)** — the event backbone. KRaft removes ZooKeeper, so this is one container, not a cluster of them.

  Chosen over NATS JetStream on three independent grounds. **Log compaction is the right primitive for odds:** a compacted topic keyed by `market_id` *is* the current-line snapshot, replayable from scratch, which removes a whole class of cache-coherency bugs between the bus and Redis. **Flink's native source and sink is Kafka** — every alternative means a connector fighting the grain. And it is the most transferable single piece of infrastructure knowledge in this project.

  The cost is real: partitions, consumer groups, offset management, and rebalancing are genuine concepts to learn, where JetStream is nearly free. Paid deliberately.

  **Go client: `franz-go`.** Not `confluent-kafka-go` — that binds librdkafka through cgo, which breaks `CGO_ENABLED=0` and therefore breaks the distroless image and the prime directive. `franz-go` is pure Go and keeps the static binary intact. This constraint is not optional.

  Topic design: `odds.raw.{provider}` (retention-based), `odds.normalized` (compacted, keyed by market), `price.computed` (compacted), `wager.events` (retention-based, the settlement audit trail).

### Event flow

```
provider → ingest → [odds.raw.*] → normalizer → [odds.normalized]  (compacted)
                                                       ├→ pricer → [price.computed] → stream → browser
                                                       ├→ timescale writer (line history)
                                                       └→ Flink SQL  (phase 12)
                                                            └→ [signals.steam | signals.arb | signals.clv]
results  → settle → [wager.events] → ledger → stream → browser
```

### Apache Flink — phase 12, not before

Stateful stream processing over the Kafka topics, written as **Flink SQL** jobs. It maps directly onto problems the system already has, and does so far better than imperative Go:

- **Steam detection** — hopping window over line-movement velocity, keyed by market, across books
- **Arbitrage and middles** — interval join across per-book streams
- **CLV** — event-time join of a wager's placement price against the market's closing price, with watermarks handling out-of-order arrival
- **Line-movement aggregates** — tumbling windows sunk into Timescale

**Sequencing is deliberate and load-bearing.** These features are built in plain Go in phase 9 first. Flink arrives in phase 12 and replaces that implementation. Flink is the steepest learning curve in this project and the likeliest thing to become a half-finished distraction that blocks a working demo. Building Go first means the system is complete and demoable regardless, and the Go version becomes the reference implementation to validate the streaming jobs against — same inputs, same outputs, or the Flink job is wrong.

---

## 4. Domain model

The core language of the system. Get these names right once and they propagate everywhere.

```
Sport → League → Event → Market → Selection → Price
```

- **Event** — a contest. Teams, scheduled start, live clock/state, status lifecycle.
- **Market** — a question about an event (moneyline, spread, total, player prop, futures). Carries a market type and a line where applicable.
- **Selection** — an answer to that market (a side, an over/under, a player outcome).
- **Price** — odds for a selection at a book at an instant. Immutable; a new price is a new row. This is the hypertable.
- **Book** — a sportsbook whose lines are ingested. Includes a synthetic in-house book for development.
- **Wager / Leg** — a placed bet. Straight, parlay, round robin, teaser. Legs hold the price *at placement time*, never a live reference.
- **LedgerEntry** — double-entry. Every stake, payout, void, and adjustment is two rows that sum to zero. Balances are derived, never stored as a mutable field.

### Odds math (`internal/domain/odds` — pure, zero dependencies, exhaustively tested)

- Format conversion: American ↔ decimal ↔ fractional ↔ implied probability
- Overround / vig calculation
- Devigging: multiplicative, additive, power, and Shin methods — implement all four, they disagree meaningfully on longshots
- No-vig fair odds and fair probability
- Expected value %, edge, and Kelly / fractional-Kelly stake sizing
- Closing Line Value
- Parlay pricing with correlation adjustment for same-game legs

This package is the intellectual core of the project. It must be pure functions over value types, with table-driven tests and property-based tests (`pgregory.net/rapid`) asserting invariants — probabilities sum to 1 after devig, round-trip conversions are lossless, Kelly is zero at zero edge.

---

## 5. Real-time pipeline

**Ingestion.** Each provider gets an adapter behind one interface. Adaptive polling: high frequency for live and near-tip events, low for futures, backing off on unchanged payloads. Hash each normalized market to suppress no-op updates — most polls return identical data and must not generate bus traffic. Respect provider quotas via a token-bucket limiter with the budget as a config value, and expose remaining quota as a Prometheus gauge.

**Provider is deliberately undecided.** Build `ProviderAdapter` first, ship a **synthetic provider** that runs a stochastic market-maker generating realistic line movement, steam moves, and book disagreement. This unblocks the entire pipeline, makes tests deterministic with a seeded RNG, and means demos never burn API quota. The real adapter drops in behind the same interface once the provider is chosen. Keys live in `.env` / k8s Secrets and never in git.

**Fanout.** Client subscribes to channels (`event:{id}`, `market:{id}`, `league:{slug}`), receives a snapshot, then deltas only. Every message carries a monotonic sequence number; a gap triggers client resync. Per-client bounded send queue — on overflow, drop the client's buffer and force a resync rather than letting one slow consumer apply backpressure to the entire hub. Heartbeat ping/pong with idle reaping.

---

## 6. Feature surface

Ordered roughly by build sequence, not by importance.

**Core** — live odds board across leagues; event detail with full market tree; line movement charts from history; multi-book comparison; search and filtering; odds format toggle (American/decimal/fractional).

**Betting** — bet slip with straight, parlay, round robin, and teaser support; live price-change detection on the slip with accept/reject; stake and payout calculation; wager history; open position tracking; cash-out pricing on live events.

**Analytics — the differentiator.** Positive-EV finder against a sharp reference book. Arbitrage and middle detection across books. Steam-move alerts. CLV tracking per user. Bankroll and Kelly staking calculator. A public leaderboard on ROI and CLV, not raw profit.

**Account** — email/password auth with argon2id; JWT access tokens plus rotating refresh tokens with reuse detection; optional TOTP 2FA; play-money balance; responsible-gaming-style self-imposed limits (a nod to how the real domain works).

**Platform** — admin console for market suspension and manual settlement; feature flags; audit log on every state-changing action; rate limiting per user and per IP.

---

## 7. Frontend

Next.js 16 App Router, TypeScript strict, Tailwind, shadcn/ui, TanStack Query for REST, a purpose-built WebSocket client (reconnect with jittered backoff, sequence-gap resync, offline banner), Zustand for slip state.

**Containerized in both dev and prod — no `npm run dev` on the host.**

- *Dev:* a `node:24-alpine` service in compose running the Next dev server. `web/src` bind-mounted for hot reload; `node_modules` and `.next` on named volumes so host-arch binaries (sharp, SWC, esbuild) never collide with the container's. `WATCHPACK_POLLING=true` because bind-mount filesystem events are unreliable across the Docker VM boundary on macOS.
- *Prod:* multi-stage build — deps → build → runtime. `output: "standalone"` in `next.config`, runtime stage copies only `.next/standalone`, `.next/static`, and `public` onto `node:24-alpine` running as the non-root `node` user. Target image under 200MB.
- Dependency changes (`npm install`) happen through a `make` target that runs inside the container and writes the updated lockfile back through the mount. Never install into `web/node_modules` from the host.
- The browser talks to the API through the reverse proxy, never to a container hostname. `NEXT_PUBLIC_API_URL` and `NEXT_PUBLIC_WS_URL` point at proxy paths — server-side fetches use in-network service names, client-side ones cannot.

**Design process — use the gstack skills, in this order:**

1. `/design-consultation` — establishes the aesthetic, typography, color system, spacing scale, and motion language before any component is written. Do this *first*; retrofitting a design system is the most expensive mistake available here.
2. `/design-html` — production-quality reference HTML/CSS from the approved system.
3. Build components against that reference.
4. `/design-review` — QA pass for visual inconsistency, spacing drift, hierarchy problems, and AI-slop patterns.

Design constraints specific to this product: numbers change constantly, so price cells need a deliberate flash-on-change treatment that reads as information rather than noise; the board is dense, so the type scale and tabular numerals matter more than usual; dark mode is the primary theme, not an afterthought; every price must be reachable and announced by a screen reader, and live regions must not fire on every tick.

---

## 8. Repository layout

```
sharpline/
├── cmd/
│   ├── api/  ingest/  pricer/  stream/  settle/  migrate/
├── internal/
│   ├── domain/          # types + odds math — zero external deps
│   ├── platform/        # postgres, redis, kafka, otel, config, logging
│   ├── ingest/          # provider adapters, normalizer, scheduler
│   ├── pricing/         # devig, EV, kelly, arbitrage, correlation
│   ├── market/          # market state store, snapshot + delta engine
│   ├── betting/         # slip validation, placement, ledger
│   ├── settlement/      # grading, payout
│   ├── auth/            # tokens, password hashing, 2FA
│   ├── httpapi/         # handlers, middleware, OpenAPI spec
│   └── wsgw/            # websocket hub, subscription routing
├── pkg/client/          # exported Go SDK for the public API
├── web/                 # Next.js app (Dockerfile + Dockerfile.dev live here)
├── migrations/          # goose SQL migrations
├── deploy/
│   ├── docker/          # Dockerfile per service + tooling images
│   │   ├── go.Dockerfile        # one parameterized build for all 6 Go binaries
│   │   ├── tools.Dockerfile     # lint, codegen, goose, kafka CLI, psql
│   │   ├── locust.Dockerfile
│   │   └── playwright.Dockerfile
│   ├── compose/
│   │   ├── compose.yaml         # full stack
│   │   ├── compose.dev.yaml     # hot-reload overrides
│   │   ├── compose.obs.yaml     # prometheus/grafana/jaeger/otel
│   │   └── compose.tools.yaml   # one-shot tooling profiles
│   ├── proxy/           # Caddy config — single entrypoint for web + api + ws
│   ├── helm/            # the only Kubernetes deploy path
│   │   ├── templates/
│   │   └── values.yaml + values-{dev,prod}.yaml
│   └── terraform/
│       ├── modules/     # cluster, kafka-topics, grafana, dns
│       └── envs/{local,prod}/
├── flink/               # Flink SQL job definitions (phase 12)
├── analysis/            # Python: backtesting, quant notebooks
├── test/                # integration (testcontainers)
├── e2e/                 # Playwright
├── load/                # Locust WebSocket fanout tests
├── docs/adr/            # architecture decision records
└── Makefile             # every target is a docker invocation
```

---

## 9. Infrastructure

**Container inventory.** Everything below is a container. Nothing else exists.

| Container | Image basis | Notes |
|---|---|---|
| `api`, `ingest`, `pricer`, `stream`, `settle` | `distroless/static:nonroot` | Static Go binaries, one parameterized Dockerfile |
| `migrate` | `distroless/static:nonroot` | goose; compose dependency + k8s Job |
| `web` | `node:24-alpine` | Next standalone (prod) / dev server (dev) |
| `postgres` | `timescale/timescaledb:latest-pg17` | Named volume, tuned via mounted conf |
| `redis` | `redis:7-alpine` | AOF on, `--requirepass` from secret |
| `kafka` | `apache/kafka:latest` (KRaft) | No ZooKeeper. Volume for log dirs. Topics created by Terraform, not by hand |
| `kafka-ui` | `provectuslabs/kafka-ui` | Dev profile only. Non-negotiable while learning Kafka — inspecting topics, offsets, and consumer lag visually is the difference between a day and a week |
| `proxy` | `caddy:2-alpine` | Single entrypoint; TLS, HTTP/2, WS upgrade |
| `otel-collector`, `prometheus`, `grafana`, `jaeger` | upstream images | Observability stack, own compose profile |
| `tools` | custom | golangci-lint, goose, sqlc/oapi-codegen, kafka CLI, psql, terraform |
| `locust-master`, `locust-worker` | `locustio/locust` | Distributed load generation; workers scale as replicas |
| `playwright` | `mcr.microsoft.com/playwright` | E2E, one-shot |
| `flink-jobmanager`, `flink-taskmanager` | `apache/flink` | Phase 12 only. Own compose profile, off by default |

**Build rules.** Multi-stage everywhere. Static Go binaries (`CGO_ENABLED=0`) into `gcr.io/distroless/static:nonroot`. Non-root UID, read-only root filesystem, `cap_drop: ALL`, seccomp profile, no shell in the final layer. Multi-arch via buildx (arm64 for the dev Mac, amd64 for the server). BuildKit cache mounts on the Go module and build caches — without them, containerized builds are slow enough that people start cheating and running `go build` on the host, which breaks the directive.

**Local.** `docker compose up` brings up the entire stack — data plane, all six Go services, the frontend, the proxy, and the observability stack behind a profile. One command to a working system is a hard requirement; a project that needs a README ritual to start does not survive contact with a reviewer.

Compose specifics that matter: `depends_on` with `condition: service_healthy` and a real healthcheck on every stateful container; `migrate` runs to completion before `api` starts; named volumes for all persistent state so `docker compose down -v` is the reset button; a single user-defined bridge network with service-name DNS; secrets via `.env` (git-ignored, with a committed `.env.example`).

**The proxy is the only published port.** Everything else talks over the internal network. This mirrors the Kubernetes Ingress topology exactly, so the compose stack and the cluster stack are not two different systems that drift apart.

**Kubernetes — Helm only.** One chart, with `values-dev.yaml` and `values-prod.yaml`. Kustomize is deliberately not used; maintaining two deploy systems on a solo project buys nothing, and Helm is the industry-standard packaging story. Every workload in the cluster, stateful ones included — Postgres, Redis, and Kafka run as StatefulSets with PVCs, not as external managed services. The point of this project is that the whole thing computes on hardware the author controls.

Every Deployment carries resource requests and limits, liveness/readiness/startup probes, a PodDisruptionBudget, and a default-deny NetworkPolicy with explicit allows. `migrate` is a Job with a `pre-install`/`pre-upgrade` hook. Config in ConfigMaps, credentials in Secrets (sealed-secrets or SOPS before anything is pushed public). Ingress fronts `web` and `api` on one host, with WebSocket upgrade and sticky-free routing — `stream` must be horizontally scalable, so no session affinity, which means subscription state lives in Redis rather than in a pod.

HPA on CPU for `pricer`, and on a custom metric — active WebSocket connections, exported via the Prometheus adapter — for `stream`. That custom-metric autoscaler is the piece worth demoing.

`kind` locally (its nodes are containers, so the directive holds all the way down), images side-loaded with `kind load` to keep the loop fast, with a documented path to a managed cluster.

**Terraform.** Nothing is provisioned by hand. Terraform owns the kind cluster, Kafka topics and their per-topic retention/compaction settings, Grafana dashboards and alert rules, namespaces, and — when a cloud target exists — the VPC, managed node group, and DNS. Modules under `deploy/terraform/modules`, environments under `envs/{local,prod}`, state local for now with a documented path to remote backend + locking.

The reason Terraform earns its place beyond parity: Kafka topic configuration is exactly the kind of thing that gets created once with a CLI flag, forgotten, and then silently differs between laptop and cluster. Declaring it removes that failure mode entirely. Runs from the `tools` container like everything else.

**Observability.** OpenTelemetry traces spanning ingest → pricer → stream so a single odds update can be followed end to end. Prometheus metrics with a pre-built Grafana dashboard: odds staleness p50/p99, provider quota remaining, WebSocket connections and dropped clients, pricing latency, bus lag. Structured JSON logging via `log/slog` with trace correlation. **Odds staleness is the headline SLO** — define it, alert on it, put it on the dashboard.

**CI/CD (GitHub Actions).** `golangci-lint`, `go test -race`, `govulncheck`, `trivy` image scan, multi-arch build and push to GHCR, migration dry-run, deploy job. Branch protection on `main`.

CI uses **no `setup-go` or `setup-node` actions.** Every job runs `make <target>`, which runs a container. The runner is treated as a bare machine with Docker and nothing else — this is what mechanically enforces the prime directive, because any step that quietly depends on a host toolchain fails in CI even though it passed on the author's Mac. Registry-backed BuildKit cache keeps that affordable.

---

## 10. Testing

Every test tier runs in a container. `make test` starts the Go test image with the Docker socket mounted so `testcontainers-go` can spawn siblings; Playwright and Locust run as one-shot containers against the compose stack.

Unit tests are table-driven; the odds math additionally gets property-based tests. Integration tests use `testcontainers-go` against real Postgres/Redis/Kafka — no mocked databases, and no mocked broker either, because the interesting bugs live in consumer-group rebalancing and offset handling. Provider normalization uses golden files against recorded payloads. E2E via Playwright covers the critical path: sign in → browse board → build parlay → place → observe settlement.

**Load testing via Locust**, targeting WebSocket fanout; the stated goal is 10k concurrent subscribers on one node. Distributed master/worker mode, containerized, with workers scaled as a Kubernetes Deployment — driving the `stream` HPA from a Locust worker pool and watching both scale is the single best demo this project produces. Honest note: Locust generates fewer connections per worker than k6 would, since each user is a Python greenlet rather than a Go goroutine. Compensate with worker count. The tradeoff was accepted for stack parity and because distributed mode demos better.

Coverage target 80% overall, and effectively 100% on `internal/domain/odds`. Wrong odds math is the one bug class that destroys the project's credibility.

---

## 11. Roadmap

| Phase | Deliverable |
|---|---|
| 0 | Container substrate first: Dockerfiles, compose stack, Caddy, container-only Makefile, CI that installs no toolchains, ADR 001 (Go, no JVM) + ADR 002 (container mandate) |
| 1 | Domain types and odds math, fully tested. No I/O. |
| 2 | Postgres schema + migrations; Timescale hypertable for prices |
| 3 | Kafka (KRaft) + topic design via Terraform; ingest with synthetic provider; normalizer; change detection; `franz-go` producer/consumer patterns |
| 4 | Pricing engine: devig, fair value, EV, arbitrage |
| 5 | REST API + auth + OpenAPI |
| 6 | WebSocket gateway: snapshot/delta, resync, backpressure |
| 7 | Frontend — design system first, then live odds board |
| 8 | Bet slip, wagering, double-entry ledger, settlement |
| 9 | Analytics **in Go**: +EV finder, arbitrage scanner, steam detection, CLV tracking. This is the reference implementation phase 12 validates against |
| 10 | Kubernetes: Helm chart, Terraform, StatefulSets, HPA on WS connections, kind demo |
| 11 | Observability polish, Locust to 10k connections, real provider adapter |
| 12 | Stretch, in this order: Flink SQL jobs replacing phase 9 → Kong gateway → Envoy/mesh for mTLS + canary. Each independently droppable; none blocks a complete demo |

Each phase ends in a working, demoable system. Never leave the tree in a state where `docker compose up` fails.

---

## 12. Conventions

- Errors wrap with context (`fmt.Errorf("...: %w", err)`); sentinel errors in the domain package; never `panic` outside `main`.
- `context.Context` is the first parameter of anything doing I/O; every external call has a timeout.
- Interfaces are declared by the consumer, not the producer. Keep them small.
- No global mutable state. Dependencies are constructor-injected.
- Config via environment variables with a typed struct and startup validation — fail fast and loudly on a bad config.
- All money and stake values are integer minor units. Floating point never touches a balance. Odds and probabilities are floats; ledger amounts are not.
- Migrations are forward-only, and every one is reversible in review before it is applied.
- Every developer action has a `make` target, and every `make` target is a `docker` invocation. If a task requires typing `go`, `npm`, `psql`, or `goose` directly, the Makefile is incomplete — fix the Makefile.
- Base images are pinned by digest, not by floating tag. Renovate or Dependabot proposes the bumps.
- Nothing binds to a host port except the proxy. Debugging a service means going through the proxy or `docker compose exec`, not publishing another port.
- Conventional Commits. Feature branches, PRs into `main`, CI green before merge.
- ADRs in `docs/adr/` for every decision that would otherwise be re-argued in six months.

---

## 13. Open decisions

1. **Odds provider** — deferred by the author. Build against `ProviderAdapter` + synthetic provider until chosen. Candidates: The Odds API (cheapest, thinnest), SportsGameOdds, OddsJam (richest, priciest). Decision goes in an ADR with the quota math.
2. **Cluster target** — `kind` locally is settled; whether the live demo runs on a home server, a VPS with k3s, or a managed cluster is open. Manifests must stay portable until then.
3. **Repository name** — `sharpline` is a working title chosen at scaffold time. Rename freely before the first push.
4. **Kong** — deferred to phase 12, not rejected. Caddy holds the entrypoint until then. Kong's value is auth offload, per-consumer rate limiting, and request transformation; revisit once those exist in Go and the migration is a real comparison rather than speculation.
5. **Service mesh (Envoy / Istio ambient / Linkerd)** — phase 12, and only if there is a specific thing to demonstrate. mTLS between services and canary traffic shifting are legitimate; a mesh on six services without either is architecture theater and reads as such.

---

## 14. Stack parity with FanDuel

The project's framing is parity with FanDuel's public stack. Where this charter matches, diverges, and cannot match:

| FanDuel | Here | Note |
|---|---|---|
| Kubernetes | Yes | Practices, not their 100+ cluster scale |
| React | Yes | via Next.js 16 |
| TypeScript | Yes | Frontend, strict mode |
| Helm | Yes | Sole deploy path |
| Terraform | Yes | Cluster, topics, dashboards |
| Apache Kafka | Yes | KRaft, `franz-go` client |
| Apache Flink | Phase 12 | Flink SQL — no JVM code |
| Locust | Yes | Distributed, drives the HPA demo |
| Python | Yes | Locust + `analysis/` only |
| Kong | Phase 12 | Caddy until then |
| Envoy | Phase 12, optional | Only with a concrete demo |
| **Java** | **No** | Deliberate. See §2 |

**What cannot be parity, and should be said plainly in the README rather than glossed:** 100+ clusters, multi-region active-active, NFL-Sunday concurrency, real money movement, state-by-state licensing, KYC, geolocation compliance, PCI scope, a human trading desk, and proprietary pricing models.

Parity here means **architecture and tooling**, not scale or regulatory surface. Claiming more than that is what gets a project like this dismissed by the exact people it is meant to impress.

---

## 15. Notes for the next session

This directory contains only this file. Nothing has been built. Start at Phase 0.

Build the container substrate before the application code. Scaffolding a Go module and running it on the host "just to get started" is the failure mode this charter exists to prevent — the retrofit is far more expensive than doing it in the right order.

**Start Docker Desktop first** — the daemon was not running when this was written, and nothing in this project works without it. Before touching Kubernetes: install `kind` and create a cluster; `kubectl` has no contexts configured.

Run `/design-consultation` before writing a single frontend component.

---

## Design System

`/design-consultation` has been run. **Always read `DESIGN.md` before making any visual
or UI decision.** Fonts, colors, spacing, radius, motion, and the aesthetic direction
are all defined there, along with the reasoning behind each — including the two
deliberate departures from category convention (cyan/amber deltas instead of green/red,
and a decaying delta rail instead of a cell flash) that will look like bugs to anyone
who has not read the file.

Do not deviate without explicit user approval. Any approved deviation is recorded in
the Decisions Log at the bottom of `DESIGN.md`. In QA and review, flag any code that
does not match it.

---

## Skill routing

When the user's request matches an available skill, invoke it via the Skill tool. When in doubt, invoke the skill.

Key routing rules:
- Product ideas/brainstorming → invoke /office-hours
- Strategy/scope → invoke /plan-ceo-review
- Architecture → invoke /plan-eng-review
- Design system/plan review → invoke /design-consultation or /plan-design-review
- Full review pipeline → invoke /autoplan
- Bugs/errors → invoke /investigate
- QA/testing site behavior → invoke /qa or /qa-only
- Code review/diff check → invoke /review
- Visual polish → invoke /design-review
- Ship/deploy/PR → invoke /ship or /land-and-deploy
- Save progress → invoke /context-save
- Resume context → invoke /context-restore
- Author a backlog-ready spec/issue → invoke /spec
