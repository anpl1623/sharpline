// Package results is the second arrow into the system: the loop that asks an
// odds provider for the outcome of contests that have finished and writes the
// terminal status and final score onto the `events` table.
//
//	provider (scores) → results poller → events.status/score → settle → [wager.events] → ledger
//
// That arrow is drawn in CLAUDE.md §3 as an input to `settle`, beside the odds
// flow rather than carried on it, and until this package existed it was the only
// arrow in the diagram with nothing implementing it. Everything downstream was
// already finished and already tested: internal/settlement grades, pays and
// voids from a results feed, and internal/settlement/pgstore reads that feed out
// of `events`. What was missing was anything that could put a row into it, so
// `settle` polled a well-formed query against a table that could never answer
// it, and a customer's stake could be taken and never released.
//
// # Why results are their own source and not a field on the odds path
//
// The argument is stated in full on provider.ResultsProvider and again in
// queries/results.sql; the short form, because it is the question a reader of
// this package will have first:
//
//   - odds.normalized is compacted and KEYED BY MARKET, and a finished contest
//     has no priced market to key on. The books take their prices down when play
//     ends, so an ended contest yields a payload the normalizer rejects whole.
//     There is nothing to hang a result on.
//   - The normalizer's exclusion of score and clock from the published record is
//     CORRECT and is not relaxed. A record carrying a live score would be
//     republished for every market on the event on every score change, which is
//     the bus flood change detection exists to prevent.
//   - Every candidate provider serves scores from a different endpoint with its
//     own quota cost and its own lookback window. An adapter that could only
//     state a result while it was also quoting a price could not use that
//     endpoint at all, because by the time the score exists the prices are gone.
//
// # NO MOCK DATA
//
// Nothing in this package invents a result, and neither does the shipped
// provider behind it. The synthetic adapter's score is scoreAt(ev, 1) — the same
// pure function of the event's static means and a seeded pace draw that
// model.go's newEventState already calls on every live fetch, driven by the same
// latent structure that prices the market. Asking that generator for a final
// score is reading the generator's own output, exactly as the odds path reads
// its prices.
//
// The mechanical form of that claim is that the write is an UPDATE and never an
// INSERT. A result for a contest this deployment never ingested cannot create a
// contest, so there is no route by which this package adds a row to the
// catalogue. See queries/results.sql's UpsertEventResult.
//
// # The work queue lives in the database, not in memory
//
// Every tick reads `events` for contests that started long enough ago to be
// plausibly over and have not yet reached a terminal status. A poller holding
// its own pending set would lose it on every deploy, and every contest that
// finished during the gap would go unsettled with no record that it had been
// missed — which is the failure this package exists to remove, reintroduced one
// layer up.
//
// The horizon is a HINT, not an authority. It decides what to ASK about; the
// provider decides what is actually finished. That split is deliberate: the
// horizon is one number that has to be wrong for some sport, and erring wide
// costs a query the provider answers with silence, while erring narrow costs a
// customer their settlement.
//
// # The two identifier spaces, and the crossing between them
//
// A provider names a contest in ITS OWN space — the shipped generator calls one
// `syn-sba-20260820-2` — and this system names the same contest in the domain
// space internal/ingest/normalizer derives from that key when it writes the
// catalogue row: `synthetic.e.syn-sba-20260820-2`. The work queue comes out of
// the database, so every identifier the loop holds is a domain one.
//
// The crossing happens HERE, in one place, in the FORWARD direction, using the
// same derivation the ingest path used. That is not a detail of the
// implementation; it is why provider.ResultsProvider is a window query answered
// in the provider's space rather than a lookup asked in ours. The first shape of
// this package got it the other way round — it passed domain identifiers into an
// adapter that compared them against native ones — and the result was not an
// error but a silence: 325 contests queried across 135 polls, every one of them
// reported `unresolved`, nothing settled, every stake in escrow, and a healthy
// looking loop the whole time.
//
// The forward direction is the only one available. normalizer.EventIDFor embeds
// a key verbatim when it is short and safe and HASHES it otherwise, so there is
// no inverse in general — and an inverse that worked for the embedded keys and
// not the hashed ones would fail precisely where nobody could tell that it had.
// The alternative that would also work is a provider_key column on `events`,
// which is a schema change to store what the forward direction already
// determines.
//
// # Idempotence, and why there is no memo
//
// Nothing here remembers which results it has recorded. It does not need to:
// UpsertEventResult's own guards make a replay a zero-row write. Its status
// predicate is the exact complement of the set settlement reads, so the
// statement ONLY EVER MOVES A ROW INTO THE RESULTS FEED — never out of it, never
// within it — and a result cannot un-settle a ticket that has already been
// graded and paid, or restate a cancellation as an ended game after the voids
// have gone through the ledger. Its observed_at guard refuses an older
// observation than the one stored.
//
// A memo would be a second, divergeable answer to a question the database
// already answers exactly, and it would have to survive a restart, which is the
// property that just cost this system its settlement feed.
//
// # What this package cannot fix, stated rather than hidden
//
// A contest whose result nobody will ever state — one that finished before the
// provider's lookback window opened, because ingest was down for days; a
// futures market the generator has no champion draw for — stays on the work
// queue for ever. It is not silently dropped and it is not invented: the
// queue-depth gauge shows it, and resolving it is an operator's decision through
// CLAUDE.md §6's admin console and its manual settlement, which is the right
// place for a judgement call about somebody's money.
//
// The bound that follows is worth naming because it is a starvation shape: the
// queue is read oldest-first under a LIMIT, so enough permanently-stuck rows
// would crowd out fresh ones. [DefaultBatchSize] is an order of magnitude above
// any steady-state queue, and the gauge is what makes the approach visible
// before it bites.
//
// # Layering
//
// This package holds the loop and the policy and imports no database driver. The
// `events` table is reached through [Store], which internal/ingest/results/pgstore
// implements over the generated queries — the same split internal/settlement
// makes, and for the same reason: the loop is asserted against a fake in a unit
// test and against real Postgres in the integration tier, with the same code
// under test.
package results
