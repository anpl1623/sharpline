# ADR 0001: Go for all backend services, and no JVM service

- **Status:** Accepted
- **Date:** 2026-08-16
- **Charter reference:** CLAUDE.md §2, §14

## Context

Sharpline's backend has one dominant shape and it determines everything else.

**The workload is a fan-in / fan-out problem.** N provider connections stream odds in.
M browser clients stream odds out. Between them sits a pricing stage — devigging,
correlation matrices, Kelly sizing across a full slate — that must not block either side.
Every architectural property that matters follows from that shape:

- **Concurrency primitives must be cheap.** There is one long-lived unit of work per
  poller and one per connected client. At the stated load target of 10k concurrent
  WebSocket subscribers, a runtime with OS-thread-per-connection costs is disqualified
  and a runtime that forces callback or async colouring on every I/O path costs
  iteration speed.
- **Latency must be predictable, not merely low on average.** Odds staleness is this
  product's core quality metric and its headline SLO. A 200ms garbage-collection pause is
  not a percentile blip; it is a visible defect in a live line ticker, and a reviewer
  watching the board will see it.
- **The deployment story is part of the deliverable.** The whole project is framed around
  running on self-hosted compute and demonstrating a real Kubernetes posture: HPA
  scale-up, fast rollouts, small attack surface. Image size and cold-start time are not
  cosmetic here — they determine whether the autoscaling demo reads as impressive or as
  sluggish.

There is a second, separate pressure. The project's framing is stack parity with FanDuel,
and **FanDuel runs Java.** The temptation to add "one Spring Boot service" purely so the
résumé can claim the language is real, and it will recur every time someone reads a job
posting. That temptation needs to be settled in writing, once, rather than re-argued.

## Decision

**Go is the language for every backend service in this repository. There is no JVM code
anywhere in the tree — no Java, no Kotlin, no Scala, not for one service and not for one
job.**

Go was chosen because:

- **The scheduler and channels are the exact shape of this problem.** A goroutine per
  poller and per client connection is the idiomatic solution rather than a workaround.
  At roughly 2–8KB of initial stack, tens of thousands of concurrent subscribers fit
  comfortably on one modest box.
- **The concurrent collector holds sub-millisecond pause times at this heap size**,
  which keeps the staleness SLO achievable rather than aspirational.
- **`CGO_ENABLED=0` produces a single static binary** that drops into
  `gcr.io/distroless/static:nonroot` at roughly 15–25MB with no shell, no package manager,
  and no libc in the final layer. Cold starts are fast enough that HPA scale-up is
  meaningful and the Kubernetes demo is honest.

The "no JVM" half is decided explicitly and for its own reasons. **A service that exists
to name-drop a language is worse than no service.** It doubles the CI toolchain, splits
the domain model across two type systems with two sets of serialization concerns, and —
the decisive point — a reviewer *will* ask why it exists and there is no good answer. The
question "why is there a Spring Boot service here?" answered by "for the résumé" is worse
for the résumé than not having it.

Where a JVM would normally be unavoidable is Apache Flink (phase 12). Those jobs are
written as **Flink SQL**, which is declarative and language-free, with PyFlink for UDFs if
SQL runs out. This yields full streaming capability with zero JVM source in the repository.

**Frontend:** TypeScript + Next.js 15 (App Router). Not negotiable — the UI is the part a
recruiter actually looks at, and TypeScript belongs there.

**Python** earns a place in exactly two spots: Locust load tests and the backtesting /
quant notebooks under `analysis/`. Never in the serving path.

## Consequences

**Made easier.**

- One toolchain, one linter, one test runner, one build image, one CI matrix. On a solo
  project this is the difference between shipping and maintaining infrastructure.
- One domain model in one type system. `internal/domain` is the single source of truth for
  what an Event, Market, Selection, and Price are, and no serialization boundary can let
  two definitions drift apart.
- The distroless / static-binary posture applies uniformly. `cap_drop: ALL`, read-only
  root filesystem, and non-root UID work identically for all six binaries because all six
  are the same kind of artifact built by the same Dockerfile.
- The prime directive (ADR 0002) stays cheap: one builder image serves the entire backend.

**Made harder, and accepted.**

- **No `Optional`, no sum types, no exhaustiveness checking.** Modelling market states and
  wager lifecycles in Go means constants plus discipline plus tests, where an ADT would
  give a compiler guarantee. This is a real, recurring cost.
- **Error handling is verbose.** Wrapping with `fmt.Errorf("...: %w", err)` at every layer
  is correct and tedious in equal measure.
- **The numeric-tower story is worse than the JVM's.** There is no `BigDecimal`. This is
  why the charter mandates integer minor units for all money and stake values (§12) — the
  language does not protect the ledger, so the convention has to.
- **Generics are young.** Some abstractions that would be natural elsewhere are written
  out longhand.
- **The "FanDuel uses Java" question will be asked in an interview.** The answer is this
  ADR, and it is a better answer than a vestigial Spring Boot service would have been —
  but it does have to be given rather than sidestepped.

**Cost knowingly paid.**

Elixir was, genuinely, probably the better technical fit (see below). Choosing Go over it
trades some architectural elegance for ecosystem breadth and hiring-signal reach. That is
a career-strategy tradeoff, not a technical one, and it is being made with open eyes.

## Alternatives considered

### Rust — rejected

Better raw throughput, and a smaller and safer binary. But **the bottleneck here is
network I/O and Postgres, not CPU.** Rust's advantage is in the dimension this system is
not constrained by, so it buys little.

Against that, the async ecosystem's borrow-checker friction on long-lived shared state
is a direct hit on the central data structure of this project: the live market snapshot,
read by every connected client and written by the pricing stage. Every design there
becomes an `Arc<RwLock<_>>` conversation. That costs iteration speed a solo project cannot
spare.

*Revisit only if the pricing engine becomes CPU-bound* — for instance if correlation
matrices across a very large same-game-parlay slate start dominating profiles. That would
be a specific, measured reason, which is the only kind worth reopening this for.

### TypeScript / Node — rejected

A **single-threaded event loop is a poor host for the pricing math.** Devigging four ways,
correlation matrices, and Kelly sizing across a large slate are CPU work, and CPU work on
the event loop stalls every WebSocket client on the same process. The fix is worker
threads — which reintroduces exactly the concurrency problem Go solves natively, plus a
serialization boundary between the workers and the hub.

There is also a strong argument *against* homogeneity here: sharing one language across
frontend and backend sounds like a saving, but the two halves have almost no shared logic
in this system, and the deployment characteristics (image size, cold start, memory
per connection) are materially worse. **TypeScript stays on the frontend, where it
belongs and where it is not negotiable.**

### Elixir / Phoenix — rejected, and the close runner-up

**This was the closest call and it deserves to be recorded as such.** Elixir is genuinely
excellent for this problem, arguably better than Go on the merits:

- **BEAM supervision trees** map onto per-provider pollers far better than anything in Go.
  A crashed poller restarting under a supervisor with a backoff strategy is a language
  feature, not something to hand-roll.
- **Phoenix Channels are a better WebSocket abstraction than anything available in Go.**
  Topic subscription, presence tracking, and PubSub fanout across a cluster come as
  first-class primitives. Sharpline has to build all three by hand: subscription routing
  in `internal/wsgw`, presence in Redis, and fanout in the hub.
- Per-process isolation means one misbehaving client connection cannot corrupt shared
  state, which is precisely the failure mode the per-client bounded send queue exists to
  contain.

It was rejected on two non-technical grounds, stated plainly:

1. **Ecosystem breadth for odds and data tooling.** The libraries for Kafka, OpenTelemetry,
   Postgres/Timescale, and property-based testing all exist in Elixir, but several are
   thinner and less exercised than their Go equivalents. On a solo project, time spent
   patching a client library is time not spent on the product.
2. **Hiring-signal reach.** Go is on far more sportsbook and streaming-infrastructure job
   descriptions than Elixir. This project exists partly as a credential, and that makes
   reach a legitimate input rather than a shameful one.

Recorded here by name so that "we should have used Elixir" is met with "yes, quite
possibly — here is exactly why we didn't" rather than with a shrug.

### Java / Kotlin — rejected

Strong ecosystem, genuinely the industry standard in this domain, and the language FanDuel
actually runs. Rejected because **JVM startup time and image size undercut the container
and Kubernetes story that is a stated goal of this project.** A ~200MB image that takes
seconds to become ready makes HPA scale-up look bad in exactly the demo the project is
built around, and it inverts the security posture: a JRE base layer has a shell, a package
manager, and a CVE surface that `distroless/static` does not.

GraalVM native-image would close much of that gap. It would also add a second, fragile
build path — reflection configuration, reachability metadata, and substantially longer
builds — which collides head-on with the container mandate in ADR 0002.

See also §14 of the charter: parity here means **architecture and tooling, not language
checkboxes.** Kubernetes, React, TypeScript, Helm, Terraform, Kafka, Flink, Locust, and
Python are all matched. Java is the single deliberate divergence, and it is listed as such
in the README's parity table rather than quietly omitted.
