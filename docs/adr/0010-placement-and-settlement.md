# ADR 0010: Placement and settlement — a derived wager id, self-exclusion in two enforced layers, limits summed from the ledger, a named cash-out haircut, and results from the pipeline's own storage

- **Status:** Accepted
- **Date:** 2026-08-20
- **Charter reference:** CLAUDE.md §3, §4, §6, §12

## Context

Phase 8 is the first phase in which this system writes money. Everything before it reads a
provider, computes a number and shows it, and the quality bar is freshness. From here a
request moves a balance, and the bar changes to *did this happen exactly once, against the
right account, for an amount whose two halves can be shown to sum to zero*.

The material for that arrived early and is not in question. Phase 1 made an unbalanced
`domain.Transaction` unconstructible in Go. Phase 2 made an unbalanced ledger write
unstorable in Postgres, through `ledger_entries_balanced_at_commit` — a `DEFERRABLE
INITIALLY DEFERRED` constraint trigger that fires at COMMIT — and gave `wagers` and `legs`
the same treatment through `wagers_shape_at_commit` and `legs_shape_at_commit`. Migration
00005 shipped `user_limits` with its four kinds and its append-only history. Migration
00006's `ledger_entries_account_idx` was already built `(account_kind, account_user_id,
occurred_at) INCLUDE (amount_minor, kind)`, and its own comment says why: "00005's
period-scoped responsible-gaming limits are all this one index."

What is left is a set of questions that the schema deliberately declined to answer, because
they are policy rather than shape. Each has an obvious answer, and in every case the obvious
answer is wrong in a way that produces no error anywhere.

**Placement is a money write over HTTP, and HTTP retries.** A browser reloads, a proxy
retries a timed-out POST, a user double-taps. CLAUDE.md §3 assigns idempotency keys to
Redis, and in the same sentence says Redis is "Never the source of truth." Migration 00007
went further and refused an `idempotency_keys` table outright — "If you are here to add a
`rate_limits` or `idempotency_keys` table: don't." Those two statements together look like
they leave no place to put the guarantee.

**Phase 5 named the self-exclusion enforcement point and left its backstop open.**
`UserStatus.CanWager` argues at length that the check cannot live in the frontend or in
middleware over a JWT — "the ONE moment a responsible-gaming control matters most is the
minutes right after somebody decides to use it" — and concludes that the read belongs inside
the transaction that writes the wager. It says `internal/betting` **must** call
`Service.AuthorizeWagering`, or build an `auth.StatusReader` over the `pgx.Tx` it already
holds. What it does not say is what happens when a caller does not. That is the open
question this ADR closes.

**`user_limits` has had four `kind` values and no evaluator since 00005.** Three of them —
`grant`, `stake`, `loss` — are spelled identically to three `domain.EntryKind` values, and
that spelling was not a coincidence. Nothing has yet read them.

**Cash-out is the only price this system quotes on its own account.** Every other number on
the board is somebody else's opinion, normalised. A cash-out offer is Sharpline saying "I
will buy this ticket back for X", and X contains a margin that either has a name or is
hidden inside a price.

**`settle` needs a results feed, and the odds provider is deliberately undecided** (ADR 0003;
CLAUDE.md §13.1). The tempting shortcut — seed some finished events so grading can be
demonstrated — is forbidden by the charter's own data-flow rule: every value a user sees must
have travelled `provider → ingest → Kafka → normalizer → pricer → Postgres`.

## Decision

### D1 — the wager id is derived from the idempotency key, and the primary key is the guarantee

A placement carries an `Idempotency-Key` header. The wager id is **derived deterministically
from `(userID, idempotencyKey)`** — and, for a round robin, from
`(userID, idempotencyKey, combinationIndex)`, because one submitted round robin expands into
one `wagers` row per generated combination and each needs its own distinct primary key.

A replayed submit therefore does not "look up whether this was already done". It computes the
same id, attempts the same `INSERT`, and collides with `wagers_pkey`. The placement service
recognises SQLSTATE 23505 through `postgres.IsUniqueViolation`, reads the existing wager back,
and **returns it as a success**. A duplicate submit is not an error condition; it is the
correct outcome of the same request being asked twice.

The derivation is fixed by four requirements, each of which exists to prevent a specific
failure, and the exact spelling lives in the placement package's doc comment:

- **Domain-separated** — the hash input is prefixed with a constant naming this use, so the
  same tuple hashed for any other purpose in this repository can never produce a wager id.
- **Length-prefixed** — the tuple's fields are encoded with their lengths, so `("ab", "c")`
  and `("a", "bc")` cannot hash to the same id. Concatenating user-controlled strings into a
  primary key without this is a cross-user collision waiting for one unlucky key.
- **Fixed forever** — changing the hash changes every future id derived from a key a client
  may still be retrying, which silently turns replay protection off for the duration of the
  rollout. This is not a tunable.
- **Rendered inside the identifier charset** — `domain.validID` and
  `wagers_id_charset` both admit only `[A-Za-z0-9._-]{1,128}`.

**Redis is a fast path and nothing else.** A short-TTL key keyed on the same tuple
short-circuits a replay before it opens a transaction. When Redis is unreachable the
placement proceeds straight to Postgres and **correctness is unchanged**, because the
primary key — not the cache — is what makes the guarantee. This is the exact shape
`internal/platform/redis/doc.go` promised: "Redis makes the common case cheap; the database
makes it correct." It is also why 00007's refusal of an `idempotency_keys` table stands: the
`wagers` table already *is* the idempotency record, and a second table storing the same fact
is a second thing to keep in agreement.

### D2 — self-exclusion is enforced by the service and backstopped by the database

Three layers, and the answer to phase 5's open question is: **the service is the trust
boundary, and the database is the backstop.**

1. **`internal/betting` reads `users.status` inside the placement transaction**, through an
   `auth.StatusReader` built over the `pgx.Tx` it is about to insert on, and refuses
   `self_excluded`, `suspended` and `closed`. This is the authoritative check. It is inside
   the transaction, so there is no window between the read and the insert: an exclusion that
   commits first is seen, and one that commits after was not in force when the wager was
   accepted. Either answer is defensible to the customer, which is the actual requirement.

2. **The HTTP layer maps the sentinel** — `auth.ErrSelfExcluded`, already routed through
   `internal/httpapi/respond.go` — to 403 with a specific error code, so a client can
   distinguish "you may not bet" from "you are not signed in".

3. **Migration 00008 adds a `BEFORE INSERT` trigger on `wagers`** that raises when the
   inserting row's user is `self_excluded`. No route at all — a future service, an admin
   tool, a `psql` session, a migration written in a hurry — can book a bet for an excluded
   user.

An HTTP-middleware-only check is **rejected outright**, not merely ranked lower. Middleware
guards one caller of the service; the service has other callers by construction (settlement,
the admin console, tests, and whatever phase 9 adds), and a control that only one entrance
enforces is a control with a back door.

### D3 — responsible-gaming limits are a sum over `ledger_entries`, evaluated in the transaction

`user_limits.kind` values `grant`, `stake` and `loss` are spelled to match
`domain.EntryKind`. Enforcement takes that literally: **the amount consumed against a limit
is `SUM(amount_minor)` over `ledger_entries` filtered on `kind = <the limit's kind>`, scoped
to the user's account and to the period window, evaluated inside the placement transaction
against the entries this placement is about to write.**

One query shape serves all three money kinds; the only thing that varies is the string and
the window. The query is a range within `ledger_entries_account_idx` and, because that index
`INCLUDE`s both `amount_minor` and `kind`, it is an index-only scan — the schema was built
for this read before there was a reader.

`session` is excluded from this path, and not by oversight: it is a duration, not a money
sum. `user_limits_session_is_duration` makes it structurally impossible to express as an
amount, and its enforcement point is the session, not the bet slip.

The limit in force is the row for `(user_id, kind, period)` with `superseded_at IS NULL`
**and `effective_from <= now`**. The second half is what makes 00005's cooling-off real: a
loosening that has been requested but has not yet bound must not be the row that authorises
the wager it was requested for.

### D4 — cash-out is priced off the fair line, minus a named haircut

```
cashOutValue = round(stake × remainingFairDecimal × (1 − margin))
```

`remainingFairDecimal` is the product of the **fair** — devigged, from the sharp reference
book, per ADR 0006 — decimal prices of the legs still pending, multiplied by the graded
multiplier of the legs already decided. The rounding is `domain.Money.MulFloat` under the
**wager's own** `rounding` mode, which is stored on the ticket precisely so that the mode
used at placement is the mode used at close.

The margin is an explicit named constant: **`DefaultCashOutMarginBps = 500`** — 5%, in
integer basis points.

The reason for pricing off the fair line rather than off the offered line is auditability.
Quoting off the offered price would take the same money, but it would take it *inside the
vig*, where "what did the book charge me to close this early?" has no answer that can be
computed from anything the customer can see. Pricing off the devigged line and then
subtracting a constant with a name makes the book's take a number: it is on the offer, it is
in the code, and it is the same 5% on every ticket until somebody changes a named constant in
a reviewed commit.

**A cash-out is refused, rather than quoted conservatively, when:**

- any remaining leg's reference price is older than the configured maximum reference age —
  quoting off a stale line is how a book gets picked off by a customer watching a faster
  screen;
- the wager is already terminal (`WagerStatus.IsTerminal`), which includes an earlier
  cash-out;
- the computed value is not strictly positive. `wagers_returned_range` permits zero, but a
  0-value cash-out is an offer to give up a live ticket for nothing, and offering it is worse
  than declining to price it.

### D5 — the results feed is the pipeline's own storage, behind a `ResultsSource` interface

`internal/settlement` declares a small `ResultsSource` interface — consumer-side, per
CLAUDE.md §12 — and the shipped implementation **polls the `events` table for events that
have reached `status = 'ended'` carrying a final score.**

This is not fixture data, and the distinction is the whole point. The synthetic provider
(`internal/ingest/provider/synthetic`) advances events `scheduled → live → ended` on its own
clock, and `scoreAt` derives the score from the *same* latent process that prices the
market — from the event's static total and margin means plus a per-side pace draw, which is
why the score is monotone in elapsed fraction and a board never un-scores a goal.

**Corrected after implementation: the result does *not* arrive through the catalogue upsert,
and it never could.** This ADR originally claimed `internal/ingest/writer` persisted the
terminal status and score "through the ordinary catalogue upsert". That was wrong, and the
live database proved it — 115 events, zero scores, zero terminal statuses, with `settle`
polling a correct query against a table that could never answer it. Two structural reasons,
either of which is sufficient:

- `odds.normalized` is **compacted and keyed by market**, and a finished contest has no
  priced market to key on. The books take their prices down when play ends — the synthetic
  adapter models exactly that — so an ended contest yields a payload with no observation
  instant on it, which the normalizer rejects whole. There is nothing to hang a result on.
- `normalizer/payload.go` excludes score and clock from the published record **on purpose**,
  and that exclusion is correct and stays. A record carrying a live score would be
  republished for every market on the event on every score change, which is the exact bus
  flood change detection exists to prevent, at the moment the slate is busiest.

So the result travels the **second arrow CLAUDE.md §3 already draws** — `results → settle`,
beside the odds flow rather than carried on it. `internal/ingest/results` is a poller whose
work queue is a read of `events` for contests that started long enough ago to be plausibly
over; `provider.ResultsProvider` is its adapter seam, separate from `provider.Adapter`
because a deployment may legitimately have an odds source with no results source (which is
what `theoddsapi` ships as, pending ADR 0003's provider decision and the quota math for a
separately-billed `/scores` route); and `queries/results.sql`'s `UpsertEventResult` is the
write. The synthetic adapter answers it with `scoreAt(ev, 1)` — the same function the live
model calls on every fetch, so the final and the closing line are two views of one simulated
contest rather than two independent inventions.

The write is an **UPDATE and never an INSERT**, which is the mechanical form of the no-mock-data
rule: a result for a contest this deployment never ingested cannot create a contest. Its
status guard is the exact complement of `ListFinalisedEventsSince`'s set, so it only ever
moves a row *into* the results feed — a result cannot un-settle a ticket that has already been
graded and paid, and cannot restate a cancellation as an ended game after the voids have gone
through the ledger.

`domain.EventStatus.CanTransitionTo` already reserves `ended → settled` as the one edge out
of `ended`, and settlement is what drives it. A real provider's results adapter drops in
behind the same interface when ADR 0003's provider is wired, and the synthetic source remains
what makes tests deterministic and demos free.

### D6 — `wager.events` is published before COMMIT, and there is no outbox

Settlement publishes through `kafka.AuditProducer.PublishWagerEvent` — synchronous, keyed by
wager, acknowledged by every in-sync replica, retrying unboundedly by design —
**inside the transaction and before it commits**, so that a publish failure aborts the
commit. The odds producer is not substituted for it: that producer drops records after its
delivery timeout, which is correct for a line that will be re-polled in 30 seconds and
catastrophic for a settlement event that nothing will ever re-derive.

This closes the question `internal/platform/kafka/doc.go` explicitly deferred — "the correct
mechanism is a transactional outbox … and that is a phase 8 decision, made with the betting
and settlement packages." **The outbox is not built.** The window it would close is the one
between COMMIT and the ack, and publish-before-commit inverts that window rather than
eliminating it: the failure mode moves from "committed but never published" to "published but
never committed". The second is the one to prefer, because `wager.events` is keyed by wager
and its consumers must already be idempotent to survive at-least-once redelivery, whereas a
settlement that committed and vanished from the audit trail is unrecoverable by anything.
Named here rather than discovered later.

### D7 — every ledger write goes through `postgres.InTx`, and a failed transaction is never retried

Two rules that would otherwise be re-argued at every call site.

**All ledger writes go through `postgres.InTx`.** The zero-sum trigger is `DEFERRABLE
INITIALLY DEFERRED`: an unbalanced write returns success from every single `INSERT` and fails
only at COMMIT. `InTx` is the one helper that propagates a commit error faithfully instead of
losing it in a deferred rollback, so it is the only place that surfaces the defect at all.
Hand-rolled `Begin`/`Commit` around a ledger write is not a style preference; it is a way to
make the ledger's central invariant unobservable.

**A failed transaction is never retried.** `postgres.IsTransientConnectError` gates retries
and covers connection failures only — never a constraint violation, never a serialisation
error, never a commit that failed for a reason the caller did not read. A retried ledger write
double-applies, and a double-applied stake is a balance that is quietly wrong for ever.

## Consequences

**What this buys.**

- **Exactly-once placement with no exactly-once machinery.** No `idempotency_keys` table, no
  distributed lock, no leases to expire, no reconciliation job. The uniqueness guarantee is a
  primary key, which is the one mechanism in this stack that is already correct under
  concurrency, restarts, and partial failure.
- **Redis can be down and the answer stays right.** That is what makes CLAUDE.md §3's "never
  the source of truth" a design property rather than a slogan, and it is checkable: stop
  Redis, replay a submit, get the same wager back.
- **Self-exclusion has no bypass.** Not through a new service, not through the admin console,
  not through `psql`. The database says no even when the code forgets to ask.
- **The book's cash-out take is a number with a name.** `DefaultCashOutMarginBps` can be
  quoted in a README, changed in a reviewed commit, and pointed at when a customer asks what
  closing early cost them.
- **Grading is demonstrable on a laptop with no provider contract and no seeded rows.**
  `docker compose up`, wait for the synthetic slate to run its events out, watch tickets
  settle from results the pipeline produced.
- **`legs_event_status_idx` finally has its reader.** It was built in phase 2 and labelled
  "THE SETTLEMENT READ" with nothing consuming it.

**What it costs, knowingly.**

- **A replayed key with a *different* body returns the original wager, and reports success.**
  A client that reuses one `Idempotency-Key` across two different slips gets the first slip
  back with a 200 and no indication that its second slip was never placed. The industry
  answer is to store a request fingerprint and answer 409 on a mismatch; that is a second
  stored fact and a second thing that can disagree with the wager, and it is not built. The
  mitigation is entirely on the client — a fresh key per slip — which is a real obligation
  pushed outward, and it is written down here because the failure is silent.
- **The wager id is derivable by anyone who knows the user id and the key.** It is not
  secret and it is not a capability. Every read path must authorise on `wagers.user_id` and
  must not treat possession of an id as evidence of ownership. That is ordinary practice, but
  a derived id makes forgetting it more dangerous than a random one would.
- **Two concurrent submits of the same key both reach Postgres.** Redis short-circuits a
  *sequential* replay, not a simultaneous one. One transaction loses on `wagers_pkey` and
  reads back — the right outcome, at the cost of one wasted transaction and one rolled-back
  set of ledger inserts. Fixing it properly means a lock, and a lock in the placement path
  costs more than the duplicate does.
- **A round robin's idempotency is per combination, not per submission.** If a submit fails
  partway, the retry re-derives the same ids and re-inserts only what is missing — but
  "partway" cannot actually happen, because all combinations of one round robin are written
  in one transaction. The per-combination index therefore buys correctness for a case that
  the transaction already prevents, and costs a third field in the hash forever. Accepted:
  the alternative is one id for N rows, which does not exist.
- **The self-exclusion trigger fires on every wager insert.** A round robin of 20
  combinations pays 20 lookups of one indexed row. Measured against the fact that it makes a
  responsible-gaming control unbypassable, this is not close, but it is a real cost in the
  hottest write path in the system and it will show up in placement latency before anything
  else does.
- **A `loss` limit measured from `loss` entries is a *realised* figure and does not net
  winnings.** A customer with a £100 daily loss limit can hold £500 of open positions that
  have not yet graded, and the limit will not stop them, because nothing has been lost yet.
  Netting payouts against losses within the window would be a different quantity —
  arguably a better one — and it is not what the limit is defined as here. Stated plainly
  because a responsible-gaming control that means something other than what a customer
  assumes is worse than one that is simply strict.
- **The limit check is a scan whose cost grows with the period.** A `month` window over a
  heavy user's entries is a wider index range every day of that month, evaluated inside the
  transaction that holds the placement. Index-only keeps it cheap; it does not keep it
  constant.
- **A market the reference book does not quote cannot be cashed out.** ADR 0006 already
  accepts that such a market has no fair value; D4 inherits that gap and converts it from
  "no +EV surface" into "no cash-out offer" on the same tickets. The customer sees a button
  that is absent rather than an offer that is wrong, which is the right failure, but it is a
  coverage hole with a second consequence now.
- **Cash-out uses one margin for every ticket.** A 6-leg parlay with five legs already won is
  a very different risk from a straight bet at even money, and 5% is the same on both. A real
  book prices those differently. A single named constant is defensible only because it is
  *stated*; the moment it varies by ticket shape it becomes a model, and a model needs its own
  ADR and its own validation.
- **`events.status` is not a work queue and has no durable "done" marker.** The ingest
  writer's upsert sets `status = excluded.status` whenever `excluded.observed_at >=
  events.observed_at`, and there is no transition trigger on `events` (00002 defines none).
  So a poll arriving after settlement can write `ended` back over `settled`. The results
  source is therefore a **scan that must be idempotent**, and the authority for "this ticket
  is already graded" is the wager's own terminal status — guarded by
  `wagers_assert_transition` — never the event row. Anything that treats `events.status =
  'ended'` as a claim on work will re-grade.
  *(Narrowed, not closed: `UpsertEventResult` on the results path refuses a source status
  that is already terminal, so the results poller cannot do this. The catalogue upsert on the
  odds path still can — it asserts sixteen columns from whatever the provider currently says
  — so the idempotence requirement stands unchanged.)*
- **`settle` polls Postgres rather than consuming a topic.** It is the one stage of the
  pipeline that is not bus-driven, so it does not inherit the bus's ordering, replay or lag
  metrics, and its latency is a poll interval rather than a fanout. A results *topic* is the
  natural shape and it is not built, because there is no producer for one until a real
  provider exists; inventing one now would mean `ingest` publishing a synthetic results
  stream that no provider adapter will ever match.
- **Publish-before-commit can leave a phantom event on `wager.events`.** If the publish
  succeeds and the transaction then fails, the bus carries a settlement that Postgres does
  not have. Consumers are keyed by wager and must be idempotent, which absorbs a duplicate;
  a phantom is a stronger claim than a duplicate, and the honest statement is that the ledger
  is the source of truth and the topic is an audit trail that can run ahead of it. The outbox
  is where this gets fixed, and D6 says why it is not fixed yet.

## Alternatives considered

### D1: a client-supplied id, or a server-generated random id plus an `idempotency_keys` table — rejected

The textbook answer, and it loses on the same ground twice. A separate keys table puts a
high-churn, TTL-scoped, per-request row into the one `synchronous_commit`-on, fsyncing,
ledger-bearing database, for data whose correct behaviour on loss is "shrug" — which is
exactly the trade migration 00007 wrote three paragraphs refusing. And it introduces a second
record of a fact the `wagers` table already holds, which means a window in which the key row
exists and the wager does not, or the reverse, and a reconciliation nobody writes. Deriving
the id collapses the two records into one.

Letting the *client* choose the wager id directly is worse still: the id would then be
attacker-controlled text in a primary key, a Kafka message key, and a URL path segment, and
the id space would be shared across users, so one customer could squat on another's.

### D1: Redis `SET NX` as the actual idempotency guarantee — rejected

The fast, obvious version, and it is a correctness claim resting on a cache. Redis is
configured with AOF, which — as `internal/platform/redis/doc.go` says in as many words — makes
a restart lose about the last second of writes rather than everything, and "nothing in this
package or above it may depend on it. Design for the empty-Redis case." A duplicate wager
booked because a key evaporated is a wrong balance, which is the one class of bug this phase
exists to make impossible. Redis keeps the job it can do honestly: making the common case
cheap.

### D2: check `users.status` in HTTP middleware over the JWT — rejected, twice over

Rejected once by phase 5 and again here, for two independent reasons. A JWT is a snapshot
minted at login; a customer who self-excludes at 14:00 holds a token minted at 13:55 that
still says active, and every request it authorises until expiry would pass — a window exactly
one access-token lifetime wide, opening at the single moment the control matters most. That
is why status is deliberately not a claim. And middleware guards one entrance: any other
caller of the placement service — settlement, admin tooling, a phase-9 analytics job, a
test — walks straight past it. A control with one guarded door and several unguarded ones is
not a control.

### D2: the database trigger alone, with no service check — rejected

Superficially attractive: it is the unbypassable layer, so why check twice? Because the
error it produces is a raised exception at insert time with no structure, arriving after the
slip has been validated, the balance read and the ledger rows built. The service check
refuses early, returns a typed sentinel that maps to a specific 403, and can say *which*
status refused. The trigger is a backstop, and a backstop that is also the primary control
gives the customer a 500 where they should get a sentence.

### D3: enforce the `stake` limit from `SUM(wagers.stake_minor)` — rejected

Migration 00005's header suggests it, and the two figures agree today, which is precisely
what makes it a trap. They stop agreeing the moment anything other than placement moves stake:
a void refunds it, a cash-out closes the ticket at a different number, an adjustment corrects
it. `wagers.stake_minor` records what was asked for; `ledger_entries` records what moved, and
CLAUDE.md §4 makes the ledger the authority — "balances are derived, never stored as a mutable
field". Summing the ledger also handles a round robin without a special case, because each
generated ticket writes its own stake entry, whereas reconstructing the same total from ticket
rows means knowing which rows share a parent. One query, one authority, three limit kinds.

### D3: a running counter per (user, kind, period) — rejected

Faster than a sum, and it is a stored aggregate of ledger data, which is the identical mistake
as a stored balance one table over. It has to be incremented in the same transaction as the
entries, it drifts the first time anything writes entries without touching it, and its drift is
invisible until a customer exceeds a limit that says they have not. `ledger_entries_account_idx`
already makes the fold an index-only scan; buying speed with a second copy of the truth is the
trade the entire ledger design refuses.

### D4: price the cash-out off the offered line — rejected

Simpler — the offered price is right there, no devig, no reference-book dependency, no
coverage hole. It loses on the only property that makes an in-house price defensible. The
book's take would be the vig plus whatever haircut was applied, entangled, with no way to
separate them from outside and no single number to point at. Under D4 the customer can be
told: this is the fair line, and we charged 5%. Under this alternative the honest answer is
"somewhere between 2% and 9%, depending on the market's overround", which is not an answer.

### D4: quote a conservative value when the reference price is stale — rejected

Kinder to the customer than a refusal, and it is how a book gets picked off. A stale line
means the market has moved and Sharpline has not seen it; the customers who notice first are
precisely the ones who will take the offer, so every stale quote is adversely selected by
construction. "No offer right now" is an honest and temporary answer. A wrong offer is
permanent.

### D5: seed finished events so grading can be demonstrated — rejected

The fastest path to a settlement demo and a direct violation of the charter's data-flow rule.
It would also be self-defeating: a seeded result exercises the grading function and nothing
else, so the parts most likely to be wrong — status transitions surviving repeated ingest
upserts, scores arriving through the results poller, `legs_event_status_idx` actually serving
the settlement read — would all be skipped, and the demo would prove the least interesting third
of the feature.

### D5: consume a results topic instead of polling Postgres — rejected for now

The shape the rest of the pipeline uses, and the right answer once a provider results feed
exists. It is not built because there is nothing to produce it: `ingest` would have to
manufacture a results stream from the synthetic generator's own event states, which is a
producer with exactly one implementation that no real provider adapter would match, and
replacing it later would mean changing both ends. `ResultsSource` is the seam that keeps that
change cheap — the poller goes away, a consumer takes its place, and `internal/settlement`
does not move.

The correction above sharpens rather than reverses this. `internal/ingest/results` is a
*producer* of results now, but it produces them into `events` rather than onto a topic, and
that is still the right call for the same reason: a `results.*` topic would need a schema
nobody can validate against a real provider yet, and it would sit between two components in
one process. `provider.ResultsProvider` is the seam that a real vendor lands on; the topic is
a later question about transport, not about where results come from.

### D6: a transactional outbox — rejected for this phase, and named as the fix

Genuinely correct, and the mechanism `internal/platform/kafka/doc.go` already pointed at: write
the event to a Postgres table inside the same transaction as the ledger movement, and relay it
to Kafka afterwards. It closes the window in the right direction — the ledger and the audit
record commit atomically, and the relay is free to be at-least-once. It is not built here
because it is a table, a relay process or goroutine with its own lifecycle and metrics, a
poison-message policy, and a second at-least-once hop, and every consumer of `wager.events` in
this phase is internal and already idempotent. The cost of deferring it is stated in
Consequences as a phantom event rather than left to be discovered, and this paragraph is the
design to reach for when it stops being acceptable.

### D6: Kafka transactions (exactly-once semantics) — rejected

The reach a reader will expect, and it does not solve the problem that exists. A Kafka
transaction cannot span Postgres, so exactly-once on the bus alone converts a
duplicate-message problem into a lost-message problem and calls it progress. The kafka package
made this argument when it built the audit producer; nothing in phase 8 changes it.
