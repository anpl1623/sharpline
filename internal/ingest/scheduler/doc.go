// Package scheduler decides WHEN to ask the odds provider for prices, and for
// WHAT.
//
// It is the piece CLAUDE.md §5 specifies in one paragraph:
//
//	"Adaptive polling: high frequency for live and near-tip events, low for
//	 futures, backing off on unchanged payloads. […] Respect provider quotas
//	 via a token-bucket limiter with the budget as a config value, and expose
//	 remaining quota as a Prometheus gauge."
//
// Everything below is an argument for one of those four clauses. The package
// fetches nothing itself: it drives a [Poller] (the provider adapter) and a
// [Catalogue] (the event list), both consumer-declared interfaces per
// CLAUDE.md §12.
//
// # The unit of work is a LEAGUE SWEEP, not an event
//
// This is the single most consequential decision in the package and it is
// forced by the provider's pricing model, not by taste. ADR 0003 records the
// cost formula for The Odds API:
//
//	GET /v4/sports/{sport}/odds   costs   markets × regions
//
// and, quoting the ADR, "one /odds request returns every upcoming and live
// event for that sport. Cost does NOT scale with the number of events. A
// 16-game NFL slate and a 1-game slate cost the same."
//
// So polling per event would multiply the bill by the slate size and buy
// nothing. The scheduler therefore schedules one sweep per league, and derives
// that league's cadence from its MOST URGENT event: a league with one game in
// play is polled at the live cadence, and the other fifteen games in the same
// payload come along for free. CLAUDE.md §5's "high frequency for live and
// near-tip events" is honoured exactly — it is just that the fine-grained
// decision is made per league because that is the granularity a credit buys.
//
// The synthetic provider costs zero credits and would be happy with per-event
// polling, but running the two adapters on different scheduling shapes would
// mean the offline path never exercises the code the online path depends on.
// One shape, both providers.
//
// # The tiers, and the quota arithmetic that fixes them
//
// [Window] classifies an event by its relationship to now. The defaults in
// [DefaultTiers] are:
//
//	window     interval  ceiling  chosen because
//	---------  --------  -------  ----------------------------------------------
//	live         90s       90s    ADR 0003 Scenario C: the fastest cadence that
//	                              fits the $59 100K tier across four leagues.
//	                              60s does not fit that tier at all.
//	near-tip    300s      600s    The 30 minutes before kickoff, when the sharp
//	                              money arrives and the line moves most per unit
//	                              of time outside live play.
//	today       900s     3600s    ADR 0003's pregame window (8h/day @ 15 min).
//	distant    3600s        4h    ADR 0003's distant window (11h/day @ 60 min).
//	futures       6h       24h    Season and tournament outrights. They move on
//	                              news, not on a clock.
//
// The arithmetic that justifies adding near-tip and futures to the ADR's three
// windows, at 3 credits per sweep and 4 leagues, per league per day:
//
//	live      5h  @  90s = 200 req × 3 = 600 credits
//	near-tip  1h  @ 300s =  12 req × 3 =  36
//	today     7h  @ 900s =  28 req × 3 =  84
//	distant  11h  @  60m =  11 req × 3 =  33
//	futures   24h @   6h =   4 req × 3 =  12
//	scores          8/day @ 2 credits  =  16
//	                                    ────
//	                                     781 credits/league/day
//
//	× 4 leagues × 30 days = 93,720 credits/month against the 100,000 tier
//	                      = 6.3% headroom.
//
// ADR 0003's own Scenario C computes 89,400 with 10.6% headroom; the extra
// 4,320 is what re-cutting its 8h pregame window into near-tip + today and
// adding a futures window costs. A near-tip cadence of 120s instead of 300s
// would cost a further 6,480/month, taking the total to 100,200 — which does not
// fit the 100K tier at all. That is why near-tip is 300s and not 120s.
//
// Every figure above is asserted by TestDefaultConfigFitsTheRecommendedTier and
// its two siblings, against [Config.MonthlyCredits]. A cadence change that blows
// the budget fails a test rather than an invoice, and the test is the reason to
// trust these numbers rather than the prose.
//
// None of them is hardcoded either: every one is a field on [Config], because
// CLAUDE.md §5 requires the budget to be a config value and ADR 0003 requires
// the cadence to be retunable for a different tier without a code change. This
// package deliberately does NOT read the environment itself — CLAUDE.md §12 puts
// configuration loading in one place and injects the result, and
// internal/ingest.LoadConfig is that place. See the note on [Config] and the
// open item in this package's phase handoff: LoadConfig currently overrides only
// Seed and CreditsPerSweep, so the cadence and the budget are retunable in code
// but not yet from a deployment's environment.
//
// # Backing off on unchanged payloads, and recovering fast
//
// §5: "backing off on unchanged payloads". A [PollResult] reporting zero
// changed markets doubles the league's interval, repeatedly, up to the tier's
// ceiling. ANY changed market resets the multiplier to 1 immediately, and so
// does a promotion to a more urgent window — a market that has just gone live
// is not "quiet", it is about to be the busiest thing on the board.
//
// Two properties of that design are deliberate:
//
//   - The live tier's ceiling EQUALS its interval, which disables backoff
//     during live play. ADR 0003 is explicit that suppression "saves
//     essentially nothing during live play, where the payload changes on nearly
//     every poll and backoff never engages" — so the only thing live backoff
//     can do is be wrong at the worst possible moment, when a line sits still
//     for three polls and then steams.
//   - Backoff never refunds a credit. The credit is spent when the request is
//     issued; discovering afterwards that the payload was unchanged does not
//     get it back. Savings come only from the NEXT poll being deferred, which
//     is why backoff is generous in the pregame and futures windows (where a
//     line can sit unchanged for hours) and absent in the live one.
//
// # The limiter is shared, and it is two mechanisms in one
//
// [Budget] holds both halves of "respect provider quotas":
//
//   - A TOKEN BUCKET paces the burn so a burst of live events cannot drain a
//     month's allowance in an afternoon. It refills at Budget/Period and bursts
//     to one day's allocation, which is what lets a five-hour live window
//     consume at 3.4× the average rate and still average out.
//   - A PERIOD LEDGER counts credits spent against the configured budget and
//     reports what is left. That is the number exported as
//     sharpline_provider_quota_remaining, and it is overridden by the
//     provider's own x-requests-remaining whenever an adapter reports one
//     (ADR 0003, "Consequent implementation requirements" #3: "Feed the
//     Prometheus quota gauge from x-requests-remaining, the provider's own
//     number, not from a local counter").
//
// When the ledger reaches zero, [Budget.Acquire] returns [ErrQuotaExhausted]
// and the sweep does not happen. It does NOT silently fail over to the
// synthetic generator: ADR 0003's consequences say a synthetic substitution in
// a running deployment "would be indistinguishable from fabricating market
// data, which is the precise thing the no-mock-data rule forbids". A frozen
// board plus a firing ProviderQuotaExhausted alert is the honest degraded
// state.
//
// # Concurrency and shutdown
//
// One goroutine per league — CLAUDE.md §2 chose Go for exactly this shape — plus
// one planner goroutine that refreshes the catalogue and re-windows the
// leagues. Concurrent in-flight polls are bounded by a counting semaphore
// (Config.MaxConcurrentPolls) so a fifty-league slate cannot open fifty
// simultaneous provider connections.
//
// Every goroutine stops on context cancellation and [Scheduler.Run] does not
// return until all of them have. An in-flight poll is not severed the instant
// SIGTERM lands: it is given Config.ShutdownGrace to finish, because its
// provider credit is already spent and abandoning the payload wastes it for
// nothing. TestSchedulerLeaksNoGoroutines asserts the whole thing terminates
// clean.
//
// # Metric names are a contract
//
// See metrics.go. Two series here are read from outside this package by
// deploy/observability, and one of them is half of the headline SLO's
// threshold. Read the dashboard JSON and the alert rules before renaming
// anything.
package scheduler
