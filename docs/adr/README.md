# Architecture Decision Records

> ADRs in `docs/adr/` for every decision that would otherwise be re-argued in six months.
> — project charter, §12

An ADR is a record of a decision *and its reasoning*, written at the moment the decision
was made. The conclusion alone is nearly worthless six months later: what stops a decision
being relitigated is the argument that produced it, including the alternatives that were
considered and the specific reasons they lost. If an ADR here reads as a bare conclusion,
it has failed.

## Index

| # | Title | Status | Date |
|---|---|---|---|
| [0001](0001-go-no-jvm.md) | Go for all backend services, and no JVM service | Accepted | 2026-08-16 |
| [0002](0002-container-mandate.md) | Every process runs in a container | Accepted | 2026-08-16 |
| [0003](0003-odds-provider.md) | The Odds API as odds provider, synthetic fallback | Accepted | 2026-08-16 |
| [0004](0004-kafka-over-nats.md) | Apache Kafka over NATS JetStream; `franz-go` client | Accepted | 2026-08-16 |
| [0005](0005-helm-not-kustomize.md) | Helm as the sole Kubernetes deploy path | Accepted | 2026-08-16 |
| [0006](0006-fair-value-from-a-reference-book.md) | Fair value from one sharp reference book, devigged with Shin | Accepted | 2026-08-18 |
| [0007](0007-nextjs-16.md) | Move the frontend from Next.js 15 to Next.js 16 | Accepted | 2026-08-19 |

## Template

Every ADR in this directory uses the same five sections, in this order.

```markdown
# ADR NNNN: <short imperative title>

- **Status:** Proposed | Accepted | Superseded by ADR-NNNN | Deprecated
- **Date:** YYYY-MM-DD
- **Charter reference:** CLAUDE.md §N

## Context

The forces at play. What problem exists, what constraints bound it, what would
go wrong if nothing were decided. Written so a reader who has never seen the
codebase can follow it.

## Decision

What was decided, stated in the active voice and unambiguously.

## Consequences

What this makes easy, what it makes hard, and what cost is being accepted
knowingly. An ADR with only positive consequences is not being honest.

## Alternatives considered

Each rejected option with the specific reason it lost — not a generic
disadvantage, the reason it lost *here*.
```

## Conventions

- **Numbering is sequential and permanent.** Numbers are never reused, even if an ADR is
  superseded or deleted.
- **ADRs are immutable once Accepted.** A decision that changes gets a *new* ADR whose
  Status supersedes the old one, and the old one's Status is amended to point forward.
  Editing an accepted ADR in place destroys the record of what was believed when.
- **Filename is `NNNN-kebab-case-title.md`.**
- **Where an ADR and `CLAUDE.md` disagree, `CLAUDE.md` wins** and the ADR is wrong and must
  be fixed — with one explicit exception: an ADR may *resolve* an item that `CLAUDE.md` §13
  lists as an open decision. ADR 0003 does exactly that.
