# Analytics semantics — the phase-9 reference specification

> **CLAUDE.md §11, phase 9:** "Analytics **in Go**: +EV finder, arbitrage scanner, steam
> detection, CLV tracking. This is the reference implementation phase 12 validates against."
>
> **CLAUDE.md §3, on Flink:** the phase-12 SQL jobs "replace that implementation", and the
> check is "same inputs, same outputs, or the Flink job is wrong."

This document is what makes that check possible.

Phase 12 rewrites the four analytics below as Flink SQL. When it does, somebody will need to
know — without reading the Go — exactly what `pricer` and `settle` emit, from which inputs,
under which thresholds, with which window boundaries, in which order, and with which rows
deliberately absent. Every one of those is a number or a rule that two implementations can
disagree about while both compile, both pass their own tests, and neither reports an error.
Vague semantics here are not a documentation gap; they are the deliverable failing.

The standard this file is written to: **a reader with only this file and the Kafka topics
should be able to produce identical answers.** Where that is impossible in principle — and
floating-point associativity makes it impossible in a small, nameable set of places — the
file says so and states the tolerance a comparison should use instead. §10 is that list, and
it is the first thing that will bite phase 12.

**Where this document and the code disagree, the code is right and this file is wrong.**
It describes what is implemented, not what would be nice.

---

## Contents

1. [What is new in phase 9, and what is not](#1-what-is-new-in-phase-9-and-what-is-not)
2. [Conventions that bind every analytic](#2-conventions-that-bind-every-analytic)
3. [Shared inputs](#3-shared-inputs)
4. [+EV signals](#4-ev-signals)
5. [Arbitrage signals](#5-arbitrage-signals)
6. [Steam signals](#6-steam-signals)
7. [Closing line value](#7-closing-line-value)
8. [The leaderboard](#8-the-leaderboard)
9. [Idempotency and replay, in one table](#9-idempotency-and-replay-in-one-table)
10. [Floating point: where two implementations may legitimately differ](#10-floating-point-where-two-implementations-may-legitimately-differ)
11. [The phase-12 conformance checklist](#11-the-phase-12-conformance-checklist)

---

## 1. What is new in phase 9, and what is not

Phase 9 is a **surface over primitives that already exist and are already tested**. It adds
windowing, thresholds, ranking, persistence, bus publication and a query surface. It adds
exactly one new detector.

| Quantity | Where it is computed | Phase |
|---|---|---|
| Devigging (multiplicative / additive / power / Shin) | `internal/domain/odds/devig.go` | 1 |
| Expected value, EV%, edge, edge% | `internal/domain/odds/ev.go` | 1 |
| Kelly, fractional Kelly | `internal/domain/odds/kelly.go` | 1 |
| Margin (S, booking %, overround, vig) | `internal/domain/odds/vig.go` | 1 |
| CLV, CLV aggregation | `internal/domain/odds/clv.go` | 1 |
| Fair value from the sharp reference book | `internal/pricing/fairvalue.go`, ADR 0006 | 4 |
| Per-quote EV / edge / Kelly assessment | `internal/pricing/ev.go` | 4 |
| Cross-book arbitrage detection | `internal/pricing/arbitrage.go` | 4 |
| Middles detection | `internal/pricing/middles.go` | 4 |
| **Steam detection** | **`internal/analytics/steam`** | **9** |
| **Signal thresholding, ranking, persistence, publication** | **`internal/analytics`** | **9** |
| **CLV pass over graded legs** | **`internal/analytics/clv`** | **9** |

**Nothing in phase 9 re-derives a formula from the table above.** A second implementation of
one formula is the one defect this project cannot absorb, because there would then be two
answers and no way to tell which is wrong. Phase 12 inherits that rule: a Flink SQL job that
reimplements Shin devigging is not validating the Go, it is competing with it.

### Where each analytic runs

No seventh binary. CLAUDE.md §3's service table has exactly six, and a service that exists
to host analytics would contradict it. The seams chosen are the ones that already hold the
inputs:

| Analytic | Service | Package | Why there |
|---|---|---|---|
| +EV signals | `pricer` | `internal/analytics` | Already computes `ExpectedValuePercent`, `Kelly` and `FractionalKelly` on every `QuoteAssessment`. The threshold is the only new thing. |
| Arbitrage signals | `pricer` | `internal/analytics` | Already emits `ArbitrageRef` on `ComputedMarket`. |
| Steam signals | `pricer` | `internal/analytics/steam` | The cross-book quotes a hopping window needs are on the record the pricer already decodes. |
| CLV | `settle` | `internal/analytics/clv` | `odds.CLVResult`'s own doc comment: "the settle service writes one per graded leg, the API serves it, and the phase-12 Flink job reproduces it." A closing price is knowable only once an event has started, which is settlement's clock and not the pricer's. |
| Query surface + leaderboard | `api` | `internal/httpapi` | Read path only; computes nothing a detector did not already write. |

**The three `pricer` detectors run as a second consumer of `price.computed`, in the same
process, not as a hook inside the pricing pass:**

```
odds.normalized ──▶ pricing.Service ──▶ price.computed ──▶ analytics.Service
                                                              ├─▶ signals.ev
                                                              ├─▶ signals.arb
                                                              ├─▶ signals.steam
                                                              └─▶ Postgres
```

That is forced rather than chosen. `internal/pricing` requires `PriceFunc` to be a **pure
function of the record**, because the pricer suppresses a republication whose input
fingerprint has not changed and that suppression is sound only if two calls over one record
produce one answer. A detector writing to Postgres and Kafka from inside that call would break
the purity the whole change-detection path rests on. The cost is one extra decode of a record
already in the page cache; the benefit beyond correctness is that the stage depends on a topic
rather than on a function call, so it can be lifted into its own deployment later without
being rewritten.

The stage runs as a **second consumer group**, `pricer-signals`, on `price.computed`. It starts
at the beginning rather than at the end: on a compacted topic that is the current slate, so the
+EV and arbitrage surfaces are complete on a first deploy rather than holding only the markets
that have moved since. That replay produces no steam findings, correctly — a compacted snapshot
holds one record per market, and one observation is not a window.

**Ordering of the two sinks is fixed: persist, then publish.** A finding that is stored and
not published is visible to anyone who looks; a finding that is published and not stored is a
notification pointing at a row that does not exist. A publish failure returns an error, and
what happens to the record then is the **consumer's ErrorPolicy**, not this stage's choice:
under `ErrorPolicyStop` the offset stays uncommitted and the record is redelivered, under
`ErrorPolicySkip` — which is what `pricer` wires, so that one poison market cannot halt every
other market on the partition — the record is counted, logged and advanced over. The stage is
correct under both. Under Stop the redelivery re-runs the detectors and re-upserts identical
rows, so the worst case is a **repeated bus record** a consumer can deduplicate on
`kafka.Message.ID`. Under Skip nothing is lost that the market's next price change does not
re-derive, because `price.computed` is compacted and republished on every change. The worst
case of the OTHER ordering is a **dangling alert**, under either policy. Without the
idempotence of §9, a redelivery would duplicate every finding on the record instead of
correcting it.

Publishing is **synchronous** and no async variant exists: unlike a price, a signal is not
restated on the next record and there is no compacted snapshot in which a gap would show, so a
finding accepted by the client and never delivered to the broker is a loss nobody can detect.

See ADR 0011 for the argument in full.

---

## 2. Conventions that bind every analytic

### 2.1 Units

Getting a unit wrong is the highest-frequency cross-language defect available here, so every
quantity below states its unit and no field is left to convention.

| Quantity | Unit | Range | Notes |
|---|---|---|---|
| Decimal odds | decimal (European) | `(1.0, 100000.0]` | The canonical price format everywhere in this system. American and fractional are display conversions made at the edge and never appear in a signal. |
| Implied probability | probability, dimensionless | `(0, 1)` | `p = 1/d`. Carries the book's margin. |
| Fair probability | probability, dimensionless | `(0, 1)` | Margin removed. Sums to 1 across a market's complete outcome set, to within `odds.CLVDevigTolerance = 1e-9`. |
| Expected value | multiple of stake | unbounded above | `q·d − 1`. `0.05` means five percent. |
| Expected value percent | percentage points | unbounded above | `(q·d − 1)·100`. `5.0` means five percent. Both spellings are stored because the ambiguity between them is a routine factor-of-100 error. |
| Edge | multiple | `(0, 1)` as stored | `q/p − 1`. |
| Edge percent | percentage points | `(0, 100)` as stored | |
| Kelly, fractional Kelly | fraction of bankroll | `(0, 1]` as stored | **Never a money amount.** |
| Return fraction (arbitrage) | multiple of total outlay | `> 0` | `(1 − S)/S`. |
| Stake fraction (arbitrage leg) | fraction of total outlay | `(0, 1)` | `q_i / S`. |
| Steam delta | **implied probability points** | `(−1, 1)`, `≠ 0` | See §6.1 — never decimal odds. |
| Steam velocity | **implied probability points per minute** | finite, `≠ 0` | |
| CLV (probability) | probability points | `(−1, 1)` | `p_close − p_taken`. |
| CLV (percent) | percentage points of return | unbounded above, `> −100` | `(d_taken/d_close − 1)·100`. |
| ROI | ratio | `[−1, ∞)` | `Σ net_return / Σ stake`. |
| Beat rate | fraction | `[0, 1]` | Multiply by 100 for display. |
| Ages, lags, windows, spreads | **seconds**, `double precision` | see below | Never milliseconds, never a Go `time.Duration` on the wire. |
| Money | **`domain.Money`, int64 minor units** | | Appears in exactly one place in phase 9: the leaderboard's `staked_minor` and `net_return_minor`. Nothing else in the analytics surface touches a balance. |

**Ages may be negative.** `quote_age_seconds`, `oldest_leg_age_seconds` and
`arbitrage_signal_legs.age_seconds` are constrained finite, not non-negative. A negative age
means the provider's clock ran ahead of ours, and `domain.Price.Age` reports it rather than
clamping it so "a monitor can detect the skew instead of silently reporting healthy
staleness". A phase-12 job that clamps at zero will disagree with the Go on exactly the rows
that matter.

### 2.2 The line, and its two frames

`domain.Line` distinguishes **absent** (`domain.NoLine`, stored `NULL`) from **present and
equal to 0.0** (a pick'em). `Line.Equal` respects that distinction; SQL `IS DISTINCT FROM`
reproduces it. `=` does not, and using `=` is the single most likely way for a Flink job to
silently mis-join.

Two frames are in use, and which one a column is in is fixed per column, not per row:

- **Selection frame** — the line as *this selection* is quoted, so an away spread reads
  `+3.5` where the home side reads `−3.5`. Used by `ev_signals.line`,
  `arbitrage_signal_legs.line`, `wager_leg_clv.taken_line` and `wager_leg_clv.closing_line`.
- **Home frame** — the line normalised to the home side, computed by `homeFrameLine`, which
  inverts the line for `(spread, away)` and leaves every other combination alone. Used by
  `arbitrage_signals.line`, because an arbitrage is a statement about one line group and the
  group has to be identified by a single value across sides.

Line-rule validity is enforced identically in the domain and in the schema, per market type:

| Market type | Line |
|---|---|
| `moneyline`, `futures` | must be absent (`NULL`) |
| `spread` | must be present |
| `total` | must be present **and strictly positive** |
| `player_prop` | optional |

### 2.3 Event time, and the anchor

Every analytic in phase 9 is a function of **event time** — the provider's own observation
instants, propagated unchanged — and never of a wall clock. This is not a preference:

- `internal/pricing`'s engine is required to be a **pure function of the record**
  (`internal/pricing/doc.go`). An engine that read the clock would let the pricer suppress a
  republication whose result had in fact changed, and the compacted snapshot would hold an
  answer nothing would ever correct.
- Migration 00003 establishes `prices.observed_at` as "the event-time attribute phase 12's
  Flink watermarks will be assigned from". Both hypertables in migration 00009 are
  partitioned on an event-time column for the same reason.

The one clock reading anywhere in phase 9 is `detected_at` / `computed_at`, which answers
"how long after the fact did we notice". **It is stored and it is never part of an identity,
a filter, a window boundary or an ordering.** §9 restates this as a rule.

**The anchor.** Ages inside the pricer are measured from `MarketSnapshot.Anchor()`, which is
the record's `IngestedAt` when it carries one and otherwise the newest observation on the
record. It is a property of the record, so two runs over the same record produce identical
ages. Measuring from the wall clock instead would fold bus and consumer lag into every age,
and the same market would yield a finding on a quiet system and none on a backed-up one.

### 2.4 Window bounds are half-open

Every interval in this document is **`[start, end)`** — inclusive of the lower bound,
exclusive of the upper. This applies to steam windows (`window_start ≤ t < window_end`),
suspension episodes (`suspended_at ≤ t < lifted_at`), and every `graded_at` /
`observed_at` / `placed_at` range parameter on a query (`>= @from_inclusive AND <
@to_exclusive`). A closed upper bound would double-count the boundary instant into two
adjacent windows, which on a hopping window is not a rounding difference — it is a duplicate
finding.

### 2.5 Ordering and tie-breaks are part of the contract

An unstable sort is a different answer in another language. Every ordering in this document
is **total** — it terminates in a column that is unique — and every one is stated. Two rows
that swap places between refreshes make a board look broken, and two implementations that
order equal rows differently fail a diff for a reason that has nothing to do with the
analytic.

All ranking `ORDER BY` clauses on the signal tables are **all-DESC, tie-breakers included**.
That is a keyset-pagination requirement rather than an aesthetic one: a cursor is a row-value
comparison, and PostgreSQL plans a mixed-direction row-value comparison as an OR-expansion
rather than as an index range.

### 2.6 Identifiers

Every identifier column is `TEXT` matching `^[A-Za-z0-9._-]{1,128}$`, enforced by CHECK in
the schema and by `domain.validID` in Go. A phase-12 job emitting an identifier outside that
charset will be rejected at the sink rather than silently stored.

---

## 3. Shared inputs

### 3.1 `price.computed` — the input to all three `pricer` analytics

`internal/pricing.ComputedMarket`, schema version 1, message type `price.computed.v1`, on
the **compacted** topic `price.computed`, keyed by `market_id`, 6 partitions.

The fields phase 9 consumes:

| Field | Meaning |
|---|---|
| `market.id`, `market.type`, `league.id`, `event.id` | Identity, propagated unchanged from `odds.normalized`. |
| `reference.book_id` | The sharp book the fair value came from (ADR 0006). |
| `reference.source` | `catalogue` or `configured` — how that book was chosen. |
| `fair.method` | The devig method that produced the fair probabilities: one of `multiplicative`, `additive`, `power`, `shin`. |
| `fair.selections[].probability`, `.decimal` | The no-vig fair value per selection. |
| `books[]` | Every quoting book, scored, ordered by book identifier. |
| `books[].eligible` | The book's oldest quote is within `MaxReferenceAge`. |
| `books[].complete` | The book quoted every selection. |
| `books[].quotes[]` | One `QuoteAssessment` per selection — see §4.1. |
| `arbitrage[]` | Every under-round line group found on this record, best return first. |
| `observed_at` | The newest provider observation instant on the source record. |
| `ingested_at` | When `ingest` received the payload. |

**The fair value has exactly one author.** ADR 0006: fair value is derived from **one
designated sharp reference book, never from a consensus**, and a market with no eligible
reference book publishes no fair value at all and therefore generates no +EV signal. The
default devig method is **Shin**. Both facts are recorded on every signal row
(`reference_book_id`, `devig_method`) rather than assumed, because a signal that says "this
was +EV" without saying against whom and under which margin model is not reproducible.

### 3.2 `prices` — the input to CLV

The TimescaleDB hypertable from migration 00003, partitioned on `observed_at` in 12-hour
chunks, unique on `(selection_id, book_id, observed_at)`. Immutable by trigger: a price is an
observation, and rewriting one invalidates every number computed from it.

There is **no retention policy**, which is why every read of `prices` in phase 9 carries a
**required** lower time bound. An unbounded lower bound makes the planner consult the index
on every chunk that has ever existed.

### 3.3 `wagers` and `legs` — the input to CLV and the leaderboard

Migration 00006. A leg holds `price_book_id`, `price_decimal`, `price_line` and
`price_observed_at` — **the price at placement time, never a live reference** (CLAUDE.md §4).
That frozen quadruple is the taken side of every CLV comparison.

### 3.4 The four output topics

Phase 9 publishes findings to the bus as well as to Postgres. Producing them now is what
makes the phase-12 swap a genuine like-for-like replacement rather than a rewrite with no
reference output to diff against.

| Topic | Key | `Message.Type` | Retention | Partitions | `retention.ms` | `retention.bytes` |
|---|---|---|---|---|---|---|
| `signals.ev` | `market_id` | `signals.ev.v1` | **delete** | 3 | 7 days | 512 MiB |
| `signals.arb` | `market_id` | `signals.arb.v1` | **delete** | 3 | 30 days | 256 MiB |
| `signals.steam` | `market_id` | `signals.steam.v1` | **delete** | 3 | 30 days | 256 MiB |
| `signals.clv` | `wager_id` | `clv.measured.v1` | **delete** | 3 | 90 days | unlimited |

`analytics.SchemaVersion = 1` versions all three `pricer`-side signal documents together,
independently of `kafka.EnvelopeVersion`, which versions the frame around them. The three are
bumped as a set because one stage publishes them from one input record and a consumer reads
them as a set. `settlement.CLVSchemaVersion = 1` versions the CLV document separately, because
a different service on a different clock publishes it. **Adding an optional field is not a
bump; removing, renaming, or changing the meaning or the UNIT of one is.**

Note that the CLV record's type is `clv.measured.v1`, not `signals.clv.v1` — it names the
event (a measurement was made) rather than the topic, matching `wager.events`' own convention
on the settlement side.

**One Go type per finding serves both sinks.** `analytics.EVSignal`, `analytics.ArbitrageSignal`
and `analytics.SteamSignal` are simultaneously the JSON document published to the topic and
the argument to the `Store` method that writes the table. That is deliberate and it is what
keeps phase 12 honest: a job that reproduced the table rows but not the topic records, or the
other way round, would have reproduced half the contract. One type means the two sinks cannot
carry different numbers.

**All three signal topics are keyed by `market_id`, including steam**, even though a +EV
finding is about a selection and a steam finding is about a `(selection, window)`. A topic key
buys ordering and co-partitioning; the finer identity belongs where rows are stored.

All four additionally carry `compression.type=producer`, `max.message.bytes=1048576` and
`unclean.leader.election.enable=false`. They are declared in
`internal/platform/kafka/topics.go` **and** in `deploy/terraform/modules/kafka-topics`,
because CLAUDE.md §9 is explicit that topic configuration created once by hand and then
forgotten is the failure mode Terraform exists to remove.

**`signals.ev` is an addition to CLAUDE.md §3's event-flow diagram**, which names only
`signals.steam | signals.arb | signals.clv`. It is flagged as an addition in the package doc,
in `variables.tf` and in both Terraform READMEs. The reason it earns a topic: §6's Analytics
bullet leads with the positive-EV finder, and leaving it off the bus would make it the one
analytic phase 12 could not replace like for like.

**None of the four is compacted, and that is load-bearing.** Compaction keeps the latest
record per key, which is meaningful only when the latest record *supersedes* the earlier
ones — true of a market's current line, false of a finding. "The latest steam move for market
X" is not a snapshot of anything; it is one event, and the one before it is a different event
that also happened. Compacting these would do to the signal history exactly what compacting
`wager.events` would do to the settlement audit trail: destroy it invisibly, because the head
of the log still looks right.

**Ordering guarantee.** Total per key, none across keys. A consumer that needs event order
must sort on the payload's own event-time field — `window_end` for steam,
`quote_observed_at` for +EV, `observed_at` for arbitrage, `closed_at` for CLV — never on
arrival order and never on `detected_at`.

### 3.5 Read-surface defaults

Every read of `ev_signals`, `steam_signals` and `prices` takes a **required** lower time bound
(§9). The API supplies a default when the caller does not, and caps it at the corresponding
topic's retention so a query cannot ask for history the bus no longer holds.

| Read | Default lookback | Maximum | Why that default |
|---|---|---|---|
| `/signals/ev` | **6 h** | 7 d (`signals.ev` retention) | Matches the horizon beyond which this API stops calling a quote a current line. A +EV finding on a price that is no longer current is history, not an opportunity. |
| `/signals/arb` | **15 min** | 30 d | Short because an arbitrage is: the phase-4 gate found the leg-age bound binding almost constantly, and a finding whose oldest leg was observed an hour ago is a historical curiosity. |
| `/signals/steam` | **2 h** | 30 d | Longer than the arbitrage window and shorter than the +EV one. A steam move stays interesting while the follower books are still catching up, and for a while after as evidence of where the money went. |
| `/account/clv` | **90 d** | — | A quarter is long enough that a customer's CLV means something and short enough that the default page is about their current form. |
| `/leaderboard` | **90 d** | — | Matches `/account/clv` so a customer comparing their own aggregate against their board row compares the same window. |

The arbitrage read's own staleness filters default to **the detector's bounds** — 120 s leg
age and 30 s spread — not to the tighter signal-layer bounds of §5.4. That is deliberate: a
default view *narrower* than what was detected would hide findings without saying so, and a
default view *wider* cannot exist, because `arbitrage_signals_within_own_bounds` already makes
every stored finding satisfy its own recorded bounds.

---

## 4. +EV signals

**Emitted by:** `pricer`, signals stage.
**Stored in:** `ev_signals` (hypertable, partitioned on `quote_observed_at`, 1-day chunks).
**Published to:** `signals.ev`, keyed by `market_id`.

### 4.1 Input

One `QuoteAssessment` inside one `BookAssessment` inside one `ComputedMarket`. The pricing
pass has already computed every number; the signals stage decides which ones cross a
threshold and are worth recording.

A quote is a candidate only if `QuoteAssessment.Status == QuoteStatusPriced`. The other four
statuses are not near-misses, they are categorically not signals:

| Status | Meaning | Why it is not a +EV signal |
|---|---|---|
| `stale` | The quoting book's oldest quote on this market is older than `MaxReferenceAge`. | An EV against a price that is no longer offered reads as an opportunity and is not one. |
| `line_mismatch` | The book quoted a different line from the reference book **on this selection**. | Fair value came from the reference book's market at the reference book's line. Scoring a `−3.0` quote against a `−3.5` fair value prices a bet nobody can place. This is the same rule `EvaluateCLV` enforces one step later, for the same reason. |
| `no_fair_value` | The market carries no fair value for this selection. | There is nothing to score against. |
| `unpriceable` | The arithmetic refused the quote — a price at the edge of the representable range, an overflow scaling a percentage. | Counted, never silently dropped. |

**The reference book scores itself and is not excluded.** Its EV against the fair value
extracted from its own prices must be `≤ 0` and its Kelly exactly `0`, which is the cheapest
available detector of a devig that has gone wrong. So a reference-book quote is expected to
leave the finder as `not_positive`, every time.

`ev_signals.reference_book_id` may nonetheless legally equal `ev_signals.book_id`, and the
schema permits it deliberately: a book whose own market is under-round can beat its own
devigged fair value. **Under the default Shin devig that cannot happen**, so an equal pair is a
signal about the *devig* rather than about the market — which is exactly why it is storable
rather than rejected. A run of them is a bug report, not a betting opportunity.

### 4.2 Computation

Nothing is recomputed. The stored row is a projection of the assessment:

```
q  = fair_probability          from reference book, devigged with fair.method
d  = offered_decimal           the challenger book's quote
p  = offered_implied  = 1/d

expected_value          = odds.ExpectedValue(q, d)          =  q·d − 1
expected_value_percent  = odds.ExpectedValuePercent(q, d)   = (q·d − 1)·100
edge                    = odds.Edge(q, p)                   =  q/p − 1
edge_percent            = odds.EdgePercent(q, p)            = (q/p − 1)·100
kelly                   = odds.Kelly(q, d)                  = (q·d − 1)/(d − 1), floored at 0
fractional_kelly        = odds.FractionalKelly(q, d, k)     =  kelly · k
```

`k` is `kelly_fraction`, stored on the row so `fractional_kelly` is reproducible from the row
alone rather than from a configuration nobody kept. `odds.QuarterKelly = 0.25` is the value
this system defaults to, and the reason is in `kelly.go`: quarter Kelly "is the common choice
when the probability comes from a devigged market rather than a validated model, which is
exactly this system's situation."

`edge` and `expected_value` are algebraically the same quantity when `p = 1/d`, which is
exactly the case here. Both are stored because they are computed by different routes — a
division against a multiplication — and can differ in the last unit in the last place. A
consumer comparing them is comparing this package's arithmetic against itself. **Do not add
a SQL identity CHECK relating them**; see §10.

### 4.3 The three gates, in order

A scored quote passes through three gates. They are applied in this order, and each has a
named reason so a rejection is counted rather than invisible.

| # | Gate | Parameter | Default | Reasoning |
|---|---|---|---|---|
| 0 | Was it scored at all? | — | — | `Status != priced` → `not_priced`. Never second-guessed: a quote `internal/pricing` did not score has no expected value to threshold, and its fields are zero. Treating a zero as "no edge" rather than "no measurement" is the difference between an honest counter and a flattering one. |
| 0b | Is the EV positive? | — | — | `ExpectedValue <= 0` → `not_positive`. The ordinary outcome for almost every quote on almost every market, including every quote from the reference book itself. |
| 1 | **EV floor** | `MinEVPercent` | **1.0 %** | Not zero, and not out of caution. Fair value comes from **one** book's prices through **one** of four models that routinely disagree by a percentage point on an ordinary two-way market, so a 0.3% "edge" is a statement about the choice of model rather than about the market. It is also the level at which the number survives the round trip to a screen: a board listing a 0.2% edge invites a bet whose expected value is smaller than the price move between seeing it and taking it. → `below_threshold` |
| 2 | **Freshness** | `MaxQuoteAge` | **3 min** | Against `internal/pricing`'s **10 min**. ADR 0003 buys a 90-second live poll cadence, so three minutes is two polls: a quote that has survived two polls unchanged is a live price, one that has not been seen for longer has stopped being *refreshed* rather than stopped moving. Deliberately tighter than the pricer's board bound, because the pricer's job is to keep the multi-book comparison populated while a **signal is an instruction to go and place a bet, and the bet has to still exist.** → `stale` |
| 3 | **Error bar** | `MinEdgeToErrorBar` | **1.0** | See below. → `inside_error_bar` |

**Gate 3 is the one that is not obvious and it is the most valuable of the three.**

`pricing.FairValue.Disagreement` is the largest absolute probability difference between the
devig method that produced the fair value and any other method that could also have priced the
market. `internal/pricing` states the consequence exactly: *"an EV of 1% on a market where the
four methods span 3 percentage points is not a signal, and a consumer that cannot see the
spread cannot tell the difference."*

The two quantities are in different units — `Disagreement` is probability points, expected
value is profit per unit staked — so comparing them directly would be a category error. **The
conversion is the derivative.** `EV = q·d − 1`, so `∂EV/∂q = d`, and a fair-probability
uncertainty of `D` translates into an expected-value uncertainty of `D·d`:

```
ExpectedValue  >=  MinEdgeToErrorBar × Disagreement × OfferedDecimal
```

A ratio of **1** means the edge must be at least as large as the error bar on the number it
was computed from. A longshot at `d = 12` therefore needs twelve times the edge of an
even-money quote to clear the same disagreement — which is correct, and is exactly the
favourite–longshot asymmetry the four devig methods disagree about.

`Disagreement` is **negative when it was not computed** (`pricing.Options.SkipMethodComparison`).
The gate then **does not bind** and the finding is still emitted, but is counted separately as
`unbounded` so the population of unconstrained findings is visible rather than mixed in.
`DisableErrorBarGate` turns it off deliberately; the field is negatively named so the zero
value leaves the gate **on**, because a fair probability with no error bar is an opinion
presented as a measurement.

**Two of the three thresholds are stored on the row** and are enforced by the schema, so a row
cannot claim a bound it does not meet:

| Threshold | Column | Enforced by |
|---|---|---|
| Minimum EV | `threshold_ev_percent` | `ev_signals_meets_own_threshold`: `expected_value_percent >= threshold_ev_percent` |
| Maximum quote age | `max_quote_age_seconds` | `> 0`; the age filter itself is gate 2 |

`MinEdgeToErrorBar` is **not** a column — the error bar it is compared against is a property of
the source record rather than of the finding, and storing the ratio without the disagreement
would record half of a comparison. This is a known asymmetry with the other two, and is the
one +EV parameter phase 12 must be handed rather than read off a row.

The schema additionally refuses a non-positive finding outright: `expected_value > 0`,
`expected_value_percent > 0`, `edge ∈ (0,1)`, `edge_percent ∈ (0,100)`, `kelly ∈ (0,1]`,
`fractional_kelly ∈ (0,1]`, `fractional_kelly <= kelly`. **A zero-EV or negative-EV row is not
a weak signal, it is not a signal**, and `ev_signals` is structurally incapable of holding one.

**A finding that clears every gate but violates a schema CHECK is dropped and counted
(`out_of_range`), never written.** The same rules are applied in Go one layer before the
database, and the reason is not belt-and-braces: the store writes several findings per priced
market in one transaction, a constraint violation aborts it, and PostgreSQL then refuses every
subsequent statement with `25P02`. One malformed edge would take every other finding on the
market down with it, and return an error that looks transient and that redelivery cannot fix,
because the bytes on the topic have not changed. **The Go rules and the SQL constraints must
agree in both directions**, and a divergence either way is a review item.

**`MaxSignalsPerMarket`** caps how many findings one record may yield, applied **after**
ranking so the cap keeps the strongest. It defaults to **0 = no cap**, which is right for this
input: one record carries at most `books × selections` quotes — tens, not thousands — and
silently discarding a genuine finding to bound a write that is already bounded would be the
wrong trade. Capped findings are counted as `capped`.

**Two thresholds are in play at read time and they are different things.**
`threshold_ev_percent` says what the **detector was configured to emit**; the query parameter
`@min_ev_percent` says what **this reader wants to see**. A reader asking for less than the
detector emitted simply gets everything the detector emitted — there is no way to recover
findings that were never written.

**`kelly_fraction` is a configuration fact `price.computed` does not carry.** The priced record
publishes the full and the fractional stake but not the ratio between them, and deriving the
ratio by division would turn a float64 round-trip into a value a `CHECK (0, 1]` can reject on
the last unit in the last place. It is supplied to the finder, defaults to
`pricing.DefaultKellyMultiplier = odds.QuarterKelly = 0.25`, and the composition root supplies
the same value it built the pricing engine with so the two cannot drift.

### 4.4 Windowing

**None.** A +EV signal is a point observation about one quote at one instant. Its event time
is `quote_observed_at`, which is the quote's own provider instant, propagated unchanged. There
is no aggregation, no watermark and no lateness policy: a late-arriving quote produces a
signal stamped with its own instant, which lands in the correct chunk and sorts into the
correct place.

### 4.5 Ordering and ranking

| Read | Order |
|---|---|
| Cross-league / league board | `expected_value_percent DESC, quote_observed_at DESC, selection_id DESC, book_id DESC` |
| One selection's history | by `quote_observed_at` within `[from_inclusive, to_exclusive)` |

The cursor tuple is `(expected_value_percent, quote_observed_at, selection_id, book_id)`, all
DESC — the same four columns as the ranking index, in the same directions. `@observed_after`
and `@min_ev_percent` **must be re-passed unchanged across pages**; bind them into the cursor
and reject a mismatch, because changing a filter mid-pagination silently skips or repeats
rows.

The ordering is total: `(selection_id, book_id)` plus `quote_observed_at` is the natural key.

### 4.6 Exclusions — the exhaustive reason set

Every quote on every record leaves the finder under **exactly one** of these, so the counters
add up to the quotes examined and a discrepancy is a bug rather than a rounding artefact.

| Reason | Meaning |
|---|---|
| `signal` | Cleared every gate and became a finding. |
| `not_priced` | `internal/pricing` did not score it — stale by *its* bound, a different line from the reference book, or the arithmetic refused the price. Not a rejection by the finder. |
| `not_positive` | Scored, and its expected value is at or below zero. |
| `below_threshold` | Positive, but under `MinEVPercent`. |
| `stale` | Older than `MaxQuoteAge` at the record's anchor. |
| `inside_error_bar` | The edge does not exceed the fair value's own cross-method disagreement, scaled to EV units. |
| `unbounded` | A finding accepted while the error-bar gate did not bind. **It is a signal** — this reason *overlaps* `signal` rather than partitioning against it, and exists so a board full of unconstrained findings is visible. |
| `out_of_range` | Arithmetically positive but outside a schema bound. Dropped and counted, never written. |
| `capped` | Cleared every gate but fell outside `MaxSignalsPerMarket` after ranking. |

Beyond the per-quote reasons, one whole-market exclusion: **a market with no eligible
reference book has no fair value at all**, so no book on it produces a signal however good the
price looks. ADR 0006 accepts this coverage loss explicitly and counts it as
`sharpline_pricer_fair_value_total{result}`.

### 4.7 Replay

Natural key `(selection_id, book_id, quote_observed_at)`. Upsert with `ON CONFLICT … DO
UPDATE` — **not `DO NOTHING`**: a replay after a detector fix *is* the correction and must
land. `detected_at` is refreshed on the update path and is not part of the key.

Reprocessing the same `price.computed` record with the same configuration produces a
byte-identical row. Reprocessing it after a threshold change produces either an updated row
(if it still clears) or leaves the old row in place (if it no longer does) — the upsert has no
delete path, so **lowering coverage by raising a threshold does not retract already-published
findings.** That is deliberate: a finding that was true under the thresholds in force is still
a true record of what the detector emitted, and the row carries those thresholds.

---

## 5. Arbitrage signals

**Emitted by:** `pricer`, signals stage, from `ComputedMarket.Arbitrage`.
**Stored in:** `arbitrage_signals` + `arbitrage_signal_legs` (both **plain** tables).
**Published to:** `signals.arb`, keyed by `market_id`.

### 5.1 The discipline this analytic exists under

The phase-4 gate measured **68 live arbitrage findings over 1,065 records, with the leg-age
bound binding constantly**. That is the fact that shapes everything below: most cross-book
"arbitrage" is one book not having repriced yet, and a bettor who takes both legs finds the
second one gone. A firehose of stale-price arbs is worse than no arbitrage feature at all.

Two consequences, both mandatory:

1. **Every reported arbitrage carries its own staleness evidence.** `oldest_leg_age_seconds`
   and `observed_spread_seconds` are on the parent row and `age_seconds` is on every leg, and
   all of them must be rendered. A consumer has to be able to judge a finding for itself
   rather than trust that a threshold was applied.
2. **The staleness bounds are declared, stored thresholds — not magic numbers.**
   `max_leg_age_seconds` and `max_observed_spread_seconds` are columns, and
   `arbitrage_signals_within_own_bounds` makes them a database fact:
   `oldest_leg_age_seconds <= max_leg_age_seconds AND observed_spread_seconds <=
   max_observed_spread_seconds`. A row that violates the discipline cannot be stored.

### 5.2 Input

One `CrossBookMarket`: the market, its **complete** selection set, and every book's quote on
any of those selections. Built from the pricing pass's own `MarketSnapshot` via
`CrossBookMarketFrom`, so the scan and the fair value describe the same record.

The outcome set must be stated, never inferred from the prices. Summing the two implied
probabilities of a three-way soccer moneyline yields a number under 1 essentially always, and
reporting that as an arbitrage would be a firehose of losing bets.

### 5.3 The algorithm, step by step

Let `now = MarketSnapshot.Anchor()` (§2.3). Phase 12 must reproduce these steps in this
order — several of them are order-sensitive.

1. **Market gate.** If `!market.AcceptsWagers()`, emit nothing for this market. A finding on a
   market nobody can bet is noise, and the synthetic feed suspends a market for a few steps
   right after a steam move — exactly when the cross-book numbers look most attractive and
   exactly when they cannot be struck.

2. **Age filter, before grouping.** Drop every price with `p.Age(now) > max_leg_age`. This
   happens *before* the best price is chosen, so a stale outlier can never mask the fresh
   price underneath it. Reordering steps 2 and 3 changes the answer.

3. **Group by home-frame line.** `line = homeFrameLine(market.type, selection.role,
   price.line)` — inverts the line for `(spread, away)`, leaves everything else alone (§2.2).
   The per-book line is used, never the market's consensus line: arbitrage groups by the line
   *this book quoted*, so a `−2.5` quote is never netted against a `−3.5` quote.

4. **Best quote per `(line, selection)`.** The comparison, in order:
   - **longer decimal odds win** — a longer price on the same outcome lowers the implied sum
     and can only improve an arbitrage;
   - tie → the **fresher** `observed_at` — two identical prices are equally profitable and the
     newer one is likelier still to be there;
   - tie → the **lower book identifier**, lexicographically. This last one exists purely for
     determinism, and it is not optional: without it the same input yields different findings
     in different processes.

5. **Per candidate line, in ascending line order** (absent line first, then by value):
   1. If the line does not cover **every** selection, skip it. Its implied probabilities do
      not sum to anything meaningful.
   2. `S = odds.NewMargin(prices).ImpliedSum` — `Σ 1/d_i`, accumulated with
      Kahan–Babuška–Neumaier compensation. **This is the only sanctioned way to sum implied
      probabilities**, so two call sites cannot disagree about S.
   3. Require **under-round**: `1 − S > odds.FairMarketTolerance` where
      `FairMarketTolerance = 1e-12`. Note this is a *tolerance test*, not `S < 1`. Exact
      equality against 1.0 is wrong as a matter of arithmetic: the textbook fair three-way
      book — evens, 2-1, 5-1 — sums to `1 − 2⁻⁵³` in float64 and would otherwise be reported
      as an arbitrage.
   4. `return_fraction = (1 − S)/S`. Require `>= min_return`.
   5. Require `distinct_books >= min_distinct_books`.
   6. `observed_spread = newest_leg.observed_at − oldest_leg.observed_at`. Require
      `<= max_leg_spread`.
   7. `stake_fraction_i = (1/d_i) / S`, where `1/d_i` comes from
      `odds.Decimal.Probability()`. Staking in these proportions is what equalises the return
      across outcomes.
   8. `observed_at = oldest leg's instant`; `oldest_leg_age_seconds = now − oldest`. **An
      opportunity is exactly as fresh as its stalest leg.**

At most one finding per line per market: within a line there is exactly one best price per
outcome, and any other combination sums higher.

**Then the signal layer runs over each detected finding**, applying §5.4's tighter bounds,
computing the fingerprint of §5.5, and shaping the row. It re-detects nothing:
`selection_count` is `len(legs)`, `implied_sum` is the detector's `Margin.ImpliedSum`,
`return_fraction` is the detector's `Return`, and `leg_index` is the leg's position in the
detector's output — which is the market's selection display order, a property of the market
and therefore **stable across a recomputation of the same finding**. That stability is
required, because `leg_index` is half the legs table's primary key.

### 5.4 Thresholds — two layers, and the signal layer is the tighter one

**There are two sets of bounds, and phase 12 needs both.** `internal/pricing`'s scanner
decides what is *detected*; `internal/analytics` decides what is *reported as a signal*. The
detector's job is to find what is there on a board polled every 90 seconds; a signal is an
instruction to go and place a bet, so it gets the tighter of the two. Only the signal layer's
values are written onto the row.

| Bound | Detector (`pricing`) | **Signal (`analytics`)** | Reasoning for the signal value |
|---|---|---|---|
| Max leg age → `max_leg_age_seconds` | 120 s | **60 s** | ADR 0003 buys a 90-second live poll cadence, so 60 s is **less than one poll**: every leg must have been seen since the previous sweep. A leg older than that belongs to a book that missed a refresh, and a book that missed a refresh is the single commonest cause of a phantom arbitrage. (The detector's 120 s comes from `sharpline-alerts.yml`'s `le="120"` SLO-1 bucket.) |
| Max observed spread → `max_observed_spread_seconds` | 30 s | **20 s** | The spread is the **sharper** of the two staleness measurements: two legs a minute apart describe two different markets even when both are individually fresh, and an "arbitrage" assembled across them is an arbitrage against the passage of time rather than against a book. |
| Min return fraction | 0.001 | **0.005** | Half a percent of total outlay. The detector reports down to 0.1% because a detector should find what is there. A signal has to survive contact with reality: **half a percent is roughly one tick on a soft book quoting on a 10-cent American grid**, so below it the finding is inside the granularity the price was quoted at and would be erased by a single tick moving against it. |
| Min distinct books | 1 | **1** | **Deliberately 1, not 2**, and this looks like the loose setting and is the opposite. A book whose own market sums under 1 has **no cross-book staleness explanation available to it at all** — one book, one refresh, one instant — so the finding is either a genuinely mispriced market or a bug in the book. `odds.Margin.IsUnderround` names under-round "a feature, not an error to be swallowed", and dropping the single-book case would swallow exactly the finding with the fewest ways to be wrong. `distinct_books = 1` is legal in the schema and is the **stronger** finding. |

`MaxObservedSpread <= MaxLegAge` is validated at construction on both layers: every leg is
inside the age bound and therefore inside that span of each other, so a wider spread bound can
never bind and a configuration claiming one is a mistake worth failing on.

**A negative age passes both bounds**, deliberately. Provider clock skew is reported rather
than clamped everywhere else in this system, and a leg stamped in the future is a monitoring
problem rather than a stale price.

**Ages are propagated, never re-measured.** Every age and spread on a signal comes verbatim
from the `pricing.ArbitrageRef`, which measured it against `MarketSnapshot.Anchor()`.
Re-measuring at a wall clock would fold bus and consumer lag into the number, so the same
market would yield a finding on a quiet system and none on a backed-up one, and a replay would
disagree with the original run.

### 5.5 The legs fingerprint — a cross-language contract

`arbitrage_signals.legs_fingerprint` is part of the natural key, so phase 12 must reproduce it
byte for byte. `(market_id, observed_at)` is **not** unique: one market quoted by several
books can yield more than one under-round combination at a single instant. Get this function
wrong in one direction and a replay duplicates every finding; wrong in the other and two
genuinely different findings collapse into one, the second silently overwriting the first.

The definition, exactly:

1. **The input is the leg set and nothing else** — for each leg, its `selection_id`,
   `book_id`, decimal odds and line. Not the return, not the implied sum, not the ages: those
   are *consequences* of the legs, and folding a derived value in would make a recomputation
   that fixed a rounding bug produce a **new** finding rather than a correction to the old one.
2. **No clock, no random, no detector version.** The digest must be identical across
   processes, restarts, languages and years.
3. **Legs are sorted by `selection_id` before hashing.** Go map iteration order and a SQL
   engine's collect order are both unspecified; the sort is what makes the two agree.
   `selection_id` is unique within a finding (`UNIQUE (signal_id, selection_id)`), so the sort
   is total and needs no tie-break.
4. **Floats are formatted with `strconv.FormatFloat(v, 'g', -1, 64)`** — the shortest
   representation that round-trips a float64 exactly. A fixed number of decimal places would
   collapse two distinguishable prices.
5. **An absent line contributes the literal `"-"`, never an empty field.** Omitting the field
   for a moneyline would let a moneyline leg and a spread leg collide in principle, and — more
   practically — would make the SQL reproduction depend on how the engine renders a `NULL`
   inside a concatenation, which is exactly the cross-language ambiguity the digest exists to
   avoid. A hyphen is chosen because `FormatFloat` can never produce it alone.
6. **Fields and legs are separated by `0x1f`** (ASCII unit separator), a byte no identifier,
   role or formatted float can contain — so `("ab","c")` and `("a","bc")` cannot digest alike.

The digest is **SHA-256, lowercase hex, all 64 characters**. The column's CHECK admits 16–64
and this uses the full width deliberately: the digest is an *identity*, a collision in it is
silent data loss, and there is nothing to save by truncating a value written once per finding.
FNV-64a would be enough for a change detector — `internal/pricing`'s `ConfigDigest` uses it —
but this is a **key**, and a key wants the margin.

In field order, the digest input for each leg is:

```
selection_id  0x1f  book_id  0x1f  FormatFloat(decimal_odds,'g',-1,64)  0x1f  <line>  0x1f
```

where `<line>` is `FormatFloat(line,'g',-1,64)` when present and `-` when absent.

### 5.6 Windowing

**None.** An arbitrage is a property of one market at one instant, evaluated over the
cross-book quotes present on a single `price.computed` record. Its event time is
`observed_at`, defined as the **oldest** leg's instant — not the newest and not the anchor.

### 5.7 Ordering and ranking

| Read | Order |
|---|---|
| Live board, cross-league | `return_fraction DESC, observed_at DESC, id DESC` |
| Live board, one league | same, within the league |
| One market's history | `observed_at DESC` |
| Legs of a signal | `(signal_id, leg_index)` — a single ordered pass, no map needed |
| Signals stage's in-process output | `return_fraction DESC, observed_at DESC, legs_fingerprint DESC` |

**A named divergence in the last position, and it is harmless.** The database index ends in
the row's surrogate UUID, which the signals stage cannot predict because the database
generates it; the in-process comparator ends in the fingerprint instead. The fingerprint does
the same job — it is a deterministic function of the finding and is unique within
`(market, observed_at)` by the same argument that makes it a key — so both orderings are
**total and stable**, they simply break an exact `(return, instant)` tie differently. Stability
is the property that matters; agreement in that last position is not.

The phase-4 detector's own output order, one layer earlier, is `market_id ASC, line ASC
(absent first), return_fraction DESC`, which is what its golden-file tests compare.

**The arbitrage board has no cursor**, deliberately: the live set is bounded by the staleness
thresholds, so it is small, and the correct interaction is a refresh rather than a page.

### 5.8 Exclusions

At the **detector** layer:

- Markets not accepting wagers (suspended, settled, cancelled).
- Legs older than the detector's `MaxLegAge`, removed before best-price selection.
- Lines that do not cover every selection.
- Line groups whose `S` is not under-round beyond `FairMarketTolerance`.
- Findings below the detector's `MinReturn`, `MinDistinctBooks` or `MaxLegSpread`.

At the **signal** layer, one exhaustive reason per detected finding:

| Reason | Meaning |
|---|---|
| `signal` | Cleared every bound and was reported. |
| `stale_leg` | The stalest leg is older than `MaxLegAge`. **The common outcome, and the whole reason the bound exists.** |
| `wide_spread` | The legs were observed too far apart, so they describe different instants of the market rather than one. |
| `thin_return` | Below `MinReturn` — inside the granularity the prices were quoted at. |
| `too_few_books` | Fewer distinct books than `MinDistinctBooks`. Not reachable at the default of 1. |
| `out_of_range` | Outside a schema bound — an implied sum at or above 1, a non-finite return, a line contradicting the market type, a leg count outside `[2, 64]`, a duplicated selection, a leg index that is not its position. Dropped and counted rather than written, for the transaction-abort reason in §4.3. |

And one capability-level exclusion:

- **Middles are not persisted at all.** `internal/pricing/middles.go` computes them and
  `ComputedMarket.Middles` carries them, but there is no `middle_signals` table and no
  `signals.middles` topic. CLAUDE.md §6 names arbitrage-and-middles as one capability, but
  §11's phase 9 asks for no middle *alert*, and the two are different objects: an arbitrage is
  an event with a guaranteed return, a middle is a *position* whose value depends on a hit
  probability nothing in this system estimates. Persisting a middle would require storing a
  breakeven hit rate next to a number nobody can compare it against. Adding it later touches
  nothing described here.

### 5.9 Replay

Natural key `(market_id, observed_at, legs_fingerprint)`. The parent upsert returns the
**existing** `id` on the update path, which is what makes the leg replacement work:

```
BEGIN
  id := UpsertArbitrageSignal(...)     -- ON CONFLICT DO UPDATE ... RETURNING id
  DeleteArbitrageSignalLegs(id)
  UpsertArbitrageSignalLeg(id, 0..n-1)
COMMIT
```

All three statements in **one transaction**. The legs are `ON DELETE CASCADE` children — a leg
is a *part of* its parent, not evidence outliving it — so the leg set is replaced wholesale on
every recompute rather than merged.

---

## 6. Steam signals

**Emitted by:** `pricer`, signals stage (`internal/analytics/steam`).
**Stored in:** `steam_signals` (hypertable, partitioned on `window_end`, 7-day chunks).
**Published to:** `signals.steam`, keyed by `market_id`.

This is the one genuinely new detector in phase 9. Everything else in this document is a
threshold and a projection over arithmetic phases 1 and 4 already shipped. It is also the only
component here that **holds state**, because a window is by definition a statement about more
than one record.

CLAUDE.md §3 specifies its shape in one sentence: *"Steam detection — hopping window over
line-movement velocity, keyed by market, across books."*

### 6.1 The unit is implied probability, never decimal odds

This is the first decision and it is not negotiable. Decimal odds are nonlinear in probability,
so one decimal threshold means forty times as much on a favourite as on a longshot:

| Move | In probability points |
|---|---|
| `d = 1.50 → 1.60` (0.6667 → 0.6250) | **0.042** |
| `d = 3.00 → 3.10` (0.3333 → 0.3226) | 0.011 |
| `d = 10.0 → 10.1` (0.1000 → 0.0990) | **0.001** |

A board scanned with a decimal threshold would report steam on every short price and none on
any long one. Probability is the quantity a line move is *about* — the market changed its mind
by this much — and it is additive, so a delta and a velocity in probability points mean the
same thing everywhere on the board.

### 6.2 Raw implied probability, not devigged — `devig_method` is `"none"`

The detector works on `1/d` **as the book quoted it, margin included**, and writes `"none"`
into the finding's `devig_method`. That is why `steam_signals.devig_method` admits a fifth
value the other tables' equivalents do not.

**The cost, stated:** a book's margin is inside every observation, so a book that *widened* its
market without changing its opinion shows a move. In practice a book's overround is close to
constant over a three-minute window — it is a business setting, not a price — so the margin
cancels almost exactly in a **delta**, which is the only thing this detector looks at.

**The benefit, which is decisive:** devigging requires a book's **complete outcome set at one
instant** (`NewFairMarketSnapshot` refuses probabilities that do not sum sensibly), and a book
that has refreshed one side of its market and not the other is *exactly* the book whose lag
carries the signal. Devigging would drop it from the window at the moment it became
interesting, which is the opposite of what the detector is for.

### 6.2b The LINE is not part of the series, and this is the single most consequential
### thing on the page for a phase-12 reimplementation

`steamUpdate` builds each observation from a quote's `(selection_id, book_id, implied,
observed_at)` and **drops the line**. The detector therefore tracks *one series per
`(market, selection, book)`* whose value is the implied probability the book was quoting **at
whatever handicap it was quoting at the time**. A book moving `home -1 @ 2.10` to
`home -0.5 @ 1.556` contributes a `+0.167` delta to that series, exactly as a book moving
`home -1 @ 2.10` to `home -1 @ 1.556` would.

**This is deliberate and it follows the charter.** CLAUDE.md §3 asks for "line-movement
velocity", and a handicap that moves from `-1` to `-0.5` at every book inside ninety seconds is
*the* canonical steam move — the thing a bettor means by the phrase. A detector that segmented
its series by line would see that event as two unrelated series, each with one observation, and
would report nothing at all.

**A phase-12 job MUST NOT segment by line.** Nothing else in this document would have told an
implementer that, and the two designs produce almost disjoint answers, so it is stated here as a
requirement rather than left to be inferred.

**The consequence, measured on the live synthetic feed** (284 findings, one hour of a four-league
slate, at the shipped defaults). For each finding, whether the LEAD book quoted more than one
distinct line inside the window:

| market type | findings | lead's line changed | mean magnitude, line changed | mean magnitude, line held |
|---|---|---|---|---|
| spread | 144 | 141 | 0.114 | 0.059 |
| total | 132 | 132 | 0.099 | — |
| moneyline | 8 | 0 (it has no line) | — | 0.077 |

**96% of findings are handicap moves, and they are roughly twice the magnitude of the
fixed-line moves.** That is the population the surface is actually reporting, and anyone reading
`delta_probability` as "the price moved by 11 probability points" is reading it wrong: on a
lined market most of that delta is usually the handicap.

**Two honest consequences, neither of which is fixed in phase 9.**

*`MinMagnitude = 0.050` was calibrated against the wrong population.* §6.7's sweep was run over
**moneyline** markets, where a line change is impossible by construction, so it measured pure
price movement. On a lined market a single half-point step is worth 0.10–0.17 probability points
near the middle of the distribution — two to three times the threshold — so on spreads and totals
the bar is effectively met by *any* coordinated half-point move. Measured over the same live hour
the detector fired on **1.55% of evaluated windows** (80 findings / 5,161 windows on the
`sharpline_analytics_steam_windows_total` counters), against the 0.016%–0.225% §6.7 reports for
the moneyline-only rig — within the range §6.7 itself calls "a firehose rather than an alert".

*The finding is not self-describing.* `steam_signals` has no line column and no flag saying the
window spanned a line change, so a consumer cannot separate the two populations without going
back to `prices`. The table above had to be produced with a correlated subquery over the
hypertable.

**The recommended follow-up, recorded rather than performed:** a `lead_line_changed BOOLEAN` (or
the lead's opening and closing line) on `steam_signals`, and a **per-market-type**
`MinMagnitude`, calibrated on lined markets with the same sweep §6.7 used on moneylines. Both are
additive — a new migration and a config field — and neither changes anything else in this
document. They are named here so the gap is a known quantity rather than something the first
person to compare a Flink job against this one discovers as a disagreement.

### 6.3 The window — hopping, half-open, aligned to the Unix epoch

A hopping window (Flink: `HOP`; "sliding" in some dialects) of length `Window`, advancing by
`Hop`. At the defaults — **180 s every 60 s** — each observation falls in **three** consecutive
windows, so a move is seen against three framings and the cooldown of §6.8 is what stops it
firing three times.

Two properties fix the grid, and both exist so that two independent implementations agree on
which observations belong to which window:

- **Half-open, `[start, end)`.** An observation at exactly `end` belongs to the **next**
  window and never to this one. This is what makes consecutive windows a partition rather than
  an overlapping cover with a double-counted boundary, and it is what Flink's `HOP` does.
- **Aligned to the Unix epoch, in UTC.** Window *k* is `[k·Hop, k·Hop + Window)` counted in
  nanoseconds from `1970-01-01T00:00:00Z`. **Not** aligned to the first observation this
  process happened to see, which would make the answer depend on when the consumer started;
  **not** aligned to local midnight, which would make it depend on a time zone. Flink aligns
  its windows to the epoch too, so the two agree by construction rather than by coincidence.

A hop equal to the window (tumbling) is the failure mode hopping exists to remove: a jump
straddling a boundary shows as two half-moves, neither of which clears the magnitude
threshold. `Hop <= Window` is validated at construction — a longer hop would leave gaps in the
cover, so an observation could fall in no window at all.

### 6.4 Closing a window — a watermark, not a timer

Each **market** carries a watermark: the greatest observation instant seen for that market so
far, minus `AllowedLateness`, and **monotone non-decreasing** — it never moves backwards,
whatever order records arrive in. A window closes, and is evaluated **exactly once**, when the
watermark reaches or passes its end.

**Nothing here reads a wall clock.** That is what makes a replay produce the same findings as
the original run: the same log in the same order yields the same watermark sequence, the same
window closes and the same rows. A timer-based close would make the output depend on how fast
the consumer was running, which would make the phase-12 comparison meaningless.

**Allowed lateness is sized against the feed's own lag, and getting it wrong is the one way to
make this detector silently blind.** Books quote off lagged views — the synthetic generator's
deepest book is 9 base steps, **90 seconds**, behind — and each book stamps its quote with the
instant of the view it is quoting. A lagged book's observation covering event-time *T* does not
**arrive** until up to its lag plus one poll interval after the sharp book's. Close the window
before then and the lagged books are simply absent from it, the correlation is computed over
one book, and no finding is possible.

**Lateness policy.** An observation older than the watermark is **late**: it is dropped,
counted, and **cannot resurrect a window that has already been evaluated**. Admitting it would
mean a window's answer depended on when the reader asked, which is the property the watermark
exists to remove.

Note the contrast with §9's general replay rule. A *reprocessing of the whole log* re-derives
the same watermark sequence and upserts identical rows; a *late arrival within one run* is
dropped. The two are consistent because the watermark is a function of the record order, not
of the clock.

**Watermark jumps are bounded.** A watermark that leaps hours forward — the first record after
an outage, or a replay reaching a gap — would otherwise evaluate every window in between, over
a buffer that has been pruned and holds nothing, producing nothing but a stall inside a Kafka
handler while the group's rebalance is blocked. Past `MaxWindowsPerAdvance` the detector jumps
the evaluation cursor to the newest window and **counts the skip**, because a gap that is
silently absorbed is a gap nobody investigates.

Four mechanical rules that a reimplementation must get right, each of which changes the answer:

1. **The unit of input is a whole market at one instant, not a single quote.** That is what
   the pipeline carries, and splitting it into *n* calls would let the watermark advance
   halfway through one record's worth of evidence.
2. **Quotes are recorded FIRST and the watermark is advanced SECOND.** The reverse order would
   let a record's own newest quote close a window that the same record's other quotes belong
   in — so a market whose books all report at once would evaluate a window still being filled,
   and would do it differently depending on the order the quotes happened to sit in the slice.
3. **The record's own anchor participates in the watermark**, alongside the quotes' instants.
   Without it, a record carrying no usable quote would not advance event time and a market
   that stopped moving would hold its last window open for ever.
4. **Windows are evaluated oldest-first.** Required rather than convenient: the cooldown is
   stateful, so evaluating a later window before an earlier one would suppress the wrong one of
   the pair and the answer would depend on traversal order.

**A repeat of an instant already held replaces it rather than appending.** The pipeline can
legitimately redeliver a record — a rebalance, a retry — and two points at one instant would
make the "first" and "last" of a window depend on which copy landed second. This is the same
statement migration 00009 makes at the storage layer with an upsert on the natural key.

**Observations dropped before they reach a series** — a zero instant, a zero selection or book
identifier, or an implied probability that is non-finite or outside `(0, 1)` — are skipped
silently rather than counted as late. They are malformed input, not evidence about timing.

### 6.5 What is measured, per window, per selection

Steam is **directional**, so the unit of detection is a **selection**, not a market. "The home
side steamed" and "the away side steamed" are the same market and opposite findings. Migration
00009 keys `steam_signals` on `(market_id, selection_id, window_start, window_end)` for exactly
this reason; keying by market alone would drop half the findings.

For one selection in one window, for each book with **at least two** observations inside it:

```
Δ_b        = p_last − p_first,  ordered by observation instant, in probability points
moved_at_b = the END instant of the largest single step in the direction of Δ_b
             ties broken by the EARLIEST such instant
```

**Δ is first-to-last rather than a sum of absolute steps** because the finding is about where
the market *ended up*: a book that moved out and back has not steamed.

**`moved_at` is an argmax over steps rather than the window's last instant** because it is the
propagation *timing* that distinguishes a lead from a follower, and the largest step is where
the move actually happened.

Both are single window functions in SQL — `first_value` / `last_value`, and `lag` inside an
argmax — which is a requirement of the phase-12 rewrite rather than a happy accident.

**The lead book** is the qualifying book (`|Δ_b| >= MinMagnitude`) whose `moved_at` is
**earliest**. Ties are broken by the **greater `|Δ|`**, then by the **lexicographically smaller
book identifier**. That makes the choice total: two books cannot tie on all three, because a
book appears once per `(selection, window)`.

**The followers** are every other qualifying book whose Δ has the **same sign** as the lead's
and whose lag — `moved_at_b − moved_at_lead` — lies in `[0, MaxFollowerLag]`. **A lag of
exactly zero is legal and common**: books that share a view of one latent process reprice on
the same event-time grid, and simultaneity is corroboration rather than a disqualification.

**The finding's delta is the LEAD BOOK'S, not an average across books.** A steam move in
progress has followers that have only partly repriced, so averaging would understate the move
by however much of it has not propagated yet — and would understate it **most** at the moment
the finding is most valuable. The lead is by definition the book that has already made the
move.

```
velocity = Δ_lead / (Window in minutes)
```

It is a **window-average rate**, not an instantaneous one, and it is the standard `HOP`
formulation: last minus first over the window length.

### 6.6 Cross-book correlation, and what it does *not* do

```
correlation = mean over every book with >= 2 observations in the window of
                +1  if sign(Δ_b) = sign(Δ_lead) and |Δ_b| >= NoiseFloor
                −1  if sign(Δ_b) ≠ sign(Δ_lead) and |Δ_b| >= NoiseFloor
                 0  otherwise
```

The mean signed agreement, in `[−1, 1]`, and a single `GROUP BY` in SQL. The lead contributes
`+1` to its own statistic by construction, so a lone book scores `1/n` where *n* is the number
of books with data — which is why a modest `MinCorrelation` is still a real constraint when
several books are quoting.

**Be honest about what this discriminates.** On a feed where every book views one latent
process — which is what the synthetic generator builds and what a real market approximately is
— **ordinary drift is also correlated across books**, because the books are looking at the same
thing. **Correlation therefore does NOT separate steam from drift.** What it separates is a
genuine market move from **one book's tick rounding or its persistent per-event bias moving
alone**, which is the commonest way to manufacture a phantom signal from a single soft book.

**The magnitude threshold is the primary discriminator.** A steam move in the generator is a
discrete jump of 0.4σ to 1.5σ of the latent process landing inside one 10-second step, where
ordinary drift is a mean-reverting mixture whose movement over a three-minute window is a
fraction of that.

**The `NoiseFloor` is not decoration.** Without it, a book that did not move at all still has a
*sign*, because the last unit in the last place of a float64 is never exactly zero after a tick
conversion — so a quiet book would contribute `±1` at random and the correlation would be a
coin flip weighted by rounding.

### 6.7 The parameters, with their derivation

Every one is written onto every finding, so a deployment that re-tunes leaves a stored
population that can still be separated into two regimes. A zero field takes the documented
default, so the zero configuration is a working detector.

| Parameter | Default | Unit | Derivation |
|---|---|---|---|
| `Window` | **3 min** | duration | Sits at the **tighter of two bounds, not a compromise between them.** *Lower:* the window must contain at least two observations per book or there is no delta to measure, and ADR 0003 buys a 90-second live cadence, so anything under 180 s cannot guarantee two. *Upper:* a steam move is instantaneous (the generator lands the whole amplitude in one 10-second step) where ordinary drift accumulates with the square root of elapsed time — so every second past the lower bound costs discrimination. A faster provider cadence would justify a shorter window and a sharper detector. |
| `Hop` | **1 min** | duration | `Window/Hop = 3`, so each observation is seen in three framings and a move near a boundary is not cut in half by it. A finer hop buys diminishing coverage for linearly more windows to evaluate and suppress. |
| `AllowedLateness` | **3 min** | duration | 90 s of deepest book lag **plus** 90 s of poll cadence (ADR 0003). Too small and the lagged books are absent from every window, the correlation is computed over one book, and nothing ever fires — **a failure that looks exactly like a quiet market.** Too large and findings are correct but late, which for an alerting surface is its own kind of useless. |
| `MaxFollowerLag` | **2 min** | duration | Must exceed the deepest book's view lag (90 s in the generator) or the softest book — whose participation is the most informative — could never qualify. Must stay well inside the window, because both instants have to lie inside `[start, end)` to be compared at all. |
| `Cooldown` | **5 min** of **event** time | duration | One jump appears in `Window/Hop = 3` consecutive windows, and propagation through lagged books stretches that to four or five. Five minutes covers the run with margin. Deliberately shorter than the generator's three-hour `steamFullBlocks`, because a market that genuinely steams twice in ten minutes has done something worth two alerts. |
| `MinMagnitude` | **0.050** | probability points | See the derivation and the measurement below. |
| `MinVelocity` | **0.050 / `Window` in minutes** = **0.01667** at the default window | probability points / min | Resolved as `MinMagnitude / Window.Minutes()` rather than taken from a constant, so raising the magnitude bar alone cannot leave the velocity bar at the old default's value. At the shipped window it is `MinMagnitude / 3` exactly, so it binds at the same place and is **redundant**. That is deliberate, not an oversight: the two stop being redundant the instant the window length changes, which is precisely when a stored population of findings would otherwise become uninterpretable. Both are stored on every finding. |
| `MinCorrelation` | **0.5** | dimensionless, `[−1, 1]` | With five books quoting, 0.5 means at least three net books agreeing with the lead: enough to refuse a move one book made alone, not so much that a single soft book disagreeing kills a real finding. |
| `MinFollowers` | **1** | count | A move nobody follows is a move by one book. **Not raised above one** because the deepest-lagged books may not have reported inside the window at all, and requiring three corroborators would make the detector's sensitivity a function of which books happened to refresh — a property that would not survive a change of provider. Migration 00009 CHECKs `follower_count >= 1`, so a lower value would produce rows the database refuses. |
| `NoiseFloor` | **0.005** | probability points | Comfortably above one tick on the softest book in the generator's set (a 10-cent American grid) and comfortably below the magnitude threshold. Correlation statistic only. |
| `MaxSamplesPerSeries` | **64** | count | Bounds retained observations per `(market, selection, book)`. The pruner is what actually bounds memory — it drops everything older than the oldest window still capable of being evaluated — and this is the backstop for a feed producing observations far faster than the watermark advances. At the default cadence one book contributes two or three observations per window, so 64 is more than twenty windows of history. |
| `MaxWindowsPerAdvance` | **32** | count | See §6.4. |

**Where `MinMagnitude = 0.050` comes from.** It is **calibrated against the generator, not
derived and hoped for**; the derivation below and the measurement after it agree, which is why
both are recorded. `TestFiresOnGeneratedSteamAndNotOnDrift` is the measurement rather than a
check on one.

*The derivation.* The synthetic generator is dimensionless by
construction: every latent process is in units of its own stationary standard deviation, and a
league's line dispersion converts it to points, goals or yards. Running that through the normal
model the generator prices with — `∂P/∂latent ≈ φ(0)·lineSD/resultSD` — gives almost the same
factor in every league:

| League | `φ(0) × lineSD / resultSD` | probability points per latent σ |
|---|---|---|
| basketball | `0.3989 × 1.60 / 11.50` | 0.055 |
| gridiron | `0.3989 × 1.10 / 13.50` | 0.033 |
| ice | `0.3989 × 0.30 / 2.25` | 0.053 |
| football | `0.3989 × 0.22 / 1.40` | 0.063 |

A steam jump is `(steamMinAbsZ + |z|)·steamAmplitude` — 0.4σ to about 1.5σ, mean 0.8σ — so a
large steam move is **five to eight probability points** and a small one is two. Ordinary drift
over three minutes is a fraction of that: the mixture's fast component has a twelve-minute
half-life on a one-minute grid and the slow one an hour's, so a three-minute change is well
under half a latent σ.

*The measurement, and why the answer is five points rather than three.* Sweeping this threshold
over six hours of model time across a whole league's moneyline markets collapses the candidate
count as a Gaussian tail and then flattens:

| `MinMagnitude` | 0.030 | 0.040 | **0.050** | 0.060 | 0.070 | 0.080 |
|---|---|---|---|---|---|---|
| candidate windows | 647 | 175 | **43** | 15 | 7 | 4 |

The steep part is ordinary drift, whose window change is Gaussian; the flat part is the steam
amplitude distribution, which is not. **The knee is the boundary between the two populations,
and 0.050 sits past it.** At 0.030 the detector fires on about 1.15% of windows — roughly seven
times as often as the generator plants steam moves at all, which is a firehose rather than an
alert; at 0.050 the detector fires on 0.016%–0.225% of windows depending on the league.

**That sweep was run on MONEYLINE markets only, and the shipped detector runs on all of them.**
The measured live rate across a four-league slate is 1.55% of evaluated windows, an order of
magnitude higher, because 96% of findings on lined markets are handicap moves rather than
fixed-line price moves. §6.2b has the measurement and the recommended follow-up. Do not read the
figures in this section as the rate of the shipped configuration; they are the rate of the
calibration rig.

**Two honest consequences.** *Recall is deliberately low*: only moves above about one latent σ
clear five points, roughly a fifth of the moves the generator makes. That is the right trade for
an alert surface — a missed small move costs nothing and a false alert costs the next one being
read — but it is a trade and it is named rather than discovered. *Sensitivity varies by league*,
because the conversion factor above does: five points is 0.8σ on the football league and 1.5σ on
the gridiron one, so the same threshold is stricter on the sport whose line moves least in
probability terms. A per-league threshold is the obvious refinement if the gridiron surface ever
looks too quiet.

It is the parameter most worth re-deriving against a real provider, which is
why it is stored on every finding as `threshold_magnitude` rather than being implicit in the
code that produced it.

**Configuration rules validated at construction**, each of which would otherwise produce a
detector that runs, looks healthy and reports nothing — the failure mode this package has to be
defended against, because "no steam today" is also the correct output most of the time and the
two are indistinguishable from outside:

```
Hop         <= Window          otherwise consecutive windows leave gaps
MaxFollowerLag <= Window       otherwise it can never bind
NoiseFloor  <= MinMagnitude    otherwise the lead book itself counts as not having moved
MaxSamplesPerSeries >= 2       otherwise no delta could ever be measured
MinFollowers >= 1              the database CHECKs it anyway
MinMagnitude ∈ (0, 1)          it is probability points, not decimal odds and not a percentage
MinCorrelation ∈ [−1, 1]
every threshold finite         a NaN floor rejects everything and reports nothing
```

### 6.8 The cooldown

With a 180-second window hopping every 60, **one jump appears in three consecutive windows**,
and propagation makes it worse: a follower repricing 90 seconds later extends the run. Without
suppression one steam move would emit four or five findings and the alerting surface would be
useless.

`Cooldown` suppresses a finding for a `(market_id, selection_id, direction)` triple whose
`window_end` is within the cooldown of the last finding **emitted** for that triple. It is
keyed on **direction** as well as selection because a move out and a move back are two events
and the second is not a duplicate of the first.

**It is measured on `window_end`, which is event time, not on a clock.** A wall-clock cooldown
would suppress different findings on a replay than on the original run, and the phase-12
comparison would fail for a reason that had nothing to do with the SQL.

`Cooldown = 0` is **not expressible** — a zero field takes the default — deliberately: with
`Window/Hop = 3` a detector without a cooldown emits every move three times, and that should be
an explicit decision rather than an omitted field.

### 6.9 Direction, and the identities the schema enforces

| `direction` | Meaning |
|---|---|
| `shorten` | Implied probability **rose** — the price got shorter; money came in on this side |
| `drift` | Implied probability **fell** — the price lengthened |

The schema pins the agreements so the fields can never disagree:

```sql
(direction = 'shorten')                 = (delta_probability > 0.0)
(velocity_probability_per_minute > 0.0) = (delta_probability > 0.0)
magnitude_probability_points            = abs(delta_probability)
follower_count                          = jsonb_array_length(followers)
participating_books                     = follower_count + 1        -- >= 2
window_start <= lead_moved_at < window_end
```

and `steam_signals_meets_own_thresholds` makes "this row meets the thresholds it claims" a
database fact:

```sql
abs(velocity_probability_per_minute) >= threshold_velocity
AND magnitude_probability_points     >= threshold_magnitude
AND cross_book_correlation           >= threshold_correlation
AND follower_count                   >= min_followers
```

`delta_probability = 0` is refused outright: a move of exactly zero is not a move.

### 6.10 `followers` — the JSON contract

`followers` is `JSONB`, constrained to be an array with `follower_count =
jsonb_array_length(followers)`. It is a JSON array rather than a child table for two reasons:
TimescaleDB forbids a foreign key targeting a hypertable, and a Flink SQL job emits one row per
detection carrying an `ARRAY<ROW<...>>` which serialises to exactly this shape and would
otherwise have to be shredded into a second sink.

Each element, **ordered by `lag_seconds` ascending and then by `book_id` ascending** — an
ordering a database cannot enforce, so the writer owns it and phase 12 must reproduce it:

```json
{
  "book_id":           "syn-tallowcreek",
  "moved_at":          "2026-08-20T14:03:41Z",
  "lag_seconds":       12.5,
  "delta_probability": 0.021
}
```

- `moved_at` is **RFC 3339 with a UTC offset**.
- `lag_seconds` is `moved_at − lead_moved_at`, in seconds, and is **`>= 0` by construction**: a
  book that moved before the lead would *be* the lead.
- `delta_probability` is that book's own signed window change, **same sign as the lead's by
  construction**.
- `book_id` is **not** FK-enforced inside JSONB. Emitting a book the catalogue does not know is
  a writer defect the database will not catch — stated rather than hidden.

### 6.11 Ordering and ranking

**Steam is ranked by RECENCY, not by magnitude.** Magnitude is a *filter* (`@min_magnitude`),
not the sort key. A steam move is an alert about something happening now; a two-hour-old
4-point move is history, and putting it above a fresh 1-point move would make the surface
useless for the thing it exists for.

| Read | Order |
|---|---|
| Recent board, cross-league / league | `window_end DESC, market_id DESC, selection_id DESC` |
| One market's history | `window_end DESC` within the bound |

Cursor tuple `(window_end, market_id, selection_id)`, all DESC. Total, because
`(market_id, selection_id, window_start, window_end)` is the natural key and `window_start` is
determined by `window_end` for a fixed window length.

### 6.12 Exclusions

- A book with **fewer than two observations** inside the window — no delta to measure.
- A book whose `|Δ|` is below `MinMagnitude` — it does not qualify as lead or follower (it may
  still contribute to the correlation statistic if `|Δ| >= NoiseFloor`).
- A window with fewer than `MinFollowers` qualifying followers.
- A window failing any of the four thresholds.
- `delta_probability = 0`.
- An observation older than the watermark — **dropped as late**, and it cannot reopen an
  evaluated window.
- Windows skipped past `MaxWindowsPerAdvance` after a watermark jump — counted, not silently
  absorbed.
- A finding suppressed by the cooldown for its `(market, selection, direction)` triple.

### 6.13 State, and what happens when it is lost

The detector holds a bounded ring of recent observations per `(market, selection, book)`, the
per-market watermark, the last evaluated window end, and the cooldown table. All of it is
bounded and prunable, and a market's state is released when the market is tombstoned upstream.

It is **not safe for concurrent use** — it is driven from one consumer handler goroutine, records
arrive sequentially, and a mutex would buy nothing but a false suggestion that a second caller
is expected.

**A consumer-group rebalance moves a partition to a replica with no window history**, which
evaluates its first window or two over partial data. The correction path is §9 and nothing
else: the finding is keyed on `(market, selection, window_start, window_end)`, so re-evaluating
the window later corrects the row in place rather than duplicating it. **A transient wrong
answer at a rebalance boundary is the accepted cost**, and it is acceptable only because that
correction path exists.

**All three detectors run before anything is written.** A store failure on the first +EV
finding must not leave the steam detector's window state un-advanced: the detectors mutate
state, the writes do not, so running every detector first means a redelivery re-runs them over
a detector that has already absorbed the record. That is exactly why the steam detector drops
observations older than its watermark and why every write is an upsert.

**The honest consequence, worth knowing before it is discovered:** on that redelivery the +EV
and arbitrage findings are recomputed identically and re-upserted, but the record's quotes are
now behind the steam watermark and are dropped as late, so a *closed* window is not
re-evaluated. A persist failure that took a steam finding down with it is therefore recovered
by a **full replay against fresh detector state** — an offset reset — and not by the
redelivery itself. Under the `ErrorPolicySkip` the pricer wires there is no redelivery at all,
which does not change the conclusion: an offset reset is the recovery path either way. The alternative ordering (detect-then-write per detector) trades this for a
worse failure: a transient store outage would silently skip windows instead.

### 6.14 Replay

Natural key `(market_id, selection_id, window_start, window_end)`. Upsert. A window
re-evaluated after a detector fix or an offset reset updates the existing row.

**A window that no longer meets its thresholds after a fix is not deleted.** As with +EV
(§4.7), there is no delete path, and the row carries the thresholds that were in force when it
was written.

---

## 7. Closing line value

**Computed by:** `settle`, per graded leg.
**Stored in:** `wager_leg_clv` (plain table, PK `leg_id`).
**Published to:** `signals.clv`, keyed by `wager_id`.

Defining the closing price *is* the work of this feature. The arithmetic is four lines and
has been in `internal/domain/odds/clv.go` since phase 1; what phase 9 adds is a precise,
replay-stable answer to "which prices are the two snapshots".

### 7.1 The arithmetic, and what it refuses

`odds.EvaluateCLV(taken, closing FairMarketSnapshot, selection)`:

```
p_t = taken fair probability     d_t = 1/p_t
p_c = closing fair probability   d_c = 1/p_c

probability_clv = p_c − p_t                          probability points
percent_clv     = (d_t / d_c − 1) × 100              percentage points of return
beat_close      = probability_clv > odds.CLVTieBand  (= 1e-12)
magnitude       = |percent_clv|
```

**Both inputs are FAIR (no-vig) probabilities**, and that is enforced structurally rather than
documented: the only way to construct a `FairMarketSnapshot` is `NewFairMarketSnapshot`, which
requires the **complete** outcome set of one market and rejects any set whose probabilities do
not sum to 1 within `odds.CLVDevigTolerance = 1e-9`. A vigged book cannot pass that check —
the tightest overround any real book posts is on the order of `1e-2`, seven orders of
magnitude above the tolerance.

Why this matters, with the standard worked example:

```
taken    home −110 / away −110    implied 0.5238095 each, Σ = 1.0476190 (4.545% hold)
closing  home −105 / away −105    implied 0.5121951 each, Σ = 1.0243902 (2.381% hold)
```

Devigged, both snapshots are 0.5 / 0.5 — the market's estimate did not move, and true CLV is
exactly zero. Compare the raw decimals and you get ≈ **−2.2173%**: a confident report that the
bettor lost 2.2 points of value on a line that never moved. Only the book's margin changed.
**That number is not CLV**, and a phase-12 job that joins raw prices will produce it.

`EvaluateCLV` refuses, in this order:

1. Either snapshot not built by `NewFairMarketSnapshot` → `ErrCLVMissingIdentity`.
2. Different market ids → `ErrCLVMarketMismatch`.
3. The selection absent from either snapshot → `ErrCLVSelectionAbsent`.
4. Different **sets** of selections → `ErrCLVOutcomeSetChanged`. Fair probabilities are a
   distribution over a sample space; if a runner was withdrawn between the wager and the
   close, the two are distributions over different sample spaces and no single component of
   them is comparable. There is no opt-out.
5. Different **lines**, compared with `domain.Line.Equal` → `ErrCLVLineMoved` (§7.6).
6. `closing.observed_at < taken.observed_at` → `ErrCLVClosingBeforeTaken`.

### 7.2 The devig — one method, both sides

`devig_method` is **one column, not two**: the same method devigs both snapshots. Devigging
the taken snapshot with Shin and the closing snapshot with multiplicative measures the
difference between two margin models, not closing line value — the same category error as
comparing raw prices.

| Rule | Value |
|---|---|
| Default method | **Shin** (`clv.DefaultDevigMethod = odds.MethodShin`) |
| Why Shin | It must match `internal/pricing`'s default, and not by coincidence: a user's +EV signals and their CLV are both statements about the same fair probability. Computing them under two margin models would let the surface report a price as +3% EV and then score it as having lost value on a line that never moved. |
| Fallback | If **either** side refuses the configured method, **both** are recomputed with multiplicative and the row records `multiplicative`. |
| Why both sides fall back | Falling back on only the failing side would break the single-method rule in the one situation where it matters most. |
| Why multiplicative | It is total — `q = p/S` cannot go negative and needs no root-find — so a market it also refuses is a market whose prices are not a market. Same argument as `internal/pricing/fairvalue.go`. |
| Neither method works | No row. `ReasonNotDevigable`. |
| **Quote order** | Both sides are devigged in **selection-id order**. The four methods are order-independent in exact arithmetic and **not** bit-independent in float64; a fixed order is what makes a replay reproduce a stored row. Phase 12 must use the same order. |

### 7.3 The closing snapshot — the exact definition

> The **closing snapshot** of market `M` is: for **every** selection `s` of `M`, the price at
> **book `B`** for `s` with the **greatest** `observed_at` satisfying `observed_at <= as_of`
> **and** `observed_at > not_before` **and** `observed_at` not inside any suspension episode
> of `M`.
>
> It is a closing snapshot **only if** every selection of `M` yielded such a price **and**
> every one of them agrees on the market line.

Six things in that definition are decisions rather than mechanics.

**1. `as_of` is `events.scheduled_start`.** Not the actual kickoff, not the instant the
event's status changed to `live`, not the last observation before the status changed, and not
"the last price we have". `scheduled_start` is the only candidate that is knowable before the
event starts, stable under replay because it is a stored column rather than a consequence of
when the poller happened to fire, and identical in Go and in Flink SQL without either side
needing a status-transition history. It is also what "closing line" means in the literature.

Rejected, and why each fails:

- **Actual kickoff.** There is no such column. The closest is the first observation carrying
  status `live`, which is a function of the poll cadence (ADR 0003's ladder), so two replays
  with different cadences would close the same market at two different instants.
- **The last price before the market suspended.** Circular: a market may suspend and reopen
  several times, so "the last price before suspension" is not a single instant.
- **The maximum `observed_at` on the market.** Contaminated by in-play prices, which answer a
  different question — see rule 6.

A **rescheduled** fixture closes at the **new** `scheduled_start`, because that is the row the
query reads. That is correct: the market that traded into the rescheduled start is the market
the wager was live against.

**A market has no usable closing instant, and produces no row (`ReasonNoClose`), when:**

- `scheduled_start` is unset; or
- `event_status` is `scheduled`, `postponed` or `unknown`.

An event that has not started has not closed, whatever its scheduled start says. `postponed`
is the case that matters, because `domain.EventStatus` admits `postponed → scheduled`: its
`scheduled_start` has moved to a date in the **future**, and reading a close at a future
instant would measure the wager against whatever the market happens to be quoting right now.

**2. `not_before` is REQUIRED**, and is both a chunk filter and a semantic bound. `prices`
has no retention policy, so an unbounded lower bound consults every chunk that ever existed —
`analytics.sql` calls this the sharpest edge on that table. It is equally a semantic bound: a
quote from six days before kickoff is not a closing line, it is a market nobody has priced
since, and scoring a wager against it would report the bettor as having beaten a number the
market had abandoned.

There are **two** lookbacks, both parameters of the CLV pass and both exported so phase 12 can
be handed the same numbers:

| Parameter | Default | Bounds |
|---|---|---|
| `ClosingLookback` | **24 h** | Lower bound set by the slowest polling tier a bettable market sits on (ADR 0003's cadence ladder tops out well inside a day) — anything shorter drops legitimate closes on quiet markets. Upper bound is where the number stops meaning anything: two days out, a market nobody has repriced is dormant, not closing. |
| `TakenLookback` | **24 h** | Change detection hashes a whole normalised market, so a book's selections are normally written together and the sibling quotes share the leg's instant to the microsecond. The window is not for the normal case — it is for the market whose one quiet side has not been republished in a while. |

`as_of` is **inclusive** (a quote observed exactly at `as_of` counts, which is what makes the
leg's own quote eligible for its own snapshot); `not_before` is **exclusive**. A zero
`not_before` is a programming error, not "no bound".

**3. One book — and it is the book the wager was struck at.**

Devigging is defined over a complete market: the margin is the excess of `Σ 1/d` over 1, and a
set of best-prices-across-books has no such excess to remove. `NewFairMarketSnapshot` enforces
this mechanically. So the close is one book's whole outcome set, never a mosaic.

**Which book is a real decision, and this system fixes it as the same book the leg was priced
at.** `odds/clv.go` permits either — "the two snapshots may come from different books, and
usually should" — and describes scoring against a sharp reference book as the standard
construction. **That construction is declined here**, and not on preference:

> A sharp reference book quotes its own line. When the customer took home −3 at their book
> and the reference book closed at −3.5, `EvaluateCLV` reports a **line move** — and
> `AggregateCLV` excludes every line-moved sample from the mean and the beat rate. Scoring
> against a different book would therefore drop most spread and total legs out of every
> aggregate, because two books disagreeing by half a point is the ordinary state of a market
> rather than an exception.

Same-book scoring keeps the line comparison meaningful: a line move then means **the market
moved**, which is what the flag is for, instead of meaning two books disagreed. It also
measures the number the customer could actually have had, which is the honest reading of "did
you beat the close".

**The cost is named rather than hidden:** two users betting two different books are scored
against two different closes, so the leaderboard compares each user against their own book's
market. A single mandated reference book would not fix that — it would replace it with a mass
exclusion — and the only real fix is a consensus close over several sharp books, which is a
model rather than a query and belongs behind its own ADR.

`wager_leg_clv.closing_book_id` is stored separately from `taken_book_id` anyway, because the
column exists to survive that rule changing.

**4. A suspended market's stale quote is not a close.** This is the predicate that makes the
query non-obvious. When a market is suspended the books stop moving it, so the last quote
before kickoff may be a frozen price from an hour earlier that nobody could have bet.

```sql
NOT EXISTS (
    SELECT 1 FROM market_suspensions ms
     WHERE ms.market_id = @market_id
       AND ms.suspended_at <= p.observed_at
       AND (ms.lifted_at IS NULL OR p.observed_at < ms.lifted_at)
)
```

Half-open (§2.4): a quote at the exact instant a suspension is **lifted** counts; one at the
instant it **begins** does not.

- **Suspended and reopened before the start** needs no special case. Quotes during the
  episode are excluded, quotes after the lift are eligible, and the last eligible one wins.
- **Suspended and never reopened** (`lifted_at IS NULL`) excludes every quote from
  `suspended_at` onwards, so the query falls back to the last quote *before* the suspension
  began — correctly, the last price at which the market was actually open. If that falls
  outside `not_before`, the selection yields nothing and the snapshot is incomplete, which is
  also correct: there is no close.

**`markets.status` is deliberately NOT consulted.** It is current state and says nothing about
what was true at `as_of`, where `market_suspensions` is the episode history. 00007's
`market_suspensions_one_open_idx` guarantees at most one open episode per market, so the
anti-join cannot be confused by overlapping episodes.

**5. There is no tie-break, and phase 12 needs none.** `prices_natural_key_idx` is UNIQUE on
`(selection_id, book_id, observed_at)`, so two prices for one selection at one book at one
instant cannot exist.

**6. An in-play wager has no CLV under this definition, and that is deliberate.** A bet struck
after kickoff has a placement instant **after** the closing instant, so `EvaluateCLV` returns
`ErrCLVClosingBeforeTaken` and the pass reports `ReasonCloseBeforeTake` and writes no row.

This is not a gap to be patched later with a different closing rule. Closing line value is a
claim about the **pre-game** market's final estimate; an in-play price answers a different
question — it is conditioned on a scoreline and a clock — and the pre-game close is not the
right comparison for it. Measuring one against the other would produce a number that looks
like CLV, ranks like CLV, and means something else. A live-wager analogue exists in the
literature and would need its own definition, its own columns and its own ADR. **On a system
with live betting enabled, a visible share of graded legs is unmeasurable for this reason and
that share is not a defect** — it is counted under its own reason label for exactly that
purpose.

### 7.4 The taken snapshot — same rule, two extra requirements

A leg stores its own price and nothing else, and devigging needs the whole outcome set, so the
market as it stood when the leg was booked has to be reconstructed from `prices` too. It is
reconstructed with the **same statement**, at the leg's own book, with the leg's own
`price_observed_at` as `as_of` and `TakenLookback` as the lower bound.

One rule for both sides is what makes the two snapshots `EvaluateCLV` compares comparable in
the first place. Two rules would eventually differ in a way that shows up as a systematic
bias rather than as an error. Applying the suspension exclusion to the taken side is harmless
— a wager cannot be placed into a suspended market — and keeping it means there is literally
one query.

The taken side carries **one extra requirement** the closing side does not: the quote the
reconstruction finds for the leg's **own** selection must be the leg's own quote, i.e. its
`observed_at` must equal the leg's `price_observed_at` **exactly**. Because
`prices_natural_key_idx` is UNIQUE on `(selection_id, book_id, observed_at)`, that equality
identifies the row uniquely and therefore pins the price too. A mismatch means the
reconstruction found a *different* quote from the one the customer was sold — a reconstruction
that describes a market the wager was not struck in. It produces `ReasonTakenQuoteMismatch`
and no row.

### 7.5 Completeness and coherence — both are the caller's check, and neither is optional

**Completeness.** The lateral join in `MarketSnapshotAtBookAsOf` is an INNER join, so a
selection with no eligible quote produces **no row**. Every row carries `market_selections`,
the count of selections the market actually has.

```
len(quotes) < market_selections  ⇒  DISCARD THE WHOLE SNAPSHOT
```

Do not devig the subset. Do not fall back to another book. Do not write a CLV row. Devigging a
partial outcome set produces probabilities wrong by the missing selection's entire mass —
`NewFairMarketSnapshot` will reject them, so the failure is loud, but the caller should not
have got that far. A store **cannot** report incompleteness by returning an error, which is
precisely why the count travels on the result.

**Coherence — every selection must agree on the line.** Change detection hashes a whole
normalised market, so a book's selections are normally written together and share an instant.
*Normally* is not *always*: a snapshot assembled per selection can end up holding the home
side at `−3.5` and the away side at `+3` if one of them has no eligible quote at the newer
instant.

That is not a market. Every quote is converted into the **market's frame** — the away side of
a **spread** is inverted, and nothing else is, which is exactly the inverse of
`domain.EffectiveLine` — and all of them must then be equal under `domain.Line.Equal`, which
distinguishes an absent line from a pick'em of `0.0`. Totals and player props share an
absolute threshold across both sides; moneylines and futures have no line at all.
Disagreement is **refused, never resolved**: picking one of the two lines would require a rule
that would itself have to be written down and reproduced. A snapshot that fails this produces
`ReasonTakenIncoherent` or `ReasonClosingIncoherent` and no row.

This is a strengthening of "the snapshot was incomplete" rather than a new category: an
incoherent snapshot is one whose selections are not all describing the same market question,
which is the same defect an absent selection has.

**The snapshot's instant is its newest quote.** A snapshot is assembled from up to *n* quotes
with up to *n* distinct instants, and `odds.FairMarketSnapshot` needs one. It is the
**maximum** of the quotes' `observed_at` — the instant at which every part of the snapshot was
simultaneously true — and never the closing instant itself, because the closing instant is a
value of ours (a scheduled start) rather than a provider observation, and migration 00009
requires `taken_at` and `closed_at` to be provider instants off one clock so that
`closed_at >= taken_at` can be a database constraint. On the taken side the maximum is always
the leg's own `price_observed_at`, since no quote may exceed the `as_of` bound and the leg's
own quote sits exactly on it.

### 7.6 The line move — display-only, never aggregated

A spread of `−3` closing at `−3.5` is not the same market question. The fair probability of
"home −3" and of "home −3.5" answer different questions, and their difference is not value
captured — it is mostly the probability mass sitting on a three-point margin. Converting
between them needs a distribution of game margins, which is a model, not arithmetic.

- `EvaluateCLV` **rejects** a line move with `ErrCLVLineMoved`.
- `EvaluateCLVAcrossLineMove` computes the number anyway and sets `CLVResult.LineMoved`.
- `AggregateCLV` **excludes every line-moved sample** from the mean and from the beat rate,
  and reports how many it dropped as `LineMovedExcluded`.

The indicative number is for a per-wager display that wants to show "you took −3, it closed
−3.5". **It must never reach a leaderboard**, and both the aggregate and the leaderboard query
enforce that rather than trusting it.

Stored as `line_moved`, with the schema pinning the definition:
`line_moved = (taken_line IS DISTINCT FROM closing_line)`. `IS DISTINCT FROM`, not `<>`,
because `NULL` is a meaningful value here (§2.2).

### 7.7 Void and push

| Outcome | `leg_status` | `voided` | Counted in aggregates? |
|---|---|---|---|
| Won | `won` | false | **yes** |
| Lost | `lost` | false | **yes** |
| Pushed | `push` | false | **yes, at full weight** |
| Voided | `void` | true | **no — numerator and denominator alike** |

**A push is not a void.** A push had action: the price was taken and the market closed, and
only the settlement returned the stake. CLV measures the quality of the price, not the result.
Excluding pushes would make a bettor's CLV depend on the scoreboard — precisely the dependency
CLV exists to remove — and would bias the metric toward market types that cannot push.
`CLVSample` carries no push flag because CLV does not need to know.

**A void has no closing line to be measured against**, because the market never closed. It is
excluded from every statistic, numerator and denominator alike.

Schema-pinned: `voided = (leg_status = 'void')`, and `leg_status` is one of
`won | lost | void | push` — **never `pending`**. A leg with no result is not a CLV row that
is waiting; it is not a CLV row.

### 7.8 Absence is meaningful — the complete outcome table

Migration 00009's rule, which this pass implements: **"we could not measure it" and "it
measured zero" must not share a shape**, or a leaderboard cannot tell them apart. A leg gets a
complete row or no row, never a row of nulls.

| Case | Result |
|---|---|
| Everything present, same line | **A row.** Ranked. |
| Line moved between take and close | **A row**, `line_moved = true`. Served for display; excluded from every aggregate by `AggregateCLV` and by the leaderboard query. |
| Leg voided (market cancelled, event abandoned) | **A row**, `voided = true`. Also excluded from aggregates. **A push is not void and is ranked.** |
| Market has no usable closing instant — no `scheduled_start`, or event `scheduled` / `postponed` / `unknown` | **No row.** `ReasonNoClose` |
| Taken snapshot incomplete | **No row.** `ReasonTakenIncomplete` |
| Taken snapshot's lines disagree | **No row.** `ReasonTakenIncoherent` |
| Reconstructed quote for the leg's own selection is not the leg's own quote | **No row.** `ReasonTakenQuoteMismatch` |
| Closing snapshot incomplete — including "every candidate close was inside a suspension", which surfaces here because the excluded quotes leave selections unpriced | **No row.** `ReasonClosingIncomplete` |
| Closing snapshot's lines disagree | **No row.** `ReasonClosingIncoherent` |
| Outcome set changed between take and close | **No row.** `ReasonOutcomeSetChanged` |
| Close would precede the take (an in-play wager) | **No row.** `ReasonCloseBeforeTake` |
| Neither the configured method nor multiplicative could devig a side | **No row.** `ReasonNotDevigable` |

The reason labels are the bounded set `no_close`, `taken_incomplete`, `taken_incoherent`,
`taken_quote_mismatch`, `closing_incomplete`, `closing_incoherent`, `outcome_set_changed`,
`close_before_take`, `not_devigable` — suitable as a metric label, and the vocabulary phase 12
should count under too.

**"The market never traded at the taken line" is not a distinct case**, which is worth stating
because a reimplementation will look for it. The taken line is not supplied from outside — it
is *read off* the taken snapshot, which is the market as it stood when the leg was booked. So
either that snapshot exists (and the taken line is whatever it says, by construction) or it
does not (and there is no measurement). What varies is whether the *closing* snapshot's line
matches, and that is the line-move row above.

**Three kinds of failure, handled differently.** Conflating the second and third is the
failure mode the CLV pass's signature exists to prevent:

| Failure | Meaning | What to do |
|---|---|---|
| `ErrUnusableLeg` | The work-queue row is not a graded leg — a malformed identifier, an unknown market type, a non-terminal status, a missing observation instant. A defect in whatever produced it. | **Never retry.** Do not count it among the honest exclusions. |
| `ErrUnmeasurable` (wrapping one of the nine reasons) | The data cannot answer for this leg. The documented outcome for an in-play wager, a market that shut early, or a field that lost a runner. | Write no row, count the reason. **Not a failure.** |
| Anything else | The store failed. Transient. | Retry on the next pass. |

A missing `wager_leg_clv` row means "this leg has no measurable closing line value", which is
a different and honest claim from "this leg's CLV was zero". `ListGradedLegsAwaitingCLV` is
the work queue that distinguishes them: a graded leg with no CLV row is *either* not yet
measured *or* legitimately unmeasurable, and the queue's required `graded_at` lower bound is
what stops the permanently-unmeasurable set from being re-attempted forever.

### 7.9 Aggregation

`odds.AggregateCLV` over `[]CLVSample`, and its SQL equivalent `GetUserCLVAggregate`:

| Output | Rule |
|---|---|
| `Samples` | every sample supplied |
| `Counted` | `NOT voided AND NOT line_moved` |
| `VoidExcluded` | `voided` |
| `LineMovedExcluded` | `NOT voided AND line_moved` |
| `BeatCount` | counted samples with `beat_close` |
| `BeatRate` | `BeatCount / Counted` |
| `MeanProbabilityCLV` | **unweighted** arithmetic mean over counted samples |
| `MeanPercentCLV` | **unweighted** arithmetic mean over counted samples |

**Unweighted by stake, on purpose.** CLV is a property of the price, not of the stake, and
stake-weighting would let a bettor buy leaderboard position by sizing up.

**The means are genuinely nullable and the null must survive.** `AggregateCLV` returns
`ErrCLVNoSamples` when nothing is countable — an empty slice, or a slice in which everything
was excluded — "rather than reporting the mean of an empty set as zero, which would put a user
with three voided wagers on the leaderboard at exactly par". `GetUserCLVAggregate` returns
`pgtype.Float8` with `Valid = false` for the same case. **The API must render that as "no
measurable wagers", never as `0.00%`.** A `COALESCE(…, 0)` anywhere on this path reintroduces
precisely the bug the sentinel exists to prevent.

`GetUserCLVAggregate` is a three-CTE shape on purpose: it always returns **exactly one row**,
even for a user with no history at all — honest zeros with null means, rather than
`pgx.ErrNoRows` that every caller has to special-case.

The three exclusion counts are the point. A user whose CLV is computed from a third of their
wagers is a different claim from one whose CLV is computed from all of them, and the row says
which.

### 7.10 The pass — how legs reach the measurer

`settle` grades a leg and then measures it, and the two are separate steps because the close
is not knowable until the event starts while grading is not possible until it ends.
`ListGradedLegsAwaitingCLV` is what makes the second step restartable: it is a `LEFT JOIN`
anti-join (not `NOT IN`, so a `NULL` cannot make the predicate unknown for every row) over
graded legs with no CLV row, **ordered oldest-first** so a backlog does not starve the oldest
work.

| Parameter | Default | Reasoning |
|---|---|---|
| Poll interval | **30 s** | Chosen against what *feeds* the queue, not against how fast a database can be polled: a leg becomes measurable only when settlement grades it, and a result cannot reach `events` faster than ingest's live-tier cadence, so the queue fills in bursts separated by tens of seconds. Six times the settlement interval on purpose — CLV is a report, and a report thirty seconds behind the ledger is not behind at all, whereas a balance is. |
| Batch | **500 legs** | A **fairness** bound rather than a memory one. Every leg graded from one result carries that result's finalisation instant, so a popular game's legs all share one `graded_at`; a batch smaller than one game's leg count could be filled entirely by legs at a single instant and the pass could not step past them. |
| Retry window | **24 h** | The retry budget for a leg whose measurement failed for a reason that might yet be fixed — a backfilled price, a suspension recorded late — and simultaneously the point at which the system stops asking a question the data has already refused to answer. Erring long is cheap and erring short is not: **a leg that ages out unmeasured is unmeasured for ever**, because nothing puts it back. The cost is that every permanently-unmeasurable leg inside the window is re-attempted once per pass — two indexed reads, bounded and reported as the floor of the queue-depth gauge. |
| Per-leg timeout | **20 s** | Bounds the two reads *and* the synchronous publish, which is the part that can genuinely take a long time. Matches the transaction timeout so a wedged broker looks the same on both of the binary's loops. |

None of these four changes a measurement's *value* — they decide only which legs are attempted
and when. Phase 12 needs the retry window's semantics (a leg outside it is never retried) but
not the cadence.

### 7.11 Replay

Natural key `leg_id` — the primary key. One leg has exactly one close, so idempotency is free
and needs no extra column. That property is also why the table is plain rather than a
hypertable: a hypertable's primary key must contain the partitioning column, and adding a time
column to this key would destroy it.

`user_id` is denormalised onto the row and **cannot be pinned by a composite foreign key**:
that would need `UNIQUE (id, user_id)` on `wagers`, which migration 00006 does not declare and
00009 may not add. It is a plain FK to `users(id)` plus a **writer obligation**: the writer
must copy `user_id` from the leg's own wager. This is stated rather than papered over; adding
that unique constraint in a later migration is what would close it.

---

## 8. The leaderboard

**Served by:** `api`. **Queries:** `LeaderboardByROI`, `LeaderboardByCLV`.
**A query, not a view** — the minimum-sample thresholds are parameters, and a view would
freeze them.

CLAUDE.md §6: "A public leaderboard on ROI and CLV, **not raw profit**."

### 8.1 Why not profit, stated so nobody re-adds it

Raw profit ranks by **stake size** and by **variance**. The user who staked the maximum on one
coin flip and won tops a profit board, and the ranking then teaches exactly the behaviour a
responsible-gaming-aware product should not. Both measures below are normalised against that:

- **ROI** = `SUM(net_return_minor) / SUM(stake_minor)`. Stake-normalised, so betting bigger
  cannot improve it. A losing bettor has a negative ROI at any stake size, which is the gate:
  **a high-stake losing bettor cannot outrank a low-stake winning one, structurally, at any
  sample size.**
- **CLV** = the unweighted mean of `percent_clv` over countable rows, for `AggregateCLV`'s
  stated reason.

### 8.2 Which wagers count

```sql
status IN ('won', 'lost', 'push', 'cashed_out')
AND placed_at >= @from_inclusive AND placed_at < @to_exclusive
```

| Status | Counts? | Why |
|---|---|---|
| `won`, `lost` | yes | Terminal, with action. |
| `push` | **yes** | A push had action — the price was taken and the market closed. Excluding pushes would drop the most common outcome on totals and flatter anyone who specialises in them. Matches §7.7. |
| `cashed_out` | **yes** | A real settled outcome, with whatever was paid at cash-out as the return. Excluding it would let a user launder a losing position out of their record. |
| `placed`, `open` | no | Unresolved. Including an open wager's stake in the denominator with a null return would drag every active bettor's ROI toward −1. |
| `void` | **no — numerator and denominator alike** | A void had **no action**. 00006's `wagers_return_matches_outcome` pins `returned_minor = stake_minor`, so its net return is exactly zero: including it leaves the numerator unchanged while inflating the denominator, pulling every ROI toward zero in proportion to how many of a user's wagers the book cancelled. That is turnover that never happened. It also matches `odds.CLVSample.Void`, so both halves of the board apply the same rule to the same events. |

The two halves use **different time columns**: the wager half filters on `wagers.placed_at`,
the CLV half on `wager_leg_clv.graded_at`. They are not the same instant and are not
interchangeable — a wager placed inside the window and graded outside it contributes to the
ROI half and not to the CLV half. Phase 12 must reproduce that asymmetry rather than
harmonising it.

### 8.3 Money stays integer

`staked_minor` and `net_return_minor` are `BIGINT` minor units, cast explicitly because
`sum(bigint)` returns `NUMERIC` in PostgreSQL and numeric would arrive in Go as a decimal
string, forcing a parse onto the money path.

They arrive as bare `int64`, **not** `domain.Money`, and that is correct: every *stored* money
column is bounded by CHECK to `±domain.MaxSafeMoney`, but a `SUM` is not, so the read boundary
must pass them through `domain.FromMinorUnits`, which errors on overflow. Do not add a sqlc
override.

`roi` and `beat_rate` are **ratios and therefore floats**, per CLAUDE.md §12's split. The ROI
denominator cannot be zero: `wagers_stake_range` pins `stake_minor > 0` and the threshold
guarantees at least one row.

### 8.4 Minimum sample

Both thresholds are **parameters with no in-query default**, so the API declares the values
and reports them next to the board — which is the honest presentation.

| Parameter | Applies to | API default | Bounds |
|---|---|---|---|
| `@min_settled_wagers` | `settled_wagers` from the wager half | **20** | `[1, 1000000]` |
| `@min_clv_samples` | `clv_samples` from the CLV half | **20** | `[1, 1000000]` |
| window `[from, to)` | `wagers.placed_at` and `wager_leg_clv.graded_at` | **last 90 days** | caller-supplied |

**Twenty is a product decision, and it is stated rather than implied.** The reasoning is the
one CLAUDE.md §6 gives for refusing a profit ranking: the failure mode is one lucky
maximum-stake bet at the top of the board, and a floor is what makes that *unrepresentable*
rather than merely unlikely. **Twenty is not enough for CLV to be statistically strong** —
nothing at this scale is — which is exactly why the response reports the floors **and** each
row's sample counts instead of implying a confidence it does not have. The upper bound of
1,000,000 exists because a floor above it admits nobody and is more likely a client bug than
an intention.

**The default window is 90 days rather than all time**, and it matches the per-user CLV
endpoint's default so a customer comparing their own aggregate against their leaderboard row
is comparing the same window. An all-time board would **ossify**: the same names would sit at
the top for ever, because nobody's early sample can ever be diluted.

Without them, a single maximum-stake winning wager is an ROI of +0.9 on a sample of one and
tops the board forever. **Two thresholds rather than one** because the two measures have
different denominators: a user can have fifty settled wagers and three countable CLV rows (the
rest line-moved), and ranking them on a three-sample CLV mean next to someone with fifty is
the same defect the wager threshold exists to prevent.

This is a **threshold rather than a confidence interval**, deliberately. A Wilson or bootstrap
interval is the statistically better answer and is unexplainable on a public page, where
"minimum 20 settled wagers" is immediately understandable and is the convention every real
leaderboard uses.

### 8.5 Joins and exclusions

- **The CLV join is INNER, not LEFT**, and that is load-bearing: a user with zero countable
  CLV samples is **absent** from the board rather than present with a null or a zero. It is
  the same choice `AggregateCLV` makes by returning `ErrCLVNoSamples`.
- The CLV half filters `NOT voided AND NOT line_moved` — exactly `AggregateCLV`'s two
  exclusions, and exactly the predicate on the partial index
  `wager_leg_clv_countable_idx`.
- `JOIN users u ON u.id = s.user_id WHERE u.status = 'active'`.
- **No column of `users` is selected except `u.id`.** This is a public board and `users` has
  no display name — only an email address, which must never leave the API. The board returns
  `user_id` and the API maps it to a public handle. A future `users.display_name` is the right
  place for that, not this query.

### 8.6 Tie-breaks

```
ROI board:  roi DESC, mean_percent_clv DESC, settled_wagers DESC, user_id ASC
CLV board:  mean_percent_clv DESC, roi DESC, clv_samples DESC, user_id ASC
```

Each ranks on its own measure, breaks with the **other** measure — so a tie on results is
broken by process, which is the ordering the charter's preference for CLV over profit implies
— then by **sample count**, so the better-evidenced record wins a genuine tie, and finally by
`user_id`, which is a primary key and therefore makes the ordering **total**. Without that
last component two equal rows swap places between refreshes and the board looks broken.

### 8.7 A known precision divergence

PostgreSQL's `avg(double precision)` is a **naive running sum**, where `odds.AggregateCLV`
uses Kahan–Babuška–Neumaier compensation. `clv.go` is explicit that the difference matters at
leaderboard scale: naive summation's worst-case error over a hundred thousand samples of
magnitude ~1 is around `2e-6`, "which is larger than the margin that separates adjacent
leaderboard rows".

Mitigated rather than ignored, in two ways. First, the sum here is **per user**, so `n` is one
user's sample count — hundreds to low thousands, not the hundred thousand the worst case
assumes. Second, the tie-break chain terminates in `user_id`, so two rows whose means are
equal to within float error still order **stably** between refreshes rather than swapping.

If a user ever accumulates enough samples for this to bite, the fix is to hand the rows to
`odds.AggregateCLV` in Go rather than to make PostgreSQL compensate.
`ListUserCLVFirstPage` already returns exactly what that function consumes. **A phase-12
comparison against the leaderboard should use the aggregate tolerance in §10, not equality.**

---

## 9. Idempotency and replay, in one table

Nothing in phase 9 is append-only, and that is a decision rather than an omission. `prices`
and `ledger_entries` are **observations** — they happened, nobody may edit them. Everything
phase 9 writes is a **derivation**: a function of those observations plus a set of parameters.
Re-run the function over the same inputs with the same parameters and you must get the same
row back; re-run it with a corrected detector and you *should* get a corrected row.

Freezing derived rows would make a detector bug permanent in storage and would make the
phase-12 cutover — which is precisely "recompute these rows with a different engine and diff
them" — impossible to perform in place.

| Table | Natural key | Conflict action |
|---|---|---|
| `ev_signals` | `(selection_id, book_id, quote_observed_at)` | `DO UPDATE` |
| `steam_signals` | `(market_id, selection_id, window_start, window_end)` | `DO UPDATE` |
| `arbitrage_signals` | `(market_id, observed_at, legs_fingerprint)` | `DO UPDATE`, returning the existing `id` |
| `arbitrage_signal_legs` | child of the above, `ON DELETE CASCADE` | deleted and reinserted wholesale |
| `wager_leg_clv` | `leg_id` (the primary key) | `DO UPDATE` |

**Three rules that phase 12 must honour, and that are easy to get wrong:**

1. **`DO UPDATE`, never `DO NOTHING`.** A replay after a fix *is* the correction. `DO NOTHING`
   would silently preserve the bug.
2. **No natural key contains a clock reading of ours.** `detected_at` and `computed_at` are
   stored, are genuinely useful — they answer "how long after the fact did we notice" — and
   are **never** part of an identity, a window boundary, a filter or an ordering. A key
   containing `detected_at` would make every replay INSERT a duplicate, and the table would
   grow a fresh copy of every finding each time a consumer group's offsets were reset.
3. **Partitioning is on event time.** A hypertable's unique index must contain the
   partitioning column; combine that with rule 2 and the partitioning column is *forced* to be
   an event-time column. `ev_signals` → `quote_observed_at`; `steam_signals` → `window_end`.
   A Flink job that time-indexed these by processing time would have to translate between two
   time domains before it could compare anything.

There is **no retention policy** on either hypertable, which is why **every read of
`ev_signals`, `steam_signals` or `prices` takes a required lower time bound**. An unbounded
read consults every chunk ever created.

### 9.1 Two write failures are retried in place, and both are a consequence of the keys above

**A deadlock victim (`40P01`, or `40001` under a serialisable transaction).** Postgres rolled the
transaction back, so nothing was written. It is re-run in place, up to three attempts, with a
short backoff — not returned to the consumer as a failed record. Every retried attempt is counted
as `sharpline_analytics_writes_total{sink="store",outcome="contended"}`; exhausting the budget
fails the record with the SQLSTATE intact, so sustained contention is visible rather than absorbed
into latency.

**A foreign-key violation (`23503`) — the catalogue-lag race.** Every foreign key on the phase-9
tables points at the catalogue, which `ingest` writes and the signals stage only reads. `ingest`'s
Timescale writer and `pricer` are two independent consumers of `odds.normalized` and nothing
orders them, so a market can be priced, published on `price.computed` and read by the signals
stage in the instant before its `markets` row commits. It is at its worst on a cold start, where
a fresh consumer group replays the whole compacted topic against an empty catalogue.

This is **not** a disagreement between a detector and `migrations/00009`, and it must not be
reported as one. The detectors are validated against the CHECK constraints before any write; a
foreign key cannot be validated that way, because a finding derived from one `price.computed`
record carries no evidence about what has been committed. It is retried on the same three-attempt
budget, counted as `outcome="catalogue_lag"`, and if the parent still has not landed the record is
counted as `sharpline_analytics_records_total{result="deferred"}` and **advanced over, not
failed**.

Deferring rather than failing is forced by what the two error policies actually do.
`ErrorPolicyStop` would halt the entire signals consumer over a transient referential gap — on a
cold start, over most of the first replay. `ErrorPolicySkip`, which is what `pricer` wires,
advances past the record regardless, so returning an error buys nothing and costs an ERROR line
per record claiming a redelivery that never happens. What actually recovers the finding is the
market's **next price change**: `price.computed` is compacted and republished on every change, the
detectors re-derive the identical finding, and the upsert lands the row the first attempt would
have written.

A record is deferred only if **every** finding on it failed this way. `errors.Join` is satisfied
by any leaf, so one genuine store outage sharing a record with one unanchored finding is reported
as the outage.

Phase 12 inherits this race unchanged — a Flink sink writing these tables sees the same two
independent writers — and needs the same three-way classification: rolled back, not-yet-anchored,
and actually broken.

This is safe **because of the table above and for no other reason**: every write is an upsert on
a key derived from the input alone, so a re-run produces the row the first attempt would have,
and the bus publish happens strictly after the persist returns, so a retry cannot double-publish.
A deadlock is also the one failure whose outcome is unambiguous — the server chose this
transaction as the victim and undid it — unlike `08007 transaction_resolution_unknown`, which is
deliberately not retried anywhere in this system.

**The contention it exists for is structural, and phase 12 will meet it too.** `ingest` upserts
the catalogue spine — sports, leagues, events, markets, books, selections — in foreign-key
order, and `INSERT … ON CONFLICT DO UPDATE` takes an *exclusive* row lock on every conflicting
row before it evaluates its `WHERE`, holding it to `COMMIT`. A signals insert takes `FOR KEY
SHARE` on those same rows in referential-integrity-trigger order, which is a different order.
Two orders, two processes, one cycle. The root-cause mitigation lives in `ingest`, which now
filters rows that are byte-identical out of the statement with a `WHERE NOT EXISTS (… IS NOT
DISTINCT FROM …)` anti-join **before** they can reach `ON CONFLICT` — an unchanged catalogue row
is therefore never locked at all, and `FOR KEY SHARE` is compatible with itself. The retry
remains as the backstop for the genuinely-changed row.

A Flink sink writing these same tables inherits both halves of the problem and neither of the
mitigations, so it needs its own.

---

## 10. Floating point: where two implementations may legitimately differ

This section is the first thing that will bite phase 12, and it is worth naming before it
does. Bit-identical agreement between a Go implementation and a Flink SQL one is **not
achievable and is not the standard.** Flink's aggregation order varies with parallelism, and
several of the quantities below are computed by algebraically-equivalent-but-numerically-
different routes on purpose.

### 10.1 Comparison tolerances

| Class of value | Tolerance | Rationale |
|---|---|---|
| Per-sample scalars: `expected_value`, `edge`, `probability_clv`, `percent_clv`, `return_fraction`, `stake_fraction`, `delta_probability`, `velocity_*` | **relative `1e-12`** | Matches the tolerance the odds domain's own tests use, and is the number `clv.go` names for the phase-12 comparison. |
| Aggregates: `mean_probability_clv`, `mean_percent_clv`, `roi`, `beat_rate`, any leaderboard column | **relative `1e-9`** | Summation order is not reproducible across parallelism. |
| Devig residual `|Σp − 1|` | **absolute `1e-9`** (`odds.CLVDevigTolerance`) | The gate a fair snapshot must clear. Absolute rather than relative because the target is exactly 1. |
| Under/over-round classification | **absolute `1e-12`** (`odds.FairMarketTolerance`) | The half-width of the band around `S = 1`. |
| `beat_close` | **exact, via the band** | Not a tolerance on a comparison — the tie band *is* the semantics. |

**Any disagreement larger than the relevant tolerance is a defect in one of the two
implementations, not a rounding difference.**

### 10.2 The specific places associativity bites

**Summation of implied probabilities.** `odds.NewMargin` and `NewFairMarketSnapshot` both use
Kahan–Babuška–Neumaier compensation; PostgreSQL's `avg` and `sum` do not, and Flink's
aggregation order varies with parallelism. Anywhere `Σ` appears — `implied_sum`, the CLV means,
the leaderboard means — the two implementations may differ in the last few ULPs.

**Two routes to the same quantity.** `edge = q/p − 1` (a division) and
`expected_value = q·d − 1` (a multiplication) are the same number when `p = 1/d`, but in
float64 they can differ in the last unit in the last place. Both are stored. **The migration
declines an arithmetic-identity CHECK relating them, deliberately** — a SQL re-derivation of a
multi-step float formula is not bit-identical to the Go, and a CHECK would reject correct rows.
The same applies to `percent_clv`, which is three chained operations.

The identities that *are* enforced in SQL are exactly the ones that are a **single IEEE
operation** and therefore bit-identical in both languages:

```sql
probability_clv              = closing_fair - taken_fair        -- one subtraction
magnitude                    = abs(percent_clv)                 -- exact
magnitude_probability_points = abs(delta_probability)           -- exact
beat_close                   = (probability_clv > 1e-12)        -- a comparison
line_moved                   = (taken_line IS DISTINCT FROM closing_line)
voided                       = (leg_status = 'void')
participating_books          = follower_count + 1
follower_count               = jsonb_array_length(followers)
```

That split — enforce the single-operation identities, decline the multi-step ones — is the
rule for anything phase 12 adds.

**Compiler contraction.** `odds.ExpectedValue` computes `q·d` inside `grossReturn`, behind an
explicit `float64` conversion that acts as a rounding barrier, because the compiler is
permitted — and on arm64 actually chooses — to contract `q·d − 1` into a single fused
multiply-subtract with only one rounding. That is a *more* accurate answer and a *different*
one, and it would differ between the arm64 development machine and the amd64 server. A pricing
number that changes with the host architecture is not reproducible.

**Steam is mostly exact, and the exceptions are named.** Window boundaries are integer
nanosecond arithmetic off the Unix epoch and must agree bit for bit — an off-by-one there is a
different window, not a rounding difference. `Δ_b = p_last − p_first` is a single subtraction.
The correlation is a mean of values drawn from `{+1, 0, −1}`, so its numerator is exact and
only the final division rounds. `velocity = Δ / (Window in minutes)` is one division. The one
place to watch is the `moved_at` argmax: two steps whose magnitudes differ only in the last
ULP would be ordered differently by two implementations, which is why the tie-break is
specified as **the earliest such instant** rather than left to the sort.

**Exact zeros are exact.** `ExpectedValue` short-circuits the exact break-even case to a
literal `0`, and `Kelly` returns exactly `0` whenever `q·d <= 1`. `Kelly(BreakevenProbability(d), d) == 0`
holds with **no tolerance** for every legal `d` whose reciprocal is normal — CLAUDE.md §4 names
that invariant. A phase-12 implementation that returns `1e-17` there has a defect, not a
rounding difference: "stake nothing" has to be exact, because a fraction of `1e-16` is not a
curiosity once it reaches a bankroll and a ledger.

### 10.3 Things that must be exact regardless

- Every **count**: `samples`, `counted`, `void_excluded`, `line_moved_excluded`,
  `beat_count`, `settled_wagers`, `clv_samples`, `follower_count`, `participating_books`,
  `distinct_books`, `selection_count`.
- Every **money** value. Minor units are integers end to end, including in JSON.
- Every **identifier**, **status**, **direction** and **devig method** string.
- Every **ordering**, including tie-breaks (§2.5).
- Every **boolean**: `beat_close`, `line_moved`, `voided`.
- The **legs fingerprint** (§5.5).
- **Which rows exist at all.** The exclusion rules in §4.6, §5.8, §6.12 and §7.8 are the
  highest-value part of this document to diff: a phase-12 job that produces the right numbers
  for the wrong set of rows is more wrong, not less, than one whose numbers are off in the
  twelfth decimal.

---

## 11. The phase-12 conformance checklist

Reproduce these and the Flink jobs are equivalent. Miss one and they are not, whatever the
numbers say.

**Keys and replay**

- [ ] The four natural keys of §9, exactly.
- [ ] Upsert semantics: correction lands, never `DO NOTHING`.
- [ ] No clock reading of ours in any key, window bound, filter or ordering.
- [ ] Event-time partitioning: `quote_observed_at` for +EV, `window_end` for steam.
- [ ] Three-way classification of a refused write (§9.1): rolled back (`40P01`/`40001`) and
      not-yet-anchored (`23503`) are retried in place; only the third kind is a failure. A
      record whose every finding was refused for the second reason is **deferred**, not failed,
      and is recovered by the market's next reprice rather than by a redelivery.

**+EV**

- [ ] Only `priced` quotes; `stale`, `line_mismatch`, `no_fair_value` and `unpriceable` are
      categorically excluded, not merely unlikely.
- [ ] Fair value from **one** reference book, never a consensus; the market's `devig_method`
      recorded and reproduced.
- [ ] Three gates in order — EV floor **1.0 %**, freshness **3 min**, error bar **1.0×**.
- [ ] The error-bar gate compares `ExpectedValue` against `Disagreement × OfferedDecimal`, not
      against `Disagreement` directly. A negative `Disagreement` means the gate does not bind
      and the finding is counted `unbounded`.
- [ ] `expected_value_percent >= threshold_ev_percent`, and strictly positive EV.
- [ ] `fractional_kelly = kelly · kelly_fraction`, with `kelly_fraction` supplied (0.25 by
      default) rather than derived by division.
- [ ] Ranking and cursor tuple `(ev%, quote_observed_at, selection_id, book_id)`, all DESC.
- [ ] The exhaustive reason set of §4.6, with `unbounded` overlapping `signal` rather than
      partitioning against it.

**Arbitrage**

- [ ] Market gate → age filter → group by home-frame line → best price, in that order.
- [ ] Best-price tie-break: longer odds, then fresher, then lower book id.
- [ ] Under-round tested against `FairMarketTolerance = 1e-12`, not against `1.0`.
- [ ] `observed_at` is the **oldest** leg's instant.
- [ ] Signal bounds **60 s / 20 s / 0.5 %**, tighter than the detector's 120 s / 30 s / 0.1 %,
      and written onto the row.
- [ ] `distinct_books = 1` is legal and is the stronger finding.
- [ ] The fingerprint contract of §5.5 byte for byte — SHA-256, full 64 hex, `0x1f`
      separators, `-` for an absent line, legs sorted by `selection_id`.
- [ ] `leg_index` is the market's selection display order and is stable across recomputation.
- [ ] Leg age and observed spread present on every reported finding.

**Steam**

- [ ] **Raw** implied probability, margin included, `devig_method = "none"` — never decimal
      odds and never devigged.
- [ ] Hopping windows, half-open, **aligned to the Unix epoch in UTC**: window *k* is
      `[k·Hop, k·Hop + Window)`. 180 s every 60 s by default.
- [ ] A **watermark** closes windows, never a timer: greatest observation instant per market
      minus `AllowedLateness` (180 s), monotone non-decreasing, each window evaluated once.
- [ ] Late observations are dropped and counted; they cannot reopen an evaluated window.
- [ ] Directional — `selection_id` in the key, `direction` keyed into the cooldown.
- [ ] `Δ_b` is first-to-last; `moved_at_b` is the end instant of the largest step in Δ's
      direction, ties to the earliest.
- [ ] Lead = earliest `moved_at` among qualifying books; ties by greater `|Δ|`, then smaller
      book id.
- [ ] Followers = every other **qualifying** book (`|Δ_b| >= MinMagnitude`, the same bar the
      lead had to clear) with the same sign and lag in `[0, MaxFollowerLag]`; **lag 0 is legal
      and common**. Dropping the magnitude bar here admits a book that barely twitched as
      corroboration and turns `MinFollowers = 1` into no constraint at all.
- [ ] The finding's delta is the **lead's**, not an average.
- [ ] `velocity = Δ_lead / (Window in minutes)` — a window-average rate.
- [ ] Correlation = mean signed agreement with a `NoiseFloor`; it screens out a lone book's
      tick, and it does **not** separate steam from drift.
- [ ] All four thresholds met, and stored on the row. `MinMagnitude` is **0.050** probability
      points — calibrated past the knee of the candidate-count sweep, not derived; 0.030 is a
      ten-times firehose.
- [ ] Cooldown on `(market, selection, direction)`, measured on `window_end` in **event time**.
- [ ] `followers` ordered by `lag_seconds` ascending then `book_id` ascending; `moved_at`
      RFC 3339 UTC.
- [ ] Ranked by recency; magnitude is a filter.
- [ ] **The series is NOT segmented by line** (§6.2b). An observation is
      `(selection, book, implied, observed_at)`; a handicap move contributes to the same series
      as a price move at a fixed handicap. A line-segmented implementation produces an almost
      disjoint answer set, and 96% of live findings are handicap moves.
- [ ] Fires on a generated steam move; does **not** fire on ordinary drift.

**CLV**

- [ ] `as_of = events.scheduled_start`; a rescheduled fixture closes at the new start.
- [ ] No close when `scheduled_start` is unset or the event is `scheduled` / `postponed` /
      `unknown`.
- [ ] Two required lookbacks, `ClosingLookback` and `TakenLookback`, both 24 h by default;
      `as_of` inclusive, `not_before` exclusive.
- [ ] **The closing book is the leg's own book**, not a sharp reference book.
- [ ] Complete outcome set; snapshot discarded whole if short.
- [ ] Line coherence: every quote converted to the market frame must agree under
      `Line.Equal`; disagreement is refused, never resolved.
- [ ] The taken snapshot's quote for the leg's own selection must match
      `price_observed_at` exactly.
- [ ] The snapshot's instant is the **maximum** of its quotes' `observed_at`.
- [ ] Suspension exclusion, half-open, from `market_suspensions` — not from `markets.status`.
- [ ] One devig method both sides; fall back to multiplicative on **both** if either refuses;
      quotes devigged in **selection-id order**.
- [ ] Line move → display only, excluded from every aggregate.
- [ ] Void excluded from numerator and denominator; **push included at full weight**.
- [ ] `beat_close = probability_clv > 1e-12`.
- [ ] Absence is a row that does not exist, never a row of nulls, classified by one of the
      nine reasons.
- [ ] In-play wagers are unmeasurable by design, and the count is not a defect.

**Leaderboard**

- [ ] `won | lost | push | cashed_out`; `void`, `placed` and `open` excluded.
- [ ] ROI = `Σ net_return / Σ stake`, stake-normalised.
- [ ] CLV mean unweighted.
- [ ] Both minimum-sample thresholds applied, as parameters.
- [ ] CLV join INNER — no countable samples means absent, not zero.
- [ ] Tie-break chains of §8.6, terminating in `user_id`.
- [ ] `wagers.placed_at` for the wager half, `wager_leg_clv.graded_at` for the CLV half.

---

## Related documents

- **ADR 0011** — the phase-9 decisions and their costs.
- **ADR 0006** — fair value from one sharp reference book, devigged with Shin.
- **ADR 0003** — the odds provider, and the synthetic generator phase 9 is tested against.
- `migrations/00009_analytics.sql` — the schema, and the authority on every constraint.
- `internal/platform/postgres/queries/analytics.sql` — the queries, and the authority on the
  closing-price statement.
- `internal/analytics/` — the signals stage: `ev.go`, `arb.go`, `payload.go`, `validate.go`.
- `internal/analytics/steam/` — the detector, its `Config`, and the derivations behind every
  threshold.
- `internal/analytics/clv/` — the closing-price join, the reason enumeration, and the two
  lookbacks.
- `internal/settlement/clv.go` — the pass that drives the CLV work queue.
- `internal/domain/odds/clv.go` — the CLV arithmetic and its own phase-12 contract section.
- `internal/pricing/arbitrage.go` — the arbitrage detector and its (looser) thresholds.
- `internal/platform/kafka/topics.go` — the topic registry, including the `signals.ev`
  addition.
- `CLAUDE.md` §3, §6, §11 — the charter this all answers to.

---

*Where this document and the code disagree, the code is right and this file is wrong. Fixing
the document is not optional — it is the artefact phase 12 reimplements from.*
