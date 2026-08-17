# ADR 0003: The Odds API as odds provider, with a synthetic fallback

- **Status:** Accepted — **supersedes the deferral in CLAUDE.md §13.1**
- **Date:** 2026-08-16
- **Charter reference:** CLAUDE.md §0, §5, §13.1 (open decision #1)

> §13.1 of the charter lists the odds provider as deliberately undecided and asks that the
> decision be recorded "in an ADR with the quota math". This is that ADR. The author
> resolved the open decision on 2026-08-16: **prioritize a real provider, with the
> synthetic generator as the fallback path.** The remainder of §5 — build `ProviderAdapter`
> first, ship a synthetic provider behind it — is unchanged and still binding.

## Context

The charter's §0 success condition is real odds, ingested from a licensed data provider,
normalized, priced, and streamed live. Everything downstream — the Timescale hypertable of
line history, CLV, steam detection, book disagreement, the +EV finder against a sharp
reference book — is only interesting if the input is real. Synthetic data can exercise
every code path but it cannot make the analytics *mean* anything, because the generator
and the analyzer would share the author's assumptions about how lines move.

Three candidates were named in §13.1: **The Odds API** (cheapest, thinnest),
**SportsGameOdds**, and **OddsJam** (richest, priciest).

The binding constraint is not features. It is **quota economics against a polling
architecture**, because §5 mandates adaptive polling and §9 mandates that remaining quota
be exposed as a Prometheus gauge — the charter already anticipates that quota is a live
operational concern, not a signup detail. A provider whose cost model punishes frequent
polling makes the headline SLO (odds staleness) unaffordable, regardless of how good its
data is.

## Decision

**The Odds API (`https://api.the-odds-api.com/v4`) is the odds provider.**

`ingest` selects its adapter at startup:

| `ODDS_API_KEY` | Adapter |
|---|---|
| set | Real adapter, polling The Odds API within the configured quota budget |
| empty | Synthetic stochastic market-maker, seeded from `SHARPLINE_SYNTHETIC_SEED` |

Both sit behind the identical `ProviderAdapter` interface. Nothing downstream of `ingest`
can tell which is running. The synthetic path is the CI path, the offline path, and the
default for day-to-day development — the quota math below shows why that is a necessity
rather than a preference.

The synthetic provider is a **live generator**, not fixture data. It computes market
behaviour at request time from a seeded RNG. It is therefore not a violation of the
no-mock-data rule: an event on screen still travelled the whole real path
`provider → ingest → Kafka → normalizer → pricer → Postgres/Redis → api/stream → browser`.

### Why The Odds API and not the other two

- **Its cost model is legible and it is published.** Cost is an arithmetic function of
  markets and regions, and every response returns `x-requests-remaining`,
  `x-requests-used`, and `x-requests-last` headers. That last header means the token-bucket
  limiter §5 mandates can be reconciled against the provider's own accounting on every
  single call rather than estimated. A quota gauge fed by the provider's own number cannot
  silently drift.
- **There is a genuinely free tier**, which means the repository is usable by someone who
  clones it without a credit card. That matters for a project whose stated goal is being
  reproducible for anyone who looks at it.
- **It is the cheapest of the three at the tiers that matter**, and the thinness §13.1
  notes — fewer exotic markets, fewer books — is not a constraint at this scale. The
  charter's core board is moneyline, spread, and total across a handful of US books, which
  The Odds API covers on its base endpoint.
- OddsJam is richer and materially more expensive; its price only pays for itself with
  player-prop and low-hold-market coverage the charter does not require until well past
  phase 11. SportsGameOdds sits between the two without a decisive advantage on either
  axis.

**This decision is reversible by design and that is the point.** The `ProviderAdapter`
interface is the abstraction that makes swapping providers a single new implementation
plus a golden-file set, so choosing the cheapest legible option now costs nothing later.

---

## The quota math

**Pricing and cost model verified 2026-08-16** by fetching `the-odds-api.com` and
`the-odds-api.com/liveapi/guides/v4/`, corroborated against a third-party 2026 pricing
comparison. Prices are USD per month.

### Published tiers

| Plan | Price / month | Credits / month | Credits / day (30d) |
|---|---:|---:|---:|
| Starter (free) | $0 | 500 | 16.7 |
| 20K | $30 | 20,000 | 667 |
| 100K | $59 | 100,000 | 3,333 |
| 5M | $119 | 5,000,000 | 166,667 |
| 15M | $249 | 15,000,000 | 500,000 |

All tiers, free included, advertise access to all sports, all betting markets, and
historical odds. The tiers differ in credit allowance, not in capability — which is
unusually clean, and is part of why this provider is easy to reason about.

### Cost model

| Endpoint | Cost in credits |
|---|---|
| `GET /v4/sports` | **0** — free |
| `GET /v4/sports/{sport}/events` | **0** — free |
| `GET /v4/sports/{sport}/odds` | `markets × regions` |
| `GET /v4/sports/{sport}/events/{id}/odds` | `unique markets returned × regions` |
| `GET /v4/sports/{sport}/scores` | 1, or **2** with `daysFrom` |
| `GET /v4/sports/{sport}/participants` | 1 |
| `GET /v4/sports/{sport}/event-markets` | 1 |
| `GET /v4/historical/...` | **10 ×** `markets × regions` |

Four facts drive everything below:

1. **One `/odds` request returns every upcoming and live event for that sport.** Cost does
   *not* scale with the number of events. A 16-game NFL slate and a 1-game slate cost the
   same. This is enormously favourable to a full-board product and it is the single most
   important property of this provider's pricing.
2. **Cost is multiplicative in markets × regions.** Adding a fourth market to a two-region
   request costs two credits, not one.
3. **`/events` and `/sports` are free.** The event and league catalogue can be refreshed as
   often as we like at zero cost. Credits are spent only on prices.
4. **The `bookmakers` parameter is cheaper than `regions` for cross-region book coverage.**
   Every group of 10 named bookmakers counts as one region-equivalent. Naming ten specific
   books that span `us`, `us2`, and `eu` costs 1 region-equivalent where
   `regions=us,us2,eu` costs 3.

### Fixed assumptions

Stated explicitly so the arithmetic can be re-run when they change. These are **assumptions
about Sharpline's own polling schedule, not published provider facts**:

| Symbol | Meaning | Value |
|---|---|---|
| `M` | Featured markets on the board | 3 — `h2h`, `spreads`, `totals` |
| `R` | Regions | 1 (`us`) unless stated |
| `C` | Cost of one league sweep | `M × R` = **3 credits** |
| `L` | Leagues polled | 4 — NFL, NBA, MLB, NHL |
| — | Live in-play hours per league per day, in season | 5 |
| — | Same-day pregame window | 8 h/day @ 15 min cadence |
| — | Distant pregame + futures window | 11 h/day @ 60 min cadence |
| — | Settlement scores polling | 8/day @ 2 credits (`daysFrom` set) |
| — | Month | 30 days |

### The master formula

Live polling dominates, and it reduces to something memorable.

```
league-seconds of live coverage per day = L leagues × 5 h × 3600 s
                                        = 4 × 18,000
                                        = 72,000 league-seconds/day

requests/day (live)   = 72,000 / cadence_seconds
credits/day  (live)   = 3 × 72,000 / cadence_seconds  = 216,000 / cadence
credits/month (live)  = 30 × 216,000 / cadence        = 6,480,000 / cadence
```

Non-live overhead is a constant:

```
pregame   8 h @ 15 min = 32 requests × 3 =  96 credits/league/day
distant  11 h @ 60 min = 11 requests × 3 =  33 credits/league/day
scores        8 @ 2 credits              =  16 credits/league/day
                                          ───
                                            145 credits/league/day
        × 4 leagues × 30 days           = 17,400 credits/month
```

**Monthly total ≈ 6,480,000 / cadence_seconds + 17,400** for 4 leagues, 3 markets, 1 region.

### Live cadence sensitivity — 4 leagues, 3 markets, 1 region

| Live cadence | Live credits/mo | + overhead | Monthly total | Cheapest sufficient tier |
|---:|---:|---:|---:|---|
| 5 s | 1,296,000 | 17,400 | **1,313,400** | 5M — $119 |
| 10 s | 648,000 | 17,400 | **665,400** | 5M — $119 |
| 15 s | 432,000 | 17,400 | **449,400** | 5M — $119 |
| 30 s | 216,000 | 17,400 | **233,400** | 5M — $119 |
| 60 s | 108,000 | 17,400 | **125,400** | 5M — $119 |
| 90 s | 72,000 | 17,400 | **89,400** | **100K — $59** |
| 120 s | 54,000 | 17,400 | **71,400** | 100K — $59 |
| 300 s | 21,600 | 17,400 | **39,000** | 100K — $59 |

The cliff between 90 s and 60 s is the whole decision. **90-second live cadence across four
leagues fits the $59 tier with 10.6% headroom. 60-second cadence does not fit it at all** —
it needs the $119 tier, at which point cadence can drop to 15 s for the same money.

### Worked scenarios

#### Scenario A — free tier (Starter, 500 credits/month)

```
500 credits/month ÷ 30 days                 = 16.7 credits/day
at C = 3 (3 markets, 1 region)              = 5.6 requests/day
                                            = 1 request every 4 h 19 min

Absolute cheapest possible request, 1 market × 1 region = 1 credit:
500 requests/month ÷ 30                     = 16.7 requests/day
                                            = 1 request every 86 minutes
```

**Verdict: the free tier cannot drive a live board. Not at reduced cadence, not for one
league, not at all.** A board refreshing every 86 minutes is not a live odds product; it is
a screenshot.

What the free tier *is* sufficient for, and it is genuinely sufficient for these:

- validating the real adapter against real payload shapes during development
- **recording the golden files** the charter's §10 mandates for normalizer regression tests
- a handful of smoke calls in CI to detect a breaking provider schema change

**This is exactly why the synthetic provider is not optional and never becomes vestigial.**
§5's reasoning — "demos never burn API quota" — is not a nicety here; the free tier's
arithmetic makes the synthetic generator the only viable development path.

#### Scenario B — 20K tier ($30), single league, 1 region

Four leagues do not fit this tier at any usable cadence, so the honest question is what one
league buys.

```
Live      4 h/day @ 120 s = 120 requests × 3 =  360 credits/day
Pregame   8 h/day @  10 m =  48 requests × 3 =  144
Distant  12 h/day @  60 m =  12 requests × 3 =   36
Scores          12/day    @ 2 credits        =   24
                                              ─────
                                                564 credits/day
                                     × 30    = 16,920 credits/month
```

Against 20,000: **15.4% headroom.**

**Verdict: $30 buys one league, three featured markets, one region, at a 2-minute live
cadence.** That is a working live board and it is visibly slower than a real sportsbook.
Adequate for a single-league demo; inadequate for the charter's multi-league board (§6:
"live odds board across leagues").

#### Scenario C — 100K tier ($59), 4 leagues, 1 region — **the recommended tier**

```
Per league per day:
  Live     5 h @  90 s = 200 requests × 3 = 600 credits
  Pregame  8 h @ 15 m  =  32 requests × 3 =  96
  Distant 11 h @ 60 m  =  11 requests × 3 =  33
  Scores        8/day  @ 2 credits        =  16
                                           ─────
                                            745 credits/league/day

  × 4 leagues  = 2,980 credits/day
  × 30 days    = 89,400 credits/month
```

Against 100,000: **10.6% headroom** — enough to absorb an unusually busy week without
tripping the limiter, and not enough to be complacent about.

**Verdict: $59/month is the realistic minimum for a multi-league live board.** Four
leagues, moneyline/spread/total, one US region, 90-second live cadence. That is a credible
demonstration of the architecture and it is the tier this project should run on.

The visible compromise is the 90-second cadence. Sharpline's staleness SLO must be set
against *this* number, not against an aspirational one: a p99 odds-staleness target of ~95
seconds is honest at this tier, and a dashboard claiming sub-10-second freshness while
running here would be lying.

#### Scenario D — 5M tier ($119), 6 leagues, 2 regions, 10-second cadence

What a genuinely FanDuel-shaped board actually costs. `R = 2` (`us,us2`), so `C = 6`.

```
Per league per day:
  Live     6 h @  10 s = 2,160 requests × 6 = 12,960 credits
  Pregame 12 h @ 120 s =   360 requests × 6 =  2,160
  Scores          24/day @ 2 credits        =     48
                                             ───────
                                              15,168 credits/league/day

  × 6 leagues  =  91,008 credits/day
  × 30 days    = 2,730,240 credits/month
```

Against 5,000,000: **45.4% headroom.**

**Verdict: $119/month buys the real thing** — six leagues, two US regions, 10-second live
cadence, and roughly 2.27M credits/month left over for player props. The 15M tier ($249)
buys sub-5-second cadence and is not justified by anything in this charter.

#### Scenario E — player props, priced separately

Props are not available on `/odds`. They require `/events/{id}/odds`, which is **per event**
and therefore scales with slate size — the one place this provider's pricing stops being
favourable.

```
8 prop markets × 1 region                   = 8 credits per event per poll
NFL Sunday: 16 events, 4 h window @ 5 min   = 48 polls

48 polls × 16 events × 8 credits            = 6,144 credits
```

**One afternoon of NFL player props costs 6,144 credits — 6.1% of the entire 100K monthly
tier, spent in four hours.** A full NFL season of Sunday props alone is ≈110,000 credits.

**Verdict: player props are a 5M-tier feature.** On the recommended 100K tier they must be
restricted to a small set of marquee events at a slow cadence, or omitted. This is worth
stating in the product surface rather than discovering at 2pm on a Sunday.

#### Scenario F — historical backfill, priced separately

The 10× multiplier makes history expensive. `10 × 3 markets × 1 region = 30 credits` per
sport per snapshot.

| Goal | Arithmetic | Credits |
|---|---|---:|
| Closing line, one NFL season (~5 kickoff slots/week × 18 weeks) | 90 snapshots × 30 | **2,700** |
| 5-min line movement, 48 h pre-game, **one NFL week** | 576 snapshots × 30 | **17,280** |
| Same, full 18-week season | 18 × 17,280 | **311,040** |

**Verdict: buy closing lines, never buy line movement.** 2,700 credits for a season of
closing prices is cheap and directly enables CLV. But one week of minute-resolution history
costs 86% of the entire 20K tier, and a season costs 3.1× the 100K tier.

The right answer is architectural and it is already in the charter: **line history is
accumulated forward from our own live polling** into the Timescale hypertable. Every price
`ingest` observes is already written as an immutable row (§4). After one season of running,
Sharpline owns a line-movement dataset that would have cost hundreds of thousands of credits
to buy — which is a considerably better story than having purchased one.

### Where change detection does and does not save money

§5 mandates hashing each normalized market to suppress no-op updates, and backing off on
unchanged payloads. It is worth being precise about what that saves, because it is easy to
over-claim:

- **It always saves bus traffic, pricing CPU, and hypertable writes.** Most polls return
  identical data, and suppressing those is the difference between a quiet system and one
  that writes thousands of no-op rows per minute. This is real and it is the primary reason
  the mechanism exists.
- **It saves credits only where it triggers a longer backoff.** The credit is spent when
  the request is issued; discovering afterwards that the payload was unchanged does not
  refund it. Savings come only from the *next* poll being deferred.
- **Therefore it saves substantially in the pregame and futures windows** — where lines can
  sit unchanged for hours and backoff can extend the interval far beyond its floor — **and
  saves essentially nothing during live play**, where the payload changes on nearly every
  poll and backoff never engages.

Since live polling is 81% of the recommended tier's budget (72,000 of 89,400 credits),
**adaptive backoff should be modelled as reducing the 17,400-credit overhead, not the live
cost.** Every scenario above is computed without assuming any backoff saving, which makes
them upper bounds rather than optimistic estimates.

### Consequent implementation requirements

Not decisions, but things this arithmetic makes mandatory:

1. **Use `bookmakers=` rather than `regions=` for cross-region book coverage.** Ten named
   books spanning `us`, `us2`, and `eu` cost one region-equivalent instead of three. §6's
   multi-book comparison feature should be built on the `bookmakers` parameter.
2. **Refresh the event and league catalogue aggressively** — `/events` and `/sports` are
   free. Only price polling costs anything.
3. **Feed the Prometheus quota gauge from `x-requests-remaining`,** the provider's own
   number, not from a local counter. §5 requires the gauge; using the response header
   makes it authoritative and drift-proof.
4. **The token-bucket budget must be a config value** (§5) so the cadence can be retuned
   for a different tier without a code change.
5. **The limiter must fail to synthetic, not fail to stale.** When the budget is exhausted
   the correct behaviour is a loud alert and a visible degraded state — never a board that
   silently shows hour-old prices as if they were live.

---

## Consequences

**Made easier.**

- The analytics are real. CLV, steam detection, book disagreement, and the +EV finder
  operate on genuine market data, which is the difference between demonstrating an
  algorithm and demonstrating a result.
- The cost is legible and bounded. $0 for development, $59 for a credible multi-league live
  board, $119 for a genuinely fast one. No usage-based surprise.
- The free tier makes the repository clonable and runnable by anyone, and the golden-file
  regression tests §10 requires can be recorded without spending money.
- `x-requests-last` on every response means the quota gauge is reconciled against the
  provider's own accounting rather than estimated.

**Made harder, and accepted.**

- **The staleness SLO is now provider-bound, not architecture-bound.** At the recommended
  tier the system can price and fan out an update in single-digit milliseconds and still
  show a 90-second-old line, because that is when it was last fetched. The Grafana
  dashboard must distinguish *provider staleness* from *pipeline staleness* or it will
  measure the wrong thing and flatter the wrong component.
- **Player props are effectively out of reach below $119/month.** §6 lists player props in
  the market tree; on the 100K tier they are restricted to a handful of events.
- **Historical line movement is not purchasable at this budget.** It has to be accumulated,
  which means the interesting dataset does not exist until the system has been running for
  a while. This is a real limitation on early demos of the line-movement chart.
- **A provider outage now has a visible product consequence.** The synthetic fallback
  covers development but must not silently substitute for real data in a running
  deployment — that would be indistinguishable from fabricating market data, which is the
  precise thing the no-mock-data rule forbids. Failover must be explicit and surfaced.
- **A third-party dependency in the critical path**, with terms that can change. Mitigated
  by `ProviderAdapter` making replacement a single implementation.

**Costs knowingly paid.**

Coverage breadth. §13.1 called The Odds API "thinnest" and that is accurate: fewer exotic
markets and fewer books than OddsJam. The judgement is that breadth is not the constraint
on a project whose board is moneyline, spread, and total across four leagues — and that
paying OddsJam prices for coverage the product does not surface would be spending money to
look thorough.

## Alternatives considered

### OddsJam — rejected on cost-per-delivered-feature

Richest data of the three: deeper player props, more books, low-hold market coverage, and
purpose-built +EV and arbitrage feeds. Genuinely better data.

Rejected because **the features it charges for are features Sharpline computes itself.**
The whole intellectual core of this project is `internal/domain/odds` — devigging four
ways, fair value, EV, Kelly, arbitrage detection. Paying a premium for a provider that
delivers those as a finished product would replace the most interesting code in the
repository with an API call. That is backwards for a project whose purpose is to
demonstrate the engineering.

### SportsGameOdds — rejected on lack of a decisive advantage

Sits between the other two on both price and coverage. Nothing about it was decisively
better for this workload than The Odds API at a materially lower price. Recorded as the
natural first place to look if The Odds API's coverage becomes the binding constraint.

### Synthetic provider only, no real provider — rejected

The path the charter originally deferred to, and it is genuinely tempting: zero cost, fully
deterministic tests, no external dependency, and every code path exercised.

Rejected because **it hollows out the analytics.** A steam detector run against a generator
that the same author wrote to produce steam moves proves only that the two agree. CLV
against synthetic closing lines measures nothing. §0's success condition says "real odds
ingested from a licensed data provider", and the user's 2026-08-16 decision made prioritizing
that explicit. The synthetic generator remains essential — as the development and CI path,
which the free-tier arithmetic above shows it must be — but it is the fallback, not the
destination.

### Scraping sportsbook sites directly — rejected

Free and unlimited, and it is what a lot of hobby projects do.

Rejected on three independent grounds, any one of which is sufficient. It violates every
target site's terms of service. It is structurally fragile — a markup change breaks
ingestion silently and at the worst possible moment. And for a project explicitly framed
as a legal-liability-free demonstration, whose README leads with "no real money, no KYC,
no custody of funds", building it on a foundation of ToS violations would undermine the
exact credibility the framing exists to establish.

---

## Verification status

**Verified 2026-08-16** by direct fetch:

- Tier names, prices, and credit allowances — `the-odds-api.com` (two independent fetches
  of the pricing section), corroborated by a third-party 2026 odds-API pricing comparison
  that independently reports 20K/$30, 100K/$59, 5M/$119, 15M/$249.
- Cost formula `markets × regions`, the 10× historical multiplier, free `/sports` and
  `/events`, `/scores` at 1 or 2 credits, the `bookmakers`-as-region-equivalent rule, and
  the `x-requests-remaining` / `x-requests-used` / `x-requests-last` headers —
  `the-odds-api.com/liveapi/guides/v4/`.
- Player props and additional markets being `/events/{id}/odds`-only —
  `the-odds-api.com/sports-odds-data/betting-markets.html`.

**NOT verified, and flagged as such:**

- **Requests-per-second rate limiting.** The documentation states no per-second limit. It
  does not follow that none exists. The ingest scheduler should jitter its sweeps and
  handle HTTP 429 with backoff regardless.
- **Terms of service regarding redistribution or public display of odds data.** Not read
  as part of this ADR. **This must be checked before any public deployment**, since
  Sharpline displays provider odds in a browser. The README states that a user supplying
  their own key is responsible for compliance; that is a disclaimer, not a substitute for
  reading the terms.
- **Annual or student discounts**, and the "higher usage plans" the pricing page says are
  visible after account creation.
- **Actual number of events returned per league per sweep.** Assumed to be the full slate
  based on the documentation's statement that the endpoint mirrors the events listed by
  major bookmakers. Cost does not vary with it, so this affects payload size and
  normalizer throughput, not the budget.
- **Every polling-window figure in the assumptions table** — live hours per league per day,
  pregame windows, scores cadence. These are Sharpline's own design choices, not provider
  facts, and are stated as assumptions precisely so the arithmetic can be re-run when the
  real schedule is known.
