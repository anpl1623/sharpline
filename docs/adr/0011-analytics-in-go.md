# ADR 0011: Analytics in Go — the signals stage on `pricer`, CLV on `settle`, retention-based `signals.*` topics, `scheduled_start` as the close, and a board ranked on ROI and CLV

- **Status:** Accepted
- **Date:** 2026-08-20
- **Charter reference:** CLAUDE.md §3, §6, §9, §11, §12

## Context

Phase 9 builds what CLAUDE.md §6 calls the differentiator: the positive-EV finder, the
arbitrage scanner, steam-move alerts, CLV tracking, and a public leaderboard. On the face of
it this is the easiest phase in the roadmap, because almost none of the mathematics is new.
Phase 1 shipped four devig methods, expected value, edge, Kelly and a complete CLV
implementation as pure functions with property-based tests. Phase 4 shipped fair value from a
sharp reference book (ADR 0006), per-quote EV and Kelly on every `QuoteAssessment`, and
cross-book arbitrage and middles detection already published on `price.computed`.

What phase 9 actually has to decide is everything *around* the arithmetic — and every one of
those decisions is the kind that gets made implicitly by whoever writes the first line, and
then cannot be changed.

**This phase's output is a cross-language contract, not a feature.** CLAUDE.md §11 row 9 says
in as many words that phase 9 "is the reference implementation phase 12 validates against",
and §3 says the Flink SQL jobs "replace that implementation" under the check "same inputs,
same outputs, or the Flink job is wrong". A threshold nobody wrote down, a window boundary
that is closed at one end for no stated reason, a sort with no tie-break, a row that is
sometimes absent and sometimes zero — each of those is a place where two implementations
disagree while both compile, both pass their own tests, and neither reports an error. In
eighteen months somebody has to reproduce this in SQL from the documentation. That obligation
shapes every decision below, and it is why `docs/analytics-semantics.md` is a deliverable of
this phase rather than a courtesy.

**Four questions were open and none of them is a preference.**

*Where does analytics run?* CLAUDE.md §3's service table has exactly six binaries. A seventh
called `analytics` is the obvious shape and would contradict the charter on its first line.

*Do findings go on the bus at all?* §3's event-flow diagram draws `signals.steam |
signals.arb | signals.clv` coming out of the **phase-12 Flink jobs** — not out of phase 9. A
reading of that diagram in which phase 9 writes only to Postgres is defensible, and it would
make the phase-12 cutover a rewrite with no reference output to diff against.

*What is "the closing price"?* This is the real work of the CLV feature and it has no obvious
answer. `odds.EvaluateCLV` already fixes what may be compared — same market, same outcome set,
same line, close not before the take — but it takes two `FairMarketSnapshot` values as given
and says nothing about where they come from. "The last price before kickoff" is wrong in at
least three distinguishable ways, and each of them produces a plausible number rather than an
error.

*What does the leaderboard rank on?* §6 says "on ROI and CLV, not raw profit", which settles
the measure and settles nothing else: the exclusion rules, the minimum sample, and the
tie-break are what decide whether a high-stake losing bettor can outrank a low-stake sharp
one.

A fifth question is new work rather than a decision about existing work. **Steam detection is
the one genuinely new detector in this phase**, and the naive implementation — threshold the
line-movement velocity — fires on a single book's tick, on a book correcting its own error,
and on any longshot whose decimal odds moved a tenth of a point. The synthetic provider was
built to make the correct implementation possible, and the parameters it was built with are
the ground truth a test can assert against.

## Decision

### D1 — the analytics live on the services that already hold their inputs; there is no seventh binary

- **`pricer` gains a signals stage** — +EV emission, arbitrage emission, steam detection. It
  already consumes the market stream, already computes `ExpectedValuePercent`, `Kelly` and
  `FractionalKelly` on every `QuoteAssessment`, and already emits `ArbitrageRef` on every
  `ComputedMarket`. This is the seam with the inputs; anywhere else would mean re-deriving
  them.

  **The stage is a SECOND CONSUMER of `price.computed` inside the same process, not a hook
  inside the pricing pass**, and that is forced rather than chosen. `internal/pricing`
  requires `PriceFunc` to be a **pure function of the record**, because the pricer suppresses
  a republication whose input fingerprint has not changed and that suppression is sound only
  if two calls over one record produce one answer. A detector that wrote to Postgres and to
  Kafka from inside that call would break the purity the whole change-detection path rests on.
  Consuming the pricer's *output* costs one extra decode of a record already in the page
  cache, and buys a second property that matters more: the analytics stage can be lifted into
  its own deployment later without touching a line of it, because it depends on a topic rather
  than on a function call.

  ```
  odds.normalized ──▶ pricing.Service ──▶ price.computed ──▶ analytics.Service
                                                                ├─▶ signals.ev
                                                                ├─▶ signals.arb
                                                                ├─▶ signals.steam
                                                                └─▶ Postgres
  ```
- **`settle` computes CLV per graded leg.** This is not a preference. `odds.CLVResult`'s own
  doc comment, written in phase 1, says "the settle service writes one per graded leg, the API
  serves it, and the phase-12 Flink job reproduces it." `settle` is also the only service that
  knows a leg has been graded, which is the event that makes the measurement possible.
- **`api` serves the query surface and the leaderboard.** Read path only. It computes nothing
  a detector did not already write, which is what keeps the board reproducible: a leaderboard
  that recomputed CLV at read time would rank users on a number nothing had recorded.

A service whose reason for existing is "the analytics go somewhere" is a service a reviewer
asks about and gets no good answer for. The same argument ADR 0001 makes against a JVM service
that exists to name-drop Java applies here to a binary that exists to name a phase.

### D2 — findings are published to four retention-based `signals.*` topics, and `signals.ev` is an addition to the charter

Four new topics, declared in `internal/platform/kafka/topics.go` **and** in
`deploy/terraform/modules/kafka-topics`:

| Topic | Key | Retention | Partitions | `retention.ms` |
|---|---|---|---|---|
| `signals.ev` | `market_id` | delete | 3 | 7 days |
| `signals.arb` | `market_id` | delete | 3 | 30 days |
| `signals.steam` | `market_id` | delete | 3 | 30 days |
| `signals.clv` | `wager_id` | delete | 3 | 90 days |

**Phase 9 publishes them, not phase 12.** Producing the reference output on the same bus the
Flink jobs will produce on is what makes the phase-12 swap a genuine like-for-like
replacement: two implementations, one topic each, one diff. A phase 9 that wrote only to
Postgres would leave phase 12 with nothing to be validated against except a table it had also
been asked to write.

**`signals.ev` is not one of the three names in CLAUDE.md §3's diagram, and it is flagged as
an addition** — in the topic registry's package comment, in `variables.tf`, and in both
Terraform READMEs. §6's Analytics bullet leads with "Positive-EV finder against a sharp
reference book", and phase 9 needs it as a first-class signal for exactly the reason the other
three are: a +EV finding is an event that has to reach a subscriber, be recorded, and be
replayable. Leaving it off the bus would make the +EV finder the one analytic phase 12 could
not replace like for like.

**None of the four is compacted, and that is the load-bearing half of this decision.**
Compaction keeps the latest record per key, which is meaningful only when the latest record
*supersedes* the earlier ones — true of a market's current line, which is why
`odds.normalized` and `price.computed` are compacted, and false of a finding. "The latest
steam move for market X" is not a snapshot of anything; it is one event, and the one before it
is a different event that also happened. Compacting these would do to the signal history
exactly what §3 says compacting `wager.events` would do to the settlement audit trail —
destroy it invisibly, because the head of the log still looks right.

**Three partitions, not six.** The co-partitioning argument that sets `odds.normalized` and
`price.computed` at six does not extend: signals are join *outputs*, and a sink's partition
count does not affect an upstream shuffle. Volume is thresholded output, and the consumer is
one low-concurrency group.

**`signals.clv` is keyed by wager, the only one of the four that is not keyed by market.** A
CLV record is a fact about a wager, so keying it by `wager_id` co-partitions it with
`wager.events` and keeps a wager's placement, settlement and CLV ordered relative to one
another — which is exactly what a consumer building a user's record does. Keying it by market
would scatter one wager's legs across partitions with no ordering between them.

`signals.steam`'s **bus** key is the market even though its **table** key is
`(market, selection, window)`. The finer key belongs where rows are stored; the coarser one is
where ordering is bought.

### D3 — the close is `events.scheduled_start`, at the leg's own book, over a complete and coherent outcome set, with suspended quotes excluded

> The **closing snapshot** of market `M` is: for **every** selection `s` of `M`, the price at
> the book the leg was struck at for `s` with the **greatest** `observed_at` satisfying
> `observed_at <= as_of` **and** `observed_at > not_before` **and** `observed_at` not inside
> any suspension episode of `M`. It is a closing snapshot **only if** every selection yielded
> such a price **and** every one of them agrees on the market line.

Six parts, each of which is a decision:

1. **`as_of` is `events.scheduled_start`** — not the actual kickoff and not the instant the
   market's status changed. It is the only candidate that is knowable before the event, stable
   under replay, and identical in Go and in Flink; an actual-kickoff timestamp derived from a
   live clock feed is none of those three. It is also what "closing line" means in the
   literature. A market has **no** usable close when `scheduled_start` is unset or the event
   is `scheduled`, `postponed` or `unknown` — `postponed` is the case that matters, because
   its start has moved into the future and reading a close there would score the wager against
   whatever the market happens to be quoting now.
2. **`not_before` is required**, and there are **two** of them —
   `ClosingLookback` and `TakenLookback`, both 24 hours by default and both exported so phase
   12 can be handed the same numbers. Each is a chunk filter — `prices` has no retention
   policy, so an unbounded lower bound consults every chunk that ever existed — and equally a
   semantic bound: a quote from six days before kickoff is not a closing line, it is a market
   nobody has priced since.
3. **One book, and it is the book the wager was struck at.** `NewFairMarketSnapshot` refuses
   probabilities that do not sum to 1 within `CLVDevigTolerance`, so a best-price-per-book
   mosaic is not devigable and is not a close — that settles *one* book. **Which** book is the
   real decision, and `odds/clv.go` explicitly permits either, describing a sharp reference
   book as the standard construction. That construction is **declined**: a reference book
   quotes its own line, so scoring a customer's `−3` against a reference close of `−3.5` is a
   *line move*, and `AggregateCLV` excludes every line-moved sample. Scoring cross-book would
   therefore drop most spread and total legs out of every aggregate, because two books
   disagreeing by half a point is the ordinary state of a market. Same-book scoring keeps a
   line move meaning **the market moved**, and measures the number the customer could actually
   have had.
4. **A suspended market's stale quote is not a close.** Quotes observed inside a suspension
   episode are excluded by an anti-join against `market_suspensions`, half-open on
   `suspended_at <= t < COALESCE(lifted_at, ∞)`. `markets.status` is deliberately not
   consulted: it is current state and says nothing about what was true at `as_of`. This one
   predicate handles suspended-and-reopened with no special case, and handles
   suspended-and-never-reopened by correctly falling back to the last price at which the
   market was actually open.
5. **Completeness and coherence are both required, and both are the caller's check.** An
   incomplete snapshot is **discarded whole** — not devigged as a subset, and never backfilled
   from another book. A snapshot whose selections do not all agree on the market line, once
   every quote is converted into the market's frame, is **refused rather than resolved**:
   holding the home side at `−3.5` and the away side at `+3` is not a market, and picking one
   of them would need a rule that would itself have to be reproduced.
6. **No tie-break is needed.** `prices_natural_key_idx` is UNIQUE on `(selection_id, book_id,
   observed_at)`.

**The same statement builds the taken snapshot**, called with the leg's own book and
`price_observed_at`, bounded by `TakenLookback`. One rule for both sides is what makes the two
inputs `EvaluateCLV` compares comparable in the first place; two rules would differ in a way
that shows up as a systematic bias rather than as an error. The taken side carries **one extra
requirement**: the quote found for the leg's own selection must be the leg's own quote, its
`observed_at` equal to `price_observed_at` exactly. A mismatch means the reconstruction
describes a market the wager was not struck in.

**One devig method devigs both sides**, defaulting to Shin to match `internal/pricing` — a
user's +EV signals and their CLV are statements about the same fair probability, and two
margin models would let the surface call a price +3% EV and then score it as having lost value
on a line that never moved. If **either** side refuses the configured method, **both** are
recomputed with multiplicative and the row records it. Both sides are devigged in
**selection-id order**, because the four methods are order-independent in exact arithmetic and
not bit-independent in float64.

**An in-play wager has no CLV under this definition, deliberately.** A bet struck after
kickoff has a placement instant after the close, so the pass reports `close_before_take` and
writes no row. Closing line value is a claim about the *pre-game* market's final estimate; an
in-play price is conditioned on a scoreline and a clock and answers a different question.
Measuring one against the other would produce a number that looks like CLV, ranks like CLV,
and means something else.

**Absence is meaningful and is never a row of nulls.** No `wager_leg_clv` row is written when
the closing snapshot was incomplete, the outcome set changed, the close would precede the
take, every candidate quote was inside a suspension, or the event never started. A missing row
says "this leg has no measurable closing line value", which is a different and honest claim
from "this leg's CLV was zero".

**A line move is display-only.** `EvaluateCLV` refuses it; `EvaluateCLVAcrossLineMove`
computes it and stamps `LineMoved`; `AggregateCLV` and the leaderboard both exclude every such
sample. **Nobody is ranked by it.**

### D4 — steam is a magnitude threshold on raw implied-probability movement over an epoch-aligned hopping window, corroborated across books and closed by a watermark

Six properties, and none of them is optional.

**1. It is detected on implied probability, never on decimal odds.** Decimal odds are
nonlinear in probability, so a fixed decimal threshold means different things at different
prices: a 0.10 move in decimal odds is 0.042 probability points at `d = 1.50` and 0.001 at
`d = 10.00`. A threshold in decimal odds is forty times stricter on a longshot than on a
favourite, for no reason anyone would defend, and a board scanned with one would report steam
on every short price and none on any long one. `steam_signals` names the unit in the column,
in both directions: `delta_probability` and `velocity_probability_per_minute`.

**2. It uses RAW implied probability, margin included, and records `devig_method = "none"`.**
The cost is real and is stated: a book that widened its market without changing its opinion
shows a move. In practice a book's overround is close to constant over a three-minute window —
it is a business setting, not a price — so it cancels almost exactly in a *delta*, which is
the only thing this detector looks at. The benefit is decisive: devigging needs a book's
**complete outcome set at one instant**, and a book that has refreshed one side of its market
and not the other is *exactly* the book whose lag carries the signal. Devigging would drop it
from the window at the moment it became interesting.

**3. The magnitude threshold is the primary discriminator, and the correlation statistic is
NOT.** This is the part most likely to be misremembered, so it is stated flatly: on a feed
where every book views one latent process — which is what the synthetic generator builds and
what a real market approximately is — **ordinary drift is also correlated across books**,
because the books are all looking at the same thing. `cross_book_correlation` therefore does
**not** separate steam from drift. What it separates is a genuine market move from **one
book's tick rounding or its persistent per-event bias moving alone**, which is the commonest
way to manufacture a phantom signal from a single soft book. The separation between steam and
drift is done by `MinMagnitude`: a steam move is a discrete jump of 0.4σ–1.5σ landing inside
one 10-second step, where drift over a three-minute window is a fraction of that.

**4. The window is hopping, half-open, and aligned to the Unix epoch in UTC.** Window *k* is
`[k·Hop, k·Hop + Window)` counted from `1970-01-01T00:00:00Z` — **not** aligned to the first
observation this process saw, which would make the answer depend on when the consumer started,
and **not** to local midnight, which would make it depend on a time zone. Flink aligns its
windows to the epoch too, so the two agree by construction rather than by coincidence.
Hopping rather than tumbling because a jump straddling a boundary otherwise shows as two
half-moves, neither of which clears the threshold. The natural key contains **both** bounds,
because one `(market, selection)` pair legitimately has several overlapping windows in flight
and they are different findings.

**5. Windows are closed by a WATERMARK, not a timer.** Each market's watermark is the greatest
observation instant seen for it so far minus `AllowedLateness`, monotone non-decreasing; a
window is evaluated exactly once, when the watermark passes its end. Nothing reads a wall
clock, which is what makes a replay produce the same findings as the original run. A
late-arriving observation is dropped and counted; it cannot reopen an evaluated window,
because admitting it would make a window's answer depend on when the reader asked.

`AllowedLateness` is the parameter that makes the detector work or makes it blind, and its
failure mode is invisible: too small, and the lagged books are absent from every window, the
correlation is computed over one book, and nothing ever fires — **which looks exactly like a
quiet market.**

**6. It is directional.** The natural key includes `selection_id`, and the cooldown is keyed
on `direction` as well. Keying by market alone collapses the two sides of a two-way market
into one row and silently drops half the findings.

**The parameters, and where they come from.** The synthetic provider was built to make this
detectable, and its generator constants are the ground truth the thresholds are set against:
model step 10 s, book view lags 0/20/40/70/90 s, steam jumps of 0.4σ–1.5σ landing in one step
and holding full amplitude for three hours, and a 30-second post-steam suspension deliberately
shorter than the deepest book lag.

| Parameter | Value | Derived from |
|---|---|---|
| `Window` | 3 min | The **tighter of two bounds, not a compromise**: at least two observations per book needs ≥ 180 s at ADR 0003's 90-second cadence, and every second past that admits more drift. |
| `Hop` | 1 min | `Window/Hop = 3`, so a boundary-straddling move is caught whole. |
| `AllowedLateness` | 3 min | 90 s deepest book lag + 90 s poll cadence. |
| `MaxFollowerLag` | 2 min | Must exceed the deepest book's 90 s lag; must stay inside the window. |
| `Cooldown` | 5 min **event time** | One jump appears in 3 windows; propagation stretches it to 4–5. |
| `MinMagnitude` | 0.030 prob. points | Derived per league from `φ(0)·lineSD/resultSD`, which lands within a factor of two across all four leagues; a typical steam move is 0.03–0.05 and drift over 3 min is well under it. |
| `MinVelocity` | 0.010 pts/min | `MinMagnitude / 3` at the default window, hence **redundant by construction** — and kept because the two stop being redundant the instant the window length changes. |
| `MinCorrelation` | 0.5 | With five books, at least three net books agreeing with the lead. |
| `MinFollowers` | 1 | A move nobody follows is one book's move. Not raised, because the deepest-lagged books may not have reported in the window at all. |
| `NoiseFloor` | 0.005 pts | Above one tick on a 10-cent American grid, below the magnitude floor. Without it a quiet book contributes `±1` at random and the correlation is a coin flip weighted by rounding. |

Every one of them is **stored on every row**, and `steam_signals_meets_own_thresholds` makes
"this row meets the thresholds it claims" a database fact rather than a promise. Full
derivations are in `docs/analytics-semantics.md` §6.7.

**Both directions are gate items.** The detector must fire on a generated steam move and must
**not** fire on ordinary drift, and both are asserted against the seeded generator rather than
against a fixture.

### D5 — the board ranks on ROI and CLV, gated by two minimum-sample thresholds, and never on profit

- **ROI** = `SUM(net_return_minor) / SUM(stake_minor)` over settled wagers. Stake-normalised,
  so betting bigger cannot improve it. **A high-stake losing bettor cannot outrank a low-stake
  winning one, structurally, at any sample size** — that is the gate, and it is a property of
  the measure rather than of a filter.
- **CLV** = the **unweighted** mean of `percent_clv` over countable rows. Unweighted for
  `AggregateCLV`'s stated reason: stake-weighting would let a bettor buy leaderboard position
  by sizing up.
- **Which wagers count:** `won | lost | push | cashed_out`. `placed` and `open` are
  unresolved. **`void` is excluded from numerator and denominator alike** — a void had no
  action, its net return is pinned to zero by `wagers_return_matches_outcome`, so including it
  would leave the numerator unchanged while inflating the denominator and pulling every ROI
  toward zero in proportion to how many of a user's wagers the book cancelled. That is
  turnover that never happened, and it matches `odds.CLVSample.Void` exactly, so both halves
  of the board apply the same rule to the same events. **A push is not a void and does
  count.**
- **Two minimum-sample thresholds**, `@min_settled_wagers` and `@min_clv_samples`, both
  parameters with no in-query default so the API can show them next to the board. Two rather
  than one because the measures have different denominators: a user with fifty settled wagers
  and three countable CLV rows must not be ranked on a three-sample mean.
- **The CLV join is INNER.** A user with no countable CLV samples is **absent** from the
  board, not present with a zero — the same choice `AggregateCLV` makes by returning
  `ErrCLVNoSamples`.
- **Tie-breaks are total.** ROI board: `roi DESC, mean_percent_clv DESC, settled_wagers DESC,
  user_id ASC`. CLV board: `mean_percent_clv DESC, roi DESC, clv_samples DESC, user_id ASC`.
  Each ranks on its own measure and breaks with the other, then by sample count, then by a
  primary key.
- **No column of `users` is selected except `u.id`.** This is a public board and `users`
  carries only an email address.

### D6 — every derived table is replayable, keyed on input-derived values, and corrected by upsert

`prices` and `ledger_entries` are immutable by trigger because they are **observations**.
Everything phase 9 writes is a **derivation** — a function of those observations plus a set of
parameters — so nothing in migration 00009 is append-only. Re-run a detector over the same
inputs with the same parameters and the same row comes back; re-run it with a *corrected*
detector and a corrected row must land.

| Table | Natural key |
|---|---|
| `ev_signals` | `(selection_id, book_id, quote_observed_at)` |
| `steam_signals` | `(market_id, selection_id, window_start, window_end)` |
| `arbitrage_signals` | `(market_id, observed_at, legs_fingerprint)` |
| `wager_leg_clv` | `leg_id` |

Three rules follow, and all three are load-bearing:

1. **`ON CONFLICT … DO UPDATE`, never `DO NOTHING`.** A replay after a fix *is* the
   correction. `DO NOTHING` would silently preserve the bug it was run to remove.
2. **No natural key contains a clock reading of ours.** `detected_at` and `computed_at` are
   stored and are never part of an identity, a window bound, a filter or an ordering. A key
   containing `detected_at` would make every replay INSERT a duplicate, and the table would
   grow a fresh copy of every finding each time a consumer group's offsets were reset.
3. **Partitioning is on event time.** A hypertable's unique index must contain the
   partitioning column; combine that with rule 2 and the partitioning column is *forced* to be
   an event-time column. That is why `ev_signals` partitions on `quote_observed_at` and
   `steam_signals` on `window_end`, and it is also what keeps the Go and the Flink
   implementations in one time domain so a diff can compare them directly.

This is also what makes **lateness** need no special path: a late arrival re-evaluates its
window and upserts. Correction is the normal path, not the exception.

## Consequences

**What this makes easy.**

The phase-12 cutover becomes a diff rather than a rewrite. Two implementations write to the
same four topics and the same five tables, keyed identically on values derived from the input
alone, so "same inputs, same outputs" is something a script can check instead of something a
reviewer has to believe. Recomputing a corrected detector over a replayed topic lands
corrections in place, which is exactly the operation the cutover consists of.

Every finding is self-describing. A row that says "+EV by 2.4%" also says which reference
book, which devig method, which threshold was in force, and how old the quote was. That is
what makes a finding auditable eighteen months later by someone who does not have the
configuration that produced it — and it is the difference between a reference implementation
and a program that once produced some numbers.

The staleness discipline is a database fact rather than a convention.
`arbitrage_signals_within_own_bounds` and `steam_signals_meets_own_thresholds` mean a row that
violates its own thresholds cannot be stored, so the discipline survives a refactor of the
detector that wrote it.

**What this makes hard, and the costs accepted knowingly.**

*`pricer` now does three jobs, and its autoscaler measures one of them.* CLAUDE.md §9 puts an
HPA on `pricer` **on CPU**. The +EV and arbitrage stages are CPU-shaped and fit that. Steam
detection is not: a hopping window over cross-book quotes is *memory*-shaped, and its cost
scales with the number of markets in flight rather than with the record rate. The autoscaler
will therefore be measuring the wrong signal for the newest work on the service. This is
accepted rather than solved — a custom metric for steam would be a second HPA on a service
that already has one — and it is named here so that the first time a `pricer` replica is
OOM-killed rather than CPU-throttled, nobody spends a day on it thinking it is a leak.

*Steam window state does not survive a rebalance, and the pricer's engine must stay pure.*
`internal/pricing/doc.go` makes "the engine is a pure function of the record" a **requirement**
of the service seam, because the suppression logic depends on it. Steam windowing is by
definition stateful, so it lives outside `Engine.Price` rather than inside it. The consequence
is that a consumer-group rebalance moves a partition to a replica with no window history, and
that replica evaluates the first window or two on partial data. The mitigation is D6 and
nothing else: the finding is keyed on `(market, selection, window_start, window_end)`, so
re-evaluating the window later corrects the row in place rather than duplicating it. **A
transient wrong answer at a rebalance boundary is the accepted cost**, and it is acceptable
only because the correction path exists.

*A market with one quoting book can never produce a steam signal.* `participating_books >= 2`
is structural. A market that unmistakably steamed at the only book quoting it produces
nothing, and there is no configuration that changes that. This is the same shape of coverage
loss ADR 0006 accepted for fair value, taken for the same reason: a detector that fires
without corroboration is a detector nobody can trust, and half the point of steam is that it
is corroborated.

*The correlation statistic carries less weight than its name suggests, and the document says
so.* It would be comfortable to describe steam detection as "correlated movement" and stop
there. It would also be wrong: books that view one latent process are correlated whether the
market steams or drifts, so `cross_book_correlation` screens out a lone book's tick rounding
and does **not** discriminate steam from drift. The discrimination is carried by
`MinMagnitude` alone, which means the detector's quality rests on one number derived from a
generator — and that is exactly the number a real provider is most likely to invalidate. It is
stored on every row so the re-derivation is visible in the data.

*`MinVelocity` is redundant at the default window and is kept anyway.* It equals
`MinMagnitude / 3` exactly at a three-minute window, so it binds at the same place and adds
nothing today. Two thresholds expressing one constraint is a smell, and the justification is
narrow: they stop being redundant the instant the window length changes, which is precisely
when a stored population of findings spanning a re-tuning would otherwise become
uninterpretable. Both are stored on every row.

*`AllowedLateness` has an invisible failure mode.* Set it too small and the lagged books never
make it into a window, the correlation is computed over one book, and the detector reports
nothing — which is indistinguishable from a quiet market. Nothing in the system alerts on it.
The mitigation is the derivation being written down (90 s deepest book lag + 90 s poll
cadence) rather than the number being tuned by feel, and the parameter travelling on every
finding so a population produced under a wrong value can be identified afterwards.

*The steam detector is tuned against a generator, not against a market.* The thresholds in D4
are set against the synthetic provider's known lag structure — five books staggered at exactly
0/20/40/70/90 seconds by construction. A real provider's books are not staggered by
construction, their lags vary with market and with time of day, and the follower detection
will be materially noisier. That is a re-tuning job, not a redesign, and the thresholds are
stored on every row precisely so the re-tuning is visible in the data rather than buried in a
config change nobody recorded. But the honest statement today is: **this detector is known to
work against the generator and is unvalidated against a real feed.**

*The +EV surface inherits ADR 0006's coverage gap in full.* A market with no eligible
reference book has no fair value, and therefore no +EV signal, however good the prices look.
The gap is already visible on `sharpline_pricer_fair_value_total{result}`; phase 9 makes it
visible as an empty analytics surface too.

*Middles are computed and not persisted.* `internal/pricing/middles.go` runs on every record
and `ComputedMarket.Middles` carries the result to `price.computed`, but there is no
`middle_signals` table and no `signals.middles` topic. CLAUDE.md §6 names arbitrage-and-middles
as one capability, and this decision splits them. The reason is that they are different
objects: an arbitrage is an *event* with a guaranteed return, a middle is a *position* whose
value depends on a hit probability nothing in this system estimates, and `MiddleRef`'s own
`BreakevenHitProbability` is explicitly "a THRESHOLD, not a forecast". Persisting a middle
would mean storing a breakeven rate next to no estimate to compare it against. §11's phase 9
asks for no middle *alert*, so this is defensible — but it is a partial delivery of a charter
bullet and is recorded as one. Adding `middle_signals` later touches nothing decided here.

*The close is `scheduled_start`, so a delayed event's "closing line" precedes its actual
kickoff.* An event that starts forty minutes late has a close measured forty minutes before
the first ball, during which the market kept trading. The number is still the standard
definition of closing line and is still reproducible, which is what it was chosen for — but it
is not "the last price before play began", and anyone reading a CLV figure should know that.

*Every user is scored against their own book's close, so the CLV column of the leaderboard
compares people against different markets.* That is the direct cost of D3's same-book rule,
and it is real: a user betting a soft book with a wide margin is measured against that book's
own final estimate, not against the sharpest one available. The alternative was a mass
exclusion rather than a fix — scoring cross-book turns every half-point disagreement into a
line move and drops most spread and total legs out of every aggregate — so this is the lesser
of two distortions rather than the absence of one. The real fix is a consensus close over
several sharp books, which is a model rather than a query and belongs behind its own ADR.

*A visible share of graded legs will be unmeasurable, permanently.* In-play wagers have no
close under D3 by construction; a market that shut early and never reopened has none either;
and a field that lost a runner has a changed outcome set. Each produces a counted reason
rather than a row. On a system with live betting enabled this share is not small, and the
metrics are labelled by reason precisely so that "the CLV coverage dropped" can be attributed
rather than investigated from scratch.

*A new user is invisible on the leaderboard, by construction.* The two minimum-sample
thresholds mean a genuinely sharp bettor with fifteen wagers does not appear at all. A
threshold is also statistically cruder than a confidence interval; a Wilson or bootstrap
interval is the better answer and is unexplainable on a public page. The crude answer was
chosen for legibility and the cost is real.

*A per-user CLV mean computed in SQL is not the one `AggregateCLV` computes.* PostgreSQL's
`avg` is a naive running sum where `odds.AggregateCLV` uses Kahan–Babuška–Neumaier
compensation. At one user's sample count the difference is far below display precision, and
the tie-break chain terminates in a primary key so equal rows never swap. It is nonetheless a
divergence between two implementations of one statistic, it is recorded in the query's own
comment and in `docs/analytics-semantics.md` §8.7, and the fix — hand the rows to
`AggregateCLV` in Go — is named rather than left to be rediscovered.

*The steam threshold was calibrated on the wrong population, and the finding does not say so.*
`MinMagnitude = 0.050` was swept over **moneyline** markets, where a line change is impossible
by construction, so it measured pure price movement. The shipped detector runs on every market
type and deliberately does not segment its series by line — a handicap moving from `-1` to
`-0.5` at every book is the canonical steam move, and a line-segmented detector would report
nothing at all for it. The consequence is that on a lined market a single half-point step is
already two to three times the threshold. Measured against the live synthetic feed, **273 of
284 findings were windows in which the lead book's own line changed** (141/144 spreads, 132/132
totals, 0/8 moneylines) and the detector fired on **1.55% of evaluated windows** against the
0.016%–0.225% the moneyline rig reports. `steam_signals` has no line column and no flag for it,
so the two populations cannot be separated without going back to `prices`. The follow-up is
named in `docs/analytics-semantics.md` §6.2b — a `lead_line_changed` column and a
per-market-type `MinMagnitude` — and both are additive. This was found by the phase-9 gate
against the running system; a moneyline-only rig could not have found it, which is the argument
for the gate running against live data rather than against fixtures.

*A refused write has three meanings, not two, and the third is not this stage's fault.* Every
foreign key on the phase-9 tables points at the catalogue, which `ingest` writes and the signals
stage only reads, and nothing orders those two consumers of `odds.normalized` against each other.
A market can therefore be priced, published and read here in the instant before its `markets`
row commits — worst on a cold start, where a fresh consumer group replays the whole compacted
topic against an empty catalogue. `internal/analytics` cannot pre-validate it: it validates
findings against the CHECK constraints, and a finding derived from one `price.computed` record
carries no evidence about what has been committed. The stage therefore classifies `23503`
separately from `23514`, retries it, and — if the parent still has not landed — counts the record
`deferred` and advances over it rather than returning an error that `ErrorPolicySkip` discards
anyway while the log promises a redelivery that never comes. The finding is recovered by the
market's next reprice. The first cold start after phase 9 landed logged 109 records as database
errors on exactly this path.

*Four more topics and two more hypertables to operate.* `total_partitions` in the Terraform
module goes from 21 to 33, and neither new hypertable has a retention policy, which is why
every read of them carries a required lower time bound. The absence of a retention policy is a
deliberate deferral, not an oversight: retention on a signal table is a product decision about
how much history the analytics surface shows, and it should be made when there is history to
look at.

## Alternatives considered

### D1: a seventh binary, `cmd/analytics` — rejected

It is the obvious shape and it contradicts CLAUDE.md §3's service table on its first line. It
would also have to re-consume `price.computed` and re-derive everything `pricer` had already
computed one hop earlier, which is a second implementation of the pricing pass in all but
name. The charter's own argument against a JVM service — "a service that exists to name-drop a
language is worse than no service" — applies unchanged to a service that exists to name a
phase.

### D1: CLV on `api`, computed at read time — rejected

Tempting because it removes a write path and a table. Rejected because it makes the
leaderboard rank users on a number nothing recorded: two reads at different times would
produce different CLV for the same graded leg as `prices` chunks aged past the lookback, and
there would be nothing for phase 12 to diff against. `odds.CLVResult`'s doc comment also
assigns it to `settle` explicitly, and re-deciding that in phase 9 would leave two documents
disagreeing.

### D2: write findings to Postgres only, and let phase 12 introduce the topics — rejected

This is the reading of §3's diagram in which `signals.*` are phase-12 artefacts. It was
rejected because it makes the validation the phase exists for impossible: phase 12's job would
publish to a topic phase 9 never wrote, so "same inputs, same outputs" could only be checked
against a table, by a second piece of code written for the purpose. Publishing now costs four
topic declarations and buys a diff that needs no new machinery.

### D2: compact the `signals.*` topics — rejected

Superficially attractive because every other derived topic in this system is compacted, and
because a compacted topic gives a consumer a free snapshot. It is wrong for the same reason it
would be wrong on `wager.events`: compaction is a claim that the latest record per key
supersedes the earlier ones, and a finding supersedes nothing. The failure mode is the bad
one — the head of the log still looks correct, so nothing reports that history has been
deleted.

### D2: key `signals.steam` by `(market, selection)` — rejected

It matches the table's natural key, which is the argument for it. Rejected because a topic key
buys **ordering**, and the ordering worth having is "all of one market's signals in
production order" — a consumer rendering a market's activity wants both sides of it
interleaved correctly. The selection travels in the payload and costs nothing there. The
inverse mistake, keying the *table* by market alone, would genuinely lose data and is refused
by the schema.

### D3: define the close as "the last price before kickoff", with no further conditions — rejected three times over

Rejected once for **suspension**: the last price before kickoff on a market suspended twenty
minutes earlier is a frozen quote nobody could have bet, and scoring a wager against it
reports value that never existed.

Rejected again for **completeness**: taking the last price per selection across whatever books
happened to quote them produces a set that cannot be devigged, and `NewFairMarketSnapshot`
would reject it — loudly, but only after the caller had already decided it was a close.

Rejected a third time for **unboundedness**: with no `not_before`, a selection nobody has
priced in six days yields its six-day-old quote as a "closing line", and the query consults
every chunk `prices` has ever had in order to find it.

### D3: use the actual kickoff instant instead of `scheduled_start` — rejected

It is the more intuitive definition and it is what a bettor would say they meant. Rejected
because it is derived from the live clock feed, which means it is not knowable in advance, not
stable under replay, and not identical between the Go implementation and a Flink job reading
the same topics. A definition of "the close" that can change value on a re-run cannot anchor a
cross-language contract, and the cost — a delayed event's close precedes its first ball — is
visible and explainable where a replay-unstable definition is neither.

### D3: score the close at a sharp reference book rather than at the leg's own book — rejected

The standard construction, and the one `odds/clv.go` describes: take the price at the book the
wager was struck at and score it against a sharp reference book's close, exactly as ADR 0006
scores +EV. It is the more informative comparison in principle, because it measures the price
against the best available estimate rather than against the same book's later opinion.

Rejected on a mechanical consequence that swamps the principle. A reference book quotes its
own line, and two books disagreeing by half a point is the *ordinary* state of a spread or a
total rather than an exception. Every such disagreement would be reported by `EvaluateCLV` as
a line move, and `AggregateCLV` excludes every line-moved sample from the mean and the beat
rate — so the CLV surface would drop most spread and total legs out of every aggregate and
rank users on whatever moneylines survived. The flag would also stop meaning what it says: a
line move would mean "two books disagreed" rather than "the market moved", which is the
distinction the flag exists to draw.

The cost of the choice actually made is in the Consequences and is not disputed. What settles
it is that the rejected option does not remove that cost either — it replaces a comparability
problem with a coverage problem, and coverage is the one this project can least afford, having
already accepted ADR 0006's coverage loss on the +EV side.

### D3: renormalise across a line move so the sample can be aggregated — rejected

It would recover the samples `LineMoved` currently drops, which on spreads and totals is a
meaningful share. Rejected because converting between "home −3" and "home −3.5" requires a
distribution of game margins. That is a model, not arithmetic; it has no place in a pure
package; and a leaderboard whose CLV depended on an unvalidated margin model would be ranking
users partly on that model's errors. The indicative number remains available for a per-wager
display, which is the use it was written for.

### D4: threshold the line-movement velocity alone — rejected

The naive detector, and the one CLAUDE.md's phase-9 brief warns against by making both
directions a gate item. It fires on a single book's tick, on a book correcting its own error,
and — if measured in decimal odds — on any longshot that moved a tenth of a point. The
correlation requirement is what makes a steam signal mean "the market moved" rather than "a
book moved".

### D4: rely on cross-book correlation to separate steam from drift — rejected on the evidence

The intuitive story — steam is correlated, drift is not — is wrong on this feed and probably
wrong on a real one. The synthetic generator gives every book a lagged view of **one latent
process**, so drift moves the books together too; a real market has the same structure for the
same reason, because the books are pricing the same contest off overlapping information. A
detector that leaned on correlation for the steam/drift distinction would fire on any period
of sustained drift and would look like it was working.

The statistic is kept, with its job stated honestly: it screens out a **lone book's tick
rounding or persistent per-event bias**, which is the commonest way to manufacture a phantom
signal. The steam/drift separation is `MinMagnitude`'s, and putting the weight where it
actually falls is the difference between a threshold somebody can re-derive and a threshold
nobody can explain.

### D4: close windows on a processing-time timer — rejected

The simpler implementation, and it removes the watermark, the lateness parameter and the
per-market state that tracks them. Rejected because the output would then depend on how fast
the consumer was running: a replay of the same log on a busier machine would close windows at
different points and emit a different set of findings. That makes the phase-12 comparison
meaningless — the two implementations would differ for a reason that has nothing to do with
the SQL — and it makes the detector's own behaviour unreproducible from its inputs, which is
the property the whole phase is built around.

### D4: a tumbling window — rejected

Cheaper, and it needs no overlap bookkeeping. Rejected because a steam move that straddles a
boundary is split across two windows and can fall below the threshold in both, so the largest
single thing that happens to a line is exactly the case a tumbling window is worst at.
CLAUDE.md §3 says hopping, and the reason is this one.

### D4: detect on decimal-odds velocity — rejected

Rejected on the arithmetic in D4: a fixed decimal threshold is forty times stricter on a
longshot than on a favourite. The result would be a detector that reports steam almost
exclusively on short prices and calls the same relative move on a longshot noise.

### D5: rank on profit, or on profit with a stake cap — rejected

Rejected by CLAUDE.md §6 in as many words, and the reason is worth restating so it is not
re-argued: raw profit ranks by stake size and by variance, so the user who staked the maximum
on one coin flip and won tops the board, and the ranking then teaches the behaviour a
responsible-gaming-aware product should not. A stake cap does not fix it — it caps the
magnitude of the distortion without removing it, and it introduces a second arbitrary number
to defend.

### D5: include voided wagers in the ROI denominator — rejected

Defensible on the grounds that a void is "something that happened", and it is the behaviour
you get for free by filtering only on terminal status. Rejected because a void had **no
action**: `wagers_return_matches_outcome` pins its net return to exactly zero, so including it
leaves the numerator unchanged while inflating the denominator, and every ROI is pulled toward
zero in proportion to how many of a user's wagers the book cancelled — a number the user did
not influence. It would also put the two halves of the board in disagreement, since
`CLVSample.Void` excludes the same events from the CLV half.

### D5: exclude pushes — rejected

The mirror-image error, and the more tempting one, because a push "isn't really a bet". It is:
the price was taken and the market closed, and only the settlement returned the stake. CLV
measures the quality of the price rather than the result, so excluding pushes would make a
bettor's record depend on the scoreboard — the exact dependency CLV exists to remove — and
would bias both measures toward market types that cannot push.

### D5: rank with a confidence interval instead of a minimum-sample threshold — rejected, with regret

Statistically the better answer: a Wilson interval on beat rate, or a bootstrap on mean CLV,
would rank a fifteen-wager sharp bettor honestly instead of hiding them. Rejected because it
is unexplainable on a public page. "Minimum 25 settled wagers" is understood instantly and is
the convention every real leaderboard uses; "ranked by the lower bound of a 95% Wilson
interval" invites a support question from every user below the fold. The threshold is cruder
and it is legible, and the values are parameters so the choice is visible next to the board
rather than compiled in.

### D6: append-only signal tables, matching `prices` and `ledger_entries` — rejected

Consistent with the two tables that precede them, which is the argument. Rejected because it
confuses an observation with a derivation. A price happened; a finding is a *function* of
prices and parameters. Freezing findings would make a detector bug permanent in storage and
would make the phase-12 cutover — literally "recompute these rows with a different engine and
diff them" — impossible to perform in place. The immutability that matters is already
enforced, one layer down, on the inputs these rows are derived from.

### D6: a surrogate key plus an insert, deduplicated later — rejected

The shape you get by default from an ORM or from a naive sink. Rejected because it makes every
consumer-group offset reset grow a second copy of every finding, and because deduplicating
afterwards requires knowing the natural key anyway — at which point the natural key may as
well be the constraint. The one place a surrogate key survives is `arbitrage_signals.id`,
which exists solely so the legs child table has something to reference; the natural key is
still declared and is still the `ON CONFLICT` arbiter.
